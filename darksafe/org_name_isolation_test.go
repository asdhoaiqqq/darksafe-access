// This file is the business regression guard for organization-tenant
// isolation in organization-level access decisions. Tenants are keyed by the
// organization name exactly as submitted: Go string equality over the raw
// bytes. There is no case folding, trimming, UTF-8 validation, replacement,
// normalization or any other cleaning of a name. Two names that merely look
// alike when displayed — "acme" vs "Acme", "acme" vs "acme ", or a name
// carrying a lone 0xFF byte, a lone 0xFE byte and a genuine U+FFFD rune —
// name three different organizations with three independent version lines,
// policy sets and audit chains.
//
// The guarded rules, all already implemented by Store (org.go) and the audit
// chain (audit.go):
//
//   - Two organizations may use the very same subject id, resource id,
//     action, scope and policy id, and both sit at published version 1. One
//     publishing an allow and the other a deny makes otherwise identical
//     access come out allowed in one and denied in the other; the reason,
//     the hit policy id and the actual version all come from the deciding
//     organization alone. A similarly named organization that has never
//     published gets "organization has no published version", version 0 and
//     no hits: it can never borrow another organization's version 1.
//
//   - Case and surrounding spaces are significant: "acme", "Acme" and
//     "acme " are three tenants. Names carrying a lone 0xFF, a lone 0xFE and
//     a real U+FFFD are three tenants as well: the first two are invalid
//     UTF-8 bytes and the third is a legal character, and looking alike after
//     JSON replacement is never grounds to share a policy or a chain root.
//
//   - Once a matching allow is published in the deciding organization, a
//     request whose subject organization or resource organization differs by
//     even one byte is rejected FIRST, before any policy is evaluated:
//     "organization mismatch", version 0, no hits. The other fields being
//     complete and the subject enabled changes nothing, and an owner role
//     cannot bypass the check. Only when all three organization fields agree
//     does the organization's own policy decide.
//
//   - The audit boundary follows the same key: every Decide with a non-empty
//     decision organization appends exactly one record to that organization
//     only, saving the submitted organization bytes and the complete decision
//     verbatim. An organization-mismatch denial belongs to the organization
//     that made the decision, never to the subject's or resource's
//     organization, and reading one organization's records can never surface
//     calls stored under another name.
//
//   - Every public explanation keeps its existing wording and shape through
//     review, online recheck, offline review and the archive round trip; no
//     public decision entry point or reason string changes.
package darksafe

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Shared fixture: every organization sees the same subject id, resource id,
// scope, action and policy id, so the organization name is the only thing
// that can distinguish two outcomes.
const (
	orgNameSubjectID  = "u1"
	orgNameResourceID = "r1"
	orgNameScope      = "org/a"
	orgNameAction     = "read"
	orgNamePolicyID   = "p-shared"
)

// orgNamePolicy builds the one policy every fixture organization uses,
// differing only in the effect the caller passes. Subject, action, scope and
// policy id coincide across tenants on purpose.
func orgNamePolicy(effect Effect) Policy {
	return Policy{ID: orgNamePolicyID, Subject: orgNameSubjectID,
		Action: orgNameAction, Scope: orgNameScope, Effect: effect}
}

// orgNameRequest builds an envelope-valid request attributed to org on both
// the subject and the resource side, with the subject enabled. Optional roles
// are attached verbatim; roles never authorize in organization decisions.
func orgNameRequest(org string, roles ...string) OrgRequest {
	req := request(org, orgNameSubjectID, orgNameResourceID, orgNameScope, orgNameAction)
	if len(roles) > 0 {
		req.Subject.Roles = roles
	}
	return req
}

// mustPublishOrgName publishes one policy at expected version 0 and requires
// the resulting version to be 1.
func mustPublishOrgName(t *testing.T, s *Store, org string, effect Effect) {
	t.Helper()
	if v, err := s.Publish(org, 0, []Policy{orgNamePolicy(effect)}); err != nil || v != 1 {
		t.Fatalf("publish for %q = %d, %v; want version 1, nil", showOrg(org), v, err)
	}
}

// assertOrgNameDecision pins the full four-field explanation: verdict, exact
// reason, exact hit list and the version actually used.
func assertOrgNameDecision(t *testing.T, d Decision, allowed bool, reason string, matched []string, version int, context string) {
	t.Helper()
	if d.Allowed != allowed {
		t.Fatalf("%s: allowed = %v, want %v; decision = %+v", context, d.Allowed, allowed, d)
	}
	if d.Reason != reason {
		t.Fatalf("%s: reason = %q, want %q; decision = %+v", context, d.Reason, reason, d)
	}
	if d.Version != version {
		t.Fatalf("%s: version = %d, want %d; decision = %+v", context, d.Version, version, d)
	}
	got := d.Matched
	if len(got) == 0 {
		got = nil
	}
	want := matched
	if len(want) == 0 {
		want = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: matched = %q, want %q; decision = %+v", context, got, want, d)
	}
}

// orgChainLen returns the complete chain length of one organization through
// the public export entry point.
func orgChainLen(t *testing.T, s *Store, org string) int {
	t.Helper()
	recs, _, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatalf("export %q: %v", showOrg(org), err)
	}
	return len(recs)
}

// showOrg renders an organization name for failure messages without relying
// on its displayed form, since the confusable bytes all render alike.
func showOrg(org string) string {
	return strings.ReplaceAll(strings.ReplaceAll(org, invalidByte, `<FF>`), anotherInvalidByte, `<FE>`)
}

// TestOrgNameIsolationOppositeEffectsAndUnpublishedLookalike is the core
// tenant guard: the same subject, resource, action, scope and policy id at
// version 1 in two organizations produce opposite verdicts solely from the
// deciding organization, and a look-alike third organization that has never
// published cannot borrow either version 1.
func TestOrgNameIsolationOppositeEffectsAndUnpublishedLookalike(t *testing.T) {
	allowOrg, denyOrg, quietOrg := "acme", "Acme", "acme "
	s := NewStore()
	mustPublishOrgName(t, s, allowOrg, EffectAllow)
	mustPublishOrgName(t, s, denyOrg, EffectDeny)

	for _, org := range []string{allowOrg, denyOrg} {
		if got := s.CurrentVersion(org); got != 1 {
			t.Fatalf("current version of %q = %d, want 1", showOrg(org), got)
		}
	}
	// The look-alike has no version of its own even though both neighbors sit
	// at version 1.
	if got := s.CurrentVersion(quietOrg); got != 0 {
		t.Fatalf("current version of %q = %d, want 0", showOrg(quietOrg), got)
	}
	if _, err := s.Policies(quietOrg, 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("policies of %q at version 1: err = %v, want ErrVersionNotFound", showOrg(quietOrg), err)
	}

	allow := s.Decide(allowOrg, orgNameRequest(allowOrg))
	assertOrgNameDecision(t, allow, true, "matched allow policy",
		[]string{orgNamePolicyID}, 1, "allow tenant")

	deny := s.Decide(denyOrg, orgNameRequest(denyOrg))
	assertOrgNameDecision(t, deny, false, "matched deny policy",
		[]string{orgNamePolicyID}, 1, "deny tenant")

	// Identical request material, but the quiet organization has never
	// published: its own version line is 0 and it borrows no neighbor's v1.
	quiet := s.Decide(quietOrg, orgNameRequest(quietOrg))
	assertOrgNameDecision(t, quiet, false, "organization has no published version",
		nil, 0, "unpublished look-alike tenant")

	// A historical review asks the quiet organization for version 1: no
	// silent fallback to a neighbor's version 1 or to anything current.
	reviewed := s.Review(quietOrg, 1, orgNameRequest(quietOrg))
	assertOrgNameDecision(t, reviewed, false, "version 1 not found",
		nil, 0, "review of unpublished look-alike tenant")
}

// TestOrgNameCaseAndSurroundingSpacesNameDifferentTenants proves that case
// and leading/trailing spaces are part of the name: each variant is its own
// tenant with its own allow, and every request attributed to a different
// variant is rejected as an organization mismatch before policy evaluation,
// even though the names differ by one byte (case) or one trailing space.
func TestOrgNameCaseAndSurroundingSpacesNameDifferentTenants(t *testing.T) {
	variants := []string{"acme", "Acme", "acme "}
	s := NewStore()
	for _, org := range variants {
		mustPublishOrgName(t, s, org, EffectAllow)
	}

	// Each tenant admits access attributed to itself by its own policy.
	for _, org := range variants {
		d := s.Decide(org, orgNameRequest(org))
		assertOrgNameDecision(t, d, true, "matched allow policy",
			[]string{orgNamePolicyID}, 1, "identical name "+showOrg(org))
	}

	// Every ordered pair of distinct variants is a mismatch, whether the
	// subject organization or the resource organization is the odd field.
	for i, org := range variants {
		for j, other := range variants {
			if i == j {
				continue
			}
			subjectMismatch := orgNameRequest(org)
			subjectMismatch.SubjectOrg = other
			d := s.Decide(org, subjectMismatch)
			assertOrgNameDecision(t, d, false, "organization mismatch",
				nil, 0, "subject "+showOrg(other)+" deciding under "+showOrg(org))
			if d.Matched != nil {
				t.Fatalf("subject mismatch matched = %v, want nil before policy evaluation", d.Matched)
			}

			resourceMismatch := orgNameRequest(org)
			resourceMismatch.ResourceOrg = other
			d = s.Decide(org, resourceMismatch)
			assertOrgNameDecision(t, d, false, "organization mismatch",
				nil, 0, "resource "+showOrg(other)+" deciding under "+showOrg(org))
			if d.Matched != nil {
				t.Fatalf("resource mismatch matched = %v, want nil before policy evaluation", d.Matched)
			}
		}
	}

	// A leading space is just as significant as the trailing one.
	leading := orgNameRequest("acme")
	leading.SubjectOrg = " acme"
	d := s.Decide("acme", leading)
	assertOrgNameDecision(t, d, false, "organization mismatch", nil, 0, "leading-space subject org")

	// The three tenants advanced independently and share nothing: each still
	// reports exactly one published version.
	for _, org := range variants {
		if got := s.CurrentVersion(org); got != 1 {
			t.Fatalf("current version of %q = %d, want 1", showOrg(org), got)
		}
	}
}

// TestOrgNameRawBytesStayDistinctFromReplacementRune guards the
// display-confusable triple at the tenant boundary: a lone 0xFF, a lone 0xFE
// and a genuine U+FFFD rune. JSON collapses all three onto the same decoded
// string, so any layer that keyed organizations by a JSON rendering would
// merge them; the store must key them by raw bytes instead. The 0xFF tenant
// allows, the 0xFE tenant denies, and the U+FFFD tenant first has no
// published version, then answers from its own version 1 once it publishes.
func TestOrgNameRawBytesStayDistinctFromReplacementRune(t *testing.T) {
	orgFF := "acme" + invalidByte
	orgFE := "acme" + anotherInvalidByte
	orgFD := "acme" + replacementRune

	// Document the premise: Go distinguishes all three...
	if orgFF == orgFE || orgFF == orgFD || orgFE == orgFD {
		t.Fatalf("test premise broken: organization names must differ pairwise: %q %q %q",
			showOrg(orgFF), showOrg(orgFE), orgFD)
	}
	if !(!utf8.ValidString(orgFF) && !utf8.ValidString(orgFE) && utf8.ValidString(orgFD)) {
		t.Fatal("test premise broken: 0xFF and 0xFE are invalid UTF-8, U+FFFD is valid")
	}
	// ...while a JSON round trip collapses all three onto the same decoded
	// string. Any JSON-based tenant key would conflate the three.
	var decodedFF, decodedFE, decodedFD string
	for org, dst := range map[string]*string{orgFF: &decodedFF, orgFE: &decodedFE, orgFD: &decodedFD} {
		encoded, err := json.Marshal(org)
		if err != nil {
			t.Fatalf("marshal %q: %v", showOrg(org), err)
		}
		if err := json.Unmarshal(encoded, dst); err != nil {
			t.Fatalf("unmarshal %q: %v", showOrg(org), err)
		}
	}
	if decodedFF != decodedFE || decodedFF != decodedFD {
		t.Fatalf("test premise broken: JSON must conflate the three, got %q %q %q",
			decodedFF, decodedFE, decodedFD)
	}
	// The chain roots must keep the three display-alike names apart as well.
	if genesisFingerprint(orgFF) == genesisFingerprint(orgFE) ||
		genesisFingerprint(orgFF) == genesisFingerprint(orgFD) ||
		genesisFingerprint(orgFE) == genesisFingerprint(orgFD) {
		t.Fatal("genesis fingerprints collapsed the display-confusable organization names")
	}

	s := NewStore()
	mustPublishOrgName(t, s, orgFF, EffectAllow)
	mustPublishOrgName(t, s, orgFE, EffectDeny)

	dFF := s.Decide(orgFF, orgNameRequest(orgFF))
	assertOrgNameDecision(t, dFF, true, "matched allow policy",
		[]string{orgNamePolicyID}, 1, "0xFF tenant hits its own allow")

	dFE := s.Decide(orgFE, orgNameRequest(orgFE))
	assertOrgNameDecision(t, dFE, false, "matched deny policy",
		[]string{orgNamePolicyID}, 1, "0xFE tenant hits its own deny")

	// The genuine-U+FFFD tenant has never published and borrows no version 1.
	dFD := s.Decide(orgFD, orgNameRequest(orgFD))
	assertOrgNameDecision(t, dFD, false, "organization has no published version",
		nil, 0, "U+FFFD tenant before its own publish")

	// Cross-name attribution is rejected at the envelope, regardless of which
	// display-alike name is involved: no borrowing an allow or a deny.
	cross := orgNameRequest(orgFF)
	cross.SubjectOrg = orgFE
	assertOrgNameDecision(t, s.Decide(orgFF, cross), false, "organization mismatch",
		nil, 0, "0xFE subject deciding under the 0xFF tenant")
	cross = orgNameRequest(orgFE)
	cross.ResourceOrg = orgFD
	assertOrgNameDecision(t, s.Decide(orgFE, cross), false, "organization mismatch",
		nil, 0, "U+FFFD resource deciding under the 0xFE tenant")

	// Once the U+FFFD tenant publishes its own allow it answers from its own
	// version 1, and the decision records of the three tenants remain
	// pairwise distinguishable (raw-byte family vs JSON family fingerprints).
	mustPublishOrgName(t, s, orgFD, EffectAllow)
	dFD = s.Decide(orgFD, orgNameRequest(orgFD))
	assertOrgNameDecision(t, dFD, true, "matched allow policy",
		[]string{orgNamePolicyID}, 1, "U+FFFD tenant hits its own allow")

	fps := map[string]string{}
	for org, d := range map[string]Decision{orgFF: dFF, orgFE: dFE, orgFD: dFD} {
		// Force one fresh decision per tenant so each chain has a decision
		// record whose fingerprint can be compared.
		r := s.Decide(org, orgNameRequest(org))
		if r.Reason != d.Reason {
			t.Fatalf("fresh decision under %q changed outcome: %+v vs %+v", showOrg(org), r, d)
		}
		recs, _, err := s.AuditExport(org, 0)
		if err != nil {
			t.Fatalf("export %q: %v", showOrg(org), err)
		}
		last := recs[len(recs)-1]
		if last.Kind != AuditDecision {
			t.Fatalf("last record of %q is %s", showOrg(org), last.Kind)
		}
		fps[org] = last.Fingerprint
	}
	if fps[orgFF] == fps[orgFE] || fps[orgFF] == fps[orgFD] || fps[orgFE] == fps[orgFD] {
		t.Fatalf("decision fingerprints collapsed display-confusable tenants: %v", fps)
	}
}

// TestOrgNameMismatchRejectsBeforePolicyEvenForOwner pins the order of
// operations: against a published matching allow, changing either
// organization field by even one byte rejects with "organization mismatch"
// before policies are consulted (version 0, no hits). A complete request, an
// enabled subject and an owner role make no difference. With all three
// organization fields equal, the organization's own policy — not the role —
// decides.
func TestOrgNameMismatchRejectsBeforePolicyEvenForOwner(t *testing.T) {
	const org = "acme"
	s := NewStore()
	mustPublishOrgName(t, s, org, EffectAllow)

	// Each name differs from the decision organization by one byte (case,
	// one fewer byte, one changed final byte, one trailing space, one added
	// raw byte).
	lookAlikes := []string{
		"Acme",
		"acm",
		"acmd",
		"acme ",
		" acme",
		"acme" + invalidByte,
		"acme" + anotherInvalidByte,
		"acme" + replacementRune,
	}
	for _, other := range lookAlikes {
		// The subject carries owner, is enabled, and every other field is
		// complete. Owner must not bypass the organization boundary.
		subjectMismatch := orgNameRequest(org, "owner")
		subjectMismatch.SubjectOrg = other
		d := s.Decide(org, subjectMismatch)
		assertOrgNameDecision(t, d, false, "organization mismatch", nil, 0,
			"subject "+showOrg(other)+" with owner role")
		if d.Matched != nil {
			t.Fatalf("subject %q mismatch evaluated policies: matched = %v", showOrg(other), d.Matched)
		}

		resourceMismatch := orgNameRequest(org, "owner")
		resourceMismatch.ResourceOrg = other
		d = s.Decide(org, resourceMismatch)
		assertOrgNameDecision(t, d, false, "organization mismatch", nil, 0,
			"resource "+showOrg(other)+" with owner role")
		if d.Matched != nil {
			t.Fatalf("resource %q mismatch evaluated policies: matched = %v", showOrg(other), d.Matched)
		}

		// The neighbor names need not even exist as tenants: mismatch beats
		// the version lookup, so their (non-)existence cannot matter.
		if got := s.CurrentVersion(other); got != 0 {
			t.Fatalf("look-alike %q unexpectedly has version %d", showOrg(other), got)
		}
	}

	// Control: all three organization fields equal and owner present — the
	// published policy allows. Roles do not decide, but here the policy does.
	owner := s.Decide(org, orgNameRequest(org, "owner"))
	assertOrgNameDecision(t, owner, true, "matched allow policy",
		[]string{orgNamePolicyID}, 1, "owner subject inside its own tenant")
	plain := s.Decide(org, orgNameRequest(org))
	assertOrgNameDecision(t, plain, true, "matched allow policy",
		[]string{orgNamePolicyID}, 1, "plain subject inside its own tenant")

	// A tenant whose own current policy denies still denies when the
	// organization fields agree, even with owner present: roles never
	// override organization policy.
	const denyOrg = "Acme"
	mustPublishOrgName(t, s, denyOrg, EffectDeny)
	denied := s.Decide(denyOrg, orgNameRequest(denyOrg, "owner"))
	assertOrgNameDecision(t, denied, false, "matched deny policy",
		[]string{orgNamePolicyID}, 1, "owner cannot override the tenant deny")
}

// TestOrgNameMismatchAuditRecordBelongsToDecisionOrg pins audit attribution:
// a mismatch denial is appended exactly once, to the deciding organization,
// with the submitted subject/resource organization bytes and the full
// decision preserved; the subject's and resource's organizations get no
// record. Display-confusable raw-byte names follow the same attribution and
// byte-preservation rule.
func TestOrgNameMismatchAuditRecordBelongsToDecisionOrg(t *testing.T) {
	s := NewStore()
	mustPublishOrgName(t, s, "acme", EffectAllow) // acme seq 1
	mustPublishOrgName(t, s, "Acme", EffectDeny)  // Acme seq 1

	beforeAcme := orgChainLen(t, s, "acme")
	beforeUpper := orgChainLen(t, s, "Acme")

	// Subject belongs to the other tenant.
	subjectMismatch := orgNameRequest("acme")
	subjectMismatch.SubjectOrg = "Acme"
	d1 := s.Decide("acme", subjectMismatch)
	assertOrgNameDecision(t, d1, false, "organization mismatch", nil, 0, "subject mismatch")
	// Resource belongs to the other tenant.
	resourceMismatch := orgNameRequest("acme")
	resourceMismatch.ResourceOrg = "Acme"
	d2 := s.Decide("acme", resourceMismatch)
	assertOrgNameDecision(t, d2, false, "organization mismatch", nil, 0, "resource mismatch")

	// Exactly one record per call landed in the deciding tenant...
	if got := orgChainLen(t, s, "acme"); got != beforeAcme+2 {
		t.Fatalf("acme chain = %d, want %d (exactly one record per call)", got, beforeAcme+2)
	}
	// ...and nothing at all landed in the subject's or resource's tenant.
	if got := orgChainLen(t, s, "Acme"); got != beforeUpper {
		t.Fatalf("Acme chain = %d, want unchanged %d; mismatch records leaked across names", got, beforeUpper)
	}
	if got := len(drainAudit(t, s, "Acme", AuditDecision, "")); got != 0 {
		t.Fatalf("Acme decision records = %d, want 0", got)
	}

	acmeRecs, acmeCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", acmeRecs, acmeCP); err != nil {
		t.Fatalf("acme chain must verify: %v", err)
	}
	// seq 2 = subject mismatch, seq 3 = resource mismatch. Both are headed
	// by the deciding tenant and preserve the foreign organization bytes and
	// the complete decision verbatim.
	if r := acmeRecs[1]; r.Org != "acme" || r.Kind != AuditDecision ||
		r.Decision.Request.SubjectOrg != "Acme" || r.Decision.Request.ResourceOrg != "acme" {
		t.Fatalf("seq 2 envelope = org %q request %+v, want acme-headed subject mismatch",
			r.Org, r.Decision.Request)
	}
	if !reflect.DeepEqual(acmeRecs[1].Decision.Decision, d1) {
		t.Fatalf("seq 2 decision = %+v, want %+v", acmeRecs[1].Decision.Decision, d1)
	}
	if r := acmeRecs[2]; r.Org != "acme" || r.Kind != AuditDecision ||
		r.Decision.Request.SubjectOrg != "acme" || r.Decision.Request.ResourceOrg != "Acme" {
		t.Fatalf("seq 3 envelope = org %q request %+v, want acme-headed resource mismatch",
			r.Org, r.Decision.Request)
	}
	if !reflect.DeepEqual(acmeRecs[2].Decision.Decision, d2) {
		t.Fatalf("seq 3 decision = %+v, want %+v", acmeRecs[2].Decision.Decision, d2)
	}

	// Acme's own chain contains only its publish: a recheck of the acme
	// decision sequences under the Acme name finds nothing.
	upperRecs, upperCP, err := s.AuditExport("Acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(upperRecs) != 1 || upperRecs[0].Kind != AuditPolicyChange {
		t.Fatalf("Acme chain = %+v, want only its publish", upperRecs)
	}
	if err := VerifyAudit("Acme", upperRecs, upperCP); err != nil {
		t.Fatalf("Acme chain must verify: %v", err)
	}
	for _, seq := range []int{2, 3} {
		if _, err := s.RecheckDecision("Acme", seq); !errors.Is(err, ErrAuditNotFound) {
			t.Fatalf("Acme recheck of acme seq %d: err = %v, want ErrAuditNotFound", seq, err)
		}
	}

	// Raw-byte variant: the deciding tenant carries 0xFF and the resource
	// tenant carries 0xFE. Invalid bytes must not move attribution or get
	// cleaned in the stored request.
	orgFF := "acme" + invalidByte
	orgFE := "acme" + anotherInvalidByte
	mustPublishOrgName(t, s, orgFF, EffectAllow)
	rawReq := orgNameRequest(orgFF)
	rawReq.SubjectOrg = orgFF
	rawReq.ResourceOrg = orgFE
	d3 := s.Decide(orgFF, rawReq)
	assertOrgNameDecision(t, d3, false, "organization mismatch", nil, 0, "raw-byte resource mismatch")

	ffRecs, ffCP, err := s.AuditExport(orgFF, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(orgFF, ffRecs, ffCP); err != nil {
		t.Fatalf("0xFF chain must verify with raw bytes: %v", err)
	}
	last := ffRecs[len(ffRecs)-1]
	if last.Org != orgFF || last.Decision.Request.ResourceOrg != orgFE ||
		last.Decision.Request.SubjectOrg != orgFF {
		t.Fatalf("raw mismatch record = org %q subject %q resource %q, want raw bytes preserved",
			showOrg(last.Org), showOrg(last.Decision.Request.SubjectOrg),
			showOrg(last.Decision.Request.ResourceOrg))
	}
	if !reflect.DeepEqual(last.Decision.Decision, d3) {
		t.Fatalf("raw mismatch decision = %+v, want %+v", last.Decision.Decision, d3)
	}
	// The 0xFE name appeared only inside a request; it owns no record and its
	// chain is the empty genesis.
	if got := orgChainLen(t, s, orgFE); got != 0 {
		t.Fatalf("0xFE chain = %d, want 0; a mismatch record leaked to the resource tenant", got)
	}
	feRecs, feCP, err := s.AuditExport(orgFE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feRecs) != 0 || feCP.EndSeq != 0 || feCP.Fingerprint != genesisFingerprint(orgFE) {
		t.Fatalf("0xFE export = %+v %+v, want empty genesis", feRecs, feCP)
	}
	if got := len(drainAudit(t, s, orgFE, "", "")); got != 0 {
		t.Fatalf("0xFE drained records = %d, want 0", got)
	}
}

// TestOrgNameIsolationPreservedByReviewRecheckOfflineArchive drives one
// decision of every shape — allow, deny, unpublished-tenant denial and
// mismatch denial — and pins that the existing public entry points and
// explanation formats stay unchanged: historical review, online recheck,
// offline review from an export and offline review after an archive round
// trip all reproduce the decision, while material from one name can never be
// read under another.
func TestOrgNameIsolationPreservedByReviewRecheckOfflineArchive(t *testing.T) {
	allowOrg, denyOrg, quietOrg := "acme", "Acme", "acme "
	s := NewStore()
	mustPublishOrgName(t, s, allowOrg, EffectAllow) // acme seq 1
	mustPublishOrgName(t, s, denyOrg, EffectDeny)   // Acme seq 1

	dAllow := s.Decide(allowOrg, orgNameRequest(allowOrg)) // acme seq 2
	dDeny := s.Decide(denyOrg, orgNameRequest(denyOrg))    // Acme seq 2
	dQuiet := s.Decide(quietOrg, orgNameRequest(quietOrg)) // "acme " seq 1

	// A mismatch denial recorded against the allow tenant.
	mismatchReq := orgNameRequest(allowOrg)
	mismatchReq.SubjectOrg = denyOrg
	dMismatch := s.Decide(allowOrg, mismatchReq) // acme seq 3

	type recorded struct {
		org  string
		seq  int
		want Decision
		req  OrgRequest
	}
	cases := []recorded{
		{allowOrg, 2, dAllow, orgNameRequest(allowOrg)},
		{denyOrg, 2, dDeny, orgNameRequest(denyOrg)},
		{quietOrg, 1, dQuiet, orgNameRequest(quietOrg)},
		{allowOrg, 3, dMismatch, mismatchReq},
	}

	for _, c := range cases {
		label := showOrg(c.org) + " seq " + strconv.Itoa(c.seq)
		recs, cp, err := s.AuditExport(c.org, 0)
		if err != nil {
			t.Fatalf("%s export: %v", label, err)
		}
		if err := VerifyAudit(c.org, recs, cp); err != nil {
			t.Fatalf("%s chain verify: %v", label, err)
		}
		r := recs[c.seq-1]
		if r.Org != c.org || r.Kind != AuditDecision {
			t.Fatalf("%s record header = %q %s", label, r.Org, r.Kind)
		}
		// The submitted organization bytes and the full decision survive.
		if r.Decision.Request.SubjectOrg != c.req.SubjectOrg ||
			r.Decision.Request.ResourceOrg != c.req.ResourceOrg {
			t.Fatalf("%s stored orgs = subject %q resource %q, want %q %q",
				label, r.Decision.Request.SubjectOrg, r.Decision.Request.ResourceOrg,
				c.req.SubjectOrg, c.req.ResourceOrg)
		}
		if !reflect.DeepEqual(r.Decision.Decision, c.want) {
			t.Fatalf("%s stored decision = %+v, want %+v", label, r.Decision.Decision, c.want)
		}

		// Online recheck reproduces the decision from the tenant's own chain.
		replayed, err := s.RecheckDecision(c.org, c.seq)
		if err != nil || !decisionsEqual(replayed, c.want) {
			t.Fatalf("%s online recheck = %+v, %v; want %+v", label, replayed, err, c.want)
		}

		// Historical review reproduces version-1 judgments and still rejects
		// the mismatch request at the envelope.
		reviewed := s.Review(c.org, 1, c.req)
		if c.want.Version == 0 && c.want.Reason == "organization has no published version" {
			// The quiet tenant has no version 1 to review.
			if reviewed.Reason != "version 1 not found" || reviewed.Version != 0 || reviewed.Allowed {
				t.Fatalf("%s historical review = %+v, want version 1 not found", label, reviewed)
			}
		} else if !reflect.DeepEqual(reviewed, c.want) {
			t.Fatalf("%s historical review = %+v, want %+v", label, reviewed, c.want)
		}

		// Offline review over the chain-validated export is consistent.
		rv, err := RecheckDecisionOffline(c.org, recs, cp, c.seq)
		if err != nil || !rv.Consistent || !decisionsEqual(rv.Recomputed, c.want) {
			t.Fatalf("%s offline review = %+v, %v; want consistent %+v", label, rv, err, c.want)
		}

		// Archive round trip keeps the exact material verifiable and
		// reviewable offline.
		archive, err := EncodeAuditArchive(c.org, recs, cp)
		if err != nil {
			t.Fatalf("%s archive encode: %v", label, err)
		}
		decoded, err := DecodeAuditArchive(archive, c.org, cp)
		if err != nil {
			t.Fatalf("%s archive decode: %v", label, err)
		}
		if !reflect.DeepEqual(decoded, recs) {
			t.Fatalf("%s archive round trip changed records", label)
		}
		if err := VerifyAudit(c.org, decoded, cp); err != nil {
			t.Fatalf("%s decoded chain verify: %v", label, err)
		}
		rv2, err := RecheckDecisionOffline(c.org, decoded, cp, c.seq)
		if err != nil || !rv2.Consistent || !decisionsEqual(rv2.Recomputed, c.want) {
			t.Fatalf("%s post-archive offline review = %+v, %v; want consistent %+v",
				label, rv2, err, c.want)
		}
	}

	// Material exported under one name can never validate under another,
	// including the trailing-space look-alike, via every offline entry.
	acmeRecs, acmeCP, err := s.AuditExport(allowOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	acmeArchive, err := EncodeAuditArchive(allowOrg, acmeRecs, acmeCP)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{denyOrg, quietOrg} {
		if err := VerifyAudit(other, acmeRecs, acmeCP); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("verify acme material as %q: err = %v, want ErrInvalidRange", showOrg(other), err)
		}
		if _, err := DecodeAuditArchive(acmeArchive, other, acmeCP); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("decode acme archive as %q: err = %v, want ErrInvalidRange", showOrg(other), err)
		}
		if _, err := RecheckDecisionOffline(other, acmeRecs, acmeCP, 2); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("offline review acme material as %q: err = %v, want ErrInvalidRange", showOrg(other), err)
		}
	}

	// Read paths append nothing: every chain is exactly the length the
	// decisions left it at, and reads under other names stay empty.
	if got := orgChainLen(t, s, allowOrg); got != 3 {
		t.Fatalf("acme chain = %d, want 3", got)
	}
	if got := orgChainLen(t, s, denyOrg); got != 2 {
		t.Fatalf("Acme chain = %d, want 2", got)
	}
	if got := orgChainLen(t, s, quietOrg); got != 1 {
		t.Fatalf("quiet chain = %d, want 1", got)
	}
}

// TestOrgNameDecisionExplanationStringsUnchanged locks the public decision
// vocabulary for the organization-level entry point: these exact reason
// strings, versions and hit-list shapes are the existing contract that
// callers and auditors depend on.
func TestOrgNameDecisionExplanationStringsUnchanged(t *testing.T) {
	s := NewStore()
	req := orgNameRequest("acme")

	// Nothing published yet.
	if d := s.Decide("acme", req); d.Allowed ||
		d.Reason != "organization has no published version" || d.Version != 0 || d.Matched != nil {
		t.Fatalf("no-version decision = %+v", d)
	}

	mustPublishOrgName(t, s, "acme", EffectAllow)
	allow := s.Decide("acme", orgNameRequest("acme"))
	assertOrgNameDecision(t, allow, true, "matched allow policy",
		[]string{orgNamePolicyID}, 1, "v1 allow")

	if v, err := s.Publish("acme", 1, []Policy{orgNamePolicy(EffectDeny)}); err != nil || v != 2 {
		t.Fatalf("v2 publish = %d, %v; want 2, nil", v, err)
	}
	deny := s.Decide("acme", orgNameRequest("acme"))
	assertOrgNameDecision(t, deny, false, "matched deny policy",
		[]string{orgNamePolicyID}, 2, "v2 deny")

	mismatch := orgNameRequest("acme")
	mismatch.SubjectOrg = "Acme"
	d := s.Decide("acme", mismatch)
	assertOrgNameDecision(t, d, false, "organization mismatch", nil, 0, "mismatch wording")
	if d.Matched != nil {
		t.Fatalf("mismatch matched = %v, want nil", d.Matched)
	}
}
