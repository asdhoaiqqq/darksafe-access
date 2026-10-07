package darksafe

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// This file covers the optional firstCluster setting: when set, one exact
// candidate ID must be selected by the existing disabled/include/exclude
// rules and must enter batch 1; the other first-batch seats prefer the
// smallest fitting IDs, batch 1 still displays ascending (the anchor need
// not be first), firstBatchSize caps batch 1 as before while later batches
// keep the batchSize cap, and spreadBy's one-cluster-per-domain limit is
// never relaxed — same-domain peers of the anchor are deferred and other
// domains still enter batch 1. Omitting the field or setting it to "" must
// reproduce the original plan byte-for-byte.

// firstClusterDoc wraps firstClusterJSON into a complete plan document.
func firstClusterDoc(batchSize int, first string, firstClusterJSON, clusters string) string {
	doc := `{"app":"app","revision":"r1","image":"img","batchSize":` +
		strconv.Itoa(batchSize)
	if first != "" {
		doc += `,"firstBatchSize":` + first
	}
	if firstClusterJSON != "" {
		doc += `,"firstCluster":` + firstClusterJSON
	}
	return doc + `,"clusters":[` + clusters + `]}`
}

// specFirstClusterClusters is the spec scenario: a, b, d are east; c is
// west; e is north; all five are selected. The two literals list the same
// candidates in different file order — the result must not depend on it.
const specFirstClusterClustersA = `
    {"id":"a","tags":{"zone":"east"}},
    {"id":"b","tags":{"zone":"east"}},
    {"id":"d","tags":{"zone":"east"}},
    {"id":"c","tags":{"zone":"west"}},
    {"id":"e","tags":{"zone":"north"}}`

const specFirstClusterClustersB = `
    {"id":"e","tags":{"zone":"north"}},
    {"id":"c","tags":{"zone":"west"}},
    {"id":"a","tags":{"zone":"east"}},
    {"id":"d","tags":{"zone":"east"}},
    {"id":"b","tags":{"zone":"east"}}`

// TestPlan_FirstClusterSpecExample is the contract example: batchSize 3,
// firstBatchSize 2, spreadBy zone, firstCluster d yields
// [c,d]|[a,e]|[b] regardless of the candidates' file order.
func TestPlan_FirstClusterSpecExample(t *testing.T) {
	template := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstBatchSize": 2, "spreadBy": "zone",
  "firstCluster": "d",
  "clusters": [%s]
}`
	var plans []ReleasePlan
	for _, list := range []string{specFirstClusterClustersA, specFirstClusterClustersB} {
		plan, err := MakeReleasePlan(parsePlan(t, fmtJSONList(template, list)))
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plan)
	}
	if got := batchIDs(plans[0]); strings.Join(got, "|") != "c,d|a,e|b" {
		t.Fatalf("batches = %v, want [c d]|[a e]|[b]", got)
	}
	if !reflect.DeepEqual(plans[0], plans[1]) {
		t.Fatalf("candidate order changed the plan:\n %+v\n %+v", plans[0], plans[1])
	}
	for i, b := range plans[0].Batches {
		if b.Index != i+1 {
			t.Fatalf("batch %d has index %d", i, b.Index)
		}
		if i == 0 {
			// The anchor sits second: it is required in batch 1, not first.
			if strings.Join(b.Clusters, ",") != "c,d" {
				t.Fatalf("anchor need not be first in batch 1: %v", b.Clusters)
			}
		}
	}
}

// TestPlan_FirstClusterEveryIDOnceAndNoEmptyBatches re-checks the universal
// batching invariants with the anchor set: every selected ID appears
// exactly once, no batch is empty, and IDs stay ascending within a batch.
func TestPlan_FirstClusterEveryIDOnceAndNoEmptyBatches(t *testing.T) {
	in := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstBatchSize": 2, "spreadBy": "zone",
  "firstCluster": "d",
  "clusters": [`+specFirstClusterClustersA+`]
}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, b := range plan.Batches {
		if len(b.Clusters) == 0 {
			t.Fatalf("plan contains an empty batch: %+v", plan.Batches)
		}
		if !stringsAreAscending(b.Clusters) {
			t.Fatalf("batch %d is not ascending: %v", b.Index, b.Clusters)
		}
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

func stringsAreAscending(ids []string) bool {
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			return false
		}
	}
	return true
}

// fmtJSONList substitutes a cluster list into a one-%s template.
func fmtJSONList(template, list string) string {
	return strings.Replace(template, "%s", list, 1)
}

// TestPlan_FirstClusterPlainChunking covers ordinary batching (no spreadBy):
// the anchor is pre-seated and the smallest other IDs fill the first cap;
// later batches return to the ordinary batchSize cap.
func TestPlan_FirstClusterPlainChunking(t *testing.T) {
	clusters := `{"id":"a"},{"id":"b"},{"id":"c"},{"id":"d"},{"id":"e"}`

	// firstBatchSize 2, anchor e (ordinarily in batch 2): batch 1 is [a,e],
	// batch 2 is back at capacity 3: [b,c,d].
	in := parsePlan(t, firstClusterDoc(3, "2", `"e"`, clusters))
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,e|b,c,d" {
		t.Fatalf("batches = %v, want [a e]|[b c d]", got)
	}

	// No separate first cap: batch 1 keeps batchSize seats — [a,b,e] — and
	// the remainder forms [c,d].
	in = parsePlan(t, firstClusterDoc(3, "", `"e"`, clusters))
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "a,b,e|c,d" {
		t.Fatalf("batches = %v, want [a b e]|[c d]", got)
	}

	// An anchor that would enter batch 1 anyway changes nothing: pre-seating
	// c with cap 3 still yields the ordinary chunks.
	ordinary := parsePlan(t, firstClusterDoc(3, "", "", clusters))
	anchored := parsePlan(t, firstClusterDoc(3, "", `"c"`, clusters))
	p1, err := MakeReleasePlan(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(anchored)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("an anchor naturally in batch 1 changed the plan:\n %+v\n %+v", p1, p2)
	}
}

// TestPlan_FirstClusterSpreadByDefersSameDomain proves the anchor occupies
// its fault domain in batch 1 just like an ordinary pick: same-domain peers
// (including smaller IDs) are deferred, other domains still fill the batch,
// and deferred peers return through the ordinary later batches.
func TestPlan_FirstClusterSpreadByDefersSameDomain(t *testing.T) {
	// Without an anchor this would be [a,c,d]|[b]; anchoring b (east) makes
	// batch 1 [b,c,d] and defers a — the anchor never relaxes the domain.
	in := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "spreadBy": "zone", "firstCluster": "b",
  "clusters": [
    {"id": "a", "tags": {"zone": "east"}},
    {"id": "b", "tags": {"zone": "east"}},
    {"id": "c", "tags": {"zone": "west"}},
    {"id": "d", "tags": {"zone": "north"}}
  ]
}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "b,c,d|a" {
		t.Fatalf("batches = %v, want [b c d]|[a] (anchor domain peers deferred)", got)
	}

	// A first-batch cap of 1 holds only the anchor even though other domains
	// exist; batch 2 resumes with the ordinary rules and cap.
	in = parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 3, "firstBatchSize": 1, "spreadBy": "zone", "firstCluster": "d",
  "clusters": [
    {"id": "a", "tags": {"zone": "east"}},
    {"id": "b", "tags": {"zone": "east"}},
    {"id": "c", "tags": {"zone": "west"}},
    {"id": "d", "tags": {"zone": "north"}}
  ]
}`)
	plan, err = MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "d|a,c|b" {
		t.Fatalf("batches = %v, want [d]|[a c]|[b]", got)
	}
}

// TestPlan_FirstClusterKeepsExcludedList: selection and the excluded report
// are decided exactly as without the setting — the anchor only moves one
// already-selected cluster into batch 1.
func TestPlan_FirstClusterKeepsExcludedList(t *testing.T) {
	ordinary := parsePlan(t, `{
  "app": "app", "revision": "r1", "image": "img", "batchSize": 3,
  "include": [{"env": "prod"}],
  "clusters": [
    {"id": "c1", "tags": {"env": "prod"}},
    {"id": "c2", "disabled": true, "tags": {"env": "prod"}},
    {"id": "c3", "tags": {"env": "dev"}},
    {"id": "c4", "tags": {"env": "prod"}},
    {"id": "c5", "tags": {"env": "prod"}}
  ]
}`)
	anchored := ordinary
	anchored.FirstCluster = "c5"

	p1, err := MakeReleasePlan(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(anchored)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p1.Excluded, p2.Excluded) {
		t.Fatalf("excluded list changed:\n %+v\n %+v", p1.Excluded, p2.Excluded)
	}
	if got := batchIDs(p2); strings.Join(got, "|") != "c1,c4,c5" {
		t.Fatalf("batches = %v, want one batch [c1 c4 c5]", got)
	}
	wantExcluded := []ExcludedCluster{
		{ID: "c2", Reason: ReasonDisabled},
		{ID: "c3", Reason: ReasonIncludeNotMatched},
	}
	if !reflect.DeepEqual(p2.Excluded, wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", p2.Excluded, wantExcluded)
	}
	if p2.App != p1.App {
		t.Fatalf("app info changed: %+v vs %+v", p2.App, p1.App)
	}
}

// TestPlan_FirstClusterOmittedEmptyOrUnsetMatchesOriginal requires the three
// spellings of "no anchor" — field absent, explicit "" and the Go zero
// value — to produce exactly the original plan under both batching modes.
func TestPlan_FirstClusterOmittedEmptyOrUnsetMatchesOriginal(t *testing.T) {
	clusters := `{"id":"a","tags":{"zone":"east"}},{"id":"b","tags":{"zone":"east"}},{"id":"c","tags":{"zone":"west"}},{"id":"d","tags":{"zone":"north"}}`
	base := parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":2,"spreadBy":"zone",
  "clusters":[`+clusters+`]}`)
	explicitEmpty := parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":2,"spreadBy":"zone","firstCluster":"",
  "clusters":[`+clusters+`]}`)
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
	goIn := base
	goIn.FirstCluster = ""
	pGo, err := MakeReleasePlan(goIn)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pBase, pEmpty) || !reflect.DeepEqual(pBase, pGo) {
		t.Fatalf("omitted/empty/zero firstCluster must agree:\n base: %+v\n empty: %+v\n go: %+v", pBase, pEmpty, pGo)
	}
	if got := batchIDs(pBase); strings.Join(got, "|") != "a,c|b,d" {
		t.Fatalf("original batching changed: %v", got)
	}
}

// TestPlan_FirstClusterExactMatch pins byte-for-byte identity: case and
// surrounding whitespace are not normalized, on both the comparison and the
// output side.
func TestPlan_FirstClusterExactMatch(t *testing.T) {
	// "D" names no candidate even though "d" exists.
	in := parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":3,
  "firstCluster":"D",
  "clusters":[{"id":"d"},{"id":"a"},{"id":"b"}]}`)
	_, err := MakeReleasePlan(in)
	if err == nil || !strings.Contains(err.Error(), `不在候选列表中`) {
		t.Fatalf("case-different anchor must be not-found, got %v", err)
	}

	// " a" (leading space) is a legal ID distinct from "a" and is preserved
	// verbatim in the output.
	in = parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":3,
  "firstCluster":" a",
  "clusters":[{"id":" a"},{"id":"a"},{"id":"b"}]}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("a whitespace-bearing exact ID must match, got %v", err)
	}
	if got := strings.Join(plan.Batches[0].Clusters, ","); got != " a,a,b" {
		t.Fatalf("batch 1 = %q, want original IDs preserved", got)
	}
}

// TestPlan_FirstClusterNotFoundFails covers the "name is not a candidate"
// failure: the whole plan fails with the exact message, returns the zero
// plan, and no substitute cluster is scheduled.
func TestPlan_FirstClusterNotFoundFails(t *testing.T) {
	in := parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":2,
  "firstCluster":"nope",
  "clusters":[{"id":"c1"},{"id":"c2"}]}`)
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("a missing anchor must fail the plan")
	}
	if err.Error() != `指定的首批集群 "nope" 不在候选列表中` {
		t.Fatalf("error = %q, want the not-found message", err.Error())
	}
	assertZeroReleasePlan(t, plan)
}

// TestPlan_FirstClusterRejectedFails covers an anchor that exists but
// selection rejects: the error names the ID and the reason the fixed
// disabled → exclude → include priority assigns.
func TestPlan_FirstClusterRejectedFails(t *testing.T) {
	cases := map[string]struct {
		doc     string
		wantErr string
	}{
		"disabled": {
			`{"app":"app","revision":"r1","image":"img","batchSize":2,
			  "firstCluster":"c2",
			  "clusters":[{"id":"c1"},{"id":"c2","disabled":true}]}`,
			`指定的首批集群 "c2" 未入选：` + ReasonDisabled,
		},
		"exclude": {
			`{"app":"app","revision":"r1","image":"img","batchSize":2,
			  "firstCluster":"c2","exclude":[{"env":"temp"}],
			  "clusters":[{"id":"c1"},{"id":"c2","tags":{"env":"temp"}}]}`,
			`指定的首批集群 "c2" 未入选：` + ReasonExcludeMatched,
		},
		"include": {
			`{"app":"app","revision":"r1","image":"img","batchSize":2,
			  "firstCluster":"c2","include":[{"env":"prod"}],
			  "clusters":[{"id":"c1","tags":{"env":"prod"}},{"id":"c2","tags":{"env":"dev"}}]}`,
			`指定的首批集群 "c2" 未入选：` + ReasonIncludeNotMatched,
		},
		"exclude wins over include": {
			`{"app":"app","revision":"r1","image":"img","batchSize":2,
			  "firstCluster":"c2","include":[{"env":"prod"}],"exclude":[{"quarantine":"true"}],
			  "clusters":[{"id":"c2","tags":{"env":"prod","quarantine":"true"}}]}`,
			`指定的首批集群 "c2" 未入选：` + ReasonExcludeMatched,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plan, err := MakeReleasePlan(parsePlan(t, tc.doc))
			if err == nil {
				t.Fatal("a rejected anchor must fail the plan")
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			assertZeroReleasePlan(t, plan)
		})
	}
}

// TestPlan_FirstClusterFailurePreemptsAllRejected: an anchor rejected while
// every other candidate is rejected as well reports the anchor-specific
// failure, not the generic all-rejected list — the user asked for one
// cluster, and that exact request is what failed.
func TestPlan_FirstClusterFailurePreemptsAllRejected(t *testing.T) {
	notFound := parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":2,
  "firstCluster":"zzz","include":[{"env":"prod"}],
  "clusters":[{"id":"a","disabled":true},{"id":"b","tags":{"env":"dev"}}]}`)
	if _, err := MakeReleasePlan(notFound); err == nil ||
		!strings.Contains(err.Error(), `指定的首批集群 "zzz" 不在候选列表中`) {
		t.Fatalf("missing anchor must fail in its own right, got %v", err)
	}

	filtered := parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":2,
  "firstCluster":"b","include":[{"env":"prod"}],
  "clusters":[{"id":"a","disabled":true},{"id":"b","tags":{"env":"dev"}}]}`)
	plan, err := MakeReleasePlan(filtered)
	if err == nil {
		t.Fatal("expected failure")
	}
	if err.Error() != `指定的首批集群 "b" 未入选：`+ReasonIncludeNotMatched {
		t.Fatalf("error = %q", err.Error())
	}
	assertZeroReleasePlan(t, plan)
}

// TestPlan_FirstClusterKeepsMissingSpreadTagFailure: a selected cluster
// missing the spreadBy tag still fails the whole plan — including when that
// cluster is the anchor — and filtered-out clusters still need no tag.
func TestPlan_FirstClusterKeepsMissingSpreadTagFailure(t *testing.T) {
	in := parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":2,
  "spreadBy":"zone","firstCluster":"a",
  "clusters":[{"id":"a"},{"id":"c","tags":{"zone":"west"}}]}`)
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("the anchor missing its spread tag must fail")
	}
	if err.Error() != `集群 "a" 缺少故障域标签 "zone"` {
		t.Fatalf("error = %q, want the missing-tag failure", err.Error())
	}
	assertZeroReleasePlan(t, plan)

	// A filtered-out anchor fails with its filter reason before the tag
	// check; a filtered cluster without the tag never preempts anything.
	in = parsePlan(t, `{
  "app":"app","revision":"r1","image":"img","batchSize":2,
  "spreadBy":"zone","firstCluster":"x",
  "clusters":[{"id":"x","disabled":true},{"id":"c","tags":{"zone":"west"}}]}`)
	if _, err := MakeReleasePlan(in); err == nil ||
		err.Error() != `指定的首批集群 "x" 未入选：`+ReasonDisabled {
		t.Fatalf("anchor filter reason must win over the missing-tag check, got %v", err)
	}
}

// TestParse_FirstClusterInvalid rejects every illegal JSON spelling with a
// field-and-reason error and the zero-value config.
func TestParse_FirstClusterInvalid(t *testing.T) {
	clusters := `{"id":"c1"}`
	cases := map[string]struct {
		literal string
		wantErr string
	}{
		"whitespace only": {`"   "`, `字段 "firstCluster" 不能只含空白`},
		"tab only":        {`"\t"`, `字段 "firstCluster" 不能只含空白`},
		"number":          {"5", `字段 "firstCluster" 必须是字符串`},
		"boolean":         {"true", `字段 "firstCluster" 必须是字符串`},
		"null":            {"null", `字段 "firstCluster" 必须是字符串`},
		"array":           {`["c1"]`, `字段 "firstCluster" 必须是字符串`},
		"object":          {`{"id":"c1"}`, `字段 "firstCluster" 必须是字符串`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := firstClusterDoc(2, "", tc.literal, clusters)
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
}

// TestValidate_FirstClusterGoConfig: a directly constructed Go config uses
// the empty string as "unset"; a whitespace-only value is rejected before
// planning, and a rejected/missing anchor fails MakeReleasePlan with the
// zero-value plan.
func TestValidate_FirstClusterGoConfig(t *testing.T) {
	valid := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 3,
		FirstCluster: "c3",
		Clusters:     []Cluster{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}, {ID: "c4"}, {ID: "c5"}},
	}
	plan, err := MakeReleasePlan(valid)
	if err != nil {
		t.Fatalf("a valid anchor must plan: %v", err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "c1,c2,c3|c4,c5" {
		t.Fatalf("batches = %v, want [c1 c2 c3]|[c4 c5]", got)
	}

	// Whitespace-only is a configuration error on both validation entries.
	ws := valid
	ws.FirstCluster = " \t "
	wantErr := `字段 "firstCluster" 不能只含空白`
	if verr := ValidateReleaseInput(ws); verr == nil || verr.Error() != wantErr {
		t.Fatalf("ValidateReleaseInput = %v, want %q", verr, wantErr)
	}
	p, perr := MakeReleasePlan(ws)
	if perr == nil || perr.Error() != wantErr {
		t.Fatalf("MakeReleasePlan err = %v, want %q", perr, wantErr)
	}
	assertZeroReleasePlan(t, p)

	// Missing and rejected anchors are planning failures, not config errors.
	missing := valid
	missing.FirstCluster = "nope"
	if _, err := MakeReleasePlan(missing); err == nil ||
		err.Error() != `指定的首批集群 "nope" 不在候选列表中` {
		t.Fatalf("missing anchor error = %v", err)
	}
	disabled := valid
	disabled.Clusters[2].Disabled = true
	p, err = MakeReleasePlan(disabled)
	if err == nil || err.Error() != `指定的首批集群 "c3" 未入选：`+ReasonDisabled {
		t.Fatalf("disabled anchor error = %v", err)
	}
	assertZeroReleasePlan(t, p)
}

// TestValidate_FirstClusterInvalidUTF8 covers the legal-text requirement in
// both representations: the JSON entry rejects before business validation
// with the document location, and a Go-built config names the field.
func TestValidate_FirstClusterInvalidUTF8(t *testing.T) {
	doc := `{"app":"app","revision":"r1","image":"img","batchSize":2,` +
		`"firstCluster":"c` + badUTF8 + `","clusters":[{"id":"c1"}]}`
	_, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatal("invalid UTF-8 in firstCluster must be rejected")
	}
	if !strings.Contains(err.Error(), "UTF-8") || !strings.Contains(err.Error(), "$.firstCluster") {
		t.Fatalf("error must identify invalid UTF-8 at $.firstCluster, got %v", err)
	}

	in := validInput()
	in.FirstCluster = badUTF8
	verr := ValidateReleaseInput(in)
	if verr == nil {
		t.Fatal("Go config with invalid UTF-8 firstCluster must be rejected")
	}
	if !strings.Contains(verr.Error(), "无效 UTF-8") || !strings.Contains(verr.Error(), `字段 "firstCluster"`) {
		t.Fatalf("error = %v, want the field-located UTF-8 failure", verr)
	}
}

// TestPlan_FirstClusterHugeCapacity keeps the overflow-safe arithmetic with
// an anchor set: a huge batchSize and a small first batch must still plan.
func TestPlan_FirstClusterHugeCapacity(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: testMaxInt, FirstBatchSize: 1, FirstCluster: "c",
		Clusters: []Cluster{{ID: "c"}, {ID: "a"}, {ID: "b"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("legal huge capacity with anchor must plan: %v", err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "c|a,b" {
		t.Fatalf("batches = %v, want [c]|[a b]", got)
	}
}

// TestPlan_FirstClusterDoesNotMutateInput: planning with an anchor leaves
// the caller's config untouched and is repeatable.
func TestPlan_FirstClusterDoesNotMutateInput(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img",
		BatchSize: 3, FirstBatchSize: 2, SpreadBy: "zone", FirstCluster: "d",
		Clusters: []Cluster{
			{ID: "a", Tags: map[string]string{"zone": "east"}},
			{ID: "b", Tags: map[string]string{"zone": "east"}},
			{ID: "d", Tags: map[string]string{"zone": "east"}},
			{ID: "c", Tags: map[string]string{"zone": "west"}},
			{ID: "e", Tags: map[string]string{"zone": "north"}},
		},
	}
	before := cloneInput(in)
	p1, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, before) {
		t.Fatalf("MakeReleasePlan mutated the input:\n before %#v\n after  %#v", before, in)
	}
	p2, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatal("repeated planning gave different plans")
	}
	if got := candidateIDs(in); !reflect.DeepEqual(got, []string{"a", "b", "d", "c", "e"}) {
		t.Fatalf("candidate order changed: %v", got)
	}
}

// TestPlanCLI_FirstClusterFullPlan drives the spec scenario through the
// command line: exit 0, the exact batches, plan-only stdout, empty stderr.
func TestPlanCLI_FirstClusterFullPlan(t *testing.T) {
	doc := `{
  "app": "payments-gateway",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments-gateway:2026.10.0-r3",
  "batchSize": 3,
  "firstBatchSize": 2,
  "firstCluster": "d",
  "spreadBy": "zone",
  "clusters": [` + specFirstClusterClustersB + `]
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
		"a": "east", "b": "east", "d": "east", "c": "west", "e": "north",
	}
	assertSpreadBatches(t, p,
		[][]string{{"c", "d"}, {"a", "e"}, {"b"}}, domains, 3)
}

// TestPlanCLI_FirstClusterFailuresFailCleanly: a missing or rejected anchor
// exits 1 with empty stdout and the reason on stderr; an illegal config value
// behaves exactly the same.
func TestPlanCLI_FirstClusterFailuresFailCleanly(t *testing.T) {
	cases := map[string]struct {
		doc     string
		wantErr string
	}{
		"not found": {
			`{"app":"app","revision":"r1","image":"img","batchSize":2,
			  "firstCluster":"nope","clusters":[{"id":"c1"}]}`,
			`指定的首批集群 "nope" 不在候选列表中`,
		},
		"disabled": {
			`{"app":"app","revision":"r1","image":"img","batchSize":2,
			  "firstCluster":"c2","clusters":[{"id":"c1"},{"id":"c2","disabled":true}]}`,
			`指定的首批集群 "c2" 未入选：` + ReasonDisabled,
		},
		"include filtered": {
			`{"app":"app","revision":"r1","image":"img","batchSize":2,
			  "firstCluster":"c2","include":[{"env":"prod"}],
			  "clusters":[{"id":"c1","tags":{"env":"prod"}},{"id":"c2"}]}`,
			`指定的首批集群 "c2" 未入选：` + ReasonIncludeNotMatched,
		},
		"whitespace only": {
			firstClusterDoc(2, "", `"  "`, `{"id":"c1"}`),
			`字段 "firstCluster" 不能只含空白`,
		},
		"non-string": {
			firstClusterDoc(2, "", `42`, `{"id":"c1"}`),
			`字段 "firstCluster" 必须是字符串`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, tc.doc))
			if code != 1 {
				t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must be empty on failure, got %q", stdout)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
		})
	}
}
