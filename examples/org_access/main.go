// Command org_access_example shows why the role-based demo Access and the
// organization-level Store.Decide can reach opposite conclusions for the very
// same subject and resource. One subject (carrying the owner role) reads one
// ledger (with a legal scope) through both entry points; the program then
// publishes the matching allow policy and replays the same request, and
// finally shows the two boundaries that reject even a "would match" request
// before any policy is consulted.
//
// Run with:
//
//	go run ./examples/org_access
//
// The program takes no arguments, performs no network or file access, and
// uses only this module's public API and the Go standard library. Every
// result is deterministic.
package main

import (
	"fmt"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	// One subject, one ledger, one action, reused byte-for-byte in every
	// call below. The subject carries "owner": the broadest role the
	// role-based demo core understands. The ledger scope is a legal
	// slash-separated path.
	const (
		org         = "acme"
		ledgerID    = "ledger-main"
		ledgerScope = "acme/ledger"
		subjectID   = "svc-ledger-owner"
		action      = "read"
	)
	subject := darksafe.Subject{
		ID:    subjectID,
		Kind:  "service",
		Roles: []string{"owner"}, // grants everything under Access; ignored by Decide
	}
	ledger := darksafe.Resource{ID: ledgerID, Scope: ledgerScope}

	// --- Step 1: the demo entry point ------------------------------------
	// Access is role-based and store-free. owner matches every action/scope,
	// so the read is allowed. The matched entry is the ROLE that granted
	// access, not a policy id. Access creates no Store, has no policy
	// versions and leaves no organization audit record.
	fmt.Println("1) demo: darksafe.Access (role-based, no Store)")
	d := darksafe.Access(subject, ledger, action)
	printDecision("   Access", d)
	fmt.Println("   matched entry = the role that grants access: owner")
	fmt.Println()

	// --- Step 2: the organization-level entry point, no policy yet --------
	// A fresh Store has never had a policy published for the org. The
	// decision organization, the subject organization and the resource
	// organization are all the same "acme", so the request envelope is
	// legal; roles (including owner) are simply not consulted. With no
	// published version the result is a denial whose version is 0.
	store := darksafe.NewStore()
	baseReq := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     subject,
		Resource:    ledger,
		Action:      action,
	}
	fmt.Println("2) organization: Store.Decide before any policy is published")
	fmt.Printf("   current version before publish: %d\n", store.CurrentVersion(org))
	d = store.Decide(org, baseReq)
	printDecision("   Decide", d)
	fmt.Println("   version 0 means no published policy set was evaluated;")
	fmt.Println("   it does not by itself say the request is allowed or denied.")
	fmt.Println()

	// --- Step 3: a published but EMPTY policy set -------------------------
	// Publishing an empty set still creates a real version (1). A legal
	// request evaluated against it is denied by default ("no matching allow
	// policy"), but the decision now reports that empty set's version, so the
	// caller can tell "denied by the published empty set" apart from "nothing
	// published yet".
	v1, err := store.Publish(org, 0, nil)
	if err != nil {
		panic(fmt.Sprintf("publish empty set as version 1: %v", err))
	}
	fmt.Printf("3) published an EMPTY policy set; it became version %d\n", v1)
	d = store.Decide(org, baseReq)
	printDecision("   Decide", d)
	fmt.Println("   owner is not consulted: an empty published set still denies by default,")
	fmt.Println("   and the decision names the empty set's version (1), not 0.")
	fmt.Println()

	// --- Step 4: publish the matching allow policy, then replay ----------
	// Version 2 holds one allow policy matching this exact subject, action
	// and ledger scope. Re-submitting the identical request is now allowed.
	// The matched entry is the POLICY IDENTIFIER, and the version is the one
	// actually evaluated (2). The subject still carries owner; it plays no
	// part in this result.
	v2, err := store.Publish(org, v1, []darksafe.Policy{{
		ID:      "p-ledger-read",
		Subject: subjectID,
		Action:  action,
		Scope:   ledgerScope,
		Effect:  darksafe.EffectAllow,
	}})
	if err != nil {
		panic(fmt.Sprintf("publish allow policy as version 2: %v", err))
	}
	fmt.Printf("4) published allow policy %q; it became version %d\n", "p-ledger-read", v2)
	d = store.Decide(org, baseReq)
	printDecision("   Decide", d)
	fmt.Println("   matched entry = the policy identifier p-ledger-read, not a role.")
	fmt.Println()

	// --- Boundary A: a matching allow policy does not save a disabled ----
	// subject. The envelope rejects before the policy set is evaluated, so no
	// policy is used: version stays 0, the matched list is empty, and the
	// reason distinguishes the cause.
	fmt.Println("5) boundary A: subject disabled even though the allow policy matches")
	disabledReq := baseReq
	disabledReq.Subject = darksafe.Subject{
		ID:       subjectID,
		Kind:     "service",
		Roles:    []string{"owner"}, // still present, still irrelevant
		Disabled: true,
	}
	d = store.Decide(org, disabledReq)
	printDecision("   Decide", d)
	fmt.Println("   rejected before policy evaluation: version 0, no matched policy.")
	fmt.Println()

	// --- Boundary B: subject and resource must belong to the deciding ----
	// org. Moving either one to another organization rejects at the envelope,
	// again with version 0 and an empty matched list. Two one-field changes
	// are shown: subject org first, then resource org.
	other := "globex"
	fmt.Println("6) boundary B: subject org or resource org differs from the decision org")
	subjectOtherReq := baseReq
	subjectOtherReq.SubjectOrg = other
	d = store.Decide(org, subjectOtherReq)
	printDecision("   subjectOrg=globex", d)

	resourceOtherReq := baseReq
	resourceOtherReq.ResourceOrg = other
	d = store.Decide(org, resourceOtherReq)
	printDecision("   resourceOrg=globex", d)
	fmt.Println("   neither request reaches the policies: version 0, no matched policy.")
	fmt.Println()

	// --- Audit behavior --------------------------------------------------
	// Access never created a Store and left nothing. On this Store, every
	// Decide with the non-empty decision org "acme" was recorded (allowed and
	// denied alike), as was each successful Publish. Exporting the chain makes
	// that visible; Review/RecheckDecisionOffline stay read-only and add none.
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		panic(fmt.Sprintf("export audit chain: %v", err))
	}
	fmt.Printf("7) audit: %d records for %q from this run\n", len(records), org)
	fmt.Println("   (2 policy publishes + 6 Decide calls; the demo Access left none)")
	for _, r := range records {
		switch r.Kind {
		case darksafe.AuditPolicyChange:
			fmt.Printf("   seq %d: %s version=%d\n", r.Seq, r.Kind, r.Change.Version)
		case darksafe.AuditDecision:
			fmt.Printf("   seq %d: %s allowed=%v version=%d\n",
				r.Seq, r.Kind, r.Decision.Decision.Allowed, r.Decision.Decision.Version)
		}
	}
}

// printDecision renders the four fields every decision carries, so the
// differences between Access and Decide are visible at a glance. The matched
// list is quoted element by element to stay readable when empty.
func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("%s: allowed=%v reason=%q matched=%v version=%d\n",
		label, d.Allowed, d.Reason, quoteList(d.Matched), d.Version)
}

// quoteList renders a hit list as Go-quoted elements: [] for none,
// ["owner"] / ["p-ledger-read"] otherwise. Access hits are role names;
// Decide hits are policy identifiers.
func quoteList(items []string) string {
	out := "["
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", s)
	}
	return out + "]"
}
