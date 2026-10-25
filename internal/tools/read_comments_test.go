package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/reader"
	"github.com/scottlz0310/review-raven/internal/reader/readertest"
)

// 共有シナリオを、ツールの出力の形（成功の snapshot、または失敗の envelope）で確かめる。
func TestServeReadPRCommentsRunsTheSharedScenarios(t *testing.T) {
	outputSchema := readertest.Schema(t, "read_pr_comments.output.schema.json")
	errorSchema := readertest.Schema(t, "read_pr_comments.error.schema.json")

	for _, sc := range readertest.Load(t) {
		t.Run(sc.Name, func(t *testing.T) {
			src := readertest.NewSource(t, sc)
			res, out, err := serveReadPRComments(context.Background(), src, sc.ServerAllowlist)
			if err != nil {
				t.Fatalf("unexpected protocol error: %v", err)
			}
			if src.Calls != sc.Expect.FetchCalls || src.BodyCalls != sc.Expect.BodyFetchCalls {
				t.Fatalf("fetch calls = %d (with bodies %d), want %d (with bodies %d)", src.Calls, src.BodyCalls, sc.Expect.FetchCalls, sc.Expect.BodyFetchCalls)
			}

			if sc.Expect.Outcome == "ok" {
				if res != nil && res.IsError {
					t.Fatalf("unexpected error result: %+v", res)
				}
				output, ok := out.(reader.Output)
				if !ok {
					t.Fatalf("output = %T, want reader.Output", out)
				}
				readertest.Validate(t, outputSchema, output)
				return
			}

			if res == nil || !res.IsError {
				t.Fatalf("result = %+v, want an error result", res)
			}
			envelope, ok := out.(reader.ErrorEnvelope)
			if !ok {
				t.Fatalf("output = %T, want reader.ErrorEnvelope", out)
			}
			if envelope.Error.Code != sc.Expect.Code {
				t.Fatalf("code = %s, want %s", envelope.Error.Code, sc.Expect.Code)
			}
			readertest.Validate(t, errorSchema, envelope)
		})
	}
}

// fakeGitHub answers the comment queries for one pull request with the given
// authors, and records whether any request selected a body.
type fakeGitHub struct {
	t         *testing.T
	mu        sync.Mutex
	authors   []string // issue comment authors
	project   *string  // content of the project allowlist file at the base ref; nil = no such file
	bodyAsked bool
	requests  int
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/contents/") {
		if f.project == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "encoding": "base64", "name": "trusted-comment-authors.json", "path": ".review-raven/trusted-comment-authors.json",
			"size": len(*f.project), "content": base64.StdEncoding.EncodeToString([]byte(*f.project)),
		})
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decode request: %v", err)
	}
	withBody, _ := req.Variables["withBody"].(bool)
	f.mu.Lock()
	f.requests++
	if withBody {
		f.bodyAsked = true
	}
	f.mu.Unlock()

	refs := map[string]any{"headRefOid": strings.Repeat("1", 40), "baseRefOid": strings.Repeat("2", 40)}
	empty := map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}
	pr := map[string]any{"headRefOid": refs["headRefOid"], "baseRefOid": refs["baseRefOid"]}
	switch {
	case strings.Contains(req.Query, "reviewThreads("):
		pr["reviewThreads"] = empty
	case strings.Contains(req.Query, "reviews("):
		pr["reviews"] = empty
	default:
		var nodes []any
		for i, author := range f.authors {
			c := map[string]any{
				"databaseId": 900 + i, "url": "https://github.com/o/r/pull/1#c", "createdAt": "2026-10-25T00:00:00Z",
				"lastEditedAt": nil, "author": map[string]any{"login": author, "__typename": "User"},
			}
			if withBody {
				c["body"] = "text of " + author
			}
			nodes = append(nodes, c)
		}
		pr["comments"] = map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}})
}

func connectReadTool(t *testing.T, gh *fakeGitHub, serverLogins []string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	client, err := ghclient.NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	RegisterReadPRCommentsTool(server, func(context.Context, *mcp.CallToolRequest) (*ghclient.Client, error) { return client, nil }, serverLogins)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callRead(t *testing.T, session *mcp.ClientSession) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_pr_comments",
		Arguments: map[string]any{"owner": "o", "repo": "r", "pr": 1},
	})
	if err != nil {
		t.Fatalf("CallTool() protocol error = %v", err)
	}
	return res
}

// プロトコル越しに、許可外の投稿者がいれば、GitHub へ本文を一度も要求せず、本文を含まない失敗を返す。
func TestReadPRCommentsOverMCPNeverAsksForTextWhenAnAuthorIsNotAllowed(t *testing.T) {
	gh := &fakeGitHub{t: t, authors: []string{"thread-owl", "mallory"}}
	res := callRead(t, connectReadTool(t, gh, []string{"thread-owl"}))

	if !res.IsError {
		t.Fatalf("result = %+v, want an error", res)
	}
	if gh.bodyAsked {
		t.Fatal("a body was requested from GitHub although an author is not allowed")
	}
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), "text of") {
		t.Fatalf("the failure carries comment text: %s", raw)
	}
	var envelope reader.ErrorEnvelope
	structured, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(structured, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != reader.CodeUntrustedComment || envelope.Error.Details == nil || len(envelope.Error.Details.Comments) != 1 || envelope.Error.Details.Comments[0].Author != "mallory" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestReadPRCommentsOverMCPReturnsVerifiedText(t *testing.T) {
	gh := &fakeGitHub{t: t, authors: []string{"thread-owl", "scottlz0310-user"}}
	res := callRead(t, connectReadTool(t, gh, []string{"thread-owl", "scottlz0310-user"}))

	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	var out reader.Output
	structured, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.IssueComments) != 2 || out.IssueComments[0].Body != "text of thread-owl" || out.IssueComments[1].Body != "text of scottlz0310-user" {
		t.Fatalf("issue comments = %+v", out.IssueComments)
	}
	if out.SnapshotID == "" || !out.Pagination.Complete {
		t.Fatalf("output = %+v", out)
	}
	// 検証が通ってから本文を 1 回要求している(メタデータ 3 回 + 本文 3 回)。
	if !gh.bodyAsked {
		t.Fatal("the text was never requested")
	}
}

func TestReadPRCommentsOverMCPFailsClosedWithoutAServerAllowlist(t *testing.T) {
	gh := &fakeGitHub{t: t, authors: []string{"thread-owl"}}
	res := callRead(t, connectReadTool(t, gh, nil))

	if !res.IsError {
		t.Fatalf("result = %+v, want an error", res)
	}
	if gh.requests != 0 {
		t.Fatalf("GitHub was called %d time(s) although no author is trusted", gh.requests)
	}
	var envelope reader.ErrorEnvelope
	structured, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(structured, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != reader.CodeTrustedAuthorsNotConfigured {
		t.Fatalf("code = %s", envelope.Error.Code)
	}
}

func TestReadPRCommentsRequiresOwnerRepoAndPR(t *testing.T) {
	session := connectReadTool(t, &fakeGitHub{t: t}, []string{"thread-owl"})
	for _, args := range []map[string]any{
		{"owner": "", "repo": "r", "pr": 1},
		{"owner": "o", "repo": "", "pr": 1},
		{"owner": "o", "repo": "r", "pr": 0},
	} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_pr_comments", Arguments: args})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("args %v must be rejected", args)
		}
	}
}

// base ref のプロジェクト allowlist: あれば和集合に加わり、壊れていれば追加なしとは読まずに止まる。
func TestReadPRCommentsOverMCPAppliesTheProjectAllowlist(t *testing.T) {
	valid := `{"version":1,"additional_logins":["ci-bot"]}`
	broken := `{"version":1}`
	tests := []struct {
		name         string
		project      *string
		wantError    bool
		wantCode     string
		wantBodyRead bool
	}{
		{name: "the file adds the author", project: &valid, wantBodyRead: true},
		{name: "no file adds nobody", project: nil, wantError: true, wantCode: reader.CodeUntrustedComment},
		{name: "a malformed file stops the reader", project: &broken, wantError: true, wantCode: reader.CodeProjectAllowlistInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := &fakeGitHub{t: t, authors: []string{"thread-owl", "ci-bot"}, project: tt.project}
			res := callRead(t, connectReadTool(t, gh, []string{"thread-owl"}))

			if res.IsError != tt.wantError {
				t.Fatalf("IsError = %v, want %v: %+v", res.IsError, tt.wantError, res)
			}
			if gh.bodyAsked != tt.wantBodyRead {
				t.Fatalf("bodyAsked = %v, want %v", gh.bodyAsked, tt.wantBodyRead)
			}
			if tt.wantError {
				var envelope reader.ErrorEnvelope
				structured, _ := json.Marshal(res.StructuredContent)
				if err := json.Unmarshal(structured, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Error.Code != tt.wantCode {
					t.Fatalf("code = %s, want %s", envelope.Error.Code, tt.wantCode)
				}
			}
		})
	}
}
