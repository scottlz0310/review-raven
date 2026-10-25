package ghclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scottlz0310/review-raven/internal/reader"
)

const (
	headA = "1111111111111111111111111111111111111111"
	headB = "3333333333333333333333333333333333333333"
	baseA = "2222222222222222222222222222222222222222"
)

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// commentsServer answers the four comment queries from canned pages. A body is
// sent only when the request selects it (withBody=true), as GitHub does.
type commentsServer struct {
	t        *testing.T
	requests []gqlRequest

	threadPages  []map[string]any // reviewThreads pages; each {nodes, pageInfo}
	threadExtra  []map[string]any // continuation pages of the thread comments
	reviewPages  []map[string]any
	issuePages   []map[string]any
	headForQuery func(kind string, call int) string
	calls        map[string]int
}

func comment(id int64, login, body string, edited any) map[string]any {
	return map[string]any{
		"databaseId":   id,
		"url":          fmt.Sprintf("https://github.com/o/r/pull/1#c%d", id),
		"createdAt":    "2026-10-25T00:00:00Z",
		"lastEditedAt": edited,
		"body":         body,
		"author":       map[string]any{"login": login, "__typename": "Bot"},
	}
}

func page(next string, nodes ...map[string]any) map[string]any {
	n := make([]any, 0, len(nodes))
	for _, x := range nodes {
		n = append(n, x)
	}
	return map[string]any{"nodes": n, "pageInfo": map[string]any{"hasNextPage": next != "", "endCursor": next}}
}

func thread(id string, first map[string]any) map[string]any {
	return map[string]any{"id": id, "isResolved": false, "isOutdated": false, "path": "main.go", "line": 7, "comments": first}
}

func (s *commentsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.t.Errorf("decode request: %v", err)
	}
	s.requests = append(s.requests, req)
	withBody, _ := req.Variables["withBody"].(bool)
	if !strings.Contains(req.Query, "@include(if: $withBody)") || !strings.Contains(req.Query, "$withBody:Boolean!") && !strings.Contains(req.Query, "$withBody: Boolean!") {
		s.t.Errorf("the query must select the body through @include(if: $withBody): %s", req.Query)
	}
	if s.calls == nil {
		s.calls = map[string]int{}
	}

	var kind string
	switch {
	case strings.Contains(req.Query, "reviewThreads("):
		kind = "threads"
	case strings.Contains(req.Query, "node(id"):
		kind = "continuation"
	case strings.Contains(req.Query, "reviews("):
		kind = "reviews"
	default:
		kind = "issues"
	}
	call := s.calls[kind]
	s.calls[kind]++
	head := headA
	if s.headForQuery != nil {
		head = s.headForQuery(kind, call)
	}
	refs := map[string]any{"headRefOid": head, "baseRefOid": baseA}

	var data any
	switch kind {
	case "threads":
		pr := map[string]any{"reviewThreads": s.threadPages[call]}
		maps.Copy(pr, refs)
		data = map[string]any{"repository": map[string]any{"pullRequest": pr}}
	case "continuation":
		data = map[string]any{"node": map[string]any{"comments": s.threadExtra[call]}}
	case "reviews":
		pr := map[string]any{"reviews": s.reviewPages[call]}
		maps.Copy(pr, refs)
		data = map[string]any{"repository": map[string]any{"pullRequest": pr}}
	default:
		pr := map[string]any{"comments": s.issuePages[call]}
		maps.Copy(pr, refs)
		data = map[string]any{"repository": map[string]any{"pullRequest": pr}}
	}

	if !withBody {
		stripBodies(data)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func stripBodies(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "body")
		for _, child := range x {
			stripBodies(child)
		}
	case []any:
		for _, child := range x {
			stripBodies(child)
		}
	}
}

func newCommentsClient(t *testing.T, s *commentsServer) *Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	client, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fullServer(t *testing.T) *commentsServer {
	edited := "2026-10-25T01:00:00Z"
	return &commentsServer{
		t: t,
		// 2 ページの thread。1 つ目の thread は最初のページに 2 件、続きに 1 件のコメントを持つ。
		threadPages: []map[string]any{
			page("T1", thread("PRRT_1", page("C1", comment(101, "thread-owl", "first", nil), comment(102, "review-raven", "second", edited)))),
			page("", thread("PRRT_2", page("", comment(103, "thread-owl", "other", nil)))),
		},
		threadExtra: []map[string]any{page("", comment(104, "thread-owl", "third (on the continuation page)", nil))},
		reviewPages: []map[string]any{
			page("R1", withState(comment(201, "thread-owl", "review one", nil), "COMMENTED")),
			page("", withState(comment(202, "thread-owl", "review two", nil), "APPROVED")),
		},
		issuePages: []map[string]any{
			page("I1", comment(301, "codecov", "report", nil)),
			page("", comment(302, "scottlz0310-user", "note", nil)),
		},
	}
}

func withState(c map[string]any, state string) map[string]any {
	c["state"] = state
	return c
}

func TestCommentSnapshotWalksEveryPageOfEveryKind(t *testing.T) {
	s := fullServer(t)
	snap, err := newCommentsClient(t, s).CommentSnapshot(context.Background(), "o", "r", 1, true)
	if err != nil {
		t.Fatal(err)
	}

	if snap.HeadSHA != headA || snap.BaseSHA != baseA {
		t.Fatalf("refs = %s/%s", snap.HeadSHA, snap.BaseSHA)
	}
	if len(snap.Threads) != 2 {
		t.Fatalf("threads = %d, want 2", len(snap.Threads))
	}
	if got := len(snap.Threads[0].Comments); got != 3 {
		t.Fatalf("thread 1 has %d comments, want 3 (two on the first page, one on the continuation)", got)
	}
	if got := ids(snap.Threads[0].Comments); got != "101,102,104" {
		t.Fatalf("thread 1 comment ids = %s", got)
	}
	if got := ids(snap.Reviews); got != "201,202" {
		t.Fatalf("review ids = %s", got)
	}
	if got := ids(snap.IssueComments); got != "301,302" {
		t.Fatalf("issue comment ids = %s", got)
	}

	c := snap.Threads[0].Comments[1]
	if c.Author != "review-raven" || c.AuthorType != "Bot" || c.EditedAt != "2026-10-25T01:00:00Z" || c.Kind != reader.KindThreadComment {
		t.Fatalf("comment = %+v", c)
	}
	if snap.Threads[0].Comments[0].EditedAt != "" {
		t.Fatalf("a comment never edited must have no edit time")
	}
	if snap.Reviews[1].State != "APPROVED" || snap.Reviews[1].Kind != reader.KindReviewBody {
		t.Fatalf("review = %+v", snap.Reviews[1])
	}
	if snap.Threads[0].Comments[2].Body == nil || *snap.Threads[0].Comments[2].Body != "third (on the continuation page)" {
		t.Fatalf("the continuation page must carry its body")
	}
	for _, req := range s.requests {
		if req.Variables["withBody"] != true {
			t.Fatalf("withBody = %v, want true", req.Variables["withBody"])
		}
	}
}

func ids(comments []reader.Comment) string {
	out := make([]string, 0, len(comments))
	for _, c := range comments {
		out = append(out, c.ID)
	}
	return strings.Join(out, ",")
}

func TestCommentSnapshotWithoutBodiesNeverSelectsOne(t *testing.T) {
	s := fullServer(t)
	snap, err := newCommentsClient(t, s).CommentSnapshot(context.Background(), "o", "r", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.requests) == 0 {
		t.Fatal("no request was made")
	}
	for _, req := range s.requests {
		if v, ok := req.Variables["withBody"].(bool); !ok || v {
			t.Fatalf("withBody = %v, want false on every request of a metadata read", req.Variables["withBody"])
		}
	}
	all := append([]reader.Comment{}, snap.Reviews...)
	all = append(all, snap.IssueComments...)
	for _, th := range snap.Threads {
		all = append(all, th.Comments...)
	}
	if len(all) != 8 {
		t.Fatalf("comments = %d, want 8", len(all))
	}
	for _, c := range all {
		if c.Body != nil {
			t.Fatalf("%s %s carries a body in a metadata read", c.Kind, c.ID)
		}
		if c.Author == "" {
			t.Fatalf("%s %s has no author", c.Kind, c.ID)
		}
	}
}

func TestCommentSnapshotReportsAMovedPullRequest(t *testing.T) {
	s := fullServer(t)
	// reviews の問い合わせの時点で head が動いた
	s.headForQuery = func(kind string, _ int) string {
		if kind == "reviews" {
			return headB
		}
		return headA
	}
	_, err := newCommentsClient(t, s).CommentSnapshot(context.Background(), "o", "r", 1, false)
	if !errors.Is(err, reader.ErrSnapshotMoved) {
		t.Fatalf("error = %v, want ErrSnapshotMoved", err)
	}
}

func TestCommentSnapshotRejectsAPageWithoutACursor(t *testing.T) {
	s := &commentsServer{t: t, threadPages: []map[string]any{{
		"nodes":    []any{},
		"pageInfo": map[string]any{"hasNextPage": true, "endCursor": ""},
	}}}
	_, err := newCommentsClient(t, s).CommentSnapshot(context.Background(), "o", "r", 1, false)
	if err == nil || !strings.Contains(err.Error(), "empty endCursor") {
		t.Fatalf("error = %v, want an empty endCursor error", err)
	}
}

func TestCommentSnapshotRejectsAnInvalidPullRequestNumber(t *testing.T) {
	c, err := NewWithHTTPClientAndURLFull(http.DefaultClient, "http://127.0.0.1:0", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []int{0, -1} {
		if _, err := c.CommentSnapshot(context.Background(), "o", "r", pr, false); err == nil {
			t.Fatalf("pr %d must be rejected", pr)
		}
	}
}

func TestProjectAllowlistFile(t *testing.T) {
	const path = ".review-raven/trusted-comment-authors.json"
	file := func(content string) string {
		return fmt.Sprintf(`{"type":"file","encoding":"base64","size":%d,"name":"x","path":%q,"content":%q}`,
			len(content), path, base64.StdEncoding.EncodeToString([]byte(content)))
	}
	tests := []struct {
		name        string
		status      int
		body        string
		wantFound   bool
		wantContent string
		wantInvalid bool
		wantErr     bool
	}{
		{name: "present", status: 200, body: file(`{"version":1}`), wantFound: true, wantContent: `{"version":1}`},
		{name: "absent", status: 404, body: `{"message":"Not Found"}`},
		{name: "a directory is not a file", status: 200, body: `[{"type":"file","name":"a","path":"x/a"}]`, wantFound: true, wantInvalid: true},
		{name: "too large", status: 200, body: fmt.Sprintf(`{"type":"file","encoding":"base64","size":%d,"name":"x","path":%q,"content":""}`, maxProjectFileBytes+1, path), wantFound: true, wantInvalid: true},
		{name: "a failed request is not a missing file", status: 500, body: `{"message":"boom"}`, wantErr: true},
		{name: "a refused request is not a missing file", status: 403, body: `{"message":"forbidden"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotRef string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotRef = r.URL.Query().Get("ref")
				if !strings.HasSuffix(r.URL.Path, "/contents/"+path) {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			client, err := NewWithHTTPClientAndURLFull(srv.Client(), srv.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}

			data, found, err := client.ProjectAllowlistFile(context.Background(), "o", "r", path, baseA)
			if gotRef != baseA {
				t.Fatalf("ref = %q, want the base commit %s", gotRef, baseA)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v (err %v)", found, tt.wantFound, err)
			}
			switch {
			case tt.wantInvalid:
				if !errors.Is(err, reader.ErrProjectAllowlistInvalid) {
					t.Fatalf("error = %v, want ErrProjectAllowlistInvalid", err)
				}
			case tt.wantErr:
				if err == nil || errors.Is(err, reader.ErrProjectAllowlistInvalid) {
					t.Fatalf("error = %v, want a plain request error", err)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if string(data) != tt.wantContent {
					t.Fatalf("content = %q, want %q", data, tt.wantContent)
				}
			}
		})
	}
}
