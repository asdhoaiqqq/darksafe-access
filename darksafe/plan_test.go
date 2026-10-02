package darksafe

import (
	"strings"
	"testing"
)

func parsePlan(t *testing.T, data string) ReleasePlanInput {
	t.Helper()
	in, err := ParseReleaseInput([]byte(data))
	if err != nil {
		t.Fatalf("ParseReleaseInput: %v", err)
	}
	return in
}

func TestPlan_BasicBatchingAndSorting(t *testing.T) {
	in := parsePlan(t, `{
		"app": "payments",
		"revision": "v1.2.3",
		"image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3"},
			{"id": "c1"},
			{"id": "c2"},
			{"id": "c4"},
			{"id": "c5"}
		]
	}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.App.Name != "payments" || plan.App.Revision != "v1.2.3" || plan.App.Image != "reg/payments:v1.2.3" {
		t.Fatalf("app info mismatch: %+v", plan.App)
	}
	if len(plan.Batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(plan.Batches))
	}
	want := [][]string{{"c1", "c2"}, {"c3", "c4"}, {"c5"}}
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

func TestPlan_IncludeExcludeAndPriority(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app",
		"revision": "r1",
		"image": "img",
		"batchSize": 10,
		"clusters": [
			{"id": "a", "tags": {"env": "prod", "region": "us"}},
			{"id": "b", "tags": {"env": "prod", "region": "eu"}},
			{"id": "c", "tags": {"env": "dev"}},
			{"id": "d", "tags": {"env": "prod"}, "disabled": true},
			{"id": "e", "tags": {"env": "prod", "region": "us", "spot": "true"}}
		],
		"include": [{"env": "prod"}],
		"exclude": [{"region": "eu"}, {"spot": "true"}]
	}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "a" {
		t.Fatalf("expected only [a], got %+v", plan.Batches)
	}
	reasons := map[string]string{}
	for _, e := range plan.Excluded {
		reasons[e.ID] = e.Reason
	}
	if reasons["b"] != ReasonExcludeMatched {
		t.Fatalf("b reason = %q", reasons["b"])
	}
	if reasons["c"] != ReasonIncludeNotMatched {
		t.Fatalf("c reason = %q", reasons["c"])
	}
	if reasons["d"] != ReasonDisabled {
		t.Fatalf("d reason = %q", reasons["d"])
	}
	if reasons["e"] != ReasonExcludeMatched {
		t.Fatalf("e reason = %q", reasons["e"])
	}
}

func TestPlan_EmptyIncludeAllowsAll(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 10,
		"clusters": [{"id": "x", "tags": {"env": "dev"}}],
		"include": []
	}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "x" {
		t.Fatalf("expected x selected, got %+v", plan.Batches)
	}
}

func TestPlan_EmptyStringTagValueMatches(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 10,
		"clusters": [{"id": "x", "tags": {"env": ""}}],
		"include": [{"env": ""}]
	}`)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "x" {
		t.Fatalf("empty string value should match, got %+v", plan.Batches)
	}
}

func TestPlan_TagMissingDoesNotMatch(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 10,
		"clusters": [{"id": "x", "tags": {"env": "prod"}}],
		"include": [{"env": "prod", "region": "us"}]
	}`)
	_, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected error when no cluster matches include")
	}
	if !strings.Contains(err.Error(), "x："+ReasonIncludeNotMatched) {
		t.Fatalf("expected reason in %q", err.Error())
	}
}

func TestPlan_DeterministicRegardlessOfOrder(t *testing.T) {
	base := `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 2,
		"clusters": [
			{"id": "a", "tags": {"env": "prod", "region": "us"}},
			{"id": "b", "tags": {"env": "dev"}},
			{"id": "c", "tags": {"env": "prod"}, "disabled": true}
		],
		"include": [{"env": "prod", "region": "us"}, {"env": "dev"}]
	}`
	shuffled := `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 2,
		"clusters": [
			{"id": "c", "tags": {"env": "prod"}, "disabled": true},
			{"id": "b", "tags": {"env": "dev"}},
			{"id": "a", "tags": {"region": "us", "env": "prod"}}
		],
		"include": [{"env": "dev"}, {"region": "us", "env": "prod"}]
	}`
	p1, err := MakeReleasePlan(parsePlan(t, base))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(parsePlan(t, shuffled))
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Batches) != len(p2.Batches) || len(p1.Excluded) != len(p2.Excluded) {
		t.Fatalf("plans differ: %+v vs %+v", p1, p2)
	}
	for i := range p1.Batches {
		if strings.Join(p1.Batches[i].Clusters, ",") != strings.Join(p2.Batches[i].Clusters, ",") {
			t.Fatalf("batch %d differs", i)
		}
	}
	for i := range p1.Excluded {
		if p1.Excluded[i] != p2.Excluded[i] {
			t.Fatalf("excluded %d differs: %+v vs %+v", i, p1.Excluded[i], p2.Excluded[i])
		}
	}
}

func TestPlan_EmptyCandidateListFails(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 2,
		"clusters": []
	}`)
	_, err := MakeReleasePlan(in)
	if err == nil || !strings.Contains(err.Error(), "未提供候选集群") {
		t.Fatalf("expected 未提供候选集群 error, got %v", err)
	}
}

func TestPlan_AllExcludedFailsWithReasons(t *testing.T) {
	in := parsePlan(t, `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 2,
		"clusters": [
			{"id": "z", "disabled": true},
			{"id": "y", "tags": {"env": "dev"}}
		],
		"include": [{"env": "prod"}]
	}`)
	_, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "没有符合规则的可用集群") {
		t.Fatalf("missing summary in %q", msg)
	}
	if !strings.Contains(msg, "y："+ReasonIncludeNotMatched) {
		t.Fatalf("missing y reason in %q", msg)
	}
	if !strings.Contains(msg, "z："+ReasonDisabled) {
		t.Fatalf("missing z reason in %q", msg)
	}
}

func TestParse_ValidationErrors(t *testing.T) {
	cases := map[string]string{
		"app empty":        `{"app": "  ", "revision": "r", "image": "i", "batchSize": 1, "clusters": []}`,
		"app missing":      `{"revision": "r", "image": "i", "batchSize": 1, "clusters": []}`,
		"revision empty":   `{"app": "a", "revision": "", "image": "i", "batchSize": 1, "clusters": []}`,
		"image empty":      `{"app": "a", "revision": "r", "image": "\t", "batchSize": 1, "clusters": []}`,
		"id empty":         `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [{"id": " "}]}`,
		"id missing":       `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [{}]}`,
		"duplicate id":     `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [{"id": "x"}, {"id": "x"}]}`,
		"batch zero":       `{"app": "a", "revision": "r", "image": "i", "batchSize": 0, "clusters": []}`,
		"batch negative":   `{"app": "a", "revision": "r", "image": "i", "batchSize": -1, "clusters": []}`,
		"batch float":      `{"app": "a", "revision": "r", "image": "i", "batchSize": 1.5, "clusters": []}`,
		"batch string":     `{"app": "a", "revision": "r", "image": "i", "batchSize": "2", "clusters": []}`,
		"clusters missing": `{"app": "a", "revision": "r", "image": "i", "batchSize": 1}`,
		"clusters object":  `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": {}}`,
		"empty tag key":    `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [{"id": "x", "tags": {"": "v"}}]}`,
		"non-string tag":   `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [{"id": "x", "tags": {"env": 1}}]}`,
		"empty condition":  `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [], "include": [{}]}`,
		"cond empty key":   `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [], "exclude": [{"": "v"}]}`,
		"cond non-string":  `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [], "include": [{"env": true}]}`,
		"disabled string":  `{"app": "a", "revision": "r", "image": "i", "batchSize": 1, "clusters": [{"id": "x", "disabled": "yes"}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseReleaseInput([]byte(input)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestParse_MalformedJSON(t *testing.T) {
	if _, err := ParseReleaseInput([]byte(`{not json`)); err == nil {
		t.Fatal("expected JSON error")
	}
	if _, err := ParseReleaseInput([]byte(`{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]} trailing`)); err == nil {
		t.Fatal("expected trailing content error")
	}
}
