package darksafe

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// This file covers the decode-then-review entry points added so one
// `darksafe review` invocation validates the complete audit chain exactly
// once: DecodeVerifiedAuditArchive returns an already validated
// VerifiedAuditArchive, and its RecheckDecisionOffline method replays the
// target decision without a second whole-chain walk. The standalone
// DecodeAuditArchive / RecheckDecisionOffline functions keep validating raw
// material themselves; these tests pin both shapes, plus the guarantees
// that a verified archive cannot be mutated into certifying different
// bytes and that a fresh decode always re-validates.

// verifiedReviewArchive builds, encodes and decodes a complete export for
// org and returns its already validated form together with the retained
// checkpoint.
func verifiedReviewArchive(t *testing.T, org string, recs []AuditRecord, cp Checkpoint) *VerifiedAuditArchive {
	t.Helper()
	archive, err := EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatalf("encode archive: %v", err)
	}
	got, err := DecodeVerifiedAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatalf("decode verified archive: %v", err)
	}
	return got
}

// TestVerifiedArchiveReviewsTargetWithoutRevalidation walks the happy path
// of the single-validation flow: the returned archive carries the org and
// retained checkpoint, its records are the complete export, and reviewing
// the target replays the recorded version.
func TestVerifiedArchiveReviewsTargetWithoutRevalidation(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq1
	original := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))      // seq2
	s.Publish("acme", 1, nil)                                                       // seq3, later publish
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))                  // seq4
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	got := verifiedReviewArchive(t, "acme", recs, cp)
	if got.Org != "acme" {
		t.Fatalf("Org = %q, want acme", got.Org)
	}
	if !reflect.DeepEqual(got.Checkpoint, cp) {
		t.Fatalf("Checkpoint = %+v, want %+v", got.Checkpoint, cp)
	}
	if len(got.records) != len(recs) {
		t.Fatalf("validated records = %d, want %d", len(got.records), len(recs))
	}

	review, err := got.RecheckDecisionOffline(2)
	if err != nil {
		t.Fatalf("review on validated archive: %v", err)
	}
	if !review.Consistent || !reflect.DeepEqual(review.Original, original) ||
		!reflect.DeepEqual(review.Recomputed, original) {
		t.Fatalf("validated review = %+v, want consistent with %+v", review, original)
	}
	if review.Seq != 2 || review.Recomputed.Version != 1 {
		t.Fatalf("review = %+v, want seq 2 replayed at version 1", review)
	}
}

// TestVerifiedArchiveReviewKeepsTargetFailures proves that skipping the
// second chain check removes none of the target-level failure reasons:
// non-positive and absent sequences stay ErrAuditNotFound, a policy change
// target stays ErrAuditNotADecision, and a used version the material does
// not carry before the target stays ErrVersionNotFound.
func TestVerifiedArchiveReviewKeepsTargetFailures(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                  // seq2
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := verifiedReviewArchive(t, "acme", recs, cp)

	for _, seq := range []int{0, -1} {
		if _, err := got.RecheckDecisionOffline(seq); !errors.Is(err, ErrAuditNotFound) {
			t.Fatalf("non-positive seq %d err = %v", seq, err)
		}
	}
	if _, err := got.RecheckDecisionOffline(99); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("absent seq err = %v", err)
	}
	if _, err := got.RecheckDecisionOffline(1); !errors.Is(err, ErrAuditNotADecision) {
		t.Fatalf("policy change target err = %v", err)
	}

	// An empty, genesis-validated archive reviews nothing.
	empty, emptyCP, err := s.AuditExport("ghost", 0)
	if err != nil {
		t.Fatal(err)
	}
	emptyArchive := verifiedReviewArchive(t, "ghost", empty, emptyCP)
	if _, err := emptyArchive.RecheckDecisionOffline(1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("empty validated archive review err = %v", err)
	}

	// A chain VerifyAudit accepts but whose decision names a version carried
	// only after it: the single validation passes the decode, and the review
	// still refuses with ErrVersionNotFound rather than substituting.
	req := request("acme", "u1", "r1", "org/a", "read")
	laterRecs, laterCP := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 2},
			}
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 2, Policies: []Policy{
				allowPolicy("p1", "u1", "read", "org/a", false),
			}}, nil
		},
	})
	later := verifiedReviewArchive(t, "acme", laterRecs, laterCP)
	if _, err := later.RecheckDecisionOffline(1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("later-only version on validated archive err = %v", err)
	}
}

// TestVerifiedArchiveReviewsInconsistencyAsResult reproduces the
// legitimate-but-inconsistent case through the single-validation path:
// both decisions come back, Consistent is false, and no error is raised.
func TestVerifiedArchiveReviewsInconsistencyAsResult(t *testing.T) {
	req := request("acme", "u1", "r1", "org/a", "read")
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 1, Policies: []Policy{
				allowPolicy("p1", "u1", "read", "org/a", false),
			}}, nil
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: false, Reason: "matched deny policy", Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	got := verifiedReviewArchive(t, "acme", recs, cp)
	review, err := got.RecheckDecisionOffline(2)
	if err != nil {
		t.Fatalf("inconsistency is a result, not an error: %v", err)
	}
	if review.Consistent {
		t.Fatal("legitimate material with a disagreeing record must be inconsistent")
	}
	if review.Original.Allowed || review.Original.Reason != "matched deny policy" {
		t.Fatalf("original not preserved: %+v", review.Original)
	}
	if !review.Recomputed.Allowed || review.Recomputed.Reason != "matched allow policy" {
		t.Fatalf("recomputed wrong: %+v", review.Recomputed)
	}
}

// TestDecodeVerifiedRejectsEveryMaterialFailure ensures the decode end of
// the single-validation path keeps the same error taxonomy and never
// returns a value on failure: empty org is ErrMissingOrganization,
// framing/checksum damage is ErrInvalidArchive, and chain/checkpoint
// damage — including a record AFTER the review target — is
// ErrInvalidRange.
func TestDecodeVerifiedRejectsEveryMaterialFailure(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}) // seq1
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                  // seq2 target
	s.Publish("acme", 1, nil)                                                       // seq3
	s.Decide("acme", request("acme", "u2", "r1", "org/a", "read"))                  // seq4
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := DecodeVerifiedAuditArchive(good, "", cp); !errors.Is(err, ErrMissingOrganization) || got != nil {
		t.Fatalf("empty org err = %v, got = %v", err, got)
	}
	if got, err := DecodeVerifiedAuditArchive([]byte("not an archive"), "acme", cp); !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("garbage err = %v, got = %v", err, got)
	}
	checksumBad := append([]byte(nil), good...)
	checksumBad[len(checksumBad)-1] ^= 0xFF
	if got, err := DecodeVerifiedAuditArchive(checksumBad, "acme", cp); !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("bad checksum err = %v, got = %v", err, got)
	}
	wrongFP := Checkpoint{Org: "acme", EndSeq: cp.EndSeq, Fingerprint: cp.Fingerprint[:len(cp.Fingerprint)-1] + "0"}
	if got, err := DecodeVerifiedAuditArchive(good, "acme", wrongFP); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("wrong checkpoint err = %v, got = %v", err, got)
	}
	if got, err := DecodeVerifiedAuditArchive(good, "globex", cp); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("foreign org err = %v, got = %v", err, got)
	}

	// Tamper a record AFTER the target (seq 2) and rebuild a valid checksum,
	// so framing passes and only the whole-chain check can catch it.
	tamperedPayload := reframeVerifiedPayload(t, good, func(p []byte) {
		idx := indexVerifiedBytes(t, p, []byte("u2"))
		p[idx] ^= 0x01
	})
	if got, err := DecodeVerifiedAuditArchive(tamperedPayload, "acme", cp); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("later-record tamper err = %v, got = %v", err, got)
	}
}

// TestVerifiedArchiveReviewIsDetachedAndReadOnly proves the single
// validation flow neither mutates the underlying records nor shares the
// returned decisions' matched lists with them.
func TestVerifiedArchiveReviewIsDetachedAndReadOnly(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := verifiedReviewArchive(t, "acme", recs, cp)
	snapshot := append([]AuditRecord(nil), got.records...)

	review, err := got.RecheckDecisionOffline(2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.records, snapshot) {
		t.Fatal("review mutated the validated records")
	}
	// Mutating the validated material in place cannot reach a decision the
	// review already handed out.
	got.records[1].Decision.Decision.Matched[0] = "forged"
	got.records[1].Decision.Request.Subject.ID = "hacker"
	if !reflect.DeepEqual(review.Original.Matched, []string{"p1"}) {
		t.Fatalf("original aliases validated material: %+v", review.Original)
	}
	if !reflect.DeepEqual(review.Recomputed.Matched, []string{"p1"}) {
		t.Fatalf("recomputed aliases validated material: %+v", review.Recomputed)
	}
}

// TestDecodeVerifiedRevalidatesPerCall locks in "no reused verdict":
// mutating the material or swapping the checkpoint is only reachable by a
// fresh decode, which re-validates and rejects. A value obtained earlier
// never certifies the changed bytes.
func TestDecodeVerifiedRevalidatesPerCall(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	first, err := DecodeVerifiedAuditArchive(good, "acme", cp)
	if err != nil {
		t.Fatalf("first decode: %v", err)
	}

	// A second decode with a different checkpoint cannot ride on the first
	// call's verdict.
	other := Checkpoint{Org: "acme", EndSeq: 0, Fingerprint: genesisFingerprint("acme")}
	if got, err := DecodeVerifiedAuditArchive(good, "acme", other); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("swapped checkpoint must re-validate and fail, err = %v got = %v", err, got)
	}

	// The first value stays valid and reviews its own material unaffected.
	review, err := first.RecheckDecisionOffline(2)
	if err != nil || !review.Consistent {
		t.Fatalf("earlier verified archive must stay valid for its own material: %+v %v", review, err)
	}
}

// TestStandaloneEntryPointsStillSelfValidate confirms the individually
// called functions keep their original guarantees on raw material:
// DecodeAuditArchive returns plain records, RecheckDecisionOffline still
// chain-validates them itself, and the two stay independently usable.
func TestStandaloneEntryPointsStillSelfValidate(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("DecodeAuditArchive: %v", err)
	}
	// The standalone recheck receives raw material and must validate it
	// itself: tamper after decoding and expect the full chain check to fire.
	decoded[1].Decision.Request.Subject.ID = "hacker"
	if _, err := RecheckDecisionOffline("acme", decoded, cp, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("standalone recheck must self-validate raw records, err = %v", err)
	}
}

// reframeVerifiedPayload rebuilds one archive frame after mutate edits the
// payload, with a consistent declared length and checksum, so a test can
// alter content without tripping framing/checksum and reach chain
// validation.
func reframeVerifiedPayload(t *testing.T, data []byte, mutate func(payload []byte)) []byte {
	t.Helper()
	magicLen := len(archiveMagic)
	n := int(binary.BigEndian.Uint32(data[magicLen : magicLen+4]))
	payload := append([]byte(nil), data[magicLen+4:magicLen+4+n]...)
	mutate(payload)
	out := append([]byte(nil), archiveMagic...)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	out = append(out, lenBuf[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	return append(out, sum[:]...)
}

// indexVerifiedBytes fails the test when needle is absent.
func indexVerifiedBytes(t *testing.T, haystack, needle []byte) int {
	t.Helper()
	idx := bytes.Index(haystack, needle)
	if idx < 0 {
		t.Fatalf("byte marker %q not found in payload", needle)
	}
	return idx
}
