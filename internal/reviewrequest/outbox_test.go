package reviewrequest

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeDeliverer is a scriptable reviewer side. The call counters are 1-based.
type fakeDeliverer struct {
	mu       sync.Mutex
	accepts  []AcceptMessage
	cancels  []CancelMessage
	onAccept func(call int, msg AcceptMessage) (AcceptResult, error)
	onCancel func(call int, msg CancelMessage) (CancelResult, error)
}

func (f *fakeDeliverer) Accept(_ context.Context, msg AcceptMessage) (AcceptResult, error) {
	f.mu.Lock()
	f.accepts = append(f.accepts, msg)
	call := len(f.accepts)
	f.mu.Unlock()
	if f.onAccept == nil {
		return AcceptResult{}, errors.New("accept is not scripted")
	}
	return f.onAccept(call, msg)
}

func (f *fakeDeliverer) Cancel(_ context.Context, msg CancelMessage) (CancelResult, error) {
	f.mu.Lock()
	f.cancels = append(f.cancels, msg)
	call := len(f.cancels)
	f.mu.Unlock()
	if f.onCancel == nil {
		return CancelResult{}, errors.New("cancel is not scripted")
	}
	return f.onCancel(call, msg)
}

func (f *fakeDeliverer) acceptCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accepts)
}

func (f *fakeDeliverer) cancelCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}

func acceptOK(_ int, _ AcceptMessage) (AcceptResult, error) {
	return AcceptResult{CandidateID: cand1}, nil
}

func cancelOK(_ int, msg CancelMessage) (CancelResult, error) {
	return CancelResult{CancelID: msg.CancelID, CandidateStatus: "not-accepted"}, nil
}

func failWith(code string) error { return newError(code, "scripted %s", code) }

// outboxEnv is an env whose retry spacing is deterministic (no jitter spread).
func outboxEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, func(d *Deps) { d.Jitter = func() float64 { return 0.5 } })
}

func (e *env) deliver(d Deliverer) int {
	e.t.Helper()
	n, err := e.svc.DeliverDue(e.ctx, d, 50)
	if err != nil {
		e.t.Fatalf("DeliverDue: %v", err)
	}
	return n
}

func (e *env) outboxOutcome(cycleID string, n int) string {
	e.t.Helper()
	var outcome sql.NullString
	if err := e.db.QueryRow(
		`SELECT outcome FROM request_outbox WHERE cycle_id = ? ORDER BY id LIMIT 1 OFFSET ?`, cycleID, n).Scan(&outcome); err != nil {
		e.t.Fatalf("outbox outcome of %s #%d: %v", cycleID, n, err)
	}
	return outcome.String
}

func TestDeliverAcceptSucceeds(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	d := &fakeDeliverer{onAccept: acceptOK}

	if n := e.deliver(d); n != 1 {
		t.Fatalf("handled %d messages, want 1", n)
	}
	// 配送する payload は契約の accept_review_candidate 入力に適合する。
	mustValid(t, "accept_review_candidate.input", d.accepts[0])
	if d.accepts[0].CycleID != cyc1 || d.accepts[0].Round != 1 || d.accepts[0].HeadSHA != headA || d.accepts[0].SourceID != nil {
		t.Fatalf("unexpected message: %+v", d.accepts[0])
	}

	snap := e.snapshot(cyc1)
	if snap.Dispatch.State != DispatchAccepted || snap.Dispatch.CandidateID == nil || *snap.Dispatch.CandidateID != cand1 {
		t.Fatalf("dispatch not accepted: %+v", snap.Dispatch)
	}
	if got := e.outboxOutcome(cyc1, 0); got != outcomeDelivered {
		t.Fatalf("outcome = %q, want %q", got, outcomeDelivered)
	}
	if n := e.deliver(d); n != 0 || d.acceptCalls() != 1 {
		t.Fatalf("a delivered message must not be sent again (handled %d, calls %d)", n, d.acceptCalls())
	}
}

func TestDeliverAcceptCarriesTheWebhookSource(t *testing.T) {
	e := outboxEnv(t)
	if _, err := e.svc.IngestCandidate(e.ctx, owl, IngestInput{
		SourceID: "delivery:0f1e2d3c-0001", Owner: testOwner, Repo: testRepo, PRNumber: testPR,
		HeadSHA: headA, Reason: "opened", Delivery: "webhook",
	}); err != nil {
		t.Fatal(err)
	}
	d := &fakeDeliverer{onAccept: acceptOK}
	e.deliver(d)
	if d.accepts[0].SourceID == nil || *d.accepts[0].SourceID != "delivery:0f1e2d3c-0001" {
		t.Fatalf("the source must travel with the candidate: %+v", d.accepts[0])
	}
	mustValid(t, "accept_review_candidate.input", d.accepts[0])
}

func TestDeliverAcceptRetriesWithBackoff(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	d := &fakeDeliverer{onAccept: func(call int, msg AcceptMessage) (AcceptResult, error) {
		if call <= 3 {
			return AcceptResult{}, failWith(CodeUnavailable)
		}
		return acceptOK(call, msg)
	}}

	// 失敗のたびに待ち時間が倍になる(1s, 2s, 4s)。待ち時間が過ぎるまで再送しない。
	for i, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if n := e.deliver(d); n != 1 {
			t.Fatalf("attempt %d: handled %d, want 1", i+1, n)
		}
		if snap := e.snapshot(cyc1); snap.Dispatch.State != DispatchPending || snap.Dispatch.Attempts != i+1 {
			t.Fatalf("attempt %d: dispatch = %+v", i+1, snap.Dispatch)
		}
		e.clock.advance(wait - time.Millisecond)
		if n := e.deliver(d); n != 0 {
			t.Fatalf("attempt %d: retried %v too early", i+1, wait)
		}
		e.clock.advance(time.Millisecond)
	}

	if n := e.deliver(d); n != 1 {
		t.Fatalf("final attempt handled %d, want 1", n)
	}
	snap := e.snapshot(cyc1)
	if snap.Dispatch.State != DispatchAccepted || snap.Dispatch.ErrorCode != nil || d.acceptCalls() != 4 {
		t.Fatalf("dispatch = %+v after %d calls", snap.Dispatch, d.acceptCalls())
	}
}

func TestDeliverAcceptGivesUpAtTheLimit(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	d := &fakeDeliverer{onAccept: func(int, AcceptMessage) (AcceptResult, error) {
		return AcceptResult{}, failWith(CodeUnavailable)
	}}
	limit := DefaultSettings().Delivery.MaxAttempts

	for range limit + 5 {
		e.deliver(d)
		e.clock.advance(2 * time.Minute) // 最大待ち時間を超えて進め、毎回 due にする
	}
	if got := d.acceptCalls(); got != limit {
		t.Fatalf("attempts = %d, want exactly the limit %d", got, limit)
	}
	snap := e.snapshot(cyc1)
	if snap.Dispatch.State != DispatchFailed || snap.Dispatch.ErrorCode == nil || *snap.Dispatch.ErrorCode != CodeUnavailable ||
		snap.Dispatch.Attempts != limit || snap.Terminal {
		t.Fatalf("a failed delivery must be failed, not terminal, not queued: %+v", snap)
	}
	if got := e.outboxOutcome(cyc1, 0); got != outcomeAbandoned {
		t.Fatalf("outcome = %q, want %q", got, outcomeAbandoned)
	}
}

// 認証・権限・schema 競合などの非再送エラーは、再送せず原因をそのまま公開する。
func TestDeliverAcceptRejectionIsFinal(t *testing.T) {
	for _, code := range []string{CodeOriginRejected, CodePermissionDenied, CodeHeadConflict, CodeCandidateCancelled, CodeInvalidArgument} {
		t.Run(code, func(t *testing.T) {
			e := outboxEnv(t)
			e.request("agent-req-0001", headA, "opened")
			d := &fakeDeliverer{onAccept: func(int, AcceptMessage) (AcceptResult, error) { return AcceptResult{}, failWith(code) }}

			e.deliver(d)
			e.clock.advance(10 * time.Minute)
			e.deliver(d)

			if d.acceptCalls() != 1 {
				t.Fatalf("a non-retryable rejection was retried (%d calls)", d.acceptCalls())
			}
			snap := e.snapshot(cyc1)
			if snap.Dispatch.State != DispatchFailed || *snap.Dispatch.ErrorCode != code || snap.Dispatch.Attempts != 1 {
				t.Fatalf("dispatch = %+v", snap.Dispatch)
			}
			if got := e.outboxOutcome(cyc1, 0); got != outcomeRejected {
				t.Fatalf("outcome = %q, want %q", got, outcomeRejected)
			}
		})
	}
}

func TestDeliverTransportFailureIsALostAcknowledgement(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	d := &fakeDeliverer{onAccept: func(int, AcceptMessage) (AcceptResult, error) { return AcceptResult{}, errors.New("connection reset") }}

	e.deliver(d)
	snap := e.snapshot(cyc1)
	if snap.Dispatch.State != DispatchPending || snap.Dispatch.Attempts != 1 {
		t.Fatalf("a transport failure must be retried, not final: %+v", snap.Dispatch)
	}
}

func TestShutdownIsNotCountedAsAFailedDelivery(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	ctx, cancel := context.WithCancel(context.Background())
	d := &fakeDeliverer{onAccept: func(int, AcceptMessage) (AcceptResult, error) {
		cancel() // 配送中に停止が始まる
		return AcceptResult{}, context.Canceled
	}}

	if _, err := e.svc.DeliverDue(ctx, d, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeliverDue() error = %v, want context.Canceled", err)
	}
	if snap := e.snapshot(cyc1); snap.Dispatch.Attempts != 0 || snap.Dispatch.State != DispatchPending {
		t.Fatalf("an interrupted delivery must not count as an attempt: %+v", snap.Dispatch)
	}
	// 次回の起動で、同じメッセージを再び配送する。
	good := &fakeDeliverer{onAccept: acceptOK}
	if n := e.deliver(good); n != 1 || e.snapshot(cyc1).Dispatch.State != DispatchAccepted {
		t.Fatalf("the interrupted message must be delivered after restart (handled %d)", n)
	}
}

func TestDeliverSkipsMessagesThatNoLongerMatter(t *testing.T) {
	t.Run("a superseded request is not delivered", func(t *testing.T) {
		e := outboxEnv(t)
		e.request("agent-req-0001", headA, "opened")
		e.heads.set(headB)
		e.request("agent-req-0002", headB, "synchronized")
		d := &fakeDeliverer{onAccept: acceptOK}

		e.deliver(d)
		if d.acceptCalls() != 1 || d.accepts[0].CycleID != cyc2 {
			t.Fatalf("only the current request must be delivered: %+v", d.accepts)
		}
		if got := e.outboxOutcome(cyc1, 0); got != outcomeSkipped {
			t.Fatalf("outcome of the superseded request = %q, want %q", got, outcomeSkipped)
		}
	})

	t.Run("a cancelling request is not delivered", func(t *testing.T) {
		e := outboxEnv(t)
		e.request("agent-req-0001", headA, "opened")
		cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
		d := &fakeDeliverer{onAccept: acceptOK, onCancel: cancelOK}

		e.deliver(d)
		if d.acceptCalls() != 0 || d.cancelCalls() != 1 {
			t.Fatalf("calls: accept=%d cancel=%d; the candidate must not be started once a cancellation was requested", d.acceptCalls(), d.cancelCalls())
		}
		if got := e.outboxOutcome(cyc1, 0); got != outcomeSkipped {
			t.Fatalf("outcome of the accept message = %q, want %q", got, outcomeSkipped)
		}
		if snap := e.snapshot(cyc1); !snap.Terminal || *snap.TerminalReason != TerminalCancelled {
			t.Fatalf("the acknowledged cancellation must be terminal: %+v", snap)
		}
	})

	t.Run("a request removed after its retention is dropped", func(t *testing.T) {
		e := outboxEnv(t)
		e.request("agent-req-0001", headA, "opened")
		if _, err := e.db.Exec(`DELETE FROM review_requests WHERE cycle_id = ?`, cyc1); err != nil {
			t.Fatal(err)
		}
		d := &fakeDeliverer{onAccept: acceptOK}
		e.deliver(d)
		if d.acceptCalls() != 0 || e.outboxOutcome(cyc1, 0) != outcomeDropped {
			t.Fatalf("calls=%d outcome=%q", d.acceptCalls(), e.outboxOutcome(cyc1, 0))
		}
	})
}

func TestDeliverCancelAcknowledged(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)
	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	d := &fakeDeliverer{onCancel: cancelOK}

	e.deliver(d)
	mustValid(t, "cancel_review_candidate.input", d.cancels[0])
	if d.cancels[0].CancelID != cxl1 || d.cancels[0].CycleID != cyc1 {
		t.Fatalf("unexpected message: %+v", d.cancels[0])
	}
	snap := e.snapshot(cyc1)
	if !snap.Terminal || *snap.TerminalReason != TerminalCancelled || snap.Cancellation.State != CancelConfirmed {
		t.Fatalf("cancel must be confirmed by the acknowledgement: %+v", snap)
	}
	if got := e.outboxOutcome(cyc1, 1); got != outcomeDelivered {
		t.Fatalf("outcome = %q, want %q", got, outcomeDelivered)
	}
}

func TestDeliverCancelWithdrawnWhenTheResultWon(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)
	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	d := &fakeDeliverer{onCancel: func(int, CancelMessage) (CancelResult, error) {
		return CancelResult{}, failWith(CodeCancelResultPosted)
	}}

	e.deliver(d)
	snap := e.snapshot(cyc1)
	if snap.Cancellation.State != CancelNone || snap.Cancellation.ErrorCode == nil || *snap.Cancellation.ErrorCode != CodeCancelResultPosted || snap.Terminal {
		t.Fatalf("the cancellation must be withdrawn, not confirmed: %+v", snap)
	}
	if got := e.outboxOutcome(cyc1, 1); got != outcomeWithdrawn {
		t.Fatalf("outcome = %q, want %q", got, outcomeWithdrawn)
	}
	// 結果の参照は publish_review_result で反映され、cycle は結果で終端になる。
	if _, err := e.svc.PublishResult(e.ctx, owl, resultInput(nil)); err != nil {
		t.Fatal(err)
	}
	if snap := e.snapshot(cyc1); !snap.Terminal || *snap.TerminalReason != TerminalResult {
		t.Fatalf("the result must end the cycle: %+v", snap)
	}
}

func TestDeliverCancelRetriesWhileTheWriteIsUnconfirmed(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)
	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	d := &fakeDeliverer{onCancel: func(call int, msg CancelMessage) (CancelResult, error) {
		if call <= 2 {
			return CancelResult{}, failWith(CodeCancelUnconfirmed)
		}
		return cancelOK(call, msg)
	}}

	for i := 1; i <= 2; i++ {
		e.deliver(d)
		snap := e.snapshot(cyc1)
		if snap.Terminal || snap.Cancellation.State != CancelRequested || snap.Cancellation.ErrorCode == nil || *snap.Cancellation.ErrorCode != CodeCancelUnconfirmed {
			t.Fatalf("attempt %d: an unconfirmed cancellation must stay requested with its cause: %+v", i, snap)
		}
		e.clock.advance(time.Minute)
	}
	e.deliver(d)
	if d.cancelCalls() != 3 {
		t.Fatalf("cancel calls = %d, want 3", d.cancelCalls())
	}
	for _, m := range d.cancels {
		if m.CancelID != cxl1 {
			t.Fatalf("every retry must reuse the same cancel id: %+v", d.cancels)
		}
	}
	snap := e.snapshot(cyc1)
	if !snap.Terminal || snap.Cancellation.State != CancelConfirmed || snap.Cancellation.ErrorCode != nil {
		t.Fatalf("the acknowledgement must confirm and clear the cause: %+v", snap)
	}
}

// 再送の上限に達した取消は、原因を公開して止まり、同じ取消 key の再送で明示的に再開できる。
func TestDeliverCancelGivesUpThenResumesWithTheSameKey(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)
	in := cancelInput(e, cyc1, "agent-cancel-0001")
	cancel(t, e, in)
	d := &fakeDeliverer{onCancel: func(int, CancelMessage) (CancelResult, error) { return CancelResult{}, failWith(CodeUnavailable) }}
	limit := DefaultSettings().Delivery.MaxAttempts

	for range limit + 3 {
		e.deliver(d)
		e.clock.advance(2 * time.Minute)
	}
	if d.cancelCalls() != limit {
		t.Fatalf("cancel attempts = %d, want the limit %d", d.cancelCalls(), limit)
	}
	snap := e.snapshot(cyc1)
	if snap.Terminal || snap.Cancellation.State != CancelRequested || *snap.Cancellation.ErrorCode != CodeUnavailable {
		t.Fatalf("an abandoned cancellation must stay requested and publish its cause: %+v", snap)
	}

	d.onCancel = cancelOK
	cancel(t, e, in) // 同じ key の再送 = 明示的な配送再開
	e.deliver(d)
	if snap := e.snapshot(cyc1); !snap.Terminal || snap.Cancellation.State != CancelConfirmed {
		t.Fatalf("resuming must deliver the cancellation again: %+v", snap)
	}
}

func TestDeliverCancelRejectionIsFinal(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)
	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	d := &fakeDeliverer{onCancel: func(int, CancelMessage) (CancelResult, error) { return CancelResult{}, failWith(CodePermissionDenied) }}

	e.deliver(d)
	e.clock.advance(10 * time.Minute)
	e.deliver(d)
	if d.cancelCalls() != 1 {
		t.Fatalf("a non-retryable rejection was retried (%d calls)", d.cancelCalls())
	}
	snap := e.snapshot(cyc1)
	if snap.Terminal || snap.Cancellation.State != CancelRequested || *snap.Cancellation.ErrorCode != CodePermissionDenied {
		t.Fatalf("the cause must be published: %+v", snap)
	}
	if got := e.outboxOutcome(cyc1, 1); got != outcomeRejected {
		t.Fatalf("outcome = %q, want %q", got, outcomeRejected)
	}
}

func TestDeliverCancelSkippedAfterTheResultWon(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	if _, err := e.svc.PublishResult(e.ctx, owl, resultInput(nil)); err != nil { // 結果が先に確定し、取消は撤回される
		t.Fatal(err)
	}
	d := &fakeDeliverer{onAccept: acceptOK, onCancel: cancelOK}

	e.deliver(d)
	if d.acceptCalls() != 0 || d.cancelCalls() != 0 {
		t.Fatalf("nothing may be sent for a finished request: accept=%d cancel=%d", d.acceptCalls(), d.cancelCalls())
	}
}

// 契約 scenario recover-failed-cycle-with-cancel を、配送 worker を通して再現する。
func TestRecoverFailedCycleThroughTheWorker(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	d := &fakeDeliverer{
		onAccept: func(int, AcceptMessage) (AcceptResult, error) { return AcceptResult{}, failWith(CodeUnavailable) },
		onCancel: cancelOK,
	}
	for range DefaultSettings().Delivery.MaxAttempts {
		e.deliver(d)
		e.clock.advance(2 * time.Minute)
	}
	if snap := e.snapshot(cyc1); snap.Dispatch.State != DispatchFailed || snap.Terminal {
		t.Fatalf("dispatch must fail without a terminal state: %+v", snap)
	}

	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	e.deliver(d)
	if snap := e.snapshot(cyc1); !snap.Terminal || *snap.TerminalReason != TerminalCancelled {
		t.Fatalf("the acknowledgement must cancel the failed cycle: %+v", snap)
	}

	// 同じ HEAD でも、新しい key は新 cycle・新 round になり、今度は配送が成功する。
	d.onAccept = acceptOK
	out := e.request("agent-req-0002", headA, "opened")
	if out.CycleID != cyc2 || out.Round != 2 {
		t.Fatalf("recovery must open round 2: %+v", out)
	}
	e.deliver(d)
	if snap := e.snapshot(cyc2); snap.Dispatch.State != DispatchAccepted {
		t.Fatalf("the new cycle must be dispatched: %+v", snap.Dispatch)
	}
}

func TestRunDeliveryProcessesUntilStopped(t *testing.T) {
	e := outboxEnv(t)
	e.request("agent-req-0001", headA, "opened")
	d := &fakeDeliverer{onAccept: acceptOK}

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.RunDelivery(ctx, d, 5*time.Millisecond) }()

	deadline := time.Now().Add(5 * time.Second)
	for d.acceptCalls() == 0 {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("the worker did not deliver the pending message")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunDelivery() = %v, want context.Canceled", err)
	}
	if err := e.svc.RunDelivery(context.Background(), d, 0); err == nil {
		t.Fatal("a non-positive interval must be rejected")
	}
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		name     string
		attempts int
		jitter   float64
		want     time.Duration
	}{
		{"first retry waits the initial backoff", 1, 0.5, time.Second},
		{"the wait doubles", 2, 0.5, 2 * time.Second},
		{"and doubles again", 4, 0.5, 8 * time.Second},
		{"the wait is capped", 10, 0.5, time.Minute},
		{"jitter floor", 1, 0, 800 * time.Millisecond},
		{"jitter ceiling", 1, 1, 1200 * time.Millisecond},
		{"the cap applies before the jitter", 10, 1, 72 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) { d.Jitter = func() float64 { return tc.jitter } })
			if got := e.svc.backoff(tc.attempts); got != tc.want {
				t.Fatalf("backoff(%d) = %v, want %v", tc.attempts, got, tc.want)
			}
		})
	}
}

func TestOutboxColumnIsAddedToAnExistingDatabase(t *testing.T) {
	db := openDB(t, ":memory:")
	// 台帳を最初の版(outcome 列なし)で作った DB。
	if _, err := db.Exec(`CREATE TABLE request_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT, cycle_id TEXT NOT NULL, kind TEXT NOT NULL, payload TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL, delivered_at TEXT, created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO request_outbox (cycle_id, kind, payload, next_attempt_at, created_at) VALUES ('c', 'accept', '{}', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}

	for range 2 { // 2 回目は列が既にあるため何もしない(冪等)
		if err := applySchema(context.Background(), db); err != nil {
			t.Fatalf("applySchema: %v", err)
		}
	}
	var outcome sql.NullString
	if err := db.QueryRow(`SELECT outcome FROM request_outbox`).Scan(&outcome); err != nil || outcome.Valid {
		t.Fatalf("outcome = %v, err = %v; want the new column with existing rows left pending", outcome, err)
	}
}
