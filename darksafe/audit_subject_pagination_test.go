// Regression tests for subject-filtered audit pagination. The scenarios
// here pin down how AuditQuery/AuditPage behave when a caller pages
// through one subject's history while the organization's chain keeps
// growing: policy publishes, rollbacks and other subjects' decisions are
// interleaved with the target subject's records, and new records of every
// kind may be appended between two page reads.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// subjectChain is a deterministic mixed chain in organization "acme":
//
//	seq 1  publish v1 (allow u1 read, deny u1 write, allow u2 read)
//	seq 2  u1 read  -> allow (version 1)
//	seq 3  u2 read  -> allow (version 1)
//	seq 4  publish v2 (v1 plus allow u3 read)
//	seq 5  u1 write -> deny  (version 2)
//	seq 6  u3 read  -> allow (version 2)
//	seq 7  rollback to v1, creating version 3
//	seq 8  u1 read  -> allow (version 3)
//
// Subject u1 therefore matches exactly at sequences 2, 5 and 8, with both
// an allow and a deny among them, separated by policy changes and other
// subjects' decisions.
type subjectChain struct {
	store *Store
	// u1Decisions holds the Decision values returned by the three u1
	// Decide calls, in chain order (seqs 2, 5, 8).
	u1Decisions []Decision
	// u1Requests holds the exact requests submitted for u1.
	u1Requests []OrgRequest
}

// buildSubjectChain creates the chain above and returns the captured
// decisions and requests for the target subject.
func buildSubjectChain(t *testing.T) subjectChain {
	t.Helper()
	s := NewStore()
	v1 := []Policy{
		allowPolicy("p-allow-read", "u1", "read", "org/a", false),
		{ID: "p-deny-write", Subject: "u1", Action: "write", Scope: "org/a", Effect: EffectDeny},
		allowPolicy("p-allow-u2", "u2", "read", "org/a", false),
	}
	if _, err := s.Publish("acme", 0, v1); err != nil {
		t.Fatal(err)
	}
	u1Req := func(action string) OrgRequest {
		req := request("acme", "u1", "r1", "org/a", action)
		req.Subject.Roles = []string{"auditor"}
		return req
	}
	var got subjectChain
	got.store = s

	read1 := u1Req("read")
	got.u1Requests = append(got.u1Requests, read1)
	got.u1Decisions = append(got.u1Decisions, s.Decide("acme", read1)) // seq 2
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))     // seq 3

	v2 := append(append([]Policy(nil), v1...), allowPolicy("p-allow-u3", "u3", "read", "org/a", false))
	if _, err := s.Publish("acme", 1, v2); err != nil { // seq 4
		t.Fatal(err)
	}
	write := u1Req("write")
	got.u1Requests = append(got.u1Requests, write)
	got.u1Decisions = append(got.u1Decisions, s.Decide("acme", write)) // seq 5
	s.Decide("acme", request("acme", "u3", "r1", "org/a", "read"))     // seq 6

	if _, err := s.Rollback("acme", 2, 1); err != nil { // seq 7
		t.Fatal(err)
	}
	read2 := u1Req("read")
	got.u1Requests = append(got.u1Requests, read2)
	got.u1Decisions = append(got.u1Decisions, s.Decide("acme", read2)) // seq 8
	return got
}

// drainSubjectPages pages through a pinned subject query with the given
// page size, asserting on every page that the checkpoint never changes and
// the cursor strictly advances (a repeated position would loop forever).
// It returns the concatenated records and every page seen.
func drainSubjectPages(t *testing.T, s *Store, first *AuditPage, pageSize int, kind, subject string) ([]AuditRecord, []*AuditPage) {
	t.Helper()
	pages := []*AuditPage{first}
	recs := append([]AuditRecord(nil), first.Records...)
	prevNext := 0
	for pages[len(pages)-1].Next != 0 {
		next := pages[len(pages)-1].Next
		if next <= prevNext {
			t.Fatalf("cursor did not advance: previous %d, next %d", prevNext, next)
		}
		prevNext = next
		page, err := s.AuditPage(first.Checkpoint, next, pageSize, kind, subject)
		if err != nil {
			t.Fatalf("AuditPage(%d): %v", next, err)
		}
		if page.Checkpoint != first.Checkpoint {
			t.Fatalf("checkpoint changed between pages: %+v then %+v", first.Checkpoint, page.Checkpoint)
		}
		if page.EndSeq != first.EndSeq {
			t.Fatalf("page end moved with the growing chain: %d then %d", first.EndSeq, page.EndSeq)
		}
		pages = append(pages, page)
		recs = append(recs, page.Records...)
	}
	return recs, pages
}

// seqsOf extracts the sequence numbers of a record slice.
func seqsOf(recs []AuditRecord) []int {
	out := make([]int, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}

func TestSubjectFilterReturnsOnlyThatSubjectsDecisions(t *testing.T) {
	ch := buildSubjectChain(t)
	s := ch.store

	// Another organization uses the same subject identifier; its records
	// must never influence the acme view.
	if _, err := s.Publish("globex", 0, []Policy{allowPolicy("g1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))

	recs := drainAudit(t, s, "acme", "", "u1")
	wantSeqs := []int{2, 5, 8}
	if !reflect.DeepEqual(seqsOf(recs), wantSeqs) {
		t.Fatalf("u1 seqs = %v, want %v", seqsOf(recs), wantSeqs)
	}
	sawAllow, sawDeny := false, false
	for i, r := range recs {
		if r.Org != "acme" {
			t.Fatalf("record %d leaked from organization %q", r.Seq, r.Org)
		}
		if r.Kind != AuditDecision || r.Decision == nil || r.Change != nil {
			t.Fatalf("record %d is not a pure decision: %+v", r.Seq, r)
		}
		// The original request, including roles, survives intact.
		if !reflect.DeepEqual(r.Decision.Request, ch.u1Requests[i]) {
			t.Fatalf("record %d request = %+v, want %+v", r.Seq, r.Decision.Request, ch.u1Requests[i])
		}
		// The full decision explanation is the one originally returned.
		if !reflect.DeepEqual(r.Decision.Decision, ch.u1Decisions[i]) {
			t.Fatalf("record %d decision = %+v, want %+v", r.Seq, r.Decision.Decision, ch.u1Decisions[i])
		}
		if r.Decision.Decision.Reason == "" || r.Decision.Decision.Version == 0 {
			t.Fatalf("record %d lost its explanation: %+v", r.Seq, r.Decision.Decision)
		}
		if r.Decision.Decision.Allowed {
			sawAllow = true
		} else {
			sawDeny = true
		}
	}
	if !sawAllow || !sawDeny {
		t.Fatalf("subject history must keep allows and denies, got %+v", seqsOf(recs))
	}

	// The globex chain is queryable on its own and disjoint from acme's.
	globexRecs := drainAudit(t, s, "globex", "", "u1")
	if !reflect.DeepEqual(seqsOf(globexRecs), []int{2, 3}) {
		t.Fatalf("globex u1 seqs = %v, want [2 3]", seqsOf(globexRecs))
	}
	// Re-reading acme after the globex appends changes nothing.
	again := drainAudit(t, s, "acme", "", "u1")
	if !reflect.DeepEqual(seqsOf(again), wantSeqs) {
		t.Fatalf("acme u1 seqs after globex activity = %v, want %v", seqsOf(again), wantSeqs)
	}
}

func TestSubjectAndCategoryFiltersAreBothRequired(t *testing.T) {
	ch := buildSubjectChain(t)
	s := ch.store

	// decision + subject: exactly the subject's decisions.
	decisions := drainAudit(t, s, "acme", AuditDecision, "u1")
	if !reflect.DeepEqual(seqsOf(decisions), []int{2, 5, 8}) {
		t.Fatalf("decision+u1 seqs = %v, want [2 5 8]", seqsOf(decisions))
	}
	// policy_change + subject: the subject has no policy changes, so the
	// conjunction is empty. Treating either filter as a fallback would
	// return the three changes or the three decisions instead.
	changes := drainAudit(t, s, "acme", AuditPolicyChange, "u1")
	if len(changes) != 0 {
		t.Fatalf("policy_change+u1 = %v, want empty (filters must both apply)", seqsOf(changes))
	}
	// A subject that only ever produced decisions likewise sees no changes.
	if got := drainAudit(t, s, "acme", AuditPolicyChange, "u2"); len(got) != 0 {
		t.Fatalf("policy_change+u2 = %v, want empty", seqsOf(got))
	}
}

func TestSubjectPaginationCountsMatchesNotScannedRecords(t *testing.T) {
	ch := buildSubjectChain(t)
	s := ch.store

	// Page size 2 against matches at seqs 2, 5, 8: the first page must
	// skip over the non-matching seqs 3 and 4 and still deliver two
	// records instead of stopping after two scanned records.
	first, err := s.AuditQuery("acme", 1, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 2 || !reflect.DeepEqual(seqsOf(first.Records), []int{2, 5}) {
		t.Fatalf("first page = %v, want [2 5]", seqsOf(first.Records))
	}
	recs, pages := drainSubjectPages(t, s, first, 2, "", "u1")
	if !reflect.DeepEqual(seqsOf(recs), []int{2, 5, 8}) {
		t.Fatalf("paged u1 seqs = %v, want [2 5 8]", seqsOf(recs))
	}
	// Page sizes reflect matched records: 2 then 1, and the second page
	// terminates the range.
	if len(pages) != 2 || len(pages[1].Records) != 1 || pages[1].Next != 0 {
		t.Fatalf("unexpected page shape: %d pages, last %+v", len(pages), pages[len(pages)-1])
	}
	// Ascending by original sequence, no duplicates.
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq <= recs[i-1].Seq {
			t.Fatalf("sequences not strictly ascending: %v", seqsOf(recs))
		}
	}
}

func TestSubjectPaginationFullPageThenNonMatchingTailTerminates(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq 1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                 // seq 2, match
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                 // seq 3, match
	s.Publish("acme", 1, nil)                                                      // seq 4
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))                 // seq 5

	// The two matches exactly fill the first page; non-matching records
	// remain in the pinned range. The next page may be empty, but it must
	// end the walk rather than hand back the same cursor again.
	first, err := s.AuditQuery("acme", 1, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqsOf(first.Records), []int{2, 3}) || first.Next == 0 {
		t.Fatalf("first page = %v next %d, want [2 3] with a continuation", seqsOf(first.Records), first.Next)
	}
	second, err := s.AuditPage(first.Checkpoint, first.Next, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 0 {
		t.Fatalf("tail page = %v, want empty (only non-matching records remain)", seqsOf(second.Records))
	}
	if second.Next != 0 {
		t.Fatalf("empty tail page must terminate the range, got next %d", second.Next)
	}
	// Asking once more at the exhausted cursor stays empty and finished.
	third, err := s.AuditPage(first.Checkpoint, 0, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Records) != 0 || third.Next != 0 {
		t.Fatalf("exhausted cursor page = %+v, want empty and finished", third)
	}
}

func TestSubjectPaginationStartSeqExcludesEarlierMatches(t *testing.T) {
	ch := buildSubjectChain(t)
	s := ch.store

	// Starting at seq 5 must not backfill the earlier match at seq 2.
	page, err := s.AuditQuery("acme", 5, 10, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqsOf(page.Records), []int{5, 8}) {
		t.Fatalf("from seq 5: %v, want [5 8]", seqsOf(page.Records))
	}
	// A start between matches behaves the same: only matches at or after
	// the start are returned.
	page, err = s.AuditQuery("acme", 3, 10, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqsOf(page.Records), []int{5, 8}) {
		t.Fatalf("from seq 3: %v, want [5 8]", seqsOf(page.Records))
	}
	// Starting exactly at the last record yields just that record's match.
	page, err = s.AuditQuery("acme", 8, 10, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqsOf(page.Records), []int{8}) || page.Next != 0 {
		t.Fatalf("from seq 8: %v next %d, want [8] finished", seqsOf(page.Records), page.Next)
	}
	// A start beyond the current head is rejected, not silently clamped.
	if _, err := s.AuditQuery("acme", 9, 10, "", "u1"); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("start beyond head err = %v, want ErrInvalidRange", err)
	}
}

func TestSubjectPaginationCheckpointPinsAgainstConcurrentAppends(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow-read", "u1", "read", "org/a", false),
		{ID: "p-deny-write", Subject: "u1", Action: "write", Scope: "org/a", Effect: EffectDeny},
	}) // seq 1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))  // seq 2, match
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))  // seq 3
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "write")) // seq 4, match (deny)

	first, err := s.AuditQuery("acme", 1, 1, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqsOf(first.Records), []int{2}) || first.Checkpoint.EndSeq != 4 {
		t.Fatalf("first page = %v end %d, want [2] pinned at 4", seqsOf(first.Records), first.Checkpoint.EndSeq)
	}
	pinned := first.Checkpoint

	// The chain grows before the next page is read: the same subject
	// produces a new allow and a new deny, and a policy is published.
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))  // seq 5
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "write")) // seq 6
	if _, err := s.Publish("acme", 1, nil); err != nil {             // seq 7
		t.Fatal(err)
	}

	// Following the original checkpoint still sees only the pinned range:
	// the seq 5 and 6 matches appended after the first query stay out.
	recs, pages := drainSubjectPages(t, s, first, 1, "", "u1")
	if !reflect.DeepEqual(seqsOf(recs), []int{2, 4}) {
		t.Fatalf("pinned walk = %v, want [2 4]", seqsOf(recs))
	}
	for i, p := range pages {
		if p.Checkpoint != pinned {
			t.Fatalf("page %d checkpoint = %+v, want pinned %+v", i, p.Checkpoint, pinned)
		}
		if p.EndSeq != 4 {
			t.Fatalf("page %d end = %d, want pinned 4 despite chain head 7", i, p.EndSeq)
		}
	}

	// A fresh query pins a new checkpoint and sees the appended matches.
	fresh, err := s.AuditQuery("acme", 1, 10, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seqsOf(fresh.Records), []int{2, 4, 5, 6}) {
		t.Fatalf("fresh query = %v, want [2 4 5 6]", seqsOf(fresh.Records))
	}
	if fresh.Checkpoint.EndSeq != 7 || fresh.Checkpoint == pinned {
		t.Fatalf("fresh checkpoint = %+v, want a new pin at 7", fresh.Checkpoint)
	}
	// The fresh walk keeps both the allow and the deny that were appended.
	var allows, denies int
	for _, r := range fresh.Records {
		if r.Seq < 5 {
			continue
		}
		if r.Decision.Decision.Allowed {
			allows++
		} else {
			denies++
		}
	}
	if allows != 1 || denies != 1 {
		t.Fatalf("appended records: %d allows, %d denies, want 1 each", allows, denies)
	}
}

func TestSubjectFilterWithoutMatchesReturnsEmpty(t *testing.T) {
	ch := buildSubjectChain(t)
	s := ch.store

	// A subject that never appears: an empty, finished page, not an error
	// and never a fallback to the unfiltered chain.
	page, err := s.AuditQuery("acme", 1, 4, "", "ghost")
	if err != nil {
		t.Fatalf("unknown subject query err = %v", err)
	}
	if len(page.Records) != 0 || page.Next != 0 {
		t.Fatalf("unknown subject page = %+v, want empty and finished", page)
	}
	if got := drainAudit(t, s, "acme", AuditDecision, "ghost"); len(got) != 0 {
		t.Fatalf("unknown subject drained %v, want empty", seqsOf(got))
	}
	// The chain itself is untouched and still holds its eight records.
	if all := drainAudit(t, s, "acme", "", ""); len(all) != 8 {
		t.Fatalf("unfiltered chain = %d records, want 8", len(all))
	}
	// An organization with no records at all answers the same way.
	empty, err := s.AuditQuery("nowhere", 1, 4, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Records) != 0 || empty.Next != 0 || empty.Checkpoint.EndSeq != 0 {
		t.Fatalf("empty organization page = %+v", empty)
	}
}

func TestSubjectPaginationRejectsTamperedCheckpointFingerprint(t *testing.T) {
	ch := buildSubjectChain(t)
	s := ch.store

	first, err := s.AuditQuery("acme", 1, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Next == 0 {
		t.Fatal("test needs a multi-page range")
	}
	// Flipping the fingerprint must fail with ErrInvalidRange and deliver
	// no partial page.
	forged := first.Checkpoint
	forged.Fingerprint = "00" + forged.Fingerprint[2:]
	page, err := s.AuditPage(forged, first.Next, 2, "", "u1")
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("forged fingerprint err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		t.Fatalf("forged fingerprint delivered partial page %+v", page)
	}
	// A fingerprint borrowed from another organization's checkpoint at the
	// same end sequence is equally invalid here.
	other, err := s.AuditQuery("globex", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cross := first.Checkpoint
	cross.Fingerprint = other.Checkpoint.Fingerprint
	if _, err := s.AuditPage(cross, first.Next, 2, "", "u1"); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("cross-organization fingerprint err = %v, want ErrInvalidRange", err)
	}
	// The genuine checkpoint still works afterwards.
	if _, err := s.AuditPage(first.Checkpoint, first.Next, 2, "", "u1"); err != nil {
		t.Fatalf("genuine checkpoint rejected after forgery attempts: %v", err)
	}
}

func TestSubjectPaginationIsReadOnly(t *testing.T) {
	ch := buildSubjectChain(t)
	s := ch.store

	beforeVersion := s.CurrentVersion("acme")
	before, beforeCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Page through the subject's history in small steps, then run every
	// read-only entry point used by the walk again.
	first, err := s.AuditQuery("acme", 1, 1, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	drainSubjectPages(t, s, first, 1, "", "u1")
	drainAudit(t, s, "acme", AuditDecision, "u1")
	drainAudit(t, s, "acme", "", "ghost")

	if got := s.CurrentVersion("acme"); got != beforeVersion {
		t.Fatalf("querying changed the policy version: %d -> %d", beforeVersion, got)
	}
	after, afterCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if afterCP != beforeCP {
		t.Fatalf("querying appended records: checkpoint %+v -> %+v", beforeCP, afterCP)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("querying changed the stored records")
	}
}
