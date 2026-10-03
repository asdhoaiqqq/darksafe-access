// This file holds the single replay rule shared by online and offline
// review of one audited decision. Both entry points re-judge the recorded
// request against exactly the policy version the decision actually used:
// later publishes or rollbacks never substitute for it, and the business
// judgment (version-zero handling, envelope precedence, historical-policy
// evaluation) lives only here. The two entry points differ solely in where
// their material comes from: the live store's versions versus a validated
// export, a distinction expressed through the loadHistoricalPolicies hook.
package darksafe

// noPublishedVersionReason is the denial given when a decision evaluated
// no published version but its request envelope was admissible.
const noPublishedVersionReason = "organization has no published version"

// replayDecision recomputes one recorded decision for org from the saved
// request and the version it actually used.
//
// version == 0 replays exactly what Decide does before any version exists:
// the envelope checks first (in checkRequest's fixed reason order), then
// the "organization has not yet published a policy" denial for an
// admissible request. No historical material is consulted.
//
// For a non-zero version the full content of that version is loaded first,
// so a version absent from the available material fails through
// loadHistoricalPolicies even when the request would also be rejected by
// the envelope; an envelope denial must never mask a missing version. Only
// once the version is secured do the envelope checks run, and a rejection
// there evaluates no policies (version 0, no matches); an admissible
// request is judged from the loaded snapshot alone - subject-carried roles
// never authorize, deny overrides allow, matched ids stay sorted, and
// resource restrictions narrow rather than replace scope matching.
//
// loadHistoricalPolicies must return a detached copy of the policies of
// version, or an error (typically wrapping ErrVersionNotFound) when the
// available material does not contain that version's full content.
func replayDecision(org string, req OrgRequest, version int, loadHistoricalPolicies func(int) ([]Policy, error)) (Decision, error) {
	if version == 0 {
		if d, ok := checkRequest(org, req); !ok {
			return d, nil
		}
		return Decision{Allowed: false, Reason: noPublishedVersionReason}, nil
	}
	policies, err := loadHistoricalPolicies(version)
	if err != nil {
		return Decision{}, err
	}
	if d, ok := checkRequest(org, req); !ok {
		return d, nil
	}
	return evaluate(req, policies, version), nil
}
