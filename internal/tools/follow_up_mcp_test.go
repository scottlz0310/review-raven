package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/scottlz0310/review-raven/internal/autherr"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
)

func TestFollowUpMCPInputAndErrorContract(t *testing.T) {
	for _, tool := range []string{"reply_to_pr_comment", "create_follow_up_issue", "get_follow_up_issue"} {
		for _, invalid := range []bool{true, false} {
			t.Run(tool+map[bool]string{true: "不正入力", false: "認証失敗"}[invalid], func(t *testing.T) {
				calls := 0
				server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
				RegisterFollowUpTools(server, func(context.Context, *mcp.CallToolRequest) (*ghclient.Client, error) {
					calls++
					return nil, autherr.NewAuthRequired()
				}, []string{"raven"}, "raven[bot]", true)
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
				in := map[string]any{"owner": "o", "repo": "r", "pr": 1, "expectedHeadSha": strings.Repeat("a", 40), "source": map[string]any{"kind": "issue_comment", "id": "3"}}
				if tool == "get_follow_up_issue" {
					in["issueNumber"] = 7
				} else {
					in["expectedSnapshotId"] = strings.Repeat("b", 64)
					in["body"] = "修正しました"
				}
				if tool == "create_follow_up_issue" {
					in["reason"] = "deferred"
					in["title"] = "後続"
				}
				if invalid {
					in["expectedHeadSha"] = "short"
				}
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: in})
				if err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(res.StructuredContent)
				var envelope followUpErrorEnvelope
				if err = json.Unmarshal(data, &envelope); err != nil {
					t.Fatal(err)
				}
				want := "AUTH_REQUIRED"
				count := 1
				if invalid {
					want = "INVALID_INPUT"
					count = 0
				}
				if !res.IsError || envelope.Error == nil || envelope.Error.Code != want || envelope.Error.MayHaveChanged || calls != count {
					t.Fatalf("result=%s calls=%d", data, calls)
				}
				if len(res.Content) != 1 {
					t.Fatalf("text envelopeなし: %v", res.Content)
				}
				text, ok := res.Content[0].(*mcp.TextContent)
				if !ok || text.Text != string(data) {
					t.Fatalf("text/structured不一致: %v %s", res.Content, data)
				}
			})
		}
	}
}
