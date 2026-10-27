package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
)

func TestEscalationCommentHandler(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		head       string
		postStatus int
		wantPosts  int
		wantError  bool
	}{
		{name: "open matching head", state: "open", head: testCommitSHA, postStatus: 201, wantPosts: 1},
		{name: "moved head", state: "open", head: strings.Repeat("a", 40), wantError: true},
		{name: "closed PR", state: "closed", head: testCommitSHA, wantError: true},
		{name: "write error is not retried", state: "open", head: testCommitSHA, postStatus: 500, wantPosts: 1, wantError: true},
		{name: "incomplete write response", state: "open", head: testCommitSHA, postStatus: 202, wantPosts: 1, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42":
					_, _ = fmt.Fprintf(w, `{"state":%q,"head":{"sha":%q}}`, tt.state, tt.head)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/42/comments":
					posts++
					var body struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Body, testCommitSHA) {
						t.Errorf("投稿内容が不正: %v", err)
					}
					w.WriteHeader(tt.postStatus)
					if tt.postStatus == http.StatusAccepted {
						_, _ = fmt.Fprint(w, `{}`)
					} else {
						_, _ = fmt.Fprint(w, `{"id":123,"html_url":"https://example.test/comment/123"}`)
					}
				default:
					t.Errorf("想定外の要求: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			client, err := ghclient.NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, output, err := escalationCommentHandler(staticProvider(client))(context.Background(), nil, EscalationCommentInput{Owner: "o", Repo: "r", PR: 42, HeadSHA: testCommitSHA, Reason: "resume_unresponsive"})
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v", err)
			}
			if posts != tt.wantPosts {
				t.Errorf("投稿回数 = %d, want %d", posts, tt.wantPosts)
			}
			if !tt.wantError && (output.CommentID != "123" || output.URL != "https://example.test/comment/123") {
				t.Errorf("投稿結果 = %+v", output)
			}
		})
	}
}

func TestEscalationCommentHandlerRejectsInvalidInput(t *testing.T) {
	tests := []EscalationCommentInput{
		{Repo: "r", PR: 42, HeadSHA: testCommitSHA, Reason: "resume_unresponsive"},
		{Owner: "o", Repo: "r", PR: 42, HeadSHA: "main", Reason: "resume_unresponsive"},
		{Owner: "o", Repo: "r", PR: 42, HeadSHA: testCommitSHA, Reason: "other"},
	}
	for _, input := range tests {
		t.Run(fmt.Sprintf("%s/%s/%s", input.Owner, input.HeadSHA, input.Reason), func(t *testing.T) {
			handler := escalationCommentHandler(func(context.Context, *mcp.CallToolRequest) (*ghclient.Client, error) {
				t.Fatal("認証は呼ばれない")
				return nil, nil
			})
			_, _, err := handler(context.Background(), nil, input)
			if err == nil {
				t.Fatal("不正入力が受理されました")
			}
		})
	}
}
