package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/middleware"
	"github.com/scottlz0310/review-raven/internal/store"
	"github.com/scottlz0310/review-raven/internal/watch"
)

const (
	// testMcpSessionIDHeader mirrors the SDK's session header name for tests
	// that need to assert on its (non-)presence in stateless mode.
	testMcpSessionIDHeader = "Mcp-Session-Id"

	// testProtocolVersion20260728 is the protocol revision this server adopts.
	// go-sdk negotiates it on Streamable HTTP only when Stateless is true.
	testProtocolVersion20260728 = "2026-07-28"

	// testNewProtocolMeta is the per-request _meta block that SEP-2575 requires
	// on every 2026-07-28 request, replacing the initialize handshake.
	testNewProtocolMeta = `"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"` + testProtocolVersion20260728 + `",` +
		`"io.modelcontextprotocol/clientCapabilities":{},` +
		`"io.modelcontextprotocol/clientInfo":{"name":"test-client","version":"1.0.0"}}`
)

func TestStreamableHandlerCloseClosesWatchManager(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "server-test.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})

	handler := BuildStreamableHandler(db, 30*time.Second)
	handler.Close()
	handler.Close()

	_, _, err = handler.watchManager.Start(watch.StartInput{
		Login: "alice",
		Token: "token-a",
		Owner: "octo",
		Repo:  "demo",
		PR:    1,
	})
	if !errors.Is(err, watch.ErrManagerClosed) {
		t.Fatalf("Start() after Close() error = %v, want %v", err, watch.ErrManagerClosed)
	}
}

// TestStreamableHandlerNegotiates20260728 is the primary contract test for the
// adopted protocol. Connecting a real SDK client exercises the discovery-first
// handshake (server/discover, not the legacy initialize), and the negotiated
// version must be 2026-07-28 — go-sdk downgrades a stateful server to
// 2025-11-25, so this assertion is what actually pins Stateless: true.
// Representative tool and resource requests then confirm the registered
// surface is reachable over the negotiated protocol.
func TestStreamableHandlerNegotiates20260728(t *testing.T) {
	db := openServerTestDB(t)
	handler := BuildStreamableHandler(db, 30*time.Second)
	t.Cleanup(handler.Close)

	httpServer := httptest.NewServer(withAuthContext(handler, map[string]string{
		"token-a": "alice",
	}))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		HTTPClient:           bearerTokenHTTPClient("token-a"),
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	if got := session.InitializeResult().ProtocolVersion; got != testProtocolVersion20260728 {
		t.Fatalf("negotiated protocol version = %q, want %q (a stateful server would fall back to 2025-11-25)",
			got, testProtocolVersion20260728)
	}

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() over %s error = %v", testProtocolVersion20260728, err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("ListTools() returned no tools; the registered tool surface is unreachable over the new protocol")
	}
	if _, err := session.ListResources(context.Background(), nil); err != nil {
		t.Fatalf("ListResources() over %s error = %v", testProtocolVersion20260728, err)
	}
}

// TestStreamableHandlerServerDiscoverIsStateless pins the wire-level contract
// of the discovery RPC that replaces initialize: it must succeed, advertise
// 2026-07-28, and must not mint an Mcp-Session-Id (mcp-gateway relies on the
// absence of session affinity — see mcp-gateway docs/mcp-protocol-transparency.md).
func TestStreamableHandlerServerDiscoverIsStateless(t *testing.T) {
	db := openServerTestDB(t)
	handler := BuildStreamableHandler(db, 30*time.Second)
	t.Cleanup(handler.Close)

	httpServer := httptest.NewServer(withAuthContext(handler, map[string]string{
		"token-a": "alice",
	}))
	t.Cleanup(httpServer.Close)

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + testNewProtocolMeta + `}}`)
	req := newProtocolRequest(t, context.Background(), httpServer.URL, "token-a", "server/discover", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server/discover status = %d, want 200; body=%s", resp.StatusCode, respBody)
	}
	if sid := resp.Header.Get(testMcpSessionIDHeader); sid != "" {
		t.Fatalf("server/discover response %s = %q, want empty in stateless mode", testMcpSessionIDHeader, sid)
	}
	if !strings.Contains(string(respBody), testProtocolVersion20260728) {
		t.Fatalf("server/discover response does not advertise %s: %s", testProtocolVersion20260728, respBody)
	}
}

// TestStreamableHandlerSubscriptionsListenAcknowledges covers the happy path of
// the RPC that replaced resources/subscribe and the standalone GET stream.
// Per spec the server MUST send notifications/subscriptions/acknowledged as the
// first message on the long-lived stream, carrying the listen request's JSON-RPC
// ID as io.modelcontextprotocol/subscriptionId so clients can demultiplex.
func TestStreamableHandlerSubscriptionsListenAcknowledges(t *testing.T) {
	db := openServerTestDB(t)
	handler := BuildStreamableHandler(db, 30*time.Second)
	t.Cleanup(handler.Close)

	httpServer := httptest.NewServer(withAuthContext(handler, map[string]string{
		"token-a": "alice",
	}))
	t.Cleanup(httpServer.Close)

	// The stream stays open until the client goes away, so cancel the request
	// context once the acknowledgement has been observed.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	body := []byte(`{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{` +
		testNewProtocolMeta + `,"notifications":{"toolsListChanged":true}}}`)
	req := newProtocolRequest(t, ctx, httpServer.URL, "token-a", "subscriptions/listen", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("subscriptions/listen status = %d, want 200; body=%s", resp.StatusCode, respBody)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("subscriptions/listen Content-Type = %q, want text/event-stream (long-lived stream)", ct)
	}

	ack := readFirstSSEData(t, resp.Body)
	var decoded struct {
		Method string `json:"method"`
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(ack, &decoded); err != nil {
		t.Fatalf("decode first stream message error = %v; raw=%s", err, ack)
	}
	if decoded.Method != "notifications/subscriptions/acknowledged" {
		t.Fatalf("first stream message method = %q, want notifications/subscriptions/acknowledged; raw=%s",
			decoded.Method, ack)
	}
	if _, ok := decoded.Params.Meta["io.modelcontextprotocol/subscriptionId"]; !ok {
		t.Fatalf("acknowledgement is missing io.modelcontextprotocol/subscriptionId; raw=%s", ack)
	}
}

// TestStreamableHandlerStatelessRejects405Methods confirms Stateless: true is
// wired through to the SDK handler. Per spec a stateless server has no
// standalone GET stream to open and no session to tear down via DELETE, so both
// must return 405 — mcp-gateway forwards this status verbatim.
func TestStreamableHandlerStatelessRejects405Methods(t *testing.T) {
	db := openServerTestDB(t)
	handler := BuildStreamableHandler(db, 30*time.Second)
	t.Cleanup(handler.Close)

	httpServer := httptest.NewServer(withAuthContext(handler, map[string]string{
		"token-a": "alice",
	}))
	t.Cleanup(httpServer.Close)

	tests := []struct {
		name   string
		method string
		accept string
	}{
		{name: "standalone GET stream", method: http.MethodGet, accept: "text/event-stream"},
		{name: "session teardown DELETE", method: http.MethodDelete, accept: "application/json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, httpServer.URL, nil)
			if err != nil {
				t.Fatalf("http.NewRequest() error = %v", err)
			}
			req.Header.Set("Authorization", "Bearer token-a")
			req.Header.Set("Accept", tt.accept)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("http.Do() error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s status = %d, want 405 in stateless mode", tt.method, resp.StatusCode)
			}
		})
	}
}

func TestTokenFromToolRequestPrefersCurrentAuthorizationHeader(t *testing.T) {
	ctx := context.WithValue(context.Background(), middleware.ContextKeyToken, "old-token")
	req := &mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{
			Header: http.Header{"Authorization": {"Bearer fresh-token"}},
		},
	}

	if got := tokenFromToolRequest(ctx, req); got != "fresh-token" {
		t.Fatalf("tokenFromToolRequest() = %q, want fresh-token", got)
	}
}

func openServerTestDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "server-test.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})
	return db
}

func withAuthContext(next http.Handler, tokenLogins map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		login := tokenLogins[token]
		ctx := context.WithValue(r.Context(), middleware.ContextKeyToken, token)
		ctx = context.WithValue(ctx, middleware.ContextKeyLogin, login)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newProtocolRequest builds a raw JSON-RPC request carrying everything protocol
// 2026-07-28 requires on the wire: the SEP-2243 standard headers (Mcp-Method,
// Mcp-Protocol-Version) alongside the usual Accept/Content-Type. The SDK
// rejects requests whose headers and body disagree, so rpcMethod must match the
// method in body.
func newProtocolRequest(t *testing.T, ctx context.Context, endpoint, token, rpcMethod string, body []byte) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Protocol-Version", testProtocolVersion20260728)
	req.Header.Set("Mcp-Method", rpcMethod)
	return req
}

// readFirstSSEData returns the payload of the first `data:` line on an SSE
// stream, skipping event/id fields and keep-alive comment lines.
func readFirstSSEData(t *testing.T, r io.Reader) []byte {
	t.Helper()

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			return []byte(strings.TrimSpace(data))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading SSE stream error = %v", err)
	}
	t.Fatal("SSE stream closed before any data line was received")
	return nil
}

func bearerTokenHTTPClient(token string) *http.Client {
	return &http.Client{
		Transport: bearerTokenRoundTripper{token: token, base: http.DefaultTransport},
	}
}

type bearerTokenRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt bearerTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(req)
}

// TestSubscribeHandlerRejectsLegacyURIScheme is a regression test for the ghost
// subscription bug: before the fix, copilot-review://watch/... URIs fell through
// the watchPrefix check and returned nil (success), causing go-sdk to register a
// subscription that would never receive notifications. The fix returns
// mcp.ResourceNotFoundError for the legacy scheme.
//
// This sends the raw subscriptions/listen JSON-RPC request instead of going
// through mcp.ClientSession.Subscribe(): under protocol 2026-07-28,
// ClientSession.Subscribe() (go-sdk v1.7.0) discards the server's JSON-RPC
// error for this method — verified by wire capture that the server correctly
// returns HTTP 400 with the ResourceNotFound error while Subscribe() itself
// returns nil. That is an upstream go-sdk client-side bug, not a review-raven
// authorization gap; asserting on the raw HTTP response is what actually
// exercises our SubscribeHandler's security boundary.
func TestSubscribeHandlerRejectsRetiredURISchemes(t *testing.T) {
	for _, uri := range []string{"copilot-review://watch/abc123", "review-raven://requests/cyc_01jbq3x8n5r7t9v2w4y6z8a0c1"} {
		t.Run(uri, func(t *testing.T) {
			db := openServerTestDB(t)
			handler := BuildStreamableHandler(db, 30*time.Second)
			t.Cleanup(handler.Close)

			httpServer := httptest.NewServer(withAuthContext(handler, map[string]string{
				"token-a": "alice",
			}))
			t.Cleanup(httpServer.Close)

			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{` +
				testNewProtocolMeta + `,"notifications":{"resourceSubscriptions":["` + uri + `"]}}}`)
			req := newProtocolRequest(t, context.Background(), httpServer.URL, "token-a", "subscriptions/listen", body)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("http.Do() error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response body error = %v", err)
			}
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("subscriptions/listen for legacy URI status = 200, want an error status; body=%s", respBody)
			}
			if !strings.Contains(string(respBody), "Resource not found") {
				t.Fatalf("subscriptions/listen response = %q, want to contain \"Resource not found\"", respBody)
			}
		})
	}
}
