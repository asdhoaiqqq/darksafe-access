// Regression coverage for atomic policy-set publishing. A batch replaces the
// current version wholesale: when ANY policy in a submitted batch is illegal,
// none of the batch may take effect, even if every earlier policy was valid.
// These tests observe the real externally visible state around such a failed
// submission — access outcomes and their explanations, the current and
// historical versions, and the audit chain — rather than only the returned
// error, so a regression that let part of a failed batch go live is caught.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

const atomicOrg = "atomic"

// atomicLedgerScope and atomicLedgerResource are the resource used to tell
// the old and candidate authorizations apart.
const (
	atomicLedgerScope    = "org/atomic/ledger"
	atomicLedgerResource = "ledger-main"
	atomicSubject        = "alice"
)

// atomicV1Policies is the already published version: alice may READ the
// ledger and is explicitly DENIED write. The two policies carry distinct
// identifiers that must remain the only matched identifiers after a failed
// replacement.
func atomicV1Policies() []Policy {
	return []Policy{
		{ID: "v1-allow-read", Subject: atomicSubject, Action: "read", Scope: atomicLedgerScope, Effect: EffectAllow},
		{ID: "v1-deny-write", Subject: atomicSubject, Action: "write", Scope: atomicLedgerScope, Effect: EffectDeny},
	}
}

// atomicCandidateCore is the legal prefix shared by every failing batch: it
// flips both authorizations relative to v1 (read denied, write allowed) and
// uses identifiers that exist nowhere in the published history.
func atomicCandidateCore() []Policy {
	return []Policy{
		{ID: "v2-deny-read", Subject: atomicSubject, Action: "read", Scope: atomicLedgerScope, Effect: EffectDeny},
		{ID: "v2-allow-write", Subject: atomicSubject, Action: "write", Scope: atomicLedgerScope, Effect: EffectAllow},
	}
}

// atomicExtraPolicy is an unrelated legal policy for another subject that
// occupies the final slot of the batch; the failure cases derive it from the
// legal form and the repair cases restore exactly this content.
func atomicExtraPolicy(id string) Policy {
	return Policy{ID: id, Subject: "bob", Action: "read", Scope: "org/atomic/reports", Effect: EffectAllow}
}

func atomicRequest(action string) OrgRequest {
	return request(atomicOrg, atomicSubject, atomicLedgerResource, atomicLedgerScope, action)
}

// setupAtomicStore publishes v1 and returns the store together with a
// detached copy of exactly what v1 must contain forever after.
func setupAtomicStore(t *testing.T) (*Store, []Policy) {
	t.Helper()
	s := NewStore()
	v1 := atomicV1Policies()
	v, err := s.Publish(atomicOrg, 0, v1)
	if err != nil || v != 1 {
		t.Fatalf("publish v1 = %d, %v; want 1, nil", v, err)
	}
	// Fixture invariants: read allowed, write denied, both explained by v1.
	if d := s.Decide(atomicOrg, atomicRequest("read")); !d.Allowed || d.Version != 1 ||
		d.Reason != "matched allow policy" || !reflect.DeepEqual(d.Matched, []string{"v1-allow-read"}) {
		t.Fatalf("fixture read decision = %+v, want v1 allow via v1-allow-read", d)
	}
	if d := s.Decide(atomicOrg, atomicRequest("write")); d.Allowed || d.Version != 1 ||
		d.Reason != "matched deny policy" || !reflect.DeepEqual(d.Matched, []string{"v1-deny-write"}) {
		t.Fatalf("fixture write decision = %+v, want v1 deny via v1-deny-write", d)
	}
	return s, append([]Policy(nil), v1...)
}

// TestFailedBatchPublishChangesNothing drives both illegal-tail shapes
// through the same post-failure contract:
//
//   - the tail policy's scope contains consecutive slashes, and
//   - the tail policy reuses an earlier policy's identifier while all its
//     other fields are legal, so the duplicate cannot be excused as an
//     overwrite of the earlier entry.
//
// The correct current version is supplied, so the failure must be classified
// as ErrInvalidPolicySet, never as a version conflict.
func TestFailedBatchPublishChangesNothing(t *testing.T) {
	cases := map[string]struct {
		invalid []Policy
		fixed   []Policy
	}{
		"tail scope with consecutive slashes": {
			invalid: append(atomicCandidateCore(), func() Policy {
				p := atomicExtraPolicy("v2-extra-bob-read")
				p.Scope = "org/atomic//reports"
				return p
			}()),
			fixed: append(atomicCandidateCore(), atomicExtraPolicy("v2-extra-bob-read")),
		},
		"tail duplicates an earlier policy id": {
			// Every field of the tail is legal on its own; only the reused
			// identifier is illegal, and it matches the FIRST policy of the
			// batch rather than being allowed to overwrite it.
			invalid: append(atomicCandidateCore(), atomicExtraPolicy("v2-deny-read")),
			fixed:   append(atomicCandidateCore(), atomicExtraPolicy("v2-extra-bob-read")),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, v1 := setupAtomicStore(t)

			// Pin the audit chain exactly as it stands before the failed
			// submission: the v1 policy-change record plus the two fixture
			// decisions (read seq 2, write seq 3).
			before, cpBefore, err := s.AuditExport(atomicOrg, 0)
			if err != nil {
				t.Fatalf("AuditExport before failure: %v", err)
			}
			if len(before) != 3 || cpBefore.EndSeq != 3 {
				t.Fatalf("chain before failure = %d records, end seq %d; want 3, 3", len(before), cpBefore.EndSeq)
			}

			// Submit the batch with the CORRECT current version; it must be
			// rejected as an invalid set, not as a stale version, and return
			// no usable new version.
			newVersion, err := s.Publish(atomicOrg, 1, tc.invalid)
			if !errors.Is(err, ErrInvalidPolicySet) {
				t.Fatalf("failed publish err = %v, want ErrInvalidPolicySet", err)
			}
			if errors.Is(err, ErrVersionConflict) {
				t.Fatalf("invalid batch was classified as a version conflict: %v", err)
			}
			if newVersion != 0 {
				t.Fatalf("failed publish returned version %d, want 0", newVersion)
			}

			// Version state is untouched: current stays 1, the published v1
			// content is byte-for-byte intact, and the would-be v2 does not
			// exist with the standard not-found error.
			if got := s.CurrentVersion(atomicOrg); got != 1 {
				t.Fatalf("current version after failure = %d, want 1", got)
			}
			gotV1, err := s.Policies(atomicOrg, 1)
			if err != nil {
				t.Fatalf("Policies(v1) after failure: %v", err)
			}
			if !reflect.DeepEqual(gotV1, v1) {
				t.Fatalf("v1 content changed after failed publish:\n got %+v\nwant %+v", gotV1, v1)
			}
			if _, err := s.Policies(atomicOrg, 2); !errors.Is(err, ErrVersionNotFound) {
				t.Fatalf("would-be v2 is queryable (err=%v); want ErrVersionNotFound", err)
			}

			// The failed submission appended no policy-change record and left
			// the head sequence and checkpoint fingerprint exactly as they
			// were.
			after, cpAfter, err := s.AuditExport(atomicOrg, 0)
			if err != nil {
				t.Fatalf("AuditExport after failure: %v", err)
			}
			if len(after) != len(before) || !reflect.DeepEqual(after, before) {
				t.Fatalf("audit records changed with the failed submit:\n after %+v\nbefore %+v", after, before)
			}
			if cpAfter != cpBefore {
				t.Fatalf("checkpoint changed after failed submit:\n after %+v\nbefore %+v", cpAfter, cpBefore)
			}
			if changes := drainAudit(t, s, atomicOrg, AuditPolicyChange, ""); len(changes) != 1 {
				t.Fatalf("policy-change records after failure = %d, want 1", len(changes))
			}

			// Actual access still follows the pre-submission policies: read
			// authorized by the old allow, write stopped by the old deny. The
			// reason, matched identifiers and reported version must all come
			// from v1; an identifier belonging only to the failed set would
			// prove partial application.
			readD := s.Decide(atomicOrg, atomicRequest("read"))
			if !readD.Allowed || readD.Reason != "matched allow policy" || readD.Version != 1 ||
				!reflect.DeepEqual(readD.Matched, []string{"v1-allow-read"}) {
				t.Fatalf("read after failure = %+v, want v1 allow via v1-allow-read", readD)
			}
			writeD := s.Decide(atomicOrg, atomicRequest("write"))
			if writeD.Allowed || writeD.Reason != "matched deny policy" || writeD.Version != 1 ||
				!reflect.DeepEqual(writeD.Matched, []string{"v1-deny-write"}) {
				t.Fatalf("write after failure = %+v, want v1 deny via v1-deny-write", writeD)
			}
			for _, d := range []Decision{readD, writeD} {
				for _, id := range d.Matched {
					if id == "v2-deny-read" || id == "v2-allow-write" || id == "v2-extra-bob-read" {
						t.Fatalf("decision matched identifier %q from the failed batch: %+v", id, d)
					}
				}
			}

			// The two observation decisions append normal decision records,
			// gaplessly after the untouched head (seqs 4 and 5), each
			// persisting exactly the result and version that was returned, and
			// the full chain still verifies offline.
			obs, obsCP, err := s.AuditExport(atomicOrg, 0)
			if err != nil {
				t.Fatalf("AuditExport after observation: %v", err)
			}
			if len(obs) != 5 || obsCP.EndSeq != 5 {
				t.Fatalf("records after observation = %d, end seq %d; want 5, 5", len(obs), obsCP.EndSeq)
			}
			wantObserved := []struct {
				seq int
				d   Decision
			}{{4, readD}, {5, writeD}}
			for _, w := range wantObserved {
				r := obs[w.seq-1]
				if r.Seq != w.seq || r.Kind != AuditDecision || r.Decision == nil {
					t.Fatalf("record %d = %+v, want decision record", w.seq, r)
				}
				if !reflect.DeepEqual(r.Decision.Decision, w.d) {
					t.Fatalf("saved decision at seq %d = %+v, want %+v", w.seq, r.Decision.Decision, w.d)
				}
				if r.Decision.Decision.Version != w.d.Version {
					t.Fatalf("saved decision version at seq %d = %d, want %d", w.seq, r.Decision.Decision.Version, w.d.Version)
				}
			}
			if err := VerifyAudit(atomicOrg, obs, obsCP); err != nil {
				t.Fatalf("audit chain does not verify after failed publish: %v", err)
			}

			// Repair the illegal content and resubmit against the SAME
			// pre-failure current version. It publishes as the immediate
			// successor of v1.
			v2, err := s.Publish(atomicOrg, 1, tc.fixed)
			if err != nil || v2 != 2 {
				t.Fatalf("repaired publish = %d, %v; want 2, nil", v2, err)
			}
			gotV2, err := s.Policies(atomicOrg, 2)
			if err != nil {
				t.Fatalf("Policies(v2): %v", err)
			}
			if !reflect.DeepEqual(gotV2, tc.fixed) {
				t.Fatalf("v2 content = %+v, want the full repaired set %+v", gotV2, tc.fixed)
			}
			// The duplicate-id case must keep BOTH entries once the tail gets
			// its own identifier: the earlier policy was never overwritten.
			if len(gotV2) != 3 {
				t.Fatalf("v2 has %d policies, want the complete 3-policy set", len(gotV2))
			}
			// v1 remains queryable with its original content.
			gotV1, err = s.Policies(atomicOrg, 1)
			if err != nil {
				t.Fatalf("Policies(v1) after repaired publish: %v", err)
			}
			if !reflect.DeepEqual(gotV1, v1) {
				t.Fatalf("old v1 content changed:\n got %+v\nwant %+v", gotV1, v1)
			}

			// Exactly one new policy-change record exists: seq 6, naming v2
			// and carrying the complete new set as an ordinary publish.
			final, finalCP, err := s.AuditExport(atomicOrg, 0)
			if err != nil {
				t.Fatalf("final AuditExport: %v", err)
			}
			if len(final) != 6 {
				t.Fatalf("final records = %d, want 6", len(final))
			}
			changeRec := final[5]
			if changeRec.Seq != 6 || changeRec.Kind != AuditPolicyChange || changeRec.Change == nil {
				t.Fatalf("record 6 = %+v, want policy_change", changeRec)
			}
			ch := changeRec.Change
			if ch.Version != 2 || ch.RolledBack || ch.SourceVersion != 0 {
				t.Fatalf("new change payload = %+v, want ordinary publish of v2", ch)
			}
			if !reflect.DeepEqual(ch.Policies, tc.fixed) {
				t.Fatalf("change record policies = %+v, want %+v", ch.Policies, tc.fixed)
			}
			if changes := drainAudit(t, s, atomicOrg, AuditPolicyChange, ""); len(changes) != 2 {
				t.Fatalf("policy-change records = %d, want exactly 2 (v1 and v2)", len(changes))
			}
			if err := VerifyAudit(atomicOrg, final, finalCP); err != nil {
				t.Fatalf("final audit chain does not verify: %v", err)
			}

			// Authorization flips to the new version: read denied, write
			// allowed, explained by v2 identifiers at version 2.
			newRead := s.Decide(atomicOrg, atomicRequest("read"))
			if newRead.Allowed || newRead.Reason != "matched deny policy" || newRead.Version != 2 ||
				!reflect.DeepEqual(newRead.Matched, []string{"v2-deny-read"}) {
				t.Fatalf("read after repaired publish = %+v, want v2 deny via v2-deny-read", newRead)
			}
			newWrite := s.Decide(atomicOrg, atomicRequest("write"))
			if !newWrite.Allowed || newWrite.Reason != "matched allow policy" || newWrite.Version != 2 ||
				!reflect.DeepEqual(newWrite.Matched, []string{"v2-allow-write"}) {
				t.Fatalf("write after repaired publish = %+v, want v2 allow via v2-allow-write", newWrite)
			}
			// The post-repair observation decisions persist their actual
			// result and version too. drainAudit returns every decision
			// record (fixture seqs 2,3, post-failure seqs 4,5, and the new
			// seqs 7,8) in chain order; index them by sequence.
			all := drainAudit(t, s, atomicOrg, AuditDecision, "")
			if len(all) != 6 {
				t.Fatalf("decision records = %d, want 6", len(all))
			}
			bySeq := map[int]AuditRecord{}
			for _, r := range all {
				bySeq[r.Seq] = r
			}
			wantNew := []struct {
				seq int
				d   Decision
			}{{7, newRead}, {8, newWrite}}
			for _, w := range wantNew {
				r, ok := bySeq[w.seq]
				if !ok {
					t.Fatalf("missing decision record at seq %d", w.seq)
				}
				if !reflect.DeepEqual(r.Decision.Decision, w.d) {
					t.Fatalf("saved decision at seq %d = %+v, want %+v", w.seq, r.Decision.Decision, w.d)
				}
			}

			// The post-failure observation records still re-decide against
			// the v1 policies they actually used: the later publish cannot
			// rewrite historical judgments.
			if d, err := s.RecheckDecision(atomicOrg, 4); err != nil || !reflect.DeepEqual(d, readD) {
				t.Fatalf("recheck seq 4 = %+v, %v; want original %+v", d, err, readD)
			}
			if d, err := s.RecheckDecision(atomicOrg, 5); err != nil || !reflect.DeepEqual(d, writeD) {
				t.Fatalf("recheck seq 5 = %+v, %v; want original %+v", d, err, writeD)
			}
			if d := s.Review(atomicOrg, 1, atomicRequest("read")); !reflect.DeepEqual(d, readD) {
				t.Fatalf("review v1 read = %+v, want %+v", d, readD)
			}
			if d := s.Review(atomicOrg, 1, atomicRequest("write")); !reflect.DeepEqual(d, writeD) {
				t.Fatalf("review v1 write = %+v, want %+v", d, writeD)
			}
		})
	}
}
