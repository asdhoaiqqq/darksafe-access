// Regression coverage for a publish and a rollback contending for the same
// current version of one organization. Both operations accept or reject a
// change solely on the caller-supplied expected version, and only the winner
// may create a new version and a policy-change audit record. These tests pin
// that contract without assuming which side wins: the sequential tests pin
// each winner in turn by fixing the call order, and the concurrent test runs
// the real mutex race and judges whichever side actually succeeds by the
// same rules.
package darksafe

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

const contestOrg = "acme"

// contentionFixture builds the three full policy sets involved in the race:
//
//   - v1 is the earlier published version: an explicit DENY for the probe,
//     carrying its own identifiable marker.
//   - v2 is the newer, current version: an ALLOW for the same subject,
//     action and resource with a different marker, so the two published
//     versions have genuinely different content and authorization results.
//   - candidate is the complete set the competing Publish submits: an ALLOW
//     with its own marker, distinct from every historical version.
//
// The rollback targets v1 and the publish submits candidate, so the two
// candidate modifications produce opposite authorization results for the
// same probe request (rollback denies, publish allows).
func contentionFixture() (v1, v2, candidate []Policy) {
	v1 = []Policy{{
		ID: "contest-v1-deny", Subject: "contest-subject", Action: "read",
		Scope: "org/acme/ledger", Effect: EffectDeny,
	}}
	v2 = []Policy{{
		ID: "contest-v2-allow", Subject: "contest-subject", Action: "read",
		Scope: "org/acme/ledger", Effect: EffectAllow,
	}}
	candidate = []Policy{{
		ID: "contest-publish-allow", Subject: "contest-subject", Action: "read",
		Scope: "org/acme/ledger", Effect: EffectAllow,
	}}
	return
}

// contestProbe is the access request used to tell the two candidate outcomes
// apart after the race.
func contestProbe() OrgRequest {
	return request(contestOrg, "contest-subject", "contest-resource", "org/acme/ledger", "read")
}

// setupContendedStore publishes v1 then v2, leaving the organization at
// current version 2 with two content-distinct historical versions.
func setupContendedStore(t *testing.T) (s *Store, v1, v2, candidate []Policy) {
	t.Helper()
	v1, v2, candidate = contentionFixture()
	s = NewStore()
	if v, err := s.Publish(contestOrg, 0, v1); err != nil || v != 1 {
		t.Fatalf("publish v1 = %d, %v; want 1, nil", v, err)
	}
	if v, err := s.Publish(contestOrg, 1, v2); err != nil || v != 2 {
		t.Fatalf("publish v2 = %d, %v; want 2, nil", v, err)
	}
	if got := s.CurrentVersion(contestOrg); got != 2 {
		t.Fatalf("current version = %d, want 2 before contention", got)
	}
	// Document the fixture invariant: the two contending changes decide the
	// probe differently, and the two existing versions differ as well.
	probe := contestProbe()
	if d := evaluate(probe, v1, 1); d.Allowed {
		t.Fatalf("fixture: rollback target v1 must deny the probe, got %+v", d)
	}
	if d := evaluate(probe, candidate, 3); !d.Allowed {
		t.Fatalf("fixture: publish candidate must allow the probe, got %+v", d)
	}
	if d := evaluate(probe, v2, 2); !d.Allowed {
		t.Fatalf("fixture: current v2 must allow the probe, got %+v", d)
	}
	return s, v1, v2, candidate
}

// checkContentionOutcome verifies the complete post-race contract for either
// winner. winner is "publish" (candidate became v3) or "rollback" (v1's
// content became v3).
func checkContentionOutcome(t *testing.T, s *Store, winner string, v1, v2, candidate []Policy) {
	t.Helper()

	var wantPolicies []Policy
	switch winner {
	case "publish":
		wantPolicies = candidate
	case "rollback":
		wantPolicies = v1
	default:
		t.Fatalf("unknown winner %q", winner)
	}

	// Exactly one modification was accepted: the current version advanced by
	// exactly one, and the losing request did not consume a version number.
	if got := s.CurrentVersion(contestOrg); got != 3 {
		t.Fatalf("[%s wins] current version = %d, want 3", winner, got)
	}
	if _, err := s.Policies(contestOrg, 4); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("[%s wins] version 4 exists (err=%v); the loser must not consume a version", winner, err)
	}

	// The two pre-existing versions are fully intact and still queryable.
	gotV1, err := s.Policies(contestOrg, 1)
	if err != nil {
		t.Fatalf("[%s wins] Policies(v1): %v", winner, err)
	}
	if !reflect.DeepEqual(gotV1, v1) {
		t.Fatalf("[%s wins] v1 changed to %+v, want %+v", winner, gotV1, v1)
	}
	gotV2, err := s.Policies(contestOrg, 2)
	if err != nil {
		t.Fatalf("[%s wins] Policies(v2): %v", winner, err)
	}
	if !reflect.DeepEqual(gotV2, v2) {
		t.Fatalf("[%s wins] v2 changed to %+v, want %+v", winner, gotV2, v2)
	}

	// v3 holds exactly the winner's full policy set: no splicing of the two
	// candidate sets, no partial loss, and no trace of the loser's content.
	gotV3, err := s.Policies(contestOrg, 3)
	if err != nil {
		t.Fatalf("[%s wins] Policies(v3): %v", winner, err)
	}
	if !reflect.DeepEqual(gotV3, wantPolicies) {
		t.Fatalf("[%s wins] v3 content = %+v, want exactly %+v", winner, gotV3, wantPolicies)
	}

	// Relative to before the race, exactly ONE policy-change record exists,
	// appended gaplessly, and its payload describes the successful operation
	// only. The conflicting request left no record and no sequence gap.
	recs := drainAudit(t, s, contestOrg, "", "")
	if len(recs) != 3 {
		t.Fatalf("[%s wins] audit records = %d, want 3", winner, len(recs))
	}
	for i, r := range recs {
		if r.Seq != i+1 || r.Kind != AuditPolicyChange || r.Change == nil {
			t.Fatalf("[%s wins] unexpected record at position %d: %+v", winner, i, r)
		}
	}
	change := recs[2].Change
	if change.Version != 3 {
		t.Fatalf("[%s wins] new record names version %d, want 3 (rollback must not return the target version)", winner, change.Version)
	}
	if !reflect.DeepEqual(change.Policies, wantPolicies) {
		t.Fatalf("[%s wins] audit policies = %+v, want %+v", winner, change.Policies, wantPolicies)
	}
	switch winner {
	case "publish":
		if change.RolledBack || change.SourceVersion != 0 {
			t.Fatalf("[publish wins] change is tagged as a rollback: %+v", change)
		}
	case "rollback":
		if !change.RolledBack || change.SourceVersion != 1 {
			t.Fatalf("[rollback wins] change must name source v1: %+v", change)
		}
	}
	// The complete exported chain, including the raced record, passes the
	// existing offline verification.
	exported, cp, err := s.AuditExport(contestOrg, 0)
	if err != nil {
		t.Fatalf("[%s wins] AuditExport: %v", winner, err)
	}
	if err := VerifyAudit(contestOrg, exported, cp); err != nil {
		t.Fatalf("[%s wins] chain does not verify after contention: %v", winner, err)
	}

	// A subsequent access request is decided by the new winning version, and
	// the decision record persists the identical result.
	probe := contestProbe()
	wantDecision := evaluate(probe, wantPolicies, 3)
	d := s.Decide(contestOrg, probe)
	if !reflect.DeepEqual(d, wantDecision) {
		t.Fatalf("[%s wins] Decide = %+v, want %+v from the new version", winner, d, wantDecision)
	}
	if d.Version != 3 {
		t.Fatalf("[%s wins] decision used version %d, want 3", winner, d.Version)
	}
	decisions := drainAudit(t, s, contestOrg, AuditDecision, "")
	if len(decisions) != 1 {
		t.Fatalf("[%s wins] decision records = %d, want 1", winner, len(decisions))
	}
	dr := decisions[0]
	if dr.Seq != 4 || !reflect.DeepEqual(dr.Decision.Decision, d) ||
		!reflect.DeepEqual(dr.Decision.Request, probe) {
		t.Fatalf("[%s wins] saved decision = %+v, want %+v for request %+v", winner, dr.Decision, d, probe)
	}
	rechecked, err := s.RecheckDecision(contestOrg, dr.Seq)
	if err != nil {
		t.Fatalf("[%s wins] RecheckDecision: %v", winner, err)
	}
	if !reflect.DeepEqual(rechecked, d) {
		t.Fatalf("[%s wins] recheck = %+v, want %+v", winner, rechecked, d)
	}

	// Historical versions still review independently and were not rewritten
	// by the contention.
	if d1 := s.Review(contestOrg, 1, probe); !reflect.DeepEqual(d1, evaluate(probe, v1, 1)) {
		t.Fatalf("[%s wins] review v1 drifted: %+v", winner, d1)
	}
	if d2 := s.Review(contestOrg, 2, probe); !reflect.DeepEqual(d2, evaluate(probe, v2, 2)) {
		t.Fatalf("[%s wins] review v2 drifted: %+v", winner, d2)
	}

	// Final chain (change + decision) still verifies in full.
	exported, cp, err = s.AuditExport(contestOrg, 0)
	if err != nil {
		t.Fatalf("[%s wins] final AuditExport: %v", winner, err)
	}
	if len(exported) != 4 {
		t.Fatalf("[%s wins] final records = %d, want 4", winner, len(exported))
	}
	if err := VerifyAudit(contestOrg, exported, cp); err != nil {
		t.Fatalf("[%s wins] final chain does not verify: %v", winner, err)
	}
}

// TestPublishBeatsRollbackOnSameExpectedVersion pins the publish-winning
// ordering: the publish runs first while both callers still claim current
// version 2; the following rollback must be rejected as a conflict.
func TestPublishBeatsRollbackOnSameExpectedVersion(t *testing.T) {
	s, v1, v2, candidate := setupContendedStore(t)

	newVersion, err := s.Publish(contestOrg, 2, candidate)
	if err != nil || newVersion != 3 {
		t.Fatalf("publish = %d, %v; want 3, nil", newVersion, err)
	}
	if _, err := s.Rollback(contestOrg, 2, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("contending rollback err = %v, want ErrVersionConflict", err)
	}

	checkContentionOutcome(t, s, "publish", v1, v2, candidate)
}

// TestRollbackBeatsPublishOnSameExpectedVersion pins the rollback-winning
// ordering: the rollback runs first while both callers claim version 2; the
// following publish must be rejected as a conflict.
func TestRollbackBeatsPublishOnSameExpectedVersion(t *testing.T) {
	s, v1, v2, candidate := setupContendedStore(t)

	newVersion, err := s.Rollback(contestOrg, 2, 1)
	if err != nil || newVersion != 3 {
		t.Fatalf("rollback = %d, %v; want new version 3, not target version 1", newVersion, err)
	}
	if _, err := s.Publish(contestOrg, 2, candidate); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("contending publish err = %v, want ErrVersionConflict", err)
	}

	checkContentionOutcome(t, s, "rollback", v1, v2, candidate)
}

// TestConcurrentPublishRollbackSameVersionContention runs the two operations
// as a genuine concurrent race: both goroutines are released from one barrier
// while declaring the same expected current version. The mutex decides the
// winner; regardless of which side wins, exactly one change is accepted and
// the same outcome rules apply. The test never assumes a fixed winner.
func TestConcurrentPublishRollbackSameVersionContention(t *testing.T) {
	const iterations = 100
	seenPublishWin := false
	seenRollbackWin := false
	for iter := 0; iter < iterations; iter++ {
		s, v1, v2, candidate := setupContendedStore(t)

		start := make(chan struct{})
		type outcome struct {
			op  string
			v   int
			err error
		}
		results := make(chan outcome, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.Publish(contestOrg, 2, candidate)
			results <- outcome{"publish", v, err}
		}()
		go func() {
			defer wg.Done()
			<-start
			v, err := s.Rollback(contestOrg, 2, 1)
			results <- outcome{"rollback", v, err}
		}()
		close(start)
		wg.Wait()
		close(results)

		var winner string
		wins, conflicts := 0, 0
		for r := range results {
			switch {
			case r.err == nil:
				wins++
				winner = r.op
				if r.v != 3 {
					t.Fatalf("iter %d: %s returned version %d, want the new version 3", iter, r.op, r.v)
				}
			case errors.Is(r.err, ErrVersionConflict):
				conflicts++
			default:
				t.Fatalf("iter %d: %s unexpected error %v", iter, r.op, r.err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("iter %d: wins=%d conflicts=%d, want exactly one of each", iter, wins, conflicts)
		}
		switch winner {
		case "publish":
			seenPublishWin = true
		case "rollback":
			seenRollbackWin = true
		}
		checkContentionOutcome(t, s, winner, v1, v2, candidate)
	}
	// Informational: report if scheduling let only one side ever win. The
	// sequential tests above pin both outcomes regardless, so this is not a
	// failure condition.
	if !seenPublishWin || !seenRollbackWin {
		t.Logf("scheduler observed only one winner across %d iterations (publish=%v, rollback=%v); both orderings remain covered by the sequential tests",
			iterations, seenPublishWin, seenRollbackWin)
	}
}
