package reviewrequest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

type notifications struct {
	mu  sync.Mutex
	ids []string
}

func (n *notifications) record(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ids = append(n.ids, id)
}

func (n *notifications) take() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := slices.Clone(n.ids)
	slices.Sort(out)
	n.ids = nil
	return out
}

// 購読者への通知は、保存された状態が変わった cycle にだけ、commit の後に 1 回ずつ送る。
func TestNotifierFiresOncePerChangedCycleAfterCommit(t *testing.T) {
	e := newEnv(t)
	var got notifications
	e.svc.SetNotifier(func(id string) {
		// commit 済みの状態を読めること（transaction の中で呼んでいないこと）を確認する。
		snap, err := e.svc.Snapshot(e.ctx, agent, id)
		if err != nil {
			t.Errorf("snapshot inside the notifier: %v", err)
		} else if snap.SnapshotRevision < 1 {
			t.Errorf("notifier saw an unpublished state: %+v", snap)
		}
		got.record(id)
	})

	steps := []struct {
		name string
		do   func()
		want []string
	}{
		{"a new request", func() { e.request("agent-req-0001", headA, "opened") }, []string{cyc1}},
		{"a repeated request changes nothing", func() { e.request("agent-req-0001", headA, "opened") }, nil},
		{"joining an open request changes nothing", func() { e.request("agent-req-0002", headA, "opened") }, nil},
		{"dispatch accepted", func() { e.accept(cyc1, cand1) }, []string{cyc1}},
		{"a repeated acceptance changes nothing", func() { e.accept(cyc1, cand1) }, nil},
		{"an observation", func() {
			if _, err := e.svc.PublishObservation(e.ctx, squirrel, observation(nil)); err != nil {
				t.Fatal(err)
			}
		}, []string{cyc1}},
		{"a duplicate observation changes nothing", func() {
			if _, err := e.svc.PublishObservation(e.ctx, squirrel, observation(nil)); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"a rejected observation changes nothing", func() {
			_, err := e.svc.PublishObservation(e.ctx, squirrel, observation(func(in *ObservationInput) { in.Sequence = 1 }))
			wantCode(t, err, CodeObservationStale)
		}, nil},
		{"a new head supersedes the old cycle and opens a new one", func() {
			e.heads.set(headB)
			e.request("agent-req-0003", headB, "synchronized")
		}, []string{cyc1, cyc2}},
		{"a result", func() {
			if _, err := e.svc.PublishResult(e.ctx, owl, resultInput(nil)); err != nil {
				t.Fatal(err)
			}
		}, []string{cyc1}},
		{"a cancellation request", func() {
			in := cancelInput(e, cyc2, "agent-cancel-0001")
			in.ExpectedHeadSHA = headB // cyc2 は新しい HEAD の cycle
			cancel(t, e, in)
		}, []string{cyc2}},
		{"the acknowledgement", func() {
			if err := e.svc.ConfirmCancel(e.ctx, cyc2, cxl1); err != nil {
				t.Fatal(err)
			}
		}, []string{cyc2}},
		{"purging is not a state change", func() {
			e.clock.advance(100 * day)
			if _, err := e.svc.PurgeExpired(e.ctx); err != nil {
				t.Fatal(err)
			}
		}, nil},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			got.take()
			tc.do()
			if have := got.take(); !slices.Equal(have, tc.want) {
				t.Fatalf("notified %v, want %v", have, tc.want)
			}
		})
	}
}

func TestNotifierIsOptional(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened") // 通知先が無くても失敗しない
	e.svc.SetNotifier(nil)
	e.accept(cyc1, cand1)
}

// 存在しない PR と installation 対象外の PR は区別せず PERMISSION_DENIED にする。
func TestInaccessiblePullRequestIsPermissionDenied(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not accessible", fmt.Errorf("%w: 404", ErrPullRequestNotAccessible), CodePermissionDenied},
		{"transient failure", errors.New("connection reset"), CodeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.heads.err = tc.err
			defer func() { e.heads.err = nil }()
			wantCode(t, e.requestErr("agent-req-0001", headA, "opened"), tc.want)
		})
	}
	if n := e.outboxCount(outboxAccept); n != 0 {
		t.Fatalf("a rejected request queued %d messages", n)
	}
}

// 失敗はすべて契約のエラー envelope として返せる。
func TestErrorEnvelopeMatchesContract(t *testing.T) {
	codes := []string{
		CodeInvalidArgument, CodePermissionDenied, CodeRequestNotFound, CodeRequestExpired,
		CodeHeadConflict, CodeIdempotencyConflict, CodeRevisionConflict, CodeCancelResultPosted,
		CodeCancelUnconfirmed, CodeCandidateCancelled, CodeOriginRejected, CodeDeliveryExpired,
		CodeEpochStale, CodeSequenceConflict, CodeObservationStale, CodeProducerNotAssigned,
		CodeResultVersionStale, CodeResultHeadMismatch, CodeUnavailable,
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			err := newError(code, "reason %d", 1)
			env := err.Envelope()
			mustValid(t, "error", env)
			if env.Error.Code != code || env.Error.Retryable != err.Retryable {
				t.Fatalf("unexpected envelope: %+v", env)
			}
			if got, ok := AsError(err); !ok || got.Code != code {
				t.Fatalf("AsError() = %v, %v", got, ok)
			}
		})
	}

	t.Run("details are carried", func(t *testing.T) {
		err := newError(CodeUnavailable, "owl did not acknowledge")
		err.Details = map[string]any{"attempts": 3}
		mustValid(t, "error", err.Envelope())
	})
	t.Run("a plain error is not a contract error", func(t *testing.T) {
		if _, ok := AsError(errors.New("boom")); ok {
			t.Fatal("a plain error must not convert")
		}
	})
}
