package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/reader"
)

const ciPolicyPath = ".review-raven/ci-policy.json"

type contextSource interface {
	reader.Source
	Context(context.Context) (ghclient.PRContext, error)
	CIPolicy(context.Context, string) ([]byte, bool, error)
}

func (s *githubCommentSource) Context(ctx context.Context) (ghclient.PRContext, error) {
	return s.gh.GetPRContext(ctx, ghclient.DescriptionTarget{Owner: s.owner, Repo: s.repo, PR: s.pr})
}

func (s *githubCommentSource) CIPolicy(ctx context.Context, sha string) ([]byte, bool, error) {
	return s.gh.ProjectAllowlistFile(ctx, s.owner, s.repo, ciPolicyPath, sha)
}

type policyDocument struct {
	Path    string `json:"path"`
	Found   bool   `json:"found"`
	Content string `json:"content"`
}

type prContextOutput struct {
	SchemaVersion int `json:"schemaVersion"`
	ghclient.PRContext
	SnapshotID string                    `json:"snapshotId"`
	Allowlist  reader.OutputAllowlist    `json:"allowlist"`
	Comments   []reader.OutputCommentRef `json:"comments"`
	Pagination reader.OutputPaging       `json:"pagination"`
	CIPolicy   policyDocument            `json:"ciPolicy"`
}

func contextReadFailure(err error) (*mcp.CallToolResult, any, error) {
	var gate *reader.Error
	if errors.As(err, &gate) {
		return &mcp.CallToolResult{IsError: true}, reader.ToErrorEnvelope(gate), nil
	}
	if errors.Is(err, reader.ErrPolicyRefUnavailable) {
		return &mcp.CallToolResult{IsError: true}, reader.ToErrorEnvelope(&reader.Error{Code: reader.CodePolicyRefUnavailable, Message: "現在のbase branch参照を確認できません"}), nil
	}
	if result, ok := tryAuthResult(err); ok {
		return result, nil, nil
	}
	return nil, nil, fmt.Errorf("PR contextの取得に失敗しました: %w", err)
}

func readPRContext(ctx context.Context, src contextSource, logins []string) (prContextOutput, error) {
	before, err := src.Context(ctx)
	if err != nil {
		return prContextOutput{}, err
	}
	verified, err := reader.ReadMetadata(ctx, src, logins)
	if err != nil {
		return prContextOutput{}, err
	}
	s := verified.Snapshot
	if s.HeadSHA != before.HeadSHA || s.BaseSHA != before.BaseSHA || s.BaseRefName != before.BaseRefName || s.PolicyBaseSHA != before.PolicyBaseSHA {
		return prContextOutput{}, &reader.Error{Code: reader.CodeSnapshotChanged, Message: "投稿者検証中にPR参照が変化しました"}
	}
	data, found, err := src.CIPolicy(ctx, before.PolicyBaseSHA)
	if err != nil {
		return prContextOutput{}, fmt.Errorf("CI宣言の取得に失敗: %w", err)
	}
	if found {
		var object map[string]json.RawMessage
		if !utf8.Valid(data) || json.Unmarshal(data, &object) != nil || object == nil {
			return prContextOutput{}, &reader.Error{Code: "CI_POLICY_INVALID", Message: "CI宣言はUTF-8のJSON objectである必要があります"}
		}
	}
	after, err := src.Context(ctx)
	if err != nil {
		return prContextOutput{}, err
	}
	if before != after {
		return prContextOutput{}, &reader.Error{Code: reader.CodeSnapshotChanged, Message: "宣言の取得中にPR参照が変化しました"}
	}
	comments := make([]reader.OutputCommentRef, 0)
	add := func(c reader.Comment, threadID string) {
		comments = append(comments, reader.OutputCommentRef{Kind: c.Kind, ID: c.ID, ThreadID: threadID, Author: c.Author, AuthorType: c.AuthorType, URL: c.URL})
	}
	for _, thread := range s.Threads {
		for _, c := range thread.Comments {
			add(c, thread.ID)
		}
	}
	for _, c := range s.Reviews {
		add(c, "")
	}
	for _, c := range s.IssueComments {
		add(c, "")
	}
	return prContextOutput{SchemaVersion: 1, PRContext: before, SnapshotID: verified.SnapshotID,
		Allowlist: reader.OutputAllowlist{ServerLogins: verified.Allowlist.ServerLogins, ProjectFile: verified.Allowlist.ProjectFile, ProjectLogins: verified.Allowlist.ProjectLogins},
		Comments:  comments, Pagination: reader.OutputPaging{Complete: true},
		CIPolicy: policyDocument{Path: ciPolicyPath, Found: found, Content: string(data)}}, nil
}

// RegisterPRContextToolは専用Appだけに追加し、既存readerの本文取得契約を変えない。
func RegisterPRContextTool(server *mcp.Server, provider githubClientProvider, serverLogins []string) {
	logins := append([]string{}, serverLogins...)
	tool := &mcp.Tool{Name: "get_pr_context", Description: "reviewed側のPR参照・全コメントの投稿者metadata・CI宣言を本文なしで取得する。既存readerの全件投稿者検証を適用し、現在のbase branch先頭のpolicyBaseShaへ宣言を固定する。参照変化はSNAPSHOT_CHANGEDで停止。ciPolicy.contentのCI意味検証・skip判定は呼出し側の責務。コメント本文・PR説明は取得しない。snapshotIdはreader v2と同じ比較用IDであり、将来のwrite許可や原子的snapshotを保証しない。"}
	mcp.AddTool(server, withOutputSchema(tool, outputOrErrorOf[prContextOutput, reader.ErrorEnvelope]()),
		func(ctx context.Context, req *mcp.CallToolRequest, in ghclient.DescriptionTarget) (*mcp.CallToolResult, any, error) {
			if err := in.Validate(); err != nil {
				return contextReadFailure(err)
			}
			gh, err := provider(ctx, req)
			if err != nil {
				return contextReadFailure(err)
			}
			out, err := readPRContext(ctx, &githubCommentSource{gh: gh, owner: in.Owner, repo: in.Repo, pr: in.PR}, logins)
			if err != nil {
				return contextReadFailure(err)
			}
			return nil, out, nil
		})
}
