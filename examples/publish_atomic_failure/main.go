// Command publish_atomic_failure is the worked example for organization-level
// policy publishing's all-or-nothing replacement semantics. It uses only the
// public Store API and the Go standard library, creates its own in-memory
// store, builds its own request, and prints publish results, decisions and
// audit-chain summaries so the three number spaces stay distinguishable:
//
//   - publish-returned version: the version Publish reports on success;
//   - actually-used version:   Decision.Version the access evaluation reports;
//   - audit sequence:          the gapless organization-local record number.
//
// Run with:
//
//	go run ./examples/publish_atomic_failure
//
// The program takes no arguments, writes no files, and works fully offline.
// One organization (acme factory), one enabled subject (svc-audit-reader),
// one resource (ledger-2026) in scope acme/factory/ledger, one action (read).
// The subject's org and the resource's org both equal the decision org, the
// scope is legal, and the subject is enabled: the request passes every
// envelope check, so its access is decided entirely by published policy.
//
// Steps:
//
//  1. Publish a one-policy set (allow read) as version 1. A first read
//     decision is allowed for the reason, hit id and version of v1.
//  2. Submit a replacing set meant to flip the read to deny: a valid deny
//     policy plus an unrelated valid allow, and an INVALID trailing policy
//     whose scope carries consecutive slashes. Publish must reject the WHOLE
//     commit with ErrInvalidPolicySet: no valid policy inside the set may be
//     applied first, current stays 1, and the read stays allowed with v1's
//     reason, matched id and version. The failed commit itself appends no
//     audit record; the prior checkpoint does not move.
//  3. A read decided AFTER the failure (an observation decision) is still
//     allowed by v1 and leaves a decision record by the existing rules; that
//     record must not be counted as a change produced by the failed publish.
//  4. Correct the invalid policy and resubmit the complete set against the
//     unchanged current version 1: it becomes the immediate next version 2.
//     Publish replaces the WHOLE set, not appends: version 2 holds exactly
//     the submitted policies; v1's allow policy is gone from v2 but remains
//     queryable in version 1. The read flips to a clear denial with the deny
//     policy hit at version 2; the policy-change record saves the full new
//     set.
//  5. A resubmission whose content is valid but whose expected version is
//     stale gets ErrVersionConflict: it neither overrides current policy nor
//     leaves a change record; the caller must retry against current version.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "publish atomic failure example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	const org = "acme factory"
	const (
		subjectID   = "svc-audit-reader"
		resourceID  = "ledger-2026"
		scope       = "acme/factory/ledger"
		actionRead  = "read"
		allowReadID = "p-ledger-read-allow"
		denyReadID  = "p-ledger-read-deny"
		bystanderID = "p-other-scope-allow"
	)

	store := darksafe.NewStore()

	// The same request at every step: subject org == resource org == decision
	// org, enabled subject, legal scope, non-empty ids and action. Access is
	// therefore decided purely by the currently published policy set.
	req := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "service"}, // Disabled defaults to false
		Resource:    darksafe.Resource{ID: resourceID, Scope: scope},
		Action:      actionRead,
	}

	// -- Step 1: publish an allow-read policy set as version 1 -------------

	fmt.Println("step 1: publish allow-read policies as version 1")
	v1Set := []darksafe.Policy{{
		ID:      allowReadID,
		Subject: subjectID,
		Action:  actionRead,
		Scope:   scope,
		Effect:  darksafe.EffectAllow,
	}}
	v1, err := store.Publish(org, 0, v1Set) // 0: never published before
	if err != nil {
		return fmt.Errorf("publish v1: %w", err)
	}
	fmt.Printf("publish: ok returned-version=%d\n", v1)
	printChain(store, org)
	d := store.Decide(org, req)
	printDecision("decide : ", d)
	fmt.Println()

	// -- Step 2: failed replacement with an invalid trailing policy --------

	fmt.Println("step 2: submit deny-read set containing an invalid policy (consecutive slashes)")
	badSet := []darksafe.Policy{
		{ID: denyReadID, Subject: subjectID, Action: actionRead, Scope: scope, Effect: darksafe.EffectDeny},
		{ID: bystanderID, Subject: "svc-other", Action: actionRead, Scope: "acme/other/scope", Effect: darksafe.EffectAllow},
		// Consecutive slashes make this scope illegal; the whole set must fail.
		{ID: "p-bad-scope", Subject: subjectID, Action: actionRead, Scope: "acme//bad", Effect: darksafe.EffectAllow},
	}
	printSubmittedSet(badSet)
	returned, err := store.Publish(org, v1, badSet)
	if !errors.Is(err, darksafe.ErrInvalidPolicySet) {
		return fmt.Errorf("bad publish: err=%v, want ErrInvalidPolicySet", err)
	}
	fmt.Printf("publish: failed returned-version=%d err=%v\n", returned, err)
	fmt.Printf("current version still %d; no version consumed, no policy-change record appended\n",
		store.CurrentVersion(org))
	printChain(store, org)

	// -- Step 3: an observation decision after the failure -----------------

	fmt.Println()
	fmt.Println("step 3: re-decide the same read to observe the effect (this is a new decision, not the failed publish)")
	d2 := store.Decide(org, req)
	printDecision("decide : ", d2)
	if d2.Allowed != true || d2.Reason != "matched allow policy" ||
		len(d2.Matched) != 1 || d2.Matched[0] != allowReadID || d2.Version != 1 {
		return fmt.Errorf("post-failure read = %+v, want allow by %q at v1", d2, allowReadID)
	}
	fmt.Println("the read is still decided by v1: same reason, matched id and actually-used version")
	printChain(store, org)
	fmt.Println()

	// -- Step 4: correct and resubmit against unchanged current version ----

	fmt.Println("step 4: correct the bad policy and resubmit the whole set against current version 1")
	goodSet := []darksafe.Policy{
		{ID: denyReadID, Subject: subjectID, Action: actionRead, Scope: scope, Effect: darksafe.EffectDeny},
		{ID: bystanderID, Subject: "svc-other", Action: actionRead, Scope: "acme/other/scope", Effect: darksafe.EffectAllow},
		{ID: "p-bad-scope", Subject: subjectID, Action: actionRead, Scope: "acme/legal/scope", Effect: darksafe.EffectAllow},
	}
	printSubmittedSet(goodSet)
	v2, err := store.Publish(org, v1, goodSet)
	if err != nil {
		return fmt.Errorf("publish corrected set: %w", err)
	}
	fmt.Printf("publish: ok returned-version=%d (immediate next version after the failed attempt)\n", v2)
	d3 := store.Decide(org, req)
	printDecision("decide : ", d3)
	if d3.Allowed != false || d3.Reason != "matched deny policy" ||
		len(d3.Matched) != 1 || d3.Matched[0] != denyReadID || d3.Version != 2 {
		return fmt.Errorf("corrected read = %+v, want matched deny %q at v2", d3, denyReadID)
	}
	fmt.Println("deny-overrides: the valid allow on a different scope does not match; only the deny hits")
	printChain(store, org)

	// Publish replaces the whole set: v2 contains exactly what was submitted,
	// and the old version still serves its original content.
	gotV2, err := store.Policies(org, v2)
	if err != nil {
		return fmt.Errorf("Policies(v2): %w", err)
	}
	if !samePolicySet(gotV2, goodSet) {
		return fmt.Errorf("v2 policies = %+v, want exactly the submitted set %+v", gotV2, goodSet)
	}
	gotV1, err := store.Policies(org, 1)
	if err != nil {
		return fmt.Errorf("Policies(v1): %w", err)
	}
	if !samePolicySet(gotV1, v1Set) {
		return fmt.Errorf("v1 policies = %+v, want original allow set %+v", gotV1, v1Set)
	}
	fmt.Printf("replacement proven: version %d holds the %d submitted policies (no %q), version 1 still has it\n",
		v2, len(goodSet), allowReadID)
	fmt.Println()

	// -- Step 5: stale expected version -> ErrVersionConflict --------------

	fmt.Println("step 5: resubmit valid content with a stale expected version")
	stale := []darksafe.Policy{
		{ID: "p-stale-allow", Subject: subjectID, Action: actionRead, Scope: scope, Effect: darksafe.EffectAllow},
	}
	printSubmittedSet(stale)
	before := store.CurrentVersion(org)
	beforeRecs, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export before stale submit: %w", err)
	}
	returned, err = store.Publish(org, 1, stale) // current is already 2
	if !errors.Is(err, darksafe.ErrVersionConflict) {
		return fmt.Errorf("stale publish: err=%v, want ErrVersionConflict", err)
	}
	fmt.Printf("publish: failed returned-version=%d err=%v\n", returned, err)
	if store.CurrentVersion(org) != before {
		return fmt.Errorf("current changed to %d after stale submit, want %d", store.CurrentVersion(org), before)
	}
	afterRecs, _, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export after stale submit: %w", err)
	}
	if len(afterRecs) != len(beforeRecs) {
		return fmt.Errorf("stale submit added records: %d -> %d", len(beforeRecs), len(afterRecs))
	}
	fmt.Printf("current version still %d; no override, no change record; retry against current version\n", before)
	d4 := store.Decide(org, req)
	printDecision("decide : ", d4)
	if d4.Version != 2 || d4.Allowed || d4.Matched[0] != denyReadID {
		return fmt.Errorf("post-conflict read = %+v, want deny by %q at v2", d4, denyReadID)
	}
	fmt.Println()
	printChain(store, org)
	return nil
}

// printSubmittedSet lists every policy in a submitted batch, including the
// invalid one, so the reader can see the complete commit that was attempted.

func printSubmittedSet(policies []darksafe.Policy) {
	fmt.Printf("submitted set (%d policies; publish replaces the whole set, not appends):\n", len(policies))
	for _, p := range policies {
		fmt.Printf("  policy id=%q subject=%q action=%q scope=%q effect=%q\n",
			p.ID, p.Subject, p.Action, p.Scope, p.Effect)
	}
}

// printDecision prints the four decision fields, keeping the actually-used
// version visually separate from publish versions and audit sequences.
func printDecision(prefix string, d darksafe.Decision) {
	fmt.Printf("%sallowed=%v reason=%q matched=%q actually-used-version=%d\n",
		prefix, d.Allowed, d.Reason, d.Matched, d.Version)
}

// printChain lists the audit chain after each operation: the sequence numbers
// are their own number space, never the same number as a policy version.
func printChain(store *darksafe.Store, org string) {
	records, cp, err := store.AuditExport(org, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export audit chain: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("audit: %d record(s), checkpoint end-seq=%d\n", len(records), cp.EndSeq)
	for _, r := range records {
		switch r.Kind {
		case darksafe.AuditPolicyChange:
			fmt.Printf("  seq=%d policy_change published-version=%d policies=%d\n",
				r.Seq, r.Change.Version, len(r.Change.Policies))
		case darksafe.AuditDecision:
			d := r.Decision.Decision
			fmt.Printf("  seq=%d decision     subject=%s allowed=%v reason=%q matched=%q used-version=%d\n",
				r.Seq, r.Decision.Request.Subject.ID, d.Allowed, d.Reason, d.Matched, d.Version)
		}
	}
}

// samePolicySet compares two sets as multisets of complete policy values, so
// the replacement assertion covers every field without depending on snapshot
// ordering. Policy is comparable (only string and bool fields).
func samePolicySet(a, b []darksafe.Policy) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[darksafe.Policy]int, len(a))
	for _, p := range a {
		seen[p]++
	}
	for _, p := range b {
		seen[p]--
		if seen[p] < 0 {
			return false
		}
	}
	return true
}
