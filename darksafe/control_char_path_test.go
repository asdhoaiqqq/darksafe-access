package darksafe

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file is the regression net for the display of error LOCATIONS when a
// member name contains DEL (U+007F) or a C1 control (U+0080–U+009F). These
// are legal JSON member-name characters, so a name carrying them must still
// be accepted, compared after decoding and located precisely; the only thing
// being fixed here is how the name is shown inside an error location:
//
//   - such a character must appear as the six-character \uXXXX escape (never
//     as a real control byte with a terminal effect, and never as Go's
//     non-JSON "\xNN" spelling), so every quoted location fragment is a legal
//     JSON string that decodes back to the exact member name;
//   - a name written directly and the same name written with \uXXXX escapes
//     must render one identical location;
//   - a real control character must stay distinguishable from ordinary text
//     that merely reads backslash-u-0-0-8-5;
//   - the established rules for identifiers (dot notation), Chinese, empty
//     names, dots/brackets/quotes/backslashes, already-escaped newlines and
//     U+2028/U+2029, and real zero-based array indices are unchanged.
//
// Only the display changes: error kinds and their file-order precedence, the
// zero-value failure return, and acceptance of legal extra fields all stay as
// before. (assertNoControlEffect lives in reject_report_id_test.go and is
// shared with the all-rejected report tests.)

// All non-ASCII characters in this file are expressed as Go escapes inside
// interpreted strings, so the source itself carries no raw control bytes.

// quotedJSONStrings scans msg for JSON string literals ("...", honoring
// backslash escapes) and returns the decoded value of each. It proves every
// quoted fragment of a rendered location (and the duplicate field token) is
// decodable JSON rather than Go %q text such as "\x7f".
func quotedJSONStrings(t *testing.T, msg string) []string {
	t.Helper()
	var decoded []string
	for i := 0; i < len(msg); i++ {
		if msg[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(msg) {
			switch msg[j] {
			case '\\':
				j += 2
				continue
			case '"':
				var s string
				lit := msg[i : j+1]
				if err := json.Unmarshal([]byte(lit), &s); err != nil {
					t.Fatalf("fragment %q in message %q is not decodable JSON: %v", lit, msg, err)
				}
				decoded = append(decoded, s)
				i = j
				j = len(msg)
			default:
				j++
			}
		}
	}
	return decoded
}

// pathBase is a valid document body without any extra fields.
const pathBase = `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`

// TestParse_DuplicatePathEscapesDELAndC1Controls is the core example: a
// top-level member named meta + control + info owns an object with a
// duplicated "x"; the location must bracket the name and spell the control as
// \uXXXX, and the duplicate field is reported as "x".
func TestParse_DuplicatePathEscapesDELAndC1Controls(t *testing.T) {
	cases := []struct {
		name string
		ctrl rune
		want string // expected bracketed segment
	}{
		{"DEL", 0x7F, `$["meta\u007finfo"]`},
		{"C1 first", 0x80, `$["meta\u0080info"]`},
		{"NEL U+0085", 0x85, `$["meta\u0085info"]`},
		{"C1 last", 0x9F, `$["meta\u009finfo"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "{" + pathBase + `,"meta` + string(tc.ctrl) + `info":{"x":1,"x":2}}`
			err := parseDupErr(t, doc)
			dme, ok := err.(*duplicateMemberError)
			if !ok {
				t.Fatalf("expected *duplicateMemberError, got %T: %v", err, err)
			}
			if dme.field != "x" {
				t.Fatalf("duplicate field = %q, want x", dme.field)
			}
			if dme.path != tc.want {
				t.Fatalf("path = %q, want %q", dme.path, tc.want)
			}
			assertNoControlEffect(t, err.Error())
			segs := quotedJSONStrings(t, dme.path)
			if len(segs) != 1 || segs[0] != "meta"+string(tc.ctrl)+"info" {
				t.Fatalf("bracketed segment must decode to the original name, got %q", segs)
			}
		})
	}
}

// TestParse_TextErrorPathUnderControlNamedObject points value errors
// (invalid UTF-8 and an unpaired surrogate) inside the control-named object at
// the object segment plus ".note", while corrupt text in the member NAME
// itself still points at the owning object.
func TestParse_TextErrorPathUnderControlNamedObject(t *testing.T) {
	// name holds a raw U+0085 byte; concatenating it into the JSON body puts
	// the literal control byte into the member name.
	name := "meta\u0085info"
	wrapping := `$["meta\u0085info"]`

	t.Run("invalid UTF-8 in note value", func(t *testing.T) {
		doc := "{" + pathBase + `,"` + name + `":{"note":"` + badUTF8 + `"}}`
		err := parseTextErr(t, doc)
		want := wrapping + ".note"
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err, want)
		}
		assertNoControlEffect(t, err.Error())
	})

	t.Run("unpaired surrogate in note value", func(t *testing.T) {
		doc := "{" + pathBase + `,"` + name + `":{"note":"` + `\uD800` + `"}}`
		err := parseTextErr(t, doc)
		want := wrapping + ".note"
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `\uD800`) {
			t.Fatalf("error %q must name %q and the escape \\uD800", err, want)
		}
		assertNoControlEffect(t, err.Error())
	})

	t.Run("invalid UTF-8 in the member name points at owning object", func(t *testing.T) {
		// The corrupt name belongs to the root object, so the location is "$".
		doc := "{" + pathBase + `,"meta` + badUTF8 + `info":1}`
		err := parseTextErr(t, doc)
		if !strings.Contains(err.Error(), "成员名") || !strings.Contains(err.Error(), "$") {
			t.Fatalf("corrupt member name must point at the owning object, got %q", err)
		}
	})

	t.Run("unpaired surrogate in the member name points at owning object", func(t *testing.T) {
		doc := "{" + pathBase + `,"meta` + `\uD800` + `info":1}`
		err := parseTextErr(t, doc)
		if !strings.Contains(err.Error(), "成员名") || !strings.Contains(err.Error(), "所属对象位置 $") {
			t.Fatalf("surrogate in member name must point at the root object, got %q", err)
		}
		assertNoControlEffect(t, err.Error())
	})
}

// TestParse_ControlPathSameForEquivalentSpellings requires a literal control
// byte and its \uXXXX escape spelling to build the same location, for both the
// raw walker (a complete document) and the encoding/json fallback walk (a
// document whose duplicate precedes a malformation).
func TestParse_ControlPathSameForEquivalentSpellings(t *testing.T) {
	cases := []struct {
		name string
		ctrl rune
		esc  string
	}{
		{"DEL", 0x7F, `\u007f`},
		{"NEL", 0x85, `\u0085`},
		{"C1 last", 0x9F, `\u009f`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := `$["meta` + tc.esc + `info"]`

			literal := "{" + pathBase + `,"meta` + string(tc.ctrl) + `info":{"x":1,"x":2}}`
			escaped := "{" + pathBase + `,"meta` + tc.esc + `info":{"x":1,"x":2}}`

			litErr := parseDupErr(t, literal)
			escErr := parseDupErr(t, escaped)
			if litErr.(*duplicateMemberError).path != want {
				t.Fatalf("literal path = %q, want %q", litErr.(*duplicateMemberError).path, want)
			}
			if litErr.Error() != escErr.Error() {
				t.Fatalf("spellings must render identically:\n literal: %q\n escaped: %q",
					litErr, escErr)
			}

			// The malformed-but-duplicate-first document goes through the
			// encoding/json fallback walk and must render the same path.
			malformedLiteral := "{" + pathBase + `,"meta` + string(tc.ctrl) + `info":{"x":1,"x":2}`
			malformedEscaped := "{" + pathBase + `,"meta` + tc.esc + `info":{"x":1,"x":2}`
			mLit := parseComboFail(t, malformedLiteral)
			mEsc := parseComboFail(t, malformedEscaped)
			assertDuplicateError(t, mLit, "x", want)
			if mLit.Error() != mEsc.Error() {
				t.Fatalf("fallback walk spellings must match:\n literal: %q\n escaped: %q", mLit, mEsc)
			}
		})
	}
}

// TestParse_DuplicateControlFieldIsDecodableJSON covers the field TOKEN of the
// duplicate error (rendered separately from the path): a duplicated member
// name that itself contains DEL/C1 must be shown as a JSON string that
// decodes back to the name, never as Go's "\xNN" quoting.
func TestParse_DuplicateControlFieldIsDecodableJSON(t *testing.T) {
	field := "k\x7fx\u0085z\u009f"
	literal := "{" + pathBase + `,"` + field + `":1,"` + field + `":2}`
	escaped := "{" + pathBase + `,"k\u007fx\u0085z\u009f":1,"k\u007fx\u0085z\u009f":2}`

	litErr := parseDupErr(t, literal)
	escErr := parseDupErr(t, escaped)
	if litErr.Error() != escErr.Error() {
		t.Fatalf("field renderings must match:\n literal: %q\n escaped: %q", litErr, escErr)
	}
	if strings.Contains(litErr.Error(), `\x`) {
		t.Fatalf("field must not use Go's \\xNN quoting: %q", litErr)
	}
	wantFieldToken := `"k\u007fx\u0085z\u009f"`
	if !strings.Contains(litErr.Error(), "字段 "+wantFieldToken) {
		t.Fatalf("error %q must contain field token %q", litErr, wantFieldToken)
	}
	// The field token is one of the quoted JSON strings and decodes to the
	// original duplicated name.
	decoded := quotedJSONStrings(t, litErr.Error())
	found := false
	for _, d := range decoded {
		if d == field {
			found = true
		}
	}
	if !found {
		t.Fatalf("no quoted fragment decoded to the original field %q; got %q", field, decoded)
	}
	assertNoControlEffect(t, litErr.Error())
}

// TestParse_RealControlDistinctFromLiteralEscapeText requires that a real
// control character and ordinary text reading backslash-u-0-0-8-5 are distinct
// names and get distinct, individually decodable locations.
func TestParse_RealControlDistinctFromLiteralEscapeText(t *testing.T) {
	// escape spelling -> decodes to a name containing a real NEL
	realControl := "{" + pathBase + `,"meta\u0085info":{"x":1,"x":2}}`
	// "\\u" decodes to a real backslash, so the name is the literal text meta\u0085info
	ordinaryText := "{" + pathBase + `,"meta\\u0085info":{"x":1,"x":2}}`

	realErr := parseDupErr(t, realControl)
	textErr := parseDupErr(t, ordinaryText)

	if realErr.(*duplicateMemberError).path != `$["meta\u0085info"]` {
		t.Fatalf("real control path = %q", realErr.(*duplicateMemberError).path)
	}
	if textErr.(*duplicateMemberError).path != `$["meta\\u0085info"]` {
		t.Fatalf("ordinary-text path = %q", textErr.(*duplicateMemberError).path)
	}
	if realErr.Error() == textErr.Error() {
		t.Fatalf("a real NEL and literal \\u0085 text must not share a location:\n %q", realErr)
	}
	// Each quoted fragment decodes to a DIFFERENT original name.
	if got := quotedJSONStrings(t, realErr.(*duplicateMemberError).path); got[0] != "meta\u0085info" {
		t.Fatalf("real-control segment decoded to %q", got)
	}
	if got := quotedJSONStrings(t, textErr.(*duplicateMemberError).path); got[0] != `meta\u0085info` {
		t.Fatalf("ordinary-text segment decoded to %q", got)
	}
}

// TestParse_ControlPathKeepsExistingRulesAndArrayIndices proves the rest of
// the display convention is untouched: identifier dot notation, Chinese,
// empty/dotted/bracketed/quoted/backslash names, escaped newline and
// U+2028/U+2029, and real zero-based array indices next to a control-named
// ancestor.
func TestParse_ControlPathKeepsExistingRulesAndArrayIndices(t *testing.T) {
	cases := []struct {
		name string
		body string
		path string
	}{
		{"simple identifier keeps dot", pathBase + `,"meta":{"x":1,"x":2}`, "$.meta"},
		{"Chinese name stays bracketed and literal", pathBase + `,"元数据":{"x":1,"x":2}`, `$["元数据"]`},
		{"empty name", pathBase + `,"":{"x":1,"x":2}`, `$[""]`},
		{"dotted name", pathBase + `,"meta.info":{"x":1,"x":2}`, `$["meta.info"]`},
		{"bracket-looking name", pathBase + `,"zone[0]":{"x":1,"x":2}`, `$["zone[0]"]`},
		{"quote in name", pathBase + `,"a\"b":{"x":1,"x":2}`, `$["a\"b"]`},
		{"backslash in name", pathBase + `,"a\\b":{"x":1,"x":2}`, `$["a\\b"]`},
		{"escaped newline stays escaped", pathBase + `,"a\nb":{"x":1,"x":2}`, `$["a\nb"]`},
		// A literal U+2028 in the JSON member name still renders escaped in the
		// location (existing rule), not as a raw line separator.
		{"U+2028 stays escaped", pathBase + `,"a` + "\u2028" + `b":{"x":1,"x":2}`, `$["a\u2028b"]`},
		{"U+2029 stays escaped", pathBase + `,"a` + "\u2029" + `b":{"x":1,"x":2}`, `$["a\u2029b"]`},
		{"real array index after control-named object",
			pathBase + `,"meta\u0085info":{"items":[{"z":1,"z":2}]}`,
			`$["meta\u0085info"].items[0]`},
		{"control-named object inside an array keeps index",
			pathBase + `,"items":[{"meta\u007fx":{"z":1,"z":2}}]`,
			`$.items[0]["meta\u007fx"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseDupErr(t, "{"+tc.body+"}")
			dme := err.(*duplicateMemberError)
			if dme.path != tc.path {
				t.Fatalf("path = %q, want %q", dme.path, tc.path)
			}
			assertNoControlEffect(t, dme.path)
			// Every bracketed fragment is decodable JSON.
			quotedJSONStrings(t, dme.path)
		})
	}
}

// TestParse_ControlMemberNamesStillLegal ensures names containing DEL/C1 are
// not rejected, renamed or dropped: in extra fields they are ignored like any
// other unknown field and the plan is produced unchanged.
func TestParse_ControlMemberNamesStillLegal(t *testing.T) {
	raw := `{
		"app": "a", "revision": "r", "image": "i", "batchSize": 1,
		"clusters": [{"id": "c1"}],
		"meta\u007fx": {"k": 1},
		"a\u0085b": 2,
		"z\u009f": "ignored"
	}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatalf("legal DEL/C1 member names must be accepted: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("plan must be unaffected by control-named extra fields: %v", err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "c1" {
		t.Fatalf("unexpected plan: %+v", plan.Batches)
	}
}

// TestPlanCLI_ControlNamedObjectErrorIsClean drives the exact spec example
// through the CLI: exit 1, empty stdout, and stderr carrying the readable
// bracketed position with the control escaped and a plain "x" field.
func TestPlanCLI_ControlNamedObjectErrorIsClean(t *testing.T) {
	doc := `{
		"app": "payments", "revision": "v1", "image": "img",
		"batchSize": 1, "clusters": [],
		"meta\u0085info": {"x": 1, "x": 2}
	}`
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, `"x"`) || !strings.Contains(stderr, `$["meta\u0085info"]`) {
		t.Fatalf("stderr must name field x and the bracketed position, got %q", stderr)
	}
	if strings.Contains(stderr, "\x85") || strings.Contains(stderr, "\x7f") {
		t.Fatalf("stderr must not contain the raw control byte: %q", stderr)
	}
	assertNoControlEffect(t, stderr)
}
