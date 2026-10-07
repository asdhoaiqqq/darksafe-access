// Command online_recheck_example is the worked example for re-checking a
// recorded decision while the service is still running: Store.RecheckDecision
// reads one decision record out of the organization's live audit chain and
// replays the request the record saved against the policy version that
// decision actually used. The caller never re-submits the access, and later
// policy publishes can never change the answer.
//
// Run with:
//
//	go run ./examples/online_recheck
//
// The program takes no arguments, writes no files, and works fully offline.
// It builds its own in-memory Store around one organization, one enabled
// subject and one ledger read; the subject organization, the resource
// organization and the decision organization are all the same, the request
// is complete and its scope is legal, so the only thing that moves is the
// organization's published policy set:
//
//  1. Decide before anything is published: a legal request is denied with
//     "organization has no published version" and version 0, and that denial
//     is itself recorded at audit sequence 1.
//  2. Publish an allow-read set as version 1 (audit sequence 2) and decide
//     the same read again: allowed at sequence 3 with the version-1 policy.
//  3. Replace the current set with a deny-read set as version 2 (audit
//     sequence 4). Nothing is decided under it.
//
// Re-checking sequences 1 and 3 then reproduces both original decisions
// exactly, including the version-0 "never published" denial: version 0 is a
// real recorded verdict the replay can reproduce, not a version to look up.
// The program contrasts this with Review (the caller supplies both the
// request and the version, so version 0 is "not found"), shows the two
// record-selection errors, and proves every review is read-only.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "online recheck example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	// One organization, one enabled subject carrying no roles (roles never
	// participate in organization decisions), one ledger.
	const org = "acme factory"
	const (
		ledgerID    = "ledger-2026"
		ledgerScope = "acme/factory/ledger"
		subjectID   = "svc-audit-reader"
	)
	store := darksafe.NewStore()
	req := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "service"},
		Resource:    darksafe.Resource{ID: ledgerID, Scope: ledgerScope},
		Action:      "read",
	}

	fmt.Println("scenario: one org, one enabled subject, one ledger read;")
	fmt.Printf("subject org, resource org and decision org are all %q; request complete, scope legal.\n\n", org)

	// Step 1: decide before any policy is published. The envelope is valid,
	// so the denial is the organization default; version 0 means no published
	// policy was evaluated. The denial is recorded like every other decision.
	fmt.Println("step 1: decide before any policy is published")
	deniedAtV0 := store.Decide(org, req)
	printDecision("  decide", deniedAtV0)
	fmt.Println("  audit : this denial is itself recorded (decision record at seq 1)")
	fmt.Println()

	// Step 2: publish the allow-read set as version 1, then decide the same
	// request again. The publish occupies its own audit sequence before the
	// decision does: policy version numbers never double as audit sequences.
	fmt.Println("step 2: publish an allow-read policy set as version 1, then decide again")
	allowVersion, err := store.Publish(org, 0, []darksafe.Policy{{
		ID:      "p-ledger-read-allow",
		Subject: subjectID,
		Action:  "read",
		Scope:   ledgerScope,
		Effect:  darksafe.EffectAllow,
	}})
	if err != nil {
		return fmt.Errorf("publish allow set: %w", err)
	}
	fmt.Printf("  publish: new version %d (policy-change record at seq 2)\n", allowVersion)
	allowedAtV1 := store.Decide(org, req)
	printDecision("  decide", allowedAtV1)
	fmt.Println("  audit : decision record at seq 3 (audit seq 3 is not policy version 1)")
	fmt.Println()

	// Step 3: replace the current set with a deny-read set as version 2. No
	// decision is made under it; the earlier records must keep their verdicts.
	fmt.Println("step 3: replace the current set with a deny-read set as version 2")
	denyVersion, err := store.Publish(org, allowVersion, []darksafe.Policy{{
		ID:      "p-ledger-read-deny",
		Subject: subjectID,
		Action:  "read",
		Scope:   ledgerScope,
		Effect:  darksafe.EffectDeny,
	}})
	if err != nil {
		return fmt.Errorf("publish deny set: %w", err)
	}
	fmt.Printf("  publish: new version %d (policy-change record at seq 4); no decision made under it\n", denyVersion)
	fmt.Printf("  current version is now %d; the decisions at seq 1 and seq 3 stay as recorded\n\n", denyVersion)

	// Lay out the chain so the recheck targets below are auditable. Policy
	// versions and audit sequences are independent numberings.
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export audit chain: %w", err)
	}
	fmt.Println("audit chain before rechecking (4 records, interleaved kinds):")
	for _, r := range records {
		switch r.Kind {
		case darksafe.AuditPolicyChange:
			fmt.Printf("  seq=%d  policy_change  published version %d\n", r.Seq, r.Change.Version)
		case darksafe.AuditDecision:
			fmt.Printf("  seq=%d  decision       allowed=%-5v version=%d\n",
				r.Seq, r.Decision.Decision.Allowed, r.Decision.Decision.Version)
		}
	}
	fmt.Println()

	// Recheck the two earlier decisions by audit sequence. The record
	// supplies the request and the version it actually used; the caller does
	// not pass either, and the current deny set is never substituted.
	fmt.Println("recheck the two earlier decisions by their audit sequences")
	fmt.Println("(RecheckDecision uses the saved request and the then-current version; no access is re-submitted)")
	for _, seq := range []int{1, 3} {
		original := records[seq-1].Decision.Decision
		rechecked, err := store.RecheckDecision(org, seq)
		if err != nil {
			return fmt.Errorf("recheck seq %d: %w", seq, err)
		}
		fmt.Printf("  recheck seq=%d:\n", seq)
		printDecision("    original ", original)
		printDecision("    rechecked", rechecked)
	}
	fmt.Println(`  seq 1 denies with "organization has no published version" at version 0:`)
	fmt.Println("  the recorded verdict needed no published policy, so it replays without looking one up.")
	fmt.Println("  seq 3 still allows with the version-1 reason, policy id and version 1 itself,")
	fmt.Println("  never the current version-2 deny set.")
	fmt.Println()

	// Contrast with Review: the caller provides BOTH the request and an
	// already-existing historical version. For a legal request, version 0 is
	// not a snapshot that can be selected, so it is reported not found.
	fmt.Println("contrast: Review takes the request AND a version from the caller")
	printDecision("  Review(org, 0, req)", store.Review(org, 0, req))
	printDecision("  Review(org, 1, req)", store.Review(org, 1, req))
	printDecision("  Review(org, 2, req)", store.Review(org, 2, req))
	fmt.Println(`  a legal request at version 0 is "version 0 not found": nothing was ever published as 0,`)
	fmt.Println("  whereas rechecking the version-0 record succeeds because the record states why it used none.")
	fmt.Println()

	// The two ways selecting a record can fail before any judgment runs.
	fmt.Println("selecting the record can fail before any judgment")
	printSelectionError("recheck seq=2", store, org, 2)
	printSelectionError("recheck seq=9", store, org, 9)
	fmt.Println("  ErrAuditNotADecision: seq 2 is the version-1 policy-change record, not an access decision;")
	fmt.Println("  ErrAuditNotFound:     no record exists at seq 9. Neither is an ordinary access denial.")
	fmt.Println()

	// Read-only proof: successful and failed rechecks, and Review, neither
	// move the current version nor append audit records.
	before := len(records)
	fmt.Println("recheck and Review are read-only")
	fmt.Printf("  before: audit records=%d current version=%d\n", before, store.CurrentVersion(org))
	if _, err := store.RecheckDecision(org, 1); err != nil {
		return fmt.Errorf("recheck seq 1: %w", err)
	}
	if _, err := store.RecheckDecision(org, 3); err != nil {
		return fmt.Errorf("recheck seq 3: %w", err)
	}
	if _, err := store.RecheckDecision(org, 2); err == nil {
		return fmt.Errorf("expected ErrAuditNotADecision at seq 2")
	}
	store.Review(org, 1, req)
	after, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("re-export audit chain: %w", err)
	}
	fmt.Printf("  after : audit records=%d current version=%d\n", len(after), store.CurrentVersion(org))
	if len(after) != before {
		return fmt.Errorf("read-only review changed the audit chain: %d -> %d records", before, len(after))
	}
	fmt.Println("  unchanged: no record appended, current policy set untouched.")
	return nil
}

// printDecision prints the four fields every decision carries: allowed or
// not, the reason, the matched policy ids and the version actually used.
func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("%s: allowed=%-5v reason=%q matched=%q version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}

// printSelectionError prints a failed RecheckDecision together with the
// sentinel it matches, so the two selection failures stay distinguishable.
func printSelectionError(label string, store *darksafe.Store, org string, seq int) {
	_, err := store.RecheckDecision(org, seq)
	switch {
	case errors.Is(err, darksafe.ErrAuditNotADecision):
		fmt.Printf("  %s: %v (ErrAuditNotADecision)\n", label, err)
	case errors.Is(err, darksafe.ErrAuditNotFound):
		fmt.Printf("  %s: %v (ErrAuditNotFound)\n", label, err)
	default:
		fmt.Fprintf(os.Stderr, "online recheck example: unexpected error for %s: %v\n", label, err)
		os.Exit(1)
	}
}
