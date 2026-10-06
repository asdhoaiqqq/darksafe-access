// Command offline_review_example is the end-to-end worked example for
// `darksafe review`. It makes one real access decision, saves the complete
// audit archive and a separately retained checkpoint, and prints the exact
// review command to run next. Everything runs offline in one short program;
// after it exits neither the in-memory Store nor the original access request
// is needed for the review.
//
// Run with:
//
//	go run ./examples/offline_review
//
// The program takes no arguments. It writes two files into the working
// directory:
//
//   - acme-factory.audit: the complete audit archive, sequence 1 (the
//     resource-scoped policy publish) through sequence 2 (the read
//     decision), pinned by the independently saved checkpoint.
//   - acme-factory.checkpoint: the checkpoint kept SEPARATELY from the
//     archive; its end sequence and fingerprint are what a later review
//     must pass on the command line.
//
// Both files are deterministic: the same inputs always produce the same
// bytes and fingerprint.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// Archive and checkpoint filenames for the worked example.
const (
	archivePath    = "acme-factory.audit"
	checkpointPath = "acme-factory.checkpoint"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "offline review example: failed:", err)
		os.Exit(1)
	}
}

// run contains the whole worked flow. Every error is fatal because the
// example exists to show the exact sequence of calls: a skipped or tolerated
// failure would leave the user without the materials the printed command
// needs.
func run() error {
	// One organization. The name intentionally contains a space: it is
	// carried verbatim through publish, decision, archive and review, and
	// must be quoted on the review command line.
	const org = "acme factory"

	// One ledger, addressed by both an exact resource id and a scope path.
	// The policy binds subject + read + scope and then narrows to exactly
	// this resource id, so the same subject reading another ledger in the
	// same scope is not authorized by it.
	const (
		ledgerID    = "ledger-2026"
		ledgerScope = "acme/factory/ledger"
		subjectID   = "svc-audit-reader"
	)

	store := darksafe.NewStore()

	// Publish exactly one allow policy: only svc-audit-reader may read,
	// within the ledger scope, ledger-2026 and no other resource. Expected
	// version 0 because nothing has been published for this organization.
	published, err := store.Publish(org, 0, []darksafe.Policy{
		{
			ID:         "p-ledger-read-2026",
			Subject:    subjectID,
			Action:     "read",
			Scope:      ledgerScope,
			Effect:     darksafe.EffectAllow,
			ResourceID: ledgerID,
		},
	})
	if err != nil {
		return fmt.Errorf("publish ledger-read policy as version 1: %w", err)
	}

	// Make one real read decision through the organization-level API. Both
	// organization fields equal the decision organization, the action is
	// read, and the resource id is the one the policy narrows to.
	req := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject: darksafe.Subject{
			ID:   subjectID,
			Kind: "service",
		},
		Resource: darksafe.Resource{
			ID:    ledgerID,
			Scope: ledgerScope,
		},
		Action: "read",
	}
	decision := store.Decide(org, req)
	if !decision.Allowed {
		// The published allow policy matches this exact subject/scope/
		// resource, so a denial here means the example's own inputs drifted
		// and the archive must not be saved as "the allowed example".
		return fmt.Errorf("expected the read to be allowed, got denial: allowed=%v reason=%q matched=%v version=%d",
			decision.Allowed, decision.Reason, decision.Matched, decision.Version)
	}
	fmt.Printf("online decision: allowed=%v reason=%q matched=%v policy version=%d\n",
		decision.Allowed, decision.Reason, decision.Matched, decision.Version)
	fmt.Printf("published policy became version %d\n", published)

	// Export the complete chain (publish record + decision record) and its
	// pinning checkpoint. AuditExport returns detached copies; when the
	// Store disappears at the end of run(), these are all that remains.
	records, cp, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export complete audit chain: %w", err)
	}
	if len(records) != 2 {
		// Guard the narrative: seq 1 must be the publish and seq 2 the
		// decision; otherwise the printed --seq would point at the wrong
		// record.
		return fmt.Errorf("expected 2 audit records (publish + decision), got %d", len(records))
	}
	if records[0].Kind != darksafe.AuditPolicyChange || records[1].Kind != darksafe.AuditDecision {
		return fmt.Errorf("unexpected record kinds: seq1=%q seq2=%q", records[0].Kind, records[1].Kind)
	}
	targetSeq := records[1].Seq
	if targetSeq != 2 {
		return fmt.Errorf("decision record should be seq 2 (publish occupies seq 1), got %d", targetSeq)
	}

	// Encode and save the archive.
	archive, err := darksafe.EncodeAuditArchive(org, records, cp)
	if err != nil {
		return fmt.Errorf("encode audit archive: %w", err)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		return fmt.Errorf("write archive %s: %w", archivePath, err)
	}

	// Save the checkpoint SEPARATELY from the archive. A real retention
	// scheme keeps it elsewhere (another host, a seal, a paper record); here
	// it is simply a second file the archive cannot rewrite. The archive
	// also carries a checkpoint, but it is informational only and can never
	// stand in for this one.
	if err := writeCheckpoint(checkpointPath, cp); err != nil {
		return err
	}

	// Report the saved materials and the exact review commands to run next.
	fmt.Println()
	fmt.Println("materials saved:")
	fmt.Printf("  archive:    %s (%d bytes, %d records)\n", archivePath, len(archive), len(records))
	fmt.Printf("  checkpoint: %s  [kept separately]\n", checkpointPath)
	fmt.Println()
	fmt.Println("separately retained checkpoint:")
	fmt.Printf("  org:         %q\n", cp.Org)
	fmt.Printf("  end-seq:     %d\n", cp.EndSeq)
	fmt.Printf("  fingerprint: %s\n", cp.Fingerprint)
	fmt.Println()
	fmt.Println("Now run (the Store and the original request are gone; the review reads")
	fmt.Println("only the saved materials and never re-submits the request). Either pass")
	fmt.Println("the checkpoint fields on the command line:")
	fmt.Println()
	fmt.Println("  go run ./cmd/darksafe review \\")
	fmt.Printf("    --archive %s \\\n", archivePath)
	fmt.Printf("    --org '%s' \\\n", cp.Org)
	fmt.Printf("    --seq %d \\\n", targetSeq)
	fmt.Printf("    --end-seq %d \\\n", cp.EndSeq)
	fmt.Printf("    --fingerprint %s\n", cp.Fingerprint)
	fmt.Println()
	fmt.Println("...or hand the separately saved checkpoint straight to --checkpoint,")
	fmt.Println("which reads org/end-seq/fingerprint from that file instead. The two")
	fmt.Println("forms are mutually exclusive: --checkpoint cannot be combined with")
	fmt.Println("--org, --end-seq or --fingerprint, even when the values agree:")
	fmt.Println()
	fmt.Println("  go run ./cmd/darksafe review \\")
	fmt.Printf("    --archive %s \\\n", archivePath)
	fmt.Printf("    --seq %d \\\n", targetSeq)
	fmt.Printf("    --checkpoint %s\n", checkpointPath)
	fmt.Println()
	fmt.Println("Two ways to pick the wrong material:")
	fmt.Println()
	fmt.Printf("  (a) --seq %d instead of %d: it points at the policy publish record,\n", records[0].Seq, targetSeq)
	fmt.Println("      which is not a decision; review exits 1 with a distinguishable")
	fmt.Println("      error on stderr and prints nothing on stdout.")
	fmt.Println()
	fmt.Println("  (b) a fingerprint that does not match the archive: same exit code 1,")
	fmt.Println("      same stderr-only, no partial review.")
	return nil
}

// writeCheckpoint writes the independently retained checkpoint as small
// plain text, so the user can read what they are trusting out-of-band:
// one field per line, org quoted byte-for-byte with %q, fingerprint raw.
func writeCheckpoint(path string, cp darksafe.Checkpoint) error {
	content := fmt.Sprintf("org=%q\nend_seq=%d\nfingerprint=%s\n", cp.Org, cp.EndSeq, cp.Fingerprint)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write checkpoint %s: %w", path, err)
	}
	return nil
}
