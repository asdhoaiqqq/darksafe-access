// Command audit_resource_pagination_example shows how to page every
// decision recorded about ONE resource out of an organization's audit
// chain with AuditQuery and AuditPage's optional resource condition. The
// chain interleaves decisions about the target ledger with another
// ledger's decisions, envelope rejections that still mention the target
// (a disabled subject, an organization mismatch), policy publishes that
// carry the target resource ID in their policies, and the same resource
// identifier active in another organization — so the resource view
// genuinely has to skip non-matching records and turn pages.
//
// Run with:
//
//	go run ./examples/audit_resource_pagination
//
// The program takes no arguments, writes no files, and works fully offline.
// It demonstrates, in order:
//
//  1. Building a chain for org "acme factory" (plus a smaller "globex"
//     chain that reuses the same resource identifier).
//  2. Pinning a resource-filtered query: AuditQuery(org, 1, 2, "", "",
//     ledgerID). The trailing resourceID is optional; omitting it is the
//     historical unfiltered query. The page size bounds MATCHING records
//     per page; other resources and policy changes never fill a slot.
//  3. Appending one more decision about the same ledger after the pin,
//     then continuing the SAME query: every page reuses the first page's
//     checkpoint and cursor, the pinned end and fingerprint never change,
//     and the appended record stays out. A fresh query is the only way to
//     see it.
//  4. The conjunction rules: category policy_change plus the resource
//     condition is empty even though a published policy names the ledger;
//     adding a subject narrows further; an unknown resource gives an empty
//     page that ends the walk instead of the unfiltered history.
//  5. Isolation: the identically named resource in "globex" never enters
//     the acme view.
//  6. A checkpoint with an altered fingerprint fails with ErrInvalidRange
//     and delivers no partial records.
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
	otherOrg = "globex"
	ledgerID = "ledger-2026" // the resource whose history we page
	otherID  = "ledger-2025" // another resource sharing the chain
	scope    = "acme/factory/ledger"
	pageSize = 2 // matching records per page
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "audit resource pagination example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	store := darksafe.NewStore()

	decide := func(o, subject, resource string, disabled bool) darksafe.Decision {
		return store.Decide(o, darksafe.OrgRequest{
			SubjectOrg:  o,
			ResourceOrg: o,
			Subject:     darksafe.Subject{ID: subject, Kind: "service", Disabled: disabled},
			Resource:    darksafe.Resource{ID: resource, Scope: scope},
			Action:      "read",
		})
	}

	// Build the acme chain. The target ledger's decisions are seq 2, 4, 5,
	// 6, 8. Seq 3 and 9 concern the other ledger; seq 7 names the target
	// resource ID inside the published policy set.
	if _, err := store.Publish(org, 0, []darksafe.Policy{
		{ID: "p-ledger-read-2026", Subject: "svc-audit-reader", Action: "read", Scope: scope, Effect: darksafe.EffectAllow, ResourceID: ledgerID},
		{ID: "p-ledger-read-2025", Subject: "svc-billing", Action: "read", Scope: scope, Effect: darksafe.EffectAllow, ResourceID: otherID},
	}); err != nil {
		return fmt.Errorf("publish v1: %w", err)
	} // seq 1: policy_change
	if d := decide(org, "svc-audit-reader", ledgerID, false); !d.Allowed { // seq 2: target allow
		return fmt.Errorf("seq 2: expected allow, got %+v", d)
	}
	if d := decide(org, "svc-billing", otherID, false); !d.Allowed { // seq 3: other resource
		return fmt.Errorf("seq 3: expected allow, got %+v", d)
	}
	if d := decide(org, "svc-billing", ledgerID, false); d.Allowed { // seq 4: target, no matching allow
		return fmt.Errorf("seq 4: expected denial, got %+v", d)
	}
	if d := decide(org, "svc-disabled", ledgerID, true); d.Allowed { // seq 5: target, disabled rejection
		return fmt.Errorf("seq 5: expected denial, got %+v", d)
	}
	// Seq 6: target resource, but the resource organization disagrees with
	// the decision organization — an envelope rejection that still records
	// the requested resource ID.
	mismatch := store.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: otherOrg,
		Subject:  darksafe.Subject{ID: "svc-audit-reader", Kind: "service"},
		Resource: darksafe.Resource{ID: ledgerID, Scope: scope},
		Action:   "read",
	})
	if mismatch.Allowed || mismatch.Reason != "organization mismatch" {
		return fmt.Errorf("seq 6: expected organization-mismatch denial, got %+v", mismatch)
	}
	if _, err := store.Publish(org, 1, []darksafe.Policy{
		{ID: "p-ledger-read-2026-v2", Subject: "svc-audit-reader", Action: "read", Scope: scope, Effect: darksafe.EffectAllow, ResourceID: ledgerID},
	}); err != nil { // seq 7: policy_change naming the target resource
		return fmt.Errorf("publish v2: %w", err)
	}
	if d := decide(org, "svc-audit-reader", ledgerID, false); !d.Allowed { // seq 8: target allow (v2)
		return fmt.Errorf("seq 8: expected allow, got %+v", d)
	}
	if d := decide(org, "svc-billing", otherID, false); d.Allowed { // seq 9: other resource, v2 no match
		return fmt.Errorf("seq 9: expected denial, got %+v", d)
	}

	// A different organization that happens to use the same resource ID.
	if _, err := store.Publish(otherOrg, 0, []darksafe.Policy{
		{ID: "p-g", Subject: "svc-audit-reader", Action: "read", Scope: scope, Effect: darksafe.EffectAllow, ResourceID: ledgerID},
	}); err != nil {
		return fmt.Errorf("globex publish: %w", err)
	}
	decide(otherOrg, "svc-audit-reader", ledgerID, false)

	if err := printChain(store); err != nil {
		return err
	}

	// Pin the resource view: from sequence 1, at most 2 matching records.
	fmt.Printf("\npinned query: resource=%q start=1 pageSize=%d (page size counts matching records only)\n",
		ledgerID, pageSize)
	page, err := store.AuditQuery(org, 1, pageSize, "", "", ledgerID)
	if err != nil {
		return fmt.Errorf("audit query: %w", err)
	}
	pinned := page.Checkpoint
	pageNo := 1
	printPage(pageNo, page)
	walked := seqsOf(page)

	// The chain grows after the pin: one more decision about the SAME
	// ledger becomes seq 10.
	if d := decide(org, "svc-audit-reader", ledgerID, false); !d.Allowed {
		return fmt.Errorf("appended decision: expected allow, got %+v", d)
	}
	fmt.Println("appended after page 1: svc-audit-reader read ledger-2026 -> allow (new seq 10)")

	// Continue the SAME pinned query: the appended seq 10 cannot appear.
	for page.Next != 0 {
		page, err = store.AuditPage(pinned, page.Next, pageSize, "", "", ledgerID)
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
	if !equalInts(walked, []int{2, 4, 5, 6, 8}) {
		return fmt.Errorf("pinned walk seqs = %v, want [2 4 5 6 8]", walked)
	}
	fmt.Println("next=0 ends the walk; seq 10 stayed out and only a fresh query can see it")

	fresh, err := store.AuditQuery(org, 1, pageSize, "", "", ledgerID)
	if err != nil {
		return fmt.Errorf("fresh audit query: %w", err)
	}
	freshWalk := seqsOf(fresh)
	for fresh.Next != 0 {
		fresh, err = store.AuditPage(fresh.Checkpoint, fresh.Next, pageSize, "", "", ledgerID)
		if err != nil {
			return fmt.Errorf("fresh audit page: %w", err)
		}
		freshWalk = append(freshWalk, seqsOf(fresh)...)
	}
	if !equalInts(freshWalk, []int{2, 4, 5, 6, 8, 10}) {
		return fmt.Errorf("fresh walk seqs = %v, want [2 4 5 6 8 10]", freshWalk)
	}
	fmt.Printf("fresh query seqs: %v (the appended decision is now visible)\n", freshWalk)

	// Conjunction 1: policy-change category plus resource is empty, even
	// though seq 1 and seq 7 publish policies naming ledger-2026.
	fmt.Println("\nconjunction: category policy_change + resource condition")
	changes, err := store.AuditQuery(org, 1, pageSize, darksafe.AuditPolicyChange, "", ledgerID)
	if err != nil {
		return fmt.Errorf("policy_change+resource query: %w", err)
	}
	if len(changes.Records) != 0 || changes.Next != 0 {
		return fmt.Errorf("policy_change+resource = %+v, want empty page ending the walk", changes)
	}
	fmt.Println("  records=0 next=0 (the condition matches requested resources, never policy content)")

	// Conjunction 2: subject plus resource narrows to one subject's
	// decisions about the ledger.
	fmt.Println("conjunction: subject svc-audit-reader + resource condition")
	page, err = store.AuditQuery(org, 1, pageSize, darksafe.AuditDecision, "svc-audit-reader", ledgerID)
	if err != nil {
		return fmt.Errorf("subject+resource query: %w", err)
	}
	both := seqsOf(page)
	for page.Next != 0 {
		page, err = store.AuditPage(page.Checkpoint, page.Next, pageSize, darksafe.AuditDecision, "svc-audit-reader", ledgerID)
		if err != nil {
			return fmt.Errorf("subject+resource page: %w", err)
		}
		both = append(both, seqsOf(page)...)
	}
	if !equalInts(both, []int{2, 6, 8, 10}) {
		return fmt.Errorf("subject+resource seqs = %v, want [2 6 8 10]", both)
	}
	fmt.Printf("  seqs=%v (seq 4 is svc-billing, seq 5 the disabled subject; every condition must hold)\n", both)

	// Boundary 1: unknown resource ends on an empty page, never the chain.
	fmt.Println("boundary 1: unknown resource")
	ghost, err := store.AuditQuery(org, 1, pageSize, "", "", "ledger-does-not-exist")
	if err != nil {
		return fmt.Errorf("no-match query: %w", err)
	}
	if len(ghost.Records) != 0 || ghost.Next != 0 {
		return fmt.Errorf("no-match page = %+v, want empty with next 0", ghost)
	}
	fmt.Println("  records=0 next=0 (empty page ends the walk; the unfiltered history is NOT returned)")

	// Boundary 2: the same identifier in another organization is isolated.
	fmt.Println("boundary 2: cross-organization isolation")
	gPage, err := store.AuditQuery(otherOrg, 1, pageSize, "", "", ledgerID)
	if err != nil {
		return fmt.Errorf("globex query: %w", err)
	}
	gWalk := seqsOf(gPage)
	for gPage.Next != 0 {
		gPage, err = store.AuditPage(gPage.Checkpoint, gPage.Next, pageSize, "", "", ledgerID)
		if err != nil {
			return fmt.Errorf("globex page: %w", err)
		}
		gWalk = append(gWalk, seqsOf(gPage)...)
	}
	if !equalInts(gWalk, []int{2}) {
		return fmt.Errorf("globex walk seqs = %v, want [2]", gWalk)
	}
	fmt.Printf("  globex's view of %q = %v; acme's records never mix in (and vice versa)\n", ledgerID, gWalk)

	// Boundary 3: a tampered fingerprint fails with ErrInvalidRange.
	fmt.Println("boundary 3: checkpoint with an altered fingerprint")
	forged := pinned
	forged.Fingerprint = "00" + forged.Fingerprint[2:]
	page, err = store.AuditPage(forged, 2, pageSize, "", "", ledgerID)
	if !errors.Is(err, darksafe.ErrInvalidRange) {
		return fmt.Errorf("tampered checkpoint: err = %v, want ErrInvalidRange", err)
	}
	if page != nil {
		return fmt.Errorf("tampered checkpoint delivered partial page %+v", page)
	}
	fmt.Printf("  AuditPage -> %v (ErrInvalidRange; no partial records delivered)\n", err)

	// Everything above was read-only.
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("final export: %w", err)
	}
	if len(records) != 10 || store.CurrentVersion(org) != 2 {
		return fmt.Errorf("querying mutated state: %d records, version %d", len(records), store.CurrentVersion(org))
	}
	fmt.Printf("\nall querying was read-only: chain still %d records, current version still %d\n",
		len(records), store.CurrentVersion(org))
	return nil
}

// printChain lists both organizations' chains so the interleaving is
// visible before the filtered walk.
func printChain(store *darksafe.Store) error {
	for _, o := range []string{org, otherOrg} {
		records, _, err := store.AuditExport(o, 0)
		if err != nil {
			return fmt.Errorf("export chain: %w", err)
		}
		fmt.Printf("audit chain of %q (%d records):\n", o, len(records))
		for _, r := range records {
			switch r.Kind {
			case darksafe.AuditPolicyChange:
				fmt.Printf("  seq=%-2d policy_change version=%d policies=%d\n",
					r.Seq, r.Change.Version, len(r.Change.Policies))
			case darksafe.AuditDecision:
				d := r.Decision.Decision
				fmt.Printf("  seq=%-2d decision     resource=%s subject=%s allowed=%v reason=%q\n",
					r.Seq, r.Decision.Request.Resource.ID, r.Decision.Request.Subject.ID, d.Allowed, d.Reason)
			}
		}
	}
	return nil
}

// printPage prints one page: the pinned range, each record's original
// sequence and the cursor for the next page.
func printPage(no int, page *darksafe.AuditPage) {
	fmt.Printf("page %d (pinned range %d-%d, checkpoint end=%d fingerprint=%s):\n",
		no, page.BeginSeq, page.EndSeq, page.Checkpoint.EndSeq, page.Checkpoint.Fingerprint)
	if len(page.Records) == 0 {
		fmt.Println("  (no matching records)")
	}
	for _, r := range page.Records {
		d := r.Decision.Decision
		fmt.Printf("  seq=%d resource=%s subject=%s allowed=%v reason=%q matched=%q version=%d\n",
			r.Seq, r.Decision.Request.Resource.ID, r.Decision.Request.Subject.ID,
			d.Allowed, d.Reason, d.Matched, d.Version)
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
