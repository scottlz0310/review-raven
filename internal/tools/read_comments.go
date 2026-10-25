package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/reader"
	"github.com/scottlz0310/review-raven/internal/trustedauthors"
)

// ReadPRCommentsInput is the input of read_pr_comments.
type ReadPRCommentsInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	PR    int    `json:"pr"`
}

var readPRCommentsTool = &mcp.Tool{
	Name: "read_pr_comments",
	Description: "PR の review thread のコメント・review body・issue comment の本文を、投稿者を検証した上で返す reviewed 側の reader。" +
		"メタデータ(全ページ・resolved を含む)→サーバーの allowlist と base ref のプロジェクト allowlist による投稿者検証→本文の順をツール側で強制し、" +
		"許可外の投稿者・特定できない投稿者が 1 件でもあれば、本文を取得せずエラー(UNTRUSTED_COMMENT / AUTHOR_CHECK_FAILED)を返す。" +
		"本文を取得した後に、検証したコメントと違うもの(追加・編集・投稿者や head の変化)があれば、返さずに取り直し、変わり続ければ SNAPSHOT_CHANGED を返す。" +
		"失敗の応答に本文は含まない。成功の snapshotId は、後の再取得との比較に使う(終端で値が変われば、その間にコメントが変わっている)。" +
		"サーバーの allowlist(TRUSTED_COMMENT_AUTHORS)が空なら TRUSTED_AUTHORS_NOT_CONFIGURED で止まる。" +
		"契約は docs/design/contracts/reader。get_review_threads の include_bodies は任意で、この検証を強制しない。",
}

// githubCommentSource reads a pull request's comments for the reader.
type githubCommentSource struct {
	gh    *ghclient.Client
	owner string
	repo  string
	pr    int
}

func (s *githubCommentSource) Fetch(ctx context.Context, withBodies bool) (reader.Snapshot, error) {
	return s.gh.CommentSnapshot(ctx, s.owner, s.repo, s.pr, withBodies)
}

// ProjectAllowlist reads the project allowlist at the base commit. A missing
// file adds nobody; a file that exists but is malformed stops the reader.
func (s *githubCommentSource) ProjectAllowlist(ctx context.Context, baseSHA string) (reader.ProjectAllowlist, error) {
	data, found, err := s.gh.ProjectAllowlistFile(ctx, s.owner, s.repo, trustedauthors.ProjectFilePath, baseSHA)
	if err != nil {
		return reader.ProjectAllowlist{}, err
	}
	if !found {
		return reader.ProjectAllowlist{State: reader.ProjectAllowlistAbsent}, nil
	}
	logins, err := trustedauthors.ParseProjectFile(data)
	if err != nil {
		return reader.ProjectAllowlist{}, errors.Join(reader.ErrProjectAllowlistInvalid, err)
	}
	return reader.ProjectAllowlist{State: reader.ProjectAllowlistPresent, Logins: logins}, nil
}

// serveReadPRComments runs the reader and renders its result: the verified
// snapshot, or the contract error envelope in the structured content of an
// error result (the tool declares Out as any for the same reason as the
// review-request tools: a typed Out would replace the envelope).
func serveReadPRComments(ctx context.Context, src reader.Source, serverLogins []string) (*mcp.CallToolResult, any, error) {
	result, err := reader.Read(ctx, src, serverLogins)
	if err != nil {
		var gateErr *reader.Error
		if errors.As(err, &gateErr) {
			return &mcp.CallToolResult{IsError: true}, reader.ToErrorEnvelope(gateErr), nil
		}
		if authResult, ok := tryAuthResult(err); ok {
			return authResult, nil, nil
		}
		return nil, nil, err
	}
	return nil, reader.ToOutput(result), nil
}

func readerOutputSchema() *jsonschema.Schema {
	return outputOrErrorOf[reader.Output, reader.ErrorEnvelope]()
}

// RegisterReadPRCommentsTool adds read_pr_comments. serverLogins is the
// normalized TRUSTED_COMMENT_AUTHORS allowlist; an empty list makes the tool
// fail closed with TRUSTED_AUTHORS_NOT_CONFIGURED.
func RegisterReadPRCommentsTool(server *mcp.Server, clientProvider githubClientProvider, serverLogins []string) {
	logins := append([]string{}, serverLogins...)
	mcp.AddTool(server, withOutputSchema(readPRCommentsTool, readerOutputSchema()), func(ctx context.Context, req *mcp.CallToolRequest, in ReadPRCommentsInput) (*mcp.CallToolResult, any, error) {
		if in.Owner == "" || in.Repo == "" || in.PR <= 0 {
			return nil, nil, fmt.Errorf("owner, repo, and pr are required")
		}
		gh, err := clientProvider(ctx, req)
		if err != nil {
			if result, ok := tryAuthResult(err); ok {
				return result, nil, nil
			}
			return nil, nil, err
		}
		return serveReadPRComments(ctx, &githubCommentSource{gh: gh, owner: in.Owner, repo: in.Repo, pr: in.PR}, logins)
	})
}
