package darksafe

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// replaceByte swaps every occurrence of old with new in s.
func replaceByte(s string, old, new byte) string {
	b := []byte(s)
	for i := range b {
		if b[i] == old {
			b[i] = new
		}
	}
	return string(b)
}

// TestEncodeAuditStringMatchesJSONForValidUTF8 pins the compatibility
// guarantee: for every string that is valid UTF-8, the audit encoding is
// byte-identical to json.Marshal, so fingerprints of ordinary content are
// unchanged.
func TestEncodeAuditStringMatchesJSONForValidUTF8(t *testing.T) {
	cases := []string{
		"",
		"hello",
		"中文测试",
		"quote\"backslash\\",
		"tab\there\nnewline\r",
		"ctrl\x01\x02",
		"html <tag> & \"x\"",
		"line\u2028sep\u2029",
		"replacement\ufffd",
		"emoji 😀",
		"p-1",
		"org/payments/ledger",
	}
	for _, s := range cases {
		got := encodeAuditString(s)
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("string %q:\n got %s\nwant %s", s, got, want)
		}
	}
}

// TestEncodeAuditStringDistinguishesInvalidBytes verifies that raw invalid
// bytes are preserved distinctly, while the real U+FFFD character passes
// through as itself.
func TestEncodeAuditStringDistinguishesInvalidBytes(t *testing.T) {
	a := encodeAuditString("a\xffb")
	b := encodeAuditString("a\xfeb")
	c := encodeAuditString("a\ufffdb")
	if string(a) == string(b) {
		t.Errorf("0xFF and 0xFE encoded identically: %s", a)
	}
	if string(a) == string(c) || string(b) == string(c) {
		t.Errorf("invalid byte and U+FFFD encoded identically: %s %s %s", a, b, c)
	}
	// The real replacement character must survive, not be treated as damage.
	want, err := json.Marshal("a\ufffdb")
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != string(want) {
		t.Errorf("U+FFFD encoding = %s, want %s", c, want)
	}
}

// TestAuditEnvelopeEncodingMatchesJSONForValidContent verifies the full
// structural encoding (field order, omitempty, nil slices) matches
// json.Marshal for an envelope whose strings are all valid UTF-8.
func TestAuditEnvelopeEncodingMatchesJSONForValidContent(t *testing.T) {
	env := &hashEnvelope{
		Org:  "acme",
		Seq:  3,
		Kind: AuditDecision,
		Decision: &DecisionRecord{
			Request: OrgRequest{
				SubjectOrg:  "acme",
				ResourceOrg: "acme",
				Subject: Subject{
					ID:    "u1",
					Kind:  "user",
					Roles: []string{"owner", "org/a:read"},
				},
				Resource: Resource{ID: "r1", Scope: "org/a"},
				Action:   "read",
			},
			Decision: Decision{
				Allowed: true,
				Reason:  "matched allow policy",
				Matched: []string{"p1"},
				Version: 1,
			},
		},
		PrevFingerprint: "abc",
	}
	enc := auditHashEncoder{}
	enc.writeEnvelope(env)
	got := enc.buf.String()
	want, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("envelope encoding:\n got %s\nwant %s", got, want)
	}

	// A policy-change envelope with nil slices must also match.
	env2 := &hashEnvelope{
		Org:  "acme",
		Seq:  1,
		Kind: AuditPolicyChange,
		Change: &PolicyChange{
			Version:  1,
			Policies: nil,
		},
		PrevFingerprint: "genesis",
	}
	enc2 := auditHashEncoder{}
	enc2.writeEnvelope(env2)
	got2 := enc2.buf.String()
	want2, err := json.Marshal(env2)
	if err != nil {
		t.Fatal(err)
	}
	if got2 != string(want2) {
		t.Errorf("change envelope encoding:\n got %s\nwant %s", got2, want2)
	}
}

// TestAuditFingerprintPreservesRawInvalidBytes is the core regression test:
// strings submitted through the Go API may contain invalid UTF-8, and the
// fingerprint must cover the raw bytes. Replacing 0xFF with 0xFE (or with the
// real U+FFFD character) leaves the JSON rendering unchanged but must break
// VerifyAudit with ErrInvalidRange.
func TestAuditFingerprintPreservesRawInvalidBytes(t *testing.T) {
	s := NewStore()
	// Policy fields with invalid bytes: validation must still accept them.
	// The policy matches the request below on every field.
	policies := []Policy{{
		ID:      "p\xff",
		Subject: "中\xff文",
		Action:  "read\x99",
		Scope:   "org/a\x80",
		Effect:  EffectAllow,
	}}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatalf("publish with invalid bytes: %v", err)
	}
	// Request with invalid bytes spread across every string field.
	req := OrgRequest{
		SubjectOrg:  "acme",
		ResourceOrg: "acme",
		Subject: Subject{
			ID:    "中\xff文",
			Kind:  "user\xab",
			Roles: []string{"role\xff", "角色\xfe"},
		},
		Resource: Resource{ID: "r\xff", Scope: "org/a\x80"},
		Action:   "read\x99",
	}
	d := s.Decide("acme", req)
	if !d.Allowed {
		t.Fatalf("decision = %+v, want allowed", d)
	}

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("untampered export must verify: %v", err)
	}

	// The stored strings must be byte-identical to what was submitted.
	gotReq := recs[1].Decision.Request
	if gotReq.Subject.ID != req.Subject.ID || gotReq.Subject.Kind != req.Subject.Kind ||
		gotReq.Resource.ID != req.Resource.ID || gotReq.Resource.Scope != req.Resource.Scope ||
		gotReq.Action != req.Action {
		t.Fatalf("stored request not byte-identical: %+v", gotReq)
	}
	if !reflect.DeepEqual(gotReq.Subject.Roles, req.Subject.Roles) {
		t.Fatalf("stored roles not byte-identical: %v", gotReq.Subject.Roles)
	}
	if recs[0].Change.Policies[0].ID != policies[0].ID ||
		recs[0].Change.Policies[0].Subject != policies[0].Subject ||
		recs[0].Change.Policies[0].Action != policies[0].Action ||
		recs[0].Change.Policies[0].Scope != policies[0].Scope {
		t.Fatalf("stored policy not byte-identical: %+v", recs[0].Change.Policies[0])
	}

	// Tamper: replace the invalid byte in the subject id with a different
	// invalid byte. The JSON rendering is unchanged (both become U+FFFD),
	// but the fingerprint must now mismatch.
	t.Run("0xFF to 0xFE", func(t *testing.T) {
		tampered := append([]AuditRecord(nil), recs...)
		tampered[1].Decision.Request.Subject.ID = replaceByte(tampered[1].Decision.Request.Subject.ID, 0xFF, 0xFE)
		err := VerifyAudit("acme", tampered, cp)
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("tampered export err = %v, want ErrInvalidRange", err)
		}
	})

	// Tamper: replace the invalid byte with the real U+FFFD character. This
	// changes the byte count but the JSON rendering still shows the same
	// replacement character; it must be detected.
	t.Run("invalid byte to U+FFFD", func(t *testing.T) {
		tampered := append([]AuditRecord(nil), recs...)
		tampered[1].Decision.Request.Subject.ID = "中\ufffd文"
		err := VerifyAudit("acme", tampered, cp)
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("U+FFFD replacement err = %v, want ErrInvalidRange", err)
		}
	})

	// Tamper: invalid byte sandwiched between legal Chinese characters.
	t.Run("sandwiched invalid byte", func(t *testing.T) {
		tampered := append([]AuditRecord(nil), recs...)
		tampered[1].Decision.Request.Subject.ID = replaceByte(tampered[1].Decision.Request.Subject.ID, 0xFF, 0xFE)
		err := VerifyAudit("acme", tampered, cp)
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("sandwiched tamper err = %v, want ErrInvalidRange", err)
		}
	})

	// Tamper: a policy field.
	t.Run("policy field", func(t *testing.T) {
		tampered := append([]AuditRecord(nil), recs...)
		tampered[0].Change.Policies[0].ID = replaceByte(tampered[0].Change.Policies[0].ID, 0xFF, 0xFE)
		err := VerifyAudit("acme", tampered, cp)
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("policy tamper err = %v, want ErrInvalidRange", err)
		}
	})

	// Tamper: the org header itself.
	t.Run("org header", func(t *testing.T) {
		// Build a chain under an org with an invalid byte.
		s2 := NewStore()
		s2.Decide("acme\xff", OrgRequest{
			SubjectOrg:  "acme\xff",
			ResourceOrg: "acme\xff",
			Subject:     Subject{ID: "u1"},
			Resource:    Resource{ID: "r1", Scope: "org/a"},
			Action:      "read",
		})
		recs2, cp2, err := s2.AuditExport("acme\xff", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyAudit("acme\xff", recs2, cp2); err != nil {
			t.Fatalf("org with invalid byte must verify: %v", err)
		}
		tampered := append([]AuditRecord(nil), recs2...)
		tampered[0].Org = replaceByte(tampered[0].Org, 0xFF, 0xFE)
		err = VerifyAudit("acme\xff", tampered, cp2)
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("org header tamper err = %v, want ErrInvalidRange", err)
		}
	})

	// The legitimate U+FFFD character must not be judged as damage.
	t.Run("real U+FFFD is valid", func(t *testing.T) {
		s3 := NewStore()
		s3.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
		s3.Decide("acme", OrgRequest{
			SubjectOrg:  "acme",
			ResourceOrg: "acme",
			Subject:     Subject{ID: "中\ufffd文"},
			Resource:    Resource{ID: "r1", Scope: "org/a"},
			Action:      "read",
		})
		recs3, cp3, err := s3.AuditExport("acme", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyAudit("acme", recs3, cp3); err != nil {
			t.Fatalf("real U+FFFD must verify: %v", err)
		}
	})
}

// TestAuditFingerprintMixedValidAndInvalidRecords verifies that an export
// containing both ordinary records and records with invalid bytes validates
// normally.
func TestAuditFingerprintMixedValidAndInvalidRecords(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, []Policy{{
		ID:      "p\xff",
		Subject: "u\xff",
		Action:  "read",
		Scope:   "org/a",
		Effect:  EffectAllow,
	}})
	s.Decide("acme", OrgRequest{
		SubjectOrg:  "acme",
		ResourceOrg: "acme",
		Subject:     Subject{ID: "u\xff"},
		Resource:    Resource{ID: "r1", Scope: "org/a"},
		Action:      "read",
	})
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Fatalf("records = %d, want 5", len(recs))
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("mixed export must verify: %v", err)
	}
}

// TestAuditInvalidByteTamperingAfterTargetFailsOffline verifies that
// RecheckDecisionOffline rejects material with invalid-byte tampering,
// including damage in records after the target decision.
func TestAuditInvalidByteTamperingAfterTargetFailsOffline(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq1
	s.Decide("acme", OrgRequest{ // seq2 target
		SubjectOrg:  "acme",
		ResourceOrg: "acme",
		Subject:     Subject{ID: "u1"},
		Resource:    Resource{ID: "r1", Scope: "org/a"},
		Action:      "read",
	})
	s.Decide("acme", OrgRequest{ // seq3, invalid bytes after target
		SubjectOrg:  "acme",
		ResourceOrg: "acme",
		Subject:     Subject{ID: "u\xff"},
		Resource:    Resource{ID: "r1", Scope: "org/a"},
		Action:      "read",
	})

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Untampered: review succeeds.
	got, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatalf("offline review: %v", err)
	}
	if !got.Consistent {
		t.Fatalf("review inconsistent: %+v", got)
	}

	// Tamper the record after the target: full-export validation must fail.
	tampered := append([]AuditRecord(nil), recs...)
	tampered[2].Decision.Request.Subject.ID = replaceByte(tampered[2].Decision.Request.Subject.ID, 0xFF, 0xFE)
	if _, err := RecheckDecisionOffline("acme", tampered, cp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tamper after target err = %v, want ErrInvalidRange", err)
	}

	// Tamper the target itself.
	tampered = append([]AuditRecord(nil), recs...)
	tampered[1].Decision.Request.Subject.ID = "u\xfe"
	if _, err := RecheckDecisionOffline("acme", tampered, cp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tamper target err = %v, want ErrInvalidRange", err)
	}
}

// TestAuditInvalidByteInDecisionPayload covers the decision-side strings
// (reason and matched policies), which only appear in synthetic records.
func TestAuditInvalidByteInDecisionPayload(t *testing.T) {
	req := request("acme", "u1", "r1", "org/a", "read")
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
					Allowed: true,
					Reason:  "matched allow policy\xff",
					Matched: []string{"p1\xff", "角色\xfe"},
					Version: 1,
				},
			}
		},
	})
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("synthetic chain with invalid bytes must verify: %v", err)
	}

	// Tamper the reason.
	tampered := append([]AuditRecord(nil), recs...)
	tampered[1].Decision.Decision.Reason = replaceByte(tampered[1].Decision.Decision.Reason, 0xFF, 0xFE)
	if err := VerifyAudit("acme", tampered, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("reason tamper err = %v, want ErrInvalidRange", err)
	}

	// Tamper a matched policy entry.
	tampered = append([]AuditRecord(nil), recs...)
	tampered[1].Decision.Decision.Matched[0] = replaceByte(tampered[1].Decision.Decision.Matched[0], 0xFF, 0xFE)
	if err := VerifyAudit("acme", tampered, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("matched tamper err = %v, want ErrInvalidRange", err)
	}

	// Offline review must also reject the tampered decision payload.
	tampered = append([]AuditRecord(nil), recs...)
	tampered[1].Decision.Decision.Reason = replaceByte(tampered[1].Decision.Decision.Reason, 0xFF, 0xFE)
	if _, err := RecheckDecisionOffline("acme", tampered, cp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("offline review reason tamper err = %v, want ErrInvalidRange", err)
	}
}

// TestAuditInvalidByteDoesNotChangePolicyMatching confirms that preserving
// raw bytes does not alter policy matching or decision results: the same
// request still produces the same decision.
func TestAuditInvalidByteDoesNotChangePolicyMatching(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	req := request("acme", "u1", "r1", "org/a", "read")
	d1 := s.Decide("acme", req)
	// A request with invalid bytes in non-matching fields still matches.
	req2 := request("acme", "u1", "r1", "org/a", "read")
	req2.Subject.Kind = "user\xff"
	req2.Subject.Roles = []string{"role\xff"}
	d2 := s.Decide("acme", req2)
	if !reflect.DeepEqual(d1, d2) {
		t.Fatalf("decision changed: %+v vs %+v", d1, d2)
	}
}
