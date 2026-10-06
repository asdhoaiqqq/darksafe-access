// Regression coverage for resource-filtered audit pagination. An auditor
// paging the access history of ONE ledger (one resource identifier) sees
// that organization's chain interleaved with policy publishes, rollbacks,
// other resources' decisions and other subjects, so the ledger's own
// records may sit far apart. These tests pin the contract of the optional
// resource condition on the AuditQuery/AuditPage entry points:
//
//   - The resource filter selects exactly the decision records whose
//     request carried that resource identifier — allows and denials alike,
//     including denials caused by a disabled subject or an organization
//     mismatch — preserving original sequence, full request and the
//     complete decision explanation. Another resource in the same scope
//     never mixes in, and the same resource identifier in another
//     organization has no effect.
//   - The comparison is on the raw bytes of the identifier: case and
//     surrounding whitespace are significant, no path or wildcard
//     interpretation is applied, and Chinese, control characters and
//     invalid UTF-8 bytes each keep their distinct meaning — a lone
//     invalid byte is not the U+FFFD replacement character.
//   - The resource filter conjoins with the category and subject filters;
//     combined with kind == AuditPolicyChange it yields empty pages, never
//     the policy changes that happen to name the identifier.
//   - The page size bounds how many MATCHING records a page returns;
//     concatenating the pages along the returned cursors yields the
//     matching sequences in ascending order with no duplicates or gaps.
//     The first page's end sequence and fingerprint constrain every later
//     page: records appended after the first query never mix in, and only
//     a fresh query observes the new tail.
//   - A filter matching nothing returns an empty page that ends the walk,
//     never an error and never the unfiltered chain; a checkpoint whose
//     fingerprint was tampered with fails with ErrInvalidRange and
//     delivers no partial records. Omitting the resource argument — or
//     passing the empty string — leaves the historical behavior untouched.
//
// All of this is read-only: querying and paging never change the current
// policy version and never append audit records.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// resourcePageWalk pages a resource-filtered query to exhaustion, returning
// the concatenated records. It fails the test if a cursor repeats (the walk
// would never terminate) or if any page carries a checkpoint different from
// the first page's.
func resourcePageWalk(t *testing.T, s *Store, org string, startSeq, pageSize int, kind, subject, resource string) []AuditRecord {
	t.Helper()
	page, err := s.AuditQuery(org, startSeq, pageSize, kind, subject, resource)
	if err != nil {
		t.Fatalf("AuditQuery(%q, start %d, resource %q): %v", org, startSeq, resource, err)
	}
	pinned := page.Checkpoint
	var recs []AuditRecord
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
		page, err = s.AuditPage(pinned, page.Next, pageSize, kind, subject, resource)
		if err != nil {
			t.Fatalf("AuditPage(next %d): %v", page.Next, err)
		}
		if page.Checkpoint != pinned {
			t.Fatalf("page checkpoint = %+v, want the first page's %+v", page.Checkpoint, pinned)
		}
	}
	return recs
}

// requireResourceDecisions fails unless every record is a decision in the
// given organization whose request carried the given resource identifier.
func requireResourceDecisions(t *testing.T, org, resource string, recs []AuditRecord) {
	t.Helper()
	for _, r := range recs {
		if r.Org != org || r.Kind != AuditDecision || r.Decision == nil {
			t.Fatalf("resource filter leaked %+v", r)
		}
		if r.Decision.Request.Resource.ID != resource {
			t.Fatalf("record seq %d belongs to resource %q, want %q",
				r.Seq, r.Decision.Request.Resource.ID, resource)
		}
	}
}

// resourceFilterFixture builds one organization whose chain interleaves the
// target resource's allows and denials — including a disabled-subject
// denial and an organization-mismatch denial — with publishes, another
// resource's decisions in the SAME scope and other subjects' decisions:
//
//	seq 1  publish v1 (allow u1 read, deny u1 write)
//	seq 2  u1  read  ledger-a -> allow (v1)
//	seq 3  u1  read  ledger-b -> allow (v1)          same scope, other resource
//	seq 4  u1  write ledger-a -> matched-deny (v1)
//	seq 5  publish v2 (empty set)
//	seq 6  u2  read  ledger-a -> no-match denial (v2)
//	seq 7  u3  read  ledger-a -> disabled-subject denial
//	seq 8  u1  read  ledger-a -> organization-mismatch denial (SubjectOrg "other")
//	seq 9  u1  read  ledger-a -> no-match denial (v2)
//
// It returns the submitted requests and returned decisions of the target
// resource so tests can verify the preserved content.
func resourceFilterFixture(t *testing.T, s *Store) (reqs map[int]OrgRequest, decisions map[int]Decision) {
	t.Helper()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
		{ID: "p-deny", Subject: "u1", Action: "write", Scope: "org/a", Effect: EffectDeny},
	}); err != nil {
		t.Fatal(err)
	}
	reqs = map[int]OrgRequest{}
	decisions = map[int]Decision{}
	decide := func(seq int, req OrgRequest) {
		t.Helper()
		d := s.Decide("acme", req)
		reqs[seq] = req
		decisions[seq] = d
	}
	decide(2, request("acme", "u1", "ledger-a", "org/a", "read"))
	decide(3, request("acme", "u1", "ledger-b", "org/a", "read"))
	decide(4, request("acme", "u1", "ledger-a", "org/a", "write"))
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	decide(6, request("acme", "u2", "ledger-a", "org/a", "read"))
	disabled := request("acme", "u3", "ledger-a", "org/a", "read")
	disabled.Subject.Disabled = true
	decide(7, disabled)
	mismatch := request("acme", "u1", "ledger-a", "org/a", "read")
	mismatch.SubjectOrg = "other"
	decide(8, mismatch)
	decide(9, request("acme", "u1", "ledger-a", "org/a", "read"))
	if !decisions[2].Allowed || decisions[4].Allowed {
		t.Fatalf("fixture decisions broken: %+v", decisions)
	}
	for _, seq := range []int{6, 7, 8, 9} {
		if decisions[seq].Allowed {
			t.Fatalf("fixture seq %d should be a denial: %+v", seq, decisions[seq])
		}
	}
	return reqs, decisions
}

// TestResourceFilterSelectsOnlyThatResourcesDecisions pins the filter rule:
// from a chain mixing policy changes, another resource in the same scope
// and other subjects, the resource view returns exactly the target
// resource's decisions — every kind of denial included — with their
// original sequences, full requests and complete decision explanations. The
// same resource identifier in another organization neither leaks in nor is
// affected.
func TestResourceFilterSelectsOnlyThatResourcesDecisions(t *testing.T) {
	s := NewStore()
	reqs, decisions := resourceFilterFixture(t, s)

	// The same resource identifier actively deciding in another organization.
	if _, err := s.Publish("globex", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("globex", request("globex", "u1", "ledger-a", "org/a", "read"))
	s.Decide("globex", request("globex", "u1", "ledger-a", "org/a", "read"))

	recs := resourcePageWalk(t, s, "acme", 1, 2, "", "", "ledger-a")
	if got := recordSeqs(recs); !reflect.DeepEqual(got, []int{2, 4, 6, 7, 8, 9}) {
		t.Fatalf("ledger-a seqs = %v, want [2 4 6 7 8 9]", got)
	}
	requireResourceDecisions(t, "acme", "ledger-a", recs)
	// Original sequence, full request and complete decision explanation.
	for _, r := range recs {
		if !reflect.DeepEqual(r.Decision.Request, reqs[r.Seq]) {
			t.Fatalf("seq %d request = %+v, want %+v", r.Seq, r.Decision.Request, reqs[r.Seq])
		}
		if !reflect.DeepEqual(r.Decision.Decision, decisions[r.Seq]) {
			t.Fatalf("seq %d decision = %+v, want %+v", r.Seq, r.Decision.Decision, decisions[r.Seq])
		}
	}
	// Every outcome survives: the allow, the matched-deny, the no-match
	// denials, the disabled-subject denial and the mismatch denial.
	if !recs[0].Decision.Decision.Allowed {
		t.Fatalf("allow not preserved: %+v", recs[0].Decision.Decision)
	}
	wantReasons := []string{
		"matched deny policy", "no matching allow policy", "subject is disabled",
		"organization mismatch", "no matching allow policy",
	}
	for i, want := range wantReasons {
		if got := recs[i+1].Decision.Decision.Reason; got != want {
			t.Fatalf("denial %d reason = %q, want %q", i, got, want)
		}
	}

	// The other resource in the same scope has its own view.
	other := resourcePageWalk(t, s, "acme", 1, 4, "", "", "ledger-b")
	if got := recordSeqs(other); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("ledger-b seqs = %v, want [3]", got)
	}
	requireResourceDecisions(t, "acme", "ledger-b", other)

	// The other organization's identical resource identifier is invisible
	// here, and its own view holds only its own records.
	globex := resourcePageWalk(t, s, "globex", 1, 1, "", "", "ledger-a")
	if got := recordSeqs(globex); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("globex ledger-a seqs = %v, want [2 3]", got)
	}
	requireResourceDecisions(t, "globex", "ledger-a", globex)
	if globex[0].Fingerprint == recs[0].Fingerprint {
		t.Fatal("identical resource identifiers across organizations share a fingerprint")
	}
}

// TestResourceFilterConjoinsWithKindAndSubject pins that the resource
// condition narrows the category and subject conditions instead of
// replacing them, and that a resource filter never resurrects policy
// changes: filtering policy changes by a resource identifier that the
// published policies name yields empty pages, not the changes.
func TestResourceFilterConjoinsWithKindAndSubject(t *testing.T) {
	s := NewStore()
	resourceFilterFixture(t, s)

	// Subject + resource: only u1's decisions about ledger-a.
	recs := resourcePageWalk(t, s, "acme", 1, 2, "", "u1", "ledger-a")
	if got := recordSeqs(recs); !reflect.DeepEqual(got, []int{2, 4, 8, 9}) {
		t.Fatalf("u1+ledger-a seqs = %v, want [2 4 8 9]", got)
	}

	// Decision category + resource: identical to the resource-only view,
	// because every resource match is already a decision.
	recs = resourcePageWalk(t, s, "acme", 1, 3, AuditDecision, "", "ledger-a")
	if got := recordSeqs(recs); !reflect.DeepEqual(got, []int{2, 4, 6, 7, 8, 9}) {
		t.Fatalf("decision+ledger-a seqs = %v, want [2 4 6 7 8 9]", got)
	}

	// Policy-change category + resource: empty, even though the published
	// policies of seq 1 and the rollback-free history name the same scope.
	// The resource filter never returns policy changes.
	page, err := s.AuditQuery("acme", 1, 4, AuditPolicyChange, "", "ledger-a")
	if err != nil {
		t.Fatalf("AuditQuery(policy_change, ledger-a): %v", err)
	}
	if len(page.Records) != 0 || page.Next != 0 {
		t.Fatalf("policy_change+resource page = %+v, want empty with next 0", page)
	}
}

// TestResourceFilterPinsRangeAgainstLaterAppends pins that the first
// query's checkpoint constrains every later page: records appended after
// the first query — even this resource's own new decisions — never mix
// into the pinned walk, and only a fresh query observes the new tail.
func TestResourceFilterPinsRangeAgainstLaterAppends(t *testing.T) {
	s := NewStore()
	resourceFilterFixture(t, s)

	first, err := s.AuditQuery("acme", 1, 2, "", "", "ledger-a")
	if err != nil {
		t.Fatalf("AuditQuery: %v", err)
	}
	pinned := first.Checkpoint

	// The chain grows AFTER the first page pinned its checkpoint: one more
	// decision about the same resource, and one more policy change.
	s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read")) // seq 10
	if _, err := s.Publish("acme", 2, nil); err != nil {                 // seq 11
		t.Fatal(err)
	}

	walked := recordSeqs(first.Records)
	page := first
	for page.Next != 0 {
		page, err = s.AuditPage(pinned, page.Next, 2, "", "", "ledger-a")
		if err != nil {
			t.Fatalf("AuditPage(next %d): %v", page.Next, err)
		}
		if page.Checkpoint != pinned {
			t.Fatalf("checkpoint drifted: %+v, want %+v", page.Checkpoint, pinned)
		}
		walked = append(walked, recordSeqs(page.Records)...)
	}
	if !reflect.DeepEqual(walked, []int{2, 4, 6, 7, 8, 9}) {
		t.Fatalf("pinned walk seqs = %v, want [2 4 6 7 8 9]", walked)
	}

	// Only a fresh query pins the new head and sees the appended record.
	fresh := resourcePageWalk(t, s, "acme", 1, 2, "", "", "ledger-a")
	if got := recordSeqs(fresh); !reflect.DeepEqual(got, []int{2, 4, 6, 7, 8, 9, 10}) {
		t.Fatalf("fresh walk seqs = %v, want [2 4 6 7 8 9 10]", got)
	}
}

// TestResourceFilterBoundaries pins the quiet ends of the view: a resource
// with no decisions yields an empty page that ends the walk (never the
// unfiltered history), a tampered checkpoint fingerprint fails with
// ErrInvalidRange and delivers no partial records, and an omitted or empty
// resource condition leaves the historical unfiltered behavior untouched.
func TestResourceFilterBoundaries(t *testing.T) {
	s := NewStore()
	resourceFilterFixture(t, s)

	// No-match resource: empty page, next 0, not the full chain.
	page, err := s.AuditQuery("acme", 1, 4, "", "", "ledger-ghost")
	if err != nil {
		t.Fatalf("AuditQuery(ghost): %v", err)
	}
	if len(page.Records) != 0 || page.Next != 0 {
		t.Fatalf("ghost page = %+v, want empty with next 0", page)
	}

	// Tampered checkpoint fingerprint: range error, no partial records.
	first, err := s.AuditQuery("acme", 1, 1, "", "", "ledger-a")
	if err != nil {
		t.Fatalf("AuditQuery: %v", err)
	}
	forged := first.Checkpoint
	forged.Fingerprint = "00" + forged.Fingerprint[2:]
	page, err = s.AuditPage(forged, first.Next, 1, "", "", "ledger-a")
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tampered checkpoint: err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		t.Fatalf("tampered checkpoint delivered partial page %+v", page)
	}

	// More than one resource argument is a caller error.
	if _, err := s.AuditQuery("acme", 1, 1, "", "", "ledger-a", "ledger-b"); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("two resource filters: err = %v, want ErrInvalidPage", err)
	}
	if _, err := s.AuditPage(first.Checkpoint, first.Next, 1, "", "", "ledger-a", "ledger-b"); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("two resource filters on AuditPage: err = %v, want ErrInvalidPage", err)
	}

	// Omitting the resource argument, or passing the empty string, returns
	// the same records as the historical unfiltered query.
	omit := resourcePageWalk(t, s, "acme", 1, 3, "", "", "")
	page2, err := s.AuditQuery("acme", 1, 3, "", "")
	if err != nil {
		t.Fatalf("AuditQuery(no resource): %v", err)
	}
	var legacy []AuditRecord
	for {
		legacy = append(legacy, page2.Records...)
		if page2.Next == 0 {
			break
		}
		page2, err = s.AuditPage(page2.Checkpoint, page2.Next, 3, "", "")
		if err != nil {
			t.Fatalf("AuditPage(no resource): %v", err)
		}
	}
	if got, want := recordSeqs(omit), recordSeqs(legacy); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty resource filter seqs = %v, want the unfiltered %v", got, want)
	}
	if len(legacy) != 9 {
		t.Fatalf("unfiltered walk returned %d records, want all 9", len(legacy))
	}
}

// TestResourceFilterRawBytes pins the comparison rule: the resource
// condition matches the identifier's raw bytes exactly. Case and
// surrounding whitespace are significant, no path or wildcard
// interpretation is applied, and Chinese, control characters and invalid
// UTF-8 bytes each keep their distinct meaning — in particular a lone
// invalid byte is not the U+FFFD replacement character.
func TestResourceFilterRawBytes(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
	}); err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"Ledger-A",    // differs from "ledger-a" only in case
		"ledger-a ",   // trailing space
		" ledger-a",   // leading space
		"账本",          // non-ASCII
		"ledger\ta",   // control character
		"ledger\xffa", // invalid UTF-8 byte
		"ledger�a",    // the genuine replacement character
		"ledger-a",    // the baseline
		"ledger-*",    // a would-be wildcard, matched literally
	}
	for _, id := range ids {
		s.Decide("acme", request("acme", "u1", id, "org/a", "read"))
	}

	// Every identifier selects exactly its own record.
	for i, id := range ids {
		recs := resourcePageWalk(t, s, "acme", 1, 4, "", "", id)
		if got := recordSeqs(recs); !reflect.DeepEqual(got, []int{i + 2}) {
			t.Fatalf("resource %q seqs = %v, want [%d]", id, got, i+2)
		}
		requireResourceDecisions(t, "acme", id, recs)
	}

	// No path or wildcard interpretation: "ledger-*" does not reach
	// "ledger-a", and "ledger" alone matches nothing.
	if recs := resourcePageWalk(t, s, "acme", 1, 4, "", "", "ledger-*"); len(recs) != 1 ||
		recs[0].Decision.Request.Resource.ID != "ledger-*" {
		t.Fatalf("wildcard-like identifier did not match literally: %+v", recs)
	}
	if recs := resourcePageWalk(t, s, "acme", 1, 4, "", "", "ledger"); len(recs) != 0 {
		t.Fatalf("prefix query leaked %+v", recs)
	}

	// The lone invalid byte and the genuine replacement character are
	// distinct identifiers: neither query returns the other's record.
	bad := resourcePageWalk(t, s, "acme", 1, 4, "", "", "ledger\xffa")
	if len(bad) != 1 || bad[0].Decision.Request.Resource.ID != "ledger\xffa" {
		t.Fatalf("invalid-byte query = %+v, want only the 0xFF record", bad)
	}
	repl := resourcePageWalk(t, s, "acme", 1, 4, "", "", "ledger�a")
	if len(repl) != 1 || repl[0].Decision.Request.Resource.ID != "ledger�a" {
		t.Fatalf("replacement-character query = %+v, want only the U+FFFD record", repl)
	}
	if bad[0].Seq == repl[0].Seq {
		t.Fatal("invalid byte and replacement character collapsed onto one identifier")
	}
}

// TestResourceFilterIsReadOnly pins that resource-filtered querying and
// paging never change the current policy version and never append audit
// records.
func TestResourceFilterIsReadOnly(t *testing.T) {
	s := NewStore()
	resourceFilterFixture(t, s)
	before, cpBefore, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	versionBefore := s.CurrentVersion("acme")

	resourcePageWalk(t, s, "acme", 1, 2, "", "", "ledger-a")
	resourcePageWalk(t, s, "acme", 1, 2, AuditDecision, "u1", "ledger-a")
	resourcePageWalk(t, s, "acme", 1, 2, AuditPolicyChange, "", "ledger-a")

	after, cpAfter, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.CurrentVersion("acme") != versionBefore {
		t.Fatalf("querying changed the version: %d -> %d", versionBefore, s.CurrentVersion("acme"))
	}
	if cpBefore != cpAfter || len(after) != len(before) {
		t.Fatalf("querying mutated the chain: %d -> %d records", len(before), len(after))
	}
	if err := VerifyAudit("acme", after, cpAfter); err != nil {
		t.Fatalf("chain no longer verifies after querying: %v", err)
	}
}
