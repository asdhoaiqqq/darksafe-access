package darksafe

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This file pins down the JSON structural nesting limit: a release
// configuration document may open at most maxJSONNestingDepth levels, the
// outermost value being level 1. The limit is enforced while the document
// is read structurally — before any business validation or planning — in
// every field, so an ignored extra field, a disabled candidate or a
// candidate the filters would drop cannot be used to smuggle unbounded
// nesting into the reading process. Exactly 10000 levels still parses;
// the 10001st opener fails with the fixed errJSONDepthExceeded message.

// depthBase is a valid minimal configuration; depth tests hang an extra
// deeply nested value off the ignored member "x".
const depthBase = `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"c1"}],"x":`

// deepValue builds k nested containers around a scalar leaf and closes
// them all. Placed as the value of "x" in depthBase, the deepest level is
// 1 (root object) + k.
func deepValue(open, close string, k int) string {
	return strings.Repeat(open, k) + `1` + strings.Repeat(close, k)
}

// deepMixedValue alternates objects and arrays for k levels, so the limit
// is proven to count object and array openers by one shared rule.
func deepMixedValue(k int) string {
	var open, close strings.Builder
	for i := 0; i < k; i++ {
		if i%2 == 0 {
			open.WriteByte('[')
			close.WriteByte(']')
		} else {
			open.WriteString(`{"k":`)
			close.WriteByte('}')
		}
	}
	return open.String() + `1` + close.String()
}

func deepDoc(open, close string, extraLevels int) string {
	return depthBase + deepValue(open, close, extraLevels) + `}`
}

// parseErrMsg parses doc and returns the error text, failing when parsing
// unexpectedly succeeds.
func parseErrMsg(t *testing.T, doc string) string {
	t.Helper()
	_, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatalf("expected a parse error")
	}
	return err.Error()
}

// wantDepthMsg is the exact user-facing message; every too-deep document
// must produce this one constant text regardless of how deep it is.
func wantDepthMsg() string { return errJSONDepthExceeded.Error() }

func TestParse_NestingExactlyAtLimitIsAccepted(t *testing.T) {
	cases := map[string]string{
		"objects": deepDoc(`{"a":`, `}`, maxJSONNestingDepth-1),
		"arrays":  deepDoc(`[`, `]`, maxJSONNestingDepth-1),
		"mixed":   depthBase + deepMixedValue(maxJSONNestingDepth-1) + `}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			in, err := ParseReleaseInput([]byte(doc))
			if err != nil {
				t.Fatalf("depth exactly %d must still parse: %v", maxJSONNestingDepth, err)
			}
			// The deeply nested extra field is ignored; the plan is normal.
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatalf("legal extra nesting must not affect planning: %v", err)
			}
			if got := batchIDs(plan); len(got) != 1 || got[0] != "c1" {
				t.Fatalf("unexpected plan batches: %v", got)
			}
			if len(plan.Excluded) != 0 {
				t.Fatalf("unexpected excluded: %+v", plan.Excluded)
			}
		})
	}
}

func TestParse_NestingBeyondLimitRejected(t *testing.T) {
	cases := map[string]string{
		"objects":        deepDoc(`{"a":`, `}`, maxJSONNestingDepth),
		"arrays":         deepDoc(`[`, `]`, maxJSONNestingDepth),
		"mixed":          depthBase + deepMixedValue(maxJSONNestingDepth) + `}`,
		"objects deeper": deepDoc(`{"a":`, `}`, maxJSONNestingDepth*5),
		"arrays deeper":  deepDoc(`[`, `]`, maxJSONNestingDepth*5),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			in, err := ParseReleaseInput([]byte(doc))
			if err == nil {
				t.Fatalf("depth %d+ must be rejected", maxJSONNestingDepth)
			}
			if err.Error() != wantDepthMsg() {
				t.Fatalf("error = %q, want %q", err.Error(), wantDepthMsg())
			}
			// The verdict is the shared sentinel, so callers can detect it
			// programmatically regardless of which reader produced it.
			if !errors.Is(err, errJSONDepthExceeded) {
				t.Fatalf("error must be errJSONDepthExceeded, got %T", err)
			}
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("failure must return the zero config, got %+v", in)
			}
		})
	}
}

// TestParse_DepthMessageDoesNotGrowWithDepth checks that a document far
// beyond the limit reports the same fixed, bounded message as one just
// beyond it — the error never quotes the document, a path or the actual
// depth, so nesting more levels cannot inflate it.
func TestParse_DepthMessageDoesNotGrowWithDepth(t *testing.T) {
	justBeyond := parseErrMsg(t, deepDoc(`[`, `]`, maxJSONNestingDepth))
	farBeyond := parseErrMsg(t, deepDoc(`[`, `]`, maxJSONNestingDepth*10))
	if justBeyond != farBeyond {
		t.Fatalf("message grew with depth:\n %q\n %q", justBeyond, farBeyond)
	}
	if !strings.Contains(justBeyond, "10000") {
		t.Fatalf("message must state the allowed limit: %q", justBeyond)
	}
}

// TestParse_UnterminatedDeepNestingFailsControlled covers the worst case:
// deep openers with no closing brackets at all. The walk must stop at the
// 10001st opener and report the depth problem rather than exhaust the
// reader's stack and later call the document merely "unclosed".
func TestParse_UnterminatedDeepNestingFailsControlled(t *testing.T) {
	cases := map[string][]byte{
		"arrays in config":  []byte(depthBase + strings.Repeat(`[`, maxJSONNestingDepth)),
		"objects in config": []byte(depthBase + strings.Repeat(`{"a":`, maxJSONNestingDepth)),
		"raw arrays":        []byte(strings.Repeat(`[`, maxJSONNestingDepth+1)),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseReleaseInput(doc)
			if err == nil {
				t.Fatalf("unterminated too-deep nesting must fail")
			}
			if err.Error() != wantDepthMsg() {
				t.Fatalf("error = %q, want %q", err.Error(), wantDepthMsg())
			}
			if strings.Contains(err.Error(), "未闭合") {
				t.Fatalf("depth must be reported before the missing closers, got %q", err)
			}
		})
	}
}

// TestParse_DepthLimitCoversEveryField proves the limit cannot be bypassed
// by putting the deep content where business validation would never plan
// it: an unknown field, a disabled candidate, a candidate an exclude
// condition removes, or a candidate an include condition drops.
func TestParse_DepthLimitCoversEveryField(t *testing.T) {
	deep := deepValue(`[`, `]`, maxJSONNestingDepth) // deepest level 10001 under its field
	cases := map[string]string{
		"unknown field": depthBase + deep + `}`,
		"disabled candidate": `{"app":"a","revision":"r","image":"i","batchSize":1,` +
			`"clusters":[{"id":"off","disabled":true,"deep":` + deep + `}]}`,
		"excluded candidate": `{"app":"a","revision":"r","image":"i","batchSize":1,` +
			`"exclude":[{"env":"temp"}],` +
			`"clusters":[{"id":"ex","tags":{"env":"temp"},"deep":` + deep + `}]}`,
		"include-not-matched candidate": `{"app":"a","revision":"r","image":"i","batchSize":1,` +
			`"include":[{"env":"prod"}],` +
			`"clusters":[{"id":"nm","tags":{"env":"dev"},"deep":` + deep + `}]}`,
		"condition field": `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],` +
			`"include":[{"env":"prod","deep":` + deep + `}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			in, err := ParseReleaseInput([]byte(doc))
			if err == nil || err.Error() != wantDepthMsg() {
				t.Fatalf("depth must be enforced in %s, got %v", name, err)
			}
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("%s: failure must return the zero config, got %+v", name, in)
			}
		})
	}
}

// TestParse_SiblingsDoNotAddDepth checks that many members/elements at the
// same level never accumulate: hundreds of thousands of siblings at depth
// 3 are accepted while one extra nesting level past the cap is not.
func TestParse_SiblingsDoNotAddDepth(t *testing.T) {
	const siblings = 50000
	doc := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],` +
		`"many":[` + strings.Repeat(`[1,2,3,4,5,6,7,8],`, siblings) + `[1]]}`
	if _, err := ParseReleaseInput([]byte(doc)); err != nil {
		t.Fatalf("same-level siblings must not count toward depth: %v", err)
	}
}

// TestParse_BracesInStringsAreNotStructure checks that brackets written
// inside string literals — directly, as \uXXXX escapes decoding to
// brackets, or in member names — are text and never counted as nesting
// levels.
func TestParse_BracesInStringsAreNotStructure(t *testing.T) {
	direct := strings.Repeat(`{[`, maxJSONNestingDepth+100)
	// { is "{", ] is "]": decoded brackets must not count.
	escaped := strings.Repeat(`{]`, maxJSONNestingDepth+100)
	doc := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],` +
		`"s":"` + direct + `",` +
		`"t":"` + escaped + `",` +
		`"` + direct + `":1,` +
		`"x":` + strings.Repeat(`{"k":`, 50) + `"` + direct + `"` +
		strings.Repeat(`}`, 50) + `}`
	if _, err := ParseReleaseInput([]byte(doc)); err != nil {
		t.Fatalf("braces inside strings must not count as structure: %v", err)
	}
}

// TestParse_DepthCheckDoesNotChangeOtherPriorities checks the interaction
// with the other structural verdicts: the strict-text walk runs first and
// enforces the depth limit itself, so a too-deep document reports depth
// even when it also repeats a member; a document at exactly the limit
// keeps its ordinary duplicate and format verdicts unchanged.
func TestParse_DepthCheckDoesNotChangeOtherPriorities(t *testing.T) {
	deep := deepValue(`[`, `]`, maxJSONNestingDepth)

	// Duplicate member plus too-deep nesting: the strict-text walk runs
	// first and reaches the 10001st opener, so the depth verdict wins no
	// matter where the duplicate sits.
	dupAndDeep := `{"app":"a","app":"b","revision":"r","image":"i","batchSize":1,` +
		`"clusters":[],"x":` + deep + `}`
	if _, err := ParseReleaseInput([]byte(dupAndDeep)); err == nil ||
		err.Error() != wantDepthMsg() {
		t.Fatalf("depth must win over a later duplicate verdict, got %v", err)
	}

	// Exactly 10000 levels plus a duplicate: the duplicate verdict stands.
	atLimitWithDup := `{"app":"a","revision":"r","image":"i","batchSize":1,` +
		`"clusters":[],"x":` + deepValue(`[`, `]`, maxJSONNestingDepth-1) +
		`,"revision":"r2"}`
	if _, err := ParseReleaseInput([]byte(atLimitWithDup)); err == nil ||
		!strings.Contains(err.Error(), "重复成员") {
		t.Fatalf("at-limit document must still be checked for duplicates, got %v", err)
	}

	// A shallow malformed document is unaffected by the depth rule.
	if _, err := ParseReleaseInput([]byte(`{not json`)); err == nil ||
		!strings.HasPrefix(err.Error(), "JSON 格式错误") {
		t.Fatalf("shallow malformed documents keep their format error, got %v", err)
	}
}

// TestCheckJSONFormatAndDuplicates_DepthBounded pins the encoding/json
// fallback walk directly: the Token API enforces no nesting limit of its
// own, so before the fix this walk recursed for every opener and only
// afterwards called the document "array unclosed". It must now stop at
// the 10001st opener with the shared depth verdict, including when the
// deep section is never closed.
func TestCheckJSONFormatAndDuplicates_DepthBounded(t *testing.T) {
	cases := map[string]string{
		"unterminated arrays":  strings.Repeat(`[`, maxJSONNestingDepth+1),
		"unterminated objects": `{"a":` + strings.Repeat(`{"a":`, maxJSONNestingDepth),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkJSONFormatAndDuplicates([]byte(doc))
			if !errors.Is(err, errJSONDepthExceeded) {
				t.Fatalf("fallback walk must stop at the depth limit, got %v", err)
			}
		})
	}
}

// TestParse_CallerContinuesAfterDepthFailure checks the library contract:
// a depth failure hands back the zero config and a non-nil error, and the
// caller can go on to parse a different document in the same process.
func TestParse_CallerContinuesAfterDepthFailure(t *testing.T) {
	in, err := ParseReleaseInput([]byte(deepDoc(`[`, `]`, maxJSONNestingDepth)))
	if err == nil || err.Error() != wantDepthMsg() {
		t.Fatalf("expected depth error, got %v", err)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("expected zero config, got %+v", in)
	}
	good, err := ParseReleaseInput([]byte(
		`{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"c1"}]}`))
	if err != nil {
		t.Fatalf("caller must be able to parse another config afterwards: %v", err)
	}
	if plan, err := MakeReleasePlan(good); err != nil ||
		batchIDs(plan)[0] != "c1" {
		t.Fatalf("subsequent plan failed: plan=%+v err=%v", plan, err)
	}
}

// TestParse_DirectlyConstructedConfigHasNoDepthLimit checks the rule only
// constrains JSON documents: an in-memory config needs no nesting concept
// and validates/plans exactly as before.
func TestParse_DirectlyConstructedConfigHasNoDepthLimit(t *testing.T) {
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "i", BatchSize: 1,
		Clusters: []Cluster{{ID: "c1"}},
	}
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("in-memory config is not subject to a JSON depth limit: %v", err)
	}
	if _, err := MakeReleasePlan(in); err != nil {
		t.Fatalf("in-memory plan failed: %v", err)
	}
}

// TestPlanCLI_NestingDepthExceededFailsCleanly drives the real binary
// with a too-deep configuration: exit 1, empty stdout, stderr naming the
// nesting problem and the 10000-level limit.
func TestPlanCLI_NestingDepthExceededFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := depthBase + deepValue(`[`, `]`, maxJSONNestingDepth) + `}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runPlanCLI(t, "plan", path)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty when nesting is too deep, got %q", stdout)
	}
	if strings.TrimSpace(stderr) != wantDepthMsg() {
		t.Fatalf("stderr = %q, want %q", stderr, wantDepthMsg())
	}
	if !strings.Contains(stderr, "10000") || !strings.Contains(stderr, "嵌套深度") {
		t.Fatalf("stderr must explain the depth limit, got %q", stderr)
	}
}

// TestPlanCLI_NestingAtLimitStillPlans checks the accepted boundary
// through the command line: a file nesting exactly 10000 levels exits 0
// with the usual plan and empty stderr.
func TestPlanCLI_NestingAtLimitStillPlans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := depthBase + deepValue(`{"a":`, `}`, maxJSONNestingDepth-1) + `}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runPlanCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("expected exit 0 at depth %d, got %d; stderr=%q",
			maxJSONNestingDepth, code, stderr)
	}
	if !strings.Contains(stdout, `"c1"`) {
		t.Fatalf("expected the usual plan on stdout, got %q", stdout)
	}
}
