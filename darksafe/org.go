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
	// ResourceID optionally narrows the policy to one exact resource. Empty
	// keeps the legacy scope-only matching. A non-empty value adds a
	// condition on top of subject, action, scope and the organization
	// checks: it never replaces them. Comparison is against the request's
	// raw resource ID, byte for byte: case sensitive, spaces significant,
	// no wildcard or scope-path interpretation, and the resource needs no
	// prior registration.
	//
	// The omitempty tag is load bearing for audit fingerprints: the legacy
	// JSON fingerprint family never had this field, and an empty value must
	// keep serializing to exactly the old bytes. policyFields carries the
	// matching omitEmptyInFingerprint marker so the raw-byte fingerprint
	// family stays in sync.
	ResourceID string `json:",omitempty"`
}

// policyField describes one content field of Policy: how to read it and
// how to write it back. policyFields is the single definition of which
// fields a policy carries and in what order the audit representations
// visit them — the raw-byte fingerprint encoder, the archive codec and the
// invalid-UTF-8 scan all derive their per-field handling from it, so
// changing Policy's content means editing this list once instead of
// keeping the fingerprint, the archive read/write and the UTF-8 check in
// sync by hand.
type policyField struct {
	name string
	// str reads a string field and setStr writes it back during archive
	// decode. Boolean fields use boolean/setBool instead; exactly one pair
	// is set per entry.
	str     func(*Policy) string
	setStr  func(*Policy, string)
	boolean func(*Policy) bool
	setBool func(*Policy, bool)
	// omitEmptyInFingerprint mirrors an omitempty JSON tag: the fingerprint
	// skips the field when its value is empty, so the legacy JSON and the
	// raw-byte fingerprint families agree on whether the field participates
	// at all. The archive always stores the field, empty or not.
	omitEmptyInFingerprint bool
}

// policyFields lists Policy's content fields in canonical order: identity,
// subject, action, scope, effect, recursion flag, resource restriction.
var policyFields = []policyField{
	{name: "ID", str: func(p *Policy) string { return p.ID }, setStr: func(p *Policy, v string) { p.ID = v }},
	{name: "Subject", str: func(p *Policy) string { return p.Subject }, setStr: func(p *Policy, v string) { p.Subject = v }},
	{name: "Action", str: func(p *Policy) string { return p.Action }, setStr: func(p *Policy, v string) { p.Action = v }},
	{name: "Scope", str: func(p *Policy) string { return p.Scope }, setStr: func(p *Policy, v string) { p.Scope = v }},
	{name: "Effect", str: func(p *Policy) string { return string(p.Effect) }, setStr: func(p *Policy, v string) { p.Effect = Effect(v) }},
	{name: "Recursive", boolean: func(p *Policy) bool { return p.Recursive }, setBool: func(p *Policy, v bool) { p.Recursive = v }},
	{name: "ResourceID", str: func(p *Policy) string { return p.ResourceID }, setStr: func(p *Policy, v string) { p.ResourceID = v }, omitEmptyInFingerprint: true},
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
	audit    []*AuditRecord   // gapless, append-only audit chain
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
	return st.commitVersionLocked(org, snapshot, 0, false), nil
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
	snapshot := append([]Policy(nil), src...)
	return st.commitVersionLocked(org, snapshot, targetVersion, true), nil
}

// commitVersionLocked advances the organization to the next version with
// the given policy snapshot and appends the matching policy-change record
// to the audit chain. It runs under the store mutex, so the new version
// and its audit record become visible together; callers must have already
// validated the input and checked the expected version, so a failed
// operation never reaches this point and leaves no record. A rollback
// passes its source version and rolledBack=true; an ordinary publish
// passes 0 and false.
func (st *orgState) commitVersionLocked(org string, snapshot []Policy, sourceVersion int, rolledBack bool) int {
	st.current++
	st.versions[st.current] = snapshot
	st.appendAuditLocked(org, AuditPolicyChange, &PolicyChange{
		Version:       st.current,
		Policies:      snapshot,
		SourceVersion: sourceVersion,
		RolledBack:    rolledBack,
	}, nil)
	return st.current
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
// actually used is reported in Decision.Version.
//
// Every call with a non-empty decision organization leaves a decision
// record, including envelope rejections (disabled subject, missing fields,
// organization mismatch) and the no-published-version denial. A call with
// an empty organization rejects as before and creates no record, because
// there is no organization to attribute it to. The evaluation and record
// append are atomic: a concurrent publish can never make the recorded
// version differ from the version that was evaluated.
func (s *Store) Decide(org string, req OrgRequest) Decision {
	if org == "" {
		d, _ := checkRequest(org, req)
		return d
	}
	if d, ok := checkRequest(org, req); !ok {
		s.mu.Lock()
		st := s.orgLocked(org)
		st.appendAuditLocked(org, AuditDecision, nil, &DecisionRecord{Request: req, Decision: d})
		s.mu.Unlock()
		return d
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgLocked(org)
	version := st.current
	var d Decision
	if version == 0 {
		d = Decision{Allowed: false, Reason: "organization has no published version"}
	} else {
		d = evaluate(req, st.versions[version], version)
	}
	st.appendAuditLocked(org, AuditDecision, nil, &DecisionRecord{Request: req, Decision: d})
	return d
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
		// A resource restriction is an additional condition, not a
		// replacement for the scope check: even an equal ID cannot match a
		// resource outside the policy's scope (including outside a
		// recursive scope's subtree). The request envelope already
		// rejected an empty resource ID, so an empty ResourceID here means
		// "no restriction", never a match on a missing ID.
		if p.ResourceID != "" && p.ResourceID != req.Resource.ID {
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
