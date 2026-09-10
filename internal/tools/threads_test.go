package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	ghclient "github.com/scottlz0310/review-raven/internal/github"
)

func TestGetReviewThreadsHandlerMetadataOnlyOmitsBody(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode GraphQL request: %v", err)
		}
		query = request.Query
		response := map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"reviewThreads": map[string]any{
							"nodes": []any{map[string]any{
								"id":         "PRRT_metadata",
								"isResolved": false,
								"isOutdated": false,
								"path":       "main.go",
								"line":       42,
								"startLine":  42,
								"comments": map[string]any{
									"nodes": []any{map[string]any{
										"databaseId": 42,
										"url":        "https://github.com/example/review/42",
										"author": map[string]any{
											"login":      "thread-owl[bot]",
											"__typename": "Bot",
										},
										"createdAt": "2026-09-10T00:00:00Z",
									}},
								},
							}},
							"pageInfo": map[string]any{
								"hasNextPage": false,
								"endCursor":   "cursor-1",
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(srv.Close)

	client, err := ghclient.NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
	if err != nil {
		t.Fatalf("NewWithHTTPClientAndURLFull() error = %v", err)
	}
	handler := getReviewThreadsHandler(func(_ context.Context, _ *mcp.CallToolRequest) (*ghclient.Client, error) {
		return client, nil
	})
	includeBodies := false
	result, output, err := handler(context.Background(), nil, GetReviewThreadsInput{
		Owner:         "example",
		Repo:          "repo",
		PR:            1,
		IncludeBodies: &includeBodies,
	})
	if err != nil {
		t.Fatalf("handler returned unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("handler result = %v, want nil", result)
	}
	if len(output.Threads) != 1 || len(output.Threads[0].Comments) != 1 {
		t.Fatalf("output thread/comment counts = %d/%d, want 1/1", len(output.Threads), len(output.Threads[0].Comments))
	}
	comment := output.Threads[0].Comments[0]
	if comment.CommentID != "42" {
		t.Errorf("CommentID = %q, want 42", comment.CommentID)
	}
	if comment.AuthorType != "Bot" {
		t.Errorf("AuthorType = %q, want Bot", comment.AuthorType)
	}
	if comment.Body != nil {
		t.Errorf("Body = %q, want nil", *comment.Body)
	}
	if output.Pagination.PageCount != 1 || !output.Pagination.Complete {
		t.Errorf("Pagination = %+v, want pageCount=1 and complete=true", output.Pagination)
	}
	if strings.Contains(strings.ToLower(query), "body") {
		t.Errorf("metadata-only GraphQL query selected body: %s", query)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("json.Marshal(output) error = %v", err)
	}
	if strings.Contains(string(encoded), `"body"`) {
		t.Errorf("metadata-only output contains body: %s", encoded)
	}
}

func TestGetReviewThreadsSchemaAdvertisesOptionalBodyProjection(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	RegisterGetReviewThreadsTool(server, func(_ context.Context, _ *mcp.CallToolRequest) (*ghclient.Client, error) {
		return nil, context.Canceled
	})
	serverConnection, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = serverConnection.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	var tool *mcp.Tool
	for _, candidate := range tools.Tools {
		if candidate.Name == "get_review_threads" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("get_review_threads tool was not advertised")
	}

	inputSchemaJSON, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("json.Marshal(input schema) error = %v", err)
	}
	var inputSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(inputSchemaJSON, &inputSchema); err != nil {
		t.Fatalf("json.Unmarshal(input schema) error = %v; schema=%s", err, inputSchemaJSON)
	}
	if _, ok := inputSchema.Properties["include_bodies"]; !ok {
		t.Fatalf("input schema does not advertise include_bodies: %s", inputSchemaJSON)
	}
	for _, required := range inputSchema.Required {
		if required == "include_bodies" {
			t.Fatalf("include_bodies is required but should be optional: %s", inputSchemaJSON)
		}
	}
}
