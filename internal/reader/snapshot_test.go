package reader

import (
	"regexp"
	"strings"
	"testing"
)

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
