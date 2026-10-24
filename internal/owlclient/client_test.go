package owlclient

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"

	"github.com/scottlz0310/review-raven/internal/contracts"
	"github.com/scottlz0310/review-raven/internal/reviewrequest"
)

const (
	testToken = "service-credential-0123456789abcdef"
	cand1     = "cand_01jbq3x8n5r7t9v2w4y6z8a0d1"
	headA     = "0123456789abcdef0123456789abcdef01234567"
)

// fakeOwl is thread-owl's internal surface: it serves accept_review_candidate
// and cancel_review_candidate, validates every received input against the
// contract schema, and answers as scripted.
type fakeOwl struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	tokens   []string
	accepts  []map[string]any
	cancels  []map[string]any
	onAccept func(in map[string]any) (any, *reviewrequest.Error)
	onCancel func(in map[string]any) (any, *reviewrequest.Error)
	status   int // non-zero: answer every request with this HTTP status
}

func newFakeOwl(t *testing.T) *fakeOwl {
	t.Helper()
	f := &fakeOwl{t: t}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-owl", Version: "1"}, nil)
	register := func(name, schema string, handle func(in map[string]any) (any, *reviewrequest.Error), record func(map[string]any)) {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "fake " + name},
			func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
				if err := contracts.Validate(schema, in); err != nil {
					t.Errorf("%s received an input that violates %s: %v", name, schema, err)
				}
				f.mu.Lock()
				record(in)
				f.mu.Unlock()
				if handle == nil {
					return nil, nil, errors.New("not scripted")
				}
				out, failure := handle(in)
				if failure != nil {
					return &mcp.CallToolResult{IsError: true}, failure.Envelope(), nil
				}
				return nil, out, nil
			})
	}
	register("accept_review_candidate", "accept_review_candidate.input",
		func(in map[string]any) (any, *reviewrequest.Error) { return f.onAccept(in) },
		func(in map[string]any) { f.accepts = append(f.accepts, in) })
	register("cancel_review_candidate", "cancel_review_candidate.input",
		func(in map[string]any) (any, *reviewrequest.Error) { return f.onCancel(in) },
		func(in map[string]any) { f.cancels = append(f.cancels, in) })

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true})
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokens = append(f.tokens, r.Header.Get("Authorization"))
		status := f.status
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOwl) client() *Client {
	f.t.Helper()
	c, err := New(Config{Endpoint: f.server.URL, Token: testToken})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func acceptMessage() reviewrequest.AcceptMessage {
	return reviewrequest.AcceptMessage{
		SchemaVersion: 1, CycleID: "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1", Round: 1,
		Owner: "scottlz0310", Repo: "mcp-resource-subscriber", PRNumber: 226,
		HeadSHA: headA, Reason: "opened",
	}
}

func cancelMessage() reviewrequest.CancelMessage {
	return reviewrequest.CancelMessage{
		SchemaVersion: 1, CycleID: "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1", Round: 1,
		HeadSHA: headA, CancelID: "cxl_01jbq3x8n5r7t9v2w4y6z8a0e1",
	}
}

func failure(code string) *reviewrequest.Error {
	return &reviewrequest.Error{Code: code, Message: "scripted " + code, Retryable: code == reviewrequest.CodeUnavailable || code == reviewrequest.CodeCancelUnconfirmed}
}

func wantContractError(t *testing.T, err error, code string, retryable bool) {
	t.Helper()
	var contractErr *reviewrequest.Error
	if !errors.As(err, &contractErr) {
		t.Fatalf("error = %v (%T), want a contract error", err, err)
	}
	if contractErr.Code != code || contractErr.Retryable != retryable {
		t.Fatalf("error = {%s retryable=%v}, want {%s retryable=%v}: %v", contractErr.Code, contractErr.Retryable, code, retryable, err)
	}
	if strings.Contains(contractErr.Message, testToken) {
		t.Fatalf("the credential leaked into the error: %q", contractErr.Message)
	}
}

func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		plain bool
		ok    bool
	}{
		{"https", "https://gateway.example/mcp/thread-owl-internal", false, true},
		{"loopback http", "http://127.0.0.1:8084/mcp", false, true},
		{"localhost http", "http://localhost:8084/mcp", false, true},
		{"ipv6 loopback http", "http://[::1]:8084/mcp", false, true},
		{"plain http to another host", "http://gateway.example/mcp", false, false},
		{"plain http to a service name", "http://mcp-gateway:8080/mcp/thread-owl-internal", false, false},
		{"plain http to a service name, opted in", "http://mcp-gateway:8080/mcp/thread-owl-internal", true, true},
		{"unsupported scheme", "ftp://gateway.example/mcp", false, false},
		{"unsupported scheme, opted in", "ftp://mcp-gateway/mcp", true, false},
		{"embedded credentials", "https://user:pass@gateway.example/mcp", false, false},
		{"embedded credentials, opted in", "http://user:pass@mcp-gateway:8080/mcp", true, false},
		{"query string", "https://gateway.example/mcp?token=x", false, false},
		{"query string, opted in", "http://mcp-gateway:8080/mcp?token=x", true, false},
		{"fragment", "https://gateway.example/mcp#x", false, false},
		{"no host", "https:///mcp", false, false},
		{"empty", "", false, false},
		{"empty, opted in", "", true, false},
		{"not a url", "::::", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateEndpoint(tc.raw, tc.plain); (err == nil) != tc.ok {
				t.Fatalf("ValidateEndpoint(%q, plain=%v) error = %v, want ok=%v", tc.raw, tc.plain, err, tc.ok)
			}
		})
	}
}

func TestNewRequiresACredential(t *testing.T) {
	if _, err := New(Config{Endpoint: "https://gateway.example/mcp", Token: "  "}); err == nil {
		t.Fatal("a missing credential must be rejected")
	}
	if _, err := New(Config{Endpoint: "http://gateway.example/mcp", Token: testToken}); err == nil {
		t.Fatal("a plain http endpoint must be rejected")
	}
}

func TestAccept(t *testing.T) {
	owl := newFakeOwl(t)
	owl.onAccept = func(map[string]any) (any, *reviewrequest.Error) {
		return map[string]any{"schemaVersion": 1, "candidateId": cand1, "duplicate": false}, nil
	}
	result, err := owl.client().Accept(context.Background(), acceptMessage())
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if result.CandidateID != cand1 || result.Duplicate {
		t.Fatalf("unexpected result: %+v", result)
	}
	// 送った入力は契約に適合する(fake が検証済み)。資格情報は Bearer で送る。
	if got := owl.accepts[0]["cycleId"]; got != "cyc_01jbq3x8n5r7t9v2w4y6z8a0c1" || owl.accepts[0]["sourceId"] != nil {
		t.Fatalf("unexpected input: %+v", owl.accepts[0])
	}
	for _, header := range owl.tokens {
		if header != "Bearer "+testToken {
			t.Fatalf("Authorization = %q on every request", header)
		}
	}
}

func TestAcceptDuplicateIsIdempotent(t *testing.T) {
	owl := newFakeOwl(t)
	owl.onAccept = func(map[string]any) (any, *reviewrequest.Error) {
		return map[string]any{"schemaVersion": 1, "candidateId": cand1, "duplicate": true}, nil
	}
	result, err := owl.client().Accept(context.Background(), acceptMessage())
	if err != nil || !result.Duplicate || result.CandidateID != cand1 {
		t.Fatalf("Accept() = %+v, %v", result, err)
	}
}

func TestAcceptCarriesTheWebhookSource(t *testing.T) {
	owl := newFakeOwl(t)
	owl.onAccept = func(map[string]any) (any, *reviewrequest.Error) {
		return map[string]any{"schemaVersion": 1, "candidateId": cand1, "duplicate": false}, nil
	}
	msg := acceptMessage()
	source := "delivery:0f1e2d3c-0001"
	msg.SourceID = &source
	if _, err := owl.client().Accept(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if owl.accepts[0]["sourceId"] != source {
		t.Fatalf("sourceId = %v", owl.accepts[0]["sourceId"])
	}
}

func TestCancel(t *testing.T) {
	owl := newFakeOwl(t)
	owl.onCancel = func(in map[string]any) (any, *reviewrequest.Error) {
		return map[string]any{"schemaVersion": 1, "cancelId": in["cancelId"], "acknowledged": true, "candidateStatus": "queued", "githubWriteReconciled": true}, nil
	}
	result, err := owl.client().Cancel(context.Background(), cancelMessage())
	if err != nil || result.CancelID != cancelMessage().CancelID || result.CandidateStatus != "queued" {
		t.Fatalf("Cancel() = %+v, %v", result, err)
	}
}

// thread-owl が答えた契約のエラーは、code と再送可否をそのまま保つ。
func TestPeerErrorsKeepTheirContractCode(t *testing.T) {
	cases := []struct {
		code      string
		retryable bool
	}{
		{reviewrequest.CodeOriginRejected, false},
		{reviewrequest.CodeCandidateCancelled, false},
		{reviewrequest.CodeHeadConflict, false},
		{reviewrequest.CodePermissionDenied, false},
		{reviewrequest.CodeUnavailable, true},
	}
	for _, tc := range cases {
		t.Run("accept/"+tc.code, func(t *testing.T) {
			owl := newFakeOwl(t)
			owl.onAccept = func(map[string]any) (any, *reviewrequest.Error) { return nil, failure(tc.code) }
			_, err := owl.client().Accept(context.Background(), acceptMessage())
			wantContractError(t, err, tc.code, tc.retryable)
		})
	}
	cancelCases := []struct {
		code      string
		retryable bool
	}{
		{reviewrequest.CodeCancelResultPosted, false},
		{reviewrequest.CodeCancelUnconfirmed, true},
		{reviewrequest.CodeUnavailable, true},
		{reviewrequest.CodePermissionDenied, false},
	}
	for _, tc := range cancelCases {
		t.Run("cancel/"+tc.code, func(t *testing.T) {
			owl := newFakeOwl(t)
			owl.onCancel = func(map[string]any) (any, *reviewrequest.Error) { return nil, failure(tc.code) }
			_, err := owl.client().Cancel(context.Background(), cancelMessage())
			wantContractError(t, err, tc.code, tc.retryable)
		})
	}
}

// 資格情報の拒否や不正なリクエストは最終、5xx・429・接続断は ACK 未受信として再送対象。
func TestTransportFailures(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      string
		retryable bool
	}{
		{"refused credential", http.StatusUnauthorized, reviewrequest.CodePermissionDenied, false},
		{"forbidden", http.StatusForbidden, reviewrequest.CodePermissionDenied, false},
		{"unknown route", http.StatusNotFound, reviewrequest.CodeInvalidArgument, false},
		{"bad request", http.StatusBadRequest, reviewrequest.CodeInvalidArgument, false},
		{"rate limited", http.StatusTooManyRequests, reviewrequest.CodeUnavailable, true},
		{"server error", http.StatusInternalServerError, reviewrequest.CodeUnavailable, true},
		{"bad gateway", http.StatusBadGateway, reviewrequest.CodeUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owl := newFakeOwl(t)
			owl.status = tc.status
			_, err := owl.client().Accept(context.Background(), acceptMessage())
			wantContractError(t, err, tc.code, tc.retryable)
		})
	}

	t.Run("connection refused", func(t *testing.T) {
		owl := newFakeOwl(t)
		client := owl.client()
		owl.server.Close()
		_, err := client.Accept(context.Background(), acceptMessage())
		wantContractError(t, err, reviewrequest.CodeUnavailable, true)
	})
	t.Run("a stalled peer is bounded by the context", func(t *testing.T) {
		owl := newFakeOwl(t)
		owl.onAccept = func(map[string]any) (any, *reviewrequest.Error) {
			time.Sleep(300 * time.Millisecond)
			return map[string]any{"schemaVersion": 1, "candidateId": cand1, "duplicate": false}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := owl.client().Accept(ctx, acceptMessage())
		wantContractError(t, err, reviewrequest.CodeUnavailable, true)
	})
}

// 契約に沿わない応答は、確定として扱わない（ACK 未受信と同じ扱いで、試行上限で止まる）。
func TestMisbehavingPeerIsNeverTreatedAsAnAcknowledgement(t *testing.T) {
	cases := []struct {
		name   string
		accept any
		cancel any
	}{
		{"no candidate id", map[string]any{"schemaVersion": 1, "duplicate": false}, nil},
		{"not acknowledged", nil, map[string]any{"schemaVersion": 1, "cancelId": "cxl_01jbq3x8n5r7t9v2w4y6z8a0e1", "acknowledged": false, "candidateStatus": "queued", "githubWriteReconciled": true}},
		{"a different cancel id", nil, map[string]any{"schemaVersion": 1, "cancelId": "cxl_01jbq3x8n5r7t9v2w4y6z8a0e9", "acknowledged": true, "candidateStatus": "queued", "githubWriteReconciled": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owl := newFakeOwl(t)
			owl.onAccept = func(map[string]any) (any, *reviewrequest.Error) { return tc.accept, nil }
			owl.onCancel = func(map[string]any) (any, *reviewrequest.Error) { return tc.cancel, nil }
			var err error
			if tc.accept != nil {
				_, err = owl.client().Accept(context.Background(), acceptMessage())
			} else {
				_, err = owl.client().Cancel(context.Background(), cancelMessage())
			}
			wantContractError(t, err, reviewrequest.CodeUnavailable, true)
		})
	}
}

func TestErrorResultThatIsNotAContractErrorIsUnavailable(t *testing.T) {
	// 契約の envelope ではないエラー結果(code がない)。
	srv := mcp.NewServer(&mcp.Implementation{Name: "odd-owl", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "accept_review_candidate", Description: "odd"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"message": "boom"}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true})
	odd := httptest.NewServer(handler)
	t.Cleanup(odd.Close)

	client, err := New(Config{Endpoint: odd.URL, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Accept(context.Background(), acceptMessage())
	wantContractError(t, err, reviewrequest.CodeUnavailable, true)
}

// 実際の配送 worker と台帳を、このクライアントとレビュー側の fake に接続する。
func TestDeliversThroughTheLedgerWorker(t *testing.T) {
	owl := newFakeOwl(t)
	owl.onAccept = func(map[string]any) (any, *reviewrequest.Error) {
		return map[string]any{"schemaVersion": 1, "candidateId": cand1, "duplicate": false}, nil
	}
	owl.onCancel = func(in map[string]any) (any, *reviewrequest.Error) {
		return map[string]any{"schemaVersion": 1, "cancelId": in["cancelId"], "acknowledged": true, "candidateStatus": "queued", "githubWriteReconciled": true}, nil
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	users, err := reviewrequest.ParseACL(`{"users":{"alice":["scottlz0310/*"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := reviewrequest.NewService(context.Background(), db, reviewrequest.Deps{
		Authorizer: users,
		Heads:      fixedHead(headA),
		Producers:  reviewrequest.DenyAllProducers{},
		Settings:   reviewrequest.DefaultSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := svc.RequestReview(context.Background(), "alice", reviewrequest.RequestReviewInput{
		Owner: "scottlz0310", Repo: "mcp-resource-subscriber", PRNumber: 226,
		ExpectedHeadSHA: headA, Reason: "opened", IdempotencyKey: "agent-req-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverDue(context.Background(), owl.client(), 10); err != nil {
		t.Fatalf("DeliverDue: %v", err)
	}
	snap, err := svc.Snapshot(context.Background(), "alice", out.CycleID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Dispatch.State != reviewrequest.DispatchAccepted || snap.Dispatch.CandidateID == nil || *snap.Dispatch.CandidateID != cand1 {
		t.Fatalf("the candidate must be recorded as accepted: %+v", snap.Dispatch)
	}
	raw, _ := json.Marshal(snap)
	if err := contracts.Validate("snapshot", json.RawMessage(raw)); err != nil {
		t.Fatalf("snapshot violates the contract: %v", err)
	}

	// 取消: ACK で確定する。
	cancelOut, err := svc.CancelReview(context.Background(), "alice", reviewrequest.CancelReviewInput{
		CycleID: out.CycleID, ExpectedHeadSHA: headA, ExpectedSnapshotRevision: snap.SnapshotRevision, IdempotencyKey: "agent-cancel-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverDue(context.Background(), owl.client(), 10); err != nil {
		t.Fatalf("DeliverDue (cancel): %v", err)
	}
	if got, err := svc.Snapshot(context.Background(), "alice", out.CycleID); err != nil || !got.Terminal || got.Cancellation.State != reviewrequest.CancelConfirmed || got.Cancellation.CancelID == nil || *got.Cancellation.CancelID != cancelOut.CancelID {
		t.Fatalf("the acknowledgement must confirm the cancellation: %+v, %v", got, err)
	}
}

type fixedHead string

func (h fixedHead) CurrentHead(context.Context, string, string, int) (string, error) {
	return string(h), nil
}

// 旧実行の GitHub 書込みが未確定(false/欠落)の ACK では取消を確定せず、同じ取消 ID で再送する。
func TestCancelWithoutReconciledGitHubWriteStaysUnconfirmed(t *testing.T) {
	cases := []struct {
		name       string
		reconciled any // nil: field omitted
	}{
		{"not reconciled", false},
		{"omitted", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owl := newFakeOwl(t)
			owl.onCancel = func(in map[string]any) (any, *reviewrequest.Error) {
				out := map[string]any{"schemaVersion": 1, "cancelId": in["cancelId"], "acknowledged": true, "candidateStatus": "started"}
				if tc.reconciled != nil {
					out["githubWriteReconciled"] = tc.reconciled
				}
				return out, nil
			}
			_, err := owl.client().Cancel(context.Background(), cancelMessage())
			wantContractError(t, err, reviewrequest.CodeCancelUnconfirmed, true)
		})
	}
}

// リダイレクトは追わない: 転送先に資格情報を送らない(別ホストへの転送、https から http への降格)。
func TestRedirectIsNeverFollowedWithTheCredential(t *testing.T) {
	var mu sync.Mutex
	var reached []string
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = append(reached, r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	client, err := New(Config{Endpoint: origin.URL, Token: testToken, HTTPClient: origin.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Accept(context.Background(), acceptMessage())
	wantContractError(t, err, reviewrequest.CodeUnavailable, true)

	mu.Lock()
	defer mu.Unlock()
	if len(reached) != 0 {
		t.Fatalf("the redirect target received %d request(s) (Authorization %q)", len(reached), reached)
	}
}
