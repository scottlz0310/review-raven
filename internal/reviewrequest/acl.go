package reviewrequest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ACL is the protected "user → repository" access list for the public
// surface. It is the only source of authorization for request / read / cancel:
// the broad installation permission of the GitHub App, or knowing a cycle ID,
// never grants access. An unset or empty list denies everyone.
//
// The internal (service-to-service) actions are not part of this list; they are
// denied here and authorized separately by the stage that exposes them.
type ACL struct {
	users     map[string][]repoPattern
	operators map[string]bool
}

type repoPattern struct {
	owner string
	repo  string // "*" matches every repository of the owner
}

var rePattern = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]{0,99})/([a-z0-9._-]{1,100}|\*)$`)

// aclDocument is the JSON shape of the ACL configuration.
type aclDocument struct {
	// Users maps a GitHub login to the repositories ("owner/repo" or
	// "owner/*") it may request, read and cancel its own requests on.
	Users map[string][]string `json:"users"`
	// Operators may also cancel requests created by other principals.
	Operators []string `json:"operators"`
}

// NormalizeLogin lowercases a GitHub login and removes one trailing "[bot]",
// the same rule the comment-author gate uses, so the REST and GraphQL
// spellings of an App identity compare equal.
func NormalizeLogin(login string) string {
	return strings.TrimSuffix(strings.ToLower(login), "[bot]")
}

// ParseACL parses and validates the ACL configuration. A malformed entry
// (wildcard login, unknown field, duplicate user, an operator that is not a
// user) is an error so a typo can never silently widen or empty the list.
func ParseACL(raw string) (*ACL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("the ACL is empty: no principal would be authorized")
	}
	var doc aclDocument
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse ACL: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse ACL: trailing data after the JSON document")
	}
	if len(doc.Users) == 0 {
		return nil, errors.New("the ACL has no users: no principal would be authorized")
	}

	acl := &ACL{users: map[string][]repoPattern{}, operators: map[string]bool{}}
	for login, patterns := range doc.Users {
		name := NormalizeLogin(strings.TrimSpace(login))
		if name == "" || strings.ContainsAny(name, "*? \t") {
			return nil, fmt.Errorf("invalid ACL user %q", login)
		}
		if _, dup := acl.users[name]; dup {
			return nil, fmt.Errorf("duplicate ACL user %q after normalization", name)
		}
		if len(patterns) == 0 {
			return nil, fmt.Errorf("ACL user %q has no repositories", name)
		}
		for _, p := range patterns {
			m := rePattern.FindStringSubmatch(p)
			if m == nil {
				return nil, fmt.Errorf("invalid ACL repository %q for user %q: use owner/repo or owner/*", p, name)
			}
			acl.users[name] = append(acl.users[name], repoPattern{owner: m[1], repo: m[2]})
		}
	}
	for _, op := range doc.Operators {
		name := NormalizeLogin(strings.TrimSpace(op))
		if _, ok := acl.users[name]; !ok {
			return nil, fmt.Errorf("ACL operator %q is not listed in users", op)
		}
		acl.operators[name] = true
	}
	return acl, nil
}

// Users returns the normalized logins, sorted. Intended for startup logs.
func (a *ACL) Users() []string {
	out := make([]string, 0, len(a.users))
	for u := range a.users {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Authorize implements Authorizer.
func (a *ACL) Authorize(_ context.Context, principal, action, owner, repo, requestedBy string) bool {
	if a == nil {
		return false
	}
	name := NormalizeLogin(principal)
	patterns, ok := a.users[name]
	if !ok || name == "" || !matchesRepo(patterns, owner, repo) {
		return false
	}
	switch action {
	case ActionRequest, ActionRead:
		return true
	case ActionCancel:
		return a.operators[name] || (requestedBy != "" && NormalizeLogin(requestedBy) == name)
	default:
		return false
	}
}

func matchesRepo(patterns []repoPattern, owner, repo string) bool {
	owner, repo = strings.ToLower(owner), strings.ToLower(repo)
	for _, p := range patterns {
		if p.owner == owner && (p.repo == "*" || p.repo == repo) {
			return true
		}
	}
	return false
}
