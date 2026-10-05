package darksafe

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

// This file is a regression net for the documented failure-return contract of
// MakeReleasePlan driven the library way: a ReleasePlanInput constructed
// directly in memory — no JSON document and no command line in between. The
// README promises that MakeReleasePlan hands over a complete plan only on
// success; on every failure it returns non-nil error together with the ZERO
// ReleasePlan. These tests therefore assert only what a caller actually
// observes — the returned plan, the returned error and the untouched input —
// never how the planner organizes its intermediate selection.

// assertZeroReleasePlan checks that a failed call leaked no partial result:
// no app info, no batches and not even the excluded list computed during
// filtering are returned.
func assertZeroReleasePlan(t *testing.T, plan ReleasePlan) {
	t.Helper()
	if plan.App != (AppInfo{}) {
		t.Fatalf("failure must not return partial app info, got %+v", plan.App)
	}
	if plan.Batches != nil {
		t.Fatalf("failure must not return partial batches, got %+v", plan.Batches)
	}
	if plan.Excluded != nil {
		t.Fatalf("failure must not return a partial excluded list, got %+v", plan.Excluded)
	}
	if !reflect.DeepEqual(plan, ReleasePlan{}) {
		t.Fatalf("failure must return the zero-value ReleasePlan, got %+v", plan)
	}
}

// allRejectedLibraryError renders the exact library error for a fully rejected
// plan: the summary line followed by one "  <id>：<reason>" line per
// candidate, ascending by ID. Unlike the CLI variant there is no trailing
// newline — the error is returned, not Fprintln'ed.
func allRejectedLibraryError(entries ...string) string {
	var b strings.Builder
	b.WriteString("没有符合规则的可用集群，各候选集群未入选原因：")
	for _, e := range entries {
		b.WriteString("\n  ")
		b.WriteString(e)
	}
	return b.String()
}

// allRejectedMixedInput builds an in-memory config in which filtering has run
// and produced results — every rejection reason occurs, and the fixed
// disabled → exclude → include priority is observable in one shared error:
//   - c-both matches both the include (env=prod) and an exclude
//     (quarantine=true) condition: exclude wins;
//   - c-temp matches an exclude condition: rejected even though it would also
//     fail the include check;
//   - c-off is disabled AND matches an exclude condition: disabled wins;
//   - c-dev matches no include condition.
//
// SpreadBy is enabled while NONE of these clusters carries the zone tag; the
// all-rejected failure must still report the filter reasons rather than turn
// into a missing-fault-domain-tag error. The permutation arguments reorder the
// candidates to prove none of this depends on input order.
func allRejectedMixedInput(order string) ReleasePlanInput {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Include:   []LabelCondition{{"env": "prod"}},
		Exclude:   []LabelCondition{{"env": "temp"}, {"quarantine": "true"}},
	}
	candidates := map[string][]Cluster{
		"a": {
			{ID: "c-both", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "c-dev", Tags: map[string]string{"env": "dev"}},
			{ID: "c-off", Disabled: true, Tags: map[string]string{"env": "temp"}},
			{ID: "c-temp", Tags: map[string]string{"env": "temp"}},
		},
		"b": {
			{ID: "c-temp", Tags: map[string]string{"env": "temp"}},
			{ID: "c-off", Disabled: true, Tags: map[string]string{"env": "temp"}},
			{ID: "c-dev", Tags: map[string]string{"env": "dev"}},
			{ID: "c-both", Tags: map[string]string{"quarantine": "true", "env": "prod"}},
		},
	}
	in.Clusters = candidates[order]
	return in
}

// wantAllRejectedMixedError is the complete, exact error for
// allRejectedMixedInput: every candidate exactly once, ascending by ID with
// the reason the priority rules dictate.
func wantAllRejectedMixedError() string {
	return allRejectedLibraryError(
		"c-both："+ReasonExcludeMatched,
		"c-dev："+ReasonIncludeNotMatched,
		"c-off："+ReasonDisabled,
		"c-temp："+ReasonExcludeMatched,
	)
}

// TestMakeReleasePlan_AllRejectedReturnsZeroPlanWithReasons covers the
// "filtering produced results but no plan can be delivered" failure directly
// from an in-memory config: all four candidates are filtered out (one per
// reason/priority interaction) while spreadBy is enabled and none has the
// spread tag. The call must return a non-nil error carrying every candidate's
// reason ascending by ID, and the zero-value plan — app info, batches and the
// excluded list must not come back as partial results.
func TestMakeReleasePlan_AllRejectedReturnsZeroPlanWithReasons(t *testing.T) {
	in := allRejectedMixedInput("a")
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error when every candidate is filtered out")
	}
	assertZeroReleasePlan(t, plan)

	msg := err.Error()
	if msg != wantAllRejectedMixedError() {
		t.Fatalf("rejection error mismatch:\n got %q\nwant %q", msg, wantAllRejectedMixedError())
	}
	// The filter failure must not be replaced by the fault-domain check:
	// filtered-out clusters need no spreadBy tag.
	if strings.Contains(msg, "故障域") || strings.Contains(msg, "缺少") {
		t.Fatalf("all-rejected failure must report filter reasons, got %q", msg)
	}
}

// TestMakeReleasePlan_AllRejectedOrderIndependent runs the same configuration
// with candidates written in a different order: the error and the zero plan
// must be identical.
func TestMakeReleasePlan_AllRejectedOrderIndependent(t *testing.T) {
	planA, errA := MakeReleasePlan(allRejectedMixedInput("a"))
	planB, errB := MakeReleasePlan(allRejectedMixedInput("b"))
	if errA == nil || errB == nil {
		t.Fatal("both permutations must fail")
	}
	if errA.Error() != errB.Error() {
		t.Fatalf("candidate order changed the error:\n a: %q\n b: %q", errA.Error(), errB.Error())
	}
	if errA.Error() != wantAllRejectedMixedError() {
		t.Fatalf("unexpected error: %q", errA.Error())
	}
	assertZeroReleasePlan(t, planA)
	assertZeroReleasePlan(t, planB)
}

// missingSpreadTagInput builds an in-memory config where selection already has
// results on both sides: s-a is selected and carries the zone tag; s-b is
// selected but lacks it; x-off (disabled) and x-dev (fails the include
// condition) are filtered out and ALSO lack the tag. Planning must fail on
// s-b only — a filtered cluster's missing tag must never preempt the error.
func missingSpreadTagInput(order string) ReleasePlanInput {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Include:   []LabelCondition{{"env": "prod"}},
	}
	candidates := map[string][]Cluster{
		"a": {
			{ID: "s-a", Tags: map[string]string{"env": "prod", "zone": "z1"}},
			{ID: "s-b", Tags: map[string]string{"env": "prod"}},
			{ID: "x-off", Disabled: true},
			{ID: "x-dev", Tags: map[string]string{"env": "dev"}},
		},
		"b": {
			{ID: "x-dev", Tags: map[string]string{"env": "dev"}},
			{ID: "s-b", Tags: map[string]string{"env": "prod"}},
			{ID: "x-off", Disabled: true},
			{ID: "s-a", Tags: map[string]string{"zone": "z1", "env": "prod"}},
		},
	}
	in.Clusters = candidates[order]
	return in
}

// TestMakeReleasePlan_SelectedMissingSpreadTagReturnsZeroPlan covers the
// "some clusters selected, some excluded, but a selected cluster lacks the
// spreadBy tag" failure: the whole computation fails with the zero-value plan,
// and the error names the missing tag and the smallest offending selected
// cluster. Filtered-out clusters that also lack the tag must not be reported
// instead, and candidate input order must not change anything.
func TestMakeReleasePlan_SelectedMissingSpreadTagReturnsZeroPlan(t *testing.T) {
	const wantErr = `集群 "s-b" 缺少故障域标签 "zone"`
	for _, order := range []string{"a", "b"} {
		t.Run("order-"+order, func(t *testing.T) {
			plan, err := MakeReleasePlan(missingSpreadTagInput(order))
			if err == nil {
				t.Fatal("expected a non-nil error for the missing spread tag")
			}
			assertZeroReleasePlan(t, plan)
			if err.Error() != wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), wantErr)
			}
			// The error must name the tag key and the smallest selected
			// offender, and never a cluster filtered out earlier.
			msg := err.Error()
			if !strings.Contains(msg, `"s-b"`) || !strings.Contains(msg, `"zone"`) {
				t.Fatalf("error must name s-b and the zone tag, got %q", msg)
			}
			if strings.Contains(msg, "x-off") || strings.Contains(msg, "x-dev") {
				t.Fatalf("filtered-out clusters must not preempt the error, got %q", msg)
			}
		})
	}
}

// TestMakeReleasePlan_MissingSpreadTagReportsSmallestCluster selects several
// clusters, two of which lack the tag: regardless of input order, the error
// names m-a — the smallest ID among selected offenders — and only that one.
func TestMakeReleasePlan_MissingSpreadTagReportsSmallestCluster(t *testing.T) {
	build := func(order string) ReleasePlanInput {
		in := ReleasePlanInput{
			App: "app", Revision: "r1", Image: "img", BatchSize: 2, SpreadBy: "zone",
		}
		candidates := map[string][]Cluster{
			"a": {
				{ID: "m-c", Tags: map[string]string{"zone": "z2"}},
				{ID: "m-a"},
				{ID: "m-b", Tags: map[string]string{}},
			},
			"b": {
				{ID: "m-b", Tags: map[string]string{}},
				{ID: "m-c", Tags: map[string]string{"zone": "z2"}},
				{ID: "m-a"},
			},
		}
		in.Clusters = candidates[order]
		return in
	}
	const wantErr = `集群 "m-a" 缺少故障域标签 "zone"`
	for _, order := range []string{"a", "b"} {
		plan, err := MakeReleasePlan(build(order))
		if err == nil {
			t.Fatalf("order %s: expected error", order)
		}
		assertZeroReleasePlan(t, plan)
		if err.Error() != wantErr {
			t.Fatalf("order %s: error = %q, want %q", order, err.Error(), wantErr)
		}
	}
}

// TestMakeReleasePlan_EmptyStringSpreadTagSucceeds is the success control for
// the missing-tag failure above: a tag PRESENT with an empty string value is a
// legal fault domain and must not be mistaken for a missing tag. Two
// empty-domain clusters avoid each other across batches, a disabled cluster
// without the tag is filtered out, and a complete plan is returned.
func TestMakeReleasePlan_EmptyStringSpreadTagSucceeds(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Include:   []LabelCondition{{"env": "prod"}},
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"env": "prod", "zone": ""}},
			{ID: "b", Tags: map[string]string{"env": "prod", "zone": ""}},
			{ID: "c", Tags: map[string]string{"env": "prod", "zone": "east"}},
			{ID: "x", Disabled: true, Tags: map[string]string{"env": "prod"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("an empty-string tag value is a valid domain: %v", err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,c|b" {
		t.Fatalf("batches = %v, want [a c]|[b]", got)
	}
	if len(plan.Excluded) != 1 || plan.Excluded[0] != (ExcludedCluster{ID: "x", Reason: ReasonDisabled}) {
		t.Fatalf("excluded = %+v, want x: %s", plan.Excluded, ReasonDisabled)
	}
}

// TestMakeReleasePlan_ConfigValidationFailureReturnsZeroPlan checks that
// front-loaded config validation obeys the same failure convention. The focus
// case is a duplicate cluster ID where the SECOND record is disabled: identity
// is checked before any disabled/filter decision, so MakeReleasePlan must
// surface the configuration error first (never silently plan with the first
// record), ValidateReleaseInput must reject it on its own, and the returned
// plan is the zero value.
func TestMakeReleasePlan_ConfigValidationFailureReturnsZeroPlan(t *testing.T) {
	dup := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Clusters: []Cluster{
			{ID: "dup", Tags: map[string]string{"env": "prod", "zone": "z1"}},
			{ID: "dup", Disabled: true},
		},
	}
	verr := ValidateReleaseInput(dup)
	if verr == nil {
		t.Fatal("ValidateReleaseInput must reject the duplicate, even inside a disabled record")
	}
	if !strings.Contains(verr.Error(), `clusters[1]`) || !strings.Contains(verr.Error(), `重复的集群标识 "dup"`) {
		t.Fatalf("validation error must name position 1 and the duplicated id, got %v", verr)
	}

	plan, err := MakeReleasePlan(dup)
	if err == nil {
		t.Fatal("MakeReleasePlan must fail with a config error")
	}
	if err.Error() != verr.Error() {
		t.Fatalf("MakeReleasePlan must report the config error first:\n plan: %q\nvalidate: %q", err, verr)
	}
	assertZeroReleasePlan(t, plan)
	if strings.Contains(err.Error(), "未提供候选集群") || strings.Contains(err.Error(), "故障域") {
		t.Fatalf("config error must not be masked by a planning failure, got %q", err)
	}

	// Other pre-validation failures keep the same zero-plan convention.
	for name, mutate := range map[string]func(*ReleasePlanInput){
		"app empty":      func(in *ReleasePlanInput) { in.App = "" },
		"batch zero":     func(in *ReleasePlanInput) { in.BatchSize = 0 },
		"empty tag key":  func(in *ReleasePlanInput) { in.Clusters[0].Tags = map[string]string{"": "v"} },
		"whitespace sid": func(in *ReleasePlanInput) { in.SpreadBy = "  " },
	} {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			mutate(&in)
			p, merr := MakeReleasePlan(in)
			if merr == nil {
				t.Fatalf("expected error for %s", name)
			}
			assertZeroReleasePlan(t, p)
		})
	}
}

// cloneInput returns a deep copy of the caller-owned parts of a config
// (candidate order, cluster tags and include/exclude conditions), so a test
// can prove a failed computation left the original byte-for-byte unchanged.
func cloneInput(in ReleasePlanInput) ReleasePlanInput {
	cp := in
	cp.Clusters = append([]Cluster(nil), in.Clusters...)
	for i := range cp.Clusters {
		cp.Clusters[i].Tags = maps.Clone(in.Clusters[i].Tags)
	}
	cp.Include = cloneConditions(in.Include)
	cp.Exclude = cloneConditions(in.Exclude)
	return cp
}

func cloneConditions(conds []LabelCondition) []LabelCondition {
	if conds == nil {
		return nil
	}
	out := make([]LabelCondition, len(conds))
	for i, cond := range conds {
		out[i] = maps.Clone(cond)
	}
	return out
}

// candidateIDs returns the candidate IDs in the exact order the caller passed
// them.
func candidateIDs(in ReleasePlanInput) []string {
	ids := make([]string, len(in.Clusters))
	for i, c := range in.Clusters {
		ids[i] = c.ID
	}
	return ids
}

// TestMakeReleasePlan_FailureDoesNotMutateInput runs every failure shape and
// checks the caller's config is untouched: candidate order is preserved, and
// neither cluster tags nor include/exclude conditions are rewritten — even
// though the planner sorts and inspects them internally.
func TestMakeReleasePlan_FailureDoesNotMutateInput(t *testing.T) {
	cases := map[string]ReleasePlanInput{
		"all rejected":       allRejectedMixedInput("b"),
		"missing spread tag": missingSpreadTagInput("b"),
		"no candidates": {
			App: "app", Revision: "r1", Image: "img", BatchSize: 2,
			SpreadBy: "zone",
			Include:  []LabelCondition{{"env": "prod"}},
			Exclude:  []LabelCondition{{"env": "temp"}},
		},
		"duplicate disabled id": {
			App: "app", Revision: "r1", Image: "img", BatchSize: 2,
			Clusters: []Cluster{
				{ID: "dup", Tags: map[string]string{"env": "prod"}},
				{ID: "dup", Disabled: true, Tags: map[string]string{"env": "temp"}},
			},
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			before := cloneInput(in)
			orderBefore := candidateIDs(in)
			plan, err := MakeReleasePlan(in)
			if err == nil {
				t.Fatal("expected a failed computation")
			}
			assertZeroReleasePlan(t, plan)
			if got := candidateIDs(in); !reflect.DeepEqual(got, orderBefore) {
				t.Fatalf("candidate order changed: %v -> %v", orderBefore, got)
			}
			if !reflect.DeepEqual(in, before) {
				t.Fatalf("failed computation mutated the input:\n before: %#v\n after:  %#v", before, in)
			}
		})
	}
}

// TestValidate_EmptyCandidatesKeepsOperationsDistinct pins the distinction
// between the two public operations when every other field is legal: standalone
// validation SUCCEEDS on an empty candidate list (the config is well-formed),
// while actually planning fails with "未提供候选集群" and the zero-value plan.
// Both nil and a non-nil empty slice are empty candidate lists.
func TestValidate_EmptyCandidatesKeepsOperationsDistinct(t *testing.T) {
	for name, clusters := range map[string][]Cluster{"nil slice": nil, "empty slice": {}} {
		t.Run(name, func(t *testing.T) {
			in := ReleasePlanInput{
				App:       "app",
				Revision:  "r1",
				Image:     "img",
				BatchSize: 2,
				SpreadBy:  "zone",
				Include:   []LabelCondition{{"env": "prod"}},
				Exclude:   []LabelCondition{{"env": "temp"}},
				Clusters:  clusters,
			}
			if err := ValidateReleaseInput(in); err != nil {
				t.Fatalf("validation alone must succeed with legal fields and no candidates, got %v", err)
			}
			plan, err := MakeReleasePlan(in)
			if err == nil {
				t.Fatal("planning with no candidates must fail")
			}
			if !strings.Contains(err.Error(), "没有可发布的集群：未提供候选集群") {
				t.Fatalf("expected the no-candidate error, got %q", err.Error())
			}
			// This is distinct from the all-rejected failure, which lists
			// per-candidate reasons.
			if strings.Contains(err.Error(), "未入选原因") {
				t.Fatalf("no-candidate error must not look like an all-rejected error: %q", err)
			}
			assertZeroReleasePlan(t, plan)
		})
	}
}
