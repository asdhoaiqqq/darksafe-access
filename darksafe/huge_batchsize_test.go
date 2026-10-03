package darksafe

import (
	"strings"
	"testing"
)

// A huge but legal batchSize (users set e.g. 1000000000 to mean "capacity is
// no constraint") must not make planning allocate memory proportional to that
// number: resource use depends on the actual cluster count. The spreadBy
// rules still apply unchanged — a huge capacity does not merge conflicting
// fault domains into one batch.
func TestPlan_HugeBatchSizeWithSpreadBy(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: 1000000000,
		SpreadBy:  "zone",
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 2 {
		t.Fatalf("expected 2 batches, got %+v", plan.Batches)
	}
	if strings.Join(plan.Batches[0].Clusters, ",") != "a,c" || plan.Batches[0].Index != 1 {
		t.Fatalf("batch 1 = %+v, want index 1 with [a c]", plan.Batches[0])
	}
	if strings.Join(plan.Batches[1].Clusters, ",") != "b" || plan.Batches[1].Index != 2 {
		t.Fatalf("batch 2 = %+v, want index 2 with [b]", plan.Batches[1])
	}
}

// When every selected cluster sits in the same fault domain, even an enormous
// capacity still yields one cluster per batch: spreadBy is not overridden by
// batchSize.
func TestPlan_HugeBatchSizeSingleDomainStillSpreads(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: 1000000000,
		SpreadBy:  "zone",
		Clusters: []Cluster{
			{ID: "c1", Tags: map[string]string{"zone": "east"}},
			{ID: "c2", Tags: map[string]string{"zone": "east"}},
			{ID: "c3", Tags: map[string]string{"zone": "east"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 3 {
		t.Fatalf("expected 3 batches of one cluster each, got %+v", plan.Batches)
	}
	for i, b := range plan.Batches {
		if b.Index != i+1 || len(b.Clusters) != 1 {
			t.Fatalf("batch %d = %+v, want index %d with exactly one cluster", i+1, b, i+1)
		}
	}
}

// Two capacities that both cover the selected set produce the identical full
// plan: batchSize stays an upper bound, not an input to allocation or
// scheduling heuristics.
func TestPlan_LargeCapacitiesGiveSamePlan(t *testing.T) {
	clusters := []Cluster{
		{ID: "a", Tags: map[string]string{"zone": "east"}},
		{ID: "b", Tags: map[string]string{"zone": "east"}},
		{ID: "c", Tags: map[string]string{"zone": "west"}},
	}
	var first string
	for _, size := range []int{3, 1000000000} {
		plan, err := MakeReleasePlan(ReleasePlanInput{
			App: "app", Revision: "r1", Image: "img",
			BatchSize: size, SpreadBy: "zone", Clusters: clusters,
		})
		if err != nil {
			t.Fatal(err)
		}
		summary := planSummary(plan)
		if first == "" {
			first = summary
		} else if summary != first {
			t.Fatalf("batchSize %d changed the plan:\n got %s\nwant %s", size, summary, first)
		}
	}
}

// The same huge capacity without spreadBy keeps the legacy behavior: one
// batch holding every selected cluster in ascending order.
func TestPlan_HugeBatchSizeWithoutSpreadBy(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: 1000000000,
		Clusters: []Cluster{
			{ID: "c3"}, {ID: "c1"}, {ID: "c2"},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c1,c2,c3" {
		t.Fatalf("expected a single batch [c1 c2 c3], got %+v", plan.Batches)
	}
}

// Through the CLI a huge batchSize plans successfully: exit 0, the full plan
// on stdout, nothing on stderr.
func TestPlanCLI_HugeBatchSizeWithSpreadByPlans(t *testing.T) {
	doc := `{
	  "app": "app", "revision": "r1", "image": "img",
	  "batchSize": 1000000000, "spreadBy": "zone",
	  "clusters": [
	    {"id": "a", "tags": {"zone": "east"}},
	    {"id": "b", "tags": {"zone": "east"}},
	    {"id": "c", "tags": {"zone": "west"}}
	  ]
	}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	domains := map[string]string{"a": "east", "b": "east", "c": "west"}
	assertSpreadBatches(t, p, [][]string{{"a", "c"}, {"b"}}, domains, 1000000000)
}

// A huge capacity must not mask the existing failure condition: a selected
// cluster missing the spreadBy tag still fails the plan, naming the smallest
// offending ID and the tag.
func TestPlanCLI_HugeBatchSizeMissingTagStillFails(t *testing.T) {
	doc := `{
	  "app": "app", "revision": "r1", "image": "img",
	  "batchSize": 1000000000, "spreadBy": "zone",
	  "clusters": [
	    {"id": "c-a", "tags": {"zone": "east"}},
	    {"id": "c-b"}
	  ]
	}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q", stdout)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "c-b") || !strings.Contains(stderr, "zone") {
		t.Fatalf("stderr must name cluster c-b and tag zone, got %q", stderr)
	}
}
