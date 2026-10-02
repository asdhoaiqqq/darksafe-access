package darksafe

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// TestArchiveRoundTripPreservesEveryField archives a live export mixing
// resource-restricted publishes, a rollback and decisions, and requires the
// decoded material to be deeply identical to the export: order, sequences,
// categories, policy versions, rollback source, full requests, decision
// explanations, predecessor and own fingerprints, and resource limits.
func TestArchiveRoundTripPreservesEveryField(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "p2", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny, ResourceID: "ledger-甲"},
	})
	allowed := s.Decide("acme", request("acme", "u1", "ledger-甲", "org/a", "read"))
	if allowed.Allowed {
		t.Fatalf("resource-restricted deny must win: %+v", allowed)
	}
	s.Publish("acme", 1, nil)
	if v, err := s.Rollback("acme", 2, 1); err != nil || v != 3 {
		t.Fatalf("rollback = %d, %v", v, err)
	}
	s.Decide("acme", request("acme", "u1", "other", "org/a", "read"))

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(archive) == 0 || !bytes.HasPrefix(archive, []byte(archiveMagic)) {
		t.Fatal("archive must carry the recognizable framing")
	}
	if again, err := EncodeAuditArchive("acme", recs, cp); err != nil || !bytes.Equal(again, archive) {
		t.Fatalf("encoding must be deterministic: equal=%v err=%v", bytes.Equal(again, archive), err)
	}

	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("decoded records differ from export:\n got %+v\nwant %+v", got, recs)
	}

	// The decoded records feed the existing offline review entry directly.
	for _, seq := range []int{2, 5} {
		review, err := RecheckDecisionOffline("acme", got, cp, seq)
		if err != nil {
			t.Fatalf("offline review seq %d after archive: %v", seq, err)
		}
		if !review.Consistent {
			t.Fatalf("seq %d review inconsistent: %+v", seq, review)
		}
	}
}

// TestArchiveEmptyOrgUsesGenesisCheckpoint archives and reads an
// organization with no records, pinned by its sequence-0 root checkpoint.
func TestArchiveEmptyOrgUsesGenesisCheckpoint(t *testing.T) {
	s := NewStore()
	recs, cp, err := s.AuditExport("ghost", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 || cp.EndSeq != 0 || cp.Fingerprint != genesisFingerprint("ghost") {
		t.Fatalf("empty export = %+v, %+v", recs, cp)
	}
	archive, err := EncodeAuditArchive("ghost", recs, cp)
	if err != nil {
		t.Fatalf("encode empty: %v", err)
	}
	got, err := DecodeAuditArchive(archive, "ghost", cp)
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("decoded empty archive = %d records", len(got))
	}
	if err := VerifyAudit("ghost", got, cp); err != nil {
		t.Fatalf("decoded empty material must verify: %v", err)
	}
	if _, err := RecheckDecisionOffline("ghost", got, cp, 1); !errors.Is(err, ErrAuditNotFound) {
		t.Fatalf("review over decoded empty material err = %v", err)
	}

	// Explicit nil and an empty slice describe the same empty export.
	if _, err := EncodeAuditArchive("ghost", nil, cp); err != nil {
		t.Fatalf("nil records with genesis checkpoint: %v", err)
	}
}

// TestArchivePrefixWithOwnCheckpoint archives a prefix ending at a
// historical sequence, paired with that prefix's own checkpoint.
func TestArchivePrefixWithOwnCheckpoint(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, nil) // later record, excluded from the prefix

	prefix, prefixCP, err := s.AuditExport("acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := EncodeAuditArchive("acme", prefix, prefixCP)
	if err != nil {
		t.Fatalf("encode prefix: %v", err)
	}
	got, err := DecodeAuditArchive(archive, "acme", prefixCP)
	if err != nil {
		t.Fatalf("decode prefix: %v", err)
	}
	if !reflect.DeepEqual(got, prefix) {
		t.Fatalf("prefix round trip = %+v, want %+v", got, prefix)
	}
	review, err := RecheckDecisionOffline("acme", got, prefixCP, 2)
	if err != nil || !review.Consistent || !review.Recomputed.Allowed {
		t.Fatalf("prefix review = %+v, %v", review, err)
	}

	// The prefix archive cannot be read against the later full checkpoint.
	full, fullCP, _ := s.AuditExport("acme", 0)
	_ = full
	if _, err := DecodeAuditArchive(archive, "acme", fullCP); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("prefix against later checkpoint err = %v, want ErrInvalidRange", err)
	}
}

// TestArchivePreservesRawBytesAndShapes exercises Chinese, control
// characters, spaces, non-UTF-8 bytes, the 0xFF vs 0xFE vs genuine U+FFFD
// distinction, nil-vs-empty lists and absent payloads through an archive
// round trip.
func TestArchivePreservesRawBytesAndShapes(t *testing.T) {
	org, recs, cp := buildInvalidByteChain(t)
	archive, err := EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatalf("encode raw-byte chain: %v", err)
	}
	got, err := DecodeAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatalf("decode raw-byte chain: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("raw bytes or shapes changed across the archive:\n got %+v\nwant %+v", got, recs)
	}

	// Explicit shape probes that must survive byte-for-byte.
	shapes := []AuditRecord{
		syntheticRecord(org, 1, AuditPolicyChange, &PolicyChange{
			Version: 1, Policies: nil, // nil policy set
		}, nil, genesisFingerprint(org)),
	}
	shapes = append(shapes, syntheticRecord(org, 2, AuditPolicyChange, &PolicyChange{
		Version: 2, Policies: []Policy{}, // non-nil empty policy set
		SourceVersion: 1, RolledBack: true,
	}, nil, shapes[0].Fingerprint))
	shapes = append(shapes, syntheticRecord(org, 3, AuditDecision, nil, &DecisionRecord{
		Request: OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject: Subject{
				ID: " 中\t\n\r \"\\\x00\x01 " + invalidByte,
				// nil roles
			},
			Resource: Resource{ID: "r", Scope: "org/a"},
			Action:   "read ",
		},
		Decision: Decision{Allowed: true, Reason: "理" + invalidByte + "由", Matched: nil},
	}, shapes[1].Fingerprint))
	shapes = append(shapes, syntheticRecord(org, 4, AuditDecision, nil, &DecisionRecord{
		Request: OrgRequest{
			SubjectOrg: org, ResourceOrg: org,
			Subject:  Subject{ID: "u", Roles: []string{}}, // empty non-nil roles
			Resource: Resource{ID: "r", Scope: "org/a"},
			Action:   "read",
		},
		Decision: Decision{Allowed: false, Reason: "x", Matched: []string{}}, // empty non-nil matched
	}, shapes[2].Fingerprint))
	shapeCP := Checkpoint{Org: org, EndSeq: 4, Fingerprint: shapes[3].Fingerprint}
	arch, err := EncodeAuditArchive(org, shapes, shapeCP)
	if err != nil {
		t.Fatalf("encode shapes: %v", err)
	}
	back, err := DecodeAuditArchive(arch, org, shapeCP)
	if err != nil {
		t.Fatalf("decode shapes: %v", err)
	}
	if !reflect.DeepEqual(back, shapes) {
		t.Fatalf("nil/empty/absent shapes changed:\n got %+v\nwant %+v", back, shapes)
	}
	if back[0].Change.Policies != nil {
		t.Fatal("nil policy set decoded as non-nil")
	}
	if back[1].Change.Policies == nil || len(back[1].Change.Policies) != 0 {
		t.Fatal("empty policy set must decode non-nil and empty")
	}
	if back[2].Decision.Request.Subject.Roles != nil {
		t.Fatal("nil roles decoded as non-nil")
	}
	if back[3].Decision.Request.Subject.Roles == nil {
		t.Fatal("empty roles decoded as nil")
	}
	if back[3].Decision.Decision.Matched == nil {
		t.Fatal("empty matched list decoded as nil")
	}

	// Three byte-different strings stay distinguishable: archives of
	// one-byte-different reasons must themselves differ.
	for _, pair := range [][2]string{
		{invalidByte, anotherInvalidByte},
		{invalidByte, replacementRune},
		{anotherInvalidByte, replacementRune},
	} {
		oneRecs := singleStringRecords(t, "o", pair[0])
		twoRecs := singleStringRecords(t, "o", pair[1])
		one, err := EncodeAuditArchive("o", oneRecs,
			Checkpoint{Org: "o", EndSeq: 1, Fingerprint: oneRecs[0].Fingerprint})
		if err != nil {
			t.Fatal(err)
		}
		two, err := EncodeAuditArchive("o", twoRecs,
			Checkpoint{Org: "o", EndSeq: 1, Fingerprint: twoRecs[0].Fingerprint})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(one, two) {
			t.Fatalf("archive collapses byte-different reasons into identical bytes")
		}
		// And each must decode back to exactly its own raw reason.
		if back, err := DecodeAuditArchive(one, "o",
			Checkpoint{Org: "o", EndSeq: 1, Fingerprint: oneRecs[0].Fingerprint}); err != nil ||
			back[0].Decision.Decision.Reason != pair[0] ||
			back[0].Decision.Decision.Reason == pair[1] {
			t.Fatalf("reason round trip = %q, err = %v", back[0].Decision.Decision.Reason, err)
		}
	}
}

// singleStringRecords builds a one-record chain whose decision reason is s,
// used to prove two byte-different reasons archive differently.
func singleStringRecords(t *testing.T, org, s string) []AuditRecord {
	t.Helper()
	rec := syntheticRecord(org, 1, AuditDecision, nil, &DecisionRecord{
		Request:  request(org, "u1", "r1", "org/a", "read"),
		Decision: Decision{Allowed: false, Reason: s, Version: 0},
	}, genesisFingerprint(org))
	return []AuditRecord{rec}
}

// TestArchiveEncodingRejectsInvalidMaterial ensures encode validates the
// whole material and hands back no bytes on failure.
func TestArchiveEncodingRejectsInvalidMaterial(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, nil)
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	if b, err := EncodeAuditArchive("", recs, cp); !errors.Is(err, ErrMissingOrganization) || b != nil {
		t.Fatalf("empty org: bytes=%v err=%v", b, err)
	}

	bad := func(mutate func([]AuditRecord) []AuditRecord, checkpoint Checkpoint) {
		t.Helper()
		rs := cloneExportedRecords(recs)
		rs = mutate(rs)
		b, err := EncodeAuditArchive("acme", rs, checkpoint)
		if !errors.Is(err, ErrInvalidRange) || b != nil {
			t.Fatalf("encode bad material: bytes=%v err=%v, want ErrInvalidRange", b, err)
		}
	}
	bad(func(r []AuditRecord) []AuditRecord { return r[1:] }, cp)                       // missing beginning
	bad(func(r []AuditRecord) []AuditRecord { return r[:2] }, cp)                       // checkpoint past material
	bad(func(r []AuditRecord) []AuditRecord { r[0], r[1] = r[1], r[0]; return r }, cp)  // reordered
	bad(func(r []AuditRecord) []AuditRecord { r[2].Change.Version = 99; return r }, cp) // later record changed
	bad(func(r []AuditRecord) []AuditRecord { return r },
		Checkpoint{Org: "acme", EndSeq: cp.EndSeq, Fingerprint: cp.Fingerprint + "x"}) // bad checkpoint
	bad(func(r []AuditRecord) []AuditRecord { return r },
		Checkpoint{Org: "globex", EndSeq: cp.EndSeq, Fingerprint: cp.Fingerprint}) // foreign checkpoint org

	// A record from another organization, even fingerprint-correct for it.
	foreign := syntheticRecord("globex", 1, AuditDecision, nil, &DecisionRecord{
		Request:  request("globex", "u1", "r1", "org/a", "read"),
		Decision: Decision{Allowed: false, Version: 0},
	}, genesisFingerprint("globex"))
	mixed := cloneExportedRecords(recs)
	mixed[2] = foreign
	if b, err := EncodeAuditArchive("acme", mixed, cp); !errors.Is(err, ErrInvalidRange) || b != nil {
		t.Fatalf("foreign record encode: bytes=%v err=%v", b, err)
	}

	// A well-fingerprinted record whose payload does not match its kind.
	wrongPayload := syntheticRecord("acme", 1, AuditDecision, &PolicyChange{Version: 1}, nil, genesisFingerprint("acme"))
	if b, err := EncodeAuditArchive("acme", []AuditRecord{wrongPayload},
		Checkpoint{Org: "acme", EndSeq: 1, Fingerprint: wrongPayload.Fingerprint}); !errors.Is(err, ErrInvalidRange) || b != nil {
		t.Fatalf("payload/kind mismatch: bytes=%v err=%v", b, err)
	}
}

// reframe parses one archive frame, lets the test alter the payload, and
// rebuilds a frame with a consistent declared length and checksum so the
// failure under test is content validation, not framing.
func reframe(t *testing.T, archive []byte, mutate func(payload []byte) []byte) []byte {
	t.Helper()
	magicLen := len(archiveMagic)
	n := int(binary.BigEndian.Uint32(archive[magicLen : magicLen+4]))
	payload := append([]byte(nil), archive[magicLen+4:magicLen+4+n]...)
	payload = mutate(payload)
	out := append([]byte(nil), archiveMagic...)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)))
	out = append(out, lb[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	out = append(out, sum[:]...)
	return out
}

// TestArchiveDecodeFramingErrors covers every framing failure mode.
func TestArchiveDecodeFramingErrors(t *testing.T) {
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

	mustFail := func(name string, data []byte) {
		t.Helper()
		got, err := DecodeAuditArchive(data, "acme", cp)
		if !errors.Is(err, ErrInvalidArchive) || got != nil {
			t.Fatalf("%s: records=%v err=%v, want ErrInvalidArchive and no records", name, got, err)
		}
	}
	mustFail("nil", nil)
	mustFail("empty", []byte{})
	mustFail("garbage", []byte("this is certainly not an audit archive"))
	mustFail("almost magic", append([]byte(archiveMagic[:len(archiveMagic)-1]), 'x'))
	// Truncations at every boundary class.
	for _, cut := range []int{1, 4, 31, 32, 33, len(archiveMagic), len(archiveMagic) + 1, len(good) - 1} {
		if cut < len(good) {
			mustFail(fmt.Sprintf("truncated %d", cut), good[:cut])
		}
	}
	// Flip a byte in the magic.
	flip := append([]byte(nil), good...)
	flip[0] ^= 0x80
	mustFail("bad magic", flip)
	// Declared length beyond the actual file.
	tooLong := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(tooLong[len(archiveMagic):], binary.BigEndian.Uint32(tooLong[len(archiveMagic):])+100)
	mustFail("declared length too large", tooLong)
	// Declared length shorter: bytes of the good frame become trailing.
	tooShort := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(tooShort[len(archiveMagic):], binary.BigEndian.Uint32(tooShort[len(archiveMagic):])-1)
	mustFail("declared length too small", tooShort)
	// Damage inside the payload with the original checksum left intact.
	damaged := append([]byte(nil), good...)
	damaged[len(archiveMagic)+5] ^= 0x01
	mustFail("payload bitflip, stale checksum", damaged)
	// Damage inside the checksum.
	csumDamage := append([]byte(nil), good...)
	csumDamage[len(csumDamage)-1] ^= 0x01
	mustFail("checksum bitflip", csumDamage)
	// A second full archive concatenated to the first is trailing material.
	mustFail("concatenated second archive", append(append([]byte(nil), good...), good...))
	// Ordinary trailing junk.
	mustFail("trailing junk", append(append([]byte(nil), good...), 0, 1, 2, 3))

	// A content change with a recomputed checksum passes framing but must
	// fail chain validation with ErrInvalidRange.
	tampered := reframe(t, good, func(payload []byte) []byte {
		idx := bytes.Index(payload, []byte("matched allow policy"))
		if idx < 0 {
			t.Fatal("fixture reason not found in payload")
		}
		payload[idx] ^= 0x01 // same length, no length prefix or marker moves
		return payload
	})
	if got, err := DecodeAuditArchive(tampered, "acme", cp); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("checksum-valid tampered content: records=%v err=%v, want ErrInvalidRange", got, err)
	}
}

// TestArchiveDecodeContentValidation ensures decode validates the entire
// material against the caller's retained checkpoint and expected org.
func TestArchiveDecodeContentValidation(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	s.Publish("acme", 1, nil)
	s.Decide("acme", request("acme", "late-victim", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := DecodeAuditArchive(good, "", cp); !errors.Is(err, ErrMissingOrganization) || got != nil {
		t.Fatalf("empty org decode: %v %v", got, err)
	}
	if _, err := DecodeAuditArchive(good, "globex", cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("foreign expected org err = %v", err)
	}
	// The caller's checkpoint is authoritative; a wrong fingerprint and a
	// wrong end sequence both fail, even though the embedded checkpoint
	// matches the content.
	for _, wrong := range []Checkpoint{
		{Org: "acme", EndSeq: cp.EndSeq, Fingerprint: cp.Fingerprint + "x"},
		{Org: "acme", EndSeq: cp.EndSeq - 1, Fingerprint: cp.Fingerprint},
		{Org: "acme", EndSeq: cp.EndSeq, Fingerprint: genesisFingerprint("acme")},
	} {
		if _, err := DecodeAuditArchive(good, "acme", wrong); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("checkpoint %+v err = %v, want ErrInvalidRange", wrong, err)
		}
	}

	// Damage confined to a record after the one the caller plans to review
	// still fails the whole read.
	late := reframe(t, good, func(payload []byte) []byte {
		idx := bytes.Index(payload, []byte("late-victim"))
		if idx < 0 {
			t.Fatal("late record subject not found in payload")
		}
		payload[idx] ^= 0x01
		return payload
	})
	if _, err := DecodeAuditArchive(late, "acme", cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("late-record damage err = %v, want ErrInvalidRange", err)
	}

	// An archive of another organization cannot be read as acme even with
	// acme-shaped caller data.
	grecs, gcp, _ := NewStore().AuditExport("globex", 0)
	garchive, err := EncodeAuditArchive("globex", grecs, gcp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAuditArchive(garchive, "acme", cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("cross-org archive err = %v", err)
	}
}

// TestArchiveCheckpointInsideIsNotAuthority proves a file that internally
// carries one checkpoint still validates exclusively against the caller's.
func TestArchiveCheckpointInsideIsNotAuthority(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp, _ := s.AuditExport("acme", 0)
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}

	// Re-frame the same records with a different embedded end sequence: the
	// caller's retained checkpoint must be the one that decides.
	rewritten := reframe(t, good, func(payload []byte) []byte {
		// Header layout: u32 orgLen, org, u32 endSeq, u32 fpLen, fp, ...
		pos := 4 + len("acme")
		binary.BigEndian.PutUint32(payload[pos:pos+4], 999)
		return payload
	})
	if _, err := DecodeAuditArchive(rewritten, "acme", cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("altered embedded metadata must not be trusted: err = %v", err)
	}
}

// TestArchiveInconsistentDecisionIsNotArchiveDamage archives a chain that
// is internally valid but whose recorded decision contradicts the recorded
// policy: decode succeeds, and offline review reports the inconsistency.
func TestArchiveInconsistentDecisionIsNotArchiveDamage(t *testing.T) {
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
				Request:  req,
				Decision: Decision{Allowed: false, Reason: "matched deny policy", Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("a decision disagreement is not archive damage: %v", err)
	}
	review, err := RecheckDecisionOffline("acme", got, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if review.Consistent {
		t.Fatal("inconsistent decision must still be reported by offline review")
	}
}

// TestArchiveIsReadOnlyAndDetached proves encode and decode mutate nothing
// and return values independent of their inputs and of each other.
func TestArchiveIsReadOnlyAndDetached(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	before, beforeCP, _ := s.AuditExport("acme", 0)

	recs, cp, _ := s.AuditExport("acme", 0)
	snapshot := cloneExportedRecords(recs)
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recs, snapshot) {
		t.Fatal("encode mutated the supplied records")
	}
	// Encode/decode append nothing to any service state.
	after, afterCP, _ := s.AuditExport("acme", 0)
	if !reflect.DeepEqual(after, before) || afterCP != beforeCP {
		t.Fatal("archive operations changed service state")
	}

	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the input bytes after decode cannot reach a prior result.
	for i := range archive {
		archive[i] = 0
	}
	if got[0].Change.Policies[0].ID != "p1" {
		t.Fatal("decoded records alias the archive input bytes")
	}
	// Mutating one decoded result cannot affect a later decode.
	got[0].Change.Policies[0].ID = "hacker"
	got[1].Decision.Request.Subject.ID = "hacker"
	got[1].Decision.Decision.Matched = append(got[1].Decision.Decision.Matched, "x")
	fresh, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeAuditArchive(fresh, "acme", cp)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, recs) {
		t.Fatal("mutation of one decoded result reached another decode")
	}
}

// TestArchiveRoundTripAfterInstanceEnds simulates the stated lifecycle:
// export and archive on one instance, then (with no Store at all) read the
// bytes elsewhere and continue offline review against the retained
// checkpoint.
func TestArchiveRoundTripAfterInstanceEnds(t *testing.T) {
	first := NewStore()
	first.Publish("acme", 0, []Policy{
		{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, ResourceID: " r 1 "},
	})
	original := first.Decide("acme", request("acme", "u1", " r 1 ", "org/a", "read"))
	recs, cp, err := first.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}

	// New instance, no Store involved in decoding.
	loaded, err := DecodeAuditArchive(saved, "acme", cp)
	if err != nil {
		t.Fatalf("read after instance end: %v", err)
	}
	review, err := RecheckDecisionOffline("acme", loaded, cp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Consistent || !reflect.DeepEqual(review.Original, original) {
		t.Fatalf("post-restart review = %+v, original %+v", review, original)
	}
}
