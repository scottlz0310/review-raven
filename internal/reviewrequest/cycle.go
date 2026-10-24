package reviewrequest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// cycle is one row of review_requests.
type cycle struct {
	ID, Owner, Repo string
	PR, Round       int
	Head, Reason    string
	RequestedBy     string

	DispatchState string
	CandidateID   sql.NullString
	Attempts      int
	DispatchErr   sql.NullString

	CancelState       string
	CancelID          sql.NullString
	CancelRequestedAt sql.NullString
	CancelConfirmedAt sql.NullString
	CancelErr         sql.NullString

	ExecState      string
	ExecHold       sql.NullString
	ExecProducer   sql.NullString
	ExecEpoch      sql.NullInt64
	ExecSeq        sql.NullInt64
	ExecObservedAt sql.NullString
	ExecReceivedAt sql.NullString
	ExecOutcome    sql.NullString
	ExecExit       sql.NullInt64
	ExecStopped    bool

	ResultKind    sql.NullString
	ResultHead    sql.NullString
	ResultVersion sql.NullInt64
	ResultComment sql.NullInt64
	ResultURL     sql.NullString

	TerminalReason sql.NullString
	SupersededBy   sql.NullString
	TerminalAt     sql.NullString

	Revision  int
	CreatedAt string
	UpdatedAt string
}

// cycleColumns lists the columns in the order of cycle.fields().
var cycleColumns = []string{
	"cycle_id", "owner", "repo", "pr", "round", "expected_head", "reason", "requested_by",
	"dispatch_state", "candidate_id", "dispatch_attempts", "dispatch_error",
	"cancel_state", "cancel_id", "cancel_requested_at", "cancel_confirmed_at", "cancel_error",
	"exec_state", "exec_hold", "exec_producer", "exec_epoch", "exec_seq", "exec_observed_at",
	"exec_received_at", "exec_outcome", "exec_exit_code", "exec_stopped",
	"result_kind", "result_head", "result_version", "result_comment_id", "result_url",
	"terminal_reason", "superseded_by", "terminal_at",
	"revision", "created_at", "updated_at",
}

// fields returns pointers in cycleColumns order; used for both Scan and Exec
// (database/sql dereferences the pointers when binding parameters).
func (c *cycle) fields() []any {
	return []any{
		&c.ID, &c.Owner, &c.Repo, &c.PR, &c.Round, &c.Head, &c.Reason, &c.RequestedBy,
		&c.DispatchState, &c.CandidateID, &c.Attempts, &c.DispatchErr,
		&c.CancelState, &c.CancelID, &c.CancelRequestedAt, &c.CancelConfirmedAt, &c.CancelErr,
		&c.ExecState, &c.ExecHold, &c.ExecProducer, &c.ExecEpoch, &c.ExecSeq, &c.ExecObservedAt,
		&c.ExecReceivedAt, &c.ExecOutcome, &c.ExecExit, &c.ExecStopped,
		&c.ResultKind, &c.ResultHead, &c.ResultVersion, &c.ResultComment, &c.ResultURL,
		&c.TerminalReason, &c.SupersededBy, &c.TerminalAt,
		&c.Revision, &c.CreatedAt, &c.UpdatedAt,
	}
}

// touch records a state change: the snapshot revision increases on every
// change and never otherwise.
func (c *cycle) touch(now string) {
	c.Revision++
	c.UpdatedAt = now
}

func loadCycle(ctx context.Context, q queryer, id string) (*cycle, error) {
	c := &cycle{}
	err := q.QueryRowContext(ctx,
		`SELECT `+strings.Join(cycleColumns, ", ")+` FROM review_requests WHERE cycle_id = ?`, id).Scan(c.fields()...)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load review request: %w", err)
	}

	// 保持期限後に削除された ID は、未知の ID と区別して失効を返す。
	var expired string
	switch err := q.QueryRowContext(ctx, `SELECT cycle_id FROM expired_cycles WHERE cycle_id = ?`, id).Scan(&expired); {
	case err == nil:
		return nil, newError(CodeRequestExpired, "the request was removed after its retention period")
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("look up expired request: %w", err)
	}
	return nil, newError(CodeRequestNotFound, "no such review request")
}

func listUnfinished(ctx context.Context, q queryer, owner, repo string, pr int) ([]*cycle, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+strings.Join(cycleColumns, ", ")+` FROM review_requests
		  WHERE owner = ? AND repo = ? AND pr = ? AND terminal_reason IS NULL ORDER BY round`, owner, repo, pr)
	if err != nil {
		return nil, fmt.Errorf("list unfinished requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*cycle
	for rows.Next() {
		c := &cycle{}
		if err := rows.Scan(c.fields()...); err != nil {
			return nil, fmt.Errorf("scan review request: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review requests: %w", err)
	}
	return out, nil
}

func insertCycle(ctx context.Context, q queryer, c *cycle) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO review_requests (`+strings.Join(cycleColumns, ", ")+`) VALUES (`+
			strings.TrimSuffix(strings.Repeat("?, ", len(cycleColumns)), ", ")+`)`, c.fields()...)
	if err != nil {
		return fmt.Errorf("insert review request: %w", err)
	}
	recordChange(q, c.ID)
	return nil
}

// updateCycle writes every mutable column. The identity columns (cycle ID,
// repository, PR, round, expected head, reason, requester) never change.
func updateCycle(ctx context.Context, q queryer, c *cycle) error {
	const immutable = 8
	sets := make([]string, 0, len(cycleColumns)-immutable)
	args := make([]any, 0, len(cycleColumns)-immutable+1)
	for i, f := range c.fields() {
		if i < immutable {
			continue
		}
		sets = append(sets, cycleColumns[i]+" = ?")
		args = append(args, f)
	}
	args = append(args, c.ID)
	if _, err := q.ExecContext(ctx,
		`UPDATE review_requests SET `+strings.Join(sets, ", ")+` WHERE cycle_id = ?`, args...); err != nil {
		return fmt.Errorf("update review request: %w", err)
	}
	recordChange(q, c.ID)
	return nil
}

func (s *Service) enqueue(ctx context.Context, tx *sql.Tx, cycleID, kind, payload, now string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO request_outbox (cycle_id, kind, payload, next_attempt_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		cycleID, kind, payload, now, now); err != nil {
		return fmt.Errorf("enqueue %s message: %w", kind, err)
	}
	return nil
}

// acceptPayload is the accept_review_candidate input for the reviewer side.
func acceptPayload(c *cycle, sourceID string) string {
	var source *string
	if sourceID != "" {
		source = &sourceID
	}
	return mustJSON(map[string]any{
		"schemaVersion": SchemaVersion, "cycleId": c.ID, "round": c.Round,
		"owner": c.Owner, "repo": c.Repo, "prNumber": c.PR,
		"headSha": c.Head, "reason": c.Reason, "sourceId": source,
	})
}

// cancelPayload is the cancel_review_candidate input for the reviewer side.
func cancelPayload(c *cycle) string {
	return mustJSON(map[string]any{
		"schemaVersion": SchemaVersion, "cycleId": c.ID, "round": c.Round,
		"headSha": c.Head, "cancelId": c.CancelID.String,
	})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// 固定の形の map だけを渡すため到達しない。握りつぶさず、壊れた契約の送信を止める。
		panic("reviewrequest: marshal outbox payload: " + err.Error())
	}
	return string(b)
}

func strPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// project builds the snapshot. Freshness is derived from the clock, so it can
// change without a revision change; every stored field change bumps the revision.
func (s *Service) project(c *cycle) Snapshot {
	snap := Snapshot{
		SchemaVersion:   SchemaVersion,
		CycleID:         c.ID,
		Owner:           c.Owner,
		Repo:            c.Repo,
		PRNumber:        c.PR,
		ExpectedHeadSHA: c.Head,
		Round:           c.Round,
		Dispatch: SnapshotDispatch{
			State: c.DispatchState, CandidateID: strPtr(c.CandidateID),
			Attempts: c.Attempts, ErrorCode: strPtr(c.DispatchErr),
		},
		Cancellation: SnapshotCancel{
			State: c.CancelState, CancelID: strPtr(c.CancelID),
			RequestedAt: strPtr(c.CancelRequestedAt), ConfirmedAt: strPtr(c.CancelConfirmedAt),
			ErrorCode: strPtr(c.CancelErr),
		},
		Execution: SnapshotExec{
			State: c.ExecState, HoldReason: strPtr(c.ExecHold), ProducerID: strPtr(c.ExecProducer),
			ProducerEpoch: intPtr(c.ExecEpoch), Sequence: intPtr(c.ExecSeq),
			ObservedAt: strPtr(c.ExecObservedAt), ReceivedAt: strPtr(c.ExecReceivedAt),
			Freshness: s.freshness(c), Outcome: strPtr(c.ExecOutcome), ExitCode: intPtr(c.ExecExit),
		},
		Terminal:         c.TerminalReason.Valid,
		TerminalReason:   strPtr(c.TerminalReason),
		SupersededBy:     strPtr(c.SupersededBy),
		SnapshotRevision: c.Revision,
		UpdatedAt:        c.UpdatedAt,
	}
	if c.ResultKind.Valid {
		snap.Result = &SnapshotResult{
			Source: "thread-owl", Kind: c.ResultKind.String, HeadSHA: c.ResultHead.String,
			ResultVersion: int(c.ResultVersion.Int64),
			Post:          SnapshotPost{CommentID: c.ResultComment.Int64, URL: c.ResultURL.String},
		}
	}
	return snap
}

// freshness separates "no observation" (unknown), an explicit stop notice
// (stopped) and a silent producer (stale). An old observation delivered late
// is not treated as a new heartbeat, so staleness also compares the observation
// time with its receipt time.
func (s *Service) freshness(c *cycle) string {
	if !c.ExecObservedAt.Valid || !c.ExecReceivedAt.Valid {
		return FreshnessUnknown
	}
	if c.ExecStopped {
		return FreshnessStopped
	}
	received, err := time.Parse(timeLayout, c.ExecReceivedAt.String)
	if err != nil {
		return FreshnessUnknown
	}
	observed, err := time.Parse(time.RFC3339Nano, c.ExecObservedAt.String)
	if err != nil {
		return FreshnessUnknown
	}
	staleAfter := s.d.Settings.Observation.StaleAfter
	if s.d.Now().Sub(received) >= staleAfter || received.Sub(observed) >= staleAfter {
		return FreshnessStale
	}
	return FreshnessFresh
}
