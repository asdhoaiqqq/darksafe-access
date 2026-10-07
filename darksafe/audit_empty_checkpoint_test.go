// Regression coverage for an empty organization's root checkpoint (end
// sequence 0, fingerprint bound to the organization name) handed directly to
// AuditPage. A first query or a full export over an organization that has not
// produced any audit record returns this root checkpoint, and every such
// checkpoint names a closed, empty range. Continuing it must not depend on
// whether the store has ever visited the organization: a read-only page
// request cannot change whether the same request succeeds or how the range
// is stated merely because an earlier empty query happened to materialize
// the organization's (still empty) state.
//
// These tests pin that:
//
//   - With the genuine root checkpoint, both legal cursors — 0 and 1, the
//     one-past-the-end position that matches a non-empty range's EndSeq+1
//     end page — return an empty page with Next 0, BeginSeq 1, EndSeq 0, and
//     the caller's organization and checkpoint echoed byte-for-byte, whether
//     the organization was never seen, was first visited by an empty query,
//     or by an empty export.
//   - The empty page still rests on real checkpoint material: another
//     organization's root fingerprint or any other wrong value fails with
//     ErrInvalidRange at both cursors and delivers no page. A cursor beyond
//     1 fails the same way, and an organization without records cannot back
//     a checkpoint that already claims positive sequences.
//   - The validations that do not depend on the result (missing
//     organization, non-positive page size, unknown category, repeated
//     resource condition) are still enforced on the empty path; the set
//     filters are accepted and change nothing.
//   - After the empty checkpoint is saved, records the organization later
//     produces never reopen the old range or re-stamp its checkpoint: the
//     closed empty page is identical, an over-end cursor is rejected, and a
//     fresh query is what reveals the new record. Every call is read-only.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// requireEmptyRootPage fails unless the page is a successful terminal page
// of an empty, end-0 range: nil error, non-nil page, no records, Next 0,
// BeginSeq 1, EndSeq 0, and the caller's organization and checkpoint
// returned exactly as submitted.
func requireEmptyRootPage(t *testing.T, page *AuditPage, err error, cp Checkpoint) {
	t.Helper()
	if err != nil {
		t.Fatalf("empty root page err = %v, want nil", err)
	}
	if page == nil {
		t.Fatal("empty root page is nil with nil error")
	}
	if len(page.Records) != 0 {
		t.Fatalf("empty root page delivered %d records %v, want none",
			len(page.Records), recordSeqs(page.Records))
	}
	if page.Next != 0 {
		t.Fatalf("empty root page next = %d, want 0", page.Next)
	}
	if page.BeginSeq != 1 || page.EndSeq != 0 {
		t.Fatalf("empty root page range = %d-%d, want 1-0", page.BeginSeq, page.EndSeq)
	}
	if page.Org != cp.Org {
		t.Fatalf("empty root page org = %q, want %q", page.Org, cp.Org)
	}
	if page.Checkpoint != cp {
		t.Fatalf("empty root page checkpoint = %+v, want submitted %+v", page.Checkpoint, cp)
	}
}

// rootCheckpointsUnderEachStoreState builds the same genuine root checkpoint
// as obtained through every legitimate first-contact path, plus a
// hand-built one for an organization a store never visits at all.
func rootCheckpointsUnderEachStoreState(t *testing.T) map[string]*Store {
	t.Helper()
	org := "blank"

	neverSeen := NewStore() // no call ever names "blank"

	emptyQuery := NewStore()
	first, err := emptyQuery.AuditQuery(org, 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Checkpoint != (Checkpoint{Org: org, EndSeq: 0, Fingerprint: genesisFingerprint(org)}) ||
		len(first.Records) != 0 || first.Next != 0 || first.BeginSeq != 1 || first.EndSeq != 0 {
		t.Fatalf("first empty query = %+v, want closed root page 1-0", first)
	}

	emptyExport := NewStore()
	recs, exportCP, err := emptyExport.AuditExport(org, 0)
	if err != nil || len(recs) != 0 || exportCP.EndSeq != 0 {
		t.Fatalf("empty export = %+v, %+v, %v", recs, exportCP, err)
	}

	return map[string]*Store{
		"never visited": neverSeen,
		"empty query":   emptyQuery,
		"empty export":  emptyExport,
	}
}

// TestEmptyRootCheckpointPagesIdenticallyAcrossStoreStates pins the core
// fix: the same legal end-0 checkpoint, continued with cursor 0 or 1,
// succeeds with the same closed empty page regardless of whether the store
// has ever visited the organization. Before the fix an unseen organization
// failed cursor 1 with ErrInvalidRange and reported cursor 0's BeginSeq as 0,
// while an organization first touched by an empty query answered cursor 1
// successfully — a read-only request whose result depended on history.
func TestEmptyRootCheckpointPagesIdenticallyAcrossStoreStates(t *testing.T) {
	org := "blank"
	cp := Checkpoint{Org: org, EndSeq: 0, Fingerprint: genesisFingerprint(org)}
	stores := rootCheckpointsUnderEachStoreState(t)

	var reference *AuditPage
	for _, state := range []string{"never visited", "empty query", "empty export"} {
		s := stores[state]
		for _, cursor := range []int{0, 1} {
			page, err := s.AuditPage(cp, cursor, 10, "", "")
			requireEmptyRootPage(t, page, err, cp)
			if reference == nil {
				reference = page
			} else if !reflect.DeepEqual(page, reference) {
				t.Fatalf("%s cursor %d page = %+v, want %+v", state, cursor, page, reference)
			}
		}
	}

	// Set filters do not change the empty, closed answer either: the
	// validation passes and no record appears.
	s := stores["never visited"]
	for _, call := range []struct {
		name     string
		kind     string
		subj     string
		resource []string
	}{
		{"decision kind", AuditDecision, "", nil},
		{"policy kind", AuditPolicyChange, "", nil},
		{"subject", "", "u1", nil},
		{"resource", "", "", []string{"r1"}},
	} {
		for _, cursor := range []int{0, 1} {
			page, err := s.AuditPage(cp, cursor, 10, call.kind, call.subj, call.resource...)
			requireEmptyRootPage(t, page, err, cp)
		}
	}
}

// TestEmptyRootCheckpointStillRequiresGenuineFingerprint pins that the empty
// range grants no exemption from checkpoint verification: a foreign root or
// any other wrong fingerprint fails at both legal cursors with
// ErrInvalidRange and no page, on both an unseen organization and one an
// empty query first visited.
func TestEmptyRootCheckpointStillRequiresGenuineFingerprint(t *testing.T) {
	cp := Checkpoint{Org: "blank", EndSeq: 0, Fingerprint: genesisFingerprint("blank")}
	stores := rootCheckpointsUnderEachStoreState(t)

	wrong := map[string]Checkpoint{
		"foreign root": {Org: "blank", EndSeq: 0, Fingerprint: genesisFingerprint("globex")},
		"garbage":      {Org: "blank", EndSeq: 0, Fingerprint: "00"},
		"empty string": {Org: "blank", EndSeq: 0, Fingerprint: ""},
	}
	for state, s := range stores {
		for name, bad := range wrong {
			for _, cursor := range []int{0, 1} {
				page, err := s.AuditPage(bad, cursor, 10, "", "")
				if !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("%s / %s at cursor %d: err = %v, want ErrInvalidRange",
						state, name, cursor, err)
				}
				if page != nil {
					t.Fatalf("%s / %s at cursor %d delivered page %+v", state, name, cursor, page)
				}
			}
		}
		// The genuine checkpoint still works after the rejected calls.
		if page, err := s.AuditPage(cp, 1, 10, "", ""); err != nil || page == nil {
			t.Fatalf("%s: genuine root no longer accepted after tampered calls: %+v, %v", state, page, err)
		}
	}
}

// TestEmptyRootCheckpointRejectsOutOfRangeCursorsAndClaims pins the empty
// range boundaries. Cursor 1 is the single one-past-end position; anything
// beyond it is out of range, even if the fingerprint is the genuine root and
// even after the organization later grows real records. A checkpoint that
// claims a positive end sequence against an organization with no records is
// rejected regardless of its fingerprint.
func TestEmptyRootCheckpointRejectsOutOfRangeCursorsAndClaims(t *testing.T) {
	cp := Checkpoint{Org: "blank", EndSeq: 0, Fingerprint: genesisFingerprint("blank")}

	for state, s := range rootCheckpointsUnderEachStoreState(t) {
		for _, cursor := range []int{2, 3, 100} {
			page, err := s.AuditPage(cp, cursor, 10, "", "")
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("%s: genuine root at cursor %d err = %v, want ErrInvalidRange",
					state, cursor, err)
			}
			if page != nil {
				t.Fatalf("%s: cursor %d delivered page %+v", state, cursor, page)
			}
		}

		// No records exist, so a checkpoint asserting that sequence 1 already
		// exists can never be backed — neither with the root fingerprint nor
		// with an invented one.
		for _, claimed := range []Checkpoint{
			{Org: "blank", EndSeq: 1, Fingerprint: genesisFingerprint("blank")},
			{Org: "blank", EndSeq: 1, Fingerprint: "deadbeef"},
			{Org: "blank", EndSeq: 50, Fingerprint: genesisFingerprint("blank")},
		} {
			for _, cursor := range []int{0, 1} {
				page, err := s.AuditPage(claimed, cursor, 10, "", "")
				if !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("%s: claim %+v at cursor %d err = %v, want ErrInvalidRange",
						state, claimed, cursor, err)
				}
				if page != nil {
					t.Fatalf("%s: claim %+v at cursor %d delivered page %+v",
						state, claimed, cursor, page)
				}
			}
		}
	}
}

// TestEmptyRootCheckpointStaysClosedAfterRecordsAppend pins that a saved
// empty checkpoint is an immutable range boundary. Records produced later
// never enter the empty range from cursor 0 or 1, the returned checkpoint is
// not advanced to the current chain tail, cursor 2 stays out of range, and
// only a fresh query reveals the appended record.
func TestEmptyRootCheckpointStaysClosedAfterRecordsAppend(t *testing.T) {
	org := "blank"
	cp := Checkpoint{Org: org, EndSeq: 0, Fingerprint: genesisFingerprint(org)}

	for state, s := range rootCheckpointsUnderEachStoreState(t) {
		// The organization produces its first real records after the empty
		// checkpoint was saved.
		s.Decide(org, request(org, "u1", "r1", "org/a", "read")) // seq 1
		s.Decide(org, request(org, "u2", "r1", "org/a", "read")) // seq 2

		for _, cursor := range []int{0, 1} {
			page, err := s.AuditPage(cp, cursor, 10, "", "")
			requireEmptyRootPage(t, page, err, cp)
			if page.Checkpoint.EndSeq != 0 || page.Checkpoint.Fingerprint != cp.Fingerprint {
				t.Fatalf("%s: old empty checkpoint re-stamped to %+v", state, page.Checkpoint)
			}
		}
		// New records do not extend the old empty range's legal cursors.
		if page, err := s.AuditPage(cp, 2, 10, "", ""); !errors.Is(err, ErrInvalidRange) || page != nil {
			t.Fatalf("%s: cursor 2 after append = (%+v, %v), want nil page, ErrInvalidRange",
				state, page, err)
		}

		// A fresh query starts a new range and sees both new records.
		fresh, err := s.AuditQuery(org, 1, 10, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if got := recordSeqs(fresh.Records); !reflect.DeepEqual(got, []int{1, 2}) ||
			fresh.Checkpoint.EndSeq != 2 || fresh.Checkpoint.Fingerprint == cp.Fingerprint {
			t.Fatalf("%s: fresh query = %+v, want seqs [1 2] ending at 2 with a new tail fingerprint",
				state, fresh)
		}

		// All page calls were read-only: exactly the two decisions exist and
		// no policy was ever published.
		if got := recordSeqs(drainAudit(t, s, org, "", "")); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Fatalf("%s: chain = %v, want [1 2]", state, got)
		}
		if v := s.CurrentVersion(org); v != 0 {
			t.Fatalf("%s: version = %d, want 0; paging must not publish", state, v)
		}
	}
}

// TestEmptyRootCheckpointKeepsOrdinaryValidation pins that ending up empty
// skips no validation that is independent of the result: missing
// organization, non-positive page size, unknown category and a repeated
// resource condition are all rejected before an empty page could be
// returned, at both legal cursors.
func TestEmptyRootCheckpointKeepsOrdinaryValidation(t *testing.T) {
	cp := Checkpoint{Org: "blank", EndSeq: 0, Fingerprint: genesisFingerprint("blank")}
	s := NewStore()

	for _, cursor := range []int{0, 1} {
		if _, err := s.AuditPage(Checkpoint{}, cursor, 10, "", ""); !errors.Is(err, ErrMissingOrganization) {
			t.Fatalf("cursor %d: missing org err = %v, want ErrMissingOrganization", cursor, err)
		}
		for _, pageSize := range []int{0, -3} {
			if _, err := s.AuditPage(cp, cursor, pageSize, "", ""); !errors.Is(err, ErrInvalidPage) {
				t.Fatalf("cursor %d: page size %d err = %v, want ErrInvalidPage", cursor, pageSize, err)
			}
		}
		if _, err := s.AuditPage(cp, cursor, 10, "bogus", ""); !errors.Is(err, ErrInvalidPage) {
			t.Fatalf("cursor %d: bad category err = %v, want ErrInvalidPage", cursor, err)
		}
		if _, err := s.AuditPage(cp, cursor, 10, "", "", "r1", "r2"); !errors.Is(err, ErrInvalidPage) {
			t.Fatalf("cursor %d: two resource conditions err = %v, want ErrInvalidPage", cursor, err)
		}
	}

	// Rejected validation changed nothing and the genuine request still
	// closes the empty range.
	if recs := drainAudit(t, s, "blank", "", ""); len(recs) != 0 {
		t.Fatalf("empty org gained records: %+v", recs)
	}
	page, err := s.AuditPage(cp, 1, 10, "", "")
	requireEmptyRootPage(t, page, err, cp)
}
