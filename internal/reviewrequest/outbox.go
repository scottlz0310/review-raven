package reviewrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// AcceptMessage is the accept_review_candidate input sent to the reviewer side.
type AcceptMessage struct {
	SchemaVersion int     `json:"schemaVersion"`
	CycleID       string  `json:"cycleId"`
	Round         int     `json:"round"`
	Owner         string  `json:"owner"`
	Repo          string  `json:"repo"`
	PRNumber      int     `json:"prNumber"`
	HeadSHA       string  `json:"headSha"`
	Reason        string  `json:"reason"`
	SourceID      *string `json:"sourceId"`
}

// AcceptResult is the accept_review_candidate output.
type AcceptResult struct {
	CandidateID string
	Duplicate   bool
}

// CancelMessage is the cancel_review_candidate input sent to the reviewer side.
type CancelMessage struct {
	SchemaVersion int    `json:"schemaVersion"`
	CycleID       string `json:"cycleId"`
	Round         int    `json:"round"`
	HeadSHA       string `json:"headSha"`
	CancelID      string `json:"cancelId"`
}

// CancelResult is the cancel_review_candidate output (the acknowledgement).
type CancelResult struct {
	CancelID        string
	CandidateStatus string
}

// Deliverer sends outbox messages to the reviewer side (thread-owl).
//
// A failure must be returned as a *Error carrying the contract code the
// reviewer side answered, or as UNAVAILABLE for a lost acknowledgement or a
// 5xx-like failure. Any other (non-contract) error is treated as UNAVAILABLE.
// Authentication, permission and schema conflicts are non-retryable codes and
// are never retried or routed around.
type Deliverer interface {
	Accept(ctx context.Context, msg AcceptMessage) (AcceptResult, error)
	Cancel(ctx context.Context, msg CancelMessage) (CancelResult, error)
}

// Outbox outcomes recorded on a finished row.
const (
	outcomeDelivered = "delivered" // 受け付けられた(または取消が確認された)
	outcomeSkipped   = "skipped"   // 状態が変わり、送る意味がなくなった
	outcomeRejected  = "rejected"  // 相手が非再送のエラーで拒否した
	outcomeAbandoned = "abandoned" // 再送の上限に達した
	outcomeWithdrawn = "withdrawn" // 結果が先に確定し、取消を撤回した
	outcomeDropped   = "dropped"   // 依頼が保持期限後に削除された
)

// deliveryTimeout bounds one call to the reviewer side so a stalled peer cannot
// hold the worker; a timeout is a lost acknowledgement and is retried.
const deliveryTimeout = 30 * time.Second

type outboxRow struct {
	id       int64
	cycleID  string
	kind     string
	payload  string
	attempts int
}

// DeliverDue sends the outbox messages that are due, oldest first, and returns
// how many it handled. Delivery is at-least-once: a crash between the call and
// the bookkeeping repeats the message, which the reviewer side answers
// idempotently. Failures of the peer are recorded on the request; only a
// failure of the ledger itself is returned.
func (s *Service) DeliverDue(ctx context.Context, d Deliverer, limit int) (int, error) {
	rows, err := s.dueOutbox(ctx, limit)
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, row := range rows {
		if err := s.deliver(ctx, d, row); err != nil {
			return handled, err
		}
		handled++
	}
	return handled, nil
}

// RunDelivery calls DeliverDue every interval until ctx ends. A ledger failure
// is logged and the next tick tries again; it is the polling contract of the
// worker, not a retry of a failed delivery.
func (s *Service) RunDelivery(ctx context.Context, d Deliverer, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("reviewrequest: delivery interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := s.DeliverDue(ctx, d, 50); err != nil && ctx.Err() == nil {
			slog.Error("review request delivery failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) dueOutbox(ctx context.Context, limit int) ([]outboxRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, cycle_id, kind, payload, attempts FROM request_outbox
		  WHERE delivered_at IS NULL AND next_attempt_at <= ? ORDER BY id LIMIT ?`, s.now(), limit)
	if err != nil {
		return nil, fmt.Errorf("select due outbox rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.id, &r.cycleID, &r.kind, &r.payload, &r.attempts); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox rows: %w", err)
	}
	return out, nil
}

func (s *Service) deliver(ctx context.Context, d Deliverer, row outboxRow) error {
	c, err := loadCycle(ctx, s.db, row.cycleID)
	if err != nil {
		switch ErrorCode(err) {
		case CodeRequestNotFound, CodeRequestExpired:
			return s.finishOutbox(ctx, row.id, outcomeDropped)
		}
		return err
	}
	switch row.kind {
	case outboxAccept:
		return s.deliverAccept(ctx, d, row, c)
	case outboxCancel:
		return s.deliverCancel(ctx, d, row, c)
	default:
		return fmt.Errorf("outbox row %d has an unknown kind %q", row.id, row.kind)
	}
}

func (s *Service) deliverAccept(ctx context.Context, d Deliverer, row outboxRow, c *cycle) error {
	// 終端になった・取消が始まった・すでに受理済みの依頼へ、新しい候補を送らない。
	if c.TerminalReason.Valid || c.CancelState != CancelNone || c.DispatchState != DispatchPending {
		return s.finishOutbox(ctx, row.id, outcomeSkipped)
	}
	var msg AcceptMessage
	if err := json.Unmarshal([]byte(row.payload), &msg); err != nil {
		return fmt.Errorf("outbox row %d has an unreadable accept payload: %w", row.id, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	result, callErr := d.Accept(callCtx, msg)
	cancel()

	if callErr == nil {
		if err := s.RecordDispatchAccepted(ctx, c.ID, result.CandidateID); err != nil {
			if ErrorCode(err) == CodeInvalidArgument {
				// 別の candidate がすでに記録されている、または不正な ID。握りつぶさず記録して止める。
				slog.Error("accepted candidate cannot be recorded", "cycle", c.ID, "err", err)
				return s.finishOutbox(ctx, row.id, outcomeAbandoned)
			}
			return err
		}
		return s.finishOutbox(ctx, row.id, outcomeDelivered)
	}
	if ctx.Err() != nil {
		return ctx.Err() // 停止中の中断は配送の失敗として数えない
	}

	code, retry := classifyDeliveryError(callErr)
	if !retry {
		if err := s.RecordDispatchFailure(ctx, c.ID, code, false); err != nil {
			return err
		}
		return s.finishOutbox(ctx, row.id, outcomeRejected)
	}
	if err := s.RecordDispatchFailure(ctx, c.ID, CodeUnavailable, true); err != nil {
		return err
	}
	updated, err := loadCycle(ctx, s.db, c.ID)
	if err != nil {
		return err
	}
	if updated.DispatchState == DispatchFailed {
		return s.finishOutbox(ctx, row.id, outcomeAbandoned) // 上限に達した。別経路へ切り替えない
	}
	return s.rescheduleOutbox(ctx, row, row.attempts+1)
}

func (s *Service) deliverCancel(ctx context.Context, d Deliverer, row outboxRow, c *cycle) error {
	var msg CancelMessage
	if err := json.Unmarshal([]byte(row.payload), &msg); err != nil {
		return fmt.Errorf("outbox row %d has an unreadable cancel payload: %w", row.id, err)
	}
	// 撤回された・確認済みの・別の取消に置き換わった取消は送らない。
	if c.CancelState != CancelRequested || c.CancelID.String != msg.CancelID {
		return s.finishOutbox(ctx, row.id, outcomeSkipped)
	}

	callCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	_, callErr := d.Cancel(callCtx, msg)
	cancel()

	if callErr == nil {
		if err := s.ConfirmCancel(ctx, c.ID, msg.CancelID); err != nil {
			return err
		}
		return s.finishOutbox(ctx, row.id, outcomeDelivered)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	code, retry := classifyDeliveryError(callErr)
	switch {
	case code == CodeCancelResultPosted:
		// 結果が先に確定していた。取消を撤回し、結果は publish_review_result で反映される。
		if err := s.RecordCancelRejected(ctx, c.ID, msg.CancelID, code); err != nil {
			return err
		}
		return s.finishOutbox(ctx, row.id, outcomeWithdrawn)
	case !retry:
		if err := s.RecordCancelRejected(ctx, c.ID, msg.CancelID, code); err != nil {
			return err
		}
		return s.finishOutbox(ctx, row.id, outcomeRejected)
	}

	// 受信側の GitHub 書込み結果が不明、または ACK 未受信。同じ取消 ID で上限まで再送する。
	if err := s.RecordCancelRejected(ctx, c.ID, msg.CancelID, code); err != nil {
		return err
	}
	if row.attempts+1 >= s.d.Settings.Delivery.MaxAttempts {
		return s.finishOutbox(ctx, row.id, outcomeAbandoned)
	}
	return s.rescheduleOutbox(ctx, row, row.attempts+1)
}

// classifyDeliveryError returns the contract code of a delivery failure and
// whether it may be retried. A non-contract error (transport failure, timeout)
// is a lost acknowledgement: UNAVAILABLE and retryable.
func classifyDeliveryError(err error) (code string, retry bool) {
	if contractErr, ok := AsError(err); ok && contractErrorCodes[contractErr.Code] {
		return contractErr.Code, contractErr.Retryable
	}
	return CodeUnavailable, true
}

// backoff is the wait before attempt number attempts+1 (attempts >= 1):
// initial * multiplier^(attempts-1), capped, spread by the jitter ratio.
func (s *Service) backoff(attempts int) time.Duration {
	cfg := s.d.Settings.Delivery
	wait := float64(cfg.InitialBackoff) * math.Pow(cfg.BackoffMultiplier, float64(attempts-1))
	if limit := float64(cfg.MaxBackoff); wait > limit {
		wait = limit
	}
	wait *= 1 + cfg.JitterRatio*(2*s.d.Jitter()-1)
	if wait < 0 {
		wait = 0
	}
	return time.Duration(wait)
}

func (s *Service) rescheduleOutbox(ctx context.Context, row outboxRow, attempts int) error {
	next := s.d.Now().UTC().Add(s.backoff(attempts)).Format(timeLayout)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE request_outbox SET attempts = ?, next_attempt_at = ? WHERE id = ?`, attempts, next, row.id); err != nil {
		return fmt.Errorf("reschedule outbox row %d: %w", row.id, err)
	}
	return nil
}

func (s *Service) finishOutbox(ctx context.Context, id int64, outcome string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE request_outbox SET delivered_at = ?, outcome = ? WHERE id = ?`, s.now(), outcome, id); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", id, err)
	}
	return nil
}
