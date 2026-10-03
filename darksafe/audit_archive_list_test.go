// Regression coverage for list-length damage *inside* an otherwise sound
// archive frame. The framing tests already cover a bad magic, payload
// length and checksum; these tests keep the outer frame, total payload
// length and checksum all correct and corrupt only a count declared by the
// payload itself: the whole-material record list, and the policy and string
// lists nested inside records. Such material is unreadable as a format, so
// DecodeAuditArchive must fail with ErrInvalidArchive (errors.Is) and hand
// back nil records — never a partially decoded prefix, and never a crash or
// large allocation when the declared count is huge.
package darksafe

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// frameArchivePayload wraps a payload built (or damaged) by a test with the
// real framing: magic, declared payload length and a matching checksum, so
// the failure under test is always internal list structure, never framing.
func frameArchivePayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	out := append([]byte(nil), archiveMagic...)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)))
	out = append(out, lb[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	out = append(out, sum[:]...)
	return out
}

// extractArchivePayload returns a detached copy of one archive's payload.
func extractArchivePayload(t *testing.T, archive []byte) []byte {
	t.Helper()
	magicLen := len(archiveMagic)
	n := int(binary.BigEndian.Uint32(archive[magicLen : magicLen+4]))
	return append([]byte(nil), archive[magicLen+4:magicLen+4+n]...)
}

// archiveHeaderBytes writes the payload header (organization, end sequence,
// embedded fingerprint) exactly as the encoder does.
func archiveHeaderBytes(t *testing.T, org string, endSeq int, fingerprint string) []byte {
	t.Helper()
	w := &archiveWriter{}
	w.stringField(org)
	w.u32(endSeq)
	w.stringField(fingerprint)
	if w.err != nil {
		t.Fatalf("header writer: %v", w.err)
	}
	return w.buf
}

// archiveRecordBytes writes one record exactly as EncodeAuditArchive does.
func archiveRecordBytes(t *testing.T, r *AuditRecord) []byte {
	t.Helper()
	w := &archiveWriter{}
	w.stringField(r.Org)
	w.u32(r.Seq)
	w.stringField(r.Kind)
	w.change(r.Change)
	w.decisionRecord(r.Decision)
	w.stringField(r.PrevFingerprint)
	w.stringField(r.Fingerprint)
	if w.err != nil {
		t.Fatalf("record writer: %v", w.err)
	}
	return w.buf
}

// archiveHugeListCount is the largest count a u32 list field can declare:
// a tiny archive claiming this many items must fail deterministically
// without allocating for them.
const archiveHugeListCount = 0xffffffff // 4294967295

// u32Bytes returns one big-endian count word.
func u32Bytes(v int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	return b[:]
}

// completePolicyChangeRecord is one structurally complete record with a
// populated policy list, used as a complete prefix ahead of damage.
func completePolicyChangeRecord(t *testing.T, org string, seq int) AuditRecord {
	t.Helper()
	change := &PolicyChange{Version: 1, Policies: []Policy{
		{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "p2", Subject: "u2", Action: "read", Scope: "org/a", Effect: EffectDeny, ResourceID: "r1"},
	}}
	return syntheticRecord(org, seq, AuditPolicyChange, change, nil, genesisFingerprint(org))
}

// TestArchiveDecodeRejectsCorruptInternalListCounts covers each list kind
// against each unreadable-count class: the count promises more items than
// the body carries, the count word itself is cut off, or the items before
// the end are complete while a later item lacks required fields. Every
// case must be ErrInvalidArchive with nil records, and must not touch the
// supplied bytes.
func TestArchiveDecodeRejectsCorruptInternalListCounts(t *testing.T) {
	org := "acme"
	header := archiveHeaderBytes(t, org, 1, genesisFingerprint(org))

	// One complete record of each kind, used as an intact prefix.
	changeRec := completePolicyChangeRecord(t, org, 1)
	changeRecBytes := archiveRecordBytes(t, &changeRec)

	// policyListTail wraps a damaged policy list in the change payload of an
	// otherwise complete record: org, seq, kind, present marker, version,
	// then the supplied policy-list bytes and nothing after.
	policyListTail := func(tail []byte) []byte {
		w := &archiveWriter{}
		w.stringField(org)
		w.u32(1)
		w.stringField(AuditPolicyChange)
		w.raw([]byte{archiveTagPresent}) // change present
		w.u32(1)                         // change version
		w.buf = append(w.buf, tail...)
		return w.buf
	}
	onePolicy := func() []byte {
		w := &archiveWriter{}
		w.policy(Policy{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow})
		return w.buf
	}

	// decisionTailUpToReason lays out a present decision record through the
	// reason string, leaving the matched list for the damaged tail. Roles are
	// written complete first so the damage is unambiguously the matched list.
	decisionTailUpToReason := func() []byte {
		w := &archiveWriter{}
		w.stringField(org)                    // record org
		w.u32(2)                              // record seq
		w.stringField(AuditDecision)          // record kind
		w.raw([]byte{archiveTagNil})          // change absent
		w.raw([]byte{archiveTagPresent})      // decision present
		w.stringField(org)                    // subject org
		w.stringField(org)                    // resource org
		w.stringField("u1")                   // subject id
		w.stringField("user")                 // subject kind
		w.stringList([]string{"auditor"})     // roles: complete list
		w.boolean(false)                      // subject disabled
		w.stringField("r1")                   // resource id
		w.stringField("org/a")                // resource scope
		w.stringField("read")                 // action
		w.boolean(true)                       // decision allowed
		w.stringField("matched allow policy") // reason
		return w.buf
	}
	oneString := func(s string) []byte {
		w := &archiveWriter{}
		w.stringField(s)
		return w.buf
	}

	// subjectPrefix ends right after the subject kind, so the damaged tail
	// is the roles list nested inside the subject.
	subjectPrefix := func() []byte {
		w := &archiveWriter{}
		w.stringField(org)
		w.u32(2)
		w.stringField(AuditDecision)
		w.raw([]byte{archiveTagNil})
		w.raw([]byte{archiveTagPresent})
		w.stringField(org) // subject org
		w.stringField(org) // resource org
		w.stringField("u1")
		w.stringField("user") // subject kind; roles follow in the tail
		return w.buf
	}

	const hugeCount = archiveHugeListCount

	cases := []struct {
		name    string
		payload []byte
	}{
		// ---- Whole-material record list ----
		{
			"records/count promises items but body is empty",
			append(append([]byte(nil), header...), u32Bytes(3)...),
		},
		{
			"records/count word cut off after two bytes",
			append(append([]byte(nil), header...), 0x00, 0x00),
		},
		{
			"records/first record complete, then the payload ends",
			bytes.Join([][]byte{header, u32Bytes(2), changeRecBytes}, nil),
		},
		{
			"records/first record complete, second only carries an org field",
			func() []byte {
				w := &archiveWriter{}
				w.stringField(org) // partial second record: org present, then payload ends
				return bytes.Join([][]byte{header, u32Bytes(2), changeRecBytes, w.buf}, nil)
			}(),
		},
		{
			"records/huge count in a tiny payload allocates nothing",
			append(append([]byte(nil), header...), u32Bytes(hugeCount)...),
		},
		{
			"records/huge count with some trailing bytes",
			bytes.Join([][]byte{header, u32Bytes(hugeCount), make([]byte, 40)}, nil),
		},
		{
			"records/declared minimum bytes for second record not present",
			// 22 trailing zero bytes is exactly the smallest structurally
			// readable empty record; 21 must be truncation, not a decode.
			bytes.Join([][]byte{header, u32Bytes(2), changeRecBytes, make([]byte, 21)}, nil),
		},

		// ---- Policy list nested inside a change ----
		{
			"policies/count promises a policy but the list is empty",
			policyListTail(append([]byte{archiveTagPresent}, u32Bytes(1)...)),
		},
		{
			"policies/count word cut off after two bytes",
			policyListTail([]byte{archiveTagPresent, 0x00, 0x00}),
		},
		{
			"policies/first policy complete, second cut off",
			policyListTail(bytes.Join([][]byte{
				{archiveTagPresent}, u32Bytes(2), onePolicy(),
				{0x00, 0x00, 0x00, 0x02, 'x'}, // second policy: id length 2, one byte, end
			}, nil)),
		},
		{
			"policies/huge count in a tiny list",
			policyListTail(append([]byte{archiveTagPresent}, u32Bytes(hugeCount)...)),
		},
		{
			"policies/count clears the per-item floor but grammar cannot finish",
			// Two policies need >= 50 remaining bytes, so 60 zero bytes pass
			// the minimum-size check; both "policies" parse as empty values
			// (25 zero bytes each), but the change tail and record fingerprints
			// then run past the payload: the malformed count is still ErrInvalidArchive.
			policyListTail(bytes.Join([][]byte{{archiveTagPresent}, u32Bytes(2), make([]byte, 60)}, nil)),
		},
		{
			"policies/invalid list marker",
			policyListTail([]byte{0x7f}),
		},

		// ---- String list: decision Matched ----
		{
			"matched/count promises two items but only one is present",
			bytes.Join([][]byte{
				decisionTailUpToReason(),
				{archiveTagPresent}, u32Bytes(2), oneString("p1"),
			}, nil),
		},
		{
			"matched/count word cut off after two bytes",
			append(decisionTailUpToReason(), archiveTagPresent, 0x00, 0x00),
		},
		{
			"matched/huge count in a tiny list",
			append(append(decisionTailUpToReason(), archiveTagPresent), u32Bytes(hugeCount)...),
		},
		{
			"matched/count clears the per-item floor but record tail is missing",
			// Second item parses as an empty string and the decision version
			// word consumes the last bytes, so the record fingerprints are
			// missing: truncation, not a successful decode.
			bytes.Join([][]byte{
				decisionTailUpToReason(),
				{archiveTagPresent}, u32Bytes(2), oneString("p1"), make([]byte, 8),
			}, nil),
		},

		// ---- String list: subject Roles ----
		{
			"roles/count promises two items but only one is present",
			bytes.Join([][]byte{
				subjectPrefix(),
				{archiveTagPresent}, u32Bytes(2), oneString("auditor"),
			}, nil),
		},
		{
			"roles/count word cut off after two bytes",
			append(subjectPrefix(), archiveTagPresent, 0x00, 0x00),
		},
		{
			"roles/huge count in a tiny list",
			append(append(subjectPrefix(), archiveTagPresent), u32Bytes(hugeCount)...),
		},
	}

	cp := Checkpoint{Org: org, EndSeq: 1, Fingerprint: genesisFingerprint(org)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := frameArchivePayload(t, tc.payload)
			input := append([]byte(nil), data...)
			cpBefore := cp
			got, err := DecodeAuditArchive(data, org, cp)
			if !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("err = %v, want ErrInvalidArchive", err)
			}
			if got != nil {
				t.Fatalf("records = %v (len %d), want nil on a failed read", got, len(got))
			}
			if !bytes.Equal(data, input) {
				t.Fatal("failed decode mutated the supplied archive bytes")
			}
			if cp != cpBefore {
				t.Fatalf("failed decode mutated the checkpoint: %+v -> %+v", cpBefore, cp)
			}
		})
	}
}

// walkToLastDecisionMatchedCount parses a good payload and returns the
// payload offset of the matched-list marker and its count word in the final
// decision record, together with the real count.
func walkToLastDecisionMatchedCount(t *testing.T, payload []byte) (markerPos, countPos, count int) {
	t.Helper()
	r := &archiveReader{data: payload}
	_ = r.stringField() // org
	_ = r.u32()         // end seq
	_ = r.stringField() // fingerprint
	n := r.u32()
	if r.err != nil {
		t.Fatalf("walk header: %v", r.err)
	}
	for i := 0; i < n; i++ {
		_ = r.stringField() // org
		_ = r.u32()         // seq
		_ = r.stringField() // kind
		_ = r.change()
		switch r.marker() { // decision payload marker
		case archiveTagNil:
			_ = r.stringField() // prev fingerprint
			_ = r.stringField() // fingerprint
			continue
		case archiveTagPresent:
		default:
			t.Fatalf("walk: bad decision marker in record %d", i)
		}
		_ = r.stringField() // subject org
		_ = r.stringField() // resource org
		_ = r.stringField() // subject id
		_ = r.stringField() // subject kind
		switch r.marker() { // roles
		case archiveTagNil:
		case archiveTagPresent:
			c := r.u32()
			for j := 0; j < c; j++ {
				_ = r.stringField()
			}
		default:
			t.Fatalf("walk: bad roles marker in record %d", i)
		}
		_ = r.boolean()     // disabled
		_ = r.stringField() // resource id
		_ = r.stringField() // resource scope
		_ = r.stringField() // action
		_ = r.boolean()     // decision allowed
		_ = r.stringField() // reason
		mPos := r.pos
		switch r.marker() { // matched list marker
		case archiveTagNil, archiveTagPresent:
		default:
			t.Fatalf("walk: bad matched marker in record %d", i)
		}
		cPos := r.pos
		c := r.u32()
		if i == n-1 {
			if r.err != nil {
				t.Fatalf("walk target count: %v", r.err)
			}
			return mPos, cPos, c
		}
		for j := 0; j < c; j++ {
			_ = r.stringField()
		}
		_ = r.u32()         // decision version
		_ = r.stringField() // prev fingerprint
		_ = r.stringField() // fingerprint
	}
	t.Fatal("walk: last record was not a present decision")
	return 0, 0, 0
}

// TestArchiveCorruptListAfterValidPrefixFailsWholeRead builds a real chain
// with a complete policy change and complete decision records ahead of the
// damaged list. Damage confined to a list in the final record must still
// fail the entire read with ErrInvalidArchive: the already decoded prefix is
// never handed back, even though every earlier record was intact.
func TestArchiveCorruptListAfterValidPrefixFailsWholeRead(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))                 // seq 2, matched [p1]
	lastDecision := s.Decide("acme", request("acme", "u1", "r1", "org/a", "read")) // seq 3
	if !lastDecision.Allowed || !reflect.DeepEqual(lastDecision.Matched, []string{"p1"}) {
		t.Fatalf("fixture decision = %+v", lastDecision)
	}
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the real prefix is valid offline-review material before damage.
	loaded, err := DecodeAuditArchive(good, "acme", cp)
	if err != nil {
		t.Fatal(err)
	}
	if review, err := RecheckDecisionOffline("acme", loaded, cp, 3); err != nil || !review.Consistent {
		t.Fatalf("prefix review before damage = %+v, %v", review, err)
	}

	payload := extractArchivePayload(t, good)
	_, countPos, realCount := walkToLastDecisionMatchedCount(t, payload)
	if realCount != 1 {
		t.Fatalf("fixture: matched count = %d, want 1", realCount)
	}

	mustFailWholeRead := func(name string, damaged []byte) {
		t.Helper()
		data := frameArchivePayload(t, damaged)
		input := append([]byte(nil), data...)
		got, err := DecodeAuditArchive(data, "acme", cp)
		if !errors.Is(err, ErrInvalidArchive) || got != nil {
			t.Fatalf("%s: records=%v err=%v, want ErrInvalidArchive and nil", name, got, err)
		}
		if !bytes.Equal(data, input) {
			t.Fatalf("%s mutated the supplied archive bytes", name)
		}
	}

	// Inflate the final matched-list count past anything its body can hold;
	// the earlier policy change and both decisions were structurally complete.
	inflated := append([]byte(nil), payload...)
	binary.BigEndian.PutUint32(inflated[countPos:countPos+4], archiveHugeListCount)
	mustFailWholeRead("inflated count after complete prefix", inflated)

	// A modest inflation that clears the 4-byte-per-item floor still desyncs
	// everything after the list and cannot complete the record.
	modest := append([]byte(nil), payload...)
	binary.BigEndian.PutUint32(modest[countPos:countPos+4], uint32(realCount+1))
	mustFailWholeRead("modest count inflation after complete prefix", modest)

	// Cut the count word itself off mid-read (two of its four bytes), plus
	// everything after it: the list's declared length cannot even be read.
	cutWord := append([]byte(nil), payload[:countPos+2]...)
	mustFailWholeRead("count word truncated after complete prefix", cutWord)

	// Keep the count intact but truncate the single item's body after its
	// length word: the item promised two bytes, only one follows, so the
	// required field cannot be read.
	cutItem := append([]byte(nil), payload[:countPos+4+4+1]...) // marker, count, length word, first byte
	mustFailWholeRead("item body truncated after complete prefix", cutItem)

	// The undamaged archive still reads: damage produced no lasting effect.
	if got, err := DecodeAuditArchive(good, "acme", cp); err != nil || !reflect.DeepEqual(got, recs) {
		t.Fatalf("good archive after damage probes: %v %v", got, err)
	}
}

// TestArchiveLegalListShapesSurviveRoundTrip pins the boundary the count
// hardening must not move: nil and non-nil empty policy/string lists read
// back with their original shape, populated lists keep order and raw bytes,
// fingerprints (and thus the caller's checkpoint) are unchanged, and the
// decoded material still drives offline review directly.
func TestArchiveLegalListShapesSurviveRoundTrip(t *testing.T) {
	org := "acme"
	reqAllow := func(id string, roles []string) OrgRequest {
		r := request(org, id, "r1", "org/a", "read")
		r.Subject.Kind = "user"
		r.Subject.Roles = roles
		return r
	}
	recs, cp := syntheticChain(org, []func(int) (string, *PolicyChange, *DecisionRecord){
		func(int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 1, Policies: nil}, nil // nil policy list
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 2, Policies: []Policy{}}, nil // empty non-nil
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 3, Policies: []Policy{
				{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
				{ID: "p2", Subject: "u2", Action: "read", Scope: "org/a", Effect: EffectDeny, ResourceID: "r1"},
			}}, nil // populated policy list, fixed order
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			// Envelope rejection: version 0, nil matched list.
			d := Decision{Allowed: false, Reason: "subject is disabled"}
			req := reqAllow("u9", nil)
			req.Subject.Disabled = true
			return AuditDecision, nil, &DecisionRecord{Request: req, Decision: d}
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			d := Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 3}
			return AuditDecision, nil, &DecisionRecord{Request: reqAllow("u1", []string{"auditor", "reviewer"}), Decision: d}
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			// No matching policy: evaluate returns a non-nil empty matched list.
			d := Decision{Allowed: false, Reason: "no matching allow policy", Matched: []string{}, Version: 3}
			return AuditDecision, nil, &DecisionRecord{Request: reqAllow("nomatch-1", nil), Decision: d} // nil roles
		},
		func(int) (string, *PolicyChange, *DecisionRecord) {
			d := Decision{Allowed: false, Reason: "no matching allow policy", Matched: []string{}, Version: 3}
			return AuditDecision, nil, &DecisionRecord{Request: reqAllow("nomatch-2", []string{}), Decision: d} // empty roles
		},
	})

	archive, err := EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatalf("decode legal shapes: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("legal list shapes changed:\n got %+v\nwant %+v", got, recs)
	}
	// Explicit shape assertions, including nil-vs-empty preservation.
	if got[0].Change.Policies != nil {
		t.Fatal("nil policy list decoded as non-nil")
	}
	if got[1].Change.Policies == nil || len(got[1].Change.Policies) != 0 {
		t.Fatal("empty non-nil policy list lost its shape")
	}
	if len(got[2].Change.Policies) != 2 ||
		got[2].Change.Policies[0].ID != "p1" || got[2].Change.Policies[1].ID != "p2" ||
		got[2].Change.Policies[1].ResourceID != "r1" {
		t.Fatalf("populated policy list order/bytes not preserved: %+v", got[2].Change.Policies)
	}
	if got[3].Decision.Decision.Matched != nil {
		t.Fatal("nil matched list decoded as non-nil")
	}
	if roles := got[4].Decision.Request.Subject.Roles; len(roles) != 2 || roles[0] != "auditor" || roles[1] != "reviewer" {
		t.Fatalf("populated roles order not preserved: %+v", roles)
	}
	if m := got[4].Decision.Decision.Matched; len(m) != 1 || m[0] != "p1" {
		t.Fatalf("populated matched list not preserved: %+v", m)
	}
	if got[5].Decision.Request.Subject.Roles != nil {
		t.Fatal("nil roles decoded as non-nil")
	}
	if got[5].Decision.Decision.Matched == nil {
		t.Fatal("empty non-nil matched list decoded as nil")
	}
	if got[6].Decision.Request.Subject.Roles == nil {
		t.Fatal("empty non-nil roles decoded as nil")
	}

	// The hardening moves no fingerprint boundary: re-encoding is byte
	// identical, and every decision still reviews consistently offline.
	if again, err := EncodeAuditArchive(org, got, cp); err != nil || !bytes.Equal(again, archive) {
		t.Fatalf("re-encoding changed the archive bytes: equal=%v err=%v", bytes.Equal(again, archive), err)
	}
	for _, seq := range []int{4, 5, 6, 7} {
		review, err := RecheckDecisionOffline(org, got, cp, seq)
		if err != nil {
			t.Fatalf("offline review seq %d: %v", seq, err)
		}
		if !review.Consistent {
			t.Fatalf("seq %d review inconsistent after round trip: %+v", seq, review)
		}
	}
}

// TestArchiveListContentTamperStaysInvalidRange keeps the format-vs-content
// boundary: a list item whose bytes are changed (frame, length and checksum
// all recomputed) is fully readable as a format, so the failure is
// ErrInvalidRange from chain validation — never folded into ErrInvalidArchive.
func TestArchiveListContentTamperStaysInvalidRange(t *testing.T) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		allowPolicy("p1", "u1", "read", "org/a", false),
		{ID: "p9", Subject: "policy-tamper-subject", Action: "read", Scope: "org/a", Effect: EffectAllow},
	})
	// A decision carrying a unique role entry; the request denies but the
	// role string participates in the fingerprint.
	roleReq := request("acme", "role-tamper-victim", "r1", "org/a", "read")
	roleReq.Subject.Roles = []string{"auditor-role-marker"}
	s.Decide("acme", roleReq)
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}

	tamperAt := func(name, marker string) {
		t.Helper()
		tampered := reframe(t, good, func(payload []byte) []byte {
			idx := bytes.Index(payload, []byte(marker))
			if idx < 0 {
				t.Fatalf("%s: marker %q not found", name, marker)
			}
			payload[idx] ^= 0x01 // same length: no count or marker moves
			return payload
		})
		input := append([]byte(nil), tampered...)
		got, err := DecodeAuditArchive(tampered, "acme", cp)
		if !errors.Is(err, ErrInvalidRange) || got != nil {
			t.Fatalf("%s: records=%v err=%v, want ErrInvalidRange and nil", name, got, err)
		}
		if !bytes.Equal(tampered, input) {
			t.Fatalf("%s: decode mutated the supplied archive bytes", name)
		}
	}
	tamperAt("policy list item content", "policy-tamper-subject")
	tamperAt("string list item content", "auditor-role-marker")
}
