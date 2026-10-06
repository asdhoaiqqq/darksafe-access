// Regression coverage for checkpoint verification on AuditPage calls made
// AFTER a pinned query is already exhausted. The end cursor (0, or the
// pinned end sequence plus one) only states that the fixed range has no
// further records; it never downgrades the checkpoint check. These tests
// pin that contract of the existing AuditQuery/AuditPage entry points:
//
//   - A legal checkpoint with the end cursor 0 returns an empty page whose
//     Next stays 0 and whose checkpoint — organization, end sequence and
//     fingerprint — is carried through verbatim. Records appended after
//     the query ended neither leak into that empty page nor advance the
//     returned end sequence to the current chain tail.
//   - The cursor endSeq+1 is the other legal end position: it returns the
//     same empty end page instead of reporting the cursor out of range or
//     redelivering old records from the start. One position further is
//     still ErrInvalidRange.
//   - Both end positions verify the checkpoint exactly as an ordinary
//     mid-walk page does: an altered fingerprint, or an altered end
//     sequence paired with the old position's fingerprint, fails with
//     ErrInvalidRange and delivers no page at all.
//   - A historical checkpoint — end sequence below the current tail but
//     fingerprint genuinely matching that old position — stays valid: it
//     is neither rejected as stale nor silently rewritten to the current
//     tail's fingerprint.
//   - An organization with no records follows the same rule: its legal
//     root checkpoint (end sequence 0, genesis fingerprint) with cursor 0
//     returns the empty page, while a wrong root fingerprint is rejected.
//     A zero record count alone never proves a paging call succeeded.
//
// All of this is read-only: none of these calls appends an audit record or
// changes the current policy version, and ordinary mid-walk paging keeps
// its filtering and record order.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// endCheckpointFixture builds one organization whose chain mixes policy
// changes with access decisions:
//
//	seq 1  publish v1 (allow u1 read)
//	seq 2  u1 read -> allow (v1)
//	seq 3  u2 read -> no-match denial (v1)
//	seq 4  publish v2 (empty set)
func endCheckpointFixture(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
}

// requireEmptyEndPage fails unless the page is the empty end-of-range page
// carrying exactly the pinned checkpoint: no records, Next 0, and the
// checkpoint's organization, end sequence and fingerprint unchanged.
func requireEmptyEndPage(t *testing.T, page *AuditPage, cp Checkpoint) {
	t.Helper()
	if page == nil {
		t.Fatal("end page = nil, want the empty end page")
	}
	if len(page.Records) != 0 {
		t.Fatalf("end page records = %+v, want none", page.Records)
	}
	if page.Next != 0 {
		t.Fatalf("end page next = %d, want 0 (the walk stays finished)", page.Next)
	}
	if page.Checkpoint != cp {
		t.Fatalf("end page checkpoint = %+v, want the pinned %+v carried through verbatim",
			page.Checkpoint, cp)
	}
	if page.Org != cp.Org || page.EndSeq != cp.EndSeq {
		t.Fatalf("end page bounds = (%q, %d), want (%q, %d)",
			page.Org, page.EndSeq, cp.Org, cp.EndSeq)
	}
}

// requireInvalidRange fails unless the call returned ErrInvalidRange and
// delivered no page at all.
func requireInvalidRange(t *testing.T, page *AuditPage, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("%s err = %v, want ErrInvalidRange", what, err)
	}
	if page != nil {
		t.Fatalf("%s delivered page %+v, want none", what, page)
	}
}

// TestAuditPageAfterEndKeepsCheckpointAndRange pins the happy path of
// paging past the end: with the legal checkpoint both end cursors return
// the empty end page carrying the pinned checkpoint verbatim, records
// appended after the query ended neither mix in nor advance the returned
// end sequence, and the cursor one past the legal end position is still
// the existing out-of-range error.
func TestAuditPageAfterEndKeepsCheckpointAndRange(t *testing.T) {
	s := NewStore()
	endCheckpointFixture(t, s)

	// Page the whole chain of 4 records with page size 2, ending the walk.
	first, err := s.AuditQuery("acme", 1, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(first.Records); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("first page seqs = %v, want [1 2]", got)
	}
	cp := first.Checkpoint
	if cp.Org != "acme" || cp.EndSeq != 4 || cp.Fingerprint == "" {
		t.Fatalf("pinned checkpoint = %+v, want acme ending at 4 with a fingerprint", cp)
	}
	last, err := s.AuditPage(cp, first.Next, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(last.Records); !reflect.DeepEqual(got, []int{3, 4}) || last.Next != 0 {
		t.Fatalf("last page = %+v, want seqs [3 4] and next 0", last)
	}

	// The walk is over; calling again with the end cursor 0 and the legal
	// checkpoint returns the empty end page, checkpoint verbatim.
	end, err := s.AuditPage(cp, 0, 2, "", "")
	if err != nil {
		t.Fatalf("end cursor 0 with legal checkpoint err = %v, want nil", err)
	}
	requireEmptyEndPage(t, end, cp)

	// The organization keeps producing decisions after the query ended.
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 5
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read")) // seq 6

	// The ended query still returns its empty page: the new records do not
	// mix in, and the returned end sequence is not advanced to the new tail.
	end, err = s.AuditPage(cp, 0, 2, "", "")
	if err != nil {
		t.Fatalf("end cursor 0 after appends err = %v, want nil", err)
	}
	requireEmptyEndPage(t, end, cp)

	// The cursor endSeq+1 is the other legal end position: the same empty
	// end page, not an out-of-range error and not a redelivery from the
	// start of the range.
	end, err = s.AuditPage(cp, cp.EndSeq+1, 2, "", "")
	if err != nil {
		t.Fatalf("end cursor %d err = %v, want nil", cp.EndSeq+1, err)
	}
	requireEmptyEndPage(t, end, cp)

	// One position further remains the existing out-of-range error.
	page, err := s.AuditPage(cp, cp.EndSeq+2, 2, "", "")
	requireInvalidRange(t, page, err, "cursor beyond endSeq+1")

	// A fresh query observes the appended tail; the ended query's view was
	// read-only throughout.
	if got := recordSeqs(drainAudit(t, s, "acme", "", "")); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("fresh query seqs = %v, want [1 2 3 4 5 6]", got)
	}
	if v := s.CurrentVersion("acme"); v != 2 {
		t.Fatalf("version = %d, want 2; paging past the end must not republish", v)
	}
}

// TestAuditPageAfterEndRejectsTamperedCheckpoint pins that the end cursor
// never waives the checkpoint check: an altered fingerprint, or an altered
// end sequence still paired with the old position's fingerprint, fails
// with ErrInvalidRange at BOTH end positions — the same error category an
// ordinary mid-walk page would get — and delivers no page.
func TestAuditPageAfterEndRejectsTamperedCheckpoint(t *testing.T) {
	s := NewStore()
	endCheckpointFixture(t, s)

	first, err := s.AuditQuery("acme", 1, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cp := first.Checkpoint // end 4, fingerprint of record 4

	// Reference: a mid-walk page with a tampered checkpoint fails with
	// ErrInvalidRange; the end positions must answer with the same
	// category, never with a successful empty page.
	forgedMid := cp
	forgedMid.Fingerprint = "00"
	page, err := s.AuditPage(forgedMid, first.Next, 2, "", "")
	requireInvalidRange(t, page, err, "mid-walk forged fingerprint")

	// Altered fingerprint at the end cursor 0.
	forged := cp
	forged.Fingerprint = "00"
	page, err = s.AuditPage(forged, 0, 2, "", "")
	requireInvalidRange(t, page, err, "end cursor 0 forged fingerprint")

	// Altered fingerprint at the end cursor endSeq+1.
	page, err = s.AuditPage(forged, cp.EndSeq+1, 2, "", "")
	requireInvalidRange(t, page, err, "end cursor endSeq+1 forged fingerprint")

	// Altered end sequence still paired with the old position's
	// fingerprint: the material does not match at either end position.
	shifted := cp
	shifted.EndSeq = 3 // fingerprint still pins record 4's chain state
	page, err = s.AuditPage(shifted, 0, 2, "", "")
	requireInvalidRange(t, page, err, "end cursor 0 shifted end sequence")
	page, err = s.AuditPage(shifted, shifted.EndSeq+1, 2, "", "")
	requireInvalidRange(t, page, err, "end cursor endSeq+1 shifted end sequence")

	// The genuine checkpoint still returns its empty end page after the
	// forgeries, and nothing was appended or republished by the reads.
	end, err := s.AuditPage(cp, 0, 2, "", "")
	if err != nil {
		t.Fatalf("genuine checkpoint rejected after forgeries: %v", err)
	}
	requireEmptyEndPage(t, end, cp)
	if got := len(drainAudit(t, s, "acme", "", "")); got != 4 {
		t.Fatalf("records = %d, want 4; rejected pages must not append", got)
	}
	if v := s.CurrentVersion("acme"); v != 2 {
		t.Fatalf("version = %d, want 2; rejected pages must not republish", v)
	}
}

// TestAuditPageAfterEndAcceptsHistoricalCheckpoint pins the distinction
// between a corrupted checkpoint and a merely historical one: a checkpoint
// whose end sequence sits below the current tail but whose fingerprint
// genuinely matches that old position stays valid at the end positions —
// it is neither rejected as stale nor rewritten to the current tail's
// fingerprint.
func TestAuditPageAfterEndAcceptsHistoricalCheckpoint(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 2

	// Pin a query while the chain holds exactly 2 records.
	first, err := s.AuditQuery("acme", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	historical := first.Checkpoint
	if historical.EndSeq != 2 {
		t.Fatalf("historical checkpoint end = %d, want 2", historical.EndSeq)
	}

	// The chain grows well past the pinned position.
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 3
	if _, err := s.Publish("acme", 1, nil); err != nil {           // seq 4
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 5

	// The historical checkpoint still pages its ended range at cursor 0:
	// an empty page carrying the OLD end sequence and the OLD fingerprint,
	// not the current tail's.
	end, err := s.AuditPage(historical, 0, 10, "", "")
	if err != nil {
		t.Fatalf("historical checkpoint at end cursor 0 err = %v, want nil", err)
	}
	requireEmptyEndPage(t, end, historical)

	// Same at its own endSeq+1 cursor.
	end, err = s.AuditPage(historical, historical.EndSeq+1, 10, "", "")
	if err != nil {
		t.Fatalf("historical checkpoint at end cursor %d err = %v, want nil",
			historical.EndSeq+1, err)
	}
	requireEmptyEndPage(t, end, historical)

	// The returned fingerprint is the historical position's, provably not
	// the current tail's.
	fresh, err := s.AuditQuery("acme", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Checkpoint.EndSeq != 5 || fresh.Checkpoint.Fingerprint == historical.Fingerprint {
		t.Fatalf("fresh checkpoint = %+v, want end 5 with a fingerprint distinct from the historical one",
			fresh.Checkpoint)
	}
	if end.Checkpoint.Fingerprint != historical.Fingerprint {
		t.Fatalf("historical end page fingerprint rewritten to %q, want the pinned %q",
			end.Checkpoint.Fingerprint, historical.Fingerprint)
	}
}

// TestAuditPageEmptyOrgRootCheckpoint pins that an organization with no
// records follows the same checkpoint rule: the legal root checkpoint
// (end sequence 0, genesis fingerprint) with the end cursor 0 returns the
// empty page, while a wrong root fingerprint is rejected — no records
// existing never waives the fingerprint check, and a zero record count
// alone never proves the call succeeded.
func TestAuditPageEmptyOrgRootCheckpoint(t *testing.T) {
	s := NewStore()

	// Pin the empty query; the organization has no records at all.
	query, err := s.AuditQuery("blank", 1, 4, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(query.Records) != 0 || query.Next != 0 {
		t.Fatalf("empty-org query page = %+v, want empty with next 0", query)
	}
	root := query.Checkpoint
	if root.EndSeq != 0 || root.Fingerprint == "" {
		t.Fatalf("root checkpoint = %+v, want end 0 with the genesis fingerprint", root)
	}

	// The legal root checkpoint with the end cursor 0 returns the empty
	// page, checkpoint verbatim.
	end, err := s.AuditPage(root, 0, 4, "", "")
	if err != nil {
		t.Fatalf("root checkpoint at end cursor 0 err = %v, want nil", err)
	}
	requireEmptyEndPage(t, end, root)

	// A wrong root fingerprint is rejected even though the organization
	// holds no records: the empty result is not a free pass.
	forged := root
	forged.Fingerprint = "00"
	page, err := s.AuditPage(forged, 0, 4, "", "")
	requireInvalidRange(t, page, err, "empty-org forged root fingerprint")

	// An organization the store has never seen answers the same way: the
	// genuine genesis checkpoint pages, the forged one does not.
	virgin := Checkpoint{Org: "ghost", EndSeq: 0, Fingerprint: genesisFingerprint("ghost")}
	end, err = s.AuditPage(virgin, 0, 4, "", "")
	if err != nil {
		t.Fatalf("unseen-org root checkpoint err = %v, want nil", err)
	}
	requireEmptyEndPage(t, end, virgin)
	page, err = s.AuditPage(Checkpoint{Org: "ghost", EndSeq: 0, Fingerprint: "ff"}, 0, 4, "", "")
	requireInvalidRange(t, page, err, "unseen-org forged root fingerprint")

	// Still no records anywhere; the reads appended nothing.
	if got := len(drainAudit(t, s, "blank", "", "")); got != 0 {
		t.Fatalf("blank records = %d, want 0; empty-org paging must not append", got)
	}
	if got := len(drainAudit(t, s, "ghost", "", "")); got != 0 {
		t.Fatalf("ghost records = %d, want 0; empty-org paging must not append", got)
	}
}
