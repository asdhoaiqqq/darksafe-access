package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// This file pins the nesting-depth limit of a release configuration
// document: the whole JSON document may nest objects and arrays at most
// maxJSONDepth (10000) levels deep, the outermost object being level 1 and
// every object or array entered adding one level. The rule covers known and
// unknown fields alike — including extra content that never takes part in
// planning — so a hostile or corrupt document cannot exhaust the reading
// process's stack and memory before it is rejected. Depth exactly at the
// limit stays legal, brackets inside strings are text rather than structure,
// and members or items at the same level never add up.

// depthBase is a minimal valid configuration body (without the outer
// braces); tests append one extra field carrying the nesting under test.
const depthBase = `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"c-a"}]`

// nestedArrays returns one JSON value nested inside n arrays: n=1 is "[7]".
func nestedArrays(n int) string {
	return strings.Repeat("[", n) + "7" + strings.Repeat("]", n)
}

// parseDepthErr parses data and fails the test unless it is rejected with
// the nesting-depth error and the zero configuration.
func parseDepthErr(t *testing.T, data string) error {
	t.Helper()
	in, err := ParseReleaseInput([]byte(data))
	if err == nil {
		t.Fatalf("expected nesting-depth rejection, got success (input prefix %.80q...)", data)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("depth rejection must yield the zero config, got %+v", in)
	}
	if !strings.Contains(err.Error(), "JSON 嵌套深度超限") {
		t.Fatalf("error must report the nesting-depth excess, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "10000") {
		t.Fatalf("error must state the allowed limit 10000, got %q", err.Error())
	}
	return err
}

// TestParseDepth_ExactlyAtLimitParses: a document whose deepest value sits
// exactly at level 10000 — the root object plus 9999 nested arrays inside an
// unknown extra field — is legal and plans normally.
func TestParseDepth_ExactlyAtLimitParses(t *testing.T) {
	doc := "{" + depthBase + `,"extra":` + nestedArrays(maxJSONDepth-1) + "}"
	in, err := ParseReleaseInput([]byte(doc))
	if err != nil {
		t.Fatalf("depth exactly %d must parse, got %v", maxJSONDepth, err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("planning must succeed, got %v", err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c-a" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

// TestParseDepth_OneBeyondLimitRejected: one more array level — level 10001
// — turns the same document into a configuration error.
func TestParseDepth_OneBeyondLimitRejected(t *testing.T) {
	doc := "{" + depthBase + `,"extra":` + nestedArrays(maxJSONDepth) + "}"
	parseDepthErr(t, doc)
}

// TestParseDepth_MixedNestingCountsTheSame: objects and arrays share one
// depth counter. The root object (1) plus 4999 object+array pairs (9998)
// plus one more array is exactly level 10000 and parses; one further array
// is level 10001 and is rejected.
func TestParseDepth_MixedNestingCountsTheSame(t *testing.T) {
	pair := `{"a":[`
	pairs := strings.Repeat(pair, (maxJSONDepth-2)/2) // 4999 pairs, 9998 levels
	closePairs := strings.Repeat("]}", (maxJSONDepth-2)/2)
	atLimit := "{" + depthBase + `,"extra":` + pairs + "[7]" + closePairs + "}"
	if _, err := ParseReleaseInput([]byte(atLimit)); err != nil {
		t.Fatalf("mixed nesting at exactly %d must parse, got %v", maxJSONDepth, err)
	}

	over := "{" + depthBase + `,"extra":` + pairs + "[[7]]" + closePairs + "}"
	parseDepthErr(t, over)
}

// TestParseDepth_SameLevelDoesNotAccumulate: thousands of sibling members
// and array items stay at their own level; only descent counts.
func TestParseDepth_SameLevelDoesNotAccumulate(t *testing.T) {
	items := make([]string, 0, maxJSONDepth*2)
	for i := 0; i < maxJSONDepth*2; i++ {
		items = append(items, `{"a":[]}`)
	}
	doc := "{" + depthBase + `,"extra":[` + strings.Join(items, ",") + "]}"
	if _, err := ParseReleaseInput([]byte(doc)); err != nil {
		t.Fatalf("siblings must not accumulate depth, got %v", err)
	}
}

// TestParseDepth_BracketsInStringsAreNotStructure: a string may contain any
// number of bracket characters, escaped or literal, without adding depth.
func TestParseDepth_BracketsInStringsAreNotStructure(t *testing.T) {
	brackets := strings.Repeat("[{", maxJSONDepth*2)
	escaped := strings.Repeat(`[{`, maxJSONDepth) // "[" and "{" by escape
	doc := "{" + depthBase + `,"extra":"` + brackets + `","note":"` + brackets + escaped + `[}"}`
	if _, err := ParseReleaseInput([]byte(doc)); err != nil {
		t.Fatalf("brackets inside strings must not count as nesting, got %v", err)
	}
}

// TestParseDepth_AppliesToUnknownAndDisabledFields: the limit cannot be
// dodged by hiding the deep content in an unknown field, in an unknown field
// of a disabled candidate, or in a candidate the include/exclude rules would
// filter out.
func TestParseDepth_AppliesToUnknownAndDisabledFields(t *testing.T) {
	deep := nestedArrays(maxJSONDepth) // one level beyond the limit at field depth 2
	cases := map[string]string{
		"unknown top-level field": "{" + depthBase + `,"meta":` + deep + "}",
		"disabled candidate": "{" + depthBase[:len(depthBase)-1] + `,{"id":"off","disabled":true,"meta":` + deep + "}]}",
		"filtered-out candidate": `"app":"a","revision":"r","image":"i","batchSize":1,` +
			`"exclude":[{"env":"temp"}],` +
			`"clusters":[{"id":"c-a"},{"id":"x","tags":{"env":"temp"},"meta":` + deep + "}]}",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.HasPrefix(doc, "{") {
				doc = "{" + doc
			}
			parseDepthErr(t, doc)
		})
	}
}

// TestParseDepth_UnclosedDeepNestingStillFails: the depth verdict is reached
// while descending, so a document whose over-deep part is never closed —
// and which therefore is not even well-formed JSON — still fails with the
// depth error rather than an unbounded read or a bare format error.
func TestParseDepth_UnclosedDeepNestingStillFails(t *testing.T) {
	doc := "{" + depthBase + `,"extra":` + strings.Repeat("[", maxJSONDepth)
	parseDepthErr(t, doc)
}

// TestParseDepth_MessageDoesNotGrowWithDepth: however far beyond the limit
// the input goes, the error is the same fixed message naming the limit.
func TestParseDepth_MessageDoesNotGrowWithDepth(t *testing.T) {
	just := parseDepthErr(t, "{"+depthBase+`,"extra":`+nestedArrays(maxJSONDepth)+"}")
	deeper := parseDepthErr(t, "{"+depthBase+`,"extra":`+nestedArrays(maxJSONDepth*5)+"}")
	if just.Error() != deeper.Error() {
		t.Fatalf("depth error must not grow with the input depth:\n%q\nvs\n%q", just.Error(), deeper.Error())
	}
}

// TestParseDepth_UnderLimitPrioritiesUnchanged: below the limit the existing
// checks keep their verdicts and their order — a corrupt string is still the
// text error, a repeated member is still the duplicate error, and a legal
// deep extra field does not change the plan.
func TestParseDepth_UnderLimitPrioritiesUnchanged(t *testing.T) {
	deepOK := nestedArrays(maxJSONDepth - 1)

	t.Run("text error still wins", func(t *testing.T) {
		doc := "{" + depthBase + `,"meta":"` + badUTF8 + `","extra":` + deepOK + "}"
		err := parseTextErr(t, doc)
		if !strings.Contains(err.Error(), "无效 UTF-8") {
			t.Fatalf("expected the invalid UTF-8 verdict, got %q", err.Error())
		}
	})

	t.Run("duplicate still reported", func(t *testing.T) {
		doc := "{" + depthBase + `,"extra":` + deepOK + `,"meta":{"a":1,"a":2}}`
		in, err := ParseReleaseInput([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), "重复成员") {
			t.Fatalf("expected the duplicate-member verdict, got err=%v in=%+v", err, in)
		}
	})

	t.Run("legal deep extra field does not change the plan", func(t *testing.T) {
		plain, err := ParseReleaseInput([]byte("{" + depthBase + "}"))
		if err != nil {
			t.Fatal(err)
		}
		withExtra, err := ParseReleaseInput([]byte("{" + depthBase + `,"extra":` + deepOK + "}"))
		if err != nil {
			t.Fatal(err)
		}
		planPlain, err := MakeReleasePlan(plain)
		if err != nil {
			t.Fatal(err)
		}
		planExtra, err := MakeReleasePlan(withExtra)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(planPlain, planExtra) {
			t.Fatalf("a legal deep extra field must not change the plan:\n%+v\nvs\n%+v", planPlain, planExtra)
		}
	})
}

// TestParseDepth_CallerCanContinueAfterRejection: a rejected over-deep
// document returns the zero config and a non-nil error, and the same caller
// can go on to parse other configurations.
func TestParseDepth_CallerCanContinueAfterRejection(t *testing.T) {
	parseDepthErr(t, "{"+depthBase+`,"extra":`+nestedArrays(maxJSONDepth)+"}")
	in, err := ParseReleaseInput([]byte("{" + depthBase + "}"))
	if err != nil {
		t.Fatalf("parsing must keep working after a depth rejection, got %v", err)
	}
	if len(in.Clusters) != 1 || in.Clusters[0].ID != "c-a" {
		t.Fatalf("unexpected config after recovery: %+v", in)
	}
}

// TestPlanCLI_OverDeepConfigFailsCleanly: the plan command reports the depth
// problem on stderr, writes nothing to stdout, and exits with code 1 — no
// batch is ever produced for the rejected document.
func TestPlanCLI_OverDeepConfigFailsCleanly(t *testing.T) {
	doc := "{" + depthBase + `,"extra":` + nestedArrays(maxJSONDepth) + "}"
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty when the config is rejected, got %q", stdout)
	}
	if !strings.Contains(stderr, "JSON 嵌套深度超限") || !strings.Contains(stderr, "10000") {
		t.Fatalf("stderr must explain the depth limit, got %q", stderr)
	}
}
