package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/scottlz0310/review-raven/internal/contracts"
	"github.com/scottlz0310/review-raven/internal/middleware"
	"github.com/scottlz0310/review-raven/internal/reviewrequest"
	"github.com/scottlz0310/review-raven/internal/store"
)

const (
	rrOwner = "scottlz0310"
	rrRepo  = "mcp-resource-subscriber"
	rrPR    = 226
	rrHeadA = "0123456789abcdef0123456789abcdef01234567"
	rrHeadB = "89abcdef0123456789abcdef0123456789abcdef"
)

var rrSecret = strings.Repeat("s", 32)

// rrPublicACL is the user access list of the test servers.
const rrPublicACL = `{
	"users": {
		"alice": ["scottlz0310/*"],
		"carol": ["scottlz0310/*"],
		"dave": ["scottlz0310/*"]
	},
	"operators": ["carol"]
}`

// fixedHeads is a HeadResolver backed by a map ("owner/repo#pr" → sha).
type fixedHeads map[string]string

func (f fixedHeads) CurrentHead(_ context.Context, owner, repo string, pr int) (string, error) {
	sha, ok := f[owner+"/"+repo+"#"+itoa(pr)]
	if !ok {
		return "", reviewrequest.ErrPullRequestNotAccessible
	}
	return sha, nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type requestFixture struct {
	t       *testing.T
	db      *store.DB
	svc     *reviewrequest.Service
	server  *httptest.Server
	handler *StreamableHandler
}

// newRequestFixture serves the MCP surface with the review-request tools. In
// App mode the identity is proxy-verified; otherwise the gateway auth applies.
func newRequestFixture(t *testing.T, appMode bool) *requestFixture {
	t.Helper()
	return newRequestFixtureWith(t, appMode, nil)
}

// newRequestFixtureWith is newRequestFixture with a hook to adjust the ledger
// dependencies (for example deterministic identifiers).
func newRequestFixtureWith(t *testing.T, appMode bool, adjust func(*reviewrequest.Deps)) *requestFixture {
	t.Helper()
	db := openServerTestDB(t)
	acl, err := reviewrequest.ParseACL(rrPublicACL)
	if err != nil {
		t.Fatal(err)
	}
	deps := reviewrequest.Deps{
		Authorizer: acl,
		Heads:      fixedHeads{rrOwner + "/" + rrRepo + "#226": rrHeadA},
		Producers:  reviewrequest.DenyAllProducers{},
		Settings:   reviewrequest.DefaultSettings(),
	}
	if adjust != nil {
		adjust(&deps)
	}
	svc, err := reviewrequest.NewService(context.Background(), db.SQL(), deps)
	if err != nil {
		t.Fatal(err)
	}

	opts := BuilderOptions{TrustedCommentAuthors: []string{"review-raven"}, ReviewRequests: svc}
	auth := middleware.Auth()
	if appMode {
		opts.InstallationTokens = &testInstallationTokens{token: "installation-token"}
		auth = middleware.AppProxyAuth(rrSecret)
	}
	handler := BuildStreamableHandlerWithOptions(db, time.Second, opts)
	t.Cleanup(handler.Close)
	server := httptest.NewServer(auth(handler))
	t.Cleanup(server.Close)
	return &requestFixture{t: t, db: db, svc: svc, server: server, handler: handler}
}

func (f *requestFixture) connect(user string) *mcp.ClientSession {
	f.t.Helper()
	transport := &http.Client{Transport: appRoundTripper(func(req *http.Request) (*http.Response, error) {
		cloned := req.Clone(req.Context())
		cloned.Header.Set("X-Authenticated-User", user)
		cloned.Header.Set("Authorization", "Bearer "+rrSecret)
		return http.DefaultTransport.RoundTrip(cloned)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "request-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.server.URL, HTTPClient: transport, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = session.Close() })
	return session
}

func (f *requestFixture) cycleCount() int {
	f.t.Helper()
	var n int
	if err := f.db.SQL().QueryRow(`SELECT COUNT(*) FROM review_requests`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func call(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return result
}

func mustContract(t *testing.T, schema string, v any) {
	t.Helper()
	if err := contracts.Validate(schema, v); err != nil {
		t.Fatalf("value violates contract schema %s: %v\nvalue: %v", schema, err, v)
	}
}

// ok asserts a successful result and validates its structured content.
func ok(t *testing.T, result *mcp.CallToolResult, schema string) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool returned an error: %+v", result.StructuredContent)
	}
	out, isMap := result.StructuredContent.(map[string]any)
	if !isMap {
		t.Fatalf("structured content = %T, want an object", result.StructuredContent)
	}
	mustContract(t, schema, out)
	return out
}

// failed asserts an error envelope with the given contract code.
func failed(t *testing.T, result *mcp.CallToolResult, code string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("expected the error %s, got a success: %+v", code, result.StructuredContent)
	}
	envelope, isMap := result.StructuredContent.(map[string]any)
	if !isMap {
		t.Fatalf("structured content = %T, want an error envelope", result.StructuredContent)
	}
	mustContract(t, "error", envelope)
	detail, _ := envelope["error"].(map[string]any)
	if detail["code"] != code {
		t.Fatalf("error code = %v, want %s", detail["code"], code)
	}
}

func getArgs(cycleID string) map[string]any {
	return map[string]any{"schemaVersion": 1, "cycleId": cycleID}
}

func requestArgs(key, head string) map[string]any {
	return map[string]any{
		"schemaVersion": 1,
		"owner":         rrOwner, "repo": rrRepo, "prNumber": rrPR,
		"expectedHeadSha": head, "reason": "opened", "idempotencyKey": key,
	}
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestReviewRequestSurface(t *testing.T) {
	newTools := []string{"cancel_review_request", "get_review_request", "request_review"}

	t.Run("published in the dedicated App mode", func(t *testing.T) {
		f := newRequestFixture(t, true)
		session := f.connect("alice")
		want := slices.Concat([]string{
			"get_review_threads", "get_trusted_comment_authors", "list_check_runs_for_sha", "post_review_cycle_comment", "post_runner_escalation", "read_pr_comments",
			"reply_and_resolve_review_thread", "reply_to_review_thread", "resolve_review_thread",
		}, newTools)
		slices.Sort(want)
		if got := toolNames(t, session); !slices.Equal(got, want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
		templates, err := session.ListResourceTemplates(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(templates.ResourceTemplates) != 1 || templates.ResourceTemplates[0].URITemplate != "review-raven://requests/{cycleId}" {
			t.Fatalf("resource templates = %+v", templates.ResourceTemplates)
		}
	})

	t.Run("not published without the ledger", func(t *testing.T) {
		db := openServerTestDB(t)
		handler := BuildStreamableHandlerWithOptions(db, time.Second, BuilderOptions{
			InstallationTokens: &testInstallationTokens{token: "installation-token"},
		})
		t.Cleanup(handler.Close)
		server := httptest.NewServer(middleware.AppProxyAuth(rrSecret)(handler))
		t.Cleanup(server.Close)
		f := &requestFixture{t: t, server: server}
		got := toolNames(t, f.connect("alice"))
		for _, name := range newTools {
			if slices.Contains(got, name) {
				t.Fatalf("%s must not be published without the ledger: %v", name, got)
			}
		}
	})

	t.Run("not published outside the dedicated App mode", func(t *testing.T) {
		f := newRequestFixture(t, false)
		got := toolNames(t, f.connect("alice"))
		for _, name := range newTools {
			if slices.Contains(got, name) {
				t.Fatalf("%s must not be published without a proxy-verified identity: %v", name, got)
			}
		}
		if len(got) != 15 {
			t.Fatalf("the existing gateway surface changed: %v", got)
		}
	})
}

func TestReviewRequestFlowOverMCP(t *testing.T) {
	f := newRequestFixture(t, true)
	alice := f.connect("alice")

	created := ok(t, call(t, alice, "request_review", requestArgs("agent-req-0001", rrHeadA)), "request_review.output")
	cycleID, _ := created["cycleId"].(string)
	uri, _ := created["resourceUri"].(string)
	if created["dispatch"] != "pending" || created["round"] != float64(1) || created["deduplicated"] != false ||
		uri != "review-raven://requests/"+cycleID {
		t.Fatalf("unexpected request output: %+v", created)
	}

	t.Run("a repeated request is deduplicated", func(t *testing.T) {
		again := ok(t, call(t, alice, "request_review", requestArgs("agent-req-0001", rrHeadA)), "request_review.output")
		if again["cycleId"] != cycleID || again["deduplicated"] != true {
			t.Fatalf("unexpected repeat: %+v", again)
		}
	})

	var revision float64
	t.Run("get_review_request returns the snapshot", func(t *testing.T) {
		snap := ok(t, call(t, alice, "get_review_request", getArgs(cycleID)), "snapshot")
		if snap["owner"] != rrOwner || snap["expectedHeadSha"] != rrHeadA || snap["terminal"] != false {
			t.Fatalf("unexpected snapshot: %+v", snap)
		}
		revision, _ = snap["snapshotRevision"].(float64)
	})

	t.Run("the resource returns the same snapshot", func(t *testing.T) {
		res, err := alice.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("ReadResource: %v", err)
		}
		if len(res.Contents) != 1 || res.Contents[0].MIMEType != "application/json" {
			t.Fatalf("unexpected contents: %+v", res.Contents)
		}
		var snap map[string]any
		if err := json.Unmarshal([]byte(res.Contents[0].Text), &snap); err != nil {
			t.Fatal(err)
		}
		mustContract(t, "snapshot", snap)
		if snap["cycleId"] != cycleID || snap["snapshotRevision"] != revision {
			t.Fatalf("resource differs from the tool snapshot: %+v", snap)
		}
	})

	t.Run("errors are contract envelopes", func(t *testing.T) {
		failed(t, call(t, alice, "request_review", requestArgs("agent-req-0002", rrHeadB)), "HEAD_CONFLICT")
		failed(t, call(t, alice, "request_review", requestArgs("agent-req-0001", rrHeadB)), "IDEMPOTENCY_CONFLICT")
		failed(t, call(t, alice, "request_review", requestArgs("agent-req-0003", "abc123")), "INVALID_ARGUMENT")
		failed(t, call(t, alice, "get_review_request", getArgs("cyc_01jbq3x8n5r7t9v2w4y6z8a0c9")), "REQUEST_NOT_FOUND")
		failed(t, call(t, alice, "get_review_request", getArgs("nope")), "INVALID_ARGUMENT")
		failed(t, call(t, alice, "request_review", map[string]any{
			"schemaVersion": 1, "owner": rrOwner, "repo": rrRepo, "prNumber": 999,
			"expectedHeadSha": rrHeadA, "reason": "opened", "idempotencyKey": "agent-req-0004",
		}), "PERMISSION_DENIED") // 存在しない PR は private PR の存在を明かさない
	})

	t.Run("a self-declared requester is not accepted", func(t *testing.T) {
		before := f.cycleCount()
		args := requestArgs("agent-req-0005", rrHeadA)
		args["requestedBy"] = "someone-else"
		result, err := alice.CallTool(context.Background(), &mcp.CallToolParams{Name: "request_review", Arguments: args})
		if err == nil && (result == nil || !result.IsError) {
			t.Fatalf("an unknown input property must be rejected: %+v", result)
		}
		if after := f.cycleCount(); after != before {
			t.Fatalf("a rejected call changed the ledger: %d -> %d", before, after)
		}
	})

	t.Run("cancellation is limited to the requester and operators", func(t *testing.T) {
		args := func(key string) map[string]any {
			return map[string]any{
				"schemaVersion": 1, "cycleId": cycleID, "expectedHeadSha": rrHeadA,
				"expectedSnapshotRevision": revision, "idempotencyKey": key,
			}
		}
		failed(t, call(t, f.connect("dave"), "cancel_review_request", args("dave-cancel-0001")), "PERMISSION_DENIED")

		carol := ok(t, call(t, f.connect("carol"), "cancel_review_request", args("carol-cancel-0001")), "cancel_review_request.output")
		if carol["cancellation"] != "requested" || carol["terminal"] != false || carol["deduplicated"] != false {
			t.Fatalf("operator cancel = %+v", carol)
		}
		// 要求者本人の取消は、取消中の cycle に結合する。
		own := ok(t, call(t, alice, "cancel_review_request", args("alice-cancel-0001")), "cancel_review_request.output")
		if own["deduplicated"] != true || own["cancelId"] != carol["cancelId"] {
			t.Fatalf("requester cancel must join the pending cancellation: %+v", own)
		}
		failed(t, call(t, alice, "cancel_review_request", map[string]any{
			"schemaVersion": 1, "cycleId": cycleID, "expectedHeadSha": rrHeadB, "expectedSnapshotRevision": revision, "idempotencyKey": "alice-cancel-0002",
		}), "HEAD_CONFLICT")
	})
}

func TestReviewRequestDeniesOthers(t *testing.T) {
	f := newRequestFixture(t, true)
	alice := f.connect("alice")
	created := ok(t, call(t, alice, "request_review", requestArgs("agent-req-0001", rrHeadA)), "request_review.output")
	cycleID, _ := created["cycleId"].(string)
	uri, _ := created["resourceUri"].(string)

	// ACL にない identity は、proxy 検証済みでも拒否する（cycleId を知っていても許可しない）。
	mallory := f.connect("mallory")
	failed(t, call(t, mallory, "request_review", requestArgs("mallory-req-0001", rrHeadA)), "PERMISSION_DENIED")
	failed(t, call(t, mallory, "get_review_request", getArgs(cycleID)), "PERMISSION_DENIED")
	failed(t, call(t, mallory, "cancel_review_request", map[string]any{
		"schemaVersion": 1, "cycleId": cycleID, "expectedHeadSha": rrHeadA, "expectedSnapshotRevision": 1, "idempotencyKey": "mallory-cancel-1",
	}), "PERMISSION_DENIED")

	t.Run("resources: denied, unknown and malformed URIs are all not found", func(t *testing.T) {
		for name, target := range map[string]struct {
			session *mcp.ClientSession
			uri     string
		}{
			"other principal":   {mallory, uri},
			"unknown cycle":     {alice, "review-raven://requests/cyc_01jbq3x8n5r7t9v2w4y6z8a0c9"},
			"query string":      {alice, uri + "?x=1"},
			"fragment":          {alice, uri + "#x"},
			"trailing slash":    {alice, uri + "/"},
			"extra path":        {alice, uri + "/extra"},
			"uppercase cycle":   {alice, strings.ToUpper(uri)},
			"missing cycle":     {alice, "review-raven://requests/"},
			"legacy watch path": {alice, "review-raven://watch/" + cycleID},
		} {
			if _, err := target.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: target.uri}); err == nil {
				t.Errorf("%s: ReadResource(%q) succeeded", name, target.uri)
			}
		}
	})

	t.Run("a wrong proxy secret is rejected before any tool runs", func(t *testing.T) {
		before := f.cycleCount()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"request_review","arguments":{}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("X-Authenticated-User", "alice")
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
		rec := httptest.NewRecorder()
		middleware.AppProxyAuth(rrSecret)(f.handler).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || f.cycleCount() != before {
			t.Fatalf("status = %d, ledger rows %d -> %d", rec.Code, before, f.cycleCount())
		}
	})
}

// 購読は read と同じ認可を通す。許可されない購読は ResourceNotFound で拒否し、存在を明かさない。
func TestReviewRequestSubscriptionAuthorization(t *testing.T) {
	f := newRequestFixture(t, true)
	created := ok(t, call(t, f.connect("alice"), "request_review", requestArgs("agent-req-0001", rrHeadA)), "request_review.output")
	uri, _ := created["resourceUri"].(string)

	listen := func(user, secret, target string) (int, string) {
		t.Helper()
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{` +
			testNewProtocolMeta + `,"notifications":{"resourceSubscriptions":["` + target + `"]}}}`)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)
		req := newProtocolRequest(t, ctx, f.server.URL, secret, "subscriptions/listen", body)
		req.Header.Set("X-Authenticated-User", user)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("http.Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(raw)
		}
		return resp.StatusCode, string(readFirstSSEData(t, resp.Body))
	}

	t.Run("the requester may listen", func(t *testing.T) {
		if status, body := listen("alice", rrSecret, uri); status != http.StatusOK {
			t.Fatalf("status = %d, body = %s", status, body)
		}
	})
	for name, tc := range map[string]struct{ user, target string }{
		"another principal": {"mallory", uri},
		"unknown cycle":     {"alice", "review-raven://requests/cyc_01jbq3x8n5r7t9v2w4y6z8a0c9"},
		"malformed uri":     {"alice", uri + "?x=1"},
	} {
		t.Run("rejected: "+name, func(t *testing.T) {
			status, body := listen(tc.user, rrSecret, tc.target)
			if status == http.StatusOK || !strings.Contains(body, "Resource not found") {
				t.Fatalf("status = %d, body = %s; want ResourceNotFound", status, body)
			}
		})
	}
	t.Run("rejected: unverified proxy secret", func(t *testing.T) {
		if status, _ := listen("alice", strings.Repeat("x", 32), uri); status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})
}

// 台帳の変更は、購読先の resource へ通知される（Build が通知先を結び付ける）。
func TestReviewRequestChangesAreNotified(t *testing.T) {
	f := newRequestFixture(t, true)
	// Build が登録した通知先を、観測用に包んで差し替えるのではなく、通知が呼ばれる条件を確認する。
	created := ok(t, call(t, f.connect("alice"), "request_review", requestArgs("agent-req-0001", rrHeadA)), "request_review.output")
	cycleID, _ := created["cycleId"].(string)

	var seen []string
	f.svc.SetNotifier(func(id string) { seen = append(seen, id) })
	if err := f.svc.RecordDispatchAccepted(context.Background(), cycleID, "cand_01jbq3x8n5r7t9v2w4y6z8a0d1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{cycleID}) {
		t.Fatalf("notified %v, want [%s]", seen, cycleID)
	}
}

func TestInstallationHeadResolver(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		unauthorized bool
		wantSHA      string
		notReachable bool // ErrPullRequestNotAccessible
		failure      bool // any other error
	}{
		{name: "head of the pull request", status: 200, body: `{"head":{"sha":"` + rrHeadA + `"}}`, wantSHA: rrHeadA},
		{name: "not found is not accessible", status: 404, body: `{"message":"Not Found"}`, notReachable: true},
		{name: "forbidden is not accessible", status: 403, body: `{"message":"Resource not accessible by integration"}`, notReachable: true},
		{name: "an unverified caller is not accessible", status: 200, body: `{}`, unauthorized: true, notReachable: true},
		{name: "a server error is a transient failure", status: 502, body: `{"message":"bad gateway"}`, failure: true},
		{name: "an empty head is a failure", status: 200, body: `{"head":{"sha":""}}`, failure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &testInstallationTokens{token: "installation-token"}
			api := &http.Client{Transport: appRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") != "Bearer installation-token" {
					t.Error("the shared proxy secret or a user token reached GitHub")
				}
				return &http.Response{
					StatusCode: tc.status, Request: req, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})}
			resolver := NewInstallationHeadResolver(time.Second, source)

			var sha string
			var err error
			run := func(ctx context.Context) {
				sha, err = resolver.CurrentHead(context.WithValue(ctx, oauth2.HTTPClient, api), rrOwner, rrRepo, rrPR)
			}
			if tc.unauthorized {
				run(context.Background())
			} else {
				req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
				req.Header.Set("X-Authenticated-User", "alice")
				req.Header.Set("Authorization", "Bearer "+rrSecret)
				middleware.AppProxyAuth(rrSecret)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					run(r.Context())
				})).ServeHTTP(httptest.NewRecorder(), req)
			}

			switch {
			case tc.wantSHA != "":
				if err != nil || sha != tc.wantSHA {
					t.Fatalf("CurrentHead() = %q, %v; want %q", sha, err, tc.wantSHA)
				}
			case tc.notReachable:
				if !errors.Is(err, reviewrequest.ErrPullRequestNotAccessible) {
					t.Fatalf("CurrentHead() error = %v; want ErrPullRequestNotAccessible", err)
				}
			case tc.failure:
				if err == nil || errors.Is(err, reviewrequest.ErrPullRequestNotAccessible) {
					t.Fatalf("CurrentHead() error = %v; want a plain failure", err)
				}
			}
			if tc.unauthorized && source.calls != 0 {
				t.Error("an installation token was issued for an unverified caller")
			}
		})
	}
}
