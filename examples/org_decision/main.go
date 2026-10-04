// Command org_decision_example shows why one subject gets different answers
// from the role-based demo core (Access) and the organization-level policy
// engine (Store.Decide): the two are separate authorization bases. Access
// trusts the subject's roles; Decide ignores roles entirely and evaluates
// only the decision organization's published policies.
//
// Run with:
//
//	go run ./examples/org_decision
//
// The program takes no arguments, writes no files, and works fully offline.
// One subject carrying the owner role reads one ledger:
//
//  1. Access allows it, because the owner role is enough for the demo core.
//  2. A fresh Store denies the same read, because the organization has not
//     published any policy yet (version 0: no published policy was used).
//  3. After publishing one matching allow policy, the identical request is
//     allowed and reports the policy it matched and the version it used.
//  4. Two envelope boundaries deny before any policy is consulted: a
//     disabled subject, and a subject/resource organization that differs
//     from the decision organization. Both report version 0 and no matches.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "org decision example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	// One organization, one ledger, one subject. The subject carries the
	// owner role, which is decisive for Access and meaningless for Decide.
	const org = "acme factory"
	const (
		ledgerID    = "ledger-2026"
		ledgerScope = "acme/factory/ledger"
		subjectID   = "svc-audit-reader"
	)
	subject := darksafe.Subject{ID: subjectID, Kind: "service", Roles: []string{"owner"}}
	ledger := darksafe.Resource{ID: ledgerID, Scope: ledgerScope}

	// 1. The demo entry point's basis: roles. owner grants the read.
	printDecision("access  (role owner, no policy store)", darksafe.Access(subject, ledger, "read"))

	// 2. The organization-level basis: published policies. The request
	// envelope is valid (all three organizations agree, the scope is legal),
	// but nothing has been published, so the default is denial and the
	// version is 0 — no published policy was evaluated.
	store := darksafe.NewStore()
	req := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     subject,
		Resource:    ledger,
		Action:      "read",
	}
	printDecision("decide  (no published policy)      ", store.Decide(org, req))

	// 3. Publish one allow policy matching this subject, the read action and
	// the ledger scope, then submit the identical request again.
	version, err := store.Publish(org, 0, []darksafe.Policy{{
		ID:      "p-ledger-read-2026",
		Subject: subjectID,
		Action:  "read",
		Scope:   ledgerScope,
		Effect:  darksafe.EffectAllow,
	}})
	if err != nil {
		return fmt.Errorf("publish allow policy: %w", err)
	}
	fmt.Printf("published allow policy p-ledger-read-2026 as version %d\n", version)
	printDecision("decide  (allow policy published)   ", store.Decide(org, req))

	// Boundary 1: the same subject disabled. The matching allow policy is
	// still published, but the envelope check rejects first — no policy is
	// evaluated, so the version is 0 and the matched list is empty.
	disabled := req
	disabled.Subject.Disabled = true
	printDecision("decide  (subject disabled)         ", store.Decide(org, disabled))

	// Boundary 2: the subject's organization differs from the decision
	// organization (a resource-organization mismatch denies the same way).
	// Again no policy is evaluated: version 0, no matches.
	cross := req
	cross.SubjectOrg = "other org"
	printDecision("decide  (subject org mismatch)     ", store.Decide(org, cross))
	return nil
}

// printDecision prints the four fields both authorization bases report:
// allowed or not, the reason, the matched list (roles for Access, policy
// IDs for Decide) and the policy version actually used (0 when none was).
func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("%s: allowed=%v reason=%q matched=%q version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}
