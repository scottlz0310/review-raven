package reader

// SchemaVersion is the version of the reader contract.
const SchemaVersion = 1

// Output is the wire form of a verified snapshot (read_pr_comments).
type Output struct {
	SchemaVersion int             `json:"schemaVersion"`
	SnapshotID    string          `json:"snapshotId"`
	HeadSHA       string          `json:"headSha"`
	BaseSHA       string          `json:"baseSha"`
	Allowlist     OutputAllowlist `json:"allowlist"`
	Threads       []OutputThread  `json:"threads"`
	Reviews       []OutputComment `json:"reviews"`
	IssueComments []OutputComment `json:"issueComments"`
	Summary       OutputSummary   `json:"summary"`
	Pagination    OutputPaging    `json:"pagination"`
}

// OutputAllowlist says which allowlist was applied, without listing logins.
type OutputAllowlist struct {
	ServerLogins  int  `json:"serverLogins"`
	ProjectFile   bool `json:"projectFile"`
	ProjectLogins int  `json:"projectLogins"`
}

// OutputThread is a review thread with its verified comments.
type OutputThread struct {
	ID         string          `json:"id"`
	Path       string          `json:"path"`
	Line       *int32          `json:"line"`
	IsResolved bool            `json:"isResolved"`
	IsOutdated bool            `json:"isOutdated"`
	Comments   []OutputComment `json:"comments"`
}

// OutputComment is a verified comment. State is set for review bodies only.
type OutputComment struct {
	ID         string  `json:"id"`
	Author     string  `json:"author"`
	AuthorType string  `json:"authorType"`
	URL        string  `json:"url"`
	CreatedAt  string  `json:"createdAt"`
	EditedAt   *string `json:"editedAt"`
	State      *string `json:"state,omitempty"`
	Body       string  `json:"body"`
}

// OutputSummary counts what was returned.
type OutputSummary struct {
	Threads           int `json:"threads"`
	UnresolvedThreads int `json:"unresolvedThreads"`
	Reviews           int `json:"reviews"`
	IssueComments     int `json:"issueComments"`
}

// OutputPaging is always complete: the reader walks every page before it answers.
type OutputPaging struct {
	Complete bool `json:"complete"`
}

// ErrorEnvelope is the wire form of a failure.
type ErrorEnvelope struct {
	SchemaVersion int         `json:"schemaVersion"`
	Error         ErrorDetail `json:"error"`
}

// ErrorDetail is the body of an ErrorEnvelope.
type ErrorDetail struct {
	Code      string        `json:"code"`
	Message   string        `json:"message"`
	Retryable bool          `json:"retryable"`
	Details   *ErrorDetails `json:"details,omitempty"`
}

// ErrorDetails lists the comments that stopped the gate, without their text.
type ErrorDetails struct {
	Comments []OutputCommentRef `json:"comments"`
}

// OutputCommentRef identifies a comment that stopped the gate.
type OutputCommentRef struct {
	Kind       Kind   `json:"kind"`
	ID         string `json:"id"`
	ThreadID   string `json:"threadId,omitempty"`
	Author     string `json:"author"`
	AuthorType string `json:"authorType"`
	URL        string `json:"url"`
}

// ToOutput renders a verified result in its wire form. Every comment in a
// Result carries a body, so none is dropped here.
func ToOutput(r Result) Output {
	s := r.Snapshot
	out := Output{
		SchemaVersion: SchemaVersion,
		SnapshotID:    r.SnapshotID,
		HeadSHA:       s.HeadSHA,
		BaseSHA:       s.BaseSHA,
		Allowlist: OutputAllowlist{
			ServerLogins:  r.Allowlist.ServerLogins,
			ProjectFile:   r.Allowlist.ProjectFile,
			ProjectLogins: r.Allowlist.ProjectLogins,
		},
		Threads:       make([]OutputThread, 0, len(s.Threads)),
		Reviews:       outputComments(s.Reviews),
		IssueComments: outputComments(s.IssueComments),
		Pagination:    OutputPaging{Complete: true},
	}
	for _, t := range s.Threads {
		out.Threads = append(out.Threads, OutputThread{
			ID: t.ID, Path: t.Path, Line: t.Line, IsResolved: t.IsResolved, IsOutdated: t.IsOutdated,
			Comments: outputComments(t.Comments),
		})
		if !t.IsResolved {
			out.Summary.UnresolvedThreads++
		}
	}
	out.Summary.Threads = len(s.Threads)
	out.Summary.Reviews = len(s.Reviews)
	out.Summary.IssueComments = len(s.IssueComments)
	return out
}

func outputComments(in []Comment) []OutputComment {
	out := make([]OutputComment, 0, len(in))
	for _, c := range in {
		oc := OutputComment{ID: c.ID, Author: c.Author, AuthorType: c.AuthorType, URL: c.URL, CreatedAt: c.CreatedAt}
		if c.EditedAt != "" {
			edited := c.EditedAt
			oc.EditedAt = &edited
		}
		if c.State != "" {
			state := c.State
			oc.State = &state
		}
		if c.Body != nil {
			oc.Body = *c.Body
		}
		out = append(out, oc)
	}
	return out
}

// MaxMessageLength is the longest error message the contract allows (in characters).
const MaxMessageLength = 500

// clampMessage keeps a message within the contract, whatever an upstream error
// put in it. It cuts on a character boundary.
func clampMessage(msg string) string {
	r := []rune(msg)
	if len(r) <= MaxMessageLength {
		return msg
	}
	return string(r[:MaxMessageLength-3]) + "..."
}

// ToErrorEnvelope renders a failure. retryable is true only when the same call
// can succeed later without any change on the pull request's side.
func ToErrorEnvelope(e *Error) ErrorEnvelope {
	detail := ErrorDetail{Code: e.Code, Message: clampMessage(e.Message), Retryable: e.Code == CodeSnapshotChanged}
	if len(e.Comments) > 0 {
		refs := make([]OutputCommentRef, 0, len(e.Comments))
		for _, c := range e.Comments {
			refs = append(refs, OutputCommentRef(c))
		}
		detail.Details = &ErrorDetails{Comments: refs}
	}
	return ErrorEnvelope{SchemaVersion: SchemaVersion, Error: detail}
}
