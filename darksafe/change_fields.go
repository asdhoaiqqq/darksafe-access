// This file is the single definition of which fields a PolicyChange carries
// for every audit-side treatment of a policy-change payload: the
// fingerprint's raw canonical encoder, the valid-UTF-8 test that chooses
// between the JSON and raw fingerprint families, the archive writer and
// reader, and the detaching copy that keeps stored records independent of
// caller slices. Each of those walks the one table below instead of naming
// the fields itself, so the fields, their order and their retention rules
// mean the same thing everywhere: the new version number, the full policy
// list of that version, the rollback source version (0 for an ordinary
// publish), and the rollback marker. Adding or changing a change field here
// reaches fingerprinting, the UTF-8 decision, archiving, unarchiving and
// cloning at once; no audit path can keep a stale list of its own or miss a
// field on one of the routes.
//
// The table order is part of the wire and fingerprint contract: it is the
// historical order Version, Policies, SourceVersion, RolledBack, and the
// raw fingerprint encoder and archive reproduce it exactly. Publish and
// rollback themselves (org.go) fill the struct by name and are
// intentionally not driven from this table.
package darksafe

// changeFieldKind says how one PolicyChange field participates in
// fingerprinting and archiving.
type changeFieldKind int

const (
	// changeFieldInt is a non-negative integer field: the newly created
	// version and the rollback source version. It has no UTF-8
	// representation, so the valid-UTF-8 scan skips it while both encoders
	// write it.
	changeFieldInt changeFieldKind = iota
	// changeFieldPolicyList is the new version's full policy set. Its
	// nil-versus-empty shape and element order are preserved explicitly by
	// both encoders, and it is the one change field that can carry raw bytes
	// (through policy strings) and that needs detaching on copy.
	changeFieldPolicyList
	// changeFieldBool is the rollback marker (false for an ordinary
	// publish). It has no UTF-8 representation either.
	changeFieldBool
)

// changeFieldSpec locates one PolicyChange field. The accessors used depend
// on the kind: integers use getInt/setInt, the policy list
// getPolicies/setPolicies, booleans getBool/setBool. A field appears in
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
// order, which the archive reproduces field for field: first the new
// version, then its full policy list, then the rollback source version
// (zero for an ordinary publish), then the rollback marker.
var changeFields = [...]changeFieldSpec{
	{
		kind:   changeFieldInt,
		getInt: func(c *PolicyChange) int { return c.Version },
		setInt: func(c *PolicyChange, v int) { c.Version = v },
	},
	{
		kind:        changeFieldPolicyList,
		getPolicies: func(c *PolicyChange) []Policy { return c.Policies },
		setPolicies: func(c *PolicyChange, p []Policy) { c.Policies = p },
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

// changeHasNonUTF8 reports whether any protected string of c carries bytes
// that are not valid UTF-8. Only the policy list can hold strings; it
// ranges the single field table and the single policy field table, so its
// idea of which strings the fingerprint protects can never diverge from the
// fields the encoders write. Integer and boolean fields cannot carry a raw
// byte and are skipped by kind rather than listed separately.
func changeHasNonUTF8(c *PolicyChange) bool {
	for i := range changeFields {
		f := &changeFields[i]
		if f.kind != changeFieldPolicyList {
			continue
		}
		for _, p := range f.getPolicies(c) {
			// Which policy strings are protected is decided by the single
			// policy field table (policy_fields.go), never relisted here.
			if policyHasNonUTF8(p) {
				return true
			}
		}
	}
	return false
}

// cloneChangeLists detaches every policy-list field of src into dst,
// preserving the list's nil-versus-empty shape. It ranges the single field
// table, so a list field added to the change is detached here without this
// function changing.
func cloneChangeLists(dst, src *PolicyChange) {
	for i := range changeFields {
		f := &changeFields[i]
		if f.kind == changeFieldPolicyList {
			f.setPolicies(dst, clonePolicies(f.getPolicies(src)))
		}
	}
}
