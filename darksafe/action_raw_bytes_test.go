// Regression coverage for action names matched by their exact submitted
// bytes. One policy set can carry different effects for different actions
// of the same subject on the same resource; the request's action selects
// which policies may enter the hit list, and only the request's own raw
// action bytes do the selecting. An action that differs by case, by a
// surrounding space, or by one invalid byte is a DIFFERENT action: it must
// never borrow another action's allow policy, never fall under another
// action's deny policy, and never be normalized into another action before
// matching. The only invalid action is the empty string, and it is an
// envelope rejection decided before any policy is consulted.
//
// Every request here runs in one organization with an enabled subject and
// a legal scope, so each outcome is genuinely produced by action matching,
// not by an envelope rejection. The audit half pins that the raw action
// bytes and the full explanation survive the chain, an archive round trip
// and offline review: a decision recomputed from the saved material must
// come out identical, and records that differ only in the action must stay
// distinguishable rather than collapse onto one replaced character.
package darksafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Action fixture shared by this file: one enabled subject, one ledger, one
// legal scope, one organization.
const (
	actionOrg        = "acme"
	actionSubjectID  = "svc-ledger-writer"
	actionResourceID = "ledger-2026"
	actionScope      = "acme/factory/ledger"
)

// actionRequest builds a complete, envelope-valid organization request that
// varies only the action.
func actionRequest(action string) OrgRequest {
	return OrgRequest{
		SubjectOrg:  actionOrg,
		ResourceOrg: actionOrg,
		Subject:     Subject{ID: actionSubjectID, Kind: "service"}, // enabled
		Resource:    Resource{ID: actionResourceID, Scope: actionScope},
		Action:      action,
	}
}

// actionPolicy builds one policy for the shared subject, resource scope and
// the given action and effect.
func actionPolicy(id, action string, effect Effect) Policy {
	return Policy{ID: id, Subject: actionSubjectID, Action: action, Scope: actionScope, Effect: effect}
}

// wantActionDecision pins the complete explanation: verdict, reason, exact
// hit list (order significant) and the version actually evaluated.
func wantActionDecision(t *testing.T, got Decision, allowed bool, reason string, matched []string, version int, context string) {
	t.Helper()
	if got.Allowed != allowed {
		t.Fatalf("%s: allowed = %v, want %v; decision = %+v", context, got.Allowed, allowed, got)
	}
	if got.Reason != reason {
		t.Fatalf("%s: reason = %q, want %q; decision = %+v", context, got.Reason, reason, got)
	}
	if got.Version != version {
		t.Fatalf("%s: version = %d, want %d; decision = %+v", context, got.Version, version, got)
	}
	gotMatched := got.Matched
	if len(gotMatched) == 0 {
		gotMatched = nil
	}
	wantMatched := matched
	if len(wantMatched) == 0 {
		wantMatched = nil
	}
	if !reflect.DeepEqual(gotMatched, wantMatched) {
		t.Fatalf("%s: matched = %q, want %q; decision = %+v", context, gotMatched, wantMatched, got)
	}
}

// TestActionEffectsStayWithTheirOwnAction pins the basic isolation: one
// published version holds an allow for "read" and a deny for "write" for
// the same subject and resource. The read is allowed by its own policy
// alone, the write is denied by its own policy alone, and an action with no
// policy at all gets the default denial with an empty hit list. Neither
// action's policy may appear in the other action's explanation.
func TestActionEffectsStayWithTheirOwnAction(t *testing.T) {
	s := NewStore()
	version, err := s.Publish(actionOrg, 0, []Policy{
		actionPolicy("p-read-allow", "read", EffectAllow),
		actionPolicy("p-write-deny", "write", EffectDeny),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	read := s.Decide(actionOrg, actionRequest("read"))
	wantActionDecision(t, read, true, "matched allow policy",
		[]string{"p-read-allow"}, 1, "read hits only its own allow")

	write := s.Decide(actionOrg, actionRequest("write"))
	wantActionDecision(t, write, false, "matched deny policy",
		[]string{"p-write-deny"}, 1, "write hits only its own deny")

	// No policy names "delete": the default denial applies, explained as the
	// missing allow with an empty hit list, still at the evaluated version.
	del := s.Decide(actionOrg, actionRequest("delete"))
	wantActionDecision(t, del, false, "no matching allow policy",
		nil, 1, "delete has no policy at all")

	// The hit lists are disjoint by construction of the matcher; pin it so a
	// future "close enough" action comparison cannot leak a policy across.
	for _, hit := range read.Matched {
		if hit == "p-write-deny" {
			t.Fatalf("read borrowed the write deny policy: %+v", read)
		}
	}
	for _, hit := range write.Matched {
		if hit == "p-read-allow" {
			t.Fatalf("write borrowed the read allow policy: %+v", write)
		}
	}
}

// TestDenyOverridesOnlyWithinTheMatchedAction pins the scope of deny
// precedence: a deny policy denies only requests whose action actually
// matches it. A deny published on "write" cannot override the allow on
// "read", while one action carrying both an allow and a deny is denied,
// with BOTH identifiers kept in the hit list, sorted ascending, at the
// version actually evaluated.
func TestDenyOverridesOnlyWithinTheMatchedAction(t *testing.T) {
	s := NewStore()
	// The commit pair is submitted deny-first so the ascending order of the
	// reported hit list is produced by the decision, not by the submission.
	version, err := s.Publish(actionOrg, 0, []Policy{
		actionPolicy("p-write-deny", "write", EffectDeny),
		actionPolicy("p-commit-deny", "commit", EffectDeny),
		actionPolicy("p-commit-allow", "commit", EffectAllow),
		actionPolicy("p-read-allow", "read", EffectAllow),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	// Two deny policies exist in the same version, but neither matches the
	// read action: the read allow stands.
	read := s.Decide(actionOrg, actionRequest("read"))
	wantActionDecision(t, read, true, "matched allow policy",
		[]string{"p-read-allow"}, 1, "denies on other actions cannot override the read allow")

	// The commit action genuinely hits both effects: deny wins, and the
	// explanation keeps both identifiers in ascending order.
	commit := s.Decide(actionOrg, actionRequest("commit"))
	wantActionDecision(t, commit, false, "matched deny policy",
		[]string{"p-commit-allow", "p-commit-deny"}, 1, "allow and deny on the same action")

	// The write deny, evaluated alone, reports only its own identifier: the
	// commit pair does not leak into a request that cannot match it.
	write := s.Decide(actionOrg, actionRequest("write"))
	wantActionDecision(t, write, false, "matched deny policy",
		[]string{"p-write-deny"}, 1, "write deny stands alone")
}

// TestActionNamesDifferingByCaseOrSpaceStayDistinct pins that comparison is
// by exact bytes, not by any case folding or trimming: "read", "Read",
// " read" and "read " are four different actions, each matched only by its
// own policy, and an unlisted casing ("READ") falls to the default denial.
// All of them are non-empty, so every request reaches policy evaluation at
// the published version — none is rejected as an invalid request.
func TestActionNamesDifferingByCaseOrSpaceStayDistinct(t *testing.T) {
	variants := []struct {
		action   string
		policyID string
	}{
		{"read", "p-act-lower"},
		{"Read", "p-act-title"},
		{" read", "p-act-lead-space"},
		{"read ", "p-act-trail-space"},
	}
	policies := make([]Policy, 0, len(variants))
	for _, v := range variants {
		policies = append(policies, actionPolicy(v.policyID, v.action, EffectAllow))
	}
	s := NewStore()
	version, err := s.Publish(actionOrg, 0, policies)
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	for _, v := range variants {
		got := s.Decide(actionOrg, actionRequest(v.action))
		wantActionDecision(t, got, true, "matched allow policy",
			[]string{v.policyID}, 1, "action "+showAction(v.action))
	}

	// A casing no policy carries is the default denial, not a folded match
	// on any of the four published variants.
	shout := s.Decide(actionOrg, actionRequest("READ"))
	wantActionDecision(t, shout, false, "no matching allow policy",
		nil, 1, "unlisted casing must not fold onto a published action")
}

// showAction renders an action for test failure messages, making
// surrounding spaces visible.
func showAction(action string) string {
	return "[" + action + "]"
}

// rawByteActions returns the three byte-different-but-display-conflated
// action names used by this file. The surrounding content is identical;
// exactly one middle position differs: a lone 0xFF byte, a lone 0xFE byte,
// and a genuine U+FFFD code point (valid UTF-8, three bytes EF BF BD).
func rawByteActions() (withFF, withFE, withReplacement string) {
	return "ledger-" + invalidByte + "-read",
		"ledger-" + anotherInvalidByte + "-read",
		"ledger-" + replacementRune + "-read"
}

// TestRawByteActionsStayDistinctInDecisions pins the authorization contract
// for actions carrying bytes that are not valid UTF-8: matching is against
// the request's raw action bytes, so a replacement of 0xFF with 0xFE (or
// with a genuine U+FFFD) can never make one action ride another action's
// allow or deny. None of the three is empty and none is invalid input —
// each reaches policy evaluation at the published version.
func TestRawByteActionsStayDistinctInDecisions(t *testing.T) {
	actFF, actFE, actFD := rawByteActions()

	// Document the premise this regression guards: Go string comparison
	// distinguishes the three actions, while encoding/json renders the two
	// lone invalid bytes identically. Any normalization or replacement at
	// the authorization boundary would collapse the first two.
	if actFF == actFE || actFF == actFD || actFE == actFD {
		t.Fatalf("test premise broken: actions must differ pairwise: %q %q %q", actFF, actFE, actFD)
	}
	jFF, _ := json.Marshal(actFF)
	jFE, _ := json.Marshal(actFE)
	if !bytes.Equal(jFF, jFE) {
		t.Fatalf("test premise broken: JSON renders the invalid bytes differently: %s vs %s", jFF, jFE)
	}

	s := NewStore()
	version, err := s.Publish(actionOrg, 0, []Policy{
		actionPolicy("p-act-ff-allow", actFF, EffectAllow),
		actionPolicy("p-act-fe-deny", actFE, EffectDeny),
		// No policy at all for the genuine-U+FFFD action.
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	// The 0xFF action is allowed by its own policy alone, at the published
	// version: a non-empty raw-byte action is a valid request, never an
	// envelope rejection.
	allowFF := s.Decide(actionOrg, actionRequest(actFF))
	wantActionDecision(t, allowFF, true, "matched allow policy",
		[]string{"p-act-ff-allow"}, 1, "0xFF action hits its own allow")

	// The 0xFE action is denied by ITS OWN deny policy. The 0xFF action's
	// allow must not travel with it, and the explanation is the policy-deny
	// reason rather than the default denial.
	denyFE := s.Decide(actionOrg, actionRequest(actFE))
	wantActionDecision(t, denyFE, false, "matched deny policy",
		[]string{"p-act-fe-deny"}, 1, "0xFE action hits only its own deny")

	// The genuine-U+FFFD action has no policy: the default denial with an
	// empty hit list. Neither look-alike action's policy may be its basis.
	noPolicyFD := s.Decide(actionOrg, actionRequest(actFD))
	wantActionDecision(t, noPolicyFD, false, "no matching allow policy",
		nil, 1, "U+FFFD action has no policy at all")

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

// TestEmptyActionRejectedBeforePolicyMatching pins the one invalid action:
// the empty string is an envelope rejection decided before any policy is
// consulted — version 0, no matched policies, the "missing action" reason —
// even when a published allow policy would otherwise grant the request.
// The policy side mirrors this: a policy carrying an empty action cannot be
// published at all, so no published version can ever contain one.
func TestEmptyActionRejectedBeforePolicyMatching(t *testing.T) {
	s := NewStore()
	version, err := s.Publish(actionOrg, 0, []Policy{
		actionPolicy("p-read-allow", "read", EffectAllow),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	// The same request with the action emptied: denied by the envelope, not
	// by policy evaluation. The published allow cannot be borrowed by an
	// action-less request.
	d := s.Decide(actionOrg, actionRequest(""))
	wantActionDecision(t, d, false, "missing action", nil, 0,
		"empty action is an envelope rejection")

	// The allow really was there to borrow: the identical request carrying
	// the action is allowed. The denial above is caused by the empty action
	// and nothing else.
	allowed := s.Decide(actionOrg, actionRequest("read"))
	wantActionDecision(t, allowed, true, "matched allow policy",
		[]string{"p-read-allow"}, 1, "same request with the action present")

	// The envelope denial is audited like any other decision, preserving the
	// empty action and the version-0 explanation.
	recs, cp, err := s.AuditExport(actionOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(actionOrg, recs, cp); err != nil {
		t.Fatalf("chain must verify: %v", err)
	}
	var found bool
	for _, r := range recs {
		if r.Kind == AuditDecision && r.Decision != nil && r.Decision.Request.Action == "" {
			found = true
			if !reflect.DeepEqual(r.Decision.Decision, d) {
				t.Fatalf("empty-action record = %+v, want returned %+v", r.Decision.Decision, d)
			}
		}
	}
	if !found {
		t.Fatal("the empty-action envelope denial was not recorded in the audit chain")
	}

	// A policy with an empty action is rejected at publish and consumes no
	// version, so the empty action can never become a published grant.
	if _, err := s.Publish(actionOrg, 1, []Policy{
		actionPolicy("p-empty-action", "", EffectAllow),
	}); !errors.Is(err, ErrInvalidPolicySet) {
		t.Fatalf("publish empty-action policy err = %v, want ErrInvalidPolicySet", err)
	}
	if got := s.CurrentVersion(actionOrg); got != 1 {
		t.Fatalf("current version = %d, want 1 after rejected publish", got)
	}
}

// TestActionDecisionsSurviveArchiveAndOfflineReview pins the audit and
// offline-review half of the contract. Decisions that differ only in the
// request's action bytes — including the 0xFF/0xFE/U+FFFD triple and the
// empty-action envelope rejection — are exported, archived to bytes and
// read back. The stored records must keep the raw action bytes and the full
// explanation, stay pairwise distinguishable in the material, and the
// offline review of every one must recompute the identical decision from
// the saved action and the recorded policy version, marked consistent.
func TestActionDecisionsSurviveArchiveAndOfflineReview(t *testing.T) {
	actFF, actFE, actFD := rawByteActions()

	s := NewStore()
	version, err := s.Publish(actionOrg, 0, []Policy{
		actionPolicy("p-read-allow", "read", EffectAllow),
		actionPolicy("p-act-ff-allow", actFF, EffectAllow),
		actionPolicy("p-act-fe-deny", actFE, EffectDeny),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	// Chain layout (seq 1 is the publish): one decision per action, each
	// reaching a different conclusion by action bytes alone, plus the
	// empty-action envelope rejection.
	//	seq 2  "read" allow        seq 3  0xFF action allow
	//	seq 4  0xFE action deny    seq 5  U+FFFD action default deny
	//	seq 6  "" missing action
	online := map[int]Decision{}
	decide := func(seq int, action string) {
		online[seq] = s.Decide(actionOrg, actionRequest(action))
	}
	decide(2, "read")
	decide(3, actFF)
	decide(4, actFE)
	decide(5, actFD)
	decide(6, "")

	wantActionBySeq := map[int]string{2: "read", 3: actFF, 4: actFE, 5: actFD, 6: ""}

	recs, cp, err := s.AuditExport(actionOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(actionOrg, recs, cp); err != nil {
		t.Fatalf("chain with raw-byte actions must verify: %v", err)
	}
	if len(recs) != 6 {
		t.Fatalf("exported %d records, want 6 (one publish + five decisions)", len(recs))
	}

	// Every decision record preserves the submitted action byte for byte and
	// the decision actually returned.
	for seq, wantAction := range wantActionBySeq {
		r := recs[seq-1]
		if r.Kind != AuditDecision || r.Decision == nil {
			t.Fatalf("seq %d is not a decision record: %+v", seq, r)
		}
		if r.Decision.Request.Action != wantAction {
			t.Fatalf("seq %d stored action %q, want byte-exact %q", seq, r.Decision.Request.Action, wantAction)
		}
		if !reflect.DeepEqual(r.Decision.Decision, online[seq]) {
			t.Fatalf("seq %d stored decision %+v, want returned %+v", seq, r.Decision.Decision, online[seq])
		}
	}

	// Records that differ only in the action stay distinguishable in the
	// material: the three byte-confusable actions were not saved as one
	// replaced character, and their records' fingerprints differ pairwise.
	fps := map[string]int{}
	for _, seq := range []int{3, 4, 5} {
		fp := recs[seq-1].Fingerprint
		if other, dup := fps[fp]; dup {
			t.Fatalf("seqs %d and %d share fingerprint %s: distinct actions collapsed in the chain", other, seq, fp)
		}
		fps[fp] = seq
	}

	// The full export survives archiving and reading back unchanged.
	archive, err := EncodeAuditArchive(actionOrg, recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if again, err := EncodeAuditArchive(actionOrg, recs, cp); err != nil || !bytes.Equal(again, archive) {
		t.Fatalf("encoding must be deterministic: equal=%v err=%v", bytes.Equal(again, archive), err)
	}
	decoded, err := DecodeAuditArchive(archive, actionOrg, cp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, recs) {
		t.Fatalf("decoded records differ from export:\n got %+v\nwant %+v", decoded, recs)
	}
	// The raw action bytes in particular survived the archive byte for byte.
	for seq, wantAction := range wantActionBySeq {
		if got := decoded[seq-1].Decision.Request.Action; got != wantAction {
			t.Fatalf("seq %d action after archive = %q, want byte-exact %q", seq, got, wantAction)
		}
	}

	// Offline review of every decision, run on the decoded material,
	// recomputes the identical allow or deny from the saved action and the
	// recorded policy version, and marks original and recomputed consistent.
	for seq := 2; seq <= 6; seq++ {
		review, err := RecheckDecisionOffline(actionOrg, decoded, cp, seq)
		if err != nil {
			t.Fatalf("offline review seq %d: %v", seq, err)
		}
		if !review.Consistent {
			t.Fatalf("seq %d review inconsistent: original %+v vs recomputed %+v",
				seq, review.Original, review.Recomputed)
		}
		if !reflect.DeepEqual(review.Recomputed, online[seq]) {
			t.Fatalf("seq %d recomputed %+v != online decision %+v", seq, review.Recomputed, online[seq])
		}
		if review.Original.Version != online[seq].Version {
			t.Fatalf("seq %d recorded version %d, want the version decided online %d",
				seq, review.Original.Version, online[seq].Version)
		}
	}
}
