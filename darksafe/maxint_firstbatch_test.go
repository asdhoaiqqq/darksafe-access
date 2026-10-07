package darksafe

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// This file locks in the "small first batch, huge later capacity" contract:
// firstBatchSize is a small legal positive integer while batchSize is the
// largest positive int the running environment can represent (a legal way to
// say "capacity is no constraint"). Planning such a configuration must
// succeed — the capacity is only an upper bound per batch, so it must never
// take part in an addition that can overflow, and the plan must keep every
// selected cluster instead of stopping at the first batch.

// maxIntDoc returns the largest positive int of this environment as a JSON
// integer literal (9223372036854775807 on 64-bit, 2147483647 on 32-bit).
func maxIntLiteral() string {
	return strconv.Itoa(int(^uint(0) >> 1))
}

// TestPlan_FirstBatchWithMaxIntCapacity is the contract example: candidates
// c, a, b all selected, firstBatchSize 1, batchSize at the native int
// ceiling. The plan is exactly two batches — 1: [a], 2: [b c] — with the
// application name, revision and image echoed as usual. Previously the
// second iteration's i+batchSize overflowed the int and the slice allocation
// panicked, losing everything past the first batch.
func TestPlan_FirstBatchWithMaxIntCapacity(t *testing.T) {
	in := ReleasePlanInput{
		App: "payments", Revision: "2026.10.0-r3", Image: "registry.example.net/payments:2026.10.0-r3",
		BatchSize: int(^uint(0) >> 1), FirstBatchSize: 1,
		Clusters: []Cluster{{ID: "c"}, {ID: "a"}, {ID: "b"}},
	}
	orig := in
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a|b,c" {
		t.Fatalf("batches = %v, want [a]|[b c]", got)
	}
	for i, b := range plan.Batches {
		if b.Index != i+1 {
			t.Fatalf("batch %d has index %d, want %d", i, b.Index, i+1)
		}
	}
	wantApp := AppInfo{Name: "payments", Revision: "2026.10.0-r3", Image: "registry.example.net/payments:2026.10.0-r3"}
	if plan.App != wantApp {
		t.Fatalf("app = %+v, want %+v", plan.App, wantApp)
	}
	if len(plan.Excluded) != 0 {
		t.Fatalf("excluded = %+v, want none", plan.Excluded)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("planning mutated the caller's config:\n got  %+v\n want %+v", in, orig)
	}
}

// TestPlan_FirstBatchMaxIntCapacityNoExtraBatch: when the selected count does
// not exceed the first-batch cap, the plan is the single first batch — the
// huge later capacity must not append an empty batch.
func TestPlan_FirstBatchMaxIntCapacityNoExtraBatch(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: int(^uint(0) >> 1), FirstBatchSize: 2,
		Clusters: []Cluster{{ID: "c2"}, {ID: "c1"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c1,c2" {
		t.Fatalf("expected exactly one batch [c1 c2], got %+v", plan.Batches)
	}
}

// TestPlan_FirstBatchMaxIntCapacityWithSpreadBy: the same huge capacity under
// fault-domain spreading keeps the established domain limit — batch 1 honors
// the first-batch cap, later batches run at the (huge) batchSize cap but
// still hold at most one cluster per domain.
func TestPlan_FirstBatchMaxIntCapacityWithSpreadBy(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: int(^uint(0) >> 1), FirstBatchSize: 1, SpreadBy: "zone",
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
	if got := batchIDs(plan); strings.Join(got, "|") != "a|b,c" {
		t.Fatalf("batches = %v, want [a]|[b c]", got)
	}
}

// TestPlanCLI_FirstBatchMaxIntCapacity drives the contract example through
// the plan command: exit 0, the complete plan JSON on stdout, nothing on
// stderr. The batchSize literal is this environment's largest positive int,
// so the same document is legal on both 32-bit and 64-bit builds.
func TestPlanCLI_FirstBatchMaxIntCapacity(t *testing.T) {
	doc := `{"app":"payments","revision":"2026.10.0-r3","image":"registry.example.net/payments:2026.10.0-r3",` +
		`"batchSize":` + maxIntLiteral() + `,"firstBatchSize":1,` +
		`"clusters":[{"id":"c"},{"id":"a"},{"id":"b"}]}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	if p.App.Name != "payments" || p.App.Revision != "2026.10.0-r3" ||
		p.App.Image != "registry.example.net/payments:2026.10.0-r3" {
		t.Fatalf("app = %+v, want payments/2026.10.0-r3 echoed", p.App)
	}
	if len(p.Batches) != 2 ||
		strings.Join(p.Batches[0].Clusters, ",") != "a" || p.Batches[0].Index != 1 ||
		strings.Join(p.Batches[1].Clusters, ",") != "b,c" || p.Batches[1].Index != 2 {
		t.Fatalf("batches = %+v, want 1:[a] 2:[b c]", p.Batches)
	}
}
