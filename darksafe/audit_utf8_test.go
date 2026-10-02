package darksafe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// invalidByte is a lone byte that is never valid UTF-8 on its own.
const invalidByte = "\xff"

// anotherInvalidByte renders identically under JSON replacement but is a
// different raw byte.
const anotherInvalidByte = "\xfe"

// replacementRune is a genuine U+FFFD code point: valid UTF-8, unlike the
// lone invalid bytes.
const replacementRune = string(rune(0xFFFD))

// legacyJSONFingerprint is the exact fingerprint algorithm used before
// the raw-content integrity fix: encoding/json over the envelope. Tests
// use it to prove valid-UTF-8 records keep byte-identical fingerprints.
func legacyJSONFingerprint(r *AuditRecord) string {
	b, err := json.Marshal(&hashEnvelope{
		Org:             r.Org,
		Seq:             r.Seq,
		Kind:            r.Kind,
		Change:          r.Change,
		Decision:        r.Decision,
		PrevFingerprint: r.PrevFingerprint,
	})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// cloneExportedRecords deep-copies a record set so mutating any string
// field or roles/matched/policies slice in the copy never reaches the
// original. encoding/json cannot be used for this: it would "repair" the
// invalid bytes under test.
func cloneExportedRecords(in []AuditRecord) []AuditRecord {
	out := make([]AuditRecord, len(in))
	for i, r := range in {
		cp := r
		if r.Change != nil {
			c := *r.Change
			if r.Change.Policies != nil {
				c.Policies = append([]Policy(nil), r.Change.Policies...)
			}
			cp.Change = &c
		}
		if r.Decision != nil {
			d := *r.Decision
			if r.Decision.Request.Subject.Roles != nil {
				d.Request.Subject.Roles = append([]string(nil), r.Decision.Request.Subject.Roles...)
			}
			if r.Decision.Decision.Matched != nil {
				d.Decision.Matched = append([]string(nil), r.Decision.Decision.Matched...)
			}
			cp.Decision = &d
		}
		out[i] = cp
	}
	return out
}

// TestInvalidUTF8IsAcceptedAndPreservedByteForByte drives the live API
// with strings carrying invalid UTF-8 in every policy and request string
// reachable through the public surface. Acceptance must be unchanged, no
// byte may be deleted, replaced or normalized, and query/export/offline
// review must observe the exact submitted bytes.
func TestInvalidUTF8IsAcceptedAndPreservedByteForByte(t *testing.T) {
	s := NewStore()
	submitted := Policy{
		ID:      "策" + invalidByte + "略",
		Subject: "用" + invalidByte + "戶",
		Action:  "读" + invalidByte,
		Scope:   "org/a/资" + invalidByte + "料",
		Effect:  EffectAllow,
	}
	v, err := s.Publish("acme", 0, []Policy{submitted})
	if err != nil || v != 1 {
		t.Fatalf("publish with invalid UTF-8 = %d, %v; want accepted", v, err)
	}

	req := OrgRequest{
		SubjectOrg:  "acme",
		ResourceOrg: "acme",
		Subject: Subject{
			ID:    "用" + invalidByte + "戶",
			Kind:  "外" + invalidByte + "部",
			Roles: []string{"角" + invalidByte + "色", "普通角色"},
		},
		Resource: Resource{ID: "资" + invalidByte + "源", Scope: submitted.Scope},
		Action:   submitted.Action,
	}
	d := s.Decide("acme", req)
	if !d.Allowed || d.Version != 1 || d.Reason != "matched allow policy" {
		t.Fatalf("decision on invalid-UTF-8 request = %+v", d)
	}
	if want := []string{submitted.ID}; !reflect.DeepEqual(d.Matched, want) {
		t.Fatalf("matched = %q, want byte-exact %q", d.Matched, want)
	}
	if !strings.Contains(d.Matched[0], invalidByte) {
		t.Fatal("invalid byte normalized out of matched policy id")
	}

	// A rollback republishes the invalid-byte content and records it.
	if rv, err := s.Rollback("acme", 1, 1); err != nil || rv != 2 {
		t.Fatalf("rollback = %d, %v", rv, err)
	}
	// A normal record and an invalid-byte record coexist in one export.
	if _, err := s.Publish("acme", 2, nil); err != nil {
		t.Fatal(err)
	}
	plain := s.Decide("acme", request("acme", "plain", "r1", "org/a", "read"))
	if plain.Allowed {
		t.Fatalf("plain request under empty version must deny: %+v", plain)
	}

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("export with invalid UTF-8 must verify: %v", err)
	}

	// The export is byte-for-byte the submitted content at every level.
	gotPolicy := recs[0].Change.Policies[0]
	if !reflect.DeepEqual(gotPolicy, submitted) {
		t.Fatalf("policy bytes = %+v, want %+v", gotPolicy, submitted)
	}
	gotReq := recs[1].Decision.Request
	if !reflect.DeepEqual(gotReq, req) {
		t.Fatalf("request bytes = %+v, want %+v", gotReq, req)
	}
	if !reflect.DeepEqual(recs[1].Decision.Decision, d) {
		t.Fatalf("decision bytes = %+v, want %+v", recs[1].Decision.Decision, d)
	}
	if !reflect.DeepEqual(recs[2].Change.Policies, []Policy{submitted}) {
		t.Fatal("rollback record lost the published raw bytes")
	}
	for _, haystack := range []string{
		gotPolicy.ID, gotPolicy.Subject, gotPolicy.Action, gotPolicy.Scope,
		gotReq.Subject.ID, gotReq.Subject.Kind, gotReq.Subject.Roles[0],
		gotReq.Resource.ID, gotReq.Resource.Scope, gotReq.Action,
	} {
		if !strings.Contains(haystack, invalidByte) {
			t.Fatalf("invalid byte was deleted or normalized in %q", haystack)
		}
	}

	// Paged queries return the same bytes.
	page, err := s.AuditQuery("acme", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	queried := append([]AuditRecord(nil), page.Records...)
	for page.Next != 0 {
		page, err = s.AuditPage(page.Checkpoint, page.Next, 1, "", "")
		if err != nil {
			t.Fatal(err)
		}
		queried = append(queried, page.Records...)
	}
	if !reflect.DeepEqual(queried, recs) {
		t.Fatal("paged query bytes differ from the export bytes")
	}

	// Offline review recomputes from the raw request and recorded version.
	review, err := RecheckDecisionOffline("acme", recs, cp, 2)
	if err != nil {
		t.Fatalf("offline review: %v", err)
	}
	if !review.Consistent || !reflect.DeepEqual(review.Original, d) ||
		!reflect.DeepEqual(review.Recomputed, d) {
		t.Fatalf("offline review = %+v, want consistent with %+v", review, d)
	}
	if !strings.Contains(review.Recomputed.Matched[0], invalidByte) {
		t.Fatal("offline review normalized the matched policy id")
	}

	// An organization whose name itself carries invalid bytes: genesis
	// checkpoint, isolation and offline review all still work.
	weirdOrg := "组" + invalidByte + "织"
	if _, err := s.Publish(weirdOrg, 0, []Policy{submitted}); err != nil {
		t.Fatalf("publish into invalid-UTF-8 org: %v", err)
	}
	wreq := req
	wreq.SubjectOrg = weirdOrg
	wreq.ResourceOrg = weirdOrg
	if d := s.Decide(weirdOrg, wreq); !d.Allowed {
		t.Fatalf("decision inside invalid-UTF-8 org = %+v", d)
	}
	wrecs, wcp, err := s.AuditExport(weirdOrg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(weirdOrg, wrecs, wcp); err != nil {
		t.Fatalf("invalid-UTF-8 org chain: %v", err)
	}
	if wr, err := RecheckDecisionOffline(weirdOrg, wrecs, wcp, 2); err != nil || !wr.Consistent {
		t.Fatalf("invalid-UTF-8 org offline review = %+v, %v", wr, err)
	}
	// The sequence-0 root checkpoint is the genesis for that org too.
	empty, emptyCP, err := s.AuditExport(weirdOrg+"-none", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(weirdOrg+"-none", empty, emptyCP); err != nil {
		t.Fatalf("genesis checkpoint: %v", err)
	}
}

// TestInvalidBytesFingerprintBuildsChainOfMixedRecords checks ordinary
// and invalid-byte records chaining together: ordinary records keep the
// legacy JSON fingerprint, the invalid-byte record verifies and is pinned
// by the checkpoint, and tampering either family fails with
// ErrInvalidRange.
func TestInvalidBytesFingerprintBuildsChainOfMixedRecords(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, []Policy{{
		ID:      "策" + invalidByte + "略",
		Subject: "用" + invalidByte + "戶",
		Action:  "read",
		Scope:   "org/a",
		Effect:  EffectAllow,
	}})
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("mixed chain: %v", err)
	}
	if got := fingerprintFor(&recs[0]); got != legacyJSONFingerprint(&recs[0]) {
		t.Fatal("valid-UTF-8 record changed fingerprint family")
	}
	if got := fingerprintFor(&recs[1]); got != legacyJSONFingerprint(&recs[1]) {
		t.Fatal("valid-UTF-8 decision changed fingerprint family")
	}
	if got := fingerprintFor(&recs[2]); got == legacyJSONFingerprint(&recs[2]) {
		t.Fatal("invalid-byte record still uses the JSON fingerprint")
	}
	if recs[2].Fingerprint != cp.Fingerprint {
		t.Fatal("checkpoint does not pin the raw-encoded final record")
	}
	bad := cloneExportedRecords(recs)
	bad[2].Change.Policies[0].ID = strings.Replace(bad[2].Change.Policies[0].ID, invalidByte, anotherInvalidByte, 1)
	if err := VerifyAudit("acme", bad, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tampered invalid-byte record err = %v, want ErrInvalidRange", err)
	}
	bad = cloneExportedRecords(recs)
	bad[0].Change.Policies[0].ID = "p2"
	if err := VerifyAudit("acme", bad, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tampered ordinary record err = %v, want ErrInvalidRange", err)
	}
}

// buildInvalidByteChain constructs a chain where every fingerprinted
// string field can be probed. Record 1 is a policy change, record 2 the
// reviewed decision with an invalid byte embedded between legal Chinese
// in every nested field and list, record 3 a decision after the target.
func buildInvalidByteChain(t *testing.T) (string, []AuditRecord, Checkpoint) {
	t.Helper()
	org := "组" + invalidByte + "织"
	subjectID := "身" + invalidByte + "份"
	action := "动" + invalidByte + "作"
	scope := "范" + invalidByte + "围"
	p1 := Policy{
		ID:      "策" + invalidByte + "id",
		Subject: subjectID,
		Action:  action,
		Scope:   scope,
		Effect:  EffectAllow,
	}
	p2 := Policy{
		ID:      "命" + invalidByte + "中二",
		Subject: subjectID,
		Action:  action,
		Scope:   scope,
		Effect:  EffectAllow,
	}
	prev := genesisFingerprint(org)
	rec1 := syntheticRecord(org, 1, AuditPolicyChange, &PolicyChange{
		Version: 1, Policies: []Policy{p1, p2},
	}, nil, prev)
	prev = rec1.Fingerprint
	rec2 := syntheticRecord(org, 2, AuditDecision, nil, &DecisionRecord{
		Request: OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject: Subject{
				ID:    subjectID, // invalid byte between legal Chinese
				Kind:  "类" + invalidByte + "型",
				Roles: []string{"角" + invalidByte + "色", "正常角色"},
			},
			Resource: Resource{ID: "资" + invalidByte + "源", Scope: scope},
			Action:   action,
		},
		Decision: Decision{
			Allowed: true,
			Reason:  "理" + invalidByte + "由",
			Matched: []string{p2.ID, p1.ID}, // sorted by raw bytes
			Version: 1,
		},
	}, prev)
	prev = rec2.Fingerprint
	rec3 := syntheticRecord(org, 3, AuditDecision, nil, &DecisionRecord{
		Request: OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject:     Subject{ID: "尾" + invalidByte + "随"},
			Resource:    Resource{ID: "later", Scope: "org/a"},
			Action:      "read",
		},
		Decision: Decision{Allowed: false, Reason: "no matching allow policy", Version: 1},
	}, prev)
	recs := []AuditRecord{rec1, rec2, rec3}
	cp := Checkpoint{Org: org, EndSeq: 3, Fingerprint: rec3.Fingerprint}
	if err := VerifyAudit(org, recs, cp); err != nil {
		t.Fatalf("test fixture must verify: %v", err)
	}
	return org, recs, cp
}

// TestTamperedInvalidBytesRejectedEverywhere replaces the lone 0xFF in
// every protected field with either 0xFE (the same display after JSON
// replacement) or a genuine U+FFFD. VerifyAudit must fail with
// ErrInvalidRange and offline review must refuse before returning any
// conclusion, including when the damage is after the target decision.
func TestTamperedInvalidBytesRejectedEverywhere(t *testing.T) {
	org, good, cp := buildInvalidByteChain(t)

	// Document the root cause: JSON renders both raw bytes identically.
	j1, _ := json.Marshal("身" + invalidByte + "份")
	j2, _ := json.Marshal("身" + anotherInvalidByte + "份")
	if !bytes.Equal(j1, j2) {
		t.Fatalf("test premise broken: JSON differs: %s vs %s", j1, j2)
	}

	mutators := []struct {
		name string
		fn   func([]AuditRecord, string)
	}{
		{"header org", func(r []AuditRecord, repl string) {
			r[0].Org = strings.Replace(r[0].Org, invalidByte, repl, 1)
		}},
		{"record kind", func(r []AuditRecord, repl string) {
			r[1].Kind += repl
		}},
		{"policy id", func(r []AuditRecord, repl string) {
			p := &r[0].Change.Policies[0]
			p.ID = strings.Replace(p.ID, invalidByte, repl, 1)
		}},
		{"policy subject", func(r []AuditRecord, repl string) {
			p := &r[0].Change.Policies[0]
			p.Subject = strings.Replace(p.Subject, invalidByte, repl, 1)
		}},
		{"policy action", func(r []AuditRecord, repl string) {
			p := &r[0].Change.Policies[0]
			p.Action = strings.Replace(p.Action, invalidByte, repl, 1)
		}},
		{"policy scope", func(r []AuditRecord, repl string) {
			p := &r[0].Change.Policies[0]
			p.Scope = strings.Replace(p.Scope, invalidByte, repl, 1)
		}},
		{"policy effect", func(r []AuditRecord, repl string) {
			p := &r[0].Change.Policies[0]
			p.Effect = Effect(string(p.Effect) + repl)
		}},
		{"second policy in list", func(r []AuditRecord, repl string) {
			p := &r[0].Change.Policies[1]
			p.ID = strings.Replace(p.ID, invalidByte, repl, 1)
		}},
		{"subject org", func(r []AuditRecord, repl string) {
			r[1].Decision.Request.SubjectOrg = strings.Replace(r[1].Decision.Request.SubjectOrg, invalidByte, repl, 1)
		}},
		{"resource org", func(r []AuditRecord, repl string) {
			r[1].Decision.Request.ResourceOrg = strings.Replace(r[1].Decision.Request.ResourceOrg, invalidByte, repl, 1)
		}},
		{"subject id between chinese", func(r []AuditRecord, repl string) {
			id := &r[1].Decision.Request.Subject.ID
			*id = strings.Replace(*id, invalidByte, repl, 1)
		}},
		{"subject kind", func(r []AuditRecord, repl string) {
			r[1].Decision.Request.Subject.Kind = strings.Replace(r[1].Decision.Request.Subject.Kind, invalidByte, repl, 1)
		}},
		{"first role in list", func(r []AuditRecord, repl string) {
			roles := r[1].Decision.Request.Subject.Roles
			roles[0] = strings.Replace(roles[0], invalidByte, repl, 1)
		}},
		{"second role in list", func(r []AuditRecord, repl string) {
			r[1].Decision.Request.Subject.Roles[1] += repl
		}},
		{"resource id", func(r []AuditRecord, repl string) {
			r[1].Decision.Request.Resource.ID = strings.Replace(r[1].Decision.Request.Resource.ID, invalidByte, repl, 1)
		}},
		{"resource scope", func(r []AuditRecord, repl string) {
			r[1].Decision.Request.Resource.Scope = strings.Replace(r[1].Decision.Request.Resource.Scope, invalidByte, repl, 1)
		}},
		{"request action", func(r []AuditRecord, repl string) {
			r[1].Decision.Request.Action = strings.Replace(r[1].Decision.Request.Action, invalidByte, repl, 1)
		}},
		{"decision reason", func(r []AuditRecord, repl string) {
			r[1].Decision.Decision.Reason = strings.Replace(r[1].Decision.Decision.Reason, invalidByte, repl, 1)
		}},
		{"first matched policy", func(r []AuditRecord, repl string) {
			m := r[1].Decision.Decision.Matched
			m[0] = strings.Replace(m[0], invalidByte, repl, 1)
		}},
		{"second matched policy", func(r []AuditRecord, repl string) {
			m := r[1].Decision.Decision.Matched
			m[1] = strings.Replace(m[1], invalidByte, repl, 1)
		}},
		{"record after target", func(r []AuditRecord, repl string) {
			id := &r[2].Decision.Request.Subject.ID
			*id = strings.Replace(*id, invalidByte, repl, 1)
		}},
	}

	for _, repl := range []string{anotherInvalidByte, replacementRune} {
		for _, m := range mutators {
			recs := cloneExportedRecords(good)
			m.fn(recs, repl)
			name := m.name + "/repl=" + replacementLabel(repl)
			if err := VerifyAudit(org, recs, cp); !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("%s: VerifyAudit err = %v, want ErrInvalidRange", name, err)
			}
			// Offline review must reject first: never a successful or
			// "consistent" conclusion over tampered material.
			if got, err := RecheckDecisionOffline(org, recs, cp, 2); !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("%s: RecheckDecisionOffline err = %v, review = %+v, want ErrInvalidRange", name, err, got)
			}
		}
	}
}

// replacementLabel names a substitution without relying on rendered
// output, which displays both cases as a replacement character.
func replacementLabel(s string) string {
	switch s {
	case anotherInvalidByte:
		return "0xFE"
	case replacementRune:
		return "U+FFFD"
	default:
		return "?"
	}
}

// TestReplacementRuneIsGenuineContentToo separates three byte-different
// strings that displays may conflate: a lone 0xFF, a lone 0xFE, and a
// real U+FFFD rune. A record genuinely containing U+FFFD is valid UTF-8,
// verifies and keeps the legacy fingerprint; substituting any one for
// another is a content change.
func TestReplacementRuneIsGenuineContentToo(t *testing.T) {
	s := NewStore()
	policies := []Policy{{
		ID:      "p-" + replacementRune + "-1",
		Subject: "用" + replacementRune + "戶",
		Action:  "read", Scope: "org/a", Effect: EffectAllow,
	}}
	s.Publish("acme", 0, policies)
	req := request("acme", "用"+replacementRune+"戶", "r1", "org/a", "read")
	d := s.Decide("acme", req)
	if !d.Allowed {
		t.Fatalf("request with genuine U+FFFD = %+v", d)
	}
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("genuine U+FFFD must verify: %v", err)
	}
	if !utf8.ValidString(recs[0].Change.Policies[0].ID) {
		t.Fatal("genuine U+FFFD must remain valid UTF-8")
	}
	if fingerprintFor(&recs[0]) != legacyJSONFingerprint(&recs[0]) {
		t.Fatal("valid-UTF-8 U+FFFD record must keep the legacy JSON fingerprint")
	}
	if review, err := RecheckDecisionOffline("acme", recs, cp, 2); err != nil || !review.Consistent {
		t.Fatalf("genuine U+FFFD review = %+v, %v", review, err)
	}

	// Raw invalid bytes exported from the live API.
	raw := NewStore()
	raw.Publish("acme", 0, []Policy{{
		ID: "p" + invalidByte, Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow,
	}})
	rawRecs, rawCP, err := raw.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", rawRecs, rawCP); err != nil {
		t.Fatalf("raw-byte record must verify: %v", err)
	}

	// Replace the raw byte with a genuine U+FFFD rune: content changed.
	tampered := cloneExportedRecords(rawRecs)
	tampered[0].Change.Policies[0].ID = "p" + replacementRune
	if err := VerifyAudit("acme", tampered, rawCP); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("raw 0xFF -> U+FFFD err = %v, want ErrInvalidRange", err)
	}

	// The reverse substitution is also a change: the valid-UTF-8 record's
	// real U+FFFD replaced by a lone 0xFF.
	back := cloneExportedRecords(recs)
	back[0].Change.Policies[0].ID = "p-" + invalidByte + "-1"
	if err := VerifyAudit("acme", back, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("U+FFFD -> raw 0xFF err = %v, want ErrInvalidRange", err)
	}
}

// TestLegacyFingerprintsUnchangedForValidUTF8 proves byte-identical
// fingerprints for valid UTF-8 across policy changes, rollbacks,
// decisions, nil-vs-empty lists, and strings full of characters JSON
// escapes specially: Chinese, quotes, backslashes, control characters,
// angle/ampersand and a genuine replacement rune.
func TestLegacyFingerprintsUnchangedForValidUTF8(t *testing.T) {
	org := "acme"
	tricky := []Policy{{
		ID:        "p\"1\\中\t\n\r<>&" + replacementRune,
		Subject:   "主\"体\\",
		Action:    "a\tb\nc",
		Scope:     "org/中/区",
		Effect:    EffectAllow,
		Recursive: true,
	}}
	prev := genesisFingerprint(org)
	records := []AuditRecord{
		syntheticRecord(org, 1, AuditPolicyChange, &PolicyChange{
			Version: 1, Policies: tricky,
		}, nil, prev),
	}
	prev = records[0].Fingerprint
	// Rollback carrying a nil policy set.
	records = append(records, syntheticRecord(org, 2, AuditPolicyChange, &PolicyChange{
		Version: 2, Policies: nil, SourceVersion: 1, RolledBack: true,
	}, nil, prev))
	prev = records[1].Fingerprint
	// Decision with nil roles and nil matched lists.
	records = append(records, syntheticRecord(org, 3, AuditDecision, nil, &DecisionRecord{
		Request: OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject:     Subject{ID: "身\"份\\中"},
			Resource:    Resource{ID: "r\"1", Scope: "org/中/区"},
			Action:      "a\tb",
		},
		Decision: Decision{Allowed: true, Reason: "理\"由\n中", Version: 2},
	}, prev))
	prev = records[2].Fingerprint
	// Decision with explicitly empty (non-nil) lists.
	records = append(records, syntheticRecord(org, 4, AuditDecision, nil, &DecisionRecord{
		Request: OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject:     Subject{ID: "u2", Roles: []string{}},
			Resource:    Resource{ID: "r2", Scope: "org/a"},
			Action:      "read",
		},
		Decision: Decision{Allowed: false, Reason: "no matching allow policy", Matched: []string{}, Version: 2},
	}, prev))

	for i := range records {
		r := &records[i]
		if got := fingerprintFor(r); got != legacyJSONFingerprint(r) {
			t.Fatalf("record %d fingerprint drifted from the legacy JSON encoding", r.Seq)
		}
	}
	cp := Checkpoint{Org: org, EndSeq: len(records), Fingerprint: records[len(records)-1].Fingerprint}
	if err := VerifyAudit(org, records, cp); err != nil {
		t.Fatalf("legacy-compatible chain must verify: %v", err)
	}

	// Sanity: an invalid byte makes the same shape leave the JSON family.
	raw := syntheticRecord(org, 5, AuditDecision, nil, &DecisionRecord{
		Request:  request(org, "u"+invalidByte, "r1", "org/a", "read"),
		Decision: Decision{Allowed: false, Reason: "x", Version: 2},
	}, cp.Fingerprint)
	if fingerprintFor(&raw) == legacyJSONFingerprint(&raw) {
		t.Fatal("invalid-byte envelope unexpectedly shares the JSON fingerprint")
	}
}
