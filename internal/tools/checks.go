package tools

import (
	"context"
	"fmt"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── Tool: list_check_runs_for_sha ────────────────────────────────────────────

// Only full SHA-1 commit IDs are accepted: a branch name or short SHA would let
// the target move silently, defeating the head_sha match callers rely on.
var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkRunDeduplicationStrategy names how ListCheckRunsForSHA collapses reruns.
const checkRunDeduplicationStrategy = "latest_id_per_app_and_name"

// ListCheckRunsForSHAInput is the input schema for list_check_runs_for_sha.
type ListCheckRunsForSHAInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
}

// CheckRunAppOutput identifies the GitHub App that created a check run.
type CheckRunAppOutput struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

// CheckRunOutput is a single check run as reported by GitHub.
type CheckRunOutput struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	HeadSHA    string            `json:"head_sha"`
	Status     string            `json:"status"`
	Conclusion *string           `json:"conclusion"`
	App        CheckRunAppOutput `json:"app"`
	HTMLURL    string            `json:"html_url"`
}

// CheckRunDeduplicationOutput describes the deduplication applied to check_runs.
type CheckRunDeduplicationOutput struct {
	Strategy string `json:"strategy"`
	RawCount int    `json:"raw_count"`
}

// ListCheckRunsForSHAOutput is the output schema for list_check_runs_for_sha.
type ListCheckRunsForSHAOutput struct {
	SHA           string                      `json:"sha"`
	CheckRuns     []CheckRunOutput            `json:"check_runs"`
	Deduplication CheckRunDeduplicationOutput `json:"deduplication"`
	Pagination    ThreadPaginationOutput      `json:"pagination"`
}

var listCheckRunsForSHATool = &mcp.Tool{
	Name:        "list_check_runs_for_sha",
	Description: "指定したコミット SHA（40 桁の小文字 16 進数。PR 番号や branch 名は受け付けない）の check runs を全ページ取得して返す read-only ツール。各 run の head_sha / status / conclusion / app を含む。再実行 run は (app ID, name) ごとに最新 ID の run だけを残す（deduplication に方式と重複排除前の件数を示す）。合否判定は行わないため、呼び出し元が判定規則に基づいて判断する。",
}

func listCheckRunsForSHAHandler(
	clientProvider githubClientProvider,
) func(context.Context, *mcp.CallToolRequest, ListCheckRunsForSHAInput) (*mcp.CallToolResult, ListCheckRunsForSHAOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ListCheckRunsForSHAInput) (*mcp.CallToolResult, ListCheckRunsForSHAOutput, error) {
		if in.Owner == "" || in.Repo == "" || in.SHA == "" {
			return nil, ListCheckRunsForSHAOutput{}, fmt.Errorf("owner, repo, and sha are required")
		}
		if !commitSHAPattern.MatchString(in.SHA) {
			return nil, ListCheckRunsForSHAOutput{}, fmt.Errorf("sha must be a full 40-character lowercase hex commit SHA, got %q", in.SHA)
		}
		gh, err := clientProvider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ListCheckRunsForSHAOutput{}, nil
			}
			return nil, ListCheckRunsForSHAOutput{}, err
		}

		runs, err := gh.ListCheckRunsForSHA(ctx, in.Owner, in.Repo, in.SHA)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, ListCheckRunsForSHAOutput{}, nil
			}
			return nil, ListCheckRunsForSHAOutput{}, err
		}

		checkRuns := make([]CheckRunOutput, 0, len(runs.CheckRuns))
		for _, r := range runs.CheckRuns {
			run := CheckRunOutput{
				ID:      r.ID,
				Name:    r.Name,
				HeadSHA: r.HeadSHA,
				Status:  r.Status,
				App:     CheckRunAppOutput{ID: r.AppID, Slug: r.AppSlug},
				HTMLURL: r.HTMLURL,
			}
			if r.Conclusion != "" {
				conclusion := r.Conclusion
				run.Conclusion = &conclusion
			}
			checkRuns = append(checkRuns, run)
		}

		return nil, ListCheckRunsForSHAOutput{
			SHA:       in.SHA,
			CheckRuns: checkRuns,
			Deduplication: CheckRunDeduplicationOutput{
				Strategy: checkRunDeduplicationStrategy,
				RawCount: runs.RawCount,
			},
			Pagination: ThreadPaginationOutput{PageCount: runs.PageCount, Complete: true},
		}, nil
	}
}

// RegisterListCheckRunsForSHATool adds list_check_runs_for_sha to the MCP server.
func RegisterListCheckRunsForSHATool(server *mcp.Server, clientProvider githubClientProvider) {
	mcp.AddTool(server, listCheckRunsForSHATool, listCheckRunsForSHAHandler(clientProvider))
}
