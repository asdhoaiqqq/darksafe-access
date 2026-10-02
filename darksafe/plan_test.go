package darksafe

import (
	"fmt"
	"strings"
	"testing"
	"time"
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

// validInput returns a well-formed input for library-path tests.
func validInput() ReleasePlanInput {
	return ReleasePlanInput{
		App:       "payments",
		Revision:  "v1.2.3",
		Image:     "reg/payments:v1.2.3",
		BatchSize: 2,
		Clusters: []Cluster{
			{ID: "c3"},
			{ID: "c1"},
			{ID: "c2"},
			{ID: "c4"},
			{ID: "c5"},
		},
	}
}

func TestValidate_LibraryPathRejectsInvalidConfig(t *testing.T) {
	cases := map[string]func(*ReleasePlanInput){
		"app empty":           func(in *ReleasePlanInput) { in.App = "" },
		"app whitespace":      func(in *ReleasePlanInput) { in.App = "  \t\n" },
		"revision empty":      func(in *ReleasePlanInput) { in.Revision = "" },
		"revision whitespace": func(in *ReleasePlanInput) { in.Revision = " " },
		"image empty":         func(in *ReleasePlanInput) { in.Image = "" },
		"image whitespace":    func(in *ReleasePlanInput) { in.Image = "\t" },
		"batch zero":          func(in *ReleasePlanInput) { in.BatchSize = 0 },
		"batch negative":      func(in *ReleasePlanInput) { in.BatchSize = -1 },
		"id empty":            func(in *ReleasePlanInput) { in.Clusters[0].ID = "" },
		"id whitespace":       func(in *ReleasePlanInput) { in.Clusters[0].ID = "  " },
		"duplicate id":        func(in *ReleasePlanInput) { in.Clusters[1].ID = "c3" },
		"empty tag key":       func(in *ReleasePlanInput) { in.Clusters[0].Tags = map[string]string{"": "v"} },
		"empty include cond":  func(in *ReleasePlanInput) { in.Include = []LabelCondition{{}} },
		"empty include key":   func(in *ReleasePlanInput) { in.Include = []LabelCondition{{"": "v"}} },
		"empty exclude cond":  func(in *ReleasePlanInput) { in.Exclude = []LabelCondition{{}} },
		"empty exclude key":   func(in *ReleasePlanInput) { in.Exclude = []LabelCondition{{"": "v"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			mutate(&in)
			if err := ValidateReleaseInput(in); err == nil {
				t.Fatalf("ValidateReleaseInput: expected error for %s", name)
			}
			if _, err := MakeReleasePlan(in); err == nil {
				t.Fatalf("MakeReleasePlan: expected error for %s", name)
			}
		})
	}
}

func TestValidate_DuplicateIncludingDisabledAndFiltered(t *testing.T) {
	base := validInput()
	// Duplicate of a disabled cluster is still an error.
	disabledDup := base
	disabledDup.Clusters = append([]Cluster{{ID: "x", Disabled: true}}, Cluster{ID: "x"})
	if err := ValidateReleaseInput(disabledDup); err == nil {
		t.Fatal("duplicate of disabled cluster must be rejected")
	} else if !strings.Contains(err.Error(), `"x"`) {
		t.Fatalf("duplicate error should name the conflicting id, got %v", err)
	}
	// Duplicate of a cluster that would be filtered out is still an error.
	filteredDup := base
	filteredDup.Clusters = []Cluster{
		{ID: "x", Tags: map[string]string{"env": "dev"}},
		{ID: "x", Tags: map[string]string{"env": "prod"}},
	}
	filteredDup.Include = []LabelCondition{{"env": "prod"}}
	if err := ValidateReleaseInput(filteredDup); err == nil {
		t.Fatal("duplicate of filtered-out cluster must be rejected")
	}
}

func TestValidate_ErrorOrdering(t *testing.T) {
	// Multiple problems at once: app wins over everything else.
	in := validInput()
	in.App = " "
	in.Revision = ""
	in.Image = ""
	in.BatchSize = 0
	in.Clusters[0].ID = ""
	in.Clusters[1].ID = "c3"
	in.Clusters[0].Tags = map[string]string{"": "v"}
	in.Include = []LabelCondition{{}}
	in.Exclude = []LabelCondition{{}}
	err := ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("expected app error first, got %v", err)
	}

	// With app fixed, revision is next.
	in.App = "payments"
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("expected revision error next, got %v", err)
	}

	// Then image.
	in.Revision = "r"
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("expected image error next, got %v", err)
	}

	// Then batchSize.
	in.Image = "i"
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "batchSize") {
		t.Fatalf("expected batchSize error next, got %v", err)
	}

	// Then clusters: id problem before tag problem on the same cluster.
	in.BatchSize = 2
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "clusters[0]") || !strings.Contains(err.Error(), `"id"`) {
		t.Fatalf("expected clusters[0] id error, got %v", err)
	}

	// Fix the id; the empty tag key on the same cluster is next.
	in.Clusters[0].ID = "c3"
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "clusters[0]") || !strings.Contains(err.Error(), "标签键") {
		t.Fatalf("expected clusters[0] tag-key error, got %v", err)
	}

	// Then include conditions.
	in.Clusters[0].Tags = nil
	in.Clusters[1].ID = "c1"
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "include[0]") {
		t.Fatalf("expected include[0] error, got %v", err)
	}

	// Then exclude conditions.
	in.Include = nil
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "exclude[0]") {
		t.Fatalf("expected exclude[0] error, got %v", err)
	}
}

func TestValidate_EmptyClustersDoesNotMaskOtherErrors(t *testing.T) {
	in := ReleasePlanInput{App: "a", Revision: "r", Image: "i", BatchSize: 0}
	err := ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "batchSize") {
		t.Fatalf("empty clusters must not mask batchSize error, got %v", err)
	}
	in.BatchSize = 2
	in.Include = []LabelCondition{{}}
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "include[0]") {
		t.Fatalf("empty clusters must not mask include error, got %v", err)
	}
	// With everything valid and no candidates, planning still fails the old way.
	in.Include = nil
	_, err = MakeReleasePlan(in)
	if err == nil || !strings.Contains(err.Error(), "未提供候选集群") {
		t.Fatalf("expected 未提供候选集群, got %v", err)
	}
}

func TestValidate_TagErrorsAreDeterministic(t *testing.T) {
	// Struct path: only empty keys are invalid; sorted keys pick the first.
	in := validInput()
	in.Clusters[0].Tags = map[string]string{"z": "v", "a": "v", "": "v", "m": "v"}
	var msgs []string
	for range 5 {
		err := ValidateReleaseInput(in)
		if err == nil {
			t.Fatal("expected error")
		}
		msgs = append(msgs, err.Error())
	}
	for _, m := range msgs[1:] {
		if m != msgs[0] {
			t.Fatalf("non-deterministic errors: %q vs %q", m, msgs[0])
		}
	}
	if !strings.Contains(msgs[0], "clusters[0]") || !strings.Contains(msgs[0], "标签键") {
		t.Fatalf("expected clusters[0] empty tag key error, got %q", msgs[0])
	}

	// JSON path: multiple non-string values -> alphabetically first key wins.
	raw := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x","tags":{"z":1,"a":2,"m":3}}]}`
	var jmsgs []string
	for range 5 {
		_, err := ParseReleaseInput([]byte(raw))
		if err == nil {
			t.Fatal("expected parse error")
		}
		jmsgs = append(jmsgs, err.Error())
	}
	for _, m := range jmsgs[1:] {
		if m != jmsgs[0] {
			t.Fatalf("non-deterministic JSON errors: %q vs %q", m, jmsgs[0])
		}
	}
	if !strings.Contains(jmsgs[0], `"a"`) {
		t.Fatalf("expected error to name key \"a\", got %q", jmsgs[0])
	}
}

func TestValidate_DoesNotMutateInput(t *testing.T) {
	in := validInput()
	in.Clusters[0].Tags = map[string]string{"env": "prod"}
	in.Include = []LabelCondition{{"env": "prod"}}
	before := fmt.Sprintf("%+v", in)
	if _, err := MakeReleasePlan(in); err != nil {
		t.Fatal(err)
	}
	if after := fmt.Sprintf("%+v", in); after != before {
		t.Fatalf("MakeReleasePlan mutated input:\n before: %s\n after:  %s", before, after)
	}
	// Same config used twice yields identical plans.
	p1, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", p1) != fmt.Sprintf("%+v", p2) {
		t.Fatal("repeated planning gave different results")
	}
}

func TestValidate_StructAndJSONGiveSamePlan(t *testing.T) {
	raw := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3"}, {"id": "c1"}, {"id": "c2"}, {"id": "c4"}, {"id": "c5"}
		]
	}`
	fromJSON, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	fromStruct := validInput()
	p1, err := MakeReleasePlan(fromJSON)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := MakeReleasePlan(fromStruct)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", p1) != fmt.Sprintf("%+v", p2) {
		t.Fatalf("plans differ: %+v vs %+v", p1, p2)
	}
}

func TestValidate_InvalidStructAndJSONGiveSameError(t *testing.T) {
	// Duplicate id: both paths must reject with the same field/position info.
	raw := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x"},{"id":"x"}]}`
	_, jerr := ParseReleaseInput([]byte(raw))
	if jerr == nil {
		t.Fatal("expected JSON error")
	}
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "i", BatchSize: 1,
		Clusters: []Cluster{{ID: "x"}, {ID: "x"}},
	}
	serr := ValidateReleaseInput(in)
	if serr == nil {
		t.Fatal("expected struct error")
	}
	if jerr.Error() != serr.Error() {
		t.Fatalf("errors differ:\n JSON: %v\n struct: %v", jerr, serr)
	}
}

func TestValidate_BatchZeroDoesNotHang(t *testing.T) {
	done := make(chan struct{})
	go func() {
		in := validInput()
		in.BatchSize = 0
		_, _ = MakeReleasePlan(in)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MakeReleasePlan with batchSize=0 did not return")
	}
}

func TestValidate_BatchNegativeDoesNotPanic(t *testing.T) {
	in := validInput()
	in.BatchSize = -1
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("MakeReleasePlan panicked: %v", r)
			}
		}()
		_, err = MakeReleasePlan(in)
	}()
	if err == nil {
		t.Fatal("expected error for negative batchSize")
	}
}
