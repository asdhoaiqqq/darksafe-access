// Command save-decision is a complete walkthrough of the offline review
// material flow. Around one organization, one ledger and one read request,
// it publishes a policy that allows only one subject to read that ledger,
// takes the allowed decision through the organization-level Decide entry
// point, then saves the full audit archive and its checkpoint as two
// separate files. Once the program exits, nothing needs to stay in memory:
// "darksafe review" recomputes the saved decision from those two files
// alone, without a Store and without re-submitting the request.
//
// Run it from the repository root:
//
//	go run ./examples/save-decision
//
// It writes acme-payments.audit and acme-payments.checkpoint into the
// current directory and prints the exact review command to run next.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	// The organization name contains a space on purpose: every later call
	// must repeat it byte for byte, including the space.
	org = "acme payments"
	// The policy pins the ledger by its exact resource identifier; the
	// request below must use the same identifier and scope.
	ledgerID    = "ledger-2026-q3"
	ledgerScope = "org/payments/ledger"
	subjectID   = "u-1001"

	archivePath    = "acme-payments.audit"
	checkpointPath = "acme-payments.checkpoint"
)

func main() {
	log.SetFlags(0)
	store := darksafe.NewStore()

	// Step 1: publish version 1 — a single policy allowing only subjectID
	// to read this exact ledger. ResourceID narrows the allow to the one
	// ledger; it never replaces the subject, action and scope conditions.
	// A successful publish appends audit record 1 (a policy change record).
	version, err := store.Publish(org, 0, []darksafe.Policy{{
		ID:         "allow-u1001-read-ledger",
		Subject:    subjectID,
		Action:     "read",
		Scope:      ledgerScope,
		Effect:     darksafe.EffectAllow,
		ResourceID: ledgerID,
	}})
	if err != nil {
		log.Fatalf("publish policy set: %v", err)
	}

	// Step 2: take one real access decision at the organization level. Both
	// organizations must equal the decision organization. The allowed
	// decision appends audit record 2 (a decision record).
	decision := store.Decide(org, darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "user"},
		Resource:    darksafe.Resource{ID: ledgerID, Scope: ledgerScope},
		Action:      "read",
	})
	if !decision.Allowed {
		log.Fatalf("expected an allowed decision, got: %+v", decision)
	}

	// Step 3: export the complete audit chain together with its checkpoint.
	// The checkpoint (end sequence + fingerprint) is the independently
	// retained evidence the offline review validates against.
	records, cp, err := store.AuditExport(org, 0)
	if err != nil {
		log.Fatalf("export audit chain: %v", err)
	}

	// The review target is the decision record's sequence in the audit
	// chain — not the policy version, not the number of access requests.
	// Here record 1 is the publish and record 2 is the decision; locate it
	// from the export instead of assuming a position.
	targetSeq := 0
	for _, r := range records {
		if r.Kind == darksafe.AuditDecision {
			targetSeq = r.Seq
		}
	}
	if targetSeq == 0 {
		log.Fatal("export contains no decision record")
	}

	// Step 4: serialize the export and save the archive and the checkpoint
	// as two separate files. The archive carries a copy of the checkpoint
	// for reference only; the review always validates against the
	// separately retained one, so the two files must both be kept.
	archive, err := darksafe.EncodeAuditArchive(org, records, cp)
	if err != nil {
		log.Fatalf("encode audit archive: %v", err)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		log.Fatalf("write %s: %v", archivePath, err)
	}
	checkpointLine := fmt.Sprintf("%d %s\n", cp.EndSeq, cp.Fingerprint)
	if err := os.WriteFile(checkpointPath, []byte(checkpointLine), 0o600); err != nil {
		log.Fatalf("write %s: %v", checkpointPath, err)
	}

	fmt.Printf("published policy version %d in organization %q\n", version, org)
	fmt.Printf("decision: allowed=%v reason=%q matched=%q version=%d\n",
		decision.Allowed, decision.Reason, decision.Matched, decision.Version)
	fmt.Printf("saved %d audit records to %s\n", len(records), archivePath)
	fmt.Printf("saved checkpoint (end seq %d) separately to %s\n", cp.EndSeq, checkpointPath)
	fmt.Println()
	fmt.Println("the store can now be discarded; review the saved decision offline with:")
	fmt.Printf("  darksafe review --archive %s --org '%s' --seq %d --end-seq %d --fingerprint %s\n",
		archivePath, org, targetSeq, cp.EndSeq, cp.Fingerprint)
}
