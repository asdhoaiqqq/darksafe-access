package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// This file is the automated regression net for multi-label EXCLUDE conditions
// in the offline release plan. The distinction it protects is the AND/OR
// structure of label matching (also documented in README "标签条件怎么匹配"):
//
//   - Within one condition object every key-value pair must match (AND): a
//     cluster is excluded by {"env":"prod","quarantine":"true"} only when it
//     carries BOTH pairs. Merely sharing one label is not a hit.
//   - Across the exclude array ANY one fully matched condition excludes (OR):
//     pairs satisfied in different conditions must never be stitched together
//     into a single hit.
//
// Every assertion is written against the published result of MakeReleasePlan
// — batches, the excluded list and, on the all-rejected path, the error —
// never against the internal matching helpers.

// multiExcludeConditions are the two two-label exclude conditions used
// throughout this file: each must be satisfied wholesale.
func multiExcludeConditions() []LabelCondition {
	return []LabelCondition{
		{"env": "prod", "quarantine": "true"},
		{"region": "west", "tier": "edge"},
	}
}

// exclusionRows reports, for every candidate, whether it appears in any batch
// and — if it does not — its exclusion reason. A candidate is present at most
// once either way.
type exclusionRows map[string]struct {
	Selected bool
	Reason   string
}

func planRows(t *testing.T, plan ReleasePlan) exclusionRows {
	t.Helper()
	rows := exclusionRows{}
	for _, b := range plan.Batches {
		for _, id := range b.Clusters {
			if _, dup := rows[id]; dup {
				t.Fatalf("cluster %q appears more than once in the plan", id)
			}
			rows[id] = struct {
				Selected bool
				Reason   string
			}{Selected: true}
		}
	}
	for _, e := range plan.Excluded {
		if _, dup := rows[e.ID]; dup {
			t.Fatalf("cluster %q is both batched and recorded as excluded", e.ID)
		}
		rows[e.ID] = struct {
			Selected bool
			Reason   string
		}{Reason: e.Reason}
	}
	return rows
}

func (r exclusionRows) requireSelected(t *testing.T, id string) {
	t.Helper()
	row, ok := r[id]
	if !ok {
		t.Fatalf("cluster %q missing from the whole plan (neither batched nor excluded)", id)
	}
	if !row.Selected {
		t.Fatalf("cluster %q must be released, but is excluded: %s", id, row.Reason)
	}
}

func (r exclusionRows) requireExcluded(t *testing.T, id, reason string) {
	t.Helper()
	row, ok := r[id]
	if !ok {
		t.Fatalf("cluster %q missing from the whole plan (neither batched nor excluded)", id)
	}
	if row.Selected {
		t.Fatalf("cluster %q must be excluded (%s), but is batched", id, reason)
	}
	if row.Reason != reason {
		t.Fatalf("cluster %q reason = %q, want %q", id, row.Reason, reason)
	}
}

// TestExclude_MultiLabelConditionRequiresAllPairs is the headline AND/OR
// regression. The two exclude conditions are
// {"env":"prod","quarantine":"true"} and {"region":"west","tier":"edge"}.
//
//   - partial-one  satisfies only env=prod of condition 1: no full hit.
//   - cross-pairs  satisfies env=prod (condition 1) and region=west
//     (condition 2) but neither quarantine=true nor tier=edge:
//     the two half-hits live in different conditions and must not
//     be combined into one exclusion.
//   - hit-one      fully satisfies condition 1 (plus an unrelated label).
//   - hit-two      fully satisfies condition 2.
//   - hit-both     fully satisfies BOTH conditions: still a single exclusion
//     record, reason 命中排除条件.
//   - unrelated    matches nothing either way and is released.
//
// Extra, unrelated tags must not change any of these outcomes; keys and values
// stay exact strings.
func TestExclude_MultiLabelConditionRequiresAllPairs(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 10,
		Exclude:   multiExcludeConditions(),
		Clusters: []Cluster{
			{ID: "partial-one", Tags: map[string]string{"env": "prod"}},
			{ID: "cross-pairs", Tags: map[string]string{"env": "prod", "region": "west"}},
			{ID: "hit-one", Tags: map[string]string{
				"env": "prod", "quarantine": "true", "team": "payments",
			}},
			{ID: "hit-two", Tags: map[string]string{
				"region": "west", "tier": "edge", "note": "canary",
			}},
			{ID: "hit-both", Tags: map[string]string{
				"env": "prod", "quarantine": "true",
				"region": "west", "tier": "edge",
			}},
			{ID: "unrelated", Tags: map[string]string{"env": "dev", "region": "east"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}

	rows := planRows(t, plan)
	rows.requireSelected(t, "partial-one")
	rows.requireSelected(t, "cross-pairs")
	rows.requireSelected(t, "unrelated")
	rows.requireExcluded(t, "hit-one", ReasonExcludeMatched)
	rows.requireExcluded(t, "hit-two", ReasonExcludeMatched)
	rows.requireExcluded(t, "hit-both", ReasonExcludeMatched)

	if len(plan.Excluded) != 3 {
		t.Fatalf("excluded list = %+v, want exactly the three full hits", plan.Excluded)
	}
	// Unselected records keep the existing convention: ascending by ID, and a
	// cluster fully matching two conditions still occurs exactly once.
	wantExcluded := []ExcludedCluster{
		{ID: "hit-both", Reason: ReasonExcludeMatched},
		{ID: "hit-one", Reason: ReasonExcludeMatched},
		{ID: "hit-two", Reason: ReasonExcludeMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}

	// Selected clusters keep the existing scheduling convention: ascending by
	// ID in one batch; no excluded ID may leak into any batch.
	wantBatches := []Batch{{Index: 1, Clusters: []string{
		"cross-pairs", "partial-one", "unrelated",
	}}}
	if !reflect.DeepEqual(plan.Batches, wantBatches) {
		t.Fatalf("batches = %+v, want %+v", plan.Batches, wantBatches)
	}
}

// TestExclude_MultiLabelOrderAndTagIrrelevance proves the AND/OR result does
// not depend on how the author writes the document: reordering the conditions,
// shuffling the pairs inside a condition and the tags inside a cluster, and
// adding unrelated labels must all yield byte-identical plans. This also pins
// exact key/value comparison ("ENV" vs "env", "true" vs " true", "west" vs
// "West") through the half-hit cases.
func TestExclude_MultiLabelOrderAndTagIrrelevance(t *testing.T) {
	base := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
  "exclude": [
    {"env": "prod", "quarantine": "true"},
    {"region": "west", "tier": "edge"}
  ],
  "clusters": [
    {"id": "hit-both", "tags": {"env": "prod", "quarantine": "true", "region": "west", "tier": "edge"}},
    {"id": "cross-pairs", "tags": {"env": "prod", "region": "west"}},
    {"id": "hit-one", "tags": {"env": "prod", "quarantine": "true", "team": "payments"}},
    {"id": "partial-one", "tags": {"env": "prod"}},
    {"id": "unrelated", "tags": {"env": "dev", "region": "east"}}
  ]
}`
	shuffled := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
  "exclude": [
    {"tier": "edge", "region": "west"},
    {"quarantine": "true", "env": "prod"}
  ],
  "clusters": [
    {"id": "unrelated", "tags": {"region": "east", "env": "dev", "extra": "x"}},
    {"id": "partial-one", "tags": {"env": "prod", "extra": "x"}},
    {"id": "hit-one", "tags": {"team": "payments", "quarantine": "true", "env": "prod"}},
    {"id": "cross-pairs", "tags": {"region": "west", "env": "prod", "extra": "x"}},
    {"id": "hit-both", "tags": {"tier": "edge", "region": "west", "quarantine": "true", "env": "prod"}}
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
  "exclude": [
    {"env": "prod", "quarantine": "true"},
    {"region": "west", "tier": "edge"}
  ],
  "clusters": [
    {"id": "case-key", "tags": {"ENV": "prod", "quarantine": "true"}},
    {"id": "case-val-env", "tags": {"env": "prod", "region": "West"}},
    {"id": "space-val", "tags": {"env": "prod", "quarantine": " true"}},
    {"id": "tier-diff", "tags": {"region": "west", "tier": "edge ", "env": "prod"}}
  ]
}`
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	rows := planRows(t, plan)
	for _, id := range []string{"case-key", "case-val-env", "space-val", "tier-diff"} {
		rows.requireSelected(t, id)
	}
}

// TestExclude_EmptyStringValueDistinctFromMissingKey pins the missing-tag vs
// empty-value distinction for multi-label EXCLUDE conditions: a condition
// demanding quarantine="" is fully matched only by a cluster that CARRIES the
// key with that exact empty value; a cluster without the key cannot match, even
// though a zero-value map lookup would return "". The condition's other key
// still has to match as well.
func TestExclude_EmptyStringValueDistinctFromMissingKey(t *testing.T) {
	conds := []LabelCondition{{"quarantine": "", "env": "prod"}}
	cases := map[string]struct {
		tags   map[string]string
		reason string // "" means selected
	}{
		"empty value full match": {
			tags:   map[string]string{"env": "prod", "quarantine": ""},
			reason: ReasonExcludeMatched,
		},
		"empty value with unrelated extra tag full match": {
			tags:   map[string]string{"env": "prod", "quarantine": "", "team": "x"},
			reason: ReasonExcludeMatched,
		},
		"key missing entirely": {
			tags:   map[string]string{"env": "prod"},
			reason: "", // selected: a missing key never equals a demanded ""
		},
		"empty value but the other key mismatches": {
			tags:   map[string]string{"env": "dev", "quarantine": ""},
			reason: "", // selected: AND requires env=prod too
		},
		"empty value and other key missing": {
			tags:   map[string]string{"quarantine": ""},
			reason: "",
		},
		"non-empty value is not the empty string": {
			tags:   map[string]string{"env": "prod", "quarantine": "false"},
			reason: "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := ReleasePlanInput{
				App:       "app",
				Revision:  "r1",
				Image:     "img",
				BatchSize: 10,
				Exclude:   conds,
				// A companion cluster keeps the plan deliverable when c is
				// excluded, so c's fate is observable in the result rows.
				Clusters: []Cluster{
					{ID: "c", Tags: tc.tags},
					{ID: "keep", Tags: map[string]string{"env": "dev"}},
				},
			}
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatal(err)
			}
			rows := planRows(t, plan)
			rows.requireSelected(t, "keep")
			if tc.reason == "" {
				rows.requireSelected(t, "c")
			} else {
				rows.requireExcluded(t, "c", tc.reason)
			}
		})
	}

	// The JSON path agrees: present-but-empty excludes, absent key does not.
	doc := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 10,
  "exclude": [{"env": "prod", "quarantine": ""}],
  "clusters": [
    {"id": "has-empty", "tags": {"env": "prod", "quarantine": ""}},
    {"id": "no-key", "tags": {"env": "prod"}}
  ]
}`
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	rows := planRows(t, plan)
	rows.requireExcluded(t, "has-empty", ReasonExcludeMatched)
	rows.requireSelected(t, "no-key")
}

// TestExclude_PriorityWithIncludeAndDisabled protects how the exclude
// decision interacts with the surrounding rules (priority 停用 → 排除 → 包含):
//   - both       matches an include condition AND a multi-label exclude
//     condition: exclude wins;
//   - off-hit    is disabled AND fully satisfies an exclude condition: the
//     reason stays 集群已停用, never 命中排除条件;
//   - not-inc    matches no exclude condition (only env=prod of the first
//     one) and also fails the include filter: it must not gain
//     release eligibility merely because exclusion did not hit —
//     its reason is 未命中包含条件;
//   - selected   passes both filters and is the sole released cluster.
func TestExclude_PriorityWithIncludeAndDisabled(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 10,
		Include:   []LabelCondition{{"env": "prod"}},
		Exclude:   multiExcludeConditions(),
		Clusters: []Cluster{
			{ID: "selected", Tags: map[string]string{"env": "prod", "region": "east"}},
			{ID: "both", Tags: map[string]string{
				"env": "prod", "quarantine": "true",
			}},
			{ID: "off-hit", Disabled: true, Tags: map[string]string{
				"env": "prod", "quarantine": "true",
			}},
			{ID: "not-inc", Tags: map[string]string{"env": "dev", "region": "west"}},
			{ID: "cross-pairs", Tags: map[string]string{"env": "prod", "region": "west"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	rows := planRows(t, plan)
	rows.requireSelected(t, "selected")
	rows.requireSelected(t, "cross-pairs") // env=prod satisfies include; no full exclude hit
	rows.requireExcluded(t, "both", ReasonExcludeMatched)
	rows.requireExcluded(t, "off-hit", ReasonDisabled)
	rows.requireExcluded(t, "not-inc", ReasonIncludeNotMatched)

	wantExcluded := []ExcludedCluster{
		{ID: "both", Reason: ReasonExcludeMatched},
		{ID: "not-inc", Reason: ReasonIncludeNotMatched},
		{ID: "off-hit", Reason: ReasonDisabled},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
}

// TestExclude_AllCandidatesLoseEligibilityReturnsZeroPlan covers the complete
// failure result under multi-label exclude conditions: when every candidate
// loses eligibility the call returns a non-nil error and the zero-value plan,
// and the error preserves every candidate's reason exactly once, ascending by
// ID. Half-hits are deliberately included so that a broken AND (excluding on a
// single matching label) would change both the error and the plan: under the
// correct semantics the half-hit clusters are released through the include
// path here, so to force the all-rejected path they fail the include filter
// instead.
func TestExclude_AllCandidatesLoseEligibilityReturnsZeroPlan(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		Include:   []LabelCondition{{"env": "prod"}},
		Exclude:   multiExcludeConditions(),
		Clusters: []Cluster{
			// Sorted by ID the candidates are:
			// e-cross  env=prod + region=west: half of EACH exclude condition
			//          only. Include says env=prod → released, so exclude it
			//          from this scenario by env=dev instead; it then fails
			//          include with no full exclude hit.
			{ID: "e-cross", Tags: map[string]string{"env": "dev", "region": "west"}},
			{ID: "e-hit1", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "e-hit2", Tags: map[string]string{"region": "west", "tier": "edge"}},
			{ID: "e-off", Disabled: true, Tags: map[string]string{
				"region": "west", "tier": "edge",
			}},
			{ID: "e-partial", Tags: map[string]string{"env": "dev"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error when every candidate loses eligibility")
	}
	assertZeroReleasePlan(t, plan)

	msg := err.Error()
	want := allRejectedLibraryError(
		"e-cross："+ReasonIncludeNotMatched,
		"e-hit1："+ReasonExcludeMatched,
		"e-hit2："+ReasonExcludeMatched,
		"e-off："+ReasonDisabled,
		"e-partial："+ReasonIncludeNotMatched,
	)
	if msg != want {
		t.Fatalf("rejection error mismatch:\n got %q\nwant %q", msg, want)
	}
	// Every candidate must be accounted for exactly once.
	for _, id := range []string{"e-cross", "e-hit1", "e-hit2", "e-off", "e-partial"} {
		if strings.Count(msg, "\n  "+id+"：") != 1 {
			t.Fatalf("candidate %q must appear exactly once in %q", id, msg)
		}
	}
}

// TestExclude_UsableClustersStillProduceNormalPlan is the positive end-to-end
// guard: with available clusters around them, fully-hit clusters stay out of
// EVERY batch, the remaining clusters are scheduled with the existing
// ascending chunking convention, and exclusion records stay ascending by ID.
func TestExclude_UsableClustersStillProduceNormalPlan(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		Exclude:   multiExcludeConditions(),
		Clusters: []Cluster{
			{ID: "k1"},
			{ID: "k2", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "k3", Tags: map[string]string{"env": "prod"}},
			{ID: "k4", Tags: map[string]string{"region": "west", "tier": "edge"}},
			{ID: "k5", Tags: map[string]string{"env": "prod", "region": "west"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	wantBatches := []Batch{
		{Index: 1, Clusters: []string{"k1", "k3"}},
		{Index: 2, Clusters: []string{"k5"}},
	}
	if !reflect.DeepEqual(plan.Batches, wantBatches) {
		t.Fatalf("batches = %+v, want %+v", plan.Batches, wantBatches)
	}
	wantExcluded := []ExcludedCluster{
		{ID: "k2", Reason: ReasonExcludeMatched},
		{ID: "k4", Reason: ReasonExcludeMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}

	// Spread batching must exclude before fault domains matter: the two
	// excluded clusters carry no zone tag, yet the plan still succeeds.
	in.SpreadBy = "zone"
	for _, id := range []string{"k1", "k3", "k5"} {
		for i := range in.Clusters {
			if in.Clusters[i].ID == id {
				in.Clusters[i].Tags = setZone(in.Clusters[i].Tags, id)
			}
		}
	}
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("excluded clusters need no fault-domain tag: %v", err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "k1,k3|k5" {
		t.Fatalf("spread batches = %v, want [k1 k3]|[k5]", got)
	}
}

// setZone returns a copy of tags carrying a deterministic zone value per ID.
func setZone(tags map[string]string, id string) map[string]string {
	out := make(map[string]string, len(tags)+1)
	for k, v := range tags {
		out[k] = v
	}
	out["zone"] = "z-" + id
	return out
}
