// Package reader reads the text of a pull request's review threads, review
// bodies and issue comments for the reviewed side, enforcing the order
// "metadata of every page, author check, then text".
//
// Text written by other people is untrusted. The reader never asks the source
// for any body until every comment's author has been checked against the
// canonical allowlist, and it returns bodies only if the comments that carried
// them are exactly the comments that were checked: the same set, the same
// authors and the same edit timestamps. A comment added or edited between the
// check and the read makes the snapshot stale, which is retried and then
// reported, never returned.
package reader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/scottlz0310/review-raven/internal/trustedauthors"
)

// Kind is the place a comment was written.
type Kind string

const (
	KindThreadComment Kind = "review_thread_comment"
	KindReviewBody    Kind = "review_body"
	KindIssueComment  Kind = "issue_comment"
)

// Error codes. They are part of the shared reader contract (see
// docs/design/contracts/reader).
const (
	CodeUntrustedComment            = "UNTRUSTED_COMMENT"
	CodeAuthorCheckFailed           = "AUTHOR_CHECK_FAILED"
	CodeTrustedAuthorsNotConfigured = "TRUSTED_AUTHORS_NOT_CONFIGURED"
	CodeProjectAllowlistInvalid     = "PROJECT_ALLOWLIST_INVALID"
	CodeSnapshotChanged             = "SNAPSHOT_CHANGED"
)

// MaxAttempts is how many times a stale snapshot is re-read before giving up.
const MaxAttempts = 3

// Comment is one comment. Body is nil until the text has been read.
type Comment struct {
	Kind       Kind
	ID         string
	Author     string // empty when GitHub reports no author (a deleted account)
	AuthorType string
	URL        string
	CreatedAt  string
	EditedAt   string // empty when never edited
	State      string // review state, review bodies only
	Body       *string
}

// Thread is a review thread with all of its comments.
type Thread struct {
	ID         string
	Path       string
	Line       *int32
	IsResolved bool
	IsOutdated bool
	Comments   []Comment
}

// Snapshot is everything the reader looks at: the pull request's commits and
// all three kinds of comment.
type Snapshot struct {
	HeadSHA       string
	BaseSHA       string
	Threads       []Thread
	Reviews       []Comment
	IssueComments []Comment
}

// ProjectAllowlistState says whether the base ref carries a project allowlist.
type ProjectAllowlistState int

const (
	// ProjectAllowlistAbsent means the file does not exist at the base ref.
	ProjectAllowlistAbsent ProjectAllowlistState = iota
	// ProjectAllowlistPresent means the file exists; Logins holds its entries.
	ProjectAllowlistPresent
)

// ProjectAllowlist is the project-specific addition to the server allowlist.
type ProjectAllowlist struct {
	State  ProjectAllowlistState
	Logins []string // normalized
}

// Source reads from GitHub. Fetch must return every page; withBodies=false must
// not select any body from the upstream. Fetch returns ErrSnapshotMoved when
// the pull request's commits changed while it was paging.
type Source interface {
	Fetch(ctx context.Context, withBodies bool) (Snapshot, error)
	// ProjectAllowlist reads the project allowlist file at baseSHA. A file that
	// exists but is malformed is an error that wraps ErrProjectAllowlistInvalid.
	ProjectAllowlist(ctx context.Context, baseSHA string) (ProjectAllowlist, error)
}

// Sentinel errors a Source may return.
var (
	ErrSnapshotMoved           = errors.New("pull request changed while reading")
	ErrProjectAllowlistInvalid = errors.New("project allowlist is invalid")
)

// CommentRef identifies a comment without its text.
type CommentRef struct {
	Kind       Kind
	ID         string
	ThreadID   string
	Author     string
	AuthorType string
	URL        string
}

// Error is a gate failure. It never carries comment text.
type Error struct {
	Code     string
	Message  string
	Comments []CommentRef
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Result is a verified snapshot: every comment has a body and an author that
// passed the allowlist.
type Result struct {
	Snapshot   Snapshot
	SnapshotID string
	Allowlist  AllowlistInfo
}

// AllowlistInfo describes the allowlist that was applied.
type AllowlistInfo struct {
	ServerLogins  int
	ProjectFile   bool
	ProjectLogins int
}

// Read returns the verified snapshot of a pull request. serverLogins is the
// normalized server allowlist; an empty list trusts nobody and is an error.
func Read(ctx context.Context, src Source, serverLogins []string) (Result, error) {
	if len(serverLogins) == 0 {
		return Result{}, &Error{
			Code:    CodeTrustedAuthorsNotConfigured,
			Message: "no comment author is trusted: the server allowlist (TRUSTED_COMMENT_AUTHORS) is empty",
		}
	}

	for attempt := 1; ; attempt++ {
		result, stale, err := readOnce(ctx, src, serverLogins)
		if err != nil {
			return Result{}, err
		}
		if !stale {
			return result, nil
		}
		if attempt >= MaxAttempts {
			return Result{}, &Error{
				Code:    CodeSnapshotChanged,
				Message: fmt.Sprintf("comments changed between the author check and the text read on %d attempts; no text was returned", MaxAttempts),
			}
		}
	}
}

// readOnce does one pass. stale=true means the pass must be repeated.
func readOnce(ctx context.Context, src Source, serverLogins []string) (res Result, stale bool, err error) {
	meta, err := src.Fetch(ctx, false)
	if err != nil {
		if errors.Is(err, ErrSnapshotMoved) {
			return Result{}, true, nil
		}
		return Result{}, false, fmt.Errorf("read comment metadata: %w", err)
	}
	if err := validateIDs(meta); err != nil {
		return Result{}, false, err
	}

	project, err := src.ProjectAllowlist(ctx, meta.BaseSHA)
	if err != nil {
		if errors.Is(err, ErrProjectAllowlistInvalid) {
			return Result{}, false, &Error{Code: CodeProjectAllowlistInvalid, Message: err.Error()}
		}
		return Result{}, false, fmt.Errorf("read project allowlist: %w", err)
	}
	allowed := make(map[string]struct{}, len(serverLogins)+len(project.Logins))
	for _, login := range serverLogins {
		allowed[login] = struct{}{}
	}
	for _, login := range project.Logins {
		allowed[login] = struct{}{}
	}

	if gateErr := checkAuthors(meta, allowed); gateErr != nil {
		return Result{}, false, gateErr
	}

	full, err := src.Fetch(ctx, true)
	if err != nil {
		if errors.Is(err, ErrSnapshotMoved) {
			return Result{}, true, nil
		}
		return Result{}, false, fmt.Errorf("read comment text: %w", err)
	}
	if fingerprint(meta) != fingerprint(full) {
		return Result{}, true, nil
	}
	if err := requireBodies(full); err != nil {
		return Result{}, false, err
	}

	return Result{
		Snapshot:   full,
		SnapshotID: snapshotID(full),
		Allowlist: AllowlistInfo{
			ServerLogins:  len(serverLogins),
			ProjectFile:   project.State == ProjectAllowlistPresent,
			ProjectLogins: len(project.Logins),
		},
	}, false, nil
}

type located struct {
	comment  Comment
	threadID string
}

func allComments(s Snapshot) []located {
	var out []located
	for _, t := range s.Threads {
		for _, c := range t.Comments {
			out = append(out, located{comment: c, threadID: t.ID})
		}
	}
	for _, c := range s.Reviews {
		out = append(out, located{comment: c})
	}
	for _, c := range s.IssueComments {
		out = append(out, located{comment: c})
	}
	return out
}

func validateIDs(s Snapshot) error {
	seen := map[string]struct{}{}
	for _, l := range allComments(s) {
		if l.comment.ID == "" {
			return fmt.Errorf("read comment metadata: a %s has no id", l.comment.Kind)
		}
		key := string(l.comment.Kind) + "|" + l.comment.ID
		if _, dup := seen[key]; dup {
			return fmt.Errorf("read comment metadata: duplicate %s id %s", l.comment.Kind, l.comment.ID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// checkAuthors applies the canonical allowlist to every comment. A comment
// whose author cannot be determined fails the check as a whole, as does any
// comment by an author who is not allowed.
func checkAuthors(s Snapshot, allowed map[string]struct{}) *Error {
	var unknown, untrusted []CommentRef
	for _, l := range allComments(s) {
		c := l.comment
		ref := CommentRef{Kind: c.Kind, ID: c.ID, ThreadID: l.threadID, Author: c.Author, AuthorType: c.AuthorType, URL: c.URL}
		if c.Author == "" {
			unknown = append(unknown, ref)
			continue
		}
		if _, ok := allowed[trustedauthors.Normalize(c.Author)]; !ok {
			untrusted = append(untrusted, ref)
		}
	}
	switch {
	case len(unknown) > 0:
		return &Error{
			Code:     CodeAuthorCheckFailed,
			Message:  fmt.Sprintf("the author of %d comment(s) could not be determined; no text was read", len(unknown)),
			Comments: append(unknown, untrusted...),
		}
	case len(untrusted) > 0:
		return &Error{
			Code:     CodeUntrustedComment,
			Message:  fmt.Sprintf("%d comment(s) were written by an author who is not in the allowlist; no text was read", len(untrusted)),
			Comments: untrusted,
		}
	}
	return nil
}

func requireBodies(s Snapshot) error {
	for _, l := range allComments(s) {
		if l.comment.Body == nil {
			return fmt.Errorf("read comment text: the source returned no text for %s %s", l.comment.Kind, l.comment.ID)
		}
	}
	return nil
}

// fingerprint captures what the author check relied on. Two snapshots with the
// same fingerprint have the same comments by the same authors, none edited in
// between. Thread resolution does not matter: it carries no author or text.
func fingerprint(s Snapshot) string {
	lines := make([]string, 0, 2)
	lines = append(lines, "head="+s.HeadSHA, "base="+s.BaseSHA)
	for _, l := range allComments(s) {
		c := l.comment
		lines = append(lines, strings.Join([]string{string(c.Kind), c.ID, l.threadID, c.Author, c.AuthorType, c.EditedAt}, "|"))
	}
	sort.Strings(lines[2:])
	return strings.Join(lines, "\n")
}

// snapshotID names a verified snapshot. A later read that yields a different id
// saw different comments, authors or edits (or a moved head).
func snapshotID(s Snapshot) string {
	sum := sha256.Sum256([]byte(fingerprint(s)))
	return hex.EncodeToString(sum[:])
}
