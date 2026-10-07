// This file is the read-side regression for the audit rule that a
// record's category must always match the content it actually carries:
// a policy-change record ("policy_change") must carry a change payload and
// no decision payload, and a decision record ("decision") must carry a
// decision payload and no change payload. EncodeAuditArchive already
// refuses such material (TestArchiveEncodingRejectsInvalidMaterial), but a
// read must defend the rule on its own: the archives here never pass
// through EncodeAuditArchive.
//
// Every archive below is fully readable — correct magic, declared length,
// checksum, organization, sequences, predecessor fingerprints, embedded
// checkpoint and a separately retained checkpoint that all agree — so the
// only thing wrong is audit content. Such material is invalid audit
// content, not an unreadable file: reading the whole archive must fail
// with ErrInvalidRange (matched by errors.Is), never ErrInvalidArchive,
// and must deliver neither records nor VerifiedAuditMaterial. Decode
// happens before review, so an invalid record anywhere fails the read
// itself rather than waiting for the offline review of some target.
//
// The guarantee is pinned for both public read entries, which must agree
// on success and failure: DecodeAuditArchive and
// DecodeVerifiedAuditArchive.
package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// archiveFromRecords hand-builds one archive frame from records without
// validating them, so material EncodeAuditArchive would reject can still
// arrive at a reader as a perfectly framed, checksum-valid file. The
// payload walks the same single record field table EncodeAuditArchive
// walks, and the outer frame gets a consistent length and checksum.
func archiveFromRecords(t *testing.T, org string, cp Checkpoint, records []AuditRecord) []byte {
	t.Helper()
	pw := &archiveWriter{}
	pw.stringField(org)
	pw.u32(cp.EndSeq)
	pw.stringField(cp.Fingerprint)
	pw.u32(len(records))
	for i := range records {
		rec := &records[i]
		for j := range recordFields {
			f := &recordFields[j]
			switch f.kind {
			case recordFieldString:
				pw.stringField(f.getString(rec))
			case recordFieldInt:
				pw.u32(f.getInt(rec))
			case recordFieldChange:
				pw.change(f.getChange(rec))
			case recordFieldDecision:
				pw.decisionRecord(f.getDecision(rec))
			}
		}
	}
	if pw.err != nil {
		t.Fatal(pw.err)
	}
	return framePayload(pw.buf)
}

// validKindContentPrefix builds the two-record normal prefix the mismatch
// cases extend: a version-1 policy change carrying an allow policy, then a
// decision that allowed by that policy. The pair is valid material and is
// reviewable on its own under its own checkpoint.
func validKindContentPrefix() []AuditRecord {
	req := request("acme", "u1", "r1", "org/a", "read")
	recs, _ := syntheticChain("acme", []func(int) (string, *PolicyChange, *DecisionRecord){
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditPolicyChange, &PolicyChange{
				Version:  1,
				Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
			}, nil
		},
		func(seq int) (string, *PolicyChange, *DecisionRecord) {
			return AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
			}
		},
	})
	return recs
}

// TestArchiveDecodeRejectsCategoryContentMismatch enumerates every
// category/content combination the rule forbids for a record following a
// fully normal prefix: required payload missing, only the other payload
// present, both payloads present, and a category outside the two public
// ones (including the empty category). Framing and fingerprints all hold,
// so both read entries must reject the whole archive as content
// (ErrInvalidRange), not as an unreadable file (ErrInvalidArchive), and
// hand back nothing.
func TestArchiveDecodeRejectsCategoryContentMismatch(t *testing.T) {
	const org = "acme"
	changeV1 := &PolicyChange{Version: 1, Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}}
	decisionV1 := &DecisionRecord{
		Request:  request(org, "u1", "r1", "org/a", "read"),
		Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
	}

	cases := []struct {
		name     string
		kind     string
		change   *PolicyChange
		decision *DecisionRecord
	}{
		// Policy-change records.
		{"policy change missing both payloads", AuditPolicyChange, nil, nil},
		{"policy change carrying only a decision", AuditPolicyChange, nil, decisionV1},
		{"policy change carrying change and decision", AuditPolicyChange, changeV1, decisionV1},
		// Decision records.
		{"decision missing both payloads", AuditDecision, nil, nil},
		{"decision carrying only a change", AuditDecision, changeV1, nil},
		{"decision carrying decision and change", AuditDecision, changeV1, decisionV1},
		// Categories outside the two public ones, in every payload shape.
		{"unknown category missing both payloads", "maintenance", nil, nil},
		{"unknown category carrying only a change", "maintenance", changeV1, nil},
		{"unknown category carrying only a decision", "maintenance", nil, decisionV1},
		{"unknown category carrying both payloads", "maintenance", changeV1, decisionV1},
		{"empty category missing both payloads", "", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefix := validKindContentPrefix()
			bad := syntheticRecord(org, 3, tc.kind, tc.change, tc.decision, prefix[1].Fingerprint)
			full := append(append([]AuditRecord(nil), prefix...), bad)
			cp := Checkpoint{Org: org, EndSeq: 3, Fingerprint: bad.Fingerprint}
			data := archiveFromRecords(t, org, cp, full)

			// Control: the file is completely readable and its records
			// reconstruct in full with matching fingerprints. The rejection
			// must therefore be an audit-content verdict, not a parse failure.
			frame, ferr := decodeAuditArchiveFrame(data)
			if ferr != nil || len(frame.records) != 3 {
				t.Fatalf("control: frame must parse with 3 records, got %d, %v", len(frame.records), ferr)
			}

			got, err := DecodeAuditArchive(data, org, cp)
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("DecodeAuditArchive err = %v, want ErrInvalidRange", err)
			}
			if errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("invalid content reported as an unreadable archive: %v", err)
			}
			if got != nil {
				t.Fatalf("DecodeAuditArchive delivered %d records from invalid material", len(got))
			}

			material, err := DecodeVerifiedAuditArchive(data, org, cp)
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("DecodeVerifiedAuditArchive err = %v, want ErrInvalidRange", err)
			}
			if errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("invalid content reported as an unreadable archive: %v", err)
			}
			if !reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
				t.Fatalf("DecodeVerifiedAuditArchive delivered verified material: %+v", material)
			}
		})
	}
}

// TestArchiveMismatchAfterValidDecisionDiscardsPrefix pins the
// all-or-nothing boundary at read time: an invalid record placed after a
// normal, reviewable decision must fail the whole read of the full
// archive through both entries, never first delivering the intact normal
// prefix. The identical normal prefix, read as its own archive under its
// own checkpoint, must read and review normally — so the failure is a
// deliberate whole-archive verdict rather than an inability to read the
// records.
func TestArchiveMismatchAfterValidDecisionDiscardsPrefix(t *testing.T) {
	const org = "acme"
	prefix := validKindContentPrefix()
	prefixCP := Checkpoint{Org: org, EndSeq: 2, Fingerprint: prefix[1].Fingerprint}

	// The normal prefix really is valid, readable material on its own.
	prefixArchive, err := EncodeAuditArchive(org, prefix, prefixCP)
	if err != nil {
		t.Fatalf("encode normal prefix: %v", err)
	}
	prefixRecs, err := DecodeAuditArchive(prefixArchive, org, prefixCP)
	if err != nil || len(prefixRecs) != 2 {
		t.Fatalf("normal prefix must read: %d records, %v", len(prefixRecs), err)
	}
	prefixReview, err := RecheckDecisionOffline(org, prefixRecs, prefixCP, 2)
	if err != nil || !prefixReview.Consistent || !prefixReview.Recomputed.Allowed {
		t.Fatalf("normal prefix review = %+v, %v", prefixReview, err)
	}
	prefixMaterial, err := DecodeVerifiedAuditArchive(prefixArchive, org, prefixCP)
	if err != nil {
		t.Fatalf("normal prefix verified read: %v", err)
	}
	verifiedReview, err := RecheckVerifiedDecisionOffline(prefixMaterial, 2)
	if err != nil || !verifiedReview.Consistent || !verifiedReview.Recomputed.Allowed {
		t.Fatalf("normal prefix verified review = %+v, %v", verifiedReview, err)
	}

	// Extend that same prefix, byte for byte in its first two records, with
	// a decision-kind record carrying only a change payload. The full
	// archive is well framed and checksum valid but content-invalid.
	bad := syntheticRecord(org, 3, AuditDecision,
		&PolicyChange{Version: 1, Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}},
		nil, prefix[1].Fingerprint)
	full := append(append([]AuditRecord(nil), prefix...), bad)
	fullCP := Checkpoint{Org: org, EndSeq: 3, Fingerprint: bad.Fingerprint}
	fullArchive := archiveFromRecords(t, org, fullCP, full)

	if got, err := DecodeAuditArchive(fullArchive, org, fullCP); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("full archive after a normal decision: records=%v err=%v, want ErrInvalidRange, nil", got, err)
	}
	material, err := DecodeVerifiedAuditArchive(fullArchive, org, fullCP)
	if !errors.Is(err, ErrInvalidRange) || !reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
		t.Fatalf("verified full archive: material=%+v err=%v, want ErrInvalidRange and no material", material, err)
	}
}

// TestArchiveMismatchBeforeValidDecisionAlsoFails shows the read does not
// stop at the first invalid record's position relative to good content: a
// mismatch ahead of an otherwise normal decision and change is rejected
// just as firmly, through both entries.
func TestArchiveMismatchBeforeValidDecisionAlsoFails(t *testing.T) {
	const org = "acme"
	genesis := genesisFingerprint(org)
	// Sequence 1 claims to be a decision but carries only a change payload.
	bad := syntheticRecord(org, 1, AuditDecision, &PolicyChange{Version: 1, Policies: nil}, nil, genesis)
	change := syntheticRecord(org, 2, AuditPolicyChange,
		&PolicyChange{Version: 1, Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)}},
		nil, bad.Fingerprint)
	decision := syntheticRecord(org, 3, AuditDecision, nil, &DecisionRecord{
		Request:  request(org, "u1", "r1", "org/a", "read"),
		Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1},
	}, change.Fingerprint)
	full := []AuditRecord{bad, change, decision}
	cp := Checkpoint{Org: org, EndSeq: 3, Fingerprint: decision.Fingerprint}
	data := archiveFromRecords(t, org, cp, full)

	if got, err := DecodeAuditArchive(data, org, cp); !errors.Is(err, ErrInvalidRange) || got != nil {
		t.Fatalf("leading mismatch: records=%v err=%v, want ErrInvalidRange, nil", got, err)
	}
	material, err := DecodeVerifiedAuditArchive(data, org, cp)
	if !errors.Is(err, ErrInvalidRange) || !reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
		t.Fatalf("leading mismatch verified read: material=%+v err=%v", material, err)
	}
}

// TestArchiveDecodeAcceptsValidEmptyAndZeroContent pins the legal
// boundaries the category/content rule must not confuse with missing
// content: a policy change whose change payload is present despite an
// empty (nil or non-nil empty) policy set is still a valid change record,
// and a decision whose decision payload is present despite a deny result,
// version 0 and no matched policies is still a valid decision record.
// Empty lists and zero values are content, not absence. Both entries read
// these successfully, and a legal decision whose later review disagrees
// with the recomputed one is still read successfully: the read confirms
// only that material matches the checkpoint.
func TestArchiveDecodeAcceptsValidEmptyAndZeroContent(t *testing.T) {
	const org = "acme"
	req := request(org, "u1", "r1", "org/a", "read")

	// A change payload present with an empty policy set, in both list
	// shapes, is a valid policy-change record.
	for name, policies := range map[string][]Policy{"nil set": nil, "empty set": {}} {
		t.Run("policy change with "+name, func(t *testing.T) {
			rec := syntheticRecord(org, 1, AuditPolicyChange, &PolicyChange{Version: 1, Policies: policies}, nil, genesisFingerprint(org))
			cp := Checkpoint{Org: org, EndSeq: 1, Fingerprint: rec.Fingerprint}
			data, err := EncodeAuditArchive(org, []AuditRecord{rec}, cp)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := DecodeAuditArchive(data, org, cp)
			if err != nil {
				t.Fatalf("empty policy set must read: %v", err)
			}
			if got[0].Change == nil || got[0].Decision != nil {
				t.Fatalf("decoded payloads wrong: change=%v decision=%v", got[0].Change, got[0].Decision)
			}
			material, err := DecodeVerifiedAuditArchive(data, org, cp)
			if err != nil || reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
				t.Fatalf("verified read of empty policy set: material=%+v err=%v", material, err)
			}
			if err := VerifyAudit(org, got, cp); err != nil {
				t.Fatalf("empty policy set material must verify: %v", err)
			}
			if policies == nil && got[0].Change.Policies != nil {
				t.Fatal("nil policy set decoded as present")
			}
			if policies != nil && (got[0].Change.Policies == nil || len(got[0].Change.Policies) != 0) {
				t.Fatal("empty policy set must decode non-nil and empty")
			}
		})
	}

	// A deny decision at version 0 with no matched policies is a valid
	// decision record; nil and empty matched lists both count as content.
	for name, matched := range map[string][]string{"nil matched": nil, "empty matched": {}} {
		t.Run("version-zero deny with "+name, func(t *testing.T) {
			rec := syntheticRecord(org, 1, AuditDecision, nil, &DecisionRecord{
				Request:  req,
				Decision: Decision{Allowed: false, Reason: "organization has no published version", Matched: matched, Version: 0},
			}, genesisFingerprint(org))
			cp := Checkpoint{Org: org, EndSeq: 1, Fingerprint: rec.Fingerprint}
			data, err := EncodeAuditArchive(org, []AuditRecord{rec}, cp)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := DecodeAuditArchive(data, org, cp)
			if err != nil {
				t.Fatalf("version-zero deny must read: %v", err)
			}
			if got[0].Decision == nil || got[0].Change != nil {
				t.Fatalf("decoded payloads wrong: change=%v decision=%v", got[0].Change, got[0].Decision)
			}
			d := got[0].Decision.Decision
			if d.Allowed || d.Version != 0 || len(d.Matched) != 0 || d.Reason == "" {
				t.Fatalf("zero/empty values mistaken for missing content: %+v", d)
			}
			material, err := DecodeVerifiedAuditArchive(data, org, cp)
			if err != nil || reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
				t.Fatalf("verified read of version-zero deny: material=%+v err=%v", material, err)
			}
			// The read only confirms material matches the checkpoint; the
			// decision is nevertheless reviewable and stays consistent.
			review, err := RecheckDecisionOffline(org, got, cp, 1)
			if err != nil || !review.Consistent || review.Recomputed.Version != 0 || review.Recomputed.Allowed || len(review.Recomputed.Matched) != 0 {
				t.Fatalf("version-zero deny review = %+v, %v", review, err)
			}
			verifiedReview, err := RecheckVerifiedDecisionOffline(material, 1)
			if err != nil || !verifiedReview.Consistent {
				t.Fatalf("verified version-zero deny review = %+v, %v", verifiedReview, err)
			}
		})
	}

	// A legal decision whose conclusion later review disagrees with is
	// still valid audit content: both reads succeed, and the disagreement
	// surfaces as a review result rather than a read failure.
	t.Run("inconsistent decision is still readable", func(t *testing.T) {
		recs, cp := syntheticChain(org, []func(int) (string, *PolicyChange, *DecisionRecord){
			func(seq int) (string, *PolicyChange, *DecisionRecord) {
				return AuditPolicyChange, &PolicyChange{
					Version:  1,
					Policies: []Policy{allowPolicy("p1", "u1", "read", "org/a", false)},
				}, nil
			},
			func(seq int) (string, *PolicyChange, *DecisionRecord) {
				// Version 1 allows u1, yet the record claims a deny.
				return AuditDecision, nil, &DecisionRecord{
					Request:  req,
					Decision: Decision{Allowed: false, Reason: "matched deny policy", Matched: []string{"p1"}, Version: 1},
				}
			},
		})
		data, err := EncodeAuditArchive(org, recs, cp)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := DecodeAuditArchive(data, org, cp)
		if err != nil {
			t.Fatalf("a review disagreement must not fail the read: %v", err)
		}
		material, err := DecodeVerifiedAuditArchive(data, org, cp)
		if err != nil || reflect.DeepEqual(material, VerifiedAuditMaterial{}) {
			t.Fatalf("verified read of review-inconsistent material: material=%+v err=%v", material, err)
		}
		review, err := RecheckDecisionOffline(org, got, cp, 2)
		if err != nil || review.Consistent {
			t.Fatalf("standalone review must report the inconsistency as a result: %+v, %v", review, err)
		}
		verifiedReview, err := RecheckVerifiedDecisionOffline(material, 2)
		if err != nil || verifiedReview.Consistent {
			t.Fatalf("verified review must report the inconsistency as a result: %+v, %v", verifiedReview, err)
		}
	})
}
