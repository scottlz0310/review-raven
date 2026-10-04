package tools

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStreamableHandlerServesTrustedCommentAuthors pins the wiring from
// BuilderOptions.TrustedCommentAuthors (the TRUSTED_COMMENT_AUTHORS environment
// variable) to the tool, over the negotiated 2026-07-28 protocol. An empty
// allowlist must surface as a tool error, never as an empty list.
func TestStreamableHandlerServesTrustedCommentAuthors(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		wantLogins []string
		wantError  bool
	}{
		{name: "configured allowlist is published", configured: []string{"thread-owl", "codecov"}, wantLogins: []string{"thread-owl", "codecov"}},
		{name: "unset allowlist fails closed", configured: nil, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openServerTestDB(t)
			handler := BuildStreamableHandlerWithOptions(db, 30*time.Second, BuilderOptions{TrustedCommentAuthors: tt.configured})
			t.Cleanup(handler.Close)

			httpServer := httptest.NewServer(withAuthContext(handler, map[string]string{"token-a": "alice"}))
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

			listed, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("ListTools() error = %v", err)
			}
			found := false
			for _, tool := range listed.Tools {
				if tool.Name == "get_trusted_comment_authors" {
					found = true
				}
			}
			if !found {
				t.Fatal("get_trusted_comment_authors is not registered on the streamable handler")
			}

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "get_trusted_comment_authors",
				Arguments: map[string]any{},
			})
			if err != nil {
				t.Fatalf("CallTool() protocol error = %v", err)
			}
			if res.IsError != tt.wantError {
				t.Fatalf("res.IsError = %v, want %v; content = %v", res.IsError, tt.wantError, res.Content)
			}
			if tt.wantError {
				return
			}
			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("json.Marshal(StructuredContent) error = %v", err)
			}
			var out TrustedCommentAuthorsOutput
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("json.Unmarshal() error = %v, raw = %s", err, raw)
			}
			if !reflect.DeepEqual(out.Logins, tt.wantLogins) {
				t.Errorf("logins = %#v, want %#v", out.Logins, tt.wantLogins)
			}
		})
	}
}
