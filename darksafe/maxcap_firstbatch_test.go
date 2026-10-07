package darksafe

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file pins the "small first batch, huge later capacity" configuration:
// spreadBy is off, firstBatchSize is a small positive integer and batchSize is
// the largest positive integer the running environment can represent (or
// another huge legal value such as 2147483647, representable everywhere).
// Capacity is only an upper bound per batch, so such a config must plan
// normally once batching continues past batch 1 — it used to overflow the
// slice-end arithmetic in chunkBatches and panic with "makeslice: len out of
// range", leaving the caller without the complete plan.

// testMaxInt is the largest positive int of the running environment:
// math.MaxInt64 on a 64-bit build, math.MaxInt32 on a 32-bit one.
const testMaxInt = int(^uint(0) >> 1)

// hugeCapInput builds the contract configuration directly as a Go struct:
// candidates offered as c, a, b (selection sorts them to a, b, c), first
// batch capped at 1 and later batches at laterCap.
func hugeCapInput(laterCap int) ReleasePlanInput {
	return ReleasePlanInput{
		App:            "payments",
		Revision:       "2026.10.0-r3",
		Image:          "registry.example.net/payments:2026.10.0-r3",
		BatchSize:      laterCap,
		FirstBatchSize: 1,
		Clusters:       []Cluster{{ID: "c"}, {ID: "a"}, {ID: "b"}},
	}
}

// assertCABTwoBatches verifies the exact contract plan: exactly two batches,
// batch 1 holding only "a" and batch 2 holding "b, c", with ascending in-batch
// IDs, sequential numbering, every selected ID appearing exactly once, and
// the app coordinates echoed unchanged.
func assertCABTwoBatches(t *testing.T, plan ReleasePlan, wantApp AppInfo) {
	t.Helper()
	if plan.App != wantApp {
		t.Fatalf("app echo = %+v, want %+v", plan.App, wantApp)
	}
	if len(plan.Batches) != 2 {
		t.Fatalf("expected exactly 2 batches, got %+v", plan.Batches)
	}
	b1, b2 := plan.Batches[0], plan.Batches[1]
	if b1.Index != 1 || strings.Join(b1.Clusters, ",") != "a" {
		t.Fatalf("batch 1 = %+v, want index 1 with [a]", b1)
	}
	if b2.Index != 2 || strings.Join(b2.Clusters, ",") != "b,c" {
		t.Fatalf("batch 2 = %+v, want index 2 with [b c]", b2)
	}
	seen := map[string]int{}
	for _, b := range plan.Batches {
		if len(b.Clusters) == 0 {
			t.Fatalf("plan contains an empty batch: %+v", plan.Batches)
		}
		if !sort.StringsAreSorted(b.Clusters) {
			t.Fatalf("batch %d is not ascending: %v", b.Index, b.Clusters)
		}
		for _, id := range b.Clusters {
			seen[id]++
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if seen[id] != 1 {
			t.Fatalf("cluster %q appears %d times across the plan, want exactly 1", id, seen[id])
		}
	}
	if len(plan.Excluded) != 0 {
		t.Fatalf("expected no excluded clusters, got %+v", plan.Excluded)
	}
}

// TestPlan_SmallFirstBatchHugeLaterCapacity drives the configuration as a
// directly constructed Go config at both huge capacities. The library call
// returns the complete plan without error and must not touch the caller's
// configuration (candidate order c, a, b is offered, not sorted in place).
func TestPlan_SmallFirstBatchHugeLaterCapacity(t *testing.T) {
	for _, size := range []int{testMaxInt, 2147483647} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			in := hugeCapInput(size)
			before := hugeCapInput(size)
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatalf("legal capacity %d must plan, got error: %v", size, err)
			}
			assertCABTwoBatches(t, plan, AppInfo{
				Name: "payments", Revision: "2026.10.0-r3",
				Image: "registry.example.net/payments:2026.10.0-r3",
			})
			if !reflect.DeepEqual(in, before) {
				t.Fatalf("MakeReleasePlan mutated the caller config:\n after:  %+v\n before: %+v", in, before)
			}
		})
	}
}

// TestParse_SmallFirstBatchHugeLaterCapacity reaches the same result through
// the JSON entry point: the capacities keep their written values and the
// parsed config plans identically.
func TestParse_SmallFirstBatchHugeLaterCapacity(t *testing.T) {
	for _, size := range []int{testMaxInt, 2147483647} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			in := parsePlan(t, firstBatchDoc(size, "1",
				`{"id":"c"},{"id":"a"},{"id":"b"}`))
			if in.BatchSize != size {
				t.Fatalf("parsed batchSize = %d, want %d", in.BatchSize, size)
			}
			if in.FirstBatchSize != 1 {
				t.Fatalf("parsed firstBatchSize = %d, want 1", in.FirstBatchSize)
			}
			plan, err := MakeReleasePlan(in)
			if err != nil {
				t.Fatalf("legal capacity %d must plan, got error: %v", size, err)
			}
			assertCABTwoBatches(t, plan, AppInfo{Name: "app", Revision: "r1", Image: "img"})
		})
	}
}

// TestPlanCLI_SmallFirstBatchHugeLaterCapacity observes the contract exactly
// as a user does: exit code 0, the complete plan JSON on stdout, and an empty
// stderr — at the environment's max int and at 2147483647.
func TestPlanCLI_SmallFirstBatchHugeLaterCapacity(t *testing.T) {
	for _, size := range []int{testMaxInt, 2147483647} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			doc := `{
			  "app": "payments",
			  "revision": "2026.10.0-r3",
			  "image": "registry.example.net/payments:2026.10.0-r3",
			  "batchSize": ` + strconv.Itoa(size) + `,
			  "firstBatchSize": 1,
			  "clusters": [{"id": "c"}, {"id": "a"}, {"id": "b"}]
			}`
			code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
			if code != 0 {
				t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
			}
			if stderr != "" {
				t.Fatalf("expected empty stderr on success, got %q", stderr)
			}
			p := decodeCLIPlan(t, stdout)
			if p.App.Name != "payments" ||
				p.App.Revision != "2026.10.0-r3" ||
				p.App.Image != "registry.example.net/payments:2026.10.0-r3" {
				t.Fatalf("app coordinates not echoed: %+v", p.App)
			}
			if len(p.Batches) != 2 {
				t.Fatalf("expected 2 batches, got %+v", p.Batches)
			}
			if got := p.Batches[0]; got.Index != 1 || strings.Join(got.Clusters, ",") != "a" {
				t.Fatalf("batch 1 = %+v, want index 1 with [a]", got)
			}
			if got := p.Batches[1]; got.Index != 2 || strings.Join(got.Clusters, ",") != "b,c" {
				t.Fatalf("batch 2 = %+v, want index 2 with [b c]", got)
			}
			if len(p.Excluded) != 0 {
				t.Fatalf("expected no excluded clusters, got %+v", p.Excluded)
			}
		})
	}
}

// TestPlan_HugeLaterCapacityNoEmptyBatch: when the selection does not exceed
// the first-batch cap, only the one batch is returned — the huge later
// capacity must never cause an empty second batch to be appended.
func TestPlan_HugeLaterCapacityNoEmptyBatch(t *testing.T) {
	// Two selected clusters fit the first-batch cap of 2.
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: testMaxInt, FirstBatchSize: 2,
		Clusters: []Cluster{{ID: "b"}, {ID: "a"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "a,b" {
		t.Fatalf("expected one batch [a b], got %+v", plan.Batches)
	}
	if plan.Batches[0].Index != 1 {
		t.Fatalf("batch index = %d, want 1", plan.Batches[0].Index)
	}

	// A single selected cluster with a first-batch cap of 1 likewise closes
	// the plan after batch 1.
	in.FirstBatchSize = 1
	in.Clusters = []Cluster{{ID: "only"}}
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "only" {
		t.Fatalf("expected one batch [only], got %+v", plan.Batches)
	}
}

// TestPlan_HugeLaterCapacityKeepsSelection: disabled, include and exclude
// decide the selected set and the rejection reasons exactly as before; the
// capacities only split that set. Here the selected set a, e is split into
// [a] | [e] while b, c, d stay excluded with their established reasons.
func TestPlan_HugeLaterCapacityKeepsSelection(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": `+strconv.Itoa(testMaxInt)+`, "firstBatchSize": 1,
		"include": [{"env": "prod"}],
		"exclude": [{"quarantine": "true"}],
		"clusters": [
			{"id": "a", "tags": {"env": "prod"}},
			{"id": "c", "disabled": true, "tags": {"env": "prod"}},
			{"id": "b", "tags": {"env": "dev"}},
			{"id": "d", "tags": {"env": "prod", "quarantine": "true"}},
			{"id": "e", "tags": {"env": "prod"}}
		]
	}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a|e" {
		t.Fatalf("batches = %v, want [a]|[e]", got)
	}
	wantExcluded := []ExcludedCluster{
		{ID: "b", Reason: ReasonIncludeNotMatched},
		{ID: "c", Reason: ReasonDisabled},
		{ID: "d", Reason: ReasonExcludeMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
}

// TestPlan_HugeLaterCapacityWithSpreadBy: enabling fault-domain spreading
// keeps the established same-domain restriction even when later batches run
// at the largest capacity — batch 1 stays at its small cap, and a later batch
// of effectively unlimited capacity still splits conflicting domains apart.
func TestPlan_HugeLaterCapacityWithSpreadBy(t *testing.T) {
	// Batch 1 (cap 1) takes a(east); every remaining cluster sits in a
	// different domain, so batch 2 at the huge cap takes b, c, d at once.
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: testMaxInt, FirstBatchSize: 1, SpreadBy: "zone",
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
			{ID: "d", Tags: map[string]string{"zone": "north"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a|b,c,d" {
		t.Fatalf("batches = %v, want [a]|[b c d]", got)
	}

	// One shared domain still forces one cluster per batch: the huge later
	// capacity never merges same-domain clusters.
	in.Clusters = []Cluster{
		{ID: "a", Tags: map[string]string{"zone": "east"}},
		{ID: "b", Tags: map[string]string{"zone": "east"}},
		{ID: "c", Tags: map[string]string{"zone": "east"}},
	}
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("batches = %v, want [a]|[b]|[c] (same domain stays spread)", got)
	}
}

// TestPlan_HugeCapacityFirstBatchUnsetOrEqual: omitting firstBatchSize or
// setting it equal to the (huge) unified capacity keeps the original result —
// one batch holding every selected cluster — and the two spellings agree
// byte-for-byte.
func TestPlan_HugeCapacityFirstBatchUnsetOrEqual(t *testing.T) {
	clusters := []Cluster{{ID: "c3"}, {ID: "c1"}, {ID: "c2"}}
	unset := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: testMaxInt, Clusters: clusters,
	}
	equal := unset
	equal.FirstBatchSize = testMaxInt

	planUnset, err := MakeReleasePlan(unset)
	if err != nil {
		t.Fatal(err)
	}
	planEqual, err := MakeReleasePlan(equal)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(planUnset, planEqual) {
		t.Fatalf("unset vs equal firstBatchSize differ:\n unset: %+v\n equal: %+v", planUnset, planEqual)
	}
	if got := batchIDs(planUnset); strings.Join(got, "|") != "c1,c2,c3" {
		t.Fatalf("batches = %v, want one batch [c1 c2 c3]", got)
	}
}

// TestParse_HugeCapacityOutOfRangeStillRejected: a JSON integer beyond the
// running environment's int range keeps being refused with the established
// field error — the overflow fix must not silently accept, ignore or replace
// the written capacity. The value is legal JSON, so it must never be reported
// as a JSON format error.
func TestParse_HugeCapacityOutOfRangeStillRejected(t *testing.T) {
	literals := []string{
		"9223372036854775808",          // beyond int64 on every supported environment
		strconv.Itoa(testMaxInt) + "0", // one digit past the native max int
	}
	wantErr := `字段 "batchSize" 必须是正整数`
	for _, lit := range literals {
		t.Run(lit, func(t *testing.T) {
			doc := `{"app":"app","revision":"r1","image":"img","batchSize":` + lit +
				`,"clusters":[{"id":"c"},{"id":"a"},{"id":"b"}]}`
			in, err := ParseReleaseInput([]byte(doc))
			if err == nil {
				t.Fatalf("out-of-range batchSize %s must be rejected, got %+v", lit, in)
			}
			if err.Error() != wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), wantErr)
			}
			if strings.Contains(err.Error(), "JSON 格式错误") {
				t.Fatalf("out-of-range integer is legal JSON, not a format error: %v", err)
			}
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("rejected parse must return the zero-value config, got %+v", in)
			}

			// CLI: exit 1, empty stdout, the field error on stderr.
			code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
			if code != 1 {
				t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty on rejection, got %q", stdout)
			}
			if !strings.Contains(stderr, wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, wantErr)
			}
			if strings.Contains(stderr, "JSON 格式错误") {
				t.Fatalf("stderr must not call a legal JSON number malformed: %q", stderr)
			}
		})
	}
}

// TestValidate_FirstBatchLargerThanHugeBatchSize: the relative rule is
// unchanged — a first-batch capacity above the unified capacity is rejected as
// a configuration error by both validation entries, even when both values are
// large.
func TestValidate_FirstBatchLargerThanBatchSize(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: 2, FirstBatchSize: 3,
		Clusters: []Cluster{{ID: "c1"}},
	}
	wantErr := `字段 "firstBatchSize" 不能大于 "batchSize"`
	if err := ValidateReleaseInput(in); err == nil || err.Error() != wantErr {
		t.Fatalf("ValidateReleaseInput = %v, want %q", err, wantErr)
	}
	plan, err := MakeReleasePlan(in)
	if err == nil || err.Error() != wantErr {
		t.Fatalf("MakeReleasePlan err = %v, want %q", err, wantErr)
	}
	assertZeroReleasePlan(t, plan)
}
