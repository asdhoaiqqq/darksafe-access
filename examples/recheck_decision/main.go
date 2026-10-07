// Command recheck_decision_example shows online recheck of recorded access
// decisions with Store.RecheckDecision: the service re-judges the request
// saved in a decision record against the policy version that record actually
// used, so the caller never resubmits the access and later policy changes
// cannot alter the outcome.
//
// Run with:
//
//	go run ./examples/recheck_decision
//
// The program takes no arguments, writes no files, and works fully offline.
// One organization, one enabled subject, one ledger, one read:
//
//  1. Before anything is published, the read is denied and leaves a
//     decision record whose version is 0 (no published policy was used).
//  2. An allow policy is published as version 1; the same read is allowed
//     and leaves a second decision record.
//  3. The current set is replaced with a deny policy (version 2).
//  4. Rechecking the two earlier records by their audit sequences replays
//     each recorded request against its own recorded version: the version-0
//     denial and the version-1 allow both come back unchanged, never the
//     current deny policy.
//  5. Review(org, 0, req) is a different entry point: the caller supplies
//     the request and an existing historical version, and version 0 is not
//     one — it reports "version 0 not found", while rechecking the
//     version-0 record works fine.
//  6. A sequence naming a policy change record fails with
//     ErrAuditNotADecision and a missing sequence with ErrAuditNotFound;
//     both are record-selection failures, not access denials.
//  7. Rechecking is read-only: the current version and the audit chain are
//     exactly as the publishes left them.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "recheck decision example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	// One organization, one enabled subject, one ledger. The decision,
	// subject and resource organizations all agree, the request is complete
	// and the scope is legal, so every outcome below comes from the
	// organization's published policies alone.
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

	// Step 1: nothing published yet. The envelope is valid, but with no
	// published version the default is denial; the record's version is 0.
	fmt.Println("step 1: no policy published yet; the read is denied and recorded")
	d := store.Decide(org, req)
	printDecision("  Decide  ->", d)
	denySeq, err := onlyDecisionSeq(store, org)
	if err != nil {
		return err
	}
	fmt.Printf("  [audit] the denial is decision record seq=%d (version 0: no published policy was used)\n", denySeq)

	// Step 2: publish the allow policy as version 1, then read again. The
	// publish and the decision each occupy one audit sequence.
	fmt.Println("step 2: publish the allow-read policy, then read again")
	v1, err := store.Publish(org, 0, []darksafe.Policy{{
		ID:      "p-ledger-read-2026",
		Subject: subjectID,
		Action:  "read",
		Scope:   ledgerScope,
		Effect:  darksafe.EffectAllow,
	}})
	if err != nil {
		return fmt.Errorf("publish allow policy: %w", err)
	}
	fmt.Printf("  Publish -> new version %d (policy change record, seq=%d)\n", v1, denySeq+1)
	d = store.Decide(org, req)
	printDecision("  Decide  ->", d)
	if !d.Allowed {
		return fmt.Errorf("expected the read to be allowed under version 1, got %+v", d)
	}
	allowSeq, err := lastSeq(store, org)
	if err != nil {
		return err
	}
	fmt.Printf("  [audit] the allow is decision record seq=%d (version %d)\n", allowSeq, v1)

	// Step 3: replace the current set with a deny policy (version 2). From
	// now on the same read is denied by the current policy.
	fmt.Println("step 3: replace the current set with a deny-read policy")
	v2, err := store.Publish(org, v1, []darksafe.Policy{{
		ID:      "p-ledger-read-deny",
		Subject: subjectID,
		Action:  "read",
		Scope:   ledgerScope,
		Effect:  darksafe.EffectDeny,
	}})
	if err != nil {
		return fmt.Errorf("publish deny policy: %w", err)
	}
	fmt.Printf("  Publish -> new version %d (policy change record, seq=%d)\n", v2, allowSeq+1)
	printDecision("  Review(org, 2, same request) ->", store.Review(org, v2, req))
	fmt.Println("  the current policy now denies this read")

	// Step 4: recheck the two earlier decision records by their audit
	// sequences. RecheckDecision re-judges the request saved in each record
	// against the version that record actually used; the caller does not
	// resubmit the access and the current deny policy is never consulted.
	fmt.Println("step 4: recheck the two earlier decision records by their audit sequences")
	originals, err := decisionRecords(store, org, denySeq, allowSeq)
	if err != nil {
		return err
	}
	for _, seq := range []int{denySeq, allowSeq} {
		printDecision(fmt.Sprintf("  seq=%d original :", seq), originals[seq])
		rechecked, err := store.RecheckDecision(org, seq)
		if err != nil {
			return fmt.Errorf("recheck seq %d: %w", seq, err)
		}
		printDecision(fmt.Sprintf("  seq=%d rechecked:", seq), rechecked)
		if !sameDecision(originals[seq], rechecked) {
			return fmt.Errorf("seq %d: recheck %v differs from the recorded %v", seq, rechecked, originals[seq])
		}
	}
	fmt.Println("  each recheck used the recorded request and the version that decision")
	fmt.Println("  actually used — the version-0 denial and the version-1 allow both")
	fmt.Println("  replay unchanged; the current deny policy was never consulted")

	// Step 5: RecheckDecision and Review are different entry points. Review
	// takes a caller-supplied request and an existing historical version;
	// version 0 is not a published version, so Review reports it missing,
	// while rechecking the version-0 record above replayed fine.
	fmt.Println("step 5: version 0 is a recorded state, not a reviewable version")
	printDecision("  Review(org, 0, same request) ->", store.Review(org, 0, req))
	fmt.Println("  Review needs a caller-supplied request and an existing version;")
	fmt.Printf("  rechecking seq=%d needs neither — its recorded version 0 denial replays normally\n", denySeq)

	// Step 6: sequence selection failures are not access denials. A policy
	// change record cannot be rechecked; a sequence beyond the chain does
	// not exist. Publish records occupy sequences too, so a policy version
	// number cannot locate an access record.
	fmt.Println("step 6: sequence selection failures are not access denials")
	changeSeq := denySeq + 1 // the version-1 publish record
	if _, err := store.RecheckDecision(org, changeSeq); !errors.Is(err, darksafe.ErrAuditNotADecision) {
		return fmt.Errorf("recheck seq %d err = %v, want ErrAuditNotADecision", changeSeq, err)
	} else {
		fmt.Printf("  RecheckDecision(org, %d) -> err=%v (ErrAuditNotADecision)\n", changeSeq, err)
	}
	missingSeq := allowSeq + 2 // one past the chain head
	if _, err := store.RecheckDecision(org, missingSeq); !errors.Is(err, darksafe.ErrAuditNotFound) {
		return fmt.Errorf("recheck seq %d err = %v, want ErrAuditNotFound", missingSeq, err)
	} else {
		fmt.Printf("  RecheckDecision(org, %d) -> err=%v (ErrAuditNotFound)\n", missingSeq, err)
	}
	fmt.Println("  both are record-selection failures, not ordinary access denials;")
	fmt.Printf("  publish records occupy sequences too (version %d at seq=%d, version %d at seq=%d),\n", v1, changeSeq, v2, allowSeq+1)
	fmt.Println("  so a policy version number cannot locate an access record")

	// Step 7: rechecking is read-only. The current version is still the
	// deny set and the chain still holds exactly the four records the
	// publishes and decisions created — no recheck appended anything.
	fmt.Println("step 7: rechecking is read-only")
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export audit chain: %w", err)
	}
	if got := store.CurrentVersion(org); got != v2 {
		return fmt.Errorf("current version = %d, want %d", got, v2)
	}
	if len(records) != allowSeq+1 {
		return fmt.Errorf("audit chain holds %d records, want %d", len(records), allowSeq+1)
	}
	fmt.Printf("  current version still %d, audit chain still %d records — no recheck changed policy or appended a record\n",
		store.CurrentVersion(org), len(records))
	return nil
}

// printDecision prints the four fields every decision reports: allowed or
// not, the reason, the matched policy list and the policy version actually
// used (0 when none was).
func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("%s allowed=%v reason=%q matched=%q version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}

// sameDecision compares the four reported fields of two decisions.
func sameDecision(a, b darksafe.Decision) bool {
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

// lastSeq returns the organization's current audit head sequence.
func lastSeq(store *darksafe.Store, org string) (int, error) {
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return 0, fmt.Errorf("export audit chain: %w", err)
	}
	return len(records), nil
}

// onlyDecisionSeq returns the sequence of the single decision record in a
// chain that holds nothing else (step 1, before any publish).
func onlyDecisionSeq(store *darksafe.Store, org string) (int, error) {
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return 0, fmt.Errorf("export audit chain: %w", err)
	}
	if len(records) != 1 || records[0].Kind != darksafe.AuditDecision {
		return 0, fmt.Errorf("expected exactly one decision record, got %+v", records)
	}
	return records[0].Seq, nil
}

// decisionRecords reads the original decisions saved in the given decision
// records, straight from the exported chain.
func decisionRecords(store *darksafe.Store, org string, seqs ...int) (map[int]darksafe.Decision, error) {
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return nil, fmt.Errorf("export audit chain: %w", err)
	}
	out := make(map[int]darksafe.Decision, len(seqs))
	for _, seq := range seqs {
		if seq < 1 || seq > len(records) {
			return nil, fmt.Errorf("no audit record at seq %d", seq)
		}
		rec := records[seq-1]
		if rec.Kind != darksafe.AuditDecision || rec.Decision == nil {
			return nil, fmt.Errorf("record %d is %s, not a decision", seq, rec.Kind)
		}
		out[seq] = rec.Decision.Decision
	}
	return out, nil
}
