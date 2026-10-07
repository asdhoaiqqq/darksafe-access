package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// This file covers the optional firstCluster requirement: a named candidate
// that must enter batch 1. The existing filters decide the selected set
// exactly as before; the requirement only re-arranges that set across
// batches. The designated cluster anchors batch 1 and reserves its fault
// domain, the other first-batch seats go to the smallest IDs that fit,
// in-batch IDs stay ascending (the anchor need not be first), every selected
// ID appears exactly once, and no empty batch is emitted. Omitting the field
// or setting it to "" reproduces the original plan byte-for-byte.

// firstClusterDoc is the contract configuration with the clusters written in
// an explicit order so order-independence can be asserted across variants.
// a, b, d are east; c is west; e is north. With batchSize 3, firstBatchSize
// 2 and firstCluster "d", the plan must be [c,d] | [a,e] | [b].
func firstClusterDoc(clusterBodies string) string {
	return `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstBatchSize": 2, "firstCluster": "d",
  "spreadBy": "zone",
  "clusters": [` + clusterBodies + `]
}`
}

// TestPlan_FirstClusterContractExample pins the worked example from the
// requirement: five selected clusters, a/b/d east, c west, e north,
// firstBatchSize 2, designated d -> [c,d] | [a,e] | [b].
func TestPlan_FirstClusterContractExample(t *testing.T) {
	doc := firstClusterDoc(`
    {"id": "a", "tags": {"zone": "east"}},
    {"id": "b", "tags": {"zone": "east"}},
    {"id": "d", "tags": {"zone": "east"}},
    {"id": "c", "tags": {"zone": "west"}},
    {"id": "e", "tags": {"zone": "north"}}`)
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "c,d|a,e|b" {
		t.Fatalf("batches = %v, want [c d]|[a e]|[b]", got)
	}
	assertFirstClusterInvariant(t, plan, []string{"a", "b", "c", "d", "e"}, "d")
}

// TestPlan_FirstClusterIndependentOfFileOrder runs the contract with the
// candidates in two different orders: the plan must be identical.
func TestPlan_FirstClusterIndependentOfFileOrder(t *testing.T) {
	orderA := firstClusterDoc(`
    {"id": "a", "tags": {"zone": "east"}},
    {"id": "b", "tags": {"zone": "east"}},
    {"id": "d", "tags": {"zone": "east"}},
    {"id": "c", "tags": {"zone": "west"}},
    {"id": "e", "tags": {"zone": "north"}}`)
	orderB := firstClusterDoc(`
    {"id": "e", "tags": {"zone": "north"}},
    {"id": "c", "tags": {"zone": "west"}},
    {"id": "d", "tags": {"zone": "east"}},
    {"id": "a", "tags": {"zone": "east"}},
    {"id": "b", "tags": {"zone": "east"}}`)
	pA, errA := MakeReleasePlan(parsePlan(t, orderA))
	pB, errB := MakeReleasePlan(parsePlan(t, orderB))
	if errA != nil || errB != nil {
		t.Fatalf("both must plan: %v / %v", errA, errB)
	}
	if !reflect.DeepEqual(pA, pB) {
		t.Fatalf("candidate order changed the plan:\n a: %+v\n b: %+v", pA, pB)
	}
	if got := batchIDs(pA); strings.Join(got, "|") != "c,d|a,e|b" {
		t.Fatalf("batches = %v, want [c d]|[a e]|[b]", got)
	}
}

// assertFirstClusterInvariant checks every selected ID appears exactly once,
// batches are numbered from 1 and ascending with no empty batch, and the
// designated cluster is in batch 1.
func assertFirstClusterInvariant(t *testing.T, plan ReleasePlan, selected []string, anchor string) {
	t.Helper()
	seen := map[string]int{}
	for i, b := range plan.Batches {
		if b.Index != i+1 {
			t.Fatalf("batch %d has index %d, want %d", i, b.Index, i+1)
		}
		if len(b.Clusters) == 0 {
			t.Fatalf("plan contains an empty batch: %+v", plan.Batches)
		}
		if !stringsAreAscending(b.Clusters) {
			t.Fatalf("batch %d is not ascending: %v", b.Index, b.Clusters)
		}
		for _, id := range b.Clusters {
			if prev, dup := seen[id]; dup {
				t.Fatalf("cluster %q appears in batches %d and %d", id, prev, b.Index)
			}
			seen[id] = b.Index
		}
	}
	for _, id := range selected {
		if seen[id] != 1 && id == anchor {
			t.Fatalf("designated cluster %q is not in batch 1: %+v", anchor, plan.Batches)
		}
		if _, ok := seen[id]; !ok {
			t.Fatalf("selected cluster %q missing from plan", id)
		}
	}
	if seen[anchor] != 1 {
		t.Fatalf("designated cluster %q is in batch %d, want batch 1", anchor, seen[anchor])
	}
}

func stringsAreAscending(ids []string) bool {
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			return false
		}
	}
	return true
}

// TestPlan_FirstClusterPlainChunking covers ordinary batching (no spreadBy):
// the anchor is pulled into batch 1 ahead of larger IDs that the plain window
// would have taken, and later batches follow the regular batchSize rule.
func TestPlan_FirstClusterPlainChunking(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "firstCluster": "d",
  "clusters": [{"id": "a"}, {"id": "b"}, {"id": "c"}, {"id": "d"}, {"id": "e"}]
}`
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	// Anchor d takes one of the two seats; a (smallest) takes the other. The
	// displaced b, c continue with e at the regular capacity 2.
	if got := batchIDs(plan); strings.Join(got, "|") != "a,d|b,c|e" {
		t.Fatalf("batches = %v, want [a d]|[b c]|[e]", got)
	}
	assertFirstClusterInvariant(t, plan, []string{"a", "b", "c", "d", "e"}, "d")
}

// TestPlan_FirstClusterInsideNaturalWindowChangesNothing: when the anchor
// would already be in batch 1 by plain ascending selection, adding the
// requirement leaves the plan exactly as it was.
func TestPlan_FirstClusterInsideNaturalWindowChangesNothing(t *testing.T) {
	without := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3,
  "clusters": [{"id": "a"}, {"id": "b"}, {"id": "c"}, {"id": "d"}, {"id": "e"}]
}`)
	with := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3, "firstCluster": "a",
  "clusters": [{"id": "a"}, {"id": "b"}, {"id": "c"}, {"id": "d"}, {"id": "e"}]
}`)
	p1, err := MakeReleasePlan(without)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(with)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("an anchor already in batch 1 changed the plan:\n %+v\n %+v", p1, p2)
	}
}

// TestPlan_FirstClusterUnsetOrEmptyMatchesOriginal requires absent, explicit
// "" and the Go zero value to reproduce the original plan byte-for-byte.
func TestPlan_FirstClusterUnsetOrEmptyMatchesOriginal(t *testing.T) {
	base := parsePlan(t, `{
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
	explicitEmpty := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstBatchSize": 2, "firstCluster": "", "spreadBy": "zone",
  "clusters": [
    {"id": "a", "tags": {"zone": "east"}},
    {"id": "b", "tags": {"zone": "east"}},
    {"id": "c", "tags": {"zone": "west"}},
    {"id": "d", "tags": {"zone": "north"}},
    {"id": "e", "tags": {"zone": "west"}}
  ]
}`)
	if explicitEmpty.FirstCluster != "" {
		t.Fatalf("explicit empty firstCluster must parse as %q, got %q", "", explicitEmpty.FirstCluster)
	}
	pBase, err := MakeReleasePlan(base)
	if err != nil {
		t.Fatal(err)
	}
	pEmpty, err := MakeReleasePlan(explicitEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pBase, pEmpty) {
		t.Fatalf("absent vs empty firstCluster differ:\n %+v\n %+v", pBase, pEmpty)
	}
	goIn := base
	goIn.FirstCluster = ""
	pGo, err := MakeReleasePlan(goIn)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pBase, pGo) {
		t.Fatalf("Go zero firstCluster changed the plan:\n %+v\n %+v", pBase, pGo)
	}
	if got := batchIDs(pBase); strings.Join(got, "|") != "a,c|b,d,e" {
		t.Fatalf("original batching changed: %v", got)
	}
}

// TestPlan_FirstClusterUsesBatchSizeWhenFirstBatchSizeUnset: without a
// separate first-batch cap the anchor still joins batch 1 up to batchSize.
func TestPlan_FirstClusterUsesBatchSizeWhenFirstBatchSizeUnset(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 3,
		SpreadBy:     "zone",
		FirstCluster: "d",
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
			{ID: "d", Tags: map[string]string{"zone": "east"}},
			{ID: "e", Tags: map[string]string{"zone": "north"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// d anchors east; a and b (east) are deferred; c (west) and e (north) fill
	// the remaining two seats of the capacity-3 first batch.
	if got := batchIDs(plan); strings.Join(got, "|") != "c,d,e|a|b" {
		t.Fatalf("batches = %v, want [c d e]|[a]|[b]", got)
	}
	assertFirstClusterInvariant(t, plan, []string{"a", "b", "c", "d", "e"}, "d")
}

// TestPlan_FirstClusterReservesFaultDomain checks that anchoring a cluster
// reserves its fault domain in batch 1: same-domain clusters are deferred even
// when they have smaller IDs, and the domain limit is never relaxed; other
// domains still fill available seats.
func TestPlan_FirstClusterReservesFaultDomain(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 3,
		FirstBatchSize: 2, SpreadBy: "zone", FirstCluster: "d",
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
			{ID: "d", Tags: map[string]string{"zone": "east"}},
		},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// d anchors east; a, b (east) are both deferred; c (west) takes the other
	// first-batch seat. a and b then split across later batches one per batch.
	if got := batchIDs(plan); strings.Join(got, "|") != "c,d|a|b" {
		t.Fatalf("batches = %v, want [c d]|[a]|[b]", got)
	}
	assertFirstClusterInvariant(t, plan, []string{"a", "b", "c", "d"}, "d")
}

// TestPlan_FirstClusterSingleCapacity: with firstBatchSize 1 batch 1 holds
// only the anchor; everyone else follows by the regular rules.
func TestPlan_FirstClusterSingleCapacity(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		FirstBatchSize: 1, SpreadBy: "zone", FirstCluster: "c",
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
	if got := batchIDs(plan); strings.Join(got, "|") != "c|a,d|b" {
		t.Fatalf("batches = %v, want [c]|[a d]|[b]", got)
	}
	assertFirstClusterInvariant(t, plan, []string{"a", "b", "c", "d"}, "c")
}

// TestPlan_FirstClusterOnlySelectedCluster: a plan with one selected cluster
// designated as the anchor yields that one batch.
func TestPlan_FirstClusterOnlySelectedCluster(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstBatchSize": 2, "firstCluster": "only",
  "clusters": [{"id": "only"}]
}`
	plan, err := MakeReleasePlan(parsePlan(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "only" {
		t.Fatalf("expected one batch [only], got %+v", plan.Batches)
	}
}

// TestPlan_FirstClusterKeepsExcludedList: selection and the excluded list
// (with the established reasons and ascending order) are unchanged by the
// requirement; the anchor only changes batching.
func TestPlan_FirstClusterKeepsExcludedList(t *testing.T) {
	in := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstBatchSize": 1, "firstCluster": "c5",
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
	if got := batchIDs(plan); strings.Join(got, "|") != "c5|c1" {
		t.Fatalf("batches = %v, want [c5]|[c1]", got)
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

// TestPlan_FirstClusterExactOriginalMatch: the requirement compares the full
// original ID — case and surrounding whitespace are not normalized, the plan
// output keeps the original ID, and a near-miss fails as "not a candidate".
func TestPlan_FirstClusterExactOriginalMatch(t *testing.T) {
	// " d" is a distinct legal ID from "d"; the anchor matches it verbatim and
	// the plan keeps the leading space.
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 3,
		FirstCluster: " d",
		Clusters:     []Cluster{{ID: " d"}, {ID: "a"}, {ID: "c"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("an exact match including surrounding space must succeed: %v", err)
	}
	if got := plan.Batches[0].Clusters; !reflect.DeepEqual(got, []string{" d", "a", "c"}) {
		t.Fatalf("batch 1 = %q, want the original ID with its space preserved", got)
	}

	// "D" does not match "d".
	caseMiss := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 3,
		FirstCluster: "D",
		Clusters:     []Cluster{{ID: "a"}, {ID: "d"}},
	}
	if _, err := MakeReleasePlan(caseMiss); err == nil ||
		!strings.Contains(err.Error(), `首批指定集群 "D" 不在候选列表中`) {
		t.Fatalf("case miss must fail as not-a-candidate, got %v", err)
	}
}

// TestPlan_FirstClusterMissingFails covers "name not among the candidates":
// the whole plan fails with the zero-value plan, from both JSON and Go.
func TestPlan_FirstClusterMissingFails(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3, "firstCluster": "zzz",
  "clusters": [{"id": "a"}, {"id": "b"}]
}`
	in := parsePlan(t, doc)
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("a missing designated cluster must fail")
	}
	const want = `首批指定集群 "zzz" 不在候选列表中`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	assertZeroReleasePlan(t, plan)
}

// TestPlan_FirstClusterNotSelectedFails covers "name exists but disabled or
// filtered out": the plan fails naming the ID and the reason the established
// disabled -> exclude -> include priority assigned it; no substitute is
// chosen and the zero-value plan is returned.
func TestPlan_FirstClusterNotSelectedFails(t *testing.T) {
	cases := map[string]struct {
		doc     string
		wantErr string
	}{
		"disabled": {
			`{"app":"app","revision":"r1","image":"img","batchSize":3,"firstCluster":"c2",
			  "clusters":[{"id":"c1"},{"id":"c2","disabled":true},{"id":"c3"}]}`,
			`首批指定集群 "c2" 未入选：` + ReasonDisabled,
		},
		"exclude matched": {
			`{"app":"app","revision":"r1","image":"img","batchSize":3,"firstCluster":"x",
			  "exclude":[{"env":"temp"}],
			  "clusters":[{"id":"a"},{"id":"x","tags":{"env":"temp"}}]}`,
			`首批指定集群 "x" 未入选：` + ReasonExcludeMatched,
		},
		"include not matched": {
			`{"app":"app","revision":"r1","image":"img","batchSize":3,"firstCluster":"d",
			  "include":[{"env":"prod"}],
			  "clusters":[{"id":"a","tags":{"env":"prod"}},{"id":"d","tags":{"env":"dev"}}]}`,
			`首批指定集群 "d" 未入选：` + ReasonIncludeNotMatched,
		},
		"exclude wins over include": {
			`{"app":"app","revision":"r1","image":"img","batchSize":3,"firstCluster":"x",
			  "include":[{"env":"prod"}],"exclude":[{"quarantine":"true"}],
			  "clusters":[{"id":"a","tags":{"env":"prod"}},
			              {"id":"x","tags":{"env":"prod","quarantine":"true"}}]}`,
			`首批指定集群 "x" 未入选：` + ReasonExcludeMatched,
		},
		"disabled wins over exclude": {
			`{"app":"app","revision":"r1","image":"img","batchSize":3,"firstCluster":"x",
			  "exclude":[{"env":"temp"}],
			  "clusters":[{"id":"a"},{"id":"x","disabled":true,"tags":{"env":"temp"}}]}`,
			`首批指定集群 "x" 未入选：` + ReasonDisabled,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plan, err := MakeReleasePlan(parsePlan(t, tc.doc))
			if err == nil {
				t.Fatal("a filtered-out designated cluster must fail")
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			assertZeroReleasePlan(t, plan)
		})
	}
}

// TestPlan_FirstClusterFailurePrecedence pins where the new failure sits: it
// is a planning failure reported after config validation and after filtering,
// before the all-rejected report and before the missing-tag check.
func TestPlan_FirstClusterFailurePrecedence(t *testing.T) {
	// Every candidate filtered out AND the anchor among them: the anchor's own
	// precise reason wins over the generic all-rejected report.
	allRejected := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3, "firstCluster": "a",
  "include": [{"env": "staging"}],
  "clusters": [{"id": "b"}, {"id": "a", "disabled": true}]
}`)
	plan, err := MakeReleasePlan(allRejected)
	if err == nil {
		t.Fatal("must fail")
	}
	assertZeroReleasePlan(t, plan)
	if err.Error() != `首批指定集群 "a" 未入选：`+ReasonDisabled {
		t.Fatalf("error = %q, want the anchor reason, not the all-rejected report", err.Error())
	}
	if strings.Contains(err.Error(), "没有符合规则的可用集群") {
		t.Fatalf("anchor failure must replace the all-rejected report: %q", err.Error())
	}

	// No candidates at all: the established no-candidate failure is unchanged.
	noCandidates := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3, "firstCluster": "a",
  "clusters": []
}`)
	plan, err = MakeReleasePlan(noCandidates)
	if err == nil || !strings.Contains(err.Error(), "未提供候选集群") {
		t.Fatalf("no-candidate failure must be unchanged, got %v", err)
	}
	assertZeroReleasePlan(t, plan)

	// Anchor selected but missing the spreadBy tag still fails the established
	// missing-tag way (the anchor is selected, so ensureFirstCluster passes).
	missingTag := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstCluster": "c2", "spreadBy": "zone",
  "clusters": [{"id": "c1", "tags": {"zone": "east"}}, {"id": "c2"}]
}`)
	plan, err = MakeReleasePlan(missingTag)
	if err == nil {
		t.Fatal("a selected anchor missing the spread tag must fail")
	}
	assertZeroReleasePlan(t, plan)
	if err.Error() != `集群 "c2" 缺少故障域标签 "zone"` {
		t.Fatalf("error = %q, want the missing-tag failure", err.Error())
	}
}

// TestParse_FirstClusterInvalid rejects a whitespace-only string and every
// non-string JSON value with a field-and-reason config error and the
// zero-value config; an empty string is the legal "unset" spelling.
func TestParse_FirstClusterInvalid(t *testing.T) {
	cases := map[string]struct {
		literal string
		wantErr string
	}{
		"whitespace only": {`"  "`, `字段 "firstCluster" 不能只含空白`},
		"tab only":        {`"\t"`, `字段 "firstCluster" 不能只含空白`},
		"number":          {`42`, `字段 "firstCluster" 必须是字符串`},
		"boolean":         {`true`, `字段 "firstCluster" 必须是字符串`},
		"null":            {`null`, `字段 "firstCluster" 必须是字符串`},
		"object":          {`{"id":"a"}`, `字段 "firstCluster" 必须是字符串`},
		"array":           {`["a"]`, `字段 "firstCluster" 必须是字符串`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := `{"app":"app","revision":"r1","image":"img","batchSize":3,
			         "firstCluster":` + tc.literal + `,"clusters":[{"id":"a"}]}`
			in, err := ParseReleaseInput([]byte(doc))
			if err == nil {
				t.Fatalf("firstCluster %s must be rejected, got %+v", tc.literal, in)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			if !reflect.DeepEqual(in, ReleasePlanInput{}) {
				t.Fatalf("rejected parse must return the zero-value config, got %+v", in)
			}
		})
	}

	// A legal non-empty value is preserved exactly; "" parses as unset.
	in := parsePlan(t, `{"app":"app","revision":"r1","image":"img","batchSize":3,
	                     "firstCluster":" c-A ","clusters":[{"id":" c-A "}]}`)
	if in.FirstCluster != " c-A " {
		t.Fatalf("firstCluster = %q, want the exact original with spaces", in.FirstCluster)
	}
	empty := parsePlan(t, `{"app":"app","revision":"r1","image":"img","batchSize":3,
	                        "firstCluster":"","clusters":[{"id":"a"}]}`)
	if empty.FirstCluster != "" {
		t.Fatalf("empty firstCluster must parse as %q, got %q", "", empty.FirstCluster)
	}
}

// TestParse_FirstClusterInvalidUTF8: the shared strict-text rule applies to
// firstCluster too — a value with invalid UTF-8 is rejected as a text problem
// before any business validation.
func TestParse_FirstClusterInvalidUTF8(t *testing.T) {
	doc := []byte("{\"app\":\"app\",\"revision\":\"r1\",\"image\":\"img\",\"batchSize\":3," +
		"\"firstCluster\":\"d\xff\",\"clusters\":[{\"id\":\"a\"}]}")
	in, err := ParseReleaseInput(doc)
	if err == nil {
		t.Fatalf("invalid UTF-8 firstCluster must be rejected, got %+v", in)
	}
	if !strings.Contains(err.Error(), "无效 UTF-8") {
		t.Fatalf("error must report invalid UTF-8, got %q", err.Error())
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("rejected parse must return the zero-value config, got %+v", in)
	}
}

// TestValidate_FirstClusterGoConfig covers the in-memory entry: empty string
// means unset, whitespace-only is a config error, and invalid UTF-8 is
// rejected in the strict-text stage; MakeReleasePlan returns the zero plan.
func TestValidate_FirstClusterGoConfig(t *testing.T) {
	valid := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 3,
		FirstCluster: "b",
		Clusters:     []Cluster{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}
	plan, err := MakeReleasePlan(valid)
	if err != nil {
		t.Fatalf("a legal in-memory anchor must plan: %v", err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,b,c" {
		t.Fatalf("batches = %v, want [a b c]", got)
	}

	whitespace := valid
	whitespace.FirstCluster = " \t "
	wantWS := `字段 "firstCluster" 不能只含空白`
	if verr := ValidateReleaseInput(whitespace); verr == nil || verr.Error() != wantWS {
		t.Fatalf("ValidateReleaseInput = %v, want %q", verr, wantWS)
	}
	p, perr := MakeReleasePlan(whitespace)
	if perr == nil || perr.Error() != wantWS {
		t.Fatalf("MakeReleasePlan err = %v, want %q", perr, wantWS)
	}
	assertZeroReleasePlan(t, p)

	invalidUTF8 := valid
	invalidUTF8.FirstCluster = "d\xff"
	if verr := ValidateReleaseInput(invalidUTF8); verr == nil ||
		!strings.Contains(verr.Error(), `字段 "firstCluster"`) ||
		!strings.Contains(verr.Error(), "无效 UTF-8") {
		t.Fatalf("invalid UTF-8 firstCluster must be rejected at field firstCluster, got %v", verr)
	}
	p, perr = MakeReleasePlan(invalidUTF8)
	if perr == nil {
		t.Fatal("MakeReleasePlan must reject invalid UTF-8 firstCluster")
	}
	assertZeroReleasePlan(t, p)
}

// TestPlan_FirstClusterDoesNotMutateInput proves the requirement leaves the
// caller's config (including candidate order) untouched.
func TestPlan_FirstClusterDoesNotMutateInput(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 3, FirstBatchSize: 2,
		SpreadBy: "zone", FirstCluster: "d",
		Clusters: []Cluster{
			{ID: "e", Tags: map[string]string{"zone": "north"}},
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "d", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
		},
	}
	before := cloneInput(in)
	if _, err := MakeReleasePlan(in); err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(in); !reflect.DeepEqual(got, candidateIDs(before)) {
		t.Fatalf("candidate order changed: %v -> %v", candidateIDs(before), got)
	}
	if !reflect.DeepEqual(in, before) {
		t.Fatalf("MakeReleasePlan mutated the input:\n before: %#v\n after:  %#v", before, in)
	}
}

// TestPlanCLI_FirstClusterFullPlan drives the worked example through the
// command line: exit 0, empty stderr and the exact three-batch plan.
func TestPlanCLI_FirstClusterFullPlan(t *testing.T) {
	doc := `{
  "app": "payments",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments:2026.10.0-r3",
  "batchSize": 3,
  "firstBatchSize": 2,
  "firstCluster": "d",
  "spreadBy": "zone",
  "clusters": [
    {"id": "a", "tags": {"zone": "east"}},
    {"id": "b", "tags": {"zone": "east"}},
    {"id": "d", "tags": {"zone": "east"}},
    {"id": "c", "tags": {"zone": "west"}},
    {"id": "e", "tags": {"zone": "north"}}
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
	domains := map[string]string{"a": "east", "b": "east", "d": "east", "c": "west", "e": "north"}
	assertSpreadBatches(t, p,
		[][]string{{"c", "d"}, {"a", "e"}, {"b"}}, domains, 3)
}

// TestPlanCLI_FirstClusterFailures: missing or filtered-out anchors fail with
// exit 1, completely empty stdout, and the reason on stderr.
func TestPlanCLI_FirstClusterFailures(t *testing.T) {
	cases := map[string]string{
		"not a candidate": `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3, "firstCluster": "zzz",
  "clusters": [{"id": "a"}, {"id": "b"}]}`,
		"disabled": `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3, "firstCluster": "c2",
  "clusters": [{"id": "c1"}, {"id": "c2", "disabled": true}]}`,
		"filtered": `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3, "firstCluster": "d",
  "include": [{"env": "prod"}],
  "clusters": [{"id": "a", "tags": {"env": "prod"}}, {"id": "d", "tags": {"env": "dev"}}]}`,
	}
	wantContains := map[string]string{
		"not a candidate": `首批指定集群 "zzz" 不在候选列表中`,
		"disabled":        `首批指定集群 "c2" 未入选：` + ReasonDisabled,
		"filtered":        `首批指定集群 "d" 未入选：` + ReasonIncludeNotMatched,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
			if code != 1 {
				t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty on failure, got %q", stdout)
			}
			if !strings.Contains(stderr, wantContains[name]) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, wantContains[name])
			}
		})
	}
}

// TestPlanCLI_FirstClusterConfigErrors: a non-string value or a whitespace-only
// string is a config error at the CLI: exit 1, empty stdout, field+reason on
// stderr.
func TestPlanCLI_FirstClusterConfigErrors(t *testing.T) {
	cases := map[string]string{
		"non-string": `{"app":"app","revision":"r1","image":"img","batchSize":3,"firstCluster":42,"clusters":[{"id":"a"}]}`,
		"whitespace": `{"app":"app","revision":"r1","image":"img","batchSize":3,"firstCluster":"  ","clusters":[{"id":"a"}]}`,
	}
	want := map[string]string{
		"non-string": `字段 "firstCluster" 必须是字符串`,
		"whitespace": `字段 "firstCluster" 不能只含空白`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
			if code != 1 {
				t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty on config error, got %q", stdout)
			}
			if !strings.Contains(stderr, want[name]) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, want[name])
			}
		})
	}
}
