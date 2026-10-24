// Package reviewrequest implements the review-request ledger (#144 R1): the
// durable record of review requests, their idempotency, supersession,
// cancellation and the snapshot projection published to subscribers.
//
// It is transport-agnostic. The wire contract lives in docs/design/contracts;
// the types here marshal to exactly those shapes, and the tests validate every
// produced document against the JSON Schemas.
package reviewrequest

import "context"

// SchemaVersion is the contract schemaVersion carried by every message.
const SchemaVersion = 1

// Dispatch states of a request (delivery to the reviewer side).
const (
	DispatchPending  = "pending"
	DispatchAccepted = "accepted"
	DispatchFailed   = "failed"
)

// Cancellation states.
const (
	CancelNone      = "none"
	CancelRequested = "requested"
	CancelConfirmed = "confirmed"
)

// Terminal reasons. A request is terminal only for one of these; process exit,
// observation loss or delivery failure alone never make it terminal.
const (
	TerminalResult     = "result"
	TerminalSuperseded = "superseded"
	TerminalCancelled  = "cancelled"
)

// Execution states observed from the orchestrator.
const (
	ExecUnknown  = "unknown"
	ExecWaiting  = "waiting"
	ExecRunning  = "running"
	ExecFinished = "finished"
)

// Freshness of the latest execution observation.
const (
	FreshnessUnknown = "unknown"
	FreshnessFresh   = "fresh"
	FreshnessStale   = "stale"
	FreshnessStopped = "stopped"
)

// Actions an Authorizer decides on.
const (
	ActionRequest = "request"
	ActionRead    = "read"
	ActionCancel  = "cancel"
	// ActionIngest, ActionPublishResult and ActionObserve are the internal
	// (service-principal) surfaces; they are authorized per subject and repository.
	ActionIngest        = "ingest"
	ActionPublishResult = "publish_result"
	ActionObserve       = "observe"
)

// Authorizer decides whether a verified principal may act on a repository.
// requestedBy is the principal that created the request (empty for create/read
// on repositories that have no request yet). An allow-by-default implementation
// must never be used outside tests: unset policy denies.
type Authorizer interface {
	Authorize(ctx context.Context, principal, action, owner, repo, requestedBy string) bool
}

// HeadResolver returns the current head SHA of a pull request.
type HeadResolver interface {
	CurrentHead(ctx context.Context, owner, repo string, pr int) (string, error)
}

// ProducerPolicy decides whether an authenticated principal may publish
// observations as the given producer.
type ProducerPolicy interface {
	MayPublish(ctx context.Context, principal, producerID string) bool
}

// RequestReviewInput is the input of request_review.
type RequestReviewInput struct {
	Owner           string
	Repo            string
	PRNumber        int
	ExpectedHeadSHA string
	Reason          string
	IdempotencyKey  string
}

// RequestReviewOutput is the output of request_review.
type RequestReviewOutput struct {
	SchemaVersion    int    `json:"schemaVersion"`
	CycleID          string `json:"cycleId"`
	Round            int    `json:"round"`
	HeadSHA          string `json:"headSha"`
	Dispatch         string `json:"dispatch"`
	ResourceURI      string `json:"resourceUri"`
	SnapshotRevision int    `json:"snapshotRevision"`
	Deduplicated     bool   `json:"deduplicated"`
}

// CancelReviewInput is the input of cancel_review_request.
type CancelReviewInput struct {
	CycleID                  string
	ExpectedHeadSHA          string
	ExpectedSnapshotRevision int
	IdempotencyKey           string
}

// CancelReviewOutput is the output of cancel_review_request.
type CancelReviewOutput struct {
	SchemaVersion    int    `json:"schemaVersion"`
	CycleID          string `json:"cycleId"`
	CancelID         string `json:"cancelId"`
	Cancellation     string `json:"cancellation"`
	Terminal         bool   `json:"terminal"`
	SnapshotRevision int    `json:"snapshotRevision"`
	Deduplicated     bool   `json:"deduplicated"`
}

// IngestInput is the input of ingest_review_candidate (owl → raven).
type IngestInput struct {
	SourceID string
	Owner    string
	Repo     string
	PRNumber int
	HeadSHA  string
	Reason   string
	Delivery string
}

// IngestOutput is the output of ingest_review_candidate.
type IngestOutput struct {
	SchemaVersion int    `json:"schemaVersion"`
	CycleID       string `json:"cycleId"`
	Round         int    `json:"round"`
	Dispatch      string `json:"dispatch"`
	ResourceURI   string `json:"resourceUri"`
	Deduplicated  bool   `json:"deduplicated"`
}

// ResultInput is the input of publish_review_result (owl → raven).
type ResultInput struct {
	CycleID       string
	CandidateID   string
	HeadSHA       string
	ResultVersion int
	Kind          string
	CommentID     int64
	URL           string
	PostedAt      string
}

// ResultOutput is the output of publish_review_result.
type ResultOutput struct {
	SchemaVersion    int  `json:"schemaVersion"`
	Accepted         bool `json:"accepted"`
	SnapshotRevision int  `json:"snapshotRevision"`
	Terminal         bool `json:"terminal"`
}

// ObservationInput is the input of publish_review_observation (Squirrel → raven).
type ObservationInput struct {
	CycleID       string
	CandidateID   string
	HeadSHA       string
	ProducerID    string
	ProducerEpoch int
	Sequence      int
	ObservedAt    string
	State         string
	HoldReason    *string
	Outcome       *string
	ExitCode      *int
	Stopped       bool
}

// ObservationOutput is the output of publish_review_observation.
type ObservationOutput struct {
	SchemaVersion    int    `json:"schemaVersion"`
	Result           string `json:"result"`
	SnapshotRevision int    `json:"snapshotRevision"`
}

// Snapshot is the projection published by get_review_request and the
// review-raven://requests/{cycleId} resource.
type Snapshot struct {
	SchemaVersion    int              `json:"schemaVersion"`
	CycleID          string           `json:"cycleId"`
	Owner            string           `json:"owner"`
	Repo             string           `json:"repo"`
	PRNumber         int              `json:"prNumber"`
	ExpectedHeadSHA  string           `json:"expectedHeadSha"`
	Round            int              `json:"round"`
	Dispatch         SnapshotDispatch `json:"dispatch"`
	Cancellation     SnapshotCancel   `json:"cancellation"`
	Execution        SnapshotExec     `json:"execution"`
	Result           *SnapshotResult  `json:"result"`
	Terminal         bool             `json:"terminal"`
	TerminalReason   *string          `json:"terminalReason"`
	SupersededBy     *string          `json:"supersededBy"`
	SnapshotRevision int              `json:"snapshotRevision"`
	UpdatedAt        string           `json:"updatedAt"`
}

// SnapshotDispatch is the dispatch part of a Snapshot.
type SnapshotDispatch struct {
	State       string  `json:"state"`
	CandidateID *string `json:"candidateId"`
	Attempts    int     `json:"attempts"`
	ErrorCode   *string `json:"errorCode"`
}

// SnapshotCancel is the cancellation part of a Snapshot.
type SnapshotCancel struct {
	State       string  `json:"state"`
	CancelID    *string `json:"cancelId"`
	RequestedAt *string `json:"requestedAt"`
	ConfirmedAt *string `json:"confirmedAt"`
	ErrorCode   *string `json:"errorCode"`
}

// SnapshotExec is the execution part of a Snapshot.
type SnapshotExec struct {
	State         string  `json:"state"`
	HoldReason    *string `json:"holdReason"`
	ProducerID    *string `json:"producerId"`
	ProducerEpoch *int    `json:"producerEpoch"`
	Sequence      *int    `json:"sequence"`
	ObservedAt    *string `json:"observedAt"`
	ReceivedAt    *string `json:"receivedAt"`
	Freshness     string  `json:"freshness"`
	Outcome       *string `json:"outcome"`
	ExitCode      *int    `json:"exitCode"`
}

// SnapshotResult is the reference to the reviewer's GitHub post. It never
// carries the post body or a separate approval flag.
type SnapshotResult struct {
	Source        string       `json:"source"`
	Kind          string       `json:"kind"`
	HeadSHA       string       `json:"headSha"`
	ResultVersion int          `json:"resultVersion"`
	Post          SnapshotPost `json:"post"`
}

// SnapshotPost identifies the GitHub comment.
type SnapshotPost struct {
	CommentID int64  `json:"commentId"`
	URL       string `json:"url"`
}
