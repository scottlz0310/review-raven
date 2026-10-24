package reviewrequest

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	cases := []struct {
		prefix  string
		pattern *regexp.Regexp
	}{
		{prefixCycle, reCycleID},
		{prefixCancel, regexp.MustCompile(`^cxl_[0-9a-z]{26}$`)},
	}
	for _, tc := range cases {
		t.Run(tc.prefix, func(t *testing.T) {
			seen := map[string]bool{}
			for range 200 {
				id := NewID(tc.prefix)
				if !tc.pattern.MatchString(id) {
					t.Fatalf("id %q does not match %s", id, tc.pattern)
				}
				if seen[id] {
					t.Fatalf("duplicate id %q", id)
				}
				seen[id] = true
			}
		})
	}
}

func TestSettingsValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Settings)
		ok     bool
	}{
		{"defaults", func(*Settings) {}, true},
		{"retention below 30 days", func(s *Settings) { s.CancellationRecordRetention = 7 * day }, false},
		{"zero attempts", func(s *Settings) { s.Delivery.MaxAttempts = 0 }, false},
		{"jitter over one", func(s *Settings) { s.Delivery.JitterRatio = 2 }, false},
		{"stale before two heartbeats", func(s *Settings) { s.Observation.StaleAfter = time.Minute }, false},
		{"skew over five minutes", func(s *Settings) { s.Observation.ClockSkewLimit = 10 * time.Minute }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			tc.mutate(&s)
			if err := s.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestNewServiceRequiresPolicies(t *testing.T) {
	good := Deps{Authorizer: fakeAuth{}, Heads: &fakeHeads{}, Producers: fakeProducers{}, Settings: DefaultSettings()}
	cases := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"no authorizer", func(d *Deps) { d.Authorizer = nil }},
		{"no head resolver", func(d *Deps) { d.Heads = nil }},
		{"no producer policy", func(d *Deps) { d.Producers = nil }},
		{"invalid settings", func(d *Deps) { d.Settings.Delivery.MaxAttempts = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := good
			tc.mutate(&deps)
			if _, err := NewService(t.Context(), openDB(t, ":memory:"), deps); err == nil {
				t.Fatal("NewService succeeded without a required dependency")
			}
		})
	}
}

// scenario: idempotent-resend-and-conflict
func TestRequestReviewIdempotency(t *testing.T) {
	e := newEnv(t)

	first := e.request("agent-req-0001", headA, "opened")
	if first.CycleID != cyc1 || first.Round != 1 || first.Dispatch != DispatchPending || first.Deduplicated || first.SnapshotRevision != 1 {
		t.Fatalf("unexpected first output: %+v", first)
	}

	again := e.request("agent-req-0001", headA, "opened")
	if again.CycleID != cyc1 || !again.Deduplicated || again.SnapshotRevision != 1 {
		t.Fatalf("resend must return the same cycle: %+v", again)
	}

	wantCode(t, e.requestErr("agent-req-0001", headB, "opened"), CodeIdempotencyConflict)

	// 同じ key の再送は、PR の HEAD が動いた後でも同じ cycle を返す。
	e.heads.set(headB)
	if got := e.request("agent-req-0001", headA, "opened"); got.CycleID != cyc1 || !got.Deduplicated {
		t.Fatalf("resend after the PR head moved must still resolve to the first cycle: %+v", got)
	}

	if n := e.outboxCount(outboxAccept); n != 1 {
		t.Fatalf("accept messages = %d, want 1 (resends must not queue again)", n)
	}
}

// scenario: head-conflict-and-invalid-argument
func TestRequestReviewRejects(t *testing.T) {
	e := newEnv(t)
	valid := RequestReviewInput{Owner: testOwner, Repo: testRepo, PRNumber: testPR, ExpectedHeadSHA: headA, Reason: "opened", IdempotencyKey: "agent-req-0001"}

	t.Run("head conflict", func(t *testing.T) {
		wantCode(t, e.requestErr("agent-req-0002", headB, "opened"), CodeHeadConflict)
	})

	invalid := map[string]func(*RequestReviewInput){
		"owner uppercase":   func(in *RequestReviewInput) { in.Owner = "Scottlz0310" },
		"short head":        func(in *RequestReviewInput) { in.ExpectedHeadSHA = "abc123" },
		"uppercase head":    func(in *RequestReviewInput) { in.ExpectedHeadSHA = strings.ToUpper(headA) },
		"unknown reason":    func(in *RequestReviewInput) { in.Reason = "reopened" },
		"short key":         func(in *RequestReviewInput) { in.IdempotencyKey = "short" },
		"non-positive pr":   func(in *RequestReviewInput) { in.PRNumber = 0 },
		"repo with a slash": func(in *RequestReviewInput) { in.Repo = "a/b" },
	}
	for name, mutate := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			in := valid
			mutate(&in)
			_, err := e.svc.RequestReview(e.ctx, agent, in)
			wantCode(t, err, CodeInvalidArgument)
		})
	}

	t.Run("head lookup failure is retryable", func(t *testing.T) {
		e.heads.err = errors.New("github down")
		defer func() { e.heads.err = nil }()
		_, err := e.svc.RequestReview(e.ctx, agent, valid)
		wantCode(t, err, CodeUnavailable)
		var ce *Error
		if !errors.As(err, &ce) || !ce.Retryable {
			t.Fatalf("UNAVAILABLE must be retryable: %v", err)
		}
	})

	if n := e.outboxCount(outboxAccept); n != 0 {
		t.Fatalf("rejected requests queued %d messages", n)
	}
}

// scenario: permission-denied
func TestPermissionDenied(t *testing.T) {
	e := newEnv(t)
	first := e.request("agent-req-0001", headA, "opened")

	t.Run("request", func(t *testing.T) {
		_, err := e.svc.RequestReview(e.ctx, stranger, RequestReviewInput{
			Owner: testOwner, Repo: testRepo, PRNumber: testPR, ExpectedHeadSHA: headA, Reason: "opened", IdempotencyKey: "agent-req-0009",
		})
		wantCode(t, err, CodePermissionDenied)
	})
	t.Run("empty principal", func(t *testing.T) {
		_, err := e.svc.Snapshot(e.ctx, "", first.CycleID)
		wantCode(t, err, CodePermissionDenied)
	})
	t.Run("read even when the cycle id is known", func(t *testing.T) {
		_, err := e.svc.Snapshot(e.ctx, stranger, first.CycleID)
		wantCode(t, err, CodePermissionDenied)
	})
	t.Run("cancel by a non-requester without the operator role", func(t *testing.T) {
		e2 := newEnv(t)
		out := e2.request("agent-req-0001", headA, "opened")
		_, err := e2.svc.CancelReview(e2.ctx, owl, CancelReviewInput{
			CycleID: out.CycleID, ExpectedHeadSHA: headA, ExpectedSnapshotRevision: 1, IdempotencyKey: "cancel-key-0001",
		})
		wantCode(t, err, CodePermissionDenied)
	})
}

// scenario: not-found-and-expired
func TestNotFoundAndExpired(t *testing.T) {
	e := newEnv(t)

	_, err := e.svc.Snapshot(e.ctx, agent, cyc2)
	wantCode(t, err, CodeRequestNotFound)

	// cyc1 を superseded にして終端にし、保持期限を過ぎてから削除する。
	e.request("agent-req-0001", headA, "opened")
	e.heads.set(headB)
	e.request("agent-req-0002", headB, "synchronized")

	e.clock.advance(31 * day)
	purged, err := e.svc.PurgeExpired(e.ctx)
	if err != nil || purged != 1 {
		t.Fatalf("PurgeExpired() = %d, %v; want exactly the superseded cycle", purged, err)
	}
	_, err = e.svc.Snapshot(e.ctx, agent, cyc1)
	wantCode(t, err, CodeRequestExpired)

	// 未完了の cycle は保持期限で消えない。
	if snap := e.snapshot(cyc2); snap.Terminal {
		t.Fatalf("unfinished cycle must survive the purge: %+v", snap)
	}
}

// scenario: webhook-and-manual-merge
func TestWebhookAndManualMerge(t *testing.T) {
	e := newEnv(t)
	ingest := func(source string) IngestOutput {
		t.Helper()
		out, err := e.svc.IngestCandidate(e.ctx, owl, IngestInput{
			SourceID: source, Owner: testOwner, Repo: testRepo, PRNumber: testPR,
			HeadSHA: headA, Reason: "opened", Delivery: "webhook",
		})
		if err != nil {
			t.Fatalf("IngestCandidate(%s): %v", source, err)
		}
		mustValid(t, "ingest_review_candidate.output", out)
		return out
	}

	first := ingest("delivery:0f1e2d3c-0001")
	if first.CycleID != cyc1 || first.Deduplicated || first.Round != 1 {
		t.Fatalf("unexpected first ingest: %+v", first)
	}
	if manual := e.request("agent-req-0001", headA, "opened"); manual.CycleID != cyc1 || !manual.Deduplicated {
		t.Fatalf("a manual request for the same PR/HEAD/reason must join the open cycle: %+v", manual)
	}
	if again := ingest("delivery:0f1e2d3c-0001"); again.CycleID != cyc1 || !again.Deduplicated {
		t.Fatalf("redelivery of the same source must dedupe: %+v", again)
	}
	if other := ingest("delivery:0f1e2d3c-0002"); other.CycleID != cyc1 || !other.Deduplicated {
		t.Fatalf("a second source for the same open cycle must join it: %+v", other)
	}
	if n := e.outboxCount(outboxAccept); n != 1 {
		t.Fatalf("accept messages = %d, want 1 (no double queueing)", n)
	}

	t.Run("invalid delivery", func(t *testing.T) {
		_, err := e.svc.IngestCandidate(e.ctx, owl, IngestInput{
			SourceID: "delivery:x", Owner: testOwner, Repo: testRepo, PRNumber: testPR,
			HeadSHA: headA, Reason: "opened", Delivery: "manual",
		})
		wantCode(t, err, CodeInvalidArgument)
	})
	t.Run("denied for an unlisted principal", func(t *testing.T) {
		_, err := e.svc.IngestCandidate(e.ctx, stranger, IngestInput{
			SourceID: "delivery:y", Owner: testOwner, Repo: testRepo, PRNumber: testPR,
			HeadSHA: headA, Reason: "opened", Delivery: "webhook",
		})
		wantCode(t, err, CodePermissionDenied)
	})
}

// scenario: new-head-supersedes-old
func TestNewHeadSupersedesOld(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.heads.set(headB)
	second := e.request("agent-req-0002", headB, "synchronized")
	if second.CycleID != cyc2 || second.Round != 2 {
		t.Fatalf("new head must open round 2: %+v", second)
	}

	old := e.snapshot(cyc1)
	if !old.Terminal || *old.TerminalReason != TerminalSuperseded || old.SupersededBy == nil || *old.SupersededBy != cyc2 {
		t.Fatalf("old cycle must be superseded by the new one: %+v", old)
	}

	// 旧 HEAD の結果が遅着しても、旧 cycle の参照として保存し、新 cycle は更新しない。
	out, err := e.svc.PublishResult(e.ctx, owl, resultInput(nil))
	if err != nil {
		t.Fatalf("PublishResult for a superseded cycle: %v", err)
	}
	mustValid(t, "publish_review_result.output", out)
	old = e.snapshot(cyc1)
	if old.Result == nil || *old.TerminalReason != TerminalSuperseded {
		t.Fatalf("late result must be kept on the old cycle without changing its terminal reason: %+v", old)
	}
	if cur := e.snapshot(cyc2); cur.Result != nil || cur.Terminal || cur.Execution.State != ExecUnknown {
		t.Fatalf("new cycle must be untouched: %+v", cur)
	}
}

// scenario: observation-order-and-ownership
func TestObservationOrderAndOwnership(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)

	publish := func(in ObservationInput) (ObservationOutput, error) {
		out, err := e.svc.PublishObservation(e.ctx, squirrel, in)
		if err == nil {
			mustValid(t, "publish_review_observation.output", out)
		}
		return out, err
	}

	applied, err := publish(observation(nil))
	if err != nil || applied.Result != "applied" {
		t.Fatalf("first observation = %+v, %v", applied, err)
	}
	revision := applied.SnapshotRevision

	steps := []struct {
		name string
		in   ObservationInput
		code string // empty means success
		want string
	}{
		{"older sequence", observation(func(in *ObservationInput) { in.Sequence = 2 }), CodeObservationStale, ""},
		{"same sequence, same content", observation(nil), "", "duplicate"},
		{"same sequence, different content", observation(func(in *ObservationInput) {
			in.State, in.HoldReason = ExecRunning, nil
		}), CodeSequenceConflict, ""},
		{"stale epoch", observation(func(in *ObservationInput) { in.ProducerEpoch, in.Sequence = 0+1, 1 }), CodeObservationStale, ""},
		{"foreign producer", observation(func(in *ObservationInput) { in.ProducerID = "other-producer" }), CodeProducerNotAssigned, ""},
		{"foreign candidate", observation(func(in *ObservationInput) { in.CandidateID = cand2 }), CodeProducerNotAssigned, ""},
		{"foreign head", observation(func(in *ObservationInput) { in.HeadSHA = headB }), CodeHeadConflict, ""},
		{"clock far ahead", observation(func(in *ObservationInput) { in.Sequence, in.ObservedAt = 4, "2026-10-24T14:00:00Z" }), CodeInvalidArgument, ""},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			out, err := publish(tc.in)
			if tc.code != "" {
				wantCode(t, err, tc.code)
			} else if err != nil || out.Result != tc.want {
				t.Fatalf("got %+v, %v; want result %s", out, err, tc.want)
			}
			if got := e.snapshot(cyc1).SnapshotRevision; got != revision {
				t.Fatalf("a rejected or duplicate observation changed the revision: %d -> %d", revision, got)
			}
		})
	}

	// producer の再起動は、より大きい epoch で登録され、旧 epoch を失効させる。
	restarted, err := publish(observation(func(in *ObservationInput) { in.ProducerEpoch, in.Sequence = 2, 0 }))
	if err != nil || restarted.Result != "applied" {
		t.Fatalf("new epoch = %+v, %v", restarted, err)
	}
	_, err = publish(observation(func(in *ObservationInput) { in.ProducerEpoch, in.Sequence = 1, 9 }))
	wantCode(t, err, CodeEpochStale)

	snap := e.snapshot(cyc1)
	if *snap.Execution.ProducerEpoch != 2 || *snap.Execution.Sequence != 0 || snap.Execution.Freshness != FreshnessFresh {
		t.Fatalf("unexpected execution after restart: %+v", snap.Execution)
	}
}

func TestObservationNeedsAssignedCandidate(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	// dispatch がまだ受理されていない間は、担当 candidate が確定しないため観測を受理しない。
	_, err := e.svc.PublishObservation(e.ctx, squirrel, observation(nil))
	wantCode(t, err, CodeProducerNotAssigned)
}

// scenario: result-before-process-exit
func TestResultBeforeProcessExit(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)

	out, err := e.svc.PublishResult(e.ctx, owl, resultInput(func(in *ResultInput) { in.ResultVersion = 2 }))
	if err != nil || !out.Accepted || !out.Terminal {
		t.Fatalf("PublishResult = %+v, %v", out, err)
	}
	mustValid(t, "publish_review_result.output", out)

	// 結果の後に届く観測・heartbeat で terminal / result を退行させない。
	if _, err := e.svc.PublishObservation(e.ctx, squirrel, observation(func(in *ObservationInput) {
		in.State, in.HoldReason, in.Sequence = ExecRunning, nil, 8
	})); err != nil {
		t.Fatalf("PublishObservation after result: %v", err)
	}
	snap := e.snapshot(cyc1)
	if !snap.Terminal || *snap.TerminalReason != TerminalResult || snap.Result == nil ||
		snap.Result.Kind != "approved" || snap.Result.ResultVersion != 2 || snap.Execution.State != ExecRunning {
		t.Fatalf("snapshot regressed after a late observation: %+v", snap)
	}

	cases := []struct {
		name   string
		mutate func(*ResultInput)
		code   string
	}{
		{"older version", func(in *ResultInput) { in.ResultVersion = 1 }, CodeResultVersionStale},
		{"same version, different post", func(in *ResultInput) { in.ResultVersion, in.CommentID = 2, 1 }, CodeResultVersionStale},
		{"different head", func(in *ResultInput) { in.ResultVersion, in.HeadSHA = 3, headB }, CodeResultHeadMismatch},
		{"foreign candidate", func(in *ResultInput) { in.ResultVersion, in.CandidateID = 3, cand2 }, CodeInvalidArgument},
		{"non-github url", func(in *ResultInput) { in.ResultVersion, in.URL = 3, "https://example.com/x" }, CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.PublishResult(e.ctx, owl, resultInput(tc.mutate))
			wantCode(t, err, tc.code)
		})
	}

	t.Run("identical resend is an idempotent ACK", func(t *testing.T) {
		before := e.snapshot(cyc1).SnapshotRevision
		again, err := e.svc.PublishResult(e.ctx, owl, resultInput(func(in *ResultInput) { in.ResultVersion = 2 }))
		if err != nil || again.SnapshotRevision != before {
			t.Fatalf("resend = %+v, %v; revision must stay %d", again, err, before)
		}
	})
	t.Run("result needs the owl principal", func(t *testing.T) {
		_, err := e.svc.PublishResult(e.ctx, stranger, resultInput(nil))
		wantCode(t, err, CodePermissionDenied)
	})
}

func exhaustDelivery(t *testing.T, e *env, cycleID string) {
	t.Helper()
	for range DefaultSettings().Delivery.MaxAttempts {
		if err := e.svc.RecordDispatchFailure(e.ctx, cycleID, CodeUnavailable, true); err != nil {
			t.Fatalf("RecordDispatchFailure: %v", err)
		}
	}
}

func cancelInput(e *env, cycleID, key string) CancelReviewInput {
	return CancelReviewInput{
		CycleID: cycleID, ExpectedHeadSHA: headA,
		ExpectedSnapshotRevision: e.snapshot(cycleID).SnapshotRevision, IdempotencyKey: key,
	}
}

func cancel(t *testing.T, e *env, in CancelReviewInput) CancelReviewOutput {
	t.Helper()
	out, err := e.svc.CancelReview(e.ctx, agent, in)
	if err != nil {
		t.Fatalf("CancelReview: %v", err)
	}
	mustValid(t, "cancel_review_request.output", out)
	return out
}

// scenario: recover-failed-cycle-with-cancel
func TestRecoverFailedCycleWithCancel(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	exhaustDelivery(t, e, cyc1)

	failed := e.snapshot(cyc1)
	if failed.Dispatch.State != DispatchFailed || *failed.Dispatch.ErrorCode != CodeUnavailable || failed.Terminal {
		t.Fatalf("exhausted delivery must be failed but not terminal: %+v", failed)
	}

	in := cancelInput(e, cyc1, "agent-cancel-0001")
	requested := cancel(t, e, in)
	if requested.CancelID != cxl1 || requested.Cancellation != CancelRequested || requested.Terminal || requested.Deduplicated {
		t.Fatalf("unexpected cancel output: %+v", requested)
	}
	if again := cancel(t, e, in); !again.Deduplicated || again.CancelID != cxl1 || again.SnapshotRevision != requested.SnapshotRevision {
		t.Fatalf("resending the cancel key must return the same cancellation: %+v", again)
	}

	// 取消要求中の再依頼は、新 round を作らず既存 cycle に結合する。
	during := e.request("agent-req-0002", headA, "opened")
	if during.CycleID != cyc1 || !during.Deduplicated || during.Dispatch != DispatchFailed {
		t.Fatalf("a request during cancellation must join the existing cycle: %+v", during)
	}

	// 無効化 ACK 前は terminal にしない。ACK 後に cancelled / terminal になる。
	if snap := e.snapshot(cyc1); snap.Terminal || snap.Cancellation.State != CancelRequested {
		t.Fatalf("cancel must not be terminal before the acknowledgement: %+v", snap)
	}
	if err := e.svc.ConfirmCancel(e.ctx, cyc1, cxl1); err != nil {
		t.Fatalf("ConfirmCancel: %v", err)
	}
	snap := e.snapshot(cyc1)
	if !snap.Terminal || *snap.TerminalReason != TerminalCancelled || snap.Cancellation.State != CancelConfirmed {
		t.Fatalf("cancel must be terminal after the acknowledgement: %+v", snap)
	}
	if err := e.svc.ConfirmCancel(e.ctx, cyc1, cxl1); err != nil {
		t.Fatalf("a repeated acknowledgement must be a no-op: %v", err)
	}

	// 同じ HEAD でも、新しい key なら新 cycle・新 round。旧 key は旧 cycle を返し続ける。
	recovered := e.request("agent-req-0003", headA, "opened")
	if recovered.CycleID != cyc2 || recovered.Round != 2 || recovered.Deduplicated {
		t.Fatalf("recovery must open a new cycle and round: %+v", recovered)
	}
	if old := e.request("agent-req-0001", headA, "opened"); old.CycleID != cyc1 || !old.Deduplicated {
		t.Fatalf("an old key must keep returning the cancelled cycle: %+v", old)
	}
	if n := e.outboxCount(outboxAccept); n != 2 {
		t.Fatalf("accept messages = %d, want 2", n)
	}
	if n := e.outboxCount(outboxCancel); n != 1 {
		t.Fatalf("cancel messages = %d, want 1", n)
	}
}

func TestCancelResumeWithSameKey(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	in := cancelInput(e, cyc1, "agent-cancel-0001")
	cancel(t, e, in)

	// 配送の再送上限に達した場合、原因を公開し、terminal にはしない。
	if err := e.svc.RecordCancelRejected(e.ctx, cyc1, cxl1, CodeUnavailable); err != nil {
		t.Fatalf("RecordCancelRejected: %v", err)
	}
	snap := e.snapshot(cyc1)
	if snap.Terminal || snap.Cancellation.State != CancelRequested || snap.Cancellation.ErrorCode == nil || *snap.Cancellation.ErrorCode != CodeUnavailable {
		t.Fatalf("a failed cancel delivery must publish its cause and stay requested: %+v", snap)
	}

	// 配送が終わった(上限到達)状態にしてから、同じ取消 key の再送だけが明示的な配送再開になる。
	if _, err := e.db.Exec(`UPDATE request_outbox SET delivered_at = ?, outcome = ? WHERE kind = 'cancel'`, e.svc.now(), outcomeAbandoned); err != nil {
		t.Fatal(err)
	}
	resumed := cancel(t, e, in)
	if !resumed.Deduplicated || resumed.CancelID != cxl1 {
		t.Fatalf("resume must reuse the cancel id: %+v", resumed)
	}
	if got := e.snapshot(cyc1); got.Cancellation.ErrorCode != nil {
		t.Fatalf("resuming must clear the previous cause: %+v", got.Cancellation)
	}
	if n := e.outboxCount(outboxCancel); n != 2 {
		t.Fatalf("cancel messages = %d, want 2 (initial + resume)", n)
	}
}

// scenario: cancel-conflicts
func TestCancelConflicts(t *testing.T) {
	t.Run("stale revision", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		e.accept(cyc1, cand1) // revision 2
		in := cancelInput(e, cyc1, "agent-cancel-0001")
		in.ExpectedSnapshotRevision = 1
		_, err := e.svc.CancelReview(e.ctx, agent, in)
		wantCode(t, err, CodeRevisionConflict)
	})
	t.Run("different head", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		in := cancelInput(e, cyc1, "agent-cancel-0001")
		in.ExpectedHeadSHA = headB
		_, err := e.svc.CancelReview(e.ctx, agent, in)
		wantCode(t, err, CodeHeadConflict)
	})
	t.Run("same key, different payload", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		in := cancelInput(e, cyc1, "agent-cancel-0001")
		cancel(t, e, in)
		in.ExpectedHeadSHA = headB
		_, err := e.svc.CancelReview(e.ctx, agent, in)
		wantCode(t, err, CodeIdempotencyConflict)
	})
	t.Run("result already posted", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		if _, err := e.svc.PublishResult(e.ctx, owl, resultInput(nil)); err != nil {
			t.Fatal(err)
		}
		_, err := e.svc.CancelReview(e.ctx, agent, cancelInput(e, cyc1, "agent-cancel-0001"))
		wantCode(t, err, CodeCancelResultPosted)
	})
	t.Run("result wins a race with a pending cancellation", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		in := cancelInput(e, cyc1, "agent-cancel-0001")
		cancel(t, e, in)
		if err := e.svc.RecordCancelRejected(e.ctx, cyc1, cxl1, CodeCancelResultPosted); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.PublishResult(e.ctx, owl, resultInput(nil)); err != nil {
			t.Fatal(err)
		}
		snap := e.snapshot(cyc1)
		if !snap.Terminal || *snap.TerminalReason != TerminalResult || snap.Cancellation.State != CancelNone || snap.Result == nil {
			t.Fatalf("the result must win and the cancellation must be withdrawn: %+v", snap)
		}
		_, err := e.svc.CancelReview(e.ctx, agent, in)
		wantCode(t, err, CodeCancelResultPosted)
	})
	t.Run("github write unconfirmed keeps the cycle open", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
		if err := e.svc.RecordCancelRejected(e.ctx, cyc1, cxl1, CodeCancelUnconfirmed); err != nil {
			t.Fatal(err)
		}
		snap := e.snapshot(cyc1)
		if snap.Terminal || snap.Cancellation.State != CancelRequested || *snap.Cancellation.ErrorCode != CodeCancelUnconfirmed {
			t.Fatalf("an unconfirmed cancellation must neither confirm nor open a new round: %+v", snap)
		}
	})
	t.Run("operator may cancel another user's request", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		in := cancelInput(e, cyc1, "operator-cancel-1")
		if _, err := e.svc.CancelReview(e.ctx, operator, in); err != nil {
			t.Fatalf("operator cancel: %v", err)
		}
	})
	t.Run("only contract codes are accepted as a rejection cause", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
		wantCode(t, e.svc.RecordCancelRejected(e.ctx, cyc1, cxl1, "NOT_A_CONTRACT_CODE"), CodeInvalidArgument)
		wantCode(t, e.svc.ConfirmCancel(e.ctx, cyc1, "cxl_01jbq3x8n5r7t9v2w4y6z8a0e9"), CodeInvalidArgument)
	})
}

// scenario: cancel-before-accept (ledger side)
func TestCancelledCycleRejectsLateWork(t *testing.T) {
	e := newEnv(t)
	source := IngestInput{
		SourceID: "delivery:0f1e2d3c-0001", Owner: testOwner, Repo: testRepo, PRNumber: testPR,
		HeadSHA: headA, Reason: "opened", Delivery: "webhook",
	}
	if _, err := e.svc.IngestCandidate(e.ctx, owl, source); err != nil {
		t.Fatal(err)
	}
	// webhook 由来の cycle の依頼者は owl なので、取消は operator ロールの主体が行う。
	if _, err := e.svc.CancelReview(e.ctx, operator, cancelInput(e, cyc1, "operator-cancel-1")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmCancel(e.ctx, cyc1, cxl1); err != nil {
		t.Fatal(err)
	}

	// 取消確定後に旧 webhook が再送されても、新 round を作らず旧 cycle を返す。
	redelivered, err := e.svc.IngestCandidate(e.ctx, owl, source)
	if err != nil || redelivered.CycleID != cyc1 || !redelivered.Deduplicated || redelivered.Round != 1 {
		t.Fatalf("redelivery = %+v, %v; must resolve to the cancelled cycle", redelivered, err)
	}

	// 旧 candidate の遅い受理・結果・観測は反映しない。
	if err := e.svc.RecordDispatchAccepted(e.ctx, cyc1, cand1); err != nil {
		t.Fatal(err)
	}
	if snap := e.snapshot(cyc1); snap.Dispatch.State != DispatchPending || snap.Dispatch.CandidateID != nil {
		t.Fatalf("a late acceptance must not be applied to a cancelled cycle: %+v", snap.Dispatch)
	}
	_, err = e.svc.PublishResult(e.ctx, owl, resultInput(nil))
	wantCode(t, err, CodeCandidateCancelled)
}

func TestDispatchFailure(t *testing.T) {
	t.Run("retryable failures stay pending until the limit", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		limit := DefaultSettings().Delivery.MaxAttempts
		for i := 1; i < limit; i++ {
			if err := e.svc.RecordDispatchFailure(e.ctx, cyc1, CodeUnavailable, true); err != nil {
				t.Fatal(err)
			}
			if snap := e.snapshot(cyc1); snap.Dispatch.State != DispatchPending || snap.Dispatch.Attempts != i {
				t.Fatalf("attempt %d: %+v", i, snap.Dispatch)
			}
		}
		if err := e.svc.RecordDispatchFailure(e.ctx, cyc1, CodeUnavailable, true); err != nil {
			t.Fatal(err)
		}
		snap := e.snapshot(cyc1)
		if snap.Dispatch.State != DispatchFailed || *snap.Dispatch.ErrorCode != CodeUnavailable || snap.Terminal {
			t.Fatalf("the limit must fail delivery without making it terminal: %+v", snap)
		}
	})
	t.Run("non-retryable failures are final at once", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		if err := e.svc.RecordDispatchFailure(e.ctx, cyc1, CodeOriginRejected, false); err != nil {
			t.Fatal(err)
		}
		snap := e.snapshot(cyc1)
		if snap.Dispatch.State != DispatchFailed || *snap.Dispatch.ErrorCode != CodeOriginRejected || snap.Dispatch.Attempts != 1 {
			t.Fatalf("unexpected dispatch: %+v", snap.Dispatch)
		}
	})
	t.Run("a different candidate cannot replace the accepted one", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		e.accept(cyc1, cand1)
		e.accept(cyc1, cand1) // 同一 candidate の再通知は冪等
		wantCode(t, e.svc.RecordDispatchAccepted(e.ctx, cyc1, cand2), CodeInvalidArgument)
		if snap := e.snapshot(cyc1); snap.Dispatch.Attempts != 1 {
			t.Fatalf("a repeated acceptance must not count as another attempt: %+v", snap.Dispatch)
		}
	})
}

func TestFreshness(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ObservationInput)
		after  time.Duration
		want   string
	}{
		{"fresh right after receipt", nil, 0, FreshnessFresh},
		{"still fresh just under the limit", nil, 3*time.Minute - time.Second, FreshnessFresh},
		{"silent producer becomes stale", nil, 3 * time.Minute, FreshnessStale},
		{"old observation delivered late is not a heartbeat", func(in *ObservationInput) { in.ObservedAt = "2026-10-24T12:55:00Z" }, 0, FreshnessStale},
		{"explicit stop notice", func(in *ObservationInput) { in.Stopped = true }, 0, FreshnessStopped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.request("agent-req-0001", headA, "opened")
			e.accept(cyc1, cand1)
			if got := e.snapshot(cyc1).Execution.Freshness; got != FreshnessUnknown {
				t.Fatalf("freshness before any observation = %s, want unknown", got)
			}
			if _, err := e.svc.PublishObservation(e.ctx, squirrel, observation(tc.mutate)); err != nil {
				t.Fatal(err)
			}
			e.clock.advance(tc.after)
			if got := e.snapshot(cyc1).Execution.Freshness; got != tc.want {
				t.Fatalf("freshness = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	db := fileDB(t)
	first := newEnvOn(t, db)
	first.request("agent-req-0001", headA, "opened")
	first.accept(cyc1, cand1)
	before := first.snapshot(cyc1)

	// 同じ DB に対する新しい Service（プロセス再起動）でも、台帳・冪等性・outbox が残る。
	second := newEnvOn(t, db)
	after := second.snapshot(cyc1)
	if before.SnapshotRevision != after.SnapshotRevision || after.Dispatch.State != DispatchAccepted {
		t.Fatalf("snapshot changed across restart: %+v -> %+v", before, after)
	}
	if again := second.request("agent-req-0001", headA, "opened"); again.CycleID != cyc1 || !again.Deduplicated {
		t.Fatalf("idempotency must survive a restart: %+v", again)
	}
	if n := second.outboxCount(outboxAccept); n != 1 {
		t.Fatalf("outbox lost or duplicated across restart: %d", n)
	}
}

func TestSnapshotRejectsMalformedID(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Snapshot(e.ctx, agent, "not-a-cycle")
	wantCode(t, err, CodeInvalidArgument)
}

// 契約の入力 schema が拒否する形は、副作用の前に INVALID_ARGUMENT で止める。
func TestInputValidation(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.accept(cyc1, cand1)

	call := map[string]func() error{
		"cancel/bad cycle id": func() error {
			_, err := e.svc.CancelReview(e.ctx, agent, CancelReviewInput{CycleID: "x", ExpectedHeadSHA: headA, ExpectedSnapshotRevision: 1, IdempotencyKey: "agent-cancel-0001"})
			return err
		},
		"cancel/short head": func() error {
			_, err := e.svc.CancelReview(e.ctx, agent, CancelReviewInput{CycleID: cyc1, ExpectedHeadSHA: "abc", ExpectedSnapshotRevision: 1, IdempotencyKey: "agent-cancel-0001"})
			return err
		},
		"cancel/revision zero": func() error {
			_, err := e.svc.CancelReview(e.ctx, agent, CancelReviewInput{CycleID: cyc1, ExpectedHeadSHA: headA, ExpectedSnapshotRevision: 0, IdempotencyKey: "agent-cancel-0001"})
			return err
		},
		"cancel/short key": func() error {
			_, err := e.svc.CancelReview(e.ctx, agent, CancelReviewInput{CycleID: cyc1, ExpectedHeadSHA: headA, ExpectedSnapshotRevision: 1, IdempotencyKey: "k"})
			return err
		},
		"ingest/bad source": func() error {
			_, err := e.svc.IngestCandidate(e.ctx, owl, IngestInput{SourceID: "has space", Owner: testOwner, Repo: testRepo, PRNumber: testPR, HeadSHA: headA, Reason: "opened", Delivery: "webhook"})
			return err
		},
		"ingest/bad head": func() error {
			_, err := e.svc.IngestCandidate(e.ctx, owl, IngestInput{SourceID: "delivery:x", Owner: testOwner, Repo: testRepo, PRNumber: testPR, HeadSHA: "abc", Reason: "opened", Delivery: "webhook"})
			return err
		},
		"ingest/bad reason": func() error {
			_, err := e.svc.IngestCandidate(e.ctx, owl, IngestInput{SourceID: "delivery:x", Owner: testOwner, Repo: testRepo, PRNumber: testPR, HeadSHA: headA, Reason: "reopened", Delivery: "webhook"})
			return err
		},
		"ingest/bad owner": func() error {
			_, err := e.svc.IngestCandidate(e.ctx, owl, IngestInput{SourceID: "delivery:x", Owner: "Owner", Repo: testRepo, PRNumber: testPR, HeadSHA: headA, Reason: "opened", Delivery: "webhook"})
			return err
		},
	}
	resultCases := map[string]func(*ResultInput){
		"bad cycle id":     func(in *ResultInput) { in.CycleID = "x" },
		"bad candidate id": func(in *ResultInput) { in.CandidateID = "x" },
		"short head":       func(in *ResultInput) { in.HeadSHA = "abc" },
		"version zero":     func(in *ResultInput) { in.ResultVersion = 0 },
		"kind rejected":    func(in *ResultInput) { in.Kind = "rejected" },
		"no comment id":    func(in *ResultInput) { in.CommentID = 0 },
		"bad postedAt":     func(in *ResultInput) { in.PostedAt = "yesterday" },
	}
	for name, mutate := range resultCases {
		call["result/"+name] = func() error {
			_, err := e.svc.PublishResult(e.ctx, owl, resultInput(mutate))
			return err
		}
	}
	observationCases := map[string]func(*ObservationInput){
		"bad cycle id":              func(in *ObservationInput) { in.CycleID = "x" },
		"bad candidate id":          func(in *ObservationInput) { in.CandidateID = "x" },
		"short head":                func(in *ObservationInput) { in.HeadSHA = "abc" },
		"bad producer id":           func(in *ObservationInput) { in.ProducerID = "Bad Producer" },
		"epoch zero":                func(in *ObservationInput) { in.ProducerEpoch = 0 },
		"negative sequence":         func(in *ObservationInput) { in.Sequence = -1 },
		"bad observedAt":            func(in *ObservationInput) { in.ObservedAt = "now" },
		"unknown state":             func(in *ObservationInput) { in.State = "done" },
		"waiting with exit code":    func(in *ObservationInput) { in.ExitCode = ptr(0) },
		"waiting with outcome":      func(in *ObservationInput) { in.Outcome = ptr("completed") },
		"unknown hold reason":       func(in *ObservationInput) { in.HoldReason = ptr("sleeping") },
		"running with hold reason":  func(in *ObservationInput) { in.State, in.HoldReason = ExecRunning, ptr("busy") },
		"running with outcome":      func(in *ObservationInput) { in.State, in.HoldReason, in.Outcome = ExecRunning, nil, ptr("completed") },
		"finished with hold reason": func(in *ObservationInput) { in.State, in.Outcome = ExecFinished, ptr("completed") },
		"unknown outcome": func(in *ObservationInput) {
			in.State, in.HoldReason, in.Outcome = ExecFinished, nil, ptr("cancelled")
		},
	}
	for name, mutate := range observationCases {
		call["observation/"+name] = func() error {
			_, err := e.svc.PublishObservation(e.ctx, squirrel, observation(mutate))
			return err
		}
	}

	for name, fn := range call {
		t.Run(name, func(t *testing.T) {
			wantCode(t, fn(), CodeInvalidArgument)
		})
	}

	t.Run("finished with an outcome is accepted", func(t *testing.T) {
		in := observation(func(in *ObservationInput) {
			in.State, in.HoldReason, in.Outcome, in.ExitCode, in.Sequence = ExecFinished, nil, ptr("completed"), ptr(0), 7
		})
		out, err := e.svc.PublishObservation(e.ctx, squirrel, in)
		if err != nil {
			t.Fatal(err)
		}
		mustValid(t, "publish_review_observation.output", out)
		snap := e.snapshot(cyc1)
		if snap.Execution.State != ExecFinished || *snap.Execution.Outcome != "completed" || *snap.Execution.ExitCode != 0 || snap.Terminal {
			t.Fatalf("process exit alone must not be terminal: %+v", snap)
		}
	})
}

func TestErrorShape(t *testing.T) {
	cases := []struct {
		code      string
		retryable bool
	}{
		{CodeUnavailable, true},
		{CodeCancelUnconfirmed, true},
		{CodeHeadConflict, false},
		{CodePermissionDenied, false},
		{CodeIdempotencyConflict, false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			err := newError(tc.code, "boom %d", 1)
			if err.Retryable != tc.retryable || err.Error() != tc.code+": boom 1" || ErrorCode(err) != tc.code {
				t.Fatalf("unexpected error shape: %+v", err)
			}
		})
	}
	if ErrorCode(errors.New("plain")) != "" {
		t.Fatal("a non-contract error must have no code")
	}
}
