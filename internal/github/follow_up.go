package ghclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/go-github/v93/github"
	"github.com/shurcooL/githubv4"
)

// FollowUpIssueはmetadata-only取得では本文・titleを要求しない。
type FollowUpIssue struct {
	Number     int               `json:"number"`
	ID         string            `json:"id"`
	URL        string            `json:"url"`
	State      string            `json:"state"`
	Author     *DescriptionActor `json:"author"`
	Editor     *DescriptionActor `json:"editor"`
	EditedAt   string            `json:"editedAt"`
	Title      string            `json:"title"`
	Body       string            `json:"body"`
	BodySHA256 string            `json:"bodySha256"`
	Labels     []string          `json:"labels"`
}

type FollowUpComment struct {
	ID         string           `json:"id"`
	URL        string           `json:"url"`
	Author     DescriptionActor `json:"author"`
	Body       string           `json:"-"`
	BodySHA256 string           `json:"bodySha256"`
}

func (c *Client) FollowUpBot(ctx context.Context, login string) (DescriptionActor, error) {
	if login == "" {
		return DescriptionActor{}, fmt.Errorf("認証済みApp identityがありません")
	}
	u, _, err := c.gh.Users.Get(ctx, login)
	if err != nil {
		return DescriptionActor{}, err
	}
	if u.GetNodeID() == "" || u.GetType() != "Bot" || !strings.EqualFold(u.GetLogin(), login) {
		return DescriptionActor{}, fmt.Errorf("認証済みAppのBot identityが一致しません")
	}
	return DescriptionActor{NodeID: u.GetNodeID(), Login: u.GetLogin(), Type: u.GetType()}, nil
}

func followUpComment(c *github.IssueComment) FollowUpComment {
	return FollowUpComment{ID: strconv.FormatInt(c.GetID(), 10), URL: c.GetHTMLURL(),
		Author: DescriptionActor{NodeID: c.GetUser().GetNodeID(), Login: c.GetUser().GetLogin(), Type: c.GetUser().GetType()},
		Body:   c.GetBody(), BodySHA256: descriptionHash(c.GetBody())}
}

func (c *Client) CreateFollowUpReply(ctx context.Context, in DescriptionTarget, body string) (FollowUpComment, error) {
	comment, _, err := c.gh.Issues.CreateComment(ctx, in.Owner, in.Repo, in.PR, github.IssueCommentRequest{Body: body})
	if err != nil {
		return FollowUpComment{}, err
	}
	return followUpComment(comment), nil
}

func (c *Client) GetFollowUpReply(ctx context.Context, in DescriptionTarget, id string) (FollowUpComment, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return FollowUpComment{}, fmt.Errorf("返信IDが不正です")
	}
	comment, _, err := c.gh.Issues.GetComment(ctx, in.Owner, in.Repo, n)
	if err != nil {
		return FollowUpComment{}, err
	}
	return followUpComment(comment), nil
}

func (c *Client) CreateFollowUpIssue(ctx context.Context, in DescriptionTarget, title, body string, labels []string) (FollowUpIssue, error) {
	i, _, err := c.gh.Issues.Create(ctx, in.Owner, in.Repo, github.CreateIssueRequest{Title: title, Body: &body, Labels: labels})
	if err != nil {
		return FollowUpIssue{}, err
	}
	return FollowUpIssue{Number: i.GetNumber(), ID: i.GetNodeID(), URL: i.GetHTMLURL()}, nil
}

func (c *Client) CheckFollowUpLabels(ctx context.Context, in DescriptionTarget, labels []string) error {
	for _, name := range labels {
		label, _, err := c.gh.Issues.GetLabel(ctx, in.Owner, in.Repo, name)
		if err != nil {
			return err
		}
		if label.GetName() != name {
			return fmt.Errorf("既存label名が一致しません")
		}
	}
	return nil
}

func (c *Client) GetFollowUpIssue(ctx context.Context, in DescriptionTarget, number int, withBody bool) (FollowUpIssue, error) {
	var q struct {
		Repository struct {
			Issue *struct {
				Number       int
				ID           githubv4.String
				URL          githubv4.String
				State        githubv4.String
				Author       *descriptionQueryActor
				Editor       *descriptionQueryActor
				LastEditedAt *githubv4.DateTime
				Title        githubv4.String `graphql:"title: title @include(if:$withBody)"`
				Body         githubv4.String `graphql:"body: body @include(if:$withBody)"`
				Labels       struct {
					Nodes    []struct{ Name githubv4.String }
					PageInfo pageInfo
				} `graphql:"labels(first:100,after:$cursor)"`
			} `graphql:"issue(number:$number)"`
		} `graphql:"repository(owner:$owner,name:$repo)"`
	}
	vars := map[string]any{"owner": githubv4.String(in.Owner), "repo": githubv4.String(in.Repo), "number": githubv4.Int(number), "withBody": githubv4.Boolean(withBody), "cursor": (*githubv4.String)(nil)}
	var out FollowUpIssue
	for {
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return FollowUpIssue{}, err
		}
		i := q.Repository.Issue
		if i == nil || i.Number != number || i.ID == "" || i.URL == "" || (i.State != "OPEN" && i.State != "CLOSED") {
			return FollowUpIssue{}, fmt.Errorf("同repoの通常Issueを確認できません")
		}
		a, err := descriptionActor(i.Author)
		if err != nil {
			return FollowUpIssue{}, err
		}
		e, err := descriptionActor(i.Editor)
		if err != nil {
			return FollowUpIssue{}, err
		}
		edited := ""
		if i.LastEditedAt != nil {
			edited = i.LastEditedAt.Format("2006-01-02T15:04:05.999999999Z07:00")
		}
		page := FollowUpIssue{Number: number, ID: string(i.ID), URL: string(i.URL), State: string(i.State), Author: a, Editor: e, EditedAt: edited, Title: string(i.Title), Body: string(i.Body), Labels: []string{}}
		if out.ID != "" && !sameIssueMetadata(out, page) {
			return FollowUpIssue{}, fmt.Errorf("Issueがページング中に変化しました")
		}
		labels := out.Labels
		out = page
		out.Labels = labels
		for _, label := range i.Labels.Nodes {
			out.Labels = append(out.Labels, string(label.Name))
		}
		next, err := nextCursor(i.Labels.PageInfo)
		if err != nil {
			return FollowUpIssue{}, err
		}
		if next == nil {
			break
		}
		vars["cursor"] = next
	}
	if withBody {
		out.BodySHA256 = descriptionHash(out.Body)
	}
	return out, nil
}

func sameActor(a, b *DescriptionActor) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func sameIssueMetadata(a, b FollowUpIssue) bool {
	return a.ID == b.ID && a.Number == b.Number && a.URL == b.URL && a.State == b.State && a.EditedAt == b.EditedAt && sameActor(a.Author, b.Author) && sameActor(a.Editor, b.Editor)
}

// 検索ではtitle/bodyを選択しない。1000件制限や全件性不明は新規作成を止める。
func (c *Client) FindFollowUpIssues(ctx context.Context, in DescriptionTarget, marker string) ([]int, error) {
	query := fmt.Sprintf("repo:%s/%s is:issue in:body %q", in.Owner, in.Repo, marker)
	vars := map[string]any{"query": githubv4.String(query), "cursor": (*githubv4.String)(nil)}
	out := []int{}
	seen := map[int]bool{}
	count := -1
	for {
		var q struct {
			Search struct {
				IssueCount int
				Nodes      []struct {
					Issue struct {
						Number     int
						Repository struct{ NameWithOwner githubv4.String }
					} `graphql:"... on Issue"`
				}
				PageInfo pageInfo
			} `graphql:"search(query:$query,type:ISSUE,first:100,after:$cursor)"`
		}
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return nil, err
		}
		if q.Search.IssueCount > 1000 || (count >= 0 && count != q.Search.IssueCount) {
			return nil, fmt.Errorf("Issue検索の全件性を確認できません")
		}
		count = q.Search.IssueCount
		for _, n := range q.Search.Nodes {
			if n.Issue.Number <= 0 || !strings.EqualFold(string(n.Issue.Repository.NameWithOwner), in.Owner+"/"+in.Repo) || seen[n.Issue.Number] {
				return nil, fmt.Errorf("Issue検索結果の所属または全件性を確認できません")
			}
			seen[n.Issue.Number] = true
			out = append(out, n.Issue.Number)
		}
		next, err := nextCursor(q.Search.PageInfo)
		if err != nil {
			return nil, err
		}
		if next == nil {
			break
		}
		vars["cursor"] = next
	}
	if len(out) != count {
		return nil, fmt.Errorf("Issue検索が不完全です")
	}
	return out, nil
}
