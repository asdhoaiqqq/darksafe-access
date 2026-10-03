package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// The tests in this file pin the list-isolation contract of decision
// records: the subject's role list is part of the audited request even
// though roles play no part in authorization, and the matched-policy list
// is part of the audited explanation. Both lists must be stored and
// returned as independent copies, so a caller editing any list it handed
// in or received back can never rewrite the recorded history.

// decisionSeqs returns the sequences of all decision records in the
// organization, in chain order.
func decisionSeqs(t *testing.T, s *Store, org string) []int {
	t.Helper()
	recs := drainAudit(t, s, org, AuditDecision, "")
	seqs := make([]int, len(recs))
	for i, r := range recs {
		seqs[i] = r.Seq
	}
	return seqs
}

func TestDecisionListsAreDetachedFromCallerInput(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		allowPolicy("p-allow", "u1", "read", "org/a", false),
		{ID: "p-deny", Subject: "u2", Action: "read", Scope: "org/a", Effect: EffectDeny},
	}); err != nil {
		t.Fatal(err)
	}

	// An allow and a matched-deny decision, both with non-empty role lists.
	allowReq := request("acme", "u1", "r1", "org/a", "read")
	allowReq.Subject.Roles = []string{"auditor", "on call"}
	allow := s.Decide("acme", allowReq) // seq 2
	if !allow.Allowed || !reflect.DeepEqual(allow.Matched, []string{"p-allow"}) {
		t.Fatalf("allow decision = %+v", allow)
	}
	denyReq := request("acme", "u2", "r1", "org/a", "read")
	denyReq.Subject.Roles = []string{"contractor"}
	deny := s.Decide("acme", denyReq) // seq 3
	if deny.Allowed || !reflect.DeepEqual(deny.Matched, []string{"p-deny"}) {
		t.Fatalf("deny decision = %+v", deny)
	}

	// The caller rewrites and extends the role lists it submitted, and
	// rewrites the matched lists it got back.
	allowReq.Subject.Roles[0] = "rewritten"
	allowReq.Subject.Roles = append(allowReq.Subject.Roles, "injected")
	denyReq.Subject.Roles[0] = "rewritten"
	allow.Matched[0] = "forged"
	deny.Matched[0] = "forged"

	// Re-reading must show the decision-time content: original role order,
	// full matched list, outcome, reason and the version actually used.
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("chain broken by caller edits: %v", err)
	}
	gotAllow := recs[1].Decision
	if !reflect.DeepEqual(gotAllow.Request.Subject.Roles, []string{"auditor", "on call"}) {
		t.Fatalf("stored allow roles = %q, want original order", gotAllow.Request.Subject.Roles)
	}
	if !reflect.DeepEqual(gotAllow.Decision.Matched, []string{"p-allow"}) ||
		!gotAllow.Decision.Allowed || gotAllow.Decision.Reason != "matched allow policy" ||
		gotAllow.Decision.Version != 1 {
		t.Fatalf("stored allow decision = %+v", gotAllow.Decision)
	}
	gotDeny := recs[2].Decision
	if !reflect.DeepEqual(gotDeny.Request.Subject.Roles, []string{"contractor"}) {
		t.Fatalf("stored deny roles = %q", gotDeny.Request.Subject.Roles)
	}
	if !reflect.DeepEqual(gotDeny.Decision.Matched, []string{"p-deny"}) ||
		gotDeny.Decision.Allowed || gotDeny.Decision.Reason != "matched deny policy" ||
		gotDeny.Decision.Version != 1 {
		t.Fatalf("stored deny decision = %+v", gotDeny.Decision)
	}

	// Rechecking replays the recorded request against the recorded version,
	// so it must reproduce the original conclusions, edits notwithstanding.
	for seq, want := range map[int]Decision{2: {Allowed: true, Reason: "matched allow policy", Matched: []string{"p-allow"}, Version: 1},
		3: {Allowed: false, Reason: "matched deny policy", Matched: []string{"p-deny"}, Version: 1}} {
		got, err := s.RecheckDecision("acme", seq)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("recheck seq %d = %+v, %v; want %+v", seq, got, err, want)
		}
	}
}

func TestReadCopiesOfDecisionListsAreIsolated(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p-allow", "u1", "read", "org/a", false)})
	req := request("acme", "u1", "r1", "org/a", "read")
	req.Subject.Roles = []string{"auditor", "on call"}
	s.Decide("acme", req) // seq 2

	before := decisionSeqs(t, s, "acme")

	// Obtain the record through every read path: first page, follow-up
	// page, and full export.
	first, err := s.AuditQuery("acme", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AuditPage(first.Checkpoint, first.Next, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	exported, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	// A copy retained before any editing, for later comparison.
	kept := cloneExportedRecords(exported)

	// Edit the lists in every copy in hand.
	second.Records[0].Decision.Request.Subject.Roles[0] = "rewritten"
	second.Records[0].Decision.Decision.Matched[0] = "forged"
	exported[1].Decision.Request.Subject.Roles = append(exported[1].Decision.Request.Subject.Roles, "injected")
	exported[1].Decision.Decision.Matched[0] = "forged"

	// A fresh read still has the original content, and so does the copy
	// retained before the edits.
	fresh, freshCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh, kept) {
		t.Fatalf("fresh export differs from retained copy:\nfresh %+v\nkept %+v", fresh[1].Decision, kept[1].Decision)
	}
	if !reflect.DeepEqual(fresh[1].Decision.Request.Subject.Roles, []string{"auditor", "on call"}) ||
		!reflect.DeepEqual(fresh[1].Decision.Decision.Matched, []string{"p-allow"}) {
		t.Fatalf("stored lists changed through read copies: %+v", fresh[1].Decision)
	}

	// The original checkpoint still validates a freshly read full export,
	// and the record still rechecks to the original conclusion.
	if freshCP != cp {
		t.Fatalf("checkpoint moved after read-only edits: %+v -> %+v", cp, freshCP)
	}
	if err := VerifyAudit("acme", fresh, cp); err != nil {
		t.Fatalf("fresh export no longer verifies: %v", err)
	}
	if got, err := s.RecheckDecision("acme", 2); err != nil || !got.Allowed ||
		!reflect.DeepEqual(got.Matched, []string{"p-allow"}) {
		t.Fatalf("recheck after edits = %+v, %v", got, err)
	}

	// The edited export paired with the original checkpoint is rejected.
	if err := VerifyAudit("acme", exported, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tampered export err = %v, want ErrInvalidRange", err)
	}

	// None of the reads or edits appended anything.
	if after := decisionSeqs(t, s, "acme"); !reflect.DeepEqual(after, before) {
		t.Fatalf("reads appended records: %v -> %v", before, after)
	}
}

func TestEmptyAndNilListsKeepTheirShape(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p-allow", "u1", "read", "org/a", false)})

	// Roles never provided: nil.
	noRoles := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 2
	// Roles explicitly provided as an empty list.
	emptyReq := request("acme", "u1", "r1", "org/a", "read")
	emptyReq.Subject.Roles = []string{}
	s.Decide("acme", emptyReq) // seq 3
	// Denial that matched nothing: empty matched list, non-empty roles.
	lonelyReq := request("acme", "u9", "r1", "org/a", "read")
	lonelyReq.Subject.Roles = []string{"contractor"}
	s.Decide("acme", lonelyReq) // seq 4
	// Envelope rejection: no evaluation ran, matched stays nil.
	disabledReq := request("acme", "u1", "r1", "org/a", "read")
	disabledReq.Subject.Disabled = true
	s.Decide("acme", disabledReq) // seq 5

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("chain with empty lists: %v", err)
	}
	roles := func(seq int) []string { return recs[seq-1].Decision.Request.Subject.Roles }
	matched := func(seq int) []string { return recs[seq-1].Decision.Decision.Matched }

	// Nil and explicitly empty must not be read back as one another.
	if roles(2) != nil {
		t.Fatalf("unspecified roles read back as %#v", roles(2))
	}
	if roles(3) == nil || len(roles(3)) != 0 {
		t.Fatalf("explicit empty roles read back as %#v", roles(3))
	}
	// A denial without matched policies keeps its empty matched list and
	// its original outcome and reason.
	if matched(4) == nil || len(matched(4)) != 0 {
		t.Fatalf("no-match denial matched list = %#v", matched(4))
	}
	if d := recs[3].Decision.Decision; d.Allowed || d.Reason != "no matching allow policy" || d.Version != 1 {
		t.Fatalf("no-match denial = %+v", d)
	}
	// The envelope rejection never evaluated policies: matched stays nil.
	if matched(5) != nil {
		t.Fatalf("envelope rejection matched list = %#v", matched(5))
	}
	if !reflect.DeepEqual(noRoles.Matched, []string{"p-allow"}) {
		t.Fatalf("sanity: first decision = %+v", noRoles)
	}

	// Rechecking the no-match denial still yields the original conclusion.
	if got, err := s.RecheckDecision("acme", 4); err != nil || got.Allowed ||
		got.Reason != "no matching allow policy" {
		t.Fatalf("recheck no-match denial = %+v, %v", got, err)
	}
}

func TestDecisionListsPreserveRawContent(t *testing.T) {
	s := NewStore()
	// A matched policy id carrying a non-UTF-8 byte lands in Decision.Matched.
	policyID := "策" + invalidByte + "略"
	if _, err := s.Publish("acme", 0, []Policy{
		{ID: policyID, Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
	}); err != nil {
		t.Fatal(err)
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	// Chinese, spaces and invalid UTF-8 bytes, in a deliberate order.
	req.Subject.Roles = []string{"角 色", "普 通 角 色", "角" + invalidByte + "色"}
	d := s.Decide("acme", req) // seq 2
	if !d.Allowed || !reflect.DeepEqual(d.Matched, []string{policyID}) {
		t.Fatalf("decision = %+v", d)
	}

	// The caller edits the raw-content lists afterwards.
	req.Subject.Roles[0] = "篡改"
	req.Subject.Roles[2] = "角" + anotherInvalidByte + "色"
	d.Matched[0] = "forged"

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The chain over the raw bytes still validates end to end.
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("raw-content chain broken by caller edits: %v", err)
	}
	got := recs[1].Decision
	wantRoles := []string{"角 色", "普 通 角 色", "角" + invalidByte + "色"}
	if !reflect.DeepEqual(got.Request.Subject.Roles, wantRoles) {
		t.Fatalf("stored roles = %q, want byte-exact %q", got.Request.Subject.Roles, wantRoles)
	}
	if !reflect.DeepEqual(got.Decision.Matched, []string{policyID}) {
		t.Fatalf("stored matched = %q, want byte-exact %q", got.Decision.Matched, policyID)
	}

	// Editing the exported copy invalidates it against the checkpoint.
	recs[1].Decision.Request.Subject.Roles[2] = "角" + anotherInvalidByte + "色"
	if err := VerifyAudit("acme", recs, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("edited raw-content export err = %v, want ErrInvalidRange", err)
	}
}
