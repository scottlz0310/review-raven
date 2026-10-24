package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/middleware"
	"github.com/scottlz0310/review-raven/internal/reviewrequest"
)

var internalSecret = strings.Repeat("i", 32)

const internalServiceACL = `{
	"services": {
		"svc-owl": {"actions": ["ingest", "publish_result"], "repos": ["scottlz0310/*"]},
		"svc-squirrel": {"actions": ["observe"], "repos": ["scottlz0310/*"], "producers": ["squirrel-notifier"]},
		"svc-narrow": {"actions": ["ingest"], "repos": ["elsewhere/repo"]}
	}
}`

const testCandidate = "cand_01jbq3x8n5r7t9v2w4y6z8a0d1"

// internalFixture serves both the public surface and the separate internal
// surface over one ledger, with contract identifiers so that contract fixtures
// refer to the cycle that was actually created.
type internalFixture struct {
	*requestFixture
	internal *httptest.Server
}

func newInternalFixture(t *testing.T) *internalFixture {
	t.Helper()
	users, err := reviewrequest.ParseACL(rrPublicACL)
	if err != nil {
		t.Fatal(err)
	}
	services, err := reviewrequest.ParseServiceACL(internalServiceACL)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := reviewrequest.NewSplitAuthorizer(users, services)
	if err != nil {
		t.Fatal(err)
	}
	f := newRequestFixtureWith(t, true, func(d *reviewrequest.Deps) {
		d.Authorizer = authorizer
		d.Producers = services
		d.NewID = newContractIDs()
	})

	handler := BuildInternalHandler(f.svc)
	t.Cleanup(handler.Close)
	server := httptest.NewServer(middleware.AppProxyAuth(internalSecret)(handler))
	t.Cleanup(server.Close)
	return &internalFixture{requestFixture: f, internal: server}
}

func (f *internalFixture) connectInternal(user string) *mcp.ClientSession {
	f.t.Helper()
	transport := &http.Client{Transport: appRoundTripper(func(req *http.Request) (*http.Response, error) {
		cloned := req.Clone(req.Context())
		cloned.Header.Set("X-Authenticated-User", user)
		cloned.Header.Set("Authorization", "Bearer "+internalSecret)
		return http.DefaultTransport.RoundTrip(cloned)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "internal-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.internal.URL, HTTPClient: transport, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = session.Close() })
	return session
}

func fixtureArgs(t *testing.T, schema, name string) map[string]any {
	t.Helper()
	return contractFixture(t, schema, name)
}

// 内部面は別のサーバーで、agent 向けの tools/list には現れない。
func TestInternalSurfaceIsSeparate(t *testing.T) {
	f := newInternalFixture(t)
	internalTools := []string{"ingest_review_candidate", "publish_review_observation", "publish_review_result"}

	if got := toolNames(t, f.connectInternal("svc-owl")); !slices.Equal(got, internalTools) {
		t.Fatalf("internal tools = %v, want %v", got, internalTools)
	}
	for _, name := range toolNames(t, f.connect("alice")) {
		if slices.Contains(internalTools, name) {
			t.Fatalf("%s must not appear in the agent-facing tool list", name)
		}
	}
	templates, err := f.connectInternal("svc-owl").ListResourceTemplates(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates.ResourceTemplates) != 0 {
		t.Fatalf("the internal surface must not publish resources: %+v", templates.ResourceTemplates)
	}
}

// owl が受け付けた依頼は、公開面の読み手(利用者)から見える。結果と観測も同じ台帳に反映される。
func TestInternalSurfaceFlow(t *testing.T) {
	f := newInternalFixture(t)
	owl := f.connectInternal("svc-owl")
	squirrel := f.connectInternal("svc-squirrel")
	alice := f.connect("alice")

	ingested := ok(t, call(t, owl, "ingest_review_candidate", fixtureArgs(t, "ingest_review_candidate.input", "webhook")), "ingest_review_candidate.output")
	cycleID, _ := ingested["cycleId"].(string)
	if cycleID != contractCycleID || ingested["deduplicated"] != false || ingested["round"] != float64(1) {
		t.Fatalf("unexpected ingest output: %+v", ingested)
	}
	if again := ok(t, call(t, owl, "ingest_review_candidate", fixtureArgs(t, "ingest_review_candidate.input", "webhook")), "ingest_review_candidate.output"); again["deduplicated"] != true {
		t.Fatalf("a redelivered source must dedupe: %+v", again)
	}

	// 配送 worker(別の変更)が候補の受理を記録するまで、観測は担当 candidate が確定しない。
	failed(t, call(t, squirrel, "publish_review_observation", fixtureArgs(t, "publish_review_observation.input", "waiting-ci-pending")), "PRODUCER_NOT_ASSIGNED")
	if err := f.svc.RecordDispatchAccepted(context.Background(), cycleID, testCandidate); err != nil {
		t.Fatal(err)
	}

	observed := ok(t, call(t, squirrel, "publish_review_observation", fixtureArgs(t, "publish_review_observation.input", "waiting-ci-pending")), "publish_review_observation.output")
	if observed["result"] != "applied" {
		t.Fatalf("unexpected observation output: %+v", observed)
	}
	if dup := ok(t, call(t, squirrel, "publish_review_observation", fixtureArgs(t, "publish_review_observation.input", "waiting-ci-pending")), "publish_review_observation.output"); dup["result"] != "duplicate" {
		t.Fatalf("an identical observation must be an idempotent ACK: %+v", dup)
	}

	published := ok(t, call(t, owl, "publish_review_result", fixtureArgs(t, "publish_review_result.input", "approved")), "publish_review_result.output")
	if published["terminal"] != true || published["accepted"] != true {
		t.Fatalf("unexpected result output: %+v", published)
	}

	snap := ok(t, call(t, alice, "get_review_request", getArgs(cycleID)), "snapshot")
	execution, _ := snap["execution"].(map[string]any)
	result, _ := snap["result"].(map[string]any)
	if snap["terminal"] != true || snap["terminalReason"] != "result" || result["kind"] != "approved" ||
		execution["state"] != "waiting" || execution["holdReason"] != "ciPending" {
		t.Fatalf("the public snapshot does not reflect the internal reports: %+v", snap)
	}
}

func TestInternalSurfaceAuthorization(t *testing.T) {
	f := newInternalFixture(t)
	owl := f.connectInternal("svc-owl")
	ingest := func() map[string]any { return fixtureArgs(t, "ingest_review_candidate.input", "webhook") }
	observe := func() map[string]any {
		return fixtureArgs(t, "publish_review_observation.input", "waiting-ci-pending")
	}
	ok(t, call(t, owl, "ingest_review_candidate", ingest()), "ingest_review_candidate.output")
	if err := f.svc.RecordDispatchAccepted(context.Background(), contractCycleID, testCandidate); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		user string
		tool string
		args map[string]any
		code string
	}{
		{"squirrel may not ingest", "svc-squirrel", "ingest_review_candidate", ingest(), "PERMISSION_DENIED"},
		{"owl may not observe", "svc-owl", "publish_review_observation", observe(), "PERMISSION_DENIED"},
		{"a service outside its repositories", "svc-narrow", "ingest_review_candidate", ingest(), "PERMISSION_DENIED"},
		{"a public user may not use the internal surface", "alice", "ingest_review_candidate", ingest(), "PERMISSION_DENIED"},
		{"an unknown principal", "mallory", "publish_review_result", fixtureArgs(t, "publish_review_result.input", "approved"), "PERMISSION_DENIED"},
		{"a producer the service is not granted", "svc-squirrel", "publish_review_observation", func() map[string]any {
			args := observe()
			args["producerId"] = "other-producer"
			return args
		}(), "PRODUCER_NOT_ASSIGNED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failed(t, call(t, f.connectInternal(tc.user), tc.tool, tc.args), tc.code)
		})
	}

	t.Run("a service may not use the public tools", func(t *testing.T) {
		failed(t, call(t, f.connect("svc-owl"), "request_review", requestArgs("owl-req-0001", rrHeadA)), "PERMISSION_DENIED")
	})

	t.Run("each surface has its own proxy secret", func(t *testing.T) {
		before := f.cycleCount()
		for name, tc := range map[string]struct{ url, secret string }{
			"the public secret on the internal route": {f.internal.URL, rrSecret},
			"the internal secret on the public route": {f.server.URL, internalSecret},
			"a wrong secret on the internal route":    {f.internal.URL, strings.Repeat("x", 32)},
		} {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ingest_review_candidate","arguments":{}}}`
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, tc.url, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Authenticated-User", "svc-owl")
			req.Header.Set("Authorization", "Bearer "+tc.secret)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
			}
		}
		if after := f.cycleCount(); after != before {
			t.Fatalf("a rejected call changed the ledger: %d -> %d", before, after)
		}
	})
}

// 契約 v1 の入力 schema に適合する引数は、内部面でもそのまま受理されなければならない。
func TestInternalSurfaceAcceptsContractInputFixtures(t *testing.T) {
	f := newInternalFixture(t)
	callers := map[string]string{
		"ingest_review_candidate":    "svc-owl",
		"publish_review_result":      "svc-owl",
		"publish_review_observation": "svc-squirrel",
	}
	for tool, user := range callers {
		schema := tool + ".input"
		files, err := filepath.Glob(filepath.Join(contractFixtureDir, schema, "valid", "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no valid fixtures for %s: %v", schema, err)
		}
		session := f.connectInternal(user)
		for _, file := range files {
			name := strings.TrimSuffix(filepath.Base(file), ".json")
			t.Run(tool+"/"+name, func(t *testing.T) {
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: fixtureArgs(t, schema, name)})
				if err != nil {
					t.Fatalf("a contract-valid input was rejected at the protocol layer: %v", err)
				}
				if result.IsError {
					envelope, isMap := result.StructuredContent.(map[string]any)
					if !isMap {
						t.Fatalf("an error without a structured envelope: %+v", result)
					}
					mustContract(t, "error", envelope)
				}
			})
		}
	}
}

func TestInternalSurfaceEnforcesSchemaVersion(t *testing.T) {
	f := newInternalFixture(t)
	owl := f.connectInternal("svc-owl")
	before := f.cycleCount()

	args := fixtureArgs(t, "ingest_review_candidate.input", "webhook")
	args["schemaVersion"] = 2
	failed(t, call(t, owl, "ingest_review_candidate", args), "INVALID_ARGUMENT")

	missing := fixtureArgs(t, "ingest_review_candidate.input", "webhook")
	delete(missing, "schemaVersion")
	if result, err := owl.CallTool(context.Background(), &mcp.CallToolParams{Name: "ingest_review_candidate", Arguments: missing}); err == nil && (result == nil || !result.IsError) {
		t.Fatalf("an input without schemaVersion must be rejected: %+v", result)
	}
	if after := f.cycleCount(); after != before {
		t.Fatalf("a rejected call changed the ledger: %d -> %d", before, after)
	}
}
