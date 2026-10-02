package darksafe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// resourcePolicy builds a policy narrowed to one exact resource ID.
func resourcePolicy(id, subject, action, scope, resourceID string, effect Effect, recursive bool) Policy {
	return Policy{
		ID: id, Subject: subject, Action: action, Scope: scope,
		Effect: effect, Recursive: recursive, ResourceID: resourceID,
	}
}

// TestResourceScopedAllowOnlyHitsNamedResource is the core behavior: a
// resource-qualified allow grants exactly one resource in the scope and is
// invisible (not even listed as matched) for every other resource, which
// falls through to the existing default denial.
func TestResourceScopedAllowOnlyHitsNamedResource(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p-ledger-a", "u1", "read", "org/payments", "ledger-a", EffectAllow, false),
	}); err != nil {
		t.Fatal(err)
	}

	allow := s.Decide("acme", request("acme", "u1", "ledger-a", "org/payments", "read"))
	if !allow.Allowed || allow.Reason != "matched allow policy" || allow.Version != 1 {
		t.Fatalf("named resource decision = %+v", allow)
	}
	if !reflect.DeepEqual(allow.Matched, []string{"p-ledger-a"}) {
		t.Fatalf("matched = %v, want [p-ledger-a]", allow.Matched)
	}

	// Another resource in the very same scope: no grant, no hit, default deny.
	other := s.Decide("acme", request("acme", "u1", "ledger-b", "org/payments", "read"))
	if other.Allowed {
		t.Fatalf("other resource must not be allowed: %+v", other)
	}
	if other.Reason != "no matching allow policy" {
		t.Fatalf("other resource reason = %q, want default denial", other.Reason)
	}
	if len(other.Matched) != 0 {
		t.Fatalf("resource policy must not appear in matched: %v", other.Matched)
	}
}

// TestResourceComparisonIsExactAndRaw checks case sensitivity, significant
// spaces, a literal asterisk that is not a wildcard, and the fact that the
// resource ID is never interpreted as a scope path.
func TestResourceComparisonIsExactAndRaw(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p", "u1", "read", "org/a", "Ledger A", EffectAllow, false),
		resourcePolicy("star", "u1", "read", "org/a", "*", EffectAllow, false),
		resourcePolicy("scopelike", "u1", "read", "org/a", "org/a/child", EffectAllow, false),
	}); err != nil {
		t.Fatal(err)
	}
	decide := func(resourceID string) Decision {
		return s.Decide("acme", request("acme", "u1", resourceID, "org/a", "read"))
	}
	for _, id := range []string{
		"ledger a",   // different case
		"Ledger A ",  // trailing space
		" Ledger A",  // leading space
		"Ledger  A",  // doubled space
		"Ledger\tA",  // different whitespace
		"",           // empty is handled by the envelope, never by the policy
		"anything",   // "*" is not a wildcard
		"org/a",      // resource id is not a scope path
		"org/a/chil", // prefix of a "scope-like" resource id is not enough
	} {
		if d := decide(id); d.Allowed || len(d.Matched) != 0 {
			t.Fatalf("resource ID %q must not match: %+v", id, d)
		}
	}
	if d := decide("Ledger A"); !d.Allowed {
		t.Fatalf("exact mixed-case id with space must match: %+v", d)
	}
	// A literal asterisk resource matches only a resource literally named "*".
	if d := decide("*"); !d.Allowed || !reflect.DeepEqual(d.Matched, []string{"star"}) {
		t.Fatalf("literal '*' resource: %+v", d)
	}
	// The "scope-like" resource ID is compared as an opaque string.
	if d := decide("org/a/child"); !d.Allowed {
		t.Fatalf("scope-looking resource id must be matched literally: %+v", d)
	}
}

// TestResourceQualifierIsAdditionalToScope verifies that even an identical
// resource ID cannot match outside the policy's scope, exact or recursive.
func TestResourceQualifierIsAdditionalToScope(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("exact", "u1", "read", "org/a", "ledger", EffectAllow, false),
		resourcePolicy("subtree", "u2", "read", "org/a", "ledger", EffectAllow, true),
	}); err != nil {
		t.Fatal(err)
	}

	// Same resource ID, wrong scope: exact policy does not apply.
	if d := s.Decide("acme", request("acme", "u1", "ledger", "org/b", "read")); d.Allowed {
		t.Fatalf("equal ID outside exact scope matched: %+v", d)
	}
	if d := s.Decide("acme", request("acme", "u1", "ledger", "org/a/b", "read")); d.Allowed {
		t.Fatalf("equal ID at descendant of non-recursive scope matched: %+v", d)
	}
	// Recursive policy: same ID inside the subtree matches...
	if d := s.Decide("acme", request("acme", "u2", "ledger", "org/a/b/c", "read")); !d.Allowed {
		t.Fatalf("equal ID inside recursive subtree must match: %+v", d)
	}
	// ...same ID outside the subtree does not, and neither does a segment
	// prefix (org/ab is not under org/a).
	for _, scope := range []string{"org/b", "org/ab", "org"} {
		if d := s.Decide("acme", request("acme", "u2", "ledger", scope, "read")); d.Allowed {
			t.Fatalf("equal ID at scope %q outside subtree matched: %+v", scope, d)
		}
	}
	// Another resource ID inside the covered subtree still does not match.
	if d := s.Decide("acme", request("acme", "u2", "journal", "org/a/b", "read")); d.Allowed {
		t.Fatalf("different ID inside subtree must not match: %+v", d)
	}
}

// TestResourceQualifiedDenyAllowCombinations exercises deny precedence with
// resource-qualified and scope-wide policies published together.
func TestResourceQualifiedDenyAllowCombinations(t *testing.T) {
	t.Run("scope allow plus resource deny", func(t *testing.T) {
		s := NewStore()
		if _, err := s.Publish("acme", 0, []Policy{
			allowPolicy("scope-allow", "u1", "read", "org/ledgers", true),
			resourcePolicy("deny-a", "u1", "read", "org/ledgers", "甲", EffectDeny, true),
		}); err != nil {
			t.Fatal(err)
		}
		// Ledger 甲: the resource deny wins and both policies are reported.
		a := s.Decide("acme", request("acme", "u1", "甲", "org/ledgers/x", "read"))
		if a.Allowed || a.Reason != "matched deny policy" {
			t.Fatalf("ledger 甲 must be denied: %+v", a)
		}
		if !reflect.DeepEqual(a.Matched, []string{"deny-a", "scope-allow"}) {
			t.Fatalf("matched = %v, want both policies sorted", a.Matched)
		}
		// Ledger 乙 in the same covered subtree: only the scope allow applies.
		b := s.Decide("acme", request("acme", "u1", "乙", "org/ledgers/x", "read"))
		if !b.Allowed || !reflect.DeepEqual(b.Matched, []string{"scope-allow"}) {
			t.Fatalf("ledger 乙 must stay allowed: %+v", b)
		}
	})

	t.Run("scope deny beats resource allow", func(t *testing.T) {
		s := NewStore()
		if _, err := s.Publish("acme", 0, []Policy{
			{ID: "scope-deny", Subject: "u1", Action: "read", Scope: "org/ledgers", Effect: EffectDeny},
			resourcePolicy("allow-a", "u1", "read", "org/ledgers", "甲", EffectAllow, false),
		}); err != nil {
			t.Fatal(err)
		}
		a := s.Decide("acme", request("acme", "u1", "甲", "org/ledgers", "read"))
		if a.Allowed || a.Reason != "matched deny policy" {
			t.Fatalf("scope deny must override resource allow: %+v", a)
		}
		if !reflect.DeepEqual(a.Matched, []string{"allow-a", "scope-deny"}) {
			t.Fatalf("matched = %v, want both policies", a.Matched)
		}
		// No resource-specific allow exists for 乙, but the scope deny still hits.
		b := s.Decide("acme", request("acme", "u1", "乙", "org/ledgers", "read"))
		if b.Allowed || !reflect.DeepEqual(b.Matched, []string{"scope-deny"}) {
			t.Fatalf("ledger 乙 denied by scope policy only: %+v", b)
		}
	})

	t.Run("resource deny beats resource allow on same resource", func(t *testing.T) {
		s := NewStore()
		if _, err := s.Publish("acme", 0, []Policy{
			resourcePolicy("allow-a", "u1", "read", "org/a", "甲", EffectAllow, false),
			resourcePolicy("deny-a", "u1", "read", "org/a", "甲", EffectDeny, false),
		}); err != nil {
			t.Fatal(err)
		}
		if d := s.Decide("acme", request("acme", "u1", "甲", "org/a", "read")); d.Allowed {
			t.Fatalf("resource deny must win on 甲: %+v", d)
		}
		if d := s.Decide("acme", request("acme", "u1", "乙", "org/a", "read")); d.Allowed || len(d.Matched) != 0 {
			t.Fatalf("neither resource policy must hit 乙: %+v", d)
		}
	})

	// A version mixing unrestricted and resource-qualified policies keeps a
	// single consistent, exportable fingerprint covering both shapes.
	t.Run("mixed policy set exports and verifies", func(t *testing.T) {
		s := NewStore()
		s.Publish("acme", 0, []Policy{
			allowPolicy("wide", "u1", "read", "org/a", false),
			resourcePolicy("narrow-deny", "u1", "read", "org/a", "ledger-a", EffectDeny, false),
			allowPolicy("other-subject", "u2", "read", "org/a", true),
		})
		s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))
		recs, cp, err := s.AuditExport("acme", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyAudit("acme", recs, cp); err != nil {
			t.Fatalf("mixed-shape policy set must verify: %v", err)
		}
		if fingerprintFor(&recs[0]) != legacyJSONFingerprint(&recs[0]) {
			t.Fatal("valid-UTF-8 mixed policy set must stay in the JSON fingerprint family")
		}
		if review, err := RecheckDecisionOffline("acme", recs, cp, 2); err != nil || !review.Consistent {
			t.Fatalf("mixed-set offline review = %+v, %v", review, err)
		}
	})
}

// TestResourceQualifierDoesNotBypassEnvelopeChecks ensures missing fields,
// disabled subjects and organization mismatches reject exactly as before;
// naming a resource can never open a back door.
func TestResourceQualifierDoesNotBypassEnvelopeChecks(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p", "u1", "read", "org/a", "ledger-a", EffectAllow, false),
	}); err != nil {
		t.Fatal(err)
	}
	base := request("acme", "u1", "ledger-a", "org/a", "read")

	missingResource := base
	missingResource.Resource.ID = ""
	if d := s.Decide("acme", missingResource); d.Allowed || d.Reason != "missing resource id" {
		t.Fatalf("missing resource id: %+v", d)
	}
	disabled := base
	disabled.Subject.Disabled = true
	if d := s.Decide("acme", disabled); d.Allowed || d.Reason != "subject is disabled" {
		t.Fatalf("disabled subject: %+v", d)
	}
	mismatch := base
	mismatch.SubjectOrg = "globex"
	if d := s.Decide("acme", mismatch); d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("subject org mismatch: %+v", d)
	}
	mismatch = base
	mismatch.ResourceOrg = "globex"
	if d := s.Decide("acme", mismatch); d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("resource org mismatch: %+v", d)
	}
	if d := s.Decide("", base); d.Allowed || d.Reason != "missing decision organization" {
		t.Fatalf("missing decision org: %+v", d)
	}
}

// TestResourceNeedsNoRegistration publishes a resource-qualified policy
// without any resource registry and immediately uses it; an empty set with
// a later resource policy also keeps working.
func TestResourceNeedsNoRegistration(t *testing.T) {
	s := NewStore()
	p := resourcePolicy("p", "u1", "read", "org/a", "brand-new-ledger-404", EffectAllow, false)
	v, err := s.Publish("acme", 0, []Policy{p})
	if err != nil || v != 1 {
		t.Fatalf("publish unseen resource policy = %d, %v", v, err)
	}
	if d := s.Decide("acme", request("acme", "u1", "brand-new-ledger-404", "org/a", "read")); !d.Allowed {
		t.Fatalf("unregistered named resource must still match: %+v", d)
	}
	// Policies returned from history carry the qualifier in full.
	got, err := s.Policies("acme", 1)
	if err != nil || !reflect.DeepEqual(got, []Policy{p}) {
		t.Fatalf("historical policies = %+v, %v; want %+v", got, err, p)
	}
}

// TestResourceQualifierIsVersionedAndRollsBack pins the qualifier to
// versions: changing it later never changes review, recheck or offline
// recheck of an old decision, and a rollback restores the old qualifier.
func TestResourceQualifierIsVersionedAndRollsBack(t *testing.T) {
	s := NewStore()
	v1 := 1
	if v, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p", "u1", "read", "org/a", "甲", EffectAllow, false),
	}); err != nil || v != v1 {
		t.Fatalf("publish v1 = %d, %v", v, err)
	}
	reqA := request("acme", "u1", "甲", "org/a", "read")
	reqB := request("acme", "u1", "乙", "org/a", "read")
	originalA := s.Decide("acme", reqA) // seq 2: allowed under v1
	originalB := s.Decide("acme", reqB) // seq 3: default denial under v1
	if !originalA.Allowed || originalB.Allowed {
		t.Fatalf("setup decisions wrong: a=%+v b=%+v", originalA, originalB)
	}

	// Move the very same policy from 甲 to 乙.
	if _, err := s.Publish("acme", v1, []Policy{
		resourcePolicy("p", "u1", "read", "org/a", "乙", EffectAllow, false),
	}); err != nil {
		t.Fatal(err)
	}
	// Current version flips the outcomes.
	if d := s.Decide("acme", reqA); d.Allowed {
		t.Fatalf("v2 must no longer allow 甲: %+v", d)
	}
	if d := s.Decide("acme", reqB); !d.Allowed {
		t.Fatalf("v2 must now allow 乙: %+v", d)
	}
	// Historical review stays pinned to v1.
	if d := s.Review("acme", v1, reqA); !reflect.DeepEqual(d, originalA) {
		t.Fatalf("v1 review changed: %+v want %+v", d, originalA)
	}
	if d := s.Review("acme", v1, reqB); !reflect.DeepEqual(d, originalB) {
		t.Fatalf("v1 review of 乙 changed: %+v want %+v", d, originalB)
	}
	// Online recheck of the recorded v1 decisions is unchanged.
	if d, err := s.RecheckDecision("acme", 2); err != nil || !reflect.DeepEqual(d, originalA) {
		t.Fatalf("recheck seq2 = %+v, %v; want %+v", d, err, originalA)
	}
	if d, err := s.RecheckDecision("acme", 3); err != nil || !reflect.DeepEqual(d, originalB) {
		t.Fatalf("recheck seq3 = %+v, %v; want %+v", d, err, originalB)
	}

	// Offline recheck from an export ending at v2 reproduces the v1 verdicts.
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{2, 3} {
		review, err := RecheckDecisionOffline("acme", recs, cp, seq)
		if err != nil {
			t.Fatalf("offline recheck seq %d: %v", seq, err)
		}
		if !review.Consistent {
			t.Fatalf("offline recheck seq %d inconsistent: %+v vs %+v", seq, review.Original, review.Recomputed)
		}
	}

	// A rollback restores the 甲 qualifier as a new version and records it.
	v3, err := s.Rollback("acme", 2, v1)
	if err != nil || v3 != 3 {
		t.Fatalf("rollback = %d, %v", v3, err)
	}
	if d := s.Decide("acme", reqA); !d.Allowed || d.Version != 3 {
		t.Fatalf("after rollback 甲 must be allowed under v3: %+v", d)
	}
	rolled, err := s.Policies("acme", 3)
	if err != nil {
		t.Fatal(err)
	}
	if rolled[0].ResourceID != "甲" {
		t.Fatalf("rolled back qualifier = %q, want 甲", rolled[0].ResourceID)
	}
}

// TestResourceQualifierTamperingBreaksChain proves the qualifier is part of
// the signed policy content: changing it in exported material while keeping
// the original fingerprints and checkpoint must fail verification online and
// block offline review.
func TestResourceQualifierTamperingBreaksChain(t *testing.T) {
	s := NewStore()
	p := resourcePolicy("p", "u1", "read", "org/a", "ledger-a", EffectAllow, false)
	s.Publish("acme", 0, []Policy{p})
	s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))

	good, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	tampered := cloneExportedRecords(good)
	tampered[0].Change.Policies[0].ResourceID = "ledger-b"
	if err := VerifyAudit("acme", tampered, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("changed qualifier err = %v, want ErrInvalidRange", err)
	}
	if _, err := RecheckDecisionOffline("acme", tampered, cp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("offline review over tampered qualifier err = %v", err)
	}

	// Adding a qualifier to an originally unrestricted policy is content
	// change too, even though its fingerprints otherwise stay legacy.
	s2 := NewStore()
	s2.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s2.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	plain, plainCP, err := s2.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprintFor(&plain[0]) != legacyJSONFingerprint(&plain[0]) {
		t.Fatal("unrestricted policy must keep the legacy fingerprint")
	}
	added := cloneExportedRecords(plain)
	added[0].Change.Policies[0].ResourceID = "ledger-a"
	if err := VerifyAudit("acme", added, plainCP); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("newly added qualifier err = %v, want ErrInvalidRange", err)
	}
}

// TestResourceQualifierFingerprints shows valid-UTF-8 qualified policies
// extend the legacy JSON fingerprint family (so the two encodings agree),
// while invalid bytes in ResourceID take the raw path and remain
// distinguishable after export and offline review.
func TestResourceQualifierFingerprints(t *testing.T) {
	org := "acme"
	p := resourcePolicy("p1", "u1", "read", "org/a", "ledger-中", EffectAllow, false)
	prev := genesisFingerprint(org)
	rec := syntheticRecord(org, 1, AuditPolicyChange, &PolicyChange{
		Version: 1, Policies: []Policy{p},
	}, nil, prev)
	if fingerprintFor(&rec) != legacyJSONFingerprint(&rec) {
		t.Fatal("valid-UTF-8 resource qualifier must stay in the JSON fingerprint family")
	}
	// Sanity: a JSON fingerprint genuinely covers the qualifier.
	changedRecs := cloneExportedRecords([]AuditRecord{rec})
	changedRecs[0].Change.Policies[0].ResourceID = "ledger-文"
	if fingerprintFor(&changedRecs[0]) == fingerprintFor(&rec) {
		t.Fatal("changing the qualifier did not change the JSON fingerprint")
	}

	// Invalid UTF-8 in the qualifier is accepted, preserved byte for byte,
	// and hashed through the raw encoding.
	s := NewStore()
	raw := resourcePolicy("p", "u1", "read", "org/a", "账"+invalidByte+"本", EffectAllow, false)
	if _, err := s.Publish(org, 0, []Policy{raw}); err != nil {
		t.Fatal(err)
	}
	if d := s.Decide(org, request(org, "u1", "账"+invalidByte+"本", "org/a", "read")); !d.Allowed {
		t.Fatalf("raw-byte resource must match raw-byte qualifier: %+v", d)
	}
	// The display-identical genuine U+FFFD rune is a different resource and
	// must not match.
	if d := s.Decide(org, request(org, "u1", "账"+replacementRune+"本", "org/a", "read")); d.Allowed {
		t.Fatalf("replacement-rune resource must not match raw-byte qualifier: %+v", d)
	}
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(org, recs, cp); err != nil {
		t.Fatalf("raw-byte qualifier chain must verify: %v", err)
	}
	exported := recs[0].Change.Policies[0]
	if !reflect.DeepEqual(exported, raw) || !strings.Contains(exported.ResourceID, invalidByte) {
		t.Fatalf("qualifier bytes not preserved: %+v want %+v", exported, raw)
	}
	if fingerprintFor(&recs[0]) == legacyJSONFingerprint(&recs[0]) {
		t.Fatal("raw-byte qualifier must leave the JSON fingerprint family")
	}

	// Substituting the lone invalid byte for 0xFE or a real U+FFFD while
	// keeping the fingerprint and checkpoint must fail.
	for _, repl := range []string{anotherInvalidByte, replacementRune} {
		bad := cloneExportedRecords(recs)
		bad[0].Change.Policies[0].ResourceID = strings.Replace(bad[0].Change.Policies[0].ResourceID, invalidByte, repl, 1)
		if err := VerifyAudit(org, bad, cp); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("qualifier repl=%s err = %v, want ErrInvalidRange", replacementLabel(repl), err)
		}
	}

	// Offline review of the recorded raw-byte-named resource decision at
	// seq 2 reproduces the allow byte for byte.
	review, err := RecheckDecisionOffline(org, recs, cp, 2)
	if err != nil || !review.Consistent || !review.Recomputed.Allowed {
		t.Fatalf("offline raw-byte review = %+v, %v", review, err)
	}
}
