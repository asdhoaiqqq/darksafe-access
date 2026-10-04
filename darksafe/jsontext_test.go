package darksafe

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This file covers the JSON text rule: every string a release document
// carries must preserve the user's actual text. encoding/json silently turns
// invalid raw UTF-8 bytes and unpaired \uXXXX surrogate escapes into the
// replacement character U+FFFD; because the decoded text then feeds cluster
// identity comparison, label matching and fault-domain batching, two
// different original inputs could collapse onto one identifier or label.
// Such a document must fail to parse outright, at the exact string that is
// broken, before business validation or plan computation — while legitimate
// text (Chinese, surrogate pairs spelling non-BMP characters, a genuinely
// written U+FFFD, and an escaped-backslash "\uD800") keeps working exactly as
// before.

// utf8Case is one rejection document plus the location the error must name.
type textRejectCase struct {
	name string
	raw  []byte
	path string
}

// badUTF8 inserts one invalid byte (0xFF) between the two halves of a JSON
// string at the marked point.
func badUTF8(prefix, suffix string) []byte {
	return []byte(prefix + string([]byte{0xFF}) + suffix)
}

func TestParse_InvalidUTF8RejectedEverywhere(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []textRejectCase{
		{
			name: "app value",
			raw:  badUTF8(`{"app":"a`, `","revision":"r","image":"i","batchSize":1,"clusters":[]}`),
			path: "$.app",
		},
		{
			name: "revision value",
			raw:  badUTF8(`{"app":"a","revision":"r`, `","image":"i","batchSize":1,"clusters":[]}`),
			path: "$.revision",
		},
		{
			name: "image value",
			raw:  badUTF8(`{"app":"a","revision":"r","image":"i`, `","batchSize":1,"clusters":[]}`),
			path: "$.image",
		},
		{
			name: "spreadBy value",
			raw:  badUTF8(`{`+base+`,"spreadBy":"z`, `","clusters":[]}`),
			path: "$.spreadBy",
		},
		{
			name: "cluster id",
			raw:  badUTF8(`{`+base+`,"clusters":[{"id":"x`, `"}]}`),
			path: "$.clusters[0].id",
		},
		{
			name: "disabled cluster id still checked",
			raw:  badUTF8(`{`+base+`,"clusters":[{"id":"x`, `","disabled":true}]}`),
			path: "$.clusters[0].id",
		},
		{
			name: "tag value of a cluster filters would drop",
			raw: badUTF8(`{`+base+`,"clusters":[{"id":"x","disabled":true,"tags":{"env":"v`,
				`"}}]}`),
			path: "$.clusters[0].tags.env",
		},
		{
			name: "include condition value",
			raw:  badUTF8(`{`+base+`,"clusters":[],"include":[{"env":"p`, `"}]}`),
			path: "$.include[0].env",
		},
		{
			name: "exclude condition value",
			raw:  badUTF8(`{`+base+`,"clusters":[],"exclude":[{"env":"t`, `"}]}`),
			path: "$.exclude[0].env",
		},
		{
			name: "unknown top-level value",
			raw:  badUTF8(`{`+base+`,"clusters":[],"note":"x`, `"}`),
			path: "$.note",
		},
		{
			name: "unknown deeply nested object value",
			raw:  badUTF8(`{`+base+`,"clusters":[],"meta":{"a":{"b":"v`, `"}}}`),
			path: "$.meta.a.b",
		},
		{
			name: "unknown array item",
			raw:  badUTF8(`{`+base+`,"clusters":[],"meta":["ok","v`, `"]}`),
			path: "$.meta[1]",
		},
		{
			name: "cluster array string element",
			raw:  badUTF8(`{`+base+`,"clusters":["x`, `"]}`),
			path: "$.clusters[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := ParseReleaseInput(tc.raw)
			assertTextError(t, err, invalidTextUTF8, tc.path)
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("returned config must be zero value, got %+v", in)
			}
		})
	}
}

func TestParse_InvalidUTF8MemberNamePointsAtOwningObject(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []textRejectCase{
		{
			name: "root owns the broken name",
			raw:  badUTF8(`{`+base+`,"clusters":[],"k`, `":1}`),
			path: "$",
		},
		{
			name: "tags object owns the broken name",
			raw:  badUTF8(`{`+base+`,"clusters":[{"id":"x","tags":{"k`, `":"v"}}]}`),
			path: "$.clusters[0].tags",
		},
		{
			name: "include condition owns the broken name",
			raw:  badUTF8(`{`+base+`,"clusters":[],"include":[{"k`, `":"v"}]}`),
			path: "$.include[0]",
		},
		{
			name: "unknown object owns the broken name",
			raw:  badUTF8(`{`+base+`,"clusters":[],"meta":{"k`, `":1}}`),
			path: "$.meta",
		},
		{
			name: "object inside unknown array owns the broken name",
			raw:  badUTF8(`{`+base+`,"clusters":[],"items":[{"k`, `":1}]}`),
			path: "$.items[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReleaseInput(tc.raw)
			assertTextError(t, err, invalidTextUTF8, tc.path)
		})
	}
}

func TestParse_UnpairedSurrogateRejectedEverywhere(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	hi := jsonUEsc(0xD800)
	lo := jsonUEsc(0xDC00)
	cases := []textRejectCase{
		{
			name: "lone high in app",
			raw:  []byte(`{"app":"a` + hi + `","revision":"r","image":"i","batchSize":1,"clusters":[]}`),
			path: "$.app",
		},
		{
			name: "lone low in revision",
			raw:  []byte(`{"app":"a","revision":"r` + lo + `","image":"i","batchSize":1,"clusters":[]}`),
			path: "$.revision",
		},
		{
			name: "lone high at end of cluster id",
			raw:  []byte(`{` + base + `,"clusters":[{"id":"x` + hi + `"}]}`),
			path: "$.clusters[0].id",
		},
		{
			name: "lone low at start of cluster id",
			raw:  []byte(`{` + base + `,"clusters":[{"id":"` + lo + `x"}]}`),
			path: "$.clusters[0].id",
		},
		{
			name: "disabled cluster id with lone surrogate still checked",
			raw:  []byte(`{` + base + `,"clusters":[{"id":"x` + hi + `","disabled":true}]}`),
			path: "$.clusters[0].id",
		},
		{
			name: "filtered-out cluster tag value still checked",
			raw: []byte(`{` + base + `,"clusters":[{"id":"x","tags":{"env":"dev` + hi +
				`"}}],"include":[{"env":"prod"}]}`),
			path: "$.clusters[0].tags.env",
		},
		{
			name: "tag member name lone high",
			raw: []byte(`{` + base + `,"clusters":[{"id":"x","tags":{"k` + hi +
				`":"v"}}]}`),
			path: "$.clusters[0].tags",
		},
		{
			name: "include value lone low",
			raw:  []byte(`{` + base + `,"clusters":[],"include":[{"env":"p` + lo + `"}]}`),
			path: "$.include[0].env",
		},
		{
			name: "exclude member name lone high",
			raw:  []byte(`{` + base + `,"clusters":[],"exclude":[{"k` + hi + `":"v"}]}`),
			path: "$.exclude[0]",
		},
		{
			name: "spreadBy lone surrogate",
			raw:  []byte(`{` + base + `,"clusters":[],"spreadBy":"z` + hi + `"}`),
			path: "$.spreadBy",
		},
		{
			name: "unknown nested value",
			raw:  []byte(`{` + base + `,"clusters":[],"meta":{"a":["ok","v` + hi + `"]}}`),
			path: "$.meta.a[1]",
		},
		{
			name: "unknown member name",
			raw:  []byte(`{` + base + `,"clusters":[],"meta":{"k` + lo + `":1}}`),
			path: "$.meta",
		},
		{
			name: "top-level broken member name owned by root",
			raw:  []byte(`{` + base + `,"clusters":[],"k` + hi + `":1}`),
			path: "$",
		},
		{
			name: "high then low with literal between",
			raw:  []byte(`{` + base + `,"clusters":[{"id":"` + hi + `a` + lo + `"}]}`),
			path: "$.clusters[0].id",
		},
		{
			name: "complete pair followed by a lone high",
			raw: []byte(`{` + base + `,"clusters":[{"id":"` + jsonSurrogatePair('😀') + hi +
				`"}]}`),
			path: "$.clusters[0].id",
		},
		{
			name: "two highs in a row",
			raw:  []byte(`{` + base + `,"clusters":[{"id":"` + hi + hi + `"}]}`),
			path: "$.clusters[0].id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := ParseReleaseInput(tc.raw)
			assertTextError(t, err, invalidTextSurrogate, tc.path)
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("returned config must be zero value, got %+v", in)
			}
		})
	}
}

// assertTextError asserts an invalidTextError of the expected kind whose
// location names path, and that its message clearly distinguishes the two
// causes.
func assertTextError(t *testing.T, err error, kind, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a text error (%s at %s), got nil", kind, path)
	}
	e, ok := err.(*invalidTextError)
	if !ok {
		t.Fatalf("expected *invalidTextError, got %T: %v", err, err)
	}
	if e.kind != kind {
		t.Fatalf("kind = %q, want %q (error: %v)", e.kind, kind, err)
	}
	if e.path != path {
		t.Fatalf("path = %q, want %q (error: %v)", e.path, path, err)
	}
	msg := err.Error()
	switch kind {
	case invalidTextUTF8:
		if !strings.Contains(msg, "无效 UTF-8") {
			t.Fatalf("UTF-8 error must say so, got %q", msg)
		}
		if strings.Contains(msg, "代理项") {
			t.Fatalf("UTF-8 error must not mention surrogates, got %q", msg)
		}
	case invalidTextSurrogate:
		if !strings.Contains(msg, "代理项") {
			t.Fatalf("surrogate error must say so, got %q", msg)
		}
		if strings.Contains(msg, "UTF-8") {
			t.Fatalf("surrogate error must not mention UTF-8, got %q", msg)
		}
	}
	if !strings.Contains(msg, path) {
		t.Fatalf("error %q must name location %q", msg, path)
	}
}

// TestParse_TextErrorsBeatBusinessErrors verifies text problems surface
// before required-field and other business validation, even when several
// things are wrong at once.
func TestParse_TextErrorsBeatBusinessErrors(t *testing.T) {
	hi := jsonUEsc(0xD800)
	// Broken id bytes while revision is also missing: text wins.
	raw := badUTF8(`{"app":"a","image":"i","batchSize":1,"clusters":[{"id":"x`, `"}]}`)
	_, err := ParseReleaseInput(raw)
	assertTextError(t, err, invalidTextUTF8, "$.clusters[0].id")

	// Lone surrogate in a tag while the app value is whitespace-only.
	raw = []byte(`{"app":"  ","revision":"r","image":"i","batchSize":1,` +
		`"clusters":[{"id":"x","tags":{"env":"v` + hi + `"}}]}`)
	_, err = ParseReleaseInput(raw)
	assertTextError(t, err, invalidTextSurrogate, "$.clusters[0].tags.env")

	// Broken text on a disabled cluster beats the empty-candidates rule.
	raw = []byte(`{"app":"a","revision":"r","image":"i","batchSize":1,` +
		`"clusters":[{"id":"x` + hi + `","disabled":true}]}`)
	_, err = ParseReleaseInput(raw)
	assertTextError(t, err, invalidTextSurrogate, "$.clusters[0].id")
}

// TestParse_TextAndDuplicateShareFileOrder checks the two structural checks
// run in one document-order traversal: whichever broken token appears first
// in the file is reported.
func TestParse_TextAndDuplicateShareFileOrder(t *testing.T) {
	hi := jsonUEsc(0xD800)
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// Duplicate member in cluster 0, broken text later in cluster 1.
	raw := `{` + base + `,"clusters":[` +
		`{"id":"a","id":"b"},` +
		`{"id":"c` + hi + `"}]}`
	_, err := ParseReleaseInput([]byte(raw))
	if _, ok := err.(*duplicateMemberError); !ok {
		t.Fatalf("expected earlier duplicate error, got %v", err)
	}

	// Broken text in cluster 0's id, duplicate later in cluster 1.
	raw = `{` + base + `,"clusters":[` +
		`{"id":"a` + hi + `"},` +
		`{"id":"c","id":"d"}]}`
	_, err = ParseReleaseInput([]byte(raw))
	assertTextError(t, err, invalidTextSurrogate, "$.clusters[0].id")

	// Within one object, a broken member value earlier than the duplicate
	// member name: the value token is read first.
	raw = `{` + base + `,"clusters":[{"id":"x","tags":{"a":"` + hi + `","b":1,"b":2}}]}`
	_, err = ParseReleaseInput([]byte(raw))
	assertTextError(t, err, invalidTextSurrogate, "$.clusters[0].tags.a")
}

// TestParse_OneTokenWithBothProblemsReportsUTF8 fixes the deterministic
// choice when one string carries both defects: the raw UTF-8 failure is
// diagnosed first.
func TestParse_OneTokenWithBothProblemsReportsUTF8(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	raw := badUTF8(`{`+base+`,"clusters":[{"id":"x\uD800y`, `"}]}`)
	_, err := ParseReleaseInput(raw)
	assertTextError(t, err, invalidTextUTF8, "$.clusters[0].id")
}

// TestParse_LegalUTF8PreservedAndMatched verifies Chinese and other valid
// multibyte text is kept byte-for-byte and participates in exact matching,
// including a non-BMP character written directly.
func TestParse_LegalUTF8PreservedAndMatched(t *testing.T) {
	raw := `{
		"app": "付款服务", "revision": "修订一", "image": "镜像:最新",
		"batchSize": 2, "spreadBy": "机房",
		"clusters": [
			{"id": "集群甲", "tags": {"环境": "生产", "机房": "北京"}},
			{"id": "集群乙", "tags": {"环境": "生产", "机房": "上海"}},
			{"id": "集群丙", "tags": {"环境": "测试", "机房": "北京"}}
		],
		"include": [{"环境": "生产"}]
	}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatalf("legal UTF-8 must parse: %v", err)
	}
	if in.App != "付款服务" || in.Revision != "修订一" || in.Image != "镜像:最新" {
		t.Fatalf("app info not preserved: %+v", in.App)
	}
	if in.SpreadBy != "机房" {
		t.Fatalf("spreadBy not preserved: %q", in.SpreadBy)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// Both selected clusters carry distinct 机房 fault domains; IDs sort by
	// code point (乙 U+4E59 before 甲 U+7532) and stay in one batch of two.
	if got := strings.Join(plan.Batches[0].Clusters, ","); got != "集群乙,集群甲" {
		t.Fatalf("exact Chinese matching/spreading wrong: %q", got)
	}
	if len(plan.Excluded) != 1 || plan.Excluded[0].ID != "集群丙" {
		t.Fatalf("expected 集群丙 filtered out, got %+v", plan.Excluded)
	}
}

// TestParse_SurrogatePairsSpellNonBMPExactly covers the compatibility rule:
// a paired high/low escape denotes one non-BMP character, equal for matching
// and for duplicate-member detection to the same character written directly.
func TestParse_SurrogatePairsSpellNonBMPExactly(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	pair := jsonSurrogatePair('😀')

	// Tag key and value written as a pair match a condition written directly,
	// and vice versa.
	literal := `{` + base + `,"clusters":[{"id":"c","tags":{"😀":"😀l"}}],` +
		`"include":[{"😀":"` + pair + `l"}]}`
	escapeFirst := `{` + base + `,"clusters":[{"id":"c","tags":{"` + pair +
		`":"` + pair + `l"}}],"include":[{"😀":"😀l"}]}`
	for name, raw := range map[string]string{"literal cond": literal, "escaped cluster": escapeFirst} {
		in, err := ParseReleaseInput([]byte(raw))
		if err != nil {
			t.Fatalf("%s: pair spelling must parse: %v", name, err)
		}
		plan, err := MakeReleasePlan(in)
		if err != nil {
			t.Fatalf("%s: plan: %v", name, err)
		}
		if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "c" {
			t.Fatalf("%s: exact pair/literal matching failed: %+v", name, plan.Batches)
		}
	}

	// Direct character and pair escape in two member names decode to one name:
	// that is a duplicate, exactly as for BMP spellings.
	dup := `{` + base + `,"clusters":[{"id":"x","tags":{"😀":"a","` + pair + `":"b"}}]}`
	if _, err := ParseReleaseInput([]byte(dup)); err == nil {
		t.Fatal("literal non-BMP name and paired-escape name must be a duplicate")
	} else if dme, ok := err.(*duplicateMemberError); !ok || dme.field != "😀" {
		t.Fatalf("expected duplicate member 😀, got %v", err)
	}

	// A different non-BMP rune stays distinct even though it shares a high
	// surrogate range.
	distinct := `{` + base + `,"clusters":[{"id":"x","tags":{` +
		`"` + jsonSurrogatePair('😀') + `":"a","` + jsonSurrogatePair('😎') + `":"b"}}]}`
	if _, err := ParseReleaseInput([]byte(distinct)); err != nil {
		t.Fatalf("different non-BMP names must stay distinct: %v", err)
	}
}

// TestParse_RealReplacementCharacterAccepted makes sure the character the
// decoder uses for substitution is legal when the user actually wrote it,
// whether directly or as the valid  escape, and matches exactly.
func TestParse_RealReplacementCharacterAccepted(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	for name, spelling := range map[string]string{
		"literal":   `"a�b"`,
		"u escaped": `"a` + jsonUEsc(0xFFFD) + `b"`,
	} {
		raw := `{` + base + `,"clusters":[{"id":"x","tags":{"note":` + spelling +
			`}}],"include":[{"note":"a�b"}]}`
		in, err := ParseReleaseInput([]byte(raw))
		if err != nil {
			t.Fatalf("%s: a genuinely written U+FFFD must be accepted: %v", name, err)
		}
		plan, err := MakeReleasePlan(in)
		if err != nil {
			t.Fatalf("%s: plan: %v", name, err)
		}
		if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "x" {
			t.Fatalf("%s: real U+FFFD must match exactly, got %+v", name, plan.Batches)
		}
	}
}

// TestParse_EscapedBackslashBeforeUIsLiteralText verifies that a backslash
// escaped as "\\" leaves ordinary characters behind: the JSON token
// "x\\uD800" is the literal seven-character text x, \, u, D, 8, 0, 0, not a
// Unicode escape and not a broken string.
func TestParse_EscapedBackslashBeforeUIsLiteralText(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":2`
	raw := `{` + base + `,"clusters":[{"id":"x\\uD800"},{"id":"\\uD800y"}]}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatalf("escaped-backslash text must be accepted: %v", err)
	}
	// Parsing preserves the exact file order and decoded literal text.
	if got := [2]string{in.Clusters[0].ID, in.Clusters[1].ID}; got != [2]string{`x\uD800`, `\uD800y`} {
		t.Fatalf("literal text/order not preserved: got %q", got)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// The plan sorts by ID; '\\' (U+005C) sorts before 'x'.
	if strings.Join(plan.Batches[0].Clusters, ",") != `\uD800y,x\uD800` {
		t.Fatalf("unexpected plan: %+v", plan.Batches)
	}

	// The same literal text written two equivalent ways still collapses for
	// the ordinary duplicate-ID rule.
	dup := `{` + base + `,"clusters":[{"id":"x\\uD800"},{"id":"x\\uD800"}]}`
	if _, err := ParseReleaseInput([]byte(dup)); err == nil ||
		!strings.Contains(err.Error(), `\uD800`) {
		t.Fatalf("expected duplicate literal id, got %v", err)
	}
}

// TestPlanCLI_BrokenTextFailsCleanly drives the command-line contract for
// both kinds of broken text: exit code 1, empty stdout, a message on stderr
// that distinguishes the cause and names the field or array item — including
// for a cluster that is disabled or would be filtered out.
func TestPlanCLI_BrokenTextFailsCleanly(t *testing.T) {
	hi := jsonUEsc(0xD800)
	cases := []struct {
		name string
		raw  []byte
		kind string
		path string
	}{
		{
			name: "surrogate in cluster id",
			raw: []byte(`{"app":"a","revision":"r","image":"i","batchSize":1,` +
				`"clusters":[{"id":"x` + hi + `"}]}`),
			kind: "代理项",
			path: "$.clusters[0].id",
		},
		{
			name: "invalid utf8 in unknown array item",
			raw: badUTF8(`{"app":"a","revision":"r","image":"i","batchSize":1,`+
				`"clusters":[],"meta":["ok","v`, `"]}`),
			kind: "无效 UTF-8",
			path: "$.meta[1]",
		},
		{
			name: "surrogate in disabled cluster tag",
			raw: []byte(`{"app":"a","revision":"r","image":"i","batchSize":1,` +
				`"clusters":[{"id":"x","disabled":true,"tags":{"env":"v` + hi + `"}}]}`),
			kind: "代理项",
			path: "$.clusters[0].tags.env",
		},
		{
			name: "surrogate in member name names owning object",
			raw: []byte(`{"app":"a","revision":"r","image":"i","batchSize":1,` +
				`"clusters":[{"id":"x","tags":{"k` + hi + `":"v"}}]}`),
			kind: "代理项",
			path: "$.clusters[0].tags",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "plan.json")
			if err := os.WriteFile(path, tc.raw, 0o644); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runPlanCLI(t, "plan", path)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty, got %q", stdout)
			}
			if !strings.Contains(stderr, tc.kind) {
				t.Fatalf("stderr must distinguish %q, got %q", tc.kind, stderr)
			}
			if !strings.Contains(stderr, tc.path) {
				t.Fatalf("stderr must name %q, got %q", tc.path, stderr)
			}
		})
	}
}

// TestPlanCLI_LegalUnicodeStillPlans checks the success path through the real
// binary: Chinese text, a surrogate pair and a genuine U+FFFD all appear in
// the plan unchanged.
func TestPlanCLI_LegalUnicodeStillPlans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "付款", "revision": "r", "image": "i", "batchSize": 2,
		"clusters": [
			{"id": "集` + jsonSurrogatePair('😀') + `"},
			{"id": "x�y"}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runPlanCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	for _, want := range []string{"付款", "集😀", "x�y"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout must preserve %q, got %q", want, stdout)
		}
	}
}
