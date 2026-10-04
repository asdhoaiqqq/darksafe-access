// This file pins down the decoder's handling of corrupted *single-byte
// markers* inside an otherwise intact archive. An archive uses one-byte
// markers in two roles:
//
//   - a boolean marker (0/1) for every bool the material carries — whether a
//     policy is recursive, whether a change came from a rollback, whether a
//     subject is disabled and whether a decision allowed the access; and
//   - a presence marker (0/1) telling nil apart from a present value: the
//     change and decision payloads of each record, a change's policy list,
//     and a decision's subject-role and matched-policy lists.
//
// Only the bytes 0 and 1 are legal at any of those positions. Every
// corruption below is reframed with a consistent outer length and checksum,
// so exercising these cases proves the rejection comes from the payload's
// own format check rather than the framing-level samples in
// audit_archive_test.go or the list-count guards in
// audit_archive_listcorrupt_test.go: a decoder that treated "any non-zero
// byte as true/present" would accept these. A value outside {0,1} must fail
// the whole read with ErrInvalidArchive and no records — never a decoded
// decision that merely looks usable.
//
// A different failure is kept separate: flipping a legal boolean byte
// between 0 and 1 leaves the format fully readable, but the content no
// longer matches the retained fingerprint; such material must fail as
// ErrInvalidRange through the audit-chain check, not as archive damage.
package darksafe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// markerAt is one single-byte marker's position in a payload and the legal
// value a valid encoding carries there.
type markerAt struct {
	name string
	at   int
	want byte // the legal byte the valid fixture writes here
}

// markerFixtureSites locates every single-byte marker the three-record
// fixture built by markerCorruptionFixture carries, in payload byte
// offsets. It mirrors archiveWriter and the three field tables, so a format
// change moves these tests off their target rather than silently
// corrupting the wrong byte.
type markerFixtureSites struct {
	// The two payload presence markers per record (change, decision).
	change1, decision1 markerAt
	change2, decision2 markerAt
	change3, decision3 markerAt

	// Record 1 booleans and its policy-list marker.
	recursive11 markerAt // policy[0].Recursive = false
	recursive12 markerAt // policy[1].Recursive = true
	rolledBack1 markerAt // change.RolledBack = false
	policies1   markerAt // policy list present

	// Record 2: an ordinary allowed decision.
	disabled2 markerAt // false
	allowed2  markerAt // true
	roles2    markerAt // present, populated
	matched2  markerAt // present, populated

	// Record 3: an envelope-rejected disabled-subject decision.
	disabled3 markerAt // true
	allowed3  markerAt // false
	roles3    markerAt // present, populated (request field, never evaluated)
	matched3  markerAt // nil: an envelope denial carries no matched list
}

// markerInspector walks the known-good fixture payload in encode order. It
// exists only to locate marker bytes; every visited marker is cross-checked
// against the legal value the fixture must carry there.
type markerInspector struct {
	t   *testing.T
	b   []byte
	pos int
}

func (c *markerInspector) u32() int {
	v := int(binary.BigEndian.Uint32(c.b[c.pos : c.pos+4]))
	c.pos += 4
	return v
}

func (c *markerInspector) str() {
	n := c.u32()
	c.pos += n
}

// marker consumes one marker byte, asserts it is the expected legal value,
// and records its position.
func (c *markerInspector) marker(name string, want byte) markerAt {
	m := c.b[c.pos]
	site := markerAt{name: name, at: c.pos, want: want}
	c.pos++
	if m != want {
		c.t.Fatalf("marker inspector: %s byte = %d, want %d (fixture drift)", name, m, want)
	}
	return site
}

// presentStringList walks a present string list whose marker the caller
// already consumed.
func (c *markerInspector) presentStringList() {
	n := c.u32()
	for i := 0; i < n; i++ {
		c.str()
	}
}

// policy walks one encoded policy in the single policy field table's order
// (five strings, the Recursive bool, the always-archived ResourceID) and
// reports the bool byte's site.
func (c *markerInspector) policy(name string, wantRecursive byte) markerAt {
	c.str() // ID
	c.str() // Subject
	c.str() // Action
	c.str() // Scope
	c.str() // Effect
	site := markerAt{name: name, at: c.pos, want: wantRecursive}
	if got := c.b[c.pos]; got != wantRecursive {
		c.t.Fatalf("marker inspector: %s byte = %d, want %d (fixture drift)", name, got, wantRecursive)
	}
	c.pos++ // Recursive bool
	c.str() // ResourceID
	return site
}

// inspectMarkerFixturePayload locates every marker of the three-record
// fixture built by markerCorruptionFixture.
func inspectMarkerFixturePayload(t *testing.T, p []byte) markerFixtureSites {
	t.Helper()
	c := &markerInspector{t: t, b: p}
	c.str()    // embedded org
	c.pos += 4 // embedded end sequence
	c.str()    // embedded checkpoint fingerprint
	if n := c.u32(); n != 3 {
		t.Fatalf("fixture record count = %d, want 3", n)
	}

	// Record 1: publish of two policies (one flat, one recursive).
	c.str()    // org
	c.pos += 4 // seq
	c.str()    // kind
	s := markerFixtureSites{}
	s.change1 = c.marker("record 1 change payload", archiveTagPresent)
	c.pos += 4 // change version
	s.policies1 = c.marker("record 1 policy list", archiveTagPresent)
	if n := c.u32(); n != 2 {
		t.Fatalf("record 1 policy count = %d, want 2", n)
	}
	s.recursive11 = c.policy("policy[0].Recursive", 0)
	s.recursive12 = c.policy("policy[1].Recursive", 1)
	c.pos += 4 // source version
	s.rolledBack1 = c.marker("change.RolledBack", 0)
	s.decision1 = c.marker("record 1 decision payload", archiveTagNil)
	c.str() // prev fingerprint
	c.str() // own fingerprint

	// Record 2: an ordinary allowed decision with roles and matched policies.
	c.str()    // org
	c.pos += 4 // seq
	c.str()    // kind
	s.change2 = c.marker("record 2 change payload", archiveTagNil)
	s.decision2 = c.marker("record 2 decision payload", archiveTagPresent)
	c.str() // request SubjectOrg
	c.str() // request ResourceOrg
	c.str() // subject ID
	c.str() // subject Kind
	s.roles2 = c.marker("record 2 subject roles", archiveTagPresent)
	c.presentStringList()
	s.disabled2 = c.marker("record 2 subject.Disabled", 0)
	c.str() // resource ID
	c.str() // resource scope
	c.str() // action
	s.allowed2 = c.marker("record 2 decision.Allowed", 1)
	c.str() // decision reason
	s.matched2 = c.marker("record 2 matched list", archiveTagPresent)
	c.presentStringList()
	c.pos += 4 // decision version
	c.str()    // prev fingerprint
	c.str()    // own fingerprint

	// Record 3: a disabled-subject envelope denial (version 0, no matches).
	c.str()    // org
	c.pos += 4 // seq
	c.str()    // kind
	s.change3 = c.marker("record 3 change payload", archiveTagNil)
	s.decision3 = c.marker("record 3 decision payload", archiveTagPresent)
	c.str() // request SubjectOrg
	c.str() // request ResourceOrg
	c.str() // subject ID
	c.str() // subject Kind
	s.roles3 = c.marker("record 3 subject roles", archiveTagPresent)
	c.presentStringList()
	s.disabled3 = c.marker("record 3 subject.Disabled", 1)
	c.str() // resource ID
	c.str() // resource scope
	c.str() // action
	s.allowed3 = c.marker("record 3 decision.Allowed", 0)
	c.str() // decision reason
	s.matched3 = c.marker("record 3 matched list", archiveTagNil)
	c.pos += 4 // decision version (0)
	c.str()    // prev fingerprint
	c.str()    // own fingerprint

	if c.pos != len(p) {
		t.Fatalf("inspector ended at %d, payload length %d", c.pos, len(p))
	}
	return s
}

// markerCorruptionFixture builds complete, valid, store-produced material
// exercising both values of every boolean and both marker shapes:
//
//   - seq 1 is a publish of two policies, one flat (Recursive=false) and one
//     recursive (Recursive=true); the change is present, not a rollback,
//     its policy list present and populated, and the decision payload
//     absent.
//   - seq 2 is an ordinary allowed decision (Allowed=true) whose request
//     carries roles and whose decision carries matched policies; the
//     disabled flag is false.
//   - seq 3 is the recorded denial of a disabled subject
//     (Disabled=true, Allowed=false): an envelope rejection with version 0
//     and a nil matched list, while the request still carries roles.
//
// It is exported and archived through the public entry points and must
// decode and re-review consistently before any mutation.
func markerCorruptionFixture(t *testing.T) ([]byte, []AuditRecord, Checkpoint) {
	t.Helper()
	s := NewStore()
	s.Publish("acme", 0, []Policy{
		allowPolicy("p-flat", "u1", "read", "org/a", false),
		allowPolicy("p-rec", "u1", "read", "org/a", true),
	})
	allowedReq := request("acme", "u1", "r1", "org/a", "read")
	allowedReq.Subject.Roles = []string{"reader"}
	allowed := s.Decide("acme", allowedReq)
	if !allowed.Allowed || allowed.Reason != "matched allow policy" {
		t.Fatalf("fixture allowed decision = %+v", allowed)
	}
	disabledReq := request("acme", "u1", "r1", "org/a", "read")
	disabledReq.Subject.Roles = []string{"reader"}
	disabledReq.Subject.Disabled = true
	disabled := s.Decide("acme", disabledReq)
	if disabled.Allowed || disabled.Reason != "subject is disabled" || disabled.Version != 0 {
		t.Fatalf("fixture disabled decision = %+v, want the version-0 disabled denial", disabled)
	}
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	for _, seq := range []int{2, 3} {
		review, err := RecheckDecisionOffline("acme", got, cp, seq)
		if err != nil || !review.Consistent {
			t.Fatalf("fixture review seq %d = %+v, %v", seq, review, err)
		}
	}
	return archive, recs, cp
}

// markerCorruptionCases lists every single-byte marker the requirement
// names, across both record kinds: four bool positions (recursive probed at
// both its legal values), both payload markers of all three records, the
// change policy list, and the subject-role and matched-policy lists in
// their present and nil shapes.
func markerCorruptionCases(s markerFixtureSites) []markerAt {
	return []markerAt{
		s.recursive11, s.recursive12, s.rolledBack1,
		s.disabled2, s.allowed2, s.disabled3, s.allowed3,
		s.change1, s.decision1,
		s.change2, s.decision2,
		s.change3, s.decision3,
		s.policies1,
		s.roles2, s.matched2,
		s.roles3, s.matched3,
	}
}

// TestArchiveIllegalSingleByteMarkersRejected proves, for every boolean and
// every presence marker in both record kinds, that a byte other than 0/1
// (2 and 255 named in the requirement, plus representative other values)
// makes the archive unreadable even with a correct outer length and
// checksum: DecodeAuditArchive returns ErrInvalidArchive (matched by
// errors.Is, and never ErrInvalidRange) and no records. The failed call
// rewrites neither the input bytes nor the independently retained
// checkpoint.
func TestArchiveIllegalSingleByteMarkersRejected(t *testing.T) {
	good, _, cp := markerCorruptionFixture(t)
	basePayload := payloadOf(t, good)
	sites := inspectMarkerFixturePayload(t, basePayload)

	// Illegal byte values spanning the ranges the rule excludes: the two
	// values the requirement names and samples on both sides of 0x80.
	illegalValues := []byte{2, 3, 127, 128, 254, 255}

	for _, site := range markerCorruptionCases(sites) {
		if site.want != 0 && site.want != 1 {
			t.Fatalf("internal fixture error: %s legal byte = %d", site.name, site.want)
		}
		for _, bad := range illegalValues {
			payload := append([]byte(nil), basePayload...)
			payload[site.at] = bad
			data := framePayload(payload)
			dataSnapshot := append([]byte(nil), data...)
			cpSnapshot := cp

			got, err := DecodeAuditArchive(data, "acme", cp)
			if !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("%s <- %d: err = %v, want ErrInvalidArchive", site.name, bad, err)
			}
			if errors.Is(err, ErrInvalidRange) {
				t.Fatalf("%s <- %d: structurally illegal marker counted as a content error: %v", site.name, bad, err)
			}
			if got != nil {
				t.Fatalf("%s <- %d: returned %d records, want nil", site.name, bad, len(got))
			}
			if !bytes.Equal(data, dataSnapshot) {
				t.Fatalf("%s <- %d: failed decode rewrote the archive bytes", site.name, bad)
			}
			if cp != cpSnapshot {
				t.Fatalf("%s <- %d: failed decode rewrote the checkpoint", site.name, bad)
			}
		}
	}
}

// TestArchiveLegalBooleanFlipsAreContentErrors keeps format damage separate
// from altered content: flipping a legal boolean byte between 0 and 1
// leaves every length, marker and frame intact and the checksum is
// recomputed, so the material stays fully readable as a format. Its audit
// content has nonetheless changed, so read against the original retained
// fingerprints and external checkpoint it must fail the whole read with
// ErrInvalidRange — never be swept into ErrInvalidArchive — and deliver no
// records.
func TestArchiveLegalBooleanFlipsAreContentErrors(t *testing.T) {
	good, _, cp := markerCorruptionFixture(t)
	basePayload := payloadOf(t, good)
	sites := inspectMarkerFixturePayload(t, basePayload)

	flips := []markerAt{
		sites.recursive11, // false -> true (record 1 content)
		sites.recursive12, // true -> false (record 1 content)
		sites.rolledBack1, // false -> true (record 1 content)
		sites.disabled2,   // false -> true (record 2 content)
		sites.allowed2,    // true -> false (record 2 content)
		sites.disabled3,   // true -> false (record 3 content)
		sites.allowed3,    // false -> true (record 3 content)
	}
	for _, site := range flips {
		payload := append([]byte(nil), basePayload...)
		payload[site.at] ^= 1 // legal 0<->1 flip; payload length unchanged
		data := framePayload(payload)

		got, err := DecodeAuditArchive(data, "acme", cp)
		if !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("%s flip: err = %v, want ErrInvalidRange", site.name, err)
		}
		if errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("%s flip: a readable-format content change reported as archive damage: %v", site.name, err)
		}
		if got != nil {
			t.Fatalf("%s flip: returned %d records on chain mismatch, want nil", site.name, len(got))
		}
	}
}

// TestArchiveLaterRecordIllegalMarkerDiscardsPrefix is the read-boundary
// case the requirement names: earlier publish and decision records are all
// intact, and only a later record carries an illegal marker. The complete
// read must still fail with ErrInvalidArchive and never hand back the
// readable prefix as a successful result; the input bytes and external
// checkpoint stay untouched.
func TestArchiveLaterRecordIllegalMarkerDiscardsPrefix(t *testing.T) {
	thirdReason := "third-decision-late-marker-9c2e"
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
	// reason bytes, then one presence marker. Rewrite only that marker and
	// reframe with a consistent outer length and checksum.
	damaged := reframe(t, good, func(payload []byte) []byte {
		idx := bytes.Index(payload, []byte(thirdReason))
		if idx < 0 {
			t.Fatal("third-record reason not found")
		}
		if bytes.Index(payload[idx+1:], []byte(thirdReason)) >= 0 {
			t.Fatal("third-record reason must be unique for an unambiguous locate")
		}
		marker := idx + len(thirdReason)
		if payload[marker] != archiveTagPresent {
			t.Fatalf("matched list marker = %d, want present", payload[marker])
		}
		payload[marker] = 255
		return payload
	})

	dataSnapshot := append([]byte(nil), damaged...)
	cpSnapshot := cp
	got, err := DecodeAuditArchive(damaged, "acme", cp)
	if !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("later-record illegal marker: records=%v err=%v, want ErrInvalidArchive, nil", got, err)
	}
	if errors.Is(err, ErrInvalidRange) {
		t.Fatalf("later-record illegal marker counted as a content error: %v", err)
	}
	if !bytes.Equal(damaged, dataSnapshot) {
		t.Fatal("failed decode rewrote the archive bytes")
	}
	if cp != cpSnapshot {
		t.Fatal("failed decode rewrote the checkpoint")
	}
}

// TestArchiveMarkerDamageNeverDefersToEmbeddedCheckpoint proves the
// checkpoint carried inside the file can never substitute for the
// caller's retained evidence: an illegal marker is rejected at the format
// layer while the embedded checkpoint is intact (the file never gets far
// enough to trust it), and a legal bool flip plus a rewritten, plausible
// embedded end sequence still fails against the retained checkpoint.
func TestArchiveMarkerDamageNeverDefersToEmbeddedCheckpoint(t *testing.T) {
	good, _, cp := markerCorruptionFixture(t)
	basePayload := payloadOf(t, good)
	sites := inspectMarkerFixturePayload(t, basePayload)

	// Illegal marker with the embedded checkpoint byte-for-byte intact and a
	// valid outer checksum: the format check must reject it first.
	illegal := append([]byte(nil), basePayload...)
	illegal[sites.allowed2.at] = 2
	if got, err := DecodeAuditArchive(framePayload(illegal), "acme", cp); !errors.Is(err, ErrInvalidArchive) || got != nil {
		t.Fatalf("illegal marker with intact embedded checkpoint: records=%v err=%v", got, err)
	}

	// Legal flip plus a plausible embedded end sequence: only the retained
	// checkpoint may decide, and it still does not match the altered chain.
	rewritten := reframe(t, good, func(payload []byte) []byte {
		payload[sites.allowed2.at] = 1
		pos := 4 + len("acme") // header: u32 orgLen, org, u32 endSeq
		binary.BigEndian.PutUint32(payload[pos:pos+4], 999)
		return payload
	})
	if got, err := DecodeAuditArchive(rewritten, "acme", cp); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("flipped bool with rewritten embedded checkpoint: records=%v err=%v, want ErrInvalidRange", got, err)
	}
}

// TestArchiveValidMarkerFixtureRoundTrip is the uncorrupted control the
// guards must never disturb: the store-built fixture decodes with both
// boolean values and the present/nil list shapes intact, verifies against
// the retained checkpoint, and still serves offline review. A second
// synthetic material locks the nil / non-nil-empty / populated shapes of
// every list across both payload kinds, with every bool taking both
// values, and stays consistent under offline review.
func TestArchiveValidMarkerFixtureRoundTrip(t *testing.T) {
	archive, recs, cp := markerCorruptionFixture(t)
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("valid fixture decode: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("valid fixture changed across the archive:\n got %+v\nwant %+v", got, recs)
	}

	// Record 1: change present, decision absent, recursive both values.
	if got[0].Change == nil || got[0].Decision != nil {
		t.Fatal("record 1 payload shapes changed")
	}
	pols := got[0].Change.Policies
	if pols == nil || len(pols) != 2 || pols[0].Recursive || !pols[1].Recursive {
		t.Fatalf("policy list or Recursive values lost: %+v", pols)
	}
	if got[0].Change.RolledBack {
		t.Fatal("RolledBack=false decoded as true")
	}
	// Record 2: allowed decision with roles and matches.
	if got[1].Change != nil || got[1].Decision == nil {
		t.Fatal("record 2 payload shapes changed")
	}
	d2 := got[1].Decision
	if d2.Request.Subject.Disabled || !d2.Decision.Allowed {
		t.Fatalf("record 2 booleans lost: disabled=%v allowed=%v", d2.Request.Subject.Disabled, d2.Decision.Allowed)
	}
	if !reflect.DeepEqual(d2.Request.Subject.Roles, []string{"reader"}) {
		t.Fatalf("record 2 roles lost: %q", d2.Request.Subject.Roles)
	}
	if len(d2.Decision.Matched) != 2 {
		t.Fatalf("record 2 matched list lost: %q", d2.Decision.Matched)
	}
	// Record 3: disabled envelope denial, version 0, nil matched list.
	d3 := got[2].Decision
	if d3 == nil {
		t.Fatal("record 3 decision absent")
	}
	if !d3.Request.Subject.Disabled || d3.Decision.Allowed || d3.Decision.Version != 0 {
		t.Fatalf("record 3 booleans/version lost: %+v", d3)
	}
	if d3.Decision.Matched != nil {
		t.Fatalf("record 3 nil matched list decoded non-nil: %q", d3.Decision.Matched)
	}
	if !reflect.DeepEqual(d3.Request.Subject.Roles, []string{"reader"}) {
		t.Fatalf("record 3 roles lost: %q", d3.Request.Subject.Roles)
	}

	if err := VerifyAudit("acme", got, cp); err != nil {
		t.Fatalf("decoded fixture must verify: %v", err)
	}
	for _, seq := range []int{2, 3} {
		review, err := RecheckDecisionOffline("acme", got, cp, seq)
		if err != nil || !review.Consistent {
			t.Fatalf("fixture must remain reviewable at seq %d: %+v, %v", seq, review, err)
		}
	}

	// Explicit nil vs non-nil-empty vs populated list shapes across both
	// payload kinds, with every boolean taking both values.
	baseReq := request("acme", "u1", "r1", "org/a", "read")
	shapes, shapeCP := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		// Policy lists: nil, non-nil empty, populated (recursive=false/true).
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 1, Policies: nil, RolledBack: false}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version: 2, SourceVersion: 1, RolledBack: true, Policies: []Policy{},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 3, Policies: []Policy{
				allowPolicy("p1", "u1", "read", "org/a", false),
				allowPolicy("p2", "u1", "read", "org/a", true),
			}}, nil
		},
		// Roles/matched nil on an allowed version-3 decision.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			r := baseReq
			return AuditDecision, nil, &DecisionRecord{
				Request:  r,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1", "p2"}, Version: 3},
			}
		},
		// Non-nil empty roles and matched lists on a no-match decision.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			r := baseReq
			r.Subject.ID = "u9"
			r.Subject.Roles = []string{}
			return AuditDecision, nil, &DecisionRecord{
				Request:  r,
				Decision: Decision{Allowed: false, Reason: "no matching allow policy", Matched: []string{}, Version: 3},
			}
		},
		// Populated roles on a disabled envelope denial with a nil matched list.
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			r := baseReq
			r.Subject.Roles = []string{"reader"}
			r.Subject.Disabled = true
			return AuditDecision, nil, &DecisionRecord{
				Request:  r,
				Decision: Decision{Allowed: false, Reason: "subject is disabled", Matched: nil, Version: 0},
			}
		},
	})
	arch, err := EncodeAuditArchive("acme", shapes, shapeCP)
	if err != nil {
		t.Fatalf("encode shapes: %v", err)
	}
	back, err := DecodeAuditArchive(arch, "acme", shapeCP)
	if err != nil {
		t.Fatalf("decode shapes: %v", err)
	}
	if !reflect.DeepEqual(back, shapes) {
		t.Fatalf("list/boolean shapes changed:\n got %+v\nwant %+v", back, shapes)
	}
	if back[0].Change.Policies != nil {
		t.Fatal("nil policy list decoded non-nil")
	}
	if back[1].Change.Policies == nil || len(back[1].Change.Policies) != 0 {
		t.Fatal("non-nil empty policy list lost its shape")
	}
	if !back[1].Change.RolledBack || back[0].Change.RolledBack {
		t.Fatal("RolledBack values lost")
	}
	if len(back[2].Change.Policies) != 2 || !back[2].Change.Policies[1].Recursive {
		t.Fatal("populated recursive policy lost")
	}
	if back[3].Decision.Request.Subject.Roles != nil {
		t.Fatal("nil roles decoded non-nil")
	}
	if back[4].Decision.Request.Subject.Roles == nil || back[4].Decision.Decision.Matched == nil {
		t.Fatal("non-nil empty lists decoded as nil")
	}
	if back[5].Decision.Decision.Matched != nil || !back[5].Decision.Request.Subject.Disabled {
		t.Fatal("record 6 nil matched list / disabled flag lost")
	}

	// The decoded control material verifies and stays usable for the existing
	// offline decision review at every decision sequence.
	if err := VerifyAudit("acme", back, shapeCP); err != nil {
		t.Fatalf("shape material must verify: %v", err)
	}
	for _, seq := range []int{4, 5, 6} {
		review, err := RecheckDecisionOffline("acme", back, shapeCP, seq)
		if err != nil || !review.Consistent {
			t.Fatalf("shape material review seq %d = %+v, %v", seq, review, err)
		}
	}
}
