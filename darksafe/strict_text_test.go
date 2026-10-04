package darksafe

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// badUTF8 is an isolated byte that is not valid UTF-8 anywhere in a string.
const badUTF8 = "\xff"

// parseTextErr parses data and returns the error; it fails the test if the
// corrupt-text document is accepted.
func parseTextErr(t *testing.T, data string) error {
	t.Helper()
	in, err := ParseReleaseInput([]byte(data))
	if err == nil {
		t.Fatalf("expected corrupt-text rejection for %q", data)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("corrupt text must yield the zero config, got %+v", in)
	}
	return err
}

// TestParse_InvalidUTF8RejectedEverywhere feeds one invalid UTF-8 byte
// through every string position of the configuration — application info,
// cluster identity, tags, conditions, spreadBy, and objects nested inside
// unknown extra fields — including a disabled cluster and a cluster that
// include/exclude rules would filter out. Every position must reject the
// whole document, naming the field or array item.
func TestParse_InvalidUTF8RejectedEverywhere(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name     string
		body     string // JSON body without outer braces
		wantPath string
	}{
		{"app", `"app":"a` + badUTF8 + `","revision":"r","image":"i","batchSize":1,"clusters":[]`, "$.app"},
		{"revision", `"app":"a","revision":"` + badUTF8 + `","image":"i","batchSize":1,"clusters":[]`, "$.revision"},
		{"image", `"app":"a","revision":"r","image":"img` + badUTF8 + `","batchSize":1,"clusters":[]`, "$.image"},
		{"spreadBy", base + `,"clusters":[],"spreadBy":"zone` + badUTF8 + `"`, "$.spreadBy"},
		{"cluster id", base + `,"clusters":[{"id":"c` + badUTF8 + `"}]`, "$.clusters[0].id"},
		{"second cluster id", base + `,"clusters":[{"id":"a"},{"id":"` + badUTF8 + `"}]`, "$.clusters[1].id"},
		{"disabled cluster id", base + `,"clusters":[{"id":"x` + badUTF8 + `","disabled":true}]`, "$.clusters[0].id"},
		{"filtered-out cluster tag", base + `,"clusters":[{"id":"x","tags":{"env":"pr` + badUTF8 + `d"}}],"exclude":[{"env":"pr�d"}]`, "$.clusters[0].tags.env"},
		{"tag value", base + `,"clusters":[{"id":"x","tags":{"env":"` + badUTF8 + `"}}]`, "$.clusters[0].tags.env"},
		{"include condition value", base + `,"clusters":[],"include":[{"env":"p` + badUTF8 + `"}]`, "$.include[0].env"},
		{"exclude condition value", base + `,"clusters":[],"exclude":[{"region":"` + badUTF8 + `"}]`, "$.exclude[0].region"},
		{"unknown field value", base + `,"clusters":[],"meta":"` + badUTF8 + `"`, "$.meta"},
		{"unknown nested object", base + `,"clusters":[],"meta":{"note":"` + badUTF8 + `"}`, "$.meta.note"},
		{"unknown nested array item", base + `,"clusters":[],"items":["ok","` + badUTF8 + `"]`, "$.items[1]"},
		{"unknown deep nesting", base + `,"clusters":[],"a":{"b":[{"c":"` + badUTF8 + `"}]}`, "$.a.b[0].c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseTextErr(t, "{"+tc.body+"}")
			msg := err.Error()
			if !strings.Contains(msg, "UTF-8") {
				t.Fatalf("error %q should identify invalid UTF-8", msg)
			}
			if strings.Contains(msg, "未配对") {
				t.Fatalf("error %q should not blame Unicode escapes", msg)
			}
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not contain path %q", msg, tc.wantPath)
			}
		})
	}
}

// TestParse_InvalidUTF8InMemberName puts the invalid byte in a member name:
// the error must point at the object owning the name, in known fields and in
// unknown extra fields alike.
func TestParse_InvalidUTF8InMemberName(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name     string
		body     string
		wantPath string
	}{
		{"top-level unknown name", base + `,"clusters":[],"me` + badUTF8 + `ta":1`, "$"},
		{"cluster field name", base + `,"clusters":[{"id":"x","dis` + badUTF8 + `abled":true}]`, "$.clusters[0]"},
		{"tag key", base + `,"clusters":[{"id":"x","tags":{"e` + badUTF8 + `nv":"prod"}}]`, "$.clusters[0].tags"},
		{"include condition key", base + `,"clusters":[],"include":[{"e` + badUTF8 + `nv":"prod"}]`, "$.include[0]"},
		{"unknown nested object name", base + `,"clusters":[],"meta":{"x` + badUTF8 + `":1}`, "$.meta"},
		{"unknown array object name", base + `,"clusters":[],"items":[{"y` + badUTF8 + `":1}]`, "$.items[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseTextErr(t, "{"+tc.body+"}")
			msg := err.Error()
			if !strings.Contains(msg, "UTF-8") || !strings.Contains(msg, "成员名") {
				t.Fatalf("error %q should identify an invalid-UTF-8 member name", msg)
			}
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not name owning object %q", msg, tc.wantPath)
			}
		})
	}
}

// TestParse_UnpairedSurrogateRejectedEverywhere covers \uXXXX escapes that
// name one half of a surrogate pair without its mate: a lone high surrogate,
// a lone low surrogate, a high surrogate followed by a BMP escape, a high
// surrogate at the end of a string, and a high surrogate followed by another
// high surrogate. Values and member names are both covered, at several
// positions of the configuration.
func TestParse_UnpairedSurrogateRejectedEverywhere(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	loneHigh := `\uD800`
	loneLow := `\uDC00`
	highThenBMP := `\uD800A`
	highThenHigh := `\uD800\uDBFF`
	cases := []struct {
		name     string
		body     string
		wantPath string
	}{
		{"app lone high", `"app":"` + loneHigh + `","revision":"r","image":"i","batchSize":1,"clusters":[]`, "$.app"},
		{"app lone low", `"app":"a` + loneLow + `","revision":"r","image":"i","batchSize":1,"clusters":[]`, "$.app"},
		{"app high then BMP escape", `"app":"` + highThenBMP + `","revision":"r","image":"i","batchSize":1,"clusters":[]`, "$.app"},
		{"app high at end of string", `"app":"ab` + loneHigh + `","revision":"r","image":"i","batchSize":1,"clusters":[]`, "$.app"},
		{"app high then high", `"app":"` + highThenHigh + `","revision":"r","image":"i","batchSize":1,"clusters":[]`, "$.app"},
		{"cluster id", base + `,"clusters":[{"id":"c` + loneHigh + `"}]`, "$.clusters[0].id"},
		{"disabled cluster id", base + `,"clusters":[{"id":"` + loneLow + `","disabled":true}]`, "$.clusters[0].id"},
		{"tag value", base + `,"clusters":[{"id":"x","tags":{"env":"` + loneHigh + `"}}]`, "$.clusters[0].tags.env"},
		{"tag key", base + `,"clusters":[{"id":"x","tags":{"e` + loneHigh + `nv":"prod"}}]`, "$.clusters[0].tags"},
		{"include condition value", base + `,"clusters":[],"include":[{"env":"` + loneHigh + `"}]`, "$.include[0].env"},
		{"exclude condition key", base + `,"clusters":[],"exclude":[{"reg` + loneLow + `ion":"us"}]`, "$.exclude[0]"},
		{"spreadBy", base + `,"clusters":[],"spreadBy":"zo` + loneHigh + `ne"`, "$.spreadBy"},
		{"unknown field value", base + `,"clusters":[],"meta":"` + loneHigh + `"`, "$.meta"},
		{"unknown member name", base + `,"clusters":[],"me` + loneHigh + `ta":1`, "$"},
		{"unknown nested array item", base + `,"clusters":[],"items":["` + loneLow + `"]`, "$.items[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseTextErr(t, "{"+tc.body+"}")
			msg := err.Error()
			if !strings.Contains(msg, "未配对") {
				t.Fatalf("error %q should identify an unpaired Unicode escape", msg)
			}
			if strings.Contains(msg, "UTF-8") {
				t.Fatalf("error %q should not blame UTF-8 bytes", msg)
			}
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not contain path %q", msg, tc.wantPath)
			}
		})
	}
}

// TestParse_LegalTextAccepted guards the compatibility side: valid UTF-8
// (Chinese and a literally written "�"), a backslash-escaped "D800" that is
// ordinary text rather than an escape, and non-BMP characters written
// directly or as a surrogate pair must all pass through unchanged.
func TestParse_LegalTextAccepted(t *testing.T) {
	// A literally written "�" is a legal character and is kept as-is.
	in, err := ParseReleaseInput([]byte(`{"app":"a�b","revision":"r","image":"i","batchSize":1,"clusters":[]}`))
	if err != nil {
		t.Fatalf("literal replacement character must be accepted: %v", err)
	}
	if in.App != "a�b" {
		t.Fatalf("literal replacement character rewritten: %q", in.App)
	}

	// "\\uD800" is the six ordinary characters D-8-0-0 after a backslash,
	// not a Unicode escape, and must not be mistaken for a corrupt one.
	in, err = ParseReleaseInput([]byte(`{"app":"a\\uD800b","revision":"r","image":"i","batchSize":1,"clusters":[]}`))
	if err != nil {
		t.Fatalf(`escaped-literal "\\uD800" text must be accepted: %v`, err)
	}
	if in.App != `a\uD800b` {
		t.Fatalf(`escaped-literal text rewritten: %q`, in.App)
	}

	// Chinese and other legal UTF-8 text is used exactly as written.
	in, err = ParseReleaseInput([]byte(`{"app":"支付服务","revision":"版本","image":"镜像","batchSize":1,"clusters":[{"id":"集群-一"}]}`))
	if err != nil {
		t.Fatalf("Chinese text must be accepted: %v", err)
	}
	if in.App != "支付服务" || in.Clusters[0].ID != "集群-一" {
		t.Fatalf("Chinese text rewritten: %+v", in)
	}

	// A non-BMP character written as a surrogate pair decodes to the same
	// string as the literal character: both spellings of "😀" as a cluster
	// ID are the same identity and collide.
	pair := jsonSurrogatePair('😀')
	doc := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"😀"},{"id":"` + pair + `"}]}`
	err = parseTextErr(t, doc)
	if !strings.Contains(err.Error(), "重复的集群标识") {
		t.Fatalf("surrogate pair and literal should be the same ID, got %v", err)
	}

	// A tag value written as a surrogate pair matches a literal condition.
	in, err = ParseReleaseInput([]byte(`{"app":"a","revision":"r","image":"i","batchSize":1,` +
		`"clusters":[{"id":"c1","tags":{"mood":"` + pair + `"}}],` +
		`"include":[{"mood":"😀"}]}`))
	if err != nil {
		t.Fatalf("surrogate-pair tag value must parse: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "c1" {
		t.Fatalf("surrogate-pair tag value did not match literal condition: %+v", plan)
	}
}

// TestParse_TextErrorBeatsBusinessAndDuplicateChecks ensures corrupt text is
// reported before any decoded string is compared: before business field
// validation and before the duplicate-member check, whose decoded-name
// comparison would be meaningless for rewritten names.
func TestParse_TextErrorBeatsBusinessAndDuplicateChecks(t *testing.T) {
	// Corrupt text plus a missing app field: the text error wins.
	err := parseTextErr(t, `{"revision":"r","image":"i","batchSize":1,"clusters":[{"id":"c`+badUTF8+`"}]}`)
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("expected text error before missing-app error, got %v", err)
	}

	// Corrupt text plus a duplicate member elsewhere: the text error wins,
	// because two names rewritten to "�" must never be reported as a
	// duplicate (or silently merged).
	err = parseTextErr(t, `{"app":"a","revision":"r","image":"i","batchSize":1,`+
		`"clusters":[{"id":"x`+badUTF8+`","tags":{"env":1,"env":2}}]}`)
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("expected text error before duplicate-member error, got %v", err)
	}

	// Two DIFFERENT corrupt names in one object must be a text error, not a
	// false duplicate of two rewritten "�" names.
	err = parseTextErr(t, `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],`+
		`"meta":{"`+badUTF8+`x":1,"`+"\xfe"+`x":2}}`)
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("expected text error for distinct corrupt names, got %v", err)
	}
}

// TestParse_MalformedJSONKeepsFormatError ensures the strict text check does
// not change the established format-error wording for structurally broken
// documents.
func TestParse_MalformedJSONKeepsFormatError(t *testing.T) {
	for _, doc := range []string{
		`{not json`,
		`{"app":"a",}`,
		`{"app":"a"`,
		`{"app": "\x"}`,
		`{"app":"a"} trailing`,
	} {
		_, err := ParseReleaseInput([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), "JSON 格式错误") {
			t.Fatalf("expected JSON format error for %q, got %v", doc, err)
		}
	}
}

// TestPlanCLI_InvalidUTF8FailsCleanly drives the CLI with invalid UTF-8 in a
// tag value: exit code 1, the reason on stderr, and nothing on stdout.
func TestPlanCLI_InvalidUTF8FailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"payments","revision":"v1","image":"img","batchSize":1,` +
		`"clusters":[{"id":"c1","tags":{"env":"pr` + badUTF8 + `d"}}]}`
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
	if !strings.Contains(stderr, "UTF-8") || !strings.Contains(stderr, "$.clusters[0].tags.env") {
		t.Fatalf("stderr should name the problem and position, got %q", stderr)
	}
}

// TestPlanCLI_UnpairedSurrogateFailsCleanly drives the CLI with an unpaired
// surrogate escape in an unknown extra field: even though the field would be
// ignored, its corrupt text rejects the whole configuration with exit code
// 1, a stderr reason, and empty stdout.
func TestPlanCLI_UnpairedSurrogateFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"payments","revision":"v1","image":"img","batchSize":1,` +
		`"clusters":[{"id":"c1"}],"meta":{"note":"\uD800"}}`
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
	if !strings.Contains(stderr, "未配对") || !strings.Contains(stderr, "$.meta.note") {
		t.Fatalf("stderr should name the problem and position, got %q", stderr)
	}
}
