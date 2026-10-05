// This file is the regression guard for deny-overrides across scope levels:
// once a deny policy covers the requested scope, a more specific allow on a
// descendant scope cannot let the request through. Existing suites pin
// recursive scope matching and same-scope policy conflicts separately; this
// suite puts the parent deny and the child allow into one real request, so a
// future authorization rewrite cannot mistake "more specific" for "allow
// wins".
//
// The guarded rules, all already implemented by Store.Decide/evaluate:
//
//   - A recursive deny on "org/a" covers the read of a resource in "org/a/b"
//     even when an exact allow for this subject and action sits on "org/a/b".
//     The answer is a denial explained as "matched deny policy", and the hit
//     list keeps BOTH policy identifiers, sorted ascending, at the version
//     actually evaluated.
//   - Swapping the two policies' submission order changes nothing in the
//     explanation: neither the verdict, the reason, the hit list nor the
//     used version may move.
//   - A non-recursive deny on "org/a" covers only its own scope; the same
//     child read is then allowed by the child allow alone.
//   - "org/ab" is not a child of "org/a": matching is by slash-separated
//     segments, never by string prefix, so a recursive deny on "org/a"
//     cannot block a read exactly allowed on "org/ab".
//
// Every request here is envelope-valid (the three organizations agree and
// subject, resource and action identifiers are present), so each outcome is
// genuinely produced by policy conflict, not by a request-shape rejection.
package darksafe

import (
	"reflect"
	"testing"
)

const (
	// Shared fixture for the parent-deny/child-allow regression.
	conflictOrg       = "acme"
	conflictSubject   = "u1"
	conflictAction    = "read"
	childResourceID   = "doc-1"
	childScope        = "org/a/b"
	parentScope       = "org/a"
	siblingScope      = "org/ab"
	siblingResourceID = "doc-2"
	parentDenyID      = "p-deny-parent"
	childAllowID      = "p-allow-child"
	siblingAllowID    = "p-allow-sibling"
)

// denyPolicy builds one deny policy; allowPolicy is the shared helper in
// org_test.go.
func denyPolicy(id, subject, action, scope string, recursive bool) Policy {
	return Policy{ID: id, Subject: subject, Action: action, Scope: scope, Effect: EffectDeny, Recursive: recursive}
}

func childReadRequest() OrgRequest {
	return request(conflictOrg, conflictSubject, childResourceID, childScope, conflictAction)
}

func siblingReadRequest() OrgRequest {
	return request(conflictOrg, conflictSubject, siblingResourceID, siblingScope, conflictAction)
}

// conflictPolicies returns the recursive parent deny plus the exact child
// allow in the requested slice order, used to prove submission order cannot
// influence any part of the decision.
func conflictPolicies(denyFirst bool) []Policy {
	deny := denyPolicy(parentDenyID, conflictSubject, conflictAction, parentScope, true)
	allow := allowPolicy(childAllowID, conflictSubject, conflictAction, childScope, false)
	if denyFirst {
		return []Policy{deny, allow}
	}
	return []Policy{allow, deny}
}

// wantDecisionShape pins the full four-field explanation: allowed, reason,
// sorted matched list and the version actually evaluated.
func wantDecisionShape(t *testing.T, got Decision, allowed bool, reason string, matched []string, version int, context string) {
	t.Helper()
	if got.Allowed != allowed {
		t.Fatalf("%s: allowed = %v, want %v; decision = %+v", context, got.Allowed, allowed, got)
	}
	if got.Reason != reason {
		t.Fatalf("%s: reason = %q, want %q; decision = %+v", context, got.Reason, reason, got)
	}
	if got.Version != version {
		t.Fatalf("%s: version = %d, want %d (the published version used); decision = %+v",
			context, got.Version, version, got)
	}
	if !reflect.DeepEqual(got.Matched, matched) {
		t.Fatalf("%s: matched = %q, want %q; decision = %+v", context, got.Matched, matched, got)
	}
}

// TestRecursiveParentDenyOverridesMoreSpecificChildAllow is the central
// guard: a recursive deny on org/a covers org/a/b, so the exact allow on
// org/a/b cannot grant the child read. The denial must still carry the
// allow's identifier — it is evidence of the conflict, not something the
// final verdict erases — and the hit list is sorted by identifier.
func TestRecursiveParentDenyOverridesMoreSpecificChildAllow(t *testing.T) {
	s := NewStore()
	version, err := s.Publish(conflictOrg, 0, conflictPolicies(true))
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	d := s.Decide(conflictOrg, childReadRequest())
	wantDecisionShape(t, d,
		false, "matched deny policy",
		[]string{childAllowID, parentDenyID}, // ascending, both hits retained
		version, "recursive parent deny vs exact child allow")

	// The allow hit is deliberately present on a denied decision: it is what
	// makes the deny reason reviewable as a conflict resolution.
	foundAllow := false
	for _, id := range d.Matched {
		if id == childAllowID {
			foundAllow = true
		}
	}
	if !foundAllow {
		t.Fatalf("denial dropped the more specific allow hit %q: %+v", childAllowID, d)
	}
}

// TestConflictDecisionIsIndependentOfPolicySubmissionOrder publishes the
// identical two-policy conflict with the deny and allow swapped in the
// submitted slice. The full explanation — verdict, reason, hit list and the
// published version actually used — must be byte-for-byte stable.
func TestConflictDecisionIsIndependentOfPolicySubmissionOrder(t *testing.T) {
	decide := func(denyFirst bool) Decision {
		s := NewStore()
		version, err := s.Publish(conflictOrg, 0, conflictPolicies(denyFirst))
		if err != nil || version != 1 {
			t.Fatalf("publish (denyFirst=%v) = %d, %v; want 1, nil", denyFirst, version, err)
		}
		d := s.Decide(conflictOrg, childReadRequest())
		wantDecisionShape(t, d,
			false, "matched deny policy",
			[]string{childAllowID, parentDenyID}, 1,
			"conflict decision must not depend on submission order")
		return d
	}

	denyFirst := decide(true)
	allowFirst := decide(false)
	if !reflect.DeepEqual(denyFirst, allowFirst) {
		t.Fatalf("swapping policy submission order changed the decision:\ndeny first  = %+v\nallow first = %+v",
			denyFirst, allowFirst)
	}
}

// TestNonRecursiveParentDenyDoesNotReachChildScope pins the Recursive flag's
// meaning from the other side: the same parent deny without Recursive covers
// org/a alone, so the child read is decided by the exact child allow and
// nothing else enters the hit list.
func TestNonRecursiveParentDenyDoesNotReachChildScope(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		denyPolicy(parentDenyID, conflictSubject, conflictAction, parentScope, false),
		allowPolicy(childAllowID, conflictSubject, conflictAction, childScope, false),
	}
	version, err := s.Publish(conflictOrg, 0, policies)
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	// The child read is allowed by the exact child policy alone; the parent
	// deny is not a hit and must not appear in the explanation.
	d := s.Decide(conflictOrg, childReadRequest())
	wantDecisionShape(t, d,
		true, "matched allow policy",
		[]string{childAllowID}, version, "child read under a non-recursive parent deny")

	// The non-recursive deny still guards exactly its own scope: at org/a the
	// child allow does not match and the deny decides, proving the child
	// allow above came from scope semantics, not from deny being weakened.
	atParent := s.Decide(conflictOrg, request(conflictOrg, conflictSubject, childResourceID, parentScope, conflictAction))
	wantDecisionShape(t, atParent,
		false, "matched deny policy",
		[]string{parentDenyID}, version, "read at the denied scope itself")
}

// TestRecursiveDenyOnOrgADoesNotCoverOrgAb guards the segment boundary:
// "org/ab" is a sibling prefix, not a descendant of "org/a". An exact allow
// on org/ab grants the org/ab read, and the recursive parent deny's
// identifier must never enter the hit list. No path normalization or
// wildcard reading is involved — matching stays slash-segment based.
func TestRecursiveDenyOnOrgADoesNotCoverOrgAb(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		denyPolicy(parentDenyID, conflictSubject, conflictAction, parentScope, true),
		allowPolicy(siblingAllowID, conflictSubject, conflictAction, siblingScope, false),
	}
	version, err := s.Publish(conflictOrg, 0, policies)
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	d := s.Decide(conflictOrg, siblingReadRequest())
	wantDecisionShape(t, d,
		true, "matched allow policy",
		[]string{siblingAllowID}, version, "read at org/ab with a recursive deny on org/a")

	for _, id := range d.Matched {
		if id == parentDenyID {
			t.Fatalf("org/a recursive deny leaked into the org/ab hit list: %+v", d)
		}
	}
}

// TestConflictDecisionAuditRecordKeepsAllowHit ensures the Decide call for
// the conflicting request leaves a complete audit record: the exact
// submitted request and the actual returned four-field decision — including
// the overridden allow hit that explains why the request was denied. The
// exported chain must verify, and rechecking the record must reproduce the
// denial from the recorded material.
func TestConflictDecisionAuditRecordKeepsAllowHit(t *testing.T) {
	s := NewStore()
	version, err := s.Publish(conflictOrg, 0, conflictPolicies(true))
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}
	req := childReadRequest()
	d := s.Decide(conflictOrg, req)
	wantDecisionShape(t, d, false, "matched deny policy",
		[]string{childAllowID, parentDenyID}, version, "recorded conflict denial")

	records, cp, err := s.AuditExport(conflictOrg, 0)
	if err != nil {
		t.Fatalf("AuditExport: %v", err)
	}
	// Seq 1 is the policy change; seq 2 is the single conflict decision.
	if len(records) != 2 {
		t.Fatalf("chain length = %d, want 2 (one publish + one decision)", len(records))
	}
	if err := VerifyAudit(conflictOrg, records, cp); err != nil {
		t.Fatalf("conflict audit chain must verify: %v", err)
	}
	rec := records[1]
	if rec.Kind != AuditDecision || rec.Decision == nil {
		t.Fatalf("seq 2 is not a decision record: %+v", rec)
	}

	// The stored request is the submitted one in full.
	if !reflect.DeepEqual(rec.Decision.Request, req) {
		t.Fatalf("recorded request %+v != submitted %+v", rec.Decision.Request, req)
	}
	// The stored conclusion is the returned one in all four fields.
	if !reflect.DeepEqual(rec.Decision.Decision, d) {
		t.Fatalf("recorded decision %+v != returned %+v", rec.Decision.Decision, d)
	}
	recorded := rec.Decision.Decision
	if recorded.Allowed || recorded.Reason != "matched deny policy" || recorded.Version != version {
		t.Fatalf("recorded conclusion must be the version-%d deny-overrides explanation: %+v",
			version, recorded)
	}
	// The overridden allow stays in the record so the deny is reviewable as a
	// conflict; order remains identifier-ascending in storage.
	if !reflect.DeepEqual(recorded.Matched, []string{childAllowID, parentDenyID}) {
		t.Fatalf("recorded hit list must retain both policies sorted: %+v", recorded.Matched)
	}

	replayed, err := s.RecheckDecision(conflictOrg, rec.Seq)
	if err != nil {
		t.Fatalf("RecheckDecision: %v", err)
	}
	if !reflect.DeepEqual(replayed, d) {
		t.Fatalf("recheck %+v != recorded conflict decision %+v", replayed, d)
	}
}

// TestResolvedConflictAuditRecordsCarryFullDecision covers the audit side of
// the two non-conflicting boundaries: when the child allow decides (parent
// deny non-recursive) and when the org/ab allow decides (segment boundary),
// the recorded decision keeps the allow verdict, reason, singleton hit list
// and the version actually used.
func TestResolvedConflictAuditRecordsCarryFullDecision(t *testing.T) {
	cases := []struct {
		name     string
		policies []Policy
		req      OrgRequest
		matched  string
	}{
		{
			name: "non-recursive parent deny, child allow wins",
			policies: []Policy{
				denyPolicy(parentDenyID, conflictSubject, conflictAction, parentScope, false),
				allowPolicy(childAllowID, conflictSubject, conflictAction, childScope, false),
			},
			req:     childReadRequest(),
			matched: childAllowID,
		},
		{
			name: "recursive parent deny does not reach org/ab",
			policies: []Policy{
				denyPolicy(parentDenyID, conflictSubject, conflictAction, parentScope, true),
				allowPolicy(siblingAllowID, conflictSubject, conflictAction, siblingScope, false),
			},
			req:     siblingReadRequest(),
			matched: siblingAllowID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			version, err := s.Publish(conflictOrg, 0, tc.policies)
			if err != nil || version != 1 {
				t.Fatalf("publish = %d, %v; want 1, nil", version, err)
			}
			d := s.Decide(conflictOrg, tc.req)
			wantDecisionShape(t, d, true, "matched allow policy",
				[]string{tc.matched}, version, tc.name)

			records, cp, err := s.AuditExport(conflictOrg, 0)
			if err != nil {
				t.Fatalf("AuditExport: %v", err)
			}
			if len(records) != 2 {
				t.Fatalf("chain length = %d, want 2", len(records))
			}
			if err := VerifyAudit(conflictOrg, records, cp); err != nil {
				t.Fatalf("audit chain must verify: %v", err)
			}
			stored := records[1].Decision
			if stored == nil {
				t.Fatalf("seq 2 is not a decision record: %+v", records[1])
			}
			if !reflect.DeepEqual(stored.Request, tc.req) || !reflect.DeepEqual(stored.Decision, d) {
				t.Fatalf("recorded request/decision drifted:\nrequest: got %+v want %+v\ndecision: got %+v want %+v",
					stored.Request, tc.req, stored.Decision, d)
			}
			replayed, err := s.RecheckDecision(conflictOrg, 2)
			if err != nil {
				t.Fatalf("RecheckDecision: %v", err)
			}
			if !reflect.DeepEqual(replayed, d) {
				t.Fatalf("recheck %+v != recorded decision %+v", replayed, d)
			}
		})
	}
}
