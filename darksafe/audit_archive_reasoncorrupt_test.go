// This file pins down the decoder's handling of a corrupted decision
// *reason string length*: archives whose outer framing, declared payload
// length and checksum are all correct, but where one decision's Reason
// length word promises bytes the payload does not contain, or where that
// length word itself is only partly saved. The reason is an ordinary
// length-prefixed stringField in the wire format, so this protection is the
// generic string boundary read — but a decision reason is the field an
// auditor actually reads, and it had no regression of its own the way the
// internal list counts and the single-byte markers do.
//
// Two failure classes must stay distinct. A reason length that cannot be
// satisfied by the remaining material makes the archive unreadable: the
// read must fail with ErrInvalidArchive (matched by errors.Is), never
// panic, never provision the declared (up to 4294967295) byte count, and
// never treat the missing tail as an empty reason and continue. Flipping a
// byte inside reason *content* while leaving every length and marker legal
// instead keeps the structure readable and fails as content against the
// original fingerprints and retained checkpoint: ErrInvalidRange, never
// ErrInvalidArchive.
//
// As with the list- and marker-corruption files, every structural corruption
// below is reframed with a consistent outer length and checksum. That
// forces the observed failure to come from being unable to read the reason
// completely, not from the framing-level checksum check.
package darksafe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// locateReasonLengthWord returns the payload offset of the 4-byte length
// word that prefixes reason's content bytes. The reason content must occur
// exactly once, so the length word immediately before it is unambiguous; a
// format or fixture change that breaks this fails loudly instead of
// corrupting the wrong field.
func locateReasonLengthWord(t *testing.T, payload []byte, reason string) int {
	t.Helper()
	needle := []byte(reason)
	idx := bytes.Index(payload, needle)
	if idx < 4 {
		t.Fatalf("decision reason %q not found in payload", reason)
	}
	if bytes.Index(payload[idx+1:], needle) >= 0 {
		t.Fatalf("decision reason %q must be unique for an unambiguous locate", reason)
	}
	return idx - 4
}

// reasonChainFixture builds complete, valid material: one policy publish
// followed by one decision carrying the unique, non-empty laterReason. It
// returns the encoded archive, the caller's retained checkpoint and the
// reason text the tests corrupt.
func reasonChainFixture(t *testing.T, laterReason string) ([]byte, Checkpoint) {
	t.Helper()
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  1,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u1", "r1", "org/a", "read"),
				Decision: Decision{Allowed: true, Reason: laterReason, Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode reason fixture: %v", err)
	}
	return archive, cp
}

// TestArchiveDecisionReasonLengthCorruption covers the shapes that make a
// decision reason unreadable while the outer length and checksum still
// hold: a declared reason length larger than the bytes left (by one and by
// a wide margin), the maximum uint32 length on a body that could never hold
// it, and a length word physically cut off partway. Each must fail as
// ErrInvalidArchive with nil records — not ErrInvalidRange, not a crash,
// not a phantom allocation, and not an empty-reason continuation — and must
// leave both the archive bytes and the retained checkpoint untouched.
func TestArchiveDecisionReasonLengthCorruption(t *testing.T) {
	reason := "the-decision-reason-b7d2"
	good, cp := reasonChainFixture(t, reason)
	basePayload := payloadOf(t, good)
	lenAt := locateReasonLengthWord(t, basePayload, reason)
	if got := binary.BigEndian.Uint32(basePayload[lenAt:]); got != uint32(len(reason)) {
		t.Fatalf("located length word = %d, want %d", got, len(reason))
	}

	// expectInvalid runs one payload mutation, reframes it with a consistent
	// outer length and checksum, and asserts the whole read fails as an
	// unreadable archive without delivering records or rewriting inputs.
	expectInvalid := func(name, wantSubstr string, mutate func(p []byte) []byte) {
		t.Helper()
		payload := mutate(append([]byte(nil), basePayload...))
		data := framePayload(payload)
		dataSnapshot := append([]byte(nil), data...)
		cpSnapshot := cp

		got, err := DecodeAuditArchive(data, "acme", cp)
		if !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("%s: err = %v, want ErrInvalidArchive", name, err)
		}
		if errors.Is(err, ErrInvalidRange) {
			t.Fatalf("%s: an unreadable reason counted as a content error: %v", name, err)
		}
		if got != nil {
			t.Fatalf("%s: returned %d records on a damaged reason, want nil", name, len(got))
		}
		if wantSubstr != "" && !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("%s: err = %q, want substring %q", name, err.Error(), wantSubstr)
		}
		if !bytes.Equal(data, dataSnapshot) {
			t.Fatalf("%s: failed decode rewrote the archive bytes", name)
		}
		if cp != cpSnapshot {
			t.Fatalf("%s: failed decode rewrote the retained checkpoint", name)
		}
	}

	// Bytes left after the reason length word, which a complete reason plus
	// the matched list, version and two fingerprints would occupy.
	remaining := len(basePayload) - (lenAt + 4)

	// The length word is intact but promises more reason bytes than remain.
	// The payload itself keeps its length, so the only thing wrong is that
	// the reason cannot be read in full: the error must originate at that
	// string boundary, not at the (recomputed, valid) checksum.
	expectInvalid("reason length exceeds remaining by one", "truncated archive payload",
		func(p []byte) []byte {
			binary.BigEndian.PutUint32(p[lenAt:], uint32(remaining+1))
			return p
		})
	expectInvalid("reason length far exceeds remaining", "truncated archive payload",
		func(p []byte) []byte {
			binary.BigEndian.PutUint32(p[lenAt:], uint32(remaining)+1_000_000)
			return p
		})

	// 4294967295 reason bytes on this small body must be rejected on the
	// remaining-byte bound, without enumerating or provisioning the phantom
	// bytes. Measure the error path: a regression that trusted the length
	// would try to allocate gigabytes here.
	measureHuge := func(name string, mutate func(p []byte) []byte) {
		t.Helper()
		payload := mutate(append([]byte(nil), basePayload...))
		data := framePayload(payload)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		got, err := DecodeAuditArchive(data, "acme", cp)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrInvalidArchive) || got != nil {
			t.Fatalf("%s: records=%v err=%v, want ErrInvalidArchive, nil", name, got, err)
		}
		if !strings.Contains(err.Error(), "truncated archive payload") {
			t.Fatalf("%s: err = %q, want the reason read-boundary rejection", name, err.Error())
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
			t.Fatalf("%s allocated %d bytes, want a small error path", name, alloc)
		}
	}
	measureHuge("max uint32 reason length on small body",
		func(p []byte) []byte {
			binary.BigEndian.PutUint32(p[lenAt:], 0xffffffff)
			return p
		})

	// The length word is only partly saved: cut the payload 0..3 bytes into
	// the word (and beyond, for the fully-missing end) and reframe. The
	// reader cannot even learn the declared length, so each cut must be a
	// truncation error rather than a zero/empty reason.
	for _, cutIntoWord := range []int{0, 1, 2, 3} {
		name := "reason length word cut with " + string(rune('0'+cutIntoWord)) + " of 4 bytes present"
		expectInvalid(name, "truncated archive payload",
			func(p []byte) []byte { return p[:lenAt+cutIntoWord] })
	}
}

// TestArchiveTinyReasonHugeLengthNoAllocation is the dedicated emphasis of
// the allocation requirement: a hand-built, tiny archive that reaches a
// decision and then declares its reason to be 4294967295 bytes long with no
// bytes following. Decode must return ErrInvalidArchive promptly, without
// crashing or provisioning as if the declared length were real.
func TestArchiveTinyReasonHugeLengthNoAllocation(t *testing.T) {
	w := &archiveWriter{}
	w.stringField("acme")
	w.u32(1)
	w.stringField("g")
	w.u32(1) // one record
	w.stringField("acme")
	w.u32(1) // sequence
	w.stringField(AuditDecision)
	w.raw(nilMarker())               // change absent
	w.raw([]byte{archiveTagPresent}) // decision present
	w.stringField("acme")            // request SubjectOrg
	w.stringField("acme")            // request ResourceOrg
	w.stringField("u1")              // subject ID
	w.stringField("user")            // subject kind
	w.raw(nilMarker())               // roles nil
	w.boolean(false)                 // subject disabled
	w.stringField("r1")              // resource ID
	w.stringField("org/a")           // resource scope
	w.stringField("read")            // action
	w.boolean(true)                  // decision allowed
	w.u32(0xffffffff)                // decision reason length: 4294967295, no bytes follow
	if w.err != nil {
		t.Fatal(w.err)
	}
	// The record-count floor (22 bytes per record) must still pass, proving
	// the reader actually reaches the reason length rather than rejecting the
	// record count first.
	if len(w.buf) < 22 {
		t.Fatalf("tiny body %d bytes too short to reach the reason field", len(w.buf))
	}
	data := framePayload(w.buf)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := DecodeAuditArchive(data, "acme",
		Checkpoint{Org: "acme", EndSeq: 1, Fingerprint: "g"})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("tiny archive with huge reason length: records=%v err=%v, want ErrInvalidArchive, nil", got, err)
	}
	if !strings.Contains(err.Error(), "truncated archive payload") {
		t.Fatalf("tiny archive err = %q, want the reason read-boundary rejection", err.Error())
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("huge declared reason length allocated %d bytes, want a small error path", alloc)
	}
}

// TestArchiveLaterRecordReasonDamageDiscardsPrefix is the all-or-nothing
// boundary the requirement names: a complete policy publish and a normal,
// reviewable decision precede a later decision whose reason length is
// corrupt. The auditor's review target sits entirely in the intact part.
// The whole read must still fail and must never first hand back the intact
// records, so the earlier target cannot be reviewed out of the damaged
// material.
func TestArchiveLaterRecordReasonDamageDiscardsPrefix(t *testing.T) {
	targetReason := "matched allow policy"
	laterReason := "later-decision-reason-5e81"
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  1,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			// The decision the auditor intends to review, before the damage.
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u1", "r1", "org/a", "read"),
				Decision: Decision{Allowed: true, Reason: targetReason, Matched: []string{"p1"}, Version: 1},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u2", "r1", "org/a", "read"),
				Decision: Decision{Allowed: false, Reason: laterReason, Version: 1},
			}
		},
	})
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// The intact prefix (publish + review target) really is usable on its
	// own, so a nil result for the full archive can only mean it was
	// deliberately withheld rather than merely unread.
	prefixCP := Checkpoint{Org: "acme", EndSeq: 2, Fingerprint: recs[1].Fingerprint}
	prefixArchive, err := EncodeAuditArchive("acme", recs[:2], prefixCP)
	if err != nil {
		t.Fatalf("encode prefix: %v", err)
	}
	prefix, err := DecodeAuditArchive(prefixArchive, "acme", prefixCP)
	if err != nil || len(prefix) != 2 {
		t.Fatalf("intact prefix must decode: %d records, %v", len(prefix), err)
	}
	if review, err := RecheckDecisionOffline("acme", prefix, prefixCP, 2); err != nil || !review.Consistent {
		t.Fatalf("intact prefix target must review: %+v, %v", review, err)
	}

	basePayload := payloadOf(t, good)
	lenAt := locateReasonLengthWord(t, basePayload, laterReason)
	remaining := len(basePayload) - (lenAt + 4)

	corrupt := func(name string, mutate func(p []byte) []byte) {
		t.Helper()
		payload := mutate(append([]byte(nil), basePayload...))
		data := framePayload(payload)
		dataSnapshot := append([]byte(nil), data...)
		cpSnapshot := cp

		got, err := DecodeAuditArchive(data, "acme", cp)
		if !errors.Is(err, ErrInvalidArchive) || got != nil {
			t.Fatalf("%s: records=%v err=%v, want ErrInvalidArchive, nil", name, got, err)
		}
		if !bytes.Equal(data, dataSnapshot) || cp != cpSnapshot {
			t.Fatalf("%s: failed decode rewrote its inputs", name)
		}
	}
	// Both a one-byte overrun and the maximum declared length in the later
	// record sink the entire read, target record included.
	corrupt("later reason exceeds remaining by one", func(p []byte) []byte {
		binary.BigEndian.PutUint32(p[lenAt:], uint32(remaining+1))
		return p
	})
	corrupt("later reason declares max uint32", func(p []byte) []byte {
		binary.BigEndian.PutUint32(p[lenAt:], 0xffffffff)
		return p
	})
	corrupt("later reason length word cut mid-read", func(p []byte) []byte {
		return p[:lenAt+2]
	})
}

// TestArchiveReasonContentChangeIsNotArchiveDamage keeps format damage
// separate from untrusted content at the reason field: flip one byte inside
// the reason text, past its length word, leaving the declared length, every
// marker and the whole structure legal, and recompute the outer checksum.
// The archive is fully readable, so against the unchanged original
// fingerprints and independently retained checkpoint it must fail as
// ErrInvalidRange — not be swept into ErrInvalidArchive.
func TestArchiveReasonContentChangeIsNotArchiveDamage(t *testing.T) {
	reason := "the-decision-reason-b7d2"
	good, cp := reasonChainFixture(t, reason)

	tampered := reframe(t, good, func(payload []byte) []byte {
		at := locateReasonLengthWord(t, payload, reason) + 4 // first content byte
		payload[at] ^= 0x01                                  // same length, length word untouched
		return payload
	})
	got, err := DecodeAuditArchive(tampered, "acme", cp)
	if !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("readable-format reason content change: records=%v err=%v, want ErrInvalidRange, nil", got, err)
	}
	if errors.Is(err, ErrInvalidArchive) {
		t.Fatal("a legal-length reason byte flip must not be reported as archive-format damage")
	}
}

// TestArchiveValidReasonsRoundTrip locks the legal behavior the corruption
// guard must not disturb: reasons are restored from their exact bytes with
// length understood in bytes. Chinese, spaces, control characters, a lone
// non-UTF-8 byte and a genuine U+FFFD all survive untruncated and
// unnormalized, an empty reason stays the empty string (distinct from a
// missing field), and the material with its retained checkpoint still
// verifies and feeds offline review.
func TestArchiveValidReasonsRoundTrip(t *testing.T) {
	rawReason := " 理由 中\t\n\r\x00\x01 " + invalidByte
	emptyReason := ""
	genuineReplacement := "理" + replacementRune + "由"

	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  1,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		// A canonical allow decision: the reviewable control.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u1", "r1", "org/a", "read"),
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
			}
		},
		// Spaces, Chinese, control characters and a lone non-UTF-8 byte.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u3", "r1", "org/a", "read"),
				Decision: Decision{Allowed: false, Reason: rawReason, Version: 1},
			}
		},
		// An explicitly empty reason.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u4", "r1", "org/a", "read"),
				Decision: Decision{Allowed: false, Reason: emptyReason, Version: 1},
			}
		},
		// A genuine U+FFFD must stay distinct from the lone 0xFF above.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u5", "r1", "org/a", "read"),
				Decision: Decision{Allowed: false, Reason: genuineReplacement, Version: 1},
			}
		},
	})

	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode valid reasons: %v", err)
	}
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("decode valid reasons: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("valid reasons changed across the archive:\n got %+v\nwant %+v", got, recs)
	}

	// Byte-for-byte reason assertions; length is understood in bytes.
	wantReasons := []string{"matched allow policy", rawReason, emptyReason, genuineReplacement}
	for i, want := range wantReasons {
		if gotReason := got[i+1].Decision.Decision.Reason; gotReason != want {
			t.Fatalf("record %d reason = %q, want %q", i+2, gotReason, want)
		}
	}
	if got[3].Decision.Decision.Reason != "" {
		t.Fatal("empty reason must decode as the empty string, not absent or replaced")
	}
	// The raw-byte reason and the genuine-U+FFFD reason must not collapse.
	if got[2].Decision.Decision.Reason == got[4].Decision.Decision.Reason {
		t.Fatal("lone 0xFF reason and genuine U+FFFD reason collapsed together")
	}
	if !strings.Contains(got[2].Decision.Decision.Reason, invalidByte) {
		t.Fatal("non-UTF-8 reason byte was replaced or normalized")
	}

	// Fingerprints/checkpoint unchanged and the material still verifies.
	for i := range got {
		if got[i].Fingerprint != recs[i].Fingerprint {
			t.Fatalf("record %d fingerprint changed", i+1)
		}
	}
	if err := VerifyAudit("acme", got, cp); err != nil {
		t.Fatalf("decoded valid-reason material must verify: %v", err)
	}
	// The canonical decision in the same material remains offline-reviewable.
	review, err := RecheckDecisionOffline("acme", got, cp, 2)
	if err != nil || !review.Consistent || review.Original.Reason != "matched allow policy" {
		t.Fatalf("offline review after reason round trip = %+v, %v", review, err)
	}
}
