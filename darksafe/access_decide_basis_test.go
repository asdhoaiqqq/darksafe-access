// Regression tests pinning down the two separate authorization bases:
// the demo entry point Access trusts the subject's roles, while the
// organization-level Store.Decide evaluates only the decision
// organization's published policies and ignores roles entirely. The same
// subject may legitimately get different answers from the two; these
// tests fail if a role ever leaks into policy evaluation or a published
// policy ever leaks into the demo core.
package darksafe

import (
	"reflect"
	"testing"
)

// The shared scenario: one enabled service subject reading one ledger,
// with every identifier complete and all three organizations equal.
const (
	basisOrg        = "acme factory"
	basisLedgerID   = "ledger-2026"
	basisLedgerScope = "acme/factory/ledger"
	basisSubjectID  = "svc-audit-reader"
	basisAction     = "read"
	basisPolicyID   = "p-ledger-read-2026"
)

func basisSubject(roles ...string) Subject {
	return Subject{ID: basisSubjectID, Kind: "service", Roles: roles}
}

func basisLedger() Resource {
	return Resource{ID: basisLedgerID, Scope: basisLedgerScope}
}

func basisRequest(subject Subject) OrgRequest {
	return OrgRequest{
		SubjectOrg:  basisOrg,
		ResourceOrg: basisOrg,
		Subject:     subject,
		Resource:    basisLedger(),
		Action:      basisAction,
	}
}

// publishBasisPolicy publishes the single allow policy matching the
// scenario and returns its version (1 on a fresh store).
func publishBasisPolicy(t *testing.T, s *Store) int {
	t.Helper()
	v, err := s.Publish(basisOrg, 0, []Policy{{
		ID:      basisPolicyID,
		Subject: basisSubjectID,
		Action:  basisAction,
		Scope:   basisLedgerScope,
		Effect:  EffectAllow,
	}})
	if err != nil {
		t.Fatalf("publish allow policy: %v", err)
	}
	return v
}

// wantDecision fails the test unless got equals want in every field:
// allowed, reason, matched list and version. Comparing only Allowed
// would miss a change in the authorization basis.
func wantDecision(t *testing.T, label string, got, want Decision) {
	t.Helper()
	if got.Allowed != want.Allowed {
		t.Errorf("%s: allowed = %v, want %v", label, got.Allowed, want.Allowed)
	}
	if got.Reason != want.Reason {
		t.Errorf("%s: reason = %q, want %q", label, got.Reason, want.Reason)
	}
	if !reflect.DeepEqual(got.Matched, want.Matched) {
		t.Errorf("%s: matched = %v, want %v", label, got.Matched, want.Matched)
	}
	if got.Version != want.Version {
		t.Errorf("%s: version = %d, want %d", label, got.Version, want.Version)
	}
}

// Access grants the read from roles alone; a fresh Store denies the same
// read because nothing is published. The two Matched lists are different
// kinds of records (role names vs policy IDs) and version 0 on the two
// sides must not be read as one shared denial marker.
func TestAccessAndDecideAreSeparateBases(t *testing.T) {
	store := NewStore()

	// The demo core: owner grants the read, the matched entry is the role.
	wantDecision(t, "access with owner role",
		Access(basisSubject("owner"), basisLedger(), basisAction),
		Decision{
			Allowed: true,
			Reason:  "role grants read:acme/factory/ledger",
			Matched: []string{"owner"},
			Version: 0,
		})

	// The scope:action role grants it too, again matched as a role name.
	wantDecision(t, "access with scope role",
		Access(basisSubject("acme/factory/ledger:read"), basisLedger(), basisAction),
		Decision{
			Allowed: true,
			Reason:  "role grants read:acme/factory/ledger",
			Matched: []string{"acme/factory/ledger:read"},
			Version: 0,
		})

	// The organization level: the identical subject, ledger, action and
	// organizations, but no published policy yet. Roles — even owner —
	// play no part; the denial says exactly that no version is published.
	for _, roles := range [][]string{{"owner"}, {"acme/factory/ledger:read"}, nil} {
		wantDecision(t, "decide with no published policy",
			store.Decide(basisOrg, basisRequest(basisSubject(roles...))),
			Decision{
				Allowed: false,
				Reason:  "organization has no published version",
				Matched: nil,
				Version: 0,
			})
	}
}

// With a matching allow policy published, Decide allows a subject that
// carries no roles at all and reports the policy ID and the published
// version; Access denies the same read for lack of a granting role,
// unaffected by the published policy.
func TestPublishedPolicyDoesNotFeedAccessAndRolesDoNotFeedDecide(t *testing.T) {
	store := NewStore()
	version := publishBasisPolicy(t, store)

	// No roles on the subject: the organization level still allows, and
	// the matched entry is the policy identifier, not a role.
	wantDecision(t, "decide without roles",
		store.Decide(basisOrg, basisRequest(basisSubject())),
		Decision{
			Allowed: true,
			Reason:  "matched allow policy",
			Matched: []string{basisPolicyID},
			Version: version,
		})

	// The demo core never reads published policies: with no granting
	// role the same read is denied even though version 1 would allow it.
	wantDecision(t, "access without roles",
		Access(basisSubject(), basisLedger(), basisAction),
		Decision{
			Allowed: false,
			Reason:  "no role grants read:acme/factory/ledger",
			Matched: nil,
			Version: 0,
		})
}

// Changing only the roles carried by the request must not change any
// field of the organization-level decision: allowed, reason, matched
// list and version all stay exactly as the role-less baseline.
func TestDecideIsInvariantToRequestRoles(t *testing.T) {
	store := NewStore()
	publishBasisPolicy(t, store)

	baseline := store.Decide(basisOrg, basisRequest(basisSubject()))
	if !baseline.Allowed {
		t.Fatalf("baseline decision = %+v, want allowed", baseline)
	}
	variants := map[string][]string{
		"owner role":        {"owner"},
		"scope role":        {"acme/factory/ledger:read"},
		"unrelated roles":   {"billing:write", "admin"},
		"nil roles":         nil,
		"empty role list":   {},
		"owner plus others": {"owner", "acme/factory/ledger:read", "admin"},
	}
	for name, roles := range variants {
		got := store.Decide(basisOrg, basisRequest(basisSubject(roles...)))
		if !reflect.DeepEqual(got, baseline) {
			t.Errorf("%s: decision = %+v, want unchanged %+v", name, got, baseline)
		}
	}
}

// A disabled subject is denied by both entry points with the same
// envelope reason; neither a granting role nor a matching allow policy
// can override it, and Decide evaluates no policy (version 0, no match).
func TestDisabledSubjectDeniedOnBothBases(t *testing.T) {
	store := NewStore()
	publishBasisPolicy(t, store)

	disabled := basisSubject("owner")
	disabled.Disabled = true

	wantDecision(t, "access with disabled subject",
		Access(disabled, basisLedger(), basisAction),
		Decision{Allowed: false, Reason: "subject is disabled", Matched: nil, Version: 0})

	wantDecision(t, "decide with disabled subject",
		store.Decide(basisOrg, basisRequest(disabled)),
		Decision{Allowed: false, Reason: "subject is disabled", Matched: nil, Version: 0})
}

// A subject or resource organization different from the decision
// organization denies before any policy is evaluated, even though a
// matching allow policy is published: no matched entry, version 0.
func TestDecideOrgMismatchDeniesBeforePolicies(t *testing.T) {
	store := NewStore()
	publishBasisPolicy(t, store)

	for name, mutate := range map[string]func(*OrgRequest){
		"subject org mismatch":  func(r *OrgRequest) { r.SubjectOrg = "other org" },
		"resource org mismatch": func(r *OrgRequest) { r.ResourceOrg = "other org" },
	} {
		req := basisRequest(basisSubject("owner"))
		mutate(&req)
		wantDecision(t, name, store.Decide(basisOrg, req),
			Decision{Allowed: false, Reason: "organization mismatch", Matched: nil, Version: 0})
	}
}

// Every Decide with a non-empty organization — allow or denial — appends
// a decision record that replays exactly what was returned, while Access
// calls leave the organization's audit chain and current version alone.
func TestDecideAuditsEveryOutcomeAndAccessStaysOutOfTheChain(t *testing.T) {
	store := NewStore()
	version := publishBasisPolicy(t, store)

	// One Decide per outcome class, including the envelope rejections.
	type step struct {
		name string
		req  OrgRequest
	}
	disabled := basisRequest(basisSubject("owner"))
	disabled.Subject.Disabled = true
	crossSubject := basisRequest(basisSubject("owner"))
	crossSubject.SubjectOrg = "other org"
	crossResource := basisRequest(basisSubject("owner"))
	crossResource.ResourceOrg = "other org"
	steps := []step{
		{"allowed by policy", basisRequest(basisSubject())},
		{"disabled subject", disabled},
		{"subject org mismatch", crossSubject},
		{"resource org mismatch", crossResource},
	}

	// A second organization with nothing published contributes the
	// no-published-version denial; its chain is independent.
	const otherOrg = "globex workshop"
	otherReq := basisRequest(basisSubject())
	otherReq.SubjectOrg = otherOrg
	otherReq.ResourceOrg = otherOrg

	returned := make([]Decision, 0, len(steps)+1)
	for _, st := range steps {
		returned = append(returned, store.Decide(basisOrg, st.req))
	}
	if returned[0].Version != version {
		t.Fatalf("allowed decision used version %d, want the published %d", returned[0].Version, version)
	}
	otherDecision := store.Decide(otherOrg, otherReq)
	wantDecision(t, "decide in org without published policy", otherDecision,
		Decision{Allowed: false, Reason: "organization has no published version", Matched: nil, Version: 0})

	// Snapshot the chain head and current version, then exercise Access
	// repeatedly: none of it may reach the organization's state.
	recordsBefore, cpBefore, err := store.AuditExport(basisOrg, 0)
	if err != nil {
		t.Fatalf("export before Access calls: %v", err)
	}
	versionBefore := store.CurrentVersion(basisOrg)
	Access(basisSubject("owner"), basisLedger(), basisAction)
	Access(basisSubject(), basisLedger(), basisAction)
	Access(basisSubject("acme/factory/ledger:read"), basisLedger(), "write")
	if got := store.CurrentVersion(basisOrg); got != versionBefore {
		t.Fatalf("current version changed by Access calls: %d, want %d", got, versionBefore)
	}
	recordsAfter, cpAfter, err := store.AuditExport(basisOrg, 0)
	if err != nil {
		t.Fatalf("export after Access calls: %v", err)
	}
	if !reflect.DeepEqual(recordsAfter, recordsBefore) || cpAfter != cpBefore {
		t.Fatalf("Access calls altered the audit chain: before (%d records, %+v), after (%d records, %+v)",
			len(recordsBefore), cpBefore, len(recordsAfter), cpAfter)
	}

	// The chain holds the publish record plus one decision record per
	// Decide, in order, each replaying the returned decision and request.
	if len(recordsAfter) != 1+len(steps) {
		t.Fatalf("audit chain has %d records, want %d (1 publish + %d decisions)",
			len(recordsAfter), 1+len(steps), len(steps))
	}
	if recordsAfter[0].Kind != AuditPolicyChange {
		t.Fatalf("record 1 kind = %q, want %q", recordsAfter[0].Kind, AuditPolicyChange)
	}
	for i, st := range steps {
		rec := recordsAfter[i+1]
		if rec.Kind != AuditDecision || rec.Decision == nil {
			t.Fatalf("%s: record %d is %q, want a decision record", st.name, rec.Seq, rec.Kind)
		}
		if !reflect.DeepEqual(rec.Decision.Decision, returned[i]) {
			t.Errorf("%s: recorded decision = %+v, want the returned %+v",
				st.name, rec.Decision.Decision, returned[i])
		}
		if !reflect.DeepEqual(rec.Decision.Request, st.req) {
			t.Errorf("%s: recorded request = %+v, want %+v", st.name, rec.Decision.Request, st.req)
		}
	}
	if err := VerifyAudit(basisOrg, recordsAfter, cpAfter); err != nil {
		t.Fatalf("audit chain does not verify: %v", err)
	}

	// The other organization's denial was recorded on its own chain.
	otherRecords, otherCp, err := store.AuditExport(otherOrg, 0)
	if err != nil {
		t.Fatalf("export other org: %v", err)
	}
	if len(otherRecords) != 1 || otherRecords[0].Kind != AuditDecision ||
		!reflect.DeepEqual(otherRecords[0].Decision.Decision, otherDecision) {
		t.Fatalf("other org chain = %+v, want exactly the recorded denial", otherRecords)
	}
	if err := VerifyAudit(otherOrg, otherRecords, otherCp); err != nil {
		t.Fatalf("other org chain does not verify: %v", err)
	}
}
