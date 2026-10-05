// Regression coverage for reviewing an access request against a specified
// historical policy version while the same organization is concurrently
// publishing or rolling back policies. Store.Review must reach its
// conclusion from the requested version's snapshot alone: while that
// historical content is being read the current policy can change, but the
// result can never splice a historical allowance onto a current denial, or
// the matched policies of one version onto another version's number.
//
// Every check compares the full explanation — allowance, reason, matched
// policy identifiers and the version actually used — rather than a single
// boolean, so a regression that mixed fields across versions is pinned.
// Review is read-only, so the tests also prove that overlapping reviews add
// no versions and no audit records on top of the successful publishes and
// rollbacks they overlap with. Everything here is local and in-memory.
package darksafe

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

const (
	histReviewOrg = "acme"
	histSubject   = "u1"
	histResource  = "ledger-1"
	histScope     = "org/acme/ledger"
	histAction    = "read"

	// The four versions each carry a single policy with a distinct
	// identifier, so a returned matched list names the exact version the
	// evaluation came from.
	histAllowPolicyID = "p-v1-allow-ledger-read"
	histDenyPolicyID  = "p-v2-deny-ledger-read"
	// The policy published mid-race carries its own marker distinct from
	// both historical versions.
	histMidAllowPolicyID = "p-v3-publish-allow-ledger-read"
)

// histReviewRequest is one fully admissible request by an enabled subject.
// Decision organization, subject organization and resource organization are
// identical, the scope is legal, and the subject carries no roles (roles
// never substitute for an organization policy anyway).
func histReviewRequest() OrgRequest {
	return request(histReviewOrg, histSubject, histResource, histScope, histAction)
}

func histAllowSet() []Policy {
	return []Policy{{
		ID: histAllowPolicyID, Subject: histSubject, Action: histAction,
		Scope: histScope, Effect: EffectAllow,
	}}
}

func histDenySet() []Policy {
	return []Policy{{
		ID: histDenyPolicyID, Subject: histSubject, Action: histAction,
		Scope: histScope, Effect: EffectDeny,
	}}
}

// histMidAllowSet is the organization's third published set, deliberately
// different in conclusion from both pre-change versions and carrying its own
// policy identifier.
func histMidAllowSet() []Policy {
	return []Policy{{
		ID: histMidAllowPolicyID, Subject: histSubject, Action: histAction,
		Scope: histScope, Effect: EffectAllow,
	}}
}

func mustPublish(t *testing.T, s *Store, expected int, policies []Policy) int {
	t.Helper()
	v, err := s.Publish(histReviewOrg, expected, policies)
	if err != nil {
		t.Fatalf("publish at expected %d = %d, %v; want nil", expected, v, err)
	}
	return v
}

func mustRollback(t *testing.T, s *Store, expected, target int) int {
	t.Helper()
	v, err := s.Rollback(histReviewOrg, expected, target)
	if err != nil {
		t.Fatalf("rollback expected %d -> target %d = %d, %v; want nil", expected, target, v, err)
	}
	return v
}

// histExpect captures the complete conclusion a review of one version must
// keep returning, regardless of what the organization publishes meanwhile.
type histExpect struct {
	version int
	allowed bool
	reason  string
	matched []string
}

// wantAllow / wantDeny describe the two pre-existing historical versions.
func wantAllow(version int) histExpect {
	return histExpect{
		version: version, allowed: true, reason: "matched allow policy",
		matched: []string{histAllowPolicyID},
	}
}

func wantDeny(version int) histExpect {
	return histExpect{
		version: version, allowed: false, reason: "matched deny policy",
		matched: []string{histDenyPolicyID},
	}
}

// sameMatched treats nil and empty matched lists as equal: both describe no
// hits.
func sameMatched(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// reviewFieldError names the single field that drifted inside a concurrent
// reviewer, so a failure report distinguishes allowance, reason, matched
// policies and version rather than collapsing into one boolean.
func reviewFieldError(exp histExpect, field string, got Decision) error {
	return fmt.Errorf(
		"review of v%d drifted on %q while current policy changed: got %+v; want allowed=%v reason=%q matched=%v version=%d",
		exp.version, field, got, exp.allowed, exp.reason, exp.matched, exp.version)
}

// assertHistReview checks the full explanation against the expectation and
// reports which field drifted, rather than only the allowance.
func assertHistReview(t *testing.T, s *Store, exp histExpect, req OrgRequest, phase string) {
	t.Helper()
	d := s.Review(histReviewOrg, exp.version, req)
	if d.Allowed != exp.allowed {
		t.Fatalf("%s: review v%d allowed = %v, want %v (decision %+v)",
			phase, exp.version, d.Allowed, exp.allowed, d)
	}
	if d.Reason != exp.reason {
		t.Fatalf("%s: review v%d reason = %q, want %q (decision %+v)",
			phase, exp.version, d.Reason, exp.reason, d)
	}
	if d.Version != exp.version {
		t.Fatalf("%s: review v%d reported version %d, want the requested %d (decision %+v)",
			phase, exp.version, d.Version, exp.version, d)
	}
	if !sameMatched(d.Matched, exp.matched) {
		t.Fatalf("%s: review v%d matched = %v, want %v; hits were spliced from another version",
			phase, exp.version, d.Matched, exp.matched)
	}
}

// buildHistReviewStore publishes the two pre-existing historical versions in
// order: v1 allows the probe and v2 denies it, each with its own policy
// identifier. The organization is left at current version 2 with a deny in
// force, and both historical expectations are returned.
func buildHistReviewStore(t *testing.T) (*Store, histExpect, histExpect) {
	t.Helper()
	s := NewStore()
	v1 := mustPublish(t, s, 0, histAllowSet())
	v2 := mustPublish(t, s, v1, histDenySet())
	if v1 != 1 || v2 != 2 {
		t.Fatalf("historical versions = %d, %d; want 1, 2", v1, v2)
	}
	allow := wantAllow(v1)
	deny := wantDeny(v2)
	// Document the fixture: the same request decides differently at the two
	// versions, and the hits identify the source.
	req := histReviewRequest()
	assertHistReview(t, s, allow, req, "fixture v1")
	assertHistReview(t, s, deny, req, "fixture v2")
	return s, allow, deny
}

// TestReviewHistoricalVersionsWithSequentialPublishAndRollback drives the
// full change history serially while repeatedly reviewing both historical
// versions: v1 allow is published, v2 deny is published, a different allow is
// published as v3 (current flips to allow), and a rollback re-creates the v1
// allow content as a new version v5. Every review must keep its original
// conclusion; in particular the rollback's new version number must never be
// reported in place of the requested old one.
func TestReviewHistoricalVersionsWithSequentialPublishAndRollback(t *testing.T) {
	s, allow, deny := buildHistReviewStore(t)
	req := histReviewRequest()

	// Publish a policy whose conclusion differs from the historical v2 deny.
	v3 := mustPublish(t, s, 2, histMidAllowSet())
	if v3 != 3 {
		t.Fatalf("mid-race publish = %d, want 3", v3)
	}
	assertHistReview(t, s, allow, req, "after v3 publish")
	assertHistReview(t, s, deny, req, "after v3 publish")
	// Sanity: the current decision really has flipped, so a v1 review that
	// still denies-impossible confusion would be visible rather than moot.
	if d := s.Decide(histReviewOrg, req); !d.Allowed || d.Version != v3 ||
		!sameMatched(d.Matched, []string{histMidAllowPolicyID}) {
		t.Fatalf("current decision under v3 = %+v, want allow from %q", d, histMidAllowPolicyID)
	}

	// Publish a denying v4 so the following rollback contends from a deny
	// state and lands at version 5 (distinct from the rollback target v1).
	v4 := mustPublish(t, s, v3, histDenySet())
	if v4 != 4 {
		t.Fatalf("v4 publish = %d, want 4", v4)
	}
	v5 := mustRollback(t, s, v4, 1)
	if v5 != 5 {
		t.Fatalf("rollback to v1 = %d, want a new version 5, never the target 1", v5)
	}
	assertHistReview(t, s, allow, req, "after rollback re-creating v1 content at v5")
	assertHistReview(t, s, deny, req, "after rollback re-creating v1 content at v5")

	// The rollback-generated copy allows with the SAME content, but Review(1)
	// must still name version 1, and Review(5) must name version 5: equal
	// policy content does not collapse two version numbers.
	copyAllow := s.Review(histReviewOrg, v5, req)
	if !copyAllow.Allowed || copyAllow.Reason != "matched allow policy" || copyAllow.Version != v5 {
		t.Fatalf("review v5 = %+v, want allow carrying the new version %d", copyAllow, v5)
	}
	if !sameMatched(copyAllow.Matched, []string{histAllowPolicyID}) {
		t.Fatalf("review v5 matched = %v, want the copied v1 policy id", copyAllow.Matched)
	}
	assertHistReview(t, s, allow, req, "final v1 review")

	// History was neither deleted nor rewritten: v2 still denies, v3 still
	// carries its own allow marker, and every saved snapshot is independently
	// reviewable.
	assertHistReview(t, s, deny, req, "final v2 review")
	v3Review := s.Review(histReviewOrg, v3, req)
	if !v3Review.Allowed || v3Review.Version != v3 ||
		!sameMatched(v3Review.Matched, []string{histMidAllowPolicyID}) {
		t.Fatalf("review v3 = %+v, want allow with %q at version %d", v3Review, histMidAllowPolicyID, v3)
	}
}

// TestReviewHistoricalVersionsDuringConcurrentChanges runs many historical
// reviews concurrently with a publish and a rollback (the first iteration
// also overlaps the publish and rollback contending for one current
// version). Reviews of the allow version always return the original allow
// explanation, and reviews of the deny version always return the deny
// explanation, however the current policy moves underneath them.
func TestReviewHistoricalVersionsDuringConcurrentChanges(t *testing.T) {
	const iterations = 60
	const reviewersPerVersion = 8
	const rounds = 12

	for iter := 0; iter < iterations; iter++ {
		s, allow, deny := buildHistReviewStore(t)
		req := histReviewRequest()

		var wg sync.WaitGroup
		failures := make(chan error, reviewersPerVersion*2*rounds)

		// Reviewers spin over both historical versions for the whole change
		// window. They capture the expectation by value so no goroutine reads
		// another's result.
		reviewLoop := func(exp histExpect) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				d := s.Review(histReviewOrg, exp.version, req)
				if d.Allowed != exp.allowed {
					failures <- reviewFieldError(exp, "allowed", d)
					return
				}
				if d.Reason != exp.reason {
					failures <- reviewFieldError(exp, "reason", d)
					return
				}
				if d.Version != exp.version {
					failures <- reviewFieldError(exp, "version", d)
					return
				}
				if !sameMatched(d.Matched, exp.matched) {
					failures <- reviewFieldError(exp, "matched", d)
					return
				}
			}
		}
		for i := 0; i < reviewersPerVersion; i++ {
			wg.Add(2)
			go reviewLoop(allow)
			go reviewLoop(deny)
		}

		// On the first iteration the publish and rollback race for the same
		// current version 2: exactly one may win. Either way the current
		// policy changes underneath the overlapping reviews.
		if iter == 0 {
			var changeWG sync.WaitGroup
			var mu sync.Mutex
			wins := 0
			changeWG.Add(2)
			go func() {
				defer changeWG.Done()
				if _, err := s.Publish(histReviewOrg, 2, histMidAllowSet()); err == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
			go func() {
				defer changeWG.Done()
				if _, err := s.Rollback(histReviewOrg, 2, 1); err == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
			changeWG.Wait()
			if wins != 1 {
				t.Fatalf("iter %d: contending publish/rollback wins = %d, want exactly 1", iter, wins)
			}
		} else {
			// Subsequent iterations: publish the different-allow v3 then
			// roll back to the historical deny v2 content as a new version,
			// so reviewers overlap both a current allow and a current deny.
			v3 := mustPublish(t, s, 2, histMidAllowSet())
			mustRollback(t, s, v3, 2)
		}

		wg.Wait()
		close(failures)
		for err := range failures {
			t.Fatalf("iter %d: %v", iter, err)
		}

		// Final state depends on the change path, but both historical
		// conclusions remain exactly pinned.
		assertHistReview(t, s, allow, req, "post-concurrency v1")
		assertHistReview(t, s, deny, req, "post-concurrency v2")
	}
}

// TestReviewPublishedEmptyVersionBoundary pins the already-published empty
// policy set as a genuine historical version. A legal request against it
// defaults to deny with "no matching allow policy", no hits, and the empty
// set's own version number; a later allow must not change that review, and
// the empty version must never be confused with version 0 ("nothing
// published yet").
func TestReviewPublishedEmptyVersionBoundary(t *testing.T) {
	s := NewStore()
	req := histReviewRequest()

	// v1 is a published, real empty policy set.
	emptyV := mustPublish(t, s, 0, nil)
	if emptyV != 1 {
		t.Fatalf("empty publish = %d, want 1", emptyV)
	}
	emptyExpect := histExpect{
		version: emptyV, allowed: false,
		reason:  "no matching allow policy",
		matched: []string{},
	}
	assertHistReview(t, s, emptyExpect, req, "empty set before later policies")

	// An allow policy appears later; it must not reach back into the empty
	// historical review.
	allowV := mustPublish(t, s, emptyV, histAllowSet())
	assertHistReview(t, s, emptyExpect, req, "empty set after allow published")

	// A rollback makes the empty content current again as a new version; the
	// original empty version keeps its original number.
	reEmptyV := mustRollback(t, s, allowV, emptyV)
	if reEmptyV <= emptyV {
		t.Fatalf("rollback of empty set = %d, want a new version > %d", reEmptyV, emptyV)
	}
	assertHistReview(t, s, emptyExpect, req, "original empty set after empty rollback")
	reEmpty := s.Review(histReviewOrg, reEmptyV, req)
	if reEmpty.Allowed || reEmpty.Reason != "no matching allow policy" ||
		reEmpty.Version != reEmptyV || len(reEmpty.Matched) != 0 {
		t.Fatalf("review rolled-back empty v%d = %+v, want default deny carrying the new version %d",
			reEmptyV, reEmpty, reEmptyV)
	}

	// The published empty set is distinguishable from "never published":
	// reviewing version 0 denies with the missing-version explanation and
	// version 0, never the empty set's number.
	zero := s.Review(histReviewOrg, 0, req)
	if zero.Allowed || zero.Version != 0 || len(zero.Matched) != 0 {
		t.Fatalf("review version 0 = %+v, want deny at version 0 with no hits", zero)
	}
	if zero.Reason != "version 0 not found" {
		t.Fatalf("review version 0 reason = %q, want %q; empty published set must not stand in",
			zero.Reason, "version 0 not found")
	}
	// The live decision on the empty current set agrees, carrying the empty
	// set version rather than 0.
	live := s.Decide(histReviewOrg, req)
	if live.Allowed || live.Reason != "no matching allow policy" ||
		live.Version != reEmptyV || len(live.Matched) != 0 {
		t.Fatalf("live decide on empty current = %+v, want default deny at version %d", live, reEmptyV)
	}
}

// TestReviewNeverExistedVersionStaysNotFound reviews historical versions
// that never exist at any point in the whole run. They must deny with the
// explicit missing-version reason, version 0 and an empty hit list, even
// while the organization publishes and rolls back around them and even
// though the current policy happens to allow the very same request: the
// current policy is never borrowed.
func TestReviewNeverExistedVersionStaysNotFound(t *testing.T) {
	s := NewStore()
	req := histReviewRequest()
	const missing = 99

	assertNotFound := func(phase string) {
		t.Helper()
		d := s.Review(histReviewOrg, missing, req)
		if d.Allowed {
			t.Fatalf("%s: review of never-existed v%d was allowed: %+v", phase, missing, d)
		}
		if d.Reason != "version 99 not found" {
			t.Fatalf("%s: review v%d reason = %q, want %q", phase, missing, d.Reason, "version 99 not found")
		}
		if d.Version != 0 {
			t.Fatalf("%s: review v%d version = %d, want 0 (must not borrow a published/current number)",
				phase, missing, d.Version)
		}
		if len(d.Matched) != 0 {
			t.Fatalf("%s: review v%d matched = %v, want no borrowed hits", phase, missing, d.Matched)
		}
	}

	// Before the organization has published anything.
	assertNotFound("before any publish")
	v1 := mustPublish(t, s, 0, histAllowSet())
	assertNotFound("with allowing v1 current")
	v2 := mustPublish(t, s, v1, histDenySet())
	assertNotFound("with denying v2 current")
	v3 := mustPublish(t, s, v2, histMidAllowSet())
	assertNotFound("with a different allowing v3 current")
	mustRollback(t, s, v3, 1)
	assertNotFound("after rollback re-creating v1 content")

	// A gap version (4 exists later but 42 never does) stays missing too.
	if d := s.Review(histReviewOrg, 42, req); d.Allowed || d.Version != 0 ||
		d.Reason != "version 42 not found" || len(d.Matched) != 0 {
		t.Fatalf("review v42 = %+v, want explicit not-found denial at version 0", d)
	}
}

// TestReviewIsReadOnlyAcrossOverlappingChanges proves the overlapping
// reviews themselves mutate nothing. After all operations finish, saved
// historical snapshots are byte-identical to what was published, and the
// current version and audit chain reflect only the successful publish and
// rollback — no extra versions or records accrue from the number of reviews.
func TestReviewIsReadOnlyAcrossOverlappingChanges(t *testing.T) {
	s, allow, deny := buildHistReviewStore(t)
	req := histReviewRequest()

	// Snapshot the two historical policy sets and the audit chain before any
	// of the overlapping reviews run.
	wantV1, err := s.Policies(histReviewOrg, 1)
	if err != nil {
		t.Fatal(err)
	}
	wantV2, err := s.Policies(histReviewOrg, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Drive a publish and a rollback while many reviews (existing, missing
	// and envelope-rejecting versions) run concurrently against them.
	const reviewers = 24
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < reviewers; i++ {
			s.Review(histReviewOrg, 1, req)
			s.Review(histReviewOrg, 2, req)
			s.Review(histReviewOrg, 99, req)
			bad := req
			bad.SubjectOrg = "other"
			s.Review(histReviewOrg, 1, bad)
		}
	}()
	v3, err := s.Publish(histReviewOrg, 2, histMidAllowSet())
	if err != nil {
		t.Fatal(err)
	}
	// Roll back to the v1 allow content; this creates v4.
	v4, err := s.Rollback(histReviewOrg, v3, 1)
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if v3 != 3 || v4 != 4 {
		t.Fatalf("change versions = %d, %d; want 3 and 4", v3, v4)
	}
	if got := s.CurrentVersion(histReviewOrg); got != v4 {
		t.Fatalf("current version = %d, want %d", got, v4)
	}

	// Exactly the two changes exist beyond v2: no version consumed by reviews.
	if _, err := s.Policies(histReviewOrg, 5); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("version 5 exists (err=%v); reviews must not create versions", err)
	}
	gotV1, err := s.Policies(histReviewOrg, 1)
	if err != nil {
		t.Fatal(err)
	}
	gotV2, err := s.Policies(histReviewOrg, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotV1, wantV1) {
		t.Fatalf("saved v1 content changed by overlapping reviews: %+v want %+v", gotV1, wantV1)
	}
	if !reflect.DeepEqual(gotV2, wantV2) {
		t.Fatalf("saved v2 content changed by overlapping reviews: %+v want %+v", gotV2, wantV2)
	}
	gotV3, err := s.Policies(histReviewOrg, v3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotV3, histMidAllowSet()) {
		t.Fatalf("v3 content = %+v, want exactly the published allow set", gotV3)
	}
	gotV4, err := s.Policies(histReviewOrg, v4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotV4, wantV1) {
		t.Fatalf("rollback v4 content = %+v, want a copy of v1 %+v", gotV4, wantV1)
	}

	// The audit chain holds exactly two policy-change records for this phase
	// (the publish and the rollback), gapless and correctly tagged. Reviews —
	// including envelope rejections — append nothing.
	recs := drainAudit(t, s, histReviewOrg, AuditPolicyChange, "")
	if len(recs) != 4 {
		t.Fatalf("policy-change records = %d, want 4 (v1, v2, v3 publish, v4 rollback)", len(recs))
	}
	for i, r := range recs {
		if r.Seq != i+1 || r.Kind != AuditPolicyChange {
			t.Fatalf("unexpected change record at %d: %+v", i+1, r)
		}
	}
	chg3 := recs[2].Change
	if chg3.Version != v3 || chg3.RolledBack || chg3.SourceVersion != 0 ||
		!reflect.DeepEqual(chg3.Policies, histMidAllowSet()) {
		t.Fatalf("v3 change record = %+v, want an ordinary publish of the mid allow set", chg3)
	}
	chg4 := recs[3].Change
	if chg4.Version != v4 || !chg4.RolledBack || chg4.SourceVersion != 1 ||
		!reflect.DeepEqual(chg4.Policies, wantV1) {
		t.Fatalf("v4 change record = %+v, want rollback tagged from source v1", chg4)
	}
	if len(drainAudit(t, s, histReviewOrg, AuditDecision, "")) != 0 {
		t.Fatalf("reviews appended decision records; review must be read-only")
	}
	if recsAll, cp, err := s.AuditExport(histReviewOrg, 0); err != nil || len(recsAll) != 4 {
		t.Fatalf("full audit = %d records, %v; want exactly 4", len(recsAll), err)
	} else if err := VerifyAudit(histReviewOrg, recsAll, cp); err != nil {
		t.Fatalf("audit chain no longer verifies after overlapping reviews: %v", err)
	}

	// And the historical conclusions are still pinned after the storm.
	assertHistReview(t, s, allow, req, "read-only final v1")
	assertHistReview(t, s, deny, req, "read-only final v2")
}
