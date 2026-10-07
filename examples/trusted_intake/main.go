// Command trusted_intake_example shows how a surrounding business system
// must assemble an organization-level request for Store.Decide: the
// requester's stated intent ("I want to read this, as this identity") is
// kept separate from the facts that business system has already confirmed
// out of band — the authenticated subject id, the subject's current
// organization and disabled state, and the resource's owning organization
// and scope.
//
// Run with:
//
//	go run ./examples/trusted_intake
//
// The program takes no arguments, writes no files, and works fully offline.
//
// Store.Decide judges only the OrgRequest contents it is given. It performs
// no login or authentication and keeps no subject or resource registry to
// check the request's claims against: when three organization strings in a
// request are equal, that only means the submitted envelope is internally
// consistent — it cannot prove the requester belongs to that organization.
// Identity confirmation is therefore an already-satisfied precondition of
// this example: the two "confirmed profile" values stand in for facts the
// surrounding system resolved before this code runs (its identity lifecycle
// and its resource inventory). They are fixed, in-memory facts — not a
// platform feature, and no external service is contacted.
//
// One authenticated subject is involved in three read attempts:
//
//  1. Confirmed facts agree (enabled subject, resource owned by the decision
//     organization). After a matching allow policy is published, the read is
//     allowed with its reason, matched policy id and the version actually
//     used.
//  2. The requester claims to still be enabled while the confirmed subject
//     profile says disabled. The envelope is built from the confirmed
//     (disabled) facts, so Decide rejects with "subject is disabled",
//     version 0 and an empty matched list — no policy is evaluated.
//  3. The requester presents another organization's ledger as if it belonged
//     to the decision organization. The envelope carries the resource's
//     confirmed owning organization, so Decide rejects with
//     "organization mismatch", again version 0 with no matches.
//
// The final section exports the audit chain: it records exactly the requests
// that were submitted (including the disabled and mismatched envelopes), and
// VerifyAudit confirms the chain bytes are intact. Neither fact authenticates
// the requester — that guarantee comes solely from the surrounding system
// that confirmed the profiles, which is outside this package.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "trusted intake example: failed:", err)
		os.Exit(1)
	}
}

// readIntent is everything the requester gets to say: which subject they
// claim to be, in which organization, whether they claim to be enabled,
// which resource they claim to target and under which owning organization,
// and what they want to do. Every field here is untrusted input. The
// envelope builder below never reads it as fact — it is printed only so the
// contrast with the confirmed profiles is visible.
type readIntent struct {
	ClaimedSubjectID   string
	ClaimedSubjectOrg  string
	ClaimsEnabled      bool
	ClaimedResourceID  string
	ClaimedResourceOrg string
	Action             string
}

// confirmedSubject is the subject profile the surrounding system has already
// resolved from the authenticated session against its identity lifecycle:
// the canonical subject id, the organization that subject currently belongs
// to, and the current disabled flag. In a deployed system these values come
// from that system's authoritative store; here they are fixed in-memory
// facts so the example runs offline.
type confirmedSubject struct {
	ID       string
	Org      string
	Disabled bool
}

// confirmedResource is the resource profile the surrounding system resolved
// from its resource inventory: the canonical id, the organization that owns
// the resource, and the scope the resource is classified under.
type confirmedResource struct {
	ID    string
	Org   string
	Scope string
}

// orgRequestFromConfirmedFacts builds the OrgRequest Decide evaluates. The
// action is the requester's own intent (the caller chooses what to attempt;
// published policies decide whether it is allowed), but every FACT in the
// envelope — subject id, subject organization, disabled flag, resource id,
// resource organization and scope — comes exclusively from the confirmed
// profiles. The readIntent is not even a parameter: a claimed subject id,
// organization or enabled state can never leak into a decision as an
// unverified fact, and a conflict is resolved by keeping the claim out of
// the envelope rather than overruling the profile.
func orgRequestFromConfirmedFacts(subject confirmedSubject, resource confirmedResource, action string) darksafe.OrgRequest {
	return darksafe.OrgRequest{
		SubjectOrg:  subject.Org,
		ResourceOrg: resource.Org,
		Subject: darksafe.Subject{
			ID:       subject.ID,
			Kind:     "service",
			Disabled: subject.Disabled,
		},
		Resource: darksafe.Resource{
			ID:    resource.ID,
			Scope: resource.Scope,
		},
		Action: action,
	}
}

func run() error {
	const (
		decisionOrg = "acme factory"
		foreignOrg  = "globex holdings"

		homeLedgerID    = "ledger-2026"
		homeLedgerScope = "acme/factory/ledger"

		foreignLedgerID    = "globex-ledger-9"
		foreignLedgerScope = "globex/holdings/ledger"

		subjectID = "svc-audit-reader"
	)

	store := darksafe.NewStore()

	fmt.Println("trusted intake example: who confirms the facts behind an organization-level request")
	fmt.Printf("decision organization: %q\n", decisionOrg)
	fmt.Println("(claims below are requester input; the decision envelope is built only from confirmed profiles)")
	fmt.Println()

	// Attempt 1: confirmed facts agree — an enabled subject from the decision
	// organization reads a resource the same organization owns. Publish one
	// matching allow policy, then decide from the confirmed envelope.
	fmt.Println("attempt 1: confirmed facts agree — enabled home-org subject reads the home-org ledger")
	enabledSubject := confirmedSubject{ID: subjectID, Org: decisionOrg, Disabled: false}
	homeLedger := confirmedResource{ID: homeLedgerID, Org: decisionOrg, Scope: homeLedgerScope}
	printIntent(readIntent{
		ClaimedSubjectID:   subjectID,
		ClaimedSubjectOrg:  decisionOrg,
		ClaimsEnabled:      true,
		ClaimedResourceID:  homeLedgerID,
		ClaimedResourceOrg: decisionOrg,
		Action:             "read",
	})
	printConfirmed(enabledSubject, homeLedger)
	version, err := store.Publish(decisionOrg, 0, []darksafe.Policy{{
		ID:      "p-ledger-read-2026",
		Subject: subjectID,
		Action:  "read",
		Scope:   homeLedgerScope,
		Effect:  darksafe.EffectAllow,
	}})
	if err != nil {
		return fmt.Errorf("publish allow policy: %w", err)
	}
	fmt.Printf("published allow policy %q as version %d\n", "p-ledger-read-2026", version)
	printDecision("  decide", store.Decide(decisionOrg,
		orgRequestFromConfirmedFacts(enabledSubject, homeLedger, "read")))
	fmt.Println()

	// Attempt 2: the same caller insists they are still enabled, but the
	// identity lifecycle the surrounding system consulted says the subject is
	// disabled. The claim never reaches Decide: the envelope carries the
	// confirmed Disabled flag, so the envelope check rejects before any policy
	// is consulted (version 0, no matches).
	fmt.Println("attempt 2: requester claims to still be enabled, but the confirmed subject profile is disabled")
	disabledSubject := confirmedSubject{ID: subjectID, Org: decisionOrg, Disabled: true}
	printIntent(readIntent{
		ClaimedSubjectID:   subjectID,
		ClaimedSubjectOrg:  decisionOrg,
		ClaimsEnabled:      true,
		ClaimedResourceID:  homeLedgerID,
		ClaimedResourceOrg: decisionOrg,
		Action:             "read",
	})
	printConfirmed(disabledSubject, homeLedger)
	fmt.Println("envelope built from confirmed facts only: disabled=true; the self-claimed enabled state is discarded")
	printDecision("  decide", store.Decide(decisionOrg,
		orgRequestFromConfirmedFacts(disabledSubject, homeLedger, "read")))
	fmt.Println()

	// Attempt 3: the caller names a ledger the resource inventory says
	// belongs to another organization while claiming it belongs here. The
	// envelope carries the confirmed owning organization, so the request
	// crosses organizations and is rejected at the envelope check — again no
	// policy is evaluated, regardless of what is published for the home
	// ledger.
	fmt.Println("attempt 3: requester presents another organization's ledger as a home-org resource")
	foreignLedger := confirmedResource{ID: foreignLedgerID, Org: foreignOrg, Scope: foreignLedgerScope}
	printIntent(readIntent{
		ClaimedSubjectID:   subjectID,
		ClaimedSubjectOrg:  decisionOrg,
		ClaimsEnabled:      true,
		ClaimedResourceID:  foreignLedgerID,
		ClaimedResourceOrg: decisionOrg, // the caller claims this resource is local
		Action:             "read",
	})
	printConfirmed(enabledSubject, foreignLedger)
	fmt.Printf("envelope built from confirmed facts only: resource-org=%q; the claimed ownership is discarded\n", foreignOrg)
	printDecision("  decide", store.Decide(decisionOrg,
		orgRequestFromConfirmedFacts(enabledSubject, foreignLedger, "read")))
	fmt.Println()

	// Audit boundary: every non-empty-org Decide (three here) and the publish
	// were recorded with the envelope that was ACTUALLY submitted — disabled
	// flag and foreign resource organization included. Chain verification
	// proves those recorded bytes were not altered, reordered or spliced; it
	// says nothing about who sent them.
	fmt.Println("audit boundary: the chain keeps the submitted envelopes; it does not authenticate them")
	records, cp, err := store.AuditExport(decisionOrg, 0)
	if err != nil {
		return fmt.Errorf("export audit chain: %w", err)
	}
	fmt.Printf("records=%d (1 policy change + 3 decisions)\n", len(records))
	for _, r := range records {
		printRecord(r)
	}
	if err := darksafe.VerifyAudit(decisionOrg, records, cp); err != nil {
		return fmt.Errorf("VerifyAudit on the fresh export: %w", err)
	}
	fmt.Println("VerifyAudit(exported chain): valid")
	fmt.Printf("verification proves the recorded bytes are intact and ordered; it cannot prove the caller really was %q\n", subjectID)
	return nil
}

func printIntent(in readIntent) {
	fmt.Printf("  requester claims : subject=%q subject-org=%q enabled=%v resource=%q resource-org=%q action=%q\n",
		in.ClaimedSubjectID, in.ClaimedSubjectOrg, in.ClaimsEnabled,
		in.ClaimedResourceID, in.ClaimedResourceOrg, in.Action)
}

func printConfirmed(subject confirmedSubject, resource confirmedResource) {
	fmt.Printf("  confirmed facts  : subject=%q subject-org=%q disabled=%v resource=%q resource-org=%q scope=%q\n",
		subject.ID, subject.Org, subject.Disabled, resource.ID, resource.Org, resource.Scope)
}

// printDecision prints the four fields every decision carries: allowed or
// not, the reason, the matched policy ids and the version actually used.
func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("%s: allowed=%v reason=%q matched=%q version=%d\n",
		label, d.Allowed, d.Reason, d.Matched, d.Version)
}

// printRecord prints what the audit chain preserved for one record, showing
// that decisions keep the exact submitted envelope (disabled flag, resource
// organization) alongside the verdict.
func printRecord(r darksafe.AuditRecord) {
	switch r.Kind {
	case darksafe.AuditPolicyChange:
		fmt.Printf("    seq=%d policy_change version=%d\n", r.Seq, r.Change.Version)
	case darksafe.AuditDecision:
		req := r.Decision.Request
		d := r.Decision.Decision
		fmt.Printf("    seq=%d decision: subject=%q disabled=%v resource-org=%q allowed=%v reason=%q matched=%q version=%d\n",
			r.Seq, req.Subject.ID, req.Subject.Disabled, req.ResourceOrg,
			d.Allowed, d.Reason, d.Matched, d.Version)
	}
}
