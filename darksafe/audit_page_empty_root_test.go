// Regression coverage for AuditPage against a root checkpoint: the
// sequence-0 checkpoint whose fingerprint is the organization's genesis
// fingerprint, handed out by the first AuditQuery or AuditExport of an
// organization that has not yet produced any audit records.
//
// That checkpoint pins a finished empty range, and the answer must not
// depend on the store's incidental state:
//
//   - Whether the store has never visited the organization, has only run an
//     empty query/export against it, or has since appended records, cursor 0
//     (exhausted) and cursor 1 (the one-past-end end page) both succeed with
//     an empty page: Next 0, BeginSeq 1, EndSeq 0, and the page's org and
//     checkpoint returned exactly as submitted. Cursor 1 is the end page,
//     not an out-of-range error and not a restart.
//   - The empty range still rests on real checkpoint material: a foreign
//     organization's root fingerprint or any other wrong value fails with
//     ErrInvalidRange and delivers no page, at both cursors. A cursor above 1
//     claims records the root checkpoint says do not exist and fails the same
//     way, as does an EndSeq > 0 checkpoint against a recordless chain.
//   - Once pinned at sequence 0, records the organization produces later
//     never enter the old range and the checkpoint is never re-stamped to
//     the current tail; only a fresh query sees the new records.
//   - The empty outcome skips none of the existing argument validation:
//     missing organization, illegal page size or cursor, unknown category
//     and duplicate resource conditions still fail before any page exists.
//
// Every call here is read-only: it appends no audit record and moves no
// policy version.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// rootCheckpoint builds the sequence-0 checkpoint a first AuditQuery or
// AuditExport returns for an empty organization.
func rootCheckpoint(org string) Checkpoint {
	return Checkpoint{Org: org, EndSeq: 0, Fingerprint: genesisFingerprint(org)}
}

// requireEmptyRootPage fails unless the page is the uniform empty end page
// for a root checkpoint: nil error, no records, Next 0, BeginSeq 1,
// EndSeq 0, org and checkpoint byte-for-byte as submitted.
func requireEmptyRootPage(t *testing.T, page *AuditPage, err error, cp Checkpoint) {
	t.Helper()
	if err != nil {
		t.Fatalf("root checkpoint paging err = %v, want nil", err)
	}
	if page == nil {
		t.Fatal("root checkpoint paging delivered nil page with nil error")
	}
	if len(page.Records) != 0 {
		t.Fatalf("root page delivered %d records %v, want none", len(page.Records), recordSeqs(page.Records))
	}
	if page.Next != 0 {
		t.Fatalf("root page next = %d, want 0", page.Next)
	}
	if page.BeginSeq != 1 {
		t.Fatalf("root page begin = %d, want 1", page.BeginSeq)
	}
	if page.EndSeq != 0 {
		t.Fatalf("root page end = %d, want 0", page.EndSeq)
	}
	if page.Org != cp.Org {
		t.Fatalf("root page org = %q, want %q", page.Org, cp.Org)
	}
	if page.Checkpoint != cp {
		t.Fatalf("root page checkpoint = %+v, want submitted %+v", page.Checkpoint, cp)
	}
}

// TestAuditPageEmptyRootIsStateIndependent pins that the same root
// checkpoint produces the same empty end page whether the store has never
// seen the organization, has only queried/exported it empty, or has seen it
// via an empty query after an earlier rejection — read-only history never
// changes whether an identical paging request succeeds or what range it
// describes.
func TestAuditPageEmptyRootIsStateIndependent(t *testing.T) {
	cp := rootCheckpoint("ghost")
	cursors := []int{0, 1}

	// State 1: store that has never even visited the organization.
	fresh := NewStore()
	for _, cursor := range cursors {
		page, err := fresh.AuditPage(cp, cursor, 10, "", "")
		requireEmptyRootPage(t, page, err, cp)
	}

	// State 2: store where an empty query first materialized the org.
	queried := NewStore()
	first, err := queried.AuditQuery("ghost", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Checkpoint != cp {
		t.Fatalf("empty query checkpoint = %+v, want %+v", first.Checkpoint, cp)
	}
	for _, cursor := range cursors {
		page, err := queried.AuditPage(cp, cursor, 10, "", "")
		requireEmptyRootPage(t, page, err, cp)
	}

	// State 3: store that reached the root checkpoint via an empty export.
	exported := NewStore()
	recs, exportCP, err := exported.AuditExport("ghost", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 || exportCP != cp {
		t.Fatalf("empty export = (%v, %+v), want no records and %+v", recs, exportCP, cp)
	}
	for _, cursor := range cursors {
		page, err := exported.AuditPage(exportCP, cursor, 10, "", "")
		requireEmptyRootPage(t, page, err, exportCP)
	}
}

// TestAuditPageEmptyRootRejectsUnreliableMaterial pins that the empty range
// gets no fingerprint exemption: any fingerprint other than this
// organization's sequence-0 root — including another organization's genuine
// root fingerprint — fails with ErrInvalidRange and delivers no page at
// either terminal cursor, and neither do out-of-range cursors or a
// checkpoint claiming positive records.
func TestAuditPageEmptyRootRejectsUnreliableMaterial(t *testing.T) {
	s := NewStore()
	cp := rootCheckpoint("ghost")

	// Wrong fingerprint value, same end sequence and org.
	wrongValue := cp
	wrongValue.Fingerprint = "00"
	// Another organization's genuine root fingerprint must not validate
	// under this organization's name: the fingerprint binds the organization.
	foreignRoot := cp
	foreignRoot.Fingerprint = genesisFingerprint("someone-else")

	for _, bad := range []Checkpoint{wrongValue, foreignRoot} {
		for _, cursor := range []int{0, 1} {
			page, err := s.AuditPage(bad, cursor, 10, "", "")
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("bad root %+v at cursor %d: err = %v, want ErrInvalidRange", bad, cursor, err)
			}
			if page != nil {
				t.Fatalf("bad root %+v at cursor %d delivered page %+v", bad, cursor, page)
			}
		}
	}

	// Sanity on the distinction above: that other organization's own
	// consistent root checkpoint is a valid empty range for it — what makes
	// foreignRoot invalid is the mismatch between name and fingerprint, not
	// the fingerprint value in isolation.
	other := rootCheckpoint("someone-else")
	for _, cursor := range []int{0, 1} {
		page, err := s.AuditPage(other, cursor, 10, "", "")
		requireEmptyRootPage(t, page, err, other)
	}

	// Cursor past the one-past-end position claims positive records that do
	// not exist in the pinned range.
	for _, cursor := range []int{2, 3, 100} {
		page, err := s.AuditPage(cp, cursor, 10, "", "")
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("cursor %d on root checkpoint: err = %v, want ErrInvalidRange", cursor, err)
		}
		if page != nil {
			t.Fatalf("cursor %d delivered page %+v", cursor, page)
		}
	}

	// A checkpoint claiming positive records cannot validate against an org
	// that has none, even though its EndSeq would be a valid cursor shape.
	claimsRecords := cp
	claimsRecords.EndSeq = 1
	claimsRecords.Fingerprint = "00"
	for _, cursor := range []int{0, 1, 2} {
		page, err := s.AuditPage(claimsRecords, cursor, 10, "", "")
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("positive-end checkpoint at cursor %d: err = %v, want ErrInvalidRange", cursor, err)
		}
		if page != nil {
			t.Fatalf("positive-end checkpoint at cursor %d delivered page %+v", cursor, page)
		}
	}

	// The genuine root checkpoint is unaffected by the rejected attempts,
	// and nothing about the rejected calls materialized the organization.
	page, err := s.AuditPage(cp, 1, 10, "", "")
	requireEmptyRootPage(t, page, err, cp)
	if _, ok := s.orgs["ghost"]; ok {
		t.Fatal("rejected/empty paging materialized the organization state")
	}
}

// TestAuditPageEmptyRootStaysClosedAfterRecordsAppend pins that a saved root
// checkpoint keeps describing its finished empty range after the
// organization grows: continued paging never mixes the new records in and
// never re-stamps the checkpoint to the current tail; only a fresh query
// observes the additions.
func TestAuditPageEmptyRootStaysClosedAfterRecordsAppend(t *testing.T) {
	s := NewStore()

	// Obtain the root checkpoint through each of the two documented
	// read-only entry points.
	emptyQuery, err := s.AuditQuery("acme", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	queryCP := emptyQuery.Checkpoint
	_, exportCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if queryCP != rootCheckpoint("acme") || exportCP != queryCP {
		t.Fatalf("root checkpoints = %+v / %+v, want equal genesis roots", queryCP, exportCP)
	}

	// The organization later produces real audit records.
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
	}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 2
	if recs := drainAudit(t, s, "acme", "", ""); len(recs) != 2 {
		t.Fatalf("chain = %v, want 2 appended records", recordSeqs(recs))
	}

	// Both root checkpoints still answer the finished empty range from both
	// terminal cursors, even with a filter that the appended decision would
	// match in a fresh query.
	for _, cp := range []Checkpoint{queryCP, exportCP} {
		for _, cursor := range []int{0, 1} {
			page, err := s.AuditPage(cp, cursor, 10, AuditDecision, "u1")
			requireEmptyRootPage(t, page, err, cp)
		}
	}

	// A fresh query pins the new tail and sees the appended records; the old
	// checkpoints are untouched by it.
	fresh, err := s.AuditQuery("acme", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(fresh.Records); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("fresh query records = %v, want [1 2]", got)
	}
	if fresh.Checkpoint.EndSeq != 2 || fresh.Checkpoint.Fingerprint == queryCP.Fingerprint {
		t.Fatalf("fresh checkpoint = %+v, want end 2 with a non-genesis fingerprint", fresh.Checkpoint)
	}
	page, err := s.AuditPage(queryCP, 1, 10, "", "")
	requireEmptyRootPage(t, page, err, queryCP)

	// All paging was read-only.
	if got := recordSeqs(drainAudit(t, s, "acme", "", "")); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("chain after root paging = %v, want [1 2]", got)
	}
	if v := s.CurrentVersion("acme"); v != 1 {
		t.Fatalf("version = %d, want 1; root paging must not publish", v)
	}
}

// TestAuditPageEmptyRootKeepsArgumentValidation pins that the empty outcome
// skips none of the existing validation: missing organization, non-positive
// page size, negative cursor, unknown category and more than one resource
// condition are all rejected without delivering a page.
func TestAuditPageEmptyRootKeepsArgumentValidation(t *testing.T) {
	cp := rootCheckpoint("ghost")

	if page, err := NewStore().AuditPage(Checkpoint{}, 0, 10, "", ""); !errors.Is(err, ErrMissingOrganization) || page != nil {
		t.Fatalf("missing org = (%+v, %v), want nil page, ErrMissingOrganization", page, err)
	}
	if page, err := NewStore().AuditPage(cp, 0, 0, "", ""); !errors.Is(err, ErrInvalidPage) || page != nil {
		t.Fatalf("zero page size = (%+v, %v), want nil page, ErrInvalidPage", page, err)
	}
	if page, err := NewStore().AuditPage(cp, -1, 10, "", ""); !errors.Is(err, ErrInvalidPage) || page != nil {
		t.Fatalf("negative cursor = (%+v, %v), want nil page, ErrInvalidPage", page, err)
	}
	if page, err := NewStore().AuditPage(cp, 0, 10, "bogus", ""); !errors.Is(err, ErrInvalidPage) || page != nil {
		t.Fatalf("unknown category = (%+v, %v), want nil page, ErrInvalidPage", page, err)
	}
	if page, err := NewStore().AuditPage(cp, 0, 10, "", "", "r1", "r2"); !errors.Is(err, ErrInvalidPage) || page != nil {
		t.Fatalf("two resource conditions = (%+v, %v), want nil page, ErrInvalidPage", page, err)
	}
}
