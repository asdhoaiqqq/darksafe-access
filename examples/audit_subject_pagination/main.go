// Command audit_subject_pagination_example shows how to page one subject's
// decisions out of an organization's audit chain with AuditQuery and
// AuditPage. The chain it builds interleaves the target subject's allow and
// denial decisions with policy publishes, a rollback and another subject's
// decisions, so the target records sit several chain positions apart and
// the query genuinely has to turn pages.
//
// Run with:
//
//	go run ./examples/audit_subject_pagination
//
// The program takes no arguments, writes no files, and works fully offline.
// It demonstrates, in order:
//
//  1. Building a 10-record audit chain for org "acme factory".
//  2. Pinning a subject-filtered query (start sequence 1, page size 2) and
//     taking the first page. The page size bounds MATCHING records per
//     page; interleaved non-matching records never fill a page early.
//  3. Appending one more decision for the same subject, then continuing
//     the SAME query: every later page reuses the first page's checkpoint
//     and the returned cursor, the pinned end sequence and fingerprint
//     never change, and the new record stays out of the old query. The
//     walk ends with an empty page because non-matching records remain
//     after the last match — a normal end, not lost material.
//  4. Starting a FRESH query, which is the only way the appended record
//     appears.
//  5. Two boundaries: a subject with no decisions yields an empty page
//     that ends the walk (never the unfiltered history), and a checkpoint
//     with an altered fingerprint fails with ErrInvalidRange and delivers
//     no partial records.
//
// All querying and paging is read-only: it never changes the current
// policy version and never appends audit records.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	org      = "acme factory"
	ledgerID = "ledger-2026"
	scope    = "acme/factory/ledger"
	target   = "svc-audit-reader" // the subject whose decisions we page
	other    = "svc-billing"      // another subject sharing the chain
	pageSize = 2                  // matching records per page
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "audit subject pagination example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	store := darksafe.NewStore()

	decide := func(subject, action string) darksafe.Decision {
		return store.Decide(org, darksafe.OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject:     darksafe.Subject{ID: subject, Kind: "service"},
			Resource:    darksafe.Resource{ID: ledgerID, Scope: scope},
			Action:      action,
		})
	}

	// Build the chain. The target subject's decisions (seq 2, 4, 7, 9) sit
	// between policy changes and the other subject's decisions.
	if _, err := store.Publish(org, 0, []darksafe.Policy{
		{ID: "p-ledger-read-2026", Subject: target, Action: "read", Scope: scope, Effect: darksafe.EffectAllow},
		{ID: "p-ledger-write-deny", Subject: target, Action: "write", Scope: scope, Effect: darksafe.EffectDeny},
	}); err != nil {
		return fmt.Errorf("publish v1: %w", err)
	} // seq 1: policy_change -> version 1
	if d := decide(target, "read"); !d.Allowed { // seq 2: target allow (v1)
		return fmt.Errorf("seq 2: expected allow, got %+v", d)
	}
	if d := decide(other, "read"); d.Allowed { // seq 3: other subject, no-match denial
		return fmt.Errorf("seq 3: expected denial, got %+v", d)
	}
	if d := decide(target, "write"); d.Allowed { // seq 4: target matched-deny (v1)
		return fmt.Errorf("seq 4: expected denial, got %+v", d)
	}
	if _, err := store.Publish(org, 1, nil); err != nil { // seq 5: policy_change -> version 2 (empty set)
		return fmt.Errorf("publish v2: %w", err)
	}
	decide(other, "read")                                // seq 6: other subject denial (v2)
	decide(target, "read")                               // seq 7: target denial, empty v2 matches nothing
	if _, err := store.Rollback(org, 2, 1); err != nil { // seq 8: policy_change -> version 3 (copy of v1)
		return fmt.Errorf("rollback to v1: %w", err)
	}
	if d := decide(target, "read"); !d.Allowed { // seq 9: target allow (v3)
		return fmt.Errorf("seq 9: expected allow, got %+v", d)
	}
	decide(other, "read") // seq 10: other subject denial (v3)

	if err := printChain(store); err != nil {
		return err
	}

	// Pin the query: from sequence 1, at most 2 matching records per page.
	fmt.Printf("\npinned query: subject=%q start=1 pageSize=%d (page size counts matching records only)\n",
		target, pageSize)
	page, err := store.AuditQuery(org, 1, pageSize, "", target)
	if err != nil {
		return fmt.Errorf("audit query: %w", err)
	}
	pinned := page.Checkpoint
	pageNo := 1
	printPage(pageNo, page)
	walked := seqsOf(page)

	// The chain grows AFTER the first page pinned its checkpoint: one more
	// decision for the same target subject becomes seq 11.
	if d := decide(target, "read"); !d.Allowed {
		return fmt.Errorf("appended decision: expected allow, got %+v", d)
	}
	fmt.Println("appended after page 1: svc-audit-reader read -> allow (new seq 11)")

	// Continue the SAME query: reuse the first page's checkpoint and each
	// returned cursor. The pinned end and fingerprint never change, and the
	// appended seq 11 stays out of this walk.
	for page.Next != 0 {
		page, err = store.AuditPage(pinned, page.Next, pageSize, "", target)
		if err != nil {
			return fmt.Errorf("audit page: %w", err)
		}
		if page.Checkpoint != pinned {
			return fmt.Errorf("checkpoint drifted: %+v, want %+v", page.Checkpoint, pinned)
		}
		pageNo++
		printPage(pageNo, page)
		walked = append(walked, seqsOf(page)...)
	}
	if !equalInts(walked, []int{2, 4, 7, 9}) {
		return fmt.Errorf("pinned walk seqs = %v, want [2 4 7 9]", walked)
	}
	fmt.Println("next=0 ends the walk; the empty last page is normal: non-matching records remained after the last match")

	// Only a FRESH query pins the new head and sees the appended record.
	fmt.Println("\nfresh query after the append (only a new query pins the new head):")
	fresh, err := store.AuditQuery(org, 1, pageSize, "", target)
	if err != nil {
		return fmt.Errorf("fresh audit query: %w", err)
	}
	pageNo = 1
	printPage(pageNo, fresh)
	walked = seqsOf(fresh)
	for fresh.Next != 0 {
		fresh, err = store.AuditPage(fresh.Checkpoint, fresh.Next, pageSize, "", target)
		if err != nil {
			return fmt.Errorf("fresh audit page: %w", err)
		}
		pageNo++
		printPage(pageNo, fresh)
		walked = append(walked, seqsOf(fresh)...)
	}
	if !equalInts(walked, []int{2, 4, 7, 9, 11}) {
		return fmt.Errorf("fresh walk seqs = %v, want [2 4 7 9 11]", walked)
	}

	// Boundary 1: a subject with no decisions gets an empty page that ends
	// the walk — never an error and never the unfiltered history.
	fmt.Println("\nboundary 1: subject with no decisions")
	ghost, err := store.AuditQuery(org, 1, pageSize, "", "svc-nobody")
	if err != nil {
		return fmt.Errorf("no-match query: %w", err)
	}
	if len(ghost.Records) != 0 || ghost.Next != 0 {
		return fmt.Errorf("no-match page = %+v, want empty with next 0", ghost)
	}
	fmt.Printf("  subject=%q: records=%d next=%d (empty page ends the walk; the unfiltered history is NOT returned)\n",
		"svc-nobody", len(ghost.Records), ghost.Next)

	// Boundary 2: a checkpoint whose fingerprint was altered fails with
	// ErrInvalidRange and delivers no partial records.
	fmt.Println("boundary 2: checkpoint with an altered fingerprint")
	forged := pinned
	forged.Fingerprint = "00" + forged.Fingerprint[2:]
	page, err = store.AuditPage(forged, 2, pageSize, "", target)
	if !errors.Is(err, darksafe.ErrInvalidRange) {
		return fmt.Errorf("tampered checkpoint: err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		return fmt.Errorf("tampered checkpoint delivered partial page %+v", page)
	}
	fmt.Printf("  AuditPage -> %v (ErrInvalidRange; no partial records delivered)\n", err)

	// Everything above was read-only: no new records, no version change.
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("final export: %w", err)
	}
	if len(records) != 11 || store.CurrentVersion(org) != 3 {
		return fmt.Errorf("querying mutated state: %d records, version %d", len(records), store.CurrentVersion(org))
	}
	fmt.Printf("\nall querying was read-only: chain still %d records, current version still %d\n",
		len(records), store.CurrentVersion(org))
	return nil
}

// printChain lists the whole chain so the interleaving of matching and
// non-matching records is visible before the filtered walk.
func printChain(store *darksafe.Store) error {
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export chain: %w", err)
	}
	fmt.Printf("audit chain of %q (%d records):\n", org, len(records))
	for _, r := range records {
		switch r.Kind {
		case darksafe.AuditPolicyChange:
			fmt.Printf("  seq=%-2d policy_change version=%d policies=%d rolled_back=%v\n",
				r.Seq, r.Change.Version, len(r.Change.Policies), r.Change.RolledBack)
		case darksafe.AuditDecision:
			d := r.Decision.Decision
			fmt.Printf("  seq=%-2d decision     subject=%s action=%s allowed=%v\n",
				r.Seq, r.Decision.Request.Subject.ID, r.Decision.Request.Action, d.Allowed)
		}
	}
	return nil
}

// printPage prints one page: the pinned range and checkpoint it belongs to,
// each record with its ORIGINAL sequence, and the cursor for the next page.
func printPage(no int, page *darksafe.AuditPage) {
	fmt.Printf("page %d (pinned range %d-%d, checkpoint end=%d fingerprint=%s):\n",
		no, page.BeginSeq, page.EndSeq, page.Checkpoint.EndSeq, page.Checkpoint.Fingerprint)
	if len(page.Records) == 0 {
		fmt.Println("  (no matching records)")
	}
	for _, r := range page.Records {
		d := r.Decision.Decision
		fmt.Printf("  seq=%d subject=%s allowed=%v reason=%q matched=%q version=%d\n",
			r.Seq, r.Decision.Request.Subject.ID, d.Allowed, d.Reason, d.Matched, d.Version)
	}
	fmt.Printf("  next=%d\n", page.Next)
}

// seqsOf returns the original chain sequences of the records on a page.
func seqsOf(page *darksafe.AuditPage) []int {
	seqs := make([]int, len(page.Records))
	for i, r := range page.Records {
		seqs[i] = r.Seq
	}
	return seqs
}

func equalInts(a, b []int) bool {
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
