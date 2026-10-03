// This file holds the shared core of single-decision review. The online
// Store.RecheckDecision and the store-free RecheckDecisionOffline acquire
// their material differently — one reads an organization's live history
// and version snapshots, the other a chain-validated export paired with its
// separately retained checkpoint — but once the recorded request and the
// version it actually used are in hand, both replay the exact same
// judgment. That judgment lives only here so the two entry points can never
// drift on version-zero handling, envelope rejection order, or how a
// historical policy set is evaluated.
package darksafe

// replayStatus reports whether replayRecordedDecision produced a decision
// or why the declared version could not be replayed.
type replayStatus int

const (
	// replayDecided means a Decision was recomputed.
	replayDecided replayStatus = iota
	// replayVersionAbsent means content for the declared version exists
	// nowhere in the reviewer's material.
	replayVersionAbsent
	// replayVersionAfterTarget means the version's content exists, but only
	// in a policy change record positioned after the reviewed decision.
	// Online material never yields this: live version snapshots carry no
	// position relative to audit records.
	replayVersionAfterTarget
)

// versionPolicies is one resolver verdict: either the detached full policy
// set of the requested version, or a status explaining why it is unusable.
type versionPolicies struct {
	policies []Policy
	status   replayStatus
}

// resolveVersionPolicies supplies the full content of a non-zero version
// named by a reviewed decision. Implementations are responsible for
// detaching the returned policies from any state or input material.
type resolveVersionPolicies func(version int) versionPolicies

// replayRecordedDecision re-judges a recorded request against the version
// the record declares it actually used. Later publishes or rollbacks are
// never substituted for that version.
//
// A version-0 record first replays the envelope rejections in their fixed
// order (missing organizations and identifiers, empty action, organization
// mismatch, illegal scope, disabled subject); a request that passes is the
// "organization has no published version" denial.
//
// For a non-zero record the version is resolved before any request rule is
// applied: a missing, or only-later, version is an error the caller turns
// into ErrVersionNotFound and must never be masked by an envelope denial,
// even for a disabled subject. Once the version is in hand, the same
// envelope checks run before policy evaluation, so a rejection reports
// version 0 with no matched policies regardless of what the version
// contains. Evaluation itself is the historical policy set only: deny
// overrides allow, matched identifiers are sorted, resource restrictions
// narrow rather than replace scope matching, and subject roles never stand
// in for an organization policy.
func replayRecordedDecision(org string, req OrgRequest, version int, resolve resolveVersionPolicies) (Decision, replayStatus) {
	if version == 0 {
		// The original evaluation used no published version: replay the
		// envelope rejections exactly, in their recorded order, then the
		// not-yet-published denial.
		if d, ok := checkRequest(org, req); !ok {
			return d, replayDecided
		}
		return Decision{Allowed: false, Reason: "organization has no published version"}, replayDecided
	}
	// Resolve the version first. A request the envelope would reject must
	// not turn a missing or later-only declared version into an ordinary
	// denial.
	resolved := resolve(version)
	if resolved.status != replayDecided {
		return Decision{}, resolved.status
	}
	// The version exists as claimed, but chain integrity does not imply the
	// request was admissible: re-run the same envelope checks the online
	// decision runs before any policy is consulted. A rejection here
	// evaluates no policies, hence version 0 and no matches, even when the
	// version holds a fully matching allow policy.
	if d, ok := checkRequest(org, req); !ok {
		return d, replayDecided
	}
	return evaluate(req, resolved.policies, version), replayDecided
}
