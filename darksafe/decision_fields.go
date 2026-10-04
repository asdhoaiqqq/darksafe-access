// This file is the single definition of which fields a DecisionRecord
// carries for every audit-side treatment of decision content: the
// fingerprint's raw canonical encoder, the valid-UTF-8 test that chooses
// between the JSON and raw fingerprint families, the archive writer and
// reader, and the detaching copy that keeps stored records independent of
// caller slices. Each of those walks the one table below instead of naming
// the fields itself, so the fields and their order mean the same thing
// everywhere. Adding or changing a decision field here reaches
// fingerprinting, the UTF-8 decision, archiving, unarchiving and cloning at
// once; no audit path can keep a stale list of its own.
//
// The table order is part of the wire and fingerprint contract: it is the
// historical decision field order — the two organization names, the subject,
// the resource, the action, then the decision itself — and the archive
// mirrors it field for field. Authorization itself (org.go) is intentionally
// not driven from this table: Roles participates here as a saved request
// field even though org policy evaluation never matches on it.
package darksafe

import "unicode/utf8"

// decisionFieldKind says how one DecisionRecord field participates in
// fingerprinting and archiving.
type decisionFieldKind int

const (
	// decisionFieldString is a string field written from its exact bytes by
	// both the fingerprint encoder and the archive.
	decisionFieldString decisionFieldKind = iota
	// decisionFieldStringList is a string slice whose nil-versus-empty
	// shape and element order are preserved explicitly by both encoders.
	decisionFieldStringList
	// decisionFieldBool is a boolean field. It has no UTF-8 representation,
	// so the valid-UTF-8 scan skips it while both encoders write it.
	decisionFieldBool
	// decisionFieldInt is a non-negative integer field (the policy version
	// the decision used). It has no UTF-8 representation either.
	decisionFieldInt
)

// decisionFieldSpec locates one DecisionRecord leaf field. The accessors
// used depend on the kind: string fields use getString/setString, list
// fields getList/setList, booleans getBool/setBool and integers
// getInt/setInt. A field appears in decisionFields exactly once, and that
// entry is the only place its struct member is named.
type decisionFieldSpec struct {
	kind decisionFieldKind
	// groupTag, when non-zero, is a nesting marker the fingerprint encoder
	// emits immediately before this field, opening the subject, resource or
	// decision group exactly where the historical encoding placed it. The
	// archive has no group markers and ignores this.
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

// decisionFields lists every DecisionRecord leaf field in fingerprint
// canonical order, which the archive reproduces exactly. The subject group
// opens at Subject.ID, the resource group at Resource.ID and the decision
// group at Decision.Allowed, matching the historical nesting markers.
var decisionFields = [...]decisionFieldSpec{
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
		setInt: func(d *DecisionRecord, v int) { d.Decision.Version = v },
	},
}

// decisionRecordHasNonUTF8 reports whether any string or list element of d
// carries bytes that are not valid UTF-8. It ranges the single field table,
// so its idea of which decision strings the fingerprint protects can never
// diverge from the fields the encoders write. Boolean and integer fields
// cannot carry a raw byte and are skipped by kind rather than listed
// separately.
func decisionRecordHasNonUTF8(d *DecisionRecord) bool {
	for i := range decisionFields {
		f := &decisionFields[i]
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

// cloneDecisionLists detaches every string list field of src into dst,
// preserving each list's nil-versus-empty shape. It ranges the single field
// table, so a list field added to the record is detached here without this
// function changing.
func cloneDecisionLists(dst, src *DecisionRecord) {
	for i := range decisionFields {
		f := &decisionFields[i]
		if f.kind == decisionFieldStringList {
			f.setList(dst, cloneStrings(f.getList(src)))
		}
	}
}
