package darksafe

import (
	"testing"
)

// TestRecordFieldTableIsTheProtectedEnvelope locks the single outer field
// table to the six fields a record fingerprint historically protects, in
// their historical order. The archive walks this same table, so a field
// added, dropped or reordered here changes both fingerprint families and
// the archive in lockstep; this test then fails instead of letting one path
// silently enumerate a different outer shape than the others.
func TestRecordFieldTableIsTheProtectedEnvelope(t *testing.T) {
	want := []struct {
		key  string
		kind recordFieldKind
	}{
		{"org", recordFieldString},
		{"seq", recordFieldInt},
		{"kind", recordFieldString},
		{"change", recordFieldChangePayload},
		{"decision", recordFieldDecisionPayload},
		{"prev", recordFieldString},
	}
	if len(recordFields) != len(want) {
		t.Fatalf("recordFields has %d entries, want %d", len(recordFields), len(want))
	}
	for i := range want {
		got := &recordFields[i]
		if got.jsonKey != want[i].key || got.kind != want[i].kind {
			t.Fatalf("recordFields[%d] = (%q, kind %d), want (%q, kind %d)",
				i, got.jsonKey, got.kind, want[i].key, want[i].kind)
		}
	}
}

// TestFingerprintNeverProtectsItsOwnFingerprint is the security invariant
// behind the table's shape: AuditRecord.Fingerprint is the hash output and
// must never become a hash input. Two records identical in every protected
// outer field but carrying different Fingerprint values must therefore hash
// identically, for both the valid-UTF-8 JSON family and the invalid-UTF-8
// raw family. The predecessor link is protected in the other direction:
// changing PrevFingerprint must change the fingerprint.
func TestFingerprintNeverProtectsItsOwnFingerprint(t *testing.T) {
	cases := []struct {
		name string
		rec  AuditRecord
	}{
		{
			name: "json-family-policy-change",
			rec: AuditRecord{
				Org: "acme", Seq: 1, Kind: AuditPolicyChange,
				Change: &PolicyChange{
					Version: 1,
					Policies: []Policy{{
						ID: "p1", Subject: "主 体", Action: "read",
						Scope: "org/a/资 料", Effect: EffectAllow,
					}},
				},
				PrevFingerprint: genesisFingerprint("acme"),
			},
		},
		{
			name: "raw-family-decision",
			rec: AuditRecord{
				Org: "组" + invalidByte + "织", Seq: 2, Kind: AuditDecision,
				Decision: &DecisionRecord{
					Request: OrgRequest{
						SubjectOrg: "组" + invalidByte + "织", ResourceOrg: "x",
						Subject:  Subject{ID: "身" + invalidByte + "份"},
						Resource: Resource{ID: "r1", Scope: "org/a"},
						Action:   "read",
					},
					Decision: Decision{Allowed: true, Reason: "matched allow policy", Version: 1},
				},
				PrevFingerprint: "前" + invalidByte + "项",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.rec
			b := tc.rec
			a.Fingerprint = ""
			b.Fingerprint = "0123456789abcdef-current-fingerprint-must-be-ignored"
			fa := fingerprintFor(&a)
			fb := fingerprintFor(&b)
			if fa != fb {
				t.Fatalf("fingerprint depends on the record's own Fingerprint field: %q vs %q", fa, fb)
			}
			if fa == "" {
				t.Fatal("fingerprintFor returned empty fingerprint")
			}

			// The predecessor link is still a protected input.
			c := tc.rec
			c.PrevFingerprint = tc.rec.PrevFingerprint + "x"
			if fc := fingerprintFor(&c); fc == fa {
				t.Fatal("changing PrevFingerprint did not change the fingerprint")
			}
			// The organization binding at the chain root is still protected.
			d := tc.rec
			d.Org = tc.rec.Org + "-other"
			if fd := fingerprintFor(&d); fd == fa {
				t.Fatal("changing Org did not change the fingerprint")
			}
		})
	}
}

// TestTableDrivenArchiveRoundTripsAllOuterFields writes one record of each
// category through the table-driven archive writer and reads it back through
// the table-driven reader, proving every protected outer field the table
// lists survives (including an empty predecessor-bearing first record) and
// that the separately tailed current fingerprint lands back on the record.
func TestTableDrivenArchiveRoundTripsAllOuterFields(t *testing.T) {
	org := "acme"
	recs := []AuditRecord{
		syntheticRecord(org, 1, AuditPolicyChange, &PolicyChange{
			Version: 1, Policies: []Policy{{ID: "p1", Effect: EffectAllow}},
		}, nil, genesisFingerprint(org)),
	}
	prev := recs[0].Fingerprint
	recs = append(recs, syntheticRecord(org, 2, AuditDecision, nil, &DecisionRecord{
		Request: OrgRequest{
			SubjectOrg: org, ResourceOrg: org,
			Subject:  Subject{ID: "u1", Roles: []string{}},
			Resource: Resource{ID: "r1", Scope: "org/a"},
			Action:   "read",
		},
		Decision: Decision{Allowed: true, Reason: "matched allow policy", Matched: nil, Version: 1},
	}, prev))
	cp := Checkpoint{Org: org, EndSeq: 2, Fingerprint: recs[1].Fingerprint}

	arch, err := EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeAuditArchive(arch, org, cp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("got %d records, want %d", len(got), len(recs))
	}
	for i := range recs {
		g, w := &got[i], &recs[i]
		if g.Org != w.Org || g.Seq != w.Seq || g.Kind != w.Kind ||
			g.PrevFingerprint != w.PrevFingerprint || g.Fingerprint != w.Fingerprint {
			t.Fatalf("record %d outer fields differ after round trip:\n got %+v\nwant %+v", i+1, *g, *w)
		}
	}
}
