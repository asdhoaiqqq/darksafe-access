// This file is the business regression guard for tenant isolation keyed by
// the organization name's original bytes in organization-level access
// decisions (Store.Decide) and their audit records. The organization name is
// the tenant key, compared byte for byte with no case folding, trimming,
// UTF-8 replacement or any other cleaning: two names that look alike on
// screen — "acme" vs "Acme" vs "acme ", or a lone 0xFF byte, a lone 0xFE
// byte and a genuine U+FFFD rune at the same position — are different
// organizations with independent published versions, decisions and audit
// chains.
//
// The guarded rules, all already implemented by the raw-string-keyed
// organization map in org.go, checkRequest's plain string comparisons and
// the organization-bound genesis fingerprint in audit.go:
//
//   - Two organizations may publish version 1 using the very same policy id,
//     subject, resource id, action and scope. One set allowing and the other
//     denying makes requests identical apart from organization membership
//     come back allowed and denied respectively; the reason, the matched
//     policy id and the actual version all come from the deciding
//     organization's own set. A similarly named organization that has never
//     published gets "organization has no published version" at version 0
//     with no matches: it can never borrow another organization's version 1.
//   - Case and leading/trailing whitespace in an organization name are
//     significant: "acme", "Acme" and "acme " are different organizations.
//   - A name carrying a lone 0xFF byte, a lone 0xFE byte or a genuine
//     U+FFFD rune at the same position names three different organizations:
//     the first two are invalid UTF-8, the third is a legal character, and
//     visual similarity must never justify sharing a policy. The submitted
//     raw name is what decides; no cleaning or substitution rule exists.
//   - When an organization with a matching allow policy receives a request
//     whose subject organization or resource organization differs by even a
//     single byte, the envelope is rejected first with "organization
//     mismatch", version 0 and no matches, however complete the rest of the
//     request and even though the enabled subject carries the owner role.
//     Review applies the same gate. With all three organization fields equal
//     the organization's own policy still decides as before.
//   - Audit keeps the same boundary: every Decide with a non-empty decision
//     organization appends exactly one record under THAT organization,
//     preserving the request's submitted organization bytes and the complete
//     decision. An organization-mismatch denial belongs to the organization
//     that made the decision and is never appended under the subject's or
//     resource's organization, which acquires no state merely from being
//     named in a rejected request. Reading one organization's records never
//     mixes in calls made under another name.
//
// The public decision entry points and explanation strings are unchanged:
// this file pins existing behavior through the public API and adds no
// production code.
package darksafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Shared fixture: every organization below publishes the same policy id for
// the same subject, resource id, action and scope, so the organization name
// is the only thing that can explain differing verdicts.
const (
	orgNameSubject  = "u1"
	orgNameResource = "r1"
	orgNameScope    = "org/a"
	orgNameAction   = "read"
	orgNamePolicyID = "p-shared"
)

// orgNamePolicy builds the one shared policy with the requested effect.
func orgNamePolicy(effect Effect) Policy {
	return Policy{ID: orgNamePolicyID, Subject: orgNameSubject, Action: orgNameAction,
		Scope: orgNameScope, Effect: effect}
}

// orgNameRequest builds a complete, envelope-valid request for one
// organization: subject org, resource org and decision org all equal org,
// the subject is enabled and every identifier, the scope and the action are
// present. Tests then change one organization field to probe the boundary.
func orgNameRequest(org string) OrgRequest {
	return OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     Subject{ID: orgNameSubject, Kind: "service"},
		Resource:    Resource{ID: orgNameResource, Scope: orgNameScope},
		Action:      orgNameAction,
	}
}

// wantOrgNameEnvelopeDenial pins a rejection that happened before any policy
// was evaluated: explicit reason, version 0 and a nil match list.
func wantOrgNameEnvelopeDenial(t *testing.T, got Decision, reason, context string) {
	t.Helper()
	if got.Allowed {
		t.Fatalf("%s: allowed = true, want denial; decision = %+v", context, got)
	}
	if got.Reason != reason {
		t.Fatalf("%s: reason = %q, want %q; decision = %+v", context, got.Reason, reason, got)
	}
	if got.Version != 0 {
		t.Fatalf("%s: version = %d, want 0 (no published version evaluated); decision = %+v",
			context, got.Version, got)
	}
	if got.Matched != nil {
		t.Fatalf("%s: matched = %q, want nil (no policy evaluated); decision = %+v",
			context, got.Matched, got)
	}
}

// orgChainLen returns the current audit chain length of one organization via
// the public export entry point.
func orgChainLen(t *testing.T, s *Store, org string) int {
	t.Helper()
	recs, _, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatalf("AuditExport %q: %v", org, err)
	}
	return len(recs)
}

// TestDecideSameIdentifiersOppositeEffectsInTwoOrgs is the Decide counterpart
// of the review isolation guard: two organizations both at version 1, using
// the same policy id, subject, resource, action and scope, publish opposite
// effects. Requests identical apart from organization membership must come
// back allowed and denied, each explained by its own organization's set; and
// one organization's envelope may not be decided against the other at all.
func TestDecideSameIdentifiersOppositeEffectsInTwoOrgs(t *testing.T) {
	const allowOrg = "acme"
	const denyOrg = "Acme" // differs from allowOrg by exactly one byte (case)

	s := NewStore()
	if v, err := s.Publish(allowOrg, 0, []Policy{orgNamePolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("publish allow set = %d, %v; want 1, nil", v, err)
	}
	if v, err := s.Publish(denyOrg, 0, []Policy{orgNamePolicy(EffectDeny)}); err != nil || v != 1 {
		t.Fatalf("publish deny set = %d, %v; want 1, nil", v, err)
	}
	if s.CurrentVersion(allowOrg) != 1 || s.CurrentVersion(denyOrg) != 1 {
		t.Fatalf("both organizations must be at version 1, got %d and %d",
			s.CurrentVersion(allowOrg), s.CurrentVersion(denyOrg))
	}

	allow := s.Decide(allowOrg, orgNameRequest(allowOrg))
	if !allow.Allowed || allow.Reason != "matched allow policy" {
		t.Fatalf("allow-org decision = %+v, want allowed with matched allow policy", allow)
	}
	if !reflect.DeepEqual(allow.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("allow-org matched = %q, want [%s] from its own set", allow.Matched, orgNamePolicyID)
	}
	if allow.Version != 1 {
		t.Fatalf("allow-org version = %d, want its own version 1", allow.Version)
	}

	deny := s.Decide(denyOrg, orgNameRequest(denyOrg))
	if deny.Allowed || deny.Reason != "matched deny policy" {
		t.Fatalf("deny-org decision = %+v, want denied with matched deny policy", deny)
	}
	// The matched identifier is the same string in both organizations; the
	// verdict differs solely because each organization published its own
	// effect for that id.
	if !reflect.DeepEqual(deny.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("deny-org matched = %q, want [%s] from its own set", deny.Matched, orgNamePolicyID)
	}
	if deny.Version != 1 {
		t.Fatalf("deny-org version = %d, want its own version 1", deny.Version)
	}

	// The allow organization must not adjudicate an envelope carrying the
	// other organization's membership: the shared identifiers do not cure the
	// mismatch, and its allow policy is never consulted.
	wantOrgNameEnvelopeDenial(t, s.Decide(allowOrg, orgNameRequest(denyOrg)),
		"organization mismatch", "allow org deciding the deny-org envelope")
	wantOrgNameEnvelopeDenial(t, s.Decide(denyOrg, orgNameRequest(allowOrg)),
		"organization mismatch", "deny org deciding the allow-org envelope")

	// Versions advance independently: the deny organization moving to an
	// empty version 2 neither lends nor borrows the allow organization's
	// version 1.
	if v, err := s.Publish(denyOrg, 1, nil); err != nil || v != 2 {
		t.Fatalf("deny-org second publish = %d, %v; want 2, nil", v, err)
	}
	if got := s.CurrentVersion(allowOrg); got != 1 {
		t.Fatalf("allow-org current = %d, want 1 after the other org advanced", got)
	}
	stillAllow := s.Decide(allowOrg, orgNameRequest(allowOrg))
	if !stillAllow.Allowed || stillAllow.Version != 1 ||
		!reflect.DeepEqual(stillAllow.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("allow-org decision borrowed the other org's version 2: %+v", stillAllow)
	}
}

// TestUnpublishedLookalikeOrgCannotBorrowPublishedVersion pins that an
// organization whose name merely resembles a published one has no policy
// state of its own: its coherent request is the "no published version"
// denial at version 0 with no matches, before or after the neighbor
// publishes, and version 1 can never be resolved under its name.
func TestUnpublishedLookalikeOrgCannotBorrowPublishedVersion(t *testing.T) {
	const published = "acme"
	s := NewStore()
	if v, err := s.Publish(published, 0, []Policy{orgNamePolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", v, err)
	}

	lookalikes := map[string]string{
		"uppercase initial":       "Acme",
		"trailing space":          "acme ",
		"leading space":           " acme",
		"lone 0xFF appended":      "acme" + invalidByte,
		"lone 0xFE appended":      "acme" + anotherInvalidByte,
		"genuine U+FFFD appended": "acme" + replacementRune,
		"one byte shorter":        "acm",
	}
	for label, lookalike := range lookalikes {
		if lookalike == published {
			t.Fatalf("test premise broken: %s equals the published name", label)
		}
		if got := s.CurrentVersion(lookalike); got != 0 {
			t.Fatalf("%s: current version = %d before any publish, want 0", label, got)
		}

		d := s.Decide(lookalike, orgNameRequest(lookalike))
		wantOrgNameEnvelopeDenial(t, d, "organization has no published version", label)

		// The decision did not create a version: the neighbor's version 1 is
		// not resolvable under this name.
		if got := s.CurrentVersion(lookalike); got != 0 {
			t.Fatalf("%s: current version = %d after an unpublished denial, want 0", label, got)
		}
		if _, err := s.Policies(lookalike, 1); !errors.Is(err, ErrVersionNotFound) {
			t.Fatalf("%s: Policies(_, 1) err = %v, want ErrVersionNotFound", label, err)
		}
	}

	// While the lookalikes were decided, the published organization's chain
	// stayed at its single policy-change record: their denials were recorded
	// under their own names, not here.
	if got := orgChainLen(t, s, published); got != 1 {
		t.Fatalf("published org chain = %d records before its own decision, want 1; lookalike decisions leaked in", got)
	}

	// The published organization itself still decides from its own version 1,
	// appending its own decision record under its own name only.
	d := s.Decide(published, orgNameRequest(published))
	if !d.Allowed || d.Reason != "matched allow policy" || d.Version != 1 ||
		!reflect.DeepEqual(d.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("published org decision = %+v, want its own version-1 allow", d)
	}
	if got := orgChainLen(t, s, published); got != 2 {
		t.Fatalf("published org chain = %d after its own decision, want 2", got)
	}

	// Each lookalike's no-version decision is audited under its own name and
	// nowhere else: one decision record, no policy-change record, raw name
	// bytes preserved, chain verifiable.
	for label, lookalike := range lookalikes {
		recs, cp, err := s.AuditExport(lookalike, 0)
		if err != nil {
			t.Fatalf("%s: export: %v", label, err)
		}
		if len(recs) != 1 || recs[0].Kind != AuditDecision {
			t.Fatalf("%s: chain = %d records %+v, want one decision record", label, len(recs), recs)
		}
		if recs[0].Org != lookalike {
			t.Fatalf("%s: record org = %q, want the submitted raw name %q",
				label, recs[0].Org, lookalike)
		}
		if err := VerifyAudit(lookalike, recs, cp); err != nil {
			t.Fatalf("%s: lookalike chain must verify: %v", label, err)
		}
	}
	// The published organization's chain holds only its own publish and its
	// own decision; no lookalike call mixed in.
	if got := orgChainLen(t, s, published); got != 2 {
		t.Fatalf("published org chain = %d records, want 2; lookalike decisions leaked in", got)
	}
}

// TestOrgNameCaseAndWhitespaceNameDistinctTenants proves case and
// surrounding spaces in the organization name select different tenants,
// each with its own current version and verdict for an otherwise identical
// request. An unpublished padded name gets the version-0 denial rather than
// the neighbor's version.
func TestOrgNameCaseAndWhitespaceNameDistinctTenants(t *testing.T) {
	const (
		lower    = "acme"
		upper    = "Acme"
		trailing = "acme "
		leading  = " acme"
	)
	s := NewStore()
	if v, err := s.Publish(lower, 0, []Policy{orgNamePolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("lower publish = %d, %v", v, err)
	}
	if v, err := s.Publish(upper, 0, []Policy{orgNamePolicy(EffectDeny)}); err != nil || v != 1 {
		t.Fatalf("upper publish = %d, %v", v, err)
	}
	// The trailing-space organization publishes an EMPTY set as version 1:
	// its denial must carry version 1 (default deny under a published set),
	// staying distinguishable from the genuinely unpublished case.
	if v, err := s.Publish(trailing, 0, nil); err != nil || v != 1 {
		t.Fatalf("trailing-space publish = %d, %v", v, err)
	}

	lowerD := s.Decide(lower, orgNameRequest(lower))
	if !lowerD.Allowed || lowerD.Reason != "matched allow policy" || lowerD.Version != 1 ||
		!reflect.DeepEqual(lowerD.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("lower decision = %+v, want version-1 allow by its own policy", lowerD)
	}
	upperD := s.Decide(upper, orgNameRequest(upper))
	if upperD.Allowed || upperD.Reason != "matched deny policy" || upperD.Version != 1 ||
		!reflect.DeepEqual(upperD.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("upper decision = %+v, want version-1 deny by its own policy", upperD)
	}
	trailingD := s.Decide(trailing, orgNameRequest(trailing))
	if trailingD.Allowed || trailingD.Reason != "no matching allow policy" || trailingD.Version != 1 {
		t.Fatalf("trailing-space decision = %+v, want default deny at its own version 1", trailingD)
	}
	if trailingD.Matched == nil || len(trailingD.Matched) != 0 {
		t.Fatalf("trailing-space matched = %q, want non-nil empty list (policy set evaluated)",
			trailingD.Matched)
	}
	// The leading-space name has never published: it may not borrow version 1
	// from any of the visually adjacent tenants.
	leadingD := s.Decide(leading, orgNameRequest(leading))
	wantOrgNameEnvelopeDenial(t, leadingD, "organization has no published version",
		"unpublished leading-space organization")

	// The three published tenants carry independent chains with distinct
	// genesis fingerprints even though the policy id and version coincide.
	// This is checked before the cross-name probes below, since those probes
	// are themselves non-empty-org decisions and append records under the
	// deciding organization.
	names := []string{lower, upper, trailing}
	genesis := map[string]string{}
	for _, name := range names {
		recs, cp, err := s.AuditExport(name, 0)
		if err != nil {
			t.Fatalf("export %q: %v", name, err)
		}
		if len(recs) != 2 || recs[0].Kind != AuditPolicyChange || recs[0].Change.Version != 1 {
			t.Fatalf("%q chain = %+v, want version-1 change plus one decision", name, recs)
		}
		for i := range recs {
			if recs[i].Org != name {
				t.Fatalf("%q chain contains a record attributed to %q", name, recs[i].Org)
			}
		}
		if err := VerifyAudit(name, recs, cp); err != nil {
			t.Fatalf("%q chain must verify: %v", name, err)
		}
		g := genesisFingerprint(name)
		for other, otherGenesis := range genesis {
			if otherGenesis == g {
				t.Fatalf("organizations %q and %q share a genesis fingerprint", other, name)
			}
		}
		genesis[name] = g
	}

	// Cross-name envelopes are mismatches in both directions regardless of
	// which tenant published what. Each rejection is nevertheless a non-empty
	// decision-org call, so it appends one record under the deciding org —
	// never under the name carried by the rejected envelope.
	wantOrgNameEnvelopeDenial(t, s.Decide(lower, orgNameRequest(upper)),
		"organization mismatch", "lower org given the upper-cased envelope")
	if got := orgChainLen(t, s, lower); got != 3 {
		t.Fatalf("lower chain = %d after its mismatch decision, want 3", got)
	}
	if got := orgChainLen(t, s, upper); got != 2 {
		t.Fatalf("upper chain = %d, want 2 (the rejection was decided by the lower org)", got)
	}
	wantOrgNameEnvelopeDenial(t, s.Decide(trailing, orgNameRequest(lower)),
		"organization mismatch", "trailing-space org given the trimmed envelope")
	if got := orgChainLen(t, s, trailing); got != 3 {
		t.Fatalf("trailing chain = %d after its mismatch decision, want 3", got)
	}
	if got := orgChainLen(t, s, lower); got != 3 {
		t.Fatalf("lower chain = %d, want 3 (the rejection was decided by the trailing-space org)", got)
	}
}

// rawByteOrgNames returns the three byte-different-but-display-conflated
// organization names used by this file: a lone 0xFF and a lone 0xFE are
// invalid UTF-8 at the same position, while the genuine U+FFFD rune is a
// legal three-byte UTF-8 character.
func rawByteOrgNames() (withFF, withFE, withReplacement string) {
	return "组" + invalidByte + "织",
		"组" + anotherInvalidByte + "织",
		"组" + replacementRune + "织"
}

// TestRawByteOrgNamesAreThreeDistinctTenants pins the display-confusable
// triple at the tenant-key level. encoding/json renders the two
// invalid-UTF-8 names identically (each lone byte becomes "�"), while the
// genuine U+FFFD rune is a legal character with its own rendering; the
// store must keep three independent version-1 tenants regardless, preserve
// each submitted name byte for byte in its signed audit material (using the
// raw fingerprint family for the invalid-UTF-8 names and the legacy JSON
// family for the genuine U+FFFD name), and never accept one organization's
// record as part of another's chain.
func TestRawByteOrgNamesAreThreeDistinctTenants(t *testing.T) {
	ff, fe, fd := rawByteOrgNames()

	// Document the premise this regression guards: Go distinguishes all
	// three names, while encoding/json renders the two lone invalid bytes
	// identically (each becomes "�"). The genuine U+FFFD rune is legal
	// UTF-8 and is emitted literally rather than escaped, so its rendering
	// differs again — but it is still a third, byte-different organization
	// name that no normalization may merge with either invalid-byte tenant.
	if ff == fe || ff == fd || fe == fd {
		t.Fatalf("test premise broken: org names must differ pairwise: %q %q %q", ff, fe, fd)
	}
	jFF, _ := json.Marshal(ff)
	jFE, _ := json.Marshal(fe)
	if !bytes.Equal(jFF, jFE) {
		t.Fatalf("test premise broken: JSON must render the two invalid bytes identically: %s vs %s",
			jFF, jFE)
	}
	jFD, _ := json.Marshal(fd)
	if bytes.Equal(jFD, jFF) {
		t.Fatalf("test premise broken: the genuine U+FFFD name must not share the invalid-byte rendering: %s", jFD)
	}

	s := NewStore()
	if v, err := s.Publish(ff, 0, []Policy{orgNamePolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("0xFF org publish = %d, %v", v, err)
	}
	if v, err := s.Publish(fe, 0, []Policy{orgNamePolicy(EffectDeny)}); err != nil || v != 1 {
		t.Fatalf("0xFE org publish = %d, %v", v, err)
	}
	if v, err := s.Publish(fd, 0, []Policy{orgNamePolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("U+FFFD org publish = %d, %v", v, err)
	}

	dFF := s.Decide(ff, orgNameRequest(ff))
	if !dFF.Allowed || dFF.Reason != "matched allow policy" || dFF.Version != 1 ||
		!reflect.DeepEqual(dFF.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("0xFF org decision = %+v, want its own version-1 allow", dFF)
	}
	dFE := s.Decide(fe, orgNameRequest(fe))
	if dFE.Allowed || dFE.Reason != "matched deny policy" || dFE.Version != 1 ||
		!reflect.DeepEqual(dFE.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("0xFE org decision = %+v, want its own version-1 deny", dFE)
	}
	dFD := s.Decide(fd, orgNameRequest(fd))
	if !dFD.Allowed || dFD.Reason != "matched allow policy" || dFD.Version != 1 ||
		!reflect.DeepEqual(dFD.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("U+FFFD org decision = %+v, want its own version-1 allow", dFD)
	}

	chains := map[string][]AuditRecord{}
	checkpoints := map[string]Checkpoint{}
	for name, d := range map[string]Decision{ff: dFF, fe: dFE, fd: dFD} {
		recs, cp, err := s.AuditExport(name, 0)
		if err != nil {
			t.Fatalf("export %q: %v", name, err)
		}
		if len(recs) != 2 {
			t.Fatalf("%q chain = %d records, want publish + one decision", name, len(recs))
		}
		if err := VerifyAudit(name, recs, cp); err != nil {
			t.Fatalf("%q chain must verify: %v", name, err)
		}
		// The submitted raw name survives in the record header and in both
		// request organization fields.
		for _, r := range recs {
			if r.Org != name {
				t.Fatalf("%q chain holds a record attributed to %q", name, r.Org)
			}
			if r.Decision != nil {
				if r.Decision.Request.SubjectOrg != name || r.Decision.Request.ResourceOrg != name {
					t.Fatalf("%q decision record stores request orgs %q/%q",
						name, r.Decision.Request.SubjectOrg, r.Decision.Request.ResourceOrg)
				}
				if !reflect.DeepEqual(r.Decision.Decision, d) {
					t.Fatalf("%q stored decision %+v != returned %+v", name, r.Decision.Decision, d)
				}
			}
		}
		chains[name] = recs
		checkpoints[name] = cp
	}

	// Invalid-UTF-8 organizations use the length-prefixed raw fingerprint
	// family; the genuine U+FFFD organization is valid UTF-8 and keeps the
	// historical JSON family. Collapsing either side would pin the wrong
	// bytes.
	for _, name := range []string{ff, fe} {
		for i := range chains[name] {
			r := &chains[name][i]
			if fingerprintFor(r) == legacyJSONFingerprint(r) {
				t.Fatalf("%q record %d still uses the JSON fingerprint despite invalid bytes",
					name, r.Seq)
			}
		}
	}
	for i := range chains[fd] {
		r := &chains[fd][i]
		if fingerprintFor(r) != legacyJSONFingerprint(r) {
			t.Fatalf("genuine U+FFFD org record %d left the legacy JSON fingerprint family", r.Seq)
		}
	}

	// The two allow verdicts (0xFF org and U+FFFD org) are byte-identical
	// decisions, yet the chains are independent tenants: the genesis
	// fingerprints differ and a U+FFFD-org decision record spliced into the
	// 0xFF-org chain fails validation. Visual similarity is not shared
	// tenancy.
	if genesisFingerprint(ff) == genesisFingerprint(fd) {
		t.Fatal("the 0xFF and U+FFFD organizations share a genesis fingerprint")
	}
	if checkpoints[ff].Fingerprint == checkpoints[fd].Fingerprint {
		t.Fatal("the two allow chains share a head fingerprint despite different org names")
	}
	spliced := []AuditRecord{chains[ff][0], chains[fd][1]}
	splicedCP := Checkpoint{Org: ff, EndSeq: 2, Fingerprint: spliced[1].Fingerprint}
	if err := VerifyAudit(ff, spliced, splicedCP); err == nil {
		t.Fatal("a U+FFFD-org decision record verified inside the 0xFF-org chain")
	}

	// Advancing the 0xFF tenant to an empty version 2 changes its verdict but
	// leaves the other two tenants at version 1 with theirs.
	if v, err := s.Publish(ff, 1, nil); err != nil || v != 2 {
		t.Fatalf("0xFF org second publish = %d, %v; want 2, nil", v, err)
	}
	if s.CurrentVersion(fe) != 1 || s.CurrentVersion(fd) != 1 {
		t.Fatalf("the other raw-byte tenants must stay at version 1, got %d and %d",
			s.CurrentVersion(fe), s.CurrentVersion(fd))
	}
	if d := s.Decide(ff, orgNameRequest(ff)); d.Allowed || d.Version != 2 ||
		d.Reason != "no matching allow policy" {
		t.Fatalf("0xFF org at version 2 = %+v, want default deny at version 2", d)
	}
	if d := s.Decide(fd, orgNameRequest(fd)); !d.Allowed || d.Version != 1 {
		t.Fatalf("U+FFFD org borrowed the 0xFF org's version 2: %+v", d)
	}
}

// TestOneByteDifferentOrgFieldIsMismatchBeforePolicy proves the envelope
// gate is byte exact on both request organization fields: with a matching
// allow policy published, changing the subject organization or the resource
// organization to any name that differs by even one byte rejects with
// "organization mismatch" before policy evaluation — version 0, no matches —
// even with every other field complete, the subject enabled and carrying the
// owner role. Review shares the gate. Only the fully aligned envelope
// reaches the home organization's own policy.
func TestOneByteDifferentOrgFieldIsMismatchBeforePolicy(t *testing.T) {
	const home = "acme"
	s := NewStore()
	if v, err := s.Publish(home, 0, []Policy{orgNamePolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("publish = %d, %v; want 1, nil", v, err)
	}

	variants := []struct {
		name string
		org  string
	}{
		{"uppercase initial", "Acme"},
		{"case inside", "aCme"},
		{"trailing space", "acme "},
		{"leading space", " acme"},
		{"lone 0xFF appended", "acme" + invalidByte},
		{"lone 0xFE appended", "acme" + anotherInvalidByte},
		{"genuine U+FFFD appended", "acme" + replacementRune},
		{"one byte shorter", "acm"},
	}
	for _, tc := range variants {
		if tc.org == home || tc.org == "" {
			t.Fatalf("test premise broken: variant %q must differ from %q", tc.org, home)
		}
		t.Run(tc.name, func(t *testing.T) {
			// Subject organization differs by one byte; the subject itself is
			// enabled, complete and carries the owner role plus the scoped
			// role — neither may bypass the organization check.
			subjectReq := orgNameRequest(home)
			subjectReq.SubjectOrg = tc.org
			subjectReq.Subject.Roles = []string{"owner", orgNameScope + ":" + orgNameAction}
			wantOrgNameEnvelopeDenial(t, s.Decide(home, subjectReq),
				"organization mismatch", "Decide with differing subject org")
			wantOrgNameEnvelopeDenial(t, s.Review(home, 1, subjectReq),
				"organization mismatch", "Review with differing subject org")

			// Resource organization differs, owner role again rides along.
			resourceReq := orgNameRequest(home)
			resourceReq.ResourceOrg = tc.org
			resourceReq.Subject.Roles = []string{"owner"}
			wantOrgNameEnvelopeDenial(t, s.Decide(home, resourceReq),
				"organization mismatch", "Decide with differing resource org")
			wantOrgNameEnvelopeDenial(t, s.Review(home, 1, resourceReq),
				"organization mismatch", "Review with differing resource org")
		})
	}

	// Control: the very same complete request with all three organizations
	// equal reaches the home organization's matching policy, with or without
	// the owner role — the denials above come from the differing name alone.
	withOwner := orgNameRequest(home)
	withOwner.Subject.Roles = []string{"owner"}
	d := s.Decide(home, withOwner)
	if !d.Allowed || d.Reason != "matched allow policy" || d.Version != 1 ||
		!reflect.DeepEqual(d.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("aligned request with owner = %+v, want the home org's own allow", d)
	}
	noRoles := s.Decide(home, orgNameRequest(home))
	if !noRoles.Allowed || noRoles.Version != 1 ||
		!reflect.DeepEqual(noRoles.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("aligned request without roles = %+v, want the policy to decide", noRoles)
	}
}

// TestMismatchDecisionIsAuditedOnlyUnderDecidingOrg pins audit attribution:
// every non-empty-org Decide appends exactly one record under the deciding
// organization, preserving the submitted organization bytes and the complete
// decision. An organization-mismatch denial is recorded under the deciding
// organization only — never under the subject's or resource's organization,
// which gains no records or versions merely from being named in the rejected
// request — and reading each organization's chain never mixes in the other.
func TestMismatchDecisionIsAuditedOnlyUnderDecidingOrg(t *testing.T) {
	const home = "acme"
	const alien = "Acme"                // one byte different from home; publishes its own v1
	rawAlien := "组" + invalidByte + "织" // never publishes, invalid UTF-8

	s := NewStore()
	if v, err := s.Publish(home, 0, []Policy{orgNamePolicy(EffectAllow)}); err != nil || v != 1 {
		t.Fatalf("home publish = %d, %v", v, err)
	}
	if v, err := s.Publish(alien, 0, []Policy{orgNamePolicy(EffectDeny)}); err != nil || v != 1 {
		t.Fatalf("alien publish = %d, %v", v, err)
	}

	alienBefore, alienCPBefore, err := s.AuditExport(alien, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(alienBefore) != 1 {
		t.Fatalf("alien setup chain = %d records, want 1", len(alienBefore))
	}

	// 1) Subject organization differs: the mismatch denial lands under home.
	subjectReq := orgNameRequest(home)
	subjectReq.SubjectOrg = alien
	d1 := s.Decide(home, subjectReq)
	wantOrgNameEnvelopeDenial(t, d1, "organization mismatch", "subject-org mismatch")
	if got := orgChainLen(t, s, home); got != 2 {
		t.Fatalf("home chain = %d after one decision, want 2", got)
	}
	if got := orgChainLen(t, s, alien); got != 1 {
		t.Fatalf("subject org gained a record from a denial decided elsewhere: %d", got)
	}

	// 2) Resource organization differs: again exactly one record under home.
	resourceReq := orgNameRequest(home)
	resourceReq.ResourceOrg = alien
	d2 := s.Decide(home, resourceReq)
	wantOrgNameEnvelopeDenial(t, d2, "organization mismatch", "resource-org mismatch")
	if got := orgChainLen(t, s, home); got != 3 {
		t.Fatalf("home chain = %d after two decisions, want 3", got)
	}
	if got := orgChainLen(t, s, alien); got != 1 {
		t.Fatalf("resource org gained a record from a denial decided elsewhere: %d", got)
	}

	// 3) Subject organization is a never-seen invalid-UTF-8 name. The denial
	// is recorded under home with the raw name preserved; the named
	// organization itself acquires no record and no published version.
	rawReq := orgNameRequest(home)
	rawReq.SubjectOrg = rawAlien
	d3 := s.Decide(home, rawReq)
	wantOrgNameEnvelopeDenial(t, d3, "organization mismatch", "raw-byte subject-org mismatch")
	if got := orgChainLen(t, s, home); got != 4 {
		t.Fatalf("home chain = %d after three decisions, want 4", got)
	}
	rawExport, rawCP, err := s.AuditExport(rawAlien, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rawExport) != 0 || rawCP.EndSeq != 0 || rawCP.Fingerprint != genesisFingerprint(rawAlien) {
		t.Fatalf("raw-byte subject org gained state from being named: %+v %+v", rawExport, rawCP)
	}
	if v := s.CurrentVersion(rawAlien); v != 0 {
		t.Fatalf("raw-byte subject org current version = %d, want 0", v)
	}

	// Inspect home's complete chain: one publish plus the three mismatch
	// decisions, every record attributed to home, the requests preserving the
	// differing organization bytes and each stored decision equaling the
	// decision that was returned. The invalid-byte payload must still verify.
	homeRecs, homeCP, err := s.AuditExport(home, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(homeRecs) != 4 {
		t.Fatalf("home chain = %d records, want 4", len(homeRecs))
	}
	if err := VerifyAudit(home, homeRecs, homeCP); err != nil {
		t.Fatalf("home chain with raw-byte requests must verify: %v", err)
	}
	for i := range homeRecs {
		if homeRecs[i].Org != home {
			t.Fatalf("home record %d attributed to %q", homeRecs[i].Seq, homeRecs[i].Org)
		}
	}
	record2 := homeRecs[1].Decision
	if record2 == nil || record2.Request.SubjectOrg != alien || record2.Request.ResourceOrg != home {
		t.Fatalf("seq 2 request = %+v, want subject org %q preserved", record2, alien)
	}
	if !reflect.DeepEqual(record2.Decision, d1) {
		t.Fatalf("seq 2 decision = %+v, want returned %+v", record2.Decision, d1)
	}
	record3 := homeRecs[2].Decision
	if record3 == nil || record3.Request.ResourceOrg != alien || record3.Request.SubjectOrg != home {
		t.Fatalf("seq 3 request = %+v, want resource org %q preserved", record3, alien)
	}
	if !reflect.DeepEqual(record3.Decision, d2) {
		t.Fatalf("seq 3 decision = %+v, want returned %+v", record3.Decision, d2)
	}
	record4 := homeRecs[3].Decision
	if record4 == nil || record4.Request.SubjectOrg != rawAlien {
		t.Fatalf("seq 4 request = %+v, want raw subject org %q byte for byte", record4, rawAlien)
	}
	if !reflect.DeepEqual(record4.Decision, d3) {
		t.Fatalf("seq 4 decision = %+v, want returned %+v", record4.Decision, d3)
	}

	// The paged query view shows the same boundary: only home-attributed
	// decision records, with no record appearing under another name.
	paged := drainAudit(t, s, home, AuditDecision, "")
	if len(paged) != 3 {
		t.Fatalf("home decision page = %d records, want 3", len(paged))
	}
	for _, r := range paged {
		if r.Org != home {
			t.Fatalf("home query returned a record attributed to %q", r.Org)
		}
	}

	// The alien organization's chain is byte-for-byte unchanged across all
	// three mismatch decisions, checkpoint included.
	alienAfter, alienCPAfter, err := s.AuditExport(alien, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(alienAfter, alienBefore) || alienCPAfter != alienCPBefore {
		t.Fatalf("alien chain changed despite never being the deciding org")
	}
	if err := VerifyAudit(alien, alienAfter, alienCPAfter); err != nil {
		t.Fatalf("alien chain must still verify: %v", err)
	}

	// When the alien organization makes its own coherent decision, that
	// record lands under the alien organization — and nowhere else — proving
	// attribution follows the deciding org in both directions.
	alienDecision := s.Decide(alien, orgNameRequest(alien))
	if alienDecision.Allowed || alienDecision.Version != 1 ||
		!reflect.DeepEqual(alienDecision.Matched, []string{orgNamePolicyID}) {
		t.Fatalf("alien coherent decision = %+v, want its own version-1 deny", alienDecision)
	}
	if got := orgChainLen(t, s, alien); got != 2 {
		t.Fatalf("alien chain = %d after its own decision, want 2", got)
	}
	if got := orgChainLen(t, s, home); got != 4 {
		t.Fatalf("home chain = %d after the alien's own decision, want 4 (no cross append)", got)
	}
	alienRecs, _, err := s.AuditExport(alien, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := alienRecs[1]
	if last.Org != alien || last.Kind != AuditDecision || last.Decision == nil {
		t.Fatalf("alien seq 2 = %+v, want a decision attributed to alien", last)
	}
	if last.Decision.Request.SubjectOrg != alien || last.Decision.Request.ResourceOrg != alien {
		t.Fatalf("alien seq 2 request orgs = %q/%q, want both %q",
			last.Decision.Request.SubjectOrg, last.Decision.Request.ResourceOrg, alien)
	}
	if !reflect.DeepEqual(last.Decision.Decision, alienDecision) {
		t.Fatalf("alien seq 2 decision = %+v, want returned %+v",
			last.Decision.Decision, alienDecision)
	}
}
