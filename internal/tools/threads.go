package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	ghclient "github.com/scottlz0310/review-raven/internal/github"
)

// ─── Tool 4: get_review_threads ───────────────────────────────────────────────

// GetReviewThreadsInput is the input schema for get_review_threads.
type GetReviewThreadsInput struct {
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	PR            int    `json:"pr"`
	IncludeBodies *bool  `json:"include_bodies,omitempty"`
}

// ThreadCommentOutput is a single comment within a thread result.
type ThreadCommentOutput struct {
	CommentID  string  `json:"commentId"`
	Author     string  `json:"author"`
	AuthorType string  `json:"authorType"`
	URL        string  `json:"url"`
	Body       *string `json:"body,omitempty"`
	CreatedAt  string  `json:"createdAt"`
}

// ThreadResult is the result for a single review thread.
type ThreadResult struct {
	ID         string                `json:"id"`
	Path       string                `json:"path"`
	Line       *int32                `json:"line"`
	IsResolved bool                  `json:"isResolved"`
	Comments   []ThreadCommentOutput `json:"comments"`
}

// ThreadSummary holds aggregate counts across all threads.
type ThreadSummary struct {
	Total      int `json:"total"`
	Unresolved int `json:"unresolved"`
}

// ThreadPaginationOutput はサーバーが消費したページ数を示します。クライアントは返却前に
// 全ページを巡回するため、成功レスポンスは常に complete です。
type ThreadPaginationOutput struct {
	PageCount int  `json:"pageCount"`
	Complete  bool `json:"complete"`
}

// GetReviewThreadsOutput is the output schema for get_review_threads.
type GetReviewThreadsOutput struct {
	Threads    []ThreadResult         `json:"threads"`
	Summary    ThreadSummary          `json:"summary"`
	Pagination ThreadPaginationOutput `json:"pagination"`
}

var getReviewThreadsTool = &mcp.Tool{
	Name:        "get_review_threads",
	Description: "PR のレビュースレッド一覧（Raw コメントデータ）を返す。各スレッドに PRRT_xxx 形式の ID を含む。include_bodies=false を指定すると、コメント本文を GitHub から選択しないメタデータのみの射影を返す（省略時は true）。分類（blocking/non-blocking/suggestion）は呼び出し元 LLM がルールファイルに基づいて判断する。",
}

func getReviewThreadsHandler(
	clientProvider githubClientProvider,
) func(context.Context, *mcp.CallToolRequest, GetReviewThreadsInput) (*mcp.CallToolResult, GetReviewThreadsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in GetReviewThreadsInput) (*mcp.CallToolResult, GetReviewThreadsOutput, error) {
		if in.Owner == "" || in.Repo == "" || in.PR <= 0 {
			return nil, GetReviewThreadsOutput{}, fmt.Errorf("owner, repo, and pr are required")
		}
		gh, err := clientProvider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, GetReviewThreadsOutput{}, nil
			}
			return nil, GetReviewThreadsOutput{}, err
		}

		includeBodies := true
		if in.IncludeBodies != nil {
			includeBodies = *in.IncludeBodies
		}
		threadResult, err := gh.GetReviewThreadsWithOptions(ctx, in.Owner, in.Repo, in.PR, ghclient.ReviewThreadsOptions{
			IncludeBodies: includeBodies,
		})
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, GetReviewThreadsOutput{}, nil
			}
			return nil, GetReviewThreadsOutput{}, err
		}

		summary := ThreadSummary{Total: len(threadResult.Threads)}
		results := make([]ThreadResult, 0, len(threadResult.Threads))
		for _, t := range threadResult.Threads {
			if !t.IsResolved {
				summary.Unresolved++
			}
			comments := make([]ThreadCommentOutput, 0, len(t.Comments))
			for _, c := range t.Comments {
				comment := ThreadCommentOutput{
					CommentID:  c.CommentID,
					Author:     c.Author,
					AuthorType: c.AuthorType,
					URL:        c.URL,
					CreatedAt:  c.CreatedAt,
				}
				if includeBodies {
					body := c.Body
					comment.Body = &body
				}
				comments = append(comments, comment)
			}
			results = append(results, ThreadResult{
				ID:         t.ID,
				Path:       t.Path,
				Line:       t.Line,
				IsResolved: t.IsResolved,
				Comments:   comments,
			})
		}

		return nil, GetReviewThreadsOutput{
			Threads:    results,
			Summary:    summary,
			Pagination: ThreadPaginationOutput{PageCount: threadResult.PageCount, Complete: true},
		}, nil
	}
}

// RegisterGetReviewThreadsTool adds get_review_threads to the MCP server.
func RegisterGetReviewThreadsTool(server *mcp.Server, clientProvider githubClientProvider) {
	mcp.AddTool(server, getReviewThreadsTool, getReviewThreadsHandler(clientProvider))
}

// ─── Tool 5: reply_to_review_thread ──────────────────────────────────────────

// ReplyToThreadInput is the input schema for reply_to_review_thread.
type ReplyToThreadInput struct {
	ThreadID string `json:"threadId"`
	Body     string `json:"body"`
}

// ReplyToThreadOutput is the output schema for reply_to_review_thread.
type ReplyToThreadOutput struct {
	CommentID string  `json:"commentId"`
	CreatedAt string  `json:"createdAt"`
	Warn      *string `json:"warn,omitempty"`
}

var replyToThreadTool = &mcp.Tool{
	Name:        "reply_to_review_thread",
	Description: "指定したレビュースレッド（PRRT_xxx）に返信コメントを投稿する。スレッドが解決済みの場合は warn フィールドに thread_already_resolved を含む。",
}

func replyToThreadHandler(
	clientProvider githubClientProvider,
) func(context.Context, *mcp.CallToolRequest, ReplyToThreadInput) (*mcp.CallToolResult, ReplyToThreadOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ReplyToThreadInput) (*mcp.CallToolResult, ReplyToThreadOutput, error) {
		if in.ThreadID == "" {
			return nil, ReplyToThreadOutput{}, fmt.Errorf("threadId is required")
		}
		if strings.TrimSpace(in.Body) == "" {
			return nil, ReplyToThreadOutput{}, fmt.Errorf("body must not be empty or whitespace-only")
		}
		gh, err := clientProvider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ReplyToThreadOutput{}, nil
			}
			return nil, ReplyToThreadOutput{}, err
		}

		alreadyResolved, err := gh.IsThreadResolved(ctx, in.ThreadID)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ReplyToThreadOutput{}, nil
			}
			return nil, ReplyToThreadOutput{}, err
		}

		result, err := gh.ReplyToThread(ctx, in.ThreadID, in.Body)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ReplyToThreadOutput{}, nil
			}
			return nil, ReplyToThreadOutput{}, err
		}

		out := ReplyToThreadOutput{
			CommentID: result.CommentID,
			CreatedAt: result.CreatedAt,
		}
		if alreadyResolved {
			w := "thread_already_resolved"
			out.Warn = &w
		}
		return nil, out, nil
	}
}

// RegisterReplyToThreadTool adds reply_to_review_thread to the MCP server.
func RegisterReplyToThreadTool(server *mcp.Server, clientProvider githubClientProvider) {
	mcp.AddTool(server, replyToThreadTool, replyToThreadHandler(clientProvider))
}

// ─── Tool 6: resolve_review_thread ───────────────────────────────────────────

// ResolveThreadInput is the input schema for resolve_review_thread.
type ResolveThreadInput struct {
	ThreadID string `json:"threadId"`
}

// ResolveThreadOutput is the output schema for resolve_review_thread.
type ResolveThreadOutput struct {
	Resolved        bool `json:"resolved"`
	AlreadyResolved bool `json:"alreadyResolved,omitempty"`
}

var resolveThreadTool = &mcp.Tool{
	Name:        "resolve_review_thread",
	Description: "指定したレビュースレッド（PRRT_xxx）を解決済みにする。すでに解決済みの場合は no-op で alreadyResolved: true を返す。",
}

func resolveThreadHandler(
	clientProvider githubClientProvider,
) func(context.Context, *mcp.CallToolRequest, ResolveThreadInput) (*mcp.CallToolResult, ResolveThreadOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ResolveThreadInput) (*mcp.CallToolResult, ResolveThreadOutput, error) {
		if in.ThreadID == "" {
			return nil, ResolveThreadOutput{}, fmt.Errorf("threadId is required")
		}
		gh, err := clientProvider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ResolveThreadOutput{}, nil
			}
			return nil, ResolveThreadOutput{}, err
		}

		alreadyResolved, err := gh.ResolveThread(ctx, in.ThreadID)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ResolveThreadOutput{}, nil
			}
			return nil, ResolveThreadOutput{}, err
		}
		return nil, ResolveThreadOutput{
			Resolved:        true,
			AlreadyResolved: alreadyResolved,
		}, nil
	}
}

// RegisterResolveThreadTool adds resolve_review_thread to the MCP server.
func RegisterResolveThreadTool(server *mcp.Server, clientProvider githubClientProvider) {
	mcp.AddTool(server, resolveThreadTool, resolveThreadHandler(clientProvider))
}

// ─── Tool 7: reply_and_resolve_review_thread ──────────────────────────────────

// ReplyAndResolveInput is the input schema for reply_and_resolve_review_thread.
type ReplyAndResolveInput struct {
	ThreadID string `json:"threadId"`
	Body     string `json:"body"`
	Resolve  bool   `json:"resolve"`
}

// ReplyAndResolveOutput is the output schema for reply_and_resolve_review_thread.
type ReplyAndResolveOutput struct {
	Replied      bool    `json:"replied"`
	Resolved     bool    `json:"resolved"`
	CommentID    *string `json:"commentId"`
	ReplyError   *string `json:"replyError"`
	ResolveError *string `json:"resolveError"`
}

var replyAndResolveTool = &mcp.Tool{
	Name:        "reply_and_resolve_review_thread",
	Description: "レビュースレッドへの返信と Resolve をベストエフォートで順次実行する。返信失敗時は Resolve を実行しない。各操作の成否は replied / resolved フィールドで個別に返す。",
}

func replyAndResolveHandler(
	clientProvider githubClientProvider,
) func(context.Context, *mcp.CallToolRequest, ReplyAndResolveInput) (*mcp.CallToolResult, ReplyAndResolveOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ReplyAndResolveInput) (*mcp.CallToolResult, ReplyAndResolveOutput, error) {
		if in.ThreadID == "" {
			return nil, ReplyAndResolveOutput{}, fmt.Errorf("threadId is required")
		}
		if strings.TrimSpace(in.Body) == "" {
			return nil, ReplyAndResolveOutput{}, fmt.Errorf("body must not be empty or whitespace-only")
		}
		gh, err := clientProvider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ReplyAndResolveOutput{}, nil
			}
			return nil, ReplyAndResolveOutput{}, err
		}

		out := ReplyAndResolveOutput{}

		// Step 1: Reply.
		replyResult, err := gh.ReplyToThread(ctx, in.ThreadID, in.Body)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ReplyAndResolveOutput{}, nil
			}
			errStr := err.Error()
			out.ReplyError = &errStr
			// replied == false → skip Resolve.
			return nil, out, nil
		}
		out.Replied = true
		out.CommentID = &replyResult.CommentID

		// Step 2: Resolve (only if requested and reply succeeded).
		if !in.Resolve {
			return nil, out, nil
		}
		_, err = gh.ResolveThread(ctx, in.ThreadID)
		if err != nil {
			if errStr, ok := authErrString(err); ok {
				out.ResolveError = &errStr
				return nil, out, nil
			}
			errStr := err.Error()
			out.ResolveError = &errStr
			return nil, out, nil
		}
		out.Resolved = true
		return nil, out, nil
	}
}

// RegisterReplyAndResolveTool adds reply_and_resolve_review_thread to the MCP server.
func RegisterReplyAndResolveTool(server *mcp.Server, clientProvider githubClientProvider) {
	mcp.AddTool(server, replyAndResolveTool, replyAndResolveHandler(clientProvider))
}

// RegisterThreadTools registers Tools 4–7 (ISSUE#26) on the MCP server.
func RegisterThreadTools(server *mcp.Server, clientProvider githubClientProvider) {
	RegisterGetReviewThreadsTool(server, clientProvider)
	RegisterReplyToThreadTool(server, clientProvider)
	RegisterResolveThreadTool(server, clientProvider)
	RegisterReplyAndResolveTool(server, clientProvider)
}
