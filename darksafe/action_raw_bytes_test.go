// Regression coverage for action names compared by their exact submitted
// bytes. For organization-level decisions, the only requirement on an
// action is that it is non-empty; the request action and every policy
// action are then compared as raw strings. Two names that differ only in
// case, surrounding whitespace, or a single non-UTF-8 byte are different
// actions: a request for one may neither borrow another action's allow
// policy nor be blocked by another action's deny policy.
//
// The same policy set can therefore carry different effects for different
// actions, and deny-overrides applies only among the policies one request
// actually matches: when one action matches both its own allow and its own
// deny, both identifiers survive in the match list in ascending order and
// the decision is a deny; a deny for a sibling action never overrides an
// allow for the requested one. An action with no matching policy keeps the
// "no matching allow policy" default denial with an empty match list,
// while the empty string is an envelope rejection ("missing action") at
// version 0 before any policy is consulted.
//
// These tests pin that behavior through every usage that must preserve
// it: online Decide, historical Review, the audit records (which keep the
// request action's raw bytes and the full explanation), the lossless
// archive round trip, and offline recheck against the recorded version.
// The raw-byte probes are the three names displays can conflate — a lone
// 0xFF, a lone 0xFE, and a genuine U+FFFD code point — plus ordinary
// ASCII case and leading/trailing spaces, all of which must stay distinct
// within one organization.
package darksafe

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// rawByteActions returns three action names with an identical legal
// prefix that differ exactly at the byte position after it: a lone 0xFF
// byte, a lone 0xFE byte, and a genuine U+FFFD code point (valid UTF-8,
// three bytes EF BF BD).
func rawByteActions() (withFF, withFE, withReplacement string) {
	return "sync-" + invalidByte,
		"sync-" + anotherInvalidByte,
		"sync-" + replacementRune
}

// actionRequest builds a fully legal request for one enabled subject in
// one organization; only the action varies between calls.
func actionRequest(org, action string) OrgRequest {
	return OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     Subject{ID: "u1", Kind: "service"}, // enabled
		Resource:    Resource{ID: "r1", Scope: "org/a"},
		Action:      action,
	}
}

// TestActionCaseAndSpaceVariantsDoNotSharePolicies pins the ordinary
// confusion surface: case folding or whitespace trimming at the
// authorization boundary would let one action ride another's policy. Each
// variant gets only its own effect, and a similarly-spelled name with no
// policy gets the default denial rather than a borrowed allow.
func TestActionCaseAndSpaceVariantsDoNotSharePolicies(t *testing.T) {
	const org = "acme"
	s := NewStore()
	version, err := s.Publish(org, 0, []Policy{
		{ID: "p-read-allow", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "p-Read-deny", Subject: "u1", Action: "Read", Scope: "org/a", Effect: EffectDeny},
		{ID: "p-leading-space-allow", Subject: "u1", Action: " read", Scope: "org/a", Effect: EffectAllow},
		{ID: "p-trailing-space-deny", Subject: "u1", Action: "read ", Scope: "org/a", Effect: EffectDeny},
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1 (whitespace-only actions are non-empty and legal)", version, err)
	}

	cases := []struct {
		name    string
		action  string
		allowed bool
		reason  string
		matched []string
	}{
		{"lowercase allow", "read", true, "matched allow policy", []string{"p-read-allow"}},
		{"uppercase deny is its own action", "Read", false, "matched deny policy", []string{"p-Read-deny"}},
		{"leading space allow", " read", true, "matched allow policy", []string{"p-leading-space-allow"}},
		{"trailing space deny", "read ", false, "matched deny policy", []string{"p-trailing-space-deny"}},
		// Names sharing letters but no policy of their own: the neighboring
		// allows must not be borrowed.
		{"all caps has no policy", "READ", false, "no matching allow policy", []string{}},
		{"trimmed look-alike has no policy", "  read  ", false, "no matching allow policy", []string{}},
		{"unrelated action has no policy", "write", false, "no matching allow policy", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := s.Decide(org, actionRequest(org, tc.action))
			if d.Allowed != tc.allowed {
				t.Fatalf("action %q decision = %+v, want allowed=%v", tc.action, d, tc.allowed)
			}
			if d.Reason != tc.reason {
				t.Fatalf("action %q reason = %q, want %q", tc.action, d.Reason, tc.reason)
			}
			if !reflect.DeepEqual(d.Matched, tc.matched) {
				t.Fatalf("action %q matched = %q, want %q (no other action's policy may appear)",
					tc.action, d.Matched, tc.matched)
			}
			if d.Version != 1 {
				t.Fatalf("action %q version = %d, want the published version 1", tc.action, d.Version)
			}
			// Historical review of the same version must agree byte for byte.
			rd := s.Review(org, 1, actionRequest(org, tc.action))
			if !reflect.DeepEqual(rd, d) {
				t.Fatalf("review of action %q = %+v, want %+v", tc.action, rd, d)
			}
		})
	}
}

// TestRawByteActionsStayDistinctInDecisions covers the actions that cause
// real conflation: in the same organization, a lone 0xFF, a lone 0xFE and
// a genuine U+FFFD must never be rewritten to the same character before
// authorization. One action carries both an allow and a deny (both
// identifiers retained, sorted, deny wins); the second carries only a
// deny that must not block the third action's allow; and a name with no
// policy at all gets the empty-match default denial.
func TestRawByteActionsStayDistinctInDecisions(t *testing.T) {
	actFF, actFE, actFD := rawByteActions()

	// Document the premise this regression guards: Go string comparison
	// distinguishes the three actions, while encoding/json renders the two
	// lone invalid bytes identically. Any normalization at the
	// authorization boundary would collapse the first two.
	if actFF == actFE || actFF == actFD || actFE == actFD {
		t.Fatalf("test premise broken: actions must differ pairwise: %q %q %q", actFF, actFE, actFD)
	}
	jFF, _ := json.Marshal(actFF)
	jFE, _ := json.Marshal(actFE)
	if !bytes.Equal(jFF, jFE) {
		t.Fatalf("test premise broken: JSON renders the invalid bytes differently: %s vs %s", jFF, jFE)
	}

	const org = "acme"
	s := NewStore()
	version, err := s.Publish(org, 0, []Policy{
		// Action 1 matches BOTH its own allow and its own deny.
		{ID: "p-ff-allow", Subject: "u1", Action: actFF, Scope: "org/a", Effect: EffectAllow},
		{ID: "p-ff-deny", Subject: "u1", Action: actFF, Scope: "org/a", Effect: EffectDeny},
		// Action 2 has only a deny.
		{ID: "p-fe-deny", Subject: "u1", Action: actFE, Scope: "org/a", Effect: EffectDeny},
		// Action 3 has only an allow; action 2's deny must not reach it.
		{ID: "p-fd-allow", Subject: "u1", Action: actFD, Scope: "org/a", Effect: EffectAllow},
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want version 1", version, err)
	}

	// Action 1: deny-overrides is within the matched set; both identifiers
	// survive, sorted ascending, at the published version.
	ff := s.Decide(org, actionRequest(org, actFF))
	if ff.Allowed {
		t.Fatalf("0xFF action decision = %+v, want deny by its own deny policy", ff)
	}
	if ff.Reason != "matched deny policy" {
		t.Fatalf("0xFF action reason = %q, want %q", ff.Reason, "matched deny policy")
	}
	if want := []string{"p-ff-allow", "p-ff-deny"}; !reflect.DeepEqual(ff.Matched, want) {
		t.Fatalf("0xFF action matched = %q, want both ids sorted %q", ff.Matched, want)
	}
	if ff.Version != 1 {
		t.Fatalf("0xFF action version = %d, want 1", ff.Version)
	}

	// Action 2: only its own deny; action 1's policies never appear.
	fe := s.Decide(org, actionRequest(org, actFE))
	if fe.Allowed || fe.Reason != "matched deny policy" {
		t.Fatalf("0xFE action decision = %+v, want own-policy deny", fe)
	}
	if want := []string{"p-fe-deny"}; !reflect.DeepEqual(fe.Matched, want) {
		t.Fatalf("0xFE action matched = %q, want only %q", fe.Matched, want)
	}
	if fe.Version != 1 {
		t.Fatalf("0xFE action version = %d, want 1", fe.Version)
	}

	// Action 3: allowed solely by its own allow. The sibling action's deny
	// cannot override an allow it did not match.
	fd := s.Decide(org, actionRequest(org, actFD))
	if !fd.Allowed || fd.Reason != "matched allow policy" {
		t.Fatalf("U+FFFD action decision = %+v, want allowed by its own policy", fd)
	}
	if want := []string{"p-fd-allow"}; !reflect.DeepEqual(fd.Matched, want) {
		t.Fatalf("U+FFFD action matched = %q, want only %q", fd.Matched, want)
	}
	if fd.Version != 1 {
		t.Fatalf("U+FFFD action version = %d, want 1", fd.Version)
	}

	// A fourth, byte-different name with no policy: default denial, empty
	// (but non-nil) match list — none of the three look-alikes' effects are
	// borrowed. The request is valid, so it must not be rejected as a
	// missing or malformed action.
	none := s.Decide(org, actionRequest(org, "sync-x"))
	if none.Allowed || none.Reason != "no matching allow policy" {
		t.Fatalf("unmatched action decision = %+v, want no-matching-allow default deny", none)
	}
	if len(none.Matched) != 0 || none.Matched == nil {
		t.Fatalf("unmatched action matched = %q, want non-nil empty list", none.Matched)
	}
	if none.Version != 1 {
		t.Fatalf("unmatched action version = %d, want 1", none.Version)
	}

	// None of the non-empty raw actions is an invalid request: the reason
	// is always policy evaluation's, never the missing-action rejection.
	for i, d := range []Decision{ff, fe, fd, none} {
		if d.Reason == "missing action" {
			t.Fatalf("decision %d treated a non-empty raw action as missing: %+v", i, d)
		}
	}
}

// TestMissingActionIsEnvelopeDenialBeforePolicyMatching pins the empty
// value's separate meaning: it is rejected before policy matching at
// version 0 with no matched policy, and can never obtain access from a
// published allow policy for a non-empty action.
func TestMissingActionIsEnvelopeDenialBeforePolicyMatching(t *testing.T) {
	const org = "acme"
	s := NewStore()
	if _, err := s.Publish(org, 0, []Policy{
		{ID: "p-read-allow", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
	}); err != nil {
		t.Fatal(err)
	}

	req := actionRequest(org, "read")
	req.Action = ""
	d := s.Decide(org, req)
	if d.Allowed {
		t.Fatalf("empty-action decision = %+v, must never be allowed by a published policy", d)
	}
	if d.Reason != "missing action" {
		t.Fatalf("empty-action reason = %q, want %q", d.Reason, "missing action")
	}
	if d.Version != 0 {
		t.Fatalf("empty-action version = %d, want 0 (rejected before any published version is used)", d.Version)
	}
	if d.Matched != nil {
		t.Fatalf("empty-action matched = %q, want no matched policies (nil)", d.Matched)
	}

	// Historical review applies the same envelope rule before evaluating.
	rd := s.Review(org, 1, req)
	if !reflect.DeepEqual(rd, d) {
		t.Fatalf("review of empty action = %+v, want %+v", rd, d)
	}
}

// TestActionDistinctionSurvivesAuditArchiveAndOfflineReview drives the
// full audit path: the raw-byte actions' decisions — including one that
// is allowed, one denied by a sibling action's policy set, the
// both-effects action and the empty-match default — are exported with
// their request bytes and explanations intact, stay pairwise
// distinguishable as material, survive the archive round trip, and
// recompute to the same conclusions offline at the recorded version. An
// empty-action envelope denial is included so its version-0 record takes
// the same path.
func TestActionDistinctionSurvivesAuditArchiveAndOfflineReview(t *testing.T) {
	actFF, actFE, actFD := rawByteActions()
	const org = "acme"

	s := NewStore()
	if _, err := s.Publish(org, 0, []Policy{
		{ID: "p-ff-allow", Subject: "u1", Action: actFF, Scope: "org/a", Effect: EffectAllow},
		{ID: "p-ff-deny", Subject: "u1", Action: actFF, Scope: "org/a", Effect: EffectDeny},
		{ID: "p-fe-deny", Subject: "u1", Action: actFE, Scope: "org/a", Effect: EffectDeny},
		{ID: "p-fd-allow", Subject: "u1", Action: actFD, Scope: "org/a", Effect: EffectAllow},
	}); err != nil {
		t.Fatal(err)
	}

	// Chain layout (seq 1 is the publish):
	//	seq 2 actFF deny (both own ids)     seq 3 actFE deny (own id)
	//	seq 4 actFD allow (own id)          seq 5 "sync-x" default deny
	//	seq 6 empty action, envelope denial
	decided := map[int]Decision{}
	for _, item := range []struct {
		seq    int
		action string
	}{
		{2, actFF}, {3, actFE}, {4, actFD}, {5, "sync-x"},
	} {
		decided[item.seq] = s.Decide(org, actionRequest(org, item.action))
	}
	missingReq := actionRequest(org, actFF)
	missingReq.Action = ""
	decided[6] = s.Decide(org, missingReq)

	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 6 {
		t.Fatalf("exported %d records, want 6", len(recs))
	}
	if err := VerifyAudit(org, recs, cp); err != nil {
		t.Fatalf("chain with raw-byte actions must verify: %v", err)
	}

	// Every decision record preserves the request action's raw bytes and
	// exactly the decision returned online.
	wantAction := map[int]string{
		2: actFF, 3: actFE, 4: actFD, 5: "sync-x", 6: "",
	}
	for seq, action := range wantAction {
		got := recs[seq-1].Decision
		if got.Request.Action != action {
			t.Fatalf("seq %d stored action %q, want byte-exact %q", seq, got.Request.Action, action)
		}
		if !reflect.DeepEqual(got.Decision, decided[seq]) {
			t.Fatalf("seq %d stored decision %+v, want returned %+v", seq, got.Decision, decided[seq])
		}
	}

	// Records that differ ONLY in action (plus the conclusion that action
	// produces) must not be saved as the same character: their stored
	// actions and their fingerprints must be pairwise distinct, so the
	// material cannot make different actions look consistent.
	decisionSeqs := []int{2, 3, 4, 5}
	for i := 0; i < len(decisionSeqs); i++ {
		for j := i + 1; j < len(decisionSeqs); j++ {
			a, b := recs[decisionSeqs[i]-1], recs[decisionSeqs[j]-1]
			if a.Decision.Request.Action == b.Decision.Request.Action {
				t.Fatalf("seq %d and %d collapsed to the same action bytes", a.Seq, b.Seq)
			}
			if a.Fingerprint == b.Fingerprint {
				t.Fatalf("action-only-different seq %d and %d share fingerprint %s",
					a.Seq, b.Seq, a.Fingerprint)
			}
		}
	}

	// Full export -> archive -> re-read after the "instance ends": no store
	// is involved from here on.
	archive, err := EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatalf("encode archive: %v", err)
	}
	loaded, err := DecodeAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatalf("decode archive: %v", err)
	}
	if !reflect.DeepEqual(loaded, recs) {
		t.Fatalf("archive round trip changed action bytes or explanations:\n got %+v\nwant %+v", loaded, recs)
	}

	// Offline recheck at the recorded version reproduces each outcome —
	// allow stays allow, the sibling deny stays a deny, the both-effects
	// action keeps both ids, the default denial stays empty, and the
	// empty-action denial stays version 0 — and marks all consistent.
	for _, seq := range decisionSeqs {
		review, err := RecheckDecisionOffline(org, loaded, cp, seq)
		if err != nil {
			t.Fatalf("seq %d offline review: %v", seq, err)
		}
		if !review.Consistent {
			t.Fatalf("seq %d inconsistent: original=%+v recomputed=%+v",
				seq, review.Original, review.Recomputed)
		}
		if !reflect.DeepEqual(review.Original, decided[seq]) ||
			!reflect.DeepEqual(review.Recomputed, decided[seq]) {
			t.Fatalf("seq %d review = orig %+v recomputed %+v, want online %+v",
				seq, review.Original, review.Recomputed, decided[seq])
		}
	}

	missingReview, err := RecheckDecisionOffline(org, loaded, cp, 6)
	if err != nil {
		t.Fatalf("empty-action offline review: %v", err)
	}
	if !missingReview.Consistent {
		t.Fatalf("empty-action review inconsistent: %+v vs %+v",
			missingReview.Original, missingReview.Recomputed)
	}
	if missingReview.Recomputed.Allowed ||
		missingReview.Recomputed.Reason != "missing action" ||
		missingReview.Recomputed.Version != 0 ||
		missingReview.Recomputed.Matched != nil {
		t.Fatalf("empty-action recomputed = %+v, want missing-action denial at version 0 with no matches",
			missingReview.Recomputed)
	}
}
