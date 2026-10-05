package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// independentBatchesPlan builds a plain (no spreadBy) plan with the given
// candidate IDs and batch size.
func independentBatchesPlan(t *testing.T, ids []string, batchSize int) ReleasePlan {
	t.Helper()
	clusters := make([]Cluster, 0, len(ids))
	for _, id := range ids {
		clusters = append(clusters, Cluster{ID: id})
	}
	plan, err := MakeReleasePlan(ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: batchSize,
		Clusters:  clusters,
	})
	if err != nil {
		t.Fatalf("MakeReleasePlan: %v", err)
	}
	return plan
}

func TestPlan_BatchAppendDoesNotTouchOtherBatches(t *testing.T) {
	plan := independentBatchesPlan(t, []string{"c-a", "c-b", "c-c"}, 2)
	want := [][]string{{"c-a", "c-b"}, {"c-c"}}
	if got := batchIDs(plan); !reflect.DeepEqual(got, joinIDs(want)) {
		t.Fatalf("initial batches = %v, want %v", got, want)
	}

	// The caller takes the first batch's list, appends a note-only marker, and
	// saves it back. The second batch must keep its own member.
	plan.Batches[0].Clusters = append(plan.Batches[0].Clusters, "note-only")
	if got, w := plan.Batches[0].Clusters, []string{"c-a", "c-b", "note-only"}; !reflect.DeepEqual(got, w) {
		t.Fatalf("first batch = %v, want %v", got, w)
	}
	if got, w := plan.Batches[1].Clusters, []string{"c-c"}; !reflect.DeepEqual(got, w) {
		t.Fatalf("second batch = %v, want %v (must not be overwritten)", got, w)
	}
	if plan.Batches[1].Index != 2 {
		t.Fatalf("second batch index = %d, want 2", plan.Batches[1].Index)
	}
}

func TestPlan_MiddleBatchAppendLeavesOthersIntact(t *testing.T) {
	plan := independentBatchesPlan(t, []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7"}, 2)
	want := [][]string{{"c1", "c2"}, {"c3", "c4"}, {"c5", "c6"}, {"c7"}}
	if got := batchIDs(plan); !reflect.DeepEqual(got, joinIDs(want)) {
		t.Fatalf("initial batches = %v, want %v", got, want)
	}

	// Reorganizing only a non-last middle batch must leave every other batch
	// (earlier and later) untouched.
	plan.Batches[1].Clusters = append(plan.Batches[1].Clusters, "note-only", "note-only-2")
	wantAfter := [][]string{
		{"c1", "c2"},
		{"c3", "c4", "note-only", "note-only-2"},
		{"c5", "c6"},
		{"c7"},
	}
	gotAfter := make([][]string, len(plan.Batches))
	for i, b := range plan.Batches {
		gotAfter[i] = b.Clusters
		if b.Index != i+1 {
			t.Fatalf("batch %d index = %d", i, b.Index)
		}
	}
	if !reflect.DeepEqual(gotAfter, wantAfter) {
		t.Fatalf("batches after middle append = %v, want %v", gotAfter, wantAfter)
	}
}

func TestPlan_LocalAppendBeforeSavingKeepsOthersIntact(t *testing.T) {
	plan := independentBatchesPlan(t, []string{"c-a", "c-b", "c-c", "c-d"}, 2)

	// The caller holds the first batch's list in a local variable and appends
	// to it before saving anything back. Other batches keep their data.
	local := append(append([]string(nil), plan.Batches[0].Clusters...), "note-only")
	if got, w := plan.Batches[1].Clusters, []string{"c-c", "c-d"}; !reflect.DeepEqual(got, w) {
		t.Fatalf("second batch = %v, want %v", got, w)
	}
	plan.Batches[0].Clusters = local
	if got, w := plan.Batches[0].Clusters, []string{"c-a", "c-b", "note-only"}; !reflect.DeepEqual(got, w) {
		t.Fatalf("first batch = %v, want %v", got, w)
	}
	if got, w := plan.Batches[1].Clusters, []string{"c-c", "c-d"}; !reflect.DeepEqual(got, w) {
		t.Fatalf("second batch = %v, want %v after save", got, w)
	}
}

func TestPlan_BatchAppendPreservesAppAndExcluded(t *testing.T) {
	in := ReleasePlanInput{
		App:       "payments",
		Revision:  "v1.2.3",
		Image:     "reg/payments:v1.2.3",
		BatchSize: 2,
		Clusters: []Cluster{
			{ID: "c-a"},
			{ID: "c-b"},
			{ID: "c-c"},
			{ID: "off", Disabled: true},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	plan.Batches[0].Clusters = append(plan.Batches[0].Clusters, "note-only")

	if plan.App != (AppInfo{Name: "payments", Revision: "v1.2.3", Image: "reg/payments:v1.2.3"}) {
		t.Fatalf("app info changed: %+v", plan.App)
	}
	if !reflect.DeepEqual(plan.Excluded, []ExcludedCluster{{ID: "off", Reason: ReasonDisabled}}) {
		t.Fatalf("excluded changed: %+v", plan.Excluded)
	}
	if got, w := plan.Batches[1].Clusters, []string{"c-c"}; !reflect.DeepEqual(got, w) {
		t.Fatalf("second batch = %v, want %v", got, w)
	}
}

func TestPlan_SpreadByBatchAppendDoesNotTouchOtherBatches(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  "zone",
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
			{ID: "d", Tags: map[string]string{"zone": "west"}},
			{ID: "e", Tags: map[string]string{"zone": "east"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a", "c"}, {"b", "d"}, {"e"}}
	if got := batchIDs(plan); !reflect.DeepEqual(got, joinIDs(want)) {
		t.Fatalf("initial batches = %v, want %v", got, want)
	}

	plan.Batches[0].Clusters = append(plan.Batches[0].Clusters, "note-only")
	wantAfter := [][]string{
		{"a", "c", "note-only"},
		{"b", "d"},
		{"e"},
	}
	gotAfter := make([][]string, len(plan.Batches))
	for i, b := range plan.Batches {
		gotAfter[i] = b.Clusters
	}
	if !reflect.DeepEqual(gotAfter, wantAfter) {
		t.Fatalf("spread batches after append = %v, want %v", gotAfter, wantAfter)
	}
}

func TestPlan_SingleBatchAndSingleClusterAllowLocalAppend(t *testing.T) {
	// One cluster.
	one := independentBatchesPlan(t, []string{"only"}, 2)
	if len(one.Batches) != 1 || !reflect.DeepEqual(one.Batches[0].Clusters, []string{"only"}) {
		t.Fatalf("single-cluster plan = %+v", one.Batches)
	}
	one.Batches[0].Clusters = append(one.Batches[0].Clusters, "note-only")
	if !reflect.DeepEqual(one.Batches[0].Clusters, []string{"only", "note-only"}) {
		t.Fatalf("single-cluster batch after append = %v", one.Batches[0].Clusters)
	}

	// Single batch containing several clusters (batchSize covers all).
	single := independentBatchesPlan(t, []string{"a", "b", "c"}, 5)
	if len(single.Batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(single.Batches))
	}
	single.Batches[0].Clusters = append(single.Batches[0].Clusters, "n1", "n2")
	if !reflect.DeepEqual(single.Batches[0].Clusters, []string{"a", "b", "c", "n1", "n2"}) {
		t.Fatalf("single batch after append = %v", single.Batches[0].Clusters)
	}
}

func TestPlan_AppendedMarkersAreNotValidated(t *testing.T) {
	// batchSize bounds only the functional computation; the caller may append
	// any number of arbitrary markers with no business validation.
	plan := independentBatchesPlan(t, []string{"c-a", "c-b", "c-c"}, 2)
	markers := []string{"", "note-only", "c-a", "不存在的集群", "!!!", ""}
	plan.Batches[0].Clusters = append(plan.Batches[0].Clusters, markers...)
	if !reflect.DeepEqual(plan.Batches[0].Clusters, append([]string{"c-a", "c-b"}, markers...)) {
		t.Fatalf("first batch = %v", plan.Batches[0].Clusters)
	}
	if !reflect.DeepEqual(plan.Batches[1].Clusters, []string{"c-c"}) {
		t.Fatalf("second batch = %v, want [c-c]", plan.Batches[1].Clusters)
	}
}

func joinIDs(batches [][]string) []string {
	out := make([]string, len(batches))
	for i, b := range batches {
		out[i] = strings.Join(b, ",")
	}
	return out
}
