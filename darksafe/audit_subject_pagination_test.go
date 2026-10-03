// Regression coverage for subject-filtered audit pagination, with the
// emphasis on reads that race a growing chain. An auditor paging one
// subject's history sees that organization's chain interleaved with policy
// publishes, rollbacks and other subjects' decisions, so the subject's own
// records may sit far apart. These tests pin the contract of the existing
// AuditQuery/AuditPage entry points for that view:
//
//   - The subject filter selects exactly that subject's decision records
//     (allows and denials alike), preserving original sequence, request and
//     the complete decision explanation; policy changes and other subjects
//     never mix in, and an identical subject identifier in another
//     organization has no effect.
//   - A category filter combined with a subject filter is a conjunction,
//     never a fallback: a record must satisfy both.
//   - The page size bounds how many MATCHING records a page returns, not
//     how many chain positions were scanned; non-matching records between
//     matches never truncate a page early. Concatenating the pages along
//     the returned cursors yields the matching sequences in ascending
//     order with no duplicates or gaps, a legal start sequence excludes
//     earlier matches, and a page filled exactly while non-matching
//     records remain is followed by one empty page that terminates the
//     walk instead of repeating a cursor.
//   - The first page's end sequence and fingerprint constrain every later
//     page: records appended after the first query — even this subject's
//     own new allows and denials, or new policy publishes — never mix into
//     the pinned walk, every page carries the identical checkpoint, and
//     only a fresh query observes the new tail.
//   - A filter matching nothing returns an empty page that ends the walk,
//     never an error and never the unfiltered chain; a checkpoint whose
//     fingerprint was tampered with fails with ErrInvalidRange and
//     delivers no partial records.
//
// All of this is read-only: querying and paging never change the current
// policy version and never append audit records.
package darksafe

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

// subjectPageWalk pages a filtered query to exhaustion, returning the
// concatenated records and the checkpoint of every page seen. It fails the
// test if a cursor repeats (the walk would never terminate) or if any page
// carries a checkpoint different from the first page's.
func subjectPageWalk(t *testing.T, s *Store, org string, startSeq, pageSize int, kind, subject string) ([]AuditRecord, []Checkpoint) {
	t.Helper()
	page, err := s.AuditQuery(org, startSeq, pageSize, kind, subject)
	if err != nil {
		t.Fatalf("AuditQuery(%q, start %d): %v", org, startSeq, err)
	}
	var recs []AuditRecord
	cps := []Checkpoint{page.Checkpoint}
	seen := map[int]bool{}
	for {
		recs = append(recs, page.Records...)
		if page.Next == 0 {
			break
		}
		if seen[page.Next] {
			t.Fatalf("cursor %d returned twice; paging never terminates", page.Next)
		}
		seen[page.Next] = true
		page, err = s.AuditPage(page.Checkpoint, page.Next, pageSize, kind, subject)
		if err != nil {
			t.Fatalf("AuditPage(next %d): %v", page.Next, err)
		}
		if page.Checkpoint != cps[0] {
			t.Fatalf("page checkpoint = %+v, want the first page's %+v", page.Checkpoint, cps[0])
		}
		cps = append(cps, page.Checkpoint)
	}
	return recs, cps
}

// recordSeqs returns the sequences of the records in order.
func recordSeqs(recs []AuditRecord) []int {
	seqs := make([]int, len(recs))
	for i, r := range recs {
		seqs[i] = r.Seq
	}
	return seqs
}

// requireSubjectDecisions fails unless every record is a decision of the
// given subject in the given organization.
func requireSubjectDecisions(t *testing.T, org, subject string, recs []AuditRecord) {
	t.Helper()
	for _, r := range recs {
		if r.Org != org || r.Kind != AuditDecision || r.Decision == nil {
			t.Fatalf("subject filter leaked %+v", r)
		}
		if r.Decision.Request.Subject.ID != subject {
			t.Fatalf("record seq %d belongs to subject %q, want %q",
				r.Seq, r.Decision.Request.Subject.ID, subject)
		}
	}
}

// subjectFilterFixture builds one organization whose chain interleaves the
// target subject's allows and denials with publishes, a rollback and other
// subjects' decisions:
//
//	seq 1  publish v1 (allow u1 read, deny u1 write)
//	seq 2  u1 read   -> allow (v1)
//	seq 3  u2 read   -> no-match denial
//	seq 4  u1 write  -> matched-deny (v1)
//	seq 5  publish v2 (empty set)
//	seq 6  u2 read   -> no-match denial
//	seq 7  rollback to v1 -> v3
//	seq 8  u1 read   -> allow (v3)
//
// It returns the submitted requests and returned decisions of the target
// subject so tests can verify the preserved content.
func subjectFilterFixture(t *testing.T, s *Store) (reqs map[int]OrgRequest, decisions map[int]Decision) {
	t.Helper()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
		{ID: "p-deny", Subject: "u1", Action: "write", Scope: "org/a", Effect: EffectDeny},
	}); err != nil {
		t.Fatal(err)
	}
	reqs = map[int]OrgRequest{}
	decisions = map[int]Decision{}
	decide := func(seq int, subject, action string) {
		t.Helper()
		req := request("acme", subject, "r1", "org/a", action)
		d := s.Decide("acme", req)
		reqs[seq] = req
		decisions[seq] = d
	}
	decide(2, "u1", "read")
	decide(3, "u2", "read")
	decide(4, "u1", "write")
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	decide(6, "u2", "read")
	if _, err := s.Rollback("acme", 2, 1); err != nil {
		t.Fatal(err)
	}
	decide(8, "u1", "read")
	if !decisions[2].Allowed || decisions[4].Allowed || !decisions[8].Allowed {
		t.Fatalf("fixture decisions broken: %+v", decisions)
	}
	return reqs, decisions
}

// TestSubjectFilterSelectsOnlyThatSubjectsDecisions pins the filter rule:
// from a chain mixing policy changes, a rollback and other subjects, the
// subject view returns exactly the target subject's decisions — allows and
// denials — with their original sequences, full requests and complete
// decision explanations. The same subject identifier in another
// organization neither leaks in nor is affected.
func TestSubjectFilterSelectsOnlyThatSubjectsDecisions(t *testing.T) {
	s := NewStore()
	reqs, decisions := subjectFilterFixture(t, s)

	// The same subject identifier actively deciding in another organization.
	if _, err := s.Publish("globex", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))

	recs, _ := subjectPageWalk(t, s, "acme", 1, 2, "", "u1")
	if got := recordSeqs(recs); !reflect.DeepEqual(got, []int{2, 4, 8}) {
		t.Fatalf("u1 seqs = %v, want [2 4 8]", got)
	}
	requireSubjectDecisions(t, "acme", "u1", recs)
	// Original sequence, full request and complete decision explanation.
	for _, r := range recs {
		if !reflect.DeepEqual(r.Decision.Request, reqs[r.Seq]) {
			t.Fatalf("seq %d request = %+v, want %+v", r.Seq, r.Decision.Request, reqs[r.Seq])
		}
		if !reflect.DeepEqual(r.Decision.Decision, decisions[r.Seq]) {
			t.Fatalf("seq %d decision = %+v, want %+v", r.Seq, r.Decision.Decision, decisions[r.Seq])
		}
	}
	// Both outcomes survive: the allow, the matched-deny and the later allow.
	if !recs[0].Decision.Decision.Allowed || recs[1].Decision.Decision.Allowed ||
		!recs[2].Decision.Decision.Allowed {
		t.Fatalf("allow/deny outcomes not preserved: %+v", recs)
	}
	if recs[1].Decision.Decision.Reason != "matched deny policy" {
		t.Fatalf("deny explanation = %+v", recs[1].Decision.Decision)
	}

	// The other organization's identical subject identifier is invisible
	// here, and its own view holds only its own records.
	globex, _ := subjectPageWalk(t, s, "globex", 1, 1, "", "u1")
	if got := recordSeqs(globex); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("globex u1 seqs = %v, want [2 3]", got)
	}
	requireSubjectDecisions(t, "globex", "u1", globex)
	if globex[0].Fingerprint == recs[0].Fingerprint {
		t.Fatal("identical subject identifiers across organizations share a fingerprint")
	}

	// Reads appended nothing and changed no version in either organization.
	if got := len(drainAudit(t, s, "acme", "", "")); got != 8 {
		t.Fatalf("acme records = %d, want 8; queries must not append", got)
	}
	if got := len(drainAudit(t, s, "globex", "", "")); got != 3 {
		t.Fatalf("globex records = %d, want 3; queries must not append", got)
	}
	if v := s.CurrentVersion("acme"); v != 3 {
		t.Fatalf("acme version = %d, want 3; queries must not republish", v)
	}
}

// TestSubjectAndCategoryFiltersAreConjunctive pins that specifying both a
// category and a subject requires both conditions: the category is not a
// fallback for the subject, nor the other way round.
func TestSubjectAndCategoryFiltersAreConjunctive(t *testing.T) {
	s := NewStore()
	subjectFilterFixture(t, s)

	// Decision category + subject: the subject's decisions only.
	both, _ := subjectPageWalk(t, s, "acme", 1, 3, AuditDecision, "u1")
	if got := recordSeqs(both); !reflect.DeepEqual(got, []int{2, 4, 8}) {
		t.Fatalf("decision+u1 seqs = %v, want [2 4 8]", got)
	}
	// Policy-change category + subject: the subject made no policy change,
	// so the result is empty — the subject's decisions must NOT be returned
	// as a fallback, and the changes must NOT leak in either.
	none, _ := subjectPageWalk(t, s, "acme", 1, 2, AuditPolicyChange, "u1")
	if len(none) != 0 {
		t.Fatalf("policy_change+u1 = %+v, want empty (filters are conjunctive)", none)
	}
	// A subject with no decisions at all under the decision category.
	ghost, _ := subjectPageWalk(t, s, "acme", 1, 2, AuditDecision, "ghost")
	if len(ghost) != 0 {
		t.Fatalf("decision+ghost = %+v, want empty", ghost)
	}
}

// TestSubjectPagingCountsMatchesNotScannedRecords pins that the page size
// bounds returned matches, not scanned chain positions: non-matching
// records between matches never cut a page short, the concatenated walk is
// the ascending duplicate-free match list, and a page filled exactly while
// non-matching records remain is followed by one empty page that ends the
// walk.
func TestSubjectPagingCountsMatchesNotScannedRecords(t *testing.T) {
	s := NewStore()
	//	seq 1  publish v1 (allow u1)
	//	seq 2  u1        seq 3-4  u2        seq 5  publish v2
	//	seq 6  u3        seq 7-8  u1        seq 9  u2
	//	seq 10 u1        seq 11   u2        seq 12 publish v3
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	decide := func(subject string) { s.Decide("acme", request("acme", subject, "r1", "org/a", "read")) }
	decide("u1")              // 2
	decide("u2")              // 3
	decide("u2")              // 4
	s.Publish("acme", 1, nil) // 5
	decide("u3")              // 6
	decide("u1")              // 7
	decide("u1")              // 8
	decide("u2")              // 9
	decide("u1")              // 10
	decide("u2")              // 11
	s.Publish("acme", 2, nil) // 12
	wantSeqs := []int{2, 7, 8, 10}

	// Page size 2: matches sit up to 5 chain positions apart, far beyond
	// the page size; every page must still fill with matches.
	page, err := s.AuditQuery("acme", 1, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(page.Records); !reflect.DeepEqual(got, []int{2, 7}) {
		t.Fatalf("page 1 seqs = %v, want [2 7]; non-matches truncated the page", got)
	}
	if page.Next != 8 {
		t.Fatalf("page 1 next = %d, want 8 (first unscanned position)", page.Next)
	}
	page2, err := s.AuditPage(page.Checkpoint, page.Next, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(page2.Records); !reflect.DeepEqual(got, []int{8, 10}) {
		t.Fatalf("page 2 seqs = %v, want [8 10]", got)
	}
	// The page filled exactly while the non-matching seq 11-12 remain: the
	// walk continues with one empty page...
	if page2.Next != 11 {
		t.Fatalf("page 2 next = %d, want 11", page2.Next)
	}
	page3, err := s.AuditPage(page2.Checkpoint, page2.Next, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	// ...which terminates the walk instead of repeating the position.
	if len(page3.Records) != 0 || page3.Next != 0 {
		t.Fatalf("trailing page = %+v, want empty with next 0", page3)
	}

	// Other page sizes agree: the concatenation is always the ascending
	// match list with no duplicates or gaps.
	for _, size := range []int{1, 3, 4, 100} {
		recs, _ := subjectPageWalk(t, s, "acme", 1, size, "", "u1")
		if got := recordSeqs(recs); !reflect.DeepEqual(got, wantSeqs) {
			t.Fatalf("page size %d seqs = %v, want %v", size, got, wantSeqs)
		}
		requireSubjectDecisions(t, "acme", "u1", recs)
	}

	// A legal start sequence excludes earlier matches instead of
	// backfilling them, wherever the start lands.
	fromEight, _ := subjectPageWalk(t, s, "acme", 8, 2, "", "u1")
	if got := recordSeqs(fromEight); !reflect.DeepEqual(got, []int{8, 10}) {
		t.Fatalf("from seq 8 = %v, want [8 10]", got)
	}
	fromNine, _ := subjectPageWalk(t, s, "acme", 9, 2, "", "u1")
	if got := recordSeqs(fromNine); !reflect.DeepEqual(got, []int{10}) {
		t.Fatalf("from seq 9 = %v, want [10]", got)
	}
	// A start beyond the last match scans the tail and ends empty.
	fromEleven, _ := subjectPageWalk(t, s, "acme", 11, 2, "", "u1")
	if len(fromEleven) != 0 {
		t.Fatalf("from seq 11 = %+v, want empty", fromEleven)
	}
}

// TestSubjectQueryCheckpointPinsAgainstLaterAppends is the core
// read-while-appending guarantee: once the first page pinned an end
// sequence and fingerprint, later appends — the same subject's new allows
// AND denials, other subjects' decisions, new publishes — never mix into
// the pinned walk, every page carries the identical checkpoint, and only a
// freshly started query observes the appended tail.
func TestSubjectQueryCheckpointPinsAgainstLaterAppends(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))  // seq 2, u1 allow
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))  // seq 3
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "write")) // seq 4, u1 deny
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))  // seq 5

	first, err := s.AuditQuery("acme", 1, 1, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(first.Records); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("first page seqs = %v, want [2]", got)
	}
	cp := first.Checkpoint
	if cp.EndSeq != 5 {
		t.Fatalf("pinned end = %d, want 5", cp.EndSeq)
	}

	// The chain grows after the pin: the same subject produces a new allow
	// and a new denial, another subject decides, and a new policy version
	// is published.
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 6, u1 allow
	if _, err := s.Publish("acme", 1, nil); err != nil {           // seq 7
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 8, u1 deny (v2 empty)
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read")) // seq 9

	// Paging along the original checkpoint still sees only the pinned
	// range's matches, and every page repeats the first checkpoint exactly.
	second, err := s.AuditPage(cp, first.Next, 1, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(second.Records); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("second page seqs = %v, want [4]; appended u1 records leaked", got)
	}
	if second.Checkpoint != cp {
		t.Fatalf("second page checkpoint = %+v, want pinned %+v", second.Checkpoint, cp)
	}
	if second.EndSeq != 5 {
		t.Fatalf("second page end = %d, want pinned 5", second.EndSeq)
	}
	third, err := s.AuditPage(cp, second.Next, 1, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Records) != 0 || third.Next != 0 || third.Checkpoint != cp {
		t.Fatalf("final pinned page = %+v, want empty, next 0, pinned checkpoint", third)
	}

	// A fresh query pins the new head and sees the appended u1 records —
	// both the allow and the denials.
	fresh, _ := subjectPageWalk(t, s, "acme", 1, 2, "", "u1")
	if got := recordSeqs(fresh); !reflect.DeepEqual(got, []int{2, 4, 6, 8}) {
		t.Fatalf("fresh query seqs = %v, want [2 4 6 8]", got)
	}
	if !fresh[2].Decision.Decision.Allowed || fresh[3].Decision.Decision.Allowed {
		t.Fatalf("appended allow/deny outcomes wrong: %+v", fresh[2:])
	}

	// All the reading was read-only.
	if got := len(drainAudit(t, s, "acme", "", "")); got != 9 {
		t.Fatalf("records = %d, want 9; queries must not append", got)
	}
	if v := s.CurrentVersion("acme"); v != 2 {
		t.Fatalf("version = %d, want 2; queries must not republish", v)
	}
}

// TestSubjectPagingWhileAppendsRunConcurrently races a subject-filtered
// walk against concurrent appends of the same subject's decisions, other
// subjects' decisions and publishes. The walk must return exactly the
// matches inside the range pinned by its first page — no appended record
// mixes in, no match is dropped, no cursor repeats.
func TestSubjectPagingWhileAppendsRunConcurrently(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		allowPolicy("p-u1", "u1", "read", "org/a", false),
		allowPolicy("p-u2", "u2", "read", "org/a", false),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}
	//	seq 1 publish, then u1 at every even and u2 at every odd sequence.
	const pinnedPairs = 20
	for i := 0; i < pinnedPairs; i++ {
		s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
		s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))
	}
	wantSeqs := make([]int, 0, pinnedPairs)
	for i := 0; i < pinnedPairs; i++ {
		wantSeqs = append(wantSeqs, 2*i+2)
	}

	first, err := s.AuditQuery("acme", 1, 2, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	cp := first.Checkpoint
	pinnedEnd := cp.EndSeq

	// Writers append a fixed burst of the same subject's decisions, other
	// subjects' decisions and publishes while the walk runs.
	var wg sync.WaitGroup
	for _, subject := range []string{"u1", "u2"} {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.Decide("acme", request("acme", subject, "r1", "org/a", "read"))
			}
		}(subject)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			// Conflicts are expected and irrelevant; successful publishes
			// append policy-change records to the racing chain.
			s.Publish("acme", s.CurrentVersion("acme"), policies)
		}
	}()

	// Page the pinned query to exhaustion while the writers run.
	recs := append([]AuditRecord(nil), first.Records...)
	seen := map[int]bool{}
	page := first
	for page.Next != 0 {
		if seen[page.Next] {
			t.Fatalf("cursor %d repeated under concurrency", page.Next)
		}
		seen[page.Next] = true
		page, err = s.AuditPage(cp, page.Next, 2, "", "u1")
		if err != nil {
			t.Fatalf("AuditPage under concurrency: %v", err)
		}
		if page.Checkpoint != cp {
			t.Fatalf("checkpoint drifted under concurrency: %+v vs %+v", page.Checkpoint, cp)
		}
		recs = append(recs, page.Records...)
	}
	wg.Wait()

	// Exactly the pinned range's u1 decisions, ascending, nothing appended
	// after the pin.
	if got := recordSeqs(recs); !reflect.DeepEqual(got, wantSeqs) {
		t.Fatalf("concurrent walk seqs = %v, want %v", got, wantSeqs)
	}
	requireSubjectDecisions(t, "acme", "u1", recs)
	for _, r := range recs {
		if r.Seq > pinnedEnd {
			t.Fatalf("record seq %d leaked past the pinned end %d", r.Seq, pinnedEnd)
		}
	}

	// The appended records are real and visible to a fresh query.
	fresh := drainAudit(t, s, "acme", "", "u1")
	if len(fresh) != len(recs)+50 {
		t.Fatalf("fresh query sees %d u1 records, want %d pinned + 50 appended", len(fresh), len(recs))
	}
	if got := recordSeqs(fresh[:len(recs)]); !reflect.DeepEqual(got, wantSeqs) {
		t.Fatalf("fresh query prefix = %v, want the pinned %v", got, wantSeqs)
	}
}

// TestSubjectQueryWithoutMatchesReturnsEmpty pins the no-match behaviour:
// an unknown subject — in a chain full of other records, or in an
// organization with no records at all — yields an empty page that ends the
// walk immediately. It is not an error and never falls back to returning
// the unfiltered chain.
func TestSubjectQueryWithoutMatchesReturnsEmpty(t *testing.T) {
	s := NewStore()
	subjectFilterFixture(t, s) // 8 records, none of them subject "ghost"

	page, err := s.AuditQuery("acme", 1, 4, "", "ghost")
	if err != nil {
		t.Fatalf("no-match query err = %v, want nil", err)
	}
	if len(page.Records) != 0 || page.Next != 0 {
		t.Fatalf("no-match page = %+v, want empty with next 0", page)
	}
	if page.Checkpoint.EndSeq != 8 {
		t.Fatalf("no-match checkpoint end = %d, want the pinned head 8", page.Checkpoint.EndSeq)
	}
	// Same under the explicit decision category, and from a later start.
	if recs, _ := subjectPageWalk(t, s, "acme", 1, 2, AuditDecision, "ghost"); len(recs) != 0 {
		t.Fatalf("no-match walk = %+v, want empty (not the unfiltered chain)", recs)
	}
	if recs, _ := subjectPageWalk(t, s, "acme", 6, 2, "", "ghost"); len(recs) != 0 {
		t.Fatalf("no-match walk from seq 6 = %+v, want empty", recs)
	}

	// An organization with no records at all answers the same way.
	empty, err := s.AuditQuery("blank", 1, 4, "", "ghost")
	if err != nil {
		t.Fatalf("empty-org query err = %v, want nil", err)
	}
	if len(empty.Records) != 0 || empty.Next != 0 || empty.Checkpoint.EndSeq != 0 {
		t.Fatalf("empty-org page = %+v, want the empty genesis page", empty)
	}
}

// TestSubjectPagingRejectsTamperedCheckpointFingerprint pins that a
// checkpoint whose fingerprint was altered fails with the existing
// ErrInvalidRange and delivers no partial records — the walk must not
// continue over a checkpoint that no longer proves the pinned range.
func TestSubjectPagingRejectsTamperedCheckpointFingerprint(t *testing.T) {
	s := NewStore()
	subjectFilterFixture(t, s)

	first, err := s.AuditQuery("acme", 1, 1, "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 1 || first.Next == 0 {
		t.Fatalf("first page = %+v, want one record and a live cursor", first)
	}

	// An altered fingerprint: the error, never partial records.
	forged := first.Checkpoint
	forged.Fingerprint = "00"
	page, err := s.AuditPage(forged, first.Next, 1, "", "u1")
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("forged fingerprint err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		t.Fatalf("forged fingerprint delivered partial page %+v", page)
	}

	// A fingerprint that is genuine but belongs to a different position in
	// the chain is equally invalid for this checkpoint.
	shifted := first.Checkpoint
	shifted.EndSeq = 4 // fingerprint still pins seq 8's chain state
	page, err = s.AuditPage(shifted, first.Next, 1, "", "u1")
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("shifted end err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		t.Fatalf("shifted checkpoint delivered partial page %+v", page)
	}

	// The same pinned range presented under another organization's name
	// cannot page either: the fingerprint binds the organization.
	foreign := first.Checkpoint
	foreign.Org = "globex"
	page, err = s.AuditPage(foreign, first.Next, 1, "", "u1")
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("foreign checkpoint err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		t.Fatalf("foreign checkpoint delivered partial page %+v", page)
	}

	// The genuine checkpoint still pages normally after the forgeries.
	second, err := s.AuditPage(first.Checkpoint, first.Next, 10, "", "u1")
	if err != nil {
		t.Fatalf("genuine checkpoint rejected after forgeries: %v", err)
	}
	if got := recordSeqs(second.Records); !reflect.DeepEqual(got, []int{4, 8}) {
		t.Fatalf("genuine continuation seqs = %v, want [4 8]", got)
	}
}
