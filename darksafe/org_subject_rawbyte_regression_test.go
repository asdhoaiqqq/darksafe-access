// Regression coverage for organization-level access decisions where
// subject identifiers differ only in raw bytes that displays conflate: a
// lone 0xFF, a lone 0xFE and a genuine U+FFFD rune. The platform accepts
// non-UTF-8 subject identifiers and must keep three display-identical
// names as three distinct principals end to end:
//
//   - At authorization time a policy keyed on one raw-byte identifier can
//     never match — or be listed as a hit for — either look-alike. The
//     0xFF subject gets its own allow, the 0xFE subject gets its own
//     explicit deny without the 0xFF allow leaking into the match list,
//     and the U+FFFD subject gets a default deny with an empty match list.
//     Every explanation uses the actually published version and a reason
//     that distinguishes the allow, the policy deny and the absence of
//     authorization; another principal's policy is never the basis.
//
//   - The audit chain stores the complete submitted identifier and the
//     returned decision of every request, including a default deny that
//     matched no policy. Querying by any one non-empty identifier returns
//     exactly that principal's records, byte for byte, even when the other
//     principals' records are interleaved and pages are thin: paging never
//     mixes in, drops or repeats a target record.
//
//   - The empty value keeps its two established meanings: a request with
//     no subject identifier is a missing-subject denial at version 0 with
//     no matched policy (still recorded), while an empty subject filter on
//     an audit query means "do not filter by subject" and must never turn
//     into a lookup of that missing identifier.
package darksafe

import (
	"bytes"
	"reflect"
	"testing"
	"unicode/utf8"
)

// The three identifiers are identical apart from the single middle unit:
// a lone 0xFF byte, a lone 0xFE byte, or a real U+FFFD code point. They
// share the surrounding ASCII so the difference is exactly one place.
var (
	rawByteSubjects = []struct {
		name string
		id   string
	}{
		{"0xFF", "acct-" + invalidByte + "-name"},
		{"0xFE", "acct-" + anotherInvalidByte + "-name"},
		{"U+FFFD", "acct-" + replacementRune + "-name"},
	}
	subjectAllowFF  = rawByteSubjects[0].id // published policy: allow read
	subjectDenyFE   = rawByteSubjects[1].id // published policy: deny read
	subjectNoPolicy = rawByteSubjects[2].id // no policy at all
)

// rawByteDecisionFixture publishes the single version under test: one
// allow keyed byte-for-byte to the 0xFF subject and one explicit deny keyed
// byte-for-byte to the 0xFE subject, both for the same action and legal
// scope. The genuine-U+FFFD subject has no policy. It then interleaves the
// three principals' decisions (plus one unrelated ASCII subject and one
// missing-subject request) so subject-filtered paging always crosses the
// other principals' records. It returns the decision of each principal,
// which is identical on every repeat.
//
//	seq 1  publish v1 (allow 0xFF read, deny 0xFE read)
//	seq 2  0xFF allow      seq 3  0xFE policy-deny  seq 4  U+FFFD default-deny
//	seq 5  plain default-deny
//	seq 6  0xFF allow      seq 7  U+FFFD deny      seq 8  0xFE deny
//	seq 9  0xFF allow      seq 10 U+FFFD deny      seq 11 0xFE deny
//	seq 12 missing subject id -> envelope denial (version 0)
//	seq 13 0xFF allow      seq 14 0xFE deny        seq 15 U+FFFD deny
func rawByteDecisionFixture(t *testing.T) (s *Store, dFF, dFE, dFFFD, dMissing Decision) {
	t.Helper()
	s = NewStore()
	const (
		org    = "acme"
		res    = "res-1"
		scope  = "org/data"
		action = "read"
	)
	policies := []Policy{
		{ID: "allow-ff", Subject: subjectAllowFF, Action: action, Scope: scope, Effect: EffectAllow},
		{ID: "deny-fe", Subject: subjectDenyFE, Action: action, Scope: scope, Effect: EffectDeny},
	}
	if v, err := s.Publish(org, 0, policies); err != nil || v != 1 {
		t.Fatalf("publish = %d, %v; want version 1", v, err)
	}
	decide := func(id string) Decision {
		return s.Decide(org, request(org, id, res, scope, action))
	}
	dFF = decide(subjectAllowFF)      // 2
	dFE = decide(subjectDenyFE)       // 3
	dFFFD = decide(subjectNoPolicy)   // 4
	decide("plain-other")             // 5: an unrelated principal must not leak either
	dFF2 := decide(subjectAllowFF)    // 6
	dFFFD2 := decide(subjectNoPolicy) // 7
	dFE2 := decide(subjectDenyFE)     // 8
	dFF3 := decide(subjectAllowFF)    // 9
	dFFFD3 := decide(subjectNoPolicy) // 10
	dFE3 := decide(subjectDenyFE)     // 11
	// The missing-identifier envelope rejection is recorded too.
	dMissing = decide("")             // 12
	dFF4 := decide(subjectAllowFF)    // 13
	dFE4 := decide(subjectDenyFE)     // 14
	dFFFD4 := decide(subjectNoPolicy) // 15
	for _, d := range [][]Decision{{dFF, dFF2, dFF3, dFF4}, {dFE, dFE2, dFE3, dFE4}, {dFFFD, dFFFD2, dFFFD3, dFFFD4}} {
		if !reflect.DeepEqual(d[0], d[1]) || !reflect.DeepEqual(d[0], d[2]) || !reflect.DeepEqual(d[0], d[3]) {
			t.Fatalf("repeat decisions of one subject diverged: %+v", d)
		}
	}
	return s, dFF, dFE, dFFFD, dMissing
}

// TestRawByteSubjectIdentifiersDecideIndependently pins the authorization
// behaviour: displays may show the same name, but the allow, the explicit
// deny and the absence of policy attach to three different principals.
// Each decision uses the actually published version and its own policy
// only.
func TestRawByteSubjectIdentifiersDecideIndependently(t *testing.T) {
	// Document the test premise: the raw identifiers are distinct, while
	// encoding/json — the historical fingerprint encoding — renders the two
	// lone invalid bytes identically as U+FFFD.
	if subjectAllowFF == subjectDenyFE || subjectAllowFF == subjectNoPolicy ||
		subjectDenyFE == subjectNoPolicy {
		t.Fatal("test fixture broken: the three identifiers are not distinct")
	}
	if bytes.Equal([]byte(subjectAllowFF), []byte(subjectDenyFE)) {
		t.Fatal("0xFF and 0xFE identifiers share raw bytes")
	}
	if utf8.ValidString(subjectAllowFF) || utf8.ValidString(subjectDenyFE) {
		t.Fatal("the lone-byte identifiers must be invalid UTF-8")
	}
	if !utf8.ValidString(subjectNoPolicy) {
		t.Fatal("the genuine U+FFFD identifier must be valid UTF-8")
	}

	s, dFF, dFE, dFFFD, dMissing := rawByteDecisionFixture(t)

	// Subject 1 (0xFF): allowed, and its hit list contains its allow policy
	// alone.
	if !dFF.Allowed {
		t.Fatalf("0xFF subject decision = %+v, want allowed", dFF)
	}
	if dFF.Reason != "matched allow policy" {
		t.Fatalf("0xFF reason = %q, want matched allow policy", dFF.Reason)
	}
	if want := []string{"allow-ff"}; !reflect.DeepEqual(dFF.Matched, want) {
		t.Fatalf("0xFF matched = %v, want %v", dFF.Matched, want)
	}
	if dFF.Version != 1 {
		t.Fatalf("0xFF version = %d, want the published version 1", dFF.Version)
	}

	// Subject 2 (0xFE): denied by its own explicit deny policy, and the 0xFF
	// subject's allow must not ride along in the hit list.
	if dFE.Allowed {
		t.Fatalf("0xFE subject decision = %+v, want denied", dFE)
	}
	if dFE.Reason != "matched deny policy" {
		t.Fatalf("0xFE reason = %q, want matched deny policy", dFE.Reason)
	}
	if want := []string{"deny-fe"}; !reflect.DeepEqual(dFE.Matched, want) {
		t.Fatalf("0xFE matched = %v, want %v (the 0xFF allow must not leak in)", dFE.Matched, want)
	}
	if dFE.Version != 1 {
		t.Fatalf("0xFE version = %d, want the published version 1", dFE.Version)
	}

	// Subject 3 (genuine U+FFFD): no policy of its own, so a default deny
	// with an empty match list — the 0xFF allow and 0xFE deny both belong to
	// other principals and cannot be cited.
	if dFFFD.Allowed {
		t.Fatalf("U+FFFD subject decision = %+v, want denied", dFFFD)
	}
	if dFFFD.Reason != "no matching allow policy" {
		t.Fatalf("U+FFFD reason = %q, want no matching allow policy", dFFFD.Reason)
	}
	if len(dFFFD.Matched) != 0 {
		t.Fatalf("U+FFFD matched = %v, want no matched policies", dFFFD.Matched)
	}
	if dFFFD.Version != 1 {
		t.Fatalf("U+FFFD version = %d, want the published version 1", dFFFD.Version)
	}

	// The three explanations must be pairwise distinguishable: an allow is
	// never justified by another principal's deny, nor a default deny by
	// another principal's allow.
	reasons := map[string]bool{dFF.Reason: true, dFE.Reason: true, dFFFD.Reason: true}
	if len(reasons) != 3 {
		t.Fatalf("reasons %q/%q/%q must distinguish the three outcomes", dFF.Reason, dFE.Reason, dFFFD.Reason)
	}

	// Missing subject identifier: the established envelope denial — version
	// 0 (no policy set evaluated) and no matched policy — distinct from the
	// "no matching allow policy" default denial.
	if dMissing.Allowed || dMissing.Reason != "missing subject id" || dMissing.Version != 0 {
		t.Fatalf("missing-subject decision = %+v, want missing-subject-id denial at version 0", dMissing)
	}
	if dMissing.Matched != nil {
		t.Fatalf("missing-subject matched = %v, want nil (no policy evaluated)", dMissing.Matched)
	}

	// Sanity: the store really is at the one published version; deciding
	// never published or rolled back.
	if v := s.CurrentVersion("acme"); v != 1 {
		t.Fatalf("current version = %d, want 1", v)
	}
}

// TestRawByteSubjectAuditQueriesAreByteExact pins that the audit records
// keep each request's complete submitted identifier and returned decision,
// and that querying by any one non-empty identifier selects only records
// whose identifier is byte-for-byte identical — across thin interleaved
// pages, without mixing in, dropping or repeating a target record. The
// third principal's default deny is audited despite matching no policy.
func TestRawByteSubjectAuditQueriesAreByteExact(t *testing.T) {
	s, dFF, dFE, dFFFD, dMissing := rawByteDecisionFixture(t)
	wantSeqs := map[string][]int{
		subjectAllowFF:  {2, 6, 9, 13},
		subjectDenyFE:   {3, 8, 11, 14},
		subjectNoPolicy: {4, 7, 10, 15},
	}
	wantDecision := map[string]Decision{
		subjectAllowFF:  dFF,
		subjectDenyFE:   dFE,
		subjectNoPolicy: dFFFD,
	}
	others := map[string][]string{
		subjectAllowFF:  {subjectDenyFE, subjectNoPolicy},
		subjectDenyFE:   {subjectAllowFF, subjectNoPolicy},
		subjectNoPolicy: {subjectAllowFF, subjectDenyFE},
	}

	for _, subj := range rawByteSubjects {
		target := subj.id
		// Page size 1 forces every continuation to cross the other
		// principals' interleaved records; larger sizes must agree.
		for _, pageSize := range []int{1, 2, 3, 100} {
			recs, _ := subjectPageWalk(t, s, "acme", 1, pageSize, AuditDecision, target)
			if got := recordSeqs(recs); !reflect.DeepEqual(got, wantSeqs[target]) {
				t.Fatalf("%s pageSize %d seqs = %v, want %v", subj.name, pageSize, got, wantSeqs[target])
			}
			requireSubjectDecisions(t, "acme", target, recs)
			for _, r := range recs {
				// The stored identifier is the exact submitted bytes, not a
				// display-normalized copy.
				if got := r.Decision.Request.Subject.ID; got != target {
					t.Fatalf("%s seq %d stored identifier = %q, want byte-exact target",
						subj.name, r.Seq, got)
				}
				for _, other := range others[target] {
					if r.Decision.Request.Subject.ID == other {
						t.Fatalf("%s query leaked a look-alike principal at seq %d", subj.name, r.Seq)
					}
				}
				if !reflect.DeepEqual(r.Decision.Request, request("acme", target, "res-1", "org/data", "read")) {
					t.Fatalf("%s seq %d lost the submitted request", subj.name, r.Seq)
				}
				if !reflect.DeepEqual(r.Decision.Decision, wantDecision[target]) {
					t.Fatalf("%s seq %d decision = %+v, want %+v",
						subj.name, r.Seq, r.Decision.Decision, wantDecision[target])
				}
			}
		}
		// The raw byte survives into every returned identifier; the genuine
		// U+FFFD record stays valid UTF-8.
		recs, _ := subjectPageWalk(t, s, "acme", 1, 1, "", target)
		for _, r := range recs {
			valid := utf8.ValidString(r.Decision.Request.Subject.ID)
			if target == subjectNoPolicy {
				if !valid {
					t.Fatalf("%s record must stay valid UTF-8", subj.name)
				}
			} else if valid {
				t.Fatalf("%s record lost its invalid raw byte", subj.name)
			}
		}
	}

	// The third principal's default denial is really there and findable:
	// matching no policy never drops the audit record.
	noPolicyRecs, _ := subjectPageWalk(t, s, "acme", 1, 1, "", subjectNoPolicy)
	if len(noPolicyRecs) != len(wantSeqs[subjectNoPolicy]) {
		t.Fatalf("U+FFFD records = %d, want %d; a no-policy denial went missing",
			len(noPolicyRecs), len(wantSeqs[subjectNoPolicy]))
	}
	for _, r := range noPolicyRecs {
		if r.Decision.Decision.Allowed || r.Decision.Decision.Reason != "no matching allow policy" ||
			len(r.Decision.Decision.Matched) != 0 {
			t.Fatalf("U+FFFD seq %d recorded decision = %+v, want the default denial", r.Seq, r.Decision.Decision)
		}
	}

	// The full chain with the mixed raw/valid records still verifies, which
	// a JSON-replacing fingerprint could not provide: the 0xFF and 0xFE
	// records would collapse onto one another.
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("chain with raw-byte subject identifiers must verify: %v", err)
	}

	// Empty subject filter means "no subject filtering": the decision view
	// contains every principal — the three look-alikes, the unrelated ASCII
	// principal and the missing-identifier envelope denial (seq 12). It must
	// not be reinterpreted as a query for the empty identifier.
	all := drainAudit(t, s, "acme", AuditDecision, "")
	if got := recordSeqs(all); !reflect.DeepEqual(got, []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}) {
		t.Fatalf("empty-filter decision seqs = %v, want every decision in order", got)
	}
	var sawFF, sawFE, sawFFFD, sawMissing bool
	for _, r := range all {
		switch r.Decision.Request.Subject.ID {
		case subjectAllowFF:
			sawFF = true
		case subjectDenyFE:
			sawFE = true
		case subjectNoPolicy:
			sawFFFD = true
		case "":
			sawMissing = true
			if !reflect.DeepEqual(r.Decision.Decision, dMissing) {
				t.Fatalf("missing-subject record = %+v, want %+v", r.Decision.Decision, dMissing)
			}
		}
	}
	if !sawFF || !sawFE || !sawFFFD || !sawMissing {
		t.Fatalf("empty filter must not filter: ff=%v fe=%v fffd=%v missing=%v", sawFF, sawFE, sawFFFD, sawMissing)
	}
}
