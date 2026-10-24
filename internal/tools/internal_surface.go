package tools

import (
	"context"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/scottlz0310/review-raven/internal/reviewrequest"
)

// The internal surface is the service-to-service half of the review-request
// contract: thread-owl reports candidates and results, Squirrel reports
// execution observations. It is a separate MCP server (mounted on its own path
// with its own proxy secret), so its tools never appear in the agent-facing
// tools/list, and each call is authorized per service principal, operation and
// repository by the service ACL.

// IngestReviewCandidateToolInput is the input of ingest_review_candidate.
type IngestReviewCandidateToolInput struct {
	SchemaVersion int    `json:"schemaVersion"`
	SourceID      string `json:"sourceId"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	PRNumber      int    `json:"prNumber"`
	HeadSHA       string `json:"headSha"`
	Reason        string `json:"reason"`
	Delivery      string `json:"delivery"`
}

// ReviewPostToolInput identifies the reviewer's GitHub comment.
type ReviewPostToolInput struct {
	CommentID int64  `json:"commentId"`
	URL       string `json:"url"`
}

// PublishReviewResultToolInput is the input of publish_review_result.
type PublishReviewResultToolInput struct {
	SchemaVersion int                 `json:"schemaVersion"`
	CycleID       string              `json:"cycleId"`
	CandidateID   string              `json:"candidateId"`
	HeadSHA       string              `json:"headSha"`
	ResultVersion int                 `json:"resultVersion"`
	Kind          string              `json:"kind"`
	Post          ReviewPostToolInput `json:"post"`
	PostedAt      string              `json:"postedAt"`
}

// PublishReviewObservationToolInput is the input of publish_review_observation.
// holdReason, outcome and exitCode are required but may be null, as in the
// contract.
type PublishReviewObservationToolInput struct {
	SchemaVersion int     `json:"schemaVersion"`
	CycleID       string  `json:"cycleId"`
	CandidateID   string  `json:"candidateId"`
	HeadSHA       string  `json:"headSha"`
	ProducerID    string  `json:"producerId"`
	ProducerEpoch int     `json:"producerEpoch"`
	Sequence      int     `json:"sequence"`
	ObservedAt    string  `json:"observedAt"`
	State         string  `json:"state"`
	HoldReason    *string `json:"holdReason"`
	Outcome       *string `json:"outcome"`
	ExitCode      *int    `json:"exitCode"`
	Stopped       bool    `json:"stopped"`
}

var ingestReviewCandidateTool = &mcp.Tool{
	Name: "ingest_review_candidate",
	Description: "[内部面・thread-owl 専用] thread-owl が署名と origin を検証済みの webhook / 再依頼コメントを、公開の依頼と同じ受付へ渡す。" +
		"sourceId（deliveryId / コメント ID）が同じ、または同一 PR・HEAD・reason の未完了依頼は既存 cycle に結合し、取消済み cycle の旧 sourceId は旧 cycle を返して新 round を作らない。" +
		"サービス主体・操作・repo を保護設定で許可された呼び出しだけが成功する。",
}

var publishReviewResultTool = &mcp.Tool{
	Name: "publish_review_result",
	Description: "[内部面・thread-owl 専用] レビュー結果の GitHub 投稿への参照を通知する。raven は Verdict を生成せず参照だけを保存する。" +
		"cycle の HEAD と違う結果は RESULT_HEAD_MISMATCH、古い resultVersion は RESULT_VERSION_STALE、取消確定済みは CANDIDATE_CANCELLED、削除済みの依頼への遅延配送は DELIVERY_EXPIRED。" +
		"同一内容の再送は冪等に受理する。",
}

var publishReviewObservationTool = &mcp.Tool{
	Name: "publish_review_observation",
	Description: "[内部面・Squirrel 専用] 実行観測（待機・実行中・終了）を通知する。producerId は呼び出し主体に許可されたものだけ。" +
		"producerEpoch / sequence の順序を検証し、遅着・旧 epoch・同一 sequence で内容違い・担当外の producer は現状態を変えずに拒否する。" +
		"ローカルパス・コマンドライン・ログ本文・秘密値は受け付けない。",
}

// RegisterInternalTools adds the three internal tools to server.
func RegisterInternalTools(server *mcp.Server, svc *reviewrequest.Service) {
	mcp.AddTool(server, withOutputSchema(ingestReviewCandidateTool, outputOrError[reviewrequest.IngestOutput]()),
		func(ctx context.Context, req *mcp.CallToolRequest, in IngestReviewCandidateToolInput) (*mcp.CallToolResult, any, error) {
			principal, err := reviewRequestPrincipal(ctx, req)
			if err != nil {
				return contractToolError(err)
			}
			if err := checkSchemaVersion(in.SchemaVersion); err != nil {
				return contractToolError(err)
			}
			out, err := svc.IngestCandidate(ctx, principal, reviewrequest.IngestInput{
				SourceID: in.SourceID, Owner: in.Owner, Repo: in.Repo, PRNumber: in.PRNumber,
				HeadSHA: in.HeadSHA, Reason: in.Reason, Delivery: in.Delivery,
			})
			if err != nil {
				return contractToolError(err)
			}
			return nil, out, nil
		})

	mcp.AddTool(server, withOutputSchema(publishReviewResultTool, outputOrError[reviewrequest.ResultOutput]()),
		func(ctx context.Context, req *mcp.CallToolRequest, in PublishReviewResultToolInput) (*mcp.CallToolResult, any, error) {
			principal, err := reviewRequestPrincipal(ctx, req)
			if err != nil {
				return contractToolError(err)
			}
			if err := checkSchemaVersion(in.SchemaVersion); err != nil {
				return contractToolError(err)
			}
			out, err := svc.PublishResult(ctx, principal, reviewrequest.ResultInput{
				CycleID: in.CycleID, CandidateID: in.CandidateID, HeadSHA: in.HeadSHA,
				ResultVersion: in.ResultVersion, Kind: in.Kind,
				CommentID: in.Post.CommentID, URL: in.Post.URL, PostedAt: in.PostedAt,
			})
			if err != nil {
				return contractToolError(err)
			}
			return nil, out, nil
		})

	mcp.AddTool(server, withOutputSchema(publishReviewObservationTool, outputOrError[reviewrequest.ObservationOutput]()),
		func(ctx context.Context, req *mcp.CallToolRequest, in PublishReviewObservationToolInput) (*mcp.CallToolResult, any, error) {
			principal, err := reviewRequestPrincipal(ctx, req)
			if err != nil {
				return contractToolError(err)
			}
			if err := checkSchemaVersion(in.SchemaVersion); err != nil {
				return contractToolError(err)
			}
			out, err := svc.PublishObservation(ctx, principal, reviewrequest.ObservationInput{
				CycleID: in.CycleID, CandidateID: in.CandidateID, HeadSHA: in.HeadSHA,
				ProducerID: in.ProducerID, ProducerEpoch: in.ProducerEpoch, Sequence: in.Sequence,
				ObservedAt: in.ObservedAt, State: in.State,
				HoldReason: in.HoldReason, Outcome: in.Outcome, ExitCode: in.ExitCode, Stopped: in.Stopped,
			})
			if err != nil {
				return contractToolError(err)
			}
			return nil, out, nil
		})
}

// BuildInternalHandler serves the internal surface over Streamable HTTP (stateless,
// protocol 2026-07-28 only). Mount it behind middleware.AppProxyAuth with the
// internal proxy secret, never behind the public one. The returned handler owns
// no background state; Close only ends open sessions.
func BuildInternalHandler(svc *reviewrequest.Service) *StreamableHandler {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "review-raven-internal", Version: "0.6.0"},
		&mcp.ServerOptions{SchemaCache: schemaCache},
	)
	RegisterInternalTools(srv, svc)

	handler := &StreamableHandler{server: srv}
	handler.handler = rejectLegacyInitialize(mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			if tokenFromToolRequest(r.Context(), nil) == "" {
				return nil
			}
			return srv
		},
		&mcp.StreamableHTTPOptions{
			Stateless:                  true,
			DisableLocalhostProtection: os.Getenv("MCP_DISABLE_LOCALHOST_PROTECTION") == "true",
		},
	))
	return handler
}
