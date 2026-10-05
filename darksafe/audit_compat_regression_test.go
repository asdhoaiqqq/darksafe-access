package darksafe

// Compatibility regression guard for offline review of saved audit
// material.
//
// The other audit/archive/offline-review tests regenerate their material
// on every run: they prove the current build is self-consistent, but they
// cannot notice when generation and validation drift together and a
// checkpoint a user saved with an earlier build stops validating. This
// file pins the compatibility baseline as static checked-in data under
// testdata/auditcompat/:
//
//   - archive.bin: an archive produced by EncodeAuditArchive from a live
//     export, saved when the producing service instance ended.
//   - expected.json: the separately retained checkpoint, every record's
//     own and predecessor fingerprint, and the target decision's fields.
//
// The tests below work purely from those saved bytes — no Store is
// created — exactly like an organization reviewing its decisions after
// the service instance that decided them is gone. Nothing in a normal
// test run rewrites the fixture: it only changes through a deliberate,
// reviewable regeneration (see TestCompatFixtureRegenerate), so a drift
// in fingerprinting, archiving or recomputation fails these tests instead
// of silently re-baselining.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// compatFixtureDir holds the static compatibility baseline.
const compatFixtureDir = "auditcompat"

// compatOrg is the organization the saved chain belongs to.
const compatOrg = "acme"

// compatTargetSeq is the sequence of the reviewed decision: authorized by
// the resource-restricted allow policy, with the request resource matching
// the restriction.
const compatTargetSeq = 2

// compatCheckpoint is the separately retained checkpoint, as saved.
type compatCheckpoint struct {
	EndSeq      int    `json:"endSeq"`
	Fingerprint string `json:"fingerprint"`
}

// compatRecordPin is one record's saved identity: its sequence, category
// and both fingerprints linking it into the chain.
type compatRecordPin struct {
	Seq             int    `json:"seq"`
	Kind            string `json:"kind"`
	Fingerprint     string `json:"fingerprint"`
	PrevFingerprint string `json:"prevFingerprint"`
}

// compatDecision is the saved shape of one decision's outcome fields.
type compatDecision struct {
	Allowed bool     `json:"allowed"`
	Reason  string   `json:"reason"`
	Matched []string `json:"matched"`
	Version int      `json:"version"`
}

// compatExpected is the saved compatibility baseline next to the archive.
type compatExpected struct {
	Org        string            `json:"org"`
	Checkpoint compatCheckpoint  `json:"checkpoint"`
	Records    []compatRecordPin `json:"records"`
	TargetSeq  int               `json:"targetSeq"`
	Target     compatDecision    `json:"target"`
}

// checkpoint returns the retained checkpoint as the audit entry points
// expect it.
func (e compatExpected) checkpoint() Checkpoint {
	return Checkpoint{Org: e.Org, EndSeq: e.Checkpoint.EndSeq, Fingerprint: e.Checkpoint.Fingerprint}
}

// The fixture chain, as produced by the live API at fixture time:
//
//	seq 1  policy change: version 1 publishes the resource-restricted
//	       allow policy below (plain-text record).
//	seq 2  decision: the target request, allowed by that policy with the
//	       request resource matching the restriction (plain-text record,
//	       roles not provided).
//	seq 3  decision: a request carrying lone 0xFF bytes in every nested
//	       string, roles explicitly empty (invalid-byte record).
//	seq 4  decision: a genuine U+FFFD subject with a populated role list
//	       that itself carries a lone 0xFF (mixed-content record).
//
// Plain-text and invalid-byte records alternate through one chain, so both
// fingerprint families' chaining is pinned by the saved checkpoint.

// compatResourceAllowPolicy is the version-1 policy that authorized the
// target decision: an allow narrowed to the exact resource "r1".
func compatResourceAllowPolicy() Policy {
	return Policy{
		ID: "p1", Subject: "u1", Action: "read", Scope: "org/a",
		Effect: EffectAllow, ResourceID: "r1",
	}
}

// compatTargetRequest is the reviewed request. Its role list was never
// provided (nil), and that shape is part of the saved material.
func compatTargetRequest() OrgRequest {
	return OrgRequest{
		SubjectOrg:  compatOrg,
		ResourceOrg: compatOrg,
		Subject:     Subject{ID: "u1"},
		Resource:    Resource{ID: "r1", Scope: "org/a"},
		Action:      "read",
	}
}

// compatInvalidByteRequest carries a lone 0xFF in every nested string and
// an explicitly empty (non-nil) role list. Roles take no part in
// organization-level authorization, but they are audit request content and
// their saved shape must survive untouched.
func compatInvalidByteRequest() OrgRequest {
	return OrgRequest{
		SubjectOrg:  compatOrg,
		ResourceOrg: compatOrg,
		Subject: Subject{
			ID:    "用" + invalidByte + "戶",
			Kind:  "外" + invalidByte + "部",
			Roles: []string{},
		},
		Resource: Resource{ID: "资" + invalidByte + "源", Scope: "org/a"},
		Action:   "读" + invalidByte,
	}
}

// compatReplacementRuneRequest carries a genuine U+FFFD code point (valid
// UTF-8, unlike the lone bytes) and a populated role list whose element
// holds a lone 0xFF.
func compatReplacementRuneRequest() OrgRequest {
	return OrgRequest{
		SubjectOrg:  compatOrg,
		ResourceOrg: compatOrg,
		Subject: Subject{
			ID:    "客" + replacementRune + "商",
			Roles: []string{"角" + invalidByte + "色"},
		},
		Resource: Resource{ID: "r2", Scope: "org/a"},
		Action:   "read",
	}
}

// loadCompatFixture reads the static baseline. It never regenerates it.
func loadCompatFixture(t *testing.T) (archive []byte, exp compatExpected) {
	t.Helper()
	dir := filepath.Join("testdata", compatFixtureDir)
	archive, err := os.ReadFile(filepath.Join(dir, "archive.bin"))
	if err != nil {
		t.Fatalf("read saved archive: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatalf("read saved expectations: %v", err)
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatalf("parse saved expectations: %v", err)
	}
	// The fixture's own shape: four records, the target at its sequence.
	if exp.Org != compatOrg {
		t.Fatalf("fixture org = %q, want %q", exp.Org, compatOrg)
	}
	if len(exp.Records) != 4 {
		t.Fatalf("fixture records = %d, want 4", len(exp.Records))
	}
	if exp.TargetSeq != compatTargetSeq {
		t.Fatalf("fixture target = %d, want %d", exp.TargetSeq, compatTargetSeq)
	}
	return archive, exp
}

// decodeCompatFixture reads the saved archive against the separately
// retained checkpoint, as a reviewer would after the service ended.
func decodeCompatFixture(t *testing.T, archive []byte, exp compatExpected) []AuditRecord {
	t.Helper()
	recs, err := DecodeAuditArchive(archive, exp.Org, exp.checkpoint())
	if err != nil {
		t.Fatalf("saved archive must still decode: %v", err)
	}
	return recs
}

// TestCompatFixtureChainAndArchiveStillValidate proves the saved material
// remains valid input for every public read path: chain validation,
// archive decoding and deterministic re-encoding, with every record's
// organization, sequence, category and both fingerprints exactly as saved.
func TestCompatFixtureChainAndArchiveStillValidate(t *testing.T) {
	archive, exp := loadCompatFixture(t)
	cp := exp.checkpoint()

	recs := decodeCompatFixture(t, archive, exp)
	if err := VerifyAudit(exp.Org, recs, cp); err != nil {
		t.Fatalf("saved chain must still verify against its checkpoint: %v", err)
	}

	// Re-encoding the decoded material reproduces the saved bytes exactly:
	// the archive format itself has not drifted.
	again, err := EncodeAuditArchive(exp.Org, recs, cp)
	if err != nil {
		t.Fatalf("re-encode decoded material: %v", err)
	}
	if !bytes.Equal(again, archive) {
		t.Fatal("re-encoded archive differs from the saved bytes")
	}

	// Every record's identity is exactly what was saved.
	for i, pin := range exp.Records {
		r := recs[i]
		if r.Org != exp.Org {
			t.Fatalf("record %d org = %q, want %q", pin.Seq, r.Org, exp.Org)
		}
		if r.Seq != pin.Seq || r.Kind != pin.Kind {
			t.Fatalf("record %d = seq %d kind %q, want seq %d kind %q",
				i+1, r.Seq, r.Kind, pin.Seq, pin.Kind)
		}
		if r.Fingerprint != pin.Fingerprint {
			t.Fatalf("record %d fingerprint = %q, want saved %q", pin.Seq, r.Fingerprint, pin.Fingerprint)
		}
		if r.PrevFingerprint != pin.PrevFingerprint {
			t.Fatalf("record %d prev fingerprint = %q, want saved %q", pin.Seq, r.PrevFingerprint, pin.PrevFingerprint)
		}
	}

	// The published policy content reads back exactly as saved.
	if recs[0].Change == nil ||
		!reflect.DeepEqual(recs[0].Change.Policies, []Policy{compatResourceAllowPolicy()}) {
		t.Fatalf("published policies = %+v, want the resource-restricted allow", recs[0].Change)
	}

	// The full requests read back exactly as saved, including the role
	// list shapes: never provided stays nil, explicitly empty stays
	// non-nil empty. Roles decide nothing at the organization level, but
	// they are audit content and must not be normalized into each other.
	targetReq := recs[1].Decision.Request
	if !reflect.DeepEqual(targetReq, compatTargetRequest()) {
		t.Fatalf("target request = %+v, want %+v", targetReq, compatTargetRequest())
	}
	if targetReq.Subject.Roles != nil {
		t.Fatalf("target roles = %#v, want nil (never provided)", targetReq.Subject.Roles)
	}
	invalidReq := recs[2].Decision.Request
	if !reflect.DeepEqual(invalidReq, compatInvalidByteRequest()) {
		t.Fatalf("invalid-byte request = %+v, want %+v", invalidReq, compatInvalidByteRequest())
	}
	if invalidReq.Subject.Roles == nil || len(invalidReq.Subject.Roles) != 0 {
		t.Fatalf("invalid-byte request roles = %#v, want non-nil empty", invalidReq.Subject.Roles)
	}
	runeReq := recs[3].Decision.Request
	if !reflect.DeepEqual(runeReq, compatReplacementRuneRequest()) {
		t.Fatalf("replacement-rune request = %+v, want %+v", runeReq, compatReplacementRuneRequest())
	}

	// The byte-level boundary survives the round trip: the lone 0xFF is
	// still itself, not 0xFE and not a replacement rune, and the genuine
	// U+FFFD is still valid UTF-8.
	if id := invalidReq.Subject.ID; !strings.Contains(id, invalidByte) ||
		strings.Contains(id, anotherInvalidByte) || strings.Contains(id, replacementRune) {
		t.Fatalf("subject id bytes changed: %q", id)
	}
	if role := runeReq.Subject.Roles[0]; !strings.Contains(role, invalidByte) {
		t.Fatalf("role element lost its raw byte: %q", role)
	}
	if id := runeReq.Subject.ID; !utf8.ValidString(id) || !strings.Contains(id, replacementRune) ||
		strings.Contains(id, invalidByte) {
		t.Fatalf("genuine U+FFFD subject id changed: %q", id)
	}
}

// TestCompatFixtureOfflineReviewMatchesSavedDecision re-reviews the target
// decision purely from the saved material: the recomputed decision must
// agree with the preserved original on allowance, reason, matched policies
// and the version actually used.
func TestCompatFixtureOfflineReviewMatchesSavedDecision(t *testing.T) {
	archive, exp := loadCompatFixture(t)
	recs := decodeCompatFixture(t, archive, exp)

	want := Decision{
		Allowed: exp.Target.Allowed,
		Reason:  exp.Target.Reason,
		Matched: exp.Target.Matched,
		Version: exp.Target.Version,
	}
	// The saved baseline is the resource-restricted allow: pinned
	// concretely here so the fixture can never silently flip semantics.
	if !want.Allowed || want.Reason != "matched allow policy" || want.Version != 1 ||
		!reflect.DeepEqual(want.Matched, []string{"p1"}) {
		t.Fatalf("saved target decision = %+v, want allow by p1 at version 1", want)
	}

	review, err := RecheckDecisionOffline(exp.Org, recs, exp.checkpoint(), exp.TargetSeq)
	if err != nil {
		t.Fatalf("offline review of saved material: %v", err)
	}
	if !review.Consistent {
		t.Fatalf("saved decision no longer matches recomputation: original=%+v recomputed=%+v",
			review.Original, review.Recomputed)
	}
	if !reflect.DeepEqual(review.Original, want) {
		t.Fatalf("original = %+v, want saved %+v", review.Original, want)
	}
	if !reflect.DeepEqual(review.Recomputed, want) {
		t.Fatalf("recomputed = %+v, want saved %+v", review.Recomputed, want)
	}

	// The invalid-byte and replacement-rune decisions review consistently
	// too: both were denials under the same version.
	for _, seq := range []int{3, 4} {
		r, err := RecheckDecisionOffline(exp.Org, recs, exp.checkpoint(), seq)
		if err != nil {
			t.Fatalf("offline review seq %d: %v", seq, err)
		}
		if !r.Consistent || r.Recomputed.Allowed || r.Recomputed.Version != 1 {
			t.Fatalf("seq %d review = %+v, want consistent denial at version 1", seq, r)
		}
	}
}

// TestCompatFixtureByteSubstitutionRejected changes exactly one protected
// field of the decoded material — the lone 0xFF becomes 0xFE, or a genuine
// U+FFFD — while keeping every record fingerprint and the retained
// checkpoint as saved. Chain validation and offline review must both
// refuse with ErrInvalidRange, and the review must deliver no partial
// conclusion.
func TestCompatFixtureByteSubstitutionRejected(t *testing.T) {
	archive, exp := loadCompatFixture(t)
	cp := exp.checkpoint()
	recs := decodeCompatFixture(t, archive, exp)

	mutators := []struct {
		name string
		fn   func([]AuditRecord, string)
	}{
		{"subject id", func(r []AuditRecord, repl string) {
			id := &r[2].Decision.Request.Subject.ID
			*id = strings.Replace(*id, invalidByte, repl, 1)
		}},
		{"resource id", func(r []AuditRecord, repl string) {
			id := &r[2].Decision.Request.Resource.ID
			*id = strings.Replace(*id, invalidByte, repl, 1)
		}},
		{"role element", func(r []AuditRecord, repl string) {
			roles := r[3].Decision.Request.Subject.Roles
			roles[0] = strings.Replace(roles[0], invalidByte, repl, 1)
		}},
	}
	for _, repl := range []string{anotherInvalidByte, replacementRune} {
		for _, m := range mutators {
			// Deep-copy so the saved material itself is never mutated;
			// fingerprints and the checkpoint stay exactly as saved.
			bad := cloneExportedRecords(recs)
			m.fn(bad, repl)
			name := m.name + "/repl=" + replacementLabel(repl)
			if err := VerifyAudit(exp.Org, bad, cp); !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("%s: VerifyAudit err = %v, want ErrInvalidRange", name, err)
			}
			got, err := RecheckDecisionOffline(exp.Org, bad, cp, exp.TargetSeq)
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("%s: RecheckDecisionOffline err = %v, want ErrInvalidRange", name, err)
			}
			if !reflect.DeepEqual(got, OfflineDecisionReview{}) {
				t.Fatalf("%s: review delivered a partial conclusion: %+v", name, got)
			}
		}
	}
}

// TestCompatFixtureRegenerate deliberately re-creates the saved baseline
// from the current build. It is skipped in every normal test run: the
// whole point of the fixture is that it does NOT follow the current
// logic's output automatically, or a drift in generation and validation
// together would go unnoticed. Regenerate only when a format or semantics
// change is intentional, with
//
//	DARKSAFE_COMPAT_REGEN=1 go test ./darksafe/ -run TestCompatFixtureRegenerate
//
// and review the resulting testdata diff as carefully as a format change.
func TestCompatFixtureRegenerate(t *testing.T) {
	if os.Getenv("DARKSAFE_COMPAT_REGEN") != "1" {
		t.Skip("compatibility fixture is static; set DARKSAFE_COMPAT_REGEN=1 to deliberately regenerate")
	}

	s := NewStore()
	if _, err := s.Publish(compatOrg, 0, []Policy{compatResourceAllowPolicy()}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	target := s.Decide(compatOrg, compatTargetRequest())
	s.Decide(compatOrg, compatInvalidByteRequest())
	s.Decide(compatOrg, compatReplacementRuneRequest())

	recs, cp, err := s.AuditExport(compatOrg, 0)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	archive, err := EncodeAuditArchive(compatOrg, recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	exp := compatExpected{
		Org: compatOrg,
		Checkpoint: compatCheckpoint{
			EndSeq:      cp.EndSeq,
			Fingerprint: cp.Fingerprint,
		},
		TargetSeq: compatTargetSeq,
		Target: compatDecision{
			Allowed: target.Allowed,
			Reason:  target.Reason,
			Matched: target.Matched,
			Version: target.Version,
		},
	}
	for i := range recs {
		r := &recs[i]
		exp.Records = append(exp.Records, compatRecordPin{
			Seq:             r.Seq,
			Kind:            r.Kind,
			Fingerprint:     r.Fingerprint,
			PrevFingerprint: r.PrevFingerprint,
		})
	}

	dir := filepath.Join("testdata", compatFixtureDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "archive.bin"), archive, 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	raw, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, "expected.json"), raw, 0o644); err != nil {
		t.Fatalf("write expectations: %v", err)
	}
	t.Logf("regenerated %s: %d records, checkpoint fingerprint %s", dir, len(recs), cp.Fingerprint)
}
