package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// This file is the automated regression net for multi-label INCLUDE conditions
// in the offline release plan — the mirror image of multi_label_exclude_test.go.
// The distinction it protects is the AND/OR structure of label matching (also
// documented in README "标签条件怎么匹配"):
//
//   - Within one condition object every key-value pair must match (AND): a
//     cluster earns release eligibility through {"env":"prod","tier":"web"}
//     only when it carries BOTH pairs. Merely sharing one label is not a hit.
//   - Across the include array ANY one fully matched condition admits (OR):
//     pairs satisfied in different conditions must never be stitched together
//     into one admission — a cluster holding half of condition 1 and half of
//     condition 2 qualifies under neither.
//
// Every assertion is written against the published result of MakeReleasePlan
// — batches, the excluded list and, on the all-rejected path, the error —
// never against the internal matching helpers. Filtering and batching behavior
// itself is out of scope for change; these tests pin it as it is.

// multiIncludeConditions are the two two-label include conditions used
// throughout this file: each must be satisfied wholesale.
func multiIncludeConditions() []LabelCondition {
	return []LabelCondition{
		{"env": "prod", "tier": "web"},
		{"region": "west", "track": "stable"},
	}
}

// multiIncludeCandidates returns the candidate roster shared by the headline
// tests, covering every AND/OR interaction of the two conditions:
//   - full-one    fully satisfies condition 1 (plus an unrelated label);
//   - full-two    fully satisfies condition 2 (plus an unrelated label);
//   - full-both   fully satisfies BOTH conditions: still scheduled exactly once;
//   - partial-one satisfies only env=prod of condition 1: no full hit;
//   - partial-two satisfies only region=west of condition 2: no full hit;
//   - cross-pairs satisfies env=prod (condition 1) and region=west
//     (condition 2) but neither tier=web nor track=stable: the two half-hits
//     live in different conditions and must not be combined into an admission;
//   - unrelated   matches nothing either way.
func multiIncludeCandidates() []Cluster {
	return []Cluster{
		{ID: "full-one", Tags: map[string]string{
			"env": "prod", "tier": "web", "team": "payments",
		}},
		{ID: "full-two", Tags: map[string]string{
			"region": "west", "track": "stable", "note": "canary",
		}},
		{ID: "full-both", Tags: map[string]string{
			"env": "prod", "tier": "web",
			"region": "west", "track": "stable",
		}},
		{ID: "partial-one", Tags: map[string]string{"env": "prod"}},
		{ID: "partial-two", Tags: map[string]string{"region": "west", "track": "beta"}},
		{ID: "cross-pairs", Tags: map[string]string{"env": "prod", "region": "west"}},
		{ID: "unrelated", Tags: map[string]string{"env": "dev", "region": "east"}},
	}
}

// TestInclude_MultiLabelConditionRequiresAllPairs is the headline AND/OR
// regression: only clusters fully satisfying at least one whole condition are
// released; partial and cross-condition half-hits are kept out of every batch
// and recorded as 未命中包含条件. A cluster fully matching both conditions
// appears exactly once in the plan. Extra, unrelated tags must not change any
// of these outcomes.
func TestInclude_MultiLabelConditionRequiresAllPairs(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 10,
		Include:   multiIncludeConditions(),
		Clusters:  multiIncludeCandidates(),
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}

	rows := planRows(t, plan)
	rows.requireSelected(t, "full-one")
	rows.requireSelected(t, "full-two")
	rows.requireSelected(t, "full-both")
	rows.requireExcluded(t, "partial-one", ReasonIncludeNotMatched)
	rows.requireExcluded(t, "partial-two", ReasonIncludeNotMatched)
	rows.requireExcluded(t, "cross-pairs", ReasonIncludeNotMatched)
	rows.requireExcluded(t, "unrelated", ReasonIncludeNotMatched)

	// Unselected records keep the existing convention: ascending by ID, one
	// record per cluster, reason 未命中包含条件.
	wantExcluded := []ExcludedCluster{
		{ID: "cross-pairs", Reason: ReasonIncludeNotMatched},
		{ID: "partial-one", Reason: ReasonIncludeNotMatched},
		{ID: "partial-two", Reason: ReasonIncludeNotMatched},
		{ID: "unrelated", Reason: ReasonIncludeNotMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}

	// Selected clusters keep the existing scheduling convention: ascending by
	// ID in one batch; the double-hit cluster occurs exactly once.
	wantBatches := []Batch{{Index: 1, Clusters: []string{
		"full-both", "full-one", "full-two",
	}}}
	if !reflect.DeepEqual(plan.Batches, wantBatches) {
		t.Fatalf("batches = %+v, want %+v", plan.Batches, wantBatches)
	}
}

// TestInclude_MultiLabelOrderAndTagIrrelevance proves the AND/OR result does
// not depend on how the author writes the document: reordering the conditions,
// shuffling the pairs inside a condition and the tags inside a cluster,
// reordering the candidates and adding unrelated labels must all yield
// byte-identical plans — the released set, its schedule and the excluded list
// stay exactly the same.
func TestInclude_MultiLabelOrderAndTagIrrelevance(t *testing.T) {
	base := `{
	  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
	  "include": [
	    {"env": "prod", "tier": "web"},
	    {"region": "west", "track": "stable"}
	  ],
	  "clusters": [
	    {"id": "full-both", "tags": {"env": "prod", "tier": "web", "region": "west", "track": "stable"}},
	    {"id": "cross-pairs", "tags": {"env": "prod", "region": "west"}},
	    {"id": "full-one", "tags": {"env": "prod", "tier": "web", "team": "payments"}},
	    {"id": "partial-one", "tags": {"env": "prod"}},
	    {"id": "unrelated", "tags": {"env": "dev", "region": "east"}}
	  ]
	}`
	shuffled := `{
	  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
	  "include": [
	    {"track": "stable", "region": "west"},
	    {"tier": "web", "env": "prod"}
	  ],
	  "clusters": [
	    {"id": "unrelated", "tags": {"region": "east", "env": "dev", "extra": "x"}},
	    {"id": "partial-one", "tags": {"env": "prod", "extra": "x"}},
	    {"id": "full-one", "tags": {"team": "payments", "tier": "web", "env": "prod"}},
	    {"id": "cross-pairs", "tags": {"region": "west", "env": "prod", "extra": "x"}},
	    {"id": "full-both", "tags": {"track": "stable", "region": "west", "tier": "web", "env": "prod"}}
	  ]
	}`
	p1, err := MakeReleasePlan(parsePlan(t, base))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(parsePlan(t, shuffled))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("plans differ across order/extra-tag changes:\n %+v\n %+v", p1, p2)
	}

	// Exact string comparison protects the boundary: superficially similar
	// keys and values must not turn a half-hit into a full hit.
	doc := `{
	  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
	  "include": [
	    {"env": "prod", "tier": "web"},
	    {"region": "west", "track": "stable"}
	  ],
	  "clusters": [
	    {"id": "case-key", "tags": {"ENV": "prod", "tier": "web"}},
	    {"id": "case-val", "tags": {"env": "prod", "tier": "Web"}},
	    {"id": "space-val", "tags": {"env": "prod", "tier": "web "}},
	    {"id": "near-miss", "tags": {"env": "prod", "region": "west", "track": " stable"}}
	  ]
	}`
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err == nil {
		t.Fatal("every candidate is a near-miss, so the plan must be rejected, not delivered empty")
	}
	assertZeroReleasePlan(t, plan)
	for _, id := range []string{"case-key", "case-val", "space-val", "near-miss"} {
		if !strings.Contains(err.Error(), "\n  "+id+"："+ReasonIncludeNotMatched) {
			t.Fatalf("near-miss %q must be reported as %s, got %q", id, ReasonIncludeNotMatched, err.Error())
		}
	}
}

// TestInclude_MultiLabelWithSpreadBy combines the multi-label include filter
// with fault-domain batching:
//   - clusters that only partially match the include conditions are filtered
//     out first; even though they carry no fault-domain tag, they must not
//     break the plan or shrink the batches of the fully-matching clusters;
//   - the fully-matching clusters, all carrying the zone tag, are spread by
//     the existing rules (at most one cluster per domain per batch).
func TestInclude_MultiLabelWithSpreadBy(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Include:   multiIncludeConditions(),
		Clusters: []Cluster{
			{ID: "s-a", Tags: map[string]string{"env": "prod", "tier": "web", "zone": "z1"}},
			{ID: "s-b", Tags: map[string]string{"region": "west", "track": "stable", "zone": "z1"}},
			{ID: "s-c", Tags: map[string]string{
				"env": "prod", "tier": "web", "region": "west", "track": "stable",
				"zone": "z2", "team": "core",
			}},
			// Filtered out by the include filter and lacking the zone tag:
			// their missing tag must not fail the plan.
			{ID: "x-partial", Tags: map[string]string{"env": "prod"}},
			{ID: "x-cross", Tags: map[string]string{"env": "prod", "region": "west"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("filtered-out clusters need no fault-domain tag: %v", err)
	}
	// s-a and s-b share domain z1 and must be split; s-c (z2) joins the first
	// batch under the existing smallest-first spread convention.
	if got := batchIDs(plan); strings.Join(got, "|") != "s-a,s-c|s-b" {
		t.Fatalf("spread batches = %v, want [s-a s-c]|[s-b]", got)
	}
	wantExcluded := []ExcludedCluster{
		{ID: "x-cross", Reason: ReasonIncludeNotMatched},
		{ID: "x-partial", Reason: ReasonIncludeNotMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
}

// TestInclude_MultiLabelSpreadByMissingTagFailsWholePlan is the converse of
// the spread case above: a cluster that fully satisfies an include condition
// and is therefore selected MUST carry the fault-domain tag. When it does
// not, the whole plan is rejected — zero-value plan, no partial batches — and
// the error names the missing tag and the smallest offending selected
// cluster. Partially-matching clusters that also lack the tag must not
// preempt or mask that error.
func TestInclude_MultiLabelSpreadByMissingTagFailsWholePlan(t *testing.T) {
	build := func(order string) ReleasePlanInput {
		in := ReleasePlanInput{
			App:       "app",
			Revision:  "r1",
			Image:     "img",
			BatchSize: 2,
			SpreadBy:  "zone",
			Include:   multiIncludeConditions(),
		}
		candidates := map[string][]Cluster{
			"a": {
				{ID: "s-ok", Tags: map[string]string{"env": "prod", "tier": "web", "zone": "z1"}},
				{ID: "s-miss", Tags: map[string]string{"region": "west", "track": "stable"}},
				{ID: "x-partial", Tags: map[string]string{"env": "prod"}},
			},
			"b": {
				{ID: "x-partial", Tags: map[string]string{"env": "prod"}},
				{ID: "s-miss", Tags: map[string]string{"track": "stable", "region": "west"}},
				{ID: "s-ok", Tags: map[string]string{"tier": "web", "zone": "z1", "env": "prod"}},
			},
		}
		in.Clusters = candidates[order]
		return in
	}
	const wantErr = `集群 "s-miss" 缺少故障域标签 "zone"`
	for _, order := range []string{"a", "b"} {
		t.Run("order-"+order, func(t *testing.T) {
			plan, err := MakeReleasePlan(build(order))
			if err == nil {
				t.Fatal("a selected cluster without the fault-domain tag must fail the whole plan")
			}
			assertZeroReleasePlan(t, plan)
			if err.Error() != wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), wantErr)
			}
			if strings.Contains(err.Error(), "x-partial") {
				t.Fatalf("a filtered-out cluster must not preempt the missing-tag error, got %q", err.Error())
			}
		})
	}
}

// TestInclude_AllCandidatesPartialMatchFails covers the complete failure
// result under multi-label include conditions: when every candidate satisfies
// only part of every condition, planning must FAIL — never succeed with an
// empty batch list. The call returns the zero-value plan and a non-nil error
// listing every candidate exactly once, ascending by ID, with reason
// 未命中包含条件. spreadBy is enabled while no candidate carries the zone tag:
// the filter verdict must not be replaced by a missing-fault-domain-tag error.
func TestInclude_AllCandidatesPartialMatchFails(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Include:   multiIncludeConditions(),
		Clusters: []Cluster{
			{ID: "p-cross", Tags: map[string]string{"env": "prod", "region": "west"}},
			{ID: "p-one", Tags: map[string]string{"env": "prod"}},
			{ID: "p-two", Tags: map[string]string{"track": "stable"}},
			{ID: "p-unrelated", Tags: map[string]string{"env": "dev"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error when no candidate fully matches an include condition")
	}
	assertZeroReleasePlan(t, plan)

	msg := err.Error()
	want := allRejectedLibraryError(
		"p-cross："+ReasonIncludeNotMatched,
		"p-one："+ReasonIncludeNotMatched,
		"p-two："+ReasonIncludeNotMatched,
		"p-unrelated："+ReasonIncludeNotMatched,
	)
	if msg != want {
		t.Fatalf("rejection error mismatch:\n got %q\nwant %q", msg, want)
	}
	// Every candidate must be accounted for exactly once, and the filter
	// failure must not be masked by the fault-domain check.
	for _, id := range []string{"p-cross", "p-one", "p-two", "p-unrelated"} {
		if strings.Count(msg, "\n  "+id+"：") != 1 {
			t.Fatalf("candidate %q must appear exactly once in %q", id, msg)
		}
	}
	if strings.Contains(msg, "故障域") || strings.Contains(msg, "缺少") {
		t.Fatalf("all-rejected failure must report filter reasons, got %q", msg)
	}
}

// TestInclude_PriorityWithDisabledAndExclude protects how the include
// decision interacts with the surrounding rules (priority 停用 → 排除 → 包含):
// fully matching an include condition never grants release eligibility on its
// own —
//   - off-hit   is disabled AND fully satisfies an include condition: the
//     reason stays 集群已停用;
//   - excl-hit  fully satisfies an include condition AND an exclude
//     condition: exclude wins with 命中排除条件;
//   - half-excl fully satisfies an include condition and half of an exclude
//     condition: the half exclude hit does not matter, it is released;
//   - selected  fully satisfies an include condition and nothing else.
func TestInclude_PriorityWithDisabledAndExclude(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 10,
		Include:   multiIncludeConditions(),
		Exclude:   []LabelCondition{{"quarantine": "true", "env": "prod"}},
		Clusters: []Cluster{
			{ID: "selected", Tags: map[string]string{"env": "prod", "tier": "web"}},
			{ID: "off-hit", Disabled: true, Tags: map[string]string{
				"region": "west", "track": "stable",
			}},
			{ID: "excl-hit", Tags: map[string]string{
				"env": "prod", "tier": "web", "quarantine": "true",
			}},
			{ID: "half-excl", Tags: map[string]string{
				"env": "prod", "tier": "web", "quarantine": "false",
			}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	rows := planRows(t, plan)
	rows.requireSelected(t, "selected")
	rows.requireSelected(t, "half-excl")
	rows.requireExcluded(t, "off-hit", ReasonDisabled)
	rows.requireExcluded(t, "excl-hit", ReasonExcludeMatched)

	wantExcluded := []ExcludedCluster{
		{ID: "excl-hit", Reason: ReasonExcludeMatched},
		{ID: "off-hit", Reason: ReasonDisabled},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
	wantBatches := []Batch{{Index: 1, Clusters: []string{"half-excl", "selected"}}}
	if !reflect.DeepEqual(plan.Batches, wantBatches) {
		t.Fatalf("batches = %+v, want %+v", plan.Batches, wantBatches)
	}
}
