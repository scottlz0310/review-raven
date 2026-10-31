package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReviewCycleCommentInput は実装側の再依頼または対応サマリを指定する。
type ReviewCycleCommentInput struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	HeadSHA string `json:"headSha"`
	Kind    string `json:"kind"`
	Body    string `json:"body"`
}

// ReviewCycleCommentOutput は投稿結果の照合に使う識別情報を返す。
type ReviewCycleCommentOutput struct {
	CommentID string `json:"commentId"`
	URL       string `json:"url"`
	Author    string `json:"author"`
	HeadSHA   string `json:"headSha"`
}

var reviewCycleCommentTool = &mcp.Tool{
	Name:        "post_review_cycle_comment",
	Description: "専用AppでOPEN PRの固定HEADに、kind=re_review_request（再レビュー依頼）またはcycle_summary（レビュー対応サマリ）を投稿する。既存skillの見出しとサイクル状態を必須とし、reviewer Verdictは投稿しない。投稿結果不明時の自動再試行なし。返却IDでread_pr_commentsから実投稿者・本文を読戻して確認する。queue登録やレビュー開始は行わない。",
}

var cycleStatePattern = regexp.MustCompile("(?m)^### サイクル状態\n- cycles_done: [0-9]+\n- max_cycles: [1-9][0-9]*\n- expected_head: `([0-9a-f]{40})`\n- handled_comments:( [0-9]+(, [0-9]+)*| なし| )?\n?\\z")

func validateCycleComment(in ReviewCycleCommentInput) error {
	if strings.TrimSpace(in.Owner) == "" || strings.TrimSpace(in.Repo) == "" || in.PR <= 0 || !commitSHAPattern.MatchString(in.HeadSHA) {
		return fmt.Errorf("owner, repo, positive pr and full lowercase headSha are required")
	}
	var heading string
	switch in.Kind {
	case "re_review_request":
		heading = "@thread-owl re-review requested"
	case "cycle_summary":
		heading = "## レビュー対応サマリ（thread-owl）"
	default:
		return fmt.Errorf("kind must be re_review_request or cycle_summary")
	}
	if !strings.HasPrefix(in.Body, heading+"\n\n") || strings.Count(in.Body, heading) != 1 {
		return fmt.Errorf("body must begin with the single heading for kind")
	}
	if strings.Contains(in.Body, "Review Verdict") || (in.Kind == "cycle_summary" && strings.Contains(strings.ToLower(in.Body), "@thread-owl re-review requested")) {
		return fmt.Errorf("reviewer Verdict and re-review triggers in a summary are not allowed")
	}
	// サイクル判断はskillに残し、記録の対象HEADと単一性だけを固定する。
	for _, marker := range []string{"### サイクル状態", "- cycles_done:", "- max_cycles:", "- expected_head:", "- handled_comments:"} {
		if strings.Count(in.Body, marker) != 1 {
			return fmt.Errorf("body must contain one cycle state block")
		}
	}
	state := cycleStatePattern.FindStringSubmatch(in.Body)
	if len(state) == 0 || state[1] != in.HeadSHA {
		return fmt.Errorf("cycle state must end the body and expected_head must match headSha")
	}
	return nil
}

func reviewCycleCommentHandler(provider githubClientProvider) func(context.Context, *mcp.CallToolRequest, ReviewCycleCommentInput) (*mcp.CallToolResult, ReviewCycleCommentOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ReviewCycleCommentInput) (*mcp.CallToolResult, ReviewCycleCommentOutput, error) {
		in.Body = strings.ReplaceAll(in.Body, "\r\n", "\n")
		if err := validateCycleComment(in); err != nil {
			return nil, ReviewCycleCommentOutput{}, err
		}
		gh, err := provider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ReviewCycleCommentOutput{}, nil
			}
			return nil, ReviewCycleCommentOutput{}, err
		}
		comment, err := gh.PostReviewCycleComment(ctx, in.Owner, in.Repo, in.PR, in.HeadSHA, in.Body)
		if err != nil {
			return nil, ReviewCycleCommentOutput{}, err
		}
		return nil, ReviewCycleCommentOutput{CommentID: comment.ID, URL: comment.URL, Author: comment.Author, HeadSHA: in.HeadSHA}, nil
	}
}

// RegisterReviewCycleCommentTool は専用Appの公開面だけに投稿APIを追加する。
func RegisterReviewCycleCommentTool(server *mcp.Server, provider githubClientProvider) {
	mcp.AddTool(server, reviewCycleCommentTool, reviewCycleCommentHandler(provider))
}
