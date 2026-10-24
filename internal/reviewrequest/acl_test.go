package reviewrequest

import (
	"context"
	"strings"
	"testing"
)

func TestParseACL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"single user and repository", `{"users":{"alice":["owner/repo"]}}`, true},
		{"owner wildcard", `{"users":{"alice":["owner/*"]}}`, true},
		{"multiple repositories and operators", `{"users":{"alice":["o/a","o/b"],"bob":["o/*"]},"operators":["bob"]}`, true},
		{"login with bot suffix and uppercase", `{"users":{"Alice[bot]":["owner/repo"]}}`, true},
		{"empty string", ``, false},
		{"only whitespace", "  \n", false},
		{"no users", `{"users":{}}`, false},
		{"missing users", `{}`, false},
		{"unknown field", `{"users":{"alice":["o/r"]},"admins":["alice"]}`, false},
		{"trailing data", `{"users":{"alice":["o/r"]}} {"users":{}}`, false},
		{"not json", `alice:owner/repo`, false},
		{"user without repositories", `{"users":{"alice":[]}}`, false},
		{"bare wildcard repository", `{"users":{"alice":["*"]}}`, false},
		{"wildcard owner", `{"users":{"alice":["*/repo"]}}`, false},
		{"double wildcard", `{"users":{"alice":["owner/**"]}}`, false},
		{"too many segments", `{"users":{"alice":["owner/repo/extra"]}}`, false},
		{"uppercase repository", `{"users":{"alice":["Owner/Repo"]}}`, false},
		{"wildcard login", `{"users":{"*":["owner/repo"]}}`, false},
		{"empty login", `{"users":{"":["owner/repo"]}}`, false},
		{"duplicate login after normalization", `{"users":{"Alice":["o/r"],"alice":["o/r"]}}`, false},
		{"operator that is not a user", `{"users":{"alice":["o/r"]},"operators":["bob"]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acl, err := ParseACL(tc.raw)
			if (err == nil) != tc.ok {
				t.Fatalf("ParseACL() error = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && acl == nil {
				t.Fatal("a valid ACL must not be nil")
			}
		})
	}

	t.Run("users are normalized and sorted", func(t *testing.T) {
		acl, err := ParseACL(`{"users":{"Bob[bot]":["o/r"],"alice":["o/r"]}}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(acl.Users(), ","); got != "alice,bob" {
			t.Fatalf("Users() = %q, want alice,bob", got)
		}
	})
}

func TestACLAuthorize(t *testing.T) {
	acl, err := ParseACL(`{
		"users": {
			"alice": ["owner/repo", "wild/*"],
			"bob": ["owner/repo"],
			"ci-app": ["owner/repo"]
		},
		"operators": ["bob"]
	}`)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		principal   string
		action      string
		owner, repo string
		requestedBy string
		want        bool
	}{
		{"request on a listed repository", "alice", ActionRequest, "owner", "repo", "", true},
		{"read on a listed repository", "alice", ActionRead, "owner", "repo", "alice", true},
		{"owner wildcard matches any repository", "alice", ActionRequest, "wild", "anything", "", true},
		{"owner wildcard does not match another owner", "alice", ActionRequest, "other", "anything", "", false},
		{"unlisted repository", "alice", ActionRequest, "owner", "other", "", false},
		{"a user with a narrower list", "bob", ActionRead, "wild", "x", "", false},
		{"unknown principal", "mallory", ActionRead, "owner", "repo", "", false},
		{"empty principal", "", ActionRead, "owner", "repo", "", false},
		{"principal is normalized (case and bot suffix)", "Alice[bot]", ActionRead, "owner", "repo", "", true},
		{"repository case is normalized", "alice", ActionRead, "Owner", "Repo", "", true},
		{"requester may cancel its own request", "alice", ActionCancel, "owner", "repo", "alice", true},
		{"requester spelled with a bot suffix", "alice", ActionCancel, "owner", "repo", "Alice[bot]", true},
		{"another user may not cancel", "alice", ActionCancel, "owner", "repo", "ci-app", false},
		{"operator may cancel another user's request", "bob", ActionCancel, "owner", "repo", "alice", true},
		{"operator is still limited to its repositories", "bob", ActionCancel, "wild", "x", "alice", false},
		{"cancel without a recorded requester", "alice", ActionCancel, "owner", "repo", "", false},
		{"internal ingest is denied here", "alice", ActionIngest, "owner", "repo", "", false},
		{"internal publish_result is denied here", "alice", ActionPublishResult, "owner", "repo", "", false},
		{"internal observe is denied here", "alice", ActionObserve, "owner", "repo", "", false},
		{"unknown action", "alice", "delete", "owner", "repo", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := acl.Authorize(context.Background(), tc.principal, tc.action, tc.owner, tc.repo, tc.requestedBy)
			if got != tc.want {
				t.Fatalf("Authorize() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a nil ACL denies everyone", func(t *testing.T) {
		var none *ACL
		if none.Authorize(context.Background(), "alice", ActionRead, "owner", "repo", "") {
			t.Fatal("a nil ACL must deny")
		}
	})
}

func TestDenyAllProducers(t *testing.T) {
	if (DenyAllProducers{}).MayPublish(context.Background(), "svc-squirrel", "squirrel-notifier") {
		t.Fatal("DenyAllProducers must not authorize anyone")
	}
}
