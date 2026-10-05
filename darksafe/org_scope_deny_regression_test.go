package darksafe

import (
	"reflect"
	"testing"
)

// These tests pin the interaction between a parent-scope deny and a more
// specific child-scope allow inside one real Decide request: a deny that
// covers the request scope always wins over any allow, "more specific" never
// means "preferred allow", and a non-recursive deny covers only its own
// scope. Scope coverage follows the slash-segment rules only.

// TestRecursiveParentDenyOverridesChildAllow publishes, for one subject and
// one read action, a recursive deny on org/a and an exact allow on org/a/b.
// A read inside org/a/b must be denied, and the explanation must name both
// policies in ascending id order — not only the deny that decided the
// outcome. Publishing the two policies in either order must not change the
// outcome, the reason, the matched list, or the version actually used.
func TestRecursiveParentDenyOverridesChildAllow(t *testing.T) {
	deny := Policy{ID: "p-deny-parent", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny, Recursive: true}
	allow := allowPolicy("p-allow-child", "u1", "read", "org/a/b", false)
	wantMatched := []string{"p-allow-child", "p-deny-parent"}

	orders := map[string][]Policy{
		"deny first":  {deny, allow},
		"allow first": {allow, deny},
	}
	for name, policies := range orders {
		t.Run(name, func(t *testing.T) {
			s := NewStore()
			v, err := s.Publish("acme", 0, policies)
			if err != nil {
				t.Fatal(err)
			}
			req := request("acme", "u1", "r1", "org/a/b", "read")
			d := s.Decide("acme", req)
			if d.Allowed {
				t.Fatalf("recursive parent deny must override child allow: %+v", d)
			}
			if d.Reason != "matched deny policy" {
				t.Fatalf("reason = %q, want %q", d.Reason, "matched deny policy")
			}
			if !reflect.DeepEqual(d.Matched, wantMatched) {
				t.Fatalf("matched = %v, want both policies %v", d.Matched, wantMatched)
			}
			if d.Version != v {
				t.Fatalf("version = %d, want the published version %d", d.Version, v)
			}

			// The decision record must preserve the full request and the
			// decision actually returned, including the allow hit, so the
			// denial stays reviewable.
			recs := drainAudit(t, s, "acme", AuditDecision, "")
			if len(recs) != 1 {
				t.Fatalf("decision records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.Decision == nil {
				t.Fatalf("record %d has no decision payload: %+v", rec.Seq, rec)
			}
			if !reflect.DeepEqual(rec.Decision.Request, req) {
				t.Fatalf("recorded request = %+v, want %+v", rec.Decision.Request, req)
			}
			if !reflect.DeepEqual(rec.Decision.Decision, d) {
				t.Fatalf("recorded decision = %+v, want the returned %+v", rec.Decision.Decision, d)
			}
			if !reflect.DeepEqual(rec.Decision.Decision.Matched, wantMatched) {
				t.Fatalf("recorded matched = %v, want both policies %v",
					rec.Decision.Decision.Matched, wantMatched)
			}
		})
	}
}

// TestNonRecursiveParentDenyDoesNotCoverChild publishes a non-recursive deny
// on org/a alongside an exact allow on org/a/b. The deny covers only its own
// scope, so the child read must be allowed with only the allow policy named.
func TestNonRecursiveParentDenyDoesNotCoverChild(t *testing.T) {
	s := NewStore()
	v, err := s.Publish("acme", 0, []Policy{
		{ID: "p-deny-parent", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
		allowPolicy("p-allow-child", "u1", "read", "org/a/b", false),
	})
	if err != nil {
		t.Fatal(err)
	}
	d := s.Decide("acme", request("acme", "u1", "r1", "org/a/b", "read"))
	if !d.Allowed {
		t.Fatalf("non-recursive parent deny must not cover the child scope: %+v", d)
	}
	if d.Reason != "matched allow policy" {
		t.Fatalf("reason = %q, want %q", d.Reason, "matched allow policy")
	}
	wantMatched := []string{"p-allow-child"}
	if !reflect.DeepEqual(d.Matched, wantMatched) {
		t.Fatalf("matched = %v, want only the child allow %v", d.Matched, wantMatched)
	}
	if d.Version != v {
		t.Fatalf("version = %d, want the published version %d", d.Version, v)
	}

	recs := drainAudit(t, s, "acme", AuditDecision, "")
	if len(recs) != 1 || recs[0].Decision == nil {
		t.Fatalf("decision records = %+v, want exactly one decision record", recs)
	}
	if got := recs[0].Decision.Decision; !reflect.DeepEqual(got, d) {
		t.Fatalf("recorded decision = %+v, want the returned %+v", got, d)
	}
}

// TestRecursiveParentDenyDoesNotCoverSiblingPrefix pins the segment boundary:
// org/ab is not a descendant of org/a, so a recursive deny on org/a must not
// reach a read in org/ab, and its id must not appear in the matched list.
func TestRecursiveParentDenyDoesNotCoverSiblingPrefix(t *testing.T) {
	s := NewStore()
	v, err := s.Publish("acme", 0, []Policy{
		{ID: "p-deny-parent", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny, Recursive: true},
		allowPolicy("p-allow-sibling", "u1", "read", "org/ab", false),
	})
	if err != nil {
		t.Fatal(err)
	}
	d := s.Decide("acme", request("acme", "u1", "r1", "org/ab", "read"))
	if !d.Allowed {
		t.Fatalf("org/a recursive deny must not cover org/ab: %+v", d)
	}
	if d.Reason != "matched allow policy" {
		t.Fatalf("reason = %q, want %q", d.Reason, "matched allow policy")
	}
	wantMatched := []string{"p-allow-sibling"}
	if !reflect.DeepEqual(d.Matched, wantMatched) {
		t.Fatalf("matched = %v, want only the sibling allow %v", d.Matched, wantMatched)
	}
	if d.Version != v {
		t.Fatalf("version = %d, want the published version %d", d.Version, v)
	}

	recs := drainAudit(t, s, "acme", AuditDecision, "")
	if len(recs) != 1 || recs[0].Decision == nil {
		t.Fatalf("decision records = %+v, want exactly one decision record", recs)
	}
	if got := recs[0].Decision.Decision; !reflect.DeepEqual(got, d) {
		t.Fatalf("recorded decision = %+v, want the returned %+v", got, d)
	}
}
