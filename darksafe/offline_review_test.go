package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// syntheticRecord fingerprints one record exactly as the store would,
// letting tests build chains that can never occur through the live API
// (e.g. a decision naming a version published later).
func syntheticRecord(org string, seq int, kind string, change *PolicyChange, decision *DecisionRecord, prev string) AuditRecord {
	r := AuditRecord{
		Org:             org,
		Seq:             seq,
		Kind:            kind,
		Change:          change,
		Decision:        decision,
		PrevFingerprint: prev,
	}
	r.Fingerprint = fingerprintFor(&r)
	return r
}

// syntheticChain assumes a gapless, correctly fingerprinted chain.
func syntheticChain(org string, payloads []func(seq int) (string, *PolicyChange, *DecisionRecord)) ([]AuditRecord, Checkpoint) {
	recs := make([]AuditRecord, 0, len(payloads))
	prev := genesisFingerprint(org)
	for i, build := range payloads {
		seq := i + 1
		kind, change, decision := build(seq)
		r := syntheticRecord(org, seq, kind, change, decision, prev)
		recs = append(recs, r)
		prev = r.Fingerprint
	}
	return recs, Checkpoint{Org: org, EndSeq: len(recs), Fingerprint: prev}
}

func TestOfflineReviewReplaysRecordedVersion(t *testing.T) {
	s := NewStore()
	v1, _ := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	req := request("acme", "u1", "r1", "org/a", "read")
	original := s.Decide("acme", req) // seq 2, version 1, allowed
	origDeny := s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))

	// Later publishes and a rollback happen after the export material.
	s.Publish("acme", 1, nil)
	s.Rollback("acme", 2, 1)
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatalf("offline review: %v", err)
	}
	if !got.Consistent {
		t.Fatalf("review inconsistent: original=%+v recomputed=%+v", got.Original, got.Recomputed)
	}
	if !reflect.DeepEqual(got.Original, original) {
		t.Fatalf("original = %+v, want %+v", got.Original, original)
	}
	if !reflect.DeepEqual(got.Recomputed, original) || got.Recomputed.Version != v1 {
		t.Fatalf("recomputed = %+v, want %+v", got.Recomputed, original)
	}
	if got.Seq != 2 {
		t.Fatalf("seq = %d, want 2", got.Seq)
	}

	denied, err := RecheckDecisionOffline("acme", recs, cp, 3)
	if err != nil {
		t.Fatalf("offline review deny: %v", err)
	}
	if !denied.Consistent || !reflect.DeepEqual(denied.Original, origDeny) {
		t.Fatalf("deny review = %+v, want consistent with %+v", denied, origDeny)
	}
	if denied.Recomputed.Allowed || denied.Recomputed.Reason != "no matching allow policy" {
		t.Fatalf("recomputed deny = %+v", denied.Recomputed)
	}
}

func TestOfflineReviewUsesRollbackRecordContentAndNewVersion(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // v1 seq1
	s.Publish("acme", 1, nil)                                                       // v2 seq2
	v3, err := s.Rollback("acme", 2, 1)                                             // v3 seq3, carries v1 content
	if err != nil {
		t.Fatal(err)
	}
	original := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq4, version 3
	if !original.Allowed || original.Version != 3 {
		t.Fatalf("setup decision = %+v", original)
	}
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RecheckDecisionOffline("acme", recs, cp, 4)
	if err != nil {
		t.Fatalf("offline review: %v", err)
	}
	if !got.Consistent {
		t.Fatalf("rollback review inconsistent: %+v vs %+v", got.Original, got.Recomputed)
	}
	if got.Recomputed.Version != v3 || !got.Recomputed.Allowed {
		t.Fatalf("recomputed must use rollback's new version %d: %+v", v3, got.Recomputed)
	}
	if !reflect.DeepEqual(got.Recomputed.Matched, []string{"p1"}) {
		t.Fatalf("recomputed matched = %v, want [p1]", got.Recomputed.Matched)
	}
}

func TestOfflineReviewVersionZeroCases(t *testing.T) {
	s := NewStore()
	// A legal request before any publish: not-yet-published denial, version 0.
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq1
	// Envelope rejections are recorded too.
	missing := request("acme", "u1", "r1", "org/a", "read")
	missing.Action = ""
	s.Decide("acme", missing) // seq2
	mismatch := request("acme", "u1", "r1", "org/a", "read")
	mismatch.SubjectOrg = "globex"
	s.Decide("acme", mismatch) // seq3
	badScope := request("acme", "u1", "r1", "org/../a", "read")
	s.Decide("acme", badScope) // seq4
	disabled := request("acme", "u1", "r1", "org/a", "read")
	disabled.Subject.Disabled = true
	s.Decide("acme", disabled) // seq5

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons := []string{
		"organization has no published version",
		"missing action",
		"organization mismatch",
		"invalid scope: scope \"org/../a\" contains invalid segment \"..\"",
		"subject is disabled",
	}
	for seq, reason := range wantReasons {
		got, err := RecheckDecisionOffline("acme", recs, cp, seq+1)
		if err != nil {
			t.Fatalf("seq %d: %v", seq+1, err)
		}
		if !got.Consistent {
			t.Fatalf("seq %d inconsistent: %+v vs %+v", seq+1, got.Original, got.Recomputed)
		}
		if got.Original.Version != 0 || got.Recomputed.Version != 0 {
			t.Fatalf("seq %d versions = %d/%d, want 0", seq+1, got.Original.Version, got.Recomputed.Version)
		}
		if got.Original.Allowed || got.Recomputed.Allowed || got.Recomputed.Reason != reason {
			t.Fatalf("seq %d recomputed = %+v, want denial %q", seq+1, got.Recomputed, reason)
		}
	}
}

func TestOfflineReviewValidationErrors(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                  // seq2
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RecheckDecisionOffline("", recs, cp, 2); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("empty org err = %v", err)
	}
	for _, seq := range []int{0, -1} {
		if _, err := RecheckDecisionOffline("acme", recs, cp, seq); !errors.Is(err, ErrAuditNotFound) {
			t.Fatalf("non-positive seq %d err = %v", seq, err)
		}
	}
	if _, err := RecheckDecisionOffline("acme", recs, cp, 99); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("missing seq err = %v", err)
	}
	if _, err := RecheckDecisionOffline("acme", recs, cp, 1); !errors.Is(err, ErrAuditNotADecision) {
		t.Fatalf("policy change target err = %v", err)
	}

	// Empty export with the genesis checkpoint passes the chain check, but
	// no decision can be reviewed.
	empty, emptyCP, err := s.AuditExport("ghost", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("ghost", empty, emptyCP); err != nil {
		t.Fatalf("empty export must verify: %v", err)
	}
	if _, err := RecheckDecisionOffline("ghost", empty, emptyCP, 1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("empty export review err = %v", err)
	}
}

func TestOfflineReviewRejectsEveryTamperingIncludingAfterTarget(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                  // seq2 target
	s.Publish("acme", 1, nil)                                                       // seq3
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))                  // seq4

	good, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	mustFail := func(name string, mutate func([]AuditRecord) []AuditRecord, checkpoint Checkpoint) {
		t.Helper()
		recs := append([]AuditRecord(nil), good...)
		recs = mutate(recs)
		r, err := RecheckDecisionOffline("acme", recs, checkpoint, 2)
		if err == nil {
			t.Fatalf("%s: tampered material reviewed successfully: %+v", name, r)
		}
	}

	mustFail("rewrite target", func(r []AuditRecord) []AuditRecord {
		r[1].Decision.Decision.Allowed = !r[1].Decision.Decision.Allowed
		return r
	}, cp)
	mustFail("rewrite history policy", func(r []AuditRecord) []AuditRecord {
		r[0].Change.Policies[0].Effect = EffectDeny
		return r
	}, cp)
	mustFail("delete middle", func(r []AuditRecord) []AuditRecord {
		return []AuditRecord{r[0], r[2], r[3]}
	}, cp)
	mustFail("delete tail after target", func(r []AuditRecord) []AuditRecord {
		return r[:3]
	}, cp)
	mustFail("reorder", func(r []AuditRecord) []AuditRecord {
		r[2], r[3] = r[3], r[2]
		return r
	}, cp)
	mustFail("corruption after target", func(r []AuditRecord) []AuditRecord {
		// Record 4 sits after the reviewed seq 2; its damage must count.
		r[3].Decision.Request.Subject.ID = "hacker"
		return r
	}, cp)
	mustFail("checkpoint fingerprint mismatch", func(r []AuditRecord) []AuditRecord { return r },
		Checkpoint{Org: "acme", EndSeq: cp.EndSeq, Fingerprint: cp.Fingerprint + "x"})
	mustFail("checkpoint end mismatch", func(r []AuditRecord) []AuditRecord { return r },
		Checkpoint{Org: "acme", EndSeq: cp.EndSeq - 1, Fingerprint: cp.Fingerprint})

	// A record from another organization spliced after the target.
	foreign := syntheticRecord("acme", 4, AuditDecision, nil, &DecisionRecord{
		Request:  request("globex", "u2", "r1", "org/a", "read"),
		Decision: Decision{Allowed: false, Version: 1},
	}, good[2].Fingerprint)
	spliced := append([]AuditRecord(nil), good[:3]...)
	spliced = append(spliced, foreign)
	mustFail("foreign org record", func([]AuditRecord) []AuditRecord { return spliced },
		Checkpoint{Org: "acme", EndSeq: 4, Fingerprint: foreign.Fingerprint})
}

func TestOfflineReviewAcceptsPrefixButNotFilteredOrFragment(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                  // seq2
	s.Publish("acme", 1, nil)                                                       // seq3
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))                  // seq4

	// A legitimate prefix with its own matching checkpoint is usable.
	prefix, prefixCP, err := s.AuditExport("acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RecheckDecisionOffline("acme", prefix, prefixCP, 2)
	if err != nil {
		t.Fatalf("prefix review: %v", err)
	}
	if !got.Consistent || !got.Recomputed.Allowed {
		t.Fatalf("prefix review = %+v", got)
	}

	full, fullCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Widening the export later must not change the same target's result.
	later, err := RecheckDecisionOffline("acme", full, fullCP, 2)
	if err != nil || !later.Consistent || !reflect.DeepEqual(later.Recomputed, got.Recomputed) {
		t.Fatalf("wider export changed review: %+v vs %+v, %v", later, got, err)
	}

	// Subject-filtered records are not a complete export.
	page, err := s.AuditQuery("acme", 1, 10, AuditDecision, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecheckDecisionOffline("acme", page.Records, fullCP, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("subject-filtered export err = %v", err)
	}

	// A fragment missing the beginning cannot validate.
	if _, err := RecheckDecisionOffline("acme", full[1:], fullCP, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("fragment export err = %v", err)
	}

	// The prefix records do not validate against the later checkpoint.
	if _, err := RecheckDecisionOffline("acme", prefix, fullCP, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("prefix with foreign checkpoint err = %v", err)
	}
}

func TestOfflineReviewVersionMissingOrOnlyAfterTarget(t *testing.T) {
	req := request("acme", "u1", "r1", "org/a", "read")
	allowV2 := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}

	// A decision that names version 2, followed only later by the version-2
	// change record: a chain VerifyAudit accepts but review must refuse.
	laterRecs, laterCP := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 2},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 2, Policies: allowV2}, nil
		},
	})
	if err := VerifyAudit("acme", laterRecs, laterCP); err != nil {
		t.Fatalf("test chain must verify: %v", err)
	}
	if _, err := RecheckDecisionOffline("acme", laterRecs, laterCP, 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("version only after target err = %v", err)
	}

	// Version named by the decision appears nowhere in the export.
	missingRecs, missingCP := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 5},
			}
		},
	})
	if _, err := RecheckDecisionOffline("acme", missingRecs, missingCP, 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("missing version err = %v", err)
	}

	// A newer version present before the target must not be substituted for
	// the missing used version.
	wrongRecs, wrongCP := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 1, Policies: allowV2}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 2},
			}
		},
	})
	if _, err := RecheckDecisionOffline("acme", wrongRecs, wrongCP, 2); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("newer version substituted: err = %v", err)
	}
}

func TestOfflineReviewReportsInconsistencyWithBothDecisions(t *testing.T) {
	req := request("acme", "u1", "r1", "org/a", "read")
	// The chain is internally valid, but the stored decision contradicts
	// what the recorded version's policies produce: v1 allows u1, yet the
	// record claims a deny.
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  1,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: false, Reason: "matched deny policy", Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatalf("inconsistency is a result, not an error: %v", err)
	}
	if got.Consistent {
		t.Fatal("tampered conclusion must be flagged inconsistent")
	}
	if got.Original.Allowed || got.Original.Reason != "matched deny policy" {
		t.Fatalf("original not preserved: %+v", got.Original)
	}
	if !got.Recomputed.Allowed || got.Recomputed.Reason != "matched allow policy" ||
		got.Recomputed.Version != 1 || !reflect.DeepEqual(got.Recomputed.Matched, []string{"p1"}) {
		t.Fatalf("recomputed must be freshly derived: %+v", got.Recomputed)
	}
}

func TestOfflineReviewNilAndEmptyMatchedAreConsistent(t *testing.T) {
	req := request("acme", "u9", "r1", "org/a", "read") // no policy matches
	build := func(matched []string) ([]AuditRecord, Checkpoint) {
		return syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
			func(seq int) (string, *PolicyChange, *DecisionRecord) {
				return AuditPolicyChange, &PolicyChange{Version: 1, Policies: nil}, nil
			},
			func(seq int) (string, *PolicyChange, *DecisionRecord) {
				return AuditDecision, nil, &DecisionRecord{
					Request:  req,
					Decision: Decision{Allowed: false, Reason: "no matching allow policy", Matched: matched, Version: 1},
				}
			},
		})
	}
	for _, matched := range [][]string{nil, {}} {
		recs, cp := build(matched)
		got, err := RecheckDecisionOffline("acme", recs, cp, 2)
		if err != nil {
			t.Fatalf("matched=%v: %v", matched, err)
		}
		if !got.Consistent {
			t.Fatalf("nil and empty matched lists must agree: original=%+v recomputed=%+v",
				got.Original, got.Recomputed)
		}
	}
}

func TestOfflineReviewIsReadOnlyAndDetached(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := append([]AuditRecord(nil), recs...)

	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recs, snapshot) {
		t.Fatal("offline review modified the supplied records")
	}
	// Mutating the caller's material after the call must not reach the
	// returned decisions.
	recs[0].Change.Policies[0].Subject = "hacker"
	recs[1].Decision.Decision.Matched[0] = "forged"
	recs[1].Decision.Request.Subject.ID = "hacker"
	if !reflect.DeepEqual(got.Original.Matched, []string{"p1"}) {
		t.Fatalf("original decision aliases input: %+v", got.Original)
	}
	if !reflect.DeepEqual(got.Recomputed.Matched, []string{"p1"}) {
		t.Fatalf("recomputed decision aliases input: %+v", got.Recomputed)
	}
}

func TestOfflineReviewDenyOverridesAllow(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		{ID: "z-allow", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "a-deny", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
		{ID: "m-allow", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, Recursive: true},
	})
	original := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp, _ := s.AuditExport("acme", 0)
	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Consistent || !reflect.DeepEqual(got.Recomputed, original) {
		t.Fatalf("deny-overrides review = %+v, original %+v", got, original)
	}
	if got.Recomputed.Allowed || !reflect.DeepEqual(got.Recomputed.Matched, []string{"a-deny", "m-allow", "z-allow"}) {
		t.Fatalf("recomputed = %+v", got.Recomputed)
	}
}

func TestOfflineReviewAcrossOrganizationsIndependent(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("globex", 0, nil)
	s.Decide("globex", request("globex", "u1", "r1", "org/a", "read"))

	a, acp, _ := s.AuditExport("acme", 0)
	g, gcp, _ := s.AuditExport("globex", 0)
	gotA, err := RecheckDecisionOffline("acme", a, acp, 2)
	if err != nil || !gotA.Consistent || !gotA.Recomputed.Allowed {
		t.Fatalf("acme review = %+v, %v", gotA, err)
	}
	gotG, err := RecheckDecisionOffline("globex", g, gcp, 2)
	if err != nil || !gotG.Consistent || gotG.Recomputed.Allowed {
		t.Fatalf("globex review = %+v, %v", gotG, err)
	}
	// One organization's material cannot review another organization.
	if _, err := RecheckDecisionOffline("acme", g, gcp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("cross-org review err = %v", err)
	}
	if _, err := RecheckDecisionOffline("globex", a, acp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("cross-org review err = %v", err)
	}
}

// TestOfflineReviewNonzeroVersionStillChecksRequest covers the core fix:
// a complete, valid chain only proves the material matches its checkpoint;
// with a non-zero version, the re-evaluation must still apply the online
// request envelope rules. Every request below would be allowed by the
// version-1 policy set, yet must be rejected exactly as Decide would.
func TestOfflineReviewNonzeroVersionStillChecksRequest(t *testing.T) {
	allow := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}
	cases := []struct {
		name   string
		req    OrgRequest
		reason string
	}{
		{"disabled subject", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.Subject.Disabled = true
			return r
		}(), "subject is disabled"},
		{"subject organization mismatch", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.SubjectOrg = "globex"
			return r
		}(), "organization mismatch"},
		{"resource organization mismatch", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.ResourceOrg = "globex"
			return r
		}(), "organization mismatch"},
		{"missing subject organization", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.SubjectOrg = ""
			return r
		}(), "missing subject organization"},
		{"missing resource organization", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.ResourceOrg = ""
			return r
		}(), "missing resource organization"},
		{"missing subject id", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.Subject.ID = ""
			return r
		}(), "missing subject id"},
		{"missing resource id", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.Resource.ID = ""
			return r
		}(), "missing resource id"},
		{"missing action", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a", "read")
			r.Action = ""
			return r
		}(), "missing action"},
		{"empty scope", func() OrgRequest {
			r := request("acme", "u1", "r1", "", "read")
			return r
		}(), "invalid scope: scope is empty"},
		{"leading slash", func() OrgRequest {
			r := request("acme", "u1", "r1", "/org/a", "read")
			return r
		}(), "invalid scope: scope \"/org/a\" has a leading or trailing slash"},
		{"trailing slash", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/a/", "read")
			return r
		}(), "invalid scope: scope \"org/a/\" has a leading or trailing slash"},
		{"consecutive slashes", func() OrgRequest {
			r := request("acme", "u1", "r1", "org//a", "read")
			return r
		}(), "invalid scope: scope \"org//a\" contains consecutive slashes"},
		{"dot segment", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/./a", "read")
			return r
		}(), "invalid scope: scope \"org/./a\" contains invalid segment \".\""},
		{"dot-dot segment", func() OrgRequest {
			r := request("acme", "u1", "r1", "org/../a", "read")
			return r
		}(), "invalid scope: scope \"org/../a\" contains invalid segment \"..\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
				func(seq int) (string, *PolicyChange, *DecisionRecord) {
					return AuditPolicyChange, &PolicyChange{Version: 1, Policies: allow}, nil
				},
				func(seq int) (string, *PolicyChange, *DecisionRecord) {
					return AuditDecision, nil, &DecisionRecord{
						Request: tc.req,
						// The record claims a full allow against version 1.
						Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
					}
				},
			})
			got, err := RecheckDecisionOffline("acme", recs, cp, 2)
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if got.Consistent {
				t.Fatalf("record claims allow but the request is invalid: must be flagged inconsistent, got %+v", got)
			}
			// The original conclusion is preserved untouched.
			if !got.Original.Allowed || got.Original.Version != 1 {
				t.Fatalf("original must be preserved: %+v", got.Original)
			}
			r := got.Recomputed
			if r.Allowed {
				t.Fatalf("recomputed must deny an invalid request: %+v", r)
			}
			if r.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", r.Reason, tc.reason)
			}
			if r.Version != 0 {
				t.Fatalf("version = %d, want 0 (no policy was used)", r.Version)
			}
			if len(r.Matched) != 0 {
				t.Fatalf("matched = %v, want empty", r.Matched)
			}
		})
	}
}

// TestOfflineReviewRequestRejectionPriorityMatchesOnline verifies that when
// several envelope faults coincide, the reason and its precedence match the
// online checkRequest order, and no policy is evaluated.
func TestOfflineReviewRequestRejectionPriorityMatchesOnline(t *testing.T) {
	allow := []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}
	build := func(req OrgRequest) ([]AuditRecord, Checkpoint) {
		return syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
			func(seq int) (string, *PolicyChange, *DecisionRecord) {
				return AuditPolicyChange, &PolicyChange{Version: 1, Policies: allow}, nil
			},
			func(seq int) (string, *PolicyChange, *DecisionRecord) {
				return AuditDecision, nil, &DecisionRecord{
					Request:  req,
					Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
				}
			},
		})
	}

	// Every envelope field missing: the first online check wins.
	allMissing := OrgRequest{}
	recs, cp := build(allMissing)
	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed.Reason != "missing subject organization" || got.Recomputed.Version != 0 {
		t.Fatalf("all-missing recomputed = %+v", got.Recomputed)
	}

	// Organization mismatch precedes invalid scope and the disabled flag.
	req := request("acme", "u1", "r1", "org//a", "read")
	req.SubjectOrg = "globex"
	req.Subject.Disabled = true
	recs, cp = build(req)
	got, err = RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed.Reason != "organization mismatch" || got.Recomputed.Version != 0 {
		t.Fatalf("mismatch-before-scope recomputed = %+v", got.Recomputed)
	}

	// Invalid scope precedes the disabled flag.
	req = request("acme", "u1", "r1", "org//a", "read")
	req.Subject.Disabled = true
	recs, cp = build(req)
	got, err = RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed.Reason != "invalid scope: scope \"org//a\" contains consecutive slashes" ||
		got.Recomputed.Version != 0 {
		t.Fatalf("scope-before-disabled recomputed = %+v", got.Recomputed)
	}

	// A valid envelope with a disabled subject is rejected on the flag.
	req = request("acme", "u1", "r1", "org/a", "read")
	req.Subject.Disabled = true
	recs, cp = build(req)
	got, err = RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed.Reason != "subject is disabled" || got.Recomputed.Version != 0 ||
		len(got.Recomputed.Matched) != 0 {
		t.Fatalf("disabled recomputed = %+v", got.Recomputed)
	}
}

// TestOfflineReviewMissingVersionNotMaskedByRequestRejection ensures a
// version lookup failure still surfaces as ErrVersionNotFound even when the
// request would also be rejected at the envelope.
func TestOfflineReviewMissingVersionNotMaskedByRequestRejection(t *testing.T) {
	// No change record at all; the request is additionally invalid.
	req := request("acme", "u1", "r1", "org/a", "read")
	req.Subject.Disabled = true
	missing, missingCP := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 9},
			}
		},
	})
	if _, err := RecheckDecisionOffline("acme", missing, missingCP, 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("missing version must surface despite invalid request, got %v", err)
	}

	// The version's change record appears only after the target; the
	// cross-organization request must not mask that either.
	req = request("acme", "u1", "r1", "org/a", "read")
	req.SubjectOrg = "globex"
	later, laterCP := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 2},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  2,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
	})
	if _, err := RecheckDecisionOffline("acme", later, laterCP, 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("version only after target must surface despite invalid request, got %v", err)
	}
}

// TestOfflineReviewDenyingOriginalWithWrongVersionIsInconsistent covers the
// other inconsistency direction: the record denies, but with a version or
// reason the recomputation does not share.
func TestOfflineReviewDenyingOriginalWithWrongVersionIsInconsistent(t *testing.T) {
	// The version-1 set allows u1 read on org/a, but the recorded request
	// carries a disabled subject: a real Decide would have rejected it at
	// the envelope with version 0. The record instead claims a policy
	// deny against version 1.
	req := request("acme", "u1", "r1", "org/a", "read")
	req.Subject.Disabled = true
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  1,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request: req,
				Decision: Decision{
					Allowed: false,
					Reason:  "matched deny policy",
					Matched: []string{"p1"},
					Version: 1,
				},
			}
		},
	})
	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if got.Consistent {
		t.Fatalf("deny with the wrong version/reason must be flagged inconsistent: %+v", got)
	}
	if got.Original.Allowed || got.Original.Version != 1 ||
		got.Original.Reason != "matched deny policy" ||
		!reflect.DeepEqual(got.Original.Matched, []string{"p1"}) {
		t.Fatalf("original deny must be preserved: %+v", got.Original)
	}
	if got.Recomputed.Allowed || got.Recomputed.Version != 0 ||
		got.Recomputed.Reason != "subject is disabled" || len(got.Recomputed.Matched) != 0 {
		t.Fatalf("recomputed must be the envelope rejection with version 0: %+v", got.Recomputed)
	}
}

// TestOfflineReviewValidEnvelopeStillUsesRecordedHistoryVersion confirms the
// fix did not change the legal-request path: the recorded historical
// version alone decides, with resource restrictions and deny-override.
func TestOfflineReviewValidEnvelopeStillUsesRecordedHistoryVersion(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, ResourceID: "r1"},
	})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq2, v1
	s.Publish("acme", 1, nil)                                      // v2
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq4, v2 denies

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The seq-2 decision must still be allowed against v1's resource
	// restriction even though v2 is now current and denies.
	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Consistent || !got.Recomputed.Allowed || got.Recomputed.Version != 1 {
		t.Fatalf("v1 decision review = %+v", got)
	}
	// The later decision against the empty v2 set denies with version 2.
	got2, err := RecheckDecisionOffline("acme", recs, cp, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !got2.Consistent || got2.Recomputed.Allowed || got2.Recomputed.Version != 2 ||
		got2.Recomputed.Reason != "no matching allow policy" {
		t.Fatalf("v2 decision review = %+v", got2)
	}
}
