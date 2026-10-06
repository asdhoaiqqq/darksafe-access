package darksafe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the regression net for what a caller sees when ONE document
// carries BOTH a malformed number (1e, 1E+, 0., 01 — a missing exponent
// digit, a missing fraction, or an illegal leading zero) AND corrupt text
// (invalid UTF-8 bytes or an unpaired-surrogate \uXXXX escape). The two
// existing groups pin each failure in isolation (big_numbers_test.go and
// strict_text_test.go) and duplicate_malformed_order_test.go pins the
// duplicate-versus-format meeting order; these tests pin the
// number-versus-text meeting order, so reworking the reading logic can
// never let a later string's text problem mask an earlier number format
// error.
//
// The rule is file order over the raw structural walk:
//
//   - A malformed number read before any corrupt text is a JSON format
//     error at the number's own position; the walk must not skip past it
//     and report a later string's invalid UTF-8 or unpaired escape. This
//     holds in unknown extra fields, nested objects and arrays just as in
//     known fields — a field's not participating in release rules never
//     excuses its format error.
//   - Corrupt text read before the malformed number keeps its own verdict,
//     with the invalid-UTF-8 and unpaired-escape wordings (and positions)
//     still distinct.
//   - The duplicate-member meeting order is unchanged: a duplicate whose
//     second occurrence was read before the malformed number still names
//     the member and its object; one appearing after the number cannot
//     override the format error.
//
// Legal numbers — 1e400, a tiny 1e-400, a huge integer literal — are not
// format errors anywhere, and in unknown fields they are ignored exactly
// as before.

// malformedNumberBases are the malformed number spellings this meeting
// order covers: missing exponent digits, a missing fraction, and an
// illegal leading zero.
var malformedNumberBases = []string{"1e", "1E+", "0.", "01"}

func TestParse_MalformedNumberBeatsLaterTextError(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	for _, num := range malformedNumberBases {
		cases := []struct {
			name string
			doc  string
		}{
			{
				name: "unknown top-level field before unpaired escape",
				doc:  `{` + base + `,"clusters":[],"limit":` + num + `,"note":"\uD800"}`,
			},
			{
				name: "unknown top-level field before invalid UTF-8",
				doc:  `{` + base + `,"clusters":[],"limit":` + num + `,"note":"` + badUTF8 + `"}`,
			},
			{
				name: "nested unknown object before unpaired escape",
				doc:  `{` + base + `,"clusters":[],"meta":{"limit":` + num + `},"note":"\uD800"}`,
			},
			{
				name: "unknown array element before invalid UTF-8",
				doc:  `{` + base + `,"clusters":[],"items":[1,` + num + `],"note":"` + badUTF8 + `"}`,
			},
			{
				name: "cluster extra field before unpaired escape in later member name",
				doc: `{` + base + `,"clusters":[{"id":"x","w":` + num + `}],` +
					`"me\uD800ta":1}`,
			},
			{
				name: "deeply nested unknown value before unpaired escape",
				doc:  `{` + base + `,"clusters":[],"a":{"b":[{"c":` + num + `}]},"note":"\uDC00"}`,
			},
		}
		for _, tc := range cases {
			t.Run(num+"/"+tc.name, func(t *testing.T) {
				err := parseComboFail(t, tc.doc)
				assertJSONFormatError(t, err)
				if strings.Contains(err.Error(), "未配对") || strings.Contains(err.Error(), "UTF-8") {
					t.Fatalf("later text problem must not mask the number format error, got %v", err)
				}
			})
		}
	}
}

// TestParse_TextErrorBeforeMalformedNumberStillWins covers the other half:
// corrupt text read before the malformed number keeps its own verdict —
// invalid UTF-8 and unpaired escapes stay distinct and keep their positions.
func TestParse_TextErrorBeforeMalformedNumberStillWins(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name     string
		doc      string
		wantKind string // "UTF-8" or "未配对"
		wantPath string
	}{
		{
			name:     "invalid UTF-8 value before malformed number",
			doc:      `{` + base + `,"clusters":[],"note":"` + badUTF8 + `","limit":1e}`,
			wantKind: "UTF-8",
			wantPath: "$.note",
		},
		{
			name:     "unpaired escape value before malformed number",
			doc:      `{` + base + `,"clusters":[],"note":"\uD800","limit":1e}`,
			wantKind: "未配对",
			wantPath: "$.note",
		},
		{
			name:     "invalid UTF-8 member name before malformed number",
			doc:      `{` + base + `,"clusters":[],"me` + badUTF8 + `ta":1,"limit":0.}`,
			wantKind: "UTF-8",
			wantPath: "$",
		},
		{
			name:     "unpaired escape in nested object before malformed number",
			doc:      `{` + base + `,"clusters":[],"meta":{"note":"\uDC00"},"items":[1e]}`,
			wantKind: "未配对",
			wantPath: "$.meta.note",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseComboFail(t, tc.doc)
			msg := err.Error()
			if strings.Contains(msg, "JSON 格式错误") {
				t.Fatalf("earlier text problem must beat the later malformed number, got %v", err)
			}
			if !strings.Contains(msg, tc.wantKind) {
				t.Fatalf("error %q should identify %q", msg, tc.wantKind)
			}
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not contain path %q", msg, tc.wantPath)
			}
		})
	}
}

// TestParse_DuplicateOrderAroundMalformedNumberUnchanged pins the duplicate
// meeting order in the presence of both a malformed number and corrupt
// text: a duplicate read before the number still names its member and
// object; one read after the number cannot override the format error.
func TestParse_DuplicateOrderAroundMalformedNumberUnchanged(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`

	// Duplicate first, then the malformed number, then corrupt text: the
	// already-decided duplicate wins, naming the member and owning object.
	err := parseComboFail(t, `{`+base+`,"clusters":[],"meta":{"x":1,"x":2},`+
		`"limit":1e,"note":"\uD800"}`)
	assertDuplicateError(t, err, "x", "$.meta")

	// Malformed number first, duplicate later: the format error wins; the
	// walk must not skip the bad number to fish out the later duplicate.
	err = parseComboFail(t, `{`+base+`,"clusters":[],"limit":1e,"meta":{"x":1,"x":2}}`)
	assertJSONFormatError(t, err)

	// Corrupt text first, duplicate later, malformed number last: the text
	// verdict still leads.
	err = parseComboFail(t, `{`+base+`,"clusters":[],"note":"\uD800",`+
		`"meta":{"x":1,"x":2},"limit":1e}`)
	if msg := err.Error(); !strings.Contains(msg, "未配对") {
		t.Fatalf("expected the earlier text problem to lead, got %v", err)
	}
}

// TestParse_LegalNumbersStillAcceptedAroundFix locks the compatibility
// side: legal numbers beyond float64 range — 1e400, a tiny 1e-400, a huge
// integer literal — are well-formed JSON everywhere, are ignored in
// unknown fields (the plan matches the document without them), and in
// batchSize keep their ordinary field error rather than a format error.
func TestParse_LegalNumbersStillAcceptedAroundFix(t *testing.T) {
	base := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3", "tags": {"env": "prod"}},
			{"id": "c1", "tags": {"env": "dev"}},
			{"id": "c2", "disabled": true, "tags": {"env": "prod"}}
		],
		"include": [{"env": "prod"}]
	}`
	basePlan, err := MakeReleasePlan(parsePlan(t, base))
	if err != nil {
		t.Fatal(err)
	}
	withExtra := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3", "tags": {"env": "prod"}},
			{"id": "c1", "tags": {"env": "dev"}},
			{"id": "c2", "disabled": true, "tags": {"env": "prod"}}
		],
		"include": [{"env": "prod"}],
		"meta": {"limit": 1e400, "tiny": 1e-400, "big": 99999999999999999999},
		"notes": [1e400, {"deep": [1e-400]}]
	}`
	in, err := ParseReleaseInput([]byte(withExtra))
	if err != nil {
		t.Fatalf("legal out-of-range numbers in extra fields must parse: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("legal config must still plan: %v", err)
	}
	if planSummary(plan) != planSummary(basePlan) {
		t.Fatalf("plan differs from the document without the extra fields:\n got %+v\nwant %+v",
			plan, basePlan)
	}

	// The same legal spellings in batchSize stay a field error, never a
	// JSON format error.
	for _, num := range []string{"1e400", "1e-400", "99999999999999999999"} {
		doc := `{"app":"a","revision":"r","image":"i","batchSize":` + num + `,"clusters":[{"id":"x"}]}`
		_, err := ParseReleaseInput([]byte(doc))
		if err == nil {
			t.Fatalf("batchSize %s must be rejected", num)
		}
		if strings.Contains(err.Error(), "JSON 格式错误") {
			t.Fatalf("batchSize %s must not be a JSON format error, got %v", num, err)
		}
		if want := `字段 "batchSize" 必须是正整数`; err.Error() != want {
			t.Fatalf("batchSize %s error = %q, want %q", num, err.Error(), want)
		}
	}
}

// TestPlanCLI_MalformedNumberBeforeTextErrorFailsAsJSONError drives the CLI
// with a malformed number ahead of an unpaired escape: exit code 1, an
// empty stdout, and the JSON format error — not the later text problem —
// on stderr.
func TestPlanCLI_MalformedNumberBeforeTextErrorFailsAsJSONError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"payments","revision":"v1","image":"img","batchSize":1,` +
		`"clusters":[],"limit":1e,"note":"\uD800"}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("stderr should report the JSON format error, got %q", stderr)
	}
	if strings.Contains(stderr, "未配对") {
		t.Fatalf("stderr must not blame the later text problem, got %q", stderr)
	}
}
