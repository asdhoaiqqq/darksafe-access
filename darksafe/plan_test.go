package darksafe

import (
	"reflect"
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

func validInput() ReleasePlanInput {
	return ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		Clusters: []Cluster{
			{ID: "c1", Tags: map[string]string{"env": "prod"}},
			{ID: "c2"},
		},
	}
}

func TestMake_InvalidScalars(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ReleasePlanInput)
		want string
	}{
		{"app empty", func(in *ReleasePlanInput) { in.App = "  " }, `"app"`},
		{"revision empty", func(in *ReleasePlanInput) { in.Revision = "\t" }, `"revision"`},
		{"image empty", func(in *ReleasePlanInput) { in.Image = "" }, `"image"`},
		{"batch zero", func(in *ReleasePlanInput) { in.BatchSize = 0 }, `"batchSize"`},
		{"batch negative", func(in *ReleasePlanInput) { in.BatchSize = -3 }, `"batchSize"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mut(&in)
			plan, err := MakeReleasePlan(in)
			if err == nil {
				t.Fatalf("expected error, got plan %+v", plan)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %s", err.Error(), tc.want)
			}
			if !reflect.DeepEqual(plan, ReleasePlan{}) {
				t.Fatalf("invalid input must not produce a partial plan, got %+v", plan)
			}
		})
	}
}

func TestMake_DuplicateClusterIDRejected(t *testing.T) {
	in := validInput()
	in.Clusters = []Cluster{
		{ID: "dup", Disabled: true},
		{ID: "other"},
		{ID: "dup"},
	}
	_, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected duplicate ID error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"dup"`) {
		t.Fatalf("error must name the conflicting ID, got %q", msg)
	}
	if !strings.Contains(msg, "clusters[2]") || !strings.Contains(msg, "clusters[0]") {
		t.Fatalf("error must locate both list positions, got %q", msg)
	}
}

func TestMake_DuplicateIDCheckedEvenWhenFiltered(t *testing.T) {
	// Both copies are disabled and would never be selected; the duplicate
	// must still be rejected before selection runs.
	in := validInput()
	in.Clusters = []Cluster{{ID: "x", Disabled: true}, {ID: "x", Disabled: true}}
	if _, err := MakeReleasePlan(in); err == nil {
		t.Fatal("duplicate disabled clusters must be rejected")
	}

	// A valid input with no selectable clusters keeps the existing failure.
	in.Clusters = []Cluster{{ID: "x", Disabled: true}}
	if _, err := MakeReleasePlan(in); err == nil ||
		!strings.Contains(err.Error(), "没有符合规则的可用集群") {
		t.Fatalf("expected no-available-cluster failure, got %v", err)
	}
}

func TestMake_EmptyCandidatesDoNotMaskOtherErrors(t *testing.T) {
	in := validInput()
	in.Clusters = nil
	in.Include = []LabelCondition{{}}
	if _, err := MakeReleasePlan(in); err == nil ||
		!strings.Contains(err.Error(), "include[0] 不能为空条件对象") {
		t.Fatalf("empty cluster list must not mask the invalid condition, got %v", err)
	}

	// An empty condition list is still "no restriction" and stays valid.
	in.Include = []LabelCondition{}
	in.Exclude = []LabelCondition{}
	if _, err := MakeReleasePlan(in); err == nil ||
		!strings.Contains(err.Error(), "未提供候选集群") {
		t.Fatalf("valid empty input should reach the no-candidates failure, got %v", err)
	}
}

func TestMake_ClusterAndConditionValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ReleasePlanInput)
		want string
	}{
		{"blank cluster id", func(in *ReleasePlanInput) {
			in.Clusters[0].ID = " "
		}, "clusters[0]"},
		{"empty tag key", func(in *ReleasePlanInput) {
			in.Clusters[0].Tags = map[string]string{"": "v"}
		}, "clusters[0] 的标签键不能为空"},
		{"empty include condition", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{{}}
		}, "include[0] 不能为空条件对象"},
		{"empty include key", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{{"": "v"}}
		}, "include[0] 的标签键不能为空"},
		{"empty exclude condition", func(in *ReleasePlanInput) {
			in.Exclude = []LabelCondition{{}}
		}, "exclude[0] 不能为空条件对象"},
		{"empty exclude key", func(in *ReleasePlanInput) {
			in.Exclude = []LabelCondition{{"": "v"}}
		}, "exclude[0] 的标签键不能为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mut(&in)
			if _, err := MakeReleasePlan(in); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMake_FirstErrorOrdering(t *testing.T) {
	// Everything is wrong at once: the scalar order must win over clusters,
	// clusters over include, include over exclude.
	in := ReleasePlanInput{
		App:       " ",
		Revision:  "",
		Image:     "",
		BatchSize: 0,
		Clusters:  []Cluster{{ID: "x"}, {ID: "x"}},
		Include:   []LabelCondition{{}},
		Exclude:   []LabelCondition{{"": "v"}},
	}
	steps := []struct {
		want string
		fix  func(*ReleasePlanInput)
	}{
		{`"app"`, func(in *ReleasePlanInput) { in.App = "app" }},
		{`"revision"`, func(in *ReleasePlanInput) { in.Revision = "r1" }},
		{`"image"`, func(in *ReleasePlanInput) { in.Image = "img" }},
		{`"batchSize"`, func(in *ReleasePlanInput) { in.BatchSize = 1 }},
		{"重复的集群标识", func(in *ReleasePlanInput) { in.Clusters = []Cluster{{ID: "x"}} }},
		{"include[0]", func(in *ReleasePlanInput) { in.Include = nil }},
		{"exclude[0]", func(in *ReleasePlanInput) { in.Exclude = nil }},
	}
	for _, s := range steps {
		_, err := MakeReleasePlan(in)
		if err == nil || !strings.Contains(err.Error(), s.want) {
			t.Fatalf("want error containing %q, got %v", s.want, err)
		}
		s.fix(&in)
	}
	if _, err := MakeReleasePlan(in); err != nil {
		t.Fatalf("fully fixed input should be valid, got %v", err)
	}
}

func TestMake_IDErrorBeforeTagError(t *testing.T) {
	in := validInput()
	in.Clusters = []Cluster{{ID: "x"}, {ID: "x", Tags: map[string]string{"": "v"}}}
	_, err := MakeReleasePlan(in)
	if err == nil || !strings.Contains(err.Error(), "重复的集群标识") {
		t.Fatalf("duplicate ID must be reported before the tag-key error, got %v", err)
	}
}

func TestMake_TagErrorKeyOrderStable(t *testing.T) {
	// Errors must be identical across repeated calls regardless of map
	// iteration order; distinct invalid keys are exercised on the JSON path
	// where they can coexist.
	in := validInput()
	in.Clusters[0].Tags = map[string]string{"": "v", "env": "prod"}
	_, first := MakeReleasePlan(in)
	if first == nil {
		t.Fatal("expected empty-tag-key error")
	}
	for i := 0; i < 10; i++ {
		_, err := MakeReleasePlan(in)
		if err == nil || err.Error() != first.Error() {
			t.Fatalf("error message varies across calls: %v vs %v", err, first)
		}
	}

	jsonInput := `{
		"app": "app", "revision": "r1", "image": "img", "batchSize": 1,
		"clusters": [{"id": "x", "tags": {"zzz": 1, "aaa": 2}}]
	}`
	_, jerr := ParseReleaseInput([]byte(jsonInput))
	if jerr == nil || !strings.Contains(jerr.Error(), `"aaa"`) {
		t.Fatalf("invalid tag keys must be reported in ascending order, got %v", jerr)
	}
}

func TestParse_LibraryAndJSONAgree(t *testing.T) {
	// The same invalid configuration must be rejected through both entries.
	docs := map[string]string{
		"duplicate": `{"app":"a","revision":"r","image":"i","batchSize":2,
			"clusters":[{"id":"x","disabled":true},{"id":"x"}]}`,
		"cond": `{"app":"a","revision":"r","image":"i","batchSize":2,
			"clusters":[],"include":[{}]}`,
		"tags": `{"app":"a","revision":"r","image":"i","batchSize":2,
			"clusters":[{"id":"x","tags":{"":"v"}}]}`,
	}
	structs := map[string]ReleasePlanInput{
		"duplicate": {
			App: "a", Revision: "r", Image: "i", BatchSize: 2,
			Clusters: []Cluster{{ID: "x", Disabled: true}, {ID: "x"}},
		},
		"cond": {
			App: "a", Revision: "r", Image: "i", BatchSize: 2,
			Clusters: []Cluster{}, Include: []LabelCondition{{}},
		},
		"tags": {
			App: "a", Revision: "r", Image: "i", BatchSize: 2,
			Clusters: []Cluster{{ID: "x", Tags: map[string]string{"": "v"}}},
		},
	}
	for name, doc := range docs {
		if _, err := ParseReleaseInput([]byte(doc)); err == nil {
			t.Fatalf("%s: JSON path accepted invalid input", name)
		}
		if _, err := MakeReleasePlan(structs[name]); err == nil {
			t.Fatalf("%s: struct path accepted invalid input", name)
		}
	}
}

func TestMake_NeverMutatesInput(t *testing.T) {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		Clusters: []Cluster{
			{ID: "c3", Tags: map[string]string{"env": "prod"}},
			{ID: "c1"},
			{ID: "c2", Disabled: true},
		},
		Include: []LabelCondition{{"env": "prod"}},
		Exclude: []LabelCondition{},
	}
	snapshot := in
	tags := in.Clusters[0].Tags

	plan1, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input was mutated:\nbefore %+v\nafter  %+v", snapshot, in)
	}
	if got := tags["env"]; got != "prod" {
		t.Fatalf("caller-owned tag map was mutated: %q", got)
	}
	if !reflect.DeepEqual(plan1, plan2) {
		t.Fatalf("repeated calls differ: %+v vs %+v", plan1, plan2)
	}

	// Values are preserved verbatim: no trimming, case folding or merging.
	if got := in.Clusters[0].Tags["env"]; got != "prod" {
		t.Fatalf("tag value changed: %q", got)
	}
	if plan1.App.Name != "app" {
		t.Fatalf("app name changed: %q", plan1.App.Name)
	}
}

func TestMake_NilTagsEquivalentToEmptyTags(t *testing.T) {
	in := validInput()
	in.Clusters = []Cluster{
		{ID: "a"}, // nil tags
		{ID: "b", Tags: map[string]string{}},
	}
	in.Include = []LabelCondition{{"env": "prod"}}
	_, err := MakeReleasePlan(in)
	if err == nil || !strings.Contains(err.Error(), ReasonIncludeNotMatched) {
		t.Fatalf("nil and empty tags should both fail the include, got %v", err)
	}
}

func TestMake_EmptyTagValueStillMatches(t *testing.T) {
	in := validInput()
	in.Clusters = []Cluster{{ID: "x", Tags: map[string]string{"env": ""}}}
	in.Include = []LabelCondition{{"env": ""}}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("empty string tag value must match: %v", err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "x" {
		t.Fatalf("expected x selected, got %+v", plan.Batches)
	}
}
