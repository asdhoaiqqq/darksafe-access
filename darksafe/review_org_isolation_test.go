// Regression coverage for historical Store.Review across organizations.
//
// Two organizations can reuse the same subject, resource, scope, action,
// policy identifier and version numbers; the decision organization named in
// the call is the only organization whose historical policy set may be
// evaluated. These tests pin that isolation for opposite v1 conclusions,
// an organization-local version 2, organization-mismatch rejections, and
// the read-only contract that keeps publish, rollback, online decision and
// audit-record recheck behavior untouched.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

const (
	reviewAlphaOrg = "alpha factory"
	reviewBetaOrg  = "beta factory"

	reviewPolicyID   = "p-ledger"
	reviewSubjectID  = "svc-audit-reader"
	reviewResourceID = "ledger-2026"
	reviewScope      = "factory/ledger"
	reviewAction     = "read"
)

// reviewIsolationPolicy builds one policy carrying the identifiers shared
// by both organizations; only the effect differs.
func reviewIsolationPolicy(effect Effect) Policy {
	return Policy{
		ID:      reviewPolicyID,
		Subject: reviewSubjectID,
		Action:  reviewAction,
		Scope:   reviewScope,
		Effect:  effect,
	}
}

// reviewIsolationRequest builds the same-shaped request attributed to org:
// the subject, resource and action identifiers are byte-identical across
// the two organizations.
func reviewIsolationRequest(org string) OrgRequest {
	return request(org, reviewSubjectID, reviewResourceID, reviewScope, reviewAction)
}

// setupReviewIsolation publishes version 1 of the identically identified
// policy in both organizations with opposite effects (alpha allows, beta
// denies), then publishes version 2 only in alpha carrying the opposite
// conclusion to alpha's version 1.
func setupReviewIsolation(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if v, err := s.Publish(reviewAlphaOrg, 0, []Policy{reviewIsolationPolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("publish alpha version 1 = %d, %v; want 1, nil", v, err)
	}
	if v, err := s.Publish(reviewBetaOrg, 0, []Policy{reviewIsolationPolicy(EffectDeny)}); err != nil || v != 1 {
		t.Fatalf("publish beta version 1 = %d, %v; want 1, nil", v, err)
	}
	if v, err := s.Publish(reviewAlphaOrg, 1, []Policy{reviewIsolationPolicy(EffectDeny)}); err != nil || v != 2 {
		t.Fatalf("publish alpha version 2 = %d, %v; want 2, nil", v, err)
	}
	return s
}

// assertReviewDecision pins every preserved field of one review result.
func assertReviewDecision(t *testing.T, d Decision, allowed bool, reason string, version int, matched []string) {
	t.Helper()
	if d.Allowed != allowed || d.Reason != reason || d.Version != version {
		t.Fatalf("review = %+v, want allowed=%v reason=%q version=%d matched=%v",
			d, allowed, reason, version, matched)
	}
	if !reflect.DeepEqual(d.Matched, matched) {
		t.Fatalf("review matched = %v, want %v (full decision %+v)", d.Matched, matched, d)
	}
}

// TestReviewSameIdentifiersAcrossOrganizationsGiveOppositeConclusions: both
// organizations publish version 1 with the same policy identifier for the
// same subject, action and resource scope, but one allows and the other
// denies. Reviewing a version must evaluate only the chosen organization's
// historical content; the shared identifier must never merge policy sets or
// carry the other organization's effect across.
func TestReviewSameIdentifiersAcrossOrganizationsGiveOppositeConclusions(t *testing.T) {
	s := setupReviewIsolation(t)

	alpha := reviewIsolationRequest(reviewAlphaOrg)
	beta := reviewIsolationRequest(reviewBetaOrg)

	assertReviewDecision(t, s.Review(reviewAlphaOrg, 1, alpha),
		true, "matched allow policy", 1, []string{reviewPolicyID})
	assertReviewDecision(t, s.Review(reviewBetaOrg, 1, beta),
		false, "matched deny policy", 1, []string{reviewPolicyID})

	// The decision organization decides: swapping only which organization
	// reviews which request must reject the envelope, never evaluate the
	// other organization's same-named policy.
	assertReviewDecision(t, s.Review(reviewAlphaOrg, 1, beta),
		false, "organization mismatch", 0, nil)
	assertReviewDecision(t, s.Review(reviewBetaOrg, 1, alpha),
		false, "organization mismatch", 0, nil)
}

// TestReviewVersionOneStaysPinnedAfterVersionTwo proves later publishes
// cannot change a historical review, and that version existence is judged
// strictly inside the selected organization. Alpha really does have a
// version 2 (with the opposite conclusion); beta never published one, so
// beta's version-2 review must report it missing instead of falling back to
// beta's current version or borrowing alpha's version 2.
func TestReviewVersionOneStaysPinnedAfterVersionTwo(t *testing.T) {
	s := setupReviewIsolation(t)
	alpha := reviewIsolationRequest(reviewAlphaOrg)
	beta := reviewIsolationRequest(reviewBetaOrg)

	// Alpha's current conclusion is now the opposite of version 1.
	assertReviewDecision(t, s.Decide(reviewAlphaOrg, alpha),
		false, "matched deny policy", 2, []string{reviewPolicyID})

	// Reviewing alpha version 1 still returns the original allow conclusion
	// with the original reason, hit and version.
	assertReviewDecision(t, s.Review(reviewAlphaOrg, 1, alpha),
		true, "matched allow policy", 1, []string{reviewPolicyID})

	// Alpha's version 2 genuinely exists and carries the deny conclusion.
	assertReviewDecision(t, s.Review(reviewAlphaOrg, 2, alpha),
		false, "matched deny policy", 2, []string{reviewPolicyID})

	// Beta never published version 2 despite every identifier being shared.
	// A fallback to beta's current version 1 deny would report version 1 and
	// the matched policy; borrowing alpha's version 2 would do the same with
	// version 2. The missing-version result has neither: version 0, no
	// matches, and the explicit not-found reason.
	assertReviewDecision(t, s.Review(reviewBetaOrg, 2, beta),
		false, "version 2 not found", 0, nil)

	// Reviewing the nonexistent version through alpha's request shape must
	// fail the same way: version existence is per chosen organization.
	assertReviewDecision(t, s.Review(reviewBetaOrg, 3, beta),
		false, "version 3 not found", 0, nil)

	// Beta's version 1 is untouched and still its own historical deny.
	assertReviewDecision(t, s.Review(reviewBetaOrg, 1, beta),
		false, "matched deny policy", 1, []string{reviewPolicyID})
	if got := s.CurrentVersion(reviewBetaOrg); got != 1 {
		t.Fatalf("beta current version = %d, want 1", got)
	}
}

// TestReviewOrganizationMismatchCannotUseSameNamedIdentifiers: even when a
// matching historical allow policy exists, a request whose subject or
// resource belongs to another organization is rejected by the envelope with
// version 0 and no matches. Same-named subjects and resources do not bypass
// the rejection.
func TestReviewOrganizationMismatchCannotUseSameNamedIdentifiers(t *testing.T) {
	s := setupReviewIsolation(t)

	cases := map[string]OrgRequest{
		"subject in beta, resource in alpha": func() OrgRequest {
			r := reviewIsolationRequest(reviewAlphaOrg)
			r.SubjectOrg = reviewBetaOrg
			return r
		}(),
		"subject in alpha, resource in beta": func() OrgRequest {
			r := reviewIsolationRequest(reviewAlphaOrg)
			r.ResourceOrg = reviewBetaOrg
			return r
		}(),
		"both in beta, decision under alpha": func() OrgRequest {
			r := reviewIsolationRequest(reviewBetaOrg) // subject/resource IDs stay identical
			return r
		}(),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			// Alpha version 1 holds a fully matching allow policy under the
			// shared identifiers; the mismatch must still deny it.
			assertReviewDecision(t, s.Review(reviewAlphaOrg, 1, req),
				false, "organization mismatch", 0, nil)
		})
	}

	// Symmetric direction against beta's same-named deny policy: the
	// envelope rejection wins and reports no policy evaluation at all.
	foreign := reviewIsolationRequest(reviewAlphaOrg)
	assertReviewDecision(t, s.Review(reviewBetaOrg, 1, foreign),
		false, "organization mismatch", 0, nil)
}

// TestReviewIsReadOnlyAcrossOrganizations runs every class of review
// (success, policy deny, missing version, organization mismatch) and proves
// neither organization's current version, historical content or audit chain
// changes.
func TestReviewIsReadOnlyAcrossOrganizations(t *testing.T) {
	s := setupReviewIsolation(t)
	alpha := reviewIsolationRequest(reviewAlphaOrg)
	beta := reviewIsolationRequest(reviewBetaOrg)

	// Establish one decision record per organization, so the audit chains
	// already contain decision records reviews must not touch.
	alphaCurrent := s.Decide(reviewAlphaOrg, alpha) // alpha seq 3, version 2 deny
	betaCurrent := s.Decide(reviewBetaOrg, beta)    // beta seq 2, version 1 deny

	alphaV1, err := s.Policies(reviewAlphaOrg, 1)
	if err != nil {
		t.Fatal(err)
	}
	alphaV2, err := s.Policies(reviewAlphaOrg, 2)
	if err != nil {
		t.Fatal(err)
	}
	betaV1, err := s.Policies(reviewBetaOrg, 1)
	if err != nil {
		t.Fatal(err)
	}
	alphaRecs, alphaCP, err := s.AuditExport(reviewAlphaOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	betaRecs, betaCP, err := s.AuditExport(reviewBetaOrg, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Successful allow, policy deny, version missing, and org mismatch: all
	// read paths.
	reviews := []Decision{
		s.Review(reviewAlphaOrg, 1, alpha), // historical allow
		s.Review(reviewBetaOrg, 1, beta),   // historical deny
		s.Review(reviewAlphaOrg, 2, alpha), // current-content deny
		s.Review(reviewBetaOrg, 2, beta),   // version not found
		s.Review(reviewBetaOrg, 99, beta),  // version not found
		s.Review(reviewAlphaOrg, 1, beta),  // organization mismatch
	}
	for i, d := range reviews {
		if d.Reason == "" {
			t.Fatalf("review %d returned a decision with no reason: %+v", i, d)
		}
	}

	if got := s.CurrentVersion(reviewAlphaOrg); got != 2 {
		t.Fatalf("alpha current version = %d, want 2 after reviews", got)
	}
	if got := s.CurrentVersion(reviewBetaOrg); got != 1 {
		t.Fatalf("beta current version = %d, want 1 after reviews", got)
	}

	if got, err := s.Policies(reviewAlphaOrg, 1); err != nil || !reflect.DeepEqual(got, alphaV1) {
		t.Fatalf("alpha v1 content changed: %v, %v", got, err)
	}
	if got, err := s.Policies(reviewAlphaOrg, 2); err != nil || !reflect.DeepEqual(got, alphaV2) {
		t.Fatalf("alpha v2 content changed: %v, %v", got, err)
	}
	if got, err := s.Policies(reviewBetaOrg, 1); err != nil || !reflect.DeepEqual(got, betaV1) {
		t.Fatalf("beta v1 content changed: %v, %v", got, err)
	}
	if _, err := s.Policies(reviewBetaOrg, 2); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("beta version 2 appeared after reviews: err=%v", err)
	}

	afterAlphaRecs, afterAlphaCP, err := s.AuditExport(reviewAlphaOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterAlphaRecs, alphaRecs) || afterAlphaCP != alphaCP {
		t.Fatalf("alpha audit chain changed during reviews:\nbefore=%+v\ncp=%+v\nafter=%+v\ncp=%+v",
			alphaRecs, alphaCP, afterAlphaRecs, afterAlphaCP)
	}
	afterBetaRecs, afterBetaCP, err := s.AuditExport(reviewBetaOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterBetaRecs, betaRecs) || afterBetaCP != betaCP {
		t.Fatalf("beta audit chain changed during reviews:\nbefore=%+v\ncp=%+v\nafter=%+v\ncp=%+v",
			betaRecs, betaCP, afterBetaRecs, afterBetaCP)
	}

	// The previously recorded online decisions remain recheckable exactly
	// as recorded: reviews neither altered nor displaced them.
	if got, err := s.RecheckDecision(reviewAlphaOrg, 3); err != nil || !reflect.DeepEqual(got, alphaCurrent) {
		t.Fatalf("alpha recheck = %+v, %v; want %+v", got, err, alphaCurrent)
	}
	if got, err := s.RecheckDecision(reviewBetaOrg, 2); err != nil || !reflect.DeepEqual(got, betaCurrent) {
		t.Fatalf("beta recheck = %+v, %v; want %+v", got, err, betaCurrent)
	}
}

// TestReviewKeepsOtherEntryPointsWorking confirms the existing publish,
// rollback, online decision and audit recheck flows keep their original
// behavior alongside the isolated historical reviews.
func TestReviewKeepsOtherEntryPointsWorking(t *testing.T) {
	s := setupReviewIsolation(t)
	alpha := reviewIsolationRequest(reviewAlphaOrg)
	beta := reviewIsolationRequest(reviewBetaOrg)

	// Reviews before and after the mutating operations all agree with their
	// organization's own history.
	assertReviewDecision(t, s.Review(reviewAlphaOrg, 1, alpha),
		true, "matched allow policy", 1, []string{reviewPolicyID})
	assertReviewDecision(t, s.Review(reviewBetaOrg, 1, beta),
		false, "matched deny policy", 1, []string{reviewPolicyID})

	// A fresh online decision appends an audit record and uses the current
	// version (alpha 2 = deny).
	online := s.Decide(reviewAlphaOrg, alpha)
	if online.Allowed || online.Version != 2 || !reflect.DeepEqual(online.Matched, []string{reviewPolicyID}) {
		t.Fatalf("online alpha decision = %+v, want version 2 deny", online)
	}
	if got, err := s.RecheckDecision(reviewAlphaOrg, 3); err != nil || !reflect.DeepEqual(got, online) {
		t.Fatalf("recheck after decide = %+v, %v; want %+v", got, err, online)
	}

	// Rolling alpha back to version 1 creates version 3; online decisions
	// follow the new current version while historical reviews stay pinned.
	v3, err := s.Rollback(reviewAlphaOrg, 2, 1)
	if err != nil || v3 != 3 {
		t.Fatalf("rollback = %d, %v; want 3, nil", v3, err)
	}
	rolled := s.Decide(reviewAlphaOrg, alpha)
	if !rolled.Allowed || rolled.Version != 3 {
		t.Fatalf("decision after rollback = %+v, want allowed version 3", rolled)
	}
	assertReviewDecision(t, s.Review(reviewAlphaOrg, 1, alpha),
		true, "matched allow policy", 1, []string{reviewPolicyID})
	assertReviewDecision(t, s.Review(reviewAlphaOrg, 2, alpha),
		false, "matched deny policy", 2, []string{reviewPolicyID})

	// Beta remains fully independent: current version 1, its own deny, and
	// still no version 2 even though alpha now carries three versions.
	if got := s.CurrentVersion(reviewBetaOrg); got != 1 {
		t.Fatalf("beta current version = %d, want 1", got)
	}
	assertReviewDecision(t, s.Review(reviewBetaOrg, 2, beta),
		false, "version 2 not found", 0, nil)
}
