package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// This file is the automated regression net for multi-label INCLUDE conditions
// in the offline release plan. It protects the AND/OR structure documented in
// README "标签条件怎么匹配" from the include side:
//
//   - Within one include condition object every key-value pair must match
//     (AND): a cluster written for {"env":"prod","tier":"edge"} is eligible
//     only when it carries BOTH pairs. Merely sharing one label is not a hit.
//   - Across the include array ANY one fully matched condition is enough (OR):
//     pairs satisfied in two different conditions must never be stitched
//     together into a single hit. A cluster that only completes half of this
//     condition and half of that one is not eligible.
//
// The business value: an operator who writes several multi-label include
// conditions means "release only to clusters that fully belong to one of these
// fleets". A cluster must never enter the plan just because it happens to carry
// a few labels spread across the conditions.
//
// Every assertion is made against the published result of MakeReleasePlan —
// batches, the excluded list and, on the failure paths, the error and the
// returned plan value — never against the internal matching helpers. The
// existing helpers planRows/allRejectedLibraryError/assertZeroReleasePlan are
// reused so this net stays consistent with the exclude-side one.

// multiIncludeConditions are the two two-label include conditions used
// throughout this file: each must be satisfied wholesale, satisfying one pair
// from each is not enough.
func multiIncludeConditions() []LabelCondition {
	return []LabelCondition{
		{"env": "prod", "tier": "edge"},
		{"region": "west", "track": "canary"},
	}
}

// TestInclude_MultiLabelConditionRequiresAllPairs is the headline AND/OR
// regression. The two include conditions are
// {"env":"prod","tier":"edge"} and {"region":"west","track":"canary"}.
//
//   - partial-one  satisfies only env=prod of condition 1: not eligible.
//   - partial-two  satisfies only region=west of condition 2: not eligible.
//   - cross-pairs  satisfies env=prod (condition 1) and region=west
//     (condition 2) but neither tier=edge nor track=canary: the two half-hits
//     live in different conditions and must not combine into eligibility.
//   - hit-one      fully satisfies condition 1 (plus an unrelated label).
//   - hit-two      fully satisfies condition 2.
//   - hit-both     fully satisfies BOTH conditions: still selected exactly
//     once, appearing in a single batch.
//
// Every non-eligible cluster keeps reason 未命中包含条件; extra unrelated tags
// must not change any outcome; keys and values stay exact strings.
func TestInclude_MultiLabelConditionRequiresAllPairs(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 10,
		Include:   multiIncludeConditions(),
		Clusters: []Cluster{
			{ID: "partial-one", Tags: map[string]string{"env": "prod"}},
			{ID: "partial-two", Tags: map[string]string{"region": "west"}},
			{ID: "cross-pairs", Tags: map[string]string{"env": "prod", "region": "west"}},
			{ID: "hit-one", Tags: map[string]string{
				"env": "prod", "tier": "edge", "team": "payments",
			}},
			{ID: "hit-two", Tags: map[string]string{
				"region": "west", "track": "canary", "note": "early",
			}},
			{ID: "hit-both", Tags: map[string]string{
				"env": "prod", "tier": "edge",
				"region": "west", "track": "canary",
			}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}

	rows := planRows(t, plan)
	rows.requireSelected(t, "hit-one")
	rows.requireSelected(t, "hit-two")
	rows.requireSelected(t, "hit-both")
	rows.requireExcluded(t, "partial-one", ReasonIncludeNotMatched)
	rows.requireExcluded(t, "partial-two", ReasonIncludeNotMatched)
	rows.requireExcluded(t, "cross-pairs", ReasonIncludeNotMatched)

	// A cluster matching two conditions still occurs exactly once in the
	// ascending schedule: planRows would already fail on a duplicate ID.
	wantBatches := []Batch{{Index: 1, Clusters: []string{
		"hit-both", "hit-one", "hit-two",
	}}}
	if !reflect.DeepEqual(plan.Batches, wantBatches) {
		t.Fatalf("batches = %+v, want %+v", plan.Batches, wantBatches)
	}

	// Unselected records keep the existing convention: ascending by ID, each
	// half-hit listed exactly once with 未命中包含条件.
	wantExcluded := []ExcludedCluster{
		{ID: "cross-pairs", Reason: ReasonIncludeNotMatched},
		{ID: "partial-one", Reason: ReasonIncludeNotMatched},
		{ID: "partial-two", Reason: ReasonIncludeNotMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
}

// TestInclude_MultiLabelOrderAndTagIrrelevance proves the AND/OR result does
// not depend on how the author writes the document: reordering the two
// conditions, shuffling the pairs inside a condition and the tags inside a
// cluster, writing candidates out of ID order, and adding unrelated labels
// must all yield byte-identical plans. A second, exact-comparison document
// pins that superficially similar keys/values ("ENV" vs "env", "edge" vs
// "Edge", "canary " with a trailing space) keep a half-hit a half-hit.
func TestInclude_MultiLabelOrderAndTagIrrelevance(t *testing.T) {
	base := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
  "include": [
    {"env": "prod", "tier": "edge"},
    {"region": "west", "track": "canary"}
  ],
  "clusters": [
    {"id": "hit-both", "tags": {"env": "prod", "tier": "edge", "region": "west", "track": "canary"}},
    {"id": "cross-pairs", "tags": {"env": "prod", "region": "west"}},
    {"id": "hit-one", "tags": {"env": "prod", "tier": "edge", "team": "payments"}},
    {"id": "partial-one", "tags": {"env": "prod"}},
    {"id": "partial-two", "tags": {"region": "west"}},
    {"id": "hit-two", "tags": {"region": "west", "track": "canary"}}
  ]
}`
	shuffled := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
  "include": [
    {"track": "canary", "region": "west"},
    {"tier": "edge", "env": "prod"}
  ],
  "clusters": [
    {"id": "hit-two", "tags": {"note": "early", "track": "canary", "region": "west"}},
    {"id": "partial-two", "tags": {"region": "west", "extra": "x"}},
    {"id": "partial-one", "tags": {"env": "prod", "extra": "x"}},
    {"id": "hit-one", "tags": {"team": "payments", "tier": "edge", "env": "prod"}},
    {"id": "cross-pairs", "tags": {"region": "west", "env": "prod", "extra": "x"}},
    {"id": "hit-both", "tags": {"track": "canary", "region": "west", "tier": "edge", "env": "prod"}}
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

	// Exact string comparison protects the boundary: none of the four clusters
	// below fully matches either condition, so all stay on the not-selected
	// side even though each carries labels that look right at a glance.
	doc := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
  "include": [
    {"env": "prod", "tier": "edge"},
    {"region": "west", "track": "canary"}
  ],
  "clusters": [
    {"id": "case-key", "tags": {"ENV": "prod", "tier": "edge"}},
    {"id": "case-tier", "tags": {"env": "prod", "tier": "Edge"}},
    {"id": "space-track", "tags": {"region": "west", "track": "canary "}},
    {"id": "cross-cased", "tags": {"env": "prod", "region": "West"}}
  ]
}`
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err == nil {
		t.Fatalf("plan must fail when every candidate only half-matches, got plan %+v", plan)
	}
	assertZeroReleasePlan(t, plan)
	rows := map[string]bool{}
	for _, line := range strings.Split(err.Error(), "\n")[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, reason, ok := strings.Cut(line, "：")
		if !ok || reason != ReasonIncludeNotMatched {
			t.Fatalf("unexpected rejection line %q in %q", line, err.Error())
		}
		rows[id] = true
	}
	for _, id := range []string{"case-key", "case-tier", "space-track", "cross-cased"} {
		if !rows[id] {
			t.Fatalf("cluster %q must be rejected with %s, got %q", id, ReasonIncludeNotMatched, err.Error())
		}
	}
}

// TestInclude_EmptyStringValueDistinctFromMissingKey pins the missing-tag vs
// empty-value distinction for multi-label INCLUDE conditions: a condition
// demanding track="" is fully matched only by a cluster that CARRIES the key
// with that exact empty value (and matches the other pair too). A cluster
// without the key cannot match, even though a zero-value map lookup returns "".
func TestInclude_EmptyStringValueDistinctFromMissingKey(t *testing.T) {
	conds := []LabelCondition{{"env": "prod", "track": ""}}
	cases := map[string]struct {
		tags     map[string]string
		eligible bool
	}{
		"empty value full match": {
			tags:     map[string]string{"env": "prod", "track": ""},
			eligible: true,
		},
		"empty value full match with unrelated extra tag": {
			tags:     map[string]string{"env": "prod", "track": "", "team": "x"},
			eligible: true,
		},
		"key missing entirely": {
			tags:     map[string]string{"env": "prod"},
			eligible: false, // a missing key never equals a demanded ""
		},
		"empty value but the other key mismatches": {
			tags:     map[string]string{"env": "dev", "track": ""},
			eligible: false, // AND requires env=prod too
		},
		"empty value and other key missing": {
			tags:     map[string]string{"track": ""},
			eligible: false,
		},
		"non-empty value is not the empty string": {
			tags:     map[string]string{"env": "prod", "track": "canary"},
			eligible: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := ReleasePlanInput{
				App:       "app",
				Revision:  "r1",
				Image:     "img",
				BatchSize: 10,
				Include:   conds,
				// A companion cluster that fully matches keeps the plan
				// deliverable when c is rejected, so c's fate is observable
				// in the result rows rather than in an all-rejected error.
				Clusters: []Cluster{
					{ID: "c", Tags: tc.tags},
					{ID: "keep", Tags: map[string]string{"env": "prod", "track": ""}},
				},
			}
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatal(err)
			}
			rows := planRows(t, plan)
			rows.requireSelected(t, "keep")
			if tc.eligible {
				rows.requireSelected(t, "c")
			} else {
				rows.requireExcluded(t, "c", ReasonIncludeNotMatched)
			}
		})
	}

	// The JSON path agrees: present-but-empty matches, absent key does not.
	doc := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
  "include": [{"env": "prod", "track": ""}],
  "clusters": [
    {"id": "has-empty", "tags": {"env": "prod", "track": ""}},
    {"id": "no-key", "tags": {"env": "prod"}},
    {"id": "keeper", "tags": {"env": "prod", "track": ""}}
  ]
}`
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	rows := planRows(t, plan)
	rows.requireSelected(t, "has-empty")
	rows.requireSelected(t, "keeper")
	rows.requireExcluded(t, "no-key", ReasonIncludeNotMatched)
}

// TestInclude_PriorityWithDisabledAndExclude confirms the include filter can
// never restore eligibility that the earlier rules removed. The fixed priority
// is 停用 → 排除 → 包含:
//   - hit-off    fully satisfies an include condition but is disabled:
//     reason stays 集群已停用.
//   - hit-excl   fully satisfies an include condition AND a (multi-label)
//     exclude condition: exclude wins, reason 命中排除条件.
//   - hit-one    fully satisfies include condition 1 and is released.
//   - hit-two    fully satisfies include condition 2 and is released.
//   - cross-half satisfies one pair of each include condition: not eligible,
//     reason 未命中包含条件 — it must not slip in merely because it was not
//     excluded.
func TestInclude_PriorityWithDisabledAndExclude(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 10,
		Include:   multiIncludeConditions(),
		Exclude: []LabelCondition{
			{"quarantine": "true"},
			{"env": "temp", "scope": "public"},
		},
		Clusters: []Cluster{
			{ID: "hit-one", Tags: map[string]string{"env": "prod", "tier": "edge"}},
			{ID: "hit-two", Tags: map[string]string{"region": "west", "track": "canary"}},
			{ID: "hit-off", Disabled: true, Tags: map[string]string{
				"env": "prod", "tier": "edge",
			}},
			{ID: "hit-excl", Tags: map[string]string{
				"env": "prod", "tier": "edge", "quarantine": "true",
			}},
			{ID: "hit-excl2", Tags: map[string]string{
				"region": "west", "track": "canary",
				"env": "temp", "scope": "public",
			}},
			{ID: "cross-half", Tags: map[string]string{"env": "prod", "region": "west"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	rows := planRows(t, plan)
	rows.requireSelected(t, "hit-one")
	rows.requireSelected(t, "hit-two")
	rows.requireExcluded(t, "hit-off", ReasonDisabled)
	rows.requireExcluded(t, "hit-excl", ReasonExcludeMatched)
	rows.requireExcluded(t, "hit-excl2", ReasonExcludeMatched)
	rows.requireExcluded(t, "cross-half", ReasonIncludeNotMatched)

	wantExcluded := []ExcludedCluster{
		{ID: "cross-half", Reason: ReasonIncludeNotMatched},
		{ID: "hit-excl", Reason: ReasonExcludeMatched},
		{ID: "hit-excl2", Reason: ReasonExcludeMatched},
		{ID: "hit-off", Reason: ReasonDisabled},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
	wantBatches := []Batch{{Index: 1, Clusters: []string{"hit-one", "hit-two"}}}
	if !reflect.DeepEqual(plan.Batches, wantBatches) {
		t.Fatalf("batches = %+v, want %+v", plan.Batches, wantBatches)
	}
}

// TestInclude_SpreadByPartialHitsNeedNoFaultDomainTag combines the include
// filter with fault-domain batching. With spreadBy enabled, the clusters that
// only partially hit the include conditions — including one that takes one
// pair from each — carry NO zone tag. They must not prevent the fully
// matching, tag-complete clusters from being scheduled, and the plan must not
// fail just because unselected clusters lack the fault-domain tag.
//
// Selected clusters: s1/s2 (condition 1, domain z1) and s3 (condition 2,
// domain z2). With batchSize 2, s1 and s2 share z1 so s2 is deferred:
// batch 1 [s1 s3], batch 2 [s2].
func TestInclude_SpreadByPartialHitsNeedNoFaultDomainTag(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Include:   multiIncludeConditions(),
		Clusters: []Cluster{
			{ID: "s2", Tags: map[string]string{"env": "prod", "tier": "edge", "zone": "z1"}},
			{ID: "p1", Tags: map[string]string{"env": "prod"}},
			{ID: "s1", Tags: map[string]string{"zone": "z1", "tier": "edge", "env": "prod"}},
			{ID: "pc", Tags: map[string]string{"env": "prod", "region": "west"}},
			{ID: "p2", Tags: map[string]string{"region": "west"}},
			{ID: "s3", Tags: map[string]string{"region": "west", "track": "canary", "zone": "z2"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("unselected clusters need no fault-domain tag: %v", err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "s1,s3|s2" {
		t.Fatalf("spread batches = %v, want [s1 s3]|[s2]", got)
	}
	rows := planRows(t, plan)
	for _, id := range []string{"s1", "s2", "s3"} {
		rows.requireSelected(t, id)
	}
	for _, id := range []string{"p1", "p2", "pc"} {
		rows.requireExcluded(t, id, ReasonIncludeNotMatched)
	}
	wantExcluded := []ExcludedCluster{
		{ID: "p1", Reason: ReasonIncludeNotMatched},
		{ID: "p2", Reason: ReasonIncludeNotMatched},
		{ID: "pc", Reason: ReasonIncludeNotMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
}

// TestInclude_SpreadBySelectedMissingTagFailsWholePlan is the inverse failure:
// a cluster that FULLY matches an include condition (so it is selected) but
// lacks the fault-domain tag must fail the whole plan under the existing rule,
// even though other fully matching clusters are tag-complete. The returned
// plan is the zero value — no already-computed partial batches or excluded
// list may leak — and the error names the missing tag and the smallest
// offending selected cluster. The half-hit clusters (which also lack the tag)
// must not be reported instead.
func TestInclude_SpreadBySelectedMissingTagFailsWholePlan(t *testing.T) {
	build := func(order string) ReleasePlanInput {
		in := ReleasePlanInput{
			App:       "app",
			Revision:  "r1",
			Image:     "img",
			BatchSize: 2,
			SpreadBy:  "zone",
			Include:   multiIncludeConditions(),
		}
		// s-b fully matches condition 1 but carries no zone tag; s-a and s-c
		// are fully matching and tag-complete. h1/hc only half-match, also
		// without the tag, and must be irrelevant to this failure.
		candidates := map[string][]Cluster{
			"a": {
				{ID: "s-a", Tags: map[string]string{"env": "prod", "tier": "edge", "zone": "z1"}},
				{ID: "h1", Tags: map[string]string{"env": "prod"}},
				{ID: "s-b", Tags: map[string]string{"env": "prod", "tier": "edge"}},
				{ID: "hc", Tags: map[string]string{"env": "prod", "region": "west"}},
				{ID: "s-c", Tags: map[string]string{"region": "west", "track": "canary", "zone": "z2"}},
			},
			"b": {
				{ID: "hc", Tags: map[string]string{"region": "west", "env": "prod"}},
				{ID: "s-c", Tags: map[string]string{"zone": "z2", "track": "canary", "region": "west"}},
				{ID: "s-b", Tags: map[string]string{"tier": "edge", "env": "prod"}},
				{ID: "h1", Tags: map[string]string{"env": "prod"}},
				{ID: "s-a", Tags: map[string]string{"zone": "z1", "tier": "edge", "env": "prod"}},
			},
		}
		in.Clusters = candidates[order]
		return in
	}
	const wantErr = `集群 "s-b" 缺少故障域标签 "zone"`
	for _, order := range []string{"a", "b"} {
		t.Run("order-"+order, func(t *testing.T) {
			plan, err := MakeReleasePlan(build(order))
			if err == nil {
				t.Fatal("expected a non-nil error for the selected cluster's missing tag")
			}
			assertZeroReleasePlan(t, plan)
			if err.Error() != wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), wantErr)
			}
			msg := err.Error()
			if strings.Contains(msg, "h1") || strings.Contains(msg, "hc") {
				t.Fatalf("half-hit clusters must not preempt the missing-tag error, got %q", msg)
			}
			if strings.Contains(msg, ReasonIncludeNotMatched) {
				t.Fatalf("the missing-tag error must not be replaced by filter reasons, got %q", msg)
			}
		})
	}
}

// TestInclude_AllCandidatesOnlyPartiallyMatchFails covers the failure path
// specific to multi-label include: when every candidate satisfies only some
// of the demanded labels (one pair of a condition, or a pair from each of two
// conditions) the planner must FAIL, not succeed with an empty batch list.
// The returned plan is the zero value, and the error lists every candidate
// ascending by ID with 未命中包含条件 exactly once.
//
// spreadBy is deliberately enabled while none of these clusters carries the
// zone tag: once selection yields no eligible cluster the filter failure must
// win, and must not be replaced by a missing-fault-domain-tag error.
func TestInclude_AllCandidatesOnlyPartiallyMatchFails(t *testing.T) {
	build := func(order string) ReleasePlanInput {
		in := ReleasePlanInput{
			App:       "app",
			Revision:  "r1",
			Image:     "img",
			BatchSize: 2,
			SpreadBy:  "zone",
			Include:   multiIncludeConditions(),
		}
		// Ascending by ID the candidates are:
		// i-cross  env=prod + region=west: half of condition 1 AND half of
		//          condition 2 — the cross-condition stitching trap.
		// i-one    env=prod only: half of condition 1.
		// i-three  region=west only: half of condition 2.
		// i-two    track=canary only: the other half of condition 2 alone.
		candidates := map[string][]Cluster{
			"a": {
				{ID: "i-one", Tags: map[string]string{"env": "prod"}},
				{ID: "i-cross", Tags: map[string]string{"env": "prod", "region": "west"}},
				{ID: "i-two", Tags: map[string]string{"track": "canary"}},
				{ID: "i-three", Tags: map[string]string{"region": "west"}},
			},
			"b": {
				{ID: "i-three", Tags: map[string]string{"region": "west"}},
				{ID: "i-one", Tags: map[string]string{"env": "prod"}},
				{ID: "i-cross", Tags: map[string]string{"region": "west", "env": "prod"}},
				{ID: "i-two", Tags: map[string]string{"track": "canary"}},
			},
		}
		in.Clusters = candidates[order]
		return in
	}
	want := allRejectedLibraryError(
		"i-cross："+ReasonIncludeNotMatched,
		"i-one："+ReasonIncludeNotMatched,
		"i-three："+ReasonIncludeNotMatched,
		"i-two："+ReasonIncludeNotMatched,
	)
	for _, order := range []string{"a", "b"} {
		t.Run("order-"+order, func(t *testing.T) {
			plan, err := MakeReleasePlan(build(order))
			if err == nil {
				t.Fatalf("half-matches must not produce a plan, got %+v", plan)
			}
			assertZeroReleasePlan(t, plan)
			if err.Error() != want {
				t.Fatalf("rejection error mismatch:\n got %q\nwant %q", err.Error(), want)
			}
			msg := err.Error()
			if strings.Contains(msg, "故障域") || strings.Contains(msg, "缺少") {
				t.Fatalf("filter failure must not be masked by the fault-domain tag check, got %q", msg)
			}
			for _, id := range []string{"i-cross", "i-one", "i-three", "i-two"} {
				if strings.Count(msg, "\n  "+id+"：") != 1 {
					t.Fatalf("candidate %q must appear exactly once in %q", id, msg)
				}
			}
		})
	}
}

// TestInclude_PartialHitsCombinedWithOtherReasons keeps the all-rejected error
// honest when the half-hits share the plan with disabled and excluded
// candidates: each reason keeps its fixed-priority value, the include
// half-hits stay 未命中包含条件, and the whole list is ascending by ID. A
// cluster that fully matches include but is excluded must not be selected.
func TestInclude_PartialHitsCombinedWithOtherReasons(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		Include:   multiIncludeConditions(),
		Exclude:   []LabelCondition{{"quarantine": "true"}},
		Clusters: []Cluster{
			{ID: "r-off", Disabled: true, Tags: map[string]string{
				"env": "prod", "tier": "edge",
			}},
			{ID: "r-half", Tags: map[string]string{"env": "prod"}},
			{ID: "r-excl", Tags: map[string]string{
				"region": "west", "track": "canary", "quarantine": "true",
			}},
			{ID: "r-cross", Tags: map[string]string{"env": "prod", "region": "west"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatalf("every candidate loses eligibility; expected failure, got %+v", plan)
	}
	assertZeroReleasePlan(t, plan)
	want := allRejectedLibraryError(
		"r-cross："+ReasonIncludeNotMatched,
		"r-excl："+ReasonExcludeMatched,
		"r-half："+ReasonIncludeNotMatched,
		"r-off："+ReasonDisabled,
	)
	if err.Error() != want {
		t.Fatalf("rejection error mismatch:\n got %q\nwant %q", err.Error(), want)
	}
}
