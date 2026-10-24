package reviewrequest

import "testing"

// 取消要求中（無効化 ACK 前）は、reason が違っても同じ HEAD の再依頼を既存 cycle に結合し、
// GitHub 書込み結果が不明な旧候補と新候補を並行させない。
func TestRequestDuringCancellationJoinsRegardlessOfReason(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	if err := e.svc.RecordCancelRejected(e.ctx, cyc1, cxl1, CodeCancelUnconfirmed); err != nil {
		t.Fatal(err)
	}

	for _, reason := range []string{"re-review-requested", "synchronized", "opened"} {
		t.Run(reason, func(t *testing.T) {
			out := e.request("agent-req-"+reason, headA, reason)
			if out.CycleID != cyc1 || out.Round != 1 || !out.Deduplicated {
				t.Fatalf("a request during cancellation must join the existing cycle: %+v", out)
			}
		})
	}

	snap := e.snapshot(cyc1)
	if snap.Terminal || snap.Cancellation.State != CancelRequested || *snap.Cancellation.ErrorCode != CodeCancelUnconfirmed {
		t.Fatalf("the cancelling cycle must be untouched: %+v", snap)
	}
	if n := e.outboxCount(outboxAccept); n != 1 {
		t.Fatalf("accept messages = %d, want 1 (no parallel candidate)", n)
	}

	// 新しい HEAD は別の対象なので、取消中でも新 cycle になり、旧 cycle は superseded になる。
	e.heads.set(headB)
	if next := e.request("agent-req-newhead", headB, "synchronized"); next.CycleID != cyc2 || next.Round != 2 {
		t.Fatalf("a new head opens a new round: %+v", next)
	}
	if old := e.snapshot(cyc1); *old.TerminalReason != TerminalSuperseded {
		t.Fatalf("the cancelling cycle is superseded by the new head: %+v", old)
	}
}

// round は PR 単位の永続単調増加値で、cycle の削除・再起動をまたいでも再利用しない。
func TestRoundNeverReusedAfterPurgeOrRestart(t *testing.T) {
	db := fileDB(t)
	e := newEnvOn(t, db)
	e.request("agent-req-0001", headA, "opened")
	cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
	if err := e.svc.ConfirmCancel(e.ctx, cyc1, cxl1); err != nil {
		t.Fatal(err)
	}

	e.clock.advance(31 * day)
	if n, err := e.svc.PurgeExpired(e.ctx); err != nil || n != 1 {
		t.Fatalf("PurgeExpired() = %d, %v; want 1", n, err)
	}
	if got := e.request("agent-req-0002", headA, "opened"); got.Round != 2 || got.CycleID != cyc2 {
		t.Fatalf("round after purging every cycle = %+v, want round 2", got)
	}

	// 再起動後（実 ID 生成器）も、以前の最大 round を超える。
	restarted := newEnvOn(t, db, func(d *Deps) { d.NewID = NewID })
	restarted.clock.now = e.clock.Now()
	restarted.heads.set(headB)
	if third := restarted.request("agent-req-0003", headB, "synchronized"); third.Round != 3 {
		t.Fatalf("round after restart = %d, want 3", third.Round)
	}
}

// 冪等性・取消記録の保持期間は cycle の保持期間と独立に守る。
func TestRetentionPeriodsAreIndependent(t *testing.T) {
	longRetention := func(d *Deps) {
		d.Settings.IdempotencyRetention = 60 * day
		d.Settings.CancellationRecordRetention = 60 * day
	}

	t.Run("a cancelled request outlives the finished retention", func(t *testing.T) {
		e := newEnv(t, longRetention)
		e.request("agent-req-0001", headA, "opened")
		cancel(t, e, cancelInput(e, cyc1, "agent-cancel-0001"))
		if err := e.svc.ConfirmCancel(e.ctx, cyc1, cxl1); err != nil {
			t.Fatal(err)
		}

		e.clock.advance(31 * day)
		if n, err := e.svc.PurgeExpired(e.ctx); err != nil || n != 0 {
			t.Fatalf("PurgeExpired() = %d, %v; a cancellation record must be kept for its own retention", n, err)
		}
		// 旧 key の再送は、新 cycle ではなく取消済みの旧 cycle を返す。
		if old := e.request("agent-req-0001", headA, "opened"); old.CycleID != cyc1 || !old.Deduplicated {
			t.Fatalf("replay of the cancelled request's key = %+v", old)
		}
		if n := e.outboxCount(outboxAccept); n != 1 {
			t.Fatalf("accept messages = %d, want 1", n)
		}

		e.clock.advance(30 * day) // 合計 61 日
		if n, err := e.svc.PurgeExpired(e.ctx); err != nil || n != 1 {
			t.Fatalf("PurgeExpired() after the cancellation retention = %d, %v; want 1", n, err)
		}
	})

	t.Run("a purged request never turns a replayed key into a new cycle", func(t *testing.T) {
		e := newEnv(t, longRetention)
		e.request("agent-req-0001", headA, "opened")
		e.heads.set(headB)
		e.request("agent-req-0002", headB, "synchronized") // cyc1 は superseded（取消記録なし）

		e.clock.advance(31 * day)
		if n, err := e.svc.PurgeExpired(e.ctx); err != nil || n != 1 {
			t.Fatalf("PurgeExpired() = %d, %v; want the superseded cycle", n, err)
		}
		wantCode(t, e.requestErr("agent-req-0001", headA, "opened"), CodeRequestExpired)
		// purge は cyc1 の outbox 行を消すので残りは cyc2 の 1 件。再送が新しい cycle を作れば 2 件になる。
		if n := e.outboxCount(outboxAccept); n != 1 {
			t.Fatalf("accept messages = %d, want 1 (the replay must not open a cycle)", n)
		}
	})

	t.Run("a replayed webhook source after purge is an expired delivery", func(t *testing.T) {
		e := newEnv(t, longRetention)
		source := IngestInput{
			SourceID: "delivery:0f1e2d3c-0001", Owner: testOwner, Repo: testRepo, PRNumber: testPR,
			HeadSHA: headA, Reason: "opened", Delivery: "webhook",
		}
		if _, err := e.svc.IngestCandidate(e.ctx, owl, source); err != nil {
			t.Fatal(err)
		}
		e.heads.set(headB)
		e.request("agent-req-0002", headB, "synchronized")

		e.clock.advance(31 * day)
		if _, err := e.svc.PurgeExpired(e.ctx); err != nil {
			t.Fatal(err)
		}
		_, err := e.svc.IngestCandidate(e.ctx, owl, source)
		wantCode(t, err, CodeDeliveryExpired)
	})

	t.Run("keys and sources are dropped after their own retention", func(t *testing.T) {
		e := newEnv(t)
		e.request("agent-req-0001", headA, "opened")
		e.clock.advance(31 * day)
		if _, err := e.svc.PurgeExpired(e.ctx); err != nil {
			t.Fatal(err)
		}
		var keys int
		if err := e.db.QueryRow(`SELECT COUNT(*) FROM request_keys`).Scan(&keys); err != nil || keys != 0 {
			t.Fatalf("idempotency keys after their retention = %d, %v; want 0", keys, err)
		}
	})
}

// 内部面（owl / Squirrel）から削除済みの依頼へ届く遅延配送は、410 相当の DELIVERY_EXPIRED で停止する。
func TestDeliveryToPurgedRequestIsExpired(t *testing.T) {
	e := newEnv(t)
	e.request("agent-req-0001", headA, "opened")
	e.heads.set(headB)
	e.request("agent-req-0002", headB, "synchronized")
	e.clock.advance(31 * day)
	if _, err := e.svc.PurgeExpired(e.ctx); err != nil {
		t.Fatal(err)
	}

	_, err := e.svc.PublishResult(e.ctx, owl, resultInput(nil))
	wantCode(t, err, CodeDeliveryExpired)
	_, err = e.svc.PublishObservation(e.ctx, squirrel, observation(nil))
	wantCode(t, err, CodeDeliveryExpired)

	// 公開面の読み取りは従来どおり REQUEST_EXPIRED。
	_, err = e.svc.Snapshot(e.ctx, agent, cyc1)
	wantCode(t, err, CodeRequestExpired)
}
