package darksafe

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the display rule for member names containing DEL
// (U+007F) or a C1 control character (U+0080–U+009F) in an error location:
// such a rune must always appear as a JSON \uXXXX escape inside the quoted
// path segment, never as the raw control character and never as the
// JSON-undecodable \xNN spelling that Go's %q would choose. Every quoted
// segment must still JSON-decode back to the exact decoded member name, so a
// literal name and the same name written with \uXXXX escapes produce one
// identical location, while ordinary text that merely contains the
// characters "\u0085" stays distinguishable.

// quotedSegments extracts every bracketed JSON-string segment ("...") from a
// rendered location, honoring backslash escapes while scanning.
func quotedSegments(t *testing.T, path string) []string {
	t.Helper()
	var segs []string
	rest := path
	for {
		open := strings.Index(rest, "[\"")
		if open < 0 {
			return segs
		}
		j := open + 2
		for j < len(rest) {
			if rest[j] == '\\' {
				j += 2
				continue
			}
			if rest[j] == '"' && j+1 < len(rest) && rest[j+1] == ']' {
				break
			}
			j++
		}
		if j >= len(rest) {
			t.Fatalf("unterminated quoted segment in path %q", path)
		}
		segs = append(segs, rest[open+1:j+1])
		rest = rest[j+2:]
	}
}

// decodeSegment JSON-decodes one quoted path segment back to its member name.
func decodeSegment(t *testing.T, seg string) string {
	t.Helper()
	var name string
	if err := json.Unmarshal([]byte(seg), &name); err != nil {
		t.Fatalf("segment %q is not a decodable JSON string: %v", seg, err)
	}
	return name
}

// TestPathEncoder_EscapesEveryControlAndDecodesBack exercises
// jsonEncodePathString directly over the whole control range: the result is
// always one legal JSON string restoring the input, with no raw control
// character and no \xNN spelling.
func TestPathEncoder_EscapesEveryControlAndDecodesBack(t *testing.T) {
	for r := rune(0); r <= 0xA0; r++ {
		key := "meta" + string(r) + "info"
		enc := jsonEncodePathString(key)
		if strings.Contains(enc, `\x`) {
			t.Fatalf("U+%04X produced JSON-illegal \\xNN spelling: %s", r, enc)
		}
		assertNoControlEffect(t, enc)
		if got := decodeSegment(t, enc); got != key {
			t.Fatalf("U+%04X round trip = %q, want %q (encoded %s)", r, got, key, enc)
		}
	}
	// The C1 range including NEL (U+0085), DEL, and the line separators all
	// keep the \uXXXX spelling, while visible characters (including CJK and
	// HTML-significant ones, which keep encoding/json's HTML escaping) decode
	// back exactly.
	for _, r := range []rune{0x7f, 0x80, 0x85, 0x9f, 0x2028, 0x2029, '"', '\\', '\n', '<', '>', '&', '名', '😀'} {
		key := string(r)
		enc := jsonEncodePathString(key)
		if !utf8.ValidString(enc) {
			t.Fatalf("encoding of U+%04X is not valid UTF-8: %q", r, enc)
		}
		if got := decodeSegment(t, enc); got != key {
			t.Fatalf("U+%04X round trip = %q, want %q (encoded %s)", r, got, key, enc)
		}
	}
	// A real control rune and the ordinary six-character text "\u0085" encode
	// differently, yet each decodes back to its own original string.
	control := jsonEncodePathString("\u0085")
	literal := jsonEncodePathString(`\u0085`)
	if control == literal {
		t.Fatalf("control rune and literal escape text render identically: %s", control)
	}
	if control != `"\u0085"` {
		t.Fatalf("U+0085 encoding = %s, want \"\\u0085\"", control)
	}
	if literal != `"\\u0085"` {
		t.Fatalf("literal text encoding = %s, want \"\\\\u0085\"", literal)
	}
	if got := decodeSegment(t, literal); got != `\u0085` {
		t.Fatalf("literal text round trip = %q", got)
	}
	// The empty name keeps its established spelling.
	if got := jsonEncodePathString(""); got != `""` {
		t.Fatalf("empty name encoded as %s", got)
	}
}

// TestParse_DuplicatePathEscapesControlCharacterNames is the issue's core
// case: a top-level member made of meta, U+0085 and info owns an object with
// a duplicate "x"; the object position is $["meta\u0085info"], the duplicate
// is "x", and the same location results whether the member name is written
// literally or with the \u0085 escape.
func TestParse_DuplicatePathEscapesControlCharacterNames(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	cases := []struct {
		name      string
		body      string // JSON body without outer braces
		wantField string // decoded duplicated member name
		wantPath  string
	}{
		{
			name:      "literal NEL name owns duplicate object",
			body:      base + `,"meta` + "\u0085" + `info":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["meta\u0085info"]`,
		},
		{
			name:      "escaped NEL name owns duplicate object",
			body:      base + `,"meta\u0085info":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["meta\u0085info"]`,
		},
		{
			name:      "literal DEL name",
			body:      base + `,"meta` + "\u007f" + `info":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["meta\u007finfo"]`,
		},
		{
			name:      "escaped DEL name",
			body:      base + `,"meta\u007finfo":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["meta\u007finfo"]`,
		},
		{
			name:      "first C1 control U+0080",
			body:      base + `,"a\u0080b":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["a\u0080b"]`,
		},
		{
			name:      "last C1 control U+009F",
			body:      base + `,"a\u009fb":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["a\u009fb"]`,
		},
		{
			name:      "nested object under a control-character name",
			body:      base + `,"a\u0085b":{"c\u007fd":{"x":1,"x":2}}`,
			wantField: "x",
			wantPath:  `$["a\u0085b"]["c\u007fd"]`,
		},
		{
			name:      "real array index after a control-character name",
			body:      base + `,"a\u0085b":[{"ok":1},{"y":1,"y":2}]`,
			wantField: "y",
			wantPath:  `$["a\u0085b"][1]`,
		},
		{
			name:      "simple dotted name after a control-character name",
			body:      base + `,"a\u0085b":{"note":{"x":1,"x":2}}`,
			wantField: "x",
			wantPath:  `$["a\u0085b"].note`,
		},
		{
			name:      "duplicated member name itself contains NEL",
			body:      base + `,"meta":{"a` + "\u0085" + `b":1,"a\u0085b":2}`,
			wantField: "a\u0085b",
			wantPath:  `$.meta`,
		},
		{
			name:      "line separator name stays escaped as before",
			body:      base + `,"a\u2028b":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["a\u2028b"]`,
		},
		{
			name:      "paragraph separator name stays escaped as before",
			body:      base + `,"a\u2029b":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["a\u2029b"]`,
		},
		{
			name:      "escaped newline spelling is preserved",
			body:      base + `,"a\nb":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$["a\nb"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseDupErr(t, "{"+tc.body+"}")
			dme, ok := err.(*duplicateMemberError)
			if !ok {
				t.Fatalf("expected *duplicateMemberError, got %T: %v", err, err)
			}
			if dme.field != tc.wantField {
				t.Fatalf("field = %q, want %q", dme.field, tc.wantField)
			}
			if dme.path != tc.wantPath {
				t.Fatalf("path = %q, want %q", dme.path, tc.wantPath)
			}
			// The whole message — field and location — must be free of raw
			// control effects and of JSON-illegal \xNN escapes.
			assertNoControlEffect(t, err.Error())
			if strings.Contains(err.Error(), `\x`) {
				t.Fatalf("error uses \\xNN spelling: %q", err.Error())
			}
		})
	}
}

// TestParse_TextErrorPathsWithControlCharacterNames checks the
// invalid-UTF-8 and unpaired-surrogate verdicts: a problem in a member value
// under a control-character name reaches into the object with the member
// segment, while a problem inside the member name itself points at the
// owning object. Arrays keep their real zero-based index.
func TestParse_TextErrorPathsWithControlCharacterNames(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	litName := `"meta` + "\u0085" + `info"` // member name written with raw NEL
	escName := `"meta\u0085info"`           // same name written with the escape
	t.Run("invalid UTF-8 in nested value", func(t *testing.T) {
		for _, name := range []string{litName, escName} {
			doc := `{` + base + `,` + name + `:{"note":"a` + badUTF8 + `"}}`
			err := parseTextErr(t, doc)
			e, ok := err.(*invalidUTF8Error)
			if !ok {
				t.Fatalf("expected *invalidUTF8Error, got %T: %v", err, err)
			}
			if e.isKey || e.path != `$["meta\u0085info"].note` {
				t.Fatalf("path=%q isKey=%v, want $[\"meta\\u0085info\"].note value", e.path, e.isKey)
			}
			assertNoControlEffect(t, err.Error())
		}
	})
	t.Run("unpaired surrogate in nested value", func(t *testing.T) {
		for _, name := range []string{litName, escName} {
			doc := `{` + base + `,` + name + `:{"note":"\uD800"}}`
			err := parseTextErr(t, doc)
			e, ok := err.(*unpairedSurrogateError)
			if !ok {
				t.Fatalf("expected *unpairedSurrogateError, got %T: %v", err, err)
			}
			if e.isKey || e.path != `$["meta\u0085info"].note` {
				t.Fatalf("path=%q isKey=%v, want $[\"meta\\u0085info\"].note value", e.path, e.isKey)
			}
			assertNoControlEffect(t, err.Error())
		}
	})
	t.Run("invalid UTF-8 in member name points at owner", func(t *testing.T) {
		doc := `{` + base + `,` + `"meta` + "\u0085" + `in` + badUTF8 + `fo":1}`
		err := parseTextErr(t, doc)
		e, ok := err.(*invalidUTF8Error)
		if !ok || !e.isKey || e.path != "$" {
			t.Fatalf("got %T path=%q isKey=%v, want key error at $", err, ePath(err), eIsKey(err))
		}
	})
	t.Run("unpaired surrogate in member name points at owner", func(t *testing.T) {
		doc := `{` + base + `,"meta\u0085in\uD800fo":1}`
		err := parseTextErr(t, doc)
		e, ok := err.(*unpairedSurrogateError)
		if !ok || !e.isKey || e.path != "$" {
			t.Fatalf("got %T path=%q isKey=%v, want key error at $", err, ePath(err), eIsKey(err))
		}
	})
	t.Run("array item under control-character name keeps real index", func(t *testing.T) {
		doc := `{` + base + `,` + escName + `:["ok","\uDC00"]}`
		err := parseTextErr(t, doc)
		e, ok := err.(*unpairedSurrogateError)
		if !ok || e.isKey || e.path != `$["meta\u0085info"][1]` {
			t.Fatalf("got %T path=%q isKey=%v, want value at index 1", err, ePath(err), eIsKey(err))
		}
		assertNoControlEffect(t, err.Error())
	})
}

func ePath(err error) string {
	switch e := err.(type) {
	case *invalidUTF8Error:
		return e.path
	case *unpairedSurrogateError:
		return e.path
	}
	return ""
}

func eIsKey(err error) bool {
	switch e := err.(type) {
	case *invalidUTF8Error:
		return e.isKey
	case *unpairedSurrogateError:
		return e.isKey
	}
	return false
}

// TestParse_ControlNamePathSegmentsDecodeBack takes the reported locations
// apart and proves every quoted segment restores the exact decoded member
// name — the raw control character — while the plain text "\u0085" (a
// backslash followed by u0085) restores to those six ordinary characters
// instead, so the two stay distinguishable.
func TestParse_ControlNamePathSegmentsDecodeBack(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	cases := []struct {
		body     string
		wantSegs []string
	}{
		{
			body:     base + `,"meta` + "\u0085" + `info":{"x":1,"x":2}`,
			wantSegs: []string{"meta\u0085info"},
		},
		{
			body:     base + `,"meta\u007finfo":{"x":1,"x":2}`,
			wantSegs: []string{"meta\u007finfo"},
		},
		{
			body:     base + `,"a\u0085b":{"c\u009fd":{"x":1,"x":2}}`,
			wantSegs: []string{"a\u0085b", "c\u009fd"},
		},
		{
			// A real backslash then "u0085" is ordinary text: the decoded
			// segment contains the backslash, not the control character.
			body:     base + `,"meta\\u0085info":{"x":1,"x":2}`,
			wantSegs: []string{`meta\u0085info`},
		},
	}
	for i, tc := range cases {
		err := parseDupErr(t, "{"+tc.body+"}")
		dme := err.(*duplicateMemberError)
		segs := quotedSegments(t, dme.path)
		if len(segs) != len(tc.wantSegs) {
			t.Fatalf("case %d: got %d segments %q, want %d (path %s)",
				i, len(segs), segs, len(tc.wantSegs), dme.path)
		}
		for j, seg := range segs {
			if got := decodeSegment(t, seg); got != tc.wantSegs[j] {
				t.Fatalf("case %d segment %d: decoded %q, want %q", i, j, got, tc.wantSegs[j])
			}
		}
		assertNoControlEffect(t, dme.path)
	}
}

// TestParse_ControlNamePathSameForEquivalentSpellings ensures the two
// spellings of one legal member name — raw DEL/C1 character versus its
// \uXXXX escape — always render the exact same location.
func TestParse_ControlNamePathSameForEquivalentSpellings(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	for _, r := range []rune{0x7f, 0x80, 0x85, 0x9f} {
		literal := `{` + base + `,"meta` + string(r) + `info":{"x":1,"x":2}}`
		escaped := `{` + base + fmt.Sprintf(`,"meta\u%04xinfo":{"x":1,"x":2}}`, r)
		litErr := parseDupErr(t, literal).(*duplicateMemberError)
		escErr := parseDupErr(t, escaped).(*duplicateMemberError)
		if litErr.path != escErr.path {
			t.Fatalf("U+%04X: paths differ: %q vs %q", r, litErr.path, escErr.path)
		}
	}
}

// TestParse_ControlCharacterNamesLegalWhenDistinct verifies that member
// names containing DEL or C1 controls are accepted, kept and ignored as
// unknown fields when there is no error, and the plan is unaffected.
func TestParse_ControlCharacterNamesLegalWhenDistinct(t *testing.T) {
	raw := `{
		"app": "a", "revision": "r", "image": "i", "batchSize": 2,
		"clusters": [{"id": "c1"}, {"id": "c2"}],
		"meta\u007fextra": {"x": 1},
		"meta` + "\u0085" + `info": {"note": "ok"},
		"a\u009fb": "v",
		"ls\u2028end": 1
	}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatalf("legal control-character member names must be accepted: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c1,c2" {
		t.Fatalf("unexpected plan: %+v", plan.Batches)
	}
}

// TestPlanCLI_ControlCharacterNameDuplicateFailsCleanly drives the CLI with
// the issue's document: exit code 1, empty stdout, and stderr naming the
// duplicated member "x" and the readable, escaped object position.
func TestPlanCLI_ControlCharacterNameDuplicateFailsCleanly(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1", "image": "img",
		"batchSize": 1, "clusters": [],
		"meta\u0085info": {"x": 1, "x": 2}
	}`
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
	if !strings.Contains(stderr, `"x"`) || !strings.Contains(stderr, `$["meta\u0085info"]`) {
		t.Fatalf("stderr should name field and escaped position, got %q", stderr)
	}
	assertNoControlEffect(t, stderr)
	if strings.Contains(stderr, `\x`) {
		t.Fatalf("stderr must not contain a \\xNN escape: %q", stderr)
	}
}
