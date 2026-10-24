package reviewrequest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const timeLayout = "2006-01-02T15:04:05.000Z"

// Deps are the collaborators of the Service. Authorizer, Heads and Producers
// have no defaults on purpose: a ledger that silently allows everything, or
// skips the head check, would defeat the contract.
type Deps struct {
	Authorizer Authorizer
	Heads      HeadResolver
	Producers  ProducerPolicy
	Settings   Settings
	Now        func() time.Time
	NewID      func(prefix string) string
}

// Service is the review-request ledger.
type Service struct {
	db *sql.DB
	d  Deps

	notifyMu sync.RWMutex
	notify   func(cycleID string)
}

// SetNotifier registers the callback invoked after a transaction commits, once
// per cycle whose stored state changed (the snapshot revision increased). It is
// how resource subscribers learn about updates. The callback runs on the
// caller's goroutine and must not block for long.
func (s *Service) SetNotifier(fn func(cycleID string)) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.notify = fn
}

func (s *Service) notifyChanged(ids []string) {
	s.notifyMu.RLock()
	fn := s.notify
	s.notifyMu.RUnlock()
	if fn == nil {
		return
	}
	for _, id := range ids {
		fn(id)
	}
}

// changeLog collects, per open transaction, the cycles written by it.
var changeLog sync.Map // *sql.Tx -> *changeList

type changeList struct {
	mu  sync.Mutex
	ids []string
}

func (l *changeList) add(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.ids {
		if existing == id {
			return
		}
	}
	l.ids = append(l.ids, id)
}

// recordChange notes that the cycle id was inserted or updated inside q.
func recordChange(q queryer, id string) {
	tx, ok := q.(*sql.Tx)
	if !ok {
		return
	}
	if v, ok := changeLog.Load(tx); ok {
		v.(*changeList).add(id)
	}
}

// NewService applies the ledger schema and returns a Service.
func NewService(ctx context.Context, db *sql.DB, deps Deps) (*Service, error) {
	switch {
	case deps.Authorizer == nil:
		return nil, errors.New("reviewrequest: Authorizer is required")
	case deps.Heads == nil:
		return nil, errors.New("reviewrequest: HeadResolver is required")
	case deps.Producers == nil:
		return nil, errors.New("reviewrequest: ProducerPolicy is required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewID == nil {
		deps.NewID = NewID
	}
	if err := deps.Settings.Validate(); err != nil {
		return nil, fmt.Errorf("reviewrequest: invalid settings: %w", err)
	}
	if err := applySchema(ctx, db); err != nil {
		return nil, err
	}
	return &Service{db: db, d: deps}, nil
}

func (s *Service) now() string { return s.d.Now().UTC().Format(timeLayout) }

// ResourceURI returns the immutable subscription URI of a cycle.
func ResourceURI(cycleID string) string { return "review-raven://requests/" + cycleID }

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Service) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	changes := &changeList{}
	changeLog.Store(tx, changes)
	defer changeLog.Delete(tx)

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	// 通知は commit の後に行う。rollback した変更を購読者へ見せない。
	s.notifyChanged(changes.ids)
	return nil
}

func hashPayload(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (s *Service) authorize(ctx context.Context, principal, action, owner, repo, requestedBy string) error {
	if principal == "" || !s.d.Authorizer.Authorize(ctx, principal, action, owner, repo, requestedBy) {
		return newError(CodePermissionDenied, "principal is not permitted to %s on this repository", action)
	}
	return nil
}

// RequestReview records a review request (first request or re-request) bound
// to the expected PR head, or returns the existing cycle for a repeated one.
func (s *Service) RequestReview(ctx context.Context, principal string, in RequestReviewInput) (RequestReviewOutput, error) {
	if err := validateRequest(in); err != nil {
		return RequestReviewOutput{}, err
	}
	if err := s.authorize(ctx, principal, ActionRequest, in.Owner, in.Repo, ""); err != nil {
		return RequestReviewOutput{}, err
	}

	res, err := s.createOrMerge(ctx, createParams{
		principal: principal, owner: in.Owner, repo: in.Repo, pr: in.PRNumber,
		head: in.ExpectedHeadSHA, reason: in.Reason,
		keyKind: "request", key: in.IdempotencyKey,
		payloadHash: hashPayload(in.ExpectedHeadSHA, in.Reason),
	})
	if err != nil {
		return RequestReviewOutput{}, err
	}
	return RequestReviewOutput{
		SchemaVersion:    SchemaVersion,
		CycleID:          res.c.ID,
		Round:            res.c.Round,
		HeadSHA:          res.c.Head,
		Dispatch:         res.c.DispatchState,
		ResourceURI:      ResourceURI(res.c.ID),
		SnapshotRevision: res.c.Revision,
		Deduplicated:     res.deduplicated,
	}, nil
}

// IngestCandidate accepts a candidate the reviewer side has already verified
// (webhook delivery or re-review comment). The same source ID, or an
// unfinished request for the same PR/HEAD/reason, resolves to the existing
// cycle; a source bound to a cancelled cycle never opens a new round.
func (s *Service) IngestCandidate(ctx context.Context, principal string, in IngestInput) (IngestOutput, error) {
	if err := validateIngest(in); err != nil {
		return IngestOutput{}, err
	}
	if err := s.authorize(ctx, principal, ActionIngest, in.Owner, in.Repo, ""); err != nil {
		return IngestOutput{}, err
	}

	res, err := s.createOrMerge(ctx, createParams{
		principal: principal, owner: in.Owner, repo: in.Repo, pr: in.PRNumber,
		head: in.HeadSHA, reason: in.Reason, sourceID: in.SourceID,
	})
	if err != nil {
		return IngestOutput{}, deliveryError(err)
	}
	return IngestOutput{
		SchemaVersion: SchemaVersion,
		CycleID:       res.c.ID,
		Round:         res.c.Round,
		Dispatch:      res.c.DispatchState,
		ResourceURI:   ResourceURI(res.c.ID),
		Deduplicated:  res.deduplicated,
	}, nil
}

type createParams struct {
	principal, owner, repo string
	pr                     int
	head, reason           string
	keyKind, key           string
	payloadHash            string
	sourceID               string
}

type createResult struct {
	c            *cycle
	deduplicated bool
}

func (s *Service) createOrMerge(ctx context.Context, p createParams) (*createResult, error) {
	// 既存の束縛（同一 key・同一 sourceId）は、PR の現在 HEAD が動いていても同じ cycle を返す。
	if res, err := s.lookupBinding(ctx, s.db, p); err != nil || res != nil {
		return res, err
	}

	head, err := s.d.Heads.CurrentHead(ctx, p.owner, p.repo, p.pr)
	if errors.Is(err, ErrPullRequestNotAccessible) {
		// 存在しない PR と installation 対象外の PR を区別しない（private PR の存在を明かさない）。
		return nil, newError(CodePermissionDenied, "the pull request is not accessible")
	}
	if err != nil {
		return nil, newError(CodeUnavailable, "could not read the current PR head: %v", err)
	}
	if head != p.head {
		return nil, newError(CodeHeadConflict, "expectedHeadSha does not match the current PR head")
	}

	var out *createResult
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		// 並行する同一 key の受付に負けた場合は、先に確定した cycle を返す。
		if res, err := s.lookupBinding(ctx, tx, p); err != nil || res != nil {
			out = res
			return err
		}

		unfinished, err := listUnfinished(ctx, tx, p.owner, p.repo, p.pr)
		if err != nil {
			return err
		}
		now := s.now()
		for _, c := range unfinished {
			// 取消要求中（無効化 ACK 前）の cycle は、reason が違っても同じ HEAD の再依頼を
			// 結合する。GitHub 書込み結果が不明な旧候補と新候補を並行させないため。
			if c.Head == p.head && (c.Reason == p.reason || c.CancelState == CancelRequested) {
				if err := s.bind(ctx, tx, p, c.ID, "", now); err != nil {
					return err
				}
				out = &createResult{c: c, deduplicated: true}
				return nil
			}
		}

		id := s.d.NewID(prefixCycle)
		for _, old := range unfinished {
			old.TerminalReason = sql.NullString{String: TerminalSuperseded, Valid: true}
			old.SupersededBy = sql.NullString{String: id, Valid: true}
			old.TerminalAt = sql.NullString{String: now, Valid: true}
			old.touch(now)
			if err := updateCycle(ctx, tx, old); err != nil {
				return err
			}
		}

		// round は cycle の削除後も残る PR 単位のカウンタから採番する（永続単調増加）。
		var round int
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO pr_rounds (owner, repo, pr, last_round) VALUES (?, ?, ?, 1)
			 ON CONFLICT (owner, repo, pr) DO UPDATE SET last_round = last_round + 1
			 RETURNING last_round`,
			p.owner, p.repo, p.pr).Scan(&round); err != nil {
			return fmt.Errorf("allocate round: %w", err)
		}

		c := &cycle{
			ID: id, Owner: p.owner, Repo: p.repo, PR: p.pr, Round: round,
			Head: p.head, Reason: p.reason, RequestedBy: p.principal,
			DispatchState: DispatchPending, CancelState: CancelNone, ExecState: ExecUnknown,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := insertCycle(ctx, tx, c); err != nil {
			return err
		}
		if err := s.bind(ctx, tx, p, id, "", now); err != nil {
			return err
		}
		// 状態の保存と同じ transaction で配送を予約する（transactional outbox）。
		if err := s.enqueue(ctx, tx, c.ID, outboxAccept, acceptPayload(c, p.sourceID), now); err != nil {
			return err
		}
		out = &createResult{c: c}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lookupBinding resolves an idempotency key or source ID to its cycle.
func (s *Service) lookupBinding(ctx context.Context, q queryer, p createParams) (*createResult, error) {
	var cycleID string
	switch {
	case p.key != "":
		var storedHash string
		err := q.QueryRowContext(ctx,
			`SELECT payload_hash, cycle_id FROM request_keys
			  WHERE principal = ? AND owner = ? AND repo = ? AND pr = ? AND kind = ? AND idem_key = ?`,
			p.principal, p.owner, p.repo, p.pr, p.keyKind, p.key).Scan(&storedHash, &cycleID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("look up idempotency key: %w", err)
		}
		if storedHash != p.payloadHash {
			return nil, newError(CodeIdempotencyConflict, "idempotencyKey was already used with a different request")
		}
	case p.sourceID != "":
		err := q.QueryRowContext(ctx, `SELECT cycle_id FROM source_bindings WHERE source_id = ?`, p.sourceID).Scan(&cycleID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("look up source binding: %w", err)
		}
	default:
		return nil, nil
	}

	c, err := loadCycle(ctx, q, cycleID)
	if err != nil {
		return nil, err
	}
	return &createResult{c: c, deduplicated: true}, nil
}

func (s *Service) bind(ctx context.Context, tx *sql.Tx, p createParams, cycleID, cancelID, now string) error {
	if p.key != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO request_keys (principal, owner, repo, pr, kind, idem_key, payload_hash, cycle_id, cancel_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
			p.principal, p.owner, p.repo, p.pr, p.keyKind, p.key, p.payloadHash, cycleID, cancelID, now); err != nil {
			return fmt.Errorf("bind idempotency key: %w", err)
		}
	}
	if p.sourceID != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO source_bindings (source_id, cycle_id, created_at) VALUES (?, ?, ?)`,
			p.sourceID, cycleID, now); err != nil {
			return fmt.Errorf("bind source id: %w", err)
		}
	}
	return nil
}

// CancelReview requests the explicit cancellation of a cycle that failed
// without a result. The cancellation is only requested here; it is confirmed
// once the reviewer side acknowledges that the old candidate is disabled.
func (s *Service) CancelReview(ctx context.Context, principal string, in CancelReviewInput) (CancelReviewOutput, error) {
	if err := validateCancel(in); err != nil {
		return CancelReviewOutput{}, err
	}

	var out CancelReviewOutput
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := loadCycle(ctx, tx, in.CycleID)
		if err != nil {
			return err
		}
		if err := s.authorize(ctx, principal, ActionCancel, c.Owner, c.Repo, c.RequestedBy); err != nil {
			return err
		}

		p := createParams{
			principal: principal, owner: c.Owner, repo: c.Repo, pr: c.PR,
			keyKind: "cancel", key: in.IdempotencyKey,
			payloadHash: hashPayload(in.CycleID, in.ExpectedHeadSHA, fmt.Sprint(in.ExpectedSnapshotRevision)),
		}
		now := s.now()

		// 同じ取消 key の再送は同じ取消 ID を返す。配送が失敗している場合に限り、
		// 同じ key による再送を明示的な配送再開として扱う（旧候補の再実行ではない）。
		var storedHash string
		err = tx.QueryRowContext(ctx,
			`SELECT payload_hash FROM request_keys
			  WHERE principal = ? AND owner = ? AND repo = ? AND pr = ? AND kind = 'cancel' AND idem_key = ?`,
			p.principal, p.owner, p.repo, p.pr, p.key).Scan(&storedHash)
		switch {
		case err == nil:
			if storedHash != p.payloadHash {
				return newError(CodeIdempotencyConflict, "idempotencyKey was already used with a different request")
			}
			// 結果が先に確定して取消が撤回された後の同じ key は、取消ではなく競合として返す。
			if c.CancelState == CancelNone {
				return newError(CodeCancelResultPosted, "a review result was already posted for this cycle")
			}
			if c.CancelState == CancelRequested && c.CancelErr.Valid {
				if err := s.resumeCancel(ctx, tx, c, now); err != nil {
					return err
				}
			}
			out = cancelOutput(c, true)
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("look up cancel key: %w", err)
		}

		if c.Head != in.ExpectedHeadSHA {
			return newError(CodeHeadConflict, "expectedHeadSha does not match the cycle")
		}

		// 取消中・取消確定済みの cycle への別 key の取消は、新しい取消を作らず既存へ結合する。
		if c.CancelState != CancelNone {
			if err := s.bind(ctx, tx, p, c.ID, c.CancelID.String, now); err != nil {
				return err
			}
			out = cancelOutput(c, true)
			return nil
		}
		if c.ResultKind.Valid {
			return newError(CodeCancelResultPosted, "a review result was already posted for this cycle")
		}
		if c.Revision != in.ExpectedSnapshotRevision {
			return newError(CodeRevisionConflict, "snapshot revision changed; read the request again")
		}

		cancelID := s.d.NewID(prefixCancel)
		c.CancelState = CancelRequested
		c.CancelID = sql.NullString{String: cancelID, Valid: true}
		c.CancelRequestedAt = sql.NullString{String: now, Valid: true}
		c.touch(now)
		if err := updateCycle(ctx, tx, c); err != nil {
			return err
		}
		if err := s.bind(ctx, tx, p, c.ID, cancelID, now); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, c.ID, outboxCancel, cancelPayload(c), now); err != nil {
			return err
		}
		out = cancelOutput(c, false)
		return nil
	})
	return out, err
}

func (s *Service) resumeCancel(ctx context.Context, tx *sql.Tx, c *cycle, now string) error {
	c.CancelErr = sql.NullString{}
	c.touch(now)
	if err := updateCycle(ctx, tx, c); err != nil {
		return err
	}
	return s.enqueue(ctx, tx, c.ID, outboxCancel, cancelPayload(c), now)
}

func cancelOutput(c *cycle, deduplicated bool) CancelReviewOutput {
	return CancelReviewOutput{
		SchemaVersion:    SchemaVersion,
		CycleID:          c.ID,
		CancelID:         c.CancelID.String,
		Cancellation:     c.CancelState,
		Terminal:         c.TerminalReason.Valid,
		SnapshotRevision: c.Revision,
		Deduplicated:     deduplicated,
	}
}

// Snapshot returns the current projection of a cycle for an authorized reader.
func (s *Service) Snapshot(ctx context.Context, principal, cycleID string) (Snapshot, error) {
	if !reCycleID.MatchString(cycleID) {
		return Snapshot{}, newError(CodeInvalidArgument, "cycleId must match the contract pattern")
	}
	c, err := loadCycle(ctx, s.db, cycleID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := s.authorize(ctx, principal, ActionRead, c.Owner, c.Repo, c.RequestedBy); err != nil {
		return Snapshot{}, err
	}
	return s.project(c), nil
}
