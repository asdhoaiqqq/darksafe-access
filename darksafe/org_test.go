package darksafe

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

func allowPolicy(id, subject, action, scope string, recursive bool) Policy {
	return Policy{ID: id, Subject: subject, Action: action, Scope: scope, Effect: EffectAllow, Recursive: recursive}
}

func request(org, subjectID, resourceID, scope, action string) OrgRequest {
	return OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     Subject{ID: subjectID},
		Resource:    Resource{ID: resourceID, Scope: scope},
		Action:      action,
	}
}

func TestPublishVersionsIncrement(t *testing.T) {
	s := NewStore()
	if got := s.CurrentVersion("acme"); got != 0 {
		t.Fatalf("new org current version = %d, want 0", got)
	}
	v, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	if err != nil || v != 1 {
		t.Fatalf("first publish = %d, %v; want 1, nil", v, err)
	}
	v, err = s.Publish("acme", 1, nil)
	if err != nil || v != 2 {
		t.Fatalf("second publish = %d, %v; want 2, nil", v, err)
	}
	if got := s.CurrentVersion("acme"); got != 2 {
		t.Fatalf("current version = %d, want 2", got)
	}
}

func TestPublishVersionConflict(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish("acme", 0, nil); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale publish err = %v, want ErrVersionConflict", err)
	}
	if got := s.CurrentVersion("acme"); got != 1 {
		t.Fatalf("current version = %d, want 1 after failed publish", got)
	}
}

func TestConcurrentPublishSameVersionAtMostOneWins(t *testing.T) {
	s := NewStore()
	const n = 16
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Publish("acme", 0, nil)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	succeeded, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if succeeded != 1 || conflicts != n-1 {
		t.Fatalf("succeeded=%d conflicts=%d, want 1 and %d", succeeded, conflicts, n-1)
	}
	if got := s.CurrentVersion("acme"); got != 1 {
		t.Fatalf("current version = %d, want 1", got)
	}
}

func TestPublishValidationFailuresDoNotConsumeVersion(t *testing.T) {
	cases := map[string][]Policy{
		"empty id":        {{ID: "", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow}},
		"duplicate id":    {allowPolicy("p1", "u1", "read", "org/a", false), allowPolicy("p1", "u2", "read", "org/a", false)},
		"missing subject": {{ID: "p1", Action: "read", Scope: "org/a", Effect: EffectAllow}},
		"missing action":  {{ID: "p1", Subject: "u1", Scope: "org/a", Effect: EffectAllow}},
		"unknown effect":  {{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: "maybe"}},
		"empty scope":     {{ID: "p1", Subject: "u1", Action: "read", Scope: "", Effect: EffectAllow}},
		"leading slash":   {{ID: "p1", Subject: "u1", Action: "read", Scope: "/org/a", Effect: EffectAllow}},
		"trailing slash":  {{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a/", Effect: EffectAllow}},
		"double slash":    {{ID: "p1", Subject: "u1", Action: "read", Scope: "org//a", Effect: EffectAllow}},
		"dot segment":     {{ID: "p1", Subject: "u1", Action: "read", Scope: "org/./a", Effect: EffectAllow}},
		"dotdot segment":  {{ID: "p1", Subject: "u1", Action: "read", Scope: "org/../a", Effect: EffectAllow}},
	}
	for name, policies := range cases {
		s := NewStore()
		if _, err := s.Publish("acme", 0, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Publish("acme", 1, policies); !errors.Is(err, ErrInvalidPolicySet) {
			t.Fatalf("%s: err = %v, want ErrInvalidPolicySet", name, err)
		}
		if got := s.CurrentVersion("acme"); got != 1 {
			t.Fatalf("%s: current version = %d, want 1 (failure must not consume a version)", name, got)
		}
		if _, err := s.Policies("acme", 2); !errors.Is(err, ErrVersionNotFound) {
			t.Fatalf("%s: version 2 must not exist, err = %v", name, err)
		}
	}
}

func TestEmptyPolicySetPublishesAndDeniesAll(t *testing.T) {
	s := NewStore()
	v, err := s.Publish("acme", 0, nil)
	if err != nil || v != 1 {
		t.Fatalf("publish empty set = %d, %v", v, err)
	}
	d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if d.Allowed || d.Version != 1 {
		t.Fatalf("decision = %+v, want denied with version 1", d)
	}
}

func TestDecideRequiresPublishedVersion(t *testing.T) {
	s := NewStore()
	d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if d.Allowed || d.Version != 0 {
		t.Fatalf("decision = %+v, want denied with version 0", d)
	}
}

func TestDecideOrgChecks(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	// An allow policy matches, but the subject belongs to another org.
	req := request("acme", "u1", "r1", "org/a", "read")
	req.SubjectOrg = "other"
	if d := s.Decide("acme", req); d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("subject org mismatch: %+v", d)
	}
	// Resource belongs to another org.
	req = request("acme", "u1", "r1", "org/a", "read")
	req.ResourceOrg = "other"
	if d := s.Decide("acme", req); d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("resource org mismatch: %+v", d)
	}
}

func TestDecideMissingIdentifiersHaveDistinctReasons(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	base := request("acme", "u1", "r1", "org/a", "read")

	reasons := map[string]string{}
	mutate := map[string]func(*OrgRequest){
		"subject org":  func(r *OrgRequest) { r.SubjectOrg = "" },
		"resource org": func(r *OrgRequest) { r.ResourceOrg = "" },
		"subject id":   func(r *OrgRequest) { r.Subject.ID = "" },
		"resource id":  func(r *OrgRequest) { r.Resource.ID = "" },
		"action":       func(r *OrgRequest) { r.Action = "" },
	}
	for name, fn := range mutate {
		req := base
		fn(&req)
		d := s.Decide("acme", req)
		if d.Allowed {
			t.Fatalf("missing %s: request was allowed", name)
		}
		reasons[name] = d.Reason
	}
	seen := map[string]bool{}
	for name, reason := range reasons {
		if reason == "" || seen[reason] {
			t.Fatalf("missing %s: reason %q is empty or not distinguishable", name, reason)
		}
		seen[reason] = true
	}
	if d := s.Decide("", base); d.Allowed || d.Reason == "" {
		t.Fatalf("missing decision org: %+v", d)
	}
}

func TestDecideInvalidScopeAndDisabledSubject(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", true)}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "/org/a", "org/a/", "org//a", "org/./a", "org/../a"} {
		d := s.Decide("acme", request("acme", "u1", "r1", scope, "read"))
		if d.Allowed || d.Reason == "" || d.Reason == "matched allow policy" {
			t.Fatalf("scope %q: %+v, want denial with invalid-scope reason", scope, d)
		}
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	req.Subject.Disabled = true
	if d := s.Decide("acme", req); d.Allowed || d.Reason != "subject is disabled" {
		t.Fatalf("disabled subject: %+v", d)
	}
}

func TestScopeMatchingSegments(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		allowPolicy("exact", "u1", "read", "org/a", false),
		allowPolicy("subtree", "u2", "read", "org/a", true),
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}
	// Exact policy does not cover descendants.
	if d := s.Decide("acme", request("acme", "u1", "r1", "org/a/b", "read")); d.Allowed {
		t.Fatalf("exact policy covered descendant: %+v", d)
	}
	// Recursive policy covers descendants but not sibling prefixes.
	if d := s.Decide("acme", request("acme", "u2", "r1", "org/a/b/c", "read")); !d.Allowed {
		t.Fatalf("recursive policy missed descendant: %+v", d)
	}
	if d := s.Decide("acme", request("acme", "u2", "r1", "org/ab", "read")); d.Allowed {
		t.Fatalf("org/a must not cover org/ab: %+v", d)
	}
}

func TestDenyOverridesAllowAndMatchedSorted(t *testing.T) {
	s := NewStore()
	// Submission order must not affect the explanation order.
	policies := []Policy{
		{ID: "z-allow", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "a-deny", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
		{ID: "m-allow", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, Recursive: true},
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}
	d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if d.Allowed {
		t.Fatalf("deny must override allow: %+v", d)
	}
	want := []string{"a-deny", "m-allow", "z-allow"}
	if !reflect.DeepEqual(d.Matched, want) {
		t.Fatalf("matched = %v, want %v", d.Matched, want)
	}
	if d.Version != 1 {
		t.Fatalf("version = %d, want 1", d.Version)
	}
}

func TestRolesDoNotGrantAccess(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, nil); err != nil {
		t.Fatal(err)
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	req.Subject.Roles = []string{"owner", "org/a:read"}
	if d := s.Decide("acme", req); d.Allowed {
		t.Fatalf("subject roles must not grant access: %+v", d)
	}
}

func TestRollback(t *testing.T) {
	s := NewStore()
	v1, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish("acme", v1, nil); err != nil {
		t.Fatal(err)
	}
	// Current version 2 denies everything.
	if d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")); d.Allowed {
		t.Fatalf("version 2 should deny: %+v", d)
	}
	v3, err := s.Rollback("acme", 2, v1)
	if err != nil || v3 != 3 {
		t.Fatalf("rollback = %d, %v; want 3, nil", v3, err)
	}
	d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	if !d.Allowed || d.Version != 3 {
		t.Fatalf("after rollback: %+v, want allowed with version 3", d)
	}
	// History is intact: version 2 still holds the empty set.
	got, err := s.Policies("acme", 2)
	if err != nil || len(got) != 0 {
		t.Fatalf("version 2 policies = %v, %v; want empty", got, err)
	}
	// Rollback to a version that does not exist fails without changes.
	if _, err := s.Rollback("acme", 3, 99); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("rollback to missing version err = %v", err)
	}
	if got := s.CurrentVersion("acme"); got != 3 {
		t.Fatalf("current version = %d, want 3 after failed rollback", got)
	}
	// Stale expected version conflicts.
	if _, err := s.Rollback("acme", 1, v1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale rollback err = %v, want ErrVersionConflict", err)
	}
}

func TestSubmittedAndQueriedDataIsDetached(t *testing.T) {
	s := NewStore()
	submitted := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}
	if _, err := s.Publish("acme", 0, submitted); err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's slice after publish must not change the decision.
	submitted[0].Effect = EffectDeny
	submitted[0].Subject = "nobody"
	if d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")); !d.Allowed {
		t.Fatalf("published content changed by caller mutation: %+v", d)
	}
	// Mutating a query result must not change published content either.
	got, err := s.Policies("acme", 1)
	if err != nil {
		t.Fatal(err)
	}
	got[0].Effect = EffectDeny
	got[0].Subject = "nobody"
	if d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")); !d.Allowed {
		t.Fatalf("published content changed by query-result mutation: %+v", d)
	}
}

func TestReviewPinsHistoricalVersion(t *testing.T) {
	s := NewStore()
	v1, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	if err != nil {
		t.Fatal(err)
	}
	req := request("acme", "u1", "r1", "org/a", "read")
	before := s.Review("acme", v1, req)
	if !before.Allowed || before.Version != v1 {
		t.Fatalf("review before changes: %+v", before)
	}
	// Later publish and rollback must not change the review of v1.
	if _, err := s.Publish("acme", v1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rollback("acme", 2, 2); err != nil {
		t.Fatal(err)
	}
	after := s.Review("acme", v1, req)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("review changed after later publishes: before=%+v after=%+v", before, after)
	}
	// A version that does not exist denies with a reason, no silent fallback.
	d := s.Review("acme", 99, req)
	if d.Allowed || d.Version != 0 {
		t.Fatalf("review of missing version: %+v", d)
	}
	if d := s.Review("acme", 0, req); d.Allowed {
		t.Fatalf("review of version 0 must deny: %+v", d)
	}
}

func TestOrganizationsAreIndependent(t *testing.T) {
	s := NewStore()
	// Same subject, resource and policy identifiers in two organizations.
	if _, err := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish("globex", 0, nil); err != nil {
		t.Fatal(err)
	}
	if d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")); !d.Allowed {
		t.Fatalf("acme decision: %+v", d)
	}
	if d := s.Decide("globex", request("globex", "u1", "r1", "org/a", "read")); d.Allowed {
		t.Fatalf("globex decision leaked acme policy: %+v", d)
	}
	// Versions advance independently.
	v, err := s.Publish("globex", 1, nil)
	if err != nil || v != 2 {
		t.Fatalf("globex second publish = %d, %v; want 2, nil", v, err)
	}
	if got := s.CurrentVersion("acme"); got != 1 {
		t.Fatalf("acme current = %d, want 1", got)
	}
}
