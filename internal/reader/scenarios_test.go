package reader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/scottlz0310/review-raven/internal/trustedauthors"
)

const contractDir = "../../docs/design/contracts/reader"

type fixtureComment struct {
	ID         string  `json:"id"`
	Author     string  `json:"author"`
	AuthorType string  `json:"authorType"`
	URL        string  `json:"url"`
	CreatedAt  string  `json:"createdAt"`
	EditedAt   *string `json:"editedAt"`
	State      string  `json:"state"`
	Body       string  `json:"body"`
}

type fixtureThread struct {
	ID         string           `json:"id"`
	Path       string           `json:"path"`
	Line       *int32           `json:"line"`
	IsResolved bool             `json:"isResolved"`
	IsOutdated bool             `json:"isOutdated"`
	Comments   []fixtureComment `json:"comments"`
}

type fixtureSnapshot struct {
	Moved         bool             `json:"moved"`
	HeadSHA       string           `json:"headSha"`
	BaseSHA       string           `json:"baseSha"`
	Threads       []fixtureThread  `json:"threads"`
	Reviews       []fixtureComment `json:"reviews"`
	IssueComments []fixtureComment `json:"issueComments"`
}

type fixtureExpect struct {
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

type fixtureScenario struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	ServerAllowlist  []string `json:"serverAllowlist"`
	ProjectAllowlist struct {
		State  string   `json:"state"`
		Logins []string `json:"logins"`
	} `json:"projectAllowlist"`
	Fetches []fixtureSnapshot `json:"fetches"`
	Expect  fixtureExpect     `json:"expect"`
}

type fixtureFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	Scenarios     []fixtureScenario `json:"scenarios"`
}

func toComment(kind Kind, c fixtureComment, withBody bool) Comment {
	out := Comment{Kind: kind, ID: c.ID, Author: c.Author, AuthorType: c.AuthorType, URL: c.URL, CreatedAt: c.CreatedAt, State: c.State}
	if c.EditedAt != nil {
		out.EditedAt = *c.EditedAt
	}
	if withBody {
		body := c.Body
		out.Body = &body
	}
	return out
}

func (f fixtureSnapshot) snapshot(withBodies bool) Snapshot {
	s := Snapshot{HeadSHA: f.HeadSHA, BaseSHA: f.BaseSHA}
	for _, t := range f.Threads {
		th := Thread{ID: t.ID, Path: t.Path, Line: t.Line, IsResolved: t.IsResolved, IsOutdated: t.IsOutdated}
		for _, c := range t.Comments {
			th.Comments = append(th.Comments, toComment(KindThreadComment, c, withBodies))
		}
		s.Threads = append(s.Threads, th)
	}
	for _, c := range f.Reviews {
		s.Reviews = append(s.Reviews, toComment(KindReviewBody, c, withBodies))
	}
	for _, c := range f.IssueComments {
		s.IssueComments = append(s.IssueComments, toComment(KindIssueComment, c, withBodies))
	}
	return s
}

// scriptedSource replays a scenario's fetches in order. A metadata call gets the
// fixture without any body, as GitHub would answer a query that does not select
// bodies.
type scriptedSource struct {
	t         *testing.T
	scenario  fixtureScenario
	calls     int
	bodyCalls int
}

func (s *scriptedSource) Fetch(_ context.Context, withBodies bool) (Snapshot, error) {
	if s.calls >= len(s.scenario.Fetches) {
		s.t.Fatalf("unexpected fetch #%d (withBodies=%v): the scenario scripts only %d", s.calls+1, withBodies, len(s.scenario.Fetches))
	}
	entry := s.scenario.Fetches[s.calls]
	s.calls++
	if withBodies {
		s.bodyCalls++
	}
	if entry.Moved {
		return Snapshot{}, ErrSnapshotMoved
	}
	return entry.snapshot(withBodies), nil
}

func (s *scriptedSource) ProjectAllowlist(_ context.Context, _ string) (ProjectAllowlist, error) {
	switch s.scenario.ProjectAllowlist.State {
	case "present":
		return ProjectAllowlist{State: ProjectAllowlistPresent, Logins: s.scenario.ProjectAllowlist.Logins}, nil
	case "invalid":
		_, err := trustedauthors.ParseProjectFile([]byte(`{"version":1}`))
		return ProjectAllowlist{}, errors.Join(ErrProjectAllowlistInvalid, err)
	default:
		return ProjectAllowlist{State: ProjectAllowlistAbsent}, nil
	}
}

func loadScenarios(t *testing.T) fixtureFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixtureFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.SchemaVersion != 1 || len(f.Scenarios) == 0 {
		t.Fatalf("unexpected scenarios file: schemaVersion=%d scenarios=%d", f.SchemaVersion, len(f.Scenarios))
	}
	return f
}

func resolveSchema(t *testing.T, file string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, file))
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

func validateAgainst(t *testing.T, schema *jsonschema.Resolved, v any) {
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

func commentKey(kind Kind, id string) string { return string(kind) + ":" + id }

func resultKeys(s Snapshot) []string {
	var keys []string
	for _, l := range allComments(s) {
		keys = append(keys, commentKey(l.comment.Kind, l.comment.ID))
	}
	sort.Strings(keys)
	return keys
}

func sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
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

func allFixtureBodies(sc fixtureScenario) []string {
	var bodies []string
	for _, f := range sc.Fetches {
		for _, t := range f.Threads {
			for _, c := range t.Comments {
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

func TestScenarios(t *testing.T) {
	outputSchema := resolveSchema(t, "read_pr_comments.output.schema.json")
	errorSchema := resolveSchema(t, "read_pr_comments.error.schema.json")

	for _, sc := range loadScenarios(t).Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			src := &scriptedSource{t: t, scenario: sc}
			result, err := Read(context.Background(), src, sc.ServerAllowlist)

			if src.calls != sc.Expect.FetchCalls || src.bodyCalls != sc.Expect.BodyFetchCalls {
				t.Fatalf("fetch calls = %d (with bodies %d), want %d (with bodies %d)", src.calls, src.bodyCalls, sc.Expect.FetchCalls, sc.Expect.BodyFetchCalls)
			}

			if sc.Expect.Outcome == "ok" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got, want := resultKeys(result.Snapshot), sorted(sc.Expect.CommentIDs); !equalStrings(got, want) {
					t.Fatalf("comments = %v, want %v", got, want)
				}
				for _, l := range allComments(result.Snapshot) {
					if l.comment.Body == nil {
						t.Fatalf("%s %s has no body", l.comment.Kind, l.comment.ID)
					}
				}
				if want := sc.Expect.Allowlist; want != nil {
					got := result.Allowlist
					if got.ServerLogins != want.ServerLogins || got.ProjectFile != want.ProjectFile || got.ProjectLogins != want.ProjectLogins {
						t.Fatalf("allowlist = %+v, want %+v", got, *want)
					}
				}
				validateAgainst(t, outputSchema, ToOutput(result))
				return
			}

			var gateErr *Error
			if !errors.As(err, &gateErr) {
				t.Fatalf("error = %v, want a gate error %s", err, sc.Expect.Code)
			}
			if gateErr.Code != sc.Expect.Code {
				t.Fatalf("code = %s, want %s", gateErr.Code, sc.Expect.Code)
			}
			var refs []string
			for _, c := range gateErr.Comments {
				refs = append(refs, commentKey(c.Kind, c.ID))
			}
			if got, want := sorted(refs), sorted(sc.Expect.ErrorCommentIDs); !equalStrings(got, want) {
				t.Fatalf("error comments = %v, want %v", got, want)
			}
			envelope := ToErrorEnvelope(gateErr)
			validateAgainst(t, errorSchema, envelope)

			// 失敗の応答に、どのコメントの本文も含まれない。
			raw, _ := json.Marshal(envelope)
			for _, body := range allFixtureBodies(sc) {
				if body != "" && strings.Contains(string(raw), body) {
					t.Fatalf("error response leaks comment text %q: %s", body, raw)
				}
			}
		})
	}
}

// 検証で止まったシナリオでは、本文を要求した回数が、検証を通過した試行の分だけである。
// 取り直した試行は「メタデータ→本文」の組、最後に止まった試行は「メタデータ」だけなので、
// メタデータの呼び出し回数 = 本文の呼び出し回数 + 1 になる（GitHub を呼ばずに止まる場合は 0 回）。
func TestBodiesAreNeverRequestedBeforeTheAuthorCheckPasses(t *testing.T) {
	for _, sc := range loadScenarios(t).Scenarios {
		switch sc.Expect.Code {
		case CodeUntrustedComment, CodeAuthorCheckFailed, CodeProjectAllowlistInvalid:
			if sc.Expect.FetchCalls != 2*sc.Expect.BodyFetchCalls+1 {
				t.Fatalf("%s: fetch calls = %d, body calls = %d: a blocked attempt must stop before it asks for any text", sc.Name, sc.Expect.FetchCalls, sc.Expect.BodyFetchCalls)
			}
		case CodeTrustedAuthorsNotConfigured:
			if sc.Expect.FetchCalls != 0 {
				t.Fatalf("%s: an empty allowlist must stop before calling GitHub", sc.Name)
			}
		}
	}
}

var snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestSnapshotID(t *testing.T) {
	body := func(s string) *string { return &s }
	base := Snapshot{
		HeadSHA: strings.Repeat("1", 40), BaseSHA: strings.Repeat("2", 40),
		IssueComments: []Comment{
			{Kind: KindIssueComment, ID: "2", Author: "thread-owl", AuthorType: "Bot", Body: body("b")},
			{Kind: KindIssueComment, ID: "1", Author: "thread-owl", AuthorType: "Bot", Body: body("a")},
		},
	}
	id := snapshotID(base)
	if !snapshotIDPattern.MatchString(id) {
		t.Fatalf("snapshot id = %q", id)
	}

	reordered := base
	reordered.IssueComments = []Comment{base.IssueComments[1], base.IssueComments[0]}
	changedText := base
	changedText.IssueComments = []Comment{
		{Kind: KindIssueComment, ID: "2", Author: "thread-owl", AuthorType: "Bot", Body: body("changed")},
		base.IssueComments[1],
	}
	tests := []struct {
		name     string
		snapshot Snapshot
		wantSame bool
	}{
		{name: "order does not matter", snapshot: reordered, wantSame: true},
		{name: "body text is not part of the id (edits are tracked by editedAt)", snapshot: changedText, wantSame: true},
		{name: "edit timestamp", snapshot: withEdit(base, "2026-10-25T01:00:00Z"), wantSame: false},
		{name: "head moved", snapshot: withHead(base, strings.Repeat("3", 40)), wantSame: false},
		{name: "author changed", snapshot: withAuthor(base, "mallory"), wantSame: false},
		{name: "comment added", snapshot: withExtra(base), wantSame: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := snapshotID(tt.snapshot) == id; got != tt.wantSame {
				t.Fatalf("same id = %v, want %v", got, tt.wantSame)
			}
		})
	}
}

func withEdit(s Snapshot, edited string) Snapshot {
	out := s
	out.IssueComments = append([]Comment{}, s.IssueComments...)
	out.IssueComments[0].EditedAt = edited
	return out
}

func withHead(s Snapshot, head string) Snapshot {
	out := s
	out.HeadSHA = head
	return out
}

func withAuthor(s Snapshot, author string) Snapshot {
	out := s
	out.IssueComments = append([]Comment{}, s.IssueComments...)
	out.IssueComments[0].Author = author
	return out
}

func withExtra(s Snapshot) Snapshot {
	out := s
	out.IssueComments = append(append([]Comment{}, s.IssueComments...), Comment{Kind: KindIssueComment, ID: "9", Author: "thread-owl", AuthorType: "Bot"})
	return out
}

// 不正なプロジェクト allowlist のエラーが長くても、失敗の応答は契約（message は 500 文字以内）を満たす。
func TestProjectAllowlistErrorStaysWithinTheContract(t *testing.T) {
	errorSchema := resolveSchema(t, "read_pr_comments.error.schema.json")
	for _, value := range []string{strings.Repeat("!", 600), strings.Repeat("あ", 600)} {
		_, parseErr := trustedauthors.ParseProjectFile([]byte(`{"version":1,"additional_logins":["` + value + `"]}`))
		if parseErr == nil {
			t.Fatal("expected the project file to be rejected")
		}
		src := &failingProjectSource{err: errors.Join(ErrProjectAllowlistInvalid, parseErr)}
		_, err := Read(context.Background(), src, []string{"thread-owl"})

		var gateErr *Error
		if !errors.As(err, &gateErr) || gateErr.Code != CodeProjectAllowlistInvalid {
			t.Fatalf("error = %v, want %s", err, CodeProjectAllowlistInvalid)
		}
		validateAgainst(t, errorSchema, ToErrorEnvelope(gateErr))
	}
}

// 上流のエラーがどれだけ長くても、契約の長さを超えず、文字の途中では切らない。
func TestErrorEnvelopeClampsLongMessagesOnACharacterBoundary(t *testing.T) {
	errorSchema := resolveSchema(t, "read_pr_comments.error.schema.json")
	for _, msg := range []string{strings.Repeat("x", 501), strings.Repeat("あ", 2000), strings.Repeat("x", 500)} {
		envelope := ToErrorEnvelope(&Error{Code: CodeSnapshotChanged, Message: msg})
		got := envelope.Error.Message
		if n := len([]rune(got)); n > MaxMessageLength {
			t.Fatalf("message has %d characters, want at most %d", n, MaxMessageLength)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("message is not valid UTF-8 after clamping: %q", got)
		}
		if len([]rune(msg)) <= MaxMessageLength && got != msg {
			t.Fatalf("a message within the limit must not change: %q", got)
		}
		validateAgainst(t, errorSchema, envelope)
	}
}

type failingProjectSource struct{ err error }

func (s *failingProjectSource) Fetch(context.Context, bool) (Snapshot, error) {
	return Snapshot{HeadSHA: strings.Repeat("1", 40), BaseSHA: strings.Repeat("2", 40)}, nil
}

func (s *failingProjectSource) ProjectAllowlist(context.Context, string) (ProjectAllowlist, error) {
	return ProjectAllowlist{}, s.err
}
