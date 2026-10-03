// Regression coverage for rolling an organization back to a historical
// EMPTY policy version. The empty set is a genuine, published historical
// version: rolling back to it must (1) be accepted rather than reported as
// a missing version, (2) withdraw the current authorization by publishing
// the emptiness as a brand-new version, and (3) remain explainable and
// re-checkable online, offline from a full export, and offline from a
// decoded audit archive paired with a separately retained checkpoint.
//
// Both empty shapes a caller can publish — a nil list and an explicit
// non-nil empty list — must serve as rollback targets.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

const rollbackEmptyOrg = "rollback-empty-org"

// rollbackEmptyAllow is the single allow policy of the non-empty version
// the organization later rolls back away from.
func rollbackEmptyAllow() []Policy {
	return []Policy{{
		ID:      "rollback-empty-allow",
		Subject: "u1",
		Action:  "read",
		Scope:   "org/acme/ledger",
		Effect:  EffectAllow,
	}}
}

func rollbackEmptyRequest() OrgRequest {
	return request(rollbackEmptyOrg, "u1", "res-1", "org/acme/ledger", "read")
}

// checkRollbackToEmptyVersion drives the full user-visible scenario for one
// shape of the published empty set and pins every required observation.
func checkRollbackToEmptyVersion(t *testing.T, emptyPolicies []Policy, shape string) {
	t.Helper()
	s := NewStore()
	org := rollbackEmptyOrg
	req := rollbackEmptyRequest()
	allow := rollbackEmptyAllow()

	// v1 is the earlier, successfully published empty policy version.
	v1, err := s.Publish(org, 0, emptyPolicies)
	if err != nil || v1 != 1 {
		t.Fatalf("[%s] publish empty v1 = %d, %v; want 1, nil", shape, v1, err)
	}
	// v2 is the current version carrying one allow policy for the subject.
	if _, err := s.Publish(org, 1, allow); err != nil {
		t.Fatalf("[%s] publish allow v2: %v", shape, err)
	}
	if got := s.CurrentVersion(org); got != 2 {
		t.Fatalf("[%s] current = %d, want 2", shape, got)
	}
	// The allow really grants before the rollback, and the decision is
	// recorded (seq 3) as evidence the rollback must withdraw.
	before := s.Decide(org, req)
	if !before.Allowed || before.Reason != "matched allow policy" ||
		!reflect.DeepEqual(before.Matched, []string{"rollback-empty-allow"}) || before.Version != 2 {
		t.Fatalf("[%s] pre-rollback decision = %+v, want allow by v2", shape, before)
	}

	// Roll back to the empty historical version. It exists, so this must not
	// be ErrVersionNotFound, and it must publish a NEW version rather than
	// returning the target version number.
	newVersion, err := s.Rollback(org, 2, v1)
	if err != nil {
		t.Fatalf("[%s] rollback to empty v1: %v; an empty historical version is a real rollback target", shape, err)
	}
	if newVersion != 3 {
		t.Fatalf("[%s] rollback returned %d, want the new version 3 rather than target 1", shape, newVersion)
	}
	if got := s.CurrentVersion(org); got != 3 {
		t.Fatalf("[%s] current = %d, want 3 after rollback", shape, got)
	}

	// The new version's full policy set queries successfully and is empty;
	// it must not secretly retain the pre-rollback allow.
	gotPolicies, err := s.Policies(org, 3)
	if err != nil {
		t.Fatalf("[%s] Policies(v3): %v", shape, err)
	}
	if len(gotPolicies) != 0 {
		t.Fatalf("[%s] v3 policies = %+v, want an empty set", shape, gotPolicies)
	}

	// The identical, fully populated request is now denied by the new
	// version: default deny, no matches, and the rollback's version number.
	denied := s.Decide(org, req)
	wantDenied := Decision{
		Allowed: false,
		Reason:  "no matching allow policy",
		Matched: []string{},
		Version: 3,
	}
	if !reflect.DeepEqual(denied, wantDenied) {
		t.Fatalf("[%s] post-rollback decision = %+v, want %+v", shape, denied, wantDenied)
	}

	// Roles never feed organization policy evaluation: owner and a
	// scope-granting role change nothing after the empty rollback.
	ownerReq := req
	ownerReq.Subject.Roles = []string{"owner", "org/acme/ledger:read"}
	asOwner := s.Decide(org, ownerReq)
	if !reflect.DeepEqual(asOwner, wantDenied) {
		t.Fatalf("[%s] owner decision = %+v, roles must not change the org-policy denial %+v", shape, asOwner, wantDenied)
	}

	// The audit chain now holds six records: two publishes, the pre-rollback
	// allow decision, the rollback change, the post-rollback deny, and the
	// owner-role deny (recorded like every other Decide).
	recs := drainAudit(t, s, org, "", "")
	if len(recs) != 6 {
		t.Fatalf("[%s] audit records = %d, want 6", shape, len(recs))
	}
	for i, r := range recs {
		if r.Org != org || r.Seq != i+1 {
			t.Fatalf("[%s] record %d not gapless: %+v", shape, i+1, r)
		}
	}

	// The rollback change record carries the new version, empty content, and
	// an explicit rollback marker naming the empty source version.
	changeRec := recs[3]
	if changeRec.Kind != AuditPolicyChange || changeRec.Change == nil {
		t.Fatalf("[%s] seq 4 is not a policy change: %+v", shape, changeRec)
	}
	ch := changeRec.Change
	if ch.Version != 3 {
		t.Fatalf("[%s] rollback record version = %d, want 3", shape, ch.Version)
	}
	if len(ch.Policies) != 0 {
		t.Fatalf("[%s] rollback record policies = %+v, want empty", shape, ch.Policies)
	}
	if !ch.RolledBack || ch.SourceVersion != 1 {
		t.Fatalf("[%s] rollback record must be marked RolledBack with SourceVersion 1: %+v", shape, ch)
	}

	// The deny decision record preserves the request and the exact returned
	// reason/matches/version.
	denyRec := recs[4]
	if denyRec.Kind != AuditDecision || denyRec.Decision == nil {
		t.Fatalf("[%s] seq 5 is not a decision: %+v", shape, denyRec)
	}
	if !reflect.DeepEqual(denyRec.Decision.Request, req) {
		t.Fatalf("[%s] saved deny request = %+v, want %+v", shape, denyRec.Decision.Request, req)
	}
	if !reflect.DeepEqual(denyRec.Decision.Decision, denied) {
		t.Fatalf("[%s] saved deny decision = %+v, want %+v", shape, denyRec.Decision.Decision, denied)
	}

	// The owner-role request is recorded as the same default denial too.
	ownerRec := recs[5]
	if ownerRec.Kind != AuditDecision || ownerRec.Decision == nil {
		t.Fatalf("[%s] seq 6 is not a decision: %+v", shape, ownerRec)
	}
	if !reflect.DeepEqual(ownerRec.Decision.Request, ownerReq) ||
		!reflect.DeepEqual(ownerRec.Decision.Decision, asOwner) {
		t.Fatalf("[%s] owner record = %+v, want request %+v decision %+v",
			shape, ownerRec.Decision, ownerReq, asOwner)
	}

	// The pre-rollback allow record and historical versions are untouched.
	allowRec := recs[2]
	if allowRec.Kind != AuditDecision ||
		!reflect.DeepEqual(allowRec.Decision.Decision, before) {
		t.Fatalf("[%s] original allow record altered: %+v", shape, allowRec)
	}
	gotV1, err := s.Policies(org, 1)
	if err != nil || len(gotV1) != 0 {
		t.Fatalf("[%s] historical v1 = %v, %v; want empty set, no error", shape, gotV1, err)
	}
	gotV2, err := s.Policies(org, 2)
	if err != nil || !reflect.DeepEqual(gotV2, allow) {
		t.Fatalf("[%s] historical v2 = %+v, %v; want original allow intact", shape, gotV2, err)
	}
	if d := s.Review(org, 1, req); d.Allowed || d.Version != 1 || d.Reason != "no matching allow policy" {
		t.Fatalf("[%s] review empty v1 = %+v", shape, d)
	}
	if d := s.Review(org, 2, req); !d.Allowed || d.Version != 2 {
		t.Fatalf("[%s] review v2 = %+v, want the original allow", shape, d)
	}

	// Online recheck of the post-rollback deny reproduces it exactly.
	rechecked, err := s.RecheckDecision(org, denyRec.Seq)
	if err != nil {
		t.Fatalf("[%s] RecheckDecision(seq5): %v; emptiness must not read as a missing version", shape, err)
	}
	if !reflect.DeepEqual(rechecked, denied) {
		t.Fatalf("[%s] online recheck = %+v, want %+v", shape, rechecked, denied)
	}
	// The earlier allow decision still rechecks to the allow on version 2.
	recheckedAllow, err := s.RecheckDecision(org, allowRec.Seq)
	if err != nil {
		t.Fatalf("[%s] RecheckDecision(seq3): %v", shape, err)
	}
	if !reflect.DeepEqual(recheckedAllow, before) {
		t.Fatalf("[%s] old allow recheck = %+v, want %+v", shape, recheckedAllow, before)
	}

	// Full export with a separately retained checkpoint: the whole chain
	// verifies, and offline recheck of the deny succeeds — emptiness must
	// never surface as ErrVersionNotFound, and no other (allow) version may
	// be substituted for version 3.
	exported, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatalf("[%s] AuditExport: %v", shape, err)
	}
	if err := VerifyAudit(org, exported, cp); err != nil {
		t.Fatalf("[%s] VerifyAudit: %v", shape, err)
	}
	offline, err := RecheckDecisionOffline(org, exported, cp, denyRec.Seq)
	if err != nil {
		t.Fatalf("[%s] offline review of empty-version deny: %v", shape, err)
	}
	if !offline.Consistent {
		t.Fatalf("[%s] offline review inconsistent: original=%+v recomputed=%+v",
			shape, offline.Original, offline.Recomputed)
	}
	if !reflect.DeepEqual(offline.Original, denied) || !reflect.DeepEqual(offline.Recomputed, denied) {
		t.Fatalf("[%s] offline review = original %+v recomputed %+v, want %+v",
			shape, offline.Original, offline.Recomputed, denied)
	}
	// Offline, the older allow still replays on version 2.
	offlineAllow, err := RecheckDecisionOffline(org, exported, cp, allowRec.Seq)
	if err != nil || !offlineAllow.Consistent || !reflect.DeepEqual(offlineAllow.Recomputed, before) {
		t.Fatalf("[%s] offline old-allow review = %+v, %v", shape, offlineAllow, err)
	}

	// Round-trip through the file archive, validating only against the
	// retained checkpoint, and re-review purely from the decoded material.
	archive, err := EncodeAuditArchive(org, exported, cp)
	if err != nil {
		t.Fatalf("[%s] EncodeAuditArchive: %v", shape, err)
	}
	decoded, err := DecodeAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatalf("[%s] DecodeAuditArchive: %v", shape, err)
	}
	fromArchive, err := RecheckDecisionOffline(org, decoded, cp, denyRec.Seq)
	if err != nil {
		t.Fatalf("[%s] offline review from archive: %v", shape, err)
	}
	if !fromArchive.Consistent || !reflect.DeepEqual(fromArchive.Recomputed, denied) {
		t.Fatalf("[%s] archive review = %+v, want consistent default deny on v3", shape, fromArchive)
	}

	// Preserved failure behavior. A missing target version is not found...
	if _, err := s.Rollback(org, 3, 99); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("[%s] rollback to missing target err = %v, want ErrVersionNotFound", shape, err)
	}
	// ...version 0 is "never published", not an empty historical version: the
	// empty set must be a genuinely published version to serve as a target.
	if _, err := s.Rollback(org, 3, 0); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("[%s] rollback to version 0 err = %v, want ErrVersionNotFound", shape, err)
	}
	// An empty version of another organization is not a target here: each
	// organization's version history is independent even at the same number.
	if _, err := s.Rollback("rollback-empty-other", 0, 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("[%s] cross-org empty target err = %v, want ErrVersionNotFound", shape, err)
	}
	// ...and a stale expected version conflicts even when the target is the
	// empty version. Neither failure changes state, the version number, or
	// the audit chain.
	if _, err := s.Rollback(org, 2, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("[%s] rollback with stale expected version err = %v, want ErrVersionConflict", shape, err)
	}
	if got := s.CurrentVersion(org); got != 3 {
		t.Fatalf("[%s] current = %d after failed rollbacks, want 3", shape, got)
	}
	stillEmpty, err := s.Policies(org, 3)
	if err != nil || len(stillEmpty) != 0 {
		t.Fatalf("[%s] v3 = %+v, %v after failed rollbacks; want empty", shape, stillEmpty, err)
	}
	// A final decision is still the same new-version default denial.
	final := s.Decide(org, req)
	if !reflect.DeepEqual(final, wantDenied) {
		t.Fatalf("[%s] decision after failed rollbacks = %+v, want %+v", shape, final, wantDenied)
	}
	if after := drainAudit(t, s, org, "", ""); len(after) != 7 {
		t.Fatalf("[%s] audit records = %d after the final decision, want 7 (failed rollbacks append nothing)", shape, len(after))
	}
	// The last record is the post-failure decision, unchanged from the first
	// post-rollback default denial.
	allAfter := drainAudit(t, s, org, AuditDecision, "")
	last := allAfter[len(allAfter)-1]
	if last.Seq != 7 || !reflect.DeepEqual(last.Decision.Decision, wantDenied) {
		t.Fatalf("[%s] final decision record = %+v, want seq 7 default deny %+v", shape, last, wantDenied)
	}
}

func TestRollbackToEmptyNilPolicyVersion(t *testing.T) {
	checkRollbackToEmptyVersion(t, nil, "nil")
}

func TestRollbackToExplicitEmptyPolicyVersion(t *testing.T) {
	checkRollbackToEmptyVersion(t, []Policy{}, "explicit-empty")
}
