package darksafe

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// drainAudit collects every record of an organization using the public
// pinned-query API.
func drainAudit(t *testing.T, s *Store, org, kind, subject string) []AuditRecord {
	t.Helper()
	page, err := s.AuditQuery(org, 1, 4, kind, subject)
	if err != nil {
		t.Fatalf("AuditQuery: %v", err)
	}
	var out []AuditRecord
	for {
		out = append(out, page.Records...)
		if page.Next == 0 {
			break
		}
		page, err = s.AuditPage(page.Checkpoint, page.Next, 4, kind, subject)
		if err != nil {
			t.Fatalf("AuditPage: %v", err)
		}
	}
	return out
}

func TestPublishLeavesPolicyChangeRecord(t *testing.T) {
	s := NewStore()
	policies := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}
	v, err := s.Publish("acme", 0, policies)
	if err != nil {
		t.Fatal(err)
	}
	recs := drainAudit(t, s, "acme", "", "")
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Org != "acme" || r.Seq != 1 || r.Kind != AuditPolicyChange {
		t.Fatalf("bad record header: %+v", r)
	}
	if r.Change == nil || r.Change.Version != v || r.Change.RolledBack || r.Change.SourceVersion != 0 {
		t.Fatalf("bad change payload: %+v", r.Change)
	}
	if !reflect.DeepEqual(r.Change.Policies, policies) {
		t.Fatalf("stored policies = %+v, want %+v", r.Change.Policies, policies)
	}
	if r.PrevFingerprint == "" || r.Fingerprint == "" || r.PrevFingerprint == r.Fingerprint {
		t.Fatalf("fingerprints not populated: %+v", r)
	}
}

func TestRollbackRecordNamesSourceVersion(t *testing.T) {
	s := NewStore()
	v1, _ := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	if _, err := s.Publish("acme", v1, nil); err != nil {
		t.Fatal(err)
	}
	v3, err := s.Rollback("acme", 2, v1)
	if err != nil {
		t.Fatal(err)
	}
	recs := drainAudit(t, s, "acme", AuditPolicyChange, "")
	if len(recs) != 3 {
		t.Fatalf("change records = %d, want 3", len(recs))
	}
	rb := recs[2]
	if !rb.Change.RolledBack || rb.Change.SourceVersion != v1 || rb.Change.Version != v3 {
		t.Fatalf("rollback payload = %+v", rb.Change)
	}
	if len(rb.Change.Policies) != 1 || rb.Change.Policies[0].ID != "p1" {
		t.Fatalf("rollback must carry full new content: %+v", rb.Change.Policies)
	}
}

func TestFailedPublishAndRollbackLeaveNoRecord(t *testing.T) {
	s := NewStore()
	v1, _ := s.Publish("acme", 0, nil)
	if _, err := s.Publish("acme", v1, []Policy{{ID: "", Effect: EffectAllow}}); !errors.Is(err, ErrInvalidPolicySet) {
		t.Fatalf("invalid publish err = %v", err)
	}
	if _, err := s.Publish("acme", 0, nil); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale publish err = %v", err)
	}
	if _, err := s.Rollback("acme", v1, 99); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("bad rollback err = %v", err)
	}
	if _, err := s.Rollback("acme", 0, v1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale rollback err = %v", err)
	}
	recs := drainAudit(t, s, "acme", "", "")
	if len(recs) != 1 || recs[0].Change.Version != 1 {
		t.Fatalf("records after failures = %+v, want only version 1", recs)
	}
}

func TestDecideRecordsEveryOutcome(t *testing.T) {
	s := NewStore()
	// No published version yet: the denial is still recorded.
	d0 := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if d0.Allowed || d0.Version != 0 {
		t.Fatalf("unexpected decision %+v", d0)
	}
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
		{ID: "p-deny", Subject: "u2", Action: "read", Scope: "org/a", Effect: EffectDeny},
	}); err != nil {
		t.Fatal(err)
	}
	allow := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	deny := s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))
	noMatch := s.Decide("acme", request("acme", "u9", "r1", "org/a", "read"))
	disabled := request("acme", "u1", "r1", "org/a", "read")
	disabled.Subject.Disabled = true
	ddis := s.Decide("acme", disabled)
	mismatch := request("acme", "u1", "r1", "org/a", "read")
	mismatch.SubjectOrg = "globex"
	dmis := s.Decide("acme", mismatch)
	missing := request("acme", "u1", "r1", "org/a", "read")
	missing.Resource.ID = ""
	dmiss := s.Decide("acme", missing)

	recs := drainAudit(t, s, "acme", AuditDecision, "")
	want := []Decision{d0, allow, deny, noMatch, ddis, dmis, dmiss}
	if len(recs) != len(want) {
		t.Fatalf("decision records = %d, want %d", len(recs), len(want))
	}
	for i, r := range recs {
		if r.Kind != AuditDecision || r.Decision == nil {
			t.Fatalf("record %d not a decision: %+v", i, r)
		}
		if !reflect.DeepEqual(r.Decision.Decision, want[i]) {
			t.Fatalf("record %d decision = %+v, want %+v", i, r.Decision.Decision, want[i])
		}
	}
	// The full request is preserved, including the envelope rejection cases.
	if recs[4].Decision.Request.Subject.ID != "u1" || !recs[4].Decision.Request.Subject.Disabled {
		t.Fatalf("disabled request not stored: %+v", recs[4].Decision.Request)
	}
	if recs[5].Decision.Request.SubjectOrg != "globex" {
		t.Fatalf("mismatched org request not stored: %+v", recs[5].Decision.Request)
	}
	// Sequences ascend and match the underlying chain positions (the
	// publish sits at seq 2 and is filtered out of this view).
	wantSeqs := []int{1, 3, 4, 5, 6, 7, 8}
	gotSeqs := make([]int, len(recs))
	for i, r := range recs {
		gotSeqs[i] = r.Seq
	}
	if !reflect.DeepEqual(gotSeqs, wantSeqs) {
		t.Fatalf("decision seqs = %v, want %v", gotSeqs, wantSeqs)
	}
}

func TestDecideMissingOrgLeavesNoRecordAnywhere(t *testing.T) {
	s := NewStore()
	d := s.Decide("", request("acme", "u1", "r1", "org/a", "read"))
	if d.Allowed || d.Reason != "missing decision organization" {
		t.Fatalf("missing-org decision = %+v", d)
	}
	if _, _, err := s.AuditExport("", 0); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("export without org err = %v", err)
	}
	if _, err := s.AuditQuery("", 1, 1, "", ""); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("query without org err = %v", err)
	}
	if _, err := s.RecheckDecision("", 1); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("recheck without org err = %v", err)
	}
	if _, err := s.AuditPage(Checkpoint{}, 1, 1, "", ""); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("page without org err = %v", err)
	}
}

func TestReviewIsReadOnlyAndRepeatDecidesAppend(t *testing.T) {
	s := NewStore()
	v1, _ := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	req := request("acme", "u1", "r1", "org/a", "read")
	s.Decide("acme", req)
	before := len(drainAudit(t, s, "acme", "", ""))
	// Review, including failing review, must not write anything.
	if d := s.Review("acme", v1, req); !d.Allowed {
		t.Fatalf("review: %+v", d)
	}
	if d := s.Review("acme", 99, req); d.Allowed {
		t.Fatalf("review of missing version: %+v", d)
	}
	if got := len(drainAudit(t, s, "acme", "", "")); got != before {
		t.Fatalf("review wrote records: %d -> %d", before, got)
	}
	// The identical request decided again appends a new record.
	s.Decide("acme", req)
	s.Decide("acme", req)
	decisions := drainAudit(t, s, "acme", AuditDecision, "")
	if len(decisions) != 3 {
		t.Fatalf("repeat decisions = %d, want 3", len(decisions))
	}
	if decisions[0].Seq == decisions[1].Seq || decisions[1].Seq == decisions[2].Seq {
		t.Fatalf("repeat decisions share a sequence: %+v", decisions)
	}
}

func TestFingerprintsChainGaplessly(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, nil)
	s.Rollback("acme", 2, 1)
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("chain verification: %v", err)
	}
	if cp.EndSeq != 5 {
		t.Fatalf("checkpoint end = %d, want 5", cp.EndSeq)
	}
}

func TestEmptyOrganizationExportsGenesis(t *testing.T) {
	s := NewStore()
	page, err := s.AuditQuery("acme", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.Checkpoint.EndSeq != 0 || page.Next != 0 {
		t.Fatalf("empty page = %+v", page)
	}
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 || cp.EndSeq != 0 {
		t.Fatalf("empty export = %+v, %+v", recs, cp)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("empty export must verify: %v", err)
	}
	if cp.Fingerprint != genesisFingerprint("acme") {
		t.Fatal("empty checkpoint is not the genesis fingerprint")
	}
}

func TestAuditQueryFiltersAndPaging(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, nil)                                      // seq 1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 2
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read")) // seq 3
	s.Publish("acme", 1, nil)                                      // seq 4
	s.Decide("acme", request("acme", "u1", "r1", "org/b", "read")) // seq 5

	// Category filter.
	changes := drainAudit(t, s, "acme", AuditPolicyChange, "")
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(changes))
	}
	// Subject filter returns that subject's decisions only, never changes.
	u1 := drainAudit(t, s, "acme", "", "u1")
	if len(u1) != 2 {
		t.Fatalf("u1 records = %d, want 2", len(u1))
	}
	for _, r := range u1 {
		if r.Kind != AuditDecision || r.Decision.Request.Subject.ID != "u1" {
			t.Fatalf("subject filter leaked %+v", r)
		}
	}
	u2 := drainAudit(t, s, "acme", AuditDecision, "u2")
	if len(u2) != 1 || u2[0].Seq != 3 {
		t.Fatalf("u2 records = %+v, want seq 3", u2)
	}
	if none := drainAudit(t, s, "acme", "", "ghost"); len(none) != 0 {
		t.Fatalf("unknown subject = %+v, want empty", none)
	}

	// Ascending order across pages, page size 2.
	page, err := s.AuditQuery("acme", 1, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cp := page.Checkpoint
	var seqs []int
	seqs = append(seqs, page.Records[0].Seq, page.Records[1].Seq)
	for page.Next != 0 {
		page, err = s.AuditPage(cp, page.Next, 2, "", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Records {
			seqs = append(seqs, r.Seq)
		}
	}
	if !reflect.DeepEqual(seqs, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("paged seqs = %v, want 1..5", seqs)
	}
}

func TestAuditQueryPinsRangeAgainstLaterAppends(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, nil)
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	first, err := s.AuditQuery("acme", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Checkpoint.EndSeq != 2 {
		t.Fatalf("initial end = %d, want 2", first.Checkpoint.EndSeq)
	}
	// Records appended after the first query must not mix into its pages.
	s.Publish("acme", 1, nil)
	s.Decide("acme", request("acme", "u9", "r1", "org/a", "read"))
	second, err := s.AuditPage(first.Checkpoint, first.Next, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int
	seqs = append(seqs, first.Records[0].Seq)
	for _, r := range second.Records {
		seqs = append(seqs, r.Seq)
	}
	if !reflect.DeepEqual(seqs, []int{1, 2}) {
		t.Fatalf("later records leaked into pinned query: %v", seqs)
	}
	if second.EndSeq != 2 {
		t.Fatalf("page end = %d, want pinned 2", second.EndSeq)
	}
	// A fresh query sees the new tail.
	fresh, err := s.AuditQuery("acme", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Checkpoint.EndSeq != 4 || len(fresh.Records) != 4 {
		t.Fatalf("fresh query = %+v, want 4 records ending at 4", fresh)
	}
}

func TestAuditQueryValidationErrors(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, nil)
	if _, err := s.AuditQuery("acme", 1, 0, "", ""); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("zero page size err = %v", err)
	}
	if _, err := s.AuditQuery("acme", 1, -3, "", ""); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("negative page size err = %v", err)
	}
	if _, err := s.AuditQuery("acme", 0, 1, "", ""); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("zero start err = %v", err)
	}
	if _, err := s.AuditQuery("acme", 1, 1, "bogus", ""); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("bad category err = %v", err)
	}
	if _, _, err := s.AuditExport("acme", 99); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("export beyond head err = %v", err)
	}
	// A checkpoint past the current head is an error, not a silent cap.
	bad := Checkpoint{Org: "acme", EndSeq: 50, Fingerprint: "deadbeef"}
	if _, err := s.AuditPage(bad, 1, 1, "", ""); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("checkpoint beyond head err = %v", err)
	}
	// A checkpoint with a wrong fingerprint is an error.
	good, _ := s.AuditQuery("acme", 1, 1, "", "")
	forged := good.Checkpoint
	forged.Fingerprint = "00"
	if _, err := s.AuditPage(forged, 1, 1, "", ""); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("forged fingerprint err = %v", err)
	}
}

func TestReturnedAndSubmittedDataIsDetached(t *testing.T) {
	s := NewStore()
	req := request("acme", "u1", "r1", "org/a", "read")
	policies := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}
	s.Publish("acme", 0, policies)
	s.Decide("acme", req)

	// Mutate what the caller kept after submission.
	policies[0].Effect = EffectDeny
	policies[0].Subject = "hacker"
	req.Subject.ID = "hacker"
	req.Action = "write"

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("caller mutation reached the chain: %v", err)
	}
	if recs[0].Change.Policies[0].Subject != "u1" {
		t.Fatalf("stored policy mutated by caller: %+v", recs[0].Change.Policies[0])
	}
	if recs[1].Decision.Request.Subject.ID != "u1" || recs[1].Decision.Request.Action != "read" {
		t.Fatalf("stored request mutated by caller: %+v", recs[1].Decision.Request)
	}

	// Mutating query/export results must not change stored records.
	recs[0].Change.Policies[0].Subject = "hacker"
	recs[1].Decision.Request.Subject.ID = "hacker"
	again, _, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Change.Policies[0].Subject != "u1" || again[1].Decision.Request.Subject.ID != "u1" {
		t.Fatal("stored records changed via exported copies")
	}
	page, err := s.AuditQuery("acme", 1, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	page.Records[0].Change.Policies[0].ID = "rewritten"
	onceMore, _, _ := s.AuditExport("acme", 0)
	if onceMore[0].Change.Policies[0].ID != "p1" {
		t.Fatal("stored records changed via query copies")
	}
}

func TestVerifyAuditDetectsTampering(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, nil)
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))

	good, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, mutate func([]AuditRecord) []AuditRecord, checkpoint Checkpoint) {
		t.Helper()
		recs := append([]AuditRecord(nil), good...)
		recs = mutate(recs)
		if err := VerifyAudit("acme", recs, checkpoint); err == nil {
			t.Fatalf("%s: tampered records verified", name)
		}
	}

	// Modify a field deep inside a payload.
	check("field change", func(r []AuditRecord) []AuditRecord {
		r[1].Decision.Decision.Allowed = !r[1].Decision.Decision.Allowed
		return r
	}, cp)
	check("policy change", func(r []AuditRecord) []AuditRecord {
		r[0].Change.Policies[0].Effect = EffectDeny
		return r
	}, cp)
	// Delete a middle record, keep the tail.
	check("delete middle", func(r []AuditRecord) []AuditRecord {
		return []AuditRecord{r[0], r[2], r[3]}
	}, cp)
	// Delete the tail while still claiming the original checkpoint.
	check("delete tail", func(r []AuditRecord) []AuditRecord { return r[:2] }, cp)
	// A genuine prefix export with its matching checkpoint verifies.
	prefix, prefixCP, err := s.AuditExport("acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", prefix, prefixCP); err != nil {
		t.Fatalf("prefix export must verify: %v", err)
	}
	// Swap order.
	check("swap order", func(r []AuditRecord) []AuditRecord {
		r[0], r[1] = r[1], r[0]
		return r
	}, cp)
	// Claim a foreign checkpoint fingerprint.
	check("foreign checkpoint", func(r []AuditRecord) []AuditRecord { return r },
		Checkpoint{Org: "acme", EndSeq: 4, Fingerprint: genesisFingerprint("globex")})
	// Wrong end sequence in the checkpoint.
	check("wrong length", func(r []AuditRecord) []AuditRecord { return r[:3] }, cp)
}

func TestOrganizationsChainsAreIndependent(t *testing.T) {
	s := NewStore()
	// Identical policy and subject identifiers in two organizations.
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("globex", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))

	a, acp, _ := s.AuditExport("acme", 0)
	g, gcp, _ := s.AuditExport("globex", 0)
	if err := VerifyAudit("acme", a, acp); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("globex", g, gcp); err != nil {
		t.Fatal(err)
	}
	// Same sequence numbers, different fingerprints.
	if a[0].Fingerprint == g[0].Fingerprint || a[1].Fingerprint == g[1].Fingerprint {
		t.Fatal("organizations share fingerprints despite org-bound genesis")
	}
	// A globex record spliced into the acme chain must fail.
	spliced := append([]AuditRecord(nil), a[0], g[1])
	if err := VerifyAudit("acme", spliced, Checkpoint{
		Org: "acme", EndSeq: 2, Fingerprint: spliced[1].Fingerprint,
	}); err == nil {
		t.Fatal("foreign-organization record verified in the acme chain")
	}
	// Whole globex export cannot verify under the acme name.
	if err := VerifyAudit("acme", g, gcp); err == nil {
		t.Fatal("globex export verified as acme")
	}
}

func TestRecheckDecisionReplaysRecordedVersion(t *testing.T) {
	s := NewStore()
	v1, _ := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	req := request("acme", "u1", "r1", "org/a", "read")
	original := s.Decide("acme", req) // seq 2, version 1, allowed
	deniedReq := request("acme", "u2", "r1", "org/a", "read")
	origDeny := s.Decide("acme", deniedReq) // seq 3, denied by default

	// Later publish removes the access, then a rollback happens.
	s.Publish("acme", 1, nil)
	s.Rollback("acme", 2, 1)
	// Current version is 3. A fresh decide for u2 would still deny; u1 allowed.
	// Rechecking seq 2 must reproduce the version-1 allow.
	got, err := s.RecheckDecision("acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original) || got.Version != v1 {
		t.Fatalf("recheck = %+v, want original %+v (version %d)", got, original, v1)
	}
	gotDeny, err := s.RecheckDecision("acme", 3)
	if err != nil || !reflect.DeepEqual(gotDeny, origDeny) {
		t.Fatalf("recheck deny = %+v, %v, want %+v", gotDeny, err, origDeny)
	}

	// A no-published-version denial rechecks as the same denial.
	s.Decide("blank", request("blank", "u1", "r1", "org/a", "read"))
	if d, err := s.RecheckDecision("blank", 1); err != nil || d.Allowed ||
		d.Reason != "organization has no published version" {
		t.Fatalf("recheck no-version denial = %+v, %v", d, err)
	}
	// An envelope rejection rechecks through the same envelope checks.
	mismatch := request("acme", "u1", "r1", "org/a", "read")
	mismatch.SubjectOrg = "globex"
	s.Decide("acme", mismatch) // publish=seq1, decides=2,3,6; publish=4; rollback=5
	if d, err := s.RecheckDecision("acme", 6); err != nil || d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("recheck mismatch = %+v, %v", d, err)
	}

	// Distinguishable errors.
	if _, err := s.RecheckDecision("acme", 1); !errors.Is(err, ErrAuditNotADecision) {
		t.Fatalf("recheck policy_change err = %v", err)
	}
	if _, err := s.RecheckDecision("acme", 99); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("recheck missing seq err = %v", err)
	}
	if _, err := s.RecheckDecision("ghost", 1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("recheck unknown org err = %v", err)
	}
}

func TestConcurrentOperationsKeepChainConsistent(t *testing.T) {
	s := NewStore()
	const publishers = 8
	const deciders = 8
	const perDecider = 25
	var wg sync.WaitGroup
	var successfulChanges int64
	var decides int64

	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 6; j++ {
				expected := s.CurrentVersion("acme")
				pol := []Policy{{
					ID:      fmt.Sprintf("p-%d-%d", id, j),
					Subject: fmt.Sprintf("u%d", id),
					Action:  "read",
					Scope:   "org/a",
					Effect:  EffectAllow,
				}}
				if v, err := s.Publish("acme", expected, pol); err == nil {
					atomic.AddInt64(&successfulChanges, 1)
					_ = v
					continue
				}
				// On conflict, attempt a rollback to a known version.
				if v, err := s.Rollback("acme", s.CurrentVersion("acme"), 1); err == nil {
					atomic.AddInt64(&successfulChanges, 1)
					_ = v
				}
			}
		}(i)
	}
	for i := 0; i < deciders; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perDecider; j++ {
				s.Decide("acme", request("acme", fmt.Sprintf("u%d", id), "r1", "org/a", "read"))
				atomic.AddInt64(&decides, 1)
			}
		}(i)
	}
	wg.Wait()

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	// No missing record: one entry per successful change plus every Decide.
	want := int(successfulChanges) + int(decides)
	if len(recs) != want {
		t.Fatalf("records = %d, want %d (changes %d, decides %d)", len(recs), want, successfulChanges, decides)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("chain invalid under concurrency: %v", err)
	}
	if int64(s.CurrentVersion("acme")) != successfulChanges {
		t.Fatalf("version %d != successful changes %d", s.CurrentVersion("acme"), successfulChanges)
	}

	// No duplicate sequences and every decision matches its recorded version.
	seen := map[int]bool{}
	changesAtVersion := map[int]*AuditRecord{}
	for i := range recs {
		r := &recs[i]
		if r.Seq != i+1 {
			t.Fatalf("gap/dup in sequence at index %d: seq %d", i, r.Seq)
		}
		if seen[r.Seq] {
			t.Fatalf("duplicate sequence %d", r.Seq)
		}
		seen[r.Seq] = true
		if r.Kind == AuditPolicyChange {
			changesAtVersion[r.Change.Version] = r
		}
	}
	for i := range recs {
		r := &recs[i]
		if r.Kind != AuditDecision || r.Decision.Decision.Version == 0 {
			continue
		}
		v := r.Decision.Decision.Version
		ch, ok := changesAtVersion[v]
		if !ok {
			t.Fatalf("decision at seq %d used version %d with no change record", r.Seq, v)
		}
		// The decision must be exactly what that version's policies produce.
		got := evaluate(r.Decision.Request, ch.Change.Policies, v)
		if !reflect.DeepEqual(got, r.Decision.Decision) {
			t.Fatalf("seq %d decision %+v does not match version %d evaluation %+v",
				r.Seq, r.Decision.Decision, v, got)
		}
	}
}
