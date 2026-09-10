// Package ghclient wraps the GitHub REST and GraphQL APIs for Copilot review operations.
package ghclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

// invalidatingTransport wraps an HTTP transport and calls invalidate(token) when
// GitHub returns HTTP 401, so the auth token cache is cleared immediately rather
// than waiting for cacheTTL to expire.
type invalidatingTransport struct {
	inner      http.RoundTripper
	token      string
	invalidate func(string)
}

func (t *invalidatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode == http.StatusUnauthorized {
		t.invalidate(t.token)
	}
	return resp, err
}

// copilotBotLogin is the login passed to the GraphQL requestReviewsByLogin mutation's
// botLogins field. The [bot] suffix is part of the actor identity on GitHub's side.
const copilotBotLogin = "copilot-pull-request-reviewer[bot]"

// copilotLogins lists the known GitHub Copilot reviewer identities used for
// detection in GetReviewData. Kept separate from copilotBotLogin (request path).
//
// "Copilot" (capitalized, no [bot] suffix) is returned by the REST
// requested_reviewers endpoint and must be included here so PENDING and
// IN_PROGRESS can be detected via IsCopilotInReviewers.
var copilotLogins = []string{
	"Copilot",
	"github-copilot[bot]",
	"copilot-pull-request-reviewer[bot]",
	"github-copilot",
}

func isCopilot(login string) bool {
	for _, l := range copilotLogins {
		if login == l {
			return true
		}
	}
	return false
}

// IsCopilotLogin reports whether login belongs to a known Copilot bot identity.
func IsCopilotLogin(login string) bool {
	return isCopilot(login)
}

// ReviewStatus represents the Copilot review lifecycle state for a PR.
type ReviewStatus string

const (
	StatusNotRequested ReviewStatus = "NOT_REQUESTED"
	StatusPending      ReviewStatus = "PENDING"
	StatusInProgress   ReviewStatus = "IN_PROGRESS"
	StatusCompleted    ReviewStatus = "COMPLETED"
	StatusBlocked      ReviewStatus = "BLOCKED"
)

// PullRequestReview is a review submitted on a pull request.
// It is our own type so callers outside this package do not need to import go-github.
type PullRequestReview struct {
	ID          int64
	State       string
	SubmittedAt *time.Time
}

// ReviewData holds the raw review information fetched from GitHub.
type ReviewData struct {
	// IsCopilotInReviewers is true when Copilot is in the PR's requested reviewers list.
	IsCopilotInReviewers bool
	// LatestCopilotReview is the most recently submitted Copilot review, or nil.
	LatestCopilotReview *PullRequestReview
	// RateLimitRemaining is the number of remaining API calls in the current window.
	RateLimitRemaining int
	// RateLimitReset is when the rate limit window resets.
	RateLimitReset time.Time
	// ReviewRequestedAt is the CreatedAt of the most recent "review_requested" timeline
	// event for Copilot. Used as the primary source for staleness detection and as the
	// requestedAt baseline when no trigger_log DB entry exists (AUTO trigger).
	ReviewRequestedAt *time.Time
	// WorkStartedAt is the CreatedAt of the most recent "copilot_work_started" timeline
	// event. When WorkStartedAt > ReviewRequestedAt, the review is IN_PROGRESS;
	// otherwise it is PENDING. This field is REST-timeline-only; GraphQL has no
	// corresponding __typename for this event.
	WorkStartedAt *time.Time
	// SkippedTimeline is true when the timeline fetch was skipped due to low rate-limit
	// budget. When true, ReviewRequestedAt and WorkStartedAt are nil and callers should
	// fall back to DB-recorded requestedAt for staleness decisions.
	SkippedTimeline bool
}

// Client wraps the GitHub API for Copilot review operations.
type Client struct {
	gh        *github.Client
	v4        *githubv4.Client
	threshold time.Duration // elapsed threshold for PENDING→IN_PROGRESS
}

// NewClient creates a new GitHub API client authenticated with the given token.
// ctx should be the request context so GitHub API calls are cancelled when the
// request is cancelled or times out.
// invalidate is called with the token when GitHub returns HTTP 401, allowing the
// auth layer to clear its cache immediately. May be nil to disable invalidation.
// threshold is the elapsed time after which PENDING becomes IN_PROGRESS.
func NewClient(ctx context.Context, token string, threshold time.Duration, invalidate func(string)) *Client {
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(ctx, src)
	if invalidate != nil {
		httpClient.Transport = &invalidatingTransport{
			inner:      httpClient.Transport,
			token:      token,
			invalidate: invalidate,
		}
	}
	gh, err := github.NewClient(github.WithHTTPClient(httpClient))
	if err != nil {
		panic(fmt.Sprintf("github.NewClient: %v", err))
	}
	return &Client{
		gh:        gh,
		v4:        githubv4.NewClient(httpClient),
		threshold: threshold,
	}
}

// NewClientWithTokenSource creates a Client backed by an arbitrary
// oauth2.TokenSource. Use this with a gatewayTokenSource (wrapped in
// oauth2.ReuseTokenSource) so long-lived watch goroutines pick up tokens
// rotated by the gateway transparently.
//
// invalidatingTransport is not attached here because the "current" token is
// dynamic — invalidation on 401 is deferred to PR-B per
// scottlz0310/review-raven#29.
func NewClientWithTokenSource(ctx context.Context, ts oauth2.TokenSource, threshold time.Duration) *Client {
	httpClient := oauth2.NewClient(ctx, ts)
	gh, err := github.NewClient(github.WithHTTPClient(httpClient))
	if err != nil {
		panic(fmt.Sprintf("github.NewClient: %v", err))
	}
	return &Client{
		gh:        gh,
		v4:        githubv4.NewClient(httpClient),
		threshold: threshold,
	}
}

// NewWithClients creates a Client from pre-built REST and GraphQL API clients.
// Intended for tests that need to inject mock HTTP servers in place of api.github.com.
func NewWithClients(gh *github.Client, v4 *githubv4.Client, threshold time.Duration) *Client {
	return &Client{gh: gh, v4: v4, threshold: threshold}
}

// NewWithHTTPClientAndURL creates a Client from an HTTP client and a base URL string,
// pointing only at the REST API (v4/GraphQL is unused). Intended for integration tests
// that stand up a minimal fake HTTP server without importing go-github directly.
func NewWithHTTPClientAndURL(httpClient *http.Client, baseURL string, threshold time.Duration) (*Client, error) {
	gh, err := github.NewClient(github.WithHTTPClient(httpClient), github.WithURLs(&baseURL, &baseURL))
	if err != nil {
		return nil, fmt.Errorf("github.NewClient: %w", err)
	}
	return &Client{gh: gh, threshold: threshold}, nil
}

// NewWithHTTPClientAndURLFull creates a Client with both REST and GraphQL clients
// pointing at baseURL, using httpClient for all requests. Intended for tests that
// need to route both REST and GraphQL calls to an httptest server without importing
// go-github or githubv4 directly.
func NewWithHTTPClientAndURLFull(httpClient *http.Client, baseURL string, threshold time.Duration) (*Client, error) {
	restBase := baseURL + "/"
	gh, err := github.NewClient(github.WithHTTPClient(httpClient), github.WithURLs(&restBase, &restBase))
	if err != nil {
		return nil, fmt.Errorf("github.NewClient: %w", err)
	}
	return &Client{gh: gh, v4: githubv4.NewEnterpriseClient(baseURL, httpClient), threshold: threshold}, nil
}

// GetReviewData fetches raw reviewer and review data for a PR from GitHub.
func (c *Client) GetReviewData(ctx context.Context, owner, repo string, prNumber int) (*ReviewData, error) {
	data := &ReviewData{}

	// Fetch current requested reviewers.
	reviewers, resp, err := c.gh.PullRequests.ListReviewers(ctx, owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		data.RateLimitRemaining = resp.Rate.Remaining
		data.RateLimitReset = resp.Rate.Reset.Time
	}
	for _, u := range reviewers.Users {
		if isCopilot(u.GetLogin()) {
			data.IsCopilotInReviewers = true
			break
		}
	}

	// Short-circuit if rate limit is already low to avoid consuming the second call.
	if data.RateLimitRemaining < 10 {
		return data, nil
	}

	// Fetch submitted reviews across all pages.
	reviewOpts := &github.ListOptions{PerPage: 100}
	for {
		reviews, resp2, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, prNumber, reviewOpts)
		if err != nil {
			return nil, err
		}
		if resp2 != nil {
			data.RateLimitRemaining = resp2.Rate.Remaining
			data.RateLimitReset = resp2.Rate.Reset.Time
		}
		for _, r := range reviews {
			if !isCopilot(r.GetUser().GetLogin()) {
				continue
			}
			var sat *time.Time
			if ts := r.GetSubmittedAt(); !ts.IsZero() {
				t := ts.Time
				sat = &t
			}
			var prevSat time.Time
			if data.LatestCopilotReview != nil && data.LatestCopilotReview.SubmittedAt != nil {
				prevSat = *data.LatestCopilotReview.SubmittedAt
			}
			var currSat time.Time
			if sat != nil {
				currSat = *sat
			}
			if data.LatestCopilotReview == nil || currSat.After(prevSat) {
				data.LatestCopilotReview = &PullRequestReview{
					ID:          r.GetID(),
					State:       r.GetState(),
					SubmittedAt: sat,
				}
			}
		}
		if resp2 == nil || resp2.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp2.NextPage
	}

	// Timeline is needed when Copilot is currently in requested_reviewers (PENDING vs
	// IN_PROGRESS distinction) OR when a Copilot review already exists and we need the
	// most recent review_requested event as the authoritative staleness baseline.
	needsTimeline := data.IsCopilotInReviewers || data.LatestCopilotReview != nil
	if needsTimeline {
		// Short-circuit before timeline fetch if rate limit is too low.
		if data.RateLimitRemaining < 10 {
			data.SkippedTimeline = true
			return data, nil
		}

		// Fetch REST timeline events for event-based PENDING/IN_PROGRESS detection
		// and review_requested timestamps. copilot_work_started exists only in the
		// REST timeline (no GraphQL equivalent).
		timelineOpts := &github.ListOptions{PerPage: 100}
		for {
			events, resp3, err := c.gh.Issues.ListIssueTimeline(ctx, owner, repo, prNumber, timelineOpts)
			if err != nil {
				// Non-fatal: timeline is best-effort; proceed with what we have.
				break
			}
			if resp3 != nil {
				data.RateLimitRemaining = resp3.Rate.Remaining
				data.RateLimitReset = resp3.Rate.Reset.Time
			}
			for _, ev := range events {
				switch ev.GetEvent() {
				case "review_requested":
					// Reviewer is json:"requested_reviewer" on the Timeline struct.
					if r := ev.GetReviewer(); r != nil && isCopilot(r.GetLogin()) {
						t := ev.GetCreatedAt().Time
						if data.ReviewRequestedAt == nil || t.After(*data.ReviewRequestedAt) {
							data.ReviewRequestedAt = &t
						}
					}
				case "copilot_work_started":
					t := ev.GetCreatedAt().Time
					if data.WorkStartedAt == nil || t.After(*data.WorkStartedAt) {
						data.WorkStartedAt = &t
					}
				}
			}
			if resp3 == nil || resp3.NextPage == 0 {
				break
			}
			timelineOpts.Page = resp3.NextPage
		}
	} // end if needsTimeline

	return data, nil
}

// DeriveStatus resolves the ReviewStatus from raw data and optional trigger_log requestedAt.
// requestedAt is the DB trigger_log entry time (MANUAL trigger); nil for AUTO trigger.
// prevReviewID, when non-nil, enables ID-based staleness detection: the existing review is
// treated as stale only if its ID matches prevReviewID (same review seen before the request).
//
// PENDING vs IN_PROGRESS is determined by the timeline events in data:
//   - WorkStartedAt > ReviewRequestedAt → IN_PROGRESS
//   - otherwise (WorkStartedAt absent, or before the latest request) → PENDING
//
// Staleness detection uses data.ReviewRequestedAt (REST timeline) as the primary source,
// falling back to the requestedAt parameter when timeline data is unavailable.
func (c *Client) DeriveStatus(data *ReviewData, requestedAt *time.Time, prevReviewID *string) ReviewStatus {
	return DeriveStatus(data, requestedAt, prevReviewID)
}

// DeriveStatus resolves the ReviewStatus from raw ReviewData.
// It is the package-level counterpart of (*Client).DeriveStatus for callers that do
// not hold a Client reference (e.g. the watch manager).
func DeriveStatus(data *ReviewData, requestedAt *time.Time, prevReviewID *string) ReviewStatus {
	// Prefer timeline-sourced requestedAt for accurate staleness detection.
	// Fall back to the DB trigger_log entry only when timeline data is unavailable.
	effectiveRequestedAt := data.ReviewRequestedAt
	if effectiveRequestedAt == nil {
		effectiveRequestedAt = requestedAt
	}

	if data.LatestCopilotReview != nil {
		// Only treat this review as relevant if it was submitted at/after the request time,
		// or (when prevReviewID is set) if its ID differs from the pre-request review.
		relevant := true
		if effectiveRequestedAt != nil {
			if prevReviewID != nil {
				// ID-based: relevant only if this is a different review from when the
				// request was made. Same ID means the old review is still present (stale).
				currentID := strconv.FormatInt(data.LatestCopilotReview.ID, 10)
				relevant = currentID != *prevReviewID
			} else {
				// Timestamp-based fallback for entries without prevReviewID (backward compat).
				// - Use !Before (≥) instead of After (>) to include same-second events.
				// - Zero submittedAt means the review has no timestamp → treat as stale.
				var sat time.Time
				if data.LatestCopilotReview.SubmittedAt != nil {
					sat = *data.LatestCopilotReview.SubmittedAt
				}
				relevant = !sat.IsZero() && !sat.Before(*effectiveRequestedAt)
			}
		}
		if relevant {
			if data.LatestCopilotReview.State == "CHANGES_REQUESTED" {
				return StatusBlocked
			}
			return StatusCompleted
		}
	}
	if data.IsCopilotInReviewers {
		// Event-based PENDING vs IN_PROGRESS: copilot_work_started after review_requested
		// means work has begun on the current cycle.
		if data.WorkStartedAt != nil &&
			(data.ReviewRequestedAt == nil || data.WorkStartedAt.After(*data.ReviewRequestedAt)) {
			return StatusInProgress
		}
		return StatusPending
	}
	return StatusNotRequested
}

// DeriveStatusWithThreshold is kept for backward compatibility.
// The threshold parameter is ignored; use DeriveStatus instead.
func DeriveStatusWithThreshold(_ time.Duration, data *ReviewData, requestedAt *time.Time, prevReviewID *string) ReviewStatus {
	return DeriveStatus(data, requestedAt, prevReviewID)
}

// IsAuthError reports whether err is a GitHub authentication failure.
// It handles both REST API errors (*github.ErrorResponse with HTTP 401) and
// GraphQL errors from shurcooL/githubv4 whose message contains "401 Unauthorized".
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) {
		return ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnauthorized
	}
	// shurcooL/githubv4 returns a plain error with the message
	// "non-200 OK status code: 401 Unauthorized body: ..." for HTTP 401.
	return strings.Contains(err.Error(), "401 Unauthorized")
}

// IsGatewayAuthError reports whether err is a gateway sentinel that requires
// the user to re-authenticate. It returns true for:
//   - ErrGatewaySubjectGone: the gateway has no cached token record for the
//     subject; the user must issue a fresh client request to re-seed the cache.
//   - ErrGatewayRotationFailed: the gateway tried to rotate the refresh token
//     but GitHub explicitly rejected it; the user must re-authenticate.
//
// ErrGatewayUpstreamFailure is intentionally excluded: it is a transient
// upstream failure that may resolve on retry and does not require user action.
func IsGatewayAuthError(err error) bool {
	return errors.Is(err, ErrGatewaySubjectGone) || errors.Is(err, ErrGatewayRotationFailed)
}

// prNodeIDQuery fetches the GraphQL node ID for a pull request.
// Used by RequestCopilotReview to obtain the ID required by the mutation.
type prNodeIDQuery struct {
	Repository struct {
		PullRequest struct {
			ID githubv4.ID
		} `graphql:"pullRequest(number: $pr)"`
	} `graphql:"repository(owner: $owner, name: $repo)"`
}

// requestReviewsByLoginMutation is the GraphQL mutation for requesting PR reviews by login.
type requestReviewsByLoginMutation struct {
	RequestReviewsByLogin struct {
		ClientMutationID string `graphql:"clientMutationId"`
	} `graphql:"requestReviewsByLogin(input: $input)"`
}

// buildCopilotReviewInput constructs the mutation input for adding Copilot as a reviewer.
// union: true preserves existing reviewers (additive); false would replace the entire set.
func buildCopilotReviewInput(prNodeID githubv4.ID) githubv4.RequestReviewsByLoginInput {
	botLogins := []githubv4.String{githubv4.String(copilotBotLogin)}
	userLogins := []githubv4.String{}
	teamSlugs := []githubv4.String{}
	union := githubv4.Boolean(true)

	return githubv4.RequestReviewsByLoginInput{
		PullRequestID: prNodeID,
		BotLogins:     &botLogins,
		UserLogins:    &userLogins,
		TeamSlugs:     &teamSlugs,
		Union:         &union,
	}
}

// RequestCopilotReview adds Copilot as a reviewer on the given PR using the GraphQL
// requestReviewsByLogin mutation. This is the only reliable path on github.com;
// the REST requested_reviewers endpoint silently no-ops for bot actors (#47).
func (c *Client) RequestCopilotReview(ctx context.Context, owner, repo string, prNumber int) error {
	if prNumber <= 0 || prNumber > math.MaxInt32 {
		return fmt.Errorf("pr number out of valid range: %d", prNumber)
	}

	// Step 1: resolve the PR's GraphQL node ID (distinct from the REST integer ID).
	var nodeQ prNodeIDQuery
	if err := c.v4.Query(ctx, &nodeQ, map[string]interface{}{
		"owner": githubv4.String(owner),
		"repo":  githubv4.String(repo),
		"pr":    githubv4.Int(int32(prNumber)), //nolint:gosec // range checked above
	}); err != nil {
		return fmt.Errorf("failed to fetch PR node ID: %w", err)
	}
	prNodeID := nodeQ.Repository.PullRequest.ID
	// Guard: Query may succeed with a zero-value struct when the PR doesn't exist
	// or the token lacks permission. Catch both nil and empty-string cases.
	if prNodeID == nil || prNodeID == "" {
		return fmt.Errorf("pull request node ID is empty (PR #%d may not exist or token lacks permission)", prNumber)
	}

	// Step 2: request review via GraphQL mutation.
	var m requestReviewsByLoginMutation
	input := buildCopilotReviewInput(prNodeID)
	if err := c.v4.Mutate(ctx, &m, input, nil); err != nil {
		return fmt.Errorf("requestReviewsByLogin mutation failed: %w", err)
	}
	return nil
}

// ─── GraphQL types for review thread operations ───────────────────────────────

// reviewThreadsPageQuery fetches one page of PR review threads via GraphQL.
type reviewThreadsPageQuery struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				Nodes    []reviewThreadNode
				PageInfo struct {
					HasNextPage githubv4.Boolean
					EndCursor   githubv4.String
				}
			} `graphql:"reviewThreads(first: 100, after: $cursor)"`
		} `graphql:"pullRequest(number: $pr)"`
	} `graphql:"repository(owner: $owner, name: $repo)"`
}

type reviewThreadNode struct {
	ID         githubv4.ID
	IsResolved githubv4.Boolean
	IsOutdated githubv4.Boolean
	Path       githubv4.String
	Line       *githubv4.Int
	StartLine  *githubv4.Int
	Comments   struct {
		Nodes []struct {
			DatabaseID int64
			URL        githubv4.String
			Body       githubv4.String
			CreatedAt  githubv4.DateTime
			Author     struct {
				Login    githubv4.String
				TypeName githubv4.String `graphql:"__typename"`
			}
		}
	} `graphql:"comments(first: 10)"`
}

// reviewThreadMetadataComment は投稿者ゲートで使う本文なしコメント射影です。
// 射影を分離することで、メタデータのみの取得時に GitHub から本文を選択しません。
type reviewThreadMetadataComment struct {
	DatabaseID int64
	URL        githubv4.String
	CreatedAt  githubv4.DateTime
	Author     struct {
		Login    githubv4.String
		TypeName githubv4.String `graphql:"__typename"`
	}
}

type reviewThreadMetadataComments struct {
	Nodes    []reviewThreadMetadataComment
	PageInfo struct {
		HasNextPage githubv4.Boolean
		EndCursor   githubv4.String
	}
}

type reviewThreadMetadataNode struct {
	ID         githubv4.ID
	IsResolved githubv4.Boolean
	IsOutdated githubv4.Boolean
	Path       githubv4.String
	Line       *githubv4.Int
	StartLine  *githubv4.Int
	Comments   reviewThreadMetadataComments `graphql:"comments(first: 50)"`
}

// reviewThreadsMetadataPageQuery は GraphQL で PR レビュースレッドの本文なしページを取得します。
type reviewThreadsMetadataPageQuery struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				Nodes    []reviewThreadMetadataNode
				PageInfo struct {
					HasNextPage githubv4.Boolean
					EndCursor   githubv4.String
				}
			} `graphql:"reviewThreads(first: 100, after: $cursor)"`
		} `graphql:"pullRequest(number: $pr)"`
	} `graphql:"repository(owner: $owner, name: $repo)"`
}

type reviewThreadMetadataCommentsPageQuery struct {
	Node struct {
		PullRequestReviewThread struct {
			Comments reviewThreadMetadataComments `graphql:"comments(first: 50, after: $cursor)"`
		} `graphql:"... on PullRequestReviewThread"`
	} `graphql:"node(id: $id)"`
}

// threadNodeQuery fetches a single thread's resolved status by global node ID.
type threadNodeQuery struct {
	Node struct {
		PullRequestReviewThread struct {
			IsResolved githubv4.Boolean
		} `graphql:"... on PullRequestReviewThread"`
	} `graphql:"node(id: $id)"`
}

// threadMetadataQuery fetches owner, repo, PR number, and first comment database ID
// from a review thread node ID. Used to reply via REST API.
type threadMetadataQuery struct {
	Node struct {
		PullRequestReviewThread struct {
			PullRequest struct {
				Number     githubv4.Int
				Repository struct {
					Name  githubv4.String
					Owner struct {
						Login githubv4.String
					}
				}
			}
			Comments struct {
				Nodes []struct {
					DatabaseId int64 // githubv4.Int (int32) overflows for large comment IDs
				}
			} `graphql:"comments(first: 1)"`
		} `graphql:"... on PullRequestReviewThread"`
	} `graphql:"node(id: $id)"`
}

// resolveThreadMutation is the GraphQL mutation for resolving a review thread.
type resolveThreadMutation struct {
	ResolveReviewThread struct {
		Thread struct {
			IsResolved githubv4.Boolean
		}
	} `graphql:"resolveReviewThread(input: $input)"`
}

// ResolveReviewThreadInput is the input for resolveThreadMutation.
// Must be PascalCase so shurcooL/githubv4 sends the correct GraphQL type name.
type ResolveReviewThreadInput struct {
	ThreadID githubv4.ID `json:"threadId"`
}

// ─── Public review thread types ──────────────────────────────────────────────

// ThreadComment is a single comment within a review thread.
type ThreadComment struct {
	CommentID  string
	Author     string
	AuthorType string
	URL        string
	Body       string
	CreatedAt  string
}

// ReviewThread is the parsed representation of a PR review thread.
type ReviewThread struct {
	ID         string
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       *int32
	StartLine  *int32
	Comments   []ThreadComment
}

// ReviewThreadsOptions は GetReviewThreadsWithOptions が選択するフィールドを制御します。
type ReviewThreadsOptions struct {
	// IncludeBodies はレビュコメント本文を選択します。信頼できる投稿者の検証前に使う
	// メタデータのみの射影では false に設定します。
	IncludeBodies bool
}

// ReviewThreadsResult は GitHub から返された全ページと、消費した reviewThreads ページ数を含みます。
type ReviewThreadsResult struct {
	Threads   []ReviewThread
	PageCount int
}

// ReplyResult holds the result of a reply-to-thread operation.
type ReplyResult struct {
	CommentID string
	CreatedAt string
}

// ─── GraphQL methods ──────────────────────────────────────────────────────────

// GetReviewThreads fetches all review threads for a PR using GraphQL (paginated).
func (c *Client) GetReviewThreads(ctx context.Context, owner, repo string, pr int) ([]ReviewThread, error) {
	result, err := c.GetReviewThreadsWithOptions(ctx, owner, repo, pr, ReviewThreadsOptions{IncludeBodies: true})
	if err != nil {
		return nil, err
	}
	return result.Threads, nil
}

// GetReviewThreadsWithOptions はページネーション付き GraphQL 射影で PR の全レビュースレッドを取得します。
// メタデータのみのモードではコメント本文を選択しません。
func (c *Client) GetReviewThreadsWithOptions(
	ctx context.Context,
	owner, repo string,
	pr int,
	options ReviewThreadsOptions,
) (ReviewThreadsResult, error) {
	if pr <= 0 || pr > math.MaxInt32 {
		return ReviewThreadsResult{}, fmt.Errorf("pr number out of valid range: %d", pr)
	}
	if !options.IncludeBodies {
		return c.getReviewThreadMetadata(ctx, owner, repo, pr)
	}
	return c.getReviewThreadsWithBodies(ctx, owner, repo, pr)
}

func reviewThreadsVariables(owner, repo string, pr int) map[string]interface{} {
	vars := map[string]interface{}{
		"owner":  githubv4.String(owner),
		"repo":   githubv4.String(repo),
		"pr":     githubv4.Int(int32(pr)), //nolint:gosec // range checked above
		"cursor": (*githubv4.String)(nil),
	}
	return vars
}

func (c *Client) getReviewThreadsWithBodies(ctx context.Context, owner, repo string, pr int) (ReviewThreadsResult, error) {
	vars := reviewThreadsVariables(owner, repo, pr)
	var allNodes []reviewThreadNode
	pageCount := 0
	for {
		var q reviewThreadsPageQuery
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return ReviewThreadsResult{}, fmt.Errorf("graphql query failed: %w", err)
		}
		pageCount++
		allNodes = append(allNodes, q.Repository.PullRequest.ReviewThreads.Nodes...)
		if !bool(q.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage) {
			break
		}
		cursor := q.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
		if cursor == "" {
			return ReviewThreadsResult{}, fmt.Errorf("graphql query returned hasNextPage=true with empty endCursor")
		}
		vars["cursor"] = &cursor
	}

	threads := make([]ReviewThread, 0, len(allNodes))
	for _, n := range allNodes {
		t := ReviewThread{
			ID:         fmt.Sprintf("%v", n.ID),
			IsResolved: bool(n.IsResolved),
			IsOutdated: bool(n.IsOutdated),
			Path:       string(n.Path),
		}
		if n.Line != nil {
			v := int32(*n.Line)
			t.Line = &v
		}
		if n.StartLine != nil {
			v := int32(*n.StartLine)
			t.StartLine = &v
		}
		for _, c := range n.Comments.Nodes {
			t.Comments = append(t.Comments, ThreadComment{
				CommentID:  formatCommentID(c.DatabaseID),
				Author:     string(c.Author.Login),
				AuthorType: string(c.Author.TypeName),
				URL:        string(c.URL),
				Body:       string(c.Body),
				CreatedAt:  c.CreatedAt.Format(time.RFC3339),
			})
		}
		threads = append(threads, t)
	}
	return ReviewThreadsResult{Threads: threads, PageCount: pageCount}, nil
}

func (c *Client) getReviewThreadMetadata(ctx context.Context, owner, repo string, pr int) (ReviewThreadsResult, error) {
	vars := reviewThreadsVariables(owner, repo, pr)
	var allNodes []reviewThreadMetadataNode
	pageCount := 0
	for {
		var q reviewThreadsMetadataPageQuery
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return ReviewThreadsResult{}, fmt.Errorf("graphql metadata query failed: %w", err)
		}
		pageCount++
		allNodes = append(allNodes, q.Repository.PullRequest.ReviewThreads.Nodes...)
		if !bool(q.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage) {
			break
		}
		cursor := q.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
		if cursor == "" {
			return ReviewThreadsResult{}, fmt.Errorf("graphql metadata query returned hasNextPage=true with empty endCursor")
		}
		vars["cursor"] = &cursor
	}

	threads := make([]ReviewThread, 0, len(allNodes))
	for _, n := range allNodes {
		t := ReviewThread{
			ID:         fmt.Sprintf("%v", n.ID),
			IsResolved: bool(n.IsResolved),
			IsOutdated: bool(n.IsOutdated),
			Path:       string(n.Path),
		}
		if n.Line != nil {
			v := int32(*n.Line)
			t.Line = &v
		}
		if n.StartLine != nil {
			v := int32(*n.StartLine)
			t.StartLine = &v
		}
		if err := c.appendReviewThreadMetadataComments(ctx, &t, n.ID, n.Comments); err != nil {
			return ReviewThreadsResult{}, err
		}
		threads = append(threads, t)
	}
	return ReviewThreadsResult{Threads: threads, PageCount: pageCount}, nil
}

func (c *Client) appendReviewThreadMetadataComments(
	ctx context.Context,
	thread *ReviewThread,
	threadID githubv4.ID,
	comments reviewThreadMetadataComments,
) error {
	appendComments := func(page reviewThreadMetadataComments) {
		for _, comment := range page.Nodes {
			thread.Comments = append(thread.Comments, ThreadComment{
				CommentID:  formatCommentID(comment.DatabaseID),
				Author:     string(comment.Author.Login),
				AuthorType: string(comment.Author.TypeName),
				URL:        string(comment.URL),
				CreatedAt:  comment.CreatedAt.Format(time.RFC3339),
			})
		}
	}

	appendComments(comments)
	if !bool(comments.PageInfo.HasNextPage) {
		return nil
	}

	initialCursor := comments.PageInfo.EndCursor
	if initialCursor == "" {
		return fmt.Errorf("graphql metadata comments query returned hasNextPage=true with empty endCursor")
	}
	vars := map[string]interface{}{
		"id":     threadID,
		"cursor": &initialCursor,
	}
	for {
		var q reviewThreadMetadataCommentsPageQuery
		if err := c.v4.Query(ctx, &q, vars); err != nil {
			return fmt.Errorf("graphql metadata comments query failed: %w", err)
		}
		page := q.Node.PullRequestReviewThread.Comments
		appendComments(page)
		if !bool(page.PageInfo.HasNextPage) {
			return nil
		}
		cursor := page.PageInfo.EndCursor
		if cursor == "" {
			return fmt.Errorf("graphql metadata comments query returned hasNextPage=true with empty endCursor")
		}
		vars["cursor"] = &cursor
	}
}

func formatCommentID(databaseID int64) string {
	if databaseID <= 0 {
		return ""
	}
	return strconv.FormatInt(databaseID, 10)
}

// IsThreadResolved checks whether a review thread is already resolved.
// threadID must be a PRRT_xxx global node ID.
func (c *Client) IsThreadResolved(ctx context.Context, threadID string) (bool, error) {
	var q threadNodeQuery
	vars := map[string]interface{}{
		"id": githubv4.ID(threadID),
	}
	if err := c.v4.Query(ctx, &q, vars); err != nil {
		return false, fmt.Errorf("graphql query failed: %w", err)
	}
	return bool(q.Node.PullRequestReviewThread.IsResolved), nil
}

// ReplyToThread adds a reply to a review thread via the REST API.
// It first queries the thread node to resolve owner/repo/PR/commentID,
// then calls CreateCommentInReplyTo. This avoids the deprecated
// addPullRequestReviewCommentReply GraphQL mutation.
func (c *Client) ReplyToThread(ctx context.Context, threadID, body string) (ReplyResult, error) {
	// Resolve thread metadata via GraphQL.
	var q threadMetadataQuery
	if err := c.v4.Query(ctx, &q, map[string]interface{}{"id": githubv4.ID(threadID)}); err != nil {
		return ReplyResult{}, fmt.Errorf("graphql query failed: %w", err)
	}
	meta := q.Node.PullRequestReviewThread
	if len(meta.Comments.Nodes) == 0 {
		return ReplyResult{}, fmt.Errorf("thread has no comments")
	}
	owner := string(meta.PullRequest.Repository.Owner.Login)
	repo := string(meta.PullRequest.Repository.Name)
	prNum := int(meta.PullRequest.Number)
	commentID := int64(meta.Comments.Nodes[0].DatabaseId)

	// Post reply via REST API.
	comment, _, err := c.gh.PullRequests.CreateCommentInReplyTo(ctx, owner, repo, prNum, body, commentID)
	if err != nil {
		return ReplyResult{}, fmt.Errorf("REST API reply failed: %w", err)
	}
	return ReplyResult{
		CommentID: strconv.FormatInt(comment.GetID(), 10),
		CreatedAt: comment.GetCreatedAt().Format(time.RFC3339),
	}, nil
}

// CIStatus holds the result of a CI check run status query.
type CIStatus struct {
	// OK is true when all check runs are completed with a passing conclusion
	// (success, skipped, or neutral). Also true when no check runs exist.
	OK bool
	// PendingChecks contains the names of check runs not yet in "completed" status.
	PendingChecks []string
	// FailedChecks contains the names of check runs that completed with a
	// non-passing conclusion (e.g., failure, cancelled, timed_out, action_required).
	FailedChecks []string
}

// GetCIStatus returns a CIStatus for the PR's head commit. Check runs are
// deduplicated by (appID, name), keeping the latest (highest ID) run per
// (app, name) pair. This collapses reruns of the same check while preserving
// distinct checks that happen to share a name across different GitHub Apps.
func (c *Client) GetCIStatus(ctx context.Context, owner, repo string, prNumber int) (CIStatus, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		return CIStatus{}, fmt.Errorf("failed to get PR: %w", err)
	}
	sha := pr.GetHead().GetSHA()
	if sha == "" {
		return CIStatus{}, fmt.Errorf("PR #%d head SHA is empty", prNumber)
	}

	// Collect all check runs, deduplicating by (appID, name) to collapse reruns of the
	// same check while preserving distinct checks that share a name across different apps.
	latest := make(map[string]*github.CheckRun)
	opts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		result, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return CIStatus{}, fmt.Errorf("failed to list check runs: %w", err)
		}
		for i := range result.CheckRuns {
			r := result.CheckRuns[i]
			key := strconv.FormatInt(r.GetApp().GetID(), 10) + ":" + r.GetName()
			if existing, ok := latest[key]; !ok || r.GetID() > existing.GetID() {
				latest[key] = r
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	var pending, failed []string
	for _, r := range latest {
		name := r.GetName()
		if r.GetStatus() != "completed" {
			pending = append(pending, name)
			continue
		}
		switch r.GetConclusion() {
		case "success", "skipped", "neutral":
			// passing
		default:
			failed = append(failed, name)
		}
	}
	sort.Strings(pending)
	sort.Strings(failed)
	return CIStatus{OK: len(pending) == 0 && len(failed) == 0, PendingChecks: pending, FailedChecks: failed}, nil
}

// ResolveThread resolves a review thread. Returns true if it was already resolved before the call.
func (c *Client) ResolveThread(ctx context.Context, threadID string) (alreadyResolved bool, err error) {
	alreadyResolved, err = c.IsThreadResolved(ctx, threadID)
	if err != nil {
		return false, err
	}
	if alreadyResolved {
		return true, nil
	}
	var m resolveThreadMutation
	input := ResolveReviewThreadInput{ThreadID: githubv4.ID(threadID)}
	if err := c.v4.Mutate(ctx, &m, input, nil); err != nil {
		return false, fmt.Errorf("graphql mutation failed: %w", err)
	}
	return false, nil
}

// TokenDiagnostics holds identity and scope information for a GitHub token,
// resolved from GET /user. It never carries the token's raw value.
type TokenDiagnostics struct {
	// Login is the authenticated user's GitHub login.
	Login string
	// Scopes is the OAuth scope list granted to the token, parsed from the
	// X-OAuth-Scopes response header. Empty for fine-grained PATs and GitHub
	// App installation tokens, which do not set this header.
	Scopes []string
}

// GetTokenDiagnostics fetches the authenticated user's login and the token's
// OAuth scopes (from the X-OAuth-Scopes response header on GET /user).
// Intended to diagnose PERMISSION_DENIED failures where read operations
// succeed but a write operation is rejected because the token's scope does
// not cover it (review-raven#89). The raw token value is never returned.
func GetTokenDiagnostics(ctx context.Context, token string) (TokenDiagnostics, error) {
	if token == "" {
		return TokenDiagnostics{}, errors.New("token must not be empty")
	}
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(ctx, src)
	client, err := github.NewClient(github.WithHTTPClient(httpClient))
	if err != nil {
		return TokenDiagnostics{}, fmt.Errorf("github.NewClient: %w", err)
	}
	return getTokenDiagnostics(ctx, client)
}

func getTokenDiagnostics(ctx context.Context, client *github.Client) (TokenDiagnostics, error) {
	user, resp, err := client.Users.Get(ctx, "")
	if err != nil {
		return TokenDiagnostics{}, err
	}
	login := user.GetLogin()
	if login == "" {
		return TokenDiagnostics{}, errors.New("empty login returned from GitHub API")
	}
	var scopes []string
	if resp != nil && resp.Response != nil {
		if raw := resp.Response.Header.Get("X-OAuth-Scopes"); raw != "" {
			for _, s := range strings.Split(raw, ",") {
				if s = strings.TrimSpace(s); s != "" {
					scopes = append(scopes, s)
				}
			}
		}
	}
	return TokenDiagnostics{Login: login, Scopes: scopes}, nil
}
