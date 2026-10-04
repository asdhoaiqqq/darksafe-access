// This file is the single definition of which outer fields an AuditRecord
// carries for every audit-side treatment of record-envelope content: the
// fingerprint's JSON and raw canonical encoders, the valid-UTF-8 test that
// chooses between the two fingerprint families, and the archive writer and
// reader. Each of those walks the one table below instead of naming the
// fields itself, so the fields and their order mean the same thing
// everywhere. Adding or changing an outer record field here reaches
// fingerprinting, the UTF-8 decision, archiving and unarchiving at once; no
// audit path can keep a stale list of its own.
//
// The table order is part of the wire and fingerprint contract: it is the
// historical envelope order — organization, sequence, category, the two
// payloads, the previous fingerprint — followed by the record's own
// fingerprint, which the archive stores but the fingerprint itself never
// covers. The contents of the two payloads stay governed by their own
// field tables (policy, policy-change and decision); this table only names
// which payload sits where in the envelope.
package darksafe

import "unicode/utf8"

// recordFieldKind says how one outer AuditRecord field participates in
// fingerprinting and archiving.
type recordFieldKind int

const (
	// recordFieldString is a string field written from its exact bytes by
	// the raw fingerprint encoder and the archive, and encoded as a JSON
	// string by the legacy fingerprint family.
	recordFieldString recordFieldKind = iota
	// recordFieldInt is a non-negative integer field (the sequence number).
	// It has no UTF-8 representation, so the valid-UTF-8 scan skips it
	// while both encoders write it.
	recordFieldInt
	// recordFieldChange is the policy-change payload slot. Its nil-versus-
	// present shape is marked explicitly by the raw encoder and the
	// archive, and omitted from the JSON envelope when absent; which change
	// content is protected remains decided by the policy-change field
	// table.
	recordFieldChange
	// recordFieldDecision is the decision payload slot, treated exactly
	// like the policy-change slot; which decision content is protected
	// remains decided by the decision field table.
	recordFieldDecision
)

// recordFieldSpec locates one outer AuditRecord field. The accessors used
// depend on the kind: string fields use getString/setString, the sequence
// uses getInt/setInt and the payload slots getChange/setChange and
// getDecision/setDecision. A field appears in recordFields exactly once,
// and that entry is the only place its struct member is named.
type recordFieldSpec struct {
	kind recordFieldKind
	// jsonKey is the field's name in the legacy JSON fingerprint envelope
	// and doubles as the envelope-membership marker: every field up to and
	// including the previous fingerprint has one, while the record's own
	// fingerprint has none, because a fingerprint can never be an input to
	// itself. The archive writes every field, envelope member or not.
	jsonKey     string
	getString   func(*AuditRecord) string
	setString   func(*AuditRecord, string)
	getInt      func(*AuditRecord) int
	setInt      func(*AuditRecord, int)
	getChange   func(*AuditRecord) *PolicyChange
	setChange   func(*AuditRecord, *PolicyChange)
	getDecision func(*AuditRecord) *DecisionRecord
	setDecision func(*AuditRecord, *DecisionRecord)
}

// recordFields lists every outer AuditRecord field in fingerprint canonical
// order, which the archive reproduces exactly and then extends with the
// record's own fingerprint: Org, Seq, Kind, Change, Decision,
// PrevFingerprint, Fingerprint, exactly as the historical encodings placed
// them.
var recordFields = [...]recordFieldSpec{
	{
		kind:      recordFieldString,
		jsonKey:   "org",
		getString: func(r *AuditRecord) string { return r.Org },
		setString: func(r *AuditRecord, s string) { r.Org = s },
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
		setString: func(r *AuditRecord, s string) { r.Kind = s },
	},
	{
		kind:      recordFieldChange,
		jsonKey:   "change",
		getChange: func(r *AuditRecord) *PolicyChange { return r.Change },
		setChange: func(r *AuditRecord, c *PolicyChange) { r.Change = c },
	},
	{
		kind:        recordFieldDecision,
		jsonKey:     "decision",
		getDecision: func(r *AuditRecord) *DecisionRecord { return r.Decision },
		setDecision: func(r *AuditRecord, d *DecisionRecord) { r.Decision = d },
	},
	{
		kind:      recordFieldString,
		jsonKey:   "prev",
		getString: func(r *AuditRecord) string { return r.PrevFingerprint },
		setString: func(r *AuditRecord, s string) { r.PrevFingerprint = s },
	},
	{
		// The record's own fingerprint: archived so an export stays
		// self-describing, but never an envelope member — it carries no
		// jsonKey, so neither fingerprint family reads it as input.
		kind:      recordFieldString,
		getString: func(r *AuditRecord) string { return r.Fingerprint },
		setString: func(r *AuditRecord, s string) { r.Fingerprint = s },
	},
}

// recordUsesRawBytes reports whether any string protected by the
// fingerprint carries bytes that are not valid UTF-8. Such records are
// hashed through encodeCanonical instead of the JSON envelope, because JSON
// string encoding replaces invalid bytes with U+FFFD and would let two
// different raw contents share a fingerprint. It ranges the single field
// table, so its idea of which outer content the fingerprint protects can
// never diverge from the fields the encoders write; the record's own
// fingerprint is not an envelope member and is never inspected. Which
// strings inside the payloads are protected remains decided by the
// policy-change and decision field tables alone.
func recordUsesRawBytes(r *AuditRecord) bool {
	for i := range recordFields {
		f := &recordFields[i]
		if f.jsonKey == "" {
			continue
		}
		switch f.kind {
		case recordFieldString:
			if !utf8.ValidString(f.getString(r)) {
				return true
			}
		case recordFieldChange:
			if c := f.getChange(r); c != nil && changeHasNonUTF8(c) {
				return true
			}
		case recordFieldDecision:
			if d := f.getDecision(r); d != nil && decisionRecordHasNonUTF8(d) {
				return true
			}
		}
	}
	return false
}
