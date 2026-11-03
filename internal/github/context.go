package ghclient

import (
	"context"
	"fmt"
	"math"

	"github.com/shurcooL/githubv4"

	"github.com/scottlz0310/review-raven/internal/reader"
)

// PRContextは説明・コメント本文を含まない取得時点のPR参照情報。
type PRContext struct {
	URL           string `json:"url"`
	State         string `json:"state"`
	HeadSHA       string `json:"headSha"`
	BaseSHA       string `json:"baseSha"`
	BaseRefName   string `json:"baseRefName"`
	PolicyBaseSHA string `json:"policyBaseSha"`
}

func (c *Client) GetPRContext(ctx context.Context, in DescriptionTarget) (PRContext, error) {
	if err := in.Validate(); err != nil {
		return PRContext{}, err
	}
	if in.PR > math.MaxInt32 {
		return PRContext{}, fmt.Errorf("PR番号がGraphQL Intの範囲外です")
	}
	var q struct {
		Repository struct {
			PullRequest *struct {
				pullRequestRefs
				URL   githubv4.String
				State githubv4.String
			} `graphql:"pullRequest(number:$pr)"`
		} `graphql:"repository(owner:$owner,name:$repo)"`
	}
	if err := c.v4.Query(ctx, &q, map[string]any{
		"owner": githubv4.String(in.Owner), "repo": githubv4.String(in.Repo), "pr": githubv4.Int(int32(in.PR)),
	}); err != nil {
		return PRContext{}, fmt.Errorf("PR参照情報の取得に失敗: %w", err)
	}
	p := q.Repository.PullRequest
	if p == nil || p.URL == "" || !descriptionHeadPattern.MatchString(string(p.HeadRefOid)) ||
		!descriptionHeadPattern.MatchString(string(p.BaseRefOid)) ||
		(p.State != "OPEN" && p.State != "CLOSED" && p.State != "MERGED") {
		return PRContext{}, fmt.Errorf("PR参照情報が不完全です")
	}
	if p.BaseRef == nil || p.BaseRefName == "" || p.BaseRef.Name != p.BaseRefName ||
		p.BaseRef.Prefix != "refs/heads/" || p.BaseRef.Target.TypeName != "Commit" ||
		!descriptionHeadPattern.MatchString(string(p.BaseRef.Target.Oid)) {
		return PRContext{}, reader.ErrPolicyRefUnavailable
	}
	return PRContext{URL: string(p.URL), State: string(p.State), HeadSHA: string(p.HeadRefOid),
		BaseSHA: string(p.BaseRefOid), BaseRefName: string(p.BaseRefName), PolicyBaseSHA: string(p.BaseRef.Target.Oid)}, nil
}
