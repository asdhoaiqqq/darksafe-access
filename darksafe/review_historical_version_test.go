// Regression coverage for Store.Review against specified historical policy
// versions while the same organization is concurrently publishing and
// rolling back. The reviewed request never changes: one enabled subject
// reading one resource, with the decision, subject and resource
// organizations all equal and every request field and the scope valid.
//
// The fixture holds three pre-existing versions with distinguishable
// outcomes: v1 allows through policy "hist-v1-allow", v2 denies through
// policy "hist-v2-deny", and v3 is a published EMPTY set. The organization
// then publishes v4 (an allow whose policy id differs from every historical
// one) and rolls back to v2, producing v5 whose content equals v2's. A
// review that read current state mid-flight would be detectable: it would
// flip the allowance, return the current policy id, or report the current
// version number. Every review of a historical version must instead return
// exactly that version's conclusion — allowance, reason, matched policy
// identifiers and the original version number — and a rollback's new
// version number must never be reported as the requested old one.
//
// Review is read-only: after the overlapping operations, historical policy
// content, the current version and the audit chain must reflect only the
// successful publishes and the rollback, with no extra records or versions
// however many reviews ran.
package darksafe

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

const reviewHistOrg = "acme"

// histReviewV1Allow is the published v1: an allow for the probe request,
// carrying a policy id no other version uses.
func histReviewV1Allow() []Policy {
	return []Policy{{
		ID: "hist-v1-allow", Subject: "reviewer", Action: "read",
		Scope: "org/acme/ledger", Effect: EffectAllow,
	}}
}

// histReviewV2Deny is the published v2: an explicit deny for the same
// subject, action and scope, with its own identifiable policy id.
func histReviewV2Deny() []Policy {
	return []Policy{{
		ID: "hist-v2-deny", Subject: "reviewer", Action: "read",
		Scope: "org/acme/ledger", Effect: EffectDeny,
	}}
}

// histReviewV4Allow is the later published v4: it allows the probe like v1
// but through a fresh policy id, so a review that borrowed current content
// is unmasked by its matched list.
func histReviewV4Allow() []Policy {
	return []Policy{{
		ID: "current-v4-allow", Subject: "reviewer", Action: "read",
		Scope: "org/acme/ledger", Effect: EffectAllow,
	}}
}

// histReviewRequest is the single probe used by every review: an enabled
// subject, all three organizations equal, and a valid scope.
func histReviewRequest() OrgRequest {
	return request(reviewHistOrg, "reviewer", "ledger-7", "org/acme/ledger", "read")
}

// buildHistReviewStore publishes v1 (allow), v2 (deny) and v3 (empty set),
// leaving the organization at current version 3 with three policy-change
// audit records.
func buildHistReviewStore(t *testing.T) (*Store, OrgRequest) {
	t.Helper()
	s := NewStore()
	if v, err := s.Publish(reviewHistOrg, 0, histReviewV1Allow()); err != nil || v != 1 {
		t.Fatalf("publish v1 allow = %d, %v; want 1, nil", v, err)
	}
	if v, err := s.Publish(reviewHistOrg, 1, histReviewV2Deny()); err != nil || v != 2 {
		t.Fatalf("publish v2 deny = %d, %v; want 2, nil", v, err)
	}
	if v, err := s.Publish(reviewHistOrg, 2, nil); err != nil || v != 3 {
		t.Fatalf("publish v3 empty = %d, %v; want 3, nil", v, err)
	}
	req := histReviewRequest()
	// Document the fixture invariant: the three versions decide the probe
	// three different ways (allow, explicit deny, default deny).
	if d := evaluate(req, histReviewV1Allow(), 1); !d.Allowed {
		t.Fatalf("fixture: v1 must allow the probe, got %+v", d)
	}
	if d := evaluate(req, histReviewV2Deny(), 2); d.Allowed || d.Reason != "matched deny policy" {
		t.Fatalf("fixture: v2 must explicitly deny the probe, got %+v", d)
	}
	if d := evaluate(req, nil, 3); d.Allowed || d.Reason != "no matching allow policy" {
		t.Fatalf("fixture: v3 empty set must default-deny the probe, got %+v", d)
	}
	return s, req
}

// sameStrings compares two identifier lists, treating nil and empty as the
// same set of hits.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffReviewDecision returns "" when got matches the wanted outcome field
// by field, otherwise a description of the first differing field, so a
// failure names exactly what drifted: the allowance, the reason, the
// matched policies, or the version actually used.
func diffReviewDecision(got Decision, wantAllowed bool, wantReason string, wantMatched []string, wantVersion int) string {
	if got.Allowed != wantAllowed {
		return fmt.Sprintf("Allowed = %v, want %v", got.Allowed, wantAllowed)
	}
	if got.Reason != wantReason {
		return fmt.Sprintf("Reason = %q, want %q", got.Reason, wantReason)
	}
	if !sameStrings(got.Matched, wantMatched) {
		return fmt.Sprintf("Matched = %v, want %v", got.Matched, wantMatched)
	}
	if got.Version != wantVersion {
		return fmt.Sprintf("Version = %d, want %d", got.Version, wantVersion)
	}
	return ""
}

// checkReviewDecision asserts one review outcome field by field.
func checkReviewDecision(t *testing.T, label string, got Decision, wantAllowed bool, wantReason string, wantMatched []string, wantVersion int) {
	t.Helper()
	if d := diffReviewDecision(got, wantAllowed, wantReason, wantMatched, wantVersion); d != "" {
		t.Fatalf("%s: %s (full decision %+v)", label, d, got)
	}
}

// checkPinnedReviews reviews every version whose outcome must stay pinned
// for the whole scenario: the allowing v1, the denying v2, the empty v3
// and the never-existing v99. Each check compares the full explanation,
// not just the allowance.
func checkPinnedReviews(t *testing.T, s *Store, req OrgRequest, label string) {
	t.Helper()
	checkReviewDecision(t, label+": review v1 (historical allow)",
		s.Review(reviewHistOrg, 1, req),
		true, "matched allow policy", []string{"hist-v1-allow"}, 1)
	checkReviewDecision(t, label+": review v2 (historical deny)",
		s.Review(reviewHistOrg, 2, req),
		false, "matched deny policy", []string{"hist-v2-deny"}, 2)
	checkReviewDecision(t, label+": review v3 (published empty set)",
		s.Review(reviewHistOrg, 3, req),
		false, "no matching allow policy", nil, 3)
	checkReviewDecision(t, label+": review v99 (never existed)",
		s.Review(reviewHistOrg, 99, req),
		false, "version 99 not found", nil, 0)
}

// advanceToV5 publishes the v4 allow set and rolls back to v2, leaving the
// organization at current version 5 whose content equals v2's.
func advanceToV5(t *testing.T, s *Store) {
	t.Helper()
	if v, err := s.Publish(reviewHistOrg, 3, histReviewV4Allow()); err != nil || v != 4 {
		t.Fatalf("publish v4 allow = %d, %v; want 4, nil", v, err)
	}
	if v, err := s.Rollback(reviewHistOrg, 4, 2); err != nil || v != 5 {
		t.Fatalf("rollback to v2 = %d, %v; want new version 5, not target 2", v, err)
	}
}

// checkHistReviewFinalState verifies the complete post-change contract:
// historical content intact, current version and audit chain reflecting
// only the five successful changes, and every review — historical, new and
// missing — still returning its own version's explanation.
func checkHistReviewFinalState(t *testing.T, s *Store, req OrgRequest) {
	t.Helper()

	if got := s.CurrentVersion(reviewHistOrg); got != 5 {
		t.Fatalf("current version = %d, want 5 (three publishes, one publish, one rollback)", got)
	}

	// Every saved version keeps its original content; the rollback's v5
	// holds a copy of v2's deny set, and the empty v3 stays empty.
	wantContent := map[int][]Policy{
		1: histReviewV1Allow(),
		2: histReviewV2Deny(),
		3: nil,
		4: histReviewV4Allow(),
		5: histReviewV2Deny(),
	}
	for version, want := range wantContent {
		got, err := s.Policies(reviewHistOrg, version)
		if err != nil {
			t.Fatalf("Policies(v%d): %v", version, err)
		}
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("version %d content = %+v, want %+v (reviews must not rewrite history)", version, got, want)
		}
	}

	// The audit chain holds exactly the five successful policy changes and
	// nothing else: no review ever appended a record, and no failed or
	// retried operation consumed a sequence.
	recs := drainAudit(t, s, reviewHistOrg, "", "")
	if len(recs) != 5 {
		t.Fatalf("audit records = %d, want exactly 5 policy changes (reviews are read-only)", len(recs))
	}
	for i, r := range recs {
		if r.Seq != i+1 || r.Kind != AuditPolicyChange || r.Change == nil {
			t.Fatalf("record %d = %+v, want gapless policy-change records only", i, r)
		}
		if r.Change.Version != i+1 {
			t.Fatalf("record %d names version %d, want %d", i, r.Change.Version, i+1)
		}
	}
	for i := 0; i < 4; i++ {
		if recs[i].Change.RolledBack || recs[i].Change.SourceVersion != 0 {
			t.Fatalf("record %d is tagged as a rollback: %+v", i, recs[i].Change)
		}
	}
	if !recs[4].Change.RolledBack || recs[4].Change.SourceVersion != 2 {
		t.Fatalf("record 5 must be the rollback naming source v2: %+v", recs[4].Change)
	}
	exported, cp, err := s.AuditExport(reviewHistOrg, 0)
	if err != nil {
		t.Fatalf("AuditExport: %v", err)
	}
	if err := VerifyAudit(reviewHistOrg, exported, cp); err != nil {
		t.Fatalf("chain does not verify after overlapping reviews: %v", err)
	}

	// Historical reviews remain pinned to their own versions.
	checkPinnedReviews(t, s, req, "final")
	// The new versions review as themselves: v4 allows through its own
	// policy id, and v5 denies through v2's policy id but reports version 5.
	checkReviewDecision(t, "final: review v4 (published allow)",
		s.Review(reviewHistOrg, 4, req),
		true, "matched allow policy", []string{"current-v4-allow"}, 4)
	checkReviewDecision(t, "final: review v5 (rollback of v2)",
		s.Review(reviewHistOrg, 5, req),
		false, "matched deny policy", []string{"hist-v2-deny"}, 5)
}

// TestReviewHistoricalVersionsPinnedAcrossPublishAndRollback drives the
// changes sequentially and re-checks every pinned review after each step,
// so a regression is attributed to the exact change that broke it.
func TestReviewHistoricalVersionsPinnedAcrossPublishAndRollback(t *testing.T) {
	s, req := buildHistReviewStore(t)

	// Baseline before any further change.
	checkPinnedReviews(t, s, req, "baseline")

	// Publishing v4 flips the current conclusion to allow through a new
	// policy id; no historical review may notice.
	if v, err := s.Publish(reviewHistOrg, 3, histReviewV4Allow()); err != nil || v != 4 {
		t.Fatalf("publish v4 allow = %d, %v; want 4, nil", v, err)
	}
	checkPinnedReviews(t, s, req, "after publish v4")

	// Rolling back to v2 makes the current content equal to the historical
	// deny again, as the NEW version 5. Reviews of v2 must keep reporting
	// version 2, never the rollback's new number.
	if v, err := s.Rollback(reviewHistOrg, 4, 2); err != nil || v != 5 {
		t.Fatalf("rollback to v2 = %d, %v; want new version 5, not target 2", v, err)
	}
	checkPinnedReviews(t, s, req, "after rollback to v2 as v5")

	// Read-only: five change records, intact content, no review side effects.
	checkHistReviewFinalState(t, s, req)

	// The current decision path diverges from the historical one: Decide
	// denies through the rolled-back v5 while Review(v1) still allows
	// through v1. (Decide intentionally appends its own record, so it runs
	// after the read-only audit check above.)
	checkReviewDecision(t, "decide at current v5",
		s.Decide(reviewHistOrg, req),
		false, "matched deny policy", []string{"hist-v2-deny"}, 5)
	checkReviewDecision(t, "review v1 still historical after decide",
		s.Review(reviewHistOrg, 1, req),
		true, "matched allow policy", []string{"hist-v1-allow"}, 1)
}

// TestReviewHistoricalVersionsConcurrentWithChanges runs the publish and
// the rollback concurrently with a storm of historical reviews. Every
// single review, whenever it lands, must return exactly the pinned outcome
// of the version it named — never a splice of historical allowance with
// current content, another version's matched policies, or another
// version's number.
func TestReviewHistoricalVersionsConcurrentWithChanges(t *testing.T) {
	s, req := buildHistReviewStore(t)

	type pinned struct {
		version     int
		wantAllowed bool
		wantReason  string
		wantMatched []string
		wantVersion int
	}
	// The versions under continuous review while the changes race.
	pins := []pinned{
		{1, true, "matched allow policy", []string{"hist-v1-allow"}, 1},
		{2, false, "matched deny policy", []string{"hist-v2-deny"}, 2},
		{3, false, "no matching allow policy", nil, 3},
		{99, false, "version 99 not found", nil, 0},
	}

	const reviewers = 8
	const rounds = 300
	start := make(chan struct{})
	var failed atomic.Bool
	fail := func(format string, args ...any) {
		// Report only the first mismatch; one failure description already
		// identifies the drifted field, version and review target.
		if failed.CompareAndSwap(false, true) {
			t.Errorf(format, args...)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < reviewers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for n := 0; n < rounds && !failed.Load(); n++ {
				for _, p := range pins {
					got := s.Review(reviewHistOrg, p.version, req)
					if d := diffReviewDecision(got, p.wantAllowed, p.wantReason, p.wantMatched, p.wantVersion); d != "" {
						fail("reviewer %d round %d, review v%d during concurrent changes: %s (full decision %+v)",
							id, n, p.version, d, got)
						return
					}
				}
			}
		}(i)
	}

	// The changer publishes v4 then rolls back to v2 as v5, from one
	// goroutine so the final version numbering is deterministic.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if v, err := s.Publish(reviewHistOrg, 3, histReviewV4Allow()); err != nil || v != 4 {
			fail("concurrent publish v4 = %d, %v; want 4, nil", v, err)
			return
		}
		if v, err := s.Rollback(reviewHistOrg, 4, 2); err != nil || v != 5 {
			fail("concurrent rollback to v2 = %d, %v; want new version 5", v, err)
			return
		}
	}()

	close(start)
	wg.Wait()

	// After the storm, the whole final-state contract holds: history
	// intact, exactly five change records, every review pinned.
	checkHistReviewFinalState(t, s, req)
}

// TestReviewEmptyVersionIsNotVersionZero pins the boundary between a
// published empty policy set and the never-published version zero: both
// deny, but the empty version is a real historical version and must review
// with its own version number, the default-denial reason and an empty
// matched list, before and after a later allow is published.
func TestReviewEmptyVersionIsNotVersionZero(t *testing.T) {
	s := NewStore()
	req := histReviewRequest()
	if v, err := s.Publish(reviewHistOrg, 0, nil); err != nil || v != 1 {
		t.Fatalf("publish empty v1 = %d, %v; want 1, nil", v, err)
	}

	// The empty set is a genuine version: default denial under version 1.
	checkReviewDecision(t, "review empty v1",
		s.Review(reviewHistOrg, 1, req),
		false, "no matching allow policy", nil, 1)
	// Version zero stays the never-published state and must not absorb the
	// empty version's identity.
	checkReviewDecision(t, "review version zero",
		s.Review(reviewHistOrg, 0, req),
		false, "version 0 not found", nil, 0)

	// A later allow becomes current; the empty version's review must not
	// borrow it.
	if v, err := s.Publish(reviewHistOrg, 1, histReviewV4Allow()); err != nil || v != 2 {
		t.Fatalf("publish allow v2 = %d, %v; want 2, nil", v, err)
	}
	checkReviewDecision(t, "review empty v1 after allow v2",
		s.Review(reviewHistOrg, 1, req),
		false, "no matching allow policy", nil, 1)
	checkReviewDecision(t, "review allow v2",
		s.Review(reviewHistOrg, 2, req),
		true, "matched allow policy", []string{"current-v4-allow"}, 2)
	checkReviewDecision(t, "review version zero after publishes",
		s.Review(reviewHistOrg, 0, req),
		false, "version 0 not found", nil, 0)

	// Read-only: the chain holds exactly the two publishes.
	recs := drainAudit(t, s, reviewHistOrg, "", "")
	if len(recs) != 2 {
		t.Fatalf("audit records = %d, want 2 (reviews are read-only)", len(recs))
	}
	for i, r := range recs {
		if r.Kind != AuditPolicyChange || r.Change == nil || r.Change.Version != i+1 {
			t.Fatalf("record %d = %+v, want policy change for version %d", i, r, i+1)
		}
	}
}
