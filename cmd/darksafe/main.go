// Command darksafe is the 零信任身份与授权决策平台 entry point.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("darksafe 0.1.0")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: darksafe [demo|version|help]")
}

func runDemo() {
	subjects := []darksafe.Subject{
		{ID: "u-1001", Kind: "user", Roles: []string{"org/payments/ledger:read"}},
		{ID: "svc-batch", Kind: "service", Roles: []string{"owner"}},
		{ID: "u-1002", Kind: "user", Disabled: true},
	}
	resource := darksafe.Resource{ID: "ledger-main", Scope: "org/payments/ledger"}
	allowed := 0
	for _, subject := range subjects {
		decision := darksafe.Access(subject, resource, "read")
		if decision.Allowed {
			allowed++
		}
		fmt.Printf("subject=%s allowed=%v reason=%s\n", subject.ID, decision.Allowed, decision.Reason)
	}
	fmt.Printf("summary: %d of %d subjects allowed\n", allowed, len(subjects))

	runOrgDemo()
}

// runOrgDemo demonstrates per-organization policy publish, decision, rollback
// and historical review. Organizations manage independent policy sets; a
// subject's roles never grant access under a published policy set.
func runOrgDemo() {
	fmt.Println()
	fmt.Println("org-scoped policy store demo")

	store := darksafe.NewStore()
	org := "acme"

	subject := darksafe.Subject{ID: "u-1001", Kind: "user", Org: org}
	resource := darksafe.Resource{ID: "ledger-main", Scope: "org/payments/ledger", Org: org}

	// A new organization starts at version 0: no published version yet.
	if v, err := store.CurrentVersion(org); err == nil {
		fmt.Printf("initial version: %d\n", v)
	}
	d := store.Decide(org, subject, resource, "read")
	fmt.Printf("before publish: allowed=%v reason=%s version=%d\n", d.Allowed, d.Reason, d.Version)

	// First publish creates version 1.
	policies := []darksafe.Policy{
		{ID: "p-allow-read", Subject: "u-1001", Action: "read", Scope: "org/payments", Effect: darksafe.EffectAllow, Recursive: true},
	}
	v, err := store.Publish(org, 0, policies)
	if err != nil {
		fmt.Printf("publish failed: %v\n", err)
		return
	}
	fmt.Printf("published version: %d\n", v)
	d = store.Decide(org, subject, resource, "read")
	fmt.Printf("after publish: allowed=%v reason=%s version=%d matched=%v\n", d.Allowed, d.Reason, d.Version, d.Matched)

	// A second publish with a deny policy creates version 2.
	policies = append(policies, darksafe.Policy{ID: "p-deny-read", Subject: "u-1001", Action: "read", Scope: "org/payments/ledger", Effect: darksafe.EffectDeny})
	v, err = store.Publish(org, 1, policies)
	if err != nil {
		fmt.Printf("publish failed: %v\n", err)
		return
	}
	fmt.Printf("published version: %d\n", v)
	d = store.Decide(org, subject, resource, "read")
	fmt.Printf("after deny: allowed=%v reason=%s version=%d matched=%v\n", d.Allowed, d.Reason, d.Version, d.Matched)

	// Review against version 1 still allows, even though the current version 2
	// denies it.
	review := store.Review(org, 1, subject, resource, "read")
	fmt.Printf("review v1: allowed=%v reason=%s version=%d matched=%v\n", review.Allowed, review.Reason, review.Version, review.Matched)

	// Rollback to version 1 publishes its content as a new version 3.
	v, err = store.Rollback(org, 2, 1)
	if err != nil {
		fmt.Printf("rollback failed: %v\n", err)
		return
	}
	fmt.Printf("rolled back to version: %d\n", v)
	d = store.Decide(org, subject, resource, "read")
	fmt.Printf("after rollback: allowed=%v reason=%s version=%d matched=%v\n", d.Allowed, d.Reason, d.Version, d.Matched)

	// Optimistic concurrency: a publish with a stale expected version fails.
	_, err = store.Publish(org, 1, nil)
	fmt.Printf("stale publish: %v\n", err)

	// Cross-organization isolation: a subject from another org is denied.
	other := darksafe.Subject{ID: "u-1001", Org: "other"}
	d = store.Decide(org, other, resource, "read")
	fmt.Printf("cross-org subject: allowed=%v reason=%s\n", d.Allowed, d.Reason)
}
