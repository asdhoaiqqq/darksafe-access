package darksafe

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// auditTail returns the current number of audit records for org.
func auditTail(t *testing.T, s *Store, org string) int {
	t.Helper()
	page, err := s.AuditQuery(org, AuditQuery{PageSize: 1})
	if err != nil {
		t.Fatalf("audit tail: %v", err)
	}
	return page.UpToSeq
}

// allRecords exports every audit record of org in sequence order.
func allRecords(t *testing.T, s *Store, org string) []AuditRecord {
	t.Helper()
	n := auditTail(t, s, org)
	exp, err := s.AuditExport(org, n)
	if err != nil {
		t.Fatalf("audit export: %v", err)
	}
	return exp.Records
}

func findRecords(records []AuditRecord, cat AuditCategory) []AuditRecord {
	var out []AuditRecord
	for _, r := range records {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out
}

func TestAuditPublishAndRollbackRecords(t *testing.T) {
	s := NewStore()
	policies := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}

	v1, err := s.Publish("acme", 0, policies)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish("acme", v1, nil); err != nil {
		t.Fatal(err)
	}
	v3, err := s.Rollback("acme", 2, v1)
	if err != nil {
		t.Fatal(err)
	}

	records := allRecords(t, s, "acme")
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	for i, r := range records {
		if r.Seq != i+1 {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
		if r.Org != "acme" {
			t.Fatalf("record %d org = %q", i, r.Org)
		}
		if r.Category != AuditPolicyChange {
			t.Fatalf("record %d category = %q", i, r.Category)
		}
	}
	if records[0].Version != 1 || records[0].SourceVersion != 0 {
		t.Fatalf("publish record = %+v", records[0])
	}
	if !reflect.DeepEqual(records[0].Policies, policies) {
		t.Fatalf("publish policies = %v, want %v", records[0].Policies, policies)
	}
	if records[1].Version != 2 || len(records[1].Policies) != 0 {
		t.Fatalf("second publish record = %+v", records[1])
	}
	if records[2].Version != 3 || records[2].SourceVersion != 1 {
		t.Fatalf("rollback record = %+v", records[2])
	}
	if !reflect.DeepEqual(records[2].Policies, records[0].Policies) {
		t.Fatalf("rollback policies = %v, want %v", records[2].Policies, records[0].Policies)
	}
	_ = v3
}

func TestAuditFailedOperationsRecordNothing(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	want := allRecords(t, s, "acme")

	// Conflict on publish.
	if _, err := s.Publish("acme", 0, nil); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale publish err = %v", err)
	}
	// Invalid policies.
	if _, err := s.Publish("acme", 2, []Policy{{ID: ""}}); !errors.Is(err, ErrInvalidPolicySet) {
		t.Fatalf("invalid publish err = %v", err)
	}
	// Rollback to a missing version.
	if _, err := s.Rollback("acme", 2, 99); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("rollback to missing version err = %v", err)
	}
	// Rollback conflict.
	if _, err := s.Rollback("acme", 0, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale rollback err = %v", err)
	}

	got := allRecords(t, s, "acme")
	if len(got) != len(want) {
		t.Fatalf("failed operations added records: got %d, want %d", len(got), len(want))
	}
	if s.CurrentVersion("acme") != 2 {
		t.Fatalf("current version = %d, want 2", s.CurrentVersion("acme"))
	}
}

func TestAuditDecisionRecords(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		allowPolicy("p1", "u1", "read", "org/a", false),
		{ID: "p2", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		req     OrgRequest
		allowed bool
		reason  string
		version int
	}{
		{"deny overrides allow", request("acme", "u1", "r1", "org/a", "read"), false, "matched deny policy", 1},
		{"no matching policy", request("acme", "u2", "r1", "org/a", "read"), false, "no matching allow policy", 1},
		{"disabled subject", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.Subject.Disabled = true
			return r
		}(), false, "subject is disabled", 0},
		{"missing subject id", func() OrgRequest { r := request("acme", "u1", "r1", "org/a", "read"); r.Subject.ID = ""; return r }(), false, "missing subject id", 0},
		{"missing action", func() OrgRequest { r := request("acme", "u1", "r1", "org/a", "read"); r.Action = ""; return r }(), false, "missing action", 0},
		{"org mismatch", func() OrgRequest { r := request("acme", "u1", "r1", "org/a", "read"); r.SubjectOrg = "other"; return r }(), false, "organization mismatch", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := s.Decide("acme", tc.req)
			if d.Allowed != tc.allowed || d.Reason != tc.reason || d.Version != tc.version {
				t.Fatalf("decision = %+v, want allowed=%v reason=%q version=%d", d, tc.allowed, tc.reason, tc.version)
			}
		})
	}

	// Allow case: drop the deny policy and decide again.
	s2 := NewStore()
	if _, err := s2.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	d := s2.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if !d.Allowed || d.Version != 1 {
		t.Fatalf("allow decision = %+v", d)
	}
	records := allRecords(t, s2, "acme")
	if len(records) != 2 || records[0].Category != AuditPolicyChange || records[1].Category != AuditDecision {
		t.Fatalf("records = %+v", records)
	}
	r := records[1]
	if !r.Decision.Allowed || r.Decision.Reason != "matched allow policy" || r.Decision.Version != 1 {
		t.Fatalf("stored decision = %+v", r.Decision)
	}
	if !reflect.DeepEqual(r.Request, request("acme", "u1", "r1", "org/a", "read")) {
		t.Fatalf("stored request = %+v", r.Request)
	}
	if !reflect.DeepEqual(r.Decision.Matched, []string{"p1"}) {
		t.Fatalf("stored matched = %v", r.Decision.Matched)
	}

	// Every denial case above left a record too.
	all := allRecords(t, s, "acme")
	decisions := findRecords(all, AuditDecision)
	if len(decisions) != len(cases) {
		t.Fatalf("got %d decision records, want %d", len(decisions), len(cases))
	}
	for i, r := range decisions {
		if r.Seq != i+2 {
			t.Fatalf("decision record %d seq = %d", i, r.Seq)
		}
		if r.Decision.Allowed {
			t.Fatalf("decision record %d allowed", i)
		}
	}
}

func TestAuditUnpublishedDecisionRecorded(t *testing.T) {
	s := NewStore()
	d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if d.Allowed || d.Version != 0 {
		t.Fatalf("decision = %+v", d)
	}
	records := allRecords(t, s, "acme")
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	r := records[0]
	if r.Category != AuditDecision || r.Version != 0 || r.Decision.Allowed {
		t.Fatalf("record = %+v", r)
	}
	if r.Decision.Reason != "organization has no published version" {
		t.Fatalf("reason = %q", r.Decision.Reason)
	}
}

func TestAuditMissingDecisionOrgRecordsNothing(t *testing.T) {
	s := NewStore()
	d := s.Decide("", request("acme", "u1", "r1", "org/a", "read"))
	if d.Allowed || d.Reason != "missing decision organization" {
		t.Fatalf("decision = %+v", d)
	}
	if n := auditTail(t, s, "acme"); n != 0 {
		t.Fatalf("missing-org decide created %d records", n)
	}
	// Querying with no organization is an error, not a state change.
	if _, err := s.AuditQuery("", AuditQuery{PageSize: 1}); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("empty org query err = %v", err)
	}
}

func TestAuditRepeatedDecideAddsRecords(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	for i := 0; i < 3; i++ {
		if d := s.Decide("acme", req); !d.Allowed {
			t.Fatalf("decide %d denied: %+v", i, d)
		}
	}
	records := allRecords(t, s, "acme")
	decisions := findRecords(records, AuditDecision)
	if len(decisions) != 3 {
		t.Fatalf("got %d decision records, want 3", len(decisions))
	}
	seqs := map[int]bool{}
	for _, r := range decisions {
		if seqs[r.Seq] {
			t.Fatalf("duplicate seq %d", r.Seq)
		}
		seqs[r.Seq] = true
	}
}

func TestAuditReviewAndReverifyDoNotRecord(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	s.Decide("acme", req)
	before := auditTail(t, s, "acme")

	s.Review("acme", 1, req)
	s.Review("acme", 99, req)
	// Reverify of the decision record succeeds; reverify of the
	// policy-change record fails, but neither writes records.
	if _, err := s.ReverifyDecision("acme", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReverifyDecision("acme", 1); !errors.Is(err, ErrAuditNotDecision) {
		t.Fatalf("reverify policy record err = %v", err)
	}
	if got := auditTail(t, s, "acme"); got != before {
		t.Fatalf("review/reverify changed records: %d -> %d", before, got)
	}
}

func TestAuditSeqPerOrgIndependent(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if _, err := s.Publish("globex", 0, nil); err != nil {
		t.Fatal(err)
	}
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))

	acme := allRecords(t, s, "acme")
	globex := allRecords(t, s, "globex")
	if len(acme) != 2 || len(globex) != 3 {
		t.Fatalf("acme=%d globex=%d records", len(acme), len(globex))
	}
	for i, r := range acme {
		if r.Seq != i+1 || r.Org != "acme" {
			t.Fatalf("acme record %d = %+v", i, r)
		}
	}
	for i, r := range globex {
		if r.Seq != i+1 || r.Org != "globex" {
			t.Fatalf("globex record %d = %+v", i, r)
		}
	}
	// Same identifiers, but fingerprints differ because the org differs.
	if acme[0].Fingerprint == globex[0].Fingerprint {
		t.Fatal("identical fingerprints across organizations")
	}
	if acme[1].Fingerprint == globex[1].Fingerprint {
		t.Fatal("identical fingerprints across organizations")
	}
}

func TestAuditFingerprintChain(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}

	records := allRecords(t, s, "acme")
	if len(records) != 3 {
		t.Fatalf("got %d records", len(records))
	}
	if records[0].PrevFingerprint != "" {
		t.Fatalf("first record prev = %q, want empty", records[0].PrevFingerprint)
	}
	for i := 1; i < len(records); i++ {
		if records[i].PrevFingerprint != records[i-1].Fingerprint {
			t.Fatalf("record %d prev does not link to record %d", i, i-1)
		}
		if records[i].Fingerprint == "" || records[i].Fingerprint == records[i-1].Fingerprint {
			t.Fatalf("record %d fingerprint = %q", i, records[i].Fingerprint)
		}
	}
	// Recomputing over the stored content reproduces the fingerprints.
	for i, r := range records {
		prev := ""
		if i > 0 {
			prev = records[i-1].Fingerprint
		}
		if got := computeFingerprint(prev, &r); got != r.Fingerprint {
			t.Fatalf("record %d fingerprint does not match content", i)
		}
	}
}

func TestAuditQueryPagination(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	}
	// 6 records: 1 policy change + 5 decisions.

	page, err := s.AuditQuery("acme", AuditQuery{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.UpToSeq != 6 || page.Fingerprint == "" {
		t.Fatalf("first page range = %d %q", page.UpToSeq, page.Fingerprint)
	}
	if len(page.Records) != 2 || page.NextSeq != 2 {
		t.Fatalf("first page = %d records, next %d", len(page.Records), page.NextSeq)
	}
	if page.Records[0].Seq != 1 || page.Records[1].Seq != 2 {
		t.Fatalf("first page seqs = %d, %d", page.Records[0].Seq, page.Records[1].Seq)
	}

	// Page through the rest with the echoed range.
	got := append([]AuditRecord(nil), page.Records...)
	after := page.NextSeq
	for after != 0 {
		page, err = s.AuditQuery("acme", AuditQuery{
			PageSize: 2, UpToSeq: 6, Fingerprint: page.Fingerprint, AfterSeq: after,
		})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page.Records...)
		after = page.NextSeq
	}
	if len(got) != 6 {
		t.Fatalf("paged through %d records, want 6", len(got))
	}
	for i, r := range got {
		if r.Seq != i+1 {
			t.Fatalf("paged record %d seq = %d", i, r.Seq)
		}
	}

	// Records appended after the first query must not mix into the range.
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	page, err = s.AuditQuery("acme", AuditQuery{PageSize: 100, UpToSeq: 6, Fingerprint: page.Fingerprint, AfterSeq: 6})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.NextSeq != 0 {
		t.Fatalf("frozen range leaked new records: %+v", page)
	}
}

func TestAuditQueryFilters(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	// 4 records: change, decision u1, decision u2, change.

	tests := []struct {
		name    string
		query   AuditQuery
		wantSeq []int
	}{
		{"policy changes", AuditQuery{PageSize: 10, Category: AuditPolicyChange}, []int{1, 4}},
		{"decisions", AuditQuery{PageSize: 10, Category: AuditDecision}, []int{2, 3}},
		{"subject u1", AuditQuery{PageSize: 10, Subject: "u1"}, []int{2}},
		{"subject u2", AuditQuery{PageSize: 10, Subject: "u2"}, []int{3}},
		{"subject with policy category", AuditQuery{PageSize: 10, Subject: "u1", Category: AuditPolicyChange}, nil},
		{"no match", AuditQuery{PageSize: 10, Subject: "nobody"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.AuditQuery("acme", tc.query)
			if err != nil {
				t.Fatal(err)
			}
			var seqs []int
			for _, r := range page.Records {
				seqs = append(seqs, r.Seq)
			}
			if !reflect.DeepEqual(seqs, tc.wantSeq) {
				t.Fatalf("seqs = %v, want %v", seqs, tc.wantSeq)
			}
		})
	}
}

func TestAuditQueryEmptyOrg(t *testing.T) {
	s := NewStore()
	page, err := s.AuditQuery("acme", AuditQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.UpToSeq != 0 || page.Fingerprint != "" || len(page.Records) != 0 || page.NextSeq != 0 {
		t.Fatalf("empty org page = %+v", page)
	}
	exp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if exp.UpToSeq != 0 || exp.Fingerprint != "" || len(exp.Records) != 0 {
		t.Fatalf("empty export = %+v", exp)
	}
	if err := VerifyAuditExport(exp); err != nil {
		t.Fatalf("empty export verify: %v", err)
	}
}

func TestAuditQueryErrors(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, nil); err != nil {
		t.Fatal(err)
	}
	fp := allRecords(t, s, "acme")[0].Fingerprint

	tests := []struct {
		name  string
		org   string
		query AuditQuery
		want  error
	}{
		{"missing org", "", AuditQuery{PageSize: 1}, ErrMissingOrganization},
		{"zero page size", "acme", AuditQuery{PageSize: 0}, ErrInvalidAuditQuery},
		{"negative page size", "acme", AuditQuery{PageSize: -1}, ErrInvalidAuditQuery},
		{"negative upTo", "acme", AuditQuery{PageSize: 1, UpToSeq: -1}, ErrInvalidAuditQuery},
		{"upTo beyond tail", "acme", AuditQuery{PageSize: 1, UpToSeq: 5}, ErrInvalidAuditQuery},
		{"inverted range", "acme", AuditQuery{PageSize: 1, UpToSeq: 1, AfterSeq: 2}, ErrInvalidAuditQuery},
		{"bad fingerprint", "acme", AuditQuery{PageSize: 1, UpToSeq: 1, Fingerprint: "bogus"}, ErrAuditFingerprint},
		{"unknown category", "acme", AuditQuery{PageSize: 1, Category: "bogus"}, ErrInvalidAuditQuery},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AuditQuery(tc.org, tc.query)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// A correct fingerprint for the wrong seq also fails.
	if _, err := s.AuditQuery("acme", AuditQuery{PageSize: 1, UpToSeq: 1, Fingerprint: fp + "x"}); !errors.Is(err, ErrAuditFingerprint) {
		t.Fatalf("wrong fingerprint err = %v", err)
	}
}

func TestAuditExportErrors(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuditExport("", 1); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("missing org err = %v", err)
	}
	if _, err := s.AuditExport("acme", -1); !errors.Is(err, ErrInvalidAuditQuery) {
		t.Fatalf("negative upTo err = %v", err)
	}
	if _, err := s.AuditExport("acme", 2); !errors.Is(err, ErrInvalidAuditQuery) {
		t.Fatalf("upTo beyond tail err = %v", err)
	}
}

func TestAuditExportVerify(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))

	exp, err := s.AuditExport("acme", 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAuditExport(exp); err != nil {
		t.Fatalf("legitimate export failed: %v", err)
	}
	// Partial export also verifies against its own tail.
	partial, err := s.AuditExport("acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAuditExport(partial); err != nil {
		t.Fatalf("partial export failed: %v", err)
	}
}

func TestAuditExportTamperDetection(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))

	legit, err := s.AuditExport("acme", 4)
	if err != nil {
		t.Fatal(err)
	}

	tampered := func(name string, fn func(AuditExport) AuditExport) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			// Deep-copy so a mutation cannot leak into later subtests.
			exp := AuditExport{Org: legit.Org, UpToSeq: legit.UpToSeq, Fingerprint: legit.Fingerprint}
			exp.Records = make([]AuditRecord, len(legit.Records))
			for i := range legit.Records {
				exp.Records[i] = *cloneRecord(&legit.Records[i])
			}
			exp = fn(exp)
			if err := VerifyAuditExport(exp); err == nil {
				t.Fatalf("tampered export (%s) verified", name)
			}
		})
	}

	tampered("modified decision field", func(e AuditExport) AuditExport {
		// Record 4 is a denial; flipping it to allow must be caught.
		e.Records[3].Decision.Allowed = true
		return e
	})
	tampered("modified policy field", func(e AuditExport) AuditExport {
		e.Records[0].Policies[0].Effect = EffectDeny
		return e
	})
	tampered("modified request subject", func(e AuditExport) AuditExport {
		e.Records[1].Request.Subject.ID = "attacker"
		return e
	})
	tampered("deleted middle record", func(e AuditExport) AuditExport {
		e.Records = append(append([]AuditRecord{}, e.Records[:1]...), e.Records[2:]...)
		return e
	})
	tampered("deleted tail record", func(e AuditExport) AuditExport {
		e.Records = append([]AuditRecord{}, e.Records[:3]...)
		return e
	})
	tampered("swapped order", func(e AuditExport) AuditExport {
		e.Records = append([]AuditRecord{}, e.Records...)
		e.Records[0], e.Records[1] = e.Records[1], e.Records[0]
		return e
	})
	tampered("wrong tail fingerprint", func(e AuditExport) AuditExport {
		e.Fingerprint = "bogus"
		return e
	})
	tampered("wrong upToSeq", func(e AuditExport) AuditExport {
		e.UpToSeq = 3
		return e
	})

	// A record spliced in from another organization must fail.
	if _, err := s.Publish("globex", 0, nil); err != nil {
		t.Fatal(err)
	}
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))
	globex, err := s.AuditExport("globex", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("spliced foreign record", func(t *testing.T) {
		exp := legit
		exp.Records = append([]AuditRecord{}, legit.Records...)
		exp.Records[1] = globex.Records[0]
		if err := VerifyAuditExport(exp); err == nil {
			t.Fatal("spliced export verified")
		}
	})
}

func TestAuditReverifyDecision(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	d1 := s.Decide("acme", req)

	// Publish an empty version and decide again: this denial must keep
	// reverifying under version 2.
	if _, err := s.Publish("acme", 1, nil); err != nil {
		t.Fatal(err)
	}
	d2 := s.Decide("acme", req)
	if d2.Allowed || d2.Version != 2 {
		t.Fatalf("decision under empty version = %+v", d2)
	}

	// Rollback to v1 as v3; later changes must not touch past decisions.
	if _, err := s.Rollback("acme", 2, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReverifyDecision("acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d1) {
		t.Fatalf("reverify seq 2 = %+v, want %+v", got, d1)
	}
	got, err = s.ReverifyDecision("acme", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d2) {
		t.Fatalf("reverify seq 4 = %+v, want %+v", got, d2)
	}

	// Non-decision record is a distinguishable error.
	if _, err := s.ReverifyDecision("acme", 1); !errors.Is(err, ErrAuditNotDecision) {
		t.Fatalf("reverify policy record err = %v", err)
	}
	if _, err := s.ReverifyDecision("acme", 3); !errors.Is(err, ErrAuditNotDecision) {
		t.Fatalf("reverify policy record err = %v", err)
	}
	// Missing sequences.
	if _, err := s.ReverifyDecision("acme", 0); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("reverify seq 0 err = %v", err)
	}
	if _, err := s.ReverifyDecision("acme", 99); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("reverify missing seq err = %v", err)
	}
	if _, err := s.ReverifyDecision("acme", -1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("reverify negative seq err = %v", err)
	}
	if _, err := s.ReverifyDecision("", 1); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("reverify missing org err = %v", err)
	}
}

func TestAuditCallerMutationsDoNotAffectRecords(t *testing.T) {
	s := NewStore()
	submitted := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}
	if _, err := s.Publish("acme", 0, submitted); err != nil {
		t.Fatal(err)
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	s.Decide("acme", req)

	// Mutate everything the caller handed over after the fact.
	submitted[0].Effect = EffectDeny
	submitted[0].Subject = "nobody"
	req.Subject.ID = "attacker"
	req.Action = "delete"
	req.Resource.Scope = "org/other"

	records := allRecords(t, s, "acme")
	if err := VerifyAuditExport(AuditExport{
		Org: "acme", UpToSeq: len(records), Fingerprint: records[len(records)-1].Fingerprint, Records: records,
	}); err != nil {
		t.Fatalf("records changed after caller mutation: %v", err)
	}
	if records[1].Request.Subject.ID != "u1" || records[1].Request.Action != "read" {
		t.Fatalf("stored request mutated: %+v", records[1].Request)
	}
	if records[0].Policies[0].Subject != "u1" {
		t.Fatalf("stored policy mutated: %+v", records[0].Policies[0])
	}

	// Mutating a query result must not change the store either.
	page, err := s.AuditQuery("acme", AuditQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	page.Records[0].Policies[0].Effect = EffectDeny
	page.Records[1].Decision.Allowed = true
	records2 := allRecords(t, s, "acme")
	if err := VerifyAuditExport(AuditExport{
		Org: "acme", UpToSeq: len(records2), Fingerprint: records2[len(records2)-1].Fingerprint, Records: records2,
	}); err != nil {
		t.Fatalf("store records changed after query-result mutation: %v", err)
	}
}

func TestAuditConcurrent(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}

	const goroutines = 8
	const iterations = 20
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				req := request("acme", "u1", "r1", "org/a", "read")
				if g%2 == 0 {
					req.Subject.ID = fmt.Sprintf("u%d", g)
				}
				s.Decide("acme", req)
				if i%5 == 0 {
					cur := s.CurrentVersion("acme")
					_, _ = s.Publish("acme", cur, nil)
				}
				if i%3 == 0 {
					_, _ = s.AuditQuery("acme", AuditQuery{PageSize: 5})
				}
			}
		}(g)
	}
	wg.Wait()

	records := allRecords(t, s, "acme")
	// No missing records, no duplicate sequences.
	seen := map[int]bool{}
	for _, r := range records {
		if seen[r.Seq] {
			t.Fatalf("duplicate seq %d", r.Seq)
		}
		seen[r.Seq] = true
	}
	if len(seen) != len(records) {
		t.Fatalf("seq count mismatch: %d unique, %d records", len(seen), len(records))
	}
	for i := 1; i <= len(records); i++ {
		if !seen[i] {
			t.Fatalf("missing seq %d", i)
		}
	}
	// Every record is queryable and the export verifies.
	exp, err := s.AuditExport("acme", len(records))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAuditExport(exp); err != nil {
		t.Fatalf("concurrent export verify: %v", err)
	}
	// Every decision record matches the version it names.
	for _, r := range records {
		if r.Category != AuditDecision {
			continue
		}
		want, err := s.ReverifyDecision("acme", r.Seq)
		if err != nil {
			t.Fatalf("reverify seq %d: %v", r.Seq, err)
		}
		if !reflect.DeepEqual(want, r.Decision) {
			t.Fatalf("seq %d: reverify = %+v, stored = %+v", r.Seq, want, r.Decision)
		}
	}
	// Fingerprints still link.
	for i := 1; i < len(records); i++ {
		if records[i].PrevFingerprint != records[i-1].Fingerprint {
			t.Fatalf("chain broken at %d", i)
		}
	}
}
