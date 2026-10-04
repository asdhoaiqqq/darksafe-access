// This file is the single definition of which outer fields an AuditRecord
// carries for every audit-side treatment of record envelope content: the
// fingerprint's raw canonical encoder, the valid-UTF-8 test that chooses
// between the JSON and raw fingerprint families, the valid-UTF-8 JSON
// envelope itself, and the archive writer and reader. Each of those walks
// the one table below instead of naming the organization, sequence,
// category, payloads and predecessor fingerprint itself, so the fields and
// their order mean the same thing everywhere. Adding or changing an outer
// field here reaches fingerprinting, the UTF-8 decision, archiving and
// unarchiving at once; no audit path can keep a stale field list of its own.
//
// The table order is part of the wire and fingerprint contract: it is the
// historical envelope order — the organization, the gapless sequence, the
// category, the two payloads and then the predecessor fingerprint — and the
// archive mirrors it field for field. Nested policy, policy-change and
// decision content stays governed by its own single field tables
// (policy_fields.go, change_fields.go, decision_fields.go). Record creation
// (appendAuditLocked) and chain validation (VerifyAudit) are intentionally
// not driven from this table: they name fields for chain-semantic reasons
// (sequence continuity, category/payload agreement, the predecessor link),
// not serialization ones.
//
// One outer field is deliberately absent: AuditRecord.Fingerprint. A
// record's current fingerprint is the output of the hash and never its own
// input, so it must not protect itself; the archive still writes and reads
// it at the tail of each record separately (audit_archive.go).
package darksafe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// recordFieldKind says how one outer record field participates in
// fingerprinting and archiving.
type recordFieldKind int

const (
	// recordFieldString is an outer string written from its exact bytes by
	// the raw encoder and as a JSON string by the legacy encoder.
	recordFieldString recordFieldKind = iota
	// recordFieldInt is a non-negative integer (the gapless sequence). It
	// has no UTF-8 representation, so the valid-UTF-8 scan skips it while
	// both encoders write it.
	recordFieldInt
	// recordFieldChangePayload is the policy-change payload. The legacy
	// JSON envelope carries it under an omitempty tag, and the raw encoder
	// reaches its content through the single policy-change field table.
	recordFieldChangePayload
	// recordFieldDecisionPayload is the decision payload. The legacy JSON
	// envelope carries it under an omitempty tag, and the raw encoder
	// reaches its content through the single decision field table.
	recordFieldDecisionPayload
)

// recordFieldSpec locates one protected outer AuditRecord field. The
// accessors used depend on the kind: string fields use getString/setString,
// the sequence uses getInt/setInt, and the two payloads use
// getChange/setChange and getDecision/setDecision. A field appears in
// recordFields exactly once, and that entry is the only place its struct
// member is named for serialization.
//
// jsonKey records the legacy envelope member name; the JSON family uses it
// to place the table-driven value into the right envelope member. The raw
// encoder needs no key: its field markers are fixed by the kind exactly as
// the historical encoding emitted them.
type recordFieldSpec struct {
	kind    recordFieldKind
	jsonKey string

	getString   func(*AuditRecord) string
	setString   func(*AuditRecord, string)
	getInt      func(*AuditRecord) int
	setInt      func(*AuditRecord, int)
	getChange   func(*AuditRecord) *PolicyChange
	setChange   func(*AuditRecord, *PolicyChange)
	getDecision func(*AuditRecord) *DecisionRecord
	setDecision func(*AuditRecord, *DecisionRecord)
}

// recordFields lists every outer field protected by a record fingerprint in
// historical envelope order, which both fingerprint families and the archive
// reproduce exactly: organization, sequence, category, policy-change
// payload, decision payload, predecessor fingerprint. AuditRecord.Fingerprint
// is deliberately absent (see the file note).
var recordFields = [...]recordFieldSpec{
	{
		kind:      recordFieldString,
		jsonKey:   "org",
		getString: func(r *AuditRecord) string { return r.Org },
		setString: func(r *AuditRecord, v string) { r.Org = v },
	},
	{
		kind:    recordFieldInt,
		jsonKey: "seq",
		getInt:  func(r *AuditRecord) int { return r.Seq },
		setInt:  func(r *AuditRecord, v int) { r.Seq = v },
	},
	{
		kind:      recordFieldString,
		jsonKey:   "kind",
		getString: func(r *AuditRecord) string { return r.Kind },
		setString: func(r *AuditRecord, v string) { r.Kind = v },
	},
	{
		kind:      recordFieldChangePayload,
		jsonKey:   "change",
		getChange: func(r *AuditRecord) *PolicyChange { return r.Change },
		setChange: func(r *AuditRecord, c *PolicyChange) { r.Change = c },
	},
	{
		kind:        recordFieldDecisionPayload,
		jsonKey:     "decision",
		getDecision: func(r *AuditRecord) *DecisionRecord { return r.Decision },
		setDecision: func(r *AuditRecord, d *DecisionRecord) { r.Decision = d },
	},
	{
		kind:      recordFieldString,
		jsonKey:   "prev",
		getString: func(r *AuditRecord) string { return r.PrevFingerprint },
		setString: func(r *AuditRecord, v string) { r.PrevFingerprint = v },
	},
}

// recordJSONEnvelope is the valid-UTF-8 fingerprint family's canonical
// representation, assembled exclusively from recordFields. Its declaration
// order and tags are the frozen legacy hashEnvelope contract — org, seq,
// kind, change omitempty, decision omitempty, prev — so the marshaled bytes
// stay identical to historical fingerprints.
type recordJSONEnvelope struct {
	Org             string          `json:"org"`
	Seq             int             `json:"seq"`
	Kind            string          `json:"kind"`
	Change          *PolicyChange   `json:"change,omitempty"`
	Decision        *DecisionRecord `json:"decision,omitempty"`
	PrevFingerprint string          `json:"prev"`
}

// marshalRecordEnvelopeJSON produces exactly the bytes a legacy
// json.Marshal over the envelope produced, placing each table entry into
// the envelope member its jsonKey names. Because recordFields lists every
// protected outer member exactly once and recordJSONEnvelope keeps the
// frozen declaration and tags, no outer field can be marshaled without
// appearing in the one table.
func marshalRecordEnvelopeJSON(r *AuditRecord) ([]byte, error) {
	env := recordJSONEnvelope{}
	for i := range recordFields {
		f := &recordFields[i]
		switch f.kind {
		case recordFieldString:
			// The three string fields occupy fixed envelope members; the
			// table's JSON key rather than its position makes the placement.
			switch f.jsonKey {
			case "org":
				env.Org = f.getString(r)
			case "kind":
				env.Kind = f.getString(r)
			case "prev":
				env.PrevFingerprint = f.getString(r)
			default:
				// A string field added to the table with no envelope member
				// would otherwise be fingerprinted nowhere.
				panic(fmt.Errorf("darksafe: record field %q has no JSON envelope member", f.jsonKey))
			}
		case recordFieldInt:
			env.Seq = f.getInt(r)
		case recordFieldChangePayload:
			env.Change = f.getChange(r)
		case recordFieldDecisionPayload:
			env.Decision = f.getDecision(r)
		default:
			panic(fmt.Errorf("darksafe: unknown outer record field kind %d", f.kind))
		}
	}
	return json.Marshal(&env)
}

// writeEnvelopeRaw writes r's protected outer fields to the canonical
// raw-byte encoder in recordFields order. Each encoder method emits the same
// field marker the historical encoding used: rawString tags the strings,
// int64Tag tags the sequence, and change/decisionRecord emit their group
// markers themselves and distinguish a nil payload from a present one. The
// current fingerprint is never written.
func writeEnvelopeRaw(e *fingerprintEncoder, r *AuditRecord) {
	for i := range recordFields {
		f := &recordFields[i]
		switch f.kind {
		case recordFieldString:
			e.rawString(f.getString(r))
		case recordFieldInt:
			e.int64Tag(tagSeq, int64(f.getInt(r)))
		case recordFieldChangePayload:
			e.change(f.getChange(r))
		case recordFieldDecisionPayload:
			e.decisionRecord(f.getDecision(r))
		default:
			panic(fmt.Errorf("darksafe: unknown outer record field kind %d", f.kind))
		}
	}
}

// envelopeUsesRawBytes reports whether any string protected by the
// fingerprint carries bytes that are not valid UTF-8. Such envelopes are
// hashed through the raw canonical encoding instead of encoding/json,
// because JSON string encoding replaces invalid bytes with U+FFFD and would
// let two different raw contents share a fingerprint. It ranges the single
// outer field table, so its idea of which outer content the fingerprint
// protects can never diverge from the fields the encoders write; which
// strings the nested payloads carry is decided by the single change and
// decision field tables.
func envelopeUsesRawBytes(r *AuditRecord) bool {
	for i := range recordFields {
		f := &recordFields[i]
		switch f.kind {
		case recordFieldString:
			if !utf8.ValidString(f.getString(r)) {
				return true
			}
		case recordFieldInt:
			// Integer content cannot carry a raw byte.
		case recordFieldChangePayload:
			if c := f.getChange(r); c != nil && changeHasNonUTF8(c) {
				return true
			}
		case recordFieldDecisionPayload:
			if d := f.getDecision(r); d != nil && decisionRecordHasNonUTF8(d) {
				return true
			}
		default:
			panic(fmt.Errorf("darksafe: unknown outer record field kind %d", f.kind))
		}
	}
	return false
}

// encodeCanonical returns the raw bytes fingerprinted for a record whose
// strings include invalid UTF-8. Every string the record carries reaches the
// output byte-for-byte, at whatever nesting depth or list position it
// occupies. The prefix separates this encoding space from JSON
// fingerprints.
func encodeCanonical(r *AuditRecord) []byte {
	e := &fingerprintEncoder{buf: []byte(rawEnvelopePrefix)}
	e.tag(tagEnvelope)
	writeEnvelopeRaw(e, r)
	return e.buf
}

// fingerprintFor returns the fingerprint of a record's protected outer
// content. Records made exclusively of valid UTF-8 keep the historical JSON
// fingerprint, so old exports and checkpoints remain valid; records
// containing invalid bytes use the length-prefixed raw encoding that
// protects their exact bytes. Both families enumerate their protected fields
// exclusively from recordFields.
func fingerprintFor(r *AuditRecord) string {
	var b []byte
	if envelopeUsesRawBytes(r) {
		b = encodeCanonical(r)
	} else {
		var err error
		b, err = marshalRecordEnvelopeJSON(r)
		if err != nil {
			// All envelope fields are plain, JSON-encodable values; a
			// failure here indicates a programming error, not user input.
			panic(fmt.Errorf("darksafe: audit envelope must encode: %w", err))
		}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
