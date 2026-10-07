// Regression coverage for scope-name isolation in organization-level access
// decisions. Scope segment names may carry Chinese characters, spaces and
// bytes that are not valid UTF-8; only the slash separates segments, and
// names are compared by their raw submitted bytes — never by how they
// render. Two requests that share the organization, an enabled subject, the
// action and the resource identifier but differ only in the scope name must
// not share authorizations unless the names are byte-identical.
//
// The guarded rules, all already implemented by Store.Decide/evaluate:
//
//   - An exact (non-recursive) allow covers only the byte-identical scope.
//     A name that differs in case, carries an extra space in a segment, or
//     renders the same but differs in bytes (a lone 0xFF, a lone 0xFE, a
//     genuine U+FFFD) does not receive the allow: the outcome is the
//     default denial "no matching allow policy" with an empty hit list at
//     the published version actually evaluated.
//   - When look-alike names each carry their own policies, a request hits
//     only the policies of its own byte-exact name; another name's deny
//     never leaks into its hit list.
//   - A recursive policy keeps the same byte-exact name comparison and
//     covers the scope itself plus true descendants: "org/a" covers
//     "org/a/账本" but never "org/ab/账本"; the literal text "%2F" inside a
//     segment is not a slash, so "org/a%2F账本" is no child of "org/a";
//     and when the parent name carries spaces or non-UTF-8 bytes, only a
//     descendant that preserves the parent's raw name followed by a slash
//     belongs to the subtree.
//   - A recursive deny obeys the same boundaries: a deny that genuinely
//     hits still overrides the allow ("matched deny policy", both hit
//     identifiers sorted ascending), while a deny that does not hit cannot
//     affect another scope's access.
//   - A request scope with consecutive slashes is rejected before any
//     policy is evaluated: the reason states the scope is invalid, the
//     version is 0 and no policy is hit — the path is never tidied first
//     and then matched against an existing allow.
//
// Every request here is envelope-valid apart from the scope shape under
// test (the three organizations agree; subject, resource and action
// identifiers are present; the subject is enabled), so each outcome is
// genuinely produced by scope matching, not by an unrelated rejection.
package darksafe

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// scopeIsolationOrg is the single organization every request in this file
// shares; subject, resource and action identifiers are likewise fixed so
// that only the scope name varies between requests.
const (
	scopeIsolationOrg      = "acme"
	scopeIsolationSubject  = "u1"
	scopeIsolationResource = "ledger-2026"
	scopeIsolationAction   = "read"
)

// scopeRequest builds the request that carries the given scope and is
// otherwise identical to every other request in this file.
func scopeRequest(scope string) OrgRequest {
	return request(scopeIsolationOrg, scopeIsolationSubject, scopeIsolationResource, scope, scopeIsolationAction)
}

// TestExactScopeAllowDoesNotLeakToLookalikeNames pins the exact-policy
// boundary: after one read allow is published for a scope whose segment
// names carry Chinese characters and a space, only the byte-identical name
// hits it. Names that differ in case or carry an extra space fall through
// to the default denial with an empty hit list at the published version.
func TestExactScopeAllowDoesNotLeakToLookalikeNames(t *testing.T) {
	const (
		scope    = "org/账 本"
		policyID = "p-allow-ledger"
	)
	s := NewStore()
	version, err := s.Publish(scopeIsolationOrg, 0, []Policy{
		allowPolicy(policyID, scopeIsolationSubject, scopeIsolationAction, scope, false),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	// The byte-identical name hits the allow.
	d := s.Decide(scopeIsolationOrg, scopeRequest(scope))
	wantDecisionShape(t, d, true, "matched allow policy", []string{policyID}, version,
		"byte-identical scope name")

	// Case changes and extra spaces inside a segment are different names:
	// none of them may obtain the allow.
	lookalikes := map[string]string{
		"ascii case changed":        "ORG/账 本",
		"trailing space in segment": "org/账 本 ",
		"leading space in segment":  "org/ 账 本",
		"extra inner space":         "org/账  本",
		"space replaced by slash":   "org/账/本",
	}
	for name, alien := range lookalikes {
		if alien == scope {
			t.Fatalf("test premise broken: %s variant %q equals the allowed scope", name, alien)
		}
		d := s.Decide(scopeIsolationOrg, scopeRequest(alien))
		wantDecisionShape(t, d, false, "no matching allow policy", []string{}, version,
			"lookalike scope ("+name+")")
	}
}

// TestByteDistinctScopeNamesKeepOwnPolicies pins the raw-byte comparison
// for the three display-conflated names: a segment carrying a lone 0xFF
// byte, a lone 0xFE byte, or a genuine U+FFFD code point. Each name has its
// own policy posture (allow, deny, none) and every request must be decided
// exclusively by its own name's policies — another name's allow or deny
// must never enter its hit list.
func TestByteDistinctScopeNamesKeepOwnPolicies(t *testing.T) {
	scopeFF := "org/账" + invalidByte + "本"
	scopeFE := "org/账" + anotherInvalidByte + "本"
	scopeFD := "org/账" + replacementRune + "本"

	// Document the premise this regression guards: Go string comparison
	// distinguishes the three names, while encoding/json renders the two
	// lone invalid bytes identically. Any normalization at the scope
	// boundary would collapse them into one scope.
	if scopeFF == scopeFE || scopeFF == scopeFD || scopeFE == scopeFD {
		t.Fatalf("test premise broken: scope names must differ pairwise: %q %q %q", scopeFF, scopeFE, scopeFD)
	}
	jFF, _ := json.Marshal(scopeFF)
	jFE, _ := json.Marshal(scopeFE)
	if !bytes.Equal(jFF, jFE) {
		t.Fatalf("test premise broken: JSON renders the invalid bytes differently: %s vs %s", jFF, jFE)
	}

	const (
		allowID = "p-allow-ff"
		denyID  = "p-deny-fe"
	)
	s := NewStore()
	version, err := s.Publish(scopeIsolationOrg, 0, []Policy{
		allowPolicy(allowID, scopeIsolationSubject, scopeIsolationAction, scopeFF, false),
		denyPolicy(denyID, scopeIsolationSubject, scopeIsolationAction, scopeFE, false),
		// No policy at all names the genuine-U+FFFD scope.
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	// The 0xFF scope is allowed by its own allow alone; the 0xFE scope's
	// deny must not travel with it.
	d := s.Decide(scopeIsolationOrg, scopeRequest(scopeFF))
	wantDecisionShape(t, d, true, "matched allow policy", []string{allowID}, version,
		"scope with lone 0xFF")

	// The 0xFE scope is denied by ITS OWN deny; the 0xFF scope's allow must
	// not ride along.
	d = s.Decide(scopeIsolationOrg, scopeRequest(scopeFE))
	wantDecisionShape(t, d, false, "matched deny policy", []string{denyID}, version,
		"scope with lone 0xFE")

	// The genuine-U+FFFD scope has no policy: the default denial applies
	// with an empty hit list, and neither look-alike's policy is its basis.
	d = s.Decide(scopeIsolationOrg, scopeRequest(scopeFD))
	wantDecisionShape(t, d, false, "no matching allow policy", []string{}, version,
		"scope with genuine U+FFFD")
}

// TestRecursiveScopeKeepsRawNameBoundaries pins the recursive-policy
// boundaries: coverage extends to the scope itself and its true descendants
// (parent's raw bytes + "/" + more), and nothing else — not sibling
// prefixes, not literal "%2F" text inside a segment, and not names that
// merely render like the parent's.
func TestRecursiveScopeKeepsRawNameBoundaries(t *testing.T) {
	const (
		recID    = "p-rec-a"
		recSpace = "p-rec-space"
		recRaw   = "p-rec-raw"
	)
	spaceParent := "org/账 本"
	rawParent := "org/p" + invalidByte
	s := NewStore()
	version, err := s.Publish(scopeIsolationOrg, 0, []Policy{
		allowPolicy(recID, scopeIsolationSubject, scopeIsolationAction, "org/a", true),
		allowPolicy(recSpace, scopeIsolationSubject, scopeIsolationAction, spaceParent, true),
		allowPolicy(recRaw, scopeIsolationSubject, scopeIsolationAction, rawParent, true),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	cases := []struct {
		name    string
		scope   string
		allowed bool
		matched []string
	}{
		// True coverage: the scope itself and real descendants.
		{"recursive scope itself", "org/a", true, []string{recID}},
		{"chinese descendant", "org/a/账本", true, []string{recID}},
		{"deeper descendant", "org/a/账本/明细", true, []string{recID}},
		// "org/ab" is a sibling prefix, not a descendant; its subtree is
		// equally outside.
		{"sibling prefix subtree", "org/ab/账本", false, []string{}},
		// The literal text "%2F" inside a segment is not a slash: this is a
		// single segment "a%2F账本", not a child of "org/a".
		{"literal %2F is not a slash", "org/a%2F账本", false, []string{}},
		{"literal lowercase %2f is not a slash", "org/a%2f账本", false, []string{}},
		// A parent name with a space: only descendants preserving the
		// parent's raw name followed by a slash belong.
		{"space parent itself", spaceParent, true, []string{recSpace}},
		{"space parent descendant", spaceParent + "/子", true, []string{recSpace}},
		{"space parent extended name", spaceParent + "x/子", false, []string{}},
		{"space parent tightened name", "org/账本/子", false, []string{}},
		// A parent name with a non-UTF-8 byte: the descendant must repeat
		// the parent's exact bytes; look-alike bytes stay outside.
		{"raw parent itself", rawParent, true, []string{recRaw}},
		{"raw parent descendant", rawParent + "/child", true, []string{recRaw}},
		{"raw parent with 0xFE instead", "org/p" + anotherInvalidByte + "/child", false, []string{}},
		{"raw parent with U+FFFD instead", "org/p" + replacementRune + "/child", false, []string{}},
		{"raw parent extended name", rawParent + "x/child", false, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := "no matching allow policy"
			if tc.allowed {
				reason = "matched allow policy"
			}
			d := s.Decide(scopeIsolationOrg, scopeRequest(tc.scope))
			wantDecisionShape(t, d, tc.allowed, reason, tc.matched, version, tc.name)
		})
	}
}

// TestRecursiveDenyKeepsRawNameBoundaries pins the deny side of the same
// boundaries: a recursive deny that genuinely covers the requested scope
// still overrides the allow, with both hit identifiers kept in ascending
// order; a recursive deny whose name merely looks like the requested
// scope's parent cannot affect the request at all.
func TestRecursiveDenyKeepsRawNameBoundaries(t *testing.T) {
	const (
		denyID     = "p-deny-a"
		childAllow = "p-allow-child"
		rawDeny    = "p-deny-raw"
		feAllow    = "p-allow-fe-child"
	)
	rawParent := "org/q" + invalidByte
	feChild := "org/q" + anotherInvalidByte + "/child"
	s := NewStore()
	version, err := s.Publish(scopeIsolationOrg, 0, []Policy{
		denyPolicy(denyID, scopeIsolationSubject, scopeIsolationAction, "org/a", true),
		allowPolicy(childAllow, scopeIsolationSubject, scopeIsolationAction, "org/a/账本", false),
		denyPolicy(rawDeny, scopeIsolationSubject, scopeIsolationAction, rawParent, true),
		allowPolicy(feAllow, scopeIsolationSubject, scopeIsolationAction, feChild, false),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	// The recursive deny genuinely covers org/a/账本: it overrides the exact
	// child allow, and the hit list keeps both identifiers sorted ascending.
	d := s.Decide(scopeIsolationOrg, scopeRequest("org/a/账本"))
	wantDecisionShape(t, d, false, "matched deny policy",
		[]string{childAllow, denyID}, version, "recursive deny over exact child allow")

	// The deny on the 0xFF parent covers its own true descendant.
	d = s.Decide(scopeIsolationOrg, scopeRequest(rawParent+"/child"))
	wantDecisionShape(t, d, false, "matched deny policy",
		[]string{rawDeny}, version, "recursive deny on raw-byte parent")

	// The 0xFE-named child is decided by its own allow alone: the deny on
	// the display-identical 0xFF parent must not leak into its hit list.
	d = s.Decide(scopeIsolationOrg, scopeRequest(feChild))
	wantDecisionShape(t, d, true, "matched allow policy",
		[]string{feAllow}, version, "0xFE child under a look-alike 0xFF deny")

	// The genuine-U+FFFD descendant has no policy of its own and is outside
	// the 0xFF parent's subtree: default denial, empty hit list.
	d = s.Decide(scopeIsolationOrg, scopeRequest("org/q"+replacementRune+"/child"))
	wantDecisionShape(t, d, false, "no matching allow policy",
		[]string{}, version, "U+FFFD child under a look-alike 0xFF deny")
}

// TestConsecutiveSlashesRejectedBeforePolicyEvaluation pins the
// invalid-scope envelope rejection: a request scope with consecutive
// slashes never reaches policy evaluation. The decision is a denial whose
// reason states the scope is invalid, at version 0 with no matched policy —
// even though a published recursive allow would cover the tidied path.
func TestConsecutiveSlashesRejectedBeforePolicyEvaluation(t *testing.T) {
	const allowID = "p-allow-org"
	s := NewStore()
	version, err := s.Publish(scopeIsolationOrg, 0, []Policy{
		allowPolicy(allowID, scopeIsolationSubject, scopeIsolationAction, "org", true),
	})
	if err != nil || version != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", version, err)
	}

	// Sanity: the tidied path is genuinely allowed, so any allowance of the
	// malformed variants below could only come from normalizing first.
	d := s.Decide(scopeIsolationOrg, scopeRequest("org/账本"))
	wantDecisionShape(t, d, true, "matched allow policy", []string{allowID}, version,
		"tidied path sanity check")

	for _, scope := range []string{"org//账本", "org/a//b", "org///账本"} {
		d := s.Decide(scopeIsolationOrg, scopeRequest(scope))
		if d.Allowed {
			t.Fatalf("scope %q was allowed; consecutive slashes must reject before evaluation: %+v", scope, d)
		}
		if !strings.HasPrefix(d.Reason, "invalid scope") {
			t.Fatalf("scope %q reason = %q, want an invalid-scope explanation", scope, d.Reason)
		}
		if d.Version != 0 {
			t.Fatalf("scope %q version = %d, want 0 (no published version evaluated)", scope, d.Version)
		}
		if d.Matched != nil {
			t.Fatalf("scope %q matched = %q, want no matched policies (nil)", scope, d.Matched)
		}
	}
}
