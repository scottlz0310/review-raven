package ghclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/shurcooL/githubv4"
)

// newReview creates a PullRequestReview with the given state and optional submittedAt.
func newReview(state string, submittedAt *time.Time) *PullRequestReview {
	return &PullRequestReview{
		State:       state,
		SubmittedAt: submittedAt,
	}
}

// TestCopilotBotLoginValue guards against typos in the login constant.
// A wrong value here would cause a silent failure where GitHub accepts the mutation
// but Copilot is never actually added as a reviewer.
func TestCopilotBotLoginValue(t *testing.T) {
	const want = "copilot-pull-request-reviewer[bot]"
	if copilotBotLogin != want {
		t.Errorf("copilotBotLogin = %q, want %q", copilotBotLogin, want)
	}
}

// TestBuildCopilotReviewInput verifies the mutation input constructed by
// buildCopilotReviewInput satisfies the two critical invariants:
//
//  1. union must be true — preserves existing human reviewers.
//     union:false would replace the entire reviewer set with Copilot only.
//  2. botLogins[0] must equal copilotBotLogin — the exact identity GitHub expects.
func TestBuildCopilotReviewInput(t *testing.T) {
	testNodeID := githubv4.ID("PR_kwDOABCDEF12345")
	input := buildCopilotReviewInput(testNodeID)

	// Invariant 1: union must be true (additive, not replacing existing reviewers).
	if input.Union == nil || !bool(*input.Union) {
		t.Error("Union must be true to preserve existing human reviewers; false would remove them")
	}

	// Invariant 2: exactly one bot login with the correct value.
	if input.BotLogins == nil {
		t.Fatal("BotLogins must be set")
	}
	if len(*input.BotLogins) != 1 {
		t.Fatalf("BotLogins length = %d, want 1", len(*input.BotLogins))
	}
	if got := string((*input.BotLogins)[0]); got != copilotBotLogin {
		t.Errorf("BotLogins[0] = %q, want %q", got, copilotBotLogin)
	}

	// Sanity: userLogins and teamSlugs must be empty — we're only adding Copilot.
	if input.UserLogins == nil || len(*input.UserLogins) != 0 {
		t.Errorf("UserLogins must be empty, got %v", input.UserLogins)
	}
	if input.TeamSlugs == nil || len(*input.TeamSlugs) != 0 {
		t.Errorf("TeamSlugs must be empty, got %v", input.TeamSlugs)
	}

	// Sanity: PR node ID is passed through unchanged.
	if input.PullRequestID != testNodeID {
		t.Errorf("PullRequestID = %v, want %v", input.PullRequestID, testNodeID)
	}
}

func TestRequestCopilotReviewUsesRequestReviewsByLoginInput(t *testing.T) {
	const (
		owner    = "owner"
		repo     = "repo"
		pr       = 123
		prNodeID = "PR_kwDOABCDEF12345"
	)

	var sawNodeIDQuery atomic.Bool
	var sawRequestMutation atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
			http.Error(w, "failed to read body", http.StatusInternalServerError)
			return
		}

		var req struct {
			Query     string                     `json:"query"`
			Variables map[string]json.RawMessage `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("json.Unmarshal() error = %v", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		normalizedQuery := strings.Join(strings.Fields(req.Query), "")

		switch {
		case strings.Contains(normalizedQuery, "pullRequest(number:$pr)") && strings.Contains(normalizedQuery, "id"):
			sawNodeIDQuery.Store(true)
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"id":%q}}}}`, prNodeID)
		case strings.Contains(normalizedQuery, "requestReviewsByLogin(input:$input)"):
			sawRequestMutation.Store(true)
			if !strings.Contains(normalizedQuery, "RequestReviewsByLoginInput") {
				t.Errorf("mutation query = %q, want RequestReviewsByLoginInput", req.Query)
			}
			if strings.Contains(normalizedQuery, "requestReviewsByLoginInput") {
				t.Errorf("mutation query = %q, unexpected lower-camel input type", req.Query)
			}

			var input struct {
				PullRequestID string   `json:"pullRequestId"`
				BotLogins     []string `json:"botLogins"`
				UserLogins    []string `json:"userLogins"`
				TeamSlugs     []string `json:"teamSlugs"`
				Union         bool     `json:"union"`
			}
			if err := json.Unmarshal(req.Variables["input"], &input); err != nil {
				t.Errorf("json.Unmarshal(input) error = %v", err)
				http.Error(w, "invalid input payload", http.StatusBadRequest)
				return
			}
			if input.PullRequestID != prNodeID {
				t.Errorf("input.pullRequestId = %q, want %q", input.PullRequestID, prNodeID)
			}
			if len(input.BotLogins) != 1 || input.BotLogins[0] != copilotBotLogin {
				t.Errorf("input.botLogins = %v, want [%q]", input.BotLogins, copilotBotLogin)
			}
			if len(input.UserLogins) != 0 {
				t.Errorf("input.userLogins = %v, want empty", input.UserLogins)
			}
			if len(input.TeamSlugs) != 0 {
				t.Errorf("input.teamSlugs = %v, want empty", input.TeamSlugs)
			}
			if !input.Union {
				t.Error("input.union = false, want true")
			}

			_, _ = fmt.Fprint(w, `{"data":{"requestReviewsByLogin":{"clientMutationId":""}}}`)
		default:
			_, _ = fmt.Fprint(w, `{"data":{}}`)
		}
	}))
	defer srv.Close()

	c := &Client{
		v4: githubv4.NewEnterpriseClient(srv.URL, srv.Client()),
	}

	if err := c.RequestCopilotReview(context.Background(), owner, repo, pr); err != nil {
		t.Fatalf("RequestCopilotReview() error = %v", err)
	}
	if !sawNodeIDQuery.Load() {
		t.Fatal("did not observe PR node ID query")
	}
	if !sawRequestMutation.Load() {
		t.Fatal("did not observe requestReviewsByLogin mutation")
	}
}

func TestDeriveStatus(t *testing.T) {
	c := &Client{}

	now := time.Now()
	oneSecAgo := now.Add(-time.Second)
	oneMinAgo := now.Add(-1 * time.Minute)
	twoMinAgo := now.Add(-2 * time.Minute)
	threeMinAgo := now.Add(-3 * time.Minute)
	oneMinLater := now.Add(1 * time.Minute)

	tests := []struct {
		name        string
		data        *ReviewData
		requestedAt *time.Time
		want        ReviewStatus
	}{
		// ── no review ────────────────────────────────────────────────────────────
		{
			name: "no review, no reviewers → NOT_REQUESTED",
			data: &ReviewData{},
			want: StatusNotRequested,
		},
		{
			// No work_started event → PENDING regardless of requestedAt.
			name: "no review, copilot in reviewers, no work_started → PENDING",
			data: &ReviewData{IsCopilotInReviewers: true},
			want: StatusPending,
		},
		{
			// ReviewRequestedAt set but no WorkStartedAt → PENDING.
			name: "no review, copilot in reviewers, review_requested but no work_started → PENDING",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				ReviewRequestedAt:    &oneSecAgo,
			},
			want: StatusPending,
		},
		{
			// WorkStartedAt after ReviewRequestedAt → IN_PROGRESS.
			name: "no review, copilot in reviewers, work_started after review_requested → IN_PROGRESS",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				ReviewRequestedAt:    &twoMinAgo,
				WorkStartedAt:        &oneMinAgo,
			},
			want: StatusInProgress,
		},
		{
			// WorkStartedAt set but no ReviewRequestedAt → IN_PROGRESS.
			name: "no review, copilot in reviewers, work_started, no review_requested → IN_PROGRESS",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				WorkStartedAt:        &oneMinAgo,
			},
			want: StatusInProgress,
		},
		{
			// Rereview PENDING: new review_requested (oneMinAgo) is after old work_started (twoMinAgo).
			name: "no review, copilot in reviewers, old work_started before new review_requested → PENDING",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				ReviewRequestedAt:    &oneMinAgo,
				WorkStartedAt:        &twoMinAgo, // old cycle, before new request
			},
			want: StatusPending,
		},

		// ── review exists, no requestedAt (backward compat) ─────────────────────
		{
			name: "APPROVED review, no requestedAt → COMPLETED",
			data: &ReviewData{LatestCopilotReview: newReview("APPROVED", &oneMinAgo)},
			want: StatusCompleted,
		},
		{
			name: "CHANGES_REQUESTED review, no requestedAt → BLOCKED",
			data: &ReviewData{LatestCopilotReview: newReview("CHANGES_REQUESTED", &oneMinAgo)},
			want: StatusBlocked,
		},

		// ── review submitted AFTER requestedAt (DB fallback) ─────────────────────
		{
			name:        "APPROVED review after DB requestedAt → COMPLETED",
			data:        &ReviewData{LatestCopilotReview: newReview("APPROVED", &oneMinLater)},
			requestedAt: &now,
			want:        StatusCompleted,
		},
		{
			name:        "CHANGES_REQUESTED review after DB requestedAt → BLOCKED",
			data:        &ReviewData{LatestCopilotReview: newReview("CHANGES_REQUESTED", &oneMinLater)},
			requestedAt: &now,
			want:        StatusBlocked,
		},

		// ── timeline ReviewRequestedAt takes priority over DB requestedAt ─────────
		{
			// Review (1min ago) is AFTER timeline ReviewRequestedAt (2min ago) → COMPLETED.
			// DB requestedAt (now) would make it stale, but timeline takes priority.
			name: "APPROVED review after timeline review_requested, DB would mark stale → COMPLETED",
			data: &ReviewData{
				LatestCopilotReview: newReview("APPROVED", &oneMinAgo),
				ReviewRequestedAt:   &twoMinAgo, // timeline: 2min ago
			},
			requestedAt: &now, // DB: now (would falsely mark as stale)
			want:        StatusCompleted,
		},

		// ── review submitted at EXACTLY requestedAt (same-second inclusive) ───────
		{
			name:        "APPROVED review at exact requestedAt → COMPLETED (same-second inclusive)",
			data:        &ReviewData{LatestCopilotReview: newReview("APPROVED", &now)},
			requestedAt: &now,
			want:        StatusCompleted,
		},

		// ── stale review (submitted BEFORE review_requested) ─────────────────────
		{
			// Review (3min ago) is older than ReviewRequestedAt (1min ago) → stale.
			// No WorkStartedAt → PENDING.
			name: "stale APPROVED review, copilot in reviewers, no work_started → PENDING",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				LatestCopilotReview:  newReview("APPROVED", &threeMinAgo),
				ReviewRequestedAt:    &oneMinAgo, // new request after old review
			},
			want: StatusPending,
		},
		{
			name: "stale CHANGES_REQUESTED review, copilot in reviewers, no work_started → PENDING (not BLOCKED)",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				LatestCopilotReview:  newReview("CHANGES_REQUESTED", &threeMinAgo),
				ReviewRequestedAt:    &oneMinAgo,
			},
			want: StatusPending,
		},
		{
			// Stale review but no Copilot in reviewers (request cancelled) → NOT_REQUESTED.
			name: "stale review, copilot NOT in reviewers → NOT_REQUESTED",
			data: &ReviewData{
				LatestCopilotReview: newReview("APPROVED", &threeMinAgo),
				ReviewRequestedAt:   &oneMinAgo,
			},
			want: StatusNotRequested,
		},
		{
			// Stale review + work_started after new review_requested → IN_PROGRESS.
			name: "stale review, copilot in reviewers, work_started after review_requested → IN_PROGRESS",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				LatestCopilotReview:  newReview("APPROVED", &threeMinAgo),
				ReviewRequestedAt:    &twoMinAgo,
				WorkStartedAt:        &oneMinAgo, // after review_requested
			},
			want: StatusInProgress,
		},
		{
			// DB-only requestedAt fallback: review stale relative to DB entry.
			name: "stale review, DB requestedAt fallback, no work_started → PENDING",
			data: &ReviewData{
				IsCopilotInReviewers: true,
				LatestCopilotReview:  newReview("APPROVED", &threeMinAgo),
				// ReviewRequestedAt not set → fallback to DB requestedAt
			},
			requestedAt: &oneMinAgo, // DB: 1min ago
			want:        StatusPending,
		},

		// ── nil submittedAt on review ─────────────────────────────────────────────
		{
			name:        "review with nil submittedAt, requestedAt set → treat as stale → NOT_REQUESTED",
			data:        &ReviewData{LatestCopilotReview: newReview("APPROVED", nil)},
			requestedAt: &now,
			want:        StatusNotRequested,
		},
		{
			name: "review with nil submittedAt, no requestedAt → COMPLETED (backward compat)",
			data: &ReviewData{LatestCopilotReview: newReview("APPROVED", nil)},
			want: StatusCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.DeriveStatus(tt.data, tt.requestedAt, nil)
			if got != tt.want {
				t.Errorf("DeriveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDeriveStatusIDBasedStaleness verifies the ID-based staleness detection path:
// when prevReviewID is non-nil, DeriveStatus compares review IDs instead of timestamps.
func TestDeriveStatusIDBasedStaleness(t *testing.T) {
	c := &Client{}

	now := time.Now()
	recentRequest := now.Add(-time.Second)

	// Helper to build a review with a specific ID and state.
	newReviewWithID := func(id int64, state string) *PullRequestReview {
		return &PullRequestReview{ID: id, State: state}
	}

	oldID := "42"

	tests := []struct {
		name         string
		data         *ReviewData
		requestedAt  *time.Time
		prevReviewID *string
		want         ReviewStatus
	}{
		{
			// Same ID as prevReviewID → old review is stale → NOT_REQUESTED.
			name:         "same ID as prevReviewID, no reviewers → NOT_REQUESTED (stale)",
			data:         &ReviewData{LatestCopilotReview: newReviewWithID(42, "APPROVED")},
			requestedAt:  &recentRequest,
			prevReviewID: &oldID,
			want:         StatusNotRequested,
		},
		{
			// Same ID but Copilot is in reviewers (pending new review) → PENDING.
			name:         "same ID as prevReviewID, copilot in reviewers, within threshold → PENDING",
			data:         &ReviewData{IsCopilotInReviewers: true, LatestCopilotReview: newReviewWithID(42, "APPROVED")},
			requestedAt:  &recentRequest,
			prevReviewID: &oldID,
			want:         StatusPending,
		},
		{
			// Different ID → new review → COMPLETED.
			name:         "different ID from prevReviewID → COMPLETED",
			data:         &ReviewData{LatestCopilotReview: newReviewWithID(99, "APPROVED")},
			requestedAt:  &recentRequest,
			prevReviewID: &oldID,
			want:         StatusCompleted,
		},
		{
			// Different ID, CHANGES_REQUESTED → BLOCKED.
			name:         "different ID, CHANGES_REQUESTED → BLOCKED",
			data:         &ReviewData{LatestCopilotReview: newReviewWithID(99, "CHANGES_REQUESTED")},
			requestedAt:  &recentRequest,
			prevReviewID: &oldID,
			want:         StatusBlocked,
		},
		{
			// nil prevReviewID with requestedAt → timestamp-based fallback (backward compat).
			// newReviewWithID does not set SubmittedAt, so sat.IsZero() == true → stale → NOT_REQUESTED.
			name:         "nil prevReviewID, no SubmittedAt → timestamp fallback → NOT_REQUESTED (stale)",
			data:         &ReviewData{LatestCopilotReview: newReviewWithID(99, "APPROVED")},
			requestedAt:  &now,
			prevReviewID: nil,
			want:         StatusNotRequested,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.DeriveStatus(tt.data, tt.requestedAt, tt.prevReviewID)
			if got != tt.want {
				t.Errorf("DeriveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// newTestGHClient creates a Client backed by a test HTTP server.
// The caller must call teardown() when done.
func newTestGHClient(mux *http.ServeMux) (*Client, func()) {
	srv := httptest.NewServer(mux)
	srvURL := srv.URL + "/"
	gh, err := github.NewClient(github.WithURLs(&srvURL, &srvURL))
	if err != nil {
		panic(fmt.Sprintf("newTestGHClient: %v", err))
	}
	return &Client{gh: gh}, srv.Close
}

func TestGetCIStatus(t *testing.T) {
	const (
		owner = "owner"
		repo  = "repo"
		pr    = 1
		sha   = "abc123def456"
	)

	prJSON := fmt.Sprintf(`{"number":%d,"head":{"sha":%q}}`, pr, sha)

	makeChecksJSON := func(runs ...string) string {
		return fmt.Sprintf(`{"total_count":%d,"check_runs":[%s]}`, len(runs), join(runs, ","))
	}
	// makeRun builds a check-run JSON fragment. appID is included in the dedup key
	// (appID:name), so two runs with the same name but different appIDs are treated
	// as distinct checks. Pass appID=0 to use the default sentinel app (ID 100).
	makeRun := func(id, appID int64, name, status, conclusion string) string {
		if appID == 0 {
			appID = 100
		}
		return fmt.Sprintf(`{"id":%d,"name":%q,"status":%q,"conclusion":%q,"app":{"id":%d}}`,
			id, name, status, conclusion, appID)
	}

	tests := []struct {
		name       string
		checksJSON string
		want       CIStatus
	}{
		{
			name:       "zero check runs → true (CI not configured)",
			checksJSON: makeChecksJSON(),
			want:       CIStatus{OK: true},
		},
		{
			name:       "all success → true",
			checksJSON: makeChecksJSON(makeRun(1, 0, "ci/build", "completed", "success")),
			want:       CIStatus{OK: true},
		},
		{
			name:       "skipped → true",
			checksJSON: makeChecksJSON(makeRun(1, 0, "ci/lint", "completed", "skipped")),
			want:       CIStatus{OK: true},
		},
		{
			name:       "neutral → true",
			checksJSON: makeChecksJSON(makeRun(1, 0, "ci/optional", "completed", "neutral")),
			want:       CIStatus{OK: true},
		},
		{
			name:       "in_progress (not completed) → false with pending_checks",
			checksJSON: makeChecksJSON(makeRun(1, 0, "ci/test", "in_progress", "")),
			want:       CIStatus{OK: false, PendingChecks: []string{"ci/test"}},
		},
		{
			name:       "failure → false with failed_checks",
			checksJSON: makeChecksJSON(makeRun(1, 0, "ci/build", "completed", "failure")),
			want:       CIStatus{OK: false, FailedChecks: []string{"ci/build"}},
		},
		{
			name: "mixed success and failure → false with failed_checks",
			checksJSON: makeChecksJSON(
				makeRun(1, 0, "ci/build", "completed", "success"),
				makeRun(2, 0, "ci/test", "completed", "failure"),
			),
			want: CIStatus{OK: false, FailedChecks: []string{"ci/test"}},
		},
		{
			name: "same app, same name: latest (higher ID) wins; stale in_progress ignored",
			checksJSON: makeChecksJSON(
				makeRun(1, 0, "ci/build", "in_progress", ""),      // older: stale
				makeRun(2, 0, "ci/build", "completed", "success"), // latest: success
			),
			want: CIStatus{OK: true},
		},
		{
			name: "same app, same name: latest (higher ID) is failure",
			checksJSON: makeChecksJSON(
				makeRun(1, 0, "ci/build", "completed", "success"), // older
				makeRun(2, 0, "ci/build", "completed", "failure"), // latest
			),
			want: CIStatus{OK: false, FailedChecks: []string{"ci/build"}},
		},
		{
			// Same check name but different apps: both must be retained as distinct checks.
			// A passing run from app 200 must not hide a failing run from app 300.
			name: "same name, different apps: both retained; failing check reported",
			checksJSON: makeChecksJSON(
				makeRun(1, 200, "ci/build", "completed", "success"), // app 200, passing
				makeRun(2, 300, "ci/build", "completed", "failure"), // app 300, failing
			),
			want: CIStatus{OK: false, FailedChecks: []string{"ci/build"}},
		},
	}

	ciStatusEqual := func(a, b CIStatus) bool {
		if a.OK != b.OK {
			return false
		}
		sliceEq := func(x, y []string) bool {
			if len(x) != len(y) {
				return false
			}
			for i := range x {
				if x[i] != y[i] {
					return false
				}
			}
			return true
		}
		return sliceEq(a.PendingChecks, b.PendingChecks) && sliceEq(a.FailedChecks, b.FailedChecks)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, pr), func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, prJSON)
			})
			mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", owner, repo, sha), func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tt.checksJSON)
			})

			c, teardown := newTestGHClient(mux)
			defer teardown()

			got, err := c.GetCIStatus(context.Background(), owner, repo, pr)
			if err != nil {
				t.Fatalf("GetCIStatus() error = %v", err)
			}
			if !ciStatusEqual(got, tt.want) {
				t.Errorf("GetCIStatus() = %+v, want %+v", got, tt.want)
			}
		})
	}

	t.Run("returns false when a later check-runs page contains failure", func(t *testing.T) {
		mux := http.NewServeMux()
		pagesSeen := []string{}

		mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, pr), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, prJSON)
		})
		mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", owner, repo, sha), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			pagesSeen = append(pagesSeen, page)
			if page == "1" {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/%s/%s/commits/%s/check-runs?page=2>; rel="next"`, r.Host, owner, repo, sha))
				_, _ = fmt.Fprint(w, `{"total_count":2,"check_runs":[{"id":1,"status":"completed","conclusion":"success"}]}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"total_count":2,"check_runs":[{"id":2,"status":"completed","conclusion":"failure"}]}`)
		})

		c, teardown := newTestGHClient(mux)
		defer teardown()

		got, err := c.GetCIStatus(context.Background(), owner, repo, pr)
		if err != nil {
			t.Fatalf("GetCIStatus() error = %v", err)
		}
		if got.OK {
			t.Fatalf("GetCIStatus() = %+v, want OK=false", got)
		}
		if join(pagesSeen, ",") != "1,2" {
			t.Fatalf("GetCIStatus() did not request all pages, got pages %q, want %q", join(pagesSeen, ","), "1,2")
		}
	})

	t.Run("returns true when all check-runs across later pages succeed", func(t *testing.T) {
		mux := http.NewServeMux()
		pagesSeen := []string{}

		mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, pr), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, prJSON)
		})
		mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", owner, repo, sha), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			pagesSeen = append(pagesSeen, page)
			if page == "1" {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/%s/%s/commits/%s/check-runs?page=2>; rel="next"`, r.Host, owner, repo, sha))
				_, _ = fmt.Fprint(w, `{"total_count":2,"check_runs":[{"id":1,"status":"completed","conclusion":"success"}]}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"total_count":2,"check_runs":[{"id":2,"status":"completed","conclusion":"success"}]}`)
		})

		c, teardown := newTestGHClient(mux)
		defer teardown()

		got, err := c.GetCIStatus(context.Background(), owner, repo, pr)
		if err != nil {
			t.Fatalf("GetCIStatus() error = %v", err)
		}
		if !got.OK {
			t.Fatalf("GetCIStatus() = %+v, want OK=true", got)
		}
		if join(pagesSeen, ",") != "1,2" {
			t.Fatalf("GetCIStatus() did not request all pages, got pages %q, want %q", join(pagesSeen, ","), "1,2")
		}
	})
}

func TestGetTokenDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		scopesHdr  string
		wantScopes []string
	}{
		{
			name:       "X-OAuth-Scopes header present",
			scopesHdr:  "repo, user",
			wantScopes: []string{"repo", "user"},
		},
		{
			name:       "X-OAuth-Scopes header absent (fine-grained PAT / GitHub App token)",
			scopesHdr:  "",
			wantScopes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
				if tt.scopesHdr != "" {
					w.Header().Set("X-OAuth-Scopes", tt.scopesHdr)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"login":"octocat"}`)
			})
			c, teardown := newTestGHClient(mux)
			defer teardown()

			diag, err := getTokenDiagnostics(context.Background(), c.gh)
			if err != nil {
				t.Fatalf("getTokenDiagnostics() error = %v", err)
			}
			if diag.Login != "octocat" {
				t.Errorf("Login = %q, want %q", diag.Login, "octocat")
			}
			if join(diag.Scopes, ",") != join(tt.wantScopes, ",") {
				t.Errorf("Scopes = %v, want %v", diag.Scopes, tt.wantScopes)
			}
		})
	}

	t.Run("empty login returns error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"login":""}`)
		})
		c, teardown := newTestGHClient(mux)
		defer teardown()

		if _, err := getTokenDiagnostics(context.Background(), c.gh); err == nil {
			t.Fatal("getTokenDiagnostics() error = nil, want error for empty login")
		}
	})

	t.Run("empty token returns error", func(t *testing.T) {
		if _, err := GetTokenDiagnostics(context.Background(), ""); err == nil {
			t.Fatal("GetTokenDiagnostics() error = nil, want error for empty token")
		}
	})
}

// join concatenates strings with a separator (avoids importing strings in test file).
func join(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += sep + s
	}
	return result
}

// TestGetReviewDataTimeline verifies that GetReviewData populates ReviewRequestedAt
// and WorkStartedAt from the REST timeline (Issues.ListIssueTimeline).
func TestGetReviewDataTimeline(t *testing.T) {
	const (
		owner = "owner"
		repo  = "repo"
		pr    = 5
	)

	reviewRequestedAt := "2024-01-01T00:59:02Z"
	workStartedAt := "2024-01-01T00:59:29Z"

	// Minimal timeline JSON with review_requested and copilot_work_started events.
	timelineJSON := fmt.Sprintf(`[
		{"event":"review_requested","created_at":%q,"requested_reviewer":{"login":"Copilot","type":"Bot"}},
		{"event":"copilot_work_started","created_at":%q}
	]`, reviewRequestedAt, workStartedAt)

	writeJSON := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "100")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		_, _ = fmt.Fprint(w, body)
	}

	mux := http.NewServeMux()

	// requested reviewers: Copilot is in the list.
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", owner, repo, pr),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"users":[{"login":"Copilot","type":"Bot"}],"teams":[]}`)
		})

	// reviews: one completed review from a previous cycle.
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, pr),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `[{"id":1,"user":{"login":"copilot-pull-request-reviewer[bot]"},"state":"COMMENTED","submitted_at":"2024-01-01T00:58:00Z"}]`)
		})

	// timeline events.
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/timeline", owner, repo, pr),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, timelineJSON)
		})

	c, teardown := newTestGHClient(mux)
	defer teardown()

	data, err := c.GetReviewData(context.Background(), owner, repo, pr)
	if err != nil {
		t.Fatalf("GetReviewData() error = %v", err)
	}

	if !data.IsCopilotInReviewers {
		t.Error("IsCopilotInReviewers = false, want true (login \"Copilot\" must be recognized)")
	}

	if data.ReviewRequestedAt == nil {
		t.Fatal("ReviewRequestedAt = nil, want non-nil")
	}
	wantReviewRequestedAt, _ := time.Parse(time.RFC3339, reviewRequestedAt)
	if !data.ReviewRequestedAt.Equal(wantReviewRequestedAt) {
		t.Errorf("ReviewRequestedAt = %v, want %v", data.ReviewRequestedAt, wantReviewRequestedAt)
	}

	if data.WorkStartedAt == nil {
		t.Fatal("WorkStartedAt = nil, want non-nil")
	}
	wantWorkStartedAt, _ := time.Parse(time.RFC3339, workStartedAt)
	if !data.WorkStartedAt.Equal(wantWorkStartedAt) {
		t.Errorf("WorkStartedAt = %v, want %v", data.WorkStartedAt, wantWorkStartedAt)
	}
}

// TestGetReviewDataTimelinePicksLatestEvents verifies that when multiple
// review_requested / copilot_work_started events exist (rereview), only the
// most recent timestamps are kept.
func TestGetReviewDataTimelinePicksLatestEvents(t *testing.T) {
	const (
		owner = "owner"
		repo  = "repo"
		pr    = 6
	)

	// Two cycles: cycle-1 at 01:00, cycle-2 at 02:00.
	timelineJSON := `[
		{"event":"review_requested","created_at":"2024-01-01T01:00:00Z","requested_reviewer":{"login":"Copilot","type":"Bot"}},
		{"event":"copilot_work_started","created_at":"2024-01-01T01:01:00Z"},
		{"event":"review_requested","created_at":"2024-01-01T02:00:00Z","requested_reviewer":{"login":"Copilot","type":"Bot"}},
		{"event":"copilot_work_started","created_at":"2024-01-01T02:01:00Z"}
	]`

	writeJSON := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "100")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		_, _ = fmt.Fprint(w, body)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", owner, repo, pr),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"users":[{"login":"Copilot","type":"Bot"}],"teams":[]}`)
		})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, pr),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `[]`)
		})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/timeline", owner, repo, pr),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, timelineJSON)
		})

	c, teardown := newTestGHClient(mux)
	defer teardown()

	data, err := c.GetReviewData(context.Background(), owner, repo, pr)
	if err != nil {
		t.Fatalf("GetReviewData() error = %v", err)
	}

	wantReviewRequested, _ := time.Parse(time.RFC3339, "2024-01-01T02:00:00Z")
	wantWorkStarted, _ := time.Parse(time.RFC3339, "2024-01-01T02:01:00Z")

	if data.ReviewRequestedAt == nil || !data.ReviewRequestedAt.Equal(wantReviewRequested) {
		t.Errorf("ReviewRequestedAt = %v, want %v", data.ReviewRequestedAt, wantReviewRequested)
	}
	if data.WorkStartedAt == nil || !data.WorkStartedAt.Equal(wantWorkStarted) {
		t.Errorf("WorkStartedAt = %v, want %v", data.WorkStartedAt, wantWorkStarted)
	}
}

// TestIsCopilotLoginCoversAllKnownIdentities verifies that all known Copilot
// reviewer identities are recognized, including the REST requested_reviewers form.
func TestNewWithHTTPClientAndURLFull(t *testing.T) {
	var restHit, gqlHit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			// GraphQL request
			gqlHit.Store(true)
			_, _ = fmt.Fprint(w, `{"data":{}}`)
		} else {
			// REST request
			restHit.Store(true)
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
	if err != nil {
		t.Fatalf("NewWithHTTPClientAndURLFull() error = %v", err)
	}
	if c == nil {
		t.Fatal("NewWithHTTPClientAndURLFull() = nil")
	}
	if c.gh == nil {
		t.Error("REST client (gh) is nil")
	}
	if c.v4 == nil {
		t.Error("GraphQL client (v4) is nil")
	}
}

func TestNewWithHTTPClientAndURLFullInvalidURL(t *testing.T) {
	_, err := NewWithHTTPClientAndURLFull(http.DefaultClient, "://invalid-url", 30*time.Second)
	if err == nil {
		t.Fatal("NewWithHTTPClientAndURLFull() expected error for invalid URL, got nil")
	}
}

func TestIsCopilotLoginCoversAllKnownIdentities(t *testing.T) {
	cases := []struct {
		login string
		want  bool
	}{
		{"Copilot", true}, // REST requested_reviewers (observation #1)
		{"copilot-pull-request-reviewer[bot]", true}, // REST reviews
		{"github-copilot[bot]", true},
		{"github-copilot", true},
		{"other-user", false},
		{"copilot", false}, // case-sensitive: lowercase must not match
	}
	for _, tc := range cases {
		got := IsCopilotLogin(tc.login)
		if got != tc.want {
			t.Errorf("IsCopilotLogin(%q) = %v, want %v", tc.login, got, tc.want)
		}
	}
}

func TestGetReviewThreadsWithOptionsProjectionAndPagination(t *testing.T) {
	tests := []struct {
		name             string
		options          ReviewThreadsOptions
		wantBody         string
		wantBodySelected bool
	}{
		{
			name:             "metadata-only",
			options:          ReviewThreadsOptions{IncludeBodies: false},
			wantBodySelected: false,
		},
		{
			name:             "full",
			options:          ReviewThreadsOptions{IncludeBodies: true},
			wantBody:         "secret review body",
			wantBodySelected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var queries []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Query     string                     `json:"query"`
					Variables map[string]json.RawMessage `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode GraphQL request: %v", err)
				}
				queries = append(queries, request.Query)

				page := 1
				if cursor := request.Variables["cursor"]; len(cursor) > 0 && string(cursor) != "null" {
					page = 2
				}
				login := "thread-owl[bot]"
				if page == 2 {
					login = "scottlz0310-user"
				}
				comment := map[string]any{
					"databaseId": 1000 + page,
					"url":        fmt.Sprintf("https://github.com/example/review/%d", page),
					"author": map[string]any{
						"login":      login,
						"__typename": "Bot",
					},
					"createdAt": "2026-09-10T00:00:00Z",
				}
				if tc.wantBodySelected {
					comment["body"] = tc.wantBody
				}
				node := map[string]any{
					"id":         fmt.Sprintf("PRRT_%d", page),
					"isResolved": page == 2,
					"isOutdated": false,
					"path":       "main.go",
					"line":       10 + page,
					"startLine":  10 + page,
					"comments": map[string]any{
						"nodes": []any{comment},
					},
				}
				response := map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequest": map[string]any{
								"reviewThreads": map[string]any{
									"nodes": []any{node},
									"pageInfo": map[string]any{
										"hasNextPage": page == 1,
										"endCursor":   fmt.Sprintf("cursor-%d", page),
									},
								},
							},
						},
					},
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			t.Cleanup(srv.Close)

			client, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
			if err != nil {
				t.Fatalf("NewWithHTTPClientAndURLFull() error = %v", err)
			}
			result, err := client.GetReviewThreadsWithOptions(context.Background(), "example", "repo", 1, tc.options)
			if err != nil {
				t.Fatalf("GetReviewThreadsWithOptions() error = %v", err)
			}
			if result.PageCount != 2 {
				t.Fatalf("PageCount = %d, want 2", result.PageCount)
			}
			if len(result.Threads) != 2 {
				t.Fatalf("len(Threads) = %d, want 2", len(result.Threads))
			}
			comment := result.Threads[0].Comments[0]
			if comment.CommentID != "1001" {
				t.Errorf("CommentID = %q, want 1001", comment.CommentID)
			}
			if comment.Author != "thread-owl[bot]" {
				t.Errorf("Author = %q, want thread-owl[bot]", comment.Author)
			}
			if comment.AuthorType != "Bot" {
				t.Errorf("AuthorType = %q, want Bot", comment.AuthorType)
			}
			if comment.URL == "" {
				t.Error("URL is empty, want review comment URL")
			}
			if comment.Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", comment.Body, tc.wantBody)
			}
			for _, query := range queries {
				selected := strings.Contains(query, "body")
				if selected != tc.wantBodySelected {
					t.Errorf("query body selection = %v, want %v; query = %s", selected, tc.wantBodySelected, query)
				}
			}
		})
	}
}

func TestGetReviewThreadsDefaultIncludesBodies(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode GraphQL request: %v", err)
		}
		query = request.Query
		response := map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"reviewThreads": map[string]any{
							"nodes": []any{map[string]any{
								"id":         "PRRT_default",
								"isResolved": false,
								"isOutdated": false,
								"comments": map[string]any{
									"nodes": []any{map[string]any{
										"databaseId": 7,
										"url":        "https://github.com/example/review/7",
										"body":       "full review body",
										"createdAt":  "2026-09-10T00:00:00Z",
										"author": map[string]any{
											"login":      "thread-owl[bot]",
											"__typename": "Bot",
										},
									}},
								},
							}},
							"pageInfo": map[string]any{
								"hasNextPage": false,
								"endCursor":   "cursor-1",
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(srv.Close)

	client, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
	if err != nil {
		t.Fatalf("NewWithHTTPClientAndURLFull() error = %v", err)
	}
	threads, err := client.GetReviewThreads(context.Background(), "example", "repo", 1)
	if err != nil {
		t.Fatalf("GetReviewThreads() error = %v", err)
	}
	if len(threads) != 1 || len(threads[0].Comments) != 1 {
		t.Fatalf("thread/comment counts = %d/%d, want 1/1", len(threads), len(threads[0].Comments))
	}
	if threads[0].Comments[0].Body != "full review body" {
		t.Errorf("Body = %q, want full review body", threads[0].Comments[0].Body)
	}
	if !strings.Contains(query, "body") {
		t.Errorf("default GraphQL query omitted body: %s", query)
	}
}

func TestGetReviewThreadsMetadataPaginatesThreadComments(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string                     `json:"query"`
			Variables map[string]json.RawMessage `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode GraphQL request: %v", err)
		}
		queries = append(queries, request.Query)
		comment := func(id int) map[string]any {
			return map[string]any{
				"databaseId": id,
				"url":        fmt.Sprintf("https://github.com/example/review/%d", id),
				"createdAt":  "2026-09-10T00:00:00Z",
				"author": map[string]any{
					"login":      "thread-owl[bot]",
					"__typename": "Bot",
				},
			}
		}

		var response map[string]any
		if strings.Contains(request.Query, "reviewThreads") {
			response = map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviewThreads": map[string]any{
								"nodes": []any{map[string]any{
									"id":         "PRRT_comments",
									"isResolved": false,
									"isOutdated": false,
									"comments": map[string]any{
										"nodes": []any{comment(1)},
										"pageInfo": map[string]any{
											"hasNextPage": true,
											"endCursor":   "comment-cursor-1",
										},
									},
								}},
								"pageInfo": map[string]any{
									"hasNextPage": false,
									"endCursor":   "thread-cursor-1",
								},
							},
						},
					},
				},
			}
		} else {
			var cursor string
			if err := json.Unmarshal(request.Variables["cursor"], &cursor); err != nil || cursor != "comment-cursor-1" {
				http.Error(w, "unexpected comment cursor", http.StatusBadRequest)
				return
			}
			response = map[string]any{
				"data": map[string]any{
					"node": map[string]any{
						"comments": map[string]any{
							"nodes": []any{comment(2)},
							"pageInfo": map[string]any{
								"hasNextPage": false,
								"endCursor":   "comment-cursor-2",
							},
						},
					},
				},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(srv.Close)

	client, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
	if err != nil {
		t.Fatalf("NewWithHTTPClientAndURLFull() error = %v", err)
	}
	result, err := client.GetReviewThreadsWithOptions(context.Background(), "example", "repo", 1, ReviewThreadsOptions{})
	if err != nil {
		t.Fatalf("GetReviewThreadsWithOptions() error = %v", err)
	}
	if result.PageCount != 1 || len(result.Threads) != 1 || len(result.Threads[0].Comments) != 2 {
		t.Fatalf("result pages/threads/comments = %d/%d/%d, want 1/1/2", result.PageCount, len(result.Threads), len(result.Threads[0].Comments))
	}
	if result.Threads[0].Comments[1].CommentID != "2" {
		t.Errorf("second CommentID = %q, want 2", result.Threads[0].Comments[1].CommentID)
	}
	if len(queries) != 2 {
		t.Fatalf("GraphQL query count = %d, want 2", len(queries))
	}
	for _, query := range queries {
		if strings.Contains(query, "body") {
			t.Errorf("metadata-only query selected body: %s", query)
		}
	}
}

func TestGetReviewThreadsMetadataRejectsEmptyCursor(t *testing.T) {
	comment := map[string]any{
		"databaseId": 1,
		"url":        "https://github.com/example/review/1",
		"createdAt":  "2026-09-10T00:00:00Z",
		"author": map[string]any{
			"login":      "thread-owl[bot]",
			"__typename": "Bot",
		},
	}

	tests := []struct {
		name          string
		threadPage    map[string]any
		commentPage   map[string]any
		wantErrorPart string
	}{
		{
			name: "thread page",
			threadPage: map[string]any{
				"hasNextPage": true,
				"endCursor":   "",
			},
			wantErrorPart: "graphql metadata query returned hasNextPage=true with empty endCursor",
		},
		{
			name: "initial comment page",
			threadPage: map[string]any{
				"hasNextPage": false,
				"endCursor":   "thread-cursor-1",
			},
			commentPage: map[string]any{
				"hasNextPage": true,
				"endCursor":   "",
			},
			wantErrorPart: "graphql metadata comments query returned hasNextPage=true with empty endCursor",
		},
		{
			name: "nested comment page",
			threadPage: map[string]any{
				"hasNextPage": false,
				"endCursor":   "thread-cursor-1",
			},
			commentPage: map[string]any{
				"hasNextPage": true,
				"endCursor":   "comment-cursor-1",
			},
			wantErrorPart: "graphql metadata comments query returned hasNextPage=true with empty endCursor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Query string `json:"query"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode GraphQL request: %v", err)
				}

				response := map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequest": map[string]any{
								"reviewThreads": map[string]any{
									"nodes":    []any{},
									"pageInfo": tc.threadPage,
								},
							},
						},
					},
				}
				if tc.commentPage != nil {
					if strings.Contains(request.Query, "reviewThreads") {
						response["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)["nodes"] = []any{map[string]any{
							"id":         "PRRT_cursor",
							"isResolved": false,
							"isOutdated": false,
							"comments": map[string]any{
								"nodes":    []any{comment},
								"pageInfo": tc.commentPage,
							},
						}}
					} else {
						response = map[string]any{
							"data": map[string]any{
								"node": map[string]any{
									"comments": map[string]any{
										"nodes":    []any{},
										"pageInfo": map[string]any{"hasNextPage": true, "endCursor": ""},
									},
								},
							},
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			t.Cleanup(srv.Close)

			client, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
			if err != nil {
				t.Fatalf("NewWithHTTPClientAndURLFull() error = %v", err)
			}
			_, err = client.GetReviewThreadsWithOptions(context.Background(), "example", "repo", 1, ReviewThreadsOptions{IncludeBodies: false})
			if err == nil || !strings.Contains(err.Error(), tc.wantErrorPart) {
				t.Fatalf("GetReviewThreadsWithOptions() error = %v, want %q", err, tc.wantErrorPart)
			}
		})
	}
}
