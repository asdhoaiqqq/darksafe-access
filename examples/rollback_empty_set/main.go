// Command rollback_empty_set_example is the worked example for rolling an
// organization back to a previously published EMPTY policy version. It ties
// together two facts the API documents separately: a rollback always creates
// a NEW version that copies a historical version's content, and an empty
// published policy set default-denies. The example shows that the empty set
// is a genuine rollback target: restoring it revokes a current allow, the
// historical versions stay readable, the audit chain records both the
// rollback and the later denial as separate entries, and the two rollback
// failures (target never published, stale expected current version) are
// distinguishable and change nothing.
//
// Run with:
//
//	go run ./examples/rollback_empty_set
//
// The program takes no arguments, writes no files, and works fully offline.
// It uses only the public Store API and the Go standard library and creates
// its own in-memory store and requests.
//
// One organization (acme factory), one enabled subject (svc-billing) reading
// one resource (billing-2026) at scope acme/factory/billing; subject org,
// resource org and decision org are all the same, the request fields are
// complete and the scope is legal, so the outcome is decided entirely by the
// organization's current published policy set.
//
// The steps:
//
//  1. Publish the empty policy set as version 1. It is a real published
//     version; its content simply is the empty collection.
//  2. Publish a policy allowing the read as version 2; the same read is
//     allowed, with the policy id and applied version 2 reported.
//  3. Roll back with the current version 2 as the expected version and
//     version 1 as the target. Rollback returns the NEW version 3 (never the
//     target number), the current version becomes 3, and querying version 3
//     returns the empty set copied from version 1.
//  4. Submit the same read again: default denial, reason
//     "no matching allow policy", an empty hit list, applied version 3.
//
// Then the audit trail is printed: the rollback record (new version 3,
// source version 1, explicitly marked as a rollback, stored policy set
// empty) and the denial decision record are two different records with two
// different audit sequences; the audit sequence is never a policy version.
// Versions 1 and 2 are still queryable with their original content.
//
// Two failures that directly target this operation follow: target version 0
// returns ErrVersionNotFound because 0 is not a published historical
// version, and a stale expected current version against the existing target
// version 1 returns ErrVersionConflict. Neither advances the version,
// changes current policy, or appends an audit record.
//
// A final contrast on a never-published organization separates the empty
// published version 1 from version 0: with nothing ever published, Decide
// denies with "organization has no published version" at version 0, and
// version 0 cannot be queried or used as a Review/Rollback target.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	org        = "acme factory"
	subjectID  = "svc-billing"
	resourceID = "billing-2026"
	scope      = "acme/factory/billing"
	actionRead = "read"
	allowID    = "p-billing-read-allow"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rollback empty set example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	store := darksafe.NewStore()

	// The one request used throughout: subject org and resource org both
	// equal the decision organization, the subject is enabled and the scope
	// is legal. Decide consults published policies only; the subject carries
	// no role at all.
	req := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "service"},
		Resource:    darksafe.Resource{ID: resourceID, Scope: scope},
		Action:      actionRead,
	}
	allowSet := []darksafe.Policy{{
		ID:      allowID,
		Subject: subjectID,
		Action:  actionRead,
		Scope:   scope,
		Effect:  darksafe.EffectAllow,
	}}

	// ---- Step 1: publish the empty set as version 1. ---------------------
	fmt.Println("step 1: publish an empty policy set as version 1")
	v1, err := store.Publish(org, 0, []darksafe.Policy{}) // 0: never published
	if err != nil {
		return fmt.Errorf("publish empty v1: %w", err)
	}
	fmt.Printf("  Publish(0 policies, expected=0) -> new version %d\n", v1)
	printPolicies(org, v1, store)
	if v1 != 1 {
		return fmt.Errorf("empty publish returned version %d, want 1", v1)
	}
	if got := mustPolicies(org, v1, store); len(got) != 0 {
		return fmt.Errorf("version %d carries %d policies, want the empty set", v1, len(got))
	}
	fmt.Println("  version 1 is a real published version whose content is the empty set")
	fmt.Println()

	// ---- Step 2: publish the allow policy as version 2. ------------------
	fmt.Println("step 2: publish the allow-read policy set as version 2")
	printPolicy(allowSet[0])
	v2, err := store.Publish(org, v1, allowSet)
	if err != nil {
		return fmt.Errorf("publish allow v2: %w", err)
	}
	fmt.Printf("  Publish(1 policy, expected=%d) -> new version %d\n", v1, v2)
	dAllow := store.Decide(org, req)
	printDecision("Decide ", dAllow)
	wantAllow := darksafe.Decision{
		Allowed: true, Reason: "matched allow policy",
		Matched: []string{allowID}, Version: v2,
	}
	if !decisionEqual(dAllow, wantAllow) {
		return fmt.Errorf("v2 decision = %+v, want %+v", dAllow, wantAllow)
	}
	fmt.Println()

	// ---- Step 3: roll back from version 2 to the empty version 1. --------
	fmt.Println("step 3: roll back: current version 2, target historical version 1")
	v3, err := store.Rollback(org, v2, v1) // expected current 2, target 1
	if err != nil {
		return fmt.Errorf("rollback to v1: %w", err)
	}
	fmt.Printf("  Rollback(expected=%d, target=%d) -> new version %d\n", v2, v1, v3)
	if v3 != 3 {
		return fmt.Errorf("rollback returned version %d, want the new version 3", v3)
	}
	if got := store.CurrentVersion(org); got != v3 {
		return fmt.Errorf("current version = %d, want %d after rollback", got, v3)
	}
	fmt.Printf("  CurrentVersion -> %d\n", store.CurrentVersion(org))
	printPolicies(org, v3, store)
	if got := mustPolicies(org, v3, store); len(got) != 0 {
		return fmt.Errorf("version %d carries %d policies, want the empty set copied from v1", v3, len(got))
	}
	fmt.Println("  the rollback restored version 1's content (the empty set) as a NEW version")
	fmt.Println()

	// ---- Step 4: the same read is now denied by the empty new version. ---
	fmt.Println("step 4: submit the same read again after the rollback")
	dDeny := store.Decide(org, req)
	printDecision("Decide ", dDeny)
	wantDeny := darksafe.Decision{
		Allowed: false, Reason: "no matching allow policy",
		Matched: []string{}, Version: v3,
	}
	if !decisionEqual(dDeny, wantDeny) {
		return fmt.Errorf("post-rollback decision = %+v, want %+v", dDeny, wantDeny)
	}
	fmt.Println()

	// ---- Audit trail: the rollback and the denial are separate records. --
	fmt.Println("audit chain after the rollback (audit sequence is NOT a policy version):")
	recs, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export audit chain: %w", err)
	}
	for _, r := range recs {
		printAuditRecord(r)
	}
	headAfterDeny := snapshotHead(store, "after rollback + denial")
	if len(recs) != 5 {
		return fmt.Errorf("audit records = %d, want 5", len(recs))
	}
	rollbackRec := recs[3]
	if rollbackRec.Kind != darksafe.AuditPolicyChange || rollbackRec.Change == nil {
		return fmt.Errorf("seq 4 is %s, want a policy_change record", rollbackRec.Kind)
	}
	chg := rollbackRec.Change
	if chg.Version != v3 || chg.SourceVersion != v1 || !chg.RolledBack || len(chg.Policies) != 0 {
		return fmt.Errorf("rollback record = %+v, want version %d copied from %d, marked rollback, empty set",
			chg, v3, v1)
	}
	denialRec := recs[4]
	if denialRec.Kind != darksafe.AuditDecision || denialRec.Decision == nil ||
		!decisionEqual(denialRec.Decision.Decision, wantDeny) {
		return fmt.Errorf("seq 5 is not the post-rollback denial: %+v", denialRec)
	}
	fmt.Printf("  seq %d is the rollback record (new version %d, copied from version %d);\n",
		rollbackRec.Seq, v3, v1)
	fmt.Printf("  seq %d is the separate denial decision produced by the step-4 read\n", denialRec.Seq)
	fmt.Println()

	// ---- History is intact: v1 empty and v2 allow are still queryable. ---
	fmt.Println("history was not deleted or rewritten by the rollback:")
	printPolicies(org, v1, store)
	printPolicies(org, v2, store)
	rev1 := store.Review(org, v1, req)
	rev2 := store.Review(org, v2, req)
	printReview(v1, rev1)
	printReview(v2, rev2)
	if rev1.Allowed || rev1.Reason != "no matching allow policy" || rev1.Version != v1 {
		return fmt.Errorf("review v1 = %+v, want the empty-set denial at version 1", rev1)
	}
	if !decisionEqual(rev2, wantAllow) {
		return fmt.Errorf("review v2 = %+v, want the preserved %+v", rev2, wantAllow)
	}
	fmt.Println()

	// ---- Failure A: target version 0 is not a published history entry. ---
	fmt.Println("step 5: failure A - target version 0 was never published")
	missingVersion, errMissing := store.Rollback(org, v3, 0)
	fmt.Printf("  Rollback(expected=%d, target=0) -> returned version=%d err=%v\n", v3, missingVersion, errMissing)
	if !errors.Is(errMissing, darksafe.ErrVersionNotFound) {
		return fmt.Errorf("target-0 rollback error = %v, want ErrVersionNotFound", errMissing)
	}
	if errors.Is(errMissing, darksafe.ErrVersionConflict) {
		return fmt.Errorf("target-0 rollback error = %v must not be a version conflict", errMissing)
	}
	if missingVersion != 0 {
		return fmt.Errorf("target-0 rollback returned version %d, want 0", missingVersion)
	}
	fmt.Println("  ErrVersionNotFound: 0 is not a published historical version (the empty set is version 1, not 0)")
	fmt.Println()

	// ---- Failure B: target v1 exists, expected current version is stale. -
	fmt.Println("step 6: failure B - target version 1 exists, but the expected current version is stale")
	staleVersion, errStale := store.Rollback(org, v2, v1) // current is already 3
	fmt.Printf("  Rollback(expected=%d, target=%d) -> returned version=%d err=%v\n", v2, v1, staleVersion, errStale)
	if !errors.Is(errStale, darksafe.ErrVersionConflict) {
		return fmt.Errorf("stale rollback error = %v, want ErrVersionConflict", errStale)
	}
	if staleVersion != 0 {
		return fmt.Errorf("stale rollback returned version %d, want 0", staleVersion)
	}
	fmt.Println("  ErrVersionConflict: the current version is already 3; re-read it before retrying")
	fmt.Println()

	// ---- Both failures leave state and audit exactly as they were. -------
	fmt.Println("both failures leave everything in place:")
	if got := store.CurrentVersion(org); got != v3 {
		return fmt.Errorf("current version = %d after failures, want %d", got, v3)
	}
	fmt.Printf("  CurrentVersion -> %d\n", store.CurrentVersion(org))
	printPolicies(org, v3, store)
	snapshotHead(store, "immediately after both failures").mustEqual(headAfterDeny)
	fmt.Println()

	// ---- Contrast: an empty PUBLISHED version vs never-published v0. ------
	fmt.Println("contrast: the empty published version 1 vs never-published version 0")
	_, errP0 := store.Policies(org, 0)
	if !errors.Is(errP0, darksafe.ErrVersionNotFound) {
		return fmt.Errorf("Policies(%q, 0) error = %v, want ErrVersionNotFound", org, errP0)
	}
	fmt.Printf("  Policies(%q, 0) -> err=%v\n", org, errP0)
	fmt.Println("    (versions 1-3 are published, but no version 0 ever existed)")

	freshOrg := "acme greenfield"
	freshReq := darksafe.OrgRequest{
		SubjectOrg:  freshOrg,
		ResourceOrg: freshOrg,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "service"},
		Resource:    darksafe.Resource{ID: resourceID, Scope: scope},
		Action:      actionRead,
	}
	fmt.Printf("  separate never-published organization %q:\n", freshOrg)
	fmt.Printf("    CurrentVersion -> %d\n", store.CurrentVersion(freshOrg))
	dFresh := store.Decide(freshOrg, freshReq)
	printDecision("Decide ", dFresh)
	wantFresh := darksafe.Decision{Allowed: false, Reason: "organization has no published version"}
	if dFresh.Allowed != wantFresh.Allowed || dFresh.Reason != wantFresh.Reason ||
		dFresh.Version != 0 || len(dFresh.Matched) != 0 {
		return fmt.Errorf("never-published decision = %+v, want %+v with version 0 and no matches", dFresh, wantFresh)
	}
	revZero := store.Review(freshOrg, 0, freshReq)
	printReview(0, revZero)
	if revZero.Allowed || revZero.Reason != "version 0 not found" {
		return fmt.Errorf("review v0 = %+v, want \"version 0 not found\"", revZero)
	}
	fmt.Println("    the denial reason differs from the empty-set case:")
	fmt.Printf("      never published : %q at version 0 (no published set exists)\n", dFresh.Reason)
	fmt.Printf("      empty set v1/v3 : %q at the published version (a set with zero policies exists)\n",
		"no matching allow policy")
	// The greenfield Decide belongs to that organization's own chain.
	freshRecs, freshCP, err := store.AuditExport(freshOrg, 0)
	if err != nil {
		return fmt.Errorf("export greenfield chain: %w", err)
	}
	if err := darksafe.VerifyAudit(freshOrg, freshRecs, freshCP); err != nil {
		return fmt.Errorf("greenfield chain does not verify: %w", err)
	}
	fmt.Printf("    that denial is seq %d of %q's own chain, not a record of %q\n",
		len(freshRecs), freshOrg, org)

	return nil
}

// headState is the audit chain head captured through a complete export:
// record count, highest sequence and the fingerprint covering it. Two
// snapshots compare bit-for-bit, so a no-record failure is directly visible.
type headState struct {
	endSeq      int
	fingerprint string
	count       int
	label       string
}

func snapshotHead(store *darksafe.Store, label string) headState {
	records, cp, err := store.AuditExport(org, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback empty set example: export for %q: %v\n", label, err)
		os.Exit(1)
	}
	fmt.Printf("  [audit] %-34s records=%d head-seq=%d head-fingerprint=%s\n",
		label, len(records), cp.EndSeq, cp.Fingerprint)
	return headState{endSeq: cp.EndSeq, fingerprint: cp.Fingerprint, count: len(records), label: label}
}

func (h headState) mustEqual(want headState) {
	if h.endSeq != want.endSeq || h.fingerprint != want.fingerprint || h.count != want.count {
		fmt.Fprintf(os.Stderr, "rollback empty set example: audit state for %q = %+v, want %+v\n",
			h.label, h, want)
		os.Exit(1)
	}
	fmt.Printf("  [audit] checkpoint unchanged: head-seq=%d fingerprint=%s\n",
		h.endSeq, h.fingerprint)
}

func mustPolicies(org string, version int, store *darksafe.Store) []darksafe.Policy {
	policies, err := store.Policies(org, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback empty set example: Policies(%q, %d): %v\n", org, version, err)
		os.Exit(1)
	}
	return policies
}

func printPolicies(org string, version int, store *darksafe.Store) {
	policies := mustPolicies(org, version, store)
	fmt.Printf("  Policies(org, %d) -> %d policies %q\n", version, len(policies), policyIDs(policies))
}

func printPolicy(p darksafe.Policy) {
	fmt.Printf("    policy id=%q subject=%q action=%q scope=%q effect=%q recursive=%v resource-id=%q\n",
		p.ID, p.Subject, p.Action, p.Scope, p.Effect, p.Recursive, p.ResourceID)
}

func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("  %s -> allowed=%v reason=%q matched=%q applied-version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}

func printReview(version int, d darksafe.Decision) {
	fmt.Printf("  Review(org, %d) -> allowed=%v reason=%q matched=%q version=%d\n",
		version, d.Allowed, d.Reason, d.Matched, d.Version)
}

// printAuditRecord prints one chain entry on a single line, keeping the
// sequence column first so it is visibly a different number from the policy
// version carried in the payload.
func printAuditRecord(r darksafe.AuditRecord) {
	switch r.Kind {
	case darksafe.AuditPolicyChange:
		c := r.Change
		fmt.Printf("  seq=%d %-14s version=%d source=%d rolled-back=%-5v policies=%q\n",
			r.Seq, r.Kind, c.Version, c.SourceVersion, c.RolledBack, policyIDs(c.Policies))
	case darksafe.AuditDecision:
		d := r.Decision.Decision
		fmt.Printf("  seq=%d %-14s allowed=%-5v reason=%q matched=%q applied-version=%d\n",
			r.Seq, r.Kind, d.Allowed, d.Reason, d.Matched, d.Version)
	default:
		fmt.Printf("  seq=%d %s\n", r.Seq, r.Kind)
	}
}

func policyIDs(policies []darksafe.Policy) []string {
	ids := make([]string, len(policies))
	for i, p := range policies {
		ids[i] = p.ID
	}
	return ids
}

// decisionEqual compares the decision fields that matter for this example,
// including the nil-vs-empty shape of an empty hit list.
func decisionEqual(a, b darksafe.Decision) bool {
	if a.Allowed != b.Allowed || a.Reason != b.Reason || a.Version != b.Version {
		return false
	}
	if len(a.Matched) != len(b.Matched) {
		return false
	}
	for i := range a.Matched {
		if a.Matched[i] != b.Matched[i] {
			return false
		}
	}
	return true
}
