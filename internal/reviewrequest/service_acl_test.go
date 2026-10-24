package reviewrequest

import (
	"context"
	"strings"
	"testing"
)

func TestParseServiceACL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"owl may ingest and publish results", `{"services":{"svc-owl":{"actions":["ingest","publish_result"],"repos":["o/*"]}}}`, true},
		{"squirrel may observe as a producer", `{"services":{"svc-squirrel":{"actions":["observe"],"repos":["o/r"],"producers":["squirrel-notifier"]}}}`, true},
		{"normalized service name", `{"services":{"SVC-Owl[bot]":{"actions":["ingest"],"repos":["o/*"]}}}`, true},
		{"empty", ``, false},
		{"no services", `{"services":{}}`, false},
		{"unknown field", `{"services":{"s":{"actions":["ingest"],"repos":["o/*"],"admin":true}}}`, false},
		{"trailing data", `{"services":{"s":{"actions":["ingest"],"repos":["o/*"]}}} 1`, false},
		{"not json", `svc-owl`, false},
		{"service without actions", `{"services":{"s":{"actions":[],"repos":["o/*"]}}}`, false},
		{"a public action is not an internal action", `{"services":{"s":{"actions":["request"],"repos":["o/*"]}}}`, false},
		{"unknown action", `{"services":{"s":{"actions":["delete"],"repos":["o/*"]}}}`, false},
		{"duplicate action", `{"services":{"s":{"actions":["ingest","ingest"],"repos":["o/*"]}}}`, false},
		{"service without repositories", `{"services":{"s":{"actions":["ingest"],"repos":[]}}}`, false},
		{"bare wildcard repository", `{"services":{"s":{"actions":["ingest"],"repos":["*"]}}}`, false},
		{"invalid repository", `{"services":{"s":{"actions":["ingest"],"repos":["O/R/x"]}}}`, false},
		{"observe without producers", `{"services":{"s":{"actions":["observe"],"repos":["o/*"]}}}`, false},
		{"producers without observe", `{"services":{"s":{"actions":["ingest"],"repos":["o/*"],"producers":["p"]}}}`, false},
		{"invalid producer id", `{"services":{"s":{"actions":["observe"],"repos":["o/*"],"producers":["Bad Producer"]}}}`, false},
		{"duplicate producer", `{"services":{"s":{"actions":["observe"],"repos":["o/*"],"producers":["p","p"]}}}`, false},
		{"wildcard service name", `{"services":{"*":{"actions":["ingest"],"repos":["o/*"]}}}`, false},
		{"empty service name", `{"services":{"":{"actions":["ingest"],"repos":["o/*"]}}}`, false},
		{"duplicate service after normalization", `{"services":{"Svc":{"actions":["ingest"],"repos":["o/*"]},"svc":{"actions":["ingest"],"repos":["o/*"]}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acl, err := ParseServiceACL(tc.raw)
			if (err == nil) != tc.ok {
				t.Fatalf("ParseServiceACL() error = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && acl == nil {
				t.Fatal("a valid ACL must not be nil")
			}
		})
	}

	t.Run("names are normalized and sorted", func(t *testing.T) {
		acl, err := ParseServiceACL(`{"services":{"Svc-B":{"actions":["ingest"],"repos":["o/*"]},"svc-a[bot]":{"actions":["ingest"],"repos":["o/*"]}}}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(acl.Services(), ","); got != "svc-a,svc-b" {
			t.Fatalf("Services() = %q", got)
		}
	})
}

const testServiceACL = `{
	"services": {
		"svc-owl": {"actions": ["ingest", "publish_result"], "repos": ["owner/*"]},
		"svc-squirrel": {"actions": ["observe"], "repos": ["owner/repo"], "producers": ["squirrel-notifier"]},
		"svc-narrow": {"actions": ["ingest"], "repos": ["other/repo"]}
	}
}`

func TestServiceACLAuthorize(t *testing.T) {
	acl, err := ParseServiceACL(testServiceACL)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		principal   string
		action      string
		owner, repo string
		want        bool
	}{
		{"owl ingest on its repositories", "svc-owl", ActionIngest, "owner", "anything", true},
		{"owl publish_result", "svc-owl", ActionPublishResult, "owner", "x", true},
		{"owl may not observe", "svc-owl", ActionObserve, "owner", "x", false},
		{"owl outside its repositories", "svc-owl", ActionIngest, "other", "repo", false},
		{"squirrel observe on its repository", "svc-squirrel", ActionObserve, "owner", "repo", true},
		{"squirrel on another repository", "svc-squirrel", ActionObserve, "owner", "other", false},
		{"squirrel may not ingest", "svc-squirrel", ActionIngest, "owner", "repo", false},
		{"narrow service on its single repository", "svc-narrow", ActionIngest, "other", "repo", true},
		{"narrow service elsewhere", "svc-narrow", ActionIngest, "owner", "repo", false},
		{"unknown service", "svc-unknown", ActionIngest, "owner", "repo", false},
		{"empty principal", "", ActionIngest, "owner", "repo", false},
		{"name is normalized", "SVC-Owl[bot]", ActionIngest, "owner", "x", true},
		{"public actions are never allowed here", "svc-owl", ActionRequest, "owner", "x", false},
		{"cancel is never allowed here", "svc-owl", ActionCancel, "owner", "x", false},
		{"read is never allowed here", "svc-owl", ActionRead, "owner", "x", false},
		{"unknown action", "svc-owl", "delete", "owner", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acl.Authorize(context.Background(), tc.principal, tc.action, tc.owner, tc.repo, ""); got != tc.want {
				t.Fatalf("Authorize() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a nil ACL denies everyone", func(t *testing.T) {
		var none *ServiceACL
		if none.Authorize(context.Background(), "svc-owl", ActionIngest, "owner", "repo", "") || none.MayPublish(context.Background(), "svc-owl", "p") {
			t.Fatal("a nil service ACL must deny")
		}
	})
}

func TestServiceACLMayPublish(t *testing.T) {
	acl, err := ParseServiceACL(testServiceACL)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		principal string
		producer  string
		want      bool
	}{
		{"the producer granted to the service", "svc-squirrel", "squirrel-notifier", true},
		{"another producer id", "svc-squirrel", "other-producer", false},
		{"a service without the observe action", "svc-owl", "squirrel-notifier", false},
		{"an unknown service", "svc-unknown", "squirrel-notifier", false},
		{"name is normalized", "SVC-Squirrel", "squirrel-notifier", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acl.MayPublish(context.Background(), tc.principal, tc.producer); got != tc.want {
				t.Fatalf("MayPublish() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplitAuthorizer(t *testing.T) {
	users, err := ParseACL(`{"users":{"alice":["owner/*"]},"operators":["alice"]}`)
	if err != nil {
		t.Fatal(err)
	}
	services, err := ParseServiceACL(testServiceACL)
	if err != nil {
		t.Fatal(err)
	}
	authz, err := NewSplitAuthorizer(users, services)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		principal string
		action    string
		want      bool
	}{
		{"a user may use the public surface", "alice", ActionRequest, true},
		{"a user may cancel", "alice", ActionCancel, true},
		{"a user may not use the internal surface", "alice", ActionIngest, false},
		{"a user may not publish results", "alice", ActionPublishResult, false},
		{"a service may use the internal surface", "svc-owl", ActionIngest, true},
		{"a service may not use the public surface", "svc-owl", ActionRequest, false},
		{"a service may not read", "svc-owl", ActionRead, false},
		{"unknown action", "alice", "delete", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authz.Authorize(context.Background(), tc.principal, tc.action, "owner", "repo", "alice"); got != tc.want {
				t.Fatalf("Authorize() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("one identity cannot be both a user and a service", func(t *testing.T) {
		overlapping, err := ParseACL(`{"users":{"svc-owl":["owner/*"]}}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewSplitAuthorizer(overlapping, services); err == nil {
			t.Fatal("an identity listed as both a user and a service must be rejected")
		}
	})
	t.Run("both lists are required", func(t *testing.T) {
		if _, err := NewSplitAuthorizer(nil, services); err == nil {
			t.Fatal("a missing user ACL must be rejected")
		}
		if _, err := NewSplitAuthorizer(users, nil); err == nil {
			t.Fatal("a missing service ACL must be rejected")
		}
	})
}
