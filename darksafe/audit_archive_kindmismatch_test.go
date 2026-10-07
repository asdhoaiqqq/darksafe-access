// This file is the read-side regression for the audit category/content
// rule: a record's category must always correspond to the payload it
// actually carries. A policy-change record must carry exactly one change
// payload (and no decision), and a decision record exactly one decision
// payload (and no change); any other public category does not exist.
//
// That rule already has encode-time coverage (EncodeAuditArchive runs
// VerifyAudit before it returns bytes), but an archive is read back by a
// different program at a different time, so the read entry points must hold
// the rule on their own. Every archive below therefore comes in with
// perfect framing: the declared payload length and the checksum are
// correct, the embedded organization and checkpoint match the ones the
// caller independently retained, sequences are gapless, predecessor and
// own fingerprints all chain, and every field decodes completely. The only
// thing wrong is one record's category/content combination.
//
// Such a file is readable as a format but invalid as audit content, so
// reading the whole archive must fail with ErrInvalidRange — never
// ErrInvalidArchive — and must deliver no records and no verified
// material, even when every other record, including a normal decision
// before the bad one, is intact. The same intact prefix read on its own
// under its own checkpoint must still succeed.
package darksafe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// kindMismatchOrg is the organization the category/content fixtures use.
const kindMismatchOrg = "acme"

// handArchiveForTesting serializes records into one complete archive frame
// without going through EncodeAuditArchive. It writes exactly the bytes the
// encoder would (the payload header and the outer record fields both walk
// the same single record field table) and frames them with a consistent
// declared length and checksum, so records that EncodeAuditArchive is right
// to refuse can still reach a reader that has to refuse them too. The
// embedded checkpoint is the one the caller also retains.
func handArchiveForTesting(t *testing.T, org string, cp Checkpoint, recs []AuditRecord) []byte {
	t.Helper()
	w := &archiveWriter{}
	w.stringField(org)
	w.u32(cp.EndSeq)
	w.stringField(cp.Fingerprint)
	w.u32(len(recs))
	for i := range recs {
		rec := &recs[i]
		for j := range recordFields {
			f := &recordFields[j]
			switch f.kind {
			case recordFieldString:
				w.stringField(f.getString(rec))
			case recordFieldInt:
				w.u32(f.getInt(rec))
			case recordFieldChange:
				w.change(f.getChange(rec))
			case recordFieldDecision:
				w.decisionRecord(f.getDecision(rec))
			}
		}
	}
	if w.err != nil {
		t.Fatalf("build archive payload: %v", w.err)
	}
	return framePayload(w.buf)
}

// kindMismatchBaseChain builds two fully valid records: a normal decision
// (a version-0 default denial) followed by a policy change with a nil
// policy set. Mismatch cases below append a third record, so the invalid
// record always sits after records that would be deliverable on their own.
func kindMismatchBaseChain(org string) []AuditRecord {
	first := syntheticRecord(org, 1, AuditDecision, nil, &DecisionRecord{
		Request:  request(org, "u1", "r1", "org/a", "read"),
		Decision: Decision{Allowed: false, Reason: "organization has no published version"},
	}, genesisFingerprint(org))
	second := syntheticRecord(org, 2, AuditPolicyChange,
		&PolicyChange{Version: 1, Policies: nil}, nil, first.Fingerprint)
	return []AuditRecord{first, second}
}

// TestArchiveReadRejectsCategoryContentMismatch enumerates every illegal
// category/content combination. Each case is a structurally perfect,
// fully-fingerprinted archive whose final record alone violates the rule;
// both read entry points must fail the whole read with ErrInvalidRange,
// must not classify readable content as ErrInvalidArchive, and must hand
// back neither records nor verified material.
func TestArchiveReadRejectsCategoryContentMismatch(t *testing.T) {
	const org = kindMismatchOrg
	base := kindMismatchBaseChain(org)

	// Payload content that is itself legal; only the category/payload
	// combination under test is not.
	change := &PolicyChange{Version: 1, Policies: nil}
	decision := &DecisionRecord{
		Request:  request(org, "u2", "r1", "org/a", "read"),
		Decision: Decision{Allowed: false, Reason: "organization has no published version"},
	}

	cases := []struct {
		name     string
		kind     string
		change   *PolicyChange
		decision *DecisionRecord
		wantText string
	}{
		{"policy change missing its change content", AuditPolicyChange, nil, nil,
			"payload does not match its category"},
		{"policy change carrying only decision content", AuditPolicyChange, nil, decision,
			"payload does not match its category"},
		{"policy change carrying both payloads", AuditPolicyChange, change, decision,
			"payload does not match its category"},
		{"decision missing its decision content", AuditDecision, nil, nil,
			"payload does not match its category"},
		{"decision carrying only change content", AuditDecision, change, nil,
			"payload does not match its category"},
		{"decision carrying both payloads", AuditDecision, change, decision,
			"payload does not match its category"},
		{"category outside the two public kinds", "mystery_category", nil, nil,
			"unknown category"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The invalid record is fingerprint-correct for exactly the bytes
			// the archive carries, so a fingerprint match can never excuse it.
			bad := syntheticRecord(org, 3, tc.kind, tc.change, tc.decision, base[1].Fingerprint)
			full := append(append([]AuditRecord(nil), base...), bad)
			cp := Checkpoint{Org: org, EndSeq: 3, Fingerprint: bad.Fingerprint}
			data := handArchiveForTesting(t, org, cp, full)

			got, err := DecodeAuditArchive(data, org, cp)
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("DecodeAuditArchive err = %v, want ErrInvalidRange", err)
			}
			if errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("readable content with a bad category is not archive-format damage: %v", err)
			}
			if got != nil {
				t.Fatalf("DecodeAuditArchive delivered %d records from invalid content, want nil", len(got))
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %q, want it to name %q", err.Error(), tc.wantText)
			}

			material, merr := DecodeVerifiedAuditArchive(data, org, cp)
			if !errors.Is(merr, ErrInvalidRange) {
				t.Fatalf("DecodeVerifiedAuditArchive err = %v, want ErrInvalidRange", merr)
			}
			if errors.Is(merr, ErrInvalidArchive) {
				t.Fatalf("verified read counted a category mismatch as format damage: %v", merr)
			}
			if !reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
				t.Fatalf("DecodeVerifiedAuditArchive delivered material: %+v", material)
			}
			if !strings.Contains(merr.Error(), tc.wantText) {
				t.Fatalf("verified err = %q, want it to name %q", merr.Error(), tc.wantText)
			}
		})
	}
}

// TestArchiveMismatchAfterValidDecisionDiscardsPrefix is the all-or-nothing
// boundary: the invalid record sits after a normal decision, yet the whole
// read must fail and must never first hand back the valid prefix. The same
// valid prefix archived alone under its own matching checkpoint must read
// normally and still be reviewable.
func TestArchiveMismatchAfterValidDecisionDiscardsPrefix(t *testing.T) {
	const org = kindMismatchOrg
	base := kindMismatchBaseChain(org)

	// Record 3 declares a policy change but carries no change content (and no
	// decision): the required content is missing.
	bad := syntheticRecord(org, 3, AuditPolicyChange, nil, nil, base[1].Fingerprint)
	full := append(append([]AuditRecord(nil), base...), bad)
	fullCP := Checkpoint{Org: org, EndSeq: 3, Fingerprint: bad.Fingerprint}
	data := handArchiveForTesting(t, org, fullCP, full)

	if got, err := DecodeAuditArchive(data, org, fullCP); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("full read after a valid decision: records=%v err=%v, want ErrInvalidRange, nil", got, err)
	}
	if material, err := DecodeVerifiedAuditArchive(data, org, fullCP); !errors.Is(err, ErrInvalidRange) ||
		!reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
		t.Fatalf("verified full read: material=%+v err=%v, want ErrInvalidRange and no material", material, err)
	}

	// The normal decision prefix is genuinely good material: under its own
	// checkpoint both entries return it and the decision still reviews.
	prefix := base[:1]
	prefixCP := Checkpoint{Org: org, EndSeq: 1, Fingerprint: prefix[0].Fingerprint}
	prefixArchive, err := EncodeAuditArchive(org, prefix, prefixCP)
	if err != nil {
		t.Fatalf("encode valid prefix: %v", err)
	}
	loaded, err := DecodeAuditArchive(prefixArchive, org, prefixCP)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("valid prefix read = %d records, %v; want 1, nil", len(loaded), err)
	}
	review, err := RecheckDecisionOffline(org, loaded, prefixCP, 1)
	if err != nil || !review.Consistent {
		t.Fatalf("valid prefix review = %+v, %v; want consistent", review, err)
	}
	material, err := DecodeVerifiedAuditArchive(prefixArchive, org, prefixCP)
	if err != nil {
		t.Fatalf("verified prefix read: %v", err)
	}
	verifiedReview, err := RecheckVerifiedDecisionOffline(material, 1)
	if err != nil || !verifiedReview.Consistent {
		t.Fatalf("verified prefix review = %+v, %v; want consistent", verifiedReview, err)
	}
}

// TestArchiveLegalBoundaryCategoriesRead pins the valid records the rule
// must not sweep in: a policy change whose change content is present but
// whose policy set is empty/nil is still a change, and a decision whose
// decision content is present but denies with version 0 and no matched
// policy is still a decision. Empty or zero values are not missing content.
func TestArchiveLegalBoundaryCategoriesRead(t *testing.T) {
	const org = kindMismatchOrg

	t.Run("change content with a nil policy set", func(t *testing.T) {
		rec := syntheticRecord(org, 1, AuditPolicyChange,
			&PolicyChange{Version: 1, Policies: nil}, nil, genesisFingerprint(org))
		cp := Checkpoint{Org: org, EndSeq: 1, Fingerprint: rec.Fingerprint}
		archive, err := EncodeAuditArchive(org, []AuditRecord{rec}, cp)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		got, err := DecodeAuditArchive(archive, org, cp)
		if err != nil || len(got) != 1 {
			t.Fatalf("decode = %d records, %v; want 1, nil", len(got), err)
		}
		if got[0].Change == nil || got[0].Change.Policies != nil || got[0].Decision != nil {
			t.Fatalf("nil policy set is still a change record: change=%+v decision=%+v",
				got[0].Change, got[0].Decision)
		}
		material, err := DecodeVerifiedAuditArchive(archive, org, cp)
		if err != nil {
			t.Fatalf("verified decode: %v", err)
		}
		// It really is not a decision, so reviewing it reports that distinctly.
		if _, err := RecheckVerifiedDecisionOffline(material, 1); !errors.Is(err, ErrAuditNotADecision) {
			t.Fatalf("review of a change record err = %v, want ErrAuditNotADecision", err)
		}
	})

	decisionCases := []struct {
		name    string
		matched []string
	}{
		{"denial at version 0 with no matched list", nil},
		{"denial at version 0 with an empty matched list", []string{}},
	}
	for _, dc := range decisionCases {
		t.Run(dc.name, func(t *testing.T) {
			rec := syntheticRecord(org, 1, AuditDecision, nil, &DecisionRecord{
				Request:  request(org, "u1", "r1", "org/a", "read"),
				Decision: Decision{Allowed: false, Reason: "organization has no published version", Matched: dc.matched},
			}, genesisFingerprint(org))
			cp := Checkpoint{Org: org, EndSeq: 1, Fingerprint: rec.Fingerprint}
			archive, err := EncodeAuditArchive(org, []AuditRecord{rec}, cp)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			got, err := DecodeAuditArchive(archive, org, cp)
			if err != nil || len(got) != 1 {
				t.Fatalf("decode = %d records, %v; want 1, nil", len(got), err)
			}
			d := got[0].Decision
			if d == nil || got[0].Change != nil {
				t.Fatalf("denial/version-0 record is still a decision record: %+v", got[0])
			}
			if d.Decision.Allowed || d.Decision.Version != 0 || len(d.Decision.Matched) != 0 {
				t.Fatalf("decision values changed: %+v", d.Decision)
			}
			if (dc.matched == nil) != (d.Decision.Matched == nil) {
				t.Fatalf("matched nil-vs-empty shape changed: got %v", d.Decision.Matched)
			}

			material, err := DecodeVerifiedAuditArchive(archive, org, cp)
			if err != nil {
				t.Fatalf("verified decode: %v", err)
			}
			review, err := RecheckVerifiedDecisionOffline(material, 1)
			if err != nil || !review.Consistent {
				t.Fatalf("legal version-0 denial review = %+v, %v; want consistent", review, err)
			}
		})
	}
}

// TestVerifiedArchiveReadLeavesInconsistencyToReview keeps read success and
// review disagreement separate for the verified-material entry: the
// material matches its checkpoint and reads successfully even though the
// preserved decision contradicts what the recorded policies produce; the
// disagreement surfaces as an inconsistent review, never as an archive or
// range failure. Both read entries must agree on that success meaning.
func TestVerifiedArchiveReadLeavesInconsistencyToReview(t *testing.T) {
	const org = kindMismatchOrg
	req := request(org, "u1", "r1", "org/a", "read")
	recs, cp := syntheticChain(org, []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  1,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			// The chain is internally valid, but the stored decision claims a
			// deny the recorded allow policy cannot produce.
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: false, Reason: "matched deny policy", Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	archive, err := EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	material, err := DecodeVerifiedAuditArchive(archive, org, cp)
	if err != nil {
		t.Fatalf("a decision disagreement must not fail the verified read: %v", err)
	}
	review, err := RecheckVerifiedDecisionOffline(material, 2)
	if err != nil {
		t.Fatalf("a disagreement is a review result, not an error: %v", err)
	}
	if review.Consistent {
		t.Fatal("the preserved deny must review inconsistent against the allow policy")
	}

	loaded, err := DecodeAuditArchive(archive, org, cp)
	if err != nil || len(loaded) != len(recs) {
		t.Fatalf("plain read must keep the same success meaning: %d records, %v", len(loaded), err)
	}
}
