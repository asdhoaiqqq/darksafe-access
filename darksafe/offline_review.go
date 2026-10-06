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
//
// A review that already holds the material as decoded records
// (RecheckDecisionOffline) and one that starts from an archive file
// (DecodeAuditArchive followed by a review) historically each ran the full
// VerifyAudit chain check on their own, so `darksafe review` validated the
// same material twice per invocation. The two stages now share one pass:
// VerifyAudit runs exactly once per review, its result is carried in an
// unexported VerifiedAuditMaterial, and the review replays the target from
// that already-verified material without re-checking the chain. The
// standalone entry points keep validating their own inputs, so neither
// requires the caller to have run another entry point first.
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

// VerifiedAuditMaterial is an export that has passed the complete
// VerifyAudit chain check against the checkpoint it carries. It is the
// boundary between one validation and the review: an entry point that has
// just validated the material hands it to RecheckVerifiedDecisionOffline,
// and the review replays the target without validating the chain a second
// time.
//
// It is intentionally unexported: it can only be produced by this package's
// own validating entry points (verifyAuditMaterial, reached from
// RecheckDecisionOffline, DecodeAuditArchive and DecodeVerifiedAuditArchive),
// never from a caller's assertion. It carries no memo that survives the
// call that produced it, so modified material or a different checkpoint
// always forces a fresh validation; the embedded records are the caller's
// and stay owned by the caller.
type VerifiedAuditMaterial struct {
	org     string
	records []AuditRecord
	cp      Checkpoint
}

// verifyAuditMaterial validates the whole export exactly as VerifyAudit
// does and, on success, returns it as VerifiedAuditMaterial. It is the
// single place the chain check is performed on the offline review path; a
// failure returns no material, so a later stage can never review records
// that did not validate.
func verifyAuditMaterial(org string, records []AuditRecord, cp Checkpoint) (VerifiedAuditMaterial, error) {
	if verifyAuditObserver != nil {
		verifyAuditObserver()
	}
	if err := VerifyAudit(org, records, cp); err != nil {
		return VerifiedAuditMaterial{}, err
	}
	return VerifiedAuditMaterial{org: org, records: records, cp: cp}, nil
}

// RecheckVerifiedDecisionOffline re-evaluates one decision record from
// material a preceding stage has already chain-validated as one unit
// (verifyAuditMaterial or DecodeVerifiedAuditArchive). It does not run the
// chain check again: one review performs one complete chain validation, and
// tampering anywhere is rejected before this stage is reached. All target
// and replay guarantees are otherwise identical to
// RecheckDecisionOffline's: ErrAuditNotFound for a non-positive or absent
// target sequence, ErrAuditNotADecision when the target is a policy change
// record, and ErrVersionNotFound when the version the decision names has no
// change record before the target (including content that appears only
// after it); a newer version is never substituted. The call is read-only
// and the returned decisions are detached from the material's slices.
func RecheckVerifiedDecisionOffline(m VerifiedAuditMaterial, seq int) (OfflineDecisionReview, error) {
	return recheckVerifiedOffline(m, seq)
}

// recheckVerifiedOffline is the shared review body. It takes an unexported
// verified-material value so only this package's validating stages can
// reach the already-validated path; standalone RecheckDecisionOffline
// validates first and then calls it just as the combined archive flow does.
func recheckVerifiedOffline(m VerifiedAuditMaterial, seq int) (OfflineDecisionReview, error) {
	org, records := m.org, m.records
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
	// Detach the request list by non-nil rather than by length: a non-nil
	// empty role list with spare capacity must not be shared back with the
	// caller's material, even though evaluation never reads any element.
	req.Subject.Roles = cloneStrings(req.Subject.Roles)
	version := original.Version

	// The version's full content must travel in a same-organization policy
	// change record positioned before the decision. The replay core resolves
	// the version before replaying any request rule, so a request the
	// envelope rejects cannot mask a missing or later-only version.
	resolve := func(want int) versionPolicies {
		if policies, found := policiesBefore(records, seq-1, want); found {
			return versionPolicies{policies: policies}
		}
		if _, later := policiesBefore(records, len(records), want); later {
			return versionPolicies{status: replayVersionAfterTarget}
		}
		return versionPolicies{status: replayVersionAbsent}
	}
	recomputed, status := replayRecordedDecision(org, req, version, resolve)
	switch status {
	case replayVersionAfterTarget:
		return OfflineDecisionReview{}, fmt.Errorf(
			"%w: decision at sequence %d used version %d, whose change record appears only after it",
			ErrVersionNotFound, seq, version)
	case replayVersionAbsent:
		return OfflineDecisionReview{}, fmt.Errorf(
			"%w: decision at sequence %d used version %d, which has no policy change record in the export",
			ErrVersionNotFound, seq, version)
	}

	return OfflineDecisionReview{
		Seq:        seq,
		Original:   original,
		Recomputed: recomputed,
		Consistent: decisionsEqual(original, recomputed),
	}, nil
}

// RecheckDecisionOffline re-evaluates one decision record offline. org is
// the organization name, records is its complete export starting at
// sequence 1 (a valid prefix is sufficient when paired with its own
// checkpoint), cp is the separately saved checkpoint, and seq identifies
// the decision record to review. The call is read-only: it neither mutates
// the supplied material nor appends audit records, and the returned
// decisions are detached from the input slices.
//
// The entire export is chain-validated in this call before any result is
// produced; tampering anywhere, including in records after the target,
// fails the review. The caller does not need to have validated the
// material through another entry point first: standalone callers receive
// the exact same guarantees DecodeAuditArchive gives, and validation is
// always based on this call's material, organization and checkpoint — a
// previous validation is never reused after the material changes or the
// checkpoint changes. Distinguishable errors follow the other audit entry
// points: ErrMissingOrganization for an empty organization, ErrAuditNotFound
// for a non-positive or absent target sequence, and ErrAuditNotADecision
// when the target is a policy change record. When the saved decision names
// a policy version whose full content has no change record before the
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
	m, err := verifyAuditMaterial(org, records, cp)
	if err != nil {
		return OfflineDecisionReview{}, err
	}
	return recheckVerifiedOffline(m, seq)
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
// slice. The list is detached whenever it is non-nil, including a non-nil
// empty list that still has spare capacity, so an append by one receiver
// can never reach another copy.
func cloneDecisionValue(d Decision) Decision {
	d.Matched = cloneStrings(d.Matched)
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

// verifyAuditObserver, when non-nil, is invoked once for every complete
// VerifyAudit chain validation performed by this package. It exists so
// tests can pin the review flow to a single chain pass per invocation:
// decode and review must share one verification, never validate the same
// material twice. It is a test seam, not production behavior — it is nil in
// normal use and observes nothing about validation's result.
var verifyAuditObserver func()

// SetVerifyAuditObserverForTesting installs observe as the chain-validation
// observer: it is invoked once for every complete VerifyAudit pass the
// package performs. The returned function restores the previous observer
// (the normal nil observer). It exists only so tests can pin the offline
// review flow to one chain validation per command invocation; it has no
// effect on validation itself or on any result.
func SetVerifyAuditObserverForTesting(observe func()) (restore func()) {
	prev := verifyAuditObserver
	verifyAuditObserver = observe
	return func() { verifyAuditObserver = prev }
}
