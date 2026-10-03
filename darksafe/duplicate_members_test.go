package darksafe

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// parseDupErr parses data and returns the error; it fails the test if no
// error is returned.
func parseDupErr(t *testing.T, data string) error {
	t.Helper()
	_, err := ParseReleaseInput([]byte(data))
	if err == nil {
		t.Fatalf("expected duplicate-member error for %s", data)
	}
	return err
}

// jsonUEsc renders one BMP code point as its JSON Unicode escape spelling,
// e.g. jsonUEsc('e') is the six-character text backslash-u-0-0-6-5. Building
// these at runtime keeps the test source itself free of escape spellings.
func jsonUEsc(r rune) string { return fmt.Sprintf(`\u%04x`, r) }

// jsonEscaped spells every code point of s as a JSON Unicode escape, so
// jsonEscaped("env") names "env" using escapes only.
func jsonEscaped(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteString(jsonUEsc(r))
	}
	return b.String()
}

// jsonSurrogatePair renders a non-BMP code point (r > 0xFFFF) as an escaped
// high/low surrogate pair, e.g. U+1F600 becomes the 12-character escape text.
func jsonSurrogatePair(r rune) string {
	hi, lo := utf16.EncodeRune(r)
	return fmt.Sprintf(`\u%04x\u%04x`, hi, lo)
}

func TestParse_DuplicateMemberRejectedEverywhere(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name      string
		body      string // JSON body without outer braces
		wantField string
		wantPath  string
	}{
		{
			name:      "top-level app",
			body:      `"app":"a","app":"b","revision":"r","image":"i","batchSize":1,"clusters":[]`,
			wantField: "app",
			wantPath:  "$",
		},
		{
			name:      "top-level batchSize",
			body:      `"app":"a","revision":"r","image":"i","batchSize":1,"batchSize":2,"clusters":[]`,
			wantField: "batchSize",
			wantPath:  "$",
		},
		{
			name:      "cluster disabled",
			body:      base + `,"clusters":[{"id":"x","disabled":true,"disabled":false}]`,
			wantField: "disabled",
			wantPath:  "$.clusters[0]",
		},
		{
			name:      "cluster id",
			body:      base + `,"clusters":[{"id":"x","id":"y"}]`,
			wantField: "id",
			wantPath:  "$.clusters[0]",
		},
		{
			name:      "second cluster",
			body:      base + `,"clusters":[{"id":"a"},{"id":"b","disabled":false,"disabled":true}]`,
			wantField: "disabled",
			wantPath:  "$.clusters[1]",
		},
		{
			name:      "tags",
			body:      base + `,"clusters":[{"id":"x","tags":{"env":"prod","env":"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "include condition",
			body:      base + `,"clusters":[],"include":[{"env":"prod","env":"dev"}]`,
			wantField: "env",
			wantPath:  "$.include[0]",
		},
		{
			name:      "exclude condition",
			body:      base + `,"clusters":[],"exclude":[{"region":"us","region":"eu"}]`,
			wantField: "region",
			wantPath:  "$.exclude[0]",
		},
		{
			name:      "second include condition",
			body:      base + `,"clusters":[],"include":[{"env":"prod"},{"env":"dev","env":"staging"}]`,
			wantField: "env",
			wantPath:  "$.include[1]",
		},
		{
			name:      "unknown field nested object",
			body:      base + `,"clusters":[],"meta":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  "$.meta",
		},
		{
			name:      "unknown field array of objects",
			body:      base + `,"clusters":[],"items":[{"y":1,"y":2}]`,
			wantField: "y",
			wantPath:  "$.items[0]",
		},
		{
			name:      "unknown field deep nesting",
			body:      base + `,"clusters":[],"a":{"b":{"c":[{"z":1,"z":2}]}}`,
			wantField: "z",
			wantPath:  "$.a.b.c[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseDupErr(t, "{"+tc.body+"}")
			msg := err.Error()
			if !strings.Contains(msg, tc.wantField) {
				t.Fatalf("error %q does not name field %q", msg, tc.wantField)
			}
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not contain path %q", msg, tc.wantPath)
			}
		})
	}
}

func TestParse_DuplicateRejectedRegardlessOfValues(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// Same value twice must still be rejected.
	for _, val := range []string{`true`, `false`, `null`, `1`, `1.5`, `"str"`, `[1,2]`, `{"k":"v"}`} {
		t.Run("same value "+val, func(t *testing.T) {
			body := `{"id":"x","tags":{"env":` + val + `,"env":` + val + `}}`
			err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
			if !strings.Contains(err.Error(), "env") {
				t.Fatalf("expected field name in %q", err)
			}
		})
	}
	// Different value types on either side must be rejected.
	pairs := [][2]string{
		{`1`, `true`},
		{`"a"`, `null`},
		{`[]`, `{}`},
		{`false`, `"false"`},
	}
	for _, p := range pairs {
		t.Run("mixed "+p[0]+" "+p[1], func(t *testing.T) {
			body := `{"id":"x","tags":{"env":` + p[0] + `,"env":` + p[1] + `}}`
			err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
			if !strings.Contains(err.Error(), "env") {
				t.Fatalf("expected field name in %q", err)
			}
		})
	}
}

func TestParse_DuplicateNamesComparedAfterDecoding(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// The second key writes the first letter of "env" as its JSON Unicode
	// escape spelling; after decoding both member names are "env".
	body := `{"id":"x","tags":{"env":"prod","` + jsonUEsc('e') + `nv":"dev"}}`
	err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
	if !strings.Contains(err.Error(), `"env"`) {
		t.Fatalf("expected decoded field name \"env\" in %q", err)
	}

	// Case differences are different names.
	valid := `{` + base + `,"clusters":[{"id":"x","tags":{"env":"prod","ENV":"dev"}}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("case-different names must not be duplicates: %v", err)
	}
	// Spacing differences are different names (no trimming).
	valid = `{` + base + `,"clusters":[{"id":"x","tags":{" env":"prod","env":"dev"}}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("spacing-different names must not be duplicates: %v", err)
	}
}

// TestParse_DuplicateNamesWithUnicodeEscapes covers every spelling that must
// collapse onto one decoded name: a literal name beside a partially escaped
// one, a name written entirely with escapes, names mixing direct and escaped
// characters, and a non-BMP character written directly versus as a surrogate
// pair. The rule is exercised at each object kind — top-level app info,
// cluster objects and their tags, include/exclude conditions, and objects
// nested under unknown fields, including inside arrays whose real index must
// survive in the reported path.
func TestParse_DuplicateNamesWithUnicodeEscapes(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	e := jsonUEsc('e')
	cases := []struct {
		name      string
		body      string // JSON body without outer braces
		wantField string
		wantPath  string
	}{
		{
			name: "top-level app literal vs escape",
			body: `"app":"a","` + jsonUEsc('a') + `pp":"b",` +
				`"revision":"r","image":"i","batchSize":1,"clusters":[]`,
			wantField: "app",
			wantPath:  "$",
		},
		{
			name: "top-level batchSize both names fully escaped",
			body: `"app":"a","revision":"r","image":"i",` +
				`"` + jsonEscaped("batchSize") + `":1,` +
				`"` + jsonEscaped("batchSize") + `":2,"clusters":[]`,
			wantField: "batchSize",
			wantPath:  "$",
		},
		{
			name:      "cluster field mixing direct and escaped characters",
			body:      base + `,"clusters":[{"id":"x","` + jsonUEsc('i') + `d":"y"}]`,
			wantField: "id",
			wantPath:  "$.clusters[0]",
		},
		{
			name: "tags literal name vs partially escaped name",
			body: base + `,"clusters":[{"id":"x","tags":{` +
				`"env":"prod","` + e + `nv":"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "tags escaped name before literal name",
			body: base + `,"clusters":[{"id":"x","tags":{` +
				`"` + e + `nv":"prod","env":"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "tags both names written entirely with escapes",
			body: base + `,"clusters":[{"id":"x","tags":{` +
				`"` + jsonEscaped("env") + `":"prod",` +
				`"` + jsonEscaped("env") + `":"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "tags name mixing direct and escaped middle character",
			body: base + `,"clusters":[{"id":"x","tags":{` +
				`"env":"prod","e` + jsonUEsc('n') + `v":"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "tags non-BMP literal vs escaped surrogate pair",
			body: base + `,"clusters":[{"id":"x","tags":{` +
				`"😀":"prod","` + jsonSurrogatePair('😀') + `":"dev"}}]`,
			wantField: "😀",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "tags surrogate pair before non-BMP literal",
			body: base + `,"clusters":[{"id":"x","tags":{` +
				`"` + jsonSurrogatePair('😀') + `":"prod","😀":"dev"}}]`,
			wantField: "😀",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "include condition partial escape",
			body: base + `,"clusters":[],"include":[{` +
				`"env":"prod","` + e + `nv":"dev"}]`,
			wantField: "env",
			wantPath:  "$.include[0]",
		},
		{
			name: "exclude condition mixed spelling",
			body: base + `,"clusters":[],"exclude":[{` +
				`"regi` + jsonUEsc('o') + `n":"us","region":"eu"}]`,
			wantField: "region",
			wantPath:  "$.exclude[0]",
		},
		{
			name:      "unknown nested object escape",
			body:      base + `,"clusters":[],"meta":{"x":1,"` + jsonUEsc('x') + `":2}`,
			wantField: "x",
			wantPath:  "$.meta",
		},
		{
			name: "unknown array of objects keeps real index",
			body: base + `,"clusters":[],"items":[` +
				`{"ok":1},{"y":1,"` + jsonUEsc('y') + `":2}]`,
			wantField: "y",
			wantPath:  "$.items[1]",
		},
		{
			name: "unknown deep nesting escape",
			body: base + `,"clusters":[],` +
				`"a":{"b":{"c":[{"z":1,"` + jsonUEsc('z') + `":2}]}}`,
			wantField: "z",
			wantPath:  "$.a.b.c[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseDupErr(t, "{"+tc.body+"}")
			msg := err.Error()
			if !strings.Contains(msg, tc.wantField) {
				t.Fatalf("error %q does not name decoded field %q", msg, tc.wantField)
			}
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not contain path %q", msg, tc.wantPath)
			}
		})
	}
}

// TestParse_DuplicateEscapedNamesRegardlessOfValues mirrors
// TestParse_DuplicateRejectedRegardlessOfValues but spells the two keys with
// different yet decode-equivalent writings: equality after decoding is the
// only criterion — same value, different value, different type, all rejected.
func TestParse_DuplicateEscapedNamesRegardlessOfValues(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	secondEnv := `"` + jsonUEsc('e') + `nv"`
	// Same value twice with differently spelled (but decode-equal) keys.
	for _, val := range []string{`true`, `false`, `null`, `1`, `1.5`, `"str"`, `[1,2]`, `{"k":"v"}`} {
		t.Run("same value "+val, func(t *testing.T) {
			body := `{"id":"x","tags":{"env":` + val + `,` + secondEnv + `:` + val + `}}`
			err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
			if !strings.Contains(err.Error(), "env") {
				t.Fatalf("expected decoded field name in %q", err)
			}
		})
	}
	// Different value types on either side.
	pairs := [][2]string{
		{`1`, `true`},
		{`"a"`, `null`},
		{`[]`, `{}`},
		{`false`, `"false"`},
	}
	for _, p := range pairs {
		t.Run("mixed "+p[0]+" "+p[1], func(t *testing.T) {
			body := `{"id":"x","tags":{"env":` + p[0] + `,` + secondEnv + `:` + p[1] + `}}`
			err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
			if !strings.Contains(err.Error(), "env") {
				t.Fatalf("expected decoded field name in %q", err)
			}
		})
	}
}

// TestParse_FirstDuplicateInFileOrderWinsWithEscapes checks that escape
// spellings do not change file-order reporting: when one object contains
// several decoded duplicates, the name whose SECOND occurrence appears
// earliest in the file is reported (not the name that sorts first), and the
// reported path keeps the object's real array index.
func TestParse_FirstDuplicateInFileOrderWinsWithEscapes(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	e := jsonUEsc('e')
	// One tags object holds two duplicate pairs: "aaa" (literal) repeats at
	// position 4; "env" (one spelling escaped) repeats at position 3. The
	// escaped "env" duplicate must win even though "aaa" sorts first.
	doc := `{` + base + `,"clusters":[{"id":"a","tags":{` +
		`"aaa":1,"env":2,"` + e + `nv":3,"aaa":4}}]}`
	err := parseDupErr(t, doc)
	msg := err.Error()
	if !strings.Contains(msg, `"env"`) || !strings.Contains(msg, "$.clusters[0].tags") {
		t.Fatalf("expected earliest second occurrence (escaped env), got %q", msg)
	}

	// An escaped duplicate inside an include condition (the include field is
	// written before clusters in the file) beats a later literal duplicate in
	// a cluster's disabled field.
	doc = `{` + base + `,` +
		`"include":[{"env":"prod","` + e + `nv":"dev"}],` +
		`"clusters":[{"id":"a","disabled":true,"disabled":false}]}`
	err = parseDupErr(t, doc)
	msg = err.Error()
	if !strings.Contains(msg, `"env"`) || !strings.Contains(msg, "$.include[0]") {
		t.Fatalf("expected include[0] escaped env duplicate first, got %q", msg)
	}

	// Real array index: cluster 0 is clean; the escaped duplicate sits in
	// cluster 1's tags and the path must carry index 1.
	doc = `{` + base + `,"clusters":[` +
		`{"id":"a"},` +
		`{"id":"b","tags":{"env":1,"` + e + `nv":2}}]}`
	err = parseDupErr(t, doc)
	msg = err.Error()
	if !strings.Contains(msg, `"env"`) || !strings.Contains(msg, "$.clusters[1].tags") {
		t.Fatalf("expected clusters[1].tags escaped env duplicate, got %q", msg)
	}
}

// TestParse_EscapedDuplicateBeatsBusinessErrors checks that a decode-equal
// duplicate is reported before any business-field validation, including when
// the app name is also missing.
func TestParse_EscapedDuplicateBeatsBusinessErrors(t *testing.T) {
	d := jsonUEsc('d')
	r := jsonUEsc('r')
	e := jsonUEsc('e')
	// Escaped duplicate "disabled" plus missing app: duplicate wins.
	doc := `{"revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","disabled":true,"` + d + `isabled":false}]}`
	err := parseDupErr(t, doc)
	if !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), "$.clusters[0]") {
		t.Fatalf("expected escaped duplicate before missing-app error, got %q", err)
	}

	// Escaped duplicate in tags plus an empty tag key in the same object.
	doc = `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","tags":{"env":1,"` + e + `nv":2,"":3}}]}`
	err = parseDupErr(t, doc)
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("expected escaped duplicate before tag-key error, got %q", err)
	}

	// Top-level escaped "revision" duplicate while app is missing.
	doc = `{"revision":"r","` + r + `evision":"r2","image":"i","batchSize":1,"clusters":[]}`
	err = parseDupErr(t, doc)
	if !strings.Contains(err.Error(), `"revision"`) || !strings.Contains(err.Error(), "$") {
		t.Fatalf("expected top-level escaped duplicate before business error, got %q", err)
	}
}

// TestParse_EscapedNamesThatStayDistinct guards the negative side of the
// decoding rule: comparison is on the fully decoded name with no case folding
// or trimming, and the seen-set is per object — including two different
// clusters each holding an "env" tag, even when their spellings differ.
func TestParse_EscapedNamesThatStayDistinct(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	e := jsonUEsc('e')
	valid := []string{
		// An escaped capital letter decodes to "Env"; case is significant.
		`{` + base + `,"clusters":[{"id":"x","tags":{"env":"prod","` + jsonUEsc('E') + `nv":"dev"}}]}`,
		// An escaped leading space is part of the decoded name, never trimmed.
		`{` + base + `,"clusters":[{"id":"x","tags":{"` + jsonUEsc(' ') + `env":"prod","env":"dev"}}]}`,
		// Same decoded "env" in two different clusters' tag objects is fine.
		`{` + base + `,"clusters":[` +
			`{"id":"a","tags":{"env":"prod"}},` +
			`{"id":"b","tags":{"` + e + `nv":"dev"}}]}`,
		// Same decoded name in a parent object and its nested object.
		`{` + base + `,"clusters":[],"meta":{"env":"x","nested":{"` + e + `nv":"y"}}}`,
		// Two different non-BMP runes written as surrogate pairs stay
		// distinct: U+1F600 is not U+1F60E even though they share a high
		// surrogate.
		`{` + base + `,"clusters":[{"id":"x","tags":{` +
			`"` + jsonSurrogatePair('😀') + `":"a","` + jsonSurrogatePair('😎') + `":"b"}}]}`,
	}
	for i, raw := range valid {
		if _, err := ParseReleaseInput([]byte(raw)); err != nil {
			t.Fatalf("case %d: distinct decoded names must be accepted: %v\n%s", i, err, raw)
		}
	}
}

// TestParse_EscapedAndLiteralSpellingsPlanEqually verifies the usage contract
// for legitimate inputs: escape spellings and literal spellings of the same
// content decode to one document, so app info, cluster filtering, batch
// arrangement, and exclusion reasons are identical.
func TestParse_EscapedAndLiteralSpellingsPlanEqually(t *testing.T) {
	literal := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3", "tags": {"env": "prod"}},
			{"id": "c1", "tags": {"env": "dev"}},
			{"id": "c2", "disabled": true, "tags": {"env": "prod"}},
			{"id": "c4", "tags": {"env": "prod", "region": "us"}}
		],
		"include": [{"env": "prod"}],
		"exclude": [{"region": "us"}]
	}`
	// Same content with assorted Unicode-escape spellings in member names and
	// in one tag value; no object repeats a decoded member name.
	escaped := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3", "tags": {"` + jsonUEsc('e') + `nv": "pr` + jsonUEsc('o') + `d"}},
			{"id": "c1", "tags": {"env": "dev"}},
			{"id": "c2", "` + jsonUEsc('d') + `isabled": true, "tags": {"` + jsonEscaped("env") + `": "prod"}},
			{"id": "c4", "tags": {"env": "prod", "regi` + jsonUEsc('o') + `n": "us"}}
		],
		"include": [{"` + jsonEscaped("env") + `": "prod"}],
		"exclude": [{"region": "us"}]
	}`
	in1, err := ParseReleaseInput([]byte(literal))
	if err != nil {
		t.Fatal(err)
	}
	in2, err := ParseReleaseInput([]byte(escaped))
	if err != nil {
		t.Fatalf("escaped-spelling config must parse: %v", err)
	}
	p1, err := MakeReleasePlan(in1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(in2)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", p1) != fmt.Sprintf("%+v", p2) {
		t.Fatalf("plans differ between spellings:\n literal: %+v\n escaped: %+v", p1, p2)
	}
	if len(p1.Batches) != 1 || strings.Join(p1.Batches[0].Clusters, ",") != "c3" {
		t.Fatalf("unexpected plan: %+v", p1.Batches)
	}
	if len(p1.Excluded) != 3 {
		t.Fatalf("expected 3 excluded, got %+v", p1.Excluded)
	}
}

func TestParse_DuplicateCheckIsPerObject(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// Two clusters may both have an "env" tag.
	valid := `{` + base + `,"clusters":[` +
		`{"id":"a","tags":{"env":"prod"}},` +
		`{"id":"b","tags":{"env":"dev"}}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("two clusters sharing a tag key must be valid: %v", err)
	}
	// Two conditions may both contain "region".
	valid = `{` + base + `,"clusters":[],` +
		`"include":[{"region":"us"},{"region":"eu"}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("two conditions sharing a key must be valid: %v", err)
	}
	// Repeated identical conditions are not duplicates.
	valid = `{` + base + `,"clusters":[],` +
		`"include":[{"env":"prod"},{"env":"prod"}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("repeated identical conditions must be valid: %v", err)
	}
	// Same key in parent object and nested object is fine.
	valid = `{` + base + `,"clusters":[],"meta":{"env":"x","nested":{"env":"y"}}}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("same key in parent and nested object must be valid: %v", err)
	}
}

func TestParse_FirstDuplicateInFileOrderWins(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// Cluster 1 has a duplicate disabled, cluster 0 has a duplicate id:
	// cluster 0's duplicate appears earlier in the file.
	doc := `{` + base + `,"clusters":[` +
		`{"id":"a","id":"b"},` +
		`{"id":"c","disabled":true,"disabled":false}]}`
	err := parseDupErr(t, doc)
	msg := err.Error()
	if !strings.Contains(msg, `"id"`) || !strings.Contains(msg, "$.clusters[0]") {
		t.Fatalf("expected clusters[0] id duplicate first, got %q", msg)
	}

	// Duplicate inside tags of cluster 0 appears before duplicate disabled
	// of cluster 1.
	doc = `{` + base + `,"clusters":[` +
		`{"id":"a","tags":{"env":1,"env":2}},` +
		`{"id":"c","disabled":true,"disabled":false}]}`
	err = parseDupErr(t, doc)
	msg = err.Error()
	if !strings.Contains(msg, `"env"`) || !strings.Contains(msg, "$.clusters[0].tags") {
		t.Fatalf("expected clusters[0].tags env duplicate first, got %q", msg)
	}

	// Duplicate in an earlier include condition beats a later cluster dup.
	doc = `{` + base + `,` +
		`"include":[{"env":"prod","env":"dev"}],` +
		`"clusters":[{"id":"a","disabled":true,"disabled":false}]}`
	err = parseDupErr(t, doc)
	msg = err.Error()
	if !strings.Contains(msg, `"env"`) || !strings.Contains(msg, "$.include[0]") {
		t.Fatalf("expected include[0] env duplicate first, got %q", msg)
	}
}

func TestParse_DuplicateBeatsBusinessErrors(t *testing.T) {
	// Duplicate member plus missing app: duplicate error must come first.
	doc := `{"revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","disabled":true,"disabled":false}]}`
	err := parseDupErr(t, doc)
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected duplicate error before business error, got %q", err)
	}

	// Duplicate plus empty tag key: duplicate wins.
	doc = `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","tags":{"env":1,"env":2,"":3}}]}`
	err = parseDupErr(t, doc)
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("expected duplicate error before tag-key error, got %q", err)
	}

	// With no duplicate, business errors are still reported as before.
	doc = `{"revision":"r","image":"i","batchSize":1,"clusters":[]}`
	if _, err := ParseReleaseInput([]byte(doc)); err == nil ||
		!strings.Contains(err.Error(), "app") {
		t.Fatalf("expected missing app error, got %v", err)
	}
}

func TestParse_ValidConfigsStillPlan(t *testing.T) {
	raw := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3", "tags": {"env": "prod"}},
			{"id": "c1", "tags": {"env": "dev"}},
			{"id": "c2", "disabled": true, "tags": {"env": "prod"}},
			{"id": "c4", "tags": {"env": "prod", "region": "us"}}
		],
		"include": [{"env": "prod"}],
		"exclude": [{"region": "us"}]
	}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// c1 is dev -> include-not-matched; c2 disabled; c4 prod+us -> excluded;
	// c3 selected.
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c3" {
		t.Fatalf("unexpected plan: %+v", plan.Batches)
	}
	if len(plan.Excluded) != 3 {
		t.Fatalf("expected 3 excluded, got %+v", plan.Excluded)
	}
}

// runCLI invokes the darksafe command with the given arguments and returns
// exit code, stdout and stderr.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "./cmd/darksafe"}, args...)...)
	cmd.Dir = ".."
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run command: %v", err)
		}
	}
	return code, stdout.String(), stderr.String()
}

func TestPlanCLI_DuplicateMemberFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c1", "disabled": true, "disabled": false},
			{"id": "c2"}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "disabled") || !strings.Contains(stderr, "$.clusters[0]") {
		t.Fatalf("stderr should name field and position, got %q", stderr)
	}
}

// TestPlanCLI_DuplicateEscapedMemberFailsCleanly drives the CLI with a file
// whose duplicate only becomes visible after Unicode decoding: it must exit
// non-zero, explain the decoded name and its object position on stderr, and
// keep stdout empty (no partially generated plan).
func TestPlanCLI_DuplicateEscapedMemberFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c1", "tags": {"env": "prod", "` + jsonUEsc('e') + `nv": "dev"}},
			{"id": "c2"}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, `"env"`) || !strings.Contains(stderr, "$.clusters[0].tags") {
		t.Fatalf("stderr should name decoded field and position, got %q", stderr)
	}
}

func TestPlanCLI_ValidFileStillPlans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [{"id": "c2"}, {"id": "c1"}]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, `"c1"`) || !strings.Contains(stdout, `"c2"`) {
		t.Fatalf("expected plan on stdout, got %q", stdout)
	}
}

// TestPlanCLI_EscapedSpellingsStillPlan checks the CLI accepts a legitimate
// config that uses Unicode escape spellings and outputs the usual plan.
func TestPlanCLI_EscapedSpellingsStillPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [{"id": "c2"}, {"id": "c1", "tags": {"` + jsonUEsc('e') + `nv": "dev"}}]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, `"c1"`) || !strings.Contains(stdout, `"c2"`) {
		t.Fatalf("expected plan on stdout, got %q", stdout)
	}
}
