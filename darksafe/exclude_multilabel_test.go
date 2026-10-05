package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// This file is a regression net for the conjunction semantics of multi-label
// exclude conditions, driven through the public planning result
// (MakeReleasePlan / ParseReleaseInput) — never the internal matchers.
//
// The distinction under protection:
//
//   - One exclude condition can demand several labels; a cluster matches that
//     condition only when ALL of its key-value pairs match (logical AND).
//   - The exclude array is a logical OR: one FULLY matched condition excludes.
//     A cluster must never be excluded because a single label happens to fit,
//     nor may matching pieces of DISTINCT conditions be stitched into one hit.
//   - A cluster that fully matches two conditions still leaves the candidate
//     set exactly once: one excluded record, never two.
//
// Also pinned here: irrelevant tags change nothing; tag keys and values are
// exact-string comparisons (case and whitespace significant); a missing key is
// distinguished from a present-but-empty value; and the fixed
// disabled → exclude → include priority holds for multi-label conditions. The
// planning-result conventions — ascending scheduling order, ascending
// excluded-record order, chunk batching, and the zero-plan failure carrying
// every candidate's reason — are checked against the same semantics.

// multiLabelExcludeBase returns the two headline exclude conditions and the
// single-label include used across these tests:
//
//	exclude: {"env":"prod","quarantine":"true"}  OR  {"region":"west","tier":"edge"}
//	include: {"env":"prod"}
func multiLabelExcludeBase() (include []LabelCondition, exclude []LabelCondition) {
	return []LabelCondition{{"env": "prod"}},
		[]LabelCondition{{"env": "prod", "quarantine": "true"}, {"region": "west", "tier": "edge"}}
}

// selectedIDs reports the selected cluster IDs in scheduled (batch) order.
func selectedIDs(plan ReleasePlan) []string {
	var out []string
	for _, b := range plan.Batches {
		out = append(out, b.Clusters...)
	}
	return out
}

// excludedReasonMap maps each excluded cluster ID to its recorded reason.
func excludedReasonMap(plan ReleasePlan) map[string]string {
	m := map[string]string{}
	for _, e := range plan.Excluded {
		m[e.ID] = e.Reason
	}
	return m
}

// assertExcludedExactly fails unless plan.Excluded equals the wanted
// {id, reason} pairs in the given (ascending-ID) order. Because the comparison
// is positional and exact, a cluster recorded twice would fail it.
func assertExcludedExactly(t *testing.T, plan ReleasePlan, want []ExcludedCluster) {
	t.Helper()
	if !reflect.DeepEqual(plan.Excluded, want) {
		t.Fatalf("excluded records mismatch:\n got %+v\nwant %+v", plan.Excluded, want)
	}
}

// assertNotInAnyBatch fails if any of the given IDs appears in a batch.
func assertNotInAnyBatch(t *testing.T, plan ReleasePlan, ids ...string) {
	t.Helper()
	inBatch := map[string]bool{}
	for _, b := range plan.Batches {
		for _, id := range b.Clusters {
			inBatch[id] = true
		}
	}
	for _, id := range ids {
		if inBatch[id] {
			t.Fatalf("cluster %q must not appear in any batch: %+v", id, plan.Batches)
		}
	}
}

// TestExclude_MultiLabelConditionRequiresAllPairs is the headline regression
// for the AND/OR distinction. include is env=prod; exclude holds the two
// two-label conditions from multiLabelExcludeBase.
//
//   - partial1 {env:prod}: one key of cond1 fits — must NOT be excluded.
//   - partial2 {env:prod, region:west}: one key of EACH condition fits but
//     neither condition fully — pieces of distinct conditions must not be
//     stitched into a hit; selected.
//   - full1 {env:prod, quarantine:true}: fully matches cond1 → excluded.
//   - full2 {region:west, tier:edge}: fully matches cond2 but fails include —
//     exclude is checked first, so its reason is the exclude reason.
//   - both (all four tags): fully matches BOTH conditions → excluded exactly
//     once.
//   - irrelevant (cond1 tags + unrelated tags): excluded; extra tags neither
//     rescue nor alter the decision.
//   - disabled (cond1 tags, disabled): disabled wins over a full exclude hit.
//   - needinclude {region:west}: cond2 is only half-matched (no exclusion),
//     but env=prod is absent too — with no exclude hit, include still gates it,
//     so its reason is "未命中包含条件", never "命中排除条件".
//
// Candidates are intentionally listed out of ID order.
func TestExclude_MultiLabelConditionRequiresAllPairs(t *testing.T) {
	include, exclude := multiLabelExcludeBase()
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		Include: include, Exclude: exclude,
		Clusters: []Cluster{
			{ID: "both", Tags: map[string]string{"env": "prod", "quarantine": "true", "region": "west", "tier": "edge"}},
			{ID: "partial2", Tags: map[string]string{"env": "prod", "region": "west"}},
			{ID: "disabled", Disabled: true, Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "full1", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "needinclude", Tags: map[string]string{"region": "west"}},
			{ID: "partial1", Tags: map[string]string{"env": "prod"}},
			{ID: "irrelevant", Tags: map[string]string{"note": "x", "quarantine": "true", "env": "prod", "zone": "z9"}},
			{ID: "full2", Tags: map[string]string{"region": "west", "tier": "edge"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}

	// Ascending scheduling order; only the non-excluded, include-passing,
	// enabled clusters are selected.
	if got, want := selectedIDs(plan), []string{"partial1", "partial2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v; full plan = %+v", got, want, plan)
	}
	// Chunk batching by batchSize=2, in ascending order.
	if got, want := batchIDs(plan), []string{"partial1,partial2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	assertNotInAnyBatch(t, plan, "both", "full1", "full2", "irrelevant", "disabled", "needinclude")

	// Excluded records ascending by ID with the priority-dictated reasons.
	// "both" occurs exactly once despite matching both conditions.
	assertExcludedExactly(t, plan, []ExcludedCluster{
		{ID: "both", Reason: ReasonExcludeMatched},
		{ID: "disabled", Reason: ReasonDisabled},
		{ID: "full1", Reason: ReasonExcludeMatched},
		{ID: "full2", Reason: ReasonExcludeMatched},
		{ID: "irrelevant", Reason: ReasonExcludeMatched},
		{ID: "needinclude", Reason: ReasonIncludeNotMatched},
	})

	// Explicit count guard for the double-match cluster: one record, not two.
	var bothCount int
	for _, e := range plan.Excluded {
		if e.ID == "both" {
			bothCount++
		}
	}
	if bothCount != 1 {
		t.Fatalf("cluster matching both exclude conditions recorded %d times, want 1", bothCount)
	}
}

// TestExclude_PartialMatchAcrossConditionsCannotCombine is the focused
// "no stitching" case: the cluster satisfies one key of cond1 and one key of
// cond2, and the other half of each condition is held by different clusters.
// No half-match anywhere may turn into an exclusion.
func TestExclude_PartialMatchAcrossConditionsCannotCombine(t *testing.T) {
	_, exclude := multiLabelExcludeBase()
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 10,
		Exclude: exclude,
		Clusters: []Cluster{
			{ID: "cross", Tags: map[string]string{"env": "prod", "region": "west"}},
			{ID: "only-quarantine", Tags: map[string]string{"quarantine": "true"}},
			{ID: "only-tier", Tags: map[string]string{"tier": "edge"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selectedIDs(plan), []string{"cross", "only-quarantine", "only-tier"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no partial match may exclude: selected = %v, want %v", got, want)
	}
	if len(plan.Excluded) != 0 {
		t.Fatalf("expected no excluded records, got %+v", plan.Excluded)
	}
}

// TestExclude_EmptyStringValueIsPresentNotMissing protects the missing-key vs
// empty-value distinction for a single-key exclude: a condition demanding
// quarantine="" matches a cluster carrying that key with an empty value, but
// not a cluster lacking the key entirely, and not a non-empty value.
func TestExclude_EmptyStringValueIsPresentNotMissing(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 10,
		Exclude: []LabelCondition{{"quarantine": ""}},
		Clusters: []Cluster{
			{ID: "present-empty", Tags: map[string]string{"quarantine": ""}},
			{ID: "missing-key", Tags: map[string]string{}},
			{ID: "non-empty", Tags: map[string]string{"quarantine": "true"}},
			{ID: "other-key-only", Tags: map[string]string{"env": "prod"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selectedIDs(plan), []string{"missing-key", "non-empty", "other-key-only"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
	assertExcludedExactly(t, plan, []ExcludedCluster{
		{ID: "present-empty", Reason: ReasonExcludeMatched},
	})
}

// TestExclude_MultiLabelEmptyValueStillRequiresOtherKey extends the empty-value
// rule to a conjunction: when one required key matches an empty value, the
// OTHER key of the condition must still match — an empty-value hit on one key
// must not smuggle through a missing partner key.
func TestExclude_MultiLabelEmptyValueStillRequiresOtherKey(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 10,
		Exclude: []LabelCondition{{"env": "prod", "quarantine": ""}},
		Clusters: []Cluster{
			{ID: "both-empty-env", Tags: map[string]string{"env": "prod", "quarantine": ""}},
			{ID: "empty-but-env-missing", Tags: map[string]string{"quarantine": ""}},
			{ID: "env-ok-but-key-missing", Tags: map[string]string{"env": "prod"}},
			{ID: "env-ok-but-value-nonempty", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selectedIDs(plan), []string{"empty-but-env-missing", "env-ok-but-key-missing", "env-ok-but-value-nonempty"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
	assertExcludedExactly(t, plan, []ExcludedCluster{
		{ID: "both-empty-env", Reason: ReasonExcludeMatched},
	})
}

// TestExclude_DisabledWinsOverMatchedExclude pins the first priority rule with
// a multi-label exclude: a disabled cluster that fully matches an exclude
// condition (and the include) is recorded as disabled, never excluded.
func TestExclude_DisabledWinsOverMatchedExclude(t *testing.T) {
	include, exclude := multiLabelExcludeBase()
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 10,
		Include: include, Exclude: exclude,
		Clusters: []Cluster{
			{ID: "keep", Tags: map[string]string{"env": "prod"}},
			{ID: "off", Disabled: true, Tags: map[string]string{"env": "prod", "quarantine": "true", "region": "west", "tier": "edge"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selectedIDs(plan), []string{"keep"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
	assertExcludedExactly(t, plan, []ExcludedCluster{
		{ID: "off", Reason: ReasonDisabled},
	})
}

// TestExclude_IncludeAndExcludeBothMatchedExcludeWins pins the second priority
// rule with a multi-label exclude: a cluster satisfying the include and fully
// matching an exclude condition is excluded, while an include-only cluster
// still enters the plan.
func TestExclude_IncludeAndExcludeBothMatchedExcludeWins(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 10,
		Include: []LabelCondition{{"env": "prod"}},
		Exclude: []LabelCondition{{"env": "prod", "quarantine": "true"}},
		Clusters: []Cluster{
			{ID: "both", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "include-only", Tags: map[string]string{"env": "prod"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selectedIDs(plan), []string{"include-only"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
	assertExcludedExactly(t, plan, []ExcludedCluster{
		{ID: "both", Reason: ReasonExcludeMatched},
	})
}

// TestExclude_NoExcludeHitStillGatedByInclude shows that failing to match an
// exclude never grants release qualification on its own: the cluster still has
// to satisfy include. A half-matched exclude (region only, tier absent) leaves
// the cluster in play, where include then rejects it with the include reason.
func TestExclude_NoExcludeHitStillGatedByInclude(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 10,
		Include: []LabelCondition{{"env": "prod"}},
		Exclude: []LabelCondition{{"region": "west", "tier": "edge"}},
		Clusters: []Cluster{
			{ID: "gated", Tags: map[string]string{"region": "west"}},
			{ID: "ok", Tags: map[string]string{"env": "prod"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selectedIDs(plan), []string{"ok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
	assertExcludedExactly(t, plan, []ExcludedCluster{
		{ID: "gated", Reason: ReasonIncludeNotMatched},
	})
}

// TestExclude_ExactStringComparison drives a table of near-miss tag situations
// against one two-label exclude condition. Each case pairs the cluster under
// test with an anchor that always qualifies, so planning succeeds and the
// cluster's disposition can be read from the result. Any single non-exact
// key/value defeats the whole conjunction; an unrelated tag changes nothing.
func TestExclude_ExactStringComparison(t *testing.T) {
	cases := map[string]struct {
		tags map[string]string
		want string // "selected" or ReasonExcludeMatched
	}{
		"exact match":          {map[string]string{"env": "prod", "quarantine": "true"}, ReasonExcludeMatched},
		"value case different": {map[string]string{"env": "prod", "quarantine": "True"}, "selected"},
		"key case different":   {map[string]string{"env": "prod", "Quarantine": "true"}, "selected"},
		"env value case":       {map[string]string{"env": "Prod", "quarantine": "true"}, "selected"},
		"value leading space":  {map[string]string{"env": "prod", "quarantine": " true"}, "selected"},
		"env trailing space":   {map[string]string{"env": "prod ", "quarantine": "true"}, "selected"},
		"unrelated extra tags": {map[string]string{"env": "prod", "quarantine": "true", "zone": "", "note": "x"}, ReasonExcludeMatched},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := ReleasePlanInput{
				App: "app", Revision: "r1", Image: "img", BatchSize: 10,
				Exclude: []LabelCondition{{"env": "prod", "quarantine": "true"}},
				Clusters: []Cluster{
					{ID: "anchor", Tags: map[string]string{"env": "prod"}},
					{ID: "x", Tags: tc.tags},
				},
			}
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatal(err)
			}
			reasons := excludedReasonMap(plan)
			selected := map[string]bool{}
			for _, id := range selectedIDs(plan) {
				selected[id] = true
			}
			switch tc.want {
			case "selected":
				if !selected["x"] {
					t.Fatalf("x should be selected, excluded = %+v", plan.Excluded)
				}
				if _, ok := reasons["x"]; ok {
					t.Fatalf("x must have no excluded record, got %q", reasons["x"])
				}
			case ReasonExcludeMatched:
				if selected["x"] {
					t.Fatalf("x must not be selected")
				}
				if reasons["x"] != ReasonExcludeMatched {
					t.Fatalf("x reason = %q, want %q", reasons["x"], ReasonExcludeMatched)
				}
			}
		})
	}
}

// TestExclude_AllCandidatesLoseReturnsZeroPlanWithReasons covers the
// all-rejected failure with multi-label excludes: when every candidate loses
// release qualification, MakeReleasePlan returns a non-nil error AND the zero
// plan, and the error preserves EVERY candidate's reason exactly once in
// ascending ID order. A disabled double-match cluster reports disabled;
// half-matches that pass exclude but fail include report the include reason.
func TestExclude_AllCandidatesLoseReturnsZeroPlanWithReasons(t *testing.T) {
	include, exclude := multiLabelExcludeBase()
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		Include: include, Exclude: exclude,
		Clusters: []Cluster{
			{ID: "e6", Tags: map[string]string{"tier": "edge", "env": "prod", "region": "west", "quarantine": "true"}},
			{ID: "e4", Tags: map[string]string{"env": "dev"}},
			{ID: "e2", Tags: map[string]string{"region": "west", "tier": "edge"}},
			{ID: "e5", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "e1", Tags: map[string]string{"quarantine": "true", "env": "prod"}},
			{ID: "e3", Disabled: true, Tags: map[string]string{"env": "prod", "quarantine": "true", "region": "west", "tier": "edge"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error when every candidate loses qualification")
	}
	assertZeroReleasePlan(t, plan)

	wantErr := allRejectedLibraryError(
		"e1："+ReasonExcludeMatched,
		"e2："+ReasonExcludeMatched,
		"e3："+ReasonDisabled,
		"e4："+ReasonIncludeNotMatched,
		"e5："+ReasonExcludeMatched,
		"e6："+ReasonExcludeMatched,
	)
	if err.Error() != wantErr {
		t.Fatalf("error mismatch:\n got %q\nwant %q", err.Error(), wantErr)
	}
	// Every candidate appears in the error, each exactly once (one line each).
	msg := err.Error()
	for _, id := range []string{"e1", "e2", "e3", "e4", "e5", "e6"} {
		if strings.Count(msg, "\n  "+id+"：") != 1 {
			t.Fatalf("candidate %s must appear exactly once in error: %q", id, msg)
		}
	}
}

// TestExclude_JSONAndStructAgree drives the same multi-label configuration
// through both public entry points — a parsed JSON document and a directly
// constructed config — and requires byte-identical planning results, including
// batches and excluded records.
func TestExclude_JSONAndStructAgree(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 2,
  "include": [{"env": "prod"}],
  "exclude": [
    {"env": "prod", "quarantine": "true"},
    {"region": "west", "tier": "edge"}
  ],
  "clusters": [
    {"id": "full2", "tags": {"tier": "edge", "region": "west"}},
    {"id": "partial1", "tags": {"env": "prod"}},
    {"id": "both", "tags": {"region": "west", "tier": "edge", "env": "prod", "quarantine": "true"}},
    {"id": "partial2", "tags": {"region": "west", "env": "prod"}},
    {"id": "full1", "tags": {"quarantine": "true", "env": "prod"}}
  ]
}`
	fromJSON, err := MakeReleasePlan(parsePlan(t, doc))
	if err != nil {
		t.Fatal(err)
	}

	include, exclude := multiLabelExcludeBase()
	fromStruct, err := MakeReleasePlan(ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		Include: include, Exclude: exclude,
		Clusters: []Cluster{
			{ID: "full2", Tags: map[string]string{"tier": "edge", "region": "west"}},
			{ID: "partial1", Tags: map[string]string{"env": "prod"}},
			{ID: "both", Tags: map[string]string{"region": "west", "tier": "edge", "env": "prod", "quarantine": "true"}},
			{ID: "partial2", Tags: map[string]string{"region": "west", "env": "prod"}},
			{ID: "full1", Tags: map[string]string{"quarantine": "true", "env": "prod"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJSON, fromStruct) {
		t.Fatalf("JSON and struct results differ:\n JSON:   %+v\n struct: %+v", fromJSON, fromStruct)
	}

	// Shared expected outcome: two partial matches selected, chunked by 2;
	// three exclusions (both, full1, full2) ascending, "both" once.
	if got, want := batchIDs(fromStruct), []string{"partial1,partial2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	assertExcludedExactly(t, fromStruct, []ExcludedCluster{
		{ID: "both", Reason: ReasonExcludeMatched},
		{ID: "full1", Reason: ReasonExcludeMatched},
		{ID: "full2", Reason: ReasonExcludeMatched},
	})
}

// TestExclude_ResultIndependentOfMemberAndMapOrder proves conjunction matching
// does not depend on member order in the JSON document or on Go map iteration
// order: two documents with condition/tag members written in opposite orders
// give identical plans, and repeated planning of one in-memory config is stable
// across runs (each run re-iterates the condition maps).
func TestExclude_ResultIndependentOfMemberAndMapOrder(t *testing.T) {
	docA := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 2,
  "exclude": [
    {"env": "prod", "quarantine": "true"},
    {"region": "west", "tier": "edge"}
  ],
  "clusters": [
    {"id": "full1", "tags": {"env": "prod", "quarantine": "true"}},
    {"id": "partial", "tags": {"env": "prod", "region": "west"}}
  ]
}`
	docB := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 2,
  "exclude": [
    {"tier": "edge", "region": "west"},
    {"quarantine": "true", "env": "prod"}
  ],
  "clusters": [
    {"id": "partial", "tags": {"region": "west", "env": "prod"}},
    {"id": "full1", "tags": {"quarantine": "true", "env": "prod"}}
  ]
}`
	pa, err := MakeReleasePlan(parsePlan(t, docA))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := MakeReleasePlan(parsePlan(t, docB))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pa, pb) {
		t.Fatalf("member order changed the plan:\n a: %+v\n b: %+v", pa, pb)
	}
	if got, want := selectedIDs(pa), []string{"partial"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
	assertExcludedExactly(t, pa, []ExcludedCluster{
		{ID: "full1", Reason: ReasonExcludeMatched},
	})

	// In-memory config: condition maps are iterated afresh every call; the AND
	// verdict must be identical regardless of iteration order.
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		Exclude: []LabelCondition{{"quarantine": "true", "env": "prod"}},
		Clusters: []Cluster{
			{ID: "partial", Tags: map[string]string{"region": "west", "env": "prod"}},
			{ID: "full1", Tags: map[string]string{"quarantine": "true", "env": "prod"}},
		},
	}
	var firstPlan *ReleasePlan
	for range 5 {
		p, err := MakeReleasePlan(in)
		if err != nil {
			t.Fatal(err)
		}
		if firstPlan == nil {
			firstPlan = &p
			continue
		}
		if !reflect.DeepEqual(p, *firstPlan) {
			t.Fatalf("repeated planning gave different results:\n %+v\n %+v", *firstPlan, p)
		}
	}
}
