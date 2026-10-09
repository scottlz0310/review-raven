package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/scottlz0310/review-raven/internal/middleware"
)

type testInstallationTokens struct {
	token    string
	err      error
	calls    int
	rejected string
}

func (s *testInstallationTokens) Token(context.Context) (string, error) {
	s.calls++
	return s.token, s.err
}

func (s *testInstallationTokens) Invalidate(token string) { s.rejected = token }

type appRoundTripper func(*http.Request) (*http.Response, error)

func (f appRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestInstallationClientProvider(t *testing.T) {
	for _, tt := range []struct {
		name          string
		authenticated bool
		sourceError   bool
		status        int
		wantError     bool
	}{
		{"専用token", true, false, 200, false},
		{"利用者認証なし", false, false, 200, true},
		{"発行失敗", true, true, 200, true},
		{"失効token", true, false, 401, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := &testInstallationTokens{token: "installation-token"}
			if tt.sourceError {
				source.err = errors.New("token発行に失敗しました")
			}
			apiCalls := 0
			client := &http.Client{Transport: appRoundTripper(func(req *http.Request) (*http.Response, error) {
				apiCalls++
				if req.Header.Get("Authorization") != "Bearer installation-token" {
					t.Error("利用者tokenがGitHubに送信されました")
				}
				return &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"total_count":0,"check_runs":[]}`))}, nil
			})}
			ctx := context.WithValue(context.Background(), oauth2.HTTPClient, client)
			if tt.authenticated {
				request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
				request.Header.Set("X-Authenticated-User", "alice")
				request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
				middleware.AppProxyAuth(strings.Repeat("s", 32))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					ctx = context.WithValue(r.Context(), oauth2.HTTPClient, client)
				})).ServeHTTP(httptest.NewRecorder(), request)
			}
			gh, err := newInstallationClientProvider(time.Second, source)(ctx, nil)
			if err == nil {
				_, err = gh.ListCheckRunsForSHA(ctx, "scottlz0310", "review-raven", strings.Repeat("a", 40))
			}
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v", err)
			}
			if !tt.authenticated && source.calls != 0 {
				t.Error("未認証の呼び出しでtokenを発行しました")
			}
			if tt.sourceError && apiCalls != 0 {
				t.Error("発行失敗後にGitHubへ接続しました")
			}
			if tt.status == 401 && source.rejected != "installation-token" {
				t.Error("401で専用tokenを無効化しませんでした")
			}
		})
	}
}

func TestGitHubAppModeSurface(t *testing.T) {
	for _, appMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "gateway", true: "github-app"}[appMode], func(t *testing.T) {
			db := openServerTestDB(t)
			opts := BuilderOptions{TrustedCommentAuthors: []string{"review-raven"}}
			if appMode {
				opts.InstallationTokens = &testInstallationTokens{token: "installation-token"}
			}
			handler := BuildStreamableHandlerWithOptions(db, time.Second, opts)
			t.Cleanup(handler.Close)
			auth := middleware.Auth()
			if appMode {
				auth = middleware.AppProxyAuth(strings.Repeat("s", 32))
			}
			apiCalls := 0
			apiClient := &http.Client{Transport: appRoundTripper(func(req *http.Request) (*http.Response, error) {
				apiCalls++
				if req.Header.Get("Authorization") != "Bearer installation-token" {
					t.Error("共有proxy鍵をGitHubへ送信しました")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"total_count":0,"check_runs":[]}`))}, nil
			})}
			httpServer := httptest.NewServer(auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := context.WithValue(r.Context(), oauth2.HTTPClient, apiClient)
				handler.ServeHTTP(w, r.WithContext(ctx))
			})))
			t.Cleanup(httpServer.Close)
			transport := &http.Client{Transport: appRoundTripper(func(req *http.Request) (*http.Response, error) {
				cloned := req.Clone(req.Context())
				cloned.Header.Set("X-Authenticated-User", "alice")
				cloned.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
				return http.DefaultTransport.RoundTrip(cloned)
			})}
			client := mcp.NewClient(&mcp.Implementation{Name: "app-test", Version: "1"}, nil)
			session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint: httpServer.URL, HTTPClient: transport, DisableStandaloneSSE: true, MaxRetries: -1,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			listed, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			sort.Strings(names)
			if appMode {
				want := []string{"get_review_threads", "get_trusted_comment_authors", "list_check_runs_for_sha", "reply_and_resolve_review_thread", "reply_to_review_thread", "resolve_review_thread"}
				if !reflect.DeepEqual(names, want) {
					t.Fatalf("公開tool=%v", names)
				}
				if handler.watchManager != nil {
					t.Error("Appモードでwatch managerを開始しました")
				}
			} else if len(names) != 15 || handler.watchManager == nil {
				t.Fatalf("既存モードが変わりました: %v", names)
			}
			templates, err := session.ListResourceTemplates(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if appMode && len(templates.ResourceTemplates) != 0 {
				t.Fatal("Appモードにwatch resourceが残っています")
			}
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_trusted_comment_authors", Arguments: map[string]any{}})
			if err != nil || result.IsError {
				t.Fatalf("許可リスト取得に失敗しました: %v", err)
			}
			if appMode {
				checks, err := session.CallTool(context.Background(), &mcp.CallToolParams{
					Name: "list_check_runs_for_sha", Arguments: map[string]any{"owner": "scottlz0310", "repo": "review-raven", "sha": strings.Repeat("a", 40)},
				})
				if err != nil || checks.IsError || apiCalls != 1 {
					t.Fatalf("proxy検証後のGitHub操作が失敗しました: err=%v apiCalls=%d", err, apiCalls)
				}
				_, err = session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "review-raven://watch/old-watch"})
				if err == nil {
					t.Fatal("過去のwatch resourceを取得できました")
				}
			}
		})
	}
}

func TestGitHubAppModeRejectsForgedIdentity(t *testing.T) {
	source := &testInstallationTokens{token: "installation-token"}
	handler := BuildStreamableHandlerWithOptions(openServerTestDB(t), time.Second, BuilderOptions{InstallationTokens: source})
	t.Cleanup(handler.Close)
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"method":"tools/call","params":{"name":"resolve_review_thread","arguments":{"threadId":"PRRT_forged"}}}`))
	request.Header.Set("X-Authenticated-User", "scottlz0310-user")
	request.Header.Set("Authorization", "Bearer arbitrary-token")
	recorder := httptest.NewRecorder()
	middleware.AppProxyAuth(strings.Repeat("s", 32))(handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || source.calls != 0 {
		t.Fatalf("status=%d token発行回数=%d", recorder.Code, source.calls)
	}
	ctx := context.WithValue(context.Background(), middleware.ContextKeyLogin, "scottlz0310-user")
	ctx = context.WithValue(ctx, middleware.ContextKeyToken, "arbitrary-token")
	if _, err := newInstallationClientProvider(time.Second, source)(ctx, nil); err == nil || source.calls != 0 {
		t.Fatal("未検証のidentity contextでinstallation tokenを発行しました")
	}
}
