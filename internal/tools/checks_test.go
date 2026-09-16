package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/autherr"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
)

const testCommitSHA = "0123456789abcdef0123456789abcdef01234567"

func TestListCheckRunsForSHAHandlerRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		in   ListCheckRunsForSHAInput
	}{
		{name: "missing owner", in: ListCheckRunsForSHAInput{Repo: "r", SHA: testCommitSHA}},
		{name: "missing repo", in: ListCheckRunsForSHAInput{Owner: "o", SHA: testCommitSHA}},
		{name: "missing sha", in: ListCheckRunsForSHAInput{Owner: "o", Repo: "r"}},
		{name: "short sha", in: ListCheckRunsForSHAInput{Owner: "o", Repo: "r", SHA: testCommitSHA[:7]}},
		{name: "uppercase sha", in: ListCheckRunsForSHAInput{Owner: "o", Repo: "r", SHA: "0123456789ABCDEF0123456789ABCDEF01234567"}},
		{name: "branch name", in: ListCheckRunsForSHAInput{Owner: "o", Repo: "r", SHA: "main"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := listCheckRunsForSHAHandler(func(_ context.Context, _ *mcp.CallToolRequest) (*ghclient.Client, error) {
				t.Fatal("client provider must not be called for invalid input")
				return nil, nil
			})
			result, _, err := handler(context.Background(), nil, tt.in)
			if err == nil {
				t.Fatalf("handler() error = nil, result = %+v; want validation error", result)
			}
		})
	}
}

func TestListCheckRunsForSHAHandlerAuthErrors(t *testing.T) {
	tests := []struct {
		name     string
		provider func(t *testing.T) githubClientProvider
		want     autherr.AuthErrorType
	}{
		{
			name: "missing token",
			provider: func(_ *testing.T) githubClientProvider {
				return errorProvider(autherr.NewAuthRequired())
			},
			want: autherr.AUTH_REQUIRED,
		},
		{
			name: "GitHub returns 401",
			provider: func(t *testing.T) githubClientProvider {
				srv := new401Server()
				t.Cleanup(srv.Close)
				return staticProvider(make401GitHubClient(srv))
			},
			want: autherr.REAUTH_REQUIRED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := listCheckRunsForSHAHandler(tt.provider(t))
			result, _, err := handler(context.Background(), nil, ListCheckRunsForSHAInput{Owner: "o", Repo: "r", SHA: testCommitSHA})
			assertAuthResult(t, result, err, tt.want)
		})
	}
}

func TestListCheckRunsForSHAHandlerOutput(t *testing.T) {
	tests := []struct {
		name  string
		pages []string
		want  string
	}{
		{
			name:  "empty result",
			pages: []string{`{"total_count":0,"check_runs":[]}`},
			want: `{"sha":"` + testCommitSHA + `","check_runs":[],` +
				`"deduplication":{"strategy":"latest_id_per_app_and_name","raw_count":0},` +
				`"pagination":{"pageCount":1,"complete":true}}`,
		},
		{
			name: "rerun across pages keeps latest run and pending conclusion is null",
			pages: []string{
				`{"total_count":3,"check_runs":[` +
					`{"id":1,"name":"build","head_sha":"` + testCommitSHA + `","status":"completed","conclusion":"failure","html_url":"https://example.test/runs/1","app":{"id":15368,"slug":"github-actions"}}]}`,
				`{"total_count":3,"check_runs":[` +
					`{"id":2,"name":"build","head_sha":"` + testCommitSHA + `","status":"in_progress","conclusion":null,"html_url":"https://example.test/runs/2","app":{"id":15368,"slug":"github-actions"}},` +
					`{"id":3,"name":"lint","head_sha":"` + testCommitSHA + `","status":"completed","conclusion":"skipped","html_url":"https://example.test/runs/3","app":{"id":15368,"slug":"github-actions"}}]}`,
			},
			want: `{"sha":"` + testCommitSHA + `","check_runs":[` +
				`{"id":2,"name":"build","head_sha":"` + testCommitSHA + `","status":"in_progress","conclusion":null,"app":{"id":15368,"slug":"github-actions"},"html_url":"https://example.test/runs/2"},` +
				`{"id":3,"name":"lint","head_sha":"` + testCommitSHA + `","status":"completed","conclusion":"skipped","app":{"id":15368,"slug":"github-actions"},"html_url":"https://example.test/runs/3"}],` +
				`"deduplication":{"strategy":"latest_id_per_app_and_name","raw_count":3},` +
				`"pagination":{"pageCount":2,"complete":true}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != fmt.Sprintf("/repos/o/r/commits/%s/check-runs", testCommitSHA) {
					http.NotFound(w, r)
					return
				}
				page := 1
				if p := r.URL.Query().Get("page"); p != "" {
					if _, err := fmt.Sscan(p, &page); err != nil || page < 1 || page > len(tt.pages) {
						http.Error(w, "unexpected page", http.StatusBadRequest)
						return
					}
				}
				if page < len(tt.pages) {
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tt.pages[page-1])
			}))
			t.Cleanup(srv.Close)

			client, err := ghclient.NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
			if err != nil {
				t.Fatalf("NewWithHTTPClientAndURLFull() error = %v", err)
			}
			handler := listCheckRunsForSHAHandler(staticProvider(client))
			result, output, err := handler(context.Background(), nil, ListCheckRunsForSHAInput{Owner: "o", Repo: "r", SHA: testCommitSHA})
			if err != nil {
				t.Fatalf("handler() error = %v", err)
			}
			if result != nil {
				t.Fatalf("handler() result = %+v, want nil", result)
			}
			got, err := json.Marshal(output)
			if err != nil {
				t.Fatalf("json.Marshal(output) error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}
