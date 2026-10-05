// Command scope_parent_deny_example is the offline worked example for
// deny-overrides across scope levels in organization-level published
// policies (Store.Decide). It uses only this project's public API and the
// Go standard library, creates its own in-memory stores, and needs no
// network.
//
// Run with:
//
//	go run ./examples/scope_parent_deny
//
// One organization (acme), one enabled subject (u1), one resource id
// (doc-1) and one action (read) run through three scenarios. The resource
// id stays doc-1 in every request: none of the policies restricts
// ResourceID, so only the scope differs between requests, and that alone
// changes the decisions.
//
//  1. An exact allow on org/a/b ("a-child-allow") is published together
//     with a recursive deny on org/a ("z-parent-deny"). Reading doc-1 in
//     org/a/b matches BOTH policies; deny-overrides denies with reason
//     "matched deny policy", and the hit list keeps both policy ids sorted
//     ascending at the version actually evaluated.
//  2. The same parent deny is republished without Recursive. It then
//     covers only org/a itself, so the org/a/b read matches the child allow
//     alone and is allowed; a read exactly at org/a is still denied by the
//     non-recursive deny, proving the allow came from scope semantics.
//  3. Version 1 publishes only the recursive deny on org/a. Reading a
//     doc-1 resource located in org/ab does not match the deny (scope
//     matching is slash-separated segments, never name prefix) and there is
//     no allow either, so it is denied with "no matching allow policy": a
//     deny that misses is not an automatic allow. Version 2 adds an exact
//     allow on org/ab; the identical request then matches that allow alone
//     and passes at version 2.
//
// Scenarios 1 and 2 each start from a fresh Store, so their first (and
// only) publish is version 1. Scenario 3 publishes twice in one Store to
// show versions 1 and 2 back to back; the version on each decision is the
// published version that decision actually evaluated.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	org = "acme"

	subject = "u1"
	action  = "read"
	docID   = "doc-1"

	childScope   = "org/a/b"
	parentScope  = "org/a"
	siblingScope = "org/ab"

	childAllowID   = "a-child-allow"
	parentDenyID   = "z-parent-deny"
	siblingAllowID = "ab-exact-allow"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scope parent deny example: failed:", err)
		os.Exit(1)
	}
}

// enabledSubject is enabled and deliberately carries NO roles: Decide
// ignores subject roles entirely and evaluates only published policies.
var enabledSubject = darksafe.Subject{ID: subject, Kind: "user"}

// newRequest builds an envelope-valid request: decision org, subject org
// and resource org are all acme, the subject is enabled, and the only
// thing callers vary is the resource scope.
func newRequest(scope string) darksafe.OrgRequest {
	return darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     enabledSubject,
		Resource:    darksafe.Resource{ID: docID, Scope: scope},
		Action:      action,
	}
}

func run() error {
	if err := scenarioRecursiveDenyOverridesChildAllow(); err != nil {
		return err
	}
	fmt.Println()
	if err := scenarioNonRecursiveDenyStaysAtParent(); err != nil {
		return err
	}
	fmt.Println()
	if err := scenarioSegmentBoundaryIsNotNamePrefix(); err != nil {
		return err
	}
	return nil
}

// Scenario 1: the recursive parent deny and the exact child allow both
// match the org/a/b read. Deny wins, and the overridden allow stays in the
// matched list as evidence of the conflict.
func scenarioRecursiveDenyOverridesChildAllow() error {
	fmt.Println("scenario 1/3: recursive deny on org/a overrides the more specific exact allow on org/a/b")
	store := darksafe.NewStore()
	policies := []darksafe.Policy{
		// Exact allow on the child scope; Recursive defaults to false, so it
		// matches org/a/b only. Empty ResourceID means any resource id.
		{ID: childAllowID, Subject: subject, Action: action, Scope: childScope, Effect: darksafe.EffectAllow},
		// Deny on the parent scope with Recursive=true: it covers org/a and
		// every descendant scope, org/a/b included.
		{ID: parentDenyID, Subject: subject, Action: action, Scope: parentScope, Effect: darksafe.EffectDeny, Recursive: true},
	}
	version, err := store.Publish(org, 0, policies)
	if err != nil {
		return fmt.Errorf("scenario 1 publish: %w", err)
	}
	printPublished(store, version, policies)

	fmt.Println("request: read doc-1 located in the child scope org/a/b")
	d := store.Decide(org, newRequest(childScope))
	printRequest(childScope)
	printDecision(d)

	// Pinned expectation for readers checking the example by eye.
	if d.Allowed || d.Reason != "matched deny policy" || d.Version != version ||
		len(d.Matched) != 2 || d.Matched[0] != childAllowID || d.Matched[1] != parentDenyID {
		return fmt.Errorf("scenario 1: unexpected decision %+v", d)
	}
	return nil
}

// Scenario 2: the same deny without Recursive reaches only org/a. The
// child read is granted by the child allow alone; a read exactly at org/a
// is still denied, so the allow is a scope effect, not a weakened deny.
func scenarioNonRecursiveDenyStaysAtParent() error {
	fmt.Println("scenario 2/3: the same parent deny without Recursive stays on org/a; the child read passes")
	store := darksafe.NewStore()
	policies := []darksafe.Policy{
		{ID: childAllowID, Subject: subject, Action: action, Scope: childScope, Effect: darksafe.EffectAllow},
		// Identical to scenario 1's deny except Recursive=false: only an
		// exact-scope request at org/a matches it.
		{ID: parentDenyID, Subject: subject, Action: action, Scope: parentScope, Effect: darksafe.EffectDeny, Recursive: false},
	}
	version, err := store.Publish(org, 0, policies)
	if err != nil {
		return fmt.Errorf("scenario 2 publish: %w", err)
	}
	printPublished(store, version, policies)

	fmt.Println("request A: read doc-1 located in the child scope org/a/b (deny out of reach, child allow matches)")
	printRequest(childScope)
	child := store.Decide(org, newRequest(childScope))
	printDecision(child)
	if !child.Allowed || child.Reason != "matched allow policy" || child.Version != version ||
		len(child.Matched) != 1 || child.Matched[0] != childAllowID {
		return fmt.Errorf("scenario 2 child request: unexpected decision %+v", child)
	}

	fmt.Println("request B: read doc-1 exactly at org/a (the non-recursive deny still guards its own scope)")
	printRequest(parentScope)
	atParent := store.Decide(org, newRequest(parentScope))
	printDecision(atParent)
	if atParent.Allowed || atParent.Reason != "matched deny policy" || atParent.Version != version ||
		len(atParent.Matched) != 1 || atParent.Matched[0] != parentDenyID {
		return fmt.Errorf("scenario 2 parent request: unexpected decision %+v", atParent)
	}
	return nil
}

// Scenario 3: org/ab is a different branch, not a descendant of org/a.
// Version 1 has only the recursive deny, which cannot match org/ab; with
// no allow present the read is denied by the default rule. Version 2 adds
// an exact allow on org/ab, and the identical request is then allowed by
// that policy alone.
func scenarioSegmentBoundaryIsNotNamePrefix() error {
	fmt.Println("scenario 3/3: org/ab is not a child of org/a; scopes match by slash segments, not name prefix")
	store := darksafe.NewStore()

	denyOnly := []darksafe.Policy{
		{ID: parentDenyID, Subject: subject, Action: action, Scope: parentScope, Effect: darksafe.EffectDeny, Recursive: true},
	}
	v1, err := store.Publish(org, 0, denyOnly)
	if err != nil {
		return fmt.Errorf("scenario 3 publish v1: %w", err)
	}
	printPublished(store, v1, denyOnly)

	fmt.Println("request A (version 1): read doc-1 located in org/ab — the recursive deny cannot match, and no allow exists")
	printRequest(siblingScope)
	missed := store.Decide(org, newRequest(siblingScope))
	printDecision(missed)
	if missed.Allowed || missed.Reason != "no matching allow policy" || missed.Version != v1 || len(missed.Matched) != 0 {
		return fmt.Errorf("scenario 3 v1 request: unexpected decision %+v", missed)
	}

	// Publish the complete version-2 set: publish always replaces the whole
	// set, so the deny is listed again alongside the new exact allow.
	v2Policies := []darksafe.Policy{
		{ID: parentDenyID, Subject: subject, Action: action, Scope: parentScope, Effect: darksafe.EffectDeny, Recursive: true},
		{ID: siblingAllowID, Subject: subject, Action: action, Scope: siblingScope, Effect: darksafe.EffectAllow},
	}
	v2, err := store.Publish(org, v1, v2Policies)
	if err != nil {
		return fmt.Errorf("scenario 3 publish v2: %w", err)
	}
	printPublished(store, v2, v2Policies)

	fmt.Println("request B (version 2): the same org/ab read after an exact allow on org/ab is added")
	printRequest(siblingScope)
	allowed := store.Decide(org, newRequest(siblingScope))
	printDecision(allowed)
	if !allowed.Allowed || allowed.Reason != "matched allow policy" || allowed.Version != v2 ||
		len(allowed.Matched) != 1 || allowed.Matched[0] != siblingAllowID {
		return fmt.Errorf("scenario 3 v2 request: unexpected decision %+v", allowed)
	}
	return nil
}

// printPublished records which store version a publish created and prints
// every policy exactly as submitted: subject, action, scope, effect, the
// Recursive flag and its resource restriction.
func printPublished(_ *darksafe.Store, version int, policies []darksafe.Policy) {
	fmt.Printf("published version %d for org %q with %d policies:\n", version, org, len(policies))
	for _, p := range policies {
		resource := p.ResourceID
		if resource == "" {
			resource = "(any resource id)"
		}
		fmt.Printf("  policy %q: subject=%s action=%s scope=%s effect=%s recursive=%v resource=%s\n",
			p.ID, p.Subject, p.Action, p.Scope, p.Effect, p.Recursive, resource)
	}
}

// printRequest prints the full envelope so the three matching
// organizations and the subject's enabled, roleless state are visible
// without opening the source.
func printRequest(scope string) {
	fmt.Printf("  Decide(org=%q): subject id=%s kind=%s disabled=%v roles=%q (subject org %q); "+
		"resource id=%s scope=%s (resource org %q); action=%s\n",
		org, enabledSubject.ID, enabledSubject.Kind, enabledSubject.Disabled, enabledSubject.Roles, org,
		docID, scope, org, action)
}

// printDecision prints all four reported fields: allowed or not, the
// reason, every matched policy id (deduplicated and sorted ascending), and
// the published version actually evaluated.
func printDecision(d darksafe.Decision) {
	fmt.Printf("  decision: allowed=%v reason=%q matched=%q version=%d\n",
		d.Allowed, d.Reason, d.Matched, d.Version)
}
