package darksafe

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This file is the regression net for the ordering between the two
// whole-document JSON verdicts that run before business validation:
// duplicate-member (duplicateMemberError) and JSON format errors. Separate
// tests already cover each condition alone; these tests pin what a caller
// sees when ONE document contains both.
//
// The rule is strictly the reading order of the single structural pass:
//
//   - If the second occurrence of a member name has already been read inside
//     one object before the parse cannot continue, that already-decided
//     duplicate is the verdict — even if the duplicate's value is missing or
//     malformed, the enclosing object never closes, or extra content follows
//     the otherwise complete document. The first problem the user can fix
//     must not be replaced by a later error in the file.
//   - If unreadable JSON syntax (a number written as 1e, an extra comma in
//     an array, ...) appears first, the JSON format error wins; the walk must
//     never skip past bad syntax to hunt for a later duplicate.
//
// Both verdicts precede business checks (missing required fields, wrong tag
// value types, ...). Every failure through the public ParseReleaseInput entry
// point returns a non-nil error and the zero-value ReleasePlanInput.

const orderTestBase = `"app":"a","revision":"r","image":"i","batchSize":1`

// assertDuplicateVerdict fails unless err is exactly the duplicate-member
// verdict for field/path, with no JSON format-error wording mixed in.
func assertDuplicateVerdict(t *testing.T, err error, field, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected duplicate-member error for %q at %s, got nil", field, path)
	}
	var dme *duplicateMemberError
	if !errors.As(err, &dme) {
		t.Fatalf("expected *duplicateMemberError for %q at %s, got %T: %v", field, path, err, err)
	}
	if dme.field != field {
		t.Fatalf("duplicate field = %q, want %q (error: %v)", dme.field, field, err)
	}
	if dme.path != path {
		t.Fatalf("duplicate object path = %q, want %q (error: %v)", dme.path, path, err)
	}
	if strings.Contains(err.Error(), "JSON 格式错误") {
		t.Fatalf("a duplicate already read must win over later syntax, error mentions a format error: %v", err)
	}
}

// assertJSONFormatVerdict fails unless err is the JSON format verdict rather
// than a duplicate-member or business validation error.
func assertJSONFormatVerdict(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected JSON format error, got nil")
	}
	var dme *duplicateMemberError
	if errors.As(err, &dme) {
		t.Fatalf("syntax before the duplicate must report the format error, not the later duplicate: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "JSON 格式错误") {
		t.Fatalf("expected error with JSON 格式错误 prefix, got %v", err)
	}
}

// TestParse_DuplicateReadBeforeSyntaxErrorKeepsDuplicate covers the core
// promise: once the second member name has been read inside an object, that
// duplicate verdict survives every malformation that follows in the file.
// The cases span the cluster tag objects and the nested objects carried by
// unknown extra fields; unknown fields take no part in plan computation, yet
// their duplicates are still whole-document errors.
func TestParse_DuplicateReadBeforeSyntaxErrorKeepsDuplicate(t *testing.T) {
	cases := []struct {
		name      string
		body      string // JSON body without outer braces
		wantField string
		wantPath  string
	}{
		{
			name:      "tags duplicate then duplicate value missing at EOF",
			body:      orderTestBase + `,"clusters":[{"id":"x","tags":{"env":"p","env":`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "tags duplicate then duplicate value missing before close",
			body:      orderTestBase + `,"clusters":[{"id":"x","tags":{"env":"p","env":}}]}`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "tags duplicate then nested objects never close",
			body:      orderTestBase + `,"clusters":[{"id":"x","tags":{"env":"p","env":1}}`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "tags duplicate in a complete document followed by garbage",
			body:      orderTestBase + `,"clusters":[{"id":"x","tags":{"env":1,"env":2}}]} zzz`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "tags duplicate whose own second value is a malformed number",
			body:      orderTestBase + `,"clusters":[{"id":"x","tags":{"env":1,"env":1e}}]}`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "cluster field duplicate then stray comma in same object",
			body:      orderTestBase + `,"clusters":[{"id":"x","disabled":true,"disabled":false},,]}`,
			wantField: "disabled",
			wantPath:  "$.clusters[0]",
		},
		{
			name:      "cluster field duplicate in a complete document followed by garbage",
			body:      orderTestBase + `,"clusters":[{"id":"x","disabled":true,"disabled":false}]} trailing`,
			wantField: "disabled",
			wantPath:  "$.clusters[0]",
		},
		{
			name:      "include condition duplicate then value missing",
			body:      orderTestBase + `,"clusters":[],"include":[{"env":"p","env":}]}`,
			wantField: "env",
			wantPath:  "$.include[0]",
		},
		{
			name:      "unknown nested object duplicate then value missing",
			body:      orderTestBase + `,"clusters":[],"meta":{"x":1,"x":}}`,
			wantField: "x",
			wantPath:  "$.meta",
		},
		{
			name:      "unknown nested object duplicate then object never closes",
			body:      orderTestBase + `,"clusters":[],"meta":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  "$.meta",
		},
		{
			name:      "unknown nested object duplicate then trailing garbage",
			body:      orderTestBase + `,"clusters":[],"meta":{"x":1,"x":2}} extra`,
			wantField: "x",
			wantPath:  "$.meta",
		},
		{
			name:      "unknown array of objects duplicate with real element index, array unclosed",
			body:      orderTestBase + `,"clusters":[],"items":[{"ok":1},{"y":1,"y":2}`,
			wantField: "y",
			wantPath:  "$.items[1]",
		},
		{
			name:      "unknown deeply nested duplicate then trailing garbage",
			body:      orderTestBase + `,"clusters":[],"a":{"b":{"c":[{"z":1,"z":2}]}} garbage`,
			wantField: "z",
			wantPath:  "$.a.b.c[0]",
		},
		{
			name: "single dotted member name stays one bracketed segment with trailing garbage",
			// "meta.info" is ONE top-level member, not $.meta.info; the
			// duplicate inside it must be reported at $["meta.info"].
			body:      orderTestBase + `,"clusters":[],"meta.info":{"x":1,"x":2}} junk`,
			wantField: "x",
			wantPath:  `$["meta.info"]`,
		},
		{
			name: "decode-equal escaped name duplicate then object never closes",
			// "env" literal and env are one decoded name; the verdict
			// names the decoded field and its object even though the document
			// ends unclosed afterwards.
			body:      orderTestBase + `,"clusters":[{"id":"x","tags":{"env":"p","` + jsonUEsc('e') + `nv":1}}`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReleaseInput([]byte("{" + tc.body + "}"))
			assertDuplicateVerdict(t, err, tc.wantField, tc.wantPath)
		})
	}
}

// TestParse_SyntaxErrorReadBeforeDuplicateReportsFormat covers the reverse
// order: unreadable JSON syntax that appears first is the verdict, even when
// a duplicate member name follows later in the same document. The reader
// must not jump over the bad syntax to select the later duplicate.
func TestParse_SyntaxErrorReadBeforeDuplicateReportsFormat(t *testing.T) {
	cases := []struct {
		name string
		body string // JSON body without outer braces
	}{
		{
			name: "malformed number 1e in earlier extra field before duplicate",
			body: orderTestBase + `,"clusters":[],"x":1e,"y":{"a":1,"a":2}}`,
		},
		{
			name: "malformed number 0. before duplicate",
			body: orderTestBase + `,"clusters":[],"x":0.,"y":{"a":1,"a":2}}`,
		},
		{
			name: "malformed number in earlier cluster before duplicate in later cluster",
			body: orderTestBase + `,"clusters":[{"id":"a","w":1e},{"id":"b","tags":{"env":1,"env":2}}]}`,
		},
		{
			name: "extra comma in clusters before duplicate-bearing element",
			body: orderTestBase + `,"clusters":[{"id":"a"},,{"id":"x","tags":{"env":1,"env":2}}]}`,
		},
		{
			name: "extra comma in include array before duplicate-bearing condition",
			body: orderTestBase + `,"clusters":[],"include":[,{"env":1,"env":2}]}`,
		},
		{
			name: "malformed value in the same tags object before the duplicate key",
			body: orderTestBase + `,"clusters":[{"id":"x","tags":{"a":1e,"b":1,"b":2}}]}`,
		},
		{
			name: "extra comma in unknown array before duplicate-bearing element",
			body: orderTestBase + `,"clusters":[],"items":[1,,{"a":1,"a":2}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReleaseInput([]byte("{" + tc.body))
			assertJSONFormatVerdict(t, err)
		})
	}
}

// TestParse_DuplicateAndSyntaxBothBeatBusinessErrors shows that even when a
// business error would also exist (missing required app field, a tag value of
// the wrong type), whichever of the duplicate/format verdicts comes first in
// reading order is reported — never the business error.
func TestParse_DuplicateAndSyntaxBothBeatBusinessErrors(t *testing.T) {
	// Duplicate read first, value missing afterwards, and the required app
	// field absent as well: the duplicate still wins.
	doc := `{"revision":"r","image":"i","batchSize":1,` +
		`"clusters":[{"id":"x","tags":{"env":"p","env":}}]}`
	_, err := ParseReleaseInput([]byte(doc))
	assertDuplicateVerdict(t, err, "env", "$.clusters[0].tags")
	if strings.Contains(err.Error(), "app") {
		t.Fatalf("duplicate must beat the missing-app business error, got %v", err)
	}

	// Same shape with an escape-spelled second name, and the document also
	// unclosed: duplicate still wins over the missing-app error.
	doc = `{"revision":"r","image":"i","batchSize":1,` +
		`"clusters":[{"id":"x","tags":{"env":"p","` + jsonUEsc('e') + `nv":1}}`
	_, err = ParseReleaseInput([]byte(doc))
	assertDuplicateVerdict(t, err, "env", "$.clusters[0].tags")

	// Syntax first, duplicate later, app also missing: the format error wins
	// over both the duplicate and the missing-app business error.
	doc = `{"revision":"r","image":"i","batchSize":1,"clusters":[],` +
		`"x":1e,"y":{"a":1,"a":2}}`
	_, err = ParseReleaseInput([]byte(doc))
	assertJSONFormatVerdict(t, err)

	// Syntax first while a later include condition also has a number value
	// where a string is required: the format error must reach the caller,
	// not the business type error and not the later duplicate.
	doc = `{` + orderTestBase + `,"clusters":[],"x":1e,` +
		`"include":[{"env":1,"env":2}]}`
	_, err = ParseReleaseInput([]byte(doc))
	assertJSONFormatVerdict(t, err)
	if strings.Contains(err.Error(), "必须是字符串") {
		t.Fatalf("JSON syntax must beat the tag-value business error, got %v", err)
	}
}

// TestParse_RepeatedNameInDifferentObjectsWithSyntaxError guards two rules
// together: the same decoded name used once in each of two different objects
// is legal on its own, and a syntax error elsewhere in that document is still
// reported as a format error — the legal repetition must never be mislabeled
// a duplicate.
func TestParse_RepeatedNameInDifferentObjectsWithSyntaxError(t *testing.T) {
	// Two clusters each hold one "env" tag (legal); a malformed number later
	// in the second cluster is the only document problem.
	doc := `{` + orderTestBase + `,"clusters":[` +
		`{"id":"a","tags":{"env":"prod"}},` +
		`{"id":"b","tags":{"env":"dev"},"w":1e}]}`
	_, err := ParseReleaseInput([]byte(doc))
	assertJSONFormatVerdict(t, err)

	// The same arrangement with the syntax error repaired is accepted: the
	// two "env" names belong to different tag objects.
	doc = `{` + orderTestBase + `,"clusters":[` +
		`{"id":"a","tags":{"env":"prod"}},` +
		`{"id":"b","tags":{"env":"dev"},"w":1}]}`
	if _, err := ParseReleaseInput([]byte(doc)); err != nil {
		t.Fatalf("same member name in different objects must be legal: %v", err)
	}
}

// TestParse_DuplicateSyntaxCombosReturnZeroConfig checks the library contract
// for every combined-failure shape: a non-nil error together with the
// zero-value ReleasePlanInput — no app information or candidate clusters read
// before the failure may leak back to the caller.
func TestParse_DuplicateSyntaxCombosReturnZeroConfig(t *testing.T) {
	failures := map[string]string{
		"dup then missing value": `{` + orderTestBase +
			`,"clusters":[{"id":"x","tags":{"env":"p","env":}}]}`,
		"dup then unclosed": `{` + orderTestBase +
			`,"clusters":[{"id":"x","tags":{"env":"p","env":1}}`,
		"dup then trailing garbage": `{` + orderTestBase +
			`,"clusters":[{"id":"x","tags":{"env":1,"env":2}}]} zzz`,
		"dup in unknown object then missing value": `{` + orderTestBase +
			`,"clusters":[],"meta":{"x":1,"x":}}`,
		"escaped dup then unclosed": `{` + orderTestBase +
			`,"clusters":[{"id":"x","tags":{"env":"p","` + jsonUEsc('e') + `nv":1}}`,
		"1e before dup": `{` + orderTestBase +
			`,"clusters":[],"x":1e,"y":{"a":1,"a":2}}`,
		"extra comma before dup": `{` + orderTestBase +
			`,"clusters":[{"id":"a"},,{"id":"x","tags":{"env":1,"env":2}}]}`,
		"1e in earlier cluster before dup": `{` + orderTestBase +
			`,"clusters":[{"id":"a","w":1e},{"id":"b","tags":{"env":1,"env":2}}]}`,
	}
	for name, doc := range failures {
		t.Run(name, func(t *testing.T) {
			in, err := ParseReleaseInput([]byte(doc))
			if err == nil {
				t.Fatal("expected a non-nil error")
			}
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("failure must return the zero-value config, got %+v", in)
			}
		})
	}
}

// TestParse_ValidPlanUnchangedBesideExtraFields is the success-side guard:
// unknown extra fields — including nested objects and arrays, member names
// written with Unicode escapes, and legal JSON numbers beyond float64 range
// such as 1e400 — neither cause a false format/duplicate error nor change the
// computed batches and exclusion reasons relative to the same plan with the
// extras removed.
func TestParse_ValidPlanUnchangedBesideExtraFields(t *testing.T) {
	planDoc := func(withExtras bool) string {
		var extra string
		if withExtras {
			extra = `,"meta":{"limit":1e400,"notes":[1e400,{"deep":[1e400]}],` +
				`"nest":{"` + jsonUEsc('k') + `":"v"}},"items":[{"z":1e400}]`
		}
		return `{
			"app":"payments","revision":"v1","image":"img","batchSize":2,"spreadBy":"zone",
			"include":[{"env":"prod"}],"exclude":[{"quarantine":"true"}],
			"clusters":[
				{"id":"c-c","tags":{"env":"prod","zone":"z2"}},
				{"id":"g3","tags":{"env":"prod","quarantine":"true"}},
				{"id":"c-a","tags":{"env":"prod","zone":"z1"}},
				{"id":"g0","disabled":true},
				{"id":"c-b","tags":{"env":"prod","zone":"z1"}}
			]` + extra + `}`
	}
	base, err := MakeReleasePlan(parsePlan(t, planDoc(false)))
	if err != nil {
		t.Fatal(err)
	}
	withExtra, err := MakeReleasePlan(parsePlan(t, planDoc(true)))
	if err != nil {
		t.Fatalf("legal extra fields, escapes and huge numbers must parse and plan: %v", err)
	}
	if planSummary(withExtra) != planSummary(base) {
		t.Fatalf("plan changed due to ignored extra fields:\n extra: %+v\n base:  %+v", withExtra, base)
	}
	// Pin the actual result: c-a and c-c take batch 1 (z1/z2), c-b (z1) is
	// deferred to batch 2; g0 is disabled and g3 is excluded.
	wantBatches := [][]string{{"c-a", "c-c"}, {"c-b"}}
	if len(withExtra.Batches) != len(wantBatches) {
		t.Fatalf("batches = %+v, want %+v", withExtra.Batches, wantBatches)
	}
	for i, want := range wantBatches {
		if !reflect.DeepEqual(withExtra.Batches[i].Clusters, want) {
			t.Fatalf("batch %d = %v, want %v", i+1, withExtra.Batches[i].Clusters, want)
		}
	}
	wantExcluded := []ExcludedCluster{
		{ID: "g0", Reason: ReasonDisabled},
		{ID: "g3", Reason: ReasonExcludeMatched},
	}
	if !reflect.DeepEqual(withExtra.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", withExtra.Excluded, wantExcluded)
	}
}

// TestPlanCLI_DuplicateBeforeSyntaxFailsCleanly drives the command line with
// a document where a tag-object duplicate is read before the object closes:
// exit non-zero, stdout empty, stderr names the decoded member and the object
// position, and no JSON format-error wording leaks in.
func TestPlanCLI_DuplicateBeforeSyntaxFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app":"payments","revision":"v1","image":"img","batchSize":1,
		"clusters":[{"id":"c1","tags":{"env":"prod","` + jsonUEsc('e') + `nv":"dev"}}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runOrderCLI(t, path)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, `"env"`) || !strings.Contains(stderr, "$.clusters[0].tags") {
		t.Fatalf("stderr should name the duplicate member and object, got %q", stderr)
	}
	if strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("the already-read duplicate must be the reported problem, got %q", stderr)
	}
}

// TestPlanCLI_SyntaxBeforeDuplicateFailsCleanly drives the command line with
// unreadable JSON syntax placed before a later duplicate member: exit
// non-zero, stdout empty, stderr reports the JSON format error and must not
// name the later duplicate.
func TestPlanCLI_SyntaxBeforeDuplicateFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app":"payments","revision":"v1","image":"img","batchSize":1,
		"clusters":[],
		"x":1e,
		"y":{"a":1,"a":2}
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runOrderCLI(t, path)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("stderr should report the JSON format error, got %q", stderr)
	}
	if strings.Contains(stderr, "重复成员") {
		t.Fatalf("the reader must not skip bad syntax to report a later duplicate, got %q", stderr)
	}
}

// runOrderCLI runs the plan command against path and returns its exit code,
// stdout and stderr. It mirrors the shared runCLI helper used by the other
// CLI tests but is local to this file to keep the regression net
// self-contained.
func runOrderCLI(t *testing.T, path string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("go", "run", "./cmd/darksafe", "plan", path)
	cmd.Dir = ".."
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("failed to run command: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}
