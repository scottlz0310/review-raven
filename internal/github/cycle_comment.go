package ghclient

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/go-github/v93/github"
)

// ReviewCycleComment はGitHubが返した投稿IDと実投稿者を保持する。
type ReviewCycleComment struct {
	ID     string
	URL    string
	Author string
}

// PostReviewCycleComment はOPEN PRの期待HEADを確認してから1回だけ投稿する。
// 確認と投稿は不可分ではなく、失敗時には結果不明の可能性がある。
func (c *Client) PostReviewCycleComment(ctx context.Context, owner, repo string, prNumber int, expectedSHA, body string) (ReviewCycleComment, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		return ReviewCycleComment{}, fmt.Errorf("failed to get PR before cycle comment: %w", err)
	}
	if pr.GetState() != "open" {
		return ReviewCycleComment{}, fmt.Errorf("PR is not open")
	}
	if pr.GetHead().GetSHA() != expectedSHA {
		return ReviewCycleComment{}, fmt.Errorf("PR head does not match expected SHA")
	}
	comment, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, prNumber, github.IssueCommentRequest{Body: body})
	if err != nil {
		return ReviewCycleComment{}, fmt.Errorf("failed to post cycle comment; outcome may be unknown: %w", err)
	}
	if comment.GetID() <= 0 || comment.GetHTMLURL() == "" || comment.GetUser().GetLogin() == "" {
		return ReviewCycleComment{}, fmt.Errorf("cycle comment response is incomplete; outcome may be unknown")
	}
	return ReviewCycleComment{ID: strconv.FormatInt(comment.GetID(), 10), URL: comment.GetHTMLURL(), Author: comment.GetUser().GetLogin()}, nil
}
