// Regression coverage for the input validation shared by AuditQuery (the
// first query) and AuditPage (a checkpoint continuation). The organization,
// category and page-size rules — together with the optional resource
// condition — are maintained in one helper, so both operations must accept
// and reject the same inputs with the same sentinel, the same message and
// the same first-reported reason. What remains distinct is each entry
// point's own position argument: the first query's start sequence must be
// >= 1, while a continuation rejects a negative cursor (ErrInvalidPage) and
// a negative checkpoint end (ErrInvalidRange).
package darksafe

import (
	"errors"
	"testing"
)

// requireEquivalentFailure fails unless both entry points reject the same
// bad input with the same sentinel and the identical message text.
func requireEquivalentFailure(t *testing.T, queryErr, pageErr error, want error, label string) {
	t.Helper()
	if !errors.Is(queryErr, want) {
		t.Fatalf("%s: query err = %v, want %v", label, queryErr, want)
	}
	if !errors.Is(pageErr, want) {
		t.Fatalf("%s: page err = %v, want %v", label, pageErr, want)
	}
	if queryErr.Error() != pageErr.Error() {
		t.Fatalf("%s: query message %q != page message %q", label, queryErr, pageErr)
	}
}

// TestAuditPagingSharedValidation pins each common rule on both paths:
// duplicate resource conditions, missing organization, unknown category and
// non-positive page size fail identically, and no page is delivered.
func TestAuditPagingSharedValidation(t *testing.T) {
	s := NewStore()
	root := rootCheckpoint("acme")

	// More than one resource condition.
	_, qErr := s.AuditQuery("acme", 1, 2, "", "", "r1", "r2")
	_, pErr := s.AuditPage(root, 0, 2, "", "", "r1", "r2")
	requireEquivalentFailure(t, qErr, pErr, ErrInvalidPage, "two resource conditions")

	// Missing organization.
	_, qErr = s.AuditQuery("", 1, 1, "", "")
	_, pErr = s.AuditPage(Checkpoint{}, 0, 1, "", "")
	requireEquivalentFailure(t, qErr, pErr, ErrMissingOrganization, "missing organization")

	// Unknown category.
	_, qErr = s.AuditQuery("acme", 1, 1, "bogus", "")
	_, pErr = s.AuditPage(root, 0, 1, "bogus", "")
	requireEquivalentFailure(t, qErr, pErr, ErrInvalidPage, "unknown category")

	// Zero and negative page sizes.
	for _, size := range []int{0, -3} {
		_, qErr = s.AuditQuery("acme", 1, size, "", "")
		_, pErr = s.AuditPage(root, 0, size, "", "")
		requireEquivalentFailure(t, qErr, pErr, ErrInvalidPage, "bad page size")
	}
}

// TestAuditPagingValidationFirstReportedReason pins the historical check
// order against multiple simultaneous bad inputs: resource count, then
// organization, then category, then page size — in both operations.
func TestAuditPagingValidationFirstReportedReason(t *testing.T) {
	s := NewStore()
	root := rootCheckpoint("acme")

	// Duplicate resource conditions outrank the missing organization.
	_, qErr := s.AuditQuery("", 1, 1, "", "", "r1", "r2")
	_, pErr := s.AuditPage(Checkpoint{}, 0, 1, "", "", "r1", "r2")
	requireEquivalentFailure(t, qErr, pErr, ErrInvalidPage, "resources before org")

	// Missing organization outranks the unknown category.
	_, qErr = s.AuditQuery("", 1, 1, "bogus", "")
	_, pErr = s.AuditPage(Checkpoint{}, 0, 1, "bogus", "")
	requireEquivalentFailure(t, qErr, pErr, ErrMissingOrganization, "org before category")

	// Unknown category outranks the non-positive page size.
	_, qErr = s.AuditQuery("acme", 1, 0, "bogus", "")
	_, pErr = s.AuditPage(root, 0, 0, "bogus", "")
	requireEquivalentFailure(t, qErr, pErr, ErrInvalidPage, "category before page size")
	if qErr == nil || qErr.Error() != pErr.Error() {
		t.Fatalf("category precedence: %v / %v", qErr, pErr)
	}

	// The non-positive page size outranks the entry-point-specific position
	// argument: a bad start sequence and a negative cursor are reported only
	// after the common page-size check passes.
	_, qErr = s.AuditQuery("acme", 0, 0, "", "")
	_, pErr = s.AuditPage(root, -1, 0, "", "")
	requireEquivalentFailure(t, qErr, pErr, ErrInvalidPage, "page size before position")
}

// TestAuditPagingKeepsDistinctPositionRules pins the rules the two
// operations do not share: the first query requires start sequence >= 1,
// while a continuation requires a non-negative cursor and a non-negative
// checkpoint end, with the latter classified as ErrInvalidRange.
func TestAuditPagingKeepsDistinctPositionRules(t *testing.T) {
	s := NewStore()
	root := rootCheckpoint("acme")

	if _, err := s.AuditQuery("acme", 0, 1, "", ""); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("zero start sequence err = %v, want ErrInvalidPage", err)
	}
	page, err := s.AuditPage(root, -1, 1, "", "")
	if !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("negative cursor err = %v, want ErrInvalidPage", err)
	}
	if page != nil {
		t.Fatalf("negative cursor delivered page %+v", page)
	}
	if _, err := s.AuditPage(Checkpoint{Org: "acme", EndSeq: -1}, 0, 1, "", ""); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("negative checkpoint end err = %v, want ErrInvalidRange", err)
	}

	// The valid boundary values still behave as before: start sequence 1 and
	// cursor 0 are accepted, and the empty root range still returns its end
	// page once all checks pass.
	first, err := s.AuditQuery("acme", 1, 1, "", "")
	if err != nil {
		t.Fatalf("start sequence 1: %v", err)
	}
	cont, err := s.AuditPage(root, 0, 1, "", "")
	if err != nil {
		t.Fatalf("cursor 0 on root checkpoint: %v", err)
	}
	requireEmptyRootPage(t, cont, err, root)
	_ = first
}
