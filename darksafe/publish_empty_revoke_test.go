// Regression coverage for revoking an organization's existing access by
// publishing the complete policy set as empty. An empty published set is a
// genuine version, not the absence of one: before the revocation the probe
// request is allowed by the matching allow policy at the allow version;
// after it the same request is default-denied at the new empty version —
// never at version 0 (nothing published) and never with the old policy's
// matched identifiers left over.
//
// The revocation publish and the read decisions share one organization
// audit chain, and the chain is the arbiter when they overlap: a decision
// record positioned before the revocation record must rest on the allow
// version, one positioned after it must rest on the empty version. A later
// decision may never keep using the old allow version, and an earlier
// decision may never carry a version that had not been published yet. Each
// request's returned result and its recorded decision must agree exactly on
// allowed, reason, matched list and actual version, and one successful
// revocation produces exactly one new version and one policy-change record.
//
// Everything here is judged through the public business surface — Publish,
// Decide, CurrentVersion, Policies, the audit query/export entries and the
// recheck entries — with no change to the authorization rules.
package darksafe

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

// revokeEmptyShapes are the ways "no policies" reaches Publish; every shape
// is authorization-equivalent and must revoke identically.
func revokeEmptyShapes() map[string][]Policy {
	return map[string][]Policy{
		"nil slice":      nil,
		"empty slice":    {},
		"empty make":     make([]Policy, 0),
		"empty with cap": make([]Policy, 0, 4),
	}
}

// revokeAllowSet is the single allow policy the empty publish revokes.
func revokeAllowSet() []Policy {
	return []Policy{allowPolicy("p-ledger-read", "u1", "read", "org/acme/ledger", false)}
}

// revokeReadRequest is the fully populated, organization-consistent read
// used throughout: one enabled subject, one resource, all three
// organizations equal, valid identifiers, action and scope.
func revokeReadRequest(org string) OrgRequest {
	return request(org, "u1", "ledger-1", "org/acme/ledger", "read")
}

// revokeAllowDecision is the only acceptable pre-revocation outcome: allowed
// by the matching allow policy at version 1.
func revokeAllowDecision() Decision {
	return Decision{
		Allowed: true, Reason: "matched allow policy",
		Matched: []string{"p-ledger-read"}, Version: 1,
	}
}

// revokeDenyDecision is the only acceptable post-revocation outcome:
// default-denied at the new empty version 2 — not version 0, not the old
// allow version, and with no matched identifiers carried over.
func revokeDenyDecision() Decision {
	return Decision{
		Allowed: false, Reason: "no matching allow policy",
		Matched: []string{}, Version: 2,
	}
}

// checkRevokeDecisionRecord pins one decision record to the exact request
// and decision the scenario allows at its chain position.
func checkRevokeDecisionRecord(t *testing.T, rec AuditRecord, req OrgRequest, want Decision) {
	t.Helper()
	if rec.Kind != AuditDecision || rec.Decision == nil {
		t.Fatalf("record %d is not a decision record: %+v", rec.Seq, rec)
	}
	if !reflect.DeepEqual(rec.Decision.Request, req) {
		t.Fatalf("record %d request = %+v, want %+v", rec.Seq, rec.Decision.Request, req)
	}
	if !reflect.DeepEqual(rec.Decision.Decision, want) {
		t.Fatalf("record %d decision = %+v, want %+v", rec.Seq, rec.Decision.Decision, want)
	}
	if rec.Decision.Decision.Version == 0 {
		t.Fatalf("record %d fell back to version 0 (no published version)", rec.Seq)
	}
}

// setupRevokeStore publishes the allow set as version 1 and returns the
// store, the probe request and the decision the probe must see until the
// revocation lands.
func setupRevokeStore(t *testing.T, org string) (*Store, OrgRequest, Decision) {
	t.Helper()
	s := NewStore()
	if v, err := s.Publish(org, 0, revokeAllowSet()); err != nil || v != 1 {
		t.Fatalf("publish allow v1 = %d, %v; want 1, nil", v, err)
	}
	req := revokeReadRequest(org)
	wantAllow := revokeAllowDecision()
	if d := evaluate(req, revokeAllowSet(), 1); !reflect.DeepEqual(d, wantAllow) {
		t.Fatalf("fixture: allow set must allow the probe at v1, got %+v", d)
	}
	return s, req, wantAllow
}

// TestPublishEmptySetRevokesAccess pins the non-overlapping contract: a read
// completed before the revocation starts keeps its allow conclusion, a read
// submitted only after the revocation publish returned is always denied at
// the empty version, and the shared audit chain records exactly one new
// version, one policy-change record and one complete decision record per
// request.
func TestPublishEmptySetRevokesAccess(t *testing.T) {
	for shape, emptyPolicies := range revokeEmptyShapes() {
		t.Run(shape, func(t *testing.T) {
			org := "acme-revoke-" + shape
			s, req, wantAllow := setupRevokeStore(t, org)
			allow := revokeAllowSet()

			// Boundary 1: the read completes before the revocation starts.
			before := s.Decide(org, req)
			if !reflect.DeepEqual(before, wantAllow) {
				t.Fatalf("pre-revocation decision = %+v, want %+v", before, wantAllow)
			}

			// The revocation: publish the complete policy set as empty. It
			// succeeds as exactly the next consecutive version.
			newVersion, err := s.Publish(org, 1, emptyPolicies)
			if err != nil || newVersion != 2 {
				t.Fatalf("publish empty revocation = %d, %v; want 2, nil", newVersion, err)
			}
			if got := s.CurrentVersion(org); got != 2 {
				t.Fatalf("current version = %d, want 2 after revocation", got)
			}
			// Exactly one new version was consumed: 3 does not exist.
			if _, err := s.Policies(org, 3); !errors.Is(err, ErrVersionNotFound) {
				t.Fatalf("version 3 exists after one revocation, err = %v", err)
			}
			// The new version is a genuine empty set; the allow version is
			// untouched history underneath it.
			gotV2, err := s.Policies(org, 2)
			if err != nil || len(gotV2) != 0 {
				t.Fatalf("Policies(v2) = %+v, %v; want empty, nil", gotV2, err)
			}
			gotV1, err := s.Policies(org, 1)
			if err != nil || !reflect.DeepEqual(gotV1, allow) {
				t.Fatalf("Policies(v1) = %+v, %v; want the original allow set", gotV1, err)
			}

			// Boundary 2: a read submitted only after the revocation publish
			// returned is denied at the empty version.
			wantDeny := revokeDenyDecision()
			after := s.Decide(org, req)
			if !reflect.DeepEqual(after, wantDeny) {
				t.Fatalf("post-revocation decision = %+v, want %+v", after, wantDeny)
			}
			if after.Version == 0 {
				t.Fatalf("denial fell back to version 0 as if nothing were published")
			}

			// The shared audit chain: publish v1, the allowed read, the
			// revocation publish, the denied read — four records, gapless,
			// nothing omitted and nothing extra.
			recs := drainAudit(t, s, org, "", "")
			if len(recs) != 4 {
				t.Fatalf("audit records = %d, want 4", len(recs))
			}
			for i, r := range recs {
				if r.Seq != i+1 || r.Org != org {
					t.Fatalf("record %d has unexpected seq/org: %+v", i+1, r)
				}
			}
			if recs[0].Kind != AuditPolicyChange || recs[0].Change == nil ||
				recs[0].Change.Version != 1 ||
				!reflect.DeepEqual(recs[0].Change.Policies, allow) ||
				recs[0].Change.RolledBack {
				t.Fatalf("v1 change record altered: %+v", recs[0])
			}
			// The pre-revocation read keeps its allow conclusion on the chain.
			checkRevokeDecisionRecord(t, recs[1], req, wantAllow)
			// The revocation left exactly one policy-change record: the new
			// version 2 with empty content, an ordinary publish (no rollback).
			chg := recs[2].Change
			if recs[2].Kind != AuditPolicyChange || chg == nil {
				t.Fatalf("record 3 is not the revocation change: %+v", recs[2])
			}
			if chg.Version != 2 || len(chg.Policies) != 0 ||
				chg.RolledBack || chg.SourceVersion != 0 {
				t.Fatalf("revocation record = %+v, want version 2, empty policies, plain publish", chg)
			}
			// The post-revocation read is recorded denied at the empty version.
			checkRevokeDecisionRecord(t, recs[3], req, wantDeny)

			// The chain verifies, and each recorded decision rechecks to
			// exactly itself from the organization's own history.
			exported, cp, err := s.AuditExport(org, 0)
			if err != nil {
				t.Fatalf("AuditExport: %v", err)
			}
			if err := VerifyAudit(org, exported, cp); err != nil {
				t.Fatalf("chain does not verify after revocation: %v", err)
			}
			rcAllow, err := s.RecheckDecision(org, 2)
			if err != nil || !reflect.DeepEqual(rcAllow, wantAllow) {
				t.Fatalf("recheck of pre-revocation read = %+v, %v; want %+v", rcAllow, err, wantAllow)
			}
			rcDeny, err := s.RecheckDecision(org, 4)
			if err != nil || !reflect.DeepEqual(rcDeny, wantDeny) {
				t.Fatalf("recheck of post-revocation read = %+v, %v; want %+v", rcDeny, err, wantDeny)
			}

			// Offline replay from the export must find the empty v2 content
			// in the revocation record before the denial and must not fall
			// back to version 0 or substitute the allowing v1.
			review, err := RecheckDecisionOffline(org, exported, cp, 4)
			if err != nil {
				t.Fatalf("RecheckDecisionOffline after revocation: %v", err)
			}
			if !review.Consistent || !reflect.DeepEqual(review.Recomputed, wantDeny) {
				t.Fatalf("offline review of denial = %+v, want consistent %+v", review, wantDeny)
			}
		})
	}
}

// TestConcurrentPublishEmptySetRevocation overlaps read decisions with the
// revocation publish as a genuine race. Neither side is required to finish
// first and a run need not observe both outcomes; the organization audit
// chain decides what every decision must look like. Decision records before
// the revocation record must be allows at version 1, records after it must
// be denials at the empty version 2, and the decisions returned to the
// callers must be exactly the decisions on the chain.
func TestConcurrentPublishEmptySetRevocation(t *testing.T) {
	const (
		iterations    = 50
		readers       = 8
		readsPerReply = 10
	)
	for iter := 0; iter < iterations; iter++ {
		org := "acme-revoke-race"
		s, req, wantAllow := setupRevokeStore(t, org)
		wantDeny := revokeDenyDecision()

		// A read that completes before the revocation starts keeps its allow
		// conclusion; it joins the returned-decision accounting below.
		var returned []Decision
		var mu sync.Mutex
		collect := func(d Decision) {
			mu.Lock()
			returned = append(returned, d)
			mu.Unlock()
		}
		collect(s.Decide(org, req))

		start := make(chan struct{})
		publishResult := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.Publish(org, 1, nil)
			if err == nil && v != 2 {
				t.Errorf("revocation publish returned version %d, want 2", v)
			}
			publishResult <- err
		}()
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < readsPerReply; i++ {
					collect(s.Decide(org, req))
				}
			}()
		}
		close(start)
		wg.Wait()
		if err := <-publishResult; err != nil {
			t.Fatalf("iter %d: revocation publish failed: %v", iter, err)
		}

		// A read submitted only after the revocation returned is denied.
		collect(s.Decide(org, req))

		// Every returned decision is exactly one of the two legal outcomes —
		// never version 0, never a stale allow version with the old matched
		// list after the revocation, never a partial shape.
		allowReturns, denyReturns := 0, 0
		for _, d := range returned {
			switch {
			case reflect.DeepEqual(d, wantAllow):
				allowReturns++
			case reflect.DeepEqual(d, wantDeny):
				denyReturns++
			default:
				t.Fatalf("iter %d: unexpected returned decision %+v", iter, d)
			}
		}

		// Exactly one new version and one policy-change record came out of
		// the revocation; every access request left exactly one decision
		// record — none missing, none extra.
		if got := s.CurrentVersion(org); got != 2 {
			t.Fatalf("iter %d: current version = %d, want 2", iter, got)
		}
		if _, err := s.Policies(org, 3); !errors.Is(err, ErrVersionNotFound) {
			t.Fatalf("iter %d: version 3 exists, err = %v", iter, err)
		}
		totalDecisions := 1 + readers*readsPerReply + 1
		recs := drainAudit(t, s, org, "", "")
		if len(recs) != 2+totalDecisions {
			t.Fatalf("iter %d: audit records = %d, want %d (2 changes + %d decisions)",
				iter, len(recs), 2+totalDecisions, totalDecisions)
		}
		for i, r := range recs {
			if r.Seq != i+1 {
				t.Fatalf("iter %d: record %d has sequence %d; the chain must be gapless", iter, i+1, r.Seq)
			}
		}
		if recs[0].Kind != AuditPolicyChange || recs[0].Change == nil || recs[0].Change.Version != 1 {
			t.Fatalf("iter %d: first record is not the v1 publish: %+v", iter, recs[0])
		}
		revokeSeq := 0
		for _, r := range recs[1:] {
			if r.Kind != AuditPolicyChange {
				continue
			}
			if revokeSeq != 0 {
				t.Fatalf("iter %d: second revocation record at seq %d (first at %d)", iter, r.Seq, revokeSeq)
			}
			revokeSeq = r.Seq
			chg := r.Change
			if chg == nil || chg.Version != 2 || len(chg.Policies) != 0 ||
				chg.RolledBack || chg.SourceVersion != 0 {
				t.Fatalf("iter %d: revocation record = %+v, want version 2, empty policies, plain publish", iter, chg)
			}
		}
		if revokeSeq == 0 {
			t.Fatalf("iter %d: revocation left no policy-change record", iter)
		}

		// Chain position decides: before the revocation record every decision
		// is the allow at v1; after it every decision is the denial at the
		// empty v2. No later record may keep the old allow version and no
		// earlier record may carry the not-yet-published v2.
		allowRecords, denyRecords := 0, 0
		for _, r := range recs {
			if r.Kind != AuditDecision {
				continue
			}
			if r.Seq < revokeSeq {
				checkRevokeDecisionRecord(t, r, req, wantAllow)
				allowRecords++
			} else {
				checkRevokeDecisionRecord(t, r, req, wantDeny)
				denyRecords++
			}
		}
		// The returned results and the recorded decisions are the same
		// outcomes: identical requests, so the per-shape counts must match.
		if allowRecords != allowReturns || denyRecords != denyReturns {
			t.Fatalf("iter %d: returned vs recorded decisions disagree: allows %d/%d, denials %d/%d",
				iter, allowReturns, allowRecords, denyReturns, denyRecords)
		}

		// The full chain verifies and every recorded decision rechecks to
		// exactly itself against the version it declared.
		exported, cp, err := s.AuditExport(org, 0)
		if err != nil {
			t.Fatalf("iter %d: AuditExport: %v", iter, err)
		}
		if err := VerifyAudit(org, exported, cp); err != nil {
			t.Fatalf("iter %d: chain does not verify: %v", iter, err)
		}
		for _, r := range recs {
			if r.Kind != AuditDecision {
				continue
			}
			got, err := s.RecheckDecision(org, r.Seq)
			if err != nil || !reflect.DeepEqual(got, r.Decision.Decision) {
				t.Fatalf("iter %d: recheck of seq %d = %+v, %v; want %+v",
					iter, r.Seq, got, err, r.Decision.Decision)
			}
		}
	}
}
