package darksafe

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Effect is the decision a policy produces when it matches.
type Effect string

const (
	// EffectAllow grants access when the policy matches.
	EffectAllow Effect = "allow"
	// EffectDeny refuses access when the policy matches; it takes precedence
	// over any matching allow policy.
	EffectDeny Effect = "deny"
)

// Policy is one authorization rule published by an organization.
//
// A policy identifies a single subject, a single action and a scope path,
// and either allows or denies the request. When Recursive is true the policy
// also covers every scope below Scope; otherwise it covers only Scope itself.
type Policy struct {
	ID        string
	Subject   string
	Action    string
	Scope     string
	Effect    Effect
	Recursive bool
}

// Version is an immutable, complete snapshot of an organization's published
// policy set at a point in time.
type Version struct {
	Number   int
	Policies []Policy
}

// Distinguishable failure reasons returned in Decision.Reason.
const (
	ReasonMissingOrganization = "missing organization"
	ReasonMissingSubjectOrg   = "missing subject organization"
	ReasonMissingResourceOrg  = "missing resource organization"
	ReasonSubjectOrgMismatch  = "subject organization does not match decision organization"
	ReasonResourceOrgMismatch = "resource organization does not match decision organization"
	ReasonMissingSubject      = "missing subject identifier"
	ReasonMissingResource     = "missing resource identifier"
	ReasonMissingAction       = "missing action"
	ReasonInvalidScope        = "invalid scope"
	ReasonSubjectDisabled     = "subject is disabled"
	ReasonNoPublishedVersion  = "no published version"
	ReasonVersionNotFound     = "version not found"
	ReasonDeniedByPolicy      = "denied by policy"
	ReasonAllowedByPolicy     = "allowed by policy"
	ReasonNoMatchingPolicy    = "no matching policy"
)

// Sentinel errors returned by Store operations. Callers may compare with
// errors.Is to distinguish validation, conflict and not-found failures.
var (
	// ErrInvalidOrganization is returned when the organization identifier is
	// empty.
	ErrInvalidOrganization = errors.New("invalid organization")
	// ErrInvalidPolicy is returned when a published policy set fails
	// validation.
	ErrInvalidPolicy = errors.New("invalid policy")
	// ErrVersionConflict is returned when a publish or rollback carries an
	// expected version that does not match the organization's current version.
	ErrVersionConflict = errors.New("version conflict")
	// ErrVersionNotFound is returned when a requested historical version does
	// not exist.
	ErrVersionNotFound = errors.New("version not found")
)

// Store holds the per-organization policy versions for the lifetime of the
// service instance. It is safe for concurrent use.
//
// Each organization manages its own policy set and version sequence. Different
// organizations may use the same subject, resource and policy identifiers
// without interfering with one another.
type Store struct {
	mu   sync.Mutex
	orgs map[string]*orgStore
}

type orgStore struct {
	mu       sync.RWMutex
	current  int
	versions map[int]*Version
}

// NewStore returns an empty store. Organizations are created lazily on first
// publish; a new organization starts at version 0 and cannot be used for
// decisions until its first successful publish.
func NewStore() *Store {
	return &Store{orgs: make(map[string]*orgStore)}
}

func (s *Store) orgStore(org string) *orgStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[org]
	if !ok {
		o = &orgStore{versions: make(map[int]*Version)}
		s.orgs[org] = o
	}
	return o
}

// Publish validates policies and, only when expectedVersion matches the
// organization's current version, publishes the complete set as a new version.
// It returns the new version number.
//
// Validation failures (duplicate or empty policy identifiers, invalid scope
// paths, missing subject or action, unknown effect) reject the whole publish:
// the current version and history are left untouched and no version number is
// consumed. An empty policy set is valid and publishes successfully; its
// decisions are all denied.
//
// Publish copies policies, so later mutation of the caller's slice does not
// affect published history.
func (s *Store) Publish(org string, expectedVersion int, policies []Policy) (int, error) {
	if org == "" {
		return 0, fmt.Errorf("%w: organization is required", ErrInvalidOrganization)
	}
	if err := validatePolicies(policies); err != nil {
		return 0, err
	}
	o := s.orgStore(org)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current != expectedVersion {
		return 0, fmt.Errorf("%w: current version is %d, caller expected %d", ErrVersionConflict, o.current, expectedVersion)
	}
	num := o.current + 1
	o.versions[num] = &Version{Number: num, Policies: clonePolicies(policies)}
	o.current = num
	return num, nil
}

// Rollback publishes the complete content of targetVersion (a version already
// present in this organization's history) as a new version. History is never
// rewritten or deleted. It returns the new version number.
//
// expectedVersion must match the organization's current version for the
// rollback to succeed. A target that does not exist returns ErrVersionNotFound
// without changing the current version or history.
func (s *Store) Rollback(org string, expectedVersion, targetVersion int) (int, error) {
	if org == "" {
		return 0, fmt.Errorf("%w: organization is required", ErrInvalidOrganization)
	}
	if targetVersion <= 0 {
		return 0, fmt.Errorf("%w: target version %d does not exist", ErrVersionNotFound, targetVersion)
	}
	o := s.orgStore(org)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current != expectedVersion {
		return 0, fmt.Errorf("%w: current version is %d, caller expected %d", ErrVersionConflict, o.current, expectedVersion)
	}
	target, ok := o.versions[targetVersion]
	if !ok {
		return 0, fmt.Errorf("%w: target version %d does not exist", ErrVersionNotFound, targetVersion)
	}
	num := o.current + 1
	o.versions[num] = &Version{Number: num, Policies: clonePolicies(target.Policies)}
	o.current = num
	return num, nil
}

// CurrentVersion returns the organization's current version. A new
// organization that has never published is at version 0.
func (s *Store) CurrentVersion(org string) (int, error) {
	if org == "" {
		return 0, fmt.Errorf("%w: organization is required", ErrInvalidOrganization)
	}
	o := s.orgStore(org)
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.current, nil
}

// Policies returns a copy of the complete policy set of the given version in
// this organization's history. The returned slice is a copy: later mutation by
// the caller does not change published history.
func (s *Store) Policies(org string, version int) ([]Policy, error) {
	if org == "" {
		return nil, fmt.Errorf("%w: organization is required", ErrInvalidOrganization)
	}
	o := s.orgStore(org)
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.versions[version]
	if !ok {
		return nil, fmt.Errorf("%w: version %d does not exist", ErrVersionNotFound, version)
	}
	return clonePolicies(v.Policies), nil
}

// Decide evaluates a request against the organization's currently published
// version. The request must carry the organization of the subject and of the
// resource, and both must match the decision organization; otherwise the
// request is denied even if an allow policy would match. Missing organization,
// subject, resource or action identifiers, and invalid scope paths, are denied
// with distinguishable reasons. A disabled subject is denied.
//
// The subject's own roles never grant access under a published policy set.
// Default deny: a matching deny policy takes precedence over any matching
// allow policy. The decision records the actual version used and lists every
// matching policy identifier, deduplicated and sorted lexicographically.
func (s *Store) Decide(org string, subject Subject, resource Resource, action string) Decision {
	o := s.orgStore(org)
	o.mu.RLock()
	version := o.current
	o.mu.RUnlock()
	return s.evaluate(org, version, subject, resource, action)
}

// Review re-evaluates a request against a specific historical version of the
// organization. Later publishes or rollbacks do not change its conclusion,
// reason or matched list. A version that does not exist is denied with
// reason VersionNotFound; the current version is never used as a fallback.
func (s *Store) Review(org string, version int, subject Subject, resource Resource, action string) Decision {
	if org == "" {
		return Decision{Allowed: false, Reason: ReasonMissingOrganization}
	}
	if version <= 0 {
		return Decision{Allowed: false, Reason: fmt.Sprintf("%s: version %d", ReasonVersionNotFound, version), Version: version}
	}
	o := s.orgStore(org)
	o.mu.RLock()
	_, ok := o.versions[version]
	o.mu.RUnlock()
	if !ok {
		return Decision{Allowed: false, Reason: fmt.Sprintf("%s: version %d", ReasonVersionNotFound, version), Version: version}
	}
	return s.evaluate(org, version, subject, resource, action)
}

func (s *Store) evaluate(org string, version int, subject Subject, resource Resource, action string) Decision {
	if org == "" {
		return Decision{Allowed: false, Reason: ReasonMissingOrganization, Version: version}
	}
	if subject.Org == "" {
		return Decision{Allowed: false, Reason: ReasonMissingSubjectOrg, Version: version}
	}
	if resource.Org == "" {
		return Decision{Allowed: false, Reason: ReasonMissingResourceOrg, Version: version}
	}
	if subject.Org != org {
		return Decision{Allowed: false, Reason: ReasonSubjectOrgMismatch, Version: version}
	}
	if resource.Org != org {
		return Decision{Allowed: false, Reason: ReasonResourceOrgMismatch, Version: version}
	}
	if subject.ID == "" {
		return Decision{Allowed: false, Reason: ReasonMissingSubject, Version: version}
	}
	if resource.ID == "" {
		return Decision{Allowed: false, Reason: ReasonMissingResource, Version: version}
	}
	if action == "" {
		return Decision{Allowed: false, Reason: ReasonMissingAction, Version: version}
	}
	if !validScopePath(resource.Scope) {
		return Decision{Allowed: false, Reason: fmt.Sprintf("%s: %q", ReasonInvalidScope, resource.Scope), Version: version}
	}
	if subject.Disabled {
		return Decision{Allowed: false, Reason: ReasonSubjectDisabled, Version: version}
	}
	o := s.orgStore(org)
	o.mu.RLock()
	v, ok := o.versions[version]
	var policies []Policy
	if ok {
		policies = clonePolicies(v.Policies)
	}
	o.mu.RUnlock()
	if !ok {
		return Decision{Allowed: false, Reason: ReasonNoPublishedVersion, Version: version}
	}
	matched, hasAllow, hasDeny := matchPolicies(policies, subject.ID, action, resource.Scope)
	if hasDeny {
		return Decision{Allowed: false, Reason: ReasonDeniedByPolicy, Matched: matched, Version: version}
	}
	if hasAllow {
		return Decision{Allowed: true, Reason: ReasonAllowedByPolicy, Matched: matched, Version: version}
	}
	return Decision{Allowed: false, Reason: ReasonNoMatchingPolicy, Matched: matched, Version: version}
}

// matchPolicies returns the sorted, deduplicated identifiers of every policy
// matching the request, and whether any of them allows or denies it.
func matchPolicies(policies []Policy, subjectID, action, scope string) (matched []string, hasAllow, hasDeny bool) {
	seen := make(map[string]bool)
	for _, p := range policies {
		if p.Subject != subjectID || p.Action != action {
			continue
		}
		if !scopeCovers(p.Scope, scope, p.Recursive) {
			continue
		}
		if !seen[p.ID] {
			seen[p.ID] = true
			matched = append(matched, p.ID)
		}
		switch p.Effect {
		case EffectAllow:
			hasAllow = true
		case EffectDeny:
			hasDeny = true
		}
	}
	sort.Strings(matched)
	return matched, hasAllow, hasDeny
}

// scopeCovers reports whether resourceScope is covered by policyScope. When
// recursive is false the scopes must be equal. When recursive is true the
// policy also covers every scope below policyScope: path segments are matched
// between slashes, so "org/a" covers "org/a" and "org/a/b" but not "org/ab".
func scopeCovers(policyScope, resourceScope string, recursive bool) bool {
	if policyScope == resourceScope {
		return true
	}
	return recursive && strings.HasPrefix(resourceScope, policyScope+"/")
}

// validScopePath reports whether p is a valid scope path. A path must be
// non-empty, must not start or end with a slash, must not contain consecutive
// slashes, and must not contain "." or ".." segments.
func validScopePath(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// validatePolicies checks the complete policy set of a publish.
func validatePolicies(policies []Policy) error {
	seen := make(map[string]bool)
	for i, p := range policies {
		if p.ID == "" {
			return fmt.Errorf("%w: policy at index %d has empty identifier", ErrInvalidPolicy, i)
		}
		if seen[p.ID] {
			return fmt.Errorf("%w: duplicate policy identifier %q", ErrInvalidPolicy, p.ID)
		}
		seen[p.ID] = true
		if p.Subject == "" {
			return fmt.Errorf("%w: policy %q has empty subject", ErrInvalidPolicy, p.ID)
		}
		if p.Action == "" {
			return fmt.Errorf("%w: policy %q has empty action", ErrInvalidPolicy, p.ID)
		}
		if !validScopePath(p.Scope) {
			return fmt.Errorf("%w: policy %q has invalid scope %q", ErrInvalidPolicy, p.ID, p.Scope)
		}
		if p.Effect != EffectAllow && p.Effect != EffectDeny {
			return fmt.Errorf("%w: policy %q has unknown effect %q", ErrInvalidPolicy, p.ID, p.Effect)
		}
	}
	return nil
}

func clonePolicies(in []Policy) []Policy {
	if in == nil {
		return nil
	}
	out := make([]Policy, len(in))
	copy(out, in)
	return out
}
