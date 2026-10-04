// Regression coverage for the atomicity of a FAILED policy-set publish. A
// batch submitted to replace the current version is all-or-nothing: when any
// policy in the set is invalid, the whole commit is rejected with
// ErrInvalidPolicySet and no prefix of the valid leading policies may take
// effect. These tests pin the complete failure contract from an organization
// with a live, authorizing version:
//
//   - the error is ErrInvalidPolicySet, never ErrVersionConflict, and no
//     usable new version number comes back;
//   - authorization afterwards is still decided by the pre-commit version,
//     with reasons, matched policy ids and the version all from the old set
//     and no hit identifier that exists only in the failed set;
//   - the current version, every published version's content and the
//     would-be next version's absence are unchanged;
//   - the failed commit appends no policy-change record and leaves the
//     existing records, the head sequence and the checkpoint fingerprint
//     bit-for-bit intact, while ordinary decisions made afterwards still
//     record exactly what was returned;
//   - resubmitting the corrected set against the pre-failure current version
//     then succeeds as the immediate next version, flips authorization, and
//     adds exactly one policy-change record for the full new set.
//
// Two invalid shapes are covered: a trailing policy whose scope contains
// consecutive slashes, and a trailing policy that reuses the identifier of
// an earlier policy in the SAME submitted set. The duplicate's other fields
// are all valid and differ from the first occurrence, so the collision
// cannot be silently eliminated by overwriting the earlier policy.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

const atomicOrg = "acme"

// atomicV1 is the originally published set: the probe subject may read the
// ledger and is explicitly denied writes.
func atomicV1() []Policy {
	return []Policy{
		{ID: "v1-allow-read", Subject: "u1", Action: "read", Scope: "org/acme/ledger", Effect: EffectAllow},
		{ID: "v1-deny-write", Subject: "u1", Action: "write", Scope: "org/acme/ledger", Effect: EffectDeny},
	}
}

// atomicCandidatePrefix is the valid leading part of the replacing batch: it
// flips the probe's outcomes (read denied, write allowed), so any partial
// application of the batch is observable in the post-failure decisions.
func atomicCandidatePrefix() []Policy {
	return []Policy{
		{ID: "v2-deny-read", Subject: "u1", Action: "read", Scope: "org/acme/ledger", Effect: EffectDeny},
		{ID: "v2-allow-write", Subject: "u1", Action: "write", Scope: "org/acme/ledger", Effect: EffectAllow},
	}
}

// atomicInvalidTails are the rejected final policies, one per subtest.
func atomicInvalidTails() map[string]Policy {
	return map[string]Policy{
		"consecutive slashes in scope": {
			ID: "v2-extra", Subject: "u1", Action: "read",
			Scope: "org/acme//ledger", Effect: EffectAllow,
		},
		// Every field but the identifier is valid, and the content differs
		// from the earlier "v2-deny-read" entry, so the duplicate is a real
		// second policy that cannot be merged away.
		"duplicate id within the set": {
			ID: "v2-deny-read", Subject: "u1", Action: "write",
			Scope: "org/acme/ledger", Effect: EffectDeny,
		},
	}
}

// atomicFixedTail replaces the invalid final policy in the corrected
// resubmission: valid, and scoped to a different subject so it never
// interferes with the probe decisions pinned below.
func atomicFixedTail() Policy {
	return Policy{ID: "v2-extra", Subject: "u2", Action: "read", Scope: "org/acme/ledger", Effect: EffectAllow}
}

func atomicReadReq() OrgRequest {
	return request(atomicOrg, "u1", "ledger-1", "org/acme/ledger", "read")
}

func atomicWriteReq() OrgRequest {
	return request(atomicOrg, "u1", "ledger-1", "org/acme/ledger", "write")
}

// TestFailedPublishLeavesAuthorizationAndAuditUntouched runs the full
// failure-and-recovery contract for each invalid tail shape.
func TestFailedPublishLeavesAuthorizationAndAuditUntouched(t *testing.T) {
	for name, badTail := range atomicInvalidTails() {
		t.Run(name, func(t *testing.T) {
			s := NewStore()
			v1 := atomicV1()
			if v, err := s.Publish(atomicOrg, 0, v1); err != nil || v != 1 {
				t.Fatalf("publish v1 = %d, %v; want 1, nil", v, err)
			}

			readReq, writeReq := atomicReadReq(), atomicWriteReq()
			// The pre-commit authorization outcomes, explained entirely by v1.
			wantRead := Decision{
				Allowed: true, Reason: "matched allow policy",
				Matched: []string{"v1-allow-read"}, Version: 1,
			}
			wantWrite := Decision{
				Allowed: false, Reason: "matched deny policy",
				Matched: []string{"v1-deny-write"}, Version: 1,
			}
			if d := s.Decide(atomicOrg, readReq); !reflect.DeepEqual(d, wantRead) {
				t.Fatalf("baseline read = %+v, want %+v", d, wantRead)
			}
			if d := s.Decide(atomicOrg, writeReq); !reflect.DeepEqual(d, wantWrite) {
				t.Fatalf("baseline write = %+v, want %+v", d, wantWrite)
			}

			// Snapshot the audit state the failed commit must preserve:
			// publish v1 (seq 1) and the two baseline decisions (seq 2, 3).
			preRecs, preCP, err := s.AuditExport(atomicOrg, 0)
			if err != nil {
				t.Fatalf("AuditExport before failure: %v", err)
			}
			if len(preRecs) != 3 || preCP.EndSeq != 3 {
				t.Fatalf("records before failure = %d, checkpoint %+v; want 3 records ending at seq 3",
					len(preRecs), preCP)
			}
			if err := VerifyAudit(atomicOrg, preRecs, preCP); err != nil {
				t.Fatalf("chain does not verify before failure: %v", err)
			}

			// The failing commit: valid leading policies that would flip both
			// outcomes, then the invalid final policy, submitted against the
			// correct current version.
			candidate := append(atomicCandidatePrefix(), badTail)
			v, err := s.Publish(atomicOrg, 1, candidate)
			if !errors.Is(err, ErrInvalidPolicySet) {
				t.Fatalf("publish err = %v, want ErrInvalidPolicySet", err)
			}
			if errors.Is(err, ErrVersionConflict) {
				t.Fatalf("publish err = %v must not be reported as a version conflict", err)
			}
			if v != 0 {
				t.Fatalf("failed publish returned version %d; no usable new version may come back", v)
			}

			// Version state is exactly as before the attempt.
			if got := s.CurrentVersion(atomicOrg); got != 1 {
				t.Fatalf("current version = %d, want 1 after failed publish", got)
			}
			gotV1, err := s.Policies(atomicOrg, 1)
			if err != nil || !reflect.DeepEqual(gotV1, v1) {
				t.Fatalf("Policies(v1) = %+v, %v; want the original set, nil", gotV1, err)
			}
			if _, err := s.Policies(atomicOrg, 2); !errors.Is(err, ErrVersionNotFound) {
				t.Fatalf("version 2 exists after failed publish, err = %v; want ErrVersionNotFound", err)
			}

			// The failed commit left no trace in the audit chain: the
			// records, the head sequence and the checkpoint fingerprint are
			// bit-for-bit the pre-attempt ones.
			postRecs, postCP, err := s.AuditExport(atomicOrg, 0)
			if err != nil {
				t.Fatalf("AuditExport after failure: %v", err)
			}
			if postCP != preCP {
				t.Fatalf("checkpoint changed after failed publish: %+v vs %+v", preCP, postCP)
			}
			if !reflect.DeepEqual(postRecs, preRecs) {
				t.Fatalf("audit records changed after failed publish")
			}
			if err := VerifyAudit(atomicOrg, postRecs, postCP); err != nil {
				t.Fatalf("chain does not verify after failed publish: %v", err)
			}

			// Authorization is still decided by v1 alone: the explanations
			// carry only v1's reasons, hit identifiers and version, never an
			// identifier that exists only in the failed set.
			gotRead := s.Decide(atomicOrg, readReq)
			if !reflect.DeepEqual(gotRead, wantRead) {
				t.Fatalf("read after failure = %+v, want %+v from v1", gotRead, wantRead)
			}
			gotWrite := s.Decide(atomicOrg, writeReq)
			if !reflect.DeepEqual(gotWrite, wantWrite) {
				t.Fatalf("write after failure = %+v, want %+v from v1", gotWrite, wantWrite)
			}

			// Those observation decisions are still recorded under the
			// existing rules, and each record persists exactly the decision
			// that was returned, including its version.
			decisions := drainAudit(t, s, atomicOrg, AuditDecision, "")
			if len(decisions) != 4 {
				t.Fatalf("decision records = %d, want 4", len(decisions))
			}
			for i, want := range []Decision{wantRead, wantWrite, gotRead, gotWrite} {
				dr := decisions[i]
				if dr.Seq != i+2 || !reflect.DeepEqual(dr.Decision.Decision, want) {
					t.Fatalf("decision record %d = seq %d %+v, want seq %d storing %+v",
						i, dr.Seq, dr.Decision, i+2, want)
				}
			}

			// Correct the invalid tail and resubmit the same batch against
			// the pre-failure current version: it publishes as the immediate
			// next version.
			fixed := append(atomicCandidatePrefix(), atomicFixedTail())
			v, err = s.Publish(atomicOrg, 1, fixed)
			if err != nil || v != 2 {
				t.Fatalf("corrected publish = %d, %v; want 2, nil", v, err)
			}
			if got := s.CurrentVersion(atomicOrg); got != 2 {
				t.Fatalf("current version = %d, want 2 after corrected publish", got)
			}

			// Authorization flips exactly as the new set dictates.
			wantReadV2 := Decision{
				Allowed: false, Reason: "matched deny policy",
				Matched: []string{"v2-deny-read"}, Version: 2,
			}
			wantWriteV2 := Decision{
				Allowed: true, Reason: "matched allow policy",
				Matched: []string{"v2-allow-write"}, Version: 2,
			}
			if d := s.Decide(atomicOrg, readReq); !reflect.DeepEqual(d, wantReadV2) {
				t.Fatalf("read after corrected publish = %+v, want %+v", d, wantReadV2)
			}
			if d := s.Decide(atomicOrg, writeReq); !reflect.DeepEqual(d, wantWriteV2) {
				t.Fatalf("write after corrected publish = %+v, want %+v", d, wantWriteV2)
			}

			// Exactly one policy-change record was added for the corrected
			// commit, carrying the complete new set; the v1 record is intact.
			changes := drainAudit(t, s, atomicOrg, AuditPolicyChange, "")
			if len(changes) != 2 {
				t.Fatalf("policy-change records = %d, want 2", len(changes))
			}
			if changes[0].Change.Version != 1 || !reflect.DeepEqual(changes[0].Change.Policies, v1) {
				t.Fatalf("v1 change record altered: %+v", changes[0].Change)
			}
			chg := changes[1].Change
			if chg.Version != 2 || chg.RolledBack || chg.SourceVersion != 0 {
				t.Fatalf("new change record = %+v, want plain publish of version 2", chg)
			}
			if !reflect.DeepEqual(chg.Policies, fixed) {
				t.Fatalf("new change policies = %+v, want the full corrected set %+v", chg.Policies, fixed)
			}

			// The old version still serves its original content, and the
			// final chain verifies end to end.
			gotV1, err = s.Policies(atomicOrg, 1)
			if err != nil || !reflect.DeepEqual(gotV1, v1) {
				t.Fatalf("Policies(v1) after corrected publish = %+v, %v; want the original set", gotV1, err)
			}
			finalRecs, finalCP, err := s.AuditExport(atomicOrg, 0)
			if err != nil {
				t.Fatalf("final AuditExport: %v", err)
			}
			if err := VerifyAudit(atomicOrg, finalRecs, finalCP); err != nil {
				t.Fatalf("final chain does not verify: %v", err)
			}
		})
	}
}
