package darksafe

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests guard the rule that duplicate JSON object members are detected
// by their names *after JSON string decoding*: a member written directly
// ("env") and one written via Unicode escapes are the same member, including
// non-BMP characters written as surrogate pairs. The production check already
// walks decoded tokens (checkDuplicateMembers in plan.go); these cases make
// sure that behavior cannot regress.
//
// JSON escape spellings are built at runtime by uHex/escRunes below rather
// than written literally, and they only ever occur inside member names, so
// each document isolates the name-decoding rule.

// uHexPrefix is the two-character start of a JSON Unicode escape ("\u"),
// assembled at runtime.
func uHexPrefix() string { return string('\\') + "u" }

// uHex renders one rune the way JSON accepts it inside a string: a BMP rune
// becomes a \uXXXX escape and a supplementary-plane rune becomes a pair of
// UTF-16 surrogate escapes.
func uHex(r rune) string {
	if r <= 0xFFFF {
		return uHexPrefix() + fmt.Sprintf("%04x", r)
	}
	r -= 0x10000
	hi := 0xD800 + r/0x400
	lo := 0xDC00 + r%0x400
	return uHexPrefix() + fmt.Sprintf("%04x", hi) + uHexPrefix() + fmt.Sprintf("%04x", lo)
}

// escRunes renders every rune as a Unicode escape spelling.
func escRunes(rs ...rune) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(uHex(r))
	}
	return b.String()
}

// escAll escapes every rune of a name.
func escAll(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteString(uHex(r))
	}
	return b.String()
}

// partialEsc escapes only a name's first rune and leaves the rest literal,
// exercising the mixed direct/escaped spelling.
func partialEsc(s string) string {
	var b strings.Builder
	first := true
	for _, r := range s {
		if first {
			b.WriteString(uHex(r))
			first = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// qkey renders a (possibly escape-spelled) member name as a quoted JSON key.
func qkey(spelling string) string { return `"` + spelling + `"` }

const decodedDupBase = `"app":"a","revision":"r","image":"i","batchSize":1`

// TestParse_DecodedDuplicateNames documents that two member names which differ
// only in how their characters are escaped are duplicates once decoded, in
// every kind of object; the error names the decoded name and the object's
// position.
func TestParse_DecodedDuplicateNames(t *testing.T) {
	cases := []struct {
		name      string
		body      string // JSON body without outer braces
		wantField string
		wantPath  string
	}{
		{
			name:      "top-level direct first then escaped",
			body:      `"app":"a",` + qkey(partialEsc("app")) + `:"b","revision":"r","image":"i","batchSize":1,"clusters":[]`,
			wantField: "app",
			wantPath:  "$",
		},
		{
			name:      "top-level escaped first then direct",
			body:      qkey(partialEsc("app")) + `:"a","app":"b","revision":"r","image":"i","batchSize":1,"clusters":[]`,
			wantField: "app",
			wantPath:  "$",
		},
		{
			name:      "cluster tags direct then escaped",
			body:      decodedDupBase + `,"clusters":[{"id":"x","tags":{"env":"prod",` + qkey(partialEsc("env")) + `:"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "cluster tags escaped first then direct",
			body:      decodedDupBase + `,"clusters":[{"id":"x","tags":{` + qkey(partialEsc("env")) + `:"prod","env":"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "cluster tags both fully escaped",
			body: decodedDupBase + `,"clusters":[{"id":"x","tags":{` +
				qkey(escAll("env")) + `:"prod",` + qkey(escAll("env")) + `:"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "mixed escapes at different character positions",
			body: decodedDupBase + `,"clusters":[{"id":"x","tags":{` +
				// first letter of "env" escaped vs. last letter escaped
				qkey(uHex('e')+"nv") + `:"prod",` + qkey("en"+uHex('v')) + `:"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name: "include condition direct and escaped",
			body: decodedDupBase + `,"clusters":[],"include":[{` +
				`"env":"prod",` + qkey(partialEsc("env")) + `:"dev"}]`,
			wantField: "env",
			wantPath:  "$.include[0]",
		},
		{
			name: "exclude condition direct and escaped",
			body: decodedDupBase + `,"clusters":[],"exclude":[{` +
				`"region":"us",` + qkey(partialEsc("region")) + `:"eu"}]`,
			wantField: "region",
			wantPath:  "$.exclude[0]",
		},
		{
			name:      "cluster object own member id",
			body:      decodedDupBase + `,"clusters":[{` + qkey(partialEsc("id")) + `:"a","id":"b"}]`,
			wantField: "id",
			wantPath:  "$.clusters[0]",
		},
		{
			name:      "unknown extra field nested object",
			body:      decodedDupBase + `,"clusters":[],"meta":{"x":1,` + qkey(partialEsc("x")) + `:2}`,
			wantField: "x",
			wantPath:  "$.meta",
		},
		{
			name:      "unknown extra field object nested inside array",
			body:      decodedDupBase + `,"clusters":[],"items":[{"y":1,` + qkey(partialEsc("y")) + `:2}]`,
			wantField: "y",
			wantPath:  "$.items[0]",
		},
		{
			name: "non-ASCII name direct versus escape",
			body: decodedDupBase + `,"clusters":[],"meta":{` +
				qkey("é") + `:1,` + qkey(uHex(0x00E9)) + `:2}`,
			wantField: "é",
			wantPath:  "$.meta",
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

// TestParse_DecodedDuplicateNames_SurrogatePairs covers a name with a legal
// supplementary-plane character: written directly it is the same name as a
// surrogate-pair escape, in either order and when both names use the pair.
func TestParse_DecodedDuplicateNames_SurrogatePairs(t *testing.T) {
	const smile = "😀" // U+1F600 written directly
	pair := uHex(0x1F600)
	docs := []string{
		`{` + decodedDupBase + `,"clusters":[{"id":"x","tags":{` + qkey(smile) + `:1,` + qkey(pair) + `:2}}]}`,
		`{` + decodedDupBase + `,"clusters":[{"id":"x","tags":{` + qkey(pair) + `:1,` + qkey(smile) + `:2}}]}`,
		`{` + decodedDupBase + `,"clusters":[{"id":"x","tags":{` + qkey(pair) + `:1,` + qkey(pair) + `:2}}]}`,
	}
	for i, doc := range docs {
		err := parseDupErr(t, doc)
		msg := err.Error()
		if !strings.Contains(msg, smile) {
			t.Fatalf("case %d: error should show decoded non-BMP name, got %q", i, msg)
		}
		if !strings.Contains(msg, "$.clusters[0].tags") {
			t.Fatalf("case %d: error should locate tags object, got %q", i, msg)
		}
	}

	// The same check applies at the top level and inside an unknown field.
	err := parseDupErr(t, `{`+qkey(pair)+`:1,`+qkey(smile)+`:2,`+decodedDupBase+`,"clusters":[]}`)
	msg := err.Error()
	if !strings.Contains(msg, smile) || !strings.HasSuffix(msg, "于 $") {
		t.Fatalf("top-level non-BMP duplicate not reported at root: %v", err)
	}
	err = parseDupErr(t, `{`+decodedDupBase+`,"clusters":[],"meta":{`+
		qkey(smile)+`:1,`+qkey(pair)+`:2}}`)
	if !strings.Contains(err.Error(), smile) || !strings.Contains(err.Error(), "$.meta") {
		t.Fatalf("unknown-field non-BMP duplicate not reported: %v", err)
	}
}

// TestParse_DecodedDuplicateRejectedForAllValuePairs mirrors the literal
// value-pair rule using differently-spelled names: equal values, different
// values, and different value types must all reject the whole configuration.
func TestParse_DecodedDuplicateRejectedForAllValuePairs(t *testing.T) {
	// Same value under both spellings.
	for _, val := range []string{`true`, `false`, `null`, `1`, `1.5`, `"str"`, `[1,2]`, `{"k":"v"}`} {
		t.Run("same value "+val, func(t *testing.T) {
			doc := `{` + decodedDupBase + `,"clusters":[` +
				`{"id":"x","tags":{"env":` + val + `,` + qkey(partialEsc("env")) + `:` + val + `}}]}`
			err := parseDupErr(t, doc)
			if !strings.Contains(err.Error(), `"env"`) ||
				!strings.Contains(err.Error(), "$.clusters[0].tags") {
				t.Fatalf("escaped-name same-value pair must be rejected, got %q", err)
			}
		})
	}
	// Different values and different value types.
	pairs := [][2]string{
		{`"prod"`, `"dev"`},
		{`1`, `2`},
		{`1`, `true`},
		{`"a"`, `null`},
		{`[]`, `{}`},
		{`false`, `"false"`},
	}
	for _, p := range pairs {
		t.Run("values "+p[0]+" "+p[1], func(t *testing.T) {
			doc := `{` + decodedDupBase + `,"clusters":[` +
				`{"id":"x","tags":{` + qkey(partialEsc("env")) + `:` + p[0] + `,"env":` + p[1] + `}}]}`
			err := parseDupErr(t, doc)
			if !strings.Contains(err.Error(), `"env"`) {
				t.Fatalf("escaped-name pair must be rejected regardless of values, got %q", err)
			}
		})
	}
}

// TestParse_DecodedDuplicateArrayIndexAndFileOrder checks that a decoded
// duplicate inside an object nested in an array keeps the actual array index,
// and that when several duplicates exist the earliest second occurrence in the
// file is reported — not a name-sorted choice.
func TestParse_DecodedDuplicateArrayIndexAndFileOrder(t *testing.T) {
	// Object at index 1 of an unknown-field array.
	err := parseDupErr(t, `{`+decodedDupBase+`,"clusters":[],"items":[{"ok":1},{"y":1,`+
		qkey(partialEsc("y"))+`:2}]}`)
	if !strings.Contains(err.Error(), `"y"`) || !strings.Contains(err.Error(), "$.items[1]") {
		t.Fatalf("expected duplicate at $.items[1], got %q", err)
	}

	// Second cluster object, escaped duplicate of its own member.
	err = parseDupErr(t, `{`+decodedDupBase+`,"clusters":[{"id":"a"},{`+
		qkey(partialEsc("id"))+`:"b","id":"c"}]}`)
	if !strings.Contains(err.Error(), `"id"`) || !strings.Contains(err.Error(), "$.clusters[1]") {
		t.Fatalf("expected duplicate at $.clusters[1], got %q", err)
	}

	// Two decoded duplicates: "z" sorts after "a", but z's second occurrence
	// appears earlier in the file, so z must be reported.
	err = parseDupErr(t, `{`+decodedDupBase+`,"clusters":[],"meta":{`+
		`"z":1,`+qkey(partialEsc("z"))+`:2,"a":3,`+qkey(partialEsc("a"))+`:4}}`)
	if !strings.Contains(err.Error(), `"z"`) || !strings.Contains(err.Error(), "$.meta") {
		t.Fatalf("expected earliest second occurrence (z), got %q", err)
	}

	// Earlier object beats a later object regardless of member names.
	err = parseDupErr(t, `{`+decodedDupBase+`,"clusters":[`+
		`{"id":"a","tags":{"zzz":1,`+qkey(partialEsc("zzz"))+`:2}},`+
		`{"id":"b","tags":{"aaa":1,`+qkey(partialEsc("aaa"))+`:2}}]}`)
	if !strings.Contains(err.Error(), "zzz") || !strings.Contains(err.Error(), "$.clusters[0].tags") {
		t.Fatalf("expected clusters[0] duplicate first in file order, got %q", err)
	}
}

// TestParse_DecodedDuplicateBeatsBusinessValidation ensures a decoded
// duplicate is reported before business-field validation — even when the
// configuration also lacks a required application name.
func TestParse_DecodedDuplicateBeatsBusinessValidation(t *testing.T) {
	// Missing "app" plus an escaped duplicate: duplicate must win.
	doc := `{"revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","disabled":true,` + qkey(partialEsc("disabled")) + `:false}]}`
	err := parseDupErr(t, doc)
	msg := err.Error()
	if !strings.Contains(msg, `"disabled"`) || !strings.Contains(msg, "$.clusters[0]") {
		t.Fatalf("expected escaped duplicate before missing-app error, got %q", msg)
	}

	// Escaped duplicate in tags plus an empty tag key: duplicate wins.
	doc = `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","tags":{` + qkey(partialEsc("env")) + `:1,"env":2,"":3}}]}`
	err = parseDupErr(t, doc)
	if !strings.Contains(err.Error(), `"env"`) {
		t.Fatalf("expected escaped duplicate before empty-key error, got %q", err)
	}

	// Escaped duplicate in an unknown field plus missing app: still duplicate.
	doc = `{"revision":"r","image":"i","batchSize":1,"clusters":[],` +
		`"meta":{"x":1,` + qkey(partialEsc("x")) + `:2}}`
	err = parseDupErr(t, doc)
	if !strings.Contains(err.Error(), `"x"`) || !strings.Contains(err.Error(), "$.meta") {
		t.Fatalf("expected unknown-field duplicate before business error, got %q", err)
	}
}

// TestParse_DecodedDistinctNamesRemainValid guards legitimate inputs:
// comparison is exact equality of the decoded strings, with no case folding,
// trimming, Unicode normalization, or unification across separate objects.
func TestParse_DecodedDistinctNamesRemainValid(t *testing.T) {
	valid := []struct {
		name string
		body string
	}{
		{
			name: "case differs after decoding",
			body: decodedDupBase + `,"clusters":[{"id":"x","tags":{"env":"prod",` +
				qkey(escAll("ENV")) + `:"dev"}}]`,
		},
		{
			name: "leading space vs no space",
			body: decodedDupBase + `,"clusters":[{"id":"x","tags":{" env":"prod",` +
				qkey(escAll("env")) + `:"dev"}}]`,
		},
		{
			name: "precomposed letter vs decomposed letters",
			// "é" (U+00E9) is a different string from "e" + U+0301; JSON does
			// not normalize, so these are distinct members.
			body: decodedDupBase + `,"clusters":[{"id":"x","tags":{` +
				qkey("é") + `:"1",` + qkey(escRunes('e', 0x0301)) + `:"2"}}]`,
		},
		{
			name: "same tag key on two different clusters",
			body: decodedDupBase + `,"clusters":[` +
				`{"id":"a","tags":{"env":"prod"}},` +
				`{"id":"b","tags":{` + qkey(partialEsc("env")) + `:"dev"}}]`,
		},
		{
			name: "same key in parent and nested unknown objects",
			body: decodedDupBase + `,"clusters":[],"meta":{` +
				qkey(partialEsc("env")) + `:"x","nested":{"env":"y"}}`,
		},
		{
			name: "same key in two include conditions",
			body: decodedDupBase + `,"clusters":[],"include":[{` +
				qkey(partialEsc("env")) + `:"prod"},{"env":"dev"}]`,
		},
		{
			name: "two different non-BMP names",
			// U+1F600 (😀) vs U+1F601 (😁), one via a surrogate pair.
			body: decodedDupBase + `,"clusters":[{"id":"x","tags":{` +
				qkey("😀") + `:"1",` + qkey(uHex(0x1F601)) + `:"2"}}]`,
		},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseReleaseInput([]byte("{" + tc.body + "}")); err != nil {
				t.Fatalf("distinct decoded names must not be duplicates: %v", err)
			}
		})
	}
}

// TestParse_EscapedAndDirectNamesProduceEquivalentPlans checks that escaped
// and direct spellings of the same member names express identical content:
// app info, cluster filtering, batch arrangement, and non-selection reasons
// must match between the two documents.
func TestParse_EscapedAndDirectNamesProduceEquivalentPlans(t *testing.T) {
	direct := `{
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
	// Same document with every structural member name spelled with its first
	// letter escaped; values stay literal so only name decoding is exercised.
	escaped := `{
		` + qkey(partialEsc("app")) + `: "payments", ` + qkey(partialEsc("revision")) + `: "v1.2.3", ` + qkey(partialEsc("image")) + `: "reg/payments:v1.2.3",
		` + qkey(partialEsc("batchSize")) + `: 2,
		` + qkey(partialEsc("clusters")) + `: [
			{` + qkey(partialEsc("id")) + `: "c3", ` + qkey(partialEsc("tags")) + `: {` + qkey(partialEsc("env")) + `: "prod"}},
			{` + qkey(partialEsc("id")) + `: "c1", ` + qkey(partialEsc("tags")) + `: {` + qkey(partialEsc("env")) + `: "dev"}},
			{` + qkey(partialEsc("id")) + `: "c2", ` + qkey(partialEsc("disabled")) + `: true, ` + qkey(partialEsc("tags")) + `: {` + qkey(partialEsc("env")) + `: "prod"}},
			{` + qkey(partialEsc("id")) + `: "c4", ` + qkey(partialEsc("tags")) + `: {` + qkey(partialEsc("env")) + `: "prod", ` + qkey(partialEsc("region")) + `: "us"}}
		],
		` + qkey(partialEsc("include")) + `: [{` + qkey(partialEsc("env")) + `: "prod"}],
		` + qkey(partialEsc("exclude")) + `: [{` + qkey(partialEsc("region")) + `: "us"}]
	}`
	plan1 := mustPlan(t, direct, "direct")
	plan2 := mustPlan(t, escaped, "escaped")
	if plan1.App != plan2.App {
		t.Fatalf("app info differs: %+v vs %+v", plan1.App, plan2.App)
	}
	if len(plan1.Batches) != len(plan2.Batches) {
		t.Fatalf("batch count differs: %+v vs %+v", plan1.Batches, plan2.Batches)
	}
	for i := range plan1.Batches {
		if plan1.Batches[i].Index != plan2.Batches[i].Index ||
			strings.Join(plan1.Batches[i].Clusters, ",") != strings.Join(plan2.Batches[i].Clusters, ",") {
			t.Fatalf("batch %d differs: %+v vs %+v", i, plan1.Batches[i], plan2.Batches[i])
		}
	}
	if len(plan1.Excluded) != len(plan2.Excluded) {
		t.Fatalf("excluded count differs: %+v vs %+v", plan1.Excluded, plan2.Excluded)
	}
	for i := range plan1.Excluded {
		if plan1.Excluded[i] != plan2.Excluded[i] {
			t.Fatalf("excluded[%d] differs: %+v vs %+v", i, plan1.Excluded[i], plan2.Excluded[i])
		}
	}

	// Escaped spelling of the optional spreadBy field and fault-domain tag key
	// must behave identically to the direct spelling.
	withSpreadDirect := `{` + decodedDupBase + `,"spreadBy":"zone","clusters":[` +
		`{"id":"a","tags":{"zone":"z1"}},{"id":"b","tags":{"zone":"z1"}},` +
		`{"id":"c","tags":{"zone":"z2"}}]}`
	withSpreadEscaped := `{` + decodedDupBase + `,` + qkey(partialEsc("spreadBy")) + `:"zone","clusters":[` +
		`{"id":"a","tags":{` + qkey(partialEsc("zone")) + `:"z1"}},` +
		`{"id":"b","tags":{` + qkey(partialEsc("zone")) + `:"z1"}},` +
		`{"id":"c","tags":{` + qkey(partialEsc("zone")) + `:"z2"}}]}`
	p1 := mustPlan(t, withSpreadDirect, "direct spreadBy")
	p2 := mustPlan(t, withSpreadEscaped, "escaped spreadBy")
	if len(p1.Batches) != len(p2.Batches) {
		t.Fatalf("spread batch count differs: %+v vs %+v", p1.Batches, p2.Batches)
	}
	for i := range p1.Batches {
		if strings.Join(p1.Batches[i].Clusters, ",") != strings.Join(p2.Batches[i].Clusters, ",") {
			t.Fatalf("spread batch %d differs: %+v vs %+v", i, p1.Batches[i], p2.Batches[i])
		}
	}
}

// mustPlan parses and plans a document, failing the test on either error.
func mustPlan(t *testing.T, doc, label string) ReleasePlan {
	t.Helper()
	in, err := ParseReleaseInput([]byte(doc))
	if err != nil {
		t.Fatalf("%s config: %v", label, err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("%s plan: %v", label, err)
	}
	return plan
}

// TestPlanCLI_DecodedDuplicateMemberFailsCleanly is the command-level
// guarantee: reading such a file via "plan" exits non-zero, explains the
// decoded duplicate and its position on stderr, and writes no partial plan to
// stdout.
func TestPlanCLI_DecodedDuplicateMemberFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c1", "tags": {"env": "prod", ` + qkey(partialEsc("env")) + `: "dev"}},
			{"id": "c2"}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty (no partial plan), got %q", stdout)
	}
	if !strings.Contains(stderr, `"env"`) {
		t.Fatalf("stderr should name the decoded field, got %q", stderr)
	}
	if !strings.Contains(stderr, "$.clusters[0].tags") {
		t.Fatalf("stderr should name the object position, got %q", stderr)
	}
}
