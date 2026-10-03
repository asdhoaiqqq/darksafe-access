// This file pins down the decoder's handling of corrupted *internal list
// lengths*: archives whose outer framing, declared payload length and
// checksum are all correct, but where a count declared inside the payload
// cannot be satisfied by the bytes that follow. Three lists carry counts:
// the material's record list, a policy change's policy list, and the string
// lists used for subject roles and matched policies.
//
// Every corruption below is reframed with a consistent outer length and
// checksum, so exercising these cases proves the protection comes from
// decoding the payload's own structure rather than from the framing-level
// samples in audit_archive_test.go. A count the body cannot satisfy, a
// count word cut off mid-read, and a complete early item followed by an
// incomplete later item are all unreadable archives: DecodeAuditArchive
// must return ErrInvalidArchive (matched by errors.Is) and nil records,
// never a prefix and never a crash or hostile allocation.
package darksafe

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// listSite locates one length-counted list inside a payload, in payload
// byte offsets. itemEnds marks the byte just past each complete item.
type listSite struct {
	markerAt int
	countAt  int // -1 when the list is encoded nil
	itemEnds []int
}

// payloadInspector walks the known-good, two-record fixture payload in
// encode order. It exists only to locate the bytes a test corrupts; it
// mirrors archiveWriter, so a format change moves these tests off their
// target rather than silently corrupting the wrong field.
type payloadInspector struct {
	b   []byte
	pos int
}

func (c *payloadInspector) u32() int {
	v := int(binary.BigEndian.Uint32(c.b[c.pos : c.pos+4]))
	c.pos += 4
	return v
}

func (c *payloadInspector) str() {
	n := c.u32()
	c.pos += n
}

func (c *payloadInspector) marker() byte {
	m := c.b[c.pos]
	c.pos++
	return m
}

func (c *payloadInspector) stringListSite() listSite {
	s := listSite{markerAt: c.pos, countAt: -1}
	if c.marker() == archiveTagNil {
		return s
	}
	s.countAt = c.pos
	n := c.u32()
	s.itemEnds = make([]int, 0, n)
	for i := 0; i < n; i++ {
		c.str()
		s.itemEnds = append(s.itemEnds, c.pos)
	}
	return s
}

func (c *payloadInspector) policySite() {
	c.str() // ID
	c.str() // Subject
	c.str() // Action
	c.str() // Scope
	c.str() // Effect
	c.pos++ // Recursive bool
	c.str() // ResourceID
}

func (c *payloadInspector) policyListSite() listSite {
	s := listSite{markerAt: c.pos, countAt: -1}
	if c.marker() == archiveTagNil {
		return s
	}
	s.countAt = c.pos
	n := c.u32()
	s.itemEnds = make([]int, 0, n)
	for i := 0; i < n; i++ {
		c.policySite()
		s.itemEnds = append(s.itemEnds, c.pos)
	}
	return s
}

// inspectTwoRecordPayload locates the four counted lists in the fixture
// built by listCorruptionFixture.
func inspectTwoRecordPayload(t *testing.T, p []byte) fixtureSites {
	t.Helper()
	c := &payloadInspector{b: p}
	c.str()    // embedded org
	c.pos += 4 // embedded end sequence
	c.str()    // embedded checkpoint fingerprint
	records := listSite{countAt: c.pos}
	n := c.u32() // record count
	if n != 2 {
		t.Fatalf("fixture record count = %d, want 2", n)
	}

	// Record 1: policy change with two policies, absent decision.
	c.str()    // org
	c.pos += 4 // seq
	c.str()    // kind
	if m := c.marker(); m != archiveTagPresent {
		t.Fatalf("record 1 change marker = %d, want present", m)
	}
	c.pos += 4 // change version
	policies := c.policyListSite()
	c.pos += 4 // source version
	c.pos++    // rolled back bool
	if m := c.marker(); m != archiveTagNil {
		t.Fatalf("record 1 decision marker = %d, want nil", m)
	}
	c.str() // prev fingerprint
	c.str() // own fingerprint
	records.itemEnds = append(records.itemEnds, c.pos)

	// Record 2: decision with two roles and two matched policies.
	c.str()    // org
	c.pos += 4 // seq
	c.str()    // kind
	if m := c.marker(); m != archiveTagNil {
		t.Fatalf("record 2 change marker = %d, want nil", m)
	}
	if m := c.marker(); m != archiveTagPresent {
		t.Fatalf("record 2 decision marker = %d, want present", m)
	}
	c.str() // request SubjectOrg
	c.str() // request ResourceOrg
	c.str() // subject ID
	c.str() // subject Kind
	roles := c.stringListSite()
	c.pos++ // subject disabled bool
	c.str() // resource ID
	c.str() // resource scope
	c.str() // action
	c.pos++ // decision allowed bool
	c.str() // decision reason
	matched := c.stringListSite()
	c.pos += 4 // decision version
	c.str()    // prev fingerprint
	c.str()    // own fingerprint
	records.itemEnds = append(records.itemEnds, c.pos)

	if c.pos != len(p) {
		t.Fatalf("inspector ended at %d, payload length %d", c.pos, len(p))
	}
	if len(roles.itemEnds) != 2 || len(matched.itemEnds) != 2 || len(policies.itemEnds) != 2 {
		t.Fatalf("fixture lists must each hold 2 items: policies=%d roles=%d matched=%d",
			len(policies.itemEnds), len(roles.itemEnds), len(matched.itemEnds))
	}
	return fixtureSites{records: records, policies: policies, roles: roles, matched: matched}
}

// fixtureSites are the four counted lists in the two-record fixture.
type fixtureSites struct {
	records  listSite
	policies listSite
	roles    listSite
	matched  listSite
}

// framePayload wraps a (possibly corrupted) payload with correct outer
// framing, declared length and checksum, so the failure under test is
// always internal list decoding rather than framing.
func framePayload(payload []byte) []byte {
	out := append([]byte(nil), archiveMagic...)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)))
	out = append(out, lb[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	return append(out, sum[:]...)
}

// payloadOf extracts a copy of one archive's payload.
func payloadOf(t *testing.T, archive []byte) []byte {
	t.Helper()
	magicLen := len(archiveMagic)
	n := int(binary.BigEndian.Uint32(archive[magicLen : magicLen+4]))
	return append([]byte(nil), archive[magicLen+4:magicLen+4+n]...)
}

// listCorruptionFixture builds complete, valid material: one policy change
// carrying two policies followed by one decision whose subject has two
// roles and whose decision matched two policies. All four counted lists
// therefore hold real, multi-item content.
func listCorruptionFixture(t *testing.T) ([]byte, []AuditRecord, Checkpoint) {
	t.Helper()
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		allowPolicy("p1", "u1", "read", "org/a", false),
		allowPolicy("p2", "u1", "read", "org/a", false),
	})
	req := OrgRequest{
		SubjectOrg:  "acme",
		ResourceOrg: "acme",
		Subject:     Subject{ID: "u1", Roles: []string{"role-a", "role-b"}},
		Resource:    Resource{ID: "r1", Scope: "org/a"},
		Action:      "read",
	}
	original := s.Decide("acme", req)
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	review, err := RecheckDecisionOffline("acme", got, cp, 2)
	if err != nil || !review.Consistent || !reflect.DeepEqual(review.Original, original) {
		t.Fatalf("fixture review = %+v, %v", review, err)
	}
	return archive, recs, cp
}

// TestArchiveInternalListCountCorruption covers, for every counted list,
// the shapes that make a payload unreadable: a declared count the
// remaining bytes cannot satisfy, a count word cut off mid-read, and a
// complete early item followed by a later item missing a required field.
// All arrive with valid outer framing and checksum and must be reported as
// ErrInvalidArchive with nil records — even when a complete, readable
// prefix (a full policy change and decision) precedes the damage.
func TestArchiveInternalListCountCorruption(t *testing.T) {
	good, _, cp := listCorruptionFixture(t)
	basePayload := payloadOf(t, good)
	sites := inspectTwoRecordPayload(t, basePayload)

	// expectInvalid runs one payload mutation and asserts the whole read
	// fails as an unreadable archive without delivering the intact prefix.
	// It also proves the failed call rewrites neither its input bytes nor
	// the retained checkpoint handed to it.
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
			t.Fatalf("%s: structurally unreadable list counted as a content error: %v", name, err)
		}
		if got != nil {
			t.Fatalf("%s: returned %d records on a damaged list, want nil", name, len(got))
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

	lists := []struct {
		name     string
		site     listSite
		minBytes int
	}{
		{"records", sites.records, 22},
		{"policies", sites.policies, 6*4 + 1},
		{"roles", sites.roles, 4},
		{"matched", sites.matched, 4},
	}

	for _, l := range lists {
		// A) The count word is intact but promises more items than the bytes
		// left could ever hold. Rewrite only the count (payload length
		// unchanged): the read must fail as an unreadable archive, whether
		// the per-item floor rejects the count up front or reading the
		// missing items truncates — the error class is what the caller uses.
		//
		// This count value is the smallest one whose floor exceeds the
		// remaining bytes, computed from this fixture's real offsets.
		overCount := (len(basePayload)-(l.site.countAt+4))/l.minBytes + 1
		expectInvalid(l.name+": count exceeds remaining items", "",
			func(p []byte) []byte {
				binary.BigEndian.PutUint32(p[l.site.countAt:], uint32(overCount))
				return p
			})

		// The maximum uint32 count on this small body must fail with the
		// same deterministic error for every counted list, not just the
		// record list, and without enumerating (or provisioning) the
		// phantom items.
		expectInvalid(l.name+": max uint32 count on small body", "implausible list count",
			func(p []byte) []byte {
				binary.BigEndian.PutUint32(p[l.site.countAt:], 0xffffffff)
				return p
			})

		// C) Every early item is whole; the final item is missing its last
		// required field by one byte. Enough bytes remain to pass the coarse
		// per-item count floor, so this failure must surface while reading
		// that item — proving the item-level truncation guard, not only the
		// count bound, is in place.
		lastItemEnd := l.site.itemEnds[len(l.site.itemEnds)-1]
		expectInvalid(l.name+": later item missing required field", "truncated archive payload",
			func(p []byte) []byte { return p[:lastItemEnd-1] })
	}

	// B) A count word physically cut off mid-read. For the three inner
	// lists this needs a one-record payload: the outer record-count floor
	// (22 bytes per record) must still pass so the reader actually reaches
	// the inner list and proves its own u32 truncation guard.
	inner := buildInnerListCountTruncationPayloads(t)
	cutCountWord := func(name string, full []byte, countAt int) {
		t.Helper()
		// These are single-record payloads, so the outer record-count floor
		// (22 bytes/record) still passes at this cut; the reader must reach
		// the inner list and prove its own count-word truncation guard.
		cut := countAt + 2
		data := framePayload(full[:cut])
		got, err := DecodeAuditArchive(data, "acme", cp)
		if !errors.Is(err, ErrInvalidArchive) || got != nil {
			t.Fatalf("%s: records=%v err=%v, want ErrInvalidArchive, nil", name, got, err)
		}
		if !strings.Contains(err.Error(), "truncated archive payload") {
			t.Fatalf("%s: err = %q, want a mid-read count truncation", name, err)
		}
	}
	cutCountWord("policies count word", inner.policiesPayload, inner.policiesCountAt)
	cutCountWord("roles count word", inner.rolesPayload, inner.rolesCountAt)
	cutCountWord("matched count word", inner.matchedPayload, inner.matchedCountAt)

	// B for the outer record list: header complete, its count word cut.
	headerOnly := buildArchiveHeader(t)
	recordCountCut := append(append([]byte(nil), headerOnly...), 0x00, 0x00)
	if got, err := DecodeAuditArchive(framePayload(recordCountCut), "acme", cp); !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("records count word: records=%v err=%v, want ErrInvalidArchive, nil", got, err)
	}

	// A dedicated emphasis for the requirement: a tiny archive declaring
	// 4294967295 records must return the deterministic error promptly,
	// without crashing or provisioning as if the count were real.
	w := &archiveWriter{}
	w.stringField("acme")
	w.u32(0)
	w.stringField("g")
	w.u32(0xffffffff)
	if w.err != nil {
		t.Fatal(w.err)
	}
	tinyHuge := framePayload(w.buf)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := DecodeAuditArchive(tinyHuge, "acme", cp)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("tiny archive with max count: records=%v err=%v, want ErrInvalidArchive, nil", got, err)
	}
	if !strings.Contains(err.Error(), "implausible list count") {
		t.Fatalf("tiny archive with max count: err = %q, want count-bound rejection", err)
	}
	// A regression that trusted the count would try to provision billions
	// of records; bound what this error path may allocate.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("max-count decode allocated %d bytes, want a small error path", alloc)
	}
}

// innerListTruncation holds one complete one-record payload and the offset
// of a targeted inner list's count word.
type innerListTruncation struct {
	policiesPayload              []byte
	policiesCountAt              int
	rolesPayload, matchedPayload []byte
	rolesCountAt                 int
	matchedCountAt               int
}

// buildInnerListCountTruncationPayloads hand-builds structurally complete
// one-record payloads (fingerprints are irrelevant here because a truncated
// count fails format decoding before any content check) and reports the
// offset of each inner list's count word.
func buildInnerListCountTruncationPayloads(t *testing.T) innerListTruncation {
	t.Helper()
	var out innerListTruncation

	// One policy-change record whose policy list carries two policies.
	{
		w := &archiveWriter{}
		w.stringField("acme")
		w.u32(1)
		w.stringField("g")
		w.u32(1) // one record
		w.stringField("acme")
		w.u32(1)
		w.stringField(AuditPolicyChange)
		w.raw([]byte{archiveTagPresent}) // change present
		w.u32(1)                         // change version
		w.raw([]byte{archiveTagPresent}) // policy list present
		out.policiesCountAt = len(w.buf)
		w.u32(2)
		w.policy(allowPolicy("p1", "u1", "read", "org/a", false))
		w.policy(allowPolicy("p2", "u1", "read", "org/a", false))
		w.u32(0)           // source version
		w.boolean(false)   // rolled back
		w.raw(nilMarker()) // decision absent
		w.stringField("p") // prev fingerprint
		w.stringField("f") // own fingerprint
		if w.err != nil {
			t.Fatal(w.err)
		}
		out.policiesPayload = w.buf
	}

	// One decision record whose subject roles and decision matched list
	// each carry two strings.
	{
		w := &archiveWriter{}
		w.stringField("acme")
		w.u32(1)
		w.stringField("g")
		w.u32(1) // one record
		w.stringField("acme")
		w.u32(1)
		w.stringField(AuditDecision)
		w.raw([]byte{archiveTagNil})     // change absent
		w.raw([]byte{archiveTagPresent}) // decision present
		w.stringField("acme")            // SubjectOrg
		w.stringField("acme")            // ResourceOrg
		w.stringField("u1")              // subject ID
		w.stringField("user")            // subject kind
		w.raw([]byte{archiveTagPresent}) // roles present
		out.rolesCountAt = len(w.buf)
		w.u32(2)
		w.stringField("role-a")
		w.stringField("role-b")
		w.boolean(false) // subject disabled
		w.stringField("r1")
		w.stringField("org/a")
		w.stringField("read") // action
		w.boolean(true)       // decision allowed
		w.stringField("matched allow policy")
		w.raw([]byte{archiveTagPresent}) // matched present
		out.matchedCountAt = len(w.buf)
		w.u32(2)
		w.stringField("p1")
		w.stringField("p2")
		w.u32(1)           // decision version
		w.stringField("p") // prev fingerprint
		w.stringField("f") // own fingerprint
		if w.err != nil {
			t.Fatal(w.err)
		}
		out.rolesPayload = w.buf
		out.matchedPayload = w.buf
	}
	return out
}

// nilMarker returns the one-byte absent/nil marker as a slice.
func nilMarker() []byte { return []byte{archiveTagNil} }

// buildArchiveHeader writes just the payload header (embedded org, end
// sequence and checkpoint fingerprint) that precedes the record count.
func buildArchiveHeader(t *testing.T) []byte {
	t.Helper()
	w := &archiveWriter{}
	w.stringField("acme")
	w.u32(0)
	w.stringField("g")
	if w.err != nil {
		t.Fatal(w.err)
	}
	return w.buf
}

// TestArchiveLaterRecordListDamageDiscardsPrefix is the record-level case
// the requirement names: a complete policy change and a complete decision
// have already been read when a *later* record's inner list turns out to
// promise items its bytes cannot provide. The whole read must fail and the
// intact prefix must never be handed to the caller.
func TestArchiveLaterRecordListDamageDiscardsPrefix(t *testing.T) {
	thirdReason := "third-decision-reason-7f3a"
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version: 1, Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u1", "r1", "org/a", "read"),
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  request("acme", "u1", "r1", "org/a", "read"),
				Decision: Decision{Allowed: false, Reason: thirdReason, Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	good, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// The third record's matched list follows its unique reason string:
	// reason stringField bytes, then one list marker, then its count word.
	damaged := reframe(t, good, func(payload []byte) []byte {
		idx := bytes.Index(payload, []byte(thirdReason))
		if idx < 0 {
			t.Fatal("third-record reason not found")
		}
		if bytes.Index(payload[idx+1:], []byte(thirdReason)) >= 0 {
			t.Fatal("third-record reason must be unique for an unambiguous locate")
		}
		countAt := idx + len(thirdReason) + 1 // skip reason bytes and the list marker
		binary.BigEndian.PutUint32(payload[countAt:], 0xffffffff)
		return payload
	})

	got, err := DecodeAuditArchive(damaged, "acme", cp)
	if !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("later-record list damage: records=%v err=%v, want ErrInvalidArchive, nil", got, err)
	}
}

// TestArchiveListContentChangeIsNotArchiveDamage keeps format damage
// separate from untrusted content: a single in-list byte flipped with the
// checksum recomputed is fully readable as a format (every length and
// marker intact), so it must fail as content with ErrInvalidRange — not be
// swept into ErrInvalidArchive merely because the byte lives inside a
// policy or string list.
func TestArchiveListContentChangeIsNotArchiveDamage(t *testing.T) {
	good, _, cp := listCorruptionFixture(t)
	payload := payloadOf(t, good)
	sites := inspectTwoRecordPayload(t, payload)

	flipOneListByte := func(name string, site listSite) {
		t.Helper()
		// Flip a byte in the second item's content (past its length word),
		// leaving every length prefix and marker untouched.
		secondItem := site.itemEnds[0]
		tampered := append([]byte(nil), payload...)
		tampered[secondItem+4] ^= 0x01
		data := framePayload(tampered)
		got, err := DecodeAuditArchive(data, "acme", cp)
		if !errors.Is(err, ErrInvalidRange) || got != nil {
			t.Fatalf("%s: records=%v err=%v, want ErrInvalidRange, nil", name, got, err)
		}
		if errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("%s: readable-format content change reported as archive damage", name)
		}
	}
	flipOneListByte("matched list byte", sites.matched)
	flipOneListByte("roles list byte", sites.roles)
	flipOneListByte("policy list byte", sites.policies)
}

// TestArchiveValidListShapesRoundTrip locks the legal boundaries the
// corruption guard must not disturb: nil and non-nil empty policy/string
// lists keep their exact shape, populated lists keep order and raw bytes,
// and neither fingerprints nor the caller checkpoint change. The decoded
// material still verifies and feeds offline review unchanged.
func TestArchiveValidListShapesRoundTrip(t *testing.T) {
	req := request("acme", "u1", "r1", "org/a", "read")
	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		// Policy list shapes.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 1, Policies: nil}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version: 2, SourceVersion: 1, RolledBack: true, Policies: []Policy{},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 3, Policies: []Policy{
				// Raw, non-UTF-8 bytes and a fixed order must be preserved.
				{ID: "p" + invalidByte, Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, ResourceID: "r" + anotherInvalidByte},
				allowPolicy("p2", "u1", "read", "org/a", false),
			}}, nil
		},
		// String list shapes across subject roles and matched policies.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			r := req
			r.Subject.Roles = nil
			return AuditDecision, nil, &DecisionRecord{
				Request:  r,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: nil, Version: 3},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			r := req
			r.Subject.Roles = []string{}
			return AuditDecision, nil, &DecisionRecord{
				Request:  r,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{}, Version: 3},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			r := req
			r.Subject.Roles = []string{"zeta", "alpha"} // order is preserved, not sorted
			return AuditDecision, nil, &DecisionRecord{
				Request: r,
				Decision: Decision{Allowed: true, Reason: "matched allow policy",
					Matched: []string{"p" + invalidByte, "p2"}, Version: 3},
			}
		},
	})

	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode list shapes: %v", err)
	}
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("decode list shapes: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("valid list shapes changed across the archive:\n got %+v\nwant %+v", got, recs)
	}

	// Explicit nil-vs-empty preservation for both list kinds.
	if got[0].Change.Policies != nil {
		t.Fatal("nil policy list decoded non-nil")
	}
	if got[1].Change.Policies == nil || len(got[1].Change.Policies) != 0 {
		t.Fatal("empty non-nil policy list lost its shape")
	}
	if got[3].Decision.Request.Subject.Roles != nil || got[3].Decision.Decision.Matched != nil {
		t.Fatal("nil string lists decoded non-nil")
	}
	if got[4].Decision.Request.Subject.Roles == nil || got[4].Decision.Decision.Matched == nil {
		t.Fatal("empty non-nil string lists decoded as nil")
	}
	// Populated lists: exact order and raw bytes, including non-UTF-8.
	wantRoles := []string{"zeta", "alpha"}
	if !reflect.DeepEqual(got[5].Decision.Request.Subject.Roles, wantRoles) {
		t.Fatalf("roles order/bytes changed: got %q want %q", got[5].Decision.Request.Subject.Roles, wantRoles)
	}
	wantMatched := []string{"p" + invalidByte, "p2"}
	if !reflect.DeepEqual(got[5].Decision.Decision.Matched, wantMatched) {
		t.Fatalf("matched order/bytes changed: got %q want %q", got[5].Decision.Decision.Matched, wantMatched)
	}
	policies := got[2].Change.Policies
	if policies[0].ID != "p"+invalidByte || policies[0].ResourceID != "r"+anotherInvalidByte {
		t.Fatalf("policy raw bytes changed: id=%q resource=%q", policies[0].ID, policies[0].ResourceID)
	}

	// Fingerprints and the retained checkpoint are untouched, and the
	// material remains directly usable by offline verification.
	for i := range got {
		if got[i].Fingerprint != recs[i].Fingerprint {
			t.Fatalf("record %d fingerprint changed", i+1)
		}
	}
	if err := VerifyAudit("acme", got, cp); err != nil {
		t.Fatalf("decoded valid-list material must verify: %v", err)
	}
}
