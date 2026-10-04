// This file is the regression guard pinning down that the two access entry
// points use two different authorization bases:
//
//   - Access (the demo core) decides solely from the subject's carried
//     roles: "owner" or "<scope>:<action>" grants. It never consults a
//     published policy, has no policy store, and reports the granting role
//     name in Decision.Matched with Version always 0.
//   - Store.Decide decides solely from the decision organization's currently
//     published policies. Subject roles, even "owner", never participate;
//     Decision.Matched carries policy identifiers and Version is the
//     published version actually evaluated (0 only when none was used).
//
// The two legitimately disagree about the same enabled service principal
// reading the same ledger, so every assertion below compares the full
// explanation — allowed, reason, matched entries and version — never the
// boolean alone. All material is generated in-process; the suite needs only
// the Go standard library and runs offline.
package darksafe

import (
	"reflect"
	"testing"
)

// Fixture values shared with examples/org_decision: one enabled service
// principal reading one ledger in one organization.
const (
	basisOrg        = "acme factory"
	basisLedgerID   = "ledger-2026"
	basisScope      = "acme/factory/ledger"
	basisSubjectID  = "svc-audit-reader"
	basisAction     = "read"
	basisPolicyID   = "p-ledger-read-2026"
	basisOwnerRole  = "owner"
	basisScopedRole = basisScope + ":" + basisAction // acme/factory/ledger:read
)

// basisSubject builds the enabled service principal with the given roles.
// No argument means no roles at all.
func basisSubject(roles ...string) Subject {
	return Subject{ID: basisSubjectID, Kind: "service", Roles: roles}
}

func basisLedger() Resource { return Resource{ID: basisLedgerID, Scope: basisScope} }

// basisRequest builds a complete, envelope-valid organization request:
// subject organization, resource organization and decision organization all
// agree and every identifier and the action are present.
func basisRequest(roles ...string) OrgRequest {
	return OrgRequest{
		SubjectOrg:  basisOrg,
		ResourceOrg: basisOrg,
		Subject:     basisSubject(roles...),
		Resource:    basisLedger(),
		Action:      basisAction,
	}
}

// basisAllowPolicy is the one matching allow policy: this subject, this
// action and the ledger's exact scope, with no resource qualifier.
func basisAllowPolicy() Policy {
	return allowPolicy(basisPolicyID, basisSubjectID, basisAction, basisScope, false)
}

// wantDecision compares the complete explanation an entry point must
// preserve. Matched order is significant (policy hits are documented as
// sorted); a nil list and a non-nil empty list are the same explanation,
// namely "nothing matched".
func wantDecision(t *testing.T, got Decision, allowed bool, reason string, matched []string, version int, context string) {
	t.Helper()
	if got.Allowed != allowed {
		t.Fatalf("%s: allowed = %v, want %v; decision = %+v", context, got.Allowed, allowed, got)
	}
	if got.Reason != reason {
		t.Fatalf("%s: reason = %q, want %q; decision = %+v", context, got.Reason, reason, got)
	}
	if got.Version != version {
		t.Fatalf("%s: version = %d, want %d; decision = %+v", context, got.Version, version, got)
	}
	gotMatched := got.Matched
	if len(gotMatched) == 0 {
		gotMatched = nil
	}
	wantMatched := matched
	if len(wantMatched) == 0 {
		wantMatched = nil
	}
	if !reflect.DeepEqual(gotMatched, wantMatched) {
		t.Fatalf("%s: matched = %q, want %q; decision = %+v", context, gotMatched, wantMatched, got)
	}
}

// TestAccessAndDecideUseDifferentAuthorizationBases drives the same enabled
// principal and ledger through both entry points before any policy is
// published. Access allows on a role while Decide denies for lack of a
// published version: the divergence is expected, and the two sides'
// explanations must not be read as the same kind of grant.
func TestAccessAndDecideUseDifferentAuthorizationBases(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		grant string // the role that actually grants the Access read
	}{
		{name: "owner role", roles: []string{basisOwnerRole}, grant: basisOwnerRole},
		{name: "ledger scoped read role", roles: []string{basisScopedRole}, grant: basisScopedRole},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject := basisSubject(tc.roles...)

			// Access: the carried role is the whole authorization basis. The
			// matched entry is the role that actually granted the read, and
			// version 0 means Access never evaluates a published policy.
			access := Access(subject, basisLedger(), basisAction)
			wantDecision(t, access,
				true, "role grants "+basisAction+":"+basisScope,
				[]string{tc.grant}, 0, "Access with granting role")

			// Decide on a fresh store: the envelope is valid (all three
			// organizations agree, identifiers complete) and the very same
			// roles ride along in the request, but no policy has been
			// published. Roles cannot stand in for one, so the request is
			// denied, the reason names the missing published version, the
			// matched list is empty and no version was used.
			store := NewStore()
			decide := store.Decide(basisOrg, basisRequest(tc.roles...))
			wantDecision(t, decide,
				false, "organization has no published version",
				nil, 0, "Decide before first publish")

			// The two version-0 answers are not one shared record: Access
			// allowed carrying a role name, Decide denied with an empty hit
			// list. A role name must never be reported as an org policy hit.
			if len(decide.Matched) != 0 {
				t.Fatalf("unpublished Decide must have no matched entries, got %q", decide.Matched)
			}
			if reflect.DeepEqual(access.Matched, decide.Matched) {
				t.Fatalf("Access role hit %q must not be interpretable as a Decide policy hit", access.Matched)
			}
		})
	}
}

// TestVersionZeroIsNotAUniformDenyMarker makes the meaning of version 0
// explicit across both entry points: it says "no published policy was
// evaluated", nothing about allow versus deny.
func TestVersionZeroIsNotAUniformDenyMarker(t *testing.T) {
	// Version 0 with an allow: Access granted by the owner role.
	access := Access(basisSubject(basisOwnerRole), basisLedger(), basisAction)
	wantDecision(t, access, true, "role grants "+basisAction+":"+basisScope,
		[]string{basisOwnerRole}, 0, "Access allow is also version 0")

	// Version 0 with a denial: an envelope-valid request on an organization
	// that has never published.
	store := NewStore()
	unpublished := store.Decide(basisOrg, basisRequest(basisOwnerRole))
	wantDecision(t, unpublished, false, "organization has no published version",
		nil, 0, "Decide denial on an unpublished organization")

	// A denial produced *after* evaluating a published version reports that
	// version, not 0: the published empty policy set denies by default and
	// the result must be distinguishable from "nothing published yet".
	if v, err := store.Publish(basisOrg, 0, nil); err != nil || v != 1 {
		t.Fatalf("publish empty set = %d, %v; want 1, nil", v, err)
	}
	emptySet := store.Decide(basisOrg, basisRequest(basisOwnerRole))
	wantDecision(t, emptySet, false, "no matching allow policy",
		nil, 1, "Decide denial under the published empty set")
}

// TestAccessMatchedEntryIsTheGrantingRole pins which role appears in
// Decision.Matched: it is the role that actually granted the read, in the
// role list's iteration order, never a policy identifier.
func TestAccessMatchedEntryIsTheGrantingRole(t *testing.T) {
	// A non-granting role ahead of owner does not replace the hit.
	subject := basisSubject("billing:invoice", basisOwnerRole, basisScopedRole)
	access := Access(subject, basisLedger(), basisAction)
	wantDecision(t, access, true, "role grants "+basisAction+":"+basisScope,
		[]string{basisOwnerRole}, 0, "owner encountered first grants")

	// Without owner, the scope-qualified role is the granting entry even
	// amid unrelated roles.
	subject = basisSubject("admin", basisScopedRole, "auditor")
	access = Access(subject, basisLedger(), basisAction)
	wantDecision(t, access, true, "role grants "+basisAction+":"+basisScope,
		[]string{basisScopedRole}, 0, "scope role encountered first grants")

	// A write-scoped role does not grant the read, and the policy identifier
	// is unknown to Access: it can never surface as a role hit.
	subject = basisSubject(basisScope + ":write")
	access = Access(subject, basisLedger(), basisAction)
	wantDecision(t, access, false, "no role grants "+basisAction+":"+basisScope,
		nil, 0, "write role cannot grant read")
	for _, hit := range access.Matched {
		if hit == basisPolicyID {
			t.Fatalf("Access matched a policy identifier: %q", hit)
		}
	}
}

// TestAccessResultIsIndependentOfPublishedPolicies shows the demo entry
// point cannot start depending on published-policy state: identical Access
// inputs return byte-identical decisions before a store exists, on a
// unpublished store and after a matching allow policy is published.
func TestAccessResultIsIndependentOfPublishedPolicies(t *testing.T) {
	withRole := func() Decision {
		return Access(basisSubject(basisOwnerRole), basisLedger(), basisAction)
	}
	withoutRole := func() Decision {
		return Access(basisSubject(), basisLedger(), basisAction)
	}

	before := withRole()
	store := NewStore()
	midUnpublished := withRole()
	if _, err := store.Publish(basisOrg, 0, []Policy{basisAllowPolicy()}); err != nil {
		t.Fatal(err)
	}
	afterPublish := withRole()
	if !reflect.DeepEqual(before, midUnpublished) || !reflect.DeepEqual(before, afterPublish) {
		t.Fatalf("Access decision changed around policy publishing: %+v / %+v / %+v",
			before, midUnpublished, afterPublish)
	}

	// The role-less denial is just as stable, and publishing an allow does
	// not rescue it: Access has no path from a policy to an allow.
	denyBefore := withoutRole()
	denyAfter := withoutRole()
	wantDecision(t, denyBefore, false, "no role grants "+basisAction+":"+basisScope, nil, 0,
		"role-less Access denial")
	if !reflect.DeepEqual(denyBefore, denyAfter) {
		t.Fatalf("role-less Access changed after publish: %+v vs %+v", denyBefore, denyAfter)
	}
}

// TestDecideWithMatchingAllowPolicyIgnoresRoles covers the published-policy
// half of the divergence: with a matching allow policy published, removing
// every role leaves the organization decision allowed and fully explained
// by the policy and its published version, while the demo decision denies
// for lack of a granting role.
func TestDecideWithMatchingAllowPolicyIgnoresRoles(t *testing.T) {
	store := NewStore()
	version, err := store.Publish(basisOrg, 0, []Policy{basisAllowPolicy()})
	if err != nil || version != 1 {
		t.Fatalf("publish matching allow = %d, %v; want 1, nil", version, err)
	}

	// No roles at all: the policy alone carries the organization decision.
	decide := store.Decide(basisOrg, basisRequest())
	wantDecision(t, decide, true, "matched allow policy",
		[]string{basisPolicyID}, version, "Decide with all roles removed")

	// The same request through the demo core is denied precisely because no
	// role grants this read — the published policy is not a role.
	access := Access(basisSubject(), basisLedger(), basisAction)
	wantDecision(t, access, false, "no role grants "+basisAction+":"+basisScope,
		nil, 0, "Access denies the role-less read despite the published allow")

	// The policy identifier is the org side's hit; a role name is the demo
	// side's hit. The two matched entries name different authorization
	// records and must not be unified.
	if decide.Matched[0] == basisOwnerRole || access.Matched != nil {
		t.Fatalf("hit kinds conflated: decide matched %q, access matched %q",
			decide.Matched, access.Matched)
	}
}

// TestChangingRolesNeverChangesOrgDecision varies only the carried roles.
// As long as the request content and published policy are otherwise
// unchanged, the organization decision must be identical in every
// explanatory field — not just in whether it allows. Meanwhile the demo
// decision does vary with the roles, proving the inputs are meaningful.
func TestChangingRolesNeverChangesOrgDecision(t *testing.T) {
	store := NewStore()
	version, err := store.Publish(basisOrg, 0, []Policy{basisAllowPolicy()})
	if err != nil || version != 1 {
		t.Fatalf("publish matching allow = %d, %v; want 1, nil", version, err)
	}

	roleSets := [][]string{
		nil,
		{},
		{basisOwnerRole},
		{basisScopedRole},
		{basisOwnerRole, basisScopedRole},
		{basisScopedRole, basisOwnerRole},
		{"unrelated-role", "admin"},
		{basisScope + ":write"}, // a write role says nothing about the read
	}

	baseline := store.Decide(basisOrg, basisRequest(roleSets[0]...))
	wantDecision(t, baseline, true, "matched allow policy",
		[]string{basisPolicyID}, version, "baseline org decision")

	sawAccessAllow, sawAccessDeny := false, false
	for _, roles := range roleSets {
		context := "role variant " + describeRoles(roles)
		got := store.Decide(basisOrg, basisRequest(roles...))
		// Full-field pinning: allowed, reason, matched list (and order) and
		// version all stay constant as the roles change.
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("%s: org decision changed from %+v to %+v", context, baseline, got)
		}
		if got.Allowed && (len(got.Matched) != 1 || got.Matched[0] != basisPolicyID) {
			t.Fatalf("%s: allow must be attributed to the policy, not roles: %+v", context, got)
		}

		access := Access(basisSubject(roles...), basisLedger(), basisAction)
		if access.Allowed {
			sawAccessAllow = true
			wantDecision(t, access, true, "role grants "+basisAction+":"+basisScope,
				rolesContainingGrant(roles), 0, context+" Access")
		} else {
			sawAccessDeny = true
			wantDecision(t, access, false, "no role grants "+basisAction+":"+basisScope,
				nil, 0, context+" Access")
		}
	}
	if !sawAccessAllow || !sawAccessDeny {
		t.Fatalf("role variants must move the demo decision both ways, saw allow=%v deny=%v",
			sawAccessAllow, sawAccessDeny)
	}
}

// describeRoles renders a role set for test failure messages.
func describeRoles(roles []string) string {
	if roles == nil {
		return "<nil>"
	}
	return "[" + joinRoles(roles) + "]"
}

func joinRoles(roles []string) string {
	out := ""
	for i, r := range roles {
		if i > 0 {
			out += ","
		}
		out += r
	}
	return out
}

// rolesContainingGrant returns the role that Access actually grants with,
// in iteration order, for an Access allow assertion.
func rolesContainingGrant(roles []string) []string {
	for _, r := range roles {
		if r == basisOwnerRole || r == basisScopedRole {
			return []string{r}
		}
	}
	return nil
}

// TestBothBasesRejectDisabledSubject pins the shared boundary: a disabled
// principal is denied on both sides with the same explicit reason, and
// neither an owner/scoped role nor a matching published allow policy can
// override the deactivation.
func TestBothBasesRejectDisabledSubject(t *testing.T) {
	store := NewStore()
	if _, err := store.Publish(basisOrg, 0, []Policy{basisAllowPolicy()}); err != nil {
		t.Fatal(err)
	}

	disabledSubject := basisSubject(basisOwnerRole, basisScopedRole)
	disabledSubject.Disabled = true

	access := Access(disabledSubject, basisLedger(), basisAction)
	wantDecision(t, access, false, "subject is disabled", nil, 0,
		"Access with disabled subject and granting roles")

	req := basisRequest(basisOwnerRole, basisScopedRole)
	req.Subject.Disabled = true
	decide := store.Decide(basisOrg, req)
	wantDecision(t, decide, false, "subject is disabled", nil, 0,
		"Decide with disabled subject and matching allow policy")
}

// TestOrganizationMismatchRejectsBeforePolicy pins the organization-level
// boundary: when the subject organization or resource organization differs
// from the decision organization, the request is rejected before any
// policy is consulted even though the allow policy matches — no policy hit,
// version 0, explicit organization-mismatch reason.
func TestOrganizationMismatchRejectsBeforePolicy(t *testing.T) {
	store := NewStore()
	if _, err := store.Publish(basisOrg, 0, []Policy{basisAllowPolicy()}); err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*OrgRequest){
		"subject org mismatch":  func(r *OrgRequest) { r.SubjectOrg = "globex" },
		"resource org mismatch": func(r *OrgRequest) { r.ResourceOrg = "another factory" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			// Granting roles ride along: the envelope rejection must still
			// win, proving roles cannot cure an organization mismatch.
			req := basisRequest(basisOwnerRole, basisScopedRole)
			mutate(&req)
			got := store.Decide(basisOrg, req)
			wantDecision(t, got, false, "organization mismatch", nil, 0, name)
		})
	}

	// A request whose three organizations agree but carries no role still
	// passes the envelope and reaches the matching policy, confirming the
	// denials above are caused by the mismatch and not by anything else.
	aligned := store.Decide(basisOrg, basisRequest())
	wantDecision(t, aligned, true, "matched allow policy",
		[]string{basisPolicyID}, 1, "aligned organizations with no roles")
}

// TestEveryNonEmptyOrgDecideLeavesConsistentAuditRecord pins the audit
// boundary: every Decide with a non-empty decision organization — including
// the unpublished-version denial, the disabled-subject denial and both
// organization-mismatch denials — appends exactly one decision record whose
// stored conclusion is the decision that was returned and whose stored
// request is the request that was submitted.
func TestEveryNonEmptyOrgDecideLeavesConsistentAuditRecord(t *testing.T) {
	store := NewStore()

	noPolicy := store.Decide(basisOrg, basisRequest(basisOwnerRole)) // seq 1: denial, version 0
	wantDecision(t, noPolicy, false, "organization has no published version", nil, 0,
		"recorded unpublished denial")

	version, err := store.Publish(basisOrg, 0, []Policy{basisAllowPolicy()}) // seq 2
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	allowedNoRoles := store.Decide(basisOrg, basisRequest()) // seq 3: policy allow, version 1
	wantDecision(t, allowedNoRoles, true, "matched allow policy",
		[]string{basisPolicyID}, 1, "recorded allow")

	disabled := basisRequest(basisOwnerRole)
	disabled.Subject.Disabled = true
	disabledDenial := store.Decide(basisOrg, disabled) // seq 4: envelope denial
	wantDecision(t, disabledDenial, false, "subject is disabled", nil, 0, "recorded disabled denial")

	subjectMismatch := basisRequest(basisOwnerRole)
	subjectMismatch.SubjectOrg = "globex"
	mismatchDenial := store.Decide(basisOrg, subjectMismatch) // seq 5
	wantDecision(t, mismatchDenial, false, "organization mismatch", nil, 0,
		"recorded subject-org mismatch denial")

	resourceMismatch := basisRequest(basisScopedRole)
	resourceMismatch.ResourceOrg = "globex"
	resourceMismatchDenial := store.Decide(basisOrg, resourceMismatch) // seq 6
	wantDecision(t, resourceMismatchDenial, false, "organization mismatch", nil, 0,
		"recorded resource-org mismatch denial")

	records, cp, err := store.AuditExport(basisOrg, 0)
	if err != nil {
		t.Fatalf("AuditExport: %v", err)
	}
	if len(records) != 6 {
		t.Fatalf("chain length = %d, want 6 (one publish + five decisions)", len(records))
	}
	if err := VerifyAudit(basisOrg, records, cp); err != nil {
		t.Fatalf("decision chain must verify including every denial: %v", err)
	}

	for i := range records {
		if records[i].Seq != i+1 {
			t.Fatalf("record %d has seq %d, want gapless %d", i, records[i].Seq, i+1)
		}
	}

	// The single policy change sits at seq 2; every other record is a
	// decision carrying the exact returned conclusion and submitted request.
	change := records[1]
	if change.Kind != AuditPolicyChange || change.Change == nil || change.Change.Version != 1 {
		t.Fatalf("seq 2 must be the version-1 policy change: %+v", change)
	}

	type recorded struct {
		seq      int
		decision Decision
		request  OrgRequest
	}
	want := []recorded{
		{seq: 1, decision: noPolicy, request: basisRequest(basisOwnerRole)},
		{seq: 3, decision: allowedNoRoles, request: basisRequest()},
		{seq: 4, decision: disabledDenial, request: disabled},
		{seq: 5, decision: mismatchDenial, request: subjectMismatch},
		{seq: 6, decision: resourceMismatchDenial, request: resourceMismatch},
	}
	for _, w := range want {
		r := records[w.seq-1]
		if r.Kind != AuditDecision || r.Decision == nil {
			t.Fatalf("seq %d is not a decision record: %+v", w.seq, r)
		}
		// The recorded conclusion matches the returned one field for field,
		// matched list and version included.
		if !reflect.DeepEqual(r.Decision.Decision, w.decision) {
			t.Fatalf("seq %d recorded decision %+v != returned %+v",
				w.seq, r.Decision.Decision, w.decision)
		}
		if !reflect.DeepEqual(r.Decision.Request, w.request) {
			t.Fatalf("seq %d recorded request %+v != submitted %+v",
				w.seq, r.Decision.Request, w.request)
		}
		// The existing recheck entry point preserves the same explanation
		// from the recorded material alone — denials included.
		replayed, err := store.RecheckDecision(basisOrg, w.seq)
		if err != nil {
			t.Fatalf("RecheckDecision seq %d: %v", w.seq, err)
		}
		if !reflect.DeepEqual(replayed, w.decision) {
			t.Fatalf("seq %d recheck %+v != recorded decision %+v",
				w.seq, replayed, w.decision)
		}
	}

	// Concretely, the version-0 denial records really carry empty hit lists
	// and no version, while the allow record names the policy and version 1.
	if hits := records[0].Decision.Decision.Matched; len(hits) != 0 {
		t.Fatalf("unpublished-version record must have no hits, got %q", hits)
	}
	if records[4].Decision.Decision.Version != 0 || len(records[4].Decision.Decision.Matched) != 0 {
		t.Fatalf("mismatch record must be version 0 with no hits: %+v", records[4].Decision.Decision)
	}
	allowRec := records[2].Decision.Decision
	if !reflect.DeepEqual(allowRec.Matched, []string{basisPolicyID}) || allowRec.Version != 1 {
		t.Fatalf("allow record must name %q at version 1: %+v", basisPolicyID, allowRec)
	}
}

// TestAccessLeavesAuditChainAndPolicyVersionUntouched pins the demo entry
// point's other boundary: it has no store of its own and must not alter an
// organization's audit chain, current version or published content. This
// includes organizations that have never had any record at all.
func TestAccessLeavesAuditChainAndPolicyVersionUntouched(t *testing.T) {
	store := NewStore()
	version, err := store.Publish(basisOrg, 0, []Policy{basisAllowPolicy()})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}
	before, cpBefore, err := store.AuditExport(basisOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cpBefore.EndSeq != 1 {
		t.Fatalf("setup end seq = %d, want 1", cpBefore.EndSeq)
	}

	// Exercise Access every way this fixture allows — allow, role-less
	// denial and disabled denial — none of which even receives the store.
	Access(basisSubject(basisOwnerRole), basisLedger(), basisAction)
	Access(basisSubject(), basisLedger(), basisAction)
	disabled := basisSubject(basisOwnerRole)
	disabled.Disabled = true
	Access(disabled, basisLedger(), basisAction)

	after, cpAfter, err := store.AuditExport(basisOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cpAfter.EndSeq != cpBefore.EndSeq || cpAfter.Fingerprint != cpBefore.Fingerprint {
		t.Fatalf("Access moved the audit chain: checkpoint %+v -> %+v", cpBefore, cpAfter)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("Access changed audit record content:\nbefore=%+v\nafter =%+v", before, after)
	}
	if got := store.CurrentVersion(basisOrg); got != version {
		t.Fatalf("current version = %d, want unchanged %d after Access", got, version)
	}
	published, err := store.Policies(basisOrg, version)
	if err != nil || !reflect.DeepEqual(published, []Policy{basisAllowPolicy()}) {
		t.Fatalf("published policies changed after Access: %+v, %v", published, err)
	}
	if err := VerifyAudit(basisOrg, after, cpAfter); err != nil {
		t.Fatalf("chain no longer verifies after Access: %v", err)
	}

	// An organization that only ever talked to Access has no chain and still
	// reports its empty genesis state.
	Access(basisSubject(basisOwnerRole), basisLedger(), basisAction)
	empty, emptyCP, err := store.AuditExport("factory that never decided", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 || emptyCP.EndSeq != 0 {
		t.Fatalf("Access created audit state for a fresh org: %+v, %+v", empty, emptyCP)
	}
	if err := VerifyAudit("factory that never decided", empty, emptyCP); err != nil {
		t.Fatalf("fresh-org genesis must verify: %v", err)
	}
}
