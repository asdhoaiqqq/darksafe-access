package darksafe

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// This file is the regression net for the documented failure-return contract
// of the in-memory library API: whenever MakeReleasePlan returns a non-nil
// error, the returned ReleasePlan must be the zero value — no partial app
// info, batches or excluded list may leak to the caller. Every test drives
// MakeReleasePlan with a directly constructed ReleasePlanInput (the Go-library
// usage documented in README, without JSON or the CLI) and asserts only what
// a caller can observe: the plan, the error, and the untouched input.

// assertZeroReleasePlan fails unless plan is the zero value, including nil
// (not merely empty) batches and excluded slices.
func assertZeroReleasePlan(t *testing.T, plan ReleasePlan) {
	t.Helper()
	if !reflect.DeepEqual(plan, ReleasePlan{}) {
		t.Fatalf("a failed call must return the zero-value ReleasePlan, got %+v", plan)
	}
}

// clusterIDOrder returns the candidates' IDs in slice order, so a test can
// prove a failed call never reordered the caller's own candidate list.
func clusterIDOrder(clusters []Cluster) []string {
	ids := make([]string, len(clusters))
	for i, c := range clusters {
		ids[i] = c.ID
	}
	return ids
}

// pickClusters rebuilds catalog clusters in the requested order, giving every
// call fresh tag maps so permutations never share mutable state.
func pickClusters(catalog map[string]Cluster, order ...string) []Cluster {
	clusters := make([]Cluster, 0, len(order))
	for _, id := range order {
		c := catalog[id]
		if c.Tags != nil {
			tags := make(map[string]string, len(c.Tags))
			for k, v := range c.Tags {
				tags[k] = v
			}
			c.Tags = tags
		}
		clusters = append(clusters, c)
	}
	return clusters
}

// failureBaseInput returns a valid baseline for library-path failure tests:
// only app/revision/image/batchSize are set; each case adds its own
// candidates, conditions and spreadBy.
func failureBaseInput() ReleasePlanInput {
	return ReleasePlanInput{App: "app", Revision: "r1", Image: "img", BatchSize: 2}
}

// TestMakeReleasePlan_AllRejectedReturnsZeroPlan covers "filtering produced
// verdicts for every candidate, yet no plan can be delivered": all
// candidates are filtered out. The call must return a non-nil error and the
// zero plan — app info, batches and the excluded list must not come back as
// partial results. The error carries every candidate's reason, ascending by
// the original cluster ID, with the fixed disabled > exclude > include
// priority. spreadBy names a tag none of the clusters carry; the error must
// still describe the filter reasons, and the result must not depend on the
// order candidates were written in.
func TestMakeReleasePlan_AllRejectedReturnsZeroPlan(t *testing.T) {
	//   a-off   disabled and also carrying env=temp (would match an exclude
	//           condition) -> 集群已停用 wins;
	//   b-both  matches an include condition and an exclude condition
	//           -> 命中排除条件 wins;
	//   c-plain matches no include condition -> 未命中包含条件.
	catalog := map[string]Cluster{
		"a-off":   {ID: "a-off", Disabled: true, Tags: map[string]string{"env": "temp"}},
		"b-both":  {ID: "b-both", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
		"c-plain": {ID: "c-plain", Tags: map[string]string{"env": "dev"}},
	}
	wantErr := strings.Join([]string{
		"没有符合规则的可用集群，各候选集群未入选原因：",
		"  a-off：" + ReasonDisabled,
		"  b-both：" + ReasonExcludeMatched,
		"  c-plain：" + ReasonIncludeNotMatched,
	}, "\n")

	orders := [][]string{
		{"a-off", "b-both", "c-plain"},
		{"c-plain", "a-off", "b-both"},
		{"b-both", "c-plain", "a-off"},
	}
	for _, order := range orders {
		in := failureBaseInput()
		in.SpreadBy = "zone"
		in.Include = []LabelCondition{{"env": "prod"}}
		in.Exclude = []LabelCondition{{"env": "temp"}, {"quarantine": "true"}}
		in.Clusters = pickClusters(catalog, order...)

		before := fmt.Sprintf("%+v", in)
		plan, err := MakeReleasePlan(in)
		if err == nil {
			t.Fatalf("order %v: expected a non-nil error", order)
		}
		if err.Error() != wantErr {
			t.Fatalf("order %v: error mismatch:\n got %q\nwant %q", order, err.Error(), wantErr)
		}
		assertZeroReleasePlan(t, plan)
		if strings.Contains(err.Error(), "故障域") || strings.Contains(err.Error(), "zone") {
			t.Fatalf("with every candidate filtered out the error must keep filter reasons, got %q", err.Error())
		}
		if got := clusterIDOrder(in.Clusters); strings.Join(got, ",") != strings.Join(order, ",") {
			t.Fatalf("failed call reordered candidates: got %v, want %v", got, order)
		}
		if after := fmt.Sprintf("%+v", in); after != before {
			t.Fatalf("failed planning rewrote the input:\n before: %s\n after:  %s", before, after)
		}
	}
}

// TestMakeReleasePlan_SelectedMissingSpreadTagReturnsZeroPlan covers the
// second failure shape: some clusters were selected and others excluded, but
// a selected cluster lacks the spreadBy tag. The whole computation must fail
// with only the zero plan; the error names the missing tag and the smallest
// offending selected cluster by ID. Candidate order must not change that,
// and filtered-out clusters missing the same tag must not preempt the error.
func TestMakeReleasePlan_SelectedMissingSpreadTagReturnsZeroPlan(t *testing.T) {
	// Selected: s-b carries zone; s-a and s-c (multiple offenders) do not.
	// Filtered out: x-off (disabled) and x-dev (include not matched); both
	// lack zone as well, but they never take part in the tag check.
	catalog := map[string]Cluster{
		"s-a":   {ID: "s-a", Tags: map[string]string{"env": "prod"}},
		"s-b":   {ID: "s-b", Tags: map[string]string{"env": "prod", "zone": "east"}},
		"s-c":   {ID: "s-c", Tags: map[string]string{"env": "prod"}},
		"x-off": {ID: "x-off", Disabled: true, Tags: map[string]string{"env": "prod"}},
		"x-dev": {ID: "x-dev", Tags: map[string]string{"env": "dev"}},
	}
	wantErr := `集群 "s-a" 缺少故障域标签 "zone"`

	orders := [][]string{
		{"s-a", "s-b", "s-c", "x-off", "x-dev"},
		{"x-off", "s-c", "x-dev", "s-b", "s-a"}, // filtered and later offenders written first
		{"x-dev", "x-off", "s-c", "s-a", "s-b"},
	}
	for _, order := range orders {
		in := failureBaseInput()
		in.SpreadBy = "zone"
		in.Include = []LabelCondition{{"env": "prod"}}
		in.Clusters = pickClusters(catalog, order...)

		before := fmt.Sprintf("%+v", in)
		plan, err := MakeReleasePlan(in)
		if err == nil {
			t.Fatalf("order %v: expected a non-nil error", order)
		}
		if err.Error() != wantErr {
			t.Fatalf("order %v: error mismatch:\n got %q\nwant %q", order, err.Error(), wantErr)
		}
		assertZeroReleasePlan(t, plan)
		for _, notNamed := range []string{"s-c", "x-off", "x-dev"} {
			if strings.Contains(err.Error(), notNamed) {
				t.Fatalf("error must name only the smallest selected offender s-a, got %q", err.Error())
			}
		}
		if got := clusterIDOrder(in.Clusters); strings.Join(got, ",") != strings.Join(order, ",") {
			t.Fatalf("failed call reordered candidates: got %v, want %v", got, order)
		}
		if after := fmt.Sprintf("%+v", in); after != before {
			t.Fatalf("failed planning rewrote the input:\n before: %s\n after:  %s", before, after)
		}
	}
}

// TestMakeReleasePlan_PresentEmptySpreadTagIsNotMissing is the success
// control for the missing-tag failure: a tag present with an empty-string
// value is a real fault domain and must not be mistaken for a missing tag.
// The same candidate with the tag key absent fails with the zero plan.
func TestMakeReleasePlan_PresentEmptySpreadTagIsNotMissing(t *testing.T) {
	in := failureBaseInput()
	in.SpreadBy = "zone"
	in.Clusters = []Cluster{
		{ID: "e1", Tags: map[string]string{"zone": ""}},
		{ID: "e2", Tags: map[string]string{"zone": "east"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("present tag with empty value must be a valid fault domain, got %v", err)
	}
	if plan.App.Name != "app" || len(plan.Batches) != 1 ||
		strings.Join(plan.Batches[0].Clusters, ",") != "e1,e2" {
		t.Fatalf("unexpected success plan: %+v", plan)
	}

	missing := failureBaseInput()
	missing.SpreadBy = "zone"
	missing.Clusters = []Cluster{
		{ID: "e1"}, // no tags at all: the tag key is absent
		{ID: "e2", Tags: map[string]string{"zone": "east"}},
	}
	zero, err := MakeReleasePlan(missing)
	if err == nil {
		t.Fatal("an absent tag key must fail planning")
	}
	if err.Error() != `集群 "e1" 缺少故障域标签 "zone"` {
		t.Fatalf("error mismatch: %q", err.Error())
	}
	assertZeroReleasePlan(t, zero)
}

// TestMakeReleasePlan_ConfigValidationFailureAlsoReturnsZeroPlan guarantees
// that upfront config validation obeys the same failure convention. The
// duplicate ID appears on a disabled record and the other record would be
// filtered out anyway (and neither carries the spreadBy tag); identity is
// still checked first, so both operations report the configuration error
// and MakeReleasePlan hands back the zero plan.
func TestMakeReleasePlan_ConfigValidationFailureAlsoReturnsZeroPlan(t *testing.T) {
	in := failureBaseInput()
	in.BatchSize = 1
	in.SpreadBy = "zone"
	in.Include = []LabelCondition{{"env": "prod"}}
	in.Clusters = []Cluster{
		{ID: "dup", Tags: map[string]string{"env": "dev"}},
		{ID: "dup", Disabled: true},
	}

	wantErr := `clusters[1]: 重复的集群标识 "dup"`
	verr := ValidateReleaseInput(in)
	if verr == nil {
		t.Fatal("ValidateReleaseInput must reject the duplicate ID, even on a disabled record")
	}
	if verr.Error() != wantErr {
		t.Fatalf("validation error mismatch:\n got %q\nwant %q", verr.Error(), wantErr)
	}

	before := fmt.Sprintf("%+v", in)
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("MakeReleasePlan must reject the duplicate ID")
	}
	if err.Error() != wantErr {
		t.Fatalf("MakeReleasePlan error mismatch:\n got %q\nwant %q", err.Error(), wantErr)
	}
	if err.Error() != verr.Error() {
		t.Fatalf("planning must surface the validation error first:\n plan: %q\nvalidate: %q", err, verr)
	}
	assertZeroReleasePlan(t, plan)
	if after := fmt.Sprintf("%+v", in); after != before {
		t.Fatalf("failed planning rewrote the input:\n before: %s\n after:  %s", before, after)
	}
}

// TestMakeReleasePlan_EmptyCandidatesValidateSucceedsPlanFails preserves the
// distinction between the two public operations: with an empty candidate
// list and otherwise legal fields, validation alone succeeds, while planning
// reports that no candidates were given and returns the zero plan.
func TestMakeReleasePlan_EmptyCandidatesValidateSucceedsPlanFails(t *testing.T) {
	for _, clusters := range [][]Cluster{nil, []Cluster{}} {
		in := failureBaseInput()
		in.Clusters = clusters
		if err := ValidateReleaseInput(in); err != nil {
			t.Fatalf("an empty candidate list with otherwise legal fields must validate, got %v", err)
		}
		plan, err := MakeReleasePlan(in)
		if err == nil {
			t.Fatal("planning without candidates must fail")
		}
		if err.Error() != "没有可发布的集群：未提供候选集群" {
			t.Fatalf("error mismatch: got %q", err.Error())
		}
		assertZeroReleasePlan(t, plan)
	}
}
