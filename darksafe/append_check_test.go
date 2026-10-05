package darksafe

import (
	"reflect"
	"testing"
)

func TestAppendToBatchDoesNotCorruptOthers(t *testing.T) {
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "img", BatchSize: 2,
		Clusters: []Cluster{{ID: "c-a"}, {ID: "c-b"}, {ID: "c-c"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// Append without saving back: other batches must be unaffected.
	_ = append(plan.Batches[0].Clusters, "note-only")
	if !reflect.DeepEqual(plan.Batches[1].Clusters, []string{"c-c"}) {
		t.Fatalf("batch 2 corrupted: %v", plan.Batches[1].Clusters)
	}
	// Append and save back.
	plan.Batches[0].Clusters = append(plan.Batches[0].Clusters, "note-only")
	if !reflect.DeepEqual(plan.Batches[0].Clusters, []string{"c-a", "c-b", "note-only"}) {
		t.Fatalf("batch 1 wrong: %v", plan.Batches[0].Clusters)
	}
	if !reflect.DeepEqual(plan.Batches[1].Clusters, []string{"c-c"}) {
		t.Fatalf("batch 2 corrupted: %v", plan.Batches[1].Clusters)
	}
}

func TestAppendToMiddleBatchOfThree(t *testing.T) {
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "img", BatchSize: 1,
		Clusters: []Cluster{{ID: "c-a"}, {ID: "c-b"}, {ID: "c-c"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	plan.Batches[1].Clusters = append(plan.Batches[1].Clusters, "note-only")
	if !reflect.DeepEqual(plan.Batches[0].Clusters, []string{"c-a"}) {
		t.Fatalf("batch 1 corrupted: %v", plan.Batches[0].Clusters)
	}
	if !reflect.DeepEqual(plan.Batches[2].Clusters, []string{"c-c"}) {
		t.Fatalf("batch 3 corrupted: %v", plan.Batches[2].Clusters)
	}
}

func TestAppendToSpreadBatchDoesNotCorruptOthers(t *testing.T) {
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "img", BatchSize: 2, SpreadBy: "zone",
		Clusters: []Cluster{
			{ID: "c-a", Tags: map[string]string{"zone": "z1"}},
			{ID: "c-b", Tags: map[string]string{"zone": "z1"}},
			{ID: "c-c", Tags: map[string]string{"zone": "z2"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	plan.Batches[0].Clusters = append(plan.Batches[0].Clusters, "note-only")
	want := []Batch{
		{Index: 1, Clusters: []string{"c-a", "c-c", "note-only"}},
		{Index: 2, Clusters: []string{"c-b"}},
	}
	if !reflect.DeepEqual(plan.Batches, want) {
		t.Fatalf("got %v", plan.Batches)
	}
}
