package darksafe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// resourcePolicy is a test helper that builds a policy with a ResourceID.
func resourcePolicy(id, subject, action, scope string, effect Effect, recursive bool, resourceID string) Policy {
	return Policy{
		ID:         id,
		Subject:    subject,
		Action:     action,
		Scope:      scope,
		Effect:     effect,
		Recursive:  recursive,
		ResourceID: resourceID,
	}
}

// TestResourceScopeBasicMatching verifies that a resource-scoped policy
// grants access only to the exact resource ID it names, while an unscoped
// policy in the same set covers every resource in scope.
func TestResourceScopeBasicMatching(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("p-ledger-a", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// The named resource is allowed.
	d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))
	if !d.Allowed || d.Reason != "matched allow policy" {
		t.Fatalf("named resource: %+v, want allowed", d)
	}
	if !reflect.DeepEqual(d.Matched, []string{"p-ledger-a"}) {
		t.Fatalf("matched = %v, want [p-ledger-a]", d.Matched)
	}

	// A different resource in the same scope is denied and the scoped
	// policy must not appear in the matched list.
	d = s.Decide("acme", request("acme", "u1", "ledger-b", "org/a", "read"))
	if d.Allowed || d.Reason != "no matching allow policy" {
		t.Fatalf("other resource: %+v, want denied with no matching allow", d)
	}
	if len(d.Matched) != 0 {
		t.Fatalf("matched = %v, want empty (scoped policy must not hit)", d.Matched)
	}
}

// TestResourceScopeRequiresScopeMatch verifies that resource scoping is an
// additional condition: even with the same resource ID, a scope mismatch
// still denies.
func TestResourceScopeRequiresScopeMatch(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// Same resource ID, exact scope: allowed.
	if d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read")); !d.Allowed {
		t.Fatalf("exact scope: %+v, want allowed", d)
	}
	// Same resource ID, descendant scope but policy is not recursive: denied.
	if d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a/b", "read")); d.Allowed {
		t.Fatalf("descendant scope with non-recursive policy: %+v, want denied", d)
	}
	// Same resource ID, sibling scope: denied.
	if d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/ab", "read")); d.Allowed {
		t.Fatalf("sibling scope: %+v, want denied", d)
	}
}

// TestResourceScopeWithRecursiveScope verifies that a recursive policy with
// a resource ID covers only resources within its scope subtree that also
// carry the exact ID.
func TestResourceScopeWithRecursiveScope(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, true, "ledger-a"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// Within coverage and same ID: allowed.
	if d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a/b/c", "read")); !d.Allowed {
		t.Fatalf("within coverage same ID: %+v, want allowed", d)
	}
	// Within coverage but different ID: denied.
	if d := s.Decide("acme", request("acme", "u1", "ledger-b", "org/a/b/c", "read")); d.Allowed {
		t.Fatalf("within coverage different ID: %+v, want denied", d)
	}
	// Same ID but outside coverage: denied.
	if d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/b", "read")); d.Allowed {
		t.Fatalf("outside coverage same ID: %+v, want denied", d)
	}
}

// TestResourceScopeDenyOverridesAllow verifies that a scope-level allow
// combined with a resource-scoped deny denies the named resource while
// allowing other resources in the same scope.
func TestResourceScopeDenyOverridesAllow(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		allowPolicy("scope-allow", "u1", "read", "org/a", false),
		resourcePolicy("resource-deny", "u1", "read", "org/a", EffectDeny, false, "ledger-a"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// The named resource is denied (deny overrides allow).
	d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))
	if d.Allowed || d.Reason != "matched deny policy" {
		t.Fatalf("named resource: %+v, want denied", d)
	}
	if !reflect.DeepEqual(d.Matched, []string{"resource-deny", "scope-allow"}) {
		t.Fatalf("matched = %v, want [resource-deny, scope-allow]", d.Matched)
	}

	// A different resource is allowed by the scope-level allow.
	d = s.Decide("acme", request("acme", "u1", "ledger-b", "org/a", "read"))
	if !d.Allowed || d.Reason != "matched allow policy" {
		t.Fatalf("other resource: %+v, want allowed", d)
	}
	if !reflect.DeepEqual(d.Matched, []string{"scope-allow"}) {
		t.Fatalf("matched = %v, want [scope-allow]", d.Matched)
	}
}

// TestScopeDenyOverridesResourceAllow verifies that a scope-level deny
// overrides a resource-scoped allow, even for the named resource.
func TestScopeDenyOverridesResourceAllow(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("resource-allow", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
		{ID: "scope-deny", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// The named resource is denied by the scope-level deny.
	d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))
	if d.Allowed || d.Reason != "matched deny policy" {
		t.Fatalf("named resource: %+v, want denied by scope deny", d)
	}
	if !reflect.DeepEqual(d.Matched, []string{"resource-allow", "scope-deny"}) {
		t.Fatalf("matched = %v, want [resource-allow, scope-deny]", d.Matched)
	}

	// A different resource is also denied by the scope-level deny.
	d = s.Decide("acme", request("acme", "u1", "ledger-b", "org/a", "read"))
	if d.Allowed {
		t.Fatalf("other resource: %+v, want denied by scope deny", d)
	}
}

// TestResourceScopeExactComparison verifies that resource ID comparison is
// case-sensitive, does not trim whitespace, and does not treat asterisks as
// wildcards.
func TestResourceScopeExactComparison(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "Ledger-A"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		resource  string
		wantAllow bool
	}{
		{"exact match", "Ledger-A", true},
		{"different case", "ledger-a", false},
		{"leading space", " Ledger-A", false},
		{"trailing space", "Ledger-A ", false},
		{"wildcard", "Ledger-*", false},
		{"question mark", "Ledger-?", false},
	}
	for _, tc := range cases {
		d := s.Decide("acme", request("acme", "u1", tc.resource, "org/a", "read"))
		if d.Allowed != tc.wantAllow {
			t.Fatalf("%s: allowed=%v, want %v", tc.name, d.Allowed, tc.wantAllow)
		}
	}
}

// TestResourceScopeIDNotInterpretedAsPath verifies that a resource ID
// containing slashes is compared as a whole string, not as a scope path.
func TestResourceScopeIDNotInterpretedAsPath(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "org/a/ledger"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// The ID is compared as a whole string; the slashes are literal.
	if d := s.Decide("acme", request("acme", "u1", "org/a/ledger", "org/a", "read")); !d.Allowed {
		t.Fatalf("ID with slashes: %+v, want allowed", d)
	}
	// A different ID that shares a prefix is denied.
	if d := s.Decide("acme", request("acme", "u1", "org/a/ledger/sub", "org/a", "read")); d.Allowed {
		t.Fatalf("ID with extra segment: %+v, want denied", d)
	}
}

// TestResourceScopeBackwardCompatibleEmptyID verifies that policies with an
// empty ResourceID keep their original JSON fingerprint and match every
// resource in scope, exactly as before resource scoping was added.
func TestResourceScopeBackwardCompatibleEmptyID(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		allowPolicy("p1", "u1", "read", "org/a", false),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// Empty ResourceID matches any resource in scope.
	for _, rid := range []string{"ledger-a", "ledger-b", "anything"} {
		if d := s.Decide("acme", request("acme", "u1", rid, "org/a", "read")); !d.Allowed {
			t.Fatalf("resource %q: %+v, want allowed", rid, d)
		}
	}

	// The fingerprint is the legacy JSON fingerprint (no ResourceID field).
	recs, _, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := fingerprintFor(&recs[0]); got != legacyJSONFingerprint(&recs[0]) {
		t.Fatal("empty ResourceID changed the fingerprint family")
	}
}

// TestResourceScopeAuditPreservesField verifies that the resource ID is
// stored in the audit record and survives export.
func TestResourceScopeAuditPreservesField(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("export must verify: %v", err)
	}
	if got := recs[0].Change.Policies[0].ResourceID; got != "ledger-a" {
		t.Fatalf("stored ResourceID = %q, want %q", got, "ledger-a")
	}
}

// TestResourceScopeRollbackPreservesField verifies that a rollback carries
// the resource ID of the source version into the new version.
func TestResourceScopeRollbackPreservesField(t *testing.T) {
	s := NewStore()
	v1, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Publish a different version.
	if _, err := s.Publish("acme", v1, []Policy{
		resourcePolicy("p2", "u2", "read", "org/a", EffectAllow, false, "ledger-b"),
	}); err != nil {
		t.Fatal(err)
	}
	// Rollback to v1.
	v3, err := s.Rollback("acme", 2, v1)
	if err != nil {
		t.Fatal(err)
	}

	// The rolled-back version has the original resource ID.
	got, err := s.Policies("acme", v3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResourceID != "ledger-a" {
		t.Fatalf("rolled-back policies = %+v, want ResourceID ledger-a", got)
	}

	// The decision uses the rolled-back resource ID.
	if d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read")); !d.Allowed {
		t.Fatalf("rolled-back decision for ledger-a: %+v, want allowed", d)
	}
	if d := s.Decide("acme", request("acme", "u1", "ledger-b", "org/a", "read")); d.Allowed {
		t.Fatalf("rolled-back decision for ledger-b: %+v, want denied", d)
	}
}

// TestResourceScopeReviewUsesHistoricalVersion verifies that reviewing a
// historical version uses that version's resource ID, not the current one.
func TestResourceScopeReviewUsesHistoricalVersion(t *testing.T) {
	s := NewStore()
	v1, _ := s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	v2, _ := s.Publish("acme", v1, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-b"),
	})

	// Review v1: ledger-a allowed, ledger-b denied.
	if d := s.Review("acme", v1, request("acme", "u1", "ledger-a", "org/a", "read")); !d.Allowed {
		t.Fatalf("v1 review ledger-a: %+v, want allowed", d)
	}
	if d := s.Review("acme", v1, request("acme", "u1", "ledger-b", "org/a", "read")); d.Allowed {
		t.Fatalf("v1 review ledger-b: %+v, want denied", d)
	}

	// Review v2: ledger-a denied, ledger-b allowed.
	if d := s.Review("acme", v2, request("acme", "u1", "ledger-a", "org/a", "read")); d.Allowed {
		t.Fatalf("v2 review ledger-a: %+v, want denied", d)
	}
	if d := s.Review("acme", v2, request("acme", "u1", "ledger-b", "org/a", "read")); !d.Allowed {
		t.Fatalf("v2 review ledger-b: %+v, want allowed", d)
	}
}

// TestResourceScopeOfflineReview verifies that offline review from exported
// material uses the resource ID stored in the historical version.
func TestResourceScopeOfflineReview(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	original := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))
	denied := s.Decide("acme", request("acme", "u1", "ledger-b", "org/a", "read"))

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	// The allowed decision is consistent.
	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Consistent || !reflect.DeepEqual(got.Recomputed, original) {
		t.Fatalf("allowed review = %+v, want consistent with %+v", got, original)
	}

	// The denied decision is consistent.
	got, err = RecheckDecisionOffline("acme", recs, cp, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Consistent || !reflect.DeepEqual(got.Recomputed, denied) {
		t.Fatalf("denied review = %+v, want consistent with %+v", got, denied)
	}
}

// TestResourceScopeTamperDetection verifies that modifying the resource ID
// in exported material fails verification and offline review.
func TestResourceScopeTamperDetection(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))

	good, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with the resource ID in the policy change record.
	tampered := cloneExportedRecords(good)
	tampered[0].Change.Policies[0].ResourceID = "ledger-b"
	if err := VerifyAudit("acme", tampered, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("VerifyAudit err = %v, want ErrInvalidRange", err)
	}
	if _, err := RecheckDecisionOffline("acme", tampered, cp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("offline review err = %v, want ErrInvalidRange", err)
	}
}

// TestResourceScopeNonUTF8Preserved verifies that a resource ID containing
// invalid UTF-8 bytes is preserved byte-for-byte and that tampering with
// those bytes fails verification.
func TestResourceScopeNonUTF8Preserved(t *testing.T) {
	s := NewStore()
	rawID := "ledger" + invalidByte + "-a"
	policies := []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, rawID),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// The decision matches the raw bytes.
	d := s.Decide("acme", request("acme", "u1", rawID, "org/a", "read"))
	if !d.Allowed {
		t.Fatalf("decision with raw-byte resource ID: %+v, want allowed", d)
	}

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("export with raw-byte resource ID must verify: %v", err)
	}

	// The exported resource ID is byte-for-byte the submitted content.
	if got := recs[0].Change.Policies[0].ResourceID; got != rawID {
		t.Fatalf("exported ResourceID = %q, want byte-exact %q", got, rawID)
	}
	if !strings.Contains(recs[0].Change.Policies[0].ResourceID, invalidByte) {
		t.Fatal("invalid byte was normalized out of the resource ID")
	}

	// The fingerprint uses the raw encoding (not JSON).
	if got := fingerprintFor(&recs[0]); got == legacyJSONFingerprint(&recs[0]) {
		t.Fatal("raw-byte resource ID record still uses the JSON fingerprint")
	}

	// Tampering with the raw byte fails verification.
	tampered := cloneExportedRecords(recs)
	tampered[0].Change.Policies[0].ResourceID = strings.Replace(
		tampered[0].Change.Policies[0].ResourceID, invalidByte, anotherInvalidByte, 1)
	if err := VerifyAudit("acme", tampered, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tampered raw byte err = %v, want ErrInvalidRange", err)
	}

	// Offline review is consistent for the raw-byte resource ID.
	review, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Consistent || !reflect.DeepEqual(review.Recomputed, d) {
		t.Fatalf("offline review = %+v, want consistent with %+v", review, d)
	}
}

// TestResourceScopeMissingResourceIDStillRejected verifies that a request
// without a resource ID is still rejected by the envelope check, even when
// a resource-scoped policy exists.
func TestResourceScopeMissingResourceIDStillRejected(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	req := request("acme", "u1", "", "org/a", "read")
	d := s.Decide("acme", req)
	if d.Allowed || d.Reason != "missing resource id" {
		t.Fatalf("missing resource ID: %+v, want denied with missing resource id", d)
	}
}

// TestResourceScopeDisabledSubjectStillRejected verifies that a disabled
// subject is still rejected, even when the resource ID matches.
func TestResourceScopeDisabledSubjectStillRejected(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	req := request("acme", "u1", "ledger-a", "org/a", "read")
	req.Subject.Disabled = true
	d := s.Decide("acme", req)
	if d.Allowed || d.Reason != "subject is disabled" {
		t.Fatalf("disabled subject: %+v, want denied", d)
	}
}

// TestResourceScopeOrgMismatchStillRejected verifies that an organization
// mismatch is still rejected, even when the resource ID matches.
func TestResourceScopeOrgMismatchStillRejected(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	req := request("acme", "u1", "ledger-a", "org/a", "read")
	req.SubjectOrg = "globex"
	d := s.Decide("acme", req)
	if d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("org mismatch: %+v, want denied", d)
	}
}

// TestResourceScopeVersionConflictNoChange verifies that a version conflict
// or invalid policy set does not change the version or audit records.
func TestResourceScopeVersionConflictNoChange(t *testing.T) {
	s := NewStore()
	v1, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "ledger-a"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stale expected version.
	if _, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p2", "u2", "read", "org/a", EffectAllow, false, "ledger-b"),
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale publish err = %v, want ErrVersionConflict", err)
	}

	// Invalid policy set (empty ID).
	if _, err := s.Publish("acme", v1, []Policy{
		resourcePolicy("", "u2", "read", "org/a", EffectAllow, false, "ledger-b"),
	}); !errors.Is(err, ErrInvalidPolicySet) {
		t.Fatalf("invalid publish err = %v, want ErrInvalidPolicySet", err)
	}

	// Version and policies are unchanged.
	if got := s.CurrentVersion("acme"); got != v1 {
		t.Fatalf("current version = %d, want %d", got, v1)
	}
	got, err := s.Policies("acme", v1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResourceID != "ledger-a" {
		t.Fatalf("policies changed after failed publish: %+v", got)
	}
}

// TestResourceScopeResourceNotPreRegistered verifies that a policy can
// name a resource ID that has never been seen before; no pre-registration
// is required.
func TestResourceScopeResourceNotPreRegistered(t *testing.T) {
	s := NewStore()
	// Publish a policy for a resource ID that has never been requested.
	if _, err := s.Publish("acme", 0, []Policy{
		resourcePolicy("p1", "u1", "read", "org/a", EffectAllow, false, "brand-new-resource"),
	}); err != nil {
		t.Fatal(err)
	}
	// The first request for that resource is allowed.
	if d := s.Decide("acme", request("acme", "u1", "brand-new-resource", "org/a", "read")); !d.Allowed {
		t.Fatalf("first request for pre-registered resource: %+v, want allowed", d)
	}
}

// TestResourceScopeMatchedListOnlyActualMatches verifies that the matched
// list contains only policies that meet all conditions, and that deny
// overrides allow with sorted deduped IDs.
func TestResourceScopeMatchedListOnlyActualMatches(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		resourcePolicy("z-deny-a", "u1", "read", "org/a", EffectDeny, false, "ledger-a"),
		allowPolicy("m-allow", "u1", "read", "org/a", true),
		resourcePolicy("a-allow-b", "u1", "read", "org/a", EffectAllow, false, "ledger-b"),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}

	// Request for ledger-a: scope-allow matches, resource-deny matches.
	// resource-allow-b does not match (different resource ID).
	d := s.Decide("acme", request("acme", "u1", "ledger-a", "org/a", "read"))
	if d.Allowed {
		t.Fatalf("ledger-a: %+v, want denied", d)
	}
	want := []string{"m-allow", "z-deny-a"}
	if !reflect.DeepEqual(d.Matched, want) {
		t.Fatalf("matched = %v, want %v", d.Matched, want)
	}

	// Request for ledger-b: scope-allow matches, resource-allow-b matches.
	// resource-deny-a does not match (different resource ID).
	d = s.Decide("acme", request("acme", "u1", "ledger-b", "org/a", "read"))
	if !d.Allowed {
		t.Fatalf("ledger-b: %+v, want allowed", d)
	}
	want = []string{"a-allow-b", "m-allow"}
	if !reflect.DeepEqual(d.Matched, want) {
		t.Fatalf("matched = %v, want %v", d.Matched, want)
	}
}

// TestResourceScopeAccessFunctionStillWorks verifies that the legacy
// Access function is unaffected by the Policy struct change.
func TestResourceScopeAccessFunctionStillWorks(t *testing.T) {
	subject := Subject{ID: "u1", Roles: []string{"org/a:read"}}
	resource := Resource{ID: "ledger-a", Scope: "org/a"}
	d := Access(subject, resource, "read")
	if !d.Allowed || d.Reason != "role grants read:org/a" {
		t.Fatalf("Access = %+v, want allowed", d)
	}

	// Disabled subject is still rejected.
	subject.Disabled = true
	d = Access(subject, resource, "read")
	if d.Allowed || d.Reason != "subject is disabled" {
		t.Fatalf("disabled Access = %+v, want denied", d)
	}
}
