// Regression coverage for revoking an organization's existing access by
// publishing an EMPTY policy set (not a rollback): the empty publish is an
// ordinary successful publish that creates exactly one new version and one
// policy-change record, and from that record onward every organization
// decision must deny with the empty set's version.
//
// The focus is the consistency between a revoking publish and access
// decisions that overlap it. The audit chain is the single source of truth
// for ordering: a decision record placed BEFORE the empty publish's record
// must reflect the old allow version, one placed AFTER it must reflect the
// new empty version. No decision may keep using the superseded allow version
// once the revoke record precedes it, none may fall back to the unpublished
// version 0, and none may carry a version that had not been published yet at
// its chain position. Every returned result must match its recorded
// counterpart field for field (allowed, reason, matched list, version).
//
// The scenario stays inside one organization: an enabled subject reads one
// resource, with subject, resource and decision organizations all equal and
// every identifier, action and scope valid.
package darksafe

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

// emptyRevokeOrg is the single organization every scenario here runs in.
const emptyRevokeOrg = "acme factory"

// emptyRevokeAllowSet is the original policy set: one allow policy matching
// the scenario's read request exactly.
func emptyRevokeAllowSet() []Policy {
	return []Policy{allowPolicy("p-ledger-read-2026", "svc-audit-reader", "read", "acme/factory/ledger", false)}
}

// emptyRevokeRequest is the fully populated, organization-consistent read
// request used by every scenario: enabled subject, matching action and
// scope, all three organizations equal.
func emptyRevokeRequest() OrgRequest {
	return request(emptyRevokeOrg, "svc-audit-reader", "ledger-2026", "acme/factory/ledger", "read")
}

// emptyRevokeAllowDecision is the only acceptable outcome for a decision
// recorded before the revoking publish: allowed by the allow version.
func emptyRevokeAllowDecision() Decision {
	return Decision{
		Allowed: true, Reason: "matched allow policy",
		Matched: []string{"p-ledger-read-2026"}, Version: 1,
	}
}

// emptyRevokeDenyDecision is the only acceptable outcome for a decision
// recorded after the revoking publish: default denial under the empty
// version — never version 0, never a leftover hit from the old policy.
func emptyRevokeDenyDecision() Decision {
	return Decision{
		Allowed: false, Reason: "no matching allow policy",
		Matched: []string{}, Version: 2,
	}
}

// setupEmptyRevokeStore publishes the allow set as version 1 and pins that
// the scenario request is allowed under it.
func setupEmptyRevokeStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	if v, err := s.Publish(emptyRevokeOrg, 0, emptyRevokeAllowSet()); err != nil || v != 1 {
		t.Fatalf("publish allow v1 = %d, %v; want 1, nil", v, err)
	}
	if d := s.Decide(emptyRevokeOrg, emptyRevokeRequest()); !reflect.DeepEqual(d, emptyRevokeAllowDecision()) {
		t.Fatalf("pre-revoke decision = %+v, want %+v", d, emptyRevokeAllowDecision())
	}
	return s
}

// checkEmptyRevokeChain verifies the audit chain shape shared by every
// scenario: record 1 is the allow publish, exactly one more policy-change
// record is the empty revoke publish (new version 2, empty content, not a
// rollback), and every decision record obeys the chain-order rule — before
// the revoke record it must equal the allow decision at version 1, after it
// the empty-set denial at version 2. It returns the revoke record's
// sequence. wantDecisions is the exact number of decision records expected:
// none missing, none extra.
func checkEmptyRevokeChain(t *testing.T, s *Store, wantDecisions int) int {
	t.Helper()

	recs, cp, err := s.AuditExport(emptyRevokeOrg, 0)
	if err != nil {
		t.Fatalf("AuditExport: %v", err)
	}
	if err := VerifyAudit(emptyRevokeOrg, recs, cp); err != nil {
		t.Fatalf("chain does not verify: %v", err)
	}

	// Exactly two policy-change records: the original allow publish and the
	// empty revoke publish. One successful revocation creates exactly one new
	// version and one record.
	if len(recs) != 2+wantDecisions {
		t.Fatalf("audit records = %d, want 2 changes + %d decisions", len(recs), wantDecisions)
	}
	for i, r := range recs {
		if r.Seq != i+1 || r.Org != emptyRevokeOrg {
			t.Fatalf("record at position %d has unexpected seq/org: %+v", i, r)
		}
	}
	first := recs[0]
	if first.Kind != AuditPolicyChange || first.Change == nil ||
		first.Change.Version != 1 || first.Change.RolledBack ||
		!reflect.DeepEqual(first.Change.Policies, emptyRevokeAllowSet()) {
		t.Fatalf("record 1 is not the allow publish of v1: %+v", first)
	}

	revokeSeq := 0
	decisions := 0
	for _, r := range recs[1:] {
		switch r.Kind {
		case AuditPolicyChange:
			if revokeSeq != 0 {
				t.Fatalf("second revoke record at seq %d (first at %d): one revocation must produce exactly one change record", r.Seq, revokeSeq)
			}
			chg := r.Change
			if chg == nil || chg.Version != 2 || len(chg.Policies) != 0 ||
				chg.RolledBack || chg.SourceVersion != 0 {
				t.Fatalf("revoke record at seq %d must be an ordinary publish of the empty set as v2: %+v", r.Seq, chg)
			}
			revokeSeq = r.Seq
		case AuditDecision:
			decisions++
			if r.Decision == nil {
				t.Fatalf("decision record at seq %d has no payload", r.Seq)
			}
			if !reflect.DeepEqual(r.Decision.Request, emptyRevokeRequest()) {
				t.Fatalf("decision record at seq %d stored request %+v, want %+v", r.Seq, r.Decision.Request, emptyRevokeRequest())
			}
			// The chain position decides the required outcome; the recorded
			// version must be one actually published before this record.
			want := emptyRevokeAllowDecision()
			if revokeSeq != 0 {
				want = emptyRevokeDenyDecision()
			}
			if !reflect.DeepEqual(r.Decision.Decision, want) {
				t.Fatalf("decision record at seq %d (revoke at %d) = %+v, want %+v", r.Seq, revokeSeq, r.Decision.Decision, want)
			}
			if r.Decision.Decision.Version == 0 {
				t.Fatalf("decision record at seq %d fell back to unpublished version 0", r.Seq)
			}
		default:
			t.Fatalf("unexpected record kind at seq %d: %+v", r.Seq, r)
		}
	}
	if revokeSeq == 0 {
		t.Fatalf("no revoke policy-change record found in %+v", recs)
	}
	if decisions != wantDecisions {
		t.Fatalf("decision records = %d, want %d (none missing, none extra)", decisions, wantDecisions)
	}

	// Every recorded decision rechecks to itself against its own version.
	for _, r := range recs {
		if r.Kind != AuditDecision {
			continue
		}
		got, err := s.RecheckDecision(emptyRevokeOrg, r.Seq)
		if err != nil {
			t.Fatalf("RecheckDecision(%d): %v", r.Seq, err)
		}
		if !reflect.DeepEqual(got, r.Decision.Decision) {
			t.Fatalf("RecheckDecision(%d) = %+v, want recorded %+v", r.Seq, got, r.Decision.Decision)
		}
	}
	return revokeSeq
}

// checkEmptyRevokeState verifies the version state after a successful
// revocation: current version 2, the empty set queryable as v2, the allow
// set preserved as v1, and no version 0 or 3.
func checkEmptyRevokeState(t *testing.T, s *Store) {
	t.Helper()
	if got := s.CurrentVersion(emptyRevokeOrg); got != 2 {
		t.Fatalf("current version = %d, want 2 after the empty publish", got)
	}
	gotV2, err := s.Policies(emptyRevokeOrg, 2)
	if err != nil || len(gotV2) != 0 {
		t.Fatalf("Policies(v2) = %+v, %v; want the empty set, nil", gotV2, err)
	}
	gotV1, err := s.Policies(emptyRevokeOrg, 1)
	if err != nil || !reflect.DeepEqual(gotV1, emptyRevokeAllowSet()) {
		t.Fatalf("Policies(v1) = %+v, %v; want the original allow set intact", gotV1, err)
	}
	if _, err := s.Policies(emptyRevokeOrg, 3); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("version 3 exists (err=%v); the single revocation must not consume extra versions", err)
	}
}

// TestPublishEmptySetRevokesAccess pins the sequential contract: with the
// allow policy published, the read is allowed under v1; once the complete
// policy set is published as empty, the same request is denied under the new
// empty version — with the no-matching-allow reason, an empty hit list, and
// never version 0 or a stale hit. Both empty shapes that callers submit are
// covered.
func TestPublishEmptySetRevokesAccess(t *testing.T) {
	shapes := map[string][]Policy{
		"nil slice":   nil,
		"empty slice": {},
	}
	for shape, empty := range shapes {
		t.Run(shape, func(t *testing.T) {
			s := setupEmptyRevokeStore(t)
			req := emptyRevokeRequest()

			// Boundary: a read that completed before the revocation started
			// keeps its allow conclusion (recorded at seq 2 by the setup).
			newVersion, err := s.Publish(emptyRevokeOrg, 1, empty)
			if err != nil || newVersion != 2 {
				t.Fatalf("publish empty set = %d, %v; want new version 2, nil", newVersion, err)
			}

			// Boundary: every read submitted after the revoking publish
			// returned is denied under the empty version.
			wantDenial := emptyRevokeDenyDecision()
			for i := 0; i < 2; i++ {
				if d := s.Decide(emptyRevokeOrg, req); !reflect.DeepEqual(d, wantDenial) {
					t.Fatalf("post-revoke decision %d = %+v, want %+v", i, d, wantDenial)
				}
			}

			checkEmptyRevokeState(t, s)
			// One allow decision before, two denials after: three decision
			// records around the single revoke record.
			revokeSeq := checkEmptyRevokeChain(t, s, 3)
			if revokeSeq != 3 {
				t.Fatalf("revoke record seq = %d, want 3 (allow publish, allow decision, revoke)", revokeSeq)
			}

			// The pre-revoke allow conclusion is preserved by recheck even
			// though the current version now denies.
			allow, err := s.RecheckDecision(emptyRevokeOrg, 2)
			if err != nil || !reflect.DeepEqual(allow, emptyRevokeAllowDecision()) {
				t.Fatalf("recheck of pre-revoke decision = %+v, %v; want %+v", allow, err, emptyRevokeAllowDecision())
			}
		})
	}
}

// TestConcurrentEmptyPublishRevokeAndDecide overlaps read requests with the
// revoking empty publish. Neither side is required to finish first, and not
// every run must observe both outcomes — the audit chain's stored order is
// the judge: decisions recorded before the revoke record must use the allow
// version, those after it the empty version, and each returned result must
// match its recorded counterpart exactly.
func TestConcurrentEmptyPublishRevokeAndDecide(t *testing.T) {
	const iterations = 100
	const readers = 8
	seenAllow, seenDeny := false, false
	for iter := 0; iter < iterations; iter++ {
		s := NewStore()
		if v, err := s.Publish(emptyRevokeOrg, 0, emptyRevokeAllowSet()); err != nil || v != 1 {
			t.Fatalf("iter %d: publish allow v1 = %d, %v; want 1, nil", iter, v, err)
		}

		start := make(chan struct{})
		results := make(chan Decision, readers)
		published := make(chan int, 1)
		errs := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(readers + 1)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.Publish(emptyRevokeOrg, 1, nil)
			if err != nil {
				errs <- err
				return
			}
			published <- v
		}()
		for i := 0; i < readers; i++ {
			go func() {
				defer wg.Done()
				<-start
				results <- s.Decide(emptyRevokeOrg, emptyRevokeRequest())
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		// The revoke publish is the only writer: it must succeed exactly
		// once, creating version 2.
		select {
		case err := <-errs:
			t.Fatalf("iter %d: revoke publish failed: %v", iter, err)
		default:
		}
		if v := <-published; v != 2 {
			t.Fatalf("iter %d: revoke publish returned version %d, want 2", iter, v)
		}

		// Every returned result must be one of the two lawful outcomes; the
		// per-outcome counts must match the chain exactly.
		returnedAllow, returnedDeny := 0, 0
		for d := range results {
			switch {
			case reflect.DeepEqual(d, emptyRevokeAllowDecision()):
				returnedAllow++
			case reflect.DeepEqual(d, emptyRevokeDenyDecision()):
				returnedDeny++
			default:
				t.Fatalf("iter %d: Decide returned %+v, want the v1 allow or the v2 empty-set denial", iter, d)
			}
		}

		checkEmptyRevokeState(t, s)
		revokeSeq := checkEmptyRevokeChain(t, s, readers)

		// The chain's own order defines how many of each outcome exist; the
		// returned results must agree with the records one for one.
		recs := drainAudit(t, s, emptyRevokeOrg, AuditDecision, "")
		recordedAllow, recordedDeny := 0, 0
		for _, r := range recs {
			if r.Seq < revokeSeq {
				recordedAllow++
			} else {
				recordedDeny++
			}
		}
		if returnedAllow != recordedAllow || returnedDeny != recordedDeny {
			t.Fatalf("iter %d: returned allow/deny = %d/%d, chain recorded %d/%d; every result must match its record",
				iter, returnedAllow, returnedDeny, recordedAllow, recordedDeny)
		}
		if recordedAllow > 0 {
			seenAllow = true
		}
		if recordedDeny > 0 {
			seenDeny = true
		}
	}
	// Informational: scheduling may never interleave one way. Both orderings
	// are pinned deterministically by the sequential test regardless.
	if !seenAllow || !seenDeny {
		t.Logf("scheduler observed only one interleaving across %d iterations (allow=%v, deny=%v); both orderings remain covered by the sequential test",
			iterations, seenAllow, seenDeny)
	}
}
