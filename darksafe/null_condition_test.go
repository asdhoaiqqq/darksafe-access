package darksafe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file pins the type contract of the top-level include/exclude fields:
// an absent field or an explicit empty array means "no conditions", but an
// explicit JSON null is a field type error — it must never be read as "no
// conditions", which would silently drop the filter the author wrote. The Go
// library path is unaffected: nil slices in a directly constructed config
// still mean "no conditions", exactly like empty slices.

// nullIncludeDoc is an otherwise fully legal release document whose only
// defect is "include": null. Candidate a matches env=prod, candidate b
// matches env=dev; if the null were silently dropped, both would be planned.
const nullIncludeDoc = `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 5,
  "include": null,
  "clusters": [
    {"id": "a", "tags": {"env": "prod"}},
    {"id": "b", "tags": {"env": "dev"}}
  ]
}`

// TestParse_NullIncludeRejected verifies that an explicit JSON null for
// include fails as a field type error: the error names the field and says it
// must be a condition array, and the caller gets the zero input back — never
// a config with the filter quietly removed.
func TestParse_NullIncludeRejected(t *testing.T) {
	in, err := ParseReleaseInput([]byte(nullIncludeDoc))
	if err == nil {
		t.Fatal("expected include:null to be rejected")
	}
	if !strings.Contains(err.Error(), `"include"`) {
		t.Fatalf("error must name the field, got %q", err)
	}
	if !strings.Contains(err.Error(), "条件数组") {
		t.Fatalf("error must say the field must be a condition array, got %q", err)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("failed parse must return the zero input, got %+v", in)
	}
}

// TestParse_NullExcludeRejectedEvenWithValidInclude verifies that exclude:null
// fails even when include already selects legal clusters: the include result
// must not let the exclude error be ignored.
func TestParse_NullExcludeRejectedEvenWithValidInclude(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 5,
  "include": [{"env": "prod"}],
  "exclude": null,
  "clusters": [
    {"id": "a", "tags": {"env": "prod"}},
    {"id": "b", "tags": {"env": "dev"}}
  ]
}`
	in, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatal("expected exclude:null to be rejected")
	}
	if !strings.Contains(err.Error(), `"exclude"`) || !strings.Contains(err.Error(), "条件数组") {
		t.Fatalf("error must name exclude and require a condition array, got %q", err)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("failed parse must return the zero input, got %+v", in)
	}
}

// TestParse_NullInUnknownFieldStillAllowed confirms the tightening is scoped
// to the two known top-level fields: null inside unknown extra fields — even
// ones nested next to condition-shaped data — stays legal.
func TestParse_NullInUnknownFieldStillAllowed(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2,
  "clusters": [{"id": "a"}],
  "meta": {"note": null, "nested": [{"include": null}]}
}`
	if _, err := ParseReleaseInput([]byte(doc)); err != nil {
		t.Fatalf("null in unknown fields must stay legal, got %v", err)
	}
}

// TestParse_AbsentAndEmptyConditionsUnchanged pins the existing meanings that
// must not change: omitting include/exclude or writing [] lets every
// non-disabled, non-excluded candidate into batching.
func TestParse_AbsentAndEmptyConditionsUnchanged(t *testing.T) {
	docs := map[string]string{
		"absent": `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x"},{"id":"y"}]}`,
		"empty":  `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x"},{"id":"y"}],"include":[],"exclude":[]}`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			in, err := ParseReleaseInput([]byte(doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(in.Include) != 0 || len(in.Exclude) != 0 {
				t.Fatalf("expected no conditions, got include=%v exclude=%v", in.Include, in.Exclude)
			}
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			got := 0
			for _, b := range plan.Batches {
				got += len(b.Clusters)
			}
			if got != 2 {
				t.Fatalf("both candidates must be planned, got batches %+v", plan.Batches)
			}
		})
	}
}

// TestParse_DuplicateMemberStillBeatsNullCondition protects the public error
// priority: the duplicate-member check runs before business field validation,
// so a document with both defects reports the duplicate, not the null field.
func TestParse_DuplicateMemberStillBeatsNullCondition(t *testing.T) {
	doc := `{"app":"a","app":"b","revision":"r","image":"i","batchSize":1,"clusters":[],"include":null}`
	_, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatal("expected an error")
	}
	var dup *duplicateMemberError
	if !errors.As(err, &dup) {
		t.Fatalf("duplicate-member check must win over field validation, got %q", err)
	}
}

// TestValidate_NilConditionSlicesMeanNoConditions verifies the Go library
// path is unchanged by the JSON null rejection: nil Include/Exclude slices in
// a directly constructed config are valid and behave exactly like empty
// slices — every non-disabled candidate is planned.
func TestValidate_NilConditionSlicesMeanNoConditions(t *testing.T) {
	in := validInput()
	in.Include = nil
	in.Exclude = nil
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("nil condition slices must stay valid, got %v", err)
	}
	planNil, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("plan with nil slices: %v", err)
	}
	in.Include = []LabelCondition{}
	in.Exclude = []LabelCondition{}
	planEmpty, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("plan with empty slices: %v", err)
	}
	if !reflect.DeepEqual(planNil, planEmpty) {
		t.Fatalf("nil and empty condition slices must plan identically:\n%+v\n----\n%+v", planNil, planEmpty)
	}
}

// TestPlanCLI_NullConditionFailsCleanly drives the CLI contract: a config
// with include:null exits 1, writes nothing to stdout (no partial plan), and
// explains the field type problem on stderr.
func TestPlanCLI_NullConditionFailsCleanly(t *testing.T) {
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, nullIncludeDoc))
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, `"include"`) || !strings.Contains(stderr, "条件数组") {
		t.Fatalf("stderr must name the field and the condition-array requirement, got %q", stderr)
	}
}
