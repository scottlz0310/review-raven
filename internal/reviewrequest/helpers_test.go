package reviewrequest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/scottlz0310/review-raven/internal/contracts"
)

// Fixed identifiers shared with docs/design/contracts (fixtures and scenarios).
const (
	testOwner = "scottlz0310"
	testRepo  = "mcp-resource-subscriber"
	testPR    = 226
	headA     = "0123456789abcdef0123456789abcdef01234567"
	headB     = "89abcdef0123456789abcdef0123456789abcdef"
	cand1     = "cand_01jbq3x8n5r7t9v2w4y6z8a0d1"
	cand2     = "cand_01jbq3x8n5r7t9v2w4y6z8a0d2"
	postURL   = "https://github.com/scottlz0310/mcp-resource-subscriber/pull/226#issuecomment-6384700041"
	ts        = "2026-10-24T13:00:00Z"
)

// Principals.
const (
	agent    = "agent-a"      // requester
	operator = "operator-b"   // allowed to cancel other users' requests
	owl      = "svc-owl"      // reviewer side
	squirrel = "svc-squirrel" // orchestrator
	stranger = "stranger"     // not in any ACL
)

type fakeAuth struct {
	allowed   map[string]bool
	operators map[string]bool
}

func (f fakeAuth) Authorize(_ context.Context, principal, action, _, _, requestedBy string) bool {
	if !f.allowed[principal] {
		return false
	}
	if action == ActionCancel {
		return principal == requestedBy || f.operators[principal]
	}
	return true
}

type fakeHeads struct {
	mu   sync.Mutex
	head map[string]string
	err  error
}

func (f *fakeHeads) CurrentHead(_ context.Context, owner, repo string, pr int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.head[fmt.Sprintf("%s/%s#%d", owner, repo, pr)], nil
}

func (f *fakeHeads) set(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head[fmt.Sprintf("%s/%s#%d", testOwner, testRepo, testPR)] = sha
}

type fakeProducers map[string]string // principal -> producerID it may publish as

func (f fakeProducers) MayPublish(_ context.Context, principal, producerID string) bool {
	return f[principal] == producerID
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t     *testing.T
	svc   *Service
	db    *sql.DB
	clock *fakeClock
	heads *fakeHeads
	ctx   context.Context
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// sequentialIDs yields the contract fixture identifiers: cyc_..c1, c2, ... and cxl_..e1, e2, ...
func sequentialIDs() func(prefix string) string {
	var mu sync.Mutex
	counts := map[string]int{}
	suffix := map[string]string{prefixCycle: "c", prefixCancel: "e"}
	return func(prefix string) string {
		mu.Lock()
		defer mu.Unlock()
		counts[prefix]++
		return fmt.Sprintf("%s_01jbq3x8n5r7t9v2w4y6z8a0%s%d", prefix, suffix[prefix], counts[prefix])
	}
}

func newEnv(t *testing.T, opts ...func(*Deps)) *env {
	t.Helper()
	return newEnvOn(t, openDB(t, ":memory:"), opts...)
}

// newEnvOn builds a Service on db. opts adjust the dependencies, for example
// custom retention settings or the real ID generator for a simulated restart.
func newEnvOn(t *testing.T, db *sql.DB, opts ...func(*Deps)) *env {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)}
	heads := &fakeHeads{head: map[string]string{}}
	heads.set(headA)

	deps := Deps{
		Authorizer: fakeAuth{
			allowed:   map[string]bool{agent: true, operator: true, owl: true, squirrel: true},
			operators: map[string]bool{operator: true},
		},
		Heads:     heads,
		Producers: fakeProducers{squirrel: "squirrel-notifier"},
		Settings:  DefaultSettings(),
		Now:       clock.Now,
		NewID:     sequentialIDs(),
	}
	for _, opt := range opts {
		opt(&deps)
	}
	svc, err := NewService(context.Background(), db, deps)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &env{t: t, svc: svc, db: db, clock: clock, heads: heads, ctx: context.Background()}
}

func fileDB(t *testing.T) *sql.DB {
	return openDB(t, filepath.Join(t.TempDir(), "ledger.db"))
}

// mustValid fails the test when v does not satisfy the named contract schema.
func mustValid(t *testing.T, schema string, v any) {
	t.Helper()
	if err := contracts.Validate(schema, v); err != nil {
		t.Fatalf("value violates contract schema %s: %v", schema, err)
	}
}

func (e *env) request(key, head, reason string) RequestReviewOutput {
	e.t.Helper()
	out, err := e.svc.RequestReview(e.ctx, agent, RequestReviewInput{
		Owner: testOwner, Repo: testRepo, PRNumber: testPR,
		ExpectedHeadSHA: head, Reason: reason, IdempotencyKey: key,
	})
	if err != nil {
		e.t.Fatalf("RequestReview(%s): %v", key, err)
	}
	mustValid(e.t, "request_review.output", out)
	return out
}

func (e *env) requestErr(key, head, reason string) error {
	e.t.Helper()
	_, err := e.svc.RequestReview(e.ctx, agent, RequestReviewInput{
		Owner: testOwner, Repo: testRepo, PRNumber: testPR,
		ExpectedHeadSHA: head, Reason: reason, IdempotencyKey: key,
	})
	return err
}

func (e *env) snapshot(cycleID string) Snapshot {
	e.t.Helper()
	snap, err := e.svc.Snapshot(e.ctx, agent, cycleID)
	if err != nil {
		e.t.Fatalf("Snapshot(%s): %v", cycleID, err)
	}
	mustValid(e.t, "snapshot", snap)
	return snap
}

func (e *env) accept(cycleID, candidateID string) {
	e.t.Helper()
	if err := e.svc.RecordDispatchAccepted(e.ctx, cycleID, candidateID); err != nil {
		e.t.Fatalf("RecordDispatchAccepted: %v", err)
	}
}

func (e *env) outboxCount(kind string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM request_outbox WHERE kind = ?`, kind).Scan(&n); err != nil {
		e.t.Fatalf("count outbox: %v", err)
	}
	return n
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := ErrorCode(err); got != code {
		t.Fatalf("error code = %q (err=%v), want %q", got, err, code)
	}
}

func ptr[T any](v T) *T { return &v }

func observation(over func(*ObservationInput)) ObservationInput {
	in := ObservationInput{
		CycleID: "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1", CandidateID: cand1, HeadSHA: headA,
		ProducerID: "squirrel-notifier", ProducerEpoch: 1, Sequence: 3, ObservedAt: ts,
		State: ExecWaiting, HoldReason: ptr("ciPending"),
	}
	if over != nil {
		over(&in)
	}
	return in
}

func resultInput(over func(*ResultInput)) ResultInput {
	in := ResultInput{
		CycleID: "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1", CandidateID: cand1, HeadSHA: headA,
		ResultVersion: 1, Kind: "approved", CommentID: 6384700041, URL: postURL, PostedAt: ts,
	}
	if over != nil {
		over(&in)
	}
	return in
}

const (
	cyc1 = "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1"
	cyc2 = "cyc_01jbq3x8n5r7t9v2w4y6z8a0c2"
	cxl1 = "cxl_01jbq3x8n5r7t9v2w4y6z8a0e1"
)
