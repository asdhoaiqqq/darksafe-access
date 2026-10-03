package darksafe

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

// buildAuditChainForHugePage creates a small organization: policy change
// (seq 1) interleaved with decisions of two subjects.
func buildAuditChainForHugePage(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq 1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                   // seq 2
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))                   // seq 3
	return s
}

// TestAuditHugePageSizeFirstQuery asks for MaxInt records from a three
// record organization. Page size is a cap, not an allocation request: the
// query must return all three records in ascending order with Next 0,
// without allocating for records that do not exist.
func TestAuditHugePageSizeFirstQuery(t *testing.T) {
	s := buildAuditChainForHugePage(t)
	page, err := s.AuditQuery("acme", 1, math.MaxInt, "", "")
	if err != nil {
		t.Fatalf("huge first page: %v", err)
	}
	got := make([]int, 0, len(page.Records))
	for _, r := range page.Records {
		got = append(got, r.Seq)
	}
	if !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("seqs = %v, want 1 2 3", got)
	}
	if page.Next != 0 {
		t.Fatalf("Next = %d, want 0 (range exhausted)", page.Next)
	}
	if page.EndSeq != 3 || page.BeginSeq != 1 {
		t.Fatalf("range = [%d,%d], want [1,3]", page.BeginSeq, page.EndSeq)
	}
}

// TestAuditHugePageSizeContinuation takes one small page, then requests the
// rest of the pinned range with a huge page. It must return exactly the
// records inside the original checkpoint, keeping its end sequence and
// fingerprint even after more records were appended.
func TestAuditHugePageSizeContinuation(t *testing.T) {
	s := buildAuditChainForHugePage(t)
	first, err := s.AuditQuery("acme", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Next != 2 {
		t.Fatalf("first Next = %d, want 2", first.Next)
	}
	// Append after the query was pinned; these must stay out of later pages.
	s.Publish("acme", 1, nil)                                    // seq 4
	s.Decide("acme", request("acme", "u9", "r1", "org/a", "read")) // seq 5
	rest, err := s.AuditPage(first.Checkpoint, first.Next, math.MaxInt, "", "")
	if err != nil {
		t.Fatalf("huge continuation page: %v", err)
	}
	var got []int
	got = append(got, first.Records[0].Seq)
	for _, r := range rest.Records {
		got = append(got, r.Seq)
	}
	if !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("paged seqs = %v, want 1 2 3", got)
	}
	if rest.Next != 0 {
		t.Fatalf("rest Next = %d, want 0", rest.Next)
	}
	if rest.EndSeq != 3 || rest.Checkpoint != first.Checkpoint {
		t.Fatalf("checkpoint drifted: page = %+v, want end 3 and original checkpoint %+v",
			rest.Checkpoint, first.Checkpoint)
	}
}

// TestAuditHugePageSizeFilters keeps filtering semantics with a huge page:
// interleaved non-matching records are skipped, a no-match range yields an
// empty exhausted page, and a huge page never extends the pinned range.
func TestAuditHugePageSizeFilters(t *testing.T) {
	s := buildAuditChainForHugePage(t) // 1: change, 2: u1 decision, 3: u2 decision

	// Subject filter: the interleaved policy change (seq 1) must be skipped
	// and scanning continues past it to later matches.
	u1, err := s.AuditQuery("acme", 1, math.MaxInt, "", "u1")
	if err != nil {
		t.Fatalf("huge subject page: %v", err)
	}
	if len(u1.Records) != 1 || u1.Records[0].Seq != 2 || u1.Next != 0 {
		t.Fatalf("u1 page = %+v, want only seq 2 and Next 0", u1)
	}

	// No matches anywhere in the pinned range: successful empty page, done.
	none, err := s.AuditQuery("acme", 1, math.MaxInt, "", "ghost")
	if err != nil {
		t.Fatalf("huge no-match page: %v", err)
	}
	if len(none.Records) != 0 || none.Next != 0 || none.EndSeq != 3 {
		t.Fatalf("no-match page = %+v, want empty exhausted page ending at 3", none)
	}

	// Category filter combined with continuation and a huge later page.
	first, err := s.AuditQuery("acme", 1, 1, AuditDecision, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Records[0].Seq != 2 {
		t.Fatalf("first decision seq = %d, want 2", first.Records[0].Seq)
	}
	rest, err := s.AuditPage(first.Checkpoint, first.Next, math.MaxInt, AuditDecision, "")
	if err != nil {
		t.Fatalf("huge filtered continuation: %v", err)
	}
	if len(rest.Records) != 1 || rest.Records[0].Seq != 3 || rest.Next != 0 {
		t.Fatalf("rest decisions = %+v, want only seq 3 and Next 0", rest.Records)
	}
}

// TestAuditHugePageSizeEmptyOrg covers an organization with no records: the
// genesis empty page is returned regardless of the (huge) requested size.
func TestAuditHugePageSizeEmptyOrg(t *testing.T) {
	s := NewStore()
	page, err := s.AuditQuery("acme", 1, math.MaxInt, "", "")
	if err != nil {
		t.Fatalf("huge page on empty org: %v", err)
	}
	if len(page.Records) != 0 || page.Next != 0 || page.EndSeq != 0 {
		t.Fatalf("empty-org page = %+v, want empty genesis page", page)
	}
}

// TestAuditHugePageSizeStillValidates confirms the huge-size change loosens
// nothing else: non-positive sizes are rejected, and tampered checkpoints
// fail by the old rules without returning partial records.
func TestAuditHugePageSizeStillValidates(t *testing.T) {
	s := buildAuditChainForHugePage(t)
	if _, err := s.AuditQuery("acme", 1, 0, "", ""); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("zero page size err = %v", err)
	}
	if _, err := s.AuditQuery("acme", 1, math.MinInt, "", ""); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("negative page size err = %v", err)
	}
	first, err := s.AuditQuery("acme", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	forged := first.Checkpoint
	forged.Fingerprint = "00"
	if _, err := s.AuditPage(forged, first.Next, math.MaxInt, "", ""); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("forged fingerprint with huge page err = %v", err)
	}
	if _, err := s.AuditPage(first.Checkpoint, first.Checkpoint.EndSeq+5, math.MaxInt, "", ""); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("cursor beyond pinned range with huge page err = %v", err)
	}
}
