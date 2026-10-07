// Command rollback_empty_set_example is the worked example for rolling an
// organization's current policy set back to a previously published EMPTY
// version. A rollback republishes the target version's full content as a new
// consecutive version; it never deletes or rewrites history. An empty set is
// a genuine published version that default-denies with its own version
// number, which is different from version 0 (nothing ever published).
//
// Run with:
//
//	go run ./examples/rollback_empty_set
//
// The program takes no arguments, writes no files, and works fully offline.
// It uses only the public Store API and the Go standard library, creates its
// own in-memory store and requests, and prints each state transition next to
// the audit chain, so the policy versions and audit sequences never have to
// be inferred.
//
// One organization ("acme factory"), one enabled subject
// ("svc-audit-reader") reading one resource ("ledger-2026") at scope
// acme/factory/ledger; subject org, resource org and decision org are all
// the same organization and the scope is legal, so access is decided
// entirely by that organization's published policies with the default-deny
// rules. The steps:
//
//  1. Publish the EMPTY set as version 1. It is a real published version
//     whose content is "no policies"; querying it returns zero policies.
//  2. Publish the one allow-read policy as version 2; the same read is then
//     allowed with the matched policy id and applied version 2.
//  3. Roll back with expectedVersion 2 and targetVersion 1: the empty
//     content becomes the NEW version 3, the current version becomes 3, and
//     the identical read is default-denied with reason
//     "no matching allow policy", an empty matched list and applied
//     version 3. Versions 1 and 2 stay readable with their original
//     content; the rollback copied v1's content into v3 without rewriting
//     either.
//  4. Contrast version 0 on a store that never published anything: the same
//     read is denied with "organization has no published version" at
//     version 0, and Policies(..., 0) is ErrVersionNotFound. Version 0 is
//     not a published historical version; the empty v1/v3 are.
//  5. The audit chain: the rollback record (its own sequence) names the new
//     version 3, the source version 1, the rollback marker and the saved
//     empty set; the later denied read is a separate decision record. Audit
//     sequences are not policy versions.
//  6. Two failures that reach for this operation: target version 0 returns
//     ErrVersionNotFound (nothing was ever published as 0); target v1 with
//     a stale expected current version returns ErrVersionConflict. Neither
//     advances the version, changes the current set or appends a record.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	org        = "acme factory"
	subjectID  = "svc-audit-reader"
	resourceID = "ledger-2026"
	scope      = "acme/factory/ledger"
	actionRead = "read"
	policyID   = "p-ledger-read-2026"
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
	// equal the decision organization, the subject is enabled and carries no
	// role (organization decisions never consult roles), and the scope is
	// legal. Only the current published policy set can change the outcome.
	req := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "service"},
		Resource:    darksafe.Resource{ID: resourceID, Scope: scope},
		Action:      actionRead,
	}

	// ---- Step 1: publish the empty set as version 1. ---------------------
	fmt.Println("step 1: publish an empty policy set as version 1")
	v1, err := store.Publish(org, 0, nil) // 0: this org has never published
	if err != nil {
		return fmt.Errorf("publish empty v1: %w", err)
	}
	fmt.Printf("  Publish(org, expectedVersion=0, empty set) -> new version %d\n", v1)
	printVersionPolicies(store, v1)
	if v1 != 1 {
		return fmt.Errorf("empty publish returned version %d, want 1", v1)
	}
	emptyV1, err := store.Policies(org, v1)
	if err != nil {
		return fmt.Errorf("Policies(v1): %w", err)
	}
	if len(emptyV1) != 0 {
		return fmt.Errorf("version 1 = %v policies, want empty", len(emptyV1))
	}
	fmt.Println()

	// ---- Step 2: publish the allow-read policy as version 2. -------------
	fmt.Println("step 2: publish the allow-read policy as version 2, then read")
	allowSet := []darksafe.Policy{{
		ID:      policyID,
		Subject: subjectID,
		Action:  actionRead,
		Scope:   scope,
		Effect:  darksafe.EffectAllow,
	}}
	printPolicySet(allowSet)
	v2, err := store.Publish(org, v1, allowSet)
	if err != nil {
		return fmt.Errorf("publish allow v2: %w", err)
	}
	fmt.Printf("  Publish(org, expectedVersion=%d, 1 policy) -> new version %d\n", v1, v2)

	dAllow := store.Decide(org, req) // audit seq 3
	printDecision("Decide ", dAllow)
	wantAllow := darksafe.Decision{
		Allowed: true, Reason: "matched allow policy",
		Matched: []string{policyID}, Version: v2,
	}
	if !decisionsEqual(dAllow, wantAllow) {
		return fmt.Errorf("decision under v2 = %+v, want %+v", dAllow, wantAllow)
	}
	fmt.Println()

	// ---- Step 3: roll back to the empty historical version 1. ------------
	fmt.Println("step 3: roll back from current version 2 to the empty version 1")
	v3, err := store.Rollback(org, v2, v1)
	if err != nil {
		return fmt.Errorf("rollback to empty v1: %w", err)
	}
	fmt.Printf("  Rollback(org, expectedVersion=%d, targetVersion=%d) -> new version %d\n", v2, v1, v3)
	if v3 != 3 {
		return fmt.Errorf("rollback returned version %d, want the new version 3", v3)
	}
	if got := store.CurrentVersion(org); got != v3 {
		return fmt.Errorf("CurrentVersion = %d, want %d after rollback", got, v3)
	}
	fmt.Printf("  CurrentVersion(org) = %d\n", store.CurrentVersion(org))
	printVersionPolicies(store, v3)
	emptyV3, err := store.Policies(org, v3)
	if err != nil {
		return fmt.Errorf("Policies(v3): %w", err)
	}
	if len(emptyV3) != 0 {
		return fmt.Errorf("version 3 = %v policies, want the empty set copied from v1", len(emptyV3))
	}

	// The same admissible read is now default-denied under the NEW version.
	dDeny := store.Decide(org, req) // audit seq 5
	printDecision("Decide ", dDeny)
	wantDeny := darksafe.Decision{
		Allowed: false, Reason: "no matching allow policy",
		Matched: []string{}, Version: v3,
	}
	if !decisionsEqual(dDeny, wantDeny) {
		return fmt.Errorf("decision under v3 = %+v, want %+v", dDeny, wantDeny)
	}
	fmt.Println("  the empty set default-denies, but the denial is stamped version 3, not 0")

	// History is immutable: the rollback copied v1's content into v3; v1 and
	// v2 still answer with their original content and verdicts.
	fmt.Println("  history is never deleted or rewritten:")
	printVersionPolicies(store, v1)
	printVersionPolicies(store, v2)
	printVersionPolicies(store, v3)
	printReview(store, v1, req)
	dReviewV2 := printReview(store, v2, req)
	printReview(store, v3, req)
	if !decisionsEqual(dReviewV2, wantAllow) {
		return fmt.Errorf("Review(v2) = %+v, want the preserved allow %+v", dReviewV2, wantAllow)
	}
	fmt.Println()

	// ---- Step 4: contrast version 0 (nothing ever published). ------------
	fmt.Println("step 4: the empty published versions are not the same as version 0")
	fresh := darksafe.NewStore()
	d0 := fresh.Decide(org, req)
	printDecision("Decide on a store that never published ", d0)
	wantV0 := darksafe.Decision{
		Allowed: false, Reason: "organization has no published version",
		Matched: []string{}, Version: 0,
	}
	if !decisionsEqual(d0, wantV0) {
		return fmt.Errorf("decision with no published version = %+v, want %+v", d0, wantV0)
	}
	fmt.Printf("  CurrentVersion(fresh store) = %d\n", fresh.CurrentVersion(org))
	switch _, err := fresh.Policies(org, 0); {
	case err == nil:
		return fmt.Errorf("Policies(fresh, 0) unexpectedly succeeded")
	case errors.Is(err, darksafe.ErrVersionNotFound):
		fmt.Printf("  Policies(fresh, 0) -> %v (ErrVersionNotFound)\n", err)
	default:
		return fmt.Errorf("Policies(fresh, 0) error = %v, want ErrVersionNotFound", err)
	}
	fmt.Println(`  distinction: v1/v3 are published empty sets that deny with "no matching`)
	fmt.Println(`  allow policy" and their own version number; version 0 means no published`)
	fmt.Println(`  version was ever evaluated, and it can never be a rollback target.`)
	fmt.Println()

	// ---- Step 5: the audit chain. ----------------------------------------
	fmt.Println("audit chain of \"acme factory\" after the rollback and the denied read")
	records, cp, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("AuditExport: %w", err)
	}
	fmt.Printf("records=%d\n", len(records))
	for _, r := range records {
		printAuditRecord(r)
	}
	if err := darksafe.VerifyAudit(org, records, cp); err != nil {
		return fmt.Errorf("VerifyAudit: %w", err)
	}
	fmt.Println("VerifyAudit(exported chain): valid")
	fmt.Println("note: seq is the audit chain sequence, not the policy version: the")
	fmt.Println("rollback record is seq 4 and produced policy version 3; the denial at")
	fmt.Printf("seq 5 was decided at policy version %d. The denied read is its own record,\n", v3)
	fmt.Println("separate from the rollback record.")
	fmt.Println()

	// Snapshot the chain head; both failures below must leave it bit-for-bit
	// unchanged.
	headBeforeFailures := snapshotHead(store, "before the two failed rollbacks")

	// ---- Failure 1: target version 0 is not a published history version. --
	fmt.Println("failure 1: roll back to version 0 (it was never published)")
	returnedV1, errNotFound := store.Rollback(org, v3, 0) // expected v3 is current; target 0 is the only problem
	fmt.Printf("  Rollback(org, expectedVersion=%d, targetVersion=0) -> returned version=%d\n", v3, returnedV1)
	switch {
	case errNotFound == nil:
		return fmt.Errorf("rollback to v0 unexpectedly succeeded")
	case errors.Is(errNotFound, darksafe.ErrVersionNotFound):
		fmt.Printf("  err=%v (ErrVersionNotFound)\n", errNotFound)
	case errors.Is(errNotFound, darksafe.ErrVersionConflict):
		return fmt.Errorf("rollback to v0 error = %v, must not be ErrVersionConflict", errNotFound)
	default:
		return fmt.Errorf("rollback to v0 error = %v, want ErrVersionNotFound", errNotFound)
	}
	if returnedV1 != 0 {
		return fmt.Errorf("failed rollback returned version %d, want 0", returnedV1)
	}
	fmt.Println("  cause: 0 is not a published historical version of this organization;")
	fmt.Println("  the empty v1 above is a real version, 0 is not.")
	fmt.Println()

	// ---- Failure 2: target exists, expected current version is stale. -----
	fmt.Println("failure 2: target version 1 exists, but the expected current version is stale")
	returnedV2, errConflict := store.Rollback(org, v2, v1) // current is 3, caller still says 2
	fmt.Printf("  Rollback(org, expectedVersion=%d, targetVersion=%d) -> returned version=%d\n", v2, v1, returnedV2)
	switch {
	case errConflict == nil:
		return fmt.Errorf("stale rollback unexpectedly succeeded")
	case errors.Is(errConflict, darksafe.ErrVersionConflict):
		fmt.Printf("  err=%v (ErrVersionConflict)\n", errConflict)
	case errors.Is(errConflict, darksafe.ErrVersionNotFound):
		return fmt.Errorf("stale rollback error = %v, must not be ErrVersionNotFound (target v1 exists)", errConflict)
	default:
		return fmt.Errorf("stale rollback error = %v, want ErrVersionConflict", errConflict)
	}
	if returnedV2 != 0 {
		return fmt.Errorf("stale rollback returned version %d, want 0", returnedV2)
	}
	fmt.Println("  cause: target v1 is present, but expectedVersion 2 is stale; the")
	fmt.Println("  current version is already 3. Re-read CurrentVersion before retrying.")
	fmt.Println()

	// ---- Both failures leave everything untouched. -----------------------
	fmt.Println("state after both failures")
	if got := store.CurrentVersion(org); got != v3 {
		return fmt.Errorf("CurrentVersion = %d, want %d", got, v3)
	}
	fmt.Printf("  CurrentVersion(org) = %d (not advanced)\n", store.CurrentVersion(org))
	stillEmpty, err := store.Policies(org, v3)
	if err != nil {
		return fmt.Errorf("Policies(v3) after failures: %w", err)
	}
	if len(stillEmpty) != 0 {
		return fmt.Errorf("version 3 = %v policies after failures, want still empty", len(stillEmpty))
	}
	fmt.Printf("  Policies(org, %d) is still the empty set (current policy unchanged)\n", v3)
	// The failed rollbacks themselves added no record and moved no checkpoint.
	snapshotHead(store, "immediately after the two failed rollbacks").mustEqual(headBeforeFailures)

	// An observation Decide made here is a separate operation, exactly as in
	// the failed-publish example: it leaves its OWN new decision record,
	// which must not be counted as a record produced by a failed rollback.
	dObs := store.Decide(org, req)
	printDecision("Decide ", dObs)
	if !decisionsEqual(dObs, wantDeny) {
		return fmt.Errorf("observation decision = %+v, want the same empty-set denial %+v", dObs, wantDeny)
	}
	headAfterObservation := snapshotHead(store, "after the observation decision")
	if headAfterObservation.endSeq != headBeforeFailures.endSeq+1 {
		return fmt.Errorf("chain head = %d, want %d after one observation",
			headAfterObservation.endSeq, headBeforeFailures.endSeq+1)
	}
	fmt.Printf("  seq %d comes from this separate observation Decide, not from either failed rollback\n",
		headAfterObservation.endSeq)

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
	fmt.Printf("  [audit] %-38s records=%d head-seq=%d head-fingerprint=%s\n",
		label, len(records), cp.EndSeq, cp.Fingerprint)
	return headState{endSeq: cp.EndSeq, fingerprint: cp.Fingerprint, count: len(records), label: label}
}

func (h headState) mustEqual(want headState) {
	if h.endSeq != want.endSeq || h.fingerprint != want.fingerprint || h.count != want.count {
		fmt.Fprintf(os.Stderr, "rollback empty set example: audit state for %q = %+v, want %+v\n",
			h.label, h, want)
		os.Exit(1)
	}
	fmt.Printf("  [audit] checkpoint unchanged vs %q: records=%d head-seq=%d head-fingerprint=%s\n",
		want.label, h.count, h.endSeq, h.fingerprint)
}

func printVersionPolicies(store *darksafe.Store, version int) {
	policies, err := store.Policies(org, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback empty set example: Policies(v%d): %v\n", version, err)
		os.Exit(1)
	}
	fmt.Printf("  Policies(org, %d) -> %d policies %q\n", version, len(policies), policyIDs(policies))
}

func printReview(store *darksafe.Store, version int, req darksafe.OrgRequest) darksafe.Decision {
	d := store.Review(org, version, req)
	fmt.Printf("  Review(org, %d, req) -> allowed=%v reason=%q matched=%q version=%d\n",
		version, d.Allowed, d.Reason, d.Matched, d.Version)
	return d
}

func printPolicySet(policies []darksafe.Policy) {
	for _, p := range policies {
		fmt.Printf("    policy id=%q subject=%q action=%q scope=%q effect=%q recursive=%v resource-id=%q\n",
			p.ID, p.Subject, p.Action, p.Scope, p.Effect, p.Recursive, p.ResourceID)
	}
}

func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("  %s -> allowed=%v reason=%q matched=%q version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}

func printAuditRecord(r darksafe.AuditRecord) {
	switch r.Kind {
	case darksafe.AuditPolicyChange:
		c := r.Change
		rollback := ""
		if c.RolledBack {
			rollback = fmt.Sprintf(" (rollback: complete set copied from source version %d)", c.SourceVersion)
		}
		fmt.Printf("  seq=%d policy_change version=%d source-version=%d rolled_back=%-5t saved-policy-ids=%q%s\n",
			r.Seq, c.Version, c.SourceVersion, c.RolledBack, policyIDs(c.Policies), rollback)
	case darksafe.AuditDecision:
		dr := r.Decision
		fmt.Printf("  seq=%d decision      subject=%q action=%q resource=%q allowed=%-5t reason=%q matched=%q version=%d\n",
			r.Seq, dr.Request.Subject.ID, dr.Request.Action, dr.Request.Resource.ID,
			dr.Decision.Allowed, dr.Decision.Reason, dr.Decision.Matched, dr.Decision.Version)
	default:
		fmt.Printf("  seq=%d unknown kind %q\n", r.Seq, r.Kind)
	}
}

func policyIDs(policies []darksafe.Policy) []string {
	ids := make([]string, len(policies))
	for i, p := range policies {
		ids[i] = p.ID
	}
	return ids
}

// decisionsEqual compares the four reported fields; it treats a nil matched
// list and an empty one as equal (both print as []).
func decisionsEqual(a, b darksafe.Decision) bool {
	return a.Allowed == b.Allowed && a.Reason == b.Reason &&
		a.Version == b.Version && len(a.Matched) == len(b.Matched) &&
		stringsEqual(a.Matched, b.Matched)
}

func stringsEqual(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
