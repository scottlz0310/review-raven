package ghclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/shurcooL/githubv4"

	"github.com/scottlz0310/review-raven/internal/reader"
)

// このファイルは reader.Source の GitHub 側の取得を実装する。
//
// 本文は GraphQL の `@include(if: $withBody)` で選択する。withBody=false の問い合わせは、
// 本文のフィールドを GitHub へ要求しない。投稿者の検証は、その応答で行う。
// タグを `body: body ...` と書くのは、応答の復号がタグの `(` の前までをフィールド名として照合するため
// （ディレクティブをそのまま続けると名前が一致しない）。

type pageInfo struct {
	HasNextPage githubv4.Boolean
	EndCursor   githubv4.String
}

type commentAuthor struct {
	Login    githubv4.String
	TypeName githubv4.String `graphql:"__typename"`
}

// commentCore は、3 種類のコメントに共通するフィールド。
type commentCore struct {
	DatabaseID   int64
	URL          githubv4.String
	CreatedAt    githubv4.DateTime
	LastEditedAt *githubv4.DateTime
	Body         githubv4.String `graphql:"body: body @include(if: $withBody)"`
	Author       commentAuthor
}

type threadCommentsPage struct {
	Nodes    []commentCore
	PageInfo pageInfo
}

type commentThreadNode struct {
	ID         githubv4.ID
	IsResolved githubv4.Boolean
	IsOutdated githubv4.Boolean
	Path       githubv4.String
	Line       *githubv4.Int
	Comments   threadCommentsPage `graphql:"comments(first: 50)"`
}

type pullRequestRefs struct {
	HeadRefOid githubv4.String
	BaseRefOid githubv4.String
}

type commentThreadsQuery struct {
	Repository struct {
		PullRequest struct {
			pullRequestRefs
			ReviewThreads struct {
				Nodes    []commentThreadNode
				PageInfo pageInfo
			} `graphql:"reviewThreads(first: 100, after: $cursor)"`
		} `graphql:"pullRequest(number: $pr)"`
	} `graphql:"repository(owner: $owner, name: $repo)"`
}

type threadCommentsContinuationQuery struct {
	Node struct {
		PullRequestReviewThread struct {
			Comments threadCommentsPage `graphql:"comments(first: 50, after: $cursor)"`
		} `graphql:"... on PullRequestReviewThread"`
	} `graphql:"node(id: $id)"`
}

type reviewNode struct {
	commentCore
	State githubv4.String
}

type reviewsQuery struct {
	Repository struct {
		PullRequest struct {
			pullRequestRefs
			Reviews struct {
				Nodes    []reviewNode
				PageInfo pageInfo
			} `graphql:"reviews(first: 100, after: $cursor)"`
		} `graphql:"pullRequest(number: $pr)"`
	} `graphql:"repository(owner: $owner, name: $repo)"`
}

type issueCommentsQuery struct {
	Repository struct {
		PullRequest struct {
			pullRequestRefs
			Comments struct {
				Nodes    []commentCore
				PageInfo pageInfo
			} `graphql:"comments(first: 100, after: $cursor)"`
		} `graphql:"pullRequest(number: $pr)"`
	} `graphql:"repository(owner: $owner, name: $repo)"`
}

// CommentSnapshot reads every review thread comment, review body and issue
// comment of a pull request, across all pages. With withBodies=false no body is
// requested from GitHub. It returns reader.ErrSnapshotMoved when the pull
// request's head or base changes while it is paging.
func (c *Client) CommentSnapshot(ctx context.Context, owner, repo string, pr int, withBodies bool) (reader.Snapshot, error) {
	if pr <= 0 || pr > math.MaxInt32 {
		return reader.Snapshot{}, fmt.Errorf("pr number out of valid range: %d", pr)
	}
	base := func() map[string]interface{} {
		return map[string]interface{}{
			"owner":    githubv4.String(owner),
			"repo":     githubv4.String(repo),
			"pr":       githubv4.Int(int32(pr)), //nolint:gosec // range checked above
			"cursor":   (*githubv4.String)(nil),
			"withBody": githubv4.Boolean(withBodies),
		}
	}

	var snap reader.Snapshot
	var haveRefs bool
	setRefs := func(refs pullRequestRefs) error {
		head, baseSHA := string(refs.HeadRefOid), string(refs.BaseRefOid)
		if !haveRefs {
			snap.HeadSHA, snap.BaseSHA, haveRefs = head, baseSHA, true
			return nil
		}
		if snap.HeadSHA != head || snap.BaseSHA != baseSHA {
			return reader.ErrSnapshotMoved
		}
		return nil
	}

	// review threads
	vars := base()
	for {
		var q commentThreadsQuery
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return reader.Snapshot{}, fmt.Errorf("graphql review threads query failed: %w", err)
		}
		if err := setRefs(q.Repository.PullRequest.pullRequestRefs); err != nil {
			return reader.Snapshot{}, err
		}
		for _, n := range q.Repository.PullRequest.ReviewThreads.Nodes {
			thread, err := c.readThread(ctx, n, withBodies)
			if err != nil {
				return reader.Snapshot{}, err
			}
			snap.Threads = append(snap.Threads, thread)
		}
		next, err := nextCursor(q.Repository.PullRequest.ReviewThreads.PageInfo)
		if err != nil {
			return reader.Snapshot{}, err
		}
		if next == nil {
			break
		}
		vars["cursor"] = next
	}

	// review bodies
	vars = base()
	for {
		var q reviewsQuery
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return reader.Snapshot{}, fmt.Errorf("graphql reviews query failed: %w", err)
		}
		if err := setRefs(q.Repository.PullRequest.pullRequestRefs); err != nil {
			return reader.Snapshot{}, err
		}
		for _, n := range q.Repository.PullRequest.Reviews.Nodes {
			comment := toReaderComment(reader.KindReviewBody, n.commentCore, withBodies)
			comment.State = string(n.State)
			snap.Reviews = append(snap.Reviews, comment)
		}
		next, err := nextCursor(q.Repository.PullRequest.Reviews.PageInfo)
		if err != nil {
			return reader.Snapshot{}, err
		}
		if next == nil {
			break
		}
		vars["cursor"] = next
	}

	// issue comments
	vars = base()
	for {
		var q issueCommentsQuery
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return reader.Snapshot{}, fmt.Errorf("graphql issue comments query failed: %w", err)
		}
		if err := setRefs(q.Repository.PullRequest.pullRequestRefs); err != nil {
			return reader.Snapshot{}, err
		}
		for _, n := range q.Repository.PullRequest.Comments.Nodes {
			snap.IssueComments = append(snap.IssueComments, toReaderComment(reader.KindIssueComment, n, withBodies))
		}
		next, err := nextCursor(q.Repository.PullRequest.Comments.PageInfo)
		if err != nil {
			return reader.Snapshot{}, err
		}
		if next == nil {
			break
		}
		vars["cursor"] = next
	}
	return snap, nil
}

// readThread converts a thread and walks the rest of its comments, which the
// first page of the threads query does not include.
func (c *Client) readThread(ctx context.Context, n commentThreadNode, withBodies bool) (reader.Thread, error) {
	t := reader.Thread{
		ID:         fmt.Sprintf("%v", n.ID),
		Path:       string(n.Path),
		IsResolved: bool(n.IsResolved),
		IsOutdated: bool(n.IsOutdated),
	}
	if n.Line != nil {
		v := int32(*n.Line)
		t.Line = &v
	}
	add := func(page threadCommentsPage) {
		for _, node := range page.Nodes {
			t.Comments = append(t.Comments, toReaderComment(reader.KindThreadComment, node, withBodies))
		}
	}
	add(n.Comments)

	cursor, err := nextCursor(n.Comments.PageInfo)
	if err != nil {
		return reader.Thread{}, err
	}
	for cursor != nil {
		var q threadCommentsContinuationQuery
		vars := map[string]interface{}{
			"id":       n.ID,
			"cursor":   cursor,
			"withBody": githubv4.Boolean(withBodies),
		}
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return reader.Thread{}, fmt.Errorf("graphql thread comments query failed: %w", err)
		}
		page := q.Node.PullRequestReviewThread.Comments
		add(page)
		if cursor, err = nextCursor(page.PageInfo); err != nil {
			return reader.Thread{}, err
		}
	}
	return t, nil
}

func nextCursor(p pageInfo) (*githubv4.String, error) {
	if !bool(p.HasNextPage) {
		return nil, nil
	}
	if p.EndCursor == "" {
		return nil, errors.New("graphql query returned hasNextPage=true with an empty endCursor")
	}
	cursor := p.EndCursor
	return &cursor, nil
}

func toReaderComment(kind reader.Kind, n commentCore, withBodies bool) reader.Comment {
	c := reader.Comment{
		Kind:       kind,
		ID:         formatCommentID(n.DatabaseID),
		Author:     string(n.Author.Login),
		AuthorType: string(n.Author.TypeName),
		URL:        string(n.URL),
		CreatedAt:  n.CreatedAt.Format(time.RFC3339),
	}
	if n.LastEditedAt != nil {
		c.EditedAt = n.LastEditedAt.Format(time.RFC3339)
	}
	if withBodies {
		body := string(n.Body)
		c.Body = &body
	}
	return c
}

// maxProjectFileBytes bounds the project allowlist file. It is a short list of
// logins; anything larger is a mistake, not a configuration.
const maxProjectFileBytes = 64 * 1024

// ProjectAllowlistFile reads a file at a fixed commit. found=false means the
// file does not exist at that commit. A path that is not a regular file, a file
// that is too large and content that cannot be decoded are errors that wrap
// reader.ErrProjectAllowlistInvalid; a failed request is a plain error.
func (c *Client) ProjectAllowlistFile(ctx context.Context, owner, repo, path, sha string) (data []byte, found bool, err error) {
	file, dir, resp, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: sha})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s at %s: %w", path, sha, err)
	}
	if file == nil || dir != nil {
		return nil, true, fmt.Errorf("%w: %s is not a regular file", reader.ErrProjectAllowlistInvalid, path)
	}
	if file.GetSize() > maxProjectFileBytes {
		return nil, true, fmt.Errorf("%w: %s is larger than %d bytes", reader.ErrProjectAllowlistInvalid, path, maxProjectFileBytes)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, true, fmt.Errorf("%w: decode %s: %v", reader.ErrProjectAllowlistInvalid, path, err)
	}
	return []byte(content), true, nil
}
