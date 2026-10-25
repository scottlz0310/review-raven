// Package readertest loads the shared reader scenarios
// (docs/design/contracts/reader) and replays them against an implementation of
// reader.Source. It exists for tests: both the reader itself and the MCP tool
// that serves it run the same scenarios.
package readertest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/scottlz0310/review-raven/internal/reader"
	"github.com/scottlz0310/review-raven/internal/trustedauthors"
)

// Comment is a comment in a scenario's snapshot.
type Comment struct {
	ID         string  `json:"id"`
	Author     string  `json:"author"`
	AuthorType string  `json:"authorType"`
	URL        string  `json:"url"`
	CreatedAt  string  `json:"createdAt"`
	EditedAt   *string `json:"editedAt"`
	State      string  `json:"state"`
	Body       string  `json:"body"`
}

// Thread is a review thread in a scenario's snapshot.
type Thread struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Line       *int32    `json:"line"`
	IsResolved bool      `json:"isResolved"`
	IsOutdated bool      `json:"isOutdated"`
	Comments   []Comment `json:"comments"`
}

// Fetch is the answer to one call of Source.Fetch: a snapshot (with bodies),
// or Moved, which stands for a pull request that changed while paging.
type Fetch struct {
	Moved         bool      `json:"moved"`
	HeadSHA       string    `json:"headSha"`
	BaseSHA       string    `json:"baseSha"`
	Threads       []Thread  `json:"threads"`
	Reviews       []Comment `json:"reviews"`
	IssueComments []Comment `json:"issueComments"`
}

// Expect is what a scenario requires of an implementation.
type Expect struct {
	Outcome         string   `json:"outcome"`
	Code            string   `json:"code"`
	FetchCalls      int      `json:"fetchCalls"`
	BodyFetchCalls  int      `json:"bodyFetchCalls"`
	CommentIDs      []string `json:"commentIds"`
	ErrorCommentIDs []string `json:"errorCommentIds"`
	Allowlist       *struct {
		ServerLogins  int  `json:"serverLogins"`
		ProjectFile   bool `json:"projectFile"`
		ProjectLogins int  `json:"projectLogins"`
	} `json:"allowlist"`
}

// Scenario is one shared scenario.
type Scenario struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	ServerAllowlist  []string `json:"serverAllowlist"`
	ProjectAllowlist struct {
		State  string   `json:"state"`
		Logins []string `json:"logins"`
	} `json:"projectAllowlist"`
	Fetches []Fetch `json:"fetches"`
	Expect  Expect  `json:"expect"`
}

func contractDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "design", "contracts", "reader")
}

// Load reads every shared scenario.
func Load(t testing.TB) []Scenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir(), "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		SchemaVersion int        `json:"schemaVersion"`
		Scenarios     []Scenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.SchemaVersion != 1 || len(file.Scenarios) == 0 {
		t.Fatalf("unexpected scenarios file: schemaVersion=%d scenarios=%d", file.SchemaVersion, len(file.Scenarios))
	}
	return file.Scenarios
}

func (c Comment) toComment(kind reader.Kind, withBody bool) reader.Comment {
	out := reader.Comment{Kind: kind, ID: c.ID, Author: c.Author, AuthorType: c.AuthorType, URL: c.URL, CreatedAt: c.CreatedAt, State: c.State}
	if c.EditedAt != nil {
		out.EditedAt = *c.EditedAt
	}
	if withBody {
		body := c.Body
		out.Body = &body
	}
	return out
}

func (f Fetch) snapshot(withBodies bool) reader.Snapshot {
	s := reader.Snapshot{HeadSHA: f.HeadSHA, BaseSHA: f.BaseSHA}
	for _, t := range f.Threads {
		th := reader.Thread{ID: t.ID, Path: t.Path, Line: t.Line, IsResolved: t.IsResolved, IsOutdated: t.IsOutdated}
		for _, c := range t.Comments {
			th.Comments = append(th.Comments, c.toComment(reader.KindThreadComment, withBodies))
		}
		s.Threads = append(s.Threads, th)
	}
	for _, c := range f.Reviews {
		s.Reviews = append(s.Reviews, c.toComment(reader.KindReviewBody, withBodies))
	}
	for _, c := range f.IssueComments {
		s.IssueComments = append(s.IssueComments, c.toComment(reader.KindIssueComment, withBodies))
	}
	return s
}

// Source replays a scenario's fetches in order. A metadata call gets the
// fixture without any body, as GitHub answers a query that does not select
// bodies. It implements reader.Source.
type Source struct {
	t         testing.TB
	scenario  Scenario
	Calls     int
	BodyCalls int
}

// NewSource returns a Source for sc.
func NewSource(t testing.TB, sc Scenario) *Source { return &Source{t: t, scenario: sc} }

// Fetch implements reader.Source.
func (s *Source) Fetch(_ context.Context, withBodies bool) (reader.Snapshot, error) {
	if s.Calls >= len(s.scenario.Fetches) {
		s.t.Fatalf("unexpected fetch #%d (withBodies=%v): the scenario scripts only %d", s.Calls+1, withBodies, len(s.scenario.Fetches))
	}
	entry := s.scenario.Fetches[s.Calls]
	s.Calls++
	if withBodies {
		s.BodyCalls++
	}
	if entry.Moved {
		return reader.Snapshot{}, reader.ErrSnapshotMoved
	}
	return entry.snapshot(withBodies), nil
}

// ProjectAllowlist implements reader.Source.
func (s *Source) ProjectAllowlist(_ context.Context, _ string) (reader.ProjectAllowlist, error) {
	switch s.scenario.ProjectAllowlist.State {
	case "present":
		return reader.ProjectAllowlist{State: reader.ProjectAllowlistPresent, Logins: s.scenario.ProjectAllowlist.Logins}, nil
	case "invalid":
		_, err := trustedauthors.ParseProjectFile([]byte(`{"version":1}`))
		return reader.ProjectAllowlist{}, errors.Join(reader.ErrProjectAllowlistInvalid, err)
	default:
		return reader.ProjectAllowlist{State: reader.ProjectAllowlistAbsent}, nil
	}
}

// Schema resolves a schema of the reader contract by file name.
func Schema(t testing.TB, file string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir(), file))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", file, err)
	}
	return r
}

// Validate checks v, as it appears on the wire, against a schema.
func Validate(t testing.TB, schema *jsonschema.Resolved, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("does not satisfy the contract schema: %v\n%s", err, raw)
	}
}

// Key names a comment as "kind:id".
func Key(kind reader.Kind, id string) string { return string(kind) + ":" + id }

// Keys lists the comments of a snapshot as sorted "kind:id".
func Keys(s reader.Snapshot) []string {
	var keys []string
	for _, th := range s.Threads {
		for _, c := range th.Comments {
			keys = append(keys, Key(c.Kind, c.ID))
		}
	}
	for _, c := range s.Reviews {
		keys = append(keys, Key(c.Kind, c.ID))
	}
	for _, c := range s.IssueComments {
		keys = append(keys, Key(c.Kind, c.ID))
	}
	sort.Strings(keys)
	return keys
}

// Sorted returns a sorted copy.
func Sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// Equal reports whether two string slices are equal.
func Equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Bodies lists every comment text the scenario contains.
func Bodies(sc Scenario) []string {
	var bodies []string
	for _, f := range sc.Fetches {
		for _, th := range f.Threads {
			for _, c := range th.Comments {
				bodies = append(bodies, c.Body)
			}
		}
		for _, c := range f.Reviews {
			bodies = append(bodies, c.Body)
		}
		for _, c := range f.IssueComments {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}
