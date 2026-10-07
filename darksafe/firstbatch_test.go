package darksafe

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// This file covers the optional firstBatchSize setting: batch 1 is capped at
// firstBatchSize while every later batch keeps the batchSize cap. The setting
// is only a cap — selection (disabled/include/exclude) is decided exactly as
// before, fault-domain spreading still defers conflicting clusters, every
// selected cluster appears in exactly one batch, and no empty batch is ever
// emitted. Omitting the field or setting it equal to batchSize must reproduce
// the original plan byte-for-byte.

// firstBatchDoc builds a plan document with the given batch capacity and
// cluster list; first is substituted as a raw JSON literal (use "" to omit
// the field entirely).
func firstBatchDoc(batchSize int, first string, clusters string) string {
	doc := `{"app":"app","revision":"r1","image":"img","batchSize":` +
		strconv.Itoa(batchSize)
	if first != "" {
		doc += `,"firstBatchSize":` + first
	}
	return doc + `,"clusters":[` + clusters + `]}`
}

// TestPlan_FirstBatchSpreadByThenFullCapacity is the contract example: a
// smaller first batch under fault-domain spreading, with later batches back
// at the full batchSize capacity. a,b are east; c,e west; d north. Batch 1 is
// capped at 2: a takes east, b conflicts and is deferred, c (west) still
// joins. Batch 2 runs at capacity 3 again — b, d and e all fit — proving the
// first-batch cap does not stick to later batches.
func TestPlan_FirstBatchSpreadByThenFullCapacity(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3, "firstBatchSize": 2, "spreadBy": "zone",
		"clusters": [
			{"id": "a", "tags": {"zone": "east"}},
			{"id": "b", "tags": {"zone": "east"}},
			{"id": "c", "tags": {"zone": "west"}},
			{"id": "d", "tags": {"zone": "north"}},
			{"id": "e", "tags": {"zone": "west"}}
		]
	}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,c|b,d,e" {
		t.Fatalf("batches = %v, want [a c]|[b d e]", got)
	}
	for i, b := range plan.Batches {
		if b.Index != i+1 {
			t.Fatalf("batch %d has index %d, want %d", i, b.Index, i+1)
		}
	}
	// Every selected cluster appears exactly once across the plan.
	seen := map[string]int{}
	for _, b := range plan.Batches {
		for _, id := range b.Clusters {
			seen[id]++
		}
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if seen[id] != 1 {
			t.Fatalf("cluster %q appears %d times, want exactly 1", id, seen[id])
		}
	}
}

// TestPlan_FirstBatchPlainChunking covers ordinary batching without spreadBy:
// batch 1 closes at the first-batch cap, batch 2 regains the full batchSize
// capacity, and the tail batch closes short.
func TestPlan_FirstBatchPlainChunking(t *testing.T) {
	in := parsePlan(t, firstBatchDoc(3, "2",
		`{"id":"c1"},{"id":"c2"},{"id":"c3"},{"id":"c4"},{"id":"c5"},{"id":"c6"},{"id":"c7"}`))
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "c1,c2|c3,c4,c5|c6,c7" {
		t.Fatalf("batches = %v, want [c1 c2]|[c3 c4 c5]|[c6 c7]", got)
	}
}

// TestPlan_FirstBatchUnsetOrEqualMatchesOriginal requires three spellings of
// "no separate first batch" — field absent, Go zero value, and an explicit
// value equal to batchSize — to produce exactly the original plan: same batch
// numbering, same cluster order, same excluded list with reasons.
func TestPlan_FirstBatchUnsetOrEqualMatchesOriginal(t *testing.T) {
	clusters := `{"id":"c1"},{"id":"c2","disabled":true},{"id":"c3"},{"id":"c4"},{"id":"c5"}`
	base := parsePlan(t, firstBatchDoc(2, "", clusters))
	equal := parsePlan(t, firstBatchDoc(2, "2", clusters))
	if equal.FirstBatchSize != 2 {
		t.Fatalf("parsed firstBatchSize = %d, want 2", equal.FirstBatchSize)
	}
	if base.FirstBatchSize != 0 {
		t.Fatalf("absent firstBatchSize must parse as 0 (unset), got %d", base.FirstBatchSize)
	}

	planBase, err := MakeReleasePlan(base)
	if err != nil {
		t.Fatal(err)
	}
	planEqual, err := MakeReleasePlan(equal)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(planBase, planEqual) {
		t.Fatalf("firstBatchSize == batchSize changed the plan:\n base:  %+v\n equal: %+v", planBase, planEqual)
	}

	// The Go zero value (unset) must agree with both.
	goIn := base
	goIn.FirstBatchSize = 0
	planGo, err := MakeReleasePlan(goIn)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(planBase, planGo) {
		t.Fatalf("Go zero firstBatchSize changed the plan:\n base: %+v\n go:   %+v", planBase, planGo)
	}

	if got := batchIDs(planBase); strings.Join(got, "|") != "c1,c3|c4,c5" {
		t.Fatalf("original batching changed: %v", got)
	}
	if len(planBase.Excluded) != 1 ||
		planBase.Excluded[0] != (ExcludedCluster{ID: "c2", Reason: ReasonDisabled}) {
		t.Fatalf("excluded = %+v, want c2: %s", planBase.Excluded, ReasonDisabled)
	}
}

// TestPlan_FirstBatchSingleCluster: one selected cluster yields exactly one
// batch, however the caps are set.
func TestPlan_FirstBatchSingleCluster(t *testing.T) {
	in := parsePlan(t, firstBatchDoc(3, "2", `{"id":"only"}`))
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "only" {
		t.Fatalf("expected one batch [only], got %+v", plan.Batches)
	}
	if plan.Batches[0].Index != 1 {
		t.Fatalf("batch index = %d, want 1", plan.Batches[0].Index)
	}
}

// TestPlan_FirstBatchCapIsOnlyAnUpperBound: the first batch may close short —
// when fewer clusters are selected than the cap allows, and when fault-domain
// conflicts leave no cluster that can still enter it. No empty batch is ever
// emitted in either case.
func TestPlan_FirstBatchCapIsOnlyAnUpperBound(t *testing.T) {
	// Fewer selected clusters than the first-batch cap: one short batch only.
	in := parsePlan(t, firstBatchDoc(3, "2", `{"id":"c1"}`))
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || len(plan.Batches[0].Clusters) != 1 {
		t.Fatalf("short selection must close batch 1 short without empty batches, got %+v", plan.Batches)
	}

	// Domain conflicts: batch 1 (cap 2) takes a(east) and c(west); b and d
	// conflict and are deferred. Batch 2 at capacity 3 takes b(east) and
	// d(north) — e(west) conflicts with nothing left but still fits. The
	// domain limit is never relaxed to fill a batch.
	in = parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3, "firstBatchSize": 2, "spreadBy": "zone",
		"clusters": [
			{"id": "a", "tags": {"zone": "east"}},
			{"id": "b", "tags": {"zone": "east"}},
			{"id": "c", "tags": {"zone": "west"}}
		]
	}`)
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,c|b" {
		t.Fatalf("batches = %v, want [a c]|[b] (domain limit never relaxed)", got)
	}
	for _, b := range plan.Batches {
		if len(b.Clusters) == 0 {
			t.Fatalf("plan contains an empty batch: %+v", plan.Batches)
		}
	}
}

// TestPlan_FirstBatchDoesNotChangeSelection: disabled/include/exclude decide
// the selected set exactly as without the setting; the first-batch cap only
// splits that set differently.
func TestPlan_FirstBatchDoesNotChangeSelection(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3, "firstBatchSize": 1,
		"include": [{"env": "prod"}],
		"exclude": [{"quarantine": "true"}],
		"clusters": [
			{"id": "c1", "tags": {"env": "prod"}},
			{"id": "c2", "tags": {"env": "prod", "quarantine": "true"}},
			{"id": "c3", "disabled": true, "tags": {"env": "prod"}},
			{"id": "c4", "tags": {"env": "dev"}},
			{"id": "c5", "tags": {"env": "prod"}}
		]
	}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "c1|c5" {
		t.Fatalf("batches = %v, want [c1]|[c5]", got)
	}
	wantExcluded := []ExcludedCluster{
		{ID: "c2", Reason: ReasonExcludeMatched},
		{ID: "c3", Reason: ReasonDisabled},
		{ID: "c4", Reason: ReasonIncludeNotMatched},
	}
	if !reflect.DeepEqual(plan.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", plan.Excluded, wantExcluded)
	}
}

// TestParse_FirstBatchSizeInvalid: every illegal JSON value for the optional
// field is rejected with an error naming the field and the reason — never
// ignored, rounded, truncated or clamped — and the returned config is the
// zero value. An explicit zero is rejected just like any non-positive value.
func TestParse_FirstBatchSizeInvalid(t *testing.T) {
	clusters := `{"id":"c1"}`
	cases := map[string]struct {
		literal string
		wantErr string
	}{
		"explicit zero":     {"0", `字段 "firstBatchSize" 必须是正整数`},
		"negative":          {"-2", `字段 "firstBatchSize" 必须是正整数`},
		"fraction":          {"2.5", `字段 "firstBatchSize" 必须是正整数`},
		"string":            {`"2"`, `字段 "firstBatchSize" 必须是正整数`},
		"boolean":           {"true", `字段 "firstBatchSize" 必须是正整数`},
		"null":              {"null", `字段 "firstBatchSize" 必须是正整数`},
		"overflow":          {"9223372036854775808", `字段 "firstBatchSize" 必须是正整数`},
		"larger than batch": {"3", `字段 "firstBatchSize" 不能大于 "batchSize"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in, err := ParseReleaseInput([]byte(firstBatchDoc(2, tc.literal, clusters)))
			if err == nil {
				t.Fatalf("firstBatchSize %s must be rejected, got %+v", tc.literal, in)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("rejected parse must return the zero-value config, got %+v", in)
			}
		})
	}
}

// TestValidate_FirstBatchSizeGoConfig: a directly constructed Go config uses
// the zero value as "unset"; every other illegal value is rejected before
// planning by both ValidateReleaseInput and MakeReleasePlan, the latter
// returning the zero-value plan.
func TestValidate_FirstBatchSizeGoConfig(t *testing.T) {
	valid := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		Clusters: []Cluster{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}},
	}

	// Zero means unset and plans with the original batching.
	plan, err := MakeReleasePlan(valid)
	if err != nil {
		t.Fatalf("zero firstBatchSize must be legal (unset): %v", err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "c1,c2|c3" {
		t.Fatalf("batches = %v, want [c1 c2]|[c3]", got)
	}

	cases := map[string]struct {
		first   int
		wantErr string
	}{
		"negative":          {-1, `字段 "firstBatchSize" 必须是正整数`},
		"larger than batch": {3, `字段 "firstBatchSize" 不能大于 "batchSize"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := valid
			in.FirstBatchSize = tc.first
			if verr := ValidateReleaseInput(in); verr == nil || verr.Error() != tc.wantErr {
				t.Fatalf("ValidateReleaseInput = %v, want %q", verr, tc.wantErr)
			}
			plan, perr := MakeReleasePlan(in)
			if perr == nil || perr.Error() != tc.wantErr {
				t.Fatalf("MakeReleasePlan err = %v, want %q", perr, tc.wantErr)
			}
			assertZeroReleasePlan(t, plan)
		})
	}
}

// TestPlan_FirstBatchKeepsExistingFailures: the first-batch cap never masks
// the established planning failures — all candidates filtered out still
// reports every candidate's reason, and a selected cluster missing the
// spreadBy tag still fails the whole plan with the zero-value plan.
func TestPlan_FirstBatchKeepsExistingFailures(t *testing.T) {
	allRejected := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3, "firstBatchSize": 1,
		"include": [{"env": "staging"}],
		"clusters": [{"id": "b"}, {"id": "a", "disabled": true}]
	}`)
	plan, err := MakeReleasePlan(allRejected)
	if err == nil {
		t.Fatal("all candidates filtered out must still fail")
	}
	assertZeroReleasePlan(t, plan)
	want := allRejectedLibraryError("a："+ReasonDisabled, "b："+ReasonIncludeNotMatched)
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}

	missingTag := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3, "firstBatchSize": 1, "spreadBy": "zone",
		"clusters": [
			{"id": "c1", "tags": {"zone": "east"}},
			{"id": "c2"}
		]
	}`)
	plan, err = MakeReleasePlan(missingTag)
	if err == nil {
		t.Fatal("a selected cluster missing the spread tag must still fail")
	}
	assertZeroReleasePlan(t, plan)
	if err.Error() != `集群 "c2" 缺少故障域标签 "zone"` {
		t.Fatalf("error = %q, want the missing-tag failure", err.Error())
	}
}

// TestPlanCLI_FirstBatchFullPlan drives the setting through the command line:
// the config file entry is the only interface, success exits 0 with the
// complete plan on stdout and an empty stderr.
func TestPlanCLI_FirstBatchFullPlan(t *testing.T) {
	doc := `{
	  "app": "payments",
	  "revision": "2026.10.0-r3",
	  "image": "registry.example.net/payments:2026.10.0-r3",
	  "batchSize": 3,
	  "firstBatchSize": 2,
	  "spreadBy": "zone",
	  "clusters": [
	    {"id": "a", "tags": {"zone": "east"}},
	    {"id": "b", "tags": {"zone": "east"}},
	    {"id": "c", "tags": {"zone": "west"}},
	    {"id": "d", "tags": {"zone": "north"}},
	    {"id": "e", "tags": {"zone": "west"}}
	  ]
	}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr on success, got %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	domains := map[string]string{
		"a": "east", "b": "east", "c": "west", "d": "north", "e": "west",
	}
	// Batch 1 is capped at 2; batch 2 runs at the full capacity 3 again.
	assertSpreadBatches(t, p, [][]string{{"a", "c"}, {"b", "d", "e"}}, domains, 3)
}

// TestPlanCLI_FirstBatchInvalidFailsCleanly: an illegal firstBatchSize in the
// config file exits 1, explains the field and the reason on stderr, and
// leaves stdout completely empty.
func TestPlanCLI_FirstBatchInvalidFailsCleanly(t *testing.T) {
	cases := map[string]struct {
		literal string
		wantErr string
	}{
		"zero":              {"0", `字段 "firstBatchSize" 必须是正整数`},
		"larger than batch": {"4", `字段 "firstBatchSize" 不能大于 "batchSize"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := firstBatchDoc(3, tc.literal, `{"id":"c1"}`)
			code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
			if code != 1 {
				t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty on config failure, got %q", stdout)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
		})
	}
}
