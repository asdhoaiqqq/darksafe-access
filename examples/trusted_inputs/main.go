// Command trusted_inputs_example shows where the fields of an
// organization-level request must come from. Store.Decide authenticates
// nothing: it judges exactly the OrgRequest it is handed. Subject identity,
// organization membership, the disabled flag and resource ownership are
// plain strings and booleans in the packet, and there is no subject or
// resource registry behind them that the engine could cross-check. The
// integrating business system therefore confirms those facts at its own
// boundary first and builds the request from the confirmed snapshot; the
// requester contributes only the intent (which resource, which action).
//
// Run with:
//
//	go run ./examples/trusted_inputs
//
// The program takes no arguments, writes no files, contacts no services and
// works fully offline. The "confirmed" directory and registry values are
// ordinary Go values standing in for the stores a real deployment already
// runs; authenticating the login is assumed to have happened upstream, it is
// not a feature of this package.
//
// Around one subject reading one ledger it shows:
//
//  1. Submitted intent and confirmed facts agree, subject enabled: after a
//     matching allow policy is published, the read is allowed with the
//     policy id and the actual version.
//  2. The requester claims to still be enabled while the confirmed directory
//     says disabled: the request handed to Decide carries disabled, so the
//     envelope rejects with "subject is disabled" before any policy is
//     evaluated (version 0, empty matched list).
//  3. The requester names another organization's ledger as this
//     organization's: the request carries the confirmed owner, so the
//     envelope rejects with "organization mismatch" (version 0, empty
//     matched list). A read-only Review of the packet the claim would have
//     produced shows why self-reported fields cannot be trusted: an
//     internally consistent forged packet is allowed, because the engine has
//     no way to see through it.
//
// The tail exports the audit chain, verifies it as-is, and verifies it
// again after rewriting one saved record: chain verification proves the
// saved bytes were not altered afterwards; it cannot prove the identity
// claims submitted at decision time were authentic.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// decisionOrg is the tenant the business system is currently serving. It is
// chosen by the deploying system (routing/tenant context), never taken from
// the requester's payload.
const decisionOrg = "acme factory"

// readIntent is everything the requester gets to say: "I want to perform
// this action on the resource I name." Every field is an unverified claim
// until the business system checks it against a trusted source.
type readIntent struct {
	claimsSubjectID   string // who the requester says they are
	claimsSubjectOrg  string // where they claim their account lives
	claimsEnabled     bool   // whether they claim their account is active
	namesResourceID   string // the resource handle named in the request
	claimsResourceOrg string // who they claim owns that resource
	action            string // the intended action
}

// confirmedFacts is the snapshot the integrating business system has already
// confirmed at its own boundary, before any decision call:
//
//   - authenticatedSubjectID comes from the session established by the
//     business system's own login flow. That authentication is a precondition
//     of this example, not a feature of the darksafe package;
//   - currentSubjectOrg and subjectDisabled are read live from the system's
//     directory of record;
//   - resourceOwnerOrg and resourceScope are read live from confirmed
//     resource ownership data.
//
// Nothing here is fetched from an external service by this program: the
// values are inputs at the integration boundary, exactly as a real service
// would hand them in from stores it already operates.
type confirmedFacts struct {
	authenticatedSubjectID string
	currentSubjectOrg      string
	subjectDisabled        bool
	resourceOwnerOrg       string
	resourceScope          string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "trusted inputs example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	const (
		subjectID   = "svc-audit-reader"
		ledgerID    = "ledger-2026"
		ledgerScope = "acme/factory/ledger"
		otherOrg    = "globex"
		otherScope  = "globex/factory/ledger"
	)

	fmt.Println("integration scenario: one authenticated subject requests one ledger read")
	fmt.Println("trusted boundary (established by the integrating business system, not by the engine):")
	fmt.Printf("  served decision organization : %q (tenant context, not requester supplied)\n", decisionOrg)
	fmt.Printf("  authenticated identity       : %q (login session confirmed upstream)\n", subjectID)
	fmt.Println("  directory of record          : current subject organization and disabled state")
	fmt.Println("  resource registry            : current owner organization and scope")
	fmt.Println("  Store.Decide checks none of these itself: it judges the OrgRequest it receives.")
	fmt.Println()

	store := darksafe.NewStore()

	// Publish one allow policy matching this subject, action and scope. It
	// becomes version 1 for the decision organization.
	version, err := store.Publish(decisionOrg, 0, []darksafe.Policy{{
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
	fmt.Printf("  (subject=%q action=%q scope=%q effect=%s)\n\n", subjectID, "read", ledgerScope, darksafe.EffectAllow)

	// Case 1: intent and confirmed facts agree and the account is enabled.
	enabledFacts := confirmedFacts{
		authenticatedSubjectID: subjectID,
		currentSubjectOrg:      decisionOrg,
		subjectDisabled:        false,
		resourceOwnerOrg:       decisionOrg,
		resourceScope:          ledgerScope,
	}
	intent := readIntent{
		claimsSubjectID:   subjectID,
		claimsSubjectOrg:  decisionOrg,
		claimsEnabled:     true,
		namesResourceID:   ledgerID,
		claimsResourceOrg: decisionOrg,
		action:            "read",
	}
	fmt.Println("case 1: requester intent agrees with confirmed facts, subject enabled")
	printIntent(intent)
	printFacts(ledgerID, enabledFacts)
	req1, notes := buildRequest(intent, enabledFacts)
	printNotes(notes)
	printRequest(req1)
	printVerdict("decide", store.Decide(decisionOrg, req1))
	fmt.Println()

	// Case 2: the requester still claims to be enabled, but the directory of
	// record has since disabled the account.
	disabledFacts := enabledFacts
	disabledFacts.subjectDisabled = true
	fmt.Println("case 2: requester claims enabled=true; trusted directory says disabled")
	printIntent(intent)
	printFacts(ledgerID, disabledFacts)
	req2, notes := buildRequest(intent, disabledFacts)
	printNotes(notes)
	printRequest(req2)
	printVerdict("decide", store.Decide(decisionOrg, req2))
	fmt.Println()

	// Case 3: the requester names a ledger that actually belongs to another
	// organization, claiming it belongs to the served organization. The
	// authenticated subject is still an enabled member of the served org.
	globexFacts := enabledFacts
	globexFacts.resourceOwnerOrg = otherOrg
	globexFacts.resourceScope = otherScope
	fmt.Println("case 3: requester claims another organization's ledger belongs to this organization")
	printIntent(intent)
	printFacts(ledgerID, globexFacts)
	req3, notes := buildRequest(intent, globexFacts)
	printNotes(notes)
	printRequest(req3)
	printVerdict("decide", store.Decide(decisionOrg, req3))

	// Read-only contrast, never submitted through Decide (so no audit record
	// is created): the packet the requester's claims alone would have built.
	// It is internally consistent — all three organizations are "acme
	// factory", the subject enabled — so the engine cannot see anything wrong
	// with it and allows the read against version 1. The engine's only view of
	// ownership is the string the caller put in the packet.
	forgedClaim := darksafe.OrgRequest{
		SubjectOrg:  decisionOrg,
		ResourceOrg: decisionOrg,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "service"},
		Resource:    darksafe.Resource{ID: ledgerID, Scope: ledgerScope},
		Action:      "read",
	}
	fmt.Println("  contrast (read-only Review of the packet claims alone would build; never sent to Decide):")
	printVerdict("review", store.Review(decisionOrg, 1, forgedClaim))
	fmt.Println("  the engine sees three identical organization strings and an enabled subject; it has no")
	fmt.Println("  registry telling it the ledger actually belongs to globex, so it allows the packet.")
	fmt.Println()

	// Audit tail: the chain stores exactly the submitted request and returned
	// decision — including the trusted disabled flag and the globex owner.
	records, cp, err := store.AuditExport(decisionOrg, 0)
	if err != nil {
		return fmt.Errorf("export audit chain: %w", err)
	}
	if len(records) != 4 {
		return fmt.Errorf("expected 4 audit records (1 publish + 3 decisions), got %d", len(records))
	}
	fmt.Printf("audit chain of %q saved by the three Decide calls (%d records):\n", decisionOrg, len(records))
	for _, r := range records {
		switch r.Kind {
		case darksafe.AuditPolicyChange:
			fmt.Printf("  seq=%d policy_change version=%d\n", r.Seq, r.Change.Version)
		case darksafe.AuditDecision:
			printAuditDecision(r)
		}
	}
	if err := darksafe.VerifyAudit(decisionOrg, records, cp); err != nil {
		return fmt.Errorf("verify faithful export: %w", err)
	}
	fmt.Printf("  VerifyAudit(faithful export, checkpoint end=%d): ok\n", cp.EndSeq)

	// Rewriting history changes the bytes, so chain verification fails.
	tampered := copyRecords(records)
	tampered[2].Decision.Request.Subject.Disabled = false // seq 3 is the disabled-subject denial
	tamperErr := darksafe.VerifyAudit(decisionOrg, tampered, cp)
	fmt.Printf("  VerifyAudit(after rewriting seq 3 saved request disabled=true -> false): %v\n", tamperErr)
	fmt.Println("  verification proves the saved request and decision were not altered afterwards;")
	fmt.Println("  it cannot prove the requester really was the subject named, or really enabled —")
	fmt.Println("  authenticity comes only from the trusted sources the request was built from.")
	return nil
}

// buildRequest assembles the OrgRequest the engine evaluates. The action and
// the named resource handle come from the requester's intent; the subject
// identity, both organizations, the disabled flag and the resource scope
// come exclusively from the confirmed snapshot. Conflicting claims are
// reported but never substituted in: a claim is not a fact.
func buildRequest(intent readIntent, facts confirmedFacts) (darksafe.OrgRequest, []string) {
	var notes []string
	if intent.claimsSubjectID != facts.authenticatedSubjectID {
		notes = append(notes, fmt.Sprintf(
			"conflict: claimed subject id %q != authenticated identity %q; using the authenticated identity",
			intent.claimsSubjectID, facts.authenticatedSubjectID))
	}
	if intent.claimsSubjectOrg != facts.currentSubjectOrg {
		notes = append(notes, fmt.Sprintf(
			"conflict: claimed subject org %q != confirmed org %q; using the confirmed org",
			intent.claimsSubjectOrg, facts.currentSubjectOrg))
	}
	if intent.claimsEnabled == facts.subjectDisabled {
		notes = append(notes, fmt.Sprintf(
			"conflict: requester claimed enabled=%v; directory says disabled=%v; submitting the confirmed state",
			intent.claimsEnabled, facts.subjectDisabled))
	}
	if intent.claimsResourceOrg != facts.resourceOwnerOrg {
		notes = append(notes, fmt.Sprintf(
			"conflict: claimed resource owner %q != registry owner %q; submitting the confirmed ownership",
			intent.claimsResourceOrg, facts.resourceOwnerOrg))
	}
	return darksafe.OrgRequest{
		SubjectOrg:  facts.currentSubjectOrg,
		ResourceOrg: facts.resourceOwnerOrg,
		Subject: darksafe.Subject{
			ID:       facts.authenticatedSubjectID,
			Kind:     "service",
			Disabled: facts.subjectDisabled,
		},
		Resource: darksafe.Resource{
			ID:    intent.namesResourceID,
			Scope: facts.resourceScope,
		},
		Action: intent.action,
	}, notes
}

func printIntent(in readIntent) {
	fmt.Printf("  requester intent : subject=%q subject-org=%q enabled=%v resource=%q resource-org=%q action=%q\n",
		in.claimsSubjectID, in.claimsSubjectOrg, in.claimsEnabled,
		in.namesResourceID, in.claimsResourceOrg, in.action)
}

func printFacts(resourceID string, f confirmedFacts) {
	fmt.Printf("  confirmed facts  : subject=%q subject-org=%q disabled=%v resource=%q owner-org=%q scope=%q\n",
		f.authenticatedSubjectID, f.currentSubjectOrg, f.subjectDisabled,
		resourceID, f.resourceOwnerOrg, f.resourceScope)
}

func printNotes(notes []string) {
	for _, n := range notes {
		fmt.Printf("  %s\n", n)
	}
}

func printRequest(req darksafe.OrgRequest) {
	fmt.Printf("  request submitted: decision-org=%q subject-org=%q resource-org=%q subject=%q kind=%q disabled=%v resource=%q scope=%q action=%q\n",
		decisionOrg, req.SubjectOrg, req.ResourceOrg,
		req.Subject.ID, req.Subject.Kind, req.Subject.Disabled,
		req.Resource.ID, req.Resource.Scope, req.Action)
}

func printVerdict(label string, d darksafe.Decision) {
	fmt.Printf("  %-6s: allowed=%v reason=%q matched=%q version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}

func printAuditDecision(r darksafe.AuditRecord) {
	req := r.Decision.Request
	d := r.Decision.Decision
	fmt.Printf("  seq=%d decision subject=%q subject-org=%q disabled=%v resource=%q resource-org=%q scope=%q -> allowed=%v reason=%q matched=%q version=%d\n",
		r.Seq, req.Subject.ID, req.SubjectOrg, req.Subject.Disabled,
		req.Resource.ID, req.ResourceOrg, req.Resource.Scope,
		d.Allowed, d.Reason, d.Matched, d.Version)
}

// copyRecords deep-copies an export so the copy can be tampered without
// touching the original records' shared payload pointers.
func copyRecords(in []darksafe.AuditRecord) []darksafe.AuditRecord {
	out := make([]darksafe.AuditRecord, len(in))
	for i, r := range in {
		c := r
		if r.Change != nil {
			cc := *r.Change
			c.Change = &cc
		}
		if r.Decision != nil {
			dc := *r.Decision
			c.Decision = &dc
		}
		out[i] = c
	}
	return out
}
