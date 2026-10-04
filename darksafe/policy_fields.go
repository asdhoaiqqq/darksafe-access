// This file is the single definition of which fields a Policy carries for
// every audit-side treatment of policy content: the fingerprint's raw
// canonical encoder, the valid-UTF-8 test that chooses between the JSON and
// raw fingerprint families, and the archive writer and reader. Each of
// those walks the one table below instead of naming the fields itself, so
// the fields and their order mean the same thing everywhere. Adding or
// changing a policy field here reaches fingerprinting, the UTF-8 decision,
// archiving and unarchiving at once; no audit path can keep a stale list of
// its own.
//
// The table order is part of the wire and fingerprint contract: it is the
// historical fingerprint field order, and the archive mirrors it field for
// field. Authorization itself (org.go) is intentionally not driven from
// this table.
package darksafe

import "unicode/utf8"

// policyFieldKind says how one Policy field participates in fingerprinting
// and archiving.
type policyFieldKind int

const (
	// policyFieldString is a string field written unconditionally by both
	// the fingerprint encoder and the archive.
	policyFieldString policyFieldKind = iota
	// policyFieldOptionalString is a string field carried by the legacy JSON
	// fingerprint family under an omitempty tag. The raw fingerprint encoder
	// appends it only when non-empty, so empty values keep the exact legacy
	// fingerprint bytes; the archive always writes it, including the empty
	// value, so the field still round trips on its own.
	policyFieldOptionalString
	// policyFieldBool is a boolean field. It has no UTF-8 representation, so
	// the valid-UTF-8 scan skips it while both encoders write it.
	policyFieldBool
)

// policyFieldSpec locates one Policy field. The string accessors are set for
// policyFieldString and policyFieldOptionalString; the bool accessors are
// set for policyFieldBool. A field appears in policyFields exactly once, and
// that entry is the only place its struct member is named.
type policyFieldSpec struct {
	kind      policyFieldKind
	getString func(Policy) string
	setString func(*Policy, string)
	getBool   func(Policy) bool
	setBool   func(*Policy, bool)
}

// policyFields lists every Policy field in fingerprint canonical order,
// which the archive reproduces exactly. Recursive (the sole boolean) sits
// between Effect and ResourceID exactly as it always has; ResourceID is the
// one field that is optional for fingerprints but always archived.
var policyFields = [...]policyFieldSpec{
	{
		kind:      policyFieldString,
		getString: func(p Policy) string { return p.ID },
		setString: func(p *Policy, s string) { p.ID = s },
	},
	{
		kind:      policyFieldString,
		getString: func(p Policy) string { return p.Subject },
		setString: func(p *Policy, s string) { p.Subject = s },
	},
	{
		kind:      policyFieldString,
		getString: func(p Policy) string { return p.Action },
		setString: func(p *Policy, s string) { p.Action = s },
	},
	{
		kind:      policyFieldString,
		getString: func(p Policy) string { return p.Scope },
		setString: func(p *Policy, s string) { p.Scope = s },
	},
	{
		kind:      policyFieldString,
		getString: func(p Policy) string { return string(p.Effect) },
		setString: func(p *Policy, s string) { p.Effect = Effect(s) },
	},
	{
		kind:    policyFieldBool,
		getBool: func(p Policy) bool { return p.Recursive },
		setBool: func(p *Policy, v bool) { p.Recursive = v },
	},
	{
		kind:      policyFieldOptionalString,
		getString: func(p Policy) string { return p.ResourceID },
		setString: func(p *Policy, s string) { p.ResourceID = s },
	},
}

// policyHasNonUTF8 reports whether any string field of p carries bytes that
// are not valid UTF-8. It ranges the single field table, so its idea of
// which policy strings the fingerprint protects can never diverge from the
// fields the encoders write. A boolean field cannot carry a raw byte and is
// skipped by kind rather than listed separately.
func policyHasNonUTF8(p Policy) bool {
	for i := range policyFields {
		f := &policyFields[i]
		if f.kind == policyFieldBool {
			continue
		}
		if !utf8.ValidString(f.getString(p)) {
			return true
		}
	}
	return false
}
