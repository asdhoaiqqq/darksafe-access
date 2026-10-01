package darksafe

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

const testOrg = "acme"

func testSubject(id string) Subject {
	return Subject{ID: id, Org: testOrg}
}

func testResource(id, scope string) Resource {
	return Resource{ID: id, Scope: scope, Org: testOrg}
}

func allowPolicy(id, subject, action, scope string, recursive bool) Policy {
	return Policy{ID: id, Subject: subject, Action: action, Scope: scope, Effect: EffectAllow, Recursive: recursive}
}

func denyPolicy(id, subject, action, scope string) Policy {
	return Policy{ID: id, Subject: subject, Action: action, Scope: scope, Effect: EffectDeny}
}

// --- path validation ---

func TestValidScopePath(t *testing.T) {
	valid := []string{"a", "a/b", "a/b/c", "org/payments/ledger", "org/a"}
	for _, p := range valid {
		if !validScopePath(p) {
			t.Errorf("expected %q to be valid", p)
		}
	}
	invalid := []string{"", "/a", "a/", "/a/", "a//b", "a/./b", "a/../b", ".", "..", "a/.", "a/..", "./a", "../a"}
	for _, p := range invalid {
		if validScopePath(p) {
			t.Errorf("expected %q to be invalid", p)
		}
	}
}

// --- scope matching ---

func TestScopeCovers(t *testing.T) {
	cases := []struct {
		policy, resource string
		recursive        bool
		want             bool
	}{
		{"org/a", "org/a", false, true},
		{"org/a", "org/a", true, true},
		{"org/a", "org/a/b", false, false},
		{"org/a", "org/a/b", true, true},
		{"org/a", "org/a/b/c", true, true},
		{"org/a", "org/ab", true, false}, // segment boundary: org/a does not cover org/ab
		{"org/a", "org/ab/c", true, false},
		{"org/a", "org", true, false},
		{"org/a", "org/a", true, true},
	}
	for _, c := range cases {
		if got := scopeCovers(c.policy, c.resource, c.recursive); got != c.want {
			t.Errorf("scopeCovers(%q, %q, %v) = %v, want %v", c.policy, c.resource, c.recursive, got, c.want)
		}
	}
}

// --- publish validation ---

func TestPublishValidation(t *testing.T) {
	cases := []struct {
		name     string
		policies []Policy
	}{
		{"duplicate id", []Policy{allowPolicy("p1", "u1", "read", "a", false), allowPolicy("p1", "u2", "read", "a", false)}},
		{"empty id", []Policy{allowPolicy("", "u1", "read", "a", false)}},
		{"empty subject", []Policy{{ID: "p1", Subject: "", Action: "read", Scope: "a", Effect: EffectAllow}}},
		{"empty action", []Policy{{ID: "p1", Subject: "u1", Action: "", Scope: "a", Effect: EffectAllow}}},
		{"invalid scope empty", []Policy{{ID: "p1", Subject: "u1", Action: "read", Scope: "", Effect: EffectAllow}}},
		{"invalid scope leading slash", []Policy{{ID: "p1", Subject: "u1", Action: "read", Scope: "/a", Effect: EffectAllow}}},
		{"invalid scope trailing slash", []Policy{{ID: "p1", Subject: "u1", Action: "read", Scope: "a/", Effect: EffectAllow}}},
		{"invalid scope consecutive slashes", []Policy{{ID: "p1", Subject: "u1", Action: "read", Scope: "a//b", Effect: EffectAllow}}},
		{"invalid scope dot segment", []Policy{{ID: "p1", Subject: "u1", Action: "read", Scope: "a/./b", Effect: EffectAllow}}},
		{"invalid scope dotdot segment", []Policy{{ID: "p1", Subject: "u1", Action: "read", Scope: "a/../b", Effect: EffectAllow}}},
		{"unknown effect", []Policy{{ID: "p1", Subject: "u1", Action: "read", Scope: "a", Effect: "maybe"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore()
			_, err := s.Publish(testOrg, 0, c.policies)
			if !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("expected ErrInvalidPolicy, got %v", err)
			}
			// validation failure must not change current version or history
			if v, _ := s.CurrentVersion(testOrg); v != 0 {
				t.Errorf("current version = %d, want 0", v)
			}
			if _, err := s.Policies(testOrg, 1); !errors.Is(err, ErrVersionNotFound) {
				t.Errorf("version 1 should not exist, got %v", err)
			}
		})
	}
}

func TestPublishEmptySet(t *testing.T) {
	s := NewStore()
	v, err := s.Publish(testOrg, 0, nil)
	if err != nil {
		t.Fatalf("publish empty set: %v", err)
	}
	if v != 1 {
		t.Errorf("version = %d, want 1", v)
	}
	d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a"), "read")
	if d.Allowed {
		t.Errorf("empty policy set should deny, got %+v", d)
	}
	if d.Version != 1 {
		t.Errorf("decision version = %d, want 1", d.Version)
	}
}

func TestPublishEmptyOrg(t *testing.T) {
	s := NewStore()
	_, err := s.Publish("", 0, nil)
	if !errors.Is(err, ErrInvalidOrganization) {
		t.Fatalf("expected ErrInvalidOrganization, got %v", err)
	}
}

// --- versioning ---

func TestVersionSequence(t *testing.T) {
	s := NewStore()
	if v, _ := s.CurrentVersion(testOrg); v != 0 {
		t.Fatalf("initial version = %d, want 0", v)
	}
	for want := 1; want <= 3; want++ {
		v, err := s.Publish(testOrg, want-1, []Policy{allowPolicy(fmt.Sprintf("p%d", want), "u1", "read", "a", false)})
		if err != nil {
			t.Fatalf("publish %d: %v", want, err)
		}
		if v != want {
			t.Errorf("version = %d, want %d", v, want)
		}
	}
}

func TestVersionConflict(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish(testOrg, 0, nil); err != nil {
		t.Fatal(err)
	}
	// stale expected version
	_, err := s.Publish(testOrg, 0, nil)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	// future expected version
	_, err = s.Publish(testOrg, 5, nil)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	// version unchanged after conflict
	if v, _ := s.CurrentVersion(testOrg); v != 1 {
		t.Errorf("version = %d, want 1", v)
	}
}

func TestConcurrentPublish(t *testing.T) {
	s := NewStore()
	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	conflicts := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := s.Publish(testOrg, 0, []Policy{allowPolicy("p", "u1", "read", "a", false)})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrVersionConflict):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Errorf("success = %d, want 1", success)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
	if v, _ := s.CurrentVersion(testOrg); v != 1 {
		t.Errorf("version = %d, want 1", v)
	}
}

// --- decisions ---

func TestDecideDefaultDeny(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish(testOrg, 0, nil); err != nil {
		t.Fatal(err)
	}
	d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a"), "read")
	if d.Allowed {
		t.Errorf("should deny by default, got %+v", d)
	}
	if d.Reason != ReasonNoMatchingPolicy {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonNoMatchingPolicy)
	}
}

func TestDecideAllow(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a/b", false)})
	d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a/b"), "read")
	if !d.Allowed {
		t.Errorf("should allow, got %+v", d)
	}
	if d.Reason != ReasonAllowedByPolicy {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonAllowedByPolicy)
	}
	if d.Version != 1 {
		t.Errorf("version = %d, want 1", d.Version)
	}
}

func TestDecideDenyPrecedence(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		allowPolicy("allow-1", "u1", "read", "a", true),
		denyPolicy("deny-1", "u1", "read", "a/b"),
	}
	_, _ = s.Publish(testOrg, 0, policies)
	d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a/b"), "read")
	if d.Allowed {
		t.Errorf("deny should take precedence, got %+v", d)
	}
	if d.Reason != ReasonDeniedByPolicy {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonDeniedByPolicy)
	}
}

func TestDecideDisabledSubject(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	subject := testSubject("u1")
	subject.Disabled = true
	d := s.Decide(testOrg, subject, testResource("r1", "a"), "read")
	if d.Allowed {
		t.Errorf("disabled subject should be denied, got %+v", d)
	}
	if d.Reason != ReasonSubjectDisabled {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonSubjectDisabled)
	}
}

func TestDecideRolesDoNotGrant(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, nil)
	subject := testSubject("u1")
	subject.Roles = []string{"owner", "a:read"}
	d := s.Decide(testOrg, subject, testResource("r1", "a"), "read")
	if d.Allowed {
		t.Errorf("roles must not grant access under a published policy set, got %+v", d)
	}
}

func TestDecideScopeSegmentBoundary(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", true)})
	cases := []struct {
		scope string
		want  bool
	}{
		{"org/a", true},
		{"org/a/b", true},
		{"org/a/b/c", true},
		{"org/ab", false},
		{"org/ab/c", false},
	}
	for _, c := range cases {
		d := s.Decide(testOrg, testSubject("u1"), testResource("r1", c.scope), "read")
		if d.Allowed != c.want {
			t.Errorf("scope %q: allowed = %v, want %v", c.scope, d.Allowed, c.want)
		}
	}
}

func TestDecideRecursiveVsExact(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{
		allowPolicy("exact", "u1", "read", "a", false),
		allowPolicy("recursive", "u2", "read", "a", true),
	})
	if d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a/b"), "read"); d.Allowed {
		t.Errorf("exact policy should not cover sub-scope, got %+v", d)
	}
	if d := s.Decide(testOrg, testSubject("u2"), testResource("r1", "a/b"), "read"); !d.Allowed {
		t.Errorf("recursive policy should cover sub-scope, got %+v", d)
	}
}

// --- request validation / org isolation ---

func TestDecideMissingFields(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	cases := []struct {
		name     string
		org      string
		subject  Subject
		resource Resource
		action   string
		reason   string
	}{
		{"missing org", "", testSubject("u1"), testResource("r1", "a"), "read", ReasonMissingOrganization},
		{"missing subject org", testOrg, Subject{ID: "u1"}, testResource("r1", "a"), "read", ReasonMissingSubjectOrg},
		{"missing resource org", testOrg, testSubject("u1"), Resource{ID: "r1", Scope: "a"}, "read", ReasonMissingResourceOrg},
		{"subject org mismatch", testOrg, Subject{ID: "u1", Org: "other"}, testResource("r1", "a"), "read", ReasonSubjectOrgMismatch},
		{"resource org mismatch", testOrg, testSubject("u1"), Resource{ID: "r1", Scope: "a", Org: "other"}, "read", ReasonResourceOrgMismatch},
		{"missing subject id", testOrg, Subject{Org: testOrg}, testResource("r1", "a"), "read", ReasonMissingSubject},
		{"missing resource id", testOrg, testSubject("u1"), Resource{Org: testOrg, Scope: "a"}, "read", ReasonMissingResource},
		{"missing action", testOrg, testSubject("u1"), testResource("r1", "a"), "", ReasonMissingAction},
		{"invalid scope", testOrg, testSubject("u1"), testResource("r1", "a//b"), "read", ReasonInvalidScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := s.Decide(c.org, c.subject, c.resource, c.action)
			if d.Allowed {
				t.Errorf("should be denied, got %+v", d)
			}
			if !strings.HasPrefix(d.Reason, c.reason) {
				t.Errorf("reason = %q, want prefix %q", d.Reason, c.reason)
			}
		})
	}
}

func TestDecideOrgIsolation(t *testing.T) {
	s := NewStore()
	// same subject id, different policies per org
	_, _ = s.Publish("org-a", 0, []Policy{allowPolicy("p-a", "u1", "read", "a", false)})
	_, _ = s.Publish("org-b", 0, []Policy{denyPolicy("p-b", "u1", "read", "a")})
	dA := s.Decide("org-a", Subject{ID: "u1", Org: "org-a"}, Resource{ID: "r1", Scope: "a", Org: "org-a"}, "read")
	if !dA.Allowed {
		t.Errorf("org-a should allow, got %+v", dA)
	}
	dB := s.Decide("org-b", Subject{ID: "u1", Org: "org-b"}, Resource{ID: "r1", Scope: "a", Org: "org-b"}, "read")
	if dB.Allowed {
		t.Errorf("org-b should deny, got %+v", dB)
	}
	// cross-org request: subject from org-a presented to org-b
	dCross := s.Decide("org-b", Subject{ID: "u1", Org: "org-a"}, Resource{ID: "r1", Scope: "a", Org: "org-b"}, "read")
	if dCross.Allowed {
		t.Errorf("cross-org request must be denied even if a policy would match, got %+v", dCross)
	}
	if dCross.Reason != ReasonSubjectOrgMismatch {
		t.Errorf("reason = %q, want %q", dCross.Reason, ReasonSubjectOrgMismatch)
	}
}

func TestDecideNewOrgNoVersion(t *testing.T) {
	s := NewStore()
	org := "never-published"
	d := s.Decide(org, Subject{ID: "u1", Org: org}, Resource{ID: "r1", Scope: "a", Org: org}, "read")
	if d.Allowed {
		t.Errorf("new org at version 0 cannot decide, got %+v", d)
	}
	if d.Reason != ReasonNoPublishedVersion {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonNoPublishedVersion)
	}
	if d.Version != 0 {
		t.Errorf("version = %d, want 0", d.Version)
	}
}

// --- explanation ---

func TestMatchPoliciesDedup(t *testing.T) {
	// matchPolicies must dedupe and sort even if given duplicate ids directly
	policies := []Policy{
		allowPolicy("zebra", "u1", "read", "a", true),
		allowPolicy("alpha", "u1", "read", "a/b", true),
		denyPolicy("alpha", "u1", "read", "a/b"), // duplicate id
		allowPolicy("beta", "u1", "read", "a/b", true),
	}
	matched, hasAllow, hasDeny := matchPolicies(policies, "u1", "read", "a/b")
	want := []string{"alpha", "beta", "zebra"}
	if len(matched) != len(want) {
		t.Fatalf("matched = %v, want %v", matched, want)
	}
	for i := range want {
		if matched[i] != want[i] {
			t.Errorf("matched[%d] = %q, want %q", i, matched[i], want[i])
		}
	}
	if !hasAllow || !hasDeny {
		t.Errorf("hasAllow=%v hasDeny=%v, want both true", hasAllow, hasDeny)
	}
}

func TestExplainSortedDeduped(t *testing.T) {
	s := NewStore()
	// policies submitted in non-sorted order; ids are unique (duplicate ids
	// are rejected at publish time). The explanation must still be sorted.
	policies := []Policy{
		allowPolicy("zebra", "u1", "read", "org/a", true),
		allowPolicy("alpha", "u1", "read", "org/a/b", true),
		denyPolicy("middle", "u1", "read", "org/a/b/c"),
		allowPolicy("beta", "u1", "read", "org/a/b", true),
	}
	_, _ = s.Publish(testOrg, 0, policies)
	d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "org/a/b/c"), "read")
	want := []string{"alpha", "beta", "middle", "zebra"}
	if len(d.Matched) != len(want) {
		t.Fatalf("matched = %v, want %v", d.Matched, want)
	}
	for i := range want {
		if d.Matched[i] != want[i] {
			t.Errorf("matched[%d] = %q, want %q (full: %v)", i, d.Matched[i], want[i], d.Matched)
		}
	}
}

func TestExplainOrderIndependent(t *testing.T) {
	s1 := NewStore()
	s2 := NewStore()
	set1 := []Policy{allowPolicy("a", "u1", "read", "x", true), allowPolicy("b", "u1", "read", "x/y", true)}
	set2 := []Policy{allowPolicy("b", "u1", "read", "x/y", true), allowPolicy("a", "u1", "read", "x", true)}
	_, _ = s1.Publish(testOrg, 0, set1)
	_, _ = s2.Publish(testOrg, 0, set2)
	d1 := s1.Decide(testOrg, testSubject("u1"), testResource("r1", "x/y"), "read")
	d2 := s2.Decide(testOrg, testSubject("u1"), testResource("r1", "x/y"), "read")
	if fmt.Sprint(d1.Matched) != fmt.Sprint(d2.Matched) {
		t.Errorf("matched lists differ: %v vs %v", d1.Matched, d2.Matched)
	}
}

// --- rollback ---

func TestRollback(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	_, _ = s.Publish(testOrg, 1, []Policy{denyPolicy("p2", "u1", "read", "a")})
	// current is 2; rollback to version 1 publishes its content as version 3
	v, err := s.Rollback(testOrg, 2, 1)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if v != 3 {
		t.Errorf("rollback version = %d, want 3", v)
	}
	d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a"), "read")
	if !d.Allowed {
		t.Errorf("after rollback to v1 should allow, got %+v", d)
	}
	if d.Version != 3 {
		t.Errorf("decision version = %d, want 3", d.Version)
	}
	// history still contains version 1 with its original content
	pols, err := s.Policies(testOrg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pols) != 1 || pols[0].ID != "p1" {
		t.Errorf("version 1 content changed: %+v", pols)
	}
}

func TestRollbackTargetNotFound(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	_, _ = s.Publish(testOrg, 1, nil)
	for _, target := range []int{0, 3, -1} {
		_, err := s.Rollback(testOrg, 2, target)
		if !errors.Is(err, ErrVersionNotFound) {
			t.Errorf("rollback to %d: expected ErrVersionNotFound, got %v", target, err)
		}
	}
	// current version and history unchanged
	if v, _ := s.CurrentVersion(testOrg); v != 2 {
		t.Errorf("version = %d, want 2", v)
	}
}

func TestRollbackConflict(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, nil)
	_, _ = s.Publish(testOrg, 1, nil)
	_, err := s.Rollback(testOrg, 1, 1) // stale expected
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	if v, _ := s.CurrentVersion(testOrg); v != 2 {
		t.Errorf("version = %d, want 2", v)
	}
}

func TestRollbackDoesNotRewriteHistory(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	_, _ = s.Rollback(testOrg, 1, 1)
	// version 1 still has exactly its original content
	pols, _ := s.Policies(testOrg, 1)
	if len(pols) != 1 || pols[0].ID != "p1" || pols[0].Effect != EffectAllow {
		t.Errorf("version 1 was rewritten: %+v", pols)
	}
}

// --- review ---

func TestReviewHistoricalVersion(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	_, _ = s.Publish(testOrg, 1, []Policy{denyPolicy("p2", "u1", "read", "a")})
	// review against version 1 still allows after version 2 denies it
	r := s.Review(testOrg, 1, testSubject("u1"), testResource("r1", "a"), "read")
	if !r.Allowed {
		t.Errorf("review of v1 should allow, got %+v", r)
	}
	if r.Version != 1 {
		t.Errorf("review version = %d, want 1", r.Version)
	}
	// current decision denies
	d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a"), "read")
	if d.Allowed {
		t.Errorf("current decision should deny, got %+v", d)
	}
}

func TestReviewImmutableAcrossLaterChanges(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	r1 := s.Review(testOrg, 1, testSubject("u1"), testResource("r1", "a"), "read")
	_, _ = s.Publish(testOrg, 1, []Policy{denyPolicy("p2", "u1", "read", "a")})
	_, _ = s.Rollback(testOrg, 2, 1)
	r2 := s.Review(testOrg, 1, testSubject("u1"), testResource("r1", "a"), "read")
	if r1.Allowed != r2.Allowed || r1.Reason != r2.Reason || fmt.Sprint(r1.Matched) != fmt.Sprint(r2.Matched) {
		t.Errorf("review changed after later publish/rollback: %+v vs %+v", r1, r2)
	}
}

func TestReviewNonExistentVersion(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, nil)
	for _, v := range []int{0, 2, -1} {
		r := s.Review(testOrg, v, testSubject("u1"), testResource("r1", "a"), "read")
		if r.Allowed {
			t.Errorf("review of version %d should deny, got %+v", v, r)
		}
		if r.Reason == ReasonAllowedByPolicy || r.Reason == ReasonNoMatchingPolicy {
			t.Errorf("review of version %d should report version not found, got %q", v, r.Reason)
		}
	}
}

func TestReviewDoesNotUseCurrentAsFallback(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	// version 2 does not exist; even though current (1) allows, review must deny
	r := s.Review(testOrg, 2, testSubject("u1"), testResource("r1", "a"), "read")
	if r.Allowed {
		t.Errorf("review of non-existent version must not fall back to current, got %+v", r)
	}
	if r.Version != 2 {
		t.Errorf("review version = %d, want 2", r.Version)
	}
}

// --- defensive copies ---

func TestPublishDefensiveCopy(t *testing.T) {
	s := NewStore()
	policies := []Policy{allowPolicy("p1", "u1", "read", "a", false)}
	_, _ = s.Publish(testOrg, 0, policies)
	// mutate the caller's slice after publish
	policies[0].ID = "mutated"
	policies[0].Effect = EffectDeny
	got, _ := s.Policies(testOrg, 1)
	if got[0].ID != "p1" || got[0].Effect != EffectAllow {
		t.Errorf("published content changed after caller mutation: %+v", got)
	}
}

func TestQueryDefensiveCopy(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	got, _ := s.Policies(testOrg, 1)
	got[0].ID = "mutated"
	got[0].Effect = EffectDeny
	again, _ := s.Policies(testOrg, 1)
	if again[0].ID != "p1" || again[0].Effect != EffectAllow {
		t.Errorf("published content changed after query result mutation: %+v", again)
	}
}

// --- legacy Access unchanged ---

func TestLegacyAccessStillWorks(t *testing.T) {
	d := Access(Subject{ID: "u1", Roles: []string{"owner"}}, Resource{ID: "r1", Scope: "a"}, "read")
	if !d.Allowed {
		t.Errorf("legacy Access should still grant via owner role, got %+v", d)
	}
	d = Access(Subject{ID: "u1", Disabled: true}, Resource{ID: "r1"}, "read")
	if d.Allowed {
		t.Errorf("legacy Access should deny disabled subject, got %+v", d)
	}
}

// --- full policy set per decision ---

func TestDecisionUsesOneVersion(t *testing.T) {
	s := NewStore()
	_, _ = s.Publish(testOrg, 0, []Policy{allowPolicy("p1", "u1", "read", "a", false)})
	// v1 allows; a concurrent publish creates v2 that denies. Each decision
	// must be internally consistent: fully v1 (allow, matched p1) or fully v2
	// (deny, matched p2) — never a mix of the two.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			d := s.Decide(testOrg, testSubject("u1"), testResource("r1", "a"), "read")
			switch d.Version {
			case 1:
				if !d.Allowed || d.Reason != ReasonAllowedByPolicy || len(d.Matched) != 1 || d.Matched[0] != "p1" {
					t.Errorf("v1 decision inconsistent: %+v", d)
				}
			case 2:
				if d.Allowed || d.Reason != ReasonDeniedByPolicy || len(d.Matched) != 1 || d.Matched[0] != "p2" {
					t.Errorf("v2 decision inconsistent: %+v", d)
				}
			default:
				t.Errorf("decision version = %d, want 1 or 2", d.Version)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cur, _ := s.CurrentVersion(testOrg)
			if cur == 1 {
				_, _ = s.Publish(testOrg, cur, []Policy{denyPolicy("p2", "u1", "read", "a")})
			}
		}
	}()
	wg.Wait()
}
