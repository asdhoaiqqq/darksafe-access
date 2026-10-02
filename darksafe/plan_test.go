package darksafe

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) PlanSpec {
	t.Helper()
	spec, err := ParsePlanSpec([]byte(raw))
	if err != nil {
		t.Fatalf("ParsePlanSpec failed: %v", err)
	}
	return spec
}

func TestBuildPlanBatchesAndOrder(t *testing.T) {
	// 输入顺序故意打乱，输出必须按集群标识升序并按容量分批。
	raw := `{
		"application": "billing",
		"revision": "r-9",
		"image": "registry.example/billing:r-9",
		"batchSize": 2,
		"clusters": [
			{"id": "c3"},
			{"id": "c1", "labels": {"zone": "east"}},
			{"id": "c5"},
			{"id": "c2"},
			{"id": "c4"}
		]
	}`
	plan, err := BuildPlan(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	if plan.Application != "billing" || plan.Revision != "r-9" || plan.Image != "registry.example/billing:r-9" {
		t.Fatalf("应用信息未原样保留: %+v", plan)
	}
	gotBatches := []PlanBatch{}
	for _, b := range plan.Batches {
		gotBatches = append(gotBatches, PlanBatch{Batch: b.Batch, Clusters: b.Clusters})
	}
	wantBatches := []PlanBatch{
		{Batch: 1, Clusters: []string{"c1", "c2"}},
		{Batch: 2, Clusters: []string{"c3", "c4"}},
		{Batch: 3, Clusters: []string{"c5"}},
	}
	if len(gotBatches) != len(wantBatches) {
		t.Fatalf("批次数 = %d, 期望 %d", len(gotBatches), len(wantBatches))
	}
	for i := range wantBatches {
		if gotBatches[i].Batch != wantBatches[i].Batch {
			t.Errorf("批次 %d 编号 = %d, 期望 %d", i, gotBatches[i].Batch, wantBatches[i].Batch)
		}
		if strings.Join(gotBatches[i].Clusters, ",") != strings.Join(wantBatches[i].Clusters, ",") {
			t.Errorf("批次 %d = %v, 期望 %v", i+1, gotBatches[i].Clusters, wantBatches[i].Clusters)
		}
	}
	if len(plan.Skipped) != 0 {
		t.Errorf("没有未入选集群时期望 skipped 为空, 得到 %+v", plan.Skipped)
	}
}

func TestBuildPlanDeterminism(t *testing.T) {
	// 调整集群顺序、标签键顺序、条件顺序，结果必须相同。
	rawA := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 3,
		"clusters": [
			{"id": "b", "labels": {"tier": "gold", "zone": "z1"}},
			{"id": "a", "labels": {"zone": "z1", "tier": "gold"}},
			{"id": "c", "labels": {"tier": "silver"}}
		],
		"include": [{"tier": "gold"}, {"zone": "z1", "tier": "gold"}],
		"exclude": [{"other": "x"}]
	}`
	rawB := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 3,
		"clusters": [
			{"id": "c", "labels": {"tier": "silver"}},
			{"id": "a", "labels": {"tier": "gold", "zone": "z1"}},
			{"id": "b", "labels": {"tier": "gold", "zone": "z1"}}
		],
		"include": [{"zone": "z1", "tier": "gold"}, {"tier": "gold"}],
		"exclude": []
	}`
	pa, err := BuildPlan(mustParse(t, rawA))
	if err != nil {
		t.Fatalf("plan A: %v", err)
	}
	pb, err := BuildPlan(mustParse(t, rawB))
	if err != nil {
		t.Fatalf("plan B: %v", err)
	}
	ja, _ := json.Marshal(pa)
	jb, _ := json.Marshal(pb)
	if string(ja) != string(jb) {
		t.Fatalf("结果不一致:\n%s\n%s", ja, jb)
	}
	// 同一输入重复预览也应相同。
	pb2, err := BuildPlan(mustParse(t, rawB))
	if err != nil {
		t.Fatalf("plan B2: %v", err)
	}
	jb2, _ := json.Marshal(pb2)
	if string(jb) != string(jb2) {
		t.Fatalf("重复预览结果不稳定:\n%s\n%s", jb, jb2)
	}
}

func TestSelectionRulesAndReasons(t *testing.T) {
	raw := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 10,
		"clusters": [
			{"id": "off", "disabled": true, "labels": {"env": "prod"}},
			{"id": "excl", "labels": {"env": "prod", "canary": "true"}},
			{"id": "in", "labels": {"env": "prod"}},
			{"id": "miss", "labels": {"env": "dev"}},
			{"id": "both", "disabled": true, "labels": {"canary": "true"}}
		],
		"include": [{"env": "prod"}],
		"exclude": [{"canary": "true"}]
	}`
	plan, err := BuildPlan(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "in" {
		t.Fatalf("入选集群 = %+v, 期望仅 in", plan.Batches)
	}
	wantSkip := map[string]string{
		"off":  ReasonDisabled,
		"both": ReasonDisabled, // 停用优先于排除/包含
		"excl": ReasonExcluded,
		"miss": ReasonNotIncluded,
	}
	if len(plan.Skipped) != len(wantSkip) {
		t.Fatalf("skipped = %+v", plan.Skipped)
	}
	for _, sk := range plan.Skipped {
		if wantSkip[sk.ID] != sk.Reason {
			t.Errorf("集群 %q 原因 = %q, 期望 %q", sk.ID, sk.Reason, wantSkip[sk.ID])
		}
	}
	// skipped 按标识升序。
	if plan.Skipped[0].ID != "both" || plan.Skipped[len(plan.Skipped)-1].ID != "off" {
		t.Errorf("skipped 未按标识升序: %+v", plan.Skipped)
	}
}

func TestEmptyStringLabelValueMatches(t *testing.T) {
	raw := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 10,
		"clusters": [
			{"id": "a", "labels": {"drain": ""}},
			{"id": "b", "labels": {"drain": "yes"}}
		],
		"include": [{"drain": ""}]
	}`
	plan, err := BuildPlan(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "a" {
		t.Fatalf("空字符串标签值应精确匹配, 得到 %+v", plan.Batches)
	}
}

func TestMissingLabelNeverMatches(t *testing.T) {
	raw := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 10,
		"clusters": [
			{"id": "a", "labels": {"env": "prod", "tier": "gold"}},
			{"id": "b", "labels": {"env": "Prod"}},
			{"id": "c"}
		],
		"include": [{"env": "prod", "tier": "gold"}, {"env": "PROD"}]
	}`
	plan, err := BuildPlan(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	if got := strings.Join(plan.Batches[0].Clusters, ","); got != "a" {
		t.Fatalf("大小写不应转换、缺键不应匹配, 入选 = %q", got)
	}
}

func TestDefaultLabelsAndDisabled(t *testing.T) {
	// 未提供 labels 视为空集合；未提供 disabled 视为可用。
	raw := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 1,
		"clusters": [{"id": "a"}, {"id": "b", "disabled": false}]
	}`
	plan, err := BuildPlan(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	if len(plan.Batches) != 2 {
		t.Fatalf("期望两个批次, 得到 %+v", plan.Batches)
	}
}

func TestNoDeployableClusterFails(t *testing.T) {
	raw := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 10,
		"clusters": [
			{"id": "z-off", "disabled": true},
			{"id": "a-excl", "labels": {"env": "dev"}},
			{"id": "m-miss", "labels": {"env": "qa"}}
		],
		"include": [{"env": "prod"}],
		"exclude": [{"env": "dev"}]
	}`
	_, err := BuildPlan(mustParse(t, raw))
	if err == nil {
		t.Fatal("没有可发布集群时必须返回错误")
	}
	msg := err.Error()
	for _, want := range []string{
		"没有符合规则的可用集群",
		"z-off: " + ReasonDisabled,
		"a-excl: " + ReasonExcluded,
		"m-miss: " + ReasonNotIncluded,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q:\n%s", want, msg)
		}
	}
}

func TestValidationFailures(t *testing.T) {
	base := func(extra string) string {
		return `{"application":"app","revision":"v1","image":"img","batchSize":2,` + extra + `}`
	}
	cases := map[string]string{
		"应用名为空白":         `{"application":"  ","revision":"v1","image":"img","batchSize":2,"clusters":[{"id":"a"}]}`,
		"修订号为空白":         `{"application":"a","revision":"\t","image":"img","batchSize":2,"clusters":[{"id":"a"}]}`,
		"镜像为空白":          `{"application":"a","revision":"v1","image":"  ","batchSize":2,"clusters":[{"id":"a"}]}`,
		"集群标识为空白":        base(`"clusters":[{"id":" "}]`),
		"batchSize 为零":   base(`"clusters":[{"id":"a"}],"batchSize":0`),
		"batchSize 为负":   base(`"clusters":[{"id":"a"}],"batchSize":-3`),
		"batchSize 为字符串": `{"application":"a","revision":"v1","image":"img","batchSize":"2","clusters":[{"id":"a"}]}`,
		"重复集群标识":         base(`"clusters":[{"id":"a"},{"id":"a"}]`),
		"候选列表为空":         base(`"clusters":[]`),
		"缺少候选字段":         `{"application":"a","revision":"v1","image":"img","batchSize":2}`,
		"空标签键":           base(`"clusters":[{"id":"a","labels":{"":"v"}}]`),
		"非字符串标签值":        base(`"clusters":[{"id":"a","labels":{"k":1}}]`),
		"空包含条件":          base(`"clusters":[{"id":"a"}],"include":[{}]`),
		"空排除条件":          base(`"clusters":[{"id":"a"}],"exclude":[{}]`),
		"条件中的空键":         base(`"clusters":[{"id":"a"}],"include":[{"":"v"}]`),
		"条件值非字符串":        base(`"clusters":[{"id":"a"}],"include":[{"k":true}]`),
		"未知字段":           base(`"clusters":[{"id":"a"}],"unknown":1`),
		"disabled 非布尔":   base(`"clusters":[{"id":"a","disabled":"yes"}]`),
		"clusters 不是数组":  base(`"clusters":{"id":"a"}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePlanSpec([]byte(raw)); err == nil {
				t.Fatalf("期望校验失败, 输入 %s 却通过了", name)
			}
		})
	}
}

func TestMalformedJSON(t *testing.T) {
	for _, raw := range []string{
		``,
		`{`,
		`{"application":"app"`,
		`not json`,
		`{"application":"a","revision":"v1","image":"img","batchSize":2,"clusters":[{"id":"a"}]} {"x":1}`,
	} {
		if _, err := ParsePlanSpec([]byte(raw)); err == nil {
			t.Fatalf("非法 JSON 必须报错: %q", raw)
		}
	}
}

func TestBatchCapacityLargerThanClusters(t *testing.T) {
	raw := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 100,
		"clusters": [{"id": "b"}, {"id": "a"}]
	}`
	plan, err := BuildPlan(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Batch != 1 {
		t.Fatalf("容量大于集群数时应只有一批, 得到 %+v", plan.Batches)
	}
}

func TestEachClusterAppearsOnce(t *testing.T) {
	raw := `{
		"application": "app", "revision": "v1", "image": "img:v1", "batchSize": 1,
		"clusters": [
			{"id": "a", "labels": {"env": "prod", "tier": "gold"}},
			{"id": "b", "labels": {"env": "prod", "tier": "silver"}}
		],
		"include": [{"env": "prod"}, {"tier": "gold"}]
	}`
	plan, err := BuildPlan(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	seen := map[string]int{}
	for _, b := range plan.Batches {
		for _, id := range b.Clusters {
			seen[id]++
		}
	}
	if len(seen) != 2 || seen["a"] != 1 || seen["b"] != 1 {
		t.Fatalf("集群应且只应出现一次: %+v", seen)
	}
}
