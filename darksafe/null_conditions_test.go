package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// This file covers the include/exclude null contract: an explicit JSON null
// for either field is a field type error, not "no conditions". Omitting the
// field or writing [] still means no conditions, a null inside an unknown
// extra field stays legal, and a nil slice in a directly constructed Go
// config still means no conditions.

// nullCondBase is a complete, otherwise-valid release document; the %s is
// replaced by the include/exclude member under test.
const nullCondBase = `{
	"app": "payments",
	"revision": "2026.10.0-r3",
	"image": "registry.example.net/payments:2026.10.0-r3",
	"batchSize": 5,
	"clusters": [
		{"id": "a", "tags": {"env": "prod"}},
		{"id": "b", "tags": {"env": "dev"}}
	],
	%s
}`

// TestParse_NullIncludeRejected pins the main vulnerability: with every other
// field legal, "include": null must not be read as "match everything" — the
// parse fails naming the field, and no plan containing a and b can be built.
func TestParse_NullIncludeRejected(t *testing.T) {
	doc := strings.Replace(nullCondBase, "%s", `"include": null`, 1)
	in, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatal("expected error for explicit null include")
	}
	if !strings.Contains(err.Error(), `"include"`) || !strings.Contains(err.Error(), "必须是条件数组") {
		t.Fatalf("error must name the field and the expected type, got %v", err)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("a rejected document must return the zero input, got %+v", in)
	}
}

// TestParse_NullExcludeRejected covers the symmetric case: "exclude": null
// fails even though the include conditions alone would select legal clusters.
func TestParse_NullExcludeRejected(t *testing.T) {
	doc := strings.Replace(nullCondBase, "%s", `"include": [{"env": "prod"}], "exclude": null`, 1)
	in, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatal("expected error for explicit null exclude")
	}
	if !strings.Contains(err.Error(), `"exclude"`) || !strings.Contains(err.Error(), "必须是条件数组") {
		t.Fatalf("error must name the field and the expected type, got %v", err)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("a rejected document must return the zero input, got %+v", in)
	}
}

// TestParse_OmittedAndEmptyConditionsStillMeanNone protects the existing
// meaning of omitting the fields or writing empty arrays: all non-disabled,
// non-excluded candidates take part in batching.
func TestParse_OmittedAndEmptyConditionsStillMeanNone(t *testing.T) {
	for name, member := range map[string]string{
		"both omitted": ``,
		"both empty":   `"include": [], "exclude": []`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := nullCondBase
			if member == "" {
				doc = strings.Replace(doc, ",\n\t%s\n}", "\n}", 1)
			} else {
				doc = strings.Replace(doc, "%s", member, 1)
			}
			in, err := ParseReleaseInput([]byte(doc))
			if err != nil {
				t.Fatalf("ParseReleaseInput: %v", err)
			}
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatalf("MakeReleasePlan: %v", err)
			}
			if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "a,b" {
				t.Fatalf("expected one batch [a b], got %+v", plan.Batches)
			}
		})
	}
}

// TestParse_NullInUnknownFieldStaysLegal ensures the tightening only applies
// to the top-level include/exclude fields: a null inside an unknown extra
// field is not the parser's business.
func TestParse_NullInUnknownFieldStaysLegal(t *testing.T) {
	doc := strings.Replace(nullCondBase, "%s", `"meta": {"note": null, "items": [null]}`, 1)
	if _, err := ParseReleaseInput([]byte(doc)); err != nil {
		t.Fatalf("null in an unknown extra field must stay legal, got %v", err)
	}
}

// TestValidate_NilConditionSlicesMeanNone pins the Go-construction path:
// nil Include/Exclude slices are "no conditions", exactly like empty slices.
func TestValidate_NilConditionSlicesMeanNone(t *testing.T) {
	in := validInput()
	in.Include = nil
	in.Exclude = nil
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("nil condition slices must stay legal, got %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("MakeReleasePlan: %v", err)
	}
	if len(plan.Excluded) != 0 {
		t.Fatalf("nil conditions must select every candidate, got excluded %v", plan.Excluded)
	}
}

// TestPlanCLI_NullConditionFailsCleanly drives the failure through
// `darksafe plan <file>` exactly as a user hits it: exit code 1, a completely
// empty stdout (no partial plan) and a stderr naming the field type problem.
func TestPlanCLI_NullConditionFailsCleanly(t *testing.T) {
	for name, member := range map[string]string{
		"include null": `"include": null`,
		"exclude null": `"include": [{"env": "prod"}], "exclude": null`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(nullCondBase, "%s", member, 1)
			code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
			if code != 1 {
				t.Fatalf("expected exit code 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be completely empty on a config error, got %q", stdout)
			}
			field := `"include"`
			if strings.HasPrefix(name, "exclude") {
				field = `"exclude"`
			}
			if !strings.Contains(stderr, field) || !strings.Contains(stderr, "必须是条件数组") {
				t.Fatalf("stderr must name the field and the expected type, got %q", stderr)
			}
		})
	}
}
