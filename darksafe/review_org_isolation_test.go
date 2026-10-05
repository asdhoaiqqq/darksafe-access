package darksafe

import (
	"reflect"
	"testing"
)

// publishReviewPair publishes version 1 in both organizations with the same
// subject, action, scope and policy id, but opposite effects: acme allows,
// globex denies. Every identifier coincides so only the selected decision
// organization can explain a differing review outcome.
func publishReviewPair(t *testing.T, s *Store) {
	t.Helper()
	shared := Policy{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow}
	if v, err := s.Publish("acme", 0, []Policy{shared}); err != nil || v != 1 {
		t.Fatalf("acme publish = %d, %v; want 1, nil", v, err)
	}
	shared.Effect = EffectDeny
	if v, err := s.Publish("globex", 0, []Policy{shared}); err != nil || v != 1 {
		t.Fatalf("globex publish = %d, %v; want 1, nil", v, err)
	}
}

func TestReviewOrgIsolationOppositeOutcomes(t *testing.T) {
	s := NewStore()
	publishReviewPair(t, s)

	// Same subject, resource, action, scope and policy id in both
	// organizations; only the decision organization differs.
	allow := s.Review("acme", 1, request("acme", "u1", "r1", "org/a", "read"))
	if !allow.Allowed || allow.Reason != "matched allow policy" {
		t.Fatalf("acme review = %+v, want allowed with matched allow policy", allow)
	}
	if !reflect.DeepEqual(allow.Matched, []string{"p1"}) {
		t.Fatalf("acme matched = %v, want [p1] only", allow.Matched)
	}
	if allow.Version != 1 {
		t.Fatalf("acme version = %d, want 1", allow.Version)
	}

	deny := s.Review("globex", 1, request("globex", "u1", "r1", "org/a", "read"))
	if deny.Allowed || deny.Reason != "matched deny policy" {
		t.Fatalf("globex review = %+v, want denied with matched deny policy", deny)
	}
	if !reflect.DeepEqual(deny.Matched, []string{"p1"}) {
		t.Fatalf("globex matched = %v, want [p1] only", deny.Matched)
	}
	if deny.Version != 1 {
		t.Fatalf("globex version = %d, want 1", deny.Version)
	}
}

func TestReviewOrgIsolationVersionExistence(t *testing.T) {
	s := NewStore()
	publishReviewPair(t, s)
	before := s.Review("acme", 1, request("acme", "u1", "r1", "org/a", "read"))
	if !before.Allowed {
		t.Fatalf("acme v1 review before v2 publish = %+v, want allowed", before)
	}

	// Only acme publishes a version 2, flipping its current decision to deny.
	// globex never has a version 2.
	denyV2 := Policy{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny}
	if v, err := s.Publish("acme", 1, []Policy{denyV2}); err != nil || v != 2 {
		t.Fatalf("acme second publish = %d, %v; want 2, nil", v, err)
	}
	if got := s.CurrentVersion("globex"); got != 1 {
		t.Fatalf("globex current = %d, want 1", got)
	}

	// Reviewing acme's version 1 still replays the historical allow, not the
	// current deny.
	after := s.Review("acme", 1, request("acme", "u1", "r1", "org/a", "read"))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("acme v1 review changed after v2 publish: before=%+v after=%+v", before, after)
	}

	// globex has no version 2 even though acme does and every identifier
	// coincides: no fallback to globex's current set or acme's version 2.
	d := s.Review("globex", 2, request("globex", "u1", "r1", "org/a", "read"))
	if d.Allowed || d.Reason != "version 2 not found" {
		t.Fatalf("globex v2 review = %+v, want denied with version 2 not found", d)
	}
	if d.Version != 0 {
		t.Fatalf("globex v2 review version = %d, want 0", d.Version)
	}
	if len(d.Matched) != 0 {
		t.Fatalf("globex v2 review matched = %v, want empty", d.Matched)
	}
}

func TestReviewOrgIsolationOrgMismatch(t *testing.T) {
	s := NewStore()
	publishReviewPair(t, s)

	// acme's version 1 holds a fully matching allow policy, and globex owns
	// same-named subjects and resources; neither may bypass the mismatch
	// rejection.
	base := request("acme", "u1", "r1", "org/a", "read")

	subjectOrg := base
	subjectOrg.SubjectOrg = "globex"
	d := s.Review("acme", 1, subjectOrg)
	if d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("subject org mismatch: %+v", d)
	}
	if d.Version != 0 || len(d.Matched) != 0 {
		t.Fatalf("subject org mismatch: version=%d matched=%v, want 0 and none", d.Version, d.Matched)
	}

	resourceOrg := base
	resourceOrg.ResourceOrg = "globex"
	d = s.Review("acme", 1, resourceOrg)
	if d.Allowed || d.Reason != "organization mismatch" {
		t.Fatalf("resource org mismatch: %+v", d)
	}
	if d.Version != 0 || len(d.Matched) != 0 {
		t.Fatalf("resource org mismatch: version=%d matched=%v, want 0 and none", d.Version, d.Matched)
	}
}

func TestReviewOrgIsolationReadOnly(t *testing.T) {
	s := NewStore()
	publishReviewPair(t, s)
	// acme advances to a denying version 2; globex stays at version 1.
	denyV2 := Policy{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny}
	if _, err := s.Publish("acme", 1, []Policy{denyV2}); err != nil {
		t.Fatal(err)
	}
	// One online decision per organization, so recheck has records to replay.
	acmeDecide := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	globexDecide := s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))

	snapshot := func() (int, int, []Policy, []Policy, []Policy, []AuditRecord, Checkpoint, []AuditRecord, Checkpoint) {
		acmeV1, err := s.Policies("acme", 1)
		if err != nil {
			t.Fatal(err)
		}
		acmeV2, err := s.Policies("acme", 2)
		if err != nil {
			t.Fatal(err)
		}
		globexV1, err := s.Policies("globex", 1)
		if err != nil {
			t.Fatal(err)
		}
		acmeAudit, acmeCp, err := s.AuditExport("acme", 0)
		if err != nil {
			t.Fatal(err)
		}
		globexAudit, globexCp, err := s.AuditExport("globex", 0)
		if err != nil {
			t.Fatal(err)
		}
		return s.CurrentVersion("acme"), s.CurrentVersion("globex"),
			acmeV1, acmeV2, globexV1, acmeAudit, acmeCp, globexAudit, globexCp
	}
	acmeCurB, globexCurB, acmeV1B, acmeV2B, globexV1B, acmeAuditB, acmeCpB, globexAuditB, globexCpB := snapshot()

	// Exercise every review outcome: allow, deny, missing version, mismatch.
	s.Review("acme", 1, request("acme", "u1", "r1", "org/a", "read"))
	s.Review("globex", 1, request("globex", "u1", "r1", "org/a", "read"))
	s.Review("acme", 2, request("acme", "u1", "r1", "org/a", "read"))
	s.Review("globex", 2, request("globex", "u1", "r1", "org/a", "read"))
	s.Review("acme", 99, request("acme", "u1", "r1", "org/a", "read"))
	mismatch := request("acme", "u1", "r1", "org/a", "read")
	mismatch.SubjectOrg = "globex"
	s.Review("acme", 1, mismatch)

	acmeCurA, globexCurA, acmeV1A, acmeV2A, globexV1A, acmeAuditA, acmeCpA, globexAuditA, globexCpA := snapshot()
	if acmeCurA != acmeCurB || globexCurA != globexCurB {
		t.Fatalf("current versions changed by reviews: acme %d->%d globex %d->%d",
			acmeCurB, acmeCurA, globexCurB, globexCurA)
	}
	for name, pair := range map[string][2][]Policy{
		"acme v1":   {acmeV1B, acmeV1A},
		"acme v2":   {acmeV2B, acmeV2A},
		"globex v1": {globexV1B, globexV1A},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("%s content changed by reviews", name)
		}
	}
	if !reflect.DeepEqual(acmeAuditB, acmeAuditA) || acmeCpB != acmeCpA {
		t.Fatalf("acme audit chain changed by reviews")
	}
	if !reflect.DeepEqual(globexAuditB, globexAuditA) || globexCpB != globexCpA {
		t.Fatalf("globex audit chain changed by reviews")
	}

	// Existing entry points keep their behavior after the reviews: online
	// decisions use the current version, audit recheck replays the recorded
	// one, and publish/rollback still advance history.
	if d := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")); d.Allowed || d.Version != 2 {
		t.Fatalf("acme decide after reviews = %+v, want denied with version 2", d)
	}
	if d := s.Decide("globex", request("globex", "u1", "r1", "org/a", "read")); d.Allowed || d.Version != 1 {
		t.Fatalf("globex decide after reviews = %+v, want denied with version 1", d)
	}
	recheck, err := s.RecheckDecision("acme", 3)
	if err != nil || !reflect.DeepEqual(recheck, acmeDecide) {
		t.Fatalf("acme recheck = %+v, %v; want %+v", recheck, err, acmeDecide)
	}
	recheck, err = s.RecheckDecision("globex", 2)
	if err != nil || !reflect.DeepEqual(recheck, globexDecide) {
		t.Fatalf("globex recheck = %+v, %v; want %+v", recheck, err, globexDecide)
	}
	if v, err := s.Publish("globex", 1, nil); err != nil || v != 2 {
		t.Fatalf("globex publish after reviews = %d, %v; want 2, nil", v, err)
	}
	if v, err := s.Rollback("acme", 2, 1); err != nil || v != 3 {
		t.Fatalf("acme rollback after reviews = %d, %v; want 3, nil", v, err)
	}
}
