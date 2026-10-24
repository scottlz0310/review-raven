package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/autherr"
	ghclient "github.com/scottlz0310/review-raven/internal/github"
	"github.com/scottlz0310/review-raven/internal/middleware"
	"github.com/scottlz0310/review-raven/internal/reviewrequest"
)

// requestsResourcePrefix is the immutable subscription URI of a review request.
// There is deliberately no mutable "latest request of a PR" alias.
const requestsResourcePrefix = "review-raven://requests/"

var requestURIPattern = regexp.MustCompile(`^review-raven://requests/(cyc_[0-9a-z]{26})$`)

// parseRequestURI extracts the cycle ID from a request resource URI. The URI is
// matched in full, so a crafted path or query never selects another resource.
func parseRequestURI(uri string) (string, bool) {
	m := requestURIPattern.FindStringSubmatch(uri)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// RequestReviewToolInput is the input of request_review. The caller is the
// authenticated principal; a self-declared requester is not accepted.
type RequestReviewToolInput struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	PRNumber        int    `json:"prNumber"`
	ExpectedHeadSHA string `json:"expectedHeadSha"`
	Reason          string `json:"reason"`
	IdempotencyKey  string `json:"idempotencyKey"`
}

// GetReviewRequestToolInput is the input of get_review_request.
type GetReviewRequestToolInput struct {
	SchemaVersion int    `json:"schemaVersion"`
	CycleID       string `json:"cycleId"`
}

// CancelReviewRequestToolInput is the input of cancel_review_request.
type CancelReviewRequestToolInput struct {
	SchemaVersion            int    `json:"schemaVersion"`
	CycleID                  string `json:"cycleId"`
	ExpectedHeadSHA          string `json:"expectedHeadSha"`
	ExpectedSnapshotRevision int    `json:"expectedSnapshotRevision"`
	IdempotencyKey           string `json:"idempotencyKey"`
}

var requestReviewTool = &mcp.Tool{
	Name: "request_review",
	Description: "PR のレビューを依頼する（初回・再依頼の同一入口）。expectedHeadSha は完全な 40 桁の小文字 16 進数で、現在の PR HEAD と一致しなければ HEAD_CONFLICT。" +
		"idempotencyKey は主体と PR の範囲で一意で、同じ key・同じ内容の再送は同じ cycle を返す。" +
		"返す dispatch=pending は未配送を表し、queue 登録済みを意味しない。進捗は resourceUri（review-raven://requests/{cycleId}）を購読して受け取る。" +
		"入力には契約の版 schemaVersion: 1 が必須。呼び出し主体は認証済みの identity から得る（自己申告の requestedBy は受け付けない）。失敗は error envelope（code / retryable）で返す。",
}

var getReviewRequestTool = &mcp.Tool{
	Name: "get_review_request",
	Description: "cycleId の現在の snapshot を返す read-only ツール。依頼の正本・配送状態・取消・実行観測・結果参照から再構築した射影で、結果本文や Verdict は含まない。" +
		"terminal=true は結果参照あり / superseded / cancelled のときだけ（terminalReason で区別する）。プロセス終了・観測停止・配送失敗だけでは terminal にならない。",
}

var cancelReviewRequestTool = &mcp.Tool{
	Name: "cancel_review_request",
	Description: "結果なしで失敗した cycle を、同じ HEAD で復旧するための明示取消。元の依頼主体、または保護設定で許可された運用主体だけが呼べる。" +
		"expectedSnapshotRevision が古ければ REVISION_CONFLICT。取消は要求の受付であり、旧候補の無効化 ACK を受けるまで terminal=false（cancellation=requested）。" +
		"確認後に新しい idempotencyKey で request_review を呼ぶと、新しい cycle・round で受け付ける。",
}

// reviewRequestPrincipal returns the verified caller. The identity header is
// trusted only when the shared proxy secret was verified for this request.
func reviewRequestPrincipal(ctx context.Context, req *mcp.CallToolRequest) (string, error) {
	if !middleware.ProxyVerified(ctx) {
		return "", &reviewrequest.Error{Code: reviewrequest.CodePermissionDenied, Message: "the caller is not authenticated"}
	}
	login := loginFromToolRequest(ctx, req)
	if login == "" {
		return "", &reviewrequest.Error{Code: reviewrequest.CodePermissionDenied, Message: "the caller is not authenticated"}
	}
	return login, nil
}

// checkSchemaVersion enforces the contract version carried by every input
// (schemaVersion is required by docs/design/contracts). An unsupported version
// is rejected explicitly instead of being interpreted as version 1.
func checkSchemaVersion(version int) error {
	if version != reviewrequest.SchemaVersion {
		return &reviewrequest.Error{
			Code:    reviewrequest.CodeInvalidArgument,
			Message: fmt.Sprintf("schemaVersion must be %d", reviewrequest.SchemaVersion),
		}
	}
	return nil
}

// contractToolError renders a contract error as an error envelope in the
// structured content of an error result. Errors that are not contract errors (a
// failing database, for example) propagate as protocol errors instead of being
// relabelled.
//
// The tools declare Out as any: with a typed Out the SDK overwrites the
// structured content of every result with the (zero) typed output, which would
// replace the envelope. The declared output schema is the union of the success
// shape and the envelope (see outputOrError).
func contractToolError(err error) (*mcp.CallToolResult, any, error) {
	contractErr, ok := reviewrequest.AsError(err)
	if !ok {
		return nil, nil, err
	}
	return &mcp.CallToolResult{IsError: true}, contractErr.Envelope(), nil
}

// outputOrError is the output schema of a tool: either its success output T or
// the contract error envelope.
func outputOrError[T any]() *jsonschema.Schema {
	success, err := jsonschema.For[T](nil)
	if err != nil {
		// 静的な型からの schema 導出に失敗するのは実装の誤り。起動時に止める。
		panic(fmt.Sprintf("derive output schema: %v", err))
	}
	failure, err := jsonschema.For[reviewrequest.ErrorEnvelope](nil)
	if err != nil {
		panic(fmt.Sprintf("derive error envelope schema: %v", err))
	}
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{success, failure}}
}

func withOutputSchema(tool *mcp.Tool, schema *jsonschema.Schema) *mcp.Tool {
	declared := *tool
	declared.OutputSchema = schema
	return &declared
}

// RegisterReviewRequestTools adds request_review, get_review_request and
// cancel_review_request. They are published only in the dedicated GitHub App
// mode, where the caller identity is proxy-verified.
func RegisterReviewRequestTools(server *mcp.Server, svc *reviewrequest.Service) {
	mcp.AddTool(server, withOutputSchema(requestReviewTool, outputOrError[reviewrequest.RequestReviewOutput]()), func(ctx context.Context, req *mcp.CallToolRequest, in RequestReviewToolInput) (*mcp.CallToolResult, any, error) {
		principal, err := reviewRequestPrincipal(ctx, req)
		if err != nil {
			return contractToolError(err)
		}
		if err := checkSchemaVersion(in.SchemaVersion); err != nil {
			return contractToolError(err)
		}
		out, err := svc.RequestReview(ctx, principal, reviewrequest.RequestReviewInput{
			Owner: in.Owner, Repo: in.Repo, PRNumber: in.PRNumber,
			ExpectedHeadSHA: in.ExpectedHeadSHA, Reason: in.Reason, IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return contractToolError(err)
		}
		return nil, out, nil
	})

	mcp.AddTool(server, withOutputSchema(getReviewRequestTool, outputOrError[reviewrequest.Snapshot]()), func(ctx context.Context, req *mcp.CallToolRequest, in GetReviewRequestToolInput) (*mcp.CallToolResult, any, error) {
		principal, err := reviewRequestPrincipal(ctx, req)
		if err != nil {
			return contractToolError(err)
		}
		if err := checkSchemaVersion(in.SchemaVersion); err != nil {
			return contractToolError(err)
		}
		snap, err := svc.Snapshot(ctx, principal, in.CycleID)
		if err != nil {
			return contractToolError(err)
		}
		return nil, snap, nil
	})

	mcp.AddTool(server, withOutputSchema(cancelReviewRequestTool, outputOrError[reviewrequest.CancelReviewOutput]()), func(ctx context.Context, req *mcp.CallToolRequest, in CancelReviewRequestToolInput) (*mcp.CallToolResult, any, error) {
		principal, err := reviewRequestPrincipal(ctx, req)
		if err != nil {
			return contractToolError(err)
		}
		if err := checkSchemaVersion(in.SchemaVersion); err != nil {
			return contractToolError(err)
		}
		out, err := svc.CancelReview(ctx, principal, reviewrequest.CancelReviewInput{
			CycleID: in.CycleID, ExpectedHeadSHA: in.ExpectedHeadSHA,
			ExpectedSnapshotRevision: in.ExpectedSnapshotRevision, IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return contractToolError(err)
		}
		return nil, out, nil
	})
}

// requestResourcePrincipal returns the verified caller of a resource read or
// subscription (their context carries no tool request).
func requestResourcePrincipal(ctx context.Context) (string, bool) {
	if !middleware.ProxyVerified(ctx) {
		return "", false
	}
	login := middleware.LoginFromContext(ctx)
	return login, login != ""
}

// readableSnapshot returns the snapshot for a request URI, or ok=false when the
// caller must be told the resource does not exist. Unknown, expired and
// forbidden requests are indistinguishable on purpose: a caller without access
// learns nothing about a cycle ID.
func readableSnapshot(ctx context.Context, svc *reviewrequest.Service, uri string) (reviewrequest.Snapshot, bool, error) {
	cycleID, ok := parseRequestURI(uri)
	if !ok {
		return reviewrequest.Snapshot{}, false, nil
	}
	principal, ok := requestResourcePrincipal(ctx)
	if !ok {
		return reviewrequest.Snapshot{}, false, nil
	}
	snap, err := svc.Snapshot(ctx, principal, cycleID)
	if err != nil {
		switch reviewrequest.ErrorCode(err) {
		case reviewrequest.CodeRequestNotFound, reviewrequest.CodeRequestExpired,
			reviewrequest.CodePermissionDenied, reviewrequest.CodeInvalidArgument:
			return reviewrequest.Snapshot{}, false, nil
		}
		return reviewrequest.Snapshot{}, false, err
	}
	return snap, true, nil
}

// authorizeRequestSubscription decides a subscribe (listen) for a request URI
// with the same authorization as a read.
func authorizeRequestSubscription(ctx context.Context, svc *reviewrequest.Service, uri string) error {
	if svc == nil {
		return mcp.ResourceNotFoundError(uri)
	}
	_, ok, err := readableSnapshot(ctx, svc, uri)
	if err != nil {
		return err
	}
	if !ok {
		return mcp.ResourceNotFoundError(uri)
	}
	return nil
}

// RegisterReviewRequestResource registers review-raven://requests/{cycleId}.
// Reads return the snapshot JSON; subscribers receive resources/updated
// whenever the stored state of the request changes.
func RegisterReviewRequestResource(server *mcp.Server, svc *reviewrequest.Service) {
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: requestsResourcePrefix + "{cycleId}",
		Name:        "Review Request",
		Description: "レビュー依頼 cycle の現在の snapshot（JSON）。購読すると、依頼の状態が変わるたびに resources/updated が届く。" +
			"URI だけを信用せず、snapshot の owner / repo / prNumber / expectedHeadSha を確認すること。",
		MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if req == nil || req.Params == nil || req.Params.URI == "" {
			return nil, fmt.Errorf("missing read resource request params")
		}
		uri := req.Params.URI
		snap, ok, err := readableSnapshot(ctx, svc, uri)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		data, err := json.Marshal(snap)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal review request snapshot: %w", err)
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{URI: uri, Text: string(data), MIMEType: "application/json"}},
		}, nil
	})
}

// NewInstallationHeadResolver resolves PR heads with the dedicated App's
// installation token. A pull request the installation cannot see is reported as
// not accessible, never as missing.
func NewInstallationHeadResolver(threshold time.Duration, source InstallationTokens) reviewrequest.HeadResolver {
	return installationHeadResolver{provider: newInstallationClientProvider(threshold, source)}
}

type installationHeadResolver struct {
	provider githubClientProvider
}

func (r installationHeadResolver) CurrentHead(ctx context.Context, owner, repo string, pr int) (string, error) {
	gh, err := r.provider(ctx, nil)
	if err != nil {
		return "", classifyHeadError(err)
	}
	sha, err := gh.PullRequestHead(ctx, owner, repo, pr)
	if err != nil {
		return "", classifyHeadError(err)
	}
	return sha, nil
}

func classifyHeadError(err error) error {
	if classified := ghclient.ClassifyGitHubError(err); classified != nil {
		switch classified.ErrorType {
		case autherr.NOT_FOUND, autherr.PERMISSION_DENIED, autherr.AUTH_REQUIRED, autherr.REAUTH_REQUIRED:
			return fmt.Errorf("%w: %v", reviewrequest.ErrPullRequestNotAccessible, err)
		}
	}
	return err
}
