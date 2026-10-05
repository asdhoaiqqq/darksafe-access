// Regression coverage for subject identifiers compared by their exact
// submitted bytes. The platform accepts subject identifiers that carry
// bytes which are not valid UTF-8; two identifiers that render identically
// may still be different subjects. Authorization must never merge them via
// a character replacement, and audit queries must never treat them as one
// subject.
//
// Three enabled subjects in one organization read one resource; the
// organization, action and legal scope are identical, and the subject
// identifiers differ at exactly one middle position:
//
//   - a lone 0xFF byte,
//   - a lone 0xFE byte,
//   - a genuine U+FFFD code point (valid UTF-8, three bytes EF BF BD).
//
// The one published version carries an allow policy for the first subject
// and an explicit deny policy for the second; the third has no policy at
// all. The decisions must come out as allow, own-policy deny and
// no-authorization default deny respectively, each explained by its own
// policy evidence at the published version, and the audit chain plus
// subject-filtered paging must keep the three identities apart byte for
// byte. The empty-value edge keeps its legacy meaning too: a request
// without a subject identifier is an envelope denial at version 0 with no
// matched policy, while an empty subject filter in an audit query means
// "do not filter by subject" rather than "query the missing identifier".
package darksafe

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// rawByteSubjects returns the three byte-different-but-display-conflated
// subject identifiers used by this file. The surrounding content is
// identical; exactly one middle position differs.
func rawByteSubjects() (withFF, withFE, withReplacement string) {
	return "svc-" + invalidByte + "-reader",
		"svc-" + anotherInvalidByte + "-reader",
		"svc-" + replacementRune + "-reader"
}

// TestRawByteSubjectIdentifiersStayDistinctInDecisions pins the
// authorization contract: matching is against the request's raw subject
// bytes, so a replacement of 0xFF with 0xFE (or with a genuine U+FFFD) can
// never make one subject ride another subject's allow or deny.
func TestRawByteSubjectIdentifiersStayDistinctInDecisions(t *testing.T) {
	subFF, subFE, subFD := rawByteSubjects()

	// Document the premise this regression guards: Go string comparison
	// distinguishes the three identifiers, while encoding/json renders the
	// two lone invalid bytes identically. Any normalization at the
	// authorization boundary would collapse the first two.
	if subFF == subFE || subFF == subFD || subFE == subFD {
		t.Fatalf("test premise broken: identifiers must differ pairwise: %q %q %q", subFF, subFE, subFD)
	}
	jFF, _ := json.Marshal(subFF)
	jFE, _ := json.Marshal(subFE)
	if !bytes.Equal(jFF, jFE) {
		t.Fatalf("test premise broken: JSON renders the invalid bytes differently: %s vs %s", jFF, jFE)
	}

	const (
		org        = "acme"
		resourceID = "ledger-2026"
		scope      = "acme/factory/ledger"
		action     = "read"
	)
	s := NewStore()
	version, err := s.Publish(org, 0, []Policy{
		{ID: "p-ledger-read-ff", Subject: subFF, Action: action, Scope: scope, Effect: EffectAllow},
		{ID: "p-ledger-read-fe-deny", Subject: subFE, Action: action, Scope: scope, Effect: EffectDeny},
		// No policy at all for the genuine-U+FFFD subject.
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	readReq := func(subject string) OrgRequest {
		return OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject:     Subject{ID: subject, Kind: "service"}, // all enabled
			Resource:    Resource{ID: resourceID, Scope: scope},
			Action:      action,
		}
	}

	// Subject 1: its own allow policy matches; the match list carries only
	// that policy, at the version actually published.
	allowFF := s.Decide(org, readReq(subFF))
	if !allowFF.Allowed {
		t.Fatalf("0xFF subject decision = %+v, want allowed", allowFF)
	}
	if allowFF.Reason != "matched allow policy" {
		t.Fatalf("0xFF subject reason = %q, want %q", allowFF.Reason, "matched allow policy")
	}
	if want := []string{"p-ledger-read-ff"}; !reflect.DeepEqual(allowFF.Matched, want) {
		t.Fatalf("0xFF subject matched = %q, want %q", allowFF.Matched, want)
	}
	if allowFF.Version != 1 {
		t.Fatalf("0xFF subject version = %d, want the published version 1", allowFF.Version)
	}

	// Subject 2: denied by ITS OWN deny policy. The first subject's allow
	// must not travel with it, and the explanation is the policy-deny
	// reason rather than the default denial.
	denyFE := s.Decide(org, readReq(subFE))
	if denyFE.Allowed {
		t.Fatalf("0xFE subject decision = %+v, want denied by its deny policy", denyFE)
	}
	if denyFE.Reason != "matched deny policy" {
		t.Fatalf("0xFE subject reason = %q, want %q", denyFE.Reason, "matched deny policy")
	}
	if want := []string{"p-ledger-read-fe-deny"}; !reflect.DeepEqual(denyFE.Matched, want) {
		t.Fatalf("0xFE subject matched = %q, want only its own deny %q (no other subject's policy)", denyFE.Matched, want)
	}
	if denyFE.Version != 1 {
		t.Fatalf("0xFE subject version = %d, want the published version 1", denyFE.Version)
	}

	// Subject 3: no policy names it, so the default denial applies with an
	// empty match list. Neither the allow nor the deny of the
	// look-alike subjects may be used as its basis.
	noPolicyFD := s.Decide(org, readReq(subFD))
	if noPolicyFD.Allowed {
		t.Fatalf("U+FFFD subject decision = %+v, want default deny (no matching allow)", noPolicyFD)
	}
	if noPolicyFD.Reason != "no matching allow policy" {
		t.Fatalf("U+FFFD subject reason = %q, want %q", noPolicyFD.Reason, "no matching allow policy")
	}
	if len(noPolicyFD.Matched) != 0 {
		t.Fatalf("U+FFFD subject matched = %q, want no matched policy", noPolicyFD.Matched)
	}
	if noPolicyFD.Version != 1 {
		t.Fatalf("U+FFFD subject version = %d, want the published version 1 it was evaluated under", noPolicyFD.Version)
	}

	// The three explanations are pairwise distinguishable: an allow, a
	// policy-driven deny, and a no-authorization default deny.
	reasons := []string{allowFF.Reason, denyFE.Reason, noPolicyFD.Reason}
	for i := 0; i < len(reasons); i++ {
		for j := i + 1; j < len(reasons); j++ {
			if reasons[i] == reasons[j] {
				t.Fatalf("decisions %d and %d share reason %q; the three outcomes must be distinguishable", i, j, reasons[i])
			}
		}
	}
}

// TestRawByteSubjectAuditKeepsIdentitiesAndDecisionsApart builds a chain
// in which the three subjects' decisions are interleaved with another
// subject's decisions. Every decision record must preserve the submitted
// subject bytes and the returned decision, and a subject-filtered, paged
// query for any one non-empty identifier must return only records whose
// subject is byte-identical — no look-alike mixing in, no record missing,
// no record repeated, even across pages. The third subject's default
// denials are real audit records and must be found exactly like any
// allowed decision.
func TestRawByteSubjectAuditKeepsIdentitiesAndDecisionsApart(t *testing.T) {
	subFF, subFE, subFD := rawByteSubjects()

	const (
		org        = "acme"
		other      = "svc-billing-reader"
		resourceID = "ledger-2026"
		scope      = "acme/factory/ledger"
		action     = "read"
	)
	s := NewStore()
	if _, err := s.Publish(org, 0, []Policy{
		{ID: "p-ledger-read-ff", Subject: subFF, Action: action, Scope: scope, Effect: EffectAllow},
		{ID: "p-ledger-read-fe-deny", Subject: subFE, Action: action, Scope: scope, Effect: EffectDeny},
	}); err != nil {
		t.Fatal(err)
	}

	readReq := func(subject string) OrgRequest {
		return OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject:     Subject{ID: subject, Kind: "service"},
			Resource:    Resource{ID: resourceID, Scope: scope},
			Action:      action,
		}
	}

	// Chain layout (seq 1 is the publish):
	//	seq 2  subFF allow      seq 3  other (default deny)
	//	seq 4  subFE deny       seq 5  other (default deny)
	//	seq 6  subFD default    seq 7  other (default deny)
	//	seq 8  subFF allow      seq 9  other (default deny)
	//	seq 10 subFE deny       seq 11 other (default deny)
	//	seq 12 subFD default
	decide := func(subject string) Decision {
		return s.Decide(org, readReq(subject))
	}
	firstFF := decide(subFF)  // 2
	decide(other)             // 3
	firstFE := decide(subFE)  // 4
	decide(other)             // 5
	firstFD := decide(subFD)  // 6
	decide(other)             // 7
	secondFF := decide(subFF) // 8
	decide(other)             // 9
	secondFE := decide(subFE) // 10
	decide(other)             // 11
	secondFD := decide(subFD) // 12

	// The complete export preserves each request's full subject bytes and
	// the decision actually returned, and the raw-byte records verify.
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(org, recs, cp); err != nil {
		t.Fatalf("chain with raw-byte subjects must verify: %v", err)
	}
	if len(recs) != 12 {
		t.Fatalf("exported %d records, want 12", len(recs))
	}
	wantAtSeq := map[int]struct {
		subject  string
		decision Decision
	}{
		2: {subFF, firstFF}, 4: {subFE, firstFE}, 6: {subFD, firstFD},
		8: {subFF, secondFF}, 10: {subFE, secondFE}, 12: {subFD, secondFD},
	}
	for seq, want := range wantAtSeq {
		got := recs[seq-1].Decision
		if got == nil {
			t.Fatalf("seq %d has no decision payload", seq)
		}
		if got.Request.Subject.ID != want.subject {
			t.Fatalf("seq %d stored subject %q, want byte-exact %q", seq, got.Request.Subject.ID, want.subject)
		}
		if !reflect.DeepEqual(got.Decision, want.decision) {
			t.Fatalf("seq %d stored decision %+v, want returned %+v", seq, got.Decision, want.decision)
		}
	}

	// Subject-filtered walks, one matching record per page so every match
	// boundary is genuinely crossed. The helper also rejects cursor
	// repetition (non-termination) and checkpoint drift.
	cases := []struct {
		name     string
		subject  string
		wantSeqs []int
		allowed  bool
		reason   string
		matched  []string
	}{
		{"0xFF subject", subFF, []int{2, 8}, true, "matched allow policy", []string{"p-ledger-read-ff"}},
		{"0xFE subject", subFE, []int{4, 10}, false, "matched deny policy", []string{"p-ledger-read-fe-deny"}},
		// The third subject's default denials are audited and findable even
		// though no policy ever matched.
		{"U+FFFD subject", subFD, []int{6, 12}, false, "no matching allow policy", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := subjectPageWalk(t, s, org, 1, 1, "", tc.subject)
			if seqs := recordSeqs(got); !reflect.DeepEqual(seqs, tc.wantSeqs) {
				t.Fatalf("seqs = %v, want %v; look-alike records mixed in, dropped or repeated", seqs, tc.wantSeqs)
			}
			requireSubjectDecisions(t, org, tc.subject, got)
			for _, r := range got {
				// Byte-identical subject, including the raw invalid bytes.
				if r.Decision.Request.Subject.ID != tc.subject {
					t.Fatalf("seq %d subject %q, want %q", r.Seq, r.Decision.Request.Subject.ID, tc.subject)
				}
				if r.Decision.Decision.Allowed != tc.allowed ||
					r.Decision.Decision.Reason != tc.reason ||
					!reflect.DeepEqual(r.Decision.Decision.Matched, tc.matched) ||
					r.Decision.Decision.Version != 1 {
					t.Fatalf("seq %d preserved decision = %+v, want allowed=%v reason=%q matched=%q version=1",
						r.Seq, r.Decision.Decision, tc.allowed, tc.reason, tc.matched)
				}
			}
		})
	}

	// Querying any one identifier must not surface either look-alike. The
	// pairwise checks above already establish this, but pin it directly: no
	// target query ever returns the other subjects' sequences.
	for _, filter := range []string{subFF, subFE, subFD} {
		got, _ := subjectPageWalk(t, s, org, 1, 2, "", filter)
		for _, r := range got {
			if r.Decision.Request.Subject.ID != filter {
				t.Fatalf("filter %q returned record of %q", filter, r.Decision.Request.Subject.ID)
			}
		}
	}

	// All of this was read-only: the chain still ends at sequence 12.
	if got := len(drainAudit(t, s, org, "", "")); got != 12 {
		t.Fatalf("chain has %d records after queries, want 12; querying must not append", got)
	}
}

// TestMissingSubjectIDAndEmptySubjectFilterKeepDistinctMeanings pins the
// two unrelated empty values: a request submitted without a subject
// identifier is an envelope denial at version 0 with no matched policy,
// but it is still audited; an empty subject value in an audit query means
// the query is not subject-filtered at all, never a lookup of the missing
// identifier.
func TestMissingSubjectIDAndEmptySubjectFilterKeepDistinctMeanings(t *testing.T) {
	const org = "acme"
	s := NewStore()
	if _, err := s.Publish(org, 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
		{ID: "p-deny", Subject: "u2", Action: "read", Scope: "org/a", Effect: EffectDeny},
	}); err != nil {
		t.Fatal(err)
	}

	// A request whose subject identifier is absent: an envelope rejection,
	// evaluated under no published version, with no matched policy.
	missing := request(org, "u1", "r1", "org/a", "read")
	missing.Subject.ID = ""
	d := s.Decide(org, missing)
	if d.Allowed {
		t.Fatalf("missing-subject decision = %+v, want denial", d)
	}
	if d.Reason != "missing subject id" {
		t.Fatalf("missing-subject reason = %q, want %q", d.Reason, "missing subject id")
	}
	if d.Version != 0 {
		t.Fatalf("missing-subject version = %d, want 0 (no published version evaluated)", d.Version)
	}
	if d.Matched != nil {
		t.Fatalf("missing-subject matched = %q, want no matched policies (nil)", d.Matched)
	}

	// A normal decision afterwards proves the empty filter below has
	// non-empty subjects to return.
	if d := s.Decide(org, request(org, "u1", "r1", "org/a", "read")); !d.Allowed {
		t.Fatalf("u1 decision = %+v, want allowed by p-allow", d)
	}

	// The missing-subject denial is itself an audit record, preserving the
	// empty identifier and the version-0 decision.
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(org, recs, cp); err != nil {
		t.Fatalf("chain must verify: %v", err)
	}
	var foundMissing bool
	for _, r := range recs {
		if r.Kind == AuditDecision && r.Decision.Request.Subject.ID == "" {
			foundMissing = true
			if !reflect.DeepEqual(r.Decision.Decision, d) {
				t.Fatalf("missing-subject record = %+v, want %+v", r.Decision.Decision, d)
			}
		}
	}
	if !foundMissing {
		t.Fatal("the missing-subject envelope denial was not recorded in the audit chain")
	}

	// An empty subject filter with the decision category returns EVERY
	// subject's decisions, including u1's and the missing-identifier one.
	// It must not be reinterpreted as a filter for the missing identifier.
	allDecisions := drainAudit(t, s, org, AuditDecision, "")
	if len(allDecisions) != 2 {
		t.Fatalf("decision-only unfiltered query returned %d records, want 2", len(allDecisions))
	}
	var sawNonEmpty, sawEmpty bool
	for _, r := range allDecisions {
		if r.Decision.Request.Subject.ID == "" {
			sawEmpty = true
		} else {
			sawNonEmpty = true
		}
	}
	if !sawNonEmpty {
		t.Fatal("empty subject filter behaved like a missing-identifier filter: u1's decision was dropped")
	}
	if !sawEmpty {
		t.Fatal("the missing-subject decision was not returned by the unfiltered query")
	}

	// With no category either, policy-change records come back as well, so
	// the empty filter is the full history, not a subject-shaped subset.
	full := drainAudit(t, s, org, "", "")
	if len(full) != 3 || full[0].Kind != AuditPolicyChange {
		t.Fatalf("unfiltered query = %d records, want publish + 2 decisions", len(full))
	}
}
