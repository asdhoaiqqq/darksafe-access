// This file implements organization-scoped policy publishing, rollback,
// and review on top of the decision core. Each organization versions its
// own policy set independently; all state is in-memory and lives until
// the service instance ends.
package darksafe

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Effect is the outcome a policy produces when it matches a request.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Policy binds one subject and one action to a scope with an effect.
type Policy struct {
	ID        string
	Subject   string
	Action    string
	Scope     string
	Effect    Effect
	Recursive bool // false: only the exact scope; true: the scope and its descendants
}

// Sentinel errors so callers can distinguish failure causes with errors.Is.
var (
	// ErrVersionConflict means the caller's expected current version is stale.
	ErrVersionConflict = errors.New("darksafe: version conflict")
	// ErrVersionNotFound means the requested historical version does not exist.
	ErrVersionNotFound = errors.New("darksafe: version not found")
	// ErrInvalidPolicySet means the submitted policy set failed validation.
	ErrInvalidPolicySet = errors.New("darksafe: invalid policy set")
	// ErrMissingOrganization means an operation was called without an organization.
	ErrMissingOrganization = errors.New("darksafe: organization is required")
)

// OrgRequest is an access request evaluated against one organization's
// published policies. Both organizations must equal the decision organization.
type OrgRequest struct {
	SubjectOrg  string
	ResourceOrg string
	Subject     Subject
	Resource    Resource
	Action      string
}

// Store holds every organization's published policy versions. It is safe
// for concurrent use and keeps all state in memory.
type Store struct {
	mu   sync.Mutex
	orgs map[string]*orgState
}

type orgState struct {
	current  int              // 0 means nothing published yet
	versions map[int][]Policy // immutable snapshots per version
	audit    []*AuditRecord   // organization-local, 1-based consecutive chain
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{orgs: make(map[string]*orgState)}
}

func (s *Store) orgLocked(name string) *orgState {
	st, ok := s.orgs[name]
	if !ok {
		st = &orgState{versions: make(map[int][]Policy)}
		s.orgs[name] = st
	}
	return st
}

// CurrentVersion reports the organization's current version, 0 if it has
// never published.
func (s *Store) CurrentVersion(org string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.orgs[org]; ok {
		return st.current
	}
	return 0
}

// Publish validates and publishes a complete policy set as the next
// version. expectedVersion must equal the organization's current version;
// on any validation or version failure nothing changes and no version
// number is consumed.
func (s *Store) Publish(org string, expectedVersion int, policies []Policy) (int, error) {
	if org == "" {
		return 0, ErrMissingOrganization
	}
	snapshot, err := validatePolicies(policies)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgLocked(org)
	if expectedVersion != st.current {
		return 0, fmt.Errorf("%w: expected %d, current is %d", ErrVersionConflict, expectedVersion, st.current)
	}
	st.current++
	st.versions[st.current] = snapshot
	s.appendRecordLocked(st, &AuditRecord{
		Org: org, Category: AuditPolicyChange,
		Version: st.current, Policies: clonePolicies(snapshot),
	})
	return st.current, nil
}

// Rollback republishes the full content of an existing historical version
// as a new version. History is never deleted or rewritten.
func (s *Store) Rollback(org string, expectedVersion, targetVersion int) (int, error) {
	if org == "" {
		return 0, ErrMissingOrganization
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgLocked(org)
	src, ok := st.versions[targetVersion]
	if !ok {
		return 0, fmt.Errorf("%w: organization %q has no version %d", ErrVersionNotFound, org, targetVersion)
	}
	if expectedVersion != st.current {
		return 0, fmt.Errorf("%w: expected %d, current is %d", ErrVersionConflict, expectedVersion, st.current)
	}
	st.current++
	st.versions[st.current] = append([]Policy(nil), src...)
	s.appendRecordLocked(st, &AuditRecord{
		Org: org, Category: AuditPolicyChange,
		Version: st.current, SourceVersion: targetVersion,
		Policies: clonePolicies(src),
	})
	return st.current, nil
}

// Policies returns a copy of the full policy set of one version. Mutating
// the result does not affect published content.
func (s *Store) Policies(org string, version int) ([]Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.orgs[org]
	if !ok {
		return nil, fmt.Errorf("%w: organization %q has no version %d", ErrVersionNotFound, org, version)
	}
	src, ok := st.versions[version]
	if !ok {
		return nil, fmt.Errorf("%w: organization %q has no version %d", ErrVersionNotFound, org, version)
	}
	return append([]Policy(nil), src...), nil
}

// Decide evaluates a request against the organization's current version.
// The whole decision uses that single version's snapshot, and the version
// actually used is reported in Decision.Version. Every decision that
// names a decision organization is appended to the audit chain,
// including denials; a missing decision org is rejected as before and
// leaves no record.
func (s *Store) Decide(org string, req OrgRequest) Decision {
	if d, ok := checkRequest(org, req); !ok {
		if org != "" {
			s.appendDecisionRecord(org, req, d)
		}
		return d
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgLocked(org)
	version := st.current
	var policies []Policy
	if version > 0 {
		policies = append([]Policy(nil), st.versions[version]...)
	}
	var d Decision
	if version == 0 {
		d = Decision{Allowed: false, Reason: "organization has no published version"}
	} else {
		d = evaluate(req, policies, version)
	}
	s.appendRecordLocked(st, &AuditRecord{
		Org: org, Category: AuditDecision,
		Version:  version,
		Request:  cloneRequest(req),
		Decision: cloneDecision(d),
	})
	return d
}

// appendDecisionRecord appends an early-rejection decision (version 0)
// to the organization's audit chain.
func (s *Store) appendDecisionRecord(org string, req OrgRequest, d Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgLocked(org)
	s.appendRecordLocked(st, &AuditRecord{
		Org: org, Category: AuditDecision,
		Version:  0,
		Request:  cloneRequest(req),
		Decision: cloneDecision(d),
	})
}

// Review re-evaluates a request against a specific historical version.
// Later publishes or rollbacks cannot change its outcome; a version that
// does not exist is a denial with a reason, never a silent fallback to
// the current version.
func (s *Store) Review(org string, version int, req OrgRequest) Decision {
	if d, ok := checkRequest(org, req); !ok {
		return d
	}
	s.mu.Lock()
	var policies []Policy
	found := false
	if st, ok := s.orgs[org]; ok {
		if src, ok := st.versions[version]; ok {
			policies = append([]Policy(nil), src...)
			found = true
		}
	}
	s.mu.Unlock()
	if !found {
		return Decision{Allowed: false, Reason: fmt.Sprintf("version %d not found", version)}
	}
	return evaluate(req, policies, version)
}

// checkRequest validates the request envelope. Every failure is a denial
// with a distinguishable reason.
func checkRequest(org string, req OrgRequest) (Decision, bool) {
	switch {
	case org == "":
		return Decision{Allowed: false, Reason: "missing decision organization"}, false
	case req.SubjectOrg == "":
		return Decision{Allowed: false, Reason: "missing subject organization"}, false
	case req.ResourceOrg == "":
		return Decision{Allowed: false, Reason: "missing resource organization"}, false
	case req.Subject.ID == "":
		return Decision{Allowed: false, Reason: "missing subject id"}, false
	case req.Resource.ID == "":
		return Decision{Allowed: false, Reason: "missing resource id"}, false
	case req.Action == "":
		return Decision{Allowed: false, Reason: "missing action"}, false
	}
	if req.SubjectOrg != org || req.ResourceOrg != org {
		return Decision{Allowed: false, Reason: "organization mismatch"}, false
	}
	if err := validateScope(req.Resource.Scope); err != nil {
		return Decision{Allowed: false, Reason: "invalid scope: " + err.Error()}, false
	}
	if req.Subject.Disabled {
		return Decision{Allowed: false, Reason: "subject is disabled"}, false
	}
	return Decision{}, true
}

// evaluate applies one version's policies to a request. Subject roles are
// deliberately ignored: only the selected version's policies decide.
func evaluate(req OrgRequest, policies []Policy, version int) Decision {
	seen := make(map[string]struct{})
	denied := false
	allowed := false
	for _, p := range policies {
		if p.Subject != req.Subject.ID || p.Action != req.Action {
			continue
		}
		if !scopeMatches(p, req.Resource.Scope) {
			continue
		}
		seen[p.ID] = struct{}{}
		switch p.Effect {
		case EffectDeny:
			denied = true
		case EffectAllow:
			allowed = true
		}
	}
	matched := make([]string, 0, len(seen))
	for id := range seen {
		matched = append(matched, id)
	}
	sort.Strings(matched)
	switch {
	case denied:
		return Decision{Allowed: false, Reason: "matched deny policy", Matched: matched, Version: version}
	case allowed:
		return Decision{Allowed: true, Reason: "matched allow policy", Matched: matched, Version: version}
	default:
		return Decision{Allowed: false, Reason: "no matching allow policy", Matched: matched, Version: version}
	}
}

// scopeMatches reports whether a policy's scope covers the request scope.
// Matching is by slash-separated segments, so "org/a" never covers "org/ab".
func scopeMatches(p Policy, scope string) bool {
	if p.Scope == scope {
		return true
	}
	return p.Recursive && strings.HasPrefix(scope, p.Scope+"/")
}

// validatePolicies checks a submitted policy set and returns a detached
// copy, so later caller mutations cannot affect published content.
func validatePolicies(policies []Policy) ([]Policy, error) {
	seen := make(map[string]struct{}, len(policies))
	out := make([]Policy, len(policies))
	for i, p := range policies {
		if p.ID == "" {
			return nil, fmt.Errorf("%w: policy at index %d has an empty id", ErrInvalidPolicySet, i)
		}
		if _, dup := seen[p.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate policy id %q", ErrInvalidPolicySet, p.ID)
		}
		seen[p.ID] = struct{}{}
		if p.Subject == "" {
			return nil, fmt.Errorf("%w: policy %q is missing a subject", ErrInvalidPolicySet, p.ID)
		}
		if p.Action == "" {
			return nil, fmt.Errorf("%w: policy %q is missing an action", ErrInvalidPolicySet, p.ID)
		}
		if p.Effect != EffectAllow && p.Effect != EffectDeny {
			return nil, fmt.Errorf("%w: policy %q has unknown effect %q", ErrInvalidPolicySet, p.ID, p.Effect)
		}
		if err := validateScope(p.Scope); err != nil {
			return nil, fmt.Errorf("%w: policy %q has invalid scope: %v", ErrInvalidPolicySet, p.ID, err)
		}
		out[i] = p
	}
	return out, nil
}

// validateScope rejects empty paths, leading/trailing slashes, consecutive
// slashes, and "." or ".." segments.
func validateScope(scope string) error {
	if scope == "" {
		return errors.New("scope is empty")
	}
	if strings.HasPrefix(scope, "/") || strings.HasSuffix(scope, "/") {
		return fmt.Errorf("scope %q has a leading or trailing slash", scope)
	}
	for _, seg := range strings.Split(scope, "/") {
		if seg == "" {
			return fmt.Errorf("scope %q contains consecutive slashes", scope)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("scope %q contains invalid segment %q", scope, seg)
		}
	}
	return nil
}
