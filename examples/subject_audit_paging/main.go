// Command subject_audit_paging_example is the worked example for paging one
// subject's own decisions out of an organization's audit chain with
// AuditQuery / AuditPage.
//
// Run with:
//
//	go run ./examples/subject_audit_paging
//
// The program takes no arguments, writes no files, and works fully offline.
// In one organization it builds a chain that interleaves the target
// subject's allows and denials with policy publishes and another subject's
// decisions, then:
//
//  1. Queries the target subject from sequence 1, two MATCHING records per
//     page; four target records sit among eleven chain records, so the walk
//     really pages.
//  2. After the first page, appends one more decision of the SAME subject
//     and then continues the pinned walk: the appended record never enters
//     the old query; only a freshly started query shows it.
//  3. Walks into the empty terminal page that follows the last match.
//  4. Demonstrates the two boundaries: a subject with no decisions gets an
//     empty ending page (never the unfiltered chain), and continuing on a
//     checkpoint whose fingerprint was altered fails with ErrInvalidRange
//     and delivers no partial records.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	org = "acme factory"

	ledgerID    = "ledger-2026"
	ledgerScope = "acme/factory/ledger"

	target  = "svc-ledger-job"   // the subject whose decisions are paged
	other   = "svc-batch-report" // another subject whose decisions interleave
	unknown = "svc-nobody"       // a subject that never decides here

	pageSize = 2
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "subject audit paging example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	store := darksafe.NewStore()

	// seq 1, publish version 1: the target may read the ledger scope but is
	// explicitly denied writes; the other subject has no policy yet.
	if _, err := store.Publish(org, 0, []darksafe.Policy{
		{ID: "p-ledger-read", Subject: target, Action: "read", Scope: ledgerScope, Effect: darksafe.EffectAllow},
		{ID: "p-ledger-write-deny", Subject: target, Action: "write", Scope: ledgerScope, Effect: darksafe.EffectDeny},
	}); err != nil {
		return fmt.Errorf("publish version 1: %w", err)
	}

	decide := func(subject, action string, disabled bool) darksafe.Decision {
		return store.Decide(org, darksafe.OrgRequest{
			SubjectOrg:  org,
			ResourceOrg: org,
			Subject:     darksafe.Subject{ID: subject, Kind: "service", Disabled: disabled},
			Resource:    darksafe.Resource{ID: ledgerID, Scope: ledgerScope},
			Action:      action,
		})
	}

	// seq 2, target read: matched allow under version 1.
	if d := decide(target, "read", false); !d.Allowed || d.Reason != "matched allow policy" ||
		len(d.Matched) != 1 || d.Matched[0] != "p-ledger-read" || d.Version != 1 {
		return fmt.Errorf("seq 2 decision broken: %+v", d)
	}
	// seq 3, the other subject reads: a real decision record that the target
	// filter must skip.
	decide(other, "read", false)
	// seq 4, target write: the explicit deny wins over nothing else.
	if d := decide(target, "write", false); d.Allowed || d.Reason != "matched deny policy" ||
		len(d.Matched) != 1 || d.Matched[0] != "p-ledger-write-deny" || d.Version != 1 {
		return fmt.Errorf("seq 4 decision broken: %+v", d)
	}

	// seq 5, publish version 2: only the other subject keeps a policy. The
	// target now matches nothing.
	if _, err := store.Publish(org, 1, []darksafe.Policy{
		{ID: "p-batch-read", Subject: other, Action: "read", Scope: ledgerScope, Effect: darksafe.EffectAllow},
	}); err != nil {
		return fmt.Errorf("publish version 2: %w", err)
	}

	// seq 6, other read: allowed under version 2.
	decide(other, "read", false)
	// seq 7, target read under version 2: default deny, empty matched list.
	if d := decide(target, "read", false); d.Allowed || d.Reason != "no matching allow policy" ||
		len(d.Matched) != 0 || d.Version != 2 {
		return fmt.Errorf("seq 7 decision broken: %+v", d)
	}
	// seq 8, other read again.
	decide(other, "read", false)
	// seq 9, target read with the subject disabled: rejected by the envelope
	// before any policy is evaluated, so the version is 0.
	if d := decide(target, "read", true); d.Allowed || d.Reason != "subject is disabled" ||
		len(d.Matched) != 0 || d.Version != 0 {
		return fmt.Errorf("seq 9 decision broken: %+v", d)
	}
	// seq 10-11, two more records of the other subject. They are the
	// non-matching tail after the target's last match.
	decide(other, "read", false)
	decide(other, "write", false)

	fmt.Println("== 1. audit material generated in organization \"acme factory\" ==")
	chain, _, err := store.AuditExport(org, 0)
	if err != nil {
		return err
	}
	fmt.Printf("chain length before the query: %d records\n", len(chain))
	for _, r := range chain {
		printChainRecord(r)
	}

	// First page: start at sequence 1, at most two MATCHING records, filtered
	// to the target subject. The returned checkpoint pins the whole walk.
	fmt.Println("== 2. first page: AuditQuery(org, startSeq=1, pageSize=2, subject=\"svc-ledger-job\") ==")
	first, err := store.AuditQuery(org, 1, pageSize, "", target)
	if err != nil {
		return err
	}
	printPage(first, 1)
	pinned := first.Checkpoint

	// While the walk is paused, the SAME subject produces another decision.
	fmt.Println("== 3. one more decision of the SAME subject is appended while paging ==")
	appended := decide(target, "read", false) // seq 12, under version 2
	tail, _, err := store.AuditExport(org, 0)
	if err != nil {
		return err
	}
	last := tail[len(tail)-1]
	printChainRecord(last)
	fmt.Printf("(appended decision: allowed=%v reason=%q matched=%q version=%d)\n",
		appended.Allowed, appended.Reason, appended.Matched, appended.Version)

	// Continue the ORIGINAL query with the first page's checkpoint and
	// cursor. The checkpoint still pins end_seq=11, so seq 12 cannot appear.
	fmt.Println("== 4. continue the SAME pinned query with the returned checkpoint and cursor ==")
	page := first
	pageNo := 1
	for page.Next != 0 {
		pageNo++
		page, err = store.AuditPage(pinned, page.Next, pageSize, "", target)
		if err != nil {
			return err
		}
		printPage(page, pageNo)
	}
	fmt.Println("next=0: the pinned walk is finished; the cursor, not the number of rows shown, ends it")

	// A fresh query re-pins the chain head and now includes seq 12.
	fmt.Println("== 5. a fresh AuditQuery re-pins the chain and includes the appended seq 12 ==")
	fresh, err := store.AuditQuery(org, 1, pageSize, "", target)
	if err != nil {
		return err
	}
	printPage(fresh, 1)
	page = fresh
	pageNo = 1
	for page.Next != 0 {
		pageNo++
		page, err = store.AuditPage(page.Checkpoint, page.Next, pageSize, "", target)
		if err != nil {
			return err
		}
		printPage(page, pageNo)
	}

	// Boundary 1: a subject with no decisions gets one empty page that ends
	// the walk; the chain is never returned unfiltered.
	fmt.Println("== 6. boundary: a subject with no decision records ==")
	empty, err := store.AuditQuery(org, 1, pageSize, "", unknown)
	if err != nil {
		return err
	}
	fmt.Printf("AuditQuery(subject=%q): records=%d next=%d end_seq=%d\n",
		unknown, len(empty.Records), empty.Next, empty.EndSeq)
	fmt.Println("(the walk ends on this empty page; no policy change or other subject's record is returned)")

	// Boundary 2: continuing on a checkpoint whose fingerprint was altered
	// fails with ErrInvalidRange and hands back no page at all.
	fmt.Println("== 7. boundary: continuing with an altered checkpoint fingerprint ==")
	tampered := pinned
	tampered.Fingerprint = flipFirstHexDigit(tampered.Fingerprint)
	bad, err := store.AuditPage(tampered, first.Next, pageSize, "", target)
	fmt.Printf("AuditPage(tampered checkpoint, next=%d): err=%q\n", first.Next, err)
	fmt.Printf("errors.Is(err, darksafe.ErrInvalidRange) = %v; partial page delivered = %v\n",
		errors.Is(err, darksafe.ErrInvalidRange), bad != nil)

	// All querying was read-only: version and chain are exactly what the
	// appends left (version 2, twelve records from one seq-1...seq-12 chain).
	fmt.Println("== 8. read-only check ==")
	all, _, err := store.AuditExport(org, 0)
	if err != nil {
		return err
	}
	fmt.Printf("CurrentVersion=%d, total audit records=%d: queries and paging neither appended records nor republished policies\n",
		store.CurrentVersion(org), len(all))
	return nil
}

// printChainRecord prints one full chain record with its ORIGINAL sequence:
// policy changes and every subject's decisions alike, so the interleave is
// visible before filtering is applied.
func printChainRecord(r darksafe.AuditRecord) {
	switch r.Kind {
	case darksafe.AuditPolicyChange:
		verb := "publish "
		if r.Change.RolledBack {
			verb = "rollback"
		}
		fmt.Printf("  seq=%-2d policy_change  %s version %d (%d policies)\n",
			r.Seq, verb, r.Change.Version, len(r.Change.Policies))
	case darksafe.AuditDecision:
		fmt.Printf("  seq=%-2d decision       %s\n", r.Seq, decisionRow(r))
	}
}

// printPage prints one page's window, cursor and the checkpoint shared by
// the whole walk, then each record's original sequence and decision detail.
func printPage(p *darksafe.AuditPage, n int) {
	fmt.Printf("page %d: begin_seq=%d end_seq=%d next=%d checkpoint={end_seq=%d fingerprint=%s}\n",
		n, p.BeginSeq, p.EndSeq, p.Next, p.Checkpoint.EndSeq, p.Checkpoint.Fingerprint)
	if len(p.Records) == 0 {
		fmt.Println("  (no matching records on this page)")
	}
	for _, r := range p.Records {
		fmt.Printf("  seq=%-2d %s\n", r.Seq, decisionRow(r))
	}
}

// decisionRow renders the fields a subject audit must preserve: the
// requesting subject, the allow/deny outcome, the reason, the matched
// policies and the policy version actually used.
func decisionRow(r darksafe.AuditRecord) string {
	d := r.Decision.Decision
	return fmt.Sprintf("subject=%-17s action=%-5s allowed=%-5t reason=%q matched=%q version=%d",
		r.Decision.Request.Subject.ID, r.Decision.Request.Action,
		d.Allowed, d.Reason, d.Matched, d.Version)
}

// flipFirstHexDigit changes exactly one hex digit of a fingerprint, modeling
// a retained checkpoint whose fingerprint was altered in storage.
func flipFirstHexDigit(fp string) string {
	if fp[0] == '0' {
		return "1" + fp[1:]
	}
	return "0" + fp[1:]
}
