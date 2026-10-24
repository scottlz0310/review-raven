package reviewrequest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ServiceACL is the protected "service principal → operations → repositories"
// list of the internal (service-to-service) surface. A service principal is the
// identity the gateway registered for owl or Squirrel; it is never a user of
// the public surface, and the two lists must not share a name (see
// NewSplitAuthorizer).
//
// Each service names the operations it may call and the repositories it may
// act on; a service that publishes observations also names the producer IDs it
// may publish as, which is how an observation is tied to its sender. An
// operation, repository or producer that is not listed is denied.
type ServiceACL struct {
	services map[string]*serviceGrant
}

type serviceGrant struct {
	actions   map[string]bool
	repos     []repoPattern
	producers map[string]bool
}

type serviceACLDocument struct {
	Services map[string]serviceGrantDocument `json:"services"`
}

type serviceGrantDocument struct {
	Actions   []string `json:"actions"`
	Repos     []string `json:"repos"`
	Producers []string `json:"producers"`
}

// Internal actions a service may be granted.
var internalActions = []string{ActionIngest, ActionPublishResult, ActionObserve}

// ParseServiceACL parses and validates the service ACL. Every malformed entry
// is an error so that a typo can never silently widen or empty a grant.
func ParseServiceACL(raw string) (*ServiceACL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("the service ACL is empty: no service would be authorized")
	}
	var doc serviceACLDocument
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse service ACL: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse service ACL: trailing data after the JSON document")
	}
	if len(doc.Services) == 0 {
		return nil, errors.New("the service ACL has no services: no service would be authorized")
	}

	acl := &ServiceACL{services: map[string]*serviceGrant{}}
	for rawName, grant := range doc.Services {
		name := NormalizeLogin(strings.TrimSpace(rawName))
		if name == "" || strings.ContainsAny(name, "*? \t") {
			return nil, fmt.Errorf("invalid service name %q", rawName)
		}
		if _, dup := acl.services[name]; dup {
			return nil, fmt.Errorf("duplicate service %q after normalization", name)
		}
		parsed, err := parseGrant(name, grant)
		if err != nil {
			return nil, err
		}
		acl.services[name] = parsed
	}
	return acl, nil
}

func parseGrant(name string, grant serviceGrantDocument) (*serviceGrant, error) {
	if len(grant.Actions) == 0 {
		return nil, fmt.Errorf("service %q has no actions", name)
	}
	out := &serviceGrant{actions: map[string]bool{}, producers: map[string]bool{}}
	for _, a := range grant.Actions {
		if !slices.Contains(internalActions, a) {
			return nil, fmt.Errorf("service %q: unknown action %q (use %s)", name, a, strings.Join(internalActions, ", "))
		}
		if out.actions[a] {
			return nil, fmt.Errorf("service %q: duplicate action %q", name, a)
		}
		out.actions[a] = true
	}

	if len(grant.Repos) == 0 {
		return nil, fmt.Errorf("service %q has no repositories", name)
	}
	for _, p := range grant.Repos {
		m := rePattern.FindStringSubmatch(p)
		if m == nil {
			return nil, fmt.Errorf("service %q: invalid repository %q: use owner/repo or owner/*", name, p)
		}
		out.repos = append(out.repos, repoPattern{owner: m[1], repo: m[2]})
	}

	// producers は、観測を送る権限(observe)を持つ service だけが持つ。
	switch {
	case out.actions[ActionObserve] && len(grant.Producers) == 0:
		return nil, fmt.Errorf("service %q may observe but lists no producers", name)
	case !out.actions[ActionObserve] && len(grant.Producers) > 0:
		return nil, fmt.Errorf("service %q lists producers without the observe action", name)
	}
	for _, p := range grant.Producers {
		if !reProducerID.MatchString(p) {
			return nil, fmt.Errorf("service %q: invalid producer %q", name, p)
		}
		if out.producers[p] {
			return nil, fmt.Errorf("service %q: duplicate producer %q", name, p)
		}
		out.producers[p] = true
	}
	return out, nil
}

// Services returns the normalized service names, sorted.
func (a *ServiceACL) Services() []string {
	out := make([]string, 0, len(a.services))
	for name := range a.services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Authorize implements Authorizer for the internal actions only.
func (a *ServiceACL) Authorize(_ context.Context, principal, action, owner, repo, _ string) bool {
	if a == nil {
		return false
	}
	grant, ok := a.services[NormalizeLogin(principal)]
	if !ok || !grant.actions[action] {
		return false
	}
	return matchesRepo(grant.repos, owner, repo)
}

// MayPublish implements ProducerPolicy: a service may publish only as one of
// the producers listed for it, and only if it holds the observe action.
func (a *ServiceACL) MayPublish(_ context.Context, principal, producerID string) bool {
	if a == nil {
		return false
	}
	grant, ok := a.services[NormalizeLogin(principal)]
	return ok && grant.actions[ActionObserve] && grant.producers[producerID]
}

type splitAuthorizer struct {
	public  *ACL
	service *ServiceACL
}

// NewSplitAuthorizer routes each action to the list that owns it: the user ACL
// decides request / read / cancel, the service ACL decides the internal
// actions, and nothing else is allowed. A name present in both lists is an
// error, so one identity can never act on both surfaces.
func NewSplitAuthorizer(public *ACL, service *ServiceACL) (Authorizer, error) {
	if public == nil || service == nil {
		return nil, errors.New("both the user ACL and the service ACL are required")
	}
	for _, name := range service.Services() {
		if _, shared := public.users[name]; shared {
			return nil, fmt.Errorf("%q is listed both as a user and as a service: use distinct identities", name)
		}
	}
	return splitAuthorizer{public: public, service: service}, nil
}

func (s splitAuthorizer) Authorize(ctx context.Context, principal, action, owner, repo, requestedBy string) bool {
	switch action {
	case ActionRequest, ActionRead, ActionCancel:
		return s.public.Authorize(ctx, principal, action, owner, repo, requestedBy)
	case ActionIngest, ActionPublishResult, ActionObserve:
		return s.service.Authorize(ctx, principal, action, owner, repo, requestedBy)
	default:
		return false
	}
}
