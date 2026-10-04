// This file is the single definition of which fields a PolicyChange carries
// for every audit-side treatment of policy-change content: the fingerprint's
// raw canonical encoder, the valid-UTF-8 test that chooses between the JSON
// and raw fingerprint families, the archive writer and reader, and the
// detaching copy that keeps stored records independent of caller slices. Each
// of those walks the one table below instead of naming the fields itself, so
// the fields and their order mean the same thing everywhere. Adding or
// changing a policy-change field here reaches fingerprinting, the UTF-8
// decision, archiving, unarchiving and cloning at once; no audit path can
// keep a stale list of its own.
//
// The table order is part of the wire and fingerprint contract: it is the
// historical policy-change field order — the new version, the full policy
// set, the source version, then the rollback marker — and the archive
// mirrors it field for field. Publishing and rollback themselves (org.go)
// are intentionally not driven from this table.
package darksafe

// changeFieldKind says how one PolicyChange field participates in
// fingerprinting and archiving.
type changeFieldKind int

const (
	// changeFieldInt is a non-negative integer field (a policy version
	// number). It has no UTF-8 representation, so the valid-UTF-8 scan skips
	// it while both encoders write it.
	changeFieldInt changeFieldKind = iota
	// changeFieldPolicyList is the full policy set of the new version. Its
	// nil-versus-empty shape and element order are preserved explicitly by
	// both encoders, and its policy strings are protected by the valid-UTF-8
	// scan through the single policy field table.
	changeFieldPolicyList
	// changeFieldBool is a boolean field (the rollback marker). It has no
	// UTF-8 representation either.
	changeFieldBool
)

// changeFieldSpec locates one PolicyChange field. The accessors used depend
// on the kind: integer fields use getInt/setInt, the policy list uses
// getPolicies/setPolicies and booleans getBool/setBool. A field appears in
// changeFields exactly once, and that entry is the only place its struct
// member is named.
type changeFieldSpec struct {
	kind        changeFieldKind
	getInt      func(*PolicyChange) int
	setInt      func(*PolicyChange, int)
	getPolicies func(*PolicyChange) []Policy
	setPolicies func(*PolicyChange, []Policy)
	getBool     func(*PolicyChange) bool
	setBool     func(*PolicyChange, bool)
}

// changeFields lists every PolicyChange field in fingerprint canonical
// order, which the archive reproduces exactly: Version, Policies,
// SourceVersion, RolledBack, exactly as the historical encoding placed them.
var changeFields = [...]changeFieldSpec{
	{
		kind:   changeFieldInt,
		getInt: func(c *PolicyChange) int { return c.Version },
		setInt: func(c *PolicyChange, v int) { c.Version = v },
	},
	{
		kind:        changeFieldPolicyList,
		getPolicies: func(c *PolicyChange) []Policy { return c.Policies },
		setPolicies: func(c *PolicyChange, l []Policy) { c.Policies = l },
	},
	{
		kind:   changeFieldInt,
		getInt: func(c *PolicyChange) int { return c.SourceVersion },
		setInt: func(c *PolicyChange, v int) { c.SourceVersion = v },
	},
	{
		kind:    changeFieldBool,
		getBool: func(c *PolicyChange) bool { return c.RolledBack },
		setBool: func(c *PolicyChange, v bool) { c.RolledBack = v },
	},
}

// changeHasNonUTF8 reports whether any policy string the change carries
// holds bytes that are not valid UTF-8. It ranges the single field table, so
// its idea of which change content the fingerprint protects can never
// diverge from the fields the encoders write. Integer and boolean fields
// cannot carry a raw byte and are skipped by kind rather than listed
// separately; which strings inside one policy are protected remains decided
// by the single policy field table alone.
func changeHasNonUTF8(c *PolicyChange) bool {
	for i := range changeFields {
		f := &changeFields[i]
		if f.kind != changeFieldPolicyList {
			continue
		}
		for _, p := range f.getPolicies(c) {
			if policyHasNonUTF8(p) {
				return true
			}
		}
	}
	return false
}

// cloneChangeLists detaches every policy list field of src into dst,
// preserving each list's nil-versus-empty shape exactly as clonePolicies
// defines it. It ranges the single field table, so a policy list added to
// the payload is detached here without this function changing.
func cloneChangeLists(dst, src *PolicyChange) {
	for i := range changeFields {
		f := &changeFields[i]
		if f.kind == changeFieldPolicyList {
			f.setPolicies(dst, clonePolicies(f.getPolicies(src)))
		}
	}
}
