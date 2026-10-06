package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// jsonReviewArgs is reviewArgs plus --json, in any position.
func jsonReviewArgs(path, org string, seq, endSeq int, fp string) []string {
	return append([]string{"review", "--json"}, reviewArgs(path, org, seq, endSeq, fp)[1:]...)
}

// parseJSONObject runs the command, asserts success and that stdout is one
// complete JSON object (no surrounding text), and decodes it.
func parseJSONObject(t *testing.T, args []string) (int, map[string]any, string) {
	t.Helper()
	code, out, errOut := runDispatch(t, args...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if errOut != "" {
		t.Fatalf("unexpected stderr: %q", errOut)
	}
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Fatalf("stdout must be one JSON object and nothing else:\n%s", out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, out)
	}
	return code, obj, out
}

func jsonDecisionBlock(t *testing.T, obj map[string]any, key string) map[string]any {
	t.Helper()
	block, ok := obj[key].(map[string]any)
	if !ok {
		t.Fatalf("obj[%q] is not an object: %T in %v", key, obj[key], obj)
	}
	return block
}

// recoverStringBytes reverses the documented string representation: a JSON
// string yields its UTF-8 bytes; a {"base64": ...} object yields the
// decoded original bytes.
func recoverStringBytes(t *testing.T, v any) []byte {
	t.Helper()
	switch s := v.(type) {
	case string:
		return []byte(s)
	case map[string]any:
		enc, ok := s["base64"].(string)
		if !ok {
			t.Fatalf("non-string JSON value is not a {\"base64\":...} object: %v", s)
		}
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("invalid base64 %q: %v", enc, err)
		}
		return raw
	default:
		t.Fatalf("JSON value is neither string nor base64 object: %T", v)
		return nil
	}
}

// TestReviewJSONSuccessObject verifies the happy-path object: every text
// report field has its JSON counterpart, the target sequence is separate
// from policy versions, and stdout carries nothing but the object.
func TestReviewJSONSuccessObject(t *testing.T) {
	const org = "acme payments"
	path, cp := fixtureChain(t, org)

	_, obj, raw := parseJSONObject(t, jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint))

	if got := obj["target_sequence"]; got != float64(2) {
		t.Fatalf("target_sequence = %v, want 2", got)
	}
	if got := obj["consistent"]; got != true {
		t.Fatalf("consistent = %v, want true", got)
	}
	orig := jsonDecisionBlock(t, obj, "original")
	recomp := jsonDecisionBlock(t, obj, "recomputed")
	for name, block := range map[string]map[string]any{"original": orig, "recomputed": recomp} {
		if block["allowed"] != true {
			t.Fatalf("%s.allowed = %v, want true", name, block["allowed"])
		}
		if block["reason"] != "matched allow policy" {
			t.Fatalf("%s.reason = %v", name, block["reason"])
		}
		matched, ok := block["matched_policies"].([]any)
		if !ok || len(matched) != 1 || matched[0] != "p1 ledger" {
			t.Fatalf("%s.matched_policies = %v", name, block["matched_policies"])
		}
		// Policy version is a peer field of the decision, distinct from the
		// top-level target sequence.
		if block["policy_version"] != float64(1) {
			t.Fatalf("%s.policy_version = %v, want 1", name, block["policy_version"])
		}
	}
	// Exactly the documented keys are present.
	wantKeys := map[string]bool{
		"target_sequence": true, "original": true,
		"recomputed": true, "consistent": true,
	}
	for k := range obj {
		if !wantKeys[k] {
			t.Fatalf("unexpected top-level key %q", k)
		}
	}
	if strings.Contains(raw, "target sequence") || strings.Contains(raw, "original decision") {
		t.Fatalf("JSON output must not mix in the text report:\n%s", raw)
	}
}

// TestReviewJSONReplaysRecordedVersion proves the JSON mode still uses the
// historical version: seq 2 replays v1 (allow), seq 4 replays v2 (default
// deny), never a newer set.
func TestReviewJSONReplaysRecordedVersion(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)

	_, early, _ := parseJSONObject(t, jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint))
	eb := jsonDecisionBlock(t, early, "recomputed")
	if eb["allowed"] != true || eb["policy_version"] != float64(1) {
		t.Fatalf("early replay = %v", eb)
	}

	_, late, _ := parseJSONObject(t, jsonReviewArgs(path, org, 4, cp.EndSeq, cp.Fingerprint))
	lb := jsonDecisionBlock(t, late, "recomputed")
	if lb["allowed"] != false || lb["reason"] != "no matching allow policy" ||
		lb["policy_version"] != float64(2) {
		t.Fatalf("late replay = %v", lb)
	}
}

// TestReviewJSONKeepsMatchedOrder checks matched_policies preserves the
// decision's stored order (sorted identifiers here) even when policies were
// published in a different order.
func TestReviewJSONKeepsMatchedOrder(t *testing.T) {
	const org = "acme"
	s := darksafe.NewStore()
	// Publish out of sorted order; evaluation sorts matched identifiers.
	s.Publish(org, 0, []darksafe.Policy{
		{ID: "zzz-policy", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
		{ID: "aaa-policy", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
		{ID: "mmm-policy", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
	})
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
		Action:   "read",
	})
	recs, c, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	arc, err := darksafe.EncodeAuditArchive(org, recs, c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ordered.bin")
	if err := os.WriteFile(path, arc, 0o600); err != nil {
		t.Fatal(err)
	}

	_, obj, _ := parseJSONObject(t, jsonReviewArgs(path, org, 2, c.EndSeq, c.Fingerprint))
	matched := jsonDecisionBlock(t, obj, "recomputed")["matched_policies"].([]any)
	got := make([]string, len(matched))
	for i, v := range matched {
		got[i] = v.(string)
	}
	want := []string{"aaa-policy", "mmm-policy", "zzz-policy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matched order = %v, want %v", got, want)
	}
}

// writeInconsistentArchive builds a chain whose recorded decision claims a
// deny the v1 policy cannot produce: valid material, inconsistent review.
func writeInconsistentArchive(t *testing.T) (string, darksafe.Checkpoint) {
	t.Helper()
	const org = "acme"
	req := darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
		Action:   "read",
	}
	rec1 := darksafe.AuditRecord{
		Org: org, Seq: 1, Kind: darksafe.AuditPolicyChange,
		Change: &darksafe.PolicyChange{
			Version: 1,
			Policies: []darksafe.Policy{
				{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
			},
		},
		PrevFingerprint: genesisFingerprintHex(org),
	}
	rec1.Fingerprint = envelopeFingerprint(syntheticEnvelope{
		Org: rec1.Org, Seq: rec1.Seq, Kind: rec1.Kind,
		Change: rec1.Change, PrevFingerprint: rec1.PrevFingerprint,
	})
	rec2 := darksafe.AuditRecord{
		Org: org, Seq: 2, Kind: darksafe.AuditDecision,
		Decision: &darksafe.DecisionRecord{
			Request: req,
			Decision: darksafe.Decision{
				Allowed: false, Reason: "matched deny policy", Matched: []string{"p1"}, Version: 1,
			},
		},
		PrevFingerprint: rec1.Fingerprint,
	}
	rec2.Fingerprint = envelopeFingerprint(syntheticEnvelope{
		Org: rec2.Org, Seq: rec2.Seq, Kind: rec2.Kind,
		Decision: rec2.Decision, PrevFingerprint: rec2.PrevFingerprint,
	})
	cp := darksafe.Checkpoint{Org: org, EndSeq: 2, Fingerprint: rec2.Fingerprint}
	arc, err := darksafe.EncodeAuditArchive(org, []darksafe.AuditRecord{rec1, rec2}, cp)
	if err != nil {
		t.Fatalf("synthetic chain must encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "inconsistent.bin")
	if err := os.WriteFile(path, arc, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cp
}

// TestReviewJSONInconsistentIsCompleteExitZero: legitimate material whose
// decisions disagree still yields the full object with consistent=false and
// exit 0 — disagreement is never reported as a failure, and nothing goes to
// stderr.
func TestReviewJSONInconsistentIsCompleteExitZero(t *testing.T) {
	const org = "acme"
	path, cp := writeInconsistentArchive(t)

	code, stdout, stderr := runDispatch(t, jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...)
	if code != 0 || stderr != "" {
		t.Fatalf("inconsistent review code=%d stderr=%q", code, stderr)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(stdout), &obj); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if obj["consistent"] != false {
		t.Fatalf("consistent = %v, want false", obj["consistent"])
	}
	orig := jsonDecisionBlock(t, obj, "original")
	recomp := jsonDecisionBlock(t, obj, "recomputed")
	if orig["allowed"] != false || orig["reason"] != "matched deny policy" {
		t.Fatalf("original not preserved: %v", orig)
	}
	if recomp["allowed"] != true || recomp["reason"] != "matched allow policy" {
		t.Fatalf("recomputed must replay the v1 allow: %v", recomp)
	}
	if obj["target_sequence"] != float64(2) {
		t.Fatalf("target_sequence = %v", obj["target_sequence"])
	}
}

// TestReviewJSONNilAndEmptyMatchedBothArray covers rendering of the two
// empty-hit shapes: nil and an empty matched list both serialize as [].
// Whether the two are consistent is the library's decision (unchanged by
// format); the renderer merely reflects the verdict it is given.
func TestReviewJSONNilAndEmptyMatchedBothArray(t *testing.T) {
	for _, matched := range [][]string{nil, {}} {
		r := darksafe.OfflineDecisionReview{
			Seq:        3,
			Original:   darksafe.Decision{Allowed: false, Reason: "no matching allow policy", Matched: matched, Version: 2},
			Recomputed: darksafe.Decision{Allowed: false, Reason: "no matching allow policy", Matched: matched, Version: 2},
			Consistent: true,
		}
		payload, err := renderReviewJSON(r)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(payload, &obj); err != nil {
			t.Fatalf("nil/empty output not JSON: %v\n%s", err, payload)
		}
		for _, key := range []string{"original", "recomputed"} {
			got, ok := jsonDecisionBlock(t, obj, key)["matched_policies"].([]any)
			if !ok || len(got) != 0 {
				t.Fatalf("%s.matched_policies = %v, want []", key, jsonDecisionBlock(t, obj, key)["matched_policies"])
			}
		}
		if obj["consistent"] != true {
			t.Fatalf("nil/empty payload must carry the library verdict; payload=%s", payload)
		}
	}
}

// TestReviewJSONRendererRoundTripsEveryStringKind drives the renderer
// directly with reasons and policy identifiers spanning the full byte
// space, and recovers the exact original bytes through the documented
// representation.
func TestReviewJSONRendererRoundTripsEveryStringKind(t *testing.T) {
	const (
		loneFF   = "理\xff由"
		loneFE   = " 策\xfe略 " // leading/trailing spaces around the invalid byte
		genuine  = "因" + string(rune(0xFFFD)) + "子"
		printed  = `说"明\分` + "\t\n\r" + "隔 中文"
		newlines = "\n\n"
	)
	r := darksafe.OfflineDecisionReview{
		Seq: 5,
		Original: darksafe.Decision{
			Allowed: false, Reason: loneFF,
			Matched: []string{loneFF, loneFE, genuine, printed, newlines}, Version: 9,
		},
		Recomputed: darksafe.Decision{
			Allowed: true, Reason: printed,
			Matched: []string{"p\xff", "p\xfe", "p" + string(rune(0xFFFD))}, Version: 9,
		},
		Consistent: false,
	}
	payload, err := renderReviewJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	// The raw stream is pure UTF-8 JSON: invalid bytes must never appear
	// literally, so object boundaries cannot be corrupted by control or
	// non-UTF-8 content.
	if !json.Valid(payload) {
		t.Fatalf("output is not valid JSON:\n%s", payload)
	}
	for _, bad := range []byte{0x00, 0xff, 0xfe} {
		if bytes.IndexByte(payload, bad) >= 0 {
			t.Fatalf("raw byte %#x leaked into JSON:\n%s", bad, payload)
		}
	}
	// Mandatory escapes for the printable tricky string are present.
	for _, frag := range []string{`\"`, `\\`, `\t`, `\n`, `\r`} {
		if !bytes.Contains(payload, []byte(frag)) {
			t.Fatalf("JSON missing escape %s:\n%s", frag, payload)
		}
	}
	// Ordinary Chinese stays readable, not \u-escaped.
	if !bytes.Contains(payload, []byte("中文")) {
		t.Fatalf("ordinary Chinese should stay readable:\n%s", payload)
	}

	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		t.Fatal(err)
	}
	orig := jsonDecisionBlock(t, obj, "original")
	if got := recoverStringBytes(t, orig["reason"]); !bytes.Equal(got, []byte(loneFF)) {
		t.Fatalf("reason round trip = %v, want %v", got, []byte(loneFF))
	}
	wantMatched := []string{loneFF, loneFE, genuine, printed, newlines}
	gotList := orig["matched_policies"].([]any)
	if len(gotList) != len(wantMatched) {
		t.Fatalf("matched len = %d, want %d", len(gotList), len(wantMatched))
	}
	for i, want := range wantMatched {
		if got := recoverStringBytes(t, gotList[i]); !bytes.Equal(got, []byte(want)) {
			t.Fatalf("matched[%d] round trip = %v, want %v", i, got, []byte(want))
		}
	}

	// The critical distinguishability guarantee: 0xFF, 0xFE and a real
	// U+FFFD take three different representations and decode back to three
	// different byte strings.
	recompMatched := jsonDecisionBlock(t, obj, "recomputed")["matched_policies"].([]any)
	ff := recoverStringBytes(t, recompMatched[0])
	fe := recoverStringBytes(t, recompMatched[1])
	uffd := recoverStringBytes(t, recompMatched[2])
	if !bytes.Equal(ff, []byte("p\xff")) || !bytes.Equal(fe, []byte("p\xfe")) ||
		!bytes.Equal(uffd, []byte("p"+string(rune(0xFFFD)))) {
		t.Fatalf("raw-byte recovery failed: ff=%v fe=%v uffd=%v", ff, fe, uffd)
	}
	if bytes.Equal(ff, fe) || bytes.Equal(ff, uffd) || bytes.Equal(fe, uffd) {
		t.Fatal("0xFF, 0xFE and U+FFFD collapsed to the same bytes")
	}
	// On the wire the two invalid bytes are base64 objects, the genuine rune
	// is a plain JSON string.
	if _, isObj := recompMatched[0].(map[string]any); !isObj {
		t.Fatalf("0xFF must be a base64 object, got %T", recompMatched[0])
	}
	if asStr, ok := recompMatched[2].(string); !ok || !strings.ContainsRune(asStr, '\uFFFD') {
		t.Fatalf("genuine U+FFFD must stay a readable JSON string, got %T", recompMatched[2])
	}
}

// writeRawIDArchive publishes one allow policy whose ID is exactly id and
// records an allowed decision matching it.
func writeRawIDArchive(t *testing.T, org, id string) (string, darksafe.Checkpoint) {
	t.Helper()
	s := darksafe.NewStore()
	s.Publish(org, 0, []darksafe.Policy{
		{ID: id, Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
	})
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r", Scope: "org/a"},
		Action:   "read",
	})
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	arc, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "raw.bin")
	if err := os.WriteFile(p, arc, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, cp
}

// TestReviewJSONEndToEndDistinguishesRawBytes proves the byte preservation
// survives the real command + archive round trip for the three conflatable
// cases.
func TestReviewJSONEndToEndDistinguishesRawBytes(t *testing.T) {
	const org = "raw org"
	recovered := map[string]string{}
	for name, id := range map[string]string{
		"ff":   "p\xff",
		"fe":   "p\xfe",
		"uffd": "p" + string(rune(0xFFFD)),
	} {
		path, cp := writeRawIDArchive(t, org, id)
		_, obj, raw := parseJSONObject(t, jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint))
		if strings.IndexByte(raw, 0xff) >= 0 || strings.IndexByte(raw, 0xfe) >= 0 {
			t.Fatalf("%s: invalid byte leaked into JSON text", name)
		}
		matched := jsonDecisionBlock(t, obj, "recomputed")["matched_policies"].([]any)
		recovered[name] = string(recoverStringBytes(t, matched[0]))
	}
	if recovered["ff"] != "p\xff" {
		t.Fatalf("ff recovered as %q", recovered["ff"])
	}
	if recovered["fe"] != "p\xfe" {
		t.Fatalf("fe recovered as %q", recovered["fe"])
	}
	if recovered["uffd"] != "p"+string(rune(0xFFFD)) {
		t.Fatalf("uffd recovered as %q", recovered["uffd"])
	}
	if recovered["ff"] == recovered["fe"] || recovered["ff"] == recovered["uffd"] ||
		recovered["fe"] == recovered["uffd"] {
		t.Fatalf("distinct bytes collapsed: %q %q %q", recovered["ff"], recovered["fe"], recovered["uffd"])
	}
}

// TestReviewJSONFailurePrintsNothing covers both failure families under
// --json: usage/read errors exit 2 and material/target/version errors exit
// 1, every one with empty stdout so no partial object is ever delivered.
func TestReviewJSONFailurePrintsNothing(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	dir := filepath.Dir(path)

	garbage := filepath.Join(dir, "garbage-json.bin")
	if err := os.WriteFile(garbage, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), good...)
	corrupt[len(corrupt)-40] ^= 0x01
	corruptPath := filepath.Join(dir, "corrupt-json.bin")
	if err := os.WriteFile(corruptPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	// Corruption in a record AFTER an early target: even with --json the
	// whole chain must fail and no partial object may be delivered.
	latePath, lateCP := buildLaterCorruptionArchive(t)

	// Missing-historical-version chain (valid framing, version 5 absent).
	missingVersionPath, missingCP := writeMissingVersionArchive(t)

	exit2 := [][]string{
		// Missing a required flag even though --json is present.
		{"review", "--json", "--archive", path, "--org", org,
			"--seq", "2", "--end-seq", strconv.Itoa(cp.EndSeq)},
		// Unreadable file.
		jsonReviewArgs(filepath.Join(dir, "does-not-exist.bin"), org, 2, cp.EndSeq, cp.Fingerprint),
		// Malformed --json value is itself a usage error.
		append(jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint), "--json=maybe"),
	}
	for i, args := range exit2 {
		code, out, errOut := runDispatch(t, args...)
		if code != 2 || out != "" || errOut == "" {
			t.Fatalf("exit2 case %d: code=%d stdout=%q stderr=%q", i, code, out, errOut)
		}
	}

	exit1 := [][]string{
		jsonReviewArgs(garbage, org, 2, cp.EndSeq, cp.Fingerprint),
		jsonReviewArgs(corruptPath, org, 2, cp.EndSeq, cp.Fingerprint),
		jsonReviewArgs(path, org, 99, cp.EndSeq, cp.Fingerprint),
		jsonReviewArgs(path, org, 1, cp.EndSeq, cp.Fingerprint),
		jsonReviewArgs(path, "globex", 2, cp.EndSeq, cp.Fingerprint),
		jsonReviewArgs(latePath, org, 2, lateCP.EndSeq, lateCP.Fingerprint),
		jsonReviewArgs(missingVersionPath, org, 1, missingCP.EndSeq, missingCP.Fingerprint),
	}
	for i, args := range exit1 {
		code, out, errOut := runDispatch(t, args...)
		if code != 1 || out != "" || errOut == "" {
			t.Fatalf("exit1 case %d: code=%d stdout=%q stderr=%q", i, code, out, errOut)
		}
	}
}

// TestReviewJSONFlagForms covers the accepted spellings and, crucially,
// that --json=false restores the default text report.
func TestReviewJSONFlagForms(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	base := reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)

	t.Run("bare flag before others does not swallow seq", func(t *testing.T) {
		args := append([]string{"review", "--json"}, base[1:]...)
		in, wantHelp, err := parseReviewArgs(args)
		if err != nil || wantHelp || !in.json {
			t.Fatalf("parse = %+v, help=%v, err=%v", in, wantHelp, err)
		}
		if in.seq != 2 {
			t.Fatalf("--json consumed --seq's value: seq=%d", in.seq)
		}
	})
	t.Run("equals forms", func(t *testing.T) {
		for _, tc := range []struct {
			v    string
			want bool
			ok   bool
		}{
			{"true", true, true}, {"false", false, true},
			{"1", true, true}, {"0", false, true}, {"maybe", false, false},
		} {
			args := append(append([]string{}, base...), "--json="+tc.v)
			in, _, err := parseReviewArgs(args)
			if tc.ok && (err != nil || in.json != tc.want) {
				t.Fatalf("--json=%s: in.json=%v err=%v", tc.v, in.json, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("--json=%s must be rejected", tc.v)
			}
		}
	})
	t.Run("json=false restores text report", func(t *testing.T) {
		args := append(append([]string{}, base...), "--json=false")
		code, out, errOut := runDispatch(t, args...)
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
		if !strings.Contains(out, "target sequence: 2") || strings.Contains(out, "\"target_sequence\"") {
			t.Fatalf("--json=false must print the text report:\n%s", out)
		}
	})
}

// TestReviewJSONIsReadOnly ensures JSON mode leaves the archive untouched.
func TestReviewJSONIsReadOnly(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runDispatch(t, jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...); code != 0 {
		t.Fatalf("review: %s", errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("--json modified the archive file")
	}
}

// TestReviewJSONHelpStillDocumentsFlag verifies the help entry point keeps
// documenting JSON and the existing required inputs.
func TestReviewJSONHelpStillDocumentsFlag(t *testing.T) {
	code, out, errOut := runDispatch(t, "review", "--json", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{"--json", "base64", "--archive", "Exit codes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}

// buildLaterCorruptionArchive creates a chain where the target is the early
// decision at seq 2 but a later record (seq 4, subject "late-subject") is
// tampered with a recomputed frame checksum. Decoding succeeds; the whole
// chain validation must reject it.
func buildLaterCorruptionArchive(t *testing.T) (string, darksafe.Checkpoint) {
	t.Helper()
	const org = "acme"
	s := darksafe.NewStore()
	s.Publish(org, 0, []darksafe.Policy{
		{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
	})
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
		Action:   "read",
	})
	s.Publish(org, 1, nil)
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "late-subject"},
		Resource: darksafe.Resource{ID: "r9", Scope: "org/a"},
		Action:   "read",
	})
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	tampered := reframeArchive(t, good, func(payload []byte) {
		idx := bytes.Index(payload, []byte("late-subject"))
		if idx < 0 {
			t.Fatal("later record subject not located")
		}
		payload[idx] ^= 0x01
	})
	path := filepath.Join(t.TempDir(), "late-damage-json.bin")
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cp
}

// writeMissingVersionArchive creates a valid one-record chain whose decision
// names version 5, which has no policy change record in the material.
func writeMissingVersionArchive(t *testing.T) (string, darksafe.Checkpoint) {
	t.Helper()
	const org = "acme"
	rec := darksafe.AuditRecord{
		Org: org, Seq: 1, Kind: darksafe.AuditDecision,
		Decision: &darksafe.DecisionRecord{
			Request: darksafe.OrgRequest{
				SubjectOrg: org, ResourceOrg: org,
				Subject:  darksafe.Subject{ID: "u1"},
				Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
				Action:   "read",
			},
			Decision: darksafe.Decision{
				Allowed: false, Reason: "no matching allow policy", Matched: nil, Version: 5,
			},
		},
		PrevFingerprint: genesisFingerprintHex(org),
	}
	rec.Fingerprint = envelopeFingerprint(syntheticEnvelope{
		Org: rec.Org, Seq: rec.Seq, Kind: rec.Kind,
		Decision: rec.Decision, PrevFingerprint: rec.PrevFingerprint,
	})
	cp := darksafe.Checkpoint{Org: org, EndSeq: 1, Fingerprint: rec.Fingerprint}
	arc, err := darksafe.EncodeAuditArchive(org, []darksafe.AuditRecord{rec}, cp)
	if err != nil {
		t.Fatalf("chain must encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "missing-version-json.bin")
	if err := os.WriteFile(path, arc, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cp
}
