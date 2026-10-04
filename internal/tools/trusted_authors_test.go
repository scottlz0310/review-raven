package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callTrustedAuthorsTool exercises the tool through the real MCP server/client
// wire path, so output schema validation runs (an error result must not be
// replaced by a generic "validating tool output" error).
func callTrustedAuthorsTool(t *testing.T, logins []string) *mcp.CallToolResult {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	RegisterTrustedCommentAuthorsTool(srv, logins)

	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("srv.Connect() error = %v", err)
	}
	defer func() { _ = ss.Wait() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_trusted_comment_authors",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool() protocol error = %v", err)
	}
	return res
}

func TestTrustedCommentAuthorsToolConfigured(t *testing.T) {
	tests := []struct {
		name   string
		logins []string
	}{
		{name: "single login", logins: []string{"thread-owl"}},
		{name: "several logins keep their order", logins: []string{"scottlz0310-user", "codecov", "thread-owl"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := callTrustedAuthorsTool(t, tt.logins)
			if res.IsError {
				t.Fatalf("res.IsError = true, want false; content = %v", res.Content)
			}
			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("json.Marshal(StructuredContent) error = %v", err)
			}
			var out TrustedCommentAuthorsOutput
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("json.Unmarshal() error = %v, raw = %s", err, raw)
			}
			if !reflect.DeepEqual(out.Logins, tt.logins) {
				t.Errorf("logins = %#v, want %#v", out.Logins, tt.logins)
			}
		})
	}
}

func TestTrustedCommentAuthorsToolNotConfigured(t *testing.T) {
	tests := []struct {
		name   string
		logins []string
	}{
		{name: "nil allowlist (unset)", logins: nil},
		{name: "empty allowlist", logins: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := callTrustedAuthorsTool(t, tt.logins)
			if !res.IsError {
				t.Fatal("res.IsError = false, want true (an empty allowlist must not be returned as an empty list)")
			}
			if len(res.Content) == 0 {
				t.Fatal("res.Content is empty")
			}
			tc, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("res.Content[0] is %T, want *mcp.TextContent", res.Content[0])
			}
			var e trustedAuthorsError
			if err := json.Unmarshal([]byte(tc.Text), &e); err != nil {
				t.Fatalf("json.Unmarshal() error = %v, text = %s", err, tc.Text)
			}
			if e.ErrorType != errTrustedAuthorsNotConfigured {
				t.Errorf("error_type = %q, want %q", e.ErrorType, errTrustedAuthorsNotConfigured)
			}
		})
	}
}

func TestTrustedAuthorsHandlerCopiesTheAllowlist(t *testing.T) {
	logins := []string{"codecov", "thread-owl"}
	handler := trustedAuthorsHandler(logins)

	// The input slice is mutated after registration.
	logins[0] = "attacker"

	_, out, err := handler(context.Background(), nil, TrustedCommentAuthorsInput{})
	if err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	if want := []string{"codecov", "thread-owl"}; !reflect.DeepEqual(out.Logins, want) {
		t.Fatalf("logins = %#v, want %#v (the allowlist must not follow later mutation of the input)", out.Logins, want)
	}

	// The returned slice is mutated by the caller.
	out.Logins[0] = "attacker"
	_, again, _ := handler(context.Background(), nil, TrustedCommentAuthorsInput{})
	if want := []string{"codecov", "thread-owl"}; !reflect.DeepEqual(again.Logins, want) {
		t.Fatalf("second call logins = %#v, want %#v (the handler must return a copy)", again.Logins, want)
	}
}
