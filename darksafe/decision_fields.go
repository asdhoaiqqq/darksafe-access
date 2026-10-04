// This file is the single definition of which fields a saved DecisionRecord
// carries for every audit-side treatment of decision content: the
// fingerprint's raw canonical encoder, the valid-UTF-8 test that chooses
// between the JSON and raw fingerprint families, and the archive writer and
// reader. Each of those walks the one table below instead of naming the
// fields itself, so the full request and the decision result are enumerated
// exactly once and the fields and their order mean the same thing
// everywhere. Adding or changing a field here reaches fingerprinting, the
// UTF-8 decision, archiving and unarchiving at once; no audit path can keep
// a stale list of its own.
//
// The table order is part of the wire and fingerprint contract: it is the
// historical fingerprint field order (the complete request first, then the
// decision actually returned), and the archive mirrors it field for field.
// Authorization itself (org.go) is intentionally not driven from this
// table: the subject's roles take no part in organization policy decisions,
// yet they are part of the saved request and appear here like every other
// field.
package darksafe

import "unicode/utf8"

// decisionFieldKind says how one DecisionRecord field participates in
// fingerprinting and archiving.
type decisionFieldKind int

const (
	// decisionFieldString is a plain string field, written unconditionally
	// by both the fingerprint encoder and the archive.
	decisionFieldString decisionFieldKind = iota
	// decisionFieldStringList is a []string field whose nil-versus-empty
	// shape both encodings preserve explicitly.
	decisionFieldStringList
	// decisionFieldBool is a boolean field. It has no UTF-8 representation,
	// so the valid-UTF-8 scan skips it while both encoders write it.
	decisionFieldBool
	// decisionFieldInt is an integer field (the policy version the decision
	// actually used). It too is skipped by the valid-UTF-8 scan.
	decisionFieldInt
)

// decisionFieldSpec locates one DecisionRecord field. Exactly one accessor
// pair is set, matching kind. A field appears in decisionRecordFields
// exactly once, and that entry is the only place its struct member is
// named.
type decisionFieldSpec struct {
	kind decisionFieldKind
	// groupTag is emitted by the raw fingerprint encoder immediately before
	// this field's value and reproduces the historical grouping tag that
	// opens the nested value this field starts (tagSubject, tagResource or
	// tagDecision). Zero means the field starts no group. The archive has no
	// grouping markers, so its writer and reader ignore this.
	groupTag  fingerprintTag
	getString func(*DecisionRecord) string
	setString func(*DecisionRecord, string)
	getList   func(*DecisionRecord) []string
	setList   func(*DecisionRecord, []string)
	getBool   func(*DecisionRecord) bool
	setBool   func(*DecisionRecord, bool)
	getInt    func(*DecisionRecord) int
	setInt    func(*DecisionRecord, int)
}

// decisionRecordFields lists every field of a saved decision in fingerprint
// canonical order, which the archive reproduces exactly: the subject's and
// resource's organizations, the subject (identifier, kind, roles, disabled
// flag), the resource (identifier and scope), the action, then the decision
// itself (allowed or not, reason, matched policies, version actually used).
var decisionRecordFields = [...]decisionFieldSpec{
	{
		kind:      decisionFieldString,
		getString: func(d *DecisionRecord) string { return d.Request.SubjectOrg },
		setString: func(d *DecisionRecord, s string) { d.Request.SubjectOrg = s },
	},
	{
		kind:      decisionFieldString,
		getString: func(d *DecisionRecord) string { return d.Request.ResourceOrg },
		setString: func(d *DecisionRecord, s string) { d.Request.ResourceOrg = s },
	},
	{
		kind:      decisionFieldString,
		groupTag:  tagSubject,
		getString: func(d *DecisionRecord) string { return d.Request.Subject.ID },
		setString: func(d *DecisionRecord, s string) { d.Request.Subject.ID = s },
	},
	{
		kind:      decisionFieldString,
		getString: func(d *DecisionRecord) string { return d.Request.Subject.Kind },
		setString: func(d *DecisionRecord, s string) { d.Request.Subject.Kind = s },
	},
	{
		kind:    decisionFieldStringList,
		getList: func(d *DecisionRecord) []string { return d.Request.Subject.Roles },
		setList: func(d *DecisionRecord, l []string) { d.Request.Subject.Roles = l },
	},
	{
		kind:    decisionFieldBool,
		getBool: func(d *DecisionRecord) bool { return d.Request.Subject.Disabled },
		setBool: func(d *DecisionRecord, v bool) { d.Request.Subject.Disabled = v },
	},
	{
		kind:      decisionFieldString,
		groupTag:  tagResource,
		getString: func(d *DecisionRecord) string { return d.Request.Resource.ID },
		setString: func(d *DecisionRecord, s string) { d.Request.Resource.ID = s },
	},
	{
		kind:      decisionFieldString,
		getString: func(d *DecisionRecord) string { return d.Request.Resource.Scope },
		setString: func(d *DecisionRecord, s string) { d.Request.Resource.Scope = s },
	},
	{
		kind:      decisionFieldString,
		getString: func(d *DecisionRecord) string { return d.Request.Action },
		setString: func(d *DecisionRecord, s string) { d.Request.Action = s },
	},
	{
		kind:     decisionFieldBool,
		groupTag: tagDecision,
		getBool:  func(d *DecisionRecord) bool { return d.Decision.Allowed },
		setBool:  func(d *DecisionRecord, v bool) { d.Decision.Allowed = v },
	},
	{
		kind:      decisionFieldString,
		getString: func(d *DecisionRecord) string { return d.Decision.Reason },
		setString: func(d *DecisionRecord, s string) { d.Decision.Reason = s },
	},
	{
		kind:    decisionFieldStringList,
		getList: func(d *DecisionRecord) []string { return d.Decision.Matched },
		setList: func(d *DecisionRecord, l []string) { d.Decision.Matched = l },
	},
	{
		kind:   decisionFieldInt,
		getInt: func(d *DecisionRecord) int { return d.Decision.Version },
		setInt: func(d *DecisionRecord, n int) { d.Decision.Version = n },
	},
}

// decisionRecordHasNonUTF8 reports whether any string the decision record
// protects carries bytes that are not valid UTF-8. It ranges the single
// field table, so its idea of which strings the fingerprint protects can
// never diverge from the fields the encoders write. Boolean and integer
// fields cannot carry a raw byte and are skipped by kind rather than listed
// separately.
func decisionRecordHasNonUTF8(d *DecisionRecord) bool {
	for i := range decisionRecordFields {
		f := &decisionRecordFields[i]
		switch f.kind {
		case decisionFieldString:
			if !utf8.ValidString(f.getString(d)) {
				return true
			}
		case decisionFieldStringList:
			for _, s := range f.getList(d) {
				if !utf8.ValidString(s) {
					return true
				}
			}
		}
	}
	return false
}
