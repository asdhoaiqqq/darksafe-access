// Regression coverage for rolling an organization's policy set back to a
// historically published EMPTY version. An empty set is a genuine historical
// version, not a missing one: rolling back to it must revoke a current allow,
// consume exactly one consecutive new version (the rollback returns the new
// version number, never the target's), and leave a rollback-marked
// policy-change record carrying the empty content. The resulting default
// denial must survive online recheck and offline review from both the live
// export and a decoded archive, with the other (allowing) versions never
// substituted. Owner or scope roles on the subject must not change the
// organization decision.
//
// Both empty shapes published in the wild are covered: a nil policy slice and
// an explicit non-nil empty slice. The two are authorization-equivalent, and
// every public surface must accept either as a valid rollback target.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// emptyRollbackShapes are the two ways "no policies" reaches Publish.
func emptyRollbackShapes() map[string][]Policy {
	return map[string][]Policy{
		"nil slice":      nil,
		"empty slice":    {},
		"empty make":     make([]Policy, 0),
		"empty with cap": make([]Policy, 0, 4),
	}
}

// emptyRollbackAllowSet is the current allow policy the rollback revokes.
func emptyRollbackAllowSet() []Policy {
	return []Policy{allowPolicy("p-ledger-read", "u1", "read", "org/acme/ledger", false)}
}

// emptyRollbackRequest is a fully populated, organization-consistent request
// for an enabled subject; the allow set above grants it before the rollback.
func emptyRollbackRequest(org string) OrgRequest {
	return request(org, "u1", "ledger-1", "org/acme/ledger", "read")
}

// buildEmptyRollbackStore publishes the empty set as v1, the allow set as v2,
// and records one allowed decision under v2. It leaves the organization at
// current version 2 with three audit records (publish, publish, decision).
func buildEmptyRollbackStore(t *testing.T, org string, emptyPolicies []Policy) (*Store, OrgRequest, Decision) {
	t.Helper()
	s := NewStore()
	if v, err := s.Publish(org, 0, emptyPolicies); err != nil || v != 1 {
		t.Fatalf("publish empty v1 = %d, %v; want 1, nil", v, err)
	}
	allow := emptyRollbackAllowSet()
	if v, err := s.Publish(org, 1, allow); err != nil || v != 2 {
		t.Fatalf("publish allow v2 = %d, %v; want 2, nil", v, err)
	}
	req := emptyRollbackRequest(org)
	before := s.Decide(org, req)
	wantBefore := Decision{
		Allowed: true, Reason: "matched allow policy",
		Matched: []string{"p-ledger-read"}, Version: 2,
	}
	if !reflect.DeepEqual(before, wantBefore) {
		t.Fatalf("decision under v2 = %+v, want %+v", before, wantBefore)
	}
	return s, req, before
}

// TestRollbackToEmptyVersionRevokesAuthorization is the end-to-end contract:
// rollback to the empty historical version revokes the current grant, the
// version advances exactly once, and the new version's full policy set is the
// empty collection.
func TestRollbackToEmptyVersionRevokesAuthorization(t *testing.T) {
	for shape, emptyPolicies := range emptyRollbackShapes() {
		t.Run(shape, func(t *testing.T) {
			org := "acme-" + shape
			s, req, before := buildEmptyRollbackStore(t, org, emptyPolicies)
			allow := emptyRollbackAllowSet()

			// The rollback targets the real historical v1 and must return the
			// NEW consecutive version, not the target version number.
			newVersion, err := s.Rollback(org, 2, 1)
			if err != nil || newVersion != 3 {
				t.Fatalf("rollback to empty v1 = %d, %v; want new version 3, nil", newVersion, err)
			}
			if got := s.CurrentVersion(org); got != 3 {
				t.Fatalf("current version = %d, want 3 after rollback", got)
			}

			// The new version exists and queries successfully as an empty
			// collection; history is intact underneath it.
			gotV3, err := s.Policies(org, 3)
			if err != nil {
				t.Fatalf("Policies(v3): %v", err)
			}
			if len(gotV3) != 0 {
				t.Fatalf("Policies(v3) = %+v, want empty collection", gotV3)
			}
			gotV1, err := s.Policies(org, 1)
			if err != nil || len(gotV1) != 0 {
				t.Fatalf("Policies(v1) = %+v, %v; want empty, nil", gotV1, err)
			}
			gotV2, err := s.Policies(org, 2)
			if err != nil || !reflect.DeepEqual(gotV2, allow) {
				t.Fatalf("Policies(v2) = %+v, %v; want the original allow set", gotV2, err)
			}

			// The same admissible request is now default-denied under the new
			// version, with the no-matching-allow reason and an empty hit list.
			wantDenial := Decision{
				Allowed: false, Reason: "no matching allow policy",
				Matched: []string{}, Version: 3,
			}
			d := s.Decide(org, req)
			if !reflect.DeepEqual(d, wantDenial) {
				t.Fatalf("post-rollback decision = %+v, want %+v", d, wantDenial)
			}

			// Owner and formerly-sufficient scope roles never substitute for
			// an organization policy: the empty set denies regardless.
			roleReq := req
			roleReq.Subject.Roles = []string{"owner", "org/acme/ledger:read"}
			if dRole := s.Decide(org, roleReq); !reflect.DeepEqual(dRole, wantDenial) {
				t.Fatalf("decision with owner/scoped roles = %+v, want %+v", dRole, wantDenial)
			}

			// Historical review is pinned per version and was not rewritten:
			// v1 denies empty, v2 still allows exactly as recorded before.
			if d1 := s.Review(org, 1, req); d1.Allowed || d1.Version != 1 ||
				d1.Reason != "no matching allow policy" {
				t.Fatalf("review v1 = %+v, want default denial at v1", d1)
			}
			if d2 := s.Review(org, 2, req); !reflect.DeepEqual(d2, before) {
				t.Fatalf("review v2 = %+v, want preserved %+v", d2, before)
			}

			// Audit chain: publish v1 (seq 1), publish v2 (seq 2), the allowed
			// decision (seq 3), rollback (seq 4), denial decision (seq 5), and
			// the owner-role denial decision (seq 6).
			recs, cp, err := s.AuditExport(org, 0)
			if err != nil {
				t.Fatalf("AuditExport: %v", err)
			}
			if len(recs) != 6 {
				t.Fatalf("audit records = %d, want 6", len(recs))
			}
			if err := VerifyAudit(org, recs, cp); err != nil {
				t.Fatalf("VerifyAudit after empty rollback: %v", err)
			}
			for i, r := range recs {
				if r.Seq != i+1 || r.Org != org {
					t.Fatalf("record %d has unexpected seq/org: %+v", i+1, r)
				}
			}
			// Earlier records keep their original content and order.
			if recs[0].Kind != AuditPolicyChange || recs[0].Change.Version != 1 ||
				len(recs[0].Change.Policies) != 0 || recs[0].Change.RolledBack {
				t.Fatalf("v1 change record altered: %+v", recs[0].Change)
			}
			if recs[1].Kind != AuditPolicyChange ||
				!reflect.DeepEqual(recs[1].Change.Policies, allow) ||
				recs[1].Change.RolledBack || recs[1].Change.SourceVersion != 0 {
				t.Fatalf("v2 change record altered: %+v", recs[1].Change)
			}
			if recs[2].Kind != AuditDecision ||
				!reflect.DeepEqual(recs[2].Decision.Decision, before) ||
				!reflect.DeepEqual(recs[2].Decision.Request, req) {
				t.Fatalf("pre-rollback decision record altered: %+v", recs[2].Decision)
			}
			// The rollback record names the new version, the empty content,
			// and explicitly identifies itself as a rollback from v1.
			chg := recs[3].Change
			if recs[3].Kind != AuditPolicyChange || chg == nil {
				t.Fatalf("record 4 is not a policy change: %+v", recs[3])
			}
			if chg.Version != 3 {
				t.Fatalf("rollback record version = %d, want new version 3", chg.Version)
			}
			if len(chg.Policies) != 0 {
				t.Fatalf("rollback record policies = %+v, want empty", chg.Policies)
			}
			if !chg.RolledBack || chg.SourceVersion != 1 {
				t.Fatalf("rollback record must be tagged RolledBack with source v1: %+v", chg)
			}
			// The denial record stores the request and the exact result.
			dr := recs[4].Decision
			if recs[4].Kind != AuditDecision || dr == nil {
				t.Fatalf("record 5 is not a decision: %+v", recs[4])
			}
			if !reflect.DeepEqual(dr.Decision, wantDenial) {
				t.Fatalf("stored denial = %+v, want %+v", dr.Decision, wantDenial)
			}
			if !reflect.DeepEqual(dr.Request, req) {
				t.Fatalf("stored request = %+v, want %+v", dr.Request, req)
			}

			// The owner-role denial is seq 6 and reproduces the same result.
			rcOwner, err := s.RecheckDecision(org, 6)
			if err != nil {
				t.Fatalf("RecheckDecision(6): %v", err)
			}
			if !reflect.DeepEqual(rcOwner, wantDenial) {
				t.Fatalf("owner-role recheck = %+v, want %+v", rcOwner, wantDenial)
			}
			// The earlier allowed decision rechecks unchanged as well.
			rcBefore, err := s.RecheckDecision(org, 3)
			if err != nil || !reflect.DeepEqual(rcBefore, before) {
				t.Fatalf("RecheckDecision(3) = %+v, %v; want %+v", rcBefore, err, before)
			}

			// Offline review from the complete export plus the separately
			// retained checkpoint must find the empty v3 content in the
			// rollback record before the decision; it must not report the
			// version missing and must not fall back to allowing v2.
			review, err := RecheckDecisionOffline(org, recs, cp, 5)
			if err != nil {
				t.Fatalf("RecheckDecisionOffline after empty rollback: %v", err)
			}
			if !reflect.DeepEqual(review.Original, wantDenial) ||
				!reflect.DeepEqual(review.Recomputed, wantDenial) || !review.Consistent {
				t.Fatalf("offline review = %+v, want both decisions %+v and consistent", review, wantDenial)
			}

			// The same offline review must work through an archived export:
			// encode, decode against the retained checkpoint, then review.
			archive, err := EncodeAuditArchive(org, recs, cp)
			if err != nil {
				t.Fatalf("EncodeAuditArchive: %v", err)
			}
			decoded, err := DecodeAuditArchive(archive, org, cp)
			if err != nil {
				t.Fatalf("DecodeAuditArchive: %v", err)
			}
			arcReview, err := RecheckDecisionOffline(org, decoded, cp, 5)
			if err != nil {
				t.Fatalf("offline review from archive: %v", err)
			}
			if !arcReview.Consistent || !reflect.DeepEqual(arcReview.Recomputed, wantDenial) {
				t.Fatalf("archived offline review = %+v, want consistent %+v", arcReview, wantDenial)
			}

			// A later re-publish of an allow set must not change the recorded
			// denial's offline review: v3 stays the version actually used.
			if v, err := s.Publish(org, 3, allow); err != nil || v != 4 {
				t.Fatalf("publish v4 = %d, %v; want 4, nil", v, err)
			}
			fullRecs, fullCP, err := s.AuditExport(org, 0)
			if err != nil || len(fullRecs) != 7 {
				t.Fatalf("export after v4 = %d records, %v", len(fullRecs), err)
			}
			if err := VerifyAudit(org, fullRecs, fullCP); err != nil {
				t.Fatalf("chain with v4 does not verify: %v", err)
			}
			later, err := RecheckDecisionOffline(org, fullRecs, fullCP, 5)
			if err != nil {
				t.Fatalf("offline review with later v4: %v", err)
			}
			if !later.Consistent || !reflect.DeepEqual(later.Recomputed, wantDenial) {
				t.Fatalf("review with later allowing v4 = %+v, want unchanged %+v", later, wantDenial)
			}
			if d := s.Decide(org, req); d.Version != 4 || !d.Allowed {
				t.Fatalf("live decision under v4 = %+v, want allowed at v4", d)
			}
		})
	}
}

// TestRollbackToEmptyVersionFailuresLeaveStateUntouched preserves the existing
// failure contract for an empty target: a missing historical version is
// ErrVersionNotFound, a stale expected current version is ErrVersionConflict
// (the empty target is not exempt from the expected-version check), and no
// failure moves the current version, changes policy content, or appends an
// audit record.
func TestRollbackToEmptyVersionFailuresLeaveStateUntouched(t *testing.T) {
	for shape, emptyPolicies := range emptyRollbackShapes() {
		t.Run(shape, func(t *testing.T) {
			org := "acme-fail-" + shape
			s, req, _ := buildEmptyRollbackStore(t, org, emptyPolicies)

			// Before any rollback: a stale expected version against the empty
			// target conflicts, and must not consume version 3 or a seq.
			if _, err := s.Rollback(org, 9, 1); !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("stale rollback before success err = %v, want ErrVersionConflict", err)
			}
			if got := s.CurrentVersion(org); got != 2 {
				t.Fatalf("current = %d, want 2 after failed rollback", got)
			}
			if _, err := s.Policies(org, 3); !errors.Is(err, ErrVersionNotFound) {
				t.Fatalf("v3 exists after failed rollback, err = %v", err)
			}
			preRecs, _, err := s.AuditExport(org, 0)
			if err != nil || len(preRecs) != 3 {
				t.Fatalf("records before rollback = %d, %v; want 3", len(preRecs), err)
			}

			// The successful rollback then creates v3.
			if v, err := s.Rollback(org, 2, 1); err != nil || v != 3 {
				t.Fatalf("rollback = %d, %v; want 3, nil", v, err)
			}

			// Snapshot the successful state; failed calls below must leave it
			// bit-for-bit intact, including the audit chain's head fingerprint.
			okRecs, okCP, err := s.AuditExport(org, 0)
			if err != nil || len(okRecs) != 4 {
				t.Fatalf("records after rollback = %d, %v; want 4", len(okRecs), err)
			}

			// Empty target with a stale expected version: conflict, even though
			// version 1 genuinely exists and its content is empty.
			if _, err := s.Rollback(org, 2, 1); !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("stale expected version on empty target err = %v, want ErrVersionConflict", err)
			}
			if _, err := s.Rollback(org, 0, 1); !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("expected-version-zero rollback err = %v, want ErrVersionConflict", err)
			}
			// A target that has never existed in this organization is not
			// found, empty content or otherwise.
			if _, err := s.Rollback(org, 3, 42); !errors.Is(err, ErrVersionNotFound) {
				t.Fatalf("missing target err = %v, want ErrVersionNotFound", err)
			}

			if got := s.CurrentVersion(org); got != 3 {
				t.Fatalf("current = %d, want 3 after failed rollbacks", got)
			}
			gotV3, err := s.Policies(org, 3)
			if err != nil || len(gotV3) != 0 {
				t.Fatalf("Policies(v3) = %+v, %v; want empty, nil", gotV3, err)
			}
			postRecs, postCP, err := s.AuditExport(org, 0)
			if err != nil {
				t.Fatalf("export after failures: %v", err)
			}
			if len(postRecs) != len(okRecs) {
				t.Fatalf("records changed: %d before failures, %d after", len(okRecs), len(postRecs))
			}
			if postCP != okCP {
				t.Fatalf("audit head changed after failed rollbacks: %s vs %s", okCP.Fingerprint, postCP.Fingerprint)
			}
			if err := VerifyAudit(org, postRecs, postCP); err != nil {
				t.Fatalf("chain no longer verifies after failures: %v", err)
			}
			// The pre-rollback prefix is still the same records and still
			// verifies under its own checkpoint.
			prefixCP := Checkpoint{Org: org, EndSeq: 3, Fingerprint: postRecs[2].Fingerprint}
			if err := VerifyAudit(org, postRecs[:3], prefixCP); err != nil {
				t.Fatalf("historical prefix no longer verifies: %v", err)
			}
			if !reflect.DeepEqual(postRecs[:3], preRecs) {
				t.Fatalf("pre-rollback records were altered by later failures")
			}
			// Decisions are still made by the empty new version.
			wantDenial := Decision{
				Allowed: false, Reason: "no matching allow policy",
				Matched: []string{}, Version: 3,
			}
			if d := s.Decide(org, req); !reflect.DeepEqual(d, wantDenial) {
				t.Fatalf("decision after failures = %+v, want %+v", d, wantDenial)
			}
		})
	}
}
