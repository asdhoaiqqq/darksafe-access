// Tests for the review command's --json machine-readable report. The JSON
// object must be the sole thing on stdout, keep field meanings identical to
// the text report, preserve matched-policy order, stay empty on every
// failure, and represent arbitrary (possibly non-UTF-8) string bytes under
// the documented representation rule so a lone 0xFF, a lone 0xFE and a real
// U+FFFD never collapse.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// jsonDecision mirrors one decision object in the --json report.
type jsonDecision struct {
	Allowed         bool     `json:"allowed"`
	Reason          string   `json:"reason"`
	MatchedPolicies []string `json:"matched_policies"`
	PolicyVersion   int      `json:"policy_version"`
}

// jsonReview mirrors the whole --json report object.
type jsonReview struct {
	TargetSeq  int          `json:"target_seq"`
	Original   jsonDecision `json:"original"`
	Recomputed jsonDecision `json:"recomputed"`
	Consistent bool         `json:"consistent"`
}

// withJSON appends --json to a fresh copy of an argument slice.
func withJSON(base []string) []string {
	return append(append([]string{}, base...), "--json")
}

// decodeSingleJSONReview asserts out is valid JSON holding exactly one value
// (trailing whitespace/newline allowed, never a second value or text), and
// decodes that object.
func decodeSingleJSONReview(t *testing.T, out string) jsonReview {
	t.Helper()
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("JSON output must start with the object, got %q", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("output is not valid JSON: %q", out)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var r jsonReview
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("decode review object: %v; output=%q", err, out)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("output must hold exactly one JSON value, got extra %q after object", extra)
	}
	return r
}

// TestReviewJSONHappyPathObject verifies the happy path in JSON mode: exit 0,
// nothing on stderr, one complete object whose fields map one-to-one onto the
// text report, with target sequence kept separate from policy version.
func TestReviewJSONHappyPathObject(t *testing.T) {
	const org = "acme payments"
	path, cp := fixtureChain(t, org)

	code, out, errOut := runDispatch(t, withJSON(reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint))...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if errOut != "" {
		t.Fatalf("unexpected stderr: %q", errOut)
	}
	// No text-report scaffolding may be mixed in.
	for _, banned := range []string{"target sequence:", "original decision:", "recomputed decision:", "consistent:"} {
		if strings.Contains(out, banned) {
			t.Fatalf("JSON output mixed in text report fragment %q:\n%s", banned, out)
		}
	}
	r := decodeSingleJSONReview(t, out)
	if r.TargetSeq != 2 {
		t.Fatalf("target_seq = %d, want 2", r.TargetSeq)
	}
	for name, d := range map[string]jsonDecision{"original": r.Original, "recomputed": r.Recomputed} {
		if !d.Allowed {
			t.Fatalf("%s allowed = false, want true", name)
		}
		if d.Reason != "matched allow policy" {
			t.Fatalf("%s reason = %q", name, d.Reason)
		}
		if len(d.MatchedPolicies) != 1 || d.MatchedPolicies[0] != "p1 ledger" {
			t.Fatalf("%s matched = %#v, want [p1 ledger]", name, d.MatchedPolicies)
		}
		if d.PolicyVersion != 1 {
			t.Fatalf("%s policy_version = %d, want 1; target_seq must stay %d",
				name, d.PolicyVersion, r.TargetSeq)
		}
	}
	if !r.Consistent {
		t.Fatal("consistent = false, want true")
	}
}

// TestReviewJSONUsesRecordedVersion proves a later publish does not change
// the recomputed object: seq 2 replays v1 allow, seq 4 replays v2 default
// deny with an explicit empty matched list.
func TestReviewJSONUsesRecordedVersion(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)

	code, early, _ := runDispatch(t, withJSON(reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint))...)
	if code != 0 {
		t.Fatalf("seq 2 exit = %d", code)
	}
	re := decodeSingleJSONReview(t, early)
	if !re.Original.Allowed || re.Original.PolicyVersion != 1 ||
		re.Original.MatchedPolicies[0] != "p1 ledger" {
		t.Fatalf("early object must replay v1 allow: %+v", re)
	}

	code, late, _ := runDispatch(t, withJSON(reviewArgs(path, org, 4, cp.EndSeq, cp.Fingerprint))...)
	if code != 0 {
		t.Fatalf("seq 4 exit = %d", code)
	}
	rl := decodeSingleJSONReview(t, late)
	if rl.Original.Allowed || rl.Original.Reason != "no matching allow policy" ||
		rl.Original.PolicyVersion != 2 {
		t.Fatalf("late object must replay v2 default deny: %+v", rl)
	}
	if rl.Original.MatchedPolicies == nil || len(rl.Original.MatchedPolicies) != 0 {
		t.Fatalf("no hits must encode as an empty, present list, got %#v", rl.Original.MatchedPolicies)
	}
	if !bytes.Contains([]byte(late), []byte(`"matched_policies":[]`)) {
		t.Fatalf("empty hit list must spell [], got %q", late)
	}
}

// TestReviewJSONInconsistentButValidExitsZero covers the central rule: a
// chain that verifies but whose preserved and recomputed decisions differ is
// a complete report with consistent=false and exit 0, never file damage.
func TestReviewJSONInconsistentButValidExitsZero(t *testing.T) {
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
	records := []darksafe.AuditRecord{rec1, rec2}
	cp := darksafe.Checkpoint{Org: org, EndSeq: 2, Fingerprint: rec2.Fingerprint}
	arc, err := darksafe.EncodeAuditArchive(org, records, cp)
	if err != nil {
		t.Fatalf("synthetic chain must encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "inconsistent.bin")
	if err := os.WriteFile(path, arc, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runDispatch(t, withJSON(reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint))...)
	if code != 0 {
		t.Fatalf("legitimate-but-inconsistent review must exit 0, got %d: %s", code, errOut)
	}
	r := decodeSingleJSONReview(t, out)
	if r.Consistent {
		t.Fatal("consistent must be false")
	}
	if r.Original.Allowed || r.Original.Reason != "matched deny policy" {
		t.Fatalf("original decision must be preserved verbatim: %+v", r.Original)
	}
	if !r.Recomputed.Allowed || r.Recomputed.Reason != "matched allow policy" {
		t.Fatalf("recomputed decision must be the v1 allow: %+v", r.Recomputed)
	}
	if r.TargetSeq != 2 {
		t.Fatalf("target_seq = %d, want 2", r.TargetSeq)
	}
}

// TestReviewJSONNilAndEmptyMatchedEncodeSame checks the encoder treats a nil
// hit list and a non-nil empty list identically (both []), matching the
// library's nil/empty equivalence that drives the consistency verdict.
func TestReviewJSONNilAndEmptyMatchedEncodeSame(t *testing.T) {
	encode := func(matched []string) string {
		var b strings.Builder
		writeReviewJSON(&b, darksafe.OfflineDecisionReview{
			Seq:        3,
			Original:   darksafe.Decision{Matched: matched, Reason: "x"},
			Recomputed: darksafe.Decision{Matched: matched, Reason: "x"},
			Consistent: true,
		})
		return b.String()
	}
	nilOut := encode(nil)
	emptyOut := encode([]string{})
	if nilOut != emptyOut {
		t.Fatalf("nil and empty matched lists must encode identically:\n nil=%s\nempty=%s", nilOut, emptyOut)
	}
	if !strings.Contains(nilOut, `"matched_policies":[]`) {
		t.Fatalf("both shapes must spell [], got %s", nilOut)
	}

	// End to end: the rolled-back empty-version denial carries a nil list.
	path, cp := emptyRollbackFixture(t, "acme", nil)
	code, out, errOut := runDispatch(t, withJSON(reviewArgs(path, "acme", 5, cp.EndSeq, cp.Fingerprint))...)
	if code != 0 {
		t.Fatalf("empty-version review: %d %s", code, errOut)
	}
	r := decodeSingleJSONReview(t, out)
	if len(r.Original.MatchedPolicies) != 0 || !r.Consistent || r.Original.PolicyVersion != 3 {
		t.Fatalf("empty v3 denial object wrong: %+v", r)
	}
}

// TestReviewJSONPreservesMatchedOrder ensures hit identifiers are emitted in
// recorded order with no sorting or dedup at the rendering layer.
func TestReviewJSONPreservesMatchedOrder(t *testing.T) {
	var b strings.Builder
	writeReviewJSON(&b, darksafe.OfflineDecisionReview{
		Seq: 9,
		Original: darksafe.Decision{
			Allowed: false, Reason: "matched deny policy",
			Matched: []string{"z9", "a1", "m2"}, Version: 4,
		},
		Recomputed: darksafe.Decision{Matched: []string{"z9", "a1", "m2"}, Version: 4},
	})
	out := b.String()
	want := `"matched_policies":["z9","a1","m2"]`
	if !strings.Contains(out, want) {
		t.Fatalf("matched order must be preserved verbatim, got %s", out)
	}
}

// uEscape builds a JSON \u00XX escape spelling without putting the literal
// trigger sequence in this source file: documentedUEscape("ff") is the
// six-character wire text backslash-u-0-0-f-f.
func uEscape(hexTwo string) string {
	return `\` + "u00" + hexTwo
}

// jsonRawIDArchive builds a two-record archive whose single v1 allow policy
// has the given identifier bytes, so non-UTF-8 identifiers reach the report.
func jsonRawIDArchive(t *testing.T, id string) (string, darksafe.Checkpoint) {
	t.Helper()
	const org = "raw org"
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

// TestReviewJSONRawByteCasesStayDistinct drives real archives carrying a lone
// 0xFF, a lone 0xFE and a genuine U+FFFD in the matched identifier. On the
// wire and after a standard parse the three must remain pairwise distinct,
// every object must be valid JSON, and the wire spelling must follow the
// documented rule (invalid byte -> \u00XX; real replacement character -> its
// literal UTF-8 bytes).
func TestReviewJSONRawByteCasesStayDistinct(t *testing.T) {
	cases := map[string]struct {
		id      string
		parsed0 rune // rune a standard decoder yields for the identifier tail
	}{
		"ff":   {"p\xff", 0xFF},
		"fe":   {"p\xfe", 0xFE},
		"uffd": {"p\ufffd", 0xFFFD},
	}
	wire := map[string]string{}
	parsed := map[string]string{}
	for name, tc := range cases {
		path, cp := jsonRawIDArchive(t, tc.id)
		code, out, errOut := runDispatch(t, withJSON(reviewArgs(path, "raw org", 2, cp.EndSeq, cp.Fingerprint))...)
		if code != 0 {
			t.Fatalf("%s review failed: %s", name, errOut)
		}
		if !json.Valid([]byte(out)) {
			t.Fatalf("%s output is not valid JSON: %q", name, out)
		}
		r := decodeSingleJSONReview(t, out)
		wire[name] = out
		parsed[name] = r.Original.MatchedPolicies[0]
		if got := []rune(r.Original.MatchedPolicies[0]); len(got) != 2 || got[0] != 'p' || got[1] != tc.parsed0 {
			t.Fatalf("%s parsed tail rune = %q, want U+%04X", name, r.Original.MatchedPolicies[0], tc.parsed0)
		}
	}

	// Pairwise distinct both on the wire and after a standard parse.
	names := []string{"ff", "fe", "uffd"}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if wire[names[i]] == wire[names[j]] {
				t.Fatalf("wire outputs for %s and %s collapsed", names[i], names[j])
			}
			if parsed[names[i]] == parsed[names[j]] {
				t.Fatalf("parsed strings for %s and %s collapsed", names[i], names[j])
			}
		}
	}

	// Documented wire spellings.
	if !strings.Contains(wire["ff"], `"p`+uEscape("ff")) {
		t.Fatalf("0xFF must be spelt "+uEscape("ff")+", got %q", wire["ff"])
	}
	if !strings.Contains(wire["fe"], `"p`+uEscape("fe")) {
		t.Fatalf("0xFE must be spelt "+uEscape("fe")+", got %q", wire["fe"])
	}
	if !strings.Contains(wire["uffd"], "p"+string(rune(0xFFFD))) {
		t.Fatalf("real U+FFFD must be emitted literally as EF BF BD, got %q", wire["uffd"])
	}
	replacementUTF8 := []byte{0xEF, 0xBF, 0xBD}
	for _, name := range []string{"ff", "fe"} {
		if bytes.Contains([]byte(wire[name]), replacementUTF8) {
			t.Fatalf("%s output must not silently substitute U+FFFD bytes", name)
		}
	}
	if strings.Contains(wire["uffd"], uEscape("ff")) || strings.Contains(wire["uffd"], uEscape("fe")) {
		t.Fatalf("real U+FFFD must not be spelt as an invalid-byte escape: %q", wire["uffd"])
	}
}

// decodeDocumentedJSONString inverts the representation rule documented for
// --json: it takes a JSON string literal WITHOUT its surrounding quotes and
// reconstructs the original bytes. Every \u00XX escape denotes the single raw
// byte 0xXX (the encoder never uses that spelling for a real rune, which
// would instead appear literally as UTF-8), standard short escapes map to
// their control bytes, and literal bytes pass through unchanged.
func decodeDocumentedJSONString(t *testing.T, lit string) []byte {
	t.Helper()
	out := make([]byte, 0, len(lit))
	for i := 0; i < len(lit); {
		c := lit[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= len(lit) {
			t.Fatal("dangling backslash in encoded string")
		}
		switch e := lit[i+1]; e {
		case '"', '\\', '/':
			out = append(out, e)
			i += 2
		case 'b':
			out = append(out, 0x08)
			i += 2
		case 'f':
			out = append(out, 0x0c)
			i += 2
		case 'n':
			out = append(out, '\n')
			i += 2
		case 'r':
			out = append(out, '\r')
			i += 2
		case 't':
			out = append(out, '\t')
			i += 2
		case 'u':
			if i+6 > len(lit) {
				t.Fatal("truncated unicode escape")
			}
			v, err := strconv.ParseUint(lit[i+2:i+6], 16, 16)
			if err != nil {
				t.Fatalf("bad unicode escape %q: %v", lit[i:i+6], err)
			}
			if v > 0xFF {
				t.Fatalf("encoder must not emit escapes above U+00FF, got U+%04X", v)
			}
			out = append(out, byte(v))
			i += 6
		default:
			t.Fatalf("unexpected escape %q in %q", e, lit)
		}
	}
	return out
}

// TestJSONStringRepresentationRoundTrips exercises writeJSONString directly
// over every category of byte: readable CJK, surrounding whitespace, quote,
// backslash, short-escape controls, other control bytes, DEL, invalid bytes
// 0x80/0xFF/0xFE, and genuine U+00FF and U+FFFD runes. Each encoding must be
// valid JSON, keep control bytes inside the string, and invert under the
// documented rule back to the exact original bytes; the three replacement
// look-alikes must stay distinct.
func TestJSONStringRepresentationRoundTrips(t *testing.T) {
	specials := []string{
		"p\xff", "p\xfe", "p\ufffd",
		"",
		" ",
		"  leading and trailing  \n",
		`quote " and slash \ together`,
		"tabs\tand\rcarriage\nreturns\bbell\fform",
		"ctrl\x00\x01\x1fdel\x7f",
		"invalid\x80mid",
		"中文理由",
		"real-y" + string(rune(0xFF)),      // genuine U+00FF -> bytes C3 BF
		"real-repl" + string(rune(0xFFFD)), // genuine U+FFFD -> bytes EF BF BD
		"mix 中文 \"\\\n\x01\xff" + string(rune(0xFFFD)) + string(rune(0xFF)),
	}
	encoded := make([]string, len(specials))
	for i, s := range specials {
		var b strings.Builder
		writeJSONString(&b, s)
		lit := b.String()
		if !json.Valid([]byte(lit)) {
			t.Fatalf("encoding of % x is not valid JSON: %q", []byte(s), lit)
		}
		encoded[i] = lit
		// Quoted JSON must delimit the value: no raw control byte may appear
		// outside an escape (0x7F is permitted unescaped by JSON and cannot
		// terminate a string).
		inner := lit[1 : len(lit)-1]
		for _, c := range []byte(inner) {
			if c < 0x20 {
				t.Fatalf("raw control byte 0x%02x broke the string boundary for % x", c, []byte(s))
			}
		}
		got := decodeDocumentedJSONString(t, inner)
		if !bytes.Equal(got, []byte(s)) {
			t.Fatalf("documented inverse did not restore bytes:\n want % x\n got  % x\n wire %q", []byte(s), got, lit)
		}
	}

	// The three look-alikes stay pairwise distinct on the wire.
	if encoded[0] == encoded[1] || encoded[0] == encoded[2] || encoded[1] == encoded[2] {
		t.Fatalf("0xFF, 0xFE and U+FFFD encodings collapsed: %q %q %q", encoded[0], encoded[1], encoded[2])
	}
	// And exact wire spellings.
	if encoded[0] != `"p`+uEscape("ff")+`"` {
		t.Fatalf("0xFF wire = %q", encoded[0])
	}
	if encoded[1] != `"p`+uEscape("fe")+`"` {
		t.Fatalf("0xFE wire = %q", encoded[1])
	}
	if encoded[2] != `"p`+"\xef\xbf\xbd"+`"` {
		t.Fatalf("U+FFFD wire = %q", encoded[2])
	}
	// A genuine U+00FF rune is literal C3 BF, never the 0xFF escape.
	yEnc := encoded[len(encoded)-3]
	if strings.Contains(yEnc, uEscape("ff")) || !strings.Contains(yEnc, "\xc3\xbf") {
		t.Fatalf("real U+00FF must be literal C3 BF, got %q", yEnc)
	}
}

// TestReviewJSONFailuresLeaveStdoutEmpty covers exit 1 (archive/target
// material problems, including corruption AFTER an early target, and the
// unavailable historical version) and exit 2 (bad invocation, unreadable
// file) in --json mode: the reason goes to stderr and stdout is empty, so no
// partial JSON can ever be delivered.
func TestReviewJSONFailuresLeaveStdoutEmpty(t *testing.T) {
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

	// Later-record tamper that passes framing but fails whole-chain verify,
	// with the review target (seq 2) before the damage.
	lateTamper := buildLateRecordTamper(t)

	exitOne := []struct {
		name string
		args []string
		want string
	}{
		{"unrecognized archive", reviewArgs(garbage, org, 2, cp.EndSeq, cp.Fingerprint), ""},
		{"corrupted bytes", reviewArgs(corruptPath, org, 2, cp.EndSeq, cp.Fingerprint), ""},
		{"wrong checkpoint", reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint[:len(cp.Fingerprint)-1]+"0"), ""},
		{"target past export", reviewArgs(path, org, 99, cp.EndSeq, cp.Fingerprint), ""},
		{"target is policy change", reviewArgs(path, org, 1, cp.EndSeq, cp.Fingerprint), ""},
		{"later record corrupted", reviewArgs(lateTamper.path, org, 2, lateTamper.endSeq, lateTamper.fingerprint), "invalid audit range"},
		{"historical version missing", reviewArgs(lateTamper.missingVersionPath, org, 1, lateTamper.missingEndSeq, lateTamper.missingFingerprint), "version not found"},
	}
	for _, tc := range exitOne {
		t.Run("exit1/"+tc.name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, withJSON(tc.args)...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%q", code, errOut)
			}
			if errOut == "" {
				t.Fatal("expected a specific reason on stderr")
			}
			if tc.want != "" && !strings.Contains(errOut, tc.want) {
				t.Fatalf("stderr %q must mention %q", errOut, tc.want)
			}
			if out != "" {
				t.Fatalf("stdout must stay empty on failure, got %q", out)
			}
		})
	}

	exitTwo := []struct {
		name string
		args []string
	}{
		{"missing required seq", withJSON(dropReviewFlag(reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint), "--seq"))},
		{"unreadable file", withJSON(reviewArgs(filepath.Join(t.TempDir(), "nope.bin"), org, 2, cp.EndSeq, cp.Fingerprint))},
		{"bad json boolean", append(append([]string{}, reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...), "--json=maybe")},
	}
	for _, tc := range exitTwo {
		t.Run("exit2/"+tc.name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr=%q", code, errOut)
			}
			if errOut == "" {
				t.Fatal("expected a specific reason on stderr")
			}
			if out != "" {
				t.Fatalf("stdout must stay empty on usage failure, got %q", out)
			}
		})
	}
}

// lateTamperFixture groups the artifacts for the two synthetic chains used by
// the failure test.
type lateTamperFixture struct {
	path               string
	endSeq             int
	fingerprint        string
	missingVersionPath string
	missingEndSeq      int
	missingFingerprint string
}

// buildLateRecordTamper creates (1) a valid chain whose later record is
// tampered with framing/checksum fixed so whole-chain verification fails for
// an early target, and (2) a chain whose decision names an unpublished
// version 5.
func buildLateRecordTamper(t *testing.T) lateTamperFixture {
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
	tamperedPath := filepath.Join(t.TempDir(), "late-damage-json.bin")
	if err := os.WriteFile(tamperedPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

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
				Allowed: false, Reason: "no matching allow policy", Version: 5,
			},
		},
		PrevFingerprint: genesisFingerprintHex(org),
	}
	rec.Fingerprint = envelopeFingerprint(syntheticEnvelope{
		Org: rec.Org, Seq: rec.Seq, Kind: rec.Kind,
		Decision: rec.Decision, PrevFingerprint: rec.PrevFingerprint,
	})
	missingRecs := []darksafe.AuditRecord{rec}
	mcp := darksafe.Checkpoint{Org: org, EndSeq: 1, Fingerprint: rec.Fingerprint}
	marc, err := darksafe.EncodeAuditArchive(org, missingRecs, mcp)
	if err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(t.TempDir(), "missing-version-json.bin")
	if err := os.WriteFile(missingPath, marc, 0o600); err != nil {
		t.Fatal(err)
	}

	return lateTamperFixture{
		path: tamperedPath, endSeq: cp.EndSeq, fingerprint: cp.Fingerprint,
		missingVersionPath: missingPath, missingEndSeq: 1, missingFingerprint: rec.Fingerprint,
	}
}

// dropReviewFlag removes a flag and its following value from an arg slice.
func dropReviewFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args)-2)
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// TestReviewJSONFlagForms locks in output selection: default and
// --json=false keep the exact text report, bare --json and --json=true emit
// the object, and an invalid boolean value is an exit-2 usage error.
func TestReviewJSONFlagForms(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	base := reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)

	isText := func(t *testing.T, out string) {
		t.Helper()
		if !strings.Contains(out, "target sequence: 2") || strings.HasPrefix(out, "{") {
			t.Fatalf("expected the unchanged text report, got %q", out)
		}
	}
	isJSON := func(t *testing.T, out string) {
		t.Helper()
		if decodeSingleJSONReview(t, out).TargetSeq != 2 {
			t.Fatalf("expected the JSON object, got %q", out)
		}
	}

	code, out, _ := runDispatch(t, base...)
	if code != 0 {
		t.Fatalf("default mode failed: %d", code)
	}
	isText(t, out)

	code, out, _ = runDispatch(t, append(append([]string{}, base...), "--json=false")...)
	if code != 0 {
		t.Fatalf("--json=false failed: %d", code)
	}
	isText(t, out)

	code, out, _ = runDispatch(t, append(append([]string{}, base...), "--json=true")...)
	if code != 0 {
		t.Fatalf("--json=true failed: %d", code)
	}
	isJSON(t, out)

	code, out, _ = runDispatch(t, append(append([]string{}, base...), "--json")...)
	if code != 0 {
		t.Fatalf("bare --json failed: %d", code)
	}
	isJSON(t, out)
}

// TestReviewJSONIsReadOnly ensures selecting JSON does not rewrite the
// archive or otherwise touch the file.
func TestReviewJSONIsReadOnly(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runDispatch(t, withJSON(reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint))...); code != 0 {
		t.Fatalf("json review: %d %s", code, errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("--json review modified the archive file")
	}
}

// TestReviewJSONHelp ensures the help entry documents the flag, the report
// shape and the string representation rule while keeping every pre-existing
// help marker.
func TestReviewJSONHelp(t *testing.T) {
	code, out, errOut := runDispatch(t, "review", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("review --help: code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{
		"--json", "matched_policies", "policy_version", "target_seq",
		uEscape("XX"), "consistent",
		"--archive", "--fingerprint", "Example:", "Exit codes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("review help missing %q:\n%s", want, out)
		}
	}
}
