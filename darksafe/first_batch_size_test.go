package darksafe

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// This file covers the optional firstBatchSize: batch 1 is capped by it
// while every later batch is capped by batchSize again. The rules otherwise
// do not change — ascending IDs, at most one cluster per spreadBy domain per
// batch with conflicts deferred (never blocking other domains), no empty
// batches, every selected cluster in exactly one batch, and filtering plus
// the missing-fault-domain-tag failure decided before the first-batch cap
// matters. Omitting the field (or setting it equal to batchSize) must give
// the exact original plan, and illegal values are rejected on both the JSON
// and the directly-constructed-Go-config paths before any planning runs.

// firstBatchDomainInput builds the spec's canonical in-memory setup: a and b
// share the east domain, c and e share west, d alone is north. Candidates are
// handed in non-ascending order on purpose.
func firstBatchDomainInput(batchSize, firstBatchSize int) ReleasePlanInput {
	return ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize:      batchSize,
		FirstBatchSize: firstBatchSize,
		SpreadBy:       "zone",
		Clusters: []Cluster{
			{ID: "e", Tags: map[string]string{"zone": "west"}},
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "d", Tags: map[string]string{"zone": "north"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
		},
	}
}

// TestPlan_FirstBatchSizeSpreadByCanonical is the worked example from the
// spec: batchSize 3, firstBatchSize 2 → [a,c] then [b,d,e]. b conflicts with
// a in batch 1 and is deferred, c (west) still fills the smaller first batch,
// and batch 2 runs at the restored capacity 3.
func TestPlan_FirstBatchSizeSpreadByCanonical(t *testing.T) {
	plan, err := MakeReleasePlan(firstBatchDomainInput(3, 2))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a,c", "b,d,e"}
	if got := batchIDs(plan); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	if plan.Batches[0].Index != 1 || plan.Batches[1].Index != 2 {
		t.Fatalf("batch indices = %d, %d, want 1, 2", plan.Batches[0].Index, plan.Batches[1].Index)
	}
}

// TestPlan_FirstBatchSizeRestoresBatchSizeAfterBatch1 pins the core of the
// option: later batches must not stay at the first-batch cap. With plain
// capacity batching and seven clusters, firstBatchSize 2 / batchSize 3 gives
// sizes 2,3,2 rather than 2,2,2,1.
func TestPlan_FirstBatchSizeChunkRestoresCapacity(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize:      3,
		FirstBatchSize: 2,
		Clusters: []Cluster{
			{ID: "c7"}, {ID: "c1"}, {ID: "c4"},
			{ID: "c2"}, {ID: "c6"}, {ID: "c3"}, {ID: "c5"},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c1,c2", "c3,c4,c5", "c6,c7"}
	if got := batchIDs(plan); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("batches = %v, want %v", got, want)
	}
}

// TestPlan_FirstBatchSizeOneThenFullCapacity shows the same restoration with
// spreadBy: a first batch of one is followed by a full-size batch even though
// the deferred east cluster b only becomes schedulable in batch 2 alongside
// the west cluster c.
func TestPlan_FirstBatchSizeOneThenFullCapacity(t *testing.T) {
	in := firstBatchDomainInput(3, 1)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// Batch 1 takes only a (east); batch 2 at capacity 3 takes b(east),
	// c(west), d(north); batch 3 takes the deferred e(west).
	want := []string{"a", "b,c,d", "e"}
	if got := batchIDs(plan); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("batches = %v, want %v", got, want)
	}
}

// TestPlan_FirstBatchSizeConflictShortBatchIsAllowed: the first-batch cap is
// only an upper bound and must never relax the fault-domain rule. When the
// remaining non-conflicting clusters cannot fill it, batch 1 closes short
// and the deferred clusters continue into later batches — no empty batch is
// produced and each cluster appears exactly once.
func TestPlan_FirstBatchSizeConflictShortBatchIsAllowed(t *testing.T) {
	// a, b, c all east and d west; firstBatchSize 2: a takes east and d takes
	// west ([a,d] fills at 2). Without d (only a,b,c east), batch 1 would
	// close short at one: [a],[b],[c], identical to the unthrottled schedule.
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize:      3,
		FirstBatchSize: 2,
		SpreadBy:       "zone",
		Clusters: []Cluster{
			{ID: "c", Tags: map[string]string{"zone": "east"}},
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	if got := batchIDs(plan); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("batches = %v, want %v (short batch, domains never relaxed)", got, want)
	}
}

// TestPlan_FirstBatchSizeSingleClusterIsOneBatch: with exactly one selected
// cluster the plan has a single batch whether or not the cap is smaller than
// batchSize; no empty second batch is ever appended.
func TestPlan_FirstBatchSizeSingleClusterIsOneBatch(t *testing.T) {
	for _, first := range []int{0, 1, 3} {
		in := ReleasePlanInput{
			App: "app", Revision: "r1", Image: "img",
			BatchSize:      3,
			FirstBatchSize: first,
			SpreadBy:       "zone",
			Clusters:       []Cluster{{ID: "only", Tags: map[string]string{"zone": "east"}}},
		}
		plan, err := MakeReleasePlan(in)
		if err != nil {
			t.Fatalf("firstBatchSize %d: %v", first, err)
		}
		if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "only" {
			t.Fatalf("firstBatchSize %d: expected one batch [only], got %+v", first, plan.Batches)
		}
	}
}

// TestPlan_FirstBatchSizeUnsetOrEqualLeavesPlanUnchanged pins the
// compatibility contract: omitting firstBatchSize or setting it equal to
// batchSize must reproduce the original plan exactly — batch numbering,
// cluster order and the excluded reasons — under both batching modes.
func TestPlan_FirstBatchSizeUnsetOrEqualLeavesPlanUnchanged(t *testing.T) {
	clusters := []Cluster{
		{ID: "a", Tags: map[string]string{"env": "prod", "zone": "east"}},
		{ID: "b", Tags: map[string]string{"env": "prod", "zone": "east"}},
		{ID: "c", Tags: map[string]string{"env": "prod", "zone": "west"}},
		{ID: "d", Tags: map[string]string{"env": "dev", "zone": "west"}},
		{ID: "e", Disabled: true},
	}
	for _, spread := range []string{"", "zone"} {
		t.Run("spreadBy="+spread, func(t *testing.T) {
			base := ReleasePlanInput{
				App: "app", Revision: "r1", Image: "img",
				BatchSize: 2, SpreadBy: spread,
				Include:  []LabelCondition{{"env": "prod"}},
				Clusters: clusters,
			}
			wantPlan, err := MakeReleasePlan(base)
			if err != nil {
				t.Fatal(err)
			}
			// Explicit zero (unset) and equality with batchSize must both be
			// indistinguishable from the field's absence.
			for _, first := range []int{0, 2} {
				variant := base
				variant.FirstBatchSize = first
				got, gerr := MakeReleasePlan(variant)
				if gerr != nil {
					t.Fatalf("firstBatchSize %d: %v", first, gerr)
				}
				if !reflect.DeepEqual(got, wantPlan) {
					t.Fatalf("firstBatchSize %d changed the plan:\n got %+v\nwant %+v", first, got, wantPlan)
				}
			}
		})
	}
}

// TestPlan_FirstBatchSizeOrderIndependent repeats the canonical schedule with
// a shuffled candidate order: the cap changes capacities, not the
// order-independence of the result.
func TestPlan_FirstBatchSizeOrderIndependent(t *testing.T) {
	p1, err := MakeReleasePlan(firstBatchDomainInput(3, 2))
	if err != nil {
		t.Fatal(err)
	}
	shuffled := firstBatchDomainInput(3, 2)
	shuffled.Clusters = []Cluster{
		{ID: "d", Tags: map[string]string{"zone": "north"}},
		{ID: "c", Tags: map[string]string{"zone": "west"}},
		{ID: "e", Tags: map[string]string{"zone": "west"}},
		{ID: "a", Tags: map[string]string{"zone": "east"}},
		{ID: "b", Tags: map[string]string{"zone": "east"}},
	}
	p2, err := MakeReleasePlan(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("candidate order changed the plan:\n %+v\n %+v", p1, p2)
	}
}

// TestPlan_FirstBatchSizeDoesNotRelaxDomainAcrossBatch verifies every selected
// cluster lands exactly once in an ascending, domain-unique schedule over a
// larger configuration, and that no batch after the first exceeds batchSize.
func TestPlan_FirstBatchSizeFullScheduleInvariants(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize:      3,
		FirstBatchSize: 2,
		SpreadBy:       "zone",
		Clusters: []Cluster{
			{ID: "h", Tags: map[string]string{"zone": "z2"}},
			{ID: "a", Tags: map[string]string{"zone": "z1"}},
			{ID: "e", Tags: map[string]string{"zone": "z1"}},
			{ID: "b", Tags: map[string]string{"zone": "z1"}},
			{ID: "f", Tags: map[string]string{"zone": "z2"}},
			{ID: "c", Tags: map[string]string{"zone": "z2"}},
			{ID: "g", Tags: map[string]string{"zone": "z3"}},
			{ID: "d", Tags: map[string]string{"zone": "z3"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i, b := range plan.Batches {
		if b.Index != i+1 {
			t.Fatalf("batch %d index = %d", i, b.Index)
		}
		if len(b.Clusters) == 0 {
			t.Fatalf("batch %d is empty", i+1)
		}
		cap := in.BatchSize
		if i == 0 {
			cap = in.FirstBatchSize
		}
		if len(b.Clusters) > cap {
			t.Fatalf("batch %d holds %d clusters, cap is %d", i+1, len(b.Clusters), cap)
		}
		domains := map[string]struct{}{}
		prev := ""
		for _, id := range b.Clusters {
			if id < prev {
				t.Fatalf("batch %d not ascending: %q before %q", i+1, prev, id)
			}
			prev = id
			if at, dup := seen[id]; dup {
				t.Fatalf("cluster %q appears in batch %d and again %d", id, at, i+1)
			}
			seen[id] = i + 1
			d := map[string]string{
				"a": "z1", "b": "z1", "e": "z1",
				"c": "z2", "f": "z2", "h": "z2",
				"d": "z3", "g": "z3",
			}[id]
			if _, conflict := domains[d]; conflict {
				t.Fatalf("batch %d has two clusters of domain %q", i+1, d)
			}
			domains[d] = struct{}{}
		}
	}
	if len(seen) != len(in.Clusters) {
		t.Fatalf("scheduled %d of %d clusters", len(seen), len(in.Clusters))
	}
}

// TestPlan_FirstBatchSizeDoesNotHideMissingSpreadTag: the cap must not shrink
// the validation surface. Even when batch 1 would contain only clusters that
// carry the spreadBy tag, a later selected cluster missing it still fails the
// whole plan with the existing error naming the smallest offender.
func TestPlan_FirstBatchSizeDoesNotHideMissingSpreadTag(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize:      3,
		FirstBatchSize: 1,
		SpreadBy:       "zone",
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b"}, // selected but missing zone; a first batch of one must not hide it
		},
	}
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatalf("expected missing-tag failure, got %+v", plan)
	}
	if !strings.Contains(err.Error(), `集群 "b" 缺少故障域标签 "zone"`) {
		t.Fatalf("error = %q, want it to name b and zone", err.Error())
	}
	if !reflect.DeepEqual(plan, ReleasePlan{}) {
		t.Fatalf("failure must return the zero plan, got %+v", plan)
	}
}

// TestPlan_FirstBatchSizeDoesNotChangeFiltering: when every candidate is
// filtered out, the first-batch cap changes neither the failure nor the
// per-candidate reasons listed in the error.
func TestPlan_FirstBatchSizeDoesNotChangeFiltering(t *testing.T) {
	base := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: 3, SpreadBy: "zone",
		Include: []LabelCondition{{"env": "prod"}},
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"env": "dev"}},
			{ID: "b", Disabled: true},
			{ID: "c", Tags: map[string]string{"env": "dev", "zone": "z1"}},
		},
	}
	want, err := MakeReleasePlan(base)
	if err == nil {
		t.Fatalf("base must fail with all candidates rejected, got %+v", want)
	}
	throttled := base
	throttled.FirstBatchSize = 1
	plan, terr := MakeReleasePlan(throttled)
	if terr == nil {
		t.Fatalf("throttled plan must fail the same way, got %+v", plan)
	}
	if terr.Error() != err.Error() {
		t.Fatalf("filtering failure changed:\n got %q\nwant %q", terr.Error(), err.Error())
	}
	if !reflect.DeepEqual(plan, ReleasePlan{}) {
		t.Fatalf("failure must return the zero plan, got %+v", plan)
	}
}

// firstBatchSizeDoc builds a JSON document around the given raw firstBatchSize
// member literal, with the canonical five clusters and spreadBy.
func firstBatchSizeDoc(member string) string {
	doc := `{"app":"app","revision":"r1","image":"img","batchSize":3`
	if member != "" {
		doc += `,"firstBatchSize":` + member
	}
	doc += `,"spreadBy":"zone","clusters":[` +
		`{"id":"e","tags":{"zone":"west"}},` +
		`{"id":"a","tags":{"zone":"east"}},` +
		`{"id":"d","tags":{"zone":"north"}},` +
		`{"id":"b","tags":{"zone":"east"}},` +
		`{"id":"c","tags":{"zone":"west"}}]}`
	return doc
}

// TestParse_FirstBatchSizeAcceptedValues covers the accepted shapes through
// JSON: absent and zero-value-of-field... note an explicit 0 is rejected
// (covered below); here absent and equal-to-batchSize plan like the original.
func TestParse_FirstBatchSizeAcceptedValues(t *testing.T) {
	// Absent: FirstBatchSize stays the zero value.
	in, err := ParseReleaseInput([]byte(firstBatchSizeDoc("")))
	if err != nil {
		t.Fatal(err)
	}
	if in.FirstBatchSize != 0 {
		t.Fatalf("absent firstBatchSize = %d, want 0", in.FirstBatchSize)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,c,d|b,e" {
		t.Fatalf("absent firstBatchSize changed batching: %v", got)
	}

	// Equal to batchSize: same schedule, value preserved as written.
	in, err = ParseReleaseInput([]byte(firstBatchSizeDoc("3")))
	if err != nil {
		t.Fatal(err)
	}
	if in.FirstBatchSize != 3 {
		t.Fatalf("firstBatchSize = %d, want 3", in.FirstBatchSize)
	}
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,c,d|b,e" {
		t.Fatalf("firstBatchSize == batchSize changed the plan: %v", got)
	}

	// The genuinely throttled value gives the canonical schedule.
	in, err = ParseReleaseInput([]byte(firstBatchSizeDoc("2")))
	if err != nil {
		t.Fatal(err)
	}
	if in.FirstBatchSize != 2 {
		t.Fatalf("firstBatchSize = %d, want 2", in.FirstBatchSize)
	}
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,c|b,d,e" {
		t.Fatalf("batches = %v, want [a c]|[b d e]", got)
	}
}

// TestParse_FirstBatchSizeIllegalValues pins every rejection: the field obeys
// the same positive-integer rule as batchSize and must not exceed batchSize.
// Nothing is ignored, rounded or truncated, and an explicit zero is refused;
// the returned config is always the zero value.
func TestParse_FirstBatchSizeIllegalValues(t *testing.T) {
	cases := map[string]string{
		"explicit zero":  `字段 "firstBatchSize" 必须是正整数`,
		"negative":       `字段 "firstBatchSize" 必须是正整数`,
		"fraction":       `字段 "firstBatchSize" 必须是正整数`,
		"exponent below": `字段 "firstBatchSize" 必须是正整数`,
		"string":         `字段 "firstBatchSize" 必须是正整数`,
		"boolean":        `字段 "firstBatchSize" 必须是正整数`,
		"null":           `字段 "firstBatchSize" 必须是正整数`,
	}
	literals := map[string]string{
		"explicit zero":  "0",
		"negative":       "-2",
		"fraction":       "1.5",
		"exponent below": "0.5",
		"string":         `"2"`,
		"boolean":        "true",
		"null":           "null",
	}
	for name, wantMsg := range cases {
		t.Run(name, func(t *testing.T) {
			zero, perr := ParseReleaseInput([]byte(firstBatchSizeDoc(literals[name])))
			if perr == nil {
				t.Fatalf("firstBatchSize %s must be rejected, got %+v", literals[name], zero)
			}
			if perr.Error() != wantMsg {
				t.Fatalf("error = %q, want %q", perr.Error(), wantMsg)
			}
			if !reflect.DeepEqual(zero, ReleasePlanInput{}) {
				t.Fatalf("rejected parse must return the zero config, got %+v", zero)
			}
		})
	}

	// Larger than batchSize is its own field error.
	zero, err := ParseReleaseInput([]byte(firstBatchSizeDoc("4")))
	if err == nil {
		t.Fatalf("firstBatchSize 4 with batchSize 3 must be rejected, got %+v", zero)
	}
	if want := `字段 "firstBatchSize" 不能大于 "batchSize"`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if !reflect.DeepEqual(zero, ReleasePlanInput{}) {
		t.Fatalf("rejected parse must return the zero config, got %+v", zero)
	}
}

// TestParse_FirstBatchSizeNativeIntBoundary applies the same integer-width
// rule as batchSize: a literal the running environment's int cannot hold is
// rejected with the field error, never narrowed.
func TestParse_FirstBatchSizeNativeIntBoundary(t *testing.T) {
	// A representable cap paired with an equal representable batchSize plans.
	maxInt := int(^uint(0) >> 1)
	doc := `{"app":"app","revision":"r1","image":"img","batchSize":` + strconv.Itoa(maxInt) +
		`,"firstBatchSize":` + strconv.Itoa(maxInt) +
		`,"clusters":[{"id":"c1"},{"id":"c2"}]}`
	in, err := ParseReleaseInput([]byte(doc))
	if err != nil {
		t.Fatalf("max int must be representable, got %v", err)
	}
	if in.FirstBatchSize != maxInt {
		t.Fatalf("firstBatchSize = %d, want %d", in.FirstBatchSize, maxInt)
	}

	// An out-of-range literal is a positive-integer field error on 32-bit;
	// on 64-bit the same value (4294967297) is legal, mirroring batchSize.
	lit := "4294967297"
	doc = `{"app":"app","revision":"r1","image":"img","batchSize":` + lit +
		`,"firstBatchSize":` + lit + `,"clusters":[{"id":"c1"}]}`
	_, err = ParseReleaseInput([]byte(doc))
	switch strconv.IntSize {
	case 32:
		if err == nil {
			t.Fatalf("%s must be rejected on a 32-bit environment", lit)
		}
		if want := `字段 "batchSize" 必须是正整数`; err.Error() != want {
			t.Fatalf("batchSize must be validated first, error = %q, want %q", err.Error(), want)
		}
	case 64:
		if err != nil {
			t.Fatalf("%s must be legal on a 64-bit environment, got %v", lit, err)
		}
	}
}

// TestValidate_FirstBatchSizeStructPath covers the directly constructed
// config: zero means unset, a negative value is never a capacity, and a set
// value above batchSize is refused; every failure returns the zero plan from
// MakeReleasePlan.
func TestValidate_FirstBatchSizeStructPath(t *testing.T) {
	// Zero is the unset sentinel and stays valid.
	in := firstBatchDomainInput(3, 0)
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("zero firstBatchSize means unset, got %v", err)
	}

	for name, mutate := range map[string]func(*ReleasePlanInput){
		"negative":  func(in *ReleasePlanInput) { in.FirstBatchSize = -1 },
		"above cap": func(in *ReleasePlanInput) { in.FirstBatchSize = 4 },
		"way above": func(in *ReleasePlanInput) { in.FirstBatchSize = 100 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := firstBatchDomainInput(3, 0)
			mutate(&cfg)
			verr := ValidateReleaseInput(cfg)
			if verr == nil || !strings.Contains(verr.Error(), "firstBatchSize") {
				t.Fatalf("ValidateReleaseInput must reject %s firstBatchSize, got %v", name, verr)
			}
			plan, merr := MakeReleasePlan(cfg)
			if merr == nil {
				t.Fatalf("MakeReleasePlan must reject %s firstBatchSize", name)
			}
			if merr.Error() != verr.Error() {
				t.Fatalf("MakeReleasePlan error %q != validate error %q", merr.Error(), verr.Error())
			}
			if !reflect.DeepEqual(plan, ReleasePlan{}) {
				t.Fatalf("rejected planning must return the zero plan, got %+v", plan)
			}
		})
	}

	// Equality with batchSize is accepted on the struct path.
	equal := firstBatchDomainInput(3, 3)
	if err := ValidateReleaseInput(equal); err != nil {
		t.Fatalf("firstBatchSize == batchSize must be legal, got %v", err)
	}
	plan, err := MakeReleasePlan(equal)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,c,d|b,e" {
		t.Fatalf("batches = %v, want the unthrottled schedule", got)
	}

	// An invalid batchSize is still reported before the firstBatchSize
	// relation, even when firstBatchSize would also be out of range.
	bad := firstBatchDomainInput(0, -1)
	err = ValidateReleaseInput(bad)
	if err == nil || err.Error() != `字段 "batchSize" 必须是正整数` {
		t.Fatalf("batchSize error must come first, got %v", err)
	}
}

// TestMakeReleasePlan_FirstBatchSizeDoesNotMutateInput checks the planner
// leaves the new field (and the rest of the config) untouched.
func TestMakeReleasePlan_FirstBatchSizeDoesNotMutateInput(t *testing.T) {
	in := firstBatchDomainInput(3, 2)
	before := fmt.Sprintf("%+v", in)
	if _, err := MakeReleasePlan(in); err != nil {
		t.Fatal(err)
	}
	if after := fmt.Sprintf("%+v", in); after != before {
		t.Fatalf("input mutated:\n before %s\n after  %s", before, after)
	}
}

// TestPlanCLI_FirstBatchSizeCanonical drives the whole contract through the
// real binary: the smaller first batch and the restored capacity appear on
// stdout with exit 0 and an empty stderr.
func TestPlanCLI_FirstBatchSizeCanonical(t *testing.T) {
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, firstBatchSizeDoc("2")))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	domains := map[string]string{
		"a": "east", "b": "east",
		"c": "west", "e": "west",
		"d": "north",
	}
	assertSpreadBatches(t, p, [][]string{{"a", "c"}, {"b", "d", "e"}}, domains, 3)
}

// TestPlanCLI_FirstBatchSizeAbsentOrEqualMatchesOriginal: the absent and the
// equal-to-batchSize documents must emit byte-identical plans at the CLI.
func TestPlanCLI_FirstBatchSizeAbsentOrEqualMatchesOriginal(t *testing.T) {
	code1, out1, err1 := runCLI(t, "plan", writePlanDoc(t, firstBatchSizeDoc("")))
	code2, out2, err2 := runCLI(t, "plan", writePlanDoc(t, firstBatchSizeDoc("3")))
	if code1 != 0 || code2 != 0 {
		t.Fatalf("expected exit 0 both runs, got %d (%q) and %d (%q)", code1, err1, code2, err2)
	}
	if err1 != "" || err2 != "" {
		t.Fatalf("unexpected stderr: %q / %q", err1, err2)
	}
	if out1 != out2 {
		t.Fatalf("absent and equal firstBatchSize differ:\n%s\n----\n%s", out1, out2)
	}
}

// TestPlanCLI_FirstBatchSizeIllegalValues pins the CLI failure shape: exit 1,
// empty stdout, stderr naming firstBatchSize with the concrete reason (never
// a JSON format error for the numeric literals).
func TestPlanCLI_FirstBatchSizeIllegalValues(t *testing.T) {
	cases := map[string]struct {
		lit        string
		wantInMsg  string
		isNumberOK bool
	}{
		"explicit zero": {"0", `字段 "firstBatchSize" 必须是正整数`, true},
		"negative":      {"-2", `字段 "firstBatchSize" 必须是正整数`, true},
		"fraction":      {"1.5", `字段 "firstBatchSize" 必须是正整数`, true},
		"above cap":     {"4", `字段 "firstBatchSize" 不能大于 "batchSize"`, true},
		"string":        {`"2"`, `字段 "firstBatchSize" 必须是正整数`, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, firstBatchSizeDoc(tc.lit)))
			if code != 1 {
				t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty on rejection, got %q", stdout)
			}
			if !strings.Contains(stderr, tc.wantInMsg) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.wantInMsg)
			}
			if tc.isNumberOK && strings.Contains(stderr, "JSON 格式错误") {
				t.Fatalf("numeric literal must be a field error, not a JSON format error: %q", stderr)
			}
		})
	}
}
