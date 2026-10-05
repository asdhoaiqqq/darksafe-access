// This file pins down the decoder's handling of a corrupted *decision
// reason string length*: archives whose outer framing, declared payload
// length and checksum are all correct, but where one decision's Reason
// declares more bytes than the remaining material holds, or whose length
// word itself is only partially saved. The reason is one stringField among
// many, but it is the field an auditor reads to understand a decision, so
// its length corruption gets its own regression guard rather than relying
// on the generic string coverage alone.
//
// Every corruption below is reframed with a consistent outer length and
// checksum, so exercising these cases proves the failure comes from reading
// the reason's own length information — never from the outer checksum, and
// never as a content mismatch: an unreadable reason is ErrInvalidArchive
// (matched by errors.Is) with nil records, never a crash, never a hostile
// allocation sized by the declared length, and never a silent empty reason
// that lets the read continue. A reason whose *content* changed while every
// length and marker stays legal is the opposite case and keeps its existing
// classification: ErrInvalidRange.
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

// laterReason is the unique reason of the last record in the corruption
// fixture, distinctive enough to locate unambiguously in the payload bytes.
const laterReason = "third-decision-reason-9c4e"

// reasonCorruptionFixture builds complete, valid material whose damage site
// sits late: a policy publish (seq 1), a normal decision an auditor would
// review (seq 2), and a further decision whose reason is the corruption
// target (seq 3). The intact archive must decode and review seq 2 cleanly.
func reasonCorruptionFixture(t *testing.T) ([]byte, []AuditRecord, Checkpoint) {
	t.Helper()
	req := request("acme", "u1", "r1", "org/a", "read")
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version: 1, Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: laterReason, Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	review, err := RecheckDecisionOffline("acme", got, cp, 2)
	if err != nil || !review.Consistent {
		t.Fatalf("fixture review of the seq-2 target = %+v, %v", review, err)
	}
	return archive, recs, cp
}

// reasonLengthAt locates the four-byte length word of one unique reason
// string inside a payload, and proves the word really holds the reason's
// byte length — so a test that rewrites it knows it is corrupting the
// reason's own length information and not a neighbouring field.
func reasonLengthAt(t *testing.T, payload []byte, reason string) int {
	t.Helper()
	idx := bytes.Index(payload, []byte(reason))
	if idx < 0 {
		t.Fatalf("reason %q not found in payload", reason)
	}
	if bytes.Index(payload[idx+1:], []byte(reason)) >= 0 {
		t.Fatalf("reason %q must be unique for an unambiguous locate", reason)
	}
	if idx < 4 {
		t.Fatalf("reason %q at offset %d leaves no room for its length word", reason, idx)
	}
	if got := int(binary.BigEndian.Uint32(payload[idx-4:])); got != len(reason) {
		t.Fatalf("word before reason = %d, want its byte length %d", got, len(reason))
	}
	return idx - 4
}

// TestArchiveReasonLengthCorruption covers the shapes that make a reason
// unreadable: a declared length the remaining bytes cannot satisfy, a
// length word cut off mid-read, and reason bytes only partially saved. All
// arrive with valid outer framing and checksum and must be reported as
// ErrInvalidArchive with nil records — never as ErrInvalidRange, never as a
// partial read, and never by treating the missing bytes as an empty reason.
func TestArchiveReasonLengthCorruption(t *testing.T) {
	good, _, cp := reasonCorruptionFixture(t)
	basePayload := payloadOf(t, good)
	lengthAt := reasonLengthAt(t, basePayload, laterReason)

	// expectInvalid runs one payload mutation and asserts the whole read
	// fails as an unreadable archive. It also proves the failed call
	// rewrites neither its input bytes nor the retained checkpoint handed
	// to it.
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
			t.Fatalf("%s: unreadable reason counted as a content error: %v", name, err)
		}
		if got != nil {
			t.Fatalf("%s: returned %d records on an unreadable reason, want nil", name, len(got))
		}
		if wantSubstr != "" && !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("%s: err = %q, want substring %q", name, err.Error(), wantSubstr)
		}
		if !bytes.Equal(data, dataSnapshot) {
			t.Fatalf("%s: failed decode rewrote the archive bytes", name)
		}
		if cp != cpSnapshot {
			t.Fatalf("%s: failed decode rewrote the checkpoint", name)
		}
	}

	// A) The length word is intact but declares more bytes than remain in
	// the whole payload. The read must fail from the reason's own length
	// check, not from any outer framing or checksum mismatch.
	expectInvalid("declared reason length exceeds remaining material", "truncated archive payload",
		func(p []byte) []byte {
			binary.BigEndian.PutUint32(p[lengthAt:], uint32(len(p)))
			return p
		})

	// B) The length word itself is only partially saved: the payload ends
	// two bytes into it. The read must fail reading the length, not decode
	// a reason from whatever bytes happen to be there.
	expectInvalid("reason length word partially saved", "truncated archive payload",
		func(p []byte) []byte { return p[:lengthAt+2] })

	// C) The length word is intact and still declares the full reason, but
	// the reason bytes themselves are only partially saved. The missing
	// bytes must not be treated as an empty reason: the read fails.
	expectInvalid("reason bytes partially saved", "truncated archive payload",
		func(p []byte) []byte { return p[:lengthAt+4+len(laterReason)/2] })

	// D) A dedicated emphasis for the requirement: a small archive whose
	// reason declares 4294967295 bytes must return the same deterministic
	// error promptly, without crashing or provisioning as if the declared
	// length were real.
	payload := append([]byte(nil), basePayload...)
	binary.BigEndian.PutUint32(payload[lengthAt:], 0xffffffff)
	tinyHuge := framePayload(payload)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := DecodeAuditArchive(tinyHuge, "acme", cp)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("max reason length: records=%v err=%v, want ErrInvalidArchive, nil", got, err)
	}
	if errors.Is(err, ErrInvalidRange) {
		t.Fatalf("max reason length: unreadable reason counted as a content error: %v", err)
	}
	// A regression that trusted the declared length would try to provision
	// four gigabytes for the reason; bound what this error path allocates.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("max-length reason decode allocated %d bytes, want a small error path", alloc)
	}
}

// TestArchiveLaterRecordReasonDamageDiscardsPrefix is the record-level case
// the requirement names: a complete policy publish and a complete, normal
// decision — the very record the auditor intends to review — sit before
// the decision whose reason length is corrupt. The whole read must still
// fail: the intact prefix is never delivered, and the earlier review target
// cannot be reached by reading only the undamaged part.
func TestArchiveLaterRecordReasonDamageDiscardsPrefix(t *testing.T) {
	good, _, cp := reasonCorruptionFixture(t)

	// The intact archive decodes and the seq-2 target reviews clean; the
	// damage below sits strictly after it.
	if _, err := DecodeAuditArchive(good, "acme", cp); err != nil {
		t.Fatalf("intact archive must decode: %v", err)
	}

	damaged := reframe(t, good, func(payload []byte) []byte {
		lengthAt := reasonLengthAt(t, payload, laterReason)
		binary.BigEndian.PutUint32(payload[lengthAt:], uint32(len(payload)))
		return payload
	})
	dataSnapshot := append([]byte(nil), damaged...)
	cpSnapshot := cp

	got, err := DecodeAuditArchive(damaged, "acme", cp)
	if !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("later-record reason damage: records=%v err=%v, want ErrInvalidArchive, nil", got, err)
	}
	if errors.Is(err, ErrInvalidRange) {
		t.Fatalf("later-record reason damage counted as a content error: %v", err)
	}
	if !bytes.Equal(damaged, dataSnapshot) {
		t.Fatal("failed decode rewrote the archive bytes")
	}
	if cp != cpSnapshot {
		t.Fatal("failed decode rewrote the checkpoint")
	}
}

// TestArchiveReasonContentChangeIsInvalidRange keeps format damage separate
// from untrusted content: a reason byte flipped in place — every length
// word, marker and the recomputed outer checksum still legal — decodes
// structurally and must fail as content with ErrInvalidRange against the
// unchanged audit fingerprint and retained checkpoint, not be swept into
// ErrInvalidArchive merely because the changed byte lives in a reason.
func TestArchiveReasonContentChangeIsInvalidRange(t *testing.T) {
	good, _, cp := reasonCorruptionFixture(t)

	tampered := reframe(t, good, func(payload []byte) []byte {
		idx := bytes.Index(payload, []byte(laterReason))
		if idx < 0 {
			t.Fatal("reason not found in payload")
		}
		payload[idx] ^= 0x01 // same length, no length prefix or marker moves
		return payload
	})
	got, err := DecodeAuditArchive(tampered, "acme", cp)
	if !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("reason content change: records=%v err=%v, want ErrInvalidRange, nil", got, err)
	}
	if errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("readable-format reason change reported as archive damage: %v", err)
	}
}

// TestArchiveReasonRawBytesRoundTrip locks the legal boundary the
// corruption guard must not disturb: legitimate reasons restore from their
// exact bytes with the length read as a byte count — Chinese, spaces,
// control characters and non-UTF-8 bytes are neither truncated, replaced
// nor normalized, and an empty reason stays the empty string. The decoded
// material still verifies against the same checkpoint.
func TestArchiveReasonRawBytesRoundTrip(t *testing.T) {
	reasons := []string{
		"批准：中文 理由 通过",
		"  前后 空格 保留  ",
		"控制\x00\x01\t字符\n",
		"非UTF8 " + invalidByte + " 与 " + anotherInvalidByte,
		"", // empty reason stays empty
	}
	builds := []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version: 1, Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
	}
	for _, r := range reasons {
		reason := r
		builds = append(builds, func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u1", "r1", "org/a", "read"),
				Decision: Decision{Allowed: true, Reason: reason, Matched: []string{"p1"}, Version: 1},
			}
		})
	}
	recs, cp := syntheticChain("acme", builds)

	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode reasons: %v", err)
	}
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("decode reasons: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("reasons changed across the archive:\n got %+v\nwant %+v", got, recs)
	}
	for i, want := range reasons {
		if got[i+1].Decision.Decision.Reason != want {
			t.Fatalf("reason %d = %q, want exactly %q", i, got[i+1].Decision.Decision.Reason, want)
		}
	}
	// The empty reason is a real empty string, not a dropped field.
	if got[len(reasons)].Decision.Decision.Reason != "" {
		t.Fatalf("empty reason decoded as %q", got[len(reasons)].Decision.Decision.Reason)
	}
	if err := VerifyAudit("acme", got, cp); err != nil {
		t.Fatalf("decoded raw-reason material must verify: %v", err)
	}
}
