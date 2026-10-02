// This file implements offline review of a single audited decision. It
// works solely from exported audit material: the complete record export,
// the separately retained checkpoint, and a target sequence. No Store is
// created or restored, so an organization can confirm why an access was
// allowed or denied long after the service instance that decided it has
// ended.
//
// The complete input must first validate as an audit chain. The policy
// version a decision actually used is recovered exclusively from policy
// change records preceding that decision within the same organization's
// export; later publishes or rollbacks, and a wider later export, cannot
// change the recomputed result.
package darksafe

import "fmt"

// OfflineDecisionReview compares the decision stored in an audited record
// with a decision recomputed purely from the exported material.
type OfflineDecisionReview struct {
	// Seq is the reviewed decision record's sequence.
	Seq int
	// Original is the decision preserved in the audited record.
	Original Decision
	// Recomputed is the decision evaluated again offline from the saved
	// request and the policy version it originally used.
	Recomputed Decision
	// Consistent reports whether both decisions agree on every field
	// (allowance, reason, matched policies and version), treating a nil
	// matched list and an empty matched list as the same.
	Consistent bool
}

// RecheckDecisionOffline re-evaluates one decision record offline. org is
// the organization name, records is its complete export starting at
// sequence 1 (a valid prefix is sufficient when paired with its own
// checkpoint), cp is the separately saved checkpoint, and seq identifies
// the decision record to review. The call is read-only: it neither mutates
// the supplied material nor appends audit records, and the returned
// decisions are detached from the input slices.
//
// The entire export is chain-validated before any result is produced;
// tampering anywhere, including in records after the target, fails the
// review. Distinguishable errors follow the other audit entry points:
// ErrMissingOrganization for an empty organization, ErrAuditNotFound for a
// non-positive or absent target sequence, and ErrAuditNotADecision when the
// target is a policy change record. When the saved decision names a
// policy version whose full content has no change record before the
// target, ErrVersionNotFound is returned; a newer version is never
// substituted, and a request the envelope would reject never masks the
// missing version.
//
// The fingerprint only proves the material matches its checkpoint; it
// cannot stand in for evaluating the request itself. So even when the
// declared version exists and carries an otherwise matching allow policy,
// the recomputation enforces the same request rules as the online
// decision: a disabled subject, an organization mismatch, a missing
// subject or resource organization or identifier, an empty action, and an
// illegal resource scope all deny, with the same reasons and precedence as
// Decide. Such a rejection uses no policies, so the recomputed decision
// reports version 0 with no matched policies; it is then compared field by
// field with the preserved original and may come back inconsistent.
func RecheckDecisionOffline(org string, records []AuditRecord, cp Checkpoint, seq int) (OfflineDecisionReview, error) {
	if org == "" {
		return OfflineDecisionReview{}, ErrMissingOrganization
	}
	// Validate the whole export first. A subject-filtered page, a fragment
	// missing the beginning, or a checkpoint that does not pin the records
	// all fail here, as does any corruption after the target.
	if err := VerifyAudit(org, records, cp); err != nil {
		return OfflineDecisionReview{}, err
	}
	if seq < 1 {
		return OfflineDecisionReview{}, fmt.Errorf("%w: sequence %d", ErrAuditNotFound, seq)
	}
	if seq > len(records) {
		return OfflineDecisionReview{}, fmt.Errorf("%w: export ends at %d, no sequence %d", ErrAuditNotFound, len(records), seq)
	}
	rec := &records[seq-1]
	if rec.Kind != AuditDecision || rec.Decision == nil {
		return OfflineDecisionReview{}, fmt.Errorf("%w: sequence %d is %s", ErrAuditNotADecision, seq, rec.Kind)
	}

	original := cloneDecisionValue(rec.Decision.Decision)
	req := rec.Decision.Request
	if len(req.Subject.Roles) > 0 {
		req.Subject.Roles = append([]string(nil), req.Subject.Roles...)
	}
	version := original.Version

	var recomputed Decision
	if version == 0 {
		// The original evaluation used no published version: replay the
		// envelope rejections exactly, then the not-yet-published denial.
		if d, ok := checkRequest(org, req); !ok {
			recomputed = d
		} else {
			recomputed = Decision{Allowed: false, Reason: "organization has no published version"}
		}
	} else {
		// The version's full content must travel in a same-organization
		// policy change record positioned before the decision. That check
		// comes first: a request the envelope rejects must not mask a
		// missing version with an ordinary denial result.
		policies, found := policiesBefore(records, seq-1, version)
		if !found {
			if _, later := policiesBefore(records, len(records), version); later {
				return OfflineDecisionReview{}, fmt.Errorf(
					"%w: decision at sequence %d used version %d, whose change record appears only after it",
					ErrVersionNotFound, seq, version)
			}
			return OfflineDecisionReview{}, fmt.Errorf(
				"%w: decision at sequence %d used version %d, which has no policy change record in the export",
				ErrVersionNotFound, seq, version)
		}
		// The version exists as claimed, but chain integrity does not imply
		// the request was admissible: re-run the same envelope checks the
		// online decision runs before any policy is consulted. A rejection
		// here evaluates no policies, hence version 0 and no matches, even
		// when the version holds a fully matching allow policy.
		if d, ok := checkRequest(org, req); !ok {
			recomputed = d
		} else {
			recomputed = evaluate(req, policies, version)
		}
	}

	return OfflineDecisionReview{
		Seq:        seq,
		Original:   original,
		Recomputed: recomputed,
		Consistent: decisionsEqual(original, recomputed),
	}, nil
}

// policiesBefore searches records[:limit] for the policy change record that
// published version and returns a detached copy of its full content. The
// gapless chain validation guarantees records are in sequence order.
func policiesBefore(records []AuditRecord, limit, version int) ([]Policy, bool) {
	for i := 0; i < limit; i++ {
		r := &records[i]
		if r.Kind != AuditPolicyChange || r.Change == nil {
			continue
		}
		if r.Change.Version == version {
			return append([]Policy(nil), r.Change.Policies...), true
		}
	}
	return nil, false
}

// cloneDecisionValue returns a decision detached from the input's matched
// slice.
func cloneDecisionValue(d Decision) Decision {
	if len(d.Matched) > 0 {
		d.Matched = append([]string(nil), d.Matched...)
	}
	return d
}

// decisionsEqual compares every decision field. A nil matched list and an
// empty matched list describe the same set of hits.
func decisionsEqual(a, b Decision) bool {
	if a.Allowed != b.Allowed || a.Reason != b.Reason || a.Version != b.Version {
		return false
	}
	if len(a.Matched) == 0 && len(b.Matched) == 0 {
		return true
	}
	if len(a.Matched) != len(b.Matched) {
		return false
	}
	for i := range a.Matched {
		if a.Matched[i] != b.Matched[i] {
			return false
		}
	}
	return true
}
