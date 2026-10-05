package darksafe

import (
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// This file pins the offline-review compatibility baseline: a complete
// audit export, its archive bytes and its separately retained checkpoint,
// produced once by the live feature (Publish/Decide/AuditExport/
// EncodeAuditArchive) and frozen here as constants. Nothing in this file
// regenerates the material from current logic — that is the point. If a
// future change alters fingerprinting, the archive format, chain
// validation or offline recomputation in a way that invalidates material
// users already saved, these tests fail instead of silently adopting the
// new output.
//
// The frozen scenario, built through the live API in one service instance
// that has since "ended" (no Store exists in these tests):
//
//	seq 1  publish v1: plain-text allow policy p1 (u1, read, org/a)
//	       restricted to the exact resource r1 (ResourceID "r1")
//	seq 2  TARGET decision: u1 reads r1 in org/a — the request resource
//	       matches the restriction, so v1 allows it. Roles not provided
//	       (nil); subject Kind carries a genuine U+FFFD (valid UTF-8)
//	seq 3  publish v2: a policy whose strings carry lone 0xFF bytes
//	       (invalid UTF-8), so the chain mixes both fingerprint families
//	seq 4  invalid-byte decision; Roles explicitly empty (non-nil)
//
// The archive below is the exact EncodeAuditArchive output; the checkpoint
// is the one the caller retained independently of the archive.

const compatOrg = "acme"

// compatCheckpoint is the separately saved checkpoint pinning the frozen
// export. DecodeAuditArchive and RecheckDecisionOffline must validate
// against this, not against anything embedded in the archive.
var compatCheckpoint = Checkpoint{
	Org:         compatOrg,
	EndSeq:      4,
	Fingerprint: "5695090e689ecb8058ddb76e70e2d81564e1ca4b79da27c68c82b88902c58875",
}

// compatPrevFingerprints and compatRecordFingerprints are the per-record
// linkage exactly as saved: each record's predecessor fingerprint and its
// own, starting from the sequence-0 genesis fingerprint.
var compatPrevFingerprints = []string{
	"7eb86f4d4949c004d90f09cee40af1029342326b3df0d39cb2e028b535ed5ca0",
	"7afffb7a25c008e9c881bc43ea6bcd2214b07ff66151deaa47fbf54eb7c983aa",
	"284c1a40c240aeb3e5a29010186d4bb78b4bc674dd8f35378fa3c7f71d9573b4",
	"fee002c47cebdf0642073f039f2b4d884c524105710a0f7a91aa2407dfd067f1",
}
var compatRecordFingerprints = []string{
	"7afffb7a25c008e9c881bc43ea6bcd2214b07ff66151deaa47fbf54eb7c983aa",
	"284c1a40c240aeb3e5a29010186d4bb78b4bc674dd8f35378fa3c7f71d9573b4",
	"fee002c47cebdf0642073f039f2b4d884c524105710a0f7a91aa2407dfd067f1",
	"5695090e689ecb8058ddb76e70e2d81564e1ca4b79da27c68c82b88902c58875",
}

// compatAllowPolicy is the published v1 policy that authorized the target
// decision: an allow narrowed to one exact resource.
var compatAllowPolicy = Policy{
	ID: "p1", Subject: "u1", Action: "read", Scope: "org/a",
	Effect: EffectAllow, ResourceID: "r1",
}

// compatTargetRequest is the saved request the target decision evaluated:
// the resource matches the policy's restriction, the role list was not
// provided (nil), and the subject kind holds a genuine U+FFFD rune.
var compatTargetRequest = OrgRequest{
	SubjectOrg:  compatOrg,
	ResourceOrg: compatOrg,
	Subject:     Subject{ID: "u1", Kind: "外\uFFFD部"},
	Resource:    Resource{ID: "r1", Scope: "org/a"},
	Action:      "read",
}

// compatTargetDecision is the decision preserved in record 2.
var compatTargetDecision = Decision{
	Allowed: true, Reason: "matched allow policy", Matched: []string{"p1"}, Version: 1,
}

// compatInvalidPolicy is the published v2 policy carrying lone 0xFF bytes.
var compatInvalidPolicy = Policy{
	ID: "策\xff略", Subject: "用\xff戶", Action: "读\xff",
	Scope: "org/a/资\xff料", Effect: EffectAllow,
}

// compatInvalidRequest is the saved invalid-byte request of record 4; its
// role list was explicitly empty (non-nil), a shape distinct from nil.
var compatInvalidRequest = OrgRequest{
	SubjectOrg:  compatOrg,
	ResourceOrg: compatOrg,
	Subject:     Subject{ID: "用\xff戶", Kind: "类\xff型", Roles: []string{}},
	Resource:    Resource{ID: "资\xff源", Scope: "org/a/资\xff料"},
	Action:      "读\xff",
}

// compatInvalidDecision is the decision preserved in record 4.
var compatInvalidDecision = Decision{
	Allowed: true, Reason: "matched allow policy", Matched: []string{"策\xff略"}, Version: 2,
}

// compatArchiveHex is the frozen archive, byte for byte as saved.
const compatArchiveHex = "6461726b736166652d61756469742d617263686976652d76310a0000044d0000000461636d6500000004000000403536" +
	"393530393065363839656362383035386464623736653730653264383135363465316361346237396461323763363863" +
	"3832623838393032633538383735000000040000000461636d65000000010000000d706f6c6963795f6368616e676501" +
	"0000000101000000010000000270310000000275310000000472656164000000056f72672f6100000005616c6c6f7700" +
	"000000027231000000000000000000403765623836663464343934396330303464393066303963656534306166313032" +
	"393334323332366233646630643339636232653032386235333565643563613000000040376166666662376132356330" +
	"303865396338383162633433656136626364323231346230376666363631353164656161343766626635346562376339" +
	"383361610000000461636d6500000002000000086465636973696f6e00010000000461636d650000000461636d650000" +
	"0002753100000009e5a496efbfbde983a80000000000027231000000056f72672f61000000047265616401000000146d" +
	"61746368656420616c6c6f7720706f6c6963790100000001000000027031000000010000004037616666666237613235" +
	"633030386539633838316263343365613662636432323134623037666636363135316465616134376662663534656237" +
	"633938336161000000403238346331613430633234306165623365356132393031303138366434626237386234626336" +
	"37346464386633353337386661336337663731643935373362340000000461636d65000000030000000d706f6c696379" +
	"5f6368616e67650100000002010000000100000007e7ad96ffe795a500000007e794a8ffe688b600000004e8afbbff00" +
	"00000d6f72672f612fe8b584ffe6969900000005616c6c6f770000000000000000000000000000403238346331613430" +
	"633234306165623365356132393031303138366434626237386234626336373464643866333533373866613363376637" +
	"316439353733623400000040666565303032633437636562646630363432303733663033396632623464383834633532" +
	"343130353731306130663761393161613234303764666430363766310000000461636d65000000040000000864656369" +
	"73696f6e00010000000461636d650000000461636d6500000007e794a8ffe688b600000007e7b1bbffe59e8b01000000" +
	"000000000007e8b584ffe6ba900000000d6f72672f612fe8b584ffe6969900000004e8afbbff01000000146d61746368" +
	"656420616c6c6f7720706f6c696379010000000100000007e7ad96ffe795a50000000200000040666565303032633437" +
	"636562646630363432303733663033396632623464383834633532343130353731306130663761393161613234303764" +
	"666430363766310000004035363935303930653638396563623830353864646237366537306532643831353634653163" +
	"613462373964613237633638633832623838393032633538383735c304a0ac93a4abbd56856ce07d7fc93e3958139fd2" +
	"a0ca2814504f41ab1833c4"

// compatArchiveBytes decodes the frozen archive hex.
func compatArchiveBytes(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(compatArchiveHex)
	if err != nil {
		t.Fatalf("frozen archive hex does not decode: %v", err)
	}
	return b
}

// compatRecords reads the frozen archive with the independently retained
// checkpoint — the exact material a user holds after the service instance
// that produced it has ended.
func compatRecords(t *testing.T) []AuditRecord {
	t.Helper()
	recs, err := DecodeAuditArchive(compatArchiveBytes(t), compatOrg, compatCheckpoint)
	if err != nil {
		t.Fatalf("frozen archive no longer reads against its saved checkpoint: %v", err)
	}
	return recs
}

// TestCompatMaterialStillVerifiesAndReviews reads the frozen archive with
// the frozen checkpoint and replays the whole offline flow: chain
// validation, field-by-field comparison with the saved values, and offline
// review of the target decision. No Store is created — the service
// instance that produced the material is gone.
func TestCompatMaterialStillVerifiesAndReviews(t *testing.T) {
	recs := compatRecords(t)
	if err := VerifyAudit(compatOrg, recs, compatCheckpoint); err != nil {
		t.Fatalf("frozen chain no longer verifies: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("record count = %d, want 4", len(recs))
	}

	// Organization, sequence and both fingerprints of every record are
	// exactly as saved, and the linkage chains record to record.
	wantKinds := []string{AuditPolicyChange, AuditDecision, AuditPolicyChange, AuditDecision}
	for i := range recs {
		r := &recs[i]
		if r.Org != compatOrg {
			t.Fatalf("record %d org = %q, want %q", r.Seq, r.Org, compatOrg)
		}
		if r.Seq != i+1 {
			t.Fatalf("record %d seq = %d, want %d", i, r.Seq, i+1)
		}
		if r.Kind != wantKinds[i] {
			t.Fatalf("record %d kind = %q, want %q", r.Seq, r.Kind, wantKinds[i])
		}
		if r.PrevFingerprint != compatPrevFingerprints[i] {
			t.Fatalf("record %d prev fingerprint = %s, want saved %s",
				r.Seq, r.PrevFingerprint, compatPrevFingerprints[i])
		}
		if r.Fingerprint != compatRecordFingerprints[i] {
			t.Fatalf("record %d fingerprint = %s, want saved %s",
				r.Seq, r.Fingerprint, compatRecordFingerprints[i])
		}
		if i > 0 && r.PrevFingerprint != recs[i-1].Fingerprint {
			t.Fatalf("record %d not linked to record %d", r.Seq, i)
		}
	}
	if compatCheckpoint.Fingerprint != recs[3].Fingerprint {
		t.Fatal("saved checkpoint does not pin the final record")
	}

	// The plain-text records stay in the legacy JSON fingerprint family;
	// the invalid-byte records must not.
	for _, idx := range []int{0, 1} {
		if got := fingerprintFor(&recs[idx]); got != legacyJSONFingerprint(&recs[idx]) {
			t.Fatalf("record %d left the legacy JSON fingerprint family", idx+1)
		}
	}
	for _, idx := range []int{2, 3} {
		if got := fingerprintFor(&recs[idx]); got == legacyJSONFingerprint(&recs[idx]) {
			t.Fatalf("record %d unexpectedly uses the JSON fingerprint", idx+1)
		}
	}

	// Policy content reads back byte for byte.
	if !reflect.DeepEqual(recs[0].Change, &PolicyChange{Version: 1, Policies: []Policy{compatAllowPolicy}}) {
		t.Fatalf("record 1 change = %+v", recs[0].Change)
	}
	if !reflect.DeepEqual(recs[2].Change, &PolicyChange{Version: 2, Policies: []Policy{compatInvalidPolicy}}) {
		t.Fatalf("record 3 change = %+v", recs[2].Change)
	}

	// The full requests read back byte for byte, including the nil role
	// list of the target and the explicitly empty one of record 4.
	if !reflect.DeepEqual(recs[1].Decision.Request, compatTargetRequest) {
		t.Fatalf("record 2 request = %+v, want %+v", recs[1].Decision.Request, compatTargetRequest)
	}
	if recs[1].Decision.Request.Subject.Roles != nil {
		t.Fatalf("record 2 roles = %v, want nil (not provided)", recs[1].Decision.Request.Subject.Roles)
	}
	if !reflect.DeepEqual(recs[3].Decision.Request, compatInvalidRequest) {
		t.Fatalf("record 4 request = %+v, want %+v", recs[3].Decision.Request, compatInvalidRequest)
	}
	if roles := recs[3].Decision.Request.Subject.Roles; roles == nil || len(roles) != 0 {
		t.Fatalf("record 4 roles = %v, want explicitly empty (non-nil)", roles)
	}
	if !reflect.DeepEqual(recs[1].Decision.Decision, compatTargetDecision) {
		t.Fatalf("record 2 decision = %+v, want %+v", recs[1].Decision.Decision, compatTargetDecision)
	}
	if !reflect.DeepEqual(recs[3].Decision.Decision, compatInvalidDecision) {
		t.Fatalf("record 4 decision = %+v, want %+v", recs[3].Decision.Decision, compatInvalidDecision)
	}

	// Byte distinctions survive the round trip: the lone 0xFF bytes are
	// still invalid UTF-8, the genuine U+FFFD is still a valid rune, and
	// none of 0xFF / 0xFE / U+FFFD collapsed onto another.
	kind := recs[1].Decision.Request.Subject.Kind
	if !strings.Contains(kind, "\uFFFD") || !utf8.ValidString(kind) {
		t.Fatalf("genuine U+FFFD not preserved as valid UTF-8: %q", kind)
	}
	for _, s := range []string{
		recs[2].Change.Policies[0].ID,
		recs[3].Decision.Request.Subject.ID,
		recs[3].Decision.Decision.Matched[0],
	} {
		if !strings.Contains(s, "\xff") || utf8.ValidString(s) {
			t.Fatalf("lone 0xFF not preserved byte for byte: %q", s)
		}
	}
	if compatInvalidPolicy.ID == strings.Replace(compatInvalidPolicy.ID, "\xff", "\xfe", 1) ||
		compatInvalidPolicy.ID == strings.Replace(compatInvalidPolicy.ID, "\xff", "\uFFFD", 1) {
		t.Fatal("test premise broken: 0xFF, 0xFE and U+FFFD must stay distinct")
	}

	// Offline review of the target decision: the saved allow, authorized
	// by the resource-restricted policy, recomputes to the same decision.
	review, err := RecheckDecisionOffline(compatOrg, recs, compatCheckpoint, 2)
	if err != nil {
		t.Fatalf("offline review of saved material: %v", err)
	}
	if !review.Consistent {
		t.Fatalf("saved decision no longer consistent: %+v vs %+v", review.Original, review.Recomputed)
	}
	if !reflect.DeepEqual(review.Original, compatTargetDecision) {
		t.Fatalf("original = %+v, want %+v", review.Original, compatTargetDecision)
	}
	if !reflect.DeepEqual(review.Recomputed, compatTargetDecision) {
		t.Fatalf("recomputed = %+v, want %+v", review.Recomputed, compatTargetDecision)
	}

	// The invalid-byte decision reviews consistently too, with the matched
	// policy id byte-exact.
	review4, err := RecheckDecisionOffline(compatOrg, recs, compatCheckpoint, 4)
	if err != nil {
		t.Fatalf("offline review of invalid-byte record: %v", err)
	}
	if !review4.Consistent || !reflect.DeepEqual(review4.Recomputed, compatInvalidDecision) {
		t.Fatalf("invalid-byte review = %+v, want consistent with %+v", review4, compatInvalidDecision)
	}
}

// TestCompatTamperedByteRejected changes exactly one protected field of
// the material read back from the frozen archive — the lone 0xFF becomes
// 0xFE or a genuine U+FFFD — while every record fingerprint and the saved
// checkpoint stay untouched. Chain validation and offline review must both
// refuse with ErrInvalidRange, and the review must deliver no conclusion.
func TestCompatTamperedByteRejected(t *testing.T) {
	replacements := map[string]string{"0xFE": "\xfe", "U+FFFD": "\uFFFD"}
	fields := map[string]func([]AuditRecord, string){
		"published policy id": func(recs []AuditRecord, repl string) {
			p := &recs[2].Change.Policies[0]
			p.ID = strings.Replace(p.ID, "\xff", repl, 1)
		},
		"decision subject id": func(recs []AuditRecord, repl string) {
			id := &recs[3].Decision.Request.Subject.ID
			*id = strings.Replace(*id, "\xff", repl, 1)
		},
	}
	for field, mutate := range fields {
		for replName, repl := range replacements {
			t.Run(field+"/"+replName, func(t *testing.T) {
				recs := compatRecords(t)
				mutate(recs, repl)
				// Fingerprints and the checkpoint are the saved ones;
				// only the one field's bytes differ.
				if recs[2].Fingerprint != compatRecordFingerprints[2] ||
					recs[3].Fingerprint != compatRecordFingerprints[3] {
					t.Fatal("test premise broken: record fingerprints must stay as saved")
				}
				if err := VerifyAudit(compatOrg, recs, compatCheckpoint); !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("VerifyAudit err = %v, want ErrInvalidRange", err)
				}
				got, err := RecheckDecisionOffline(compatOrg, recs, compatCheckpoint, 2)
				if !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("RecheckDecisionOffline err = %v, want ErrInvalidRange", err)
				}
				if !reflect.DeepEqual(got, OfflineDecisionReview{}) {
					t.Fatalf("tampered material delivered a partial conclusion: %+v", got)
				}
			})
		}
	}
}
