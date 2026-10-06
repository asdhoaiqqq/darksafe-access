package darksafe

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This file is the regression net for one document that carries BOTH a
// malformed JSON number AND corrupt text in a later string. The raw
// structural walker used to skip number characters without judging them, so
// it read past "1e" into the following string and reported that string's
// invalid UTF-8 bytes or unpaired surrogate first, hiding the earlier number
// format error. The ordering rule, in file order, is now:
//
//   - A malformed number appearing first (a missing exponent digit such as
//     1e or 1E+, a missing fraction digit such as 0., or an illegal leading
//     zero such as 01) is the JSON format error, in unknown extra fields,
//     nested objects and arrays alike; later corrupt text cannot overtake it.
//   - Corrupt text appearing first keeps its own verdict and position, with
//     the invalid-UTF-8 vs unpaired-surrogate distinction intact.
//   - Duplicate-member ordering is unchanged: a duplicate read before the
//     bad number still wins; a duplicate after it cannot cover the format
//     error.
//   - Legal numbers — including 1e400, tiny legal exponents and integers
//     beyond float64 range — keep being accepted in extra fields and judged
//     only by field rules (e.g. batchSize) where they are used.

// assertTextErrorAt requires err to be the strict-text verdict of the given
// kind ("utf8" or "surrogate"), whose message names path, and never the JSON
// format error: when corrupt text is read first it must stay the reported
// cause even with malformed JSON following it.
func assertTextErrorAt(t *testing.T, err error, kind, path string) {
	t.Helper()
	switch kind {
	case "utf8":
		var iu *invalidUTF8Error
		if !errors.As(err, &iu) {
			t.Fatalf("expected *invalidUTF8Error at %s, got %T: %v", path, err, err)
		}
		if !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("error %q should identify invalid UTF-8", err)
		}
		if strings.Contains(err.Error(), "未配对") {
			t.Fatalf("error %q must not blame Unicode escapes", err)
		}
	case "surrogate":
		var us *unpairedSurrogateError
		if !errors.As(err, &us) {
			t.Fatalf("expected *unpairedSurrogateError at %s, got %T: %v", path, err, err)
		}
		if !strings.Contains(err.Error(), "未配对") {
			t.Fatalf("error %q should name an unpaired Unicode escape", err)
		}
		if strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("error %q must not blame UTF-8 bytes", err)
		}
	default:
		t.Fatalf("unknown text error kind %q", kind)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not contain path %q", err, path)
	}
	if strings.Contains(err.Error(), "JSON 格式错误") {
		t.Fatalf("text problem read first must not be reported as a format error: %v", err)
	}
}

// TestParse_MalformedNumberBeforeCorruptTextIsJSONError is the headline:
// every malformed number shape, in every container kind, wins over invalid
// UTF-8 bytes or an unpaired surrogate in a string read afterwards. Every
// failure returns the zero-value config (parseComboFail).
func TestParse_MalformedNumberBeforeCorruptTextIsJSONError(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	loneHigh, loneLow := `\uD800`, `\uDC00`
	cases := []struct {
		name string
		body string // document body without the outer braces, after base
	}{
		// --- the three shapes named in the contract, later unpaired surrogate
		{name: "1e then lone high surrogate", body: base + `,"limit":1e,"note":"` + loneHigh + `"`},
		{name: "1E+ then lone high surrogate", body: base + `,"limit":1E+,"note":"` + loneHigh + `"`},
		{name: "0. then lone high surrogate", body: base + `,"limit":0.,"note":"` + loneHigh + `"`},
		{name: "1e then lone low surrogate", body: base + `,"limit":1e,"note":"` + loneLow + `"`},
		// --- later invalid UTF-8 in a value
		{name: "1e then invalid UTF-8 value", body: base + `,"limit":1e,"note":"` + badUTF8 + `"`},
		{name: "1E+ then invalid UTF-8 value", body: base + `,"limit":1E+,"note":"` + badUTF8 + `"`},
		{name: "0. then invalid UTF-8 value", body: base + `,"limit":0.,"note":"` + badUTF8 + `"`},
		// --- illegal leading zero is a number format error too
		{name: "01 then unpaired surrogate", body: base + `,"limit":01,"note":"` + loneHigh + `"`},
		{name: "01 then invalid UTF-8", body: base + `,"limit":01,"note":"` + badUTF8 + `"`},
		// --- same object inside an unknown nested object
		{name: "nested unknown object", body: base + `,"meta":{"limit":1e,"note":"` + loneHigh + `"}`},
		{name: "nested unknown object 0. then utf8", body: base + `,"meta":{"limit":0.,"note":"` + badUTF8 + `"}`},
		// --- arrays: bad element before a corrupt element, in one or several items
		{name: "array same pair", body: base + `,"items":[1e,"` + loneHigh + `"]`},
		{name: "array separate items", body: base + `,"items":[{"limit":1E+},{"note":"` + loneHigh + `"}]`},
		{name: "array bad item then utf8 item", body: base + `,"items":[0.,"` + badUTF8 + `"]`},
		// --- extra member of a known cluster object
		{name: "extra field inside cluster then later text",
			body: `"app":"a","revision":"r","image":"i","batchSize":1,` +
				`"clusters":[{"id":"c1","w":0.}],"note":"` + loneHigh + `"`},
		// --- deeply nested unknown structure
		{name: "deeply nested number then utf8",
			body: base + `,"a":{"b":[{"c":1e}]},"note":"` + badUTF8 + `"`},
		// --- corrupt text in a later member NAME is still later
		{name: "bad number then invalid-UTF-8 member name", body: base + `,"limit":1e,"no` + badUTF8 + `te":1`},
		{name: "bad number then surrogate member name", body: base + `,"limit":0.,"` + loneHigh + `x":1`},
		// --- high surrogate followed by a plain character
		{name: "1e then high-then-BMP", body: base + `,"limit":1e,"note":"` + `\uD800A` + `"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseComboFail(t, "{"+tc.body+"}")
			assertJSONFormatError(t, err)
		})
	}
}

// TestParse_CorruptTextBeforeMalformedNumberKeepsTextError is the reverse
// side: the string problem read first stays the reported error, with its
// exact kind and position, even though a malformed number follows.
func TestParse_CorruptTextBeforeMalformedNumberKeepsTextError(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	cases := []struct {
		name string
		body string
		kind string // "utf8" or "surrogate"
		path string
	}{
		{
			name: "surrogate value before 1e",
			body: base + `,"note":"\uD800","limit":1e`,
			kind: "surrogate", path: "$.note",
		},
		{
			name: "invalid UTF-8 value before 1e",
			body: base + `,"note":"` + badUTF8 + `","limit":1e`,
			kind: "utf8", path: "$.note",
		},
		{
			name: "nested surrogate before 0.",
			body: base + `,"meta":{"note":"\uDC00","limit":0.}`,
			kind: "surrogate", path: "$.meta.note",
		},
		{
			name: "array element surrogate before bad number",
			body: base + `,"items":["\uD800",1e]`,
			kind: "surrogate", path: "$.items[0]",
		},
		{
			name: "array element invalid UTF-8 before 1E+",
			body: base + `,"items":["` + badUTF8 + `",1E+]`,
			kind: "utf8", path: "$.items[0]",
		},
		{
			name: "surrogate in member name before bad number",
			body: base + `,"me\uD800ta":1,"limit":1e`,
			kind: "surrogate", path: "$",
		},
		{
			name: "invalid UTF-8 in member name before leading zero",
			body: base + `,"me` + badUTF8 + `ta":1,"limit":01`,
			kind: "utf8", path: "$",
		},
		{
			name: "surrogate before malformed number in the same object",
			body: base + `,"meta":{"note":"\uD800","n":1E+}`,
			kind: "surrogate", path: "$.meta.note",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseTextErr(t, "{"+tc.body+"}")
			assertTextErrorAt(t, err, tc.kind, tc.path)
		})
	}
}

// TestParse_DuplicateOrderUnchangedByNumberTextFix pins the meeting order
// with a third failure present: a duplicate read before the malformed number
// still reports the member and its owning object, while a duplicate (and a
// text problem) after the number cannot cover the format error.
func TestParse_DuplicateOrderUnchangedByNumberTextFix(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`

	// Duplicate first, then malformed number, then corrupt text: the earlier
	// duplicate wins exactly as without the text problem.
	err := parseComboFail(t, `{`+base+`,"meta":{"x":1,"x":2},"limit":1e,"note":"\uD800"}`)
	assertDuplicateError(t, err, "x", "$.meta")

	// Malformed number first, then a duplicate, then corrupt text: the format
	// error wins; nothing later may overtake it.
	err = parseComboFail(t, `{`+base+`,"limit":1e,"meta":{"x":1,"x":2},"note":"\uD800"}`)
	assertJSONFormatError(t, err)

	// Corrupt text first already beats a later duplicate (established
	// behavior); a malformed number between them does not change that.
	err = parseTextErr(t, `{`+base+`,"note":"`+badUTF8+`","limit":1e,"meta":{"x":1,"x":2}}`)
	assertTextErrorAt(t, err, "utf8", "$.note")
}

// TestParse_StrictNumberGrammarAcceptsLegalNumbers checks the grammar scan
// rejects only genuinely malformed literals: every legal spelling — sign,
// fraction, exponent in both cases, huge and tiny values — scans through,
// and every malformed spelling stops the walk for the decoder to report.
func TestParse_StrictNumberGrammarAcceptsLegalNumbers(t *testing.T) {
	plain := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]}`
	want := parsePlan(t, plain)
	docWithNumber := func(lit string) string {
		return `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"n":` + lit + `}`
	}
	for _, lit := range []string{
		"0", "-0", "1", "-1", "10", "0.0", "-0.25", "10.50",
		"1e0", "1E0", "1e+3", "1E-3", "-1.5e10", "2.5E+2",
		"1e400", "1e-400", // beyond float64 range in both directions
		"99999999999999999999",   // integer beyond float64 exactness/range
		"0.00000000000000000001", // tiny decimal literal
	} {
		in, err := ParseReleaseInput([]byte(docWithNumber(lit)))
		if err != nil {
			t.Fatalf("legal number %s must be accepted in an extra field, got %v", lit, err)
		}
		// The ignored extra number leaves the parsed config exactly as if the
		// field were removed.
		if !reflect.DeepEqual(in, want) {
			t.Fatalf("legal number %s changed the parsed config:\n got %+v\nwant %+v", lit, in, want)
		}
	}
	for _, lit := range []string{
		"1e", "1E+", "0.", "01", "-01", "1.", "1e+", "1e-", "-",
	} {
		_, err := ParseReleaseInput([]byte(docWithNumber(lit)))
		if err == nil {
			t.Fatalf("malformed number %s must be rejected", lit)
		}
		if !strings.Contains(err.Error(), "JSON 格式错误") {
			t.Fatalf("malformed number %s must be a JSON format error, got %v", lit, err)
		}
	}
}

// TestParse_LegalExtremeNumbersInExtraFieldsKeepPlan pins the compatibility
// promise of the grammar change: tiny legal exponents, huge legal exponents
// and integers beyond float64 range are ignored in unknown fields, and the
// plan is identical to the document with those fields removed.
func TestParse_LegalExtremeNumbersInExtraFieldsKeepPlan(t *testing.T) {
	base := `{"app":"payments","revision":"v1","image":"img","batchSize":2,` +
		`"clusters":[{"id":"c2","tags":{"zone":"a"}},{"id":"c1","tags":{"zone":"b"}}]}`
	withExtra := base[:len(base)-1] +
		`,"limit":1e-400,"big":99999999999999999999,"meta":{"v":1e400,"tiny":1e-400}}`

	basePlan, err := MakeReleasePlan(parsePlan(t, base))
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseReleaseInput([]byte(withExtra))
	if err != nil {
		t.Fatalf("legal extreme numbers in extra fields must parse: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("legal extreme numbers must not disturb planning: %v", err)
	}
	if planSummary(plan) != planSummary(basePlan) {
		t.Fatalf("plan differs from the document without the extra numbers:\n got %s\nwant %s",
			planSummary(plan), planSummary(basePlan))
	}
}

// TestParse_LegalExtremeNumbersAsBatchSizeStayFieldErrors: the strict grammar
// scan only classifies malformed literals; legal-but-not-usable batchSize
// spellings keep their existing positive-integer field error, never a JSON
// format error, and return the zero-value config.
func TestParse_LegalExtremeNumbersAsBatchSizeStayFieldErrors(t *testing.T) {
	for _, lit := range []string{"1e400", "1e-400", "99999999999999999999"} {
		raw := `{"app":"a","revision":"r","image":"i","batchSize":` + lit +
			`,"clusters":[{"id":"x"}]}`
		in, err := ParseReleaseInput([]byte(raw))
		if err == nil {
			t.Fatalf("batchSize %s must be rejected", lit)
		}
		if want := `字段 "batchSize" 必须是正整数`; err.Error() != want {
			t.Fatalf("batchSize %s: error = %q, want %q", lit, err.Error(), want)
		}
		if strings.Contains(err.Error(), "JSON 格式错误") {
			t.Fatalf("legal number %s must not be a format error: %v", lit, err)
		}
		if len(in.Clusters) != 0 || in.App != "" || in.BatchSize != 0 {
			t.Fatalf("rejected parse must return the zero config, got %+v", in)
		}
	}
}

// TestPlanCLI_MalformedNumberBeforeCorruptTextFailsCleanly pins the CLI
// contract for the fixed ordering: exit 1, empty stdout, and the JSON format
// error on stderr — the later unpaired surrogate must not appear there as
// the cause.
func TestPlanCLI_MalformedNumberBeforeCorruptTextFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"payments","revision":"v1","image":"img","batchSize":1,` +
		`"clusters":[{"id":"c1"}],"limit":1e,"note":"\uD800"}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("stderr should report the JSON format error, got %q", stderr)
	}
	if strings.Contains(stderr, "未配对") || strings.Contains(stderr, "UTF-8") {
		t.Fatalf("the later text problem must not overtake the format error, got %q", stderr)
	}
}

// TestPlanCLI_CorruptTextBeforeMalformedNumberKeepsTextError is the reverse
// case at the CLI: the earlier text problem is what stderr names.
func TestPlanCLI_CorruptTextBeforeMalformedNumberKeepsTextError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"payments","revision":"v1","image":"img","batchSize":1,` +
		`"clusters":[{"id":"c1"}],"note":"\uD800","limit":1e}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "未配对") || !strings.Contains(stderr, "$.note") {
		t.Fatalf("stderr should name the earlier unpaired escape at $.note, got %q", stderr)
	}
	if strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("the earlier text problem must stay the cause, got %q", stderr)
	}
}
