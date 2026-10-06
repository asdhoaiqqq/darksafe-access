// Regression coverage for audit pagination once a pinned range is already
// exhausted. An auditor obtains the organization's end sequence and
// fingerprint from AuditQuery and continues with AuditPage; the zero/terminal
// cursor only says "no further records exist inside this fixed range", so it
// must never make a bad checkpoint pass as a successful empty query. These
// tests pin the contract of the existing AuditQuery/AuditPage entry points
// for calls made after the walk has ended:
//
//   - With the genuine checkpoint of the original query, both terminal
//     positions — the end cursor 0 and the one-past-the-pinned-end cursor
//     EndSeq+1 — succeed with an empty page, Next stays 0, and the
//     checkpoint's organization, end sequence and fingerprint come back
//     byte-for-byte. Records the organization produces afterwards never
//     enter the finished query, from either cursor, and the returned end
//     sequence is not advanced to the current chain tail.
//   - The EndSeq+1 cursor is an empty end page, not an out-of-range error
//     and not a restart that redelivers records from sequence 1.
//   - Even at a terminal cursor the checkpoint material is still verified:
//     a different fingerprint, or a changed end sequence paired with the
//     old position's fingerprint, fails with ErrInvalidRange exactly as it
//     does mid-paging and delivers no page.
//   - A genuine checkpoint below the current chain tail stays valid: a
//     historical end sequence with the fingerprint that really covered that
//     position pages its prefix and is never blanket-rejected or silently
//     re-stamped with the current tail fingerprint.
//   - An organization with no records is not exempt: the root checkpoint
//     plus cursor 0 yields an empty page only while its genesis fingerprint
//     matches; a wrong root fingerprint is rejected. A legitimate empty
//     page (page with nil error) is therefore distinct from a checkpoint
//     error (nil page with ErrInvalidRange) — zero records alone never
//     marks the call successful.
//
// Every call here is read-only: no audit record is appended, no policy
// version changes, and ordinary continued paging keeps its filters and
// ascending record order.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// exhaustedRangeFixture builds an organization interleaving a policy change
// and access decisions:
//
//	seq 1  publish v1 (allow u1 read org/a)
//	seq 2  u1 read -> allow (v1)
//	seq 3  u2 read -> no-match denial
//
// It pages an unfiltered query (page size 2) to genuine exhaustion and
// returns the store together with the final empty page, whose checkpoint
// pins end sequence 3.
func exhaustedRangeFixture(t *testing.T) (*Store, *AuditPage) {
	t.Helper()
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
		{ID: "p-deny", Subject: "u2", Action: "write", Scope: "org/a", Effect: EffectDeny},
	}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 2, allow
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read")) // seq 3, no-match denial

	first, err := s.AuditQuery("acme", 1, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(first.Records); !reflect.DeepEqual(got, []int{1, 2}) || first.Next != 3 {
		t.Fatalf("first page = %+v, want seqs [1 2], next 3", first)
	}
	last, err := s.AuditPage(first.Checkpoint, first.Next, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(last.Records); !reflect.DeepEqual(got, []int{3}) || last.Next != 0 {
		t.Fatalf("final page = %+v, want seq [3], next 0", last)
	}
	return s, last
}

// requireEmptyTerminalPage fails unless the page is a successful empty end
// page (nil error, no records, Next 0) carrying the submitted checkpoint
// unchanged. A zero-length record list paired with an error would not
// satisfy this: the caller must distinguish a valid empty page from a
// rejected checkpoint.
func requireEmptyTerminalPage(t *testing.T, page *AuditPage, err error, cp Checkpoint, beginSeq int) {
	t.Helper()
	if err != nil {
		t.Fatalf("terminal page err = %v, want nil", err)
	}
	if page == nil {
		t.Fatal("terminal page is nil with nil error")
	}
	if len(page.Records) != 0 {
		t.Fatalf("terminal page delivered %d records %v, want none", len(page.Records), recordSeqs(page.Records))
	}
	if page.Next != 0 {
		t.Fatalf("terminal page next = %d, want 0", page.Next)
	}
	if page.BeginSeq != beginSeq {
		t.Fatalf("terminal page begin = %d, want %d", page.BeginSeq, beginSeq)
	}
	if page.EndSeq != cp.EndSeq {
		t.Fatalf("terminal page end = %d, want pinned %d", page.EndSeq, cp.EndSeq)
	}
	if page.Org != cp.Org {
		t.Fatalf("terminal page org = %q, want %q", page.Org, cp.Org)
	}
	if page.Checkpoint != cp {
		t.Fatalf("terminal page checkpoint = %+v, want original %+v", page.Checkpoint, cp)
	}
}

// TestAuditPageAfterExhaustionKeepsRangeClosed pins that calling AuditPage
// after pagination ended — at cursor 0 and at EndSeq+1 — re-verifies the
// checkpoint and answers with an empty, still-closed page. New decisions
// produced afterwards do not reopen the finished query or advance its end
// sequence toward the current chain tail.
func TestAuditPageAfterExhaustionKeepsRangeClosed(t *testing.T) {
	s, last := exhaustedRangeFixture(t)
	cp := last.Checkpoint
	if cp.EndSeq != 3 || cp.Org != "acme" || cp.Fingerprint == "" {
		t.Fatalf("pinned checkpoint = %+v, want acme ending at 3 with a fingerprint", cp)
	}

	// Cursor 0 on the already finished walk: empty page, the walk stays
	// finished, and org/end/fingerprint come back exactly as pinned.
	again, err := s.AuditPage(cp, 0, 2, "", "")
	requireEmptyTerminalPage(t, again, err, cp, 1)

	// Cursor EndSeq+1 is the other legal end position: an empty end page,
	// not ErrInvalidRange, and not a restart redelivering seq 1..3.
	onePast, err := s.AuditPage(cp, cp.EndSeq+1, 2, "", "")
	requireEmptyTerminalPage(t, onePast, err, cp, cp.EndSeq+1)

	// The organization produces a new decision after the query ended.
	s.Decide("acme", request("acme", "u9", "r1", "org/a", "read")) // seq 4
	fresh, err := s.AuditQuery("acme", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Checkpoint.EndSeq != 4 || len(fresh.Records) != 4 {
		t.Fatalf("fresh query = %+v, want 4 records ending at 4", fresh)
	}

	// Neither terminal position lets the new record into the finished query
	// or advances the returned end sequence/fingerprint to the current tail.
	for _, cursor := range []int{0, cp.EndSeq + 1} {
		page, err := s.AuditPage(cp, cursor, 10, "", "")
		requireEmptyTerminalPage(t, page, err, cp, func() int {
			if cursor == 0 {
				return 1
			}
			return cursor
		}())
		if page.Checkpoint.EndSeq == fresh.Checkpoint.EndSeq ||
			page.Checkpoint.Fingerprint == fresh.Checkpoint.Fingerprint {
			t.Fatalf("finished query advanced to the current tail: %+v vs fresh %+v",
				page.Checkpoint, fresh.Checkpoint)
		}
	}

	// Normal continued paging over the FINISHED range keeps its filter and
	// ascending order: the decision-only view skips the seq-1 policy change
	// and ends at seq 3 — seq 4, a decision appended after the pin, never
	// leaks in even though it would match the filter in a fresh query.
	d1, err := s.AuditPage(cp, 1, 1, AuditDecision, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(d1.Records); !reflect.DeepEqual(got, []int{2}) || d1.Next != 3 {
		t.Fatalf("filtered continuation page 1 = %+v, want seq [2], next 3", d1)
	}
	if d1.Checkpoint != cp {
		t.Fatalf("filtered continuation drifted checkpoint: %+v vs %+v", d1.Checkpoint, cp)
	}
	d2, err := s.AuditPage(cp, d1.Next, 1, AuditDecision, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(d2.Records); !reflect.DeepEqual(got, []int{3}) || d2.Next != 0 {
		t.Fatalf("filtered continuation page 2 = %+v, want seq [3], next 0; appended seq 4 leaked", d2)
	}
	closed, err := s.AuditPage(cp, 0, 1, AuditDecision, "")
	requireEmptyTerminalPage(t, closed, err, cp, 1)

	// All terminal and continuation calls were read-only: the appended
	// decision is the only record added by this test, and the policy
	// version never moved.
	if got := recordSeqs(drainAudit(t, s, "acme", "", "")); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Fatalf("chain after terminal calls = %v, want [1 2 3 4]", got)
	}
	if v := s.CurrentVersion("acme"); v != 1 {
		t.Fatalf("version = %d, want 1; terminal paging must not republish", v)
	}
	// The genuine checkpoint still pages normally after every closed call.
	more, err := s.AuditPage(cp, 1, 10, "", "")
	if err != nil {
		t.Fatalf("genuine checkpoint no longer pages: %v", err)
	}
	if got := recordSeqs(more.Records); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("genuine continuation = %v, want [1 2 3]", got)
	}
}

// TestAuditPageAtTerminalCursorsRejectsTamperedCheckpoint pins that an end
// marker never excuses a material mismatch: a wrong fingerprint, or an end
// sequence changed while the old position's fingerprint is kept, fails with
// ErrInvalidRange at cursor 0 and at EndSeq+1 exactly as it does on an
// ordinary mid-paging cursor, and no page is delivered.
func TestAuditPageAtTerminalCursorsRejectsTamperedCheckpoint(t *testing.T) {
	s, last := exhaustedRangeFixture(t)
	cp := last.Checkpoint

	terminalCursors := []int{0, cp.EndSeq + 1}

	// Different fingerprint on an otherwise intact checkpoint.
	wrongFP := cp
	wrongFP.Fingerprint = "00"
	for _, cursor := range terminalCursors {
		page, err := s.AuditPage(wrongFP, cursor, 2, "", "")
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("wrong fingerprint at terminal cursor %d: err = %v, want ErrInvalidRange", cursor, err)
		}
		if page != nil {
			t.Fatalf("wrong fingerprint at terminal cursor %d delivered page %+v", cursor, page)
		}
	}

	// Change only the end sequence while keeping the fingerprint that
	// covered the old position (seq 3); it matches no position under the
	// claimed end (seq 2).
	shifted := cp
	shifted.EndSeq = 2
	for _, cursor := range terminalCursors {
		page, err := s.AuditPage(shifted, cursor, 2, "", "")
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("shifted end at terminal cursor %d: err = %v, want ErrInvalidRange", cursor, err)
		}
		if page != nil {
			t.Fatalf("shifted end at terminal cursor %d delivered page %+v", cursor, page)
		}
	}

	// Re-binding the same material to another organization name fails too:
	// the fingerprint binds the organization.
	foreign := cp
	foreign.Org = "globex"
	for _, cursor := range terminalCursors {
		page, err := s.AuditPage(foreign, cursor, 2, "", "")
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("foreign org at terminal cursor %d: err = %v, want ErrInvalidRange", cursor, err)
		}
		if page != nil {
			t.Fatalf("foreign org at terminal cursor %d delivered page %+v", cursor, page)
		}
	}

	// The error category is the same one ordinary paging produces, on a
	// live cursor — terminals are not a softer validation path.
	for _, bad := range []Checkpoint{wrongFP, shifted, foreign} {
		if _, err := s.AuditPage(bad, 1, 2, "", ""); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("mid-paging with %+v: err = %v, want ErrInvalidRange", bad, err)
		}
	}

	// The genuine checkpoint is unaffected by the rejected attempts.
	if _, err := s.AuditPage(cp, 0, 2, "", ""); err != nil {
		t.Fatalf("genuine checkpoint rejected after tampered calls: %v", err)
	}
	if got := len(drainAudit(t, s, "acme", "", "")); got != 3 {
		t.Fatalf("records = %d, want 3; rejected paging must not append", got)
	}
}

// TestAuditPageHistoricalCheckpointBelowTailStaysValid pins the distinction
// between a corrupted checkpoint and an old-but-genuine one: when the chain
// tail has advanced, a checkpoint whose end sequence is below the tail is
// still valid if its fingerprint really covered that historical position.
// It pages only its own prefix, is never re-stamped with the current tail
// fingerprint, and tampering with it is rejected even at the end cursors.
func TestAuditPageHistoricalCheckpointBelowTailStaysValid(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p1", "u1", "read", "org/a", false),
	}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 2

	// The caller saved this checkpoint when seq 2 was the tail.
	oldRecs, oldCP, err := s.AuditExport("acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	if oldCP.EndSeq != 2 || len(oldRecs) != 2 {
		t.Fatalf("historical export = %+v, want 2 records ending at 2", oldCP)
	}

	// The chain grows afterwards: a new publish and a new decision.
	if _, err := s.Publish("acme", 1, nil); err != nil { // seq 3, v2 empty
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read")) // seq 4
	_, tailCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tailCP.EndSeq != 4 || tailCP.Fingerprint == oldCP.Fingerprint {
		t.Fatalf("tail checkpoint = %+v, want end 4 with a fingerprint distinct from seq 2", tailCP)
	}

	// Cursor 0 against the old range: empty success, range still ends at 2,
	// fingerprint preserved rather than replaced by the current tail's.
	closed, err := s.AuditPage(oldCP, 0, 10, "", "")
	requireEmptyTerminalPage(t, closed, err, oldCP, 1)

	// EndSeq+1 is an empty end page even though seq 3 now exists right
	// behind it: no false out-of-range, no restart delivering seq 1..2.
	onePast, err := s.AuditPage(oldCP, oldCP.EndSeq+1, 10, "", "")
	requireEmptyTerminalPage(t, onePast, err, oldCP, oldCP.EndSeq+1)

	// Ordinary continuation over the historical prefix yields exactly seq
	// 1..2 in ascending order and never reads the appended tail.
	page, err := s.AuditPage(oldCP, 1, 1, "", "")
	if err != nil {
		t.Fatalf("historical prefix paging: %v", err)
	}
	if got := recordSeqs(page.Records); !reflect.DeepEqual(got, []int{1}) || page.Next != 2 {
		t.Fatalf("historical page 1 = %+v, want seq [1], next 2", page)
	}
	page, err = s.AuditPage(oldCP, page.Next, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(page.Records); !reflect.DeepEqual(got, []int{2}) || page.Next != 0 {
		t.Fatalf("historical page 2 = %+v, want seq [2], next 0", page)
	}

	// An old checkpoint is not blanket-accepted: its material must match
	// that old position. Wrong fingerprint at either terminal cursor fails.
	tampered := oldCP
	tampered.Fingerprint = tailCP.Fingerprint // genuine value, wrong position
	for _, cursor := range []int{0, oldCP.EndSeq + 1} {
		p, err := s.AuditPage(tampered, cursor, 10, "", "")
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("historical checkpoint with tail fingerprint at cursor %d: err = %v, want ErrInvalidRange", cursor, err)
		}
		if p != nil {
			t.Fatalf("tampered historical checkpoint delivered page %+v", p)
		}
	}
	// Keeping the old fingerprint while claiming the new tail is equally bad.
	relabeled := oldCP
	relabeled.EndSeq = tailCP.EndSeq
	if _, err := s.AuditPage(relabeled, 0, 10, "", ""); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("old fingerprint under new end: err = %v, want ErrInvalidRange", err)
	}

	// Reading historical pages changed nothing.
	if got := recordSeqs(drainAudit(t, s, "acme", "", "")); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Fatalf("chain changed while reading historical checkpoint: %v", got)
	}
	if v := s.CurrentVersion("acme"); v != 2 {
		t.Fatalf("version = %d, want 2; historical paging must not republish", v)
	}
}

// TestAuditPageEmptyOrgRootCheckpointStillVerifiesFingerprint pins that an
// organization with no records gets no exemption: the genuine root
// checkpoint (end 0, genesis fingerprint) plus cursor 0 returns an empty
// page, but a wrong root fingerprint is rejected with ErrInvalidRange. The
// test distinguishes the two outcomes by (page, err), not by record count:
// a rejected call also "returns no records".
func TestAuditPageEmptyOrgRootCheckpointStillVerifiesFingerprint(t *testing.T) {
	s := NewStore()

	root, err := s.AuditQuery("blank", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if root.Checkpoint != (Checkpoint{Org: "blank", EndSeq: 0, Fingerprint: genesisFingerprint("blank")}) {
		t.Fatalf("empty-org checkpoint = %+v, want the genesis checkpoint", root.Checkpoint)
	}
	if len(root.Records) != 0 || root.Next != 0 {
		t.Fatalf("empty-org first page = %+v, want empty with next 0", root)
	}

	// Genuine root checkpoint replayed at the terminal cursor: valid empty page.
	empty, err := s.AuditPage(root.Checkpoint, 0, 10, "", "")
	requireEmptyTerminalPage(t, empty, err, root.Checkpoint, 1)

	// Same end sequence and cursor, wrong root fingerprint: rejected, and no
	// page is delivered. The empty chain did not waive the fingerprint check.
	badRoot := root.Checkpoint
	badRoot.Fingerprint = "00"
	page, err := s.AuditPage(badRoot, 0, 10, "", "")
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("wrong root fingerprint err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		t.Fatalf("wrong root fingerprint delivered page %+v", page)
	}
	// And not only at cursor 0: a one-past-root cursor must not bypass it.
	if page, err := s.AuditPage(badRoot, 1, 10, "", ""); !errors.Is(err, ErrInvalidRange) || page != nil {
		t.Fatalf("wrong root fingerprint at cursor 1 = (%+v, %v), want nil page, ErrInvalidRange", page, err)
	}

	// Nothing was created by the queries, successful or rejected.
	if recs := drainAudit(t, s, "blank", "", ""); len(recs) != 0 {
		t.Fatalf("empty org gained records: %+v", recs)
	}
	if v := s.CurrentVersion("blank"); v != 0 {
		t.Fatalf("empty org version = %d, want 0", v)
	}

	// A store that has never even seen the organization applies the same
	// rule: a matching genesis root is a valid empty page, any other
	// fingerprint is ErrInvalidRange.
	fresh := NewStore()
	neverSeen := Checkpoint{Org: "ghost", EndSeq: 0, Fingerprint: genesisFingerprint("ghost")}
	ghostPage, err := fresh.AuditPage(neverSeen, 0, 10, "", "")
	if err != nil {
		t.Fatalf("unseen org with genesis checkpoint err = %v, want nil", err)
	}
	if ghostPage == nil || len(ghostPage.Records) != 0 || ghostPage.Next != 0 ||
		ghostPage.Checkpoint != neverSeen {
		t.Fatalf("unseen org genesis page = %+v, want empty page preserving the root checkpoint", ghostPage)
	}
	badGhost := neverSeen
	badGhost.Fingerprint = genesisFingerprint("someone-else")
	if p, err := fresh.AuditPage(badGhost, 0, 10, "", ""); !errors.Is(err, ErrInvalidRange) || p != nil {
		t.Fatalf("unseen org with foreign root fingerprint = (%+v, %v), want nil page, ErrInvalidRange", p, err)
	}
}
