// This file is the business regression guard for scope-name isolation in
// organization-level access decisions. Only an actual slash ('/', one byte
// 0x2F) is a hierarchy separator: every other byte in a scope segment —
// Chinese text, spaces, percent-encoded-looking text, even bytes that are
// not valid UTF-8 — is ordinary segment name content. Two requests that
// agree in organization, enabled subject, action and resource id and differ
// ONLY in the resource scope must therefore be decided by their scope's
// original bytes. No case folding, trimming, UTF-8 replacement, percent
// decoding or string-prefix reading may move an allow or a deny from one
// scope name onto a look-alike.
//
// The guarded rules, all already implemented by scopeMatches, validateScope
// and evaluate in org.go:
//
//   - An exact (non-recursive) policy matches its scope byte for byte and
//     nothing else. After one read allow is published, the same-named scope
//     is allowed by it alone; a differently cased name, a name with an extra
//     space, and a name that only LOOKS identical because a lone 0xFF, a lone
//     0xFE and a genuine U+FFFD display alike cannot borrow the allow. With
//     no other matching allow the answer is the default denial
//     "no matching allow policy", the hit list is empty, and the version is
//     still the published version that was evaluated.
//   - When each look-alike name carries its own policy, a request hits only
//     the policy bearing its own name; another name's deny never enters its
//     hit list or verdict. The single byte 0xFF, the single byte 0xFE and a
//     real U+FFFD rune stay three distinct scope names.
//   - A recursive policy keeps the exact same name comparison and additionally
//     covers true descendants: scopes equal to its raw name, or equal to its
//     raw name plus a slash plus more segments. A recursive allow on "org/a"
//     reaches "org/a/账本" but never "org/ab/账本"; the literal segment text
//     "%2F" is not a slash, so "org/a%2F账本" is a sibling, not a descendant.
//     Parents whose own name contains spaces or invalid UTF-8 bytes cover only
//     descendants formed by appending a slash to that parent's RAW name.
//   - Recursive denies obey the identical boundary: a deny that really matches
//     still overrides an allow ("matched deny policy", both identifiers kept in
//     ascending order, at the evaluated version), while a deny that misses the
//     requested scope name cannot affect another scope's access.
//   - A request scope containing consecutive slashes is rejected before any
//     policy is evaluated — never silently tidied and then matched. The
//     explanation is an explicit invalid-scope reason, version 0 and an empty
//     hit list, even when a published allow would cover the tidied path.
//
// All requests below are envelope-valid apart from the deliberately invalid
// scopes: the three organizations agree and the subject is enabled with its
// id, the resource id and the action present, so every outcome is genuinely
// produced by scope matching or the scope legality check, not by another
// envelope rule. The audit half pins that every explanation — allow, policy
// deny and default deny — survives the chain, historical review and offline
// review with its reason, hit identifiers and actual version intact.
package darksafe

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Shared fixture: one organization, one enabled subject, one resource id and
// one action. Requests differ only in the resource scope.
const (
	scopeNameOrg        = "acme"
	scopeNameSubjectID  = "svc-ledger-reader"
	scopeNameResourceID = "ledger-2026"
	scopeNameAction     = "read"
)

// scopeRequest builds an envelope-valid organization request carrying the
// given resource scope. Subject and resource organizations agree with the
// decision organization and the subject is enabled.
func scopeRequest(scope string) OrgRequest {
	return OrgRequest{
		SubjectOrg:  scopeNameOrg,
		ResourceOrg: scopeNameOrg,
		Subject:     Subject{ID: scopeNameSubjectID, Kind: "service"}, // enabled
		Resource:    Resource{ID: scopeNameResourceID, Scope: scope},
		Action:      scopeNameAction,
	}
}

// scopePolicy builds one policy for the shared subject, action and scope.
func scopePolicy(id, scope string, effect Effect, recursive bool) Policy {
	return Policy{ID: id, Subject: scopeNameSubjectID, Action: scopeNameAction,
		Scope: scope, Effect: effect, Recursive: recursive}
}

// wantScopeDecision pins the full four-field explanation: verdict, reason,
// exact hit list (order significant; nil and empty both mean "no hits") and
// the version actually evaluated.
func wantScopeDecision(t *testing.T, got Decision, allowed bool, reason string, matched []string, version int, context string) {
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

// TestExactScopeNameGrantsOnlyTheIdenticalScope is the core isolation guard:
// one exact read allow published on one scope name admits the same-named
// request and no look-alike. Each look-alike is a legal scope (so it reaches
// policy evaluation at the published version) but matches nothing, hence the
// default denial with an empty hit list at version 1 rather than the allow.
func TestExactScopeNameGrantsOnlyTheIdenticalScope(t *testing.T) {
	exact := "org/账本" // Chinese segment name, valid UTF-8
	s := NewStore()
	version, err := s.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("p-exact-allow", exact, EffectAllow, false),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	// The same-named request hits its own allow and is admitted by it alone.
	same := s.Decide(scopeNameOrg, scopeRequest(exact))
	wantScopeDecision(t, same, true, "matched allow policy",
		[]string{"p-exact-allow"}, 1, "byte-identical scope")

	// Every look-alike is a DIFFERENT scope name: it may not borrow the
	// allow, hits nothing, and falls through to the default denial at the
	// version actually evaluated. None is an invalid request shape.
	lookAlikes := map[string]string{
		"different case in ascii segment": "Org/账本",
		"extra leading space":             " org/账本",
		"extra trailing space":            "org/账本 ",
		"extra space inside the name":     "org/账 本",
		"extra slash-separated space seg": "org/ /账本",
	}
	for name, scope := range lookAlikes {
		d := s.Decide(scopeNameOrg, scopeRequest(scope))
		wantScopeDecision(t, d, false, "no matching allow policy",
			nil, 1, name+" must not match "+showScope(exact))
	}

	// The allow really was available: re-deciding the exact scope after the
	// look-alikes still admits, proving the denials came from name mismatch.
	again := s.Decide(scopeNameOrg, scopeRequest(exact))
	wantScopeDecision(t, again, true, "matched allow policy",
		[]string{"p-exact-allow"}, 1, "exact scope after look-alike probes")
}

// rawByteScopes returns the three byte-different-but-display-conflated scope
// names used by this file. The path shape is identical and exactly one
// segment byte differs: a lone 0xFF, a lone 0xFE, and a genuine U+FFFD code
// point (valid UTF-8, the three bytes EF BF BD).
func rawByteScopes() (withFF, withFE, withReplacement string) {
	return "org/账" + invalidByte + "本",
		"org/账" + anotherInvalidByte + "本",
		"org/账" + replacementRune + "本"
}

// TestRawByteScopeNamesStayDistinct guards the display-confusable triple in
// the scope path: JSON renders the lone 0xFF and lone 0xFE identically, but
// authorization compares raw bytes. With one exact allow on the 0xFF name,
// neither the 0xFE name nor the genuine-U+FFFD name (nor the 0xFF name with
// an extra space) may be admitted; each gets the default denial at the
// evaluated version.
func TestRawByteScopeNamesStayDistinct(t *testing.T) {
	scopeFF, scopeFE, scopeFD := rawByteScopes()

	// Document the premise: Go distinguishes all three, while encoding/json
	// collapses the two lone invalid bytes to one rendering. Any matching
	// layer that normalized before comparing would fail this regression.
	if scopeFF == scopeFE || scopeFF == scopeFD || scopeFE == scopeFD {
		t.Fatalf("test premise broken: scopes must differ pairwise: %q %q %q", scopeFF, scopeFE, scopeFD)
	}
	jFF, _ := json.Marshal(scopeFF)
	jFE, _ := json.Marshal(scopeFE)
	if !bytes.Equal(jFF, jFE) {
		t.Fatalf("test premise broken: JSON renders the invalid bytes differently: %s vs %s", jFF, jFE)
	}

	s := NewStore()
	version, err := s.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("p-scope-ff-allow", scopeFF, EffectAllow, false),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	allowFF := s.Decide(scopeNameOrg, scopeRequest(scopeFF))
	wantScopeDecision(t, allowFF, true, "matched allow policy",
		[]string{"p-scope-ff-allow"}, 1, "0xFF scope hits its own allow")

	// The look-alikes are legal scopes that match nothing: default denial,
	// empty hit list, but still evaluated at published version 1 — never an
	// envelope rejection and never the 0xFF allow.
	lookAlikes := map[string]string{
		"lone 0xFE look-alike":          scopeFE,
		"genuine U+FFFD look-alike":     scopeFD,
		"0xFF name with a trailing sp.": scopeFF + " ",
		"0xFF name with a leading sp.":  " " + scopeFF,
	}
	for name, scope := range lookAlikes {
		d := s.Decide(scopeNameOrg, scopeRequest(scope))
		wantScopeDecision(t, d, false, "no matching allow policy",
			nil, 1, name)
		for _, hit := range d.Matched {
			if hit == "p-scope-ff-allow" {
				t.Fatalf("%s borrowed the 0xFF allow: %+v", name, d)
			}
		}
	}
}

// TestEachScopeNameHitsOnlyItsOwnPolicy publishes one policy per look-alike
// name in a single version and proves requests cannot cross names. The 0xFF
// name allows, the 0xFE name denies and the genuine-U+FFFD name also allows:
// each request must hit its own identifier and its own effect alone. The
// 0xFE deny in particular must never be reported on either of the other two
// requests.
func TestEachScopeNameHitsOnlyItsOwnPolicy(t *testing.T) {
	scopeFF, scopeFE, scopeFD := rawByteScopes()
	s := NewStore()
	version, err := s.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("p-scope-ff-allow", scopeFF, EffectAllow, false),
		scopePolicy("p-scope-fe-deny", scopeFE, EffectDeny, false),
		scopePolicy("p-scope-fd-allow", scopeFD, EffectAllow, false),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	dFF := s.Decide(scopeNameOrg, scopeRequest(scopeFF))
	wantScopeDecision(t, dFF, true, "matched allow policy",
		[]string{"p-scope-ff-allow"}, 1, "0xFF scope hits only its own allow")

	dFE := s.Decide(scopeNameOrg, scopeRequest(scopeFE))
	wantScopeDecision(t, dFE, false, "matched deny policy",
		[]string{"p-scope-fe-deny"}, 1, "0xFE scope hits only its own deny")

	dFD := s.Decide(scopeNameOrg, scopeRequest(scopeFD))
	wantScopeDecision(t, dFD, true, "matched allow policy",
		[]string{"p-scope-fd-allow"}, 1, "U+FFFD scope hits only its own allow")

	// A fourth, unlisted but legal name gets the default denial and must not
	// pick up ANY of the three identifiers — especially not the 0xFE deny.
	unlisted := s.Decide(scopeNameOrg, scopeRequest("org/账"+replacementRune+"目"))
	wantScopeDecision(t, unlisted, false, "no matching allow policy",
		nil, 1, "unlisted scope borrows no policy")
}

// TestRecursiveScopeCoversRealDescendantsOnly pins the recursive boundary
// using raw, byte-exact parent names. A recursive allow on "org/a":
//
//   - covers the parent itself and a true child carrying a Chinese segment;
//   - does not cover a same-prefix sibling segment ("org/ab/账本"), since only
//     a real slash starts a child segment;
//   - does not cover "org/a%2F账本": "%2F" is three literal characters in a
//     segment name, never decoded into a slash.
func TestRecursiveScopeCoversRealDescendantsOnly(t *testing.T) {
	parent := "org/a"
	s := NewStore()
	version, err := s.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("p-rec-allow", parent, EffectAllow, true),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	covered := map[string]string{
		"parent itself":     parent,
		"chinese child":     "org/a/账本",
		"deeper descendant": "org/a/账本/明细",
		"child with spaces": "org/a/账 本 ",
	}
	for name, scope := range covered {
		d := s.Decide(scopeNameOrg, scopeRequest(scope))
		wantScopeDecision(t, d, true, "matched allow policy",
			[]string{"p-rec-allow"}, 1, name)
	}

	notCovered := map[string]string{
		"sibling prefix segment": "org/ab/账本",
		"literal %2F in segment": "org/a%2F账本",
		"percent-encoded deeper": "org/a%2F账本/明细",
		"ancestor":               "org",
		"space-padded lookalike": "org/a /账本",
		"uppercase lookalike":    "org/A/账本",
	}
	for name, scope := range notCovered {
		d := s.Decide(scopeNameOrg, scopeRequest(scope))
		wantScopeDecision(t, d, false, "no matching allow policy",
			nil, 1, name)
	}
}

// TestRecursiveParentWithSpacesOrRawBytesOnlyCoversRawChildren proves the
// descendant prefix is the parent's RAW name plus a slash, even when the
// parent name itself contains spaces or invalid UTF-8 bytes. A child formed
// by appending a slash to a display-alike name is outside the subtree.
func TestRecursiveParentWithSpacesOrRawBytesOnlyCoversRawChildren(t *testing.T) {
	spacedParent := "org/账 本"
	rawParent := "org/账" + invalidByte + "本"

	cases := []struct {
		name    string
		parent  string
		child   string
		outside []string
	}{
		{
			name:   "parent name contains a space",
			parent: spacedParent,
			child:  spacedParent + "/明细",
			outside: []string{
				"org/账/本/明细",                       // space reinterpreted as a slash is not allowed
				"org/账 本x/明细",                      // one extra byte in the segment
				"org/账 本 /明细",                      // trailing space before the separator
				"org/账%20本/明细",                     // literal %20, not the space
				"org/账" + replacementRune + "本/明细", // U+FFFD, not the space
			},
		},
		{
			name:   "parent name contains a lone 0xFF byte",
			parent: rawParent,
			child:  rawParent + "/明细",
			outside: []string{
				"org/账" + anotherInvalidByte + "本/明细", // 0xFE parent prefix
				"org/账" + replacementRune + "本/明细",    // genuine U+FFFD prefix
				"org/账" + invalidByte + "本x/明细",       // longer raw segment
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			version, err := s.Publish(scopeNameOrg, 0, []Policy{
				scopePolicy("p-rec-raw", tc.parent, EffectAllow, true),
			})
			if err != nil || version != 1 {
				t.Fatalf("publish = %d, %v; want 1, nil", version, err)
			}

			atParent := s.Decide(scopeNameOrg, scopeRequest(tc.parent))
			wantScopeDecision(t, atParent, true, "matched allow policy",
				[]string{"p-rec-raw"}, 1, "read at the raw parent itself")

			child := s.Decide(scopeNameOrg, scopeRequest(tc.child))
			wantScopeDecision(t, child, true, "matched allow policy",
				[]string{"p-rec-raw"}, 1, "child formed by raw parent name plus a slash")

			for _, scope := range tc.outside {
				d := s.Decide(scopeNameOrg, scopeRequest(scope))
				wantScopeDecision(t, d, false, "no matching allow policy",
					nil, 1, "outside subtree: "+showScope(scope))
			}
		})
	}
}

// TestRecursiveDenyBoundaryIsByteExact verifies recursive denies under the
// same name rules. A recursive deny that really covers the request still
// overrides an exact allow on the child, with BOTH identifiers kept in
// ascending order at the evaluated version. But a recursive deny on a
// look-alike parent — org/a rather than org/a%2F账本's true parent, or a
// parent whose name carries 0xFE where the request carries 0xFF — does not
// match and cannot change the other scope's allow.
func TestRecursiveDenyBoundaryIsByteExact(t *testing.T) {
	// Real conflict: recursive deny on org/a covers org/a/账本 even though an
	// exact allow sits on the child.
	s := NewStore()
	version, err := s.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("a-child-allow", "org/a/账本", EffectAllow, false),
		scopePolicy("z-parent-deny", "org/a", EffectDeny, true),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}
	conflict := s.Decide(scopeNameOrg, scopeRequest("org/a/账本"))
	wantScopeDecision(t, conflict, false, "matched deny policy",
		[]string{"a-child-allow", "z-parent-deny"}, 1, "real recursive deny overrides child allow")

	// Submission order must not move the explanation.
	s2 := NewStore()
	if v, err := s2.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("z-parent-deny", "org/a", EffectDeny, true),
		scopePolicy("a-child-allow", "org/a/账本", EffectAllow, false),
	}); err != nil || v != 1 {
		t.Fatalf("reordered publish = %d, %v", v, err)
	}
	reordered := s2.Decide(scopeNameOrg, scopeRequest("org/a/账本"))
	if !reflect.DeepEqual(reordered, conflict) {
		t.Fatalf("policy submission order changed the decision:\n%+v\n%+v", conflict, reordered)
	}

	// Same-prefix miss: the deny on org/a must not reach org/ab/账本. The
	// sibling scope's own exact allow stands alone.
	s3 := NewStore()
	if v, err := s3.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("z-parent-deny", "org/a", EffectDeny, true),
		scopePolicy("ab-exact-allow", "org/ab/账本", EffectAllow, false),
	}); err != nil || v != 1 {
		t.Fatalf("sibling publish = %d, %v", v, err)
	}
	sibling := s3.Decide(scopeNameOrg, scopeRequest("org/ab/账本"))
	wantScopeDecision(t, sibling, true, "matched allow policy",
		[]string{"ab-exact-allow"}, 1, "recursive deny on org/a misses org/ab")

	// Literal-%2F miss: org/a%2F账本 is not under org/a, so its own allow
	// cannot be overridden by the org/a recursive deny.
	s4 := NewStore()
	if v, err := s4.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("z-parent-deny", "org/a", EffectDeny, true),
		scopePolicy("enc-exact-allow", "org/a%2F账本", EffectAllow, false),
	}); err != nil || v != 1 {
		t.Fatalf("encoded publish = %d, %v", v, err)
	}
	encoded := s4.Decide(scopeNameOrg, scopeRequest("org/a%2F账本"))
	wantScopeDecision(t, encoded, true, "matched allow policy",
		[]string{"enc-exact-allow"}, 1, "literal %2F scope is outside org/a's deny subtree")

	// Raw-byte miss: a recursive deny whose parent carries 0xFE must not reach
	// a child of the genuinely-different 0xFF-named parent.
	scopeFF, scopeFE, _ := rawByteScopes()
	s5 := NewStore()
	if v, err := s5.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("rec-deny-fe", scopeFE, EffectDeny, true),
		scopePolicy("child-allow-ff", scopeFF+"/明细", EffectAllow, false),
	}); err != nil || v != 1 {
		t.Fatalf("raw-byte publish = %d, %v", v, err)
	}
	rawChild := s5.Decide(scopeNameOrg, scopeRequest(scopeFF+"/明细"))
	wantScopeDecision(t, rawChild, true, "matched allow policy",
		[]string{"child-allow-ff"}, 1, "0xFE recursive deny misses the 0xFF subtree")
}

// TestConsecutiveSlashScopeRejectedBeforePolicyEvaluation guards the
// legality gate: consecutive slashes make a request scope illegal and the
// request is denied before policy evaluation, with an explicit invalid-scope
// reason, version 0 and an empty hit list. The path must not be tidied and
// then matched against the published allow that covers the normalized form.
func TestConsecutiveSlashScopeRejectedBeforePolicyEvaluation(t *testing.T) {
	s := NewStore()
	// An allow that WOULD cover every normalized form used below, proving the
	// rejection happens before it could be consulted.
	version, err := s.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("p-wide-allow", "org/a", EffectAllow, true),
		scopePolicy("p-ledger-allow", "org/a/账本", EffectAllow, false),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	illegal := []struct {
		name  string
		scope string
	}{
		{"double slash in the middle", "org//a/账本"},
		{"double slash before leaf", "org/a//账本"},
		{"triple slash", "org///a"},
		{"leading slash", "/org/a/账本"},
		{"trailing slash", "org/a/账本/"},
		{"only slashes", "//"},
	}
	for _, tc := range illegal {
		d := s.Decide(scopeNameOrg, scopeRequest(tc.scope))
		if d.Allowed {
			t.Fatalf("%s: request was allowed after tidying: %+v", tc.name, d)
		}
		if d.Version != 0 {
			t.Fatalf("%s: version = %d, want 0 (no published version may be used); decision = %+v",
				tc.name, d.Version, d)
		}
		if len(d.Matched) != 0 {
			t.Fatalf("%s: matched = %q, want no policy hits before evaluation", tc.name, d.Matched)
		}
		if !strings.HasPrefix(d.Reason, "invalid scope") {
			t.Fatalf("%s: reason = %q, want an invalid-scope rejection", tc.name, d.Reason)
		}
		namesConsecutive := strings.Contains(d.Reason, "consecutive slashes")
		namesLeadingTrailing := strings.Contains(d.Reason, "leading or trailing slash")
		if !namesConsecutive && !namesLeadingTrailing {
			t.Fatalf("%s: reason = %q, must name consecutive or leading/trailing slashes", tc.name, d.Reason)
		}
	}

	// The legal form the illegal scopes would have normalized to is genuinely
	// covered, proving the denials above come from the legality gate and not
	// from a missing allow.
	legal := s.Decide(scopeNameOrg, scopeRequest("org/a/账本"))
	wantScopeDecision(t, legal, true, "matched allow policy",
		[]string{"p-ledger-allow", "p-wide-allow"}, 1, "the un-tidied legal scope is covered")
}

// TestInvalidScopePolicyCannotBePublished mirrors the request gate on the
// publishing side: a policy whose scope contains consecutive slashes cannot
// enter any published version, so no later decision can ever match one.
func TestInvalidScopePolicyCannotBePublished(t *testing.T) {
	s := NewStore()
	if v, err := s.Publish(scopeNameOrg, 0, nil); err != nil || v != 1 {
		t.Fatalf("baseline publish = %d, %v", v, err)
	}
	_, err := s.Publish(scopeNameOrg, 1, []Policy{
		scopePolicy("p-bad", "org//a", EffectAllow, false),
	})
	if err == nil {
		t.Fatal("policy with consecutive slashes must fail validation")
	}
	if !errorContains(err, "consecutive slashes") {
		t.Fatalf("err = %v, want it to name consecutive slashes", err)
	}
	if got := s.CurrentVersion(scopeNameOrg); got != 1 {
		t.Fatalf("current version = %d, want 1 after rejected publish", got)
	}
}

// errorContains is a small strings.Contains on err's text kept local so the
// suite does not add an import just for one assertion.
func errorContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

// TestScopeNameDecisionsSurviveReviewAndAudit pins that the full explanation
// produced by raw-byte scope matching — allow, policy deny and default deny —
// is preserved with reason, hit identifiers and actual version through the
// audit record, historical review, online recheck and offline review. The
// three display-confusable scopes must also stay pairwise distinguishable in
// the signed material rather than collapse onto one replacement character.
func TestScopeNameDecisionsSurviveReviewAndAudit(t *testing.T) {
	scopeFF, scopeFE, scopeFD := rawByteScopes()
	s := NewStore()
	version, err := s.Publish(scopeNameOrg, 0, []Policy{
		scopePolicy("p-scope-ff-allow", scopeFF, EffectAllow, false),
		scopePolicy("p-scope-fe-deny", scopeFE, EffectDeny, true),
		scopePolicy("p-scope-fd-allow", scopeFD, EffectAllow, false),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	// Chain layout (seq 1 is the publish), one decision per distinguished
	// outcome:
	//	seq 2  0xFF scope              allow
	//	seq 3  0xFE scope              deny (recursive deny covers itself)
	//	seq 4  genuine U+FFFD scope    allow
	//	seq 5  unlisted raw-byte name  default deny, empty hit list, version 1
	//	seq 6  "org//a" illegal scope  envelope denial, version 0, no hits
	unlistedScope := "org/账" + invalidByte + "目"
	// Decisions must be made in a fixed order: the audit chain assigns its
	// gapless sequence numbers by Decide call order, not by these keys.
	seqScopes := []struct {
		seq   int
		scope string
	}{
		{2, scopeFF}, {3, scopeFE}, {4, scopeFD}, {5, unlistedScope}, {6, "org//a"},
	}
	scopesAtSeq := make(map[int]string, len(seqScopes))
	online := map[int]Decision{}
	for _, item := range seqScopes {
		scopesAtSeq[item.seq] = item.scope
		online[item.seq] = s.Decide(scopeNameOrg, scopeRequest(item.scope))
	}

	wantScopeDecision(t, online[2], true, "matched allow policy",
		[]string{"p-scope-ff-allow"}, 1, "seq 2")
	wantScopeDecision(t, online[3], false, "matched deny policy",
		[]string{"p-scope-fe-deny"}, 1, "seq 3")
	wantScopeDecision(t, online[4], true, "matched allow policy",
		[]string{"p-scope-fd-allow"}, 1, "seq 4")
	wantScopeDecision(t, online[5], false, "no matching allow policy",
		nil, 1, "seq 5 default deny keeps version 1")
	if d6 := online[6]; d6.Allowed || d6.Version != 0 || len(d6.Matched) != 0 ||
		!strings.HasPrefix(d6.Reason, "invalid scope") {
		t.Fatalf("seq 6 illegal scope = %+v, want version-0 invalid-scope denial with no hits", d6)
	}

	recs, cp, err := s.AuditExport(scopeNameOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 6 {
		t.Fatalf("chain length = %d, want 6 (one publish + five decisions)", len(recs))
	}
	if err := VerifyAudit(scopeNameOrg, recs, cp); err != nil {
		t.Fatalf("raw-byte scope chain must verify: %v", err)
	}

	// Each record stores the submitted scope bytes and the returned decision
	// in full; distinct scope names must not collapse in signed material.
	for seq, wantScope := range scopesAtSeq {
		r := recs[seq-1]
		if r.Kind != AuditDecision || r.Decision == nil {
			t.Fatalf("seq %d is not a decision record: %+v", seq, r)
		}
		if r.Decision.Request.Resource.Scope != wantScope {
			t.Fatalf("seq %d stored scope %q, want byte-exact %q", seq,
				r.Decision.Request.Resource.Scope, wantScope)
		}
		if !reflect.DeepEqual(r.Decision.Decision, online[seq]) {
			t.Fatalf("seq %d stored decision %+v != returned %+v",
				seq, r.Decision.Decision, online[seq])
		}
	}
	fps := map[string]int{}
	for _, seq := range []int{2, 3, 4} {
		fp := recs[seq-1].Fingerprint
		if other, dup := fps[fp]; dup {
			t.Fatalf("seqs %d and %d share fingerprint %s: confusable scopes collapsed", other, seq, fp)
		}
		fps[fp] = seq
	}

	// Historical review against version 1 reproduces every explanation, and
	// review of the illegal scope still rejects at the envelope with
	// version 0 and no hits — review never tidies before evaluating either.
	for seq, wantScope := range scopesAtSeq {
		reviewed := s.Review(scopeNameOrg, version, scopeRequest(wantScope))
		if !reflect.DeepEqual(reviewed, online[seq]) {
			t.Fatalf("seq %d (scope %q) review %+v != online %+v",
				seq, wantScope, reviewed, online[seq])
		}
	}

	// Online recheck replays from the recorded material, including the
	// default deny (version 1, empty hit list) and the envelope denial.
	for seq := range scopesAtSeq {
		replayed, err := s.RecheckDecision(scopeNameOrg, seq)
		if err != nil {
			t.Fatalf("RecheckDecision seq %d: %v", seq, err)
		}
		if !reflect.DeepEqual(replayed, online[seq]) {
			t.Fatalf("seq %d recheck %+v != recorded %+v", seq, replayed, online[seq])
		}
	}

	// Offline review over the chain-validated export recomputes each decision
	// from the saved raw scope and the recorded version, marked consistent.
	for seq := range scopesAtSeq {
		review, err := RecheckDecisionOffline(scopeNameOrg, recs, cp, seq)
		if err != nil {
			t.Fatalf("offline review seq %d: %v", seq, err)
		}
		if !review.Consistent {
			t.Fatalf("seq %d offline review inconsistent: original %+v vs recomputed %+v",
				seq, review.Original, review.Recomputed)
		}
		if !reflect.DeepEqual(review.Recomputed, online[seq]) {
			t.Fatalf("seq %d recomputed %+v != online %+v",
				seq, review.Recomputed, online[seq])
		}
	}
}

// showScope renders a scope for failure messages without relying on its
// displayed form, since the confusable bytes all render alike.
func showScope(scope string) string {
	return strings.ReplaceAll(strings.ReplaceAll(scope, invalidByte, `<FF>`), anotherInvalidByte, `<FE>`)
}
