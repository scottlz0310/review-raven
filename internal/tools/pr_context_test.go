package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/reader"
)

type contextFixture struct {
	before, after                       ghclient.PRContext
	snapshot                            reader.Snapshot
	policy                              []byte
	found                               bool
	policyErr                           error
	contexts, fetches, bodies, policies int
	policySHA                           string
}

func (f *contextFixture) Context(context.Context) (ghclient.PRContext, error) {
	f.contexts++
	if f.contexts == 1 {
		return f.before, nil
	}
	return f.after, nil
}

func (f *contextFixture) Fetch(_ context.Context, bodies bool) (reader.Snapshot, error) {
	f.fetches++
	if bodies {
		f.bodies++
	}
	return f.snapshot, nil
}

func (f *contextFixture) ProjectAllowlist(_ context.Context, sha string) (reader.ProjectAllowlist, error) {
	if sha != f.before.PolicyBaseSHA {
		return reader.ProjectAllowlist{}, errors.New("誤ったauthor宣言SHA")
	}
	return reader.ProjectAllowlist{State: reader.ProjectAllowlistPresent, Logins: []string{"project-bot"}}, nil
}

func (f *contextFixture) CIPolicy(_ context.Context, sha string) ([]byte, bool, error) {
	f.policies++
	f.policySHA = sha
	return f.policy, f.found, f.policyErr
}

func newContextFixture() *contextFixture {
	refs := ghclient.PRContext{URL: "https://github.com/o/r/pull/1", State: "OPEN", HeadSHA: strings.Repeat("1", 40),
		BaseSHA: strings.Repeat("2", 40), BaseRefName: "main", PolicyBaseSHA: strings.Repeat("4", 40)}
	comment := func(kind reader.Kind, id, author string) reader.Comment {
		return reader.Comment{Kind: kind, ID: id, Author: author, AuthorType: "Bot", URL: "https://github.com/o/r/pull/1#" + id}
	}
	return &contextFixture{before: refs, after: refs, snapshot: reader.Snapshot{
		HeadSHA: refs.HeadSHA, BaseSHA: refs.BaseSHA, BaseRefName: refs.BaseRefName, PolicyBaseSHA: refs.PolicyBaseSHA,
		Threads:       []reader.Thread{{ID: "thread", IsResolved: true, Comments: []reader.Comment{comment(reader.KindThreadComment, "1", "trusted")}}},
		Reviews:       []reader.Comment{comment(reader.KindReviewBody, "2", "project-bot")},
		IssueComments: []reader.Comment{comment(reader.KindIssueComment, "3", "trusted")},
	}, found: true, policy: []byte(`{"version":1}`)}
}

func TestReadPRContextStopsWithoutRequestingBodies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		change     func(*contextFixture)
		code       string
		wantPolicy int
	}{
		{"全種類とresolvedを含む", func(*contextFixture) {}, "", 1},
		{"宣言なし", func(f *contextFixture) { f.found = false; f.policy = nil }, "", 1},
		{"未知の投稿者", func(f *contextFixture) { f.snapshot.Reviews[0].Author = "intruder" }, reader.CodeUntrustedComment, 0},
		{"投稿者消失", func(f *contextFixture) { f.snapshot.IssueComments[0].Author = "" }, reader.CodeAuthorCheckFailed, 0},
		{"取得前HEAD変更", func(f *contextFixture) { f.snapshot.HeadSHA = strings.Repeat("5", 40) }, reader.CodeSnapshotChanged, 0},
		{"取得後HEAD変更", func(f *contextFixture) { f.after.HeadSHA = strings.Repeat("5", 40) }, reader.CodeSnapshotChanged, 1},
		{"base変更", func(f *contextFixture) { f.after.BaseSHA = strings.Repeat("5", 40) }, reader.CodeSnapshotChanged, 1},
		{"branch変更", func(f *contextFixture) { f.after.BaseRefName = "next" }, reader.CodeSnapshotChanged, 1},
		{"宣言SHA変更", func(f *contextFixture) { f.after.PolicyBaseSHA = strings.Repeat("5", 40) }, reader.CodeSnapshotChanged, 1},
		{"PR close", func(f *contextFixture) { f.after.State = "CLOSED" }, reader.CodeSnapshotChanged, 1},
		{"不正JSON", func(f *contextFixture) { f.policy = []byte("{") }, "CI_POLICY_INVALID", 1},
		{"配列JSON", func(f *contextFixture) { f.policy = []byte("[]") }, "CI_POLICY_INVALID", 1},
		{"nullJSON", func(f *contextFixture) { f.policy = []byte("null") }, "CI_POLICY_INVALID", 1},
		{"不正UTF8", func(f *contextFixture) { f.policy = []byte{'{', '"', 'x', '"', ':', '"', 255, '"', '}'} }, "CI_POLICY_INVALID", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture()
			tc.change(f)
			out, err := readPRContext(context.Background(), f, []string{"trusted"})
			if f.bodies != 0 || f.policies != tc.wantPolicy {
				t.Fatalf("本文=%d 宣言=%d", f.bodies, f.policies)
			}
			if f.policies > 0 && f.policySHA != f.before.PolicyBaseSHA {
				t.Fatal("宣言元SHA不一致")
			}
			if tc.code != "" {
				var gate *reader.Error
				if !errors.As(err, &gate) || gate.Code != tc.code {
					t.Fatalf("err=%v want=%s", err, tc.code)
				}
				if out.SnapshotID != "" {
					t.Fatal("失敗時に成功snapshotを返した")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Comments) != 3 || !out.Pagination.Complete || f.fetches != 2 || out.CIPolicy.Found != f.found {
				t.Fatalf("out=%+v fetches=%d", out, f.fetches)
			}
			raw, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"body"`) {
				t.Fatal("本文fieldが存在する")
			}
			verified, err := reader.ReadMetadata(context.Background(), f, []string{"trusted"})
			if err != nil || verified.SnapshotID != out.SnapshotID {
				t.Fatalf("snapshotID不一致: %v", err)
			}
		})
	}
}

func TestReadPRContextStopsOnPolicyTransportFailure(t *testing.T) {
	f := newContextFixture()
	f.policyErr = errors.New("宣言取得失敗")
	_, err := readPRContext(context.Background(), f, []string{"trusted"})
	if !errors.Is(err, f.policyErr) || f.contexts != 1 || f.bodies != 0 {
		t.Fatalf("err=%v fixture=%+v", err, f)
	}
}

func TestReadPRContextEmptyAllowlistStopsBeforeCommentsAndPolicy(t *testing.T) {
	f := newContextFixture()
	_, err := readPRContext(context.Background(), f, nil)
	var gate *reader.Error
	if !errors.As(err, &gate) || gate.Code != reader.CodeTrustedAuthorsNotConfigured || f.fetches != 0 || f.policies != 0 {
		t.Fatalf("err=%v fixture=%+v", err, f)
	}
}

func TestPRContextMCPContract(t *testing.T) {
	for _, author := range []string{"trusted", "intruder"} {
		t.Run(author, func(t *testing.T) {
			fixture := &fakeGitHub{t: t, authors: []string{author}}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					fixture.ServeHTTP(w, r)
					return
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var request struct {
					Variables map[string]any `json:"variables"`
					Query     string         `json:"query"`
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					t.Error(err)
					return
				}
				if _, ok := request.Variables["withBody"]; ok {
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
					fixture.ServeHTTP(w, r)
					return
				}
				if strings.Contains(strings.ToLower(request.Query), "body") {
					t.Error("contextが本文を要求")
				}
				refs := map[string]any{"url": "https://github.com/o/r/pull/1", "state": "OPEN", "headRefOid": strings.Repeat("1", 40), "baseRefOid": strings.Repeat("2", 40), "baseRefName": "main",
					"baseRef": map[string]any{"name": "main", "prefix": "refs/heads/", "target": map[string]any{"__typename": "Commit", "oid": strings.Repeat("4", 40)}}}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": refs}}})
			}))
			defer upstream.Close()
			gh, err := ghclient.NewWithHTTPClientAndURLFull(upstream.Client(), upstream.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			server := mcp.NewServer(&mcp.Implementation{Name: "context-test", Version: "1"}, nil)
			RegisterPRContextTool(server, func(context.Context, *mcp.CallToolRequest) (*ghclient.Client, error) { return gh, nil }, []string{"trusted"})
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(context.Background(), st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ss.Close() }()
			client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			listed, err := client.ListTools(context.Background(), nil)
			if err != nil || len(listed.Tools) != 1 {
				t.Fatalf("一覧=%v err=%v", listed, err)
			}
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_pr_context", Arguments: map[string]any{"owner": "o", "repo": "r", "pr": 1}})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError != (author == "intruder") || fixture.bodyAsked {
				t.Fatalf("result=%+v body=%v", result, fixture.bodyAsked)
			}
			raw, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"body"`) {
				t.Fatalf("本文fieldが存在: %s", raw)
			}
			if author == "trusted" {
				var out prContextOutput
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatal(err)
				}
				if len(out.Comments) != 1 || !out.Pagination.Complete || out.CIPolicy.Found {
					t.Fatalf("out=%+v", out)
				}
			} else if !strings.Contains(string(raw), reader.CodeUntrustedComment) {
				t.Fatalf("gate error欠落: %s", raw)
			}
		})
	}
}
