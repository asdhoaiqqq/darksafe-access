// Command scope_deny_override is the cross-scope worked example for the
// organization-level deny-overrides rule. It uses only the public Store API
// and the Go standard library, creates its own in-memory store, publishes
// real policy versions, and prints the full policy set and request next to
// every decision so the configuration can be compared directly with the
// result.
//
// Run with:
//
//	go run ./examples/scope_deny_override
//
// The program takes no arguments, writes no files, and works fully offline.
// One organization (acme), one enabled subject (u1), one resource (doc-1),
// one action (read). Three full-set publishes drive three cases:
//
//  1. A recursive deny on org/a and an exact allow on org/a/b are published
//     together as version 1. The read of doc-1 in org/a/b matches BOTH: the
//     parent deny reaches the child scope, and the verdict is a denial whose
//     hit list keeps both identifiers (sorted ascending) at version 1.
//  2. The same set is republished with the parent deny made non-recursive
//     (version 2). The deny then covers org/a alone; the child read matches
//     only the child allow and is allowed.
//  3. Version 3 keeps the recursive deny on org/a and instead adds an exact
//     allow on org/ab. org/ab is a slash-segment sibling, not a descendant
//     of org/a, so the deny cannot match and the org/ab read is allowed by
//     its own exact allow alone.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scope deny override example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	// One organization, one subject, one resource id, one action. The subject
	// carries no roles at all: roles belong to the demo Access entry point and
	// are never consulted by Store.Decide.
	const (
		org        = "acme"
		subjectID  = "u1"
		resourceID = "doc-1"
		actionRead = "read"

		parentScope  = "org/a"
		childScope   = "org/a/b"
		siblingScope = "org/ab"

		childAllowID   = "a-child-allow"
		parentDenyID   = "z-parent-deny"
		siblingAllowID = "ab-exact-allow"
	)

	store := darksafe.NewStore()

	// Case 1: one publish carries both policies. The child allow is exact on
	// org/a/b; the parent deny is recursive on org/a, so it covers the whole
	// slash-separated subtree, including org/a/b.
	fmt.Println("case 1: recursive parent deny vs more specific child allow")
	policiesV1 := []darksafe.Policy{
		{ID: childAllowID, Subject: subjectID, Action: actionRead, Scope: childScope, Effect: darksafe.EffectAllow},
		{ID: parentDenyID, Subject: subjectID, Action: actionRead, Scope: parentScope, Effect: darksafe.EffectDeny, Recursive: true},
	}
	v1, err := store.Publish(org, 0, policiesV1) // 0: the org has never published
	if err != nil {
		return fmt.Errorf("publish version 1: %w", err)
	}
	printPolicySet(v1, policiesV1)
	childReq := readRequest(org, subjectID, resourceID, childScope, actionRead)
	printRequest(org, childReq)
	printDecision(store.Decide(org, childReq))
	fmt.Println()

	// Case 2: republish the COMPLETE set with the parent deny made
	// non-recursive. expectedVersion is the current version (1); the new set
	// becomes version 2. A non-recursive policy covers its exact scope only.
	fmt.Println("case 2: parent deny made non-recursive, same child read")
	policiesV2 := []darksafe.Policy{
		{ID: childAllowID, Subject: subjectID, Action: actionRead, Scope: childScope, Effect: darksafe.EffectAllow},
		{ID: parentDenyID, Subject: subjectID, Action: actionRead, Scope: parentScope, Effect: darksafe.EffectDeny, Recursive: false},
	}
	v2, err := store.Publish(org, v1, policiesV2)
	if err != nil {
		return fmt.Errorf("publish version 2: %w", err)
	}
	printPolicySet(v2, policiesV2)
	printRequest(org, childReq) // identical request: only the policy set changed
	printDecision(store.Decide(org, childReq))
	fmt.Println()

	// Case 3: recursive deny back in force, plus an exact allow for org/ab.
	// Matching is by slash-separated segments: org/ab is not below org/a.
	fmt.Println("case 3: recursive deny on org/a cannot reach the sibling scope org/ab")
	policiesV3 := []darksafe.Policy{
		{ID: parentDenyID, Subject: subjectID, Action: actionRead, Scope: parentScope, Effect: darksafe.EffectDeny, Recursive: true},
		{ID: siblingAllowID, Subject: subjectID, Action: actionRead, Scope: siblingScope, Effect: darksafe.EffectAllow},
	}
	v3, err := store.Publish(org, v2, policiesV3)
	if err != nil {
		return fmt.Errorf("publish version 3: %w", err)
	}
	printPolicySet(v3, policiesV3)
	siblingReq := readRequest(org, subjectID, resourceID, siblingScope, actionRead)
	printRequest(org, siblingReq)
	printDecision(store.Decide(org, siblingReq))
	return nil
}

// readRequest builds an envelope-valid request: subject org and resource org
// both equal the decision organization, and the subject is enabled.
func readRequest(org, subjectID, resourceID, scope, action string) darksafe.OrgRequest {
	return darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "user"}, // Disabled defaults to false
		Resource:    darksafe.Resource{ID: resourceID, Scope: scope},
		Action:      action,
	}
}

// printPolicySet prints every field that participates in matching, including
// the empty resource-id, so "no resource restriction" is visible instead of
// assumed.
func printPolicySet(version int, policies []darksafe.Policy) {
	fmt.Printf("  published as version %d (%d policies):\n", version, len(policies))
	for _, p := range policies {
		fmt.Printf("    policy id=%q subject=%q action=%q scope=%q effect=%q recursive=%v resource-id=%q\n",
			p.ID, p.Subject, p.Action, p.Scope, p.Effect, p.Recursive, p.ResourceID)
	}
}

// printRequest prints the complete request envelope next to the policies, so
// nothing has to be reconstructed by the reader.
func printRequest(org string, req darksafe.OrgRequest) {
	fmt.Printf("  request: decision-org=%q subject-org=%q resource-org=%q subject=%q disabled=%v resource=%q scope=%q action=%q\n",
		org, req.SubjectOrg, req.ResourceOrg, req.Subject.ID, req.Subject.Disabled,
		req.Resource.ID, req.Resource.Scope, req.Action)
}

// printDecision prints the four decision fields: allowed or not, the reason,
// every matched policy identifier, and the published version actually used.
func printDecision(d darksafe.Decision) {
	fmt.Printf("  decide : allowed=%v reason=%q matched=%q version=%d\n",
		d.Allowed, d.Reason, d.Matched, d.Version)
}
