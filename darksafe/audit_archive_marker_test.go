// This file pins down the decoder's handling of corrupted *single-byte
// markers*: archives whose outer framing, declared payload length and
// checksum are all correct, but where one marker byte inside the payload
// carries a value the format does not define. The format uses one-byte
// markers for booleans (policy Recursive, change RolledBack, subject
// Disabled, decision Allowed), for payload presence (the change and
// decision payloads) and for list presence (the policy list, the subject
// roles and the matched policy list). Only 0 and 1 are legal at every one
// of these positions.
//
// Two failure classes must stay distinguishable. A marker byte of 2, 0xff
// or any other undefined value makes the archive uninterpretable: no
// non-zero byte may be read as "true" or "present", so the whole read fails
// with ErrInvalidArchive and delivers no records — even though the
// recomputed outer checksum still matches, proving the rejection comes
// from the internal format check. Flipping a boolean between its two legal
// values instead keeps the structure readable but changes the audited
// content: against the original fingerprints and the caller's retained
// checkpoint that is an audit-chain failure, ErrInvalidRange, never a
// format error and never a deliverable record.
package darksafe

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// markerEvent is one single-byte marker located in the fixture payload:
// its human label, its payload offset and the legal value the fixture
// carries there.
type markerEvent struct {
	name  string
	at    int
	value byte
}

// markerFixture builds complete, valid material exercising every marker
// kind in both of its legal states: a publish whose two policies carry
// Recursive true and false, an allowed decision with populated roles and
// matched lists, a publish with a non-nil empty policy set, one with a nil
// policy set, a rollback (RolledBack true) and a disabled-subject decision
// with nil roles and nil matched list. The chain is semantically
// consistent, so the decoded control material still passes offline review.
func markerFixture(t *testing.T) ([]byte, []AuditRecord, Checkpoint) {
	t.Helper()
	p1 := Policy{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, Recursive: true}
	p2 := Policy{ID: "p2", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, ResourceID: "r1"}
	enabledReq := request("acme", "u1", "r1", "org/a", "read")
	enabledReq.Subject.Roles = []string{"role-a", "role-b"}
	disabledReq := request("acme", "u1", "r1", "org/a", "read")
	disabledReq.Subject.Disabled = true

	recs, cp := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 1, Policies: []Policy{p1, p2}}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  enabledReq,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1", "p2"}, Version: 1},
			}
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 2, Policies: []Policy{}}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 3, Policies: nil}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{Version: 4, SourceVersion: 1, RolledBack: true, Policies: []Policy{p1, p2}}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  disabledReq,
				Decision: Decision{Allowed: false, Reason: "subject is disabled", Version: 0},
			}
		},
	})
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatalf("encode marker fixture: %v", err)
	}
	return archive, recs, cp
}

// walkMarkerPayload mirrors archiveReader over the marker fixture payload,
// recording every single-byte marker (booleans, payload presence, list
// presence) with its offset. It exists only to locate the bytes the tests
// corrupt; it follows the markers it reads, so a format change desyncs the
// walk and fails the fixture-layout expectation loudly rather than letting
// a test corrupt the wrong field.
func walkMarkerPayload(t *testing.T, p []byte) []markerEvent {
	t.Helper()
	c := &payloadInspector{b: p}
	var events []markerEvent
	mark := func(name string) {
		// Read the offset before consuming the byte: evaluation order of the
		// two reads matters and must not be left to a composite literal.
		at := c.pos
		events = append(events, markerEvent{name: name, at: at, value: c.marker()})
	}
	last := func() byte { return events[len(events)-1].value }
	stringList := func(name string) {
		mark(name)
		if last() == archiveTagNil {
			return
		}
		n := c.u32()
		for i := 0; i < n; i++ {
			c.str()
		}
	}
	policyList := func(name string, rec int) {
		mark(name)
		if last() == archiveTagNil {
			return
		}
		n := c.u32()
		for i := 0; i < n; i++ {
			c.str() // ID
			c.str() // Subject
			c.str() // Action
			c.str() // Scope
			c.str() // Effect
			mark(fmt.Sprintf("record %d policy %d recursive", rec, i))
			c.str() // ResourceID
		}
	}

	c.str()    // embedded org
	c.pos += 4 // embedded end sequence
	c.str()    // embedded checkpoint fingerprint
	n := c.u32()
	for rec := 1; rec <= n; rec++ {
		c.str()    // org
		c.pos += 4 // seq
		c.str()    // kind
		mark(fmt.Sprintf("record %d change payload", rec))
		if last() == archiveTagPresent {
			c.pos += 4 // change version
			policyList(fmt.Sprintf("record %d policy list", rec), rec)
			c.pos += 4 // source version
			mark(fmt.Sprintf("record %d rolled back", rec))
		}
		mark(fmt.Sprintf("record %d decision payload", rec))
		if last() == archiveTagPresent {
			c.str() // request SubjectOrg
			c.str() // request ResourceOrg
			c.str() // subject ID
			c.str() // subject kind
			stringList(fmt.Sprintf("record %d subject roles", rec))
			mark(fmt.Sprintf("record %d subject disabled", rec))
			c.str() // resource ID
			c.str() // resource scope
			c.str() // action
			mark(fmt.Sprintf("record %d decision allowed", rec))
			c.str() // decision reason
			stringList(fmt.Sprintf("record %d matched policies", rec))
			c.pos += 4 // decision version
		}
		c.str() // prev fingerprint
		c.str() // own fingerprint
	}
	if c.pos != len(p) {
		t.Fatalf("marker walk ended at %d, payload length %d", c.pos, len(p))
	}
	return events
}

// markerOffset returns the payload offset of one named marker site.
func markerOffset(t *testing.T, events []markerEvent, name string) int {
	t.Helper()
	for _, ev := range events {
		if ev.name == name {
			return ev.at
		}
	}
	t.Fatalf("marker site %q not walked", name)
	return 0
}

// expectMarkerFailure reframes one mutated payload with a consistent outer
// length and checksum, then asserts the read fails with the wanted error
// class (and not the other one), delivers no records, and rewrites neither
// the archive bytes nor the retained checkpoint handed to it.
func expectMarkerFailure(t *testing.T, name string, payload []byte, cp Checkpoint, want, notWant error, wantSubstr string) {
	t.Helper()
	data := framePayload(payload)
	dataSnapshot := append([]byte(nil), data...)
	cpSnapshot := cp

	got, err := DecodeAuditArchive(data, "acme", cp)
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", name, err, want)
	}
	if errors.Is(err, notWant) {
		t.Fatalf("%s: err = %v must not be %v", name, err, notWant)
	}
	if got != nil {
		t.Fatalf("%s: returned %d records on a damaged marker, want nil", name, len(got))
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

// TestArchiveMarkerFixtureLayout locks the fixture's marker map: every
// marker kind appears, in both legal states, exactly where the walker
// expects it. A format or fixture change breaks this test instead of
// silently moving the corruption targets of the other tests in this file.
func TestArchiveMarkerFixtureLayout(t *testing.T) {
	archive, _, _ := markerFixture(t)
	events := walkMarkerPayload(t, payloadOf(t, archive))

	want := []struct {
		name  string
		value byte
	}{
		{"record 1 change payload", archiveTagPresent},
		{"record 1 policy list", archiveTagPresent},
		{"record 1 policy 0 recursive", archiveTagBoolTrue},
		{"record 1 policy 1 recursive", archiveTagBoolFalse},
		{"record 1 rolled back", archiveTagBoolFalse},
		{"record 1 decision payload", archiveTagNil},
		{"record 2 change payload", archiveTagNil},
		{"record 2 decision payload", archiveTagPresent},
		{"record 2 subject roles", archiveTagPresent},
		{"record 2 subject disabled", archiveTagBoolFalse},
		{"record 2 decision allowed", archiveTagBoolTrue},
		{"record 2 matched policies", archiveTagPresent},
		{"record 3 change payload", archiveTagPresent},
		{"record 3 policy list", archiveTagPresent}, // non-nil empty set
		{"record 3 rolled back", archiveTagBoolFalse},
		{"record 3 decision payload", archiveTagNil},
		{"record 4 change payload", archiveTagPresent},
		{"record 4 policy list", archiveTagNil},
		{"record 4 rolled back", archiveTagBoolFalse},
		{"record 4 decision payload", archiveTagNil},
		{"record 5 change payload", archiveTagPresent},
		{"record 5 policy list", archiveTagPresent},
		{"record 5 policy 0 recursive", archiveTagBoolTrue},
		{"record 5 policy 1 recursive", archiveTagBoolFalse},
		{"record 5 rolled back", archiveTagBoolTrue},
		{"record 5 decision payload", archiveTagNil},
		{"record 6 change payload", archiveTagNil},
		{"record 6 decision payload", archiveTagPresent},
		{"record 6 subject roles", archiveTagNil},
		{"record 6 subject disabled", archiveTagBoolTrue},
		{"record 6 decision allowed", archiveTagBoolFalse},
		{"record 6 matched policies", archiveTagNil},
	}
	if len(events) != len(want) {
		t.Fatalf("walked %d marker sites, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].name != w.name || events[i].value != w.value {
			t.Fatalf("marker %d = %+v, want %s with value %d", i, events[i], w.name, w.value)
		}
	}
}

// TestArchiveInvalidMarkerBytesRejected corrupts every single-byte marker
// in turn to a value the format does not define (2 and 0xff). The outer
// length and checksum are recomputed, so they still validate: the
// rejection must come from the internal marker check, surface as
// ErrInvalidArchive (matched by errors.Is, and never as the content error
// ErrInvalidRange), and deliver no records — an arbitrary non-zero byte is
// not "true" and not "present".
func TestArchiveInvalidMarkerBytesRejected(t *testing.T) {
	archive, _, cp := markerFixture(t)
	basePayload := payloadOf(t, archive)
	events := walkMarkerPayload(t, basePayload)

	for _, ev := range events {
		for _, bad := range []byte{2, 0xff} {
			payload := append([]byte(nil), basePayload...)
			payload[ev.at] = bad
			expectMarkerFailure(t, fmt.Sprintf("%s -> %d", ev.name, bad),
				payload, cp, ErrInvalidArchive, ErrInvalidRange, "marker")
		}
	}
}

// TestArchiveBooleanFlipChangesContentNotFormat flips every boolean marker
// between its two legal values. The archive stays structurally readable —
// every marker, length and count is intact — but the audited content no
// longer matches the original fingerprints, so against the caller's
// retained checkpoint the read must fail as ErrInvalidRange, never be
// reported as archive-format damage, and deliver no records.
func TestArchiveBooleanFlipChangesContentNotFormat(t *testing.T) {
	archive, _, cp := markerFixture(t)
	basePayload := payloadOf(t, archive)
	events := walkMarkerPayload(t, basePayload)

	isBool := func(name string) bool {
		return strings.Contains(name, "recursive") ||
			strings.Contains(name, "rolled back") ||
			strings.Contains(name, "disabled") ||
			strings.Contains(name, "allowed")
	}
	flipped := 0
	for _, ev := range events {
		if !isBool(ev.name) {
			continue
		}
		flipped++
		payload := append([]byte(nil), basePayload...)
		payload[ev.at] = ev.value ^ 1 // 0 becomes 1, 1 becomes 0: still a legal marker
		expectMarkerFailure(t, ev.name+" flipped", payload, cp, ErrInvalidRange, ErrInvalidArchive, "")
	}
	if flipped != 12 {
		t.Fatalf("flipped %d boolean sites, want the 12 the fixture carries", flipped)
	}
}

// TestArchiveLaterRecordMarkerDamageDiscardsPrefix is the all-or-nothing
// boundary: every record but the last decodes cleanly, yet one illegal
// marker in the final record fails the whole read. The intact prefix is
// proved readable on its own, so a nil result for the full archive can
// only mean the prefix was deliberately withheld.
func TestArchiveLaterRecordMarkerDamageDiscardsPrefix(t *testing.T) {
	archive, recs, cp := markerFixture(t)
	basePayload := payloadOf(t, archive)
	events := walkMarkerPayload(t, basePayload)

	// The first five records, archived with their own checkpoint, read and
	// review fine: the prefix really is intact.
	prefixCP := Checkpoint{Org: "acme", EndSeq: 5, Fingerprint: recs[4].Fingerprint}
	prefixArchive, err := EncodeAuditArchive("acme", recs[:5], prefixCP)
	if err != nil {
		t.Fatalf("encode prefix: %v", err)
	}
	prefix, err := DecodeAuditArchive(prefixArchive, "acme", prefixCP)
	if err != nil || len(prefix) != 5 {
		t.Fatalf("intact prefix must decode: %d records, %v", len(prefix), err)
	}
	if review, err := RecheckDecisionOffline("acme", prefix, prefixCP, 2); err != nil || !review.Consistent {
		t.Fatalf("intact prefix must review: %+v, %v", review, err)
	}

	// An illegal boolean marker and an illegal list marker in the last
	// record each sink the entire read; no record prefix comes back.
	for _, site := range []string{"record 6 decision allowed", "record 6 matched policies", "record 6 change payload"} {
		payload := append([]byte(nil), basePayload...)
		payload[markerOffset(t, events, site)] = 0xff
		expectMarkerFailure(t, site, payload, cp, ErrInvalidArchive, ErrInvalidRange, "marker")
	}
}

// TestArchiveUndamagedMarkersRoundTrip is the control: the same fixture
// with no byte touched decodes cleanly, keeps every boolean's exact value
// and the nil versus non-nil-empty versus populated distinction of every
// list, and still feeds the existing offline decision review.
func TestArchiveUndamagedMarkersRoundTrip(t *testing.T) {
	archive, recs, cp := markerFixture(t)
	got, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("decode control: %v", err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("control round trip changed the records:\n got %+v\nwant %+v", got, recs)
	}

	// Booleans keep their exact values in both states.
	if !got[0].Change.Policies[0].Recursive || got[0].Change.Policies[1].Recursive {
		t.Fatal("policy Recursive booleans changed across the archive")
	}
	if got[0].Change.RolledBack || !got[4].Change.RolledBack {
		t.Fatal("RolledBack booleans changed across the archive")
	}
	if got[1].Decision.Request.Subject.Disabled || !got[5].Decision.Request.Subject.Disabled {
		t.Fatal("subject Disabled booleans changed across the archive")
	}
	if !got[1].Decision.Decision.Allowed || got[5].Decision.Decision.Allowed {
		t.Fatal("decision Allowed booleans changed across the archive")
	}

	// Nil, non-nil empty and populated lists keep their distinct shapes.
	if got[2].Change.Policies == nil || len(got[2].Change.Policies) != 0 {
		t.Fatal("non-nil empty policy set lost its shape")
	}
	if got[3].Change.Policies != nil {
		t.Fatal("nil policy set decoded as non-nil")
	}
	if got[5].Decision.Request.Subject.Roles != nil || got[5].Decision.Decision.Matched != nil {
		t.Fatal("nil roles/matched lists decoded as non-nil")
	}
	if want := []string{"role-a", "role-b"}; !reflect.DeepEqual(got[1].Decision.Request.Subject.Roles, want) {
		t.Fatalf("roles = %q, want %q", got[1].Decision.Request.Subject.Roles, want)
	}
	if want := []string{"p1", "p2"}; !reflect.DeepEqual(got[1].Decision.Decision.Matched, want) {
		t.Fatalf("matched = %q, want %q", got[1].Decision.Decision.Matched, want)
	}

	// The decoded material still serves the existing offline review, for
	// the allowed decision and for the disabled-subject denial alike.
	allowReview, err := RecheckDecisionOffline("acme", got, cp, 2)
	if err != nil || !allowReview.Consistent || !allowReview.Recomputed.Allowed {
		t.Fatalf("control allow review = %+v, %v", allowReview, err)
	}
	denyReview, err := RecheckDecisionOffline("acme", got, cp, 6)
	if err != nil || !denyReview.Consistent || denyReview.Recomputed.Allowed {
		t.Fatalf("control disabled-subject review = %+v, %v", denyReview, err)
	}
}
