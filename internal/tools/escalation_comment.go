package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EscalationCommentInput はエスカレーション対象の PR と失敗を指定する。
type EscalationCommentInput struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	HeadSHA string `json:"headSha"`
	Reason  string `json:"reason"`
}

// EscalationCommentOutput は投稿した GitHub コメントを識別する。
type EscalationCommentOutput struct {
	CommentID string `json:"commentId"`
	URL       string `json:"url"`
}

var escalationCommentTool = &mcp.Tool{
	Name:        "post_runner_escalation",
	Description: "ランナーが会話への通知にも失敗した場合に限り、OPEN PRの固定HEADを確認して定型のエスカレーションコメントを投稿する。投稿結果が不明な場合は自動再試行しない。",
}

func escalationCommentHandler(provider githubClientProvider) func(context.Context, *mcp.CallToolRequest, EscalationCommentInput) (*mcp.CallToolResult, EscalationCommentOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in EscalationCommentInput) (*mcp.CallToolResult, EscalationCommentOutput, error) {
		if strings.TrimSpace(in.Owner) == "" || strings.TrimSpace(in.Repo) == "" || in.PR <= 0 || !commitSHAPattern.MatchString(in.HeadSHA) {
			return nil, EscalationCommentOutput{}, fmt.Errorf("owner, repo, positive pr and full lowercase headSha are required")
		}
		if in.Reason != "resume_unresponsive" {
			return nil, EscalationCommentOutput{}, fmt.Errorf("unsupported escalation reason")
		}
		gh, err := provider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, EscalationCommentOutput{}, nil
			}
			return nil, EscalationCommentOutput{}, err
		}
		body := fmt.Sprintf("## ランナーからの対応依頼\n\n対象 HEAD: `%s`\n\n実装側セッションへの通知が応答しませんでした。PR の状態と実行結果を確認し、必要に応じて Squirrel Notifier の既存 resume から再開してください。\n", in.HeadSHA)
		id, url, err := gh.PostEscalationComment(ctx, in.Owner, in.Repo, in.PR, in.HeadSHA, body)
		if err != nil {
			return nil, EscalationCommentOutput{}, err
		}
		return nil, EscalationCommentOutput{CommentID: id, URL: url}, nil
	}
}

// RegisterEscalationCommentTool は専用 App モードで投稿 tool を登録する。
func RegisterEscalationCommentTool(server *mcp.Server, provider githubClientProvider) {
	mcp.AddTool(server, escalationCommentTool, escalationCommentHandler(provider))
}
