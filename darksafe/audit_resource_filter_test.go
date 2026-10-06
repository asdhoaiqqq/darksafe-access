// Regression coverage for resource-filtered audit queries. The view is the
// resource-side twin of the subject view: an auditor following ONE resource
// (for example one ledger) wants every decision ever made about it, allows
// and denials alike, while the chain interleaves those records with other
// resources' decisions, policy changes that merely mention the same
// resource ID, and the same resource identifier active in another
// organization. These tests pin the contract of the optional trailing
// resource condition on the existing AuditQuery/AuditPage entry points:
//
//   - The resource condition matches the resource ID the decision record
//     requested, byte for byte: case, spaces, control bytes and invalid
//     UTF-8 are significant, '*' is an ordinary byte, no scope path or
//     wildcard is interpreted, and a lone 0xFF is distinguishable from a
//     lone 0xFE and from a genuine U+FFFD replacement rune.
//   - It selects decision records only: envelope rejections survive
//     (disabled subject, organization mismatch), but policy-change records
//     never do, even when a published policy carries the identical
//     ResourceID. The identical resource ID in another organization cannot
//     mix in; the queried organization's chain is the only source.
//   - It is conjunctive with the category and subject conditions; the
//     policy-change category together with a resource condition is empty,
//     never a fallback to that resource's decisions.
//   - The page size bounds MATCHING records; interleaved other-resource
//     records and policy changes never occupy a slot. Concatenating the
//     pages along the cursors yields ascending original sequences with no
//     duplicates or gaps, inside the range pinned by the first page only.
//   - No matches yields an empty page that ends the walk, never the
//     unfiltered chain; a tampered checkpoint fingerprint still fails with
//     ErrInvalidRange and delivers no partial records.
//   - Absent or empty means unset, and an unset resource condition is
//     byte-for-byte the historical query. Returned records stay detached
//     copies, and querying appends nothing and changes no version.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// resourcePageWalk pages a resource-filtered query to exhaustion with the
// given extra kind/subject conditions. It fails on a repeated cursor or a
// checkpoint that drifts from the first page's.
func resourcePageWalk(t *testing.T, s *Store, org string, startSeq, pageSize int, kind, subject, resource string) []AuditRecord {
	t.Helper()
	page, err := s.AuditQuery(org, startSeq, pageSize, kind, subject, resource)
	if err != nil {
		t.Fatalf("AuditQuery(%q, start %d, resource %q): %v", org, startSeq, resource, err)
	}
	var recs []AuditRecord
	first := page.Checkpoint
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
		page, err = s.AuditPage(page.Checkpoint, page.Next, pageSize, kind, subject, resource)
		if err != nil {
			t.Fatalf("AuditPage(next %d, resource %q): %v", page.Next, resource, err)
		}
		if page.Checkpoint != first {
			t.Fatalf("page checkpoint = %+v, want the first page's %+v", page.Checkpoint, first)
		}
	}
	return recs
}

// requireResourceDecisions fails unless every record is a decision in the
// given organization whose request named exactly the given resource.
func requireResourceDecisions(t *testing.T, org, resource string, recs []AuditRecord) {
	t.Helper()
	for _, r := range recs {
		if r.Org != org || r.Kind != AuditDecision || r.Decision == nil {
			t.Fatalf("resource filter leaked %+v", r)
		}
		if r.Decision.Request.Resource.ID != resource {
			t.Fatalf("record seq %d requests resource %q, want %q",
				r.Seq, r.Decision.Request.Resource.ID, resource)
		}
	}
}

const (
	resScope  = "acme/factory/ledger"
	resOther  = "acme/other/ledger"
	ledgerA   = "ledger-2026"
	ledgerB   = "ledger-2025"
	globexOrg = "globex"
)

// resourceFixture builds one acme chain in which decisions about ledgerA
// are interleaved with ledgerB decisions, envelope rejections about
// ledgerA (disabled subject, organization mismatch), publishes and a
// rollback. One of ledgerA's decisions uses the same resource ID at a
// different scope, because the condition is on the resource identifier,
// not on the scope:
//
//	seq 1  publish v1 (resource-specific allow/deny plus scope-only deny)
//	seq 2  u1 read  ledgerA @ resScope  -> allow (v1)
//	seq 3  u2 read  ledgerB @ resScope  -> allow (v1)
//	seq 4  u1 read  ledgerB @ resScope  -> no-match denial
//	seq 5  u1 write ledgerA @ resScope  -> matched-deny (v1)
//	seq 6  disabled u9 read ledgerA    -> subject-disabled denial
//	seq 7  u1 read  ledgerA, resource org globex -> organization-mismatch denial
//	seq 8  publish v2 (empty set)
//	seq 9  u1 read  ledgerA @ resScope  -> no-match denial (v2)
//	seq 10 u2 read  ledgerB @ resScope  -> no-match denial (v2)
//	seq 11 rollback to v1 -> v3
//	seq 12 u1 read  ledgerA @ resScope  -> allow (v3)
//	seq 13 u1 read  ledgerA @ resOther  -> no-match denial (same ID, other scope)
//
// A separate globex chain also decides on a resource literally named
// ledgerA, to prove cross-organization isolation of the identifier.
//
// It returns the submitted requests keyed by sequence for content checks.
func resourceFixture(t *testing.T, s *Store) map[int]OrgRequest {
	t.Helper()
	reqs := map[int]OrgRequest{}
	if _, err := s.Publish("acme", 0, []Policy{
		{ID: "p-a-read", Subject: "u1", Action: "read", Scope: resScope, Effect: EffectAllow, ResourceID: ledgerA},
		{ID: "p-a-write", Subject: "u1", Action: "write", Scope: resScope, Effect: EffectDeny},
		{ID: "p-b-read", Subject: "u2", Action: "read", Scope: resScope, Effect: EffectAllow, ResourceID: ledgerB},
	}); err != nil {
		t.Fatal(err)
	}
	decide := func(seq int, req OrgRequest) {
		t.Helper()
		reqs[seq] = req
		s.Decide("acme", req)
	}
	decide(2, request("acme", "u1", ledgerA, resScope, "read"))
	decide(3, request("acme", "u2", ledgerB, resScope, "read"))
	decide(4, request("acme", "u1", ledgerB, resScope, "read"))
	decide(5, request("acme", "u1", ledgerA, resScope, "write"))
	decide(6, OrgRequest{
		SubjectOrg: "acme", ResourceOrg: "acme",
		Subject:  Subject{ID: "u9", Disabled: true},
		Resource: Resource{ID: ledgerA, Scope: resScope},
		Action:   "read",
	})
	decide(7, OrgRequest{
		SubjectOrg: "acme", ResourceOrg: globexOrg,
		Subject:  Subject{ID: "u1"},
		Resource: Resource{ID: ledgerA, Scope: resScope},
		Action:   "read",
	})
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	decide(9, request("acme", "u1", ledgerA, resScope, "read"))
	decide(10, request("acme", "u2", ledgerB, resScope, "read"))
	if _, err := s.Rollback("acme", 2, 1); err != nil {
		t.Fatal(err)
	}
	decide(12, request("acme", "u1", ledgerA, resScope, "read"))
	decide(13, request("acme", "u1", ledgerA, resOther, "read"))

	// The same resource identifier in another organization, including a
	// policy that names it, stays entirely outside the acme view.
	if _, err := s.Publish(globexOrg, 0, []Policy{
		{ID: "p-g", Subject: "u1", Action: "read", Scope: "g/ledger", Effect: EffectAllow, ResourceID: ledgerA},
	}); err != nil {
		t.Fatal(err)
	}
	s.Decide(globexOrg, request(globexOrg, "u1", ledgerA, "g/ledger", "read"))
	s.Decide(globexOrg, request(globexOrg, "u2", ledgerA, "g/ledger", "read"))
	return reqs
}

// TestResourceFilterSelectsOnlyThatResourcesDecisions pins the core rule:
// the resource view returns exactly the organization's decision records
// that requested the identifier — allows and denials, including the
// disabled-subject and organization-mismatch rejections, and including the
// same identifier at a different scope — while other resources, policy
// changes (even ones carrying the identical ResourceID) and the other
// organization's identically named resource never mix in. Original
// sequence, full request and the complete decision explanation survive.
func TestResourceFilterSelectsOnlyThatResourcesDecisions(t *testing.T) {
	s := NewStore()
	reqs := resourceFixture(t, s)

	recs := resourcePageWalk(t, s, "acme", 1, 2, "", "", ledgerA)
	wantSeqs := []int{2, 5, 6, 7, 9, 12, 13}
	if got := recordSeqs(recs); !reflect.DeepEqual(got, wantSeqs) {
		t.Fatalf("ledgerA seqs = %v, want %v", got, wantSeqs)
	}
	requireResourceDecisions(t, "acme", ledgerA, recs)

	for _, r := range recs {
		if !reflect.DeepEqual(r.Decision.Request, reqs[r.Seq]) {
			t.Fatalf("seq %d request = %+v, want %+v", r.Seq, r.Decision.Request, reqs[r.Seq])
		}
		if r.Decision.Decision.Reason == "" {
			t.Fatalf("seq %d decision lost its explanation: %+v", r.Seq, r.Decision.Decision)
		}
	}
	// Both outcomes and both envelope rejections survive.
	outcomes := map[int]bool{}
	reasons := map[int]string{}
	for _, r := range recs {
		outcomes[r.Seq] = r.Decision.Decision.Allowed
		reasons[r.Seq] = r.Decision.Decision.Reason
	}
	if !outcomes[2] || outcomes[5] || outcomes[6] || outcomes[7] ||
		outcomes[9] || !outcomes[12] || outcomes[13] {
		t.Fatalf("allow/deny outcomes not preserved: %v", outcomes)
	}
	if reasons[6] != "subject is disabled" {
		t.Fatalf("disabled rejection reason = %q", reasons[6])
	}
	if reasons[7] != "organization mismatch" {
		t.Fatalf("mismatch rejection reason = %q", reasons[7])
	}
	if reasons[5] != "matched deny policy" {
		t.Fatalf("matched-deny reason = %q", reasons[5])
	}

	// The other ledger's view holds only its own three records.
	bRecs := resourcePageWalk(t, s, "acme", 1, 3, "", "", ledgerB)
	if got := recordSeqs(bRecs); !reflect.DeepEqual(got, []int{3, 4, 10}) {
		t.Fatalf("ledgerB seqs = %v, want [3 4 10]", got)
	}
	requireResourceDecisions(t, "acme", ledgerB, bRecs)

	// The other organization's identical identifier is invisible here; its
	// own view holds its own records, and distinct chains keep the two
	// organizations' same-ID decisions apart.
	gRecs := resourcePageWalk(t, s, globexOrg, 1, 2, "", "", ledgerA)
	if got := recordSeqs(gRecs); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("globex ledgerA seqs = %v, want [2 3]", got)
	}
	requireResourceDecisions(t, globexOrg, ledgerA, gRecs)
	if gRecs[0].Fingerprint == recs[0].Fingerprint {
		t.Fatal("identical resource identifiers across organizations share a fingerprint")
	}

	// Queries appended nothing and changed no version.
	if got := len(drainAudit(t, s, "acme", "", "")); got != 13 {
		t.Fatalf("acme records = %d, want 13; queries must not append", got)
	}
	if got := len(drainAudit(t, s, globexOrg, "", "")); got != 3 {
		t.Fatalf("globex records = %d, want 3; queries must not append", got)
	}
	if v := s.CurrentVersion("acme"); v != 3 {
		t.Fatalf("acme version = %d, want 3; queries must not republish", v)
	}
}

// TestResourceAndCategoryAndSubjectFiltersAreConjunctive pins that the
// resource condition ANDs with the existing conditions: policy-change +
// resource is empty even though a published policy names that resource,
// decision + subject + resource narrows to one subject's records, and
// categories never fall back to each other.
func TestResourceAndCategoryAndSubjectFiltersAreConjunctive(t *testing.T) {
	s := NewStore()
	resourceFixture(t, s)

	// Policy-change category plus resource: the seq 1 policy set literally
	// contains ResourceID == ledgerA, yet the result must be empty rather
	// than the change, and the resource's decisions must not substitute for
	// it either.
	none := resourcePageWalk(t, s, "acme", 1, 2, AuditPolicyChange, "", ledgerA)
	if len(none) != 0 {
		t.Fatalf("policy_change+resource = %+v, want empty (conjunction, no fallback)", none)
	}

	// Decision + resource: the plain resource view.
	decisions := resourcePageWalk(t, s, "acme", 1, 4, AuditDecision, "", ledgerA)
	if got := recordSeqs(decisions); !reflect.DeepEqual(got, []int{2, 5, 6, 7, 9, 12, 13}) {
		t.Fatalf("decision+ledgerA seqs = %v, want [2 5 6 7 9 12 13]", got)
	}

	// Decision + subject + resource: u1's ledgerA records (the disabled u9
	// rejection at seq 6 is excluded by the subject condition), while u1's
	// ledgerB decision at seq 4 is excluded by the resource condition.
	u1A := resourcePageWalk(t, s, "acme", 1, 2, AuditDecision, "u1", ledgerA)
	if got := recordSeqs(u1A); !reflect.DeepEqual(got, []int{2, 5, 7, 9, 12, 13}) {
		t.Fatalf("u1+ledgerA seqs = %v, want [2 5 7 9 12 13]", got)
	}
	// Same resource, a subject with no decisions about it: empty.
	u2A := resourcePageWalk(t, s, "acme", 1, 2, AuditDecision, "u2", ledgerA)
	if len(u2A) != 0 {
		t.Fatalf("u2+ledgerA = %+v, want empty", u2A)
	}
	// Subject without the resource condition still sees that subject's
	// decisions about every resource; the resource condition narrows it.
	u1All := resourcePageWalk(t, s, "acme", 1, 4, AuditDecision, "u1", "")
	if got := recordSeqs(u1All); !reflect.DeepEqual(got, []int{2, 4, 5, 7, 9, 12, 13}) {
		t.Fatalf("u1 all resources seqs = %v, want [2 4 5 7 9 12 13]", got)
	}
}

// TestResourceFilterUsesRawBytes pins byte-level identity: only an equal
// string matches, so case, leading/trailing/internal spaces, a literal
// wildcard, control bytes and Chinese all keep distinct identifiers.
func TestResourceFilterUsesRawBytes(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p1", "u1", "read", "org/a", false),
	}); err != nil {
		t.Fatal(err)
	}
	// Every decision below is an allow under the scope-only policy; only
	// the resource identifiers matter.
	ids := []string{
		ledgerA,
		"Ledger-2026",  // different case
		" ledger-2026", // leading space
		"ledger-2026 ", // trailing space
		"ledger 2026",  // internal space
		"ledger*",      // '*' is an ordinary byte, not a wildcard
		"ledger\tA\n",  // tab and newline
		"账本-2026",      // Chinese
	}
	wantSeq := map[string]int{}
	for i, id := range ids {
		req := request("acme", "u1", id, "org/a", "read")
		d := s.Decide("acme", req)
		if !d.Allowed {
			t.Fatalf("decision for %q = %+v", id, d)
		}
		wantSeq[id] = i + 2 // seq 1 is the publish
	}
	// A lookalike that shares a prefix and would match under wildcard or
	// trimmed/path-relaxed comparisons must not match ledgerA.
	s.Decide("acme", request("acme", "u1", "ledger-2026/extra", "org/a", "read"))
	s.Decide("acme", request("acme", "u1", "ledger-202", "org/a", "read"))

	for _, id := range ids {
		recs := resourcePageWalk(t, s, "acme", 1, 2, "", "", id)
		if len(recs) != 1 || recs[0].Seq != wantSeq[id] {
			t.Fatalf("filter %q returned %v, want only seq %d", id, recordSeqs(recs), wantSeq[id])
		}
		if got := recs[0].Decision.Request.Resource.ID; got != id {
			t.Fatalf("filter %q returned record for %q", id, got)
		}
	}
}

// TestResourceFilterDistinguishesInvalidBytesFromReplacement pins that the
// comparison never repairs bytes: a lone 0xFF, a lone 0xFE and a genuine
// U+FFFD replacement rune embedded in the same Chinese identifier are three
// different resources, and filtering by one never returns another. The
// records themselves chain and verify across the raw/JSON fingerprint
// families.
func TestResourceFilterDistinguishesInvalidBytesFromReplacement(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p1", "u1", "read", "org/a", false),
	}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{
		"0xff":  "账" + invalidByte + "本",
		"0xfe":  "账" + anotherInvalidByte + "本",
		"ufffd": "账" + replacementRune + "本",
	}
	wantSeq := map[string]int{}
	for _, name := range []string{"0xff", "0xfe", "ufffd"} {
		id := ids[name]
		d := s.Decide("acme", request("acme", "u1", id, "org/a", "read"))
		if !d.Allowed {
			t.Fatalf("%s decision = %+v", name, d)
		}
		wantSeq[name] = 2 + len(wantSeq)
	}
	// Sanity: the mixed chain (two invalid-byte records, one genuine U+FFFD
	// record) still verifies end to end.
	all, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", all, cp); err != nil {
		t.Fatalf("mixed raw-byte chain must verify: %v", err)
	}

	for _, name := range []string{"0xff", "0xfe", "ufffd"} {
		id := ids[name]
		recs := resourcePageWalk(t, s, "acme", 1, 2, "", "", id)
		if len(recs) != 1 {
			t.Fatalf("%s: filter returned %v, want exactly one record", name, recordSeqs(recs))
		}
		if recs[0].Seq != wantSeq[name] {
			t.Fatalf("%s: seq = %d, want %d", name, recs[0].Seq, wantSeq[name])
		}
		if got := recs[0].Decision.Request.Resource.ID; got != id {
			t.Fatalf("%s: got resource bytes %q, want %q", name, got, id)
		}
	}
}

// TestResourcePagingCountsMatchesNotScannedRecords pins that page size
// bounds returned matches, so other-resource decisions and policy changes
// between matches never cut a page short, the concatenation is ascending
// and gapless, start sequences exclude earlier matches, and a final exact
// fill is followed by one empty page that ends the walk.
func TestResourcePagingCountsMatchesNotScannedRecords(t *testing.T) {
	s := NewStore()
	resourceFixture(t, s)
	wantSeqs := []int{2, 5, 6, 7, 9, 12, 13}

	// Page size 2 across the fixture: matches sit up to three chain
	// positions apart with policy changes between them; every page fills
	// with matches anyway.
	page, err := s.AuditQuery("acme", 1, 2, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(page.Records); !reflect.DeepEqual(got, []int{2, 5}) {
		t.Fatalf("page 1 seqs = %v, want [2 5]; non-matches truncated the page", got)
	}
	if page.Next != 6 {
		t.Fatalf("page 1 next = %d, want 6 (first unscanned position)", page.Next)
	}
	page2, err := s.AuditPage(page.Checkpoint, page.Next, 2, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(page2.Records); !reflect.DeepEqual(got, []int{6, 7}) {
		t.Fatalf("page 2 seqs = %v, want [6 7]", got)
	}

	// Other page sizes all concatenate to the same ascending match list.
	for _, size := range []int{1, 3, 7, 100} {
		recs := resourcePageWalk(t, s, "acme", 1, size, "", "", ledgerA)
		if got := recordSeqs(recs); !reflect.DeepEqual(got, wantSeqs) {
			t.Fatalf("page size %d seqs = %v, want %v", size, got, wantSeqs)
		}
		requireResourceDecisions(t, "acme", ledgerA, recs)
	}

	// A legal start excludes earlier matches, wherever it lands.
	fromNine := resourcePageWalk(t, s, "acme", 9, 2, "", "", ledgerA)
	if got := recordSeqs(fromNine); !reflect.DeepEqual(got, []int{9, 12, 13}) {
		t.Fatalf("from seq 9 = %v, want [9 12 13]", got)
	}
	fromTen := resourcePageWalk(t, s, "acme", 10, 2, "", "", ledgerA)
	if got := recordSeqs(fromTen); !reflect.DeepEqual(got, []int{12, 13}) {
		t.Fatalf("from seq 10 = %v, want [12 13]", got)
	}
}

// TestResourceQueryCheckpointPinsAgainstLaterAppends mirrors the subject
// view's read-while-growing guarantee for the resource condition: the
// pinned end and fingerprint rule every page, later decisions about the
// SAME resource cannot enter the old walk, and only a fresh query sees
// them.
func TestResourceQueryCheckpointPinsAgainstLaterAppends(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		{ID: "p-a", Subject: "u1", Action: "read", Scope: resScope, Effect: EffectAllow, ResourceID: ledgerA},
	}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", ledgerA, resScope, "read")) // seq 2 allow
	s.Decide("acme", request("acme", "u2", ledgerB, resScope, "read")) // seq 3 other resource
	s.Decide("acme", request("acme", "u1", ledgerA, resScope, "read")) // seq 4 allow

	first, err := s.AuditQuery("acme", 1, 1, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(first.Records); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("first page seqs = %v, want [2]", got)
	}
	cp := first.Checkpoint
	if cp.EndSeq != 4 {
		t.Fatalf("pinned end = %d, want 4", cp.EndSeq)
	}

	// After the pin: another ledgerA decision, a ledgerB decision and a
	// publish that carries the ledgerA resource ID.
	s.Decide("acme", request("acme", "u1", ledgerA, resScope, "read")) // seq 5
	s.Decide("acme", request("acme", "u2", ledgerB, resScope, "read")) // seq 6
	if _, err := s.Publish("acme", 1, []Policy{
		{ID: "p-a2", Subject: "u1", Action: "read", Scope: resScope, Effect: EffectAllow, ResourceID: ledgerA},
	}); err != nil {
		t.Fatal(err) // seq 7
	}

	second, err := s.AuditPage(cp, first.Next, 1, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if got := recordSeqs(second.Records); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("second page seqs = %v, want [4]; appended records leaked", got)
	}
	if second.Checkpoint != cp || second.EndSeq != 4 {
		t.Fatalf("second page = %+v, want pinned checkpoint/end 4", second)
	}
	third, err := s.AuditPage(cp, second.Next, 1, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Records) != 0 || third.Next != 0 || third.Checkpoint != cp {
		t.Fatalf("final pinned page = %+v, want empty, next 0, pinned checkpoint", third)
	}

	// A fresh query is the only view that reaches the appended decision.
	fresh := resourcePageWalk(t, s, "acme", 1, 2, "", "", ledgerA)
	if got := recordSeqs(fresh); !reflect.DeepEqual(got, []int{2, 4, 5}) {
		t.Fatalf("fresh seqs = %v, want [2 4 5]", got)
	}
}

// TestResourceQueryWithoutMatchesReturnsEmpty pins the no-match behavior:
// an unknown resource — in a populated chain or an empty organization —
// yields an empty page that ends the walk, never an error or the
// unfiltered chain.
func TestResourceQueryWithoutMatchesReturnsEmpty(t *testing.T) {
	s := NewStore()
	resourceFixture(t, s)

	page, err := s.AuditQuery("acme", 1, 4, "", "", "ledger-does-not-exist")
	if err != nil {
		t.Fatalf("no-match query err = %v, want nil", err)
	}
	if len(page.Records) != 0 || page.Next != 0 {
		t.Fatalf("no-match page = %+v, want empty with next 0", page)
	}
	if page.Checkpoint.EndSeq != 13 {
		t.Fatalf("no-match checkpoint end = %d, want the pinned head 13", page.Checkpoint.EndSeq)
	}
	if recs := resourcePageWalk(t, s, "acme", 6, 2, "", "", "ledger-does-not-exist"); len(recs) != 0 {
		t.Fatalf("no-match walk from seq 6 = %+v, want empty", recs)
	}

	empty, err := s.AuditQuery("blank", 1, 4, "", "", ledgerA)
	if err != nil {
		t.Fatalf("empty-org query err = %v, want nil", err)
	}
	if len(empty.Records) != 0 || empty.Next != 0 || empty.Checkpoint.EndSeq != 0 {
		t.Fatalf("empty-org page = %+v, want the empty genesis page", empty)
	}
}

// TestResourcePagingRejectsTamperedCheckpointFingerprint pins that the
// existing fingerprint guard is unchanged under a resource condition:
// ErrInvalidRange, no partial records, and the genuine checkpoint still
// pages after the forgeries.
func TestResourcePagingRejectsTamperedCheckpointFingerprint(t *testing.T) {
	s := NewStore()
	resourceFixture(t, s)

	first, err := s.AuditQuery("acme", 1, 1, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 1 || first.Next == 0 {
		t.Fatalf("first page = %+v, want one record and a live cursor", first)
	}
	forged := first.Checkpoint
	forged.Fingerprint = "00"
	page, err := s.AuditPage(forged, first.Next, 1, "", "", ledgerA)
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("forged fingerprint err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		t.Fatalf("forged fingerprint delivered partial page %+v", page)
	}
	// The genuine checkpoint still pages to the full match list.
	rest, err := s.AuditPage(first.Checkpoint, first.Next, 10, "", "", ledgerA)
	if err != nil {
		t.Fatalf("genuine continuation: %v", err)
	}
	if got := recordSeqs(rest.Records); !reflect.DeepEqual(got, []int{5, 6, 7, 9, 12, 13}) {
		t.Fatalf("genuine continuation seqs = %v, want [5 6 7 9 12 13]", got)
	}
}

// TestResourceConditionUnsetMeansUnfiltered pins the compatibility rules:
// omitting the condition and explicitly passing "" are the same historical
// query, and a non-empty condition genuinely narrows it. Passing more than
// one condition is an ErrInvalidPage caller error.
func TestResourceConditionUnsetMeansUnfiltered(t *testing.T) {
	s := NewStore()
	resourceFixture(t, s)

	omitted := drainAudit(t, s, "acme", "", "")
	explicit := resourcePageWalk(t, s, "acme", 1, 4, "", "", "")
	if !reflect.DeepEqual(recordSeqs(explicit), recordSeqs(omitted)) {
		t.Fatalf("explicit empty resource = %v, want the unfiltered %v",
			recordSeqs(explicit), recordSeqs(omitted))
	}
	if len(omitted) != 13 {
		t.Fatalf("unfiltered chain = %d records, want all 13", len(omitted))
	}
	for _, r := range omitted {
		if r.Seq == 1 || r.Seq == 8 || r.Seq == 11 {
			if r.Kind != AuditPolicyChange {
				t.Fatalf("unfiltered query lost policy change at seq %d", r.Seq)
			}
		}
	}

	// More than one resource condition is rejected before any state is
	// touched and is classified with the other bad-argument errors.
	if _, err := s.AuditQuery("acme", 1, 2, "", "", ledgerA, ledgerB); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("two resource conditions err = %v, want ErrInvalidPage", err)
	}
	first, err := s.AuditQuery("acme", 1, 2, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuditPage(first.Checkpoint, first.Next, 2, "", "", ledgerA, ledgerB); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("two resource conditions on a page err = %v, want ErrInvalidPage", err)
	}
}

// TestResourceResultsAreDetachedCopies pins that records returned through
// the resource view share no storage with the internal history: mutating a
// returned request or decision must not alter what a later query returns.
func TestResourceResultsAreDetachedCopies(t *testing.T) {
	s := NewStore()
	resourceFixture(t, s)

	first, err := s.AuditQuery("acme", 1, 3, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) == 0 {
		t.Fatal("expected records")
	}
	first.Records[0].Decision.Request.Resource.ID = "mutated"
	first.Records[0].Decision.Decision.Reason = "mutated"
	first.Records[0].Org = "mutated"

	again, err := s.AuditQuery("acme", 1, 3, "", "", ledgerA)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Records[0].Decision.Request.Resource.ID; got != ledgerA {
		t.Fatalf("internal history mutated via returned copy: resource = %q", got)
	}
	if again.Records[0].Decision.Decision.Reason != "matched allow policy" {
		t.Fatalf("internal history mutated via returned copy: reason = %q",
			again.Records[0].Decision.Decision.Reason)
	}
	if again.Records[0].Org != "acme" {
		t.Fatalf("internal history mutated via returned copy: org = %q", again.Records[0].Org)
	}
}
