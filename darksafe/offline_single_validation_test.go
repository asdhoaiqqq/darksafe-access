package darksafe

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// This file pins the single-validation shape of the offline review flow.
// Reading an archive used to run the complete VerifyAudit chain check once
// inside DecodeAuditArchive and a second time inside
// RecheckDecisionOffline; a review invocation now validates the whole
// chain exactly once and replays the target from that already-verified
// material. The standalone entry points still validate their own inputs in
// full, so other programs calling either one directly keep every prior
// guarantee.
//
// verifyAuditObserver counts every complete chain validation performed by
// the package while it is installed.

// countVerifications installs the chain-validation observer and returns the
// count and a cleanup that restores the previous observer (nil in normal
// use).
func countVerifications(t *testing.T) (count func() int, cleanup func()) {
	t.Helper()
	n := 0
	restore := SetVerifyAuditObserverForTesting(func() { n++ })
	return func() int { return n }, restore
}

// singleValidationChain builds the standard four-record fixture
// (publish v1, allow decision, publish v2, default-deny decision), archives
// it and returns archive bytes and the separately retained checkpoint.
func singleValidationChain(t *testing.T, org string) ([]byte, Checkpoint) {
	t.Helper()
	s := NewStore()
	s.Publish(org, 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide(org, request(org, "u1", "r1", "org/a", "read"))
	s.Publish(org, 1, nil)
	s.Decide(org, request(org, "u2", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	return archive, cp
}

// TestReviewFlowValidatesChainOnce pins the core requirement: the
// combined archive flow (exactly the stages `darksafe review` runs)
// performs one complete chain validation per invocation, in both text and
// --json modes.
func TestReviewFlowValidatesChainOnce(t *testing.T) {
	const org = "acme"
	archive, cp := singleValidationChain(t, org)

	count, cleanup := countVerifications(t)
	defer cleanup()

	material, err := DecodeVerifiedAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := count(); got != 1 {
		t.Fatalf("decode validated the chain %d times, want 1", got)
	}
	review, err := RecheckVerifiedDecisionOffline(material, 2)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !review.Consistent || !review.Recomputed.Allowed || review.Recomputed.Version != 1 {
		t.Fatalf("unexpected review: %+v", review)
	}
	if got := count(); got != 1 {
		t.Fatalf("review after decode triggered a %dth chain validation; want exactly 1 total", got)
	}

	// Reviewing a second target from the same validated material runs no
	// further validation either: the validation belongs to the material of
	// this invocation, not to a cache from an earlier one.
	other, err := RecheckVerifiedDecisionOffline(material, 4)
	if err != nil {
		t.Fatalf("second review: %v", err)
	}
	if !other.Consistent || other.Recomputed.Version != 2 {
		t.Fatalf("second target review = %+v", other)
	}
	if got := count(); got != 1 {
		t.Fatalf("second target added a chain validation: %d total, want 1", got)
	}
}

// TestReviewFlowFailureStillValidatesOnce ensures a rejected invocation
// also walks exactly one validation — the failure must not be discovered
// twice, and no review stage re-checks the chain.
func TestReviewFlowFailureStillValidatesOnce(t *testing.T) {
	const org = "acme"
	archive, cp := singleValidationChain(t, org)

	t.Run("archive tampered after the target", func(t *testing.T) {
		tampered := reframeSinglePayload(t, archive, func(payload []byte) {
			idx := lastIndexBytes(t, payload, []byte("u2"))
			payload[idx] ^= 0x01 // record 4 sits after the reviewed seq 2
		})
		count, cleanup := countVerifications(t)
		defer cleanup()

		material, err := DecodeVerifiedAuditArchive(tampered, org, cp)
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("tamper err = %v, want ErrInvalidRange", err)
		}
		if !reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
			t.Fatalf("failed decode delivered material: %+v", material)
		}
		if got := count(); got != 1 {
			t.Fatalf("rejected decode validated %d times, want 1", got)
		}
	})

	t.Run("target unavailable after one validation", func(t *testing.T) {
		count, cleanup := countVerifications(t)
		defer cleanup()

		material, err := DecodeVerifiedAuditArchive(archive, org, cp)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, err := RecheckVerifiedDecisionOffline(material, 99); !errors.Is(err, ErrAuditNotFound) {
			t.Fatalf("missing target err = %v", err)
		}
		if _, err := RecheckVerifiedDecisionOffline(material, 1); !errors.Is(err, ErrAuditNotADecision) {
			t.Fatalf("policy-change target err = %v", err)
		}
		if got := count(); got != 1 {
			t.Fatalf("target failures triggered %d validations, want 1", got)
		}
	})
}

// TestStandaloneEntriesEachValidateTheChain pins the guarantees for other
// programs calling the documented entry points on their own: each entry
// accepts unvalidated material and performs the complete check itself, and
// nothing requires running another entry point first.
func TestStandaloneEntriesEachValidateTheChain(t *testing.T) {
	const org = "acme"
	archive, cp := singleValidationChain(t, org)
	recs, err := DecodeAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("DecodeAuditArchive validates alone", func(t *testing.T) {
		count, cleanup := countVerifications(t)
		defer cleanup()
		got, err := DecodeAuditArchive(archive, org, cp)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 4 {
			t.Fatalf("records = %d, want 4", len(got))
		}
		if got := count(); got != 1 {
			t.Fatalf("standalone decode validated %d times, want 1", got)
		}
	})

	t.Run("RecheckDecisionOffline validates alone", func(t *testing.T) {
		count, cleanup := countVerifications(t)
		defer cleanup()
		review, err := RecheckDecisionOffline(org, recs, cp, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !review.Consistent {
			t.Fatalf("review = %+v", review)
		}
		if got := count(); got != 1 {
			t.Fatalf("standalone review validated %d times, want 1", got)
		}
	})

	t.Run("legacy composition is where the duplicate lived", func(t *testing.T) {
		// Calling the two standalone entry points in sequence validates
		// twice by construction: each standalone keeps its own guarantee.
		// The command avoids this by using the verified-material pair.
		count, cleanup := countVerifications(t)
		defer cleanup()
		loaded, err := DecodeAuditArchive(archive, org, cp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RecheckDecisionOffline(org, loaded, cp, 2); err != nil {
			t.Fatal(err)
		}
		if got := count(); got != 2 {
			t.Fatalf("standalone composition validated %d times, want 2", got)
		}
	})
}

// TestStandaloneEntriesRejectFreshMaterialAndCheckpoint ensures no
// validation conclusion survives across calls: altered material or a
// changed checkpoint is always re-checked by the call that receives it.
func TestStandaloneEntriesRejectFreshMaterialAndCheckpoint(t *testing.T) {
	const org = "acme"
	archive, cp := singleValidationChain(t, org)

	// A first good decode must not bless a second, tampered read.
	tampered := reframeSinglePayload(t, archive, func(payload []byte) {
		payload[len(payload)-1] ^= 0x01 // late content, after seq 2
	})
	if _, err := DecodeAuditArchive(archive, org, cp); err != nil {
		t.Fatalf("control decode: %v", err)
	}
	if _, err := DecodeAuditArchive(tampered, org, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tampered material reused earlier validation: %v", err)
	}

	// A changed checkpoint after a good call fails the next call.
	wrongCP := cp
	wrongCP.Fingerprint = cp.Fingerprint[:len(cp.Fingerprint)-1] + "0"
	if _, err := DecodeAuditArchive(archive, org, wrongCP); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("changed checkpoint reused earlier validation: %v", err)
	}

	// The verified-material flow is likewise per-invocation: a different
	// checkpoint forces a fresh validation and rejects.
	if _, err := DecodeVerifiedAuditArchive(archive, org, wrongCP); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("verified decode with changed checkpoint: %v", err)
	}
}

// TestVerifiedFlowMatchesStandaloneReviewResults ensures the shared
// single-validation flow produces exactly the review and errors the
// standalone flow did, including detachment of the returned decisions.
func TestVerifiedFlowMatchesStandaloneReviewResults(t *testing.T) {
	const org = "acme"
	archive, cp := singleValidationChain(t, org)

	material, err := DecodeVerifiedAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatal(err)
	}
	combined, err := RecheckVerifiedDecisionOffline(material, 2)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := DecodeAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := RecheckDecisionOffline(org, recs, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(combined, standalone) {
		t.Fatalf("combined %+v != standalone %+v", combined, standalone)
	}

	// Detachment: mutating the material after the review must not reach
	// either returned decision list.
	material.records[1].Decision.Decision.Matched[0] = "forged"
	if !reflect.DeepEqual(combined.Original.Matched, []string{"p1"}) ||
		!reflect.DeepEqual(combined.Recomputed.Matched, []string{"p1"}) {
		t.Fatalf("combined review aliases material: %+v", combined)
	}
}

// TestVerifiedFlowEmptyOrganizationCountsNoValidation pins that the
// missing-organization guard precedes every chain check in the combined
// decode.
func TestVerifiedFlowEmptyOrganizationCountsNoValidation(t *testing.T) {
	archive, _ := singleValidationChain(t, "acme")
	count, cleanup := countVerifications(t)
	defer cleanup()
	_, err := DecodeVerifiedAuditArchive(archive, "", Checkpoint{})
	if !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("err = %v, want ErrMissingOrganization", err)
	}
	if got := count(); got != 0 {
		t.Fatalf("empty org triggered %d chain validations, want 0", got)
	}
	_, err = RecheckDecisionOffline("", nil, Checkpoint{}, 1)
	if !errors.Is(err, ErrMissingOrganization) {
		t.Fatalf("standalone empty org err = %v", err)
	}
}

// reframeSinglePayload rebuilds one archive frame after mutate edits its
// payload, so content tampering reaches chain validation with a valid
// checksum.
func reframeSinglePayload(t *testing.T, data []byte, mutate func(payload []byte)) []byte {
	t.Helper()
	magicLen := len(archiveMagic)
	n := int(binary.BigEndian.Uint32(data[magicLen : magicLen+4]))
	payload := append([]byte(nil), data[magicLen+4:magicLen+4+n]...)
	mutate(payload)
	out := append([]byte(nil), archiveMagic...)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)))
	out = append(out, lb[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	return append(out, sum[:]...)
}

func lastIndexBytes(t *testing.T, haystack, needle []byte) int {
	t.Helper()
	idx := -1
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("%q not found in payload", needle)
	}
	return idx
}
