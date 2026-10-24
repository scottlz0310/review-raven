package reviewrequest

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PublishResult records the reviewer's result reference (publish_review_result).
// The ledger stores only the reference to the GitHub post; it never generates a
// Verdict and never copies the body.
func (s *Service) PublishResult(ctx context.Context, principal string, in ResultInput) (ResultOutput, error) {
	if err := validateResult(in); err != nil {
		return ResultOutput{}, err
	}

	var out ResultOutput
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := loadCycle(ctx, tx, in.CycleID)
		if err != nil {
			return err
		}
		if err := s.authorize(ctx, principal, ActionPublishResult, c.Owner, c.Repo, c.RequestedBy); err != nil {
			return err
		}
		// 取消が確定した旧候補は、結果の新規投稿も受理しない。
		if c.CancelState == CancelConfirmed {
			return newError(CodeCandidateCancelled, "the candidate was cancelled")
		}
		if c.Head != in.HeadSHA {
			return newError(CodeResultHeadMismatch, "the result head does not match the cycle head")
		}
		if c.CandidateID.Valid && c.CandidateID.String != in.CandidateID {
			return newError(CodeInvalidArgument, "candidateId does not belong to this cycle")
		}

		if c.ResultVersion.Valid {
			current := int(c.ResultVersion.Int64)
			switch {
			case in.ResultVersion < current:
				return newError(CodeResultVersionStale, "a newer result version is already recorded")
			case in.ResultVersion == current:
				if c.ResultComment.Int64 != in.CommentID {
					return newError(CodeResultVersionStale, "this result version is already recorded for a different post")
				}
				out = resultOutput(c) // 同一内容の再送は冪等 ACK。revision は増やさない。
				return nil
			}
		}

		now := s.now()
		c.ResultKind = sql.NullString{String: in.Kind, Valid: true}
		c.ResultHead = sql.NullString{String: in.HeadSHA, Valid: true}
		c.ResultVersion = sql.NullInt64{Int64: int64(in.ResultVersion), Valid: true}
		c.ResultComment = sql.NullInt64{Int64: in.CommentID, Valid: true}
		c.ResultURL = sql.NullString{String: in.URL, Valid: true}
		// 結果が先に確定していれば、取消要求は競合として退く（結果を取消扱いにしない）。
		if c.CancelState == CancelRequested {
			c.CancelState = CancelNone
			c.CancelID, c.CancelRequestedAt = sql.NullString{}, sql.NullString{}
			c.CancelErr = sql.NullString{String: CodeCancelResultPosted, Valid: true}
		}
		// superseded な cycle への遅着は、旧 cycle の参照として保存する（terminalReason は変えない）。
		if !c.TerminalReason.Valid {
			c.TerminalReason = sql.NullString{String: TerminalResult, Valid: true}
			c.TerminalAt = sql.NullString{String: now, Valid: true}
		}
		c.touch(now)
		if err := updateCycle(ctx, tx, c); err != nil {
			return err
		}
		out = resultOutput(c)
		return nil
	})
	return out, err
}

func resultOutput(c *cycle) ResultOutput {
	return ResultOutput{
		SchemaVersion: SchemaVersion, Accepted: true,
		SnapshotRevision: c.Revision, Terminal: c.TerminalReason.Valid,
	}
}

// PublishObservation applies an execution observation (publish_review_observation).
// Late, duplicated, out-of-order and foreign observations never change the
// current state; they only return a result code.
func (s *Service) PublishObservation(ctx context.Context, principal string, in ObservationInput) (ObservationOutput, error) {
	if err := validateObservation(in); err != nil {
		return ObservationOutput{}, err
	}
	observedAt, err := time.Parse(time.RFC3339Nano, in.ObservedAt)
	if err != nil {
		return ObservationOutput{}, newError(CodeInvalidArgument, "observedAt must be a UTC timestamp")
	}

	var out ObservationOutput
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := loadCycle(ctx, tx, in.CycleID)
		if err != nil {
			return err
		}
		if err := s.authorize(ctx, principal, ActionObserve, c.Owner, c.Repo, c.RequestedBy); err != nil {
			return err
		}
		if !s.d.Producers.MayPublish(ctx, principal, in.ProducerID) {
			return newError(CodeProducerNotAssigned, "principal may not publish as this producer")
		}
		if c.Head != in.HeadSHA {
			return newError(CodeHeadConflict, "the observation head does not match the cycle head")
		}
		// 担当 candidate が確定する前、または別 candidate・別 producer の観測は割当外として拒否する。
		if !c.CandidateID.Valid || c.CandidateID.String != in.CandidateID ||
			(c.ExecProducer.Valid && c.ExecProducer.String != in.ProducerID) {
			return newError(CodeProducerNotAssigned, "the producer is not assigned to this candidate")
		}
		if c.CancelState == CancelConfirmed {
			return newError(CodeCandidateCancelled, "the candidate was cancelled")
		}
		if observedAt.After(s.d.Now().Add(s.d.Settings.Observation.ClockSkewLimit)) {
			return newError(CodeInvalidArgument, "observedAt is ahead of the receiver clock beyond the allowed skew")
		}

		if c.ExecEpoch.Valid {
			switch current := int(c.ExecEpoch.Int64); {
			case in.ProducerEpoch < current:
				return newError(CodeEpochStale, "the producer epoch was superseded by a restart")
			case in.ProducerEpoch == current:
				last := int(c.ExecSeq.Int64)
				switch {
				case in.Sequence < last:
					return newError(CodeObservationStale, "a newer observation was already applied")
				case in.Sequence == last:
					if !sameObservation(c, in) {
						return newError(CodeSequenceConflict, "the sequence was already used with different content")
					}
					out = ObservationOutput{SchemaVersion: SchemaVersion, Result: "duplicate", SnapshotRevision: c.Revision}
					return nil
				}
			}
			// より大きい epoch は新しい epoch の登録で、旧 epoch を失効させる。
		}

		now := s.now()
		c.ExecState = in.State
		c.ExecHold = nullString(in.HoldReason)
		c.ExecProducer = sql.NullString{String: in.ProducerID, Valid: true}
		c.ExecEpoch = sql.NullInt64{Int64: int64(in.ProducerEpoch), Valid: true}
		c.ExecSeq = sql.NullInt64{Int64: int64(in.Sequence), Valid: true}
		c.ExecObservedAt = sql.NullString{String: in.ObservedAt, Valid: true}
		c.ExecReceivedAt = sql.NullString{String: now, Valid: true}
		c.ExecOutcome = nullString(in.Outcome)
		c.ExecExit = nullInt(in.ExitCode)
		c.ExecStopped = in.Stopped
		c.touch(now)
		if err := updateCycle(ctx, tx, c); err != nil {
			return err
		}
		out = ObservationOutput{SchemaVersion: SchemaVersion, Result: "applied", SnapshotRevision: c.Revision}
		return nil
	})
	return out, err
}

func sameObservation(c *cycle, in ObservationInput) bool {
	return c.ExecState == in.State &&
		c.ExecHold == nullString(in.HoldReason) &&
		c.ExecOutcome == nullString(in.Outcome) &&
		c.ExecExit == nullInt(in.ExitCode) &&
		c.ExecStopped == in.Stopped &&
		c.ExecObservedAt.String == in.ObservedAt
}

func nullString(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

// RecordDispatchAccepted records that the reviewer side accepted the candidate.
func (s *Service) RecordDispatchAccepted(ctx context.Context, cycleID, candidateID string) error {
	if !reCycleID.MatchString(cycleID) || !reCandidateID.MatchString(candidateID) {
		return newError(CodeInvalidArgument, "cycleId and candidateId must match the contract patterns")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := loadCycle(ctx, tx, cycleID)
		if err != nil {
			return err
		}
		// 取消済みの旧候補の受理は反映しない（owl 側も CANDIDATE_CANCELLED で拒否する）。
		if c.CancelState == CancelConfirmed {
			return nil
		}
		if c.DispatchState == DispatchAccepted {
			if c.CandidateID.String == candidateID {
				return nil
			}
			return newError(CodeInvalidArgument, "a different candidate is already accepted for this cycle")
		}
		now := s.now()
		c.DispatchState = DispatchAccepted
		c.CandidateID = sql.NullString{String: candidateID, Valid: true}
		c.Attempts++
		c.DispatchErr = sql.NullString{}
		c.touch(now)
		return updateCycle(ctx, tx, c)
	})
}

// RecordDispatchFailure records a failed delivery attempt. A retryable failure
// (lost ACK, 5xx) keeps the request pending until the attempt limit; any other
// failure (for example ORIGIN_REJECTED) is final and is never retried. A failed
// delivery is never shown as queued.
func (s *Service) RecordDispatchFailure(ctx context.Context, cycleID, code string, isRetryable bool) error {
	if !reCycleID.MatchString(cycleID) {
		return newError(CodeInvalidArgument, "cycleId must match the contract pattern")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := loadCycle(ctx, tx, cycleID)
		if err != nil {
			return err
		}
		if c.DispatchState != DispatchPending {
			return nil
		}
		c.Attempts++
		switch {
		case !isRetryable:
			c.DispatchState = DispatchFailed
			c.DispatchErr = sql.NullString{String: code, Valid: true}
		case c.Attempts >= s.d.Settings.Delivery.MaxAttempts:
			c.DispatchState = DispatchFailed
			c.DispatchErr = sql.NullString{String: CodeUnavailable, Valid: true}
		}
		c.touch(s.now())
		return updateCycle(ctx, tx, c)
	})
}

// ConfirmCancel records the reviewer side's acknowledgement that the old
// candidate (start right and result-posting right) is disabled.
func (s *Service) ConfirmCancel(ctx context.Context, cycleID, cancelID string) error {
	if !reCycleID.MatchString(cycleID) {
		return newError(CodeInvalidArgument, "cycleId must match the contract pattern")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := loadCycle(ctx, tx, cycleID)
		if err != nil {
			return err
		}
		if !c.CancelID.Valid || c.CancelID.String != cancelID {
			return newError(CodeInvalidArgument, "cancelId does not belong to this cycle")
		}
		if c.CancelState == CancelConfirmed {
			return nil
		}
		now := s.now()
		c.CancelState = CancelConfirmed
		c.CancelConfirmedAt = sql.NullString{String: now, Valid: true}
		c.CancelErr = sql.NullString{}
		if !c.TerminalReason.Valid {
			c.TerminalReason = sql.NullString{String: TerminalCancelled, Valid: true}
			c.TerminalAt = sql.NullString{String: now, Valid: true}
		}
		c.touch(now)
		return updateCycle(ctx, tx, c)
	})
}

// RecordCancelRejected records why the cancellation was not acknowledged.
//   - CANCEL_RESULT_POSTED: the result won; the cancellation is withdrawn.
//   - CANCEL_UNCONFIRMED / UNAVAILABLE: still requested and not terminal; the
//     cause is published and the same cancel key can resume delivery.
func (s *Service) RecordCancelRejected(ctx context.Context, cycleID, cancelID, code string) error {
	if !reCycleID.MatchString(cycleID) {
		return newError(CodeInvalidArgument, "cycleId must match the contract pattern")
	}
	if code != CodeCancelResultPosted && code != CodeCancelUnconfirmed && code != CodeUnavailable {
		return newError(CodeInvalidArgument, "unsupported cancellation rejection code")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := loadCycle(ctx, tx, cycleID)
		if err != nil {
			return err
		}
		if c.CancelState != CancelRequested || c.CancelID.String != cancelID {
			return newError(CodeInvalidArgument, "no matching pending cancellation")
		}
		if code == CodeCancelResultPosted {
			c.CancelState = CancelNone
			c.CancelID, c.CancelRequestedAt = sql.NullString{}, sql.NullString{}
		}
		if c.CancelErr.String == code && code != CodeCancelResultPosted {
			return nil
		}
		c.CancelErr = sql.NullString{String: code, Valid: true}
		c.touch(s.now())
		return updateCycle(ctx, tx, c)
	})
}

// PurgeExpired removes finished requests older than the retention period,
// leaving a tombstone so the ID answers REQUEST_EXPIRED and is never reused.
// Idempotency keys older than their own retention are dropped as well.
func (s *Service) PurgeExpired(ctx context.Context) (int, error) {
	now := s.d.Now().UTC()
	cutoff := now.Add(-s.d.Settings.FinishedRequestRetention).Format(timeLayout)
	keyCutoff := now.Add(-s.d.Settings.IdempotencyRetention).Format(timeLayout)

	purged := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT cycle_id FROM review_requests WHERE terminal_at IS NOT NULL AND terminal_at < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("select expired requests: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan expired request: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate expired requests: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close expired requests: %w", err)
		}

		stamp := now.Format(timeLayout)
		for _, id := range ids {
			for _, stmt := range []string{
				`INSERT OR IGNORE INTO expired_cycles (cycle_id, expired_at) VALUES (?, '` + stamp + `')`,
				`DELETE FROM request_keys WHERE cycle_id = ?`,
				`DELETE FROM source_bindings WHERE cycle_id = ?`,
				`DELETE FROM request_outbox WHERE cycle_id = ?`,
				`DELETE FROM review_requests WHERE cycle_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
					return fmt.Errorf("purge request %s: %w", id, err)
				}
			}
			purged++
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM request_keys WHERE created_at < ?`, keyCutoff); err != nil {
			return fmt.Errorf("purge idempotency keys: %w", err)
		}
		return nil
	})
	return purged, err
}
