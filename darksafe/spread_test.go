package darksafe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spreadInput returns a config with spreadBy enabled: a,b,e are east, c,d are
// west, batchSize 2.
func spreadInput() ReleasePlanInput {
	return ReleasePlanInput{
		App:       "payments",
		Revision:  "v1.2.3",
		Image:     "reg/payments:v1.2.3",
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
}

func TestPlan_SpreadByExample(t *testing.T) {
	// a,b,e east; c,d west; batchSize 2 -> [a,c],[b,d],[e].
	// a,b are adjacent in the same domain, so the first batch must not be
	// shrunk to just a: c fills the open slot.
	plan, err := MakeReleasePlan(spreadInput())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a", "c"}, {"b", "d"}, {"e"}}
	if len(plan.Batches) != len(want) {
		t.Fatalf("expected %d batches, got %d: %+v", len(want), len(plan.Batches), plan.Batches)
	}
	for i, w := range want {
		if plan.Batches[i].Index != i+1 {
			t.Fatalf("batch %d index = %d", i, plan.Batches[i].Index)
		}
		if strings.Join(plan.Batches[i].Clusters, ",") != strings.Join(w, ",") {
			t.Fatalf("batch %d = %v, want %v", i, plan.Batches[i].Clusters, w)
		}
	}
	if len(plan.Excluded) != 0 {
		t.Fatalf("expected no exclusions, got %v", plan.Excluded)
	}
}

func TestPlan_SpreadBySingleDomain(t *testing.T) {
	// All clusters in one failure domain: one cluster per batch.
	in := spreadInput()
	in.Clusters = []Cluster{
		{ID: "a", Tags: map[string]string{"zone": "east"}},
		{ID: "b", Tags: map[string]string{"zone": "east"}},
		{ID: "c", Tags: map[string]string{"zone": "east"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a"}, {"b"}, {"c"}}
	if len(plan.Batches) != len(want) {
		t.Fatalf("expected %d batches, got %+v", len(want), plan.Batches)
	}
	for i, w := range want {
		if strings.Join(plan.Batches[i].Clusters, ",") != strings.Join(w, ",") {
			t.Fatalf("batch %d = %v, want %v", i, plan.Batches[i].Clusters, w)
		}
	}
}

func TestPlan_SpreadByEmptyStringValueIsDomain(t *testing.T) {
	// An empty tag value is a real failure domain, distinct from a missing
	// tag: a,b share the empty domain, c is west.
	in := spreadInput()
	in.Clusters = []Cluster{
		{ID: "a", Tags: map[string]string{"zone": ""}},
		{ID: "b", Tags: map[string]string{"zone": ""}},
		{ID: "c", Tags: map[string]string{"zone": "west"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a", "c"}, {"b"}}
	if len(plan.Batches) != len(want) {
		t.Fatalf("expected %d batches, got %+v", len(want), plan.Batches)
	}
	for i, w := range want {
		if strings.Join(plan.Batches[i].Clusters, ",") != strings.Join(w, ",") {
			t.Fatalf("batch %d = %v, want %v", i, plan.Batches[i].Clusters, w)
		}
	}
}

func TestPlan_SpreadByMissingTagFailsSmallestID(t *testing.T) {
	// Selected cluster b lacks the tag: plan fails naming b, the smallest
	// missing ID (c is also missing).
	in := spreadInput()
	in.Clusters = []Cluster{
		{ID: "a", Tags: map[string]string{"zone": "east"}},
		{ID: "b", Tags: map[string]string{"env": "prod"}},
		{ID: "c", Tags: map[string]string{"env": "prod"}},
	}
	_, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected error for missing spreadBy tag")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"b"`) || !strings.Contains(msg, `"zone"`) {
		t.Fatalf("error should name cluster b and tag zone, got %q", msg)
	}
}

func TestPlan_SpreadByExcludedClusterMissingTagIsFine(t *testing.T) {
	// Only selected clusters need the tag: disabled and include-filtered
	// clusters without it keep their original exclusion reasons.
	in := spreadInput()
	in.Include = []LabelCondition{{"env": "prod"}}
	in.Clusters = []Cluster{
		{ID: "a", Tags: map[string]string{"env": "prod", "zone": "east"}},
		{ID: "b", Disabled: true, Tags: map[string]string{"env": "prod"}},
		{ID: "c", Tags: map[string]string{"env": "dev"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "a" {
		t.Fatalf("expected only a in plan, got %+v", plan.Batches)
	}
	reasons := map[string]string{}
	for _, e := range plan.Excluded {
		reasons[e.ID] = e.Reason
	}
	if reasons["b"] != ReasonDisabled {
		t.Fatalf("b reason = %q, want %q", reasons["b"], ReasonDisabled)
	}
	if reasons["c"] != ReasonIncludeNotMatched {
		t.Fatalf("c reason = %q, want %q", reasons["c"], ReasonIncludeNotMatched)
	}
}

func TestPlan_SpreadByUnderfullBatchWhenDomainsRunOut(t *testing.T) {
	// batchSize 3 but only two domains: the last batch is underfull.
	in := spreadInput()
	in.BatchSize = 3
	in.Clusters = []Cluster{
		{ID: "a", Tags: map[string]string{"zone": "east"}},
		{ID: "b", Tags: map[string]string{"zone": "east"}},
		{ID: "c", Tags: map[string]string{"zone": "west"}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a", "c"}, {"b"}}
	if len(plan.Batches) != len(want) {
		t.Fatalf("expected %d batches, got %+v", len(want), plan.Batches)
	}
	for i, w := range want {
		if strings.Join(plan.Batches[i].Clusters, ",") != strings.Join(w, ",") {
			t.Fatalf("batch %d = %v, want %v", i, plan.Batches[i].Clusters, w)
		}
	}
}

func TestPlan_SpreadByEmptyStringDisablesSpreading(t *testing.T) {
	// Explicit empty string behaves exactly like the field being absent.
	plain := validInput()
	withEmpty := validInput()
	withEmpty.SpreadBy = ""
	p1, err := MakeReleasePlan(plain)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(withEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p1.Batches[0].Clusters, ",") != strings.Join(p2.Batches[0].Clusters, ",") {
		t.Fatalf("empty spreadBy must not change batching: %+v vs %+v", p1.Batches, p2.Batches)
	}
}

func TestPlan_SpreadByDeterministicRegardlessOfOrder(t *testing.T) {
	base := spreadInput()
	shuffled := spreadInput()
	shuffled.Clusters = []Cluster{
		{ID: "e", Tags: map[string]string{"zone": "east"}},
		{ID: "c", Tags: map[string]string{"zone": "west"}},
		{ID: "a", Tags: map[string]string{"zone": "east"}},
		{ID: "d", Tags: map[string]string{"zone": "west"}},
		{ID: "b", Tags: map[string]string{"zone": "east"}},
	}
	shuffled.Include = []LabelCondition{{"zone": "east"}, {"zone": "west"}}
	p1, err := MakeReleasePlan(base)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Batches) != len(p2.Batches) {
		t.Fatalf("batch count differs: %d vs %d", len(p1.Batches), len(p2.Batches))
	}
	for i := range p1.Batches {
		if strings.Join(p1.Batches[i].Clusters, ",") != strings.Join(p2.Batches[i].Clusters, ",") {
			t.Fatalf("batch %d differs: %v vs %v", i, p1.Batches[i].Clusters, p2.Batches[i].Clusters)
		}
	}
	// Repeated generation is identical.
	p3, err := MakeReleasePlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p1.Batches[0].Clusters, ",") != strings.Join(p3.Batches[0].Clusters, ",") {
		t.Fatal("repeated planning gave different results")
	}
}

func TestPlan_SpreadByDoesNotMutateInput(t *testing.T) {
	in := spreadInput()
	before := ""
	for _, c := range in.Clusters {
		before += c.ID + ":" + c.Tags["zone"] + ";"
	}
	if _, err := MakeReleasePlan(in); err != nil {
		t.Fatal(err)
	}
	after := ""
	for _, c := range in.Clusters {
		after += c.ID + ":" + c.Tags["zone"] + ";"
	}
	if after != before {
		t.Fatalf("MakeReleasePlan mutated input: before=%q after=%q", before, after)
	}
}

func TestPlan_SpreadByStructAndJSONGiveSamePlan(t *testing.T) {
	raw := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2, "spreadBy": "zone",
		"clusters": [
			{"id": "a", "tags": {"zone": "east"}},
			{"id": "b", "tags": {"zone": "east"}},
			{"id": "c", "tags": {"zone": "west"}},
			{"id": "d", "tags": {"zone": "west"}},
			{"id": "e", "tags": {"zone": "east"}}
		]
	}`
	fromJSON, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	p1, err := MakeReleasePlan(fromJSON)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(spreadInput())
	if err != nil {
		t.Fatal(err)
	}
	for i := range p1.Batches {
		if strings.Join(p1.Batches[i].Clusters, ",") != strings.Join(p2.Batches[i].Clusters, ",") {
			t.Fatalf("batch %d differs between JSON and struct: %v vs %v", i, p1.Batches[i].Clusters, p2.Batches[i].Clusters)
		}
	}
}

func TestPlan_SpreadByEmptyCandidatesKeepsOriginalFailure(t *testing.T) {
	// spreadBy set but no candidates: the original failure wins.
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "i", BatchSize: 2, SpreadBy: "zone",
	}
	_, err := MakeReleasePlan(in)
	if err == nil || !strings.Contains(err.Error(), "未提供候选集群") {
		t.Fatalf("expected 未提供候选集群, got %v", err)
	}
}

func TestPlan_SpreadByAllExcludedKeepsOriginalFailure(t *testing.T) {
	// spreadBy set but nothing selected: the original failure wins.
	in := spreadInput()
	in.Clusters = []Cluster{
		{ID: "a", Disabled: true, Tags: map[string]string{"zone": "east"}},
		{ID: "b", Tags: map[string]string{"env": "dev"}},
	}
	in.Include = []LabelCondition{{"env": "prod"}}
	_, err := MakeReleasePlan(in)
	if err == nil || !strings.Contains(err.Error(), "没有符合规则的可用集群") {
		t.Fatalf("expected 没有符合规则的可用集群, got %v", err)
	}
}

func TestParse_SpreadByValidationErrors(t *testing.T) {
	cases := map[string]string{
		"whitespace": `{"app":"a","revision":"r","image":"i","batchSize":1,"spreadBy":"  ","clusters":[]}`,
		"tabs":       `{"app":"a","revision":"r","image":"i","batchSize":1,"spreadBy":"\t","clusters":[]}`,
		"non-string": `{"app":"a","revision":"r","image":"i","batchSize":1,"spreadBy":5,"clusters":[]}`,
		"null":       `{"app":"a","revision":"r","image":"i","batchSize":1,"spreadBy":null,"clusters":[]}`,
		"object":     `{"app":"a","revision":"r","image":"i","batchSize":1,"spreadBy":{},"clusters":[]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseReleaseInput([]byte(raw)); err == nil {
				t.Fatalf("expected spreadBy error for %s", name)
			} else if !strings.Contains(err.Error(), "spreadBy") {
				t.Fatalf("error should name spreadBy, got %v", err)
			}
		})
	}
}

func TestValidate_SpreadByWhitespaceRejected(t *testing.T) {
	in := validInput()
	in.SpreadBy = "   "
	if err := ValidateReleaseInput(in); err == nil {
		t.Fatal("expected whitespace spreadBy to be rejected")
	} else if !strings.Contains(err.Error(), "spreadBy") {
		t.Fatalf("error should name spreadBy, got %v", err)
	}
	if _, err := MakeReleasePlan(in); err == nil {
		t.Fatal("MakeReleasePlan must reject whitespace spreadBy too")
	}
}

func TestValidate_SpreadByEmptyStringAccepted(t *testing.T) {
	in := validInput()
	in.SpreadBy = ""
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("empty spreadBy must be valid: %v", err)
	}
}

func TestPlanCLI_SpreadByPlans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2, "spreadBy": "zone",
		"clusters": [
			{"id": "a", "tags": {"zone": "east"}},
			{"id": "b", "tags": {"zone": "east"}},
			{"id": "c", "tags": {"zone": "west"}},
			{"id": "d", "tags": {"zone": "west"}},
			{"id": "e", "tags": {"zone": "east"}}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, `"a"`) || !strings.Contains(stdout, `"c"`) {
		t.Fatalf("expected spread plan on stdout, got %q", stdout)
	}
	// First batch must pair a with c, not a with b.
	if !strings.Contains(stdout, `"clusters": [
        "a",
        "c"
      ]`) {
		t.Fatalf("expected first batch [a,c], got %q", stdout)
	}
}

func TestPlanCLI_SpreadByMissingTagFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2, "spreadBy": "zone",
		"clusters": [
			{"id": "a", "tags": {"zone": "east"}},
			{"id": "b", "tags": {"env": "prod"}}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, `"b"`) || !strings.Contains(stderr, `"zone"`) {
		t.Fatalf("stderr should name cluster b and tag zone, got %q", stderr)
	}
}

func TestPlanCLI_SpreadByWhitespaceRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2, "spreadBy": "   ",
		"clusters": [{"id": "a"}]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "spreadBy") {
		t.Fatalf("stderr should name spreadBy, got %q", stderr)
	}
}
