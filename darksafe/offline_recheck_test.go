package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// exportChain exports the full chain of org and verifies it.
func exportChain(t *testing.T, s *Store, org string) ([]AuditRecord, Checkpoint) {
	t.Helper()
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(org, recs, cp); err != nil {
		t.Fatalf("export must verify: %v", err)
	}
	return recs, cp
}

// rechain recomputes fingerprints from the first changed record on and
// returns the updated records with a matching checkpoint.
func rechain(records []AuditRecord, changedFrom int) ([]AuditRecord, Checkpoint) {
	out := append([]AuditRecord(nil), records...)
	prev := genesisFingerprint(out[0].Org)
	for i := range out {
		if i >= changedFrom {
			out[i].PrevFingerprint = prev
			out[i].Fingerprint = fingerprintFor(&out[i])
		}
		prev = out[i].Fingerprint
	}
	cp := Checkpoint{Org: out[0].Org, EndSeq: len(out), Fingerprint: out[len(out)-1].Fingerprint}
	return out, cp
}

func TestRecheckDecisionOfflineReplaysRecordedVersion(t *testing.T) {
	s := NewStore()
	v1, _ := s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	req1 := request("acme", "u1", "r1", "org/a", "read")
	d1 := s.Decide("acme", req1) // seq 2, allowed at v1
	req2 := request("acme", "u2", "r1", "org/a", "read")
	d2 := s.Decide("acme", req2) // seq 3, denied at v1
	s.Publish("acme", v1, nil)   // seq 4, v2 empty
	v3, _ := s.Rollback("acme", 2, v1)
	req3 := request("acme", "u1", "r1", "org/a", "read")
	d3 := s.Decide("acme", req3) // seq 6, allowed at v3 (rollback content)

	recs, cp := exportChain(t, s, "acme")

	recheck := func(seq int, want Decision) *OfflineRecheckResult {
		t.Helper()
		res, err := RecheckDecisionOffline("acme", recs, cp, seq)
		if err != nil {
			t.Fatalf("recheck seq %d: %v", seq, err)
		}
		if !res.Match {
			t.Fatalf("seq %d: mismatch reported: %+v", seq, res)
		}
		if !reflect.DeepEqual(res.Original, want) || !reflect.DeepEqual(res.Recomputed, want) {
			t.Fatalf("seq %d: result = %+v, want %+v", seq, res, want)
		}
		return res
	}
	recheck(2, d1)
	recheck(3, d2)
	recheck(6, d3)

	// The rollback decision notes the new version number, not v1.
	res := recheck(6, d3)
	if res.Recomputed.Version != v3 {
		t.Fatalf("rollback version = %d, want %d", res.Recomputed.Version, v3)
	}

	// A legal prefix export with its matching checkpoint rechecks the
	// target identically; a target beyond the prefix is not found.
	prefix, pcp, err := s.AuditExport("acme", 3)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := RecheckDecisionOffline("acme", prefix, pcp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pre, recheck(2, d1)) {
		t.Fatalf("prefix recheck = %+v, want full-export result", pre)
	}
	if _, err := RecheckDecisionOffline("acme", prefix, pcp, 6); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("target beyond prefix err = %v, want ErrAuditNotFound", err)
	}

	// Expanding export scope must not change the result.
	if !reflect.DeepEqual(pre.Original, d1) || !reflect.DeepEqual(pre.Recomputed, d1) || !pre.Match {
		t.Fatalf("prefix result changed: %+v", pre)
	}
}

func TestRecheckDecisionOfflineVersionZeroRestoresRejections(t *testing.T) {
	s := NewStore()
	d0 := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 1, no version
	mismatch := request("acme", "u1", "r1", "org/a", "read")
	mismatch.SubjectOrg = "globex"
	dm := s.Decide("acme", mismatch) // seq 2
	disabled := request("acme", "u1", "r1", "org/a", "read")
	disabled.Subject.Disabled = true
	dd := s.Decide("acme", disabled) // seq 3
	missing := request("acme", "u1", "r1", "org/a", "read")
	missing.Action = ""
	dmiss := s.Decide("acme", missing) // seq 4
	badscope := request("acme", "u1", "r1", "org/a", "read")
	badscope.Resource.Scope = "org/../a"
	dscope := s.Decide("acme", badscope) // seq 5

	recs, cp := exportChain(t, s, "acme")
	for _, tc := range []struct {
		seq  int
		want Decision
	}{
		{1, d0}, {2, dm}, {3, dd}, {4, dmiss}, {5, dscope},
	} {
		res, err := RecheckDecisionOffline("acme", recs, cp, tc.seq)
		if err != nil {
			t.Fatalf("seq %d: %v", tc.seq, err)
		}
		if !res.Match || !reflect.DeepEqual(res.Original, tc.want) || !reflect.DeepEqual(res.Recomputed, tc.want) {
			t.Fatalf("seq %d: result = %+v, want %+v", tc.seq, res, tc.want)
		}
		if res.Original.Version != 0 || res.Recomputed.Version != 0 {
			t.Fatalf("seq %d: version must stay 0, got %+v", tc.seq, res)
		}
	}
}

func TestRecheckDecisionOfflineErrorsAreDistinguishable(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp := exportChain(t, s, "acme")

	if _, err := RecheckDecisionOffline("", recs, cp, 2); !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("empty org err = %v, want ErrMissingOrganization", err)
	}
	if _, err := RecheckDecisionOffline("acme", recs, cp, 0); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("zero seq err = %v, want ErrAuditNotFound", err)
	}
	if _, err := RecheckDecisionOffline("acme", recs, cp, -1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("negative seq err = %v, want ErrAuditNotFound", err)
	}
	if _, err := RecheckDecisionOffline("acme", recs, cp, 99); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("missing seq err = %v, want ErrAuditNotFound", err)
	}
	if _, err := RecheckDecisionOffline("acme", recs, cp, 1); !errors.Is(err, ErrAuditNotADecision) {
		t.Fatalf("policy-change target err = %v, want ErrAuditNotADecision", err)
	}

	// Empty records with the genesis checkpoint verify but hold no decision.
	empty := []AuditRecord{}
	gcp := Checkpoint{Org: "acme", Fingerprint: genesisFingerprint("acme")}
	if _, err := RecheckDecisionOffline("acme", empty, gcp, 1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("empty export err = %v, want ErrAuditNotFound", err)
	}
	if _, err := RecheckDecisionOffline("acme", nil, gcp, 1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("nil export err = %v, want ErrAuditNotFound", err)
	}
}

func TestRecheckDecisionOfflineRejectsTampering(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, nil)
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))
	good, cp := exportChain(t, s, "acme")

	check := func(name string, mutate func([]AuditRecord) []AuditRecord, checkpoint Checkpoint) {
		t.Helper()
		recs := append([]AuditRecord(nil), good...)
		recs = mutate(recs)
		if res, err := RecheckDecisionOffline("acme", recs, checkpoint, 2); err == nil {
			t.Fatalf("%s: tampered export produced a result: %+v", name, res)
		}
	}
	// Corruption after the target must not be ignored.
	check("field after target", func(r []AuditRecord) []AuditRecord {
		r[3].Decision.Decision.Allowed = !r[3].Decision.Decision.Allowed
		return r
	}, cp)
	check("policy after target", func(r []AuditRecord) []AuditRecord {
		r[2].Change.Policies = nil
		return r
	}, cp)
	check("delete middle", func(r []AuditRecord) []AuditRecord {
		return []AuditRecord{r[0], r[2], r[3]}
	}, cp)
	check("delete tail", func(r []AuditRecord) []AuditRecord { return r[:2] }, cp)
	check("swap order", func(r []AuditRecord) []AuditRecord {
		r[0], r[1] = r[1], r[0]
		return r
	}, cp)
	check("foreign record", func(r []AuditRecord) []AuditRecord {
		foreign := r[0]
		foreign.Org = "globex"
		return append(r, foreign)
	}, cp)
	check("wrong checkpoint", func(r []AuditRecord) []AuditRecord { return r },
		Checkpoint{Org: "acme", EndSeq: 4, Fingerprint: genesisFingerprint("globex")})
}

func TestRecheckDecisionOfflineReportsMismatch(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	good, _ := exportChain(t, s, "acme")

	// Capture the genuine decision before rewriting the recorded one.
	genuine := good[1].Decision.Decision
	tampered := append([]AuditRecord(nil), good...)
	tampered[1].Decision.Decision.Allowed = false
	tampered[1].Decision.Decision.Reason = "fiddled"
	tampered[1].Decision.Decision.Matched = nil
	recs, cp := rechain(tampered, 1)

	res, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Match {
		t.Fatalf("mismatch must be reported: %+v", res)
	}
	if res.Original.Allowed || res.Original.Reason != "fiddled" {
		t.Fatalf("original not preserved: %+v", res.Original)
	}
	if !res.Recomputed.Allowed || res.Recomputed.Reason != "matched allow policy" {
		t.Fatalf("recomputed must come from the export policies: %+v", res.Recomputed)
	}
	if !reflect.DeepEqual(res.Recomputed, genuine) {
		t.Fatalf("recomputed = %+v, want genuine decision %+v", res.Recomputed, genuine)
	}
}

func TestRecheckDecisionOfflineVersionMustPrecedeTarget(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 2, v1
	s.Publish("acme", 1, nil)                                      // seq 3, v2
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read")) // seq 4, v2
	good, _ := exportChain(t, s, "acme")
	deepCopy := func() []AuditRecord {
		out := make([]AuditRecord, len(good))
		for i := range good {
			out[i] = *cloneRecord(&good[i])
		}
		return out
	}

	// Version 2's change record sits at seq 3, after the seq-2 target.
	t1 := deepCopy()
	t1[1].Decision.Decision.Version = 2
	recs1, cp1 := rechain(t1, 1)
	if _, err := RecheckDecisionOffline("acme", recs1, cp1, 2); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("version recorded only after target err = %v, want ErrVersionNotFound", err)
	}

	// Version 3 was never published at all.
	t2 := deepCopy()
	t2[3].Decision.Decision.Version = 3
	recs2, cp2 := rechain(t2, 3)
	if _, err := RecheckDecisionOffline("acme", recs2, cp2, 4); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("version absent from export err = %v, want ErrVersionNotFound", err)
	}
}

func TestRecheckDecisionOfflineResultIsDetached(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	want := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp := exportChain(t, s, "acme")

	res, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the input after the call.
	recs[1].Decision.Decision.Allowed = false
	recs[1].Decision.Decision.Reason = "mutated"
	// The result is independent of the input mutation.
	if !decisionsEqual(res.Original, want) || !decisionsEqual(res.Recomputed, want) {
		t.Fatalf("result changed after input mutation: original=%+v recomputed=%+v", res.Original, res.Recomputed)
	}
	// Mutating the returned copy must not affect the input or a fresh call.
	res.Original.Allowed = false
	res.Recomputed.Allowed = false
	if recs[1].Decision.Decision.Allowed {
		t.Fatal("input changed via returned copy")
	}
	// The tampered input no longer verifies, so no new result is produced.
	if _, err := RecheckDecisionOffline("acme", recs, cp, 2); err == nil {
		t.Fatal("tampered input produced a result")
	}
}

func TestDecisionsEqualTreatsNilAndEmptyMatchedAsEqual(t *testing.T) {
	a := Decision{Allowed: false, Reason: "x", Version: 1, Matched: nil}
	b := Decision{Allowed: false, Reason: "x", Version: 1, Matched: []string{}}
	if !decisionsEqual(a, b) {
		t.Fatal("nil and empty matched lists must compare equal")
	}
	c := Decision{Allowed: false, Reason: "x", Version: 1, Matched: []string{"p1"}}
	if decisionsEqual(a, c) {
		t.Fatal("different matched lists must compare unequal")
	}
}

func TestRecheckDecisionOfflineDenyOverridesAndRolesDoNotGrant(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		{ID: "z-allow", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "a-deny", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
	})
	req := request("acme", "u1", "r1", "org/a", "read")
	req.Subject.Roles = []string{"owner", "org/a:read"}
	d := s.Decide("acme", req)
	if d.Allowed {
		t.Fatalf("deny must override: %+v", d)
	}
	recs, cp := exportChain(t, s, "acme")
	res, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Match || res.Recomputed.Allowed {
		t.Fatalf("recheck = %+v, want deny match", res)
	}
	wantMatched := []string{"a-deny", "z-allow"}
	if !reflect.DeepEqual(res.Recomputed.Matched, wantMatched) {
		t.Fatalf("matched = %v, want %v", res.Recomputed.Matched, wantMatched)
	}
}
