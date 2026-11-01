package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/autherr"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
)

func connectDescriptionTools(t *testing.T, provider githubClientProvider) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	RegisterPRDescriptionTools(server, provider, "verified-app[bot]")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func descriptionArguments() map[string]any {
	return map[string]any{"owner": "owner", "repo": "repo", "pr": 1, "expectedHeadSha": strings.Repeat("a", 40),
		"expectedBodySha256": fmt.Sprintf("%x", sha256.Sum256([]byte("old"))), "body": ""}
}

func TestDescriptionToolsValidateBeforeAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		change     func(map[string]any)
	}{
		{"body省略", "update_pr_description", func(in map[string]any) { delete(in, "body") }},
		{"body null", "update_pr_description", func(in map[string]any) { in["body"] = nil }},
		{"不正owner", "get_pr_description", func(in map[string]any) { in["owner"] = "a/b" }},
		{"不正repo", "update_pr_description", func(in map[string]any) { in["repo"] = ".." }},
		{"不正pr", "get_pr_description", func(in map[string]any) { in["pr"] = 0 }},
		{"短縮HEAD", "update_pr_description", func(in map[string]any) { in["expectedHeadSha"] = "abcd" }},
		{"大文字hash", "update_pr_description", func(in map[string]any) { in["expectedBodySha256"] = strings.Repeat("A", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			session := connectDescriptionTools(t, func(context.Context, *mcp.CallToolRequest) (*ghclient.Client, error) {
				calls++
				return nil, autherr.NewAuthRequired()
			})
			in := descriptionArguments()
			if tc.tool == "get_pr_description" {
				delete(in, "body")
				delete(in, "expectedHeadSha")
				delete(in, "expectedBodySha256")
			}
			tc.change(in)
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: in})
			if err == nil && !res.IsError {
				t.Fatal("入力不正を受理しました")
			}
			if calls != 0 {
				t.Fatal("入力不正で認証処理を呼びました")
			}
		})
	}
}

func TestDescriptionToolsRequireProxyAuthentication(t *testing.T) {
	for _, tool := range []string{"get_pr_description", "update_pr_description"} {
		t.Run(tool, func(t *testing.T) {
			tokens := &testInstallationTokens{token: "secret-installation"}
			session := connectDescriptionTools(t, newInstallationClientProvider(time.Second, tokens))
			in := descriptionArguments()
			if tool == "get_pr_description" {
				in = map[string]any{"owner": "owner", "repo": "repo", "pr": 1}
			}
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: in})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(res.StructuredContent)
			var envelope descriptionErrorEnvelope
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}
			if !res.IsError || envelope.Error == nil || envelope.Error.Code != "AUTH_REQUIRED" || envelope.Error.MayHaveChanged || tokens.calls != 0 {
				t.Fatalf("未認証の境界が不正です: %s 発行=%d", data, tokens.calls)
			}
		})
	}
}

func TestDescriptionToolsMCPResults(t *testing.T) {
	for _, tc := range []struct {
		name, body, status, code string
		failPatch                bool
	}{
		{"読取", "", "", "", false},
		{"同一本文", "old", "unchanged", "", false},
		{"空本文へ更新", "", "updated", "", false},
		{"結果不明", "", "", "UPDATE_OUTCOME_UNKNOWN", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					_, _ = w.Write([]byte(`{"login":"verified-app[bot]","type":"Bot","node_id":"bot-id"}`))
				case http.MethodPatch:
					patches++
					if tc.failPatch {
						w.WriteHeader(500)
						return
					}
					_, _ = w.Write([]byte("{}"))
				case http.MethodPost:
					body := "old"
					var editor any
					var editedAt any
					if patches > 0 {
						body = tc.body
						editor = map[string]any{"__typename": "Bot", "login": "verified-app[bot]", "id": "bot-id"}
						editedAt = "2026-11-02T00:00:00Z"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
						"url": "https://github.com/owner/repo/pull/1", "state": "OPEN", "headRefOid": strings.Repeat("a", 40),
						"body": body, "author": nil, "editor": editor, "lastEditedAt": editedAt,
					}}}})
				default:
					t.Errorf("想定外の操作: %s", r.Method)
				}
			}))
			t.Cleanup(api.Close)
			client, err := ghclient.NewWithHTTPClientAndURLFull(api.Client(), api.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			session := connectDescriptionTools(t, staticProvider(client))
			in, tool := descriptionArguments(), "update_pr_description"
			in["body"] = tc.body
			if tc.name == "読取" {
				tool = "get_pr_description"
				in = map[string]any{"owner": "owner", "repo": "repo", "pr": 1}
			}
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: in})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(res.StructuredContent)
			if tc.code != "" {
				var envelope descriptionErrorEnvelope
				if err := json.Unmarshal(data, &envelope); err != nil {
					t.Fatal(err)
				}
				if !res.IsError || envelope.Error == nil || envelope.Error.Code != tc.code || !envelope.Error.MayHaveChanged {
					t.Fatalf("結果不明を成功・書込みなしへ変換しました: %s", data)
				}
			} else {
				var out ghclient.DescriptionUpdateResult
				if err := json.Unmarshal(data, &out); err != nil {
					t.Fatal(err)
				}
				if res.IsError || out.Status != tc.status || out.OriginalAuthor != nil {
					t.Fatalf("応答=%s", data)
				}
				if tc.status == "updated" && (out.Body != "" || out.LastEditor == nil) {
					t.Fatalf("空本文・編集者=%s", data)
				}
				if tc.status != "updated" && (out.LastEditor != nil || out.LastEditedAt != nil) {
					t.Fatalf("nullの補完=%s", data)
				}
			}
			wantPatches := 0
			if tc.status == "updated" || tc.failPatch {
				wantPatches = 1
			}
			if patches != wantPatches {
				t.Fatalf("PATCH=%d、期待=%d", patches, wantPatches)
			}
			if len(res.Content) == 0 {
				t.Fatal("text contentがありません")
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok || text.Text != string(data) {
				t.Fatalf("textとstructuredContentが一致しません: %+v", res.Content)
			}
		})
	}
}
