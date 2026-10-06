// Command publish_atomicity_example is the worked example for organizational
// policy publishing: why a failed replacing commit leaves the standing
// authorization completely untouched, and how the version returned by
// Publish, the version actually used by a decision, and audit sequence
// numbers are three different numbers.
//
// Run with:
//
//	go run ./examples/publish_atomicity
//
// The program takes no arguments, writes no files, and works fully offline.
// It uses only the public Store API and the Go standard library, creates its
// own in-memory store and requests, and prints state transitions next to
// audit state, so none of the three numbers above has to be inferred.
//
// One organization (acme factory), one enabled subject (svc-billing) reading
// one resource (billing-2026) at scope acme/factory/billing; subject and
// resource both belong to the decision organization and the scope is legal,
// so access is decided entirely by published policies with the organization
// default-deny / deny-overrides rules.
//
// The steps:
//
//  1. Publish an allowing read policy as version 1; the identical read is
//     allowed with reason, matched id and applied version all from version 1.
//     The publish and the decision each leave their own audit record.
//  2. Submit a NEW COMPLETE SET meant to flip the read to deny: one valid
//     deny policy plus one policy whose scope contains consecutive slashes
//     ("acme//factory/billing"). Validation happens before any state change,
//     so Publish reports ErrInvalidPolicySet, the valid deny policy is never
//     applied first, no version number is consumed, and no policy-change
//     record appears. The exported checkpoint is bit-for-bit the one taken
//     before the attempt.
//  3. To observe the effect, run the same Decide AFTER the failed publish.
//     That observation is a separate operation: the read is still evaluated
//     under version 1 (allow, v1 policy id, version 1) and leaves its OWN
//     new decision record under the ordinary decision rules. That record
//     must not be counted as a change produced by the failed publish.
//  4. Fix the invalid policy (consecutive slashes removed) and resubmit the
//     whole set against the UNCHANGED current version 1: it becomes version
//  2. Publishing replaces the whole set rather than appending to it: the
//     v1 allow id is absent from the version-2 change record, and the read
//     is denied with a clear denial reason, the deny policy id and version
//  2. The old version still serves its original content via Policies.
//  5. The other commit boundary: fully valid content submitted against a
//     stale expected version returns ErrVersionConflict; current policy is
//     not overwritten and no change record is left. Callers re-read the
//     current version, reconcile, and decide whether to resubmit.
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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "publish atomicity example: failed:", err)
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

	// ---- Step 1: publish an allowing policy set as version 1. ------------
	fmt.Println("step 1: publish the allow-read policy set")
	v1Set := []darksafe.Policy{{
		ID:      "p-billing-read-allow",
		Subject: subjectID,
		Action:  actionRead,
		Scope:   scope,
		Effect:  darksafe.EffectAllow,
	}}
	publishedV1, err := store.Publish(org, 0, v1Set) // 0: never published
	if err != nil {
		return fmt.Errorf("publish v1: %w", err)
	}
	fmt.Printf("  Publish -> new version %d\n", publishedV1)
	printPolicySet(v1Set)

	d1 := store.Decide(org, req)
	printDecision("Decide ", d1)
	if !d1.Allowed || d1.Version != 1 ||
		!equalStrings(d1.Matched, []string{"p-billing-read-allow"}) {
		return fmt.Errorf("step 1 decision = %+v, want allow on p-billing-read-allow at version 1", d1)
	}
	// seq 1 policy_change (version 1), seq 2 decision (the allow above).
	headAfterV1 := snapshotHead(store, "after v1 publish + allow decision")
	fmt.Println()

	// ---- Step 2: the failed replacing commit. ----------------------------
	fmt.Println("step 2: replace the set with a batch containing an illegal policy")
	badSet := []darksafe.Policy{
		{
			ID:      "p-billing-read-deny", // legal by itself: would flip the read to deny
			Subject: subjectID,
			Action:  actionRead,
			Scope:   scope,
			Effect:  darksafe.EffectDeny,
		},
		{
			ID:      "p-billing-read-extra",
			Subject: subjectID,
			Action:  actionRead,
			Scope:   "acme//factory/billing", // consecutive slashes: illegal scope
			Effect:  darksafe.EffectAllow,
		},
	}
	printPolicySet(badSet)
	badVersion, err := store.Publish(org, publishedV1, badSet)
	fmt.Printf("  Publish -> returned version=%d err=%v\n", badVersion, err)
	if !errors.Is(err, darksafe.ErrInvalidPolicySet) {
		return fmt.Errorf("failed publish error = %v, want ErrInvalidPolicySet", err)
	}
	if errors.Is(err, darksafe.ErrVersionConflict) {
		return fmt.Errorf("failed publish error = %v must not be a version conflict", err)
	}
	if badVersion != 0 {
		return fmt.Errorf("failed publish returned version %d, want 0 (no usable version)", badVersion)
	}

	// Current version and historical content are exactly as before; the
	// would-be next version does not exist; the legal deny was not applied.
	if got := store.CurrentVersion(org); got != publishedV1 {
		return fmt.Errorf("current version = %d, want %d after failed publish", got, publishedV1)
	}
	if got, err := store.Policies(org, publishedV1); err != nil || !equalPolicyIDs(got, v1Set) {
		return fmt.Errorf("version %d content = %v, %v; want the original v1 set", publishedV1, policyIDs(got), err)
	}
	if _, err := store.Policies(org, publishedV1+1); !errors.Is(err, darksafe.ErrVersionNotFound) {
		return fmt.Errorf("version %d exists after failed publish, err=%v", publishedV1+1, err)
	}
	// The failed publish adds no record and moves no checkpoint: the chain
	// head is bit-for-bit the one captured before the attempt.
	snapshotHead(store, "immediately after failed publish").mustEqual(headAfterV1)
	fmt.Println()

	// ---- Step 3: observing the effect is a separate Decide. --------------
	fmt.Println("step 3: observe with the same read after the failed publish")
	dObs := store.Decide(org, req)
	printDecision("Decide ", dObs)
	if dObs.Allowed != d1.Allowed || dObs.Reason != d1.Reason ||
		!equalStrings(dObs.Matched, d1.Matched) || dObs.Version != d1.Version {
		return fmt.Errorf("observation decision = %+v, want exactly the step-1 verdict %+v", dObs, d1)
	}
	// The observation appends its own decision record (seq 3). The failed
	// publish is not its cause; ordinary Decide recording rules are.
	headAfterObservation := snapshotHead(store, "after the observation decision")
	if headAfterObservation.endSeq != headAfterV1.endSeq+1 {
		return fmt.Errorf("chain head = %d, want %d after one observation",
			headAfterObservation.endSeq, headAfterV1.endSeq+1)
	}
	fmt.Printf("  the failed publish added no record; seq %d comes from this separate Decide, not from the failure\n",
		headAfterObservation.endSeq)
	fmt.Println()

	// ---- Step 4: correct the policy, resubmit the whole set. -------------
	fmt.Println("step 4: fix the scope and resubmit the complete set against current version 1")
	fixedSet := []darksafe.Policy{
		{
			ID:      "p-billing-read-deny",
			Subject: subjectID,
			Action:  actionRead,
			Scope:   scope,
			Effect:  darksafe.EffectDeny,
		},
		{
			ID:      "p-billing-read-extra",
			Subject: subjectID,
			Action:  actionRead,
			Scope:   "acme/factory/billing/audit", // consecutive slashes removed
			Effect:  darksafe.EffectAllow,
		},
	}
	printPolicySet(fixedSet)
	publishedV2, err := store.Publish(org, store.CurrentVersion(org), fixedSet)
	if err != nil {
		return fmt.Errorf("corrected publish: %w", err)
	}
	fmt.Printf("  Publish -> new version %d (immediate successor of %d)\n", publishedV2, publishedV1)
	if publishedV2 != publishedV1+1 {
		return fmt.Errorf("new version = %d, want %d", publishedV2, publishedV1+1)
	}

	d2 := store.Decide(org, req)
	printDecision("Decide ", d2)
	if d2.Allowed || d2.Reason != "matched deny policy" ||
		!equalStrings(d2.Matched, []string{"p-billing-read-deny"}) || d2.Version != publishedV2 {
		return fmt.Errorf("step 4 decision = %+v, want deny on p-billing-read-deny at version %d", d2, publishedV2)
	}

	// Publishing replaces the WHOLE set; the version-2 change record stores
	// exactly the submitted set, with no v1 allow policy carried over.
	recs, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export after corrected publish: %w", err)
	}
	changeV2 := lastPolicyChange(recs)
	if changeV2 == nil || changeV2.Version != publishedV2 {
		return fmt.Errorf("no policy_change record for version %d in %d records", publishedV2, len(recs))
	}
	if !equalPolicyIDs(changeV2.Policies, fixedSet) {
		return fmt.Errorf("v2 change record stores %v, want the full submitted set %v",
			policyIDs(changeV2.Policies), policyIDs(fixedSet))
	}
	fmt.Printf("  version-%d change record stores the complete new set %q (nothing appended from v1)\n",
		publishedV2, policyIDs(changeV2.Policies))
	// History is immutable: the old version still serves its original set.
	oldSet, err := store.Policies(org, publishedV1)
	if err != nil {
		return fmt.Errorf("query old version: %w", err)
	}
	fmt.Printf("  Policies(org, %d) still returns the original set %q\n", publishedV1, policyIDs(oldSet))
	headAfterV2 := snapshotHead(store, "after v2 publish + deny decision")
	fmt.Println()

	// ---- Step 5: legal content, stale expected version. ------------------
	fmt.Println("step 5: submit legal content against a stale expected version")
	staleSet := []darksafe.Policy{{
		ID:      "p-billing-read-allow",
		Subject: subjectID,
		Action:  actionRead,
		Scope:   scope,
		Effect:  darksafe.EffectAllow,
	}}
	printPolicySet(staleSet)
	staleVersion, err := store.Publish(org, publishedV1, staleSet) // current is already 2
	fmt.Printf("  Publish -> returned version=%d err=%v\n", staleVersion, err)
	if !errors.Is(err, darksafe.ErrVersionConflict) {
		return fmt.Errorf("stale publish error = %v, want ErrVersionConflict", err)
	}
	if staleVersion != 0 {
		return fmt.Errorf("stale publish returned version %d, want 0", staleVersion)
	}
	// Nothing is overwritten and no record is left.
	if got := store.CurrentVersion(org); got != publishedV2 {
		return fmt.Errorf("current version = %d, want %d after stale submit", got, publishedV2)
	}
	if current, err := store.Policies(org, publishedV2); err != nil || !equalPolicyIDs(current, fixedSet) {
		return fmt.Errorf("current set = %v, %v; want the fixed set unchanged", policyIDs(current), err)
	}
	fmt.Printf("  current version stays %d and its set is untouched; re-read it before resubmitting\n", publishedV2)
	snapshotHead(store, "immediately after stale submit").mustEqual(headAfterV2)

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
		fmt.Fprintf(os.Stderr, "publish atomicity example: export for %q: %v\n", label, err)
		os.Exit(1)
	}
	fmt.Printf("  [audit] %-44s records=%d head-seq=%d head-fingerprint=%s\n",
		label, len(records), cp.EndSeq, cp.Fingerprint)
	return headState{endSeq: cp.EndSeq, fingerprint: cp.Fingerprint, count: len(records), label: label}
}

func (h headState) mustEqual(want headState) {
	if h.endSeq != want.endSeq || h.fingerprint != want.fingerprint || h.count != want.count {
		fmt.Fprintf(os.Stderr, "publish atomicity example: audit state for %q = %+v, want %+v\n",
			h.label, h, want)
		os.Exit(1)
	}
	fmt.Printf("  [audit] checkpoint unchanged vs %q: head-seq=%d fingerprint=%s\n",
		want.label, h.endSeq, h.fingerprint)
}

func printPolicySet(policies []darksafe.Policy) {
	for _, p := range policies {
		fmt.Printf("    policy id=%q subject=%q action=%q scope=%q effect=%q recursive=%v resource-id=%q\n",
			p.ID, p.Subject, p.Action, p.Scope, p.Effect, p.Recursive, p.ResourceID)
	}
}

func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("  %s -> allowed=%v reason=%q matched=%q applied-version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}

// lastPolicyChange returns the payload of the final policy_change record in
// an exported chain, or nil when there is none.
func lastPolicyChange(records []darksafe.AuditRecord) *darksafe.PolicyChange {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind == darksafe.AuditPolicyChange {
			return records[i].Change
		}
	}
	return nil
}

func policyIDs(policies []darksafe.Policy) []string {
	ids := make([]string, len(policies))
	for i, p := range policies {
		ids[i] = p.ID
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalPolicyIDs(policies, want []darksafe.Policy) bool {
	return equalStrings(policyIDs(policies), policyIDs(want))
}
