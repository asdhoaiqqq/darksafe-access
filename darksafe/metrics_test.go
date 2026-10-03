package darksafe

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func mustOK(t *testing.T, store *MetricStore, line string) *BatchResult {
	t.Helper()
	res, err := store.IngestLine(line)
	if err != nil {
		t.Fatalf("expected success for %s, got error: %+v", line, err)
	}
	return res
}

func mustFail(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	res, err := store.IngestLine(line)
	if err == nil {
		t.Fatalf("expected failure for %s, got result: %+v", line, res)
	}
	return err
}

func TestIngestBasicSuccessAndSnapshot(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"cpu","timestamp":2000,"value":0.5,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":0.25,"labels":{"host":"a"}},
		{"name":"mem","timestamp":1000,"value":1}
	]`)
	if res.Added != 3 || res.Duplicates != 0 {
		t.Fatalf("counts = %d/%d, want 3/0", res.Added, res.Duplicates)
	}
	if len(res.Series) != 3 {
		t.Fatalf("series len = %d, want 3", len(res.Series))
	}
	// 序列排序：mem 排在最后；同指标名按标签键值对字典序 host=a 在 host=b 前。
	if res.Series[0].Name != "cpu" || res.Series[0].Labels["host"] != "a" {
		t.Fatalf("series[0] = %+v, want cpu host=a", res.Series[0])
	}
	if res.Series[1].Labels["host"] != "b" {
		t.Fatalf("series[1] = %+v, want cpu host=b", res.Series[1])
	}
	if res.Series[2].Name != "mem" {
		t.Fatalf("series[2] = %+v, want mem", res.Series[2])
	}
	// 序列内按时间戳升序（输入故意乱序）。
	if pts := res.Series[0].Points; len(pts) != 1 || pts[0].Timestamp != 1000 {
		t.Fatalf("cpu host=a points = %+v", pts)
	}
}

func TestLabelOrderDoesNotChangeIdentity(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1,"labels":{"a":"1","b":"2"}}]`)
	// 标签书写顺序不同：同一点，重复。
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":1.0,"labels":{"b":"2","a":"1"}}]`)
	if res.Added != 0 || res.Duplicates != 1 || len(res.Series) != 1 {
		t.Fatalf("got %+v, want single duplicate against same series", res)
	}
}

func TestMissingLabelDiffersFromEmptyValue(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":2,"labels":{"zone":""}}]`)
	if res.Added != 1 || len(res.Series) != 2 {
		t.Fatalf("missing label and empty label value must be distinct series, got %+v", res)
	}
	// 空对象与省略等价。
	res2 := mustOK(t, store, `[{"name":"m","timestamp":2,"value":3,"labels":{}}]`)
	if res2.Added != 1 {
		t.Fatalf("empty labels object should equal omitted labels, got added=%d", res2.Added)
	}
}

func TestEmptyLabelValuePreserved(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":1,"labels":{"zone":""}}]`)
	v, ok := res.Series[0].Labels["zone"]
	if !ok || v != "" {
		t.Fatalf("empty label value not preserved: %+v", res.Series[0].Labels)
	}
}

func TestCrossBatchDuplicateAndConflict(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	// 完全相同（1 与 1.0）→ 跨批重复。
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":1.0}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("counts = %d/%d, want 0/1", res.Added, res.Duplicates)
	}

	// 数值不同 → 整批拒绝，报告冲突细节。
	lerr := mustFail(t, store, `[{"name":"m","timestamp":1,"value":2}]`)
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("expected conflict at index 1, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "m" || c.Timestamp != 1 || c.Existing != 1 || c.Submitted != 2 {
		t.Fatalf("conflict detail = %+v", c)
	}
}

func TestFailedBatchRollsBackEntireLine(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 本批第一个点是新增，第二个点与历史冲突：整批都不得生效。
	mustFail(t, store, `[{"name":"new","timestamp":1,"value":1},{"name":"old","timestamp":1,"value":100}]`)

	res := mustOK(t, store, `[]`)
	if res.Added != 0 || res.Duplicates != 0 {
		t.Fatalf("empty batch counts = %d/%d, want 0/0", res.Added, res.Duplicates)
	}
	if len(res.Series) != 1 || res.Series[0].Name != "old" {
		t.Fatalf("failed batch must not add series, snapshot = %+v", res.Series)
	}
	if pts := res.Series[0].Points; len(pts) != 1 || pts[0].Value != 9 {
		t.Fatalf("old value must remain 9, got %+v", pts)
	}
}

func TestWithinBatchDuplicateAndConflict(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":1,"value":1.0}
	]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("counts = %d/%d, want 1/1", res.Added, res.Duplicates)
	}

	// 同批同点不同值 → 拒绝整批。
	lerr := mustFail(t, store, `[
		{"name":"n","timestamp":1,"value":1},
		{"name":"n","timestamp":1,"value":2}
	]`)
	if lerr.Index != 2 || lerr.Conflict == nil || lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 2 {
		t.Fatalf("unexpected within-batch conflict: %+v", lerr)
	}
	mustOK(t, store, `[]`) // 确认 n 未被写入
	if got := len(mustOK(t, store, `[]`).Series); got != 1 {
		t.Fatalf("series count = %d, want 1", got)
	}

	// 与已写入值重复后再提交不同值，也算冲突。
	lerr = mustFail(t, store, `[
		{"name":"m","timestamp":1,"value":1.0},
		{"name":"m","timestamp":1,"value":3}
	]`)
	if lerr.Index != 2 || lerr.Conflict == nil {
		t.Fatalf("expected conflict at index 2 against stored value, got %+v", lerr)
	}
}

// TestConflictResultIsIndependentOfStorage 保证失败返回的冲突记录是独立副本：
// 调用方为整理错误信息而增删改其中的标签，不能回写已存储序列的身份、点值，
// 也不能影响后续写入的去重与冲突判断、查询子集匹配及写入快照。
func TestConflictResultIsIndependentOfStorage(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`)

	lerr := mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	c := lerr.Conflict
	if c.Series.Name != "cpu" || c.Series.Labels["host"] != "a" ||
		c.Timestamp != 1000 || c.Existing != 1 || c.Submitted != 2 || lerr.Index != 1 {
		t.Fatalf("conflict detail = %+v", c)
	}

	// 调用方改写手中的冲突结果（改值、加键、删键、改指标名）。
	c.Series.Labels["host"] = "b"
	c.Series.Labels["extra"] = "z"
	delete(c.Series.Labels, "host")
	c.Series.Name = "mem"

	// 原序列仍按 host=a 命中，数量与均值保持正确。
	res := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 1 {
		t.Fatalf("host=a must still find the stored point, got %+v", res.Series)
	}
	// host=b 不能因这次编辑而匹配到该序列。
	res = mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"b"}}`)
	if len(res.Series) != 0 {
		t.Fatalf("edited label must not leak into queries, got %+v", res.Series)
	}
	// 后续写入快照反映真实存储。
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || snap.Series[0].Name != "cpu" ||
		len(snap.Series[0].Labels) != 1 || snap.Series[0].Labels["host"] != "a" {
		t.Fatalf("snapshot tainted by conflict edit: %+v", snap.Series)
	}
	// 后续写入的去重与冲突判断仍基于真实身份与原值。
	dup := mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":1.0,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("dedup after conflict edit = %+v, want duplicate", dup)
	}
	again := mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":3,"labels":{"host":"a"}}]`)
	if again.Conflict.Existing != 1 || again.Conflict.Submitted != 3 ||
		again.Conflict.Series.Labels["host"] != "a" {
		t.Fatalf("later conflict must report the real stored series, got %+v", again.Conflict)
	}
}

// TestConflictResultIndependentForUnlabeledAndEmptyValue 覆盖无标签序列与
// 空字符串标签值序列：编辑冲突结果中的标签集合不得给已有序列补标签或抹掉空值键。
func TestConflictResultIndependentForUnlabeledAndEmptyValue(t *testing.T) {
	// 无标签序列：在返回的空标签集合里加 host，不能给已有序列补上该标签。
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	lerr := mustFail(t, store, `[{"name":"m","timestamp":1,"value":2}]`)
	if len(lerr.Conflict.Series.Labels) != 0 {
		t.Fatalf("unlabeled conflict labels = %+v, want {}", lerr.Conflict.Series.Labels)
	}
	lerr.Conflict.Series.Labels["host"] = "b"
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2}`)
	if len(res.Series) != 1 || len(res.Series[0].Labels) != 0 {
		t.Fatalf("unlabeled series gained a label: %+v", res.Series)
	}
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"host":"b"}}`)
	if len(res.Series) != 0 {
		t.Fatalf("fabricated label must not match, got %+v", res.Series)
	}

	// 原本带空字符串标签值的序列在删除冲突结果中的该键后，仍与缺少该键的序列区别。
	store2 := NewMetricStore()
	mustOK(t, store2, `[{"name":"m","timestamp":1,"value":1,"labels":{"zone":""}}]`)
	lerr2 := mustFail(t, store2, `[{"name":"m","timestamp":1,"value":2,"labels":{"zone":""}}]`)
	if v, ok := lerr2.Conflict.Series.Labels["zone"]; !ok || v != "" {
		t.Fatalf("empty-value label not preserved in conflict: %+v", lerr2.Conflict.Series.Labels)
	}
	delete(lerr2.Conflict.Series.Labels, "zone")
	res2 := mustQuery(t, store2, `{"op":"query","name":"m","start":0,"end":2,"labels":{"zone":""}}`)
	if len(res2.Series) != 1 || res2.Series[0].Average != 1 {
		t.Fatalf("empty-value series must keep its key, got %+v", res2.Series)
	}
	snap := mustOK(t, store2, `[]`)
	if v, ok := snap.Series[0].Labels["zone"]; !ok || v != "" || len(snap.Series[0].Labels) != 1 {
		t.Fatalf("snapshot lost empty-value label: %+v", snap.Series)
	}
}

func TestOutOfOrderTimestampsSorted(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":30,"value":3},{"name":"m","timestamp":10,"value":1}]`)
	res := mustOK(t, store, `[{"name":"m","timestamp":20,"value":2}]`)
	pts := res.Series[0].Points
	if len(pts) != 3 {
		t.Fatalf("points = %+v", pts)
	}
	for i, want := range []int64{10, 20, 30} {
		if pts[i].Timestamp != want {
			t.Fatalf("point %d timestamp = %d, want %d", i, pts[i].Timestamp, want)
		}
	}
}

func TestSeriesLabelLexicographicOrder(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1,"labels":{"a":"2"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"a":"1"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"a":"1","b":"1"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"a":"1","b":""}}
	]`)
	res := mustOK(t, store, `[]`)
	got := make([]string, len(res.Series))
	for i, s := range res.Series {
		b, _ := json.Marshal(s.Labels)
		got[i] = string(b)
	}
	want := []string{
		`{"a":"1"}`,
		`{"a":"1","b":""}`,
		`{"a":"1","b":"1"}`,
		`{"a":"2"}`,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("series order = %v, want %v", got, want)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		line  string
		index int
	}{
		{`[{"timestamp":1,"value":1}]`, 1},                                       // 缺 name
		{`[{"name":"m","value":1}]`, 1},                                          // 缺 timestamp
		{`[{"name":"m","timestamp":1}]`, 1},                                      // 缺 value
		{`[{"name":"","timestamp":1,"value":1}]`, 1},                             // 空指标名
		{`[{"name":7,"timestamp":1,"value":1}]`, 1},                              // name 类型错误
		{`[{"name":"m","timestamp":true,"value":1}]`, 1},                         // timestamp 非数字
		{`[{"name":"m","timestamp":1.0,"value":1}]`, 1},                          // timestamp 非整数
		{`[{"name":"m","timestamp":"1","value":1}]`, 1},                          // timestamp 字符串
		{`[{"name":"m","timestamp":1,"value":"1"}]`, 1},                          // value 字符串
		{`[{"name":"m","timestamp":1,"value":true}]`, 1},                         // value 布尔
		{`[{"name":"m","timestamp":1,"value":1e999}]`, 1},                        // value 超出 float64
		{`[{"name":"m","timestamp":9223372036854775808,"value":1}]`, 1},          // timestamp 超 int64
		{`[{"name":"m","timestamp":-9223372036854775809,"value":1}]`, 1},         // timestamp 低于 int64
		{`[{"name":"m","timestamp":1,"value":1,"zone":"x"}]`, 1},                 // 未知字段
		{`[{"name":"m","timestamp":1,"value":1,"labels":[]}]`, 1},                // labels 非对象
		{`[{"name":"m","timestamp":1,"value":1,"labels":null}]`, 1},              // labels 为 null
		{`[{"name":"m","timestamp":1,"value":1,"labels":{"": "x"}}]`, 1},         // 空标签键
		{`[{"name":"m","timestamp":1,"value":1,"labels":{"k": 2}}]`, 1},          // 标签值非字符串
		{`[{"name":"m","timestamp":1,"value":1},{"name":7}]`, 2},                 // 错误位置从 1 开始
		{`[{"name":"m","timestamp":1,"value":1,"labels":{"a":"1","a":"2"}}]`, 1}, // 重复标签键
	}
	for _, tc := range cases {
		store := NewMetricStore()
		lerr := mustFail(t, store, tc.line)
		if lerr.Index != tc.index {
			t.Errorf("line %s: index = %d, want %d (error: %s)", tc.line, lerr.Index, tc.index, lerr.Error)
		}
		if lerr.Conflict != nil {
			t.Errorf("line %s: validation error must not carry conflict", tc.line)
		}
		// 任何失败都不应写入数据。
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Errorf("line %s: store mutated by failed batch, series=%d", tc.line, got)
		}
	}
}

func TestNonFiniteAndLargeValues(t *testing.T) {
	store := NewMetricStore()
	// 合法的大整数与浮点。
	mustOK(t, store, `[{"name":"m","timestamp":9223372036854775807,"value":1.7976931348623157e308}]`)
	mustOK(t, store, `[{"name":"m","timestamp":-9223372036854775808,"value":2.2250738585072014e-308}]`)
	if math.IsNaN(math.NaN()) { // 常量编译期无法写为 JSON，这里只确认存储值有限。
		res := mustOK(t, store, `[]`)
		for _, s := range res.Series {
			for _, p := range s.Points {
				if math.IsInf(p.Value, 0) || math.IsNaN(p.Value) {
					t.Fatalf("non-finite value stored: %v", p.Value)
				}
			}
		}
	}
}

func TestNonArrayAndMalformedLines(t *testing.T) {
	for _, line := range []string{
		`{}`,           // 对象不是数组
		`null`,         // null
		`"hello"`,      // 字符串
		`123`,          // 数字
		`[`,            // 残缺数组
		`{"name":"m"}`, // 对象
		`[] []`,        // 两个 JSON 值
		`[{"name":"m"}] extra`,
	} {
		store := NewMetricStore()
		lerr := mustFail(t, store, line)
		if lerr.Index != 0 {
			t.Errorf("line %q: whole-line failure must not carry index, got %d", line, lerr.Index)
		}
		if !strings.Contains(lerr.Error, "invalid JSON") {
			t.Errorf("line %q: error = %q, want invalid JSON reason", line, lerr.Error)
		}
	}
}

func TestEmptyBatchListsAllCurrentSeries(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[]`)
	if res.Added != 0 || res.Duplicates != 0 || len(res.Series) != 0 {
		t.Fatalf("initial empty batch = %+v", res)
	}
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	res = mustOK(t, store, `[]`)
	if res.Added != 0 || res.Duplicates != 0 || len(res.Series) != 1 {
		t.Fatalf("empty batch after writes = %+v", res)
	}
}

func TestSuccessResultShape(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "ok" {
		t.Fatalf("status = %v", got["status"])
	}
	series0 := got["series"].([]interface{})[0].(map[string]interface{})
	if series0["labels"].(map[string]interface{}) == nil {
		t.Fatal("labels should serialize as {} not null")
	}
}

func TestInt64BoundaryTimestampDistinct(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":9223372036854775807,"value":1}]`)
	lerr := mustFail(t, store, `[{"name":"m","timestamp":9223372036854775807,"value":2}]`)
	if lerr.Conflict == nil {
		t.Fatalf("boundary timestamp should be addressable for conflict detection, got %+v", lerr)
	}
}

func mustQuery(t *testing.T, store *MetricStore, line string) *QueryResult {
	t.Helper()
	res, err := store.QueryLine(line)
	if err != nil {
		t.Fatalf("expected query success for %s, got error: %+v", line, err)
	}
	return res
}

func mustQueryFail(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	res, err := store.QueryLine(line)
	if err == nil {
		t.Fatalf("expected query failure for %s, got result: %+v", line, res)
	}
	return err
}

func TestQueryRangeInclusiveAndAverage(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1500,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":3,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2500,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":10,"labels":{"host":"b"}}
	]`)

	// 闭区间：包含 start=1000、end=2000 两个端点，(1+2+3)/3 = 2。
	res := mustQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`)
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly host=a", res.Series)
	}
	s0 := res.Series[0]
	if s0.Name != "cpu" || s0.Labels["host"] != "a" || s0.Count != 3 || s0.Average != 2 {
		t.Fatalf("series[0] = %+v, want cpu host=a count=3 avg=2", s0)
	}

	// start == end：只查该时间戳一个点。
	res = mustQuery(t, store, `{"op":"query","name":"cpu","start":1500,"end":1500,"labels":{"host":"a"}}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 2 {
		t.Fatalf("point query = %+v", res.Series)
	}

	// 区间外不返回该序列（空数组，而不是 count=0 的条目）。
	res = mustQuery(t, store, `{"op":"query","name":"cpu","start":3000,"end":4000}`)
	if len(res.Series) != 0 {
		t.Fatalf("out-of-range query = %+v, want empty series", res.Series)
	}
}

func TestQueryLabelSelection(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":1,"value":2,"labels":{"zone":""}},
		{"name":"m","timestamp":1,"value":3,"labels":{"zone":"x"}},
		{"name":"m","timestamp":1,"value":4,"labels":{"zone":"x","host":"h"}},
		{"name":"other","timestamp":1,"value":9}
	]`)

	// 省略 labels：该指标全部序列（共 4 条），不合并；顺序沿用 snapshot 排序。
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2}`)
	if len(res.Series) != 4 {
		t.Fatalf("all series = %+v, want 4", res.Series)
	}
	wantOrder := []map[string]string{
		{},
		{"host": "h", "zone": "x"},
		{"zone": ""},
		{"zone": "x"},
	}
	for i, wantLabels := range wantOrder {
		got := res.Series[i].Labels
		if len(got) != len(wantLabels) {
			t.Fatalf("series[%d] labels = %v, want %v", i, got, wantLabels)
		}
		for k, v := range wantLabels {
			if gv, ok := got[k]; !ok || gv != v {
				t.Fatalf("series[%d] labels = %v, want %v", i, got, wantLabels)
			}
		}
	}

	// 空对象等价于省略。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{}}`)
	if len(res.Series) != 4 {
		t.Fatalf("empty labels = %+v, want all 4 series", res.Series)
	}

	// 子集匹配：zone=x 同时匹配带 host 额外标签和无额外标签的两条序列；
	// 排序沿用 snapshot：标签对 (host,h)<(zone,x)，故带 host 的序列在前。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"zone":"x"}}`)
	if len(res.Series) != 2 || res.Series[0].Average != 4 || res.Series[1].Average != 3 {
		t.Fatalf("subset match = %+v, want averages 4 and 3", res.Series)
	}

	// 额外约束 host 后只剩一条。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"zone":"x","host":"h"}}`)
	if len(res.Series) != 1 || res.Series[0].Average != 4 {
		t.Fatalf("fully specified match = %+v, want average 4", res.Series)
	}

	// 空字符串只匹配真实存在的空值标签，不匹配缺少该标签的序列。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"zone":""}}`)
	if len(res.Series) != 1 || res.Series[0].Average != 2 {
		t.Fatalf("empty-value match = %+v, want single series avg 2", res.Series)
	}

	// 要求一个不存在的标签：空结果。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"nope":"x"}}`)
	if len(res.Series) != 0 {
		t.Fatalf("missing label match = %+v, want empty", res.Series)
	}

	// 标签顺序不影响匹配。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"host":"h","zone":"x"}}`)
	if len(res.Series) != 1 {
		t.Fatalf("label order must not matter, got %+v", res.Series)
	}

	// name 精确匹配：不同指标不返回。
	res = mustQuery(t, store, `{"op":"query","name":"nonexistent","start":0,"end":2}`)
	if len(res.Series) != 0 {
		t.Fatalf("unknown metric = %+v, want empty", res.Series)
	}

	// 无标签序列输出为空对象而非 null。
	b, _ := json.Marshal(res)
	if string(b) != `{"status":"ok","op":"query","series":[]}` {
		t.Fatalf("empty result shape = %s", b)
	}
	res = mustQuery(t, store, `{"op":"query","name":"other","start":0,"end":2}`)
	if len(res.Series) != 1 || len(res.Series[0].Labels) != 0 {
		t.Fatalf("unlabeled series = %+v", res.Series)
	}
	b, _ = json.Marshal(res.Series[0].Labels)
	if string(b) != `{}` {
		t.Fatalf("labels should serialize as {}, got %s", b)
	}
}

func TestQueryInterleavedWritesReadYourWrites(t *testing.T) {
	store := NewMetricStore()

	// 查询只见到此前成功写入的数据。
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(res.Series) != 0 {
		t.Fatalf("query before any write = %+v, want empty", res.Series)
	}

	mustOK(t, store, `[{"name":"m","timestamp":10,"value":2}]`)
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":4}]`)
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(res.Series) != 1 || res.Series[0].Count != 2 || res.Series[0].Average != 3 {
		t.Fatalf("query after writes = %+v", res.Series)
	}

	// 失败批次整批回滚，其中任何点都不能参与后续查询。
	mustFail(t, store, `[{"name":"m","timestamp":30,"value":8},{"name":"m","timestamp":10,"value":100}]`)
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if res.Series[0].Count != 2 || res.Series[0].Average != 3 {
		t.Fatalf("rolled-back points must not be queryable, got %+v", res.Series)
	}

	// 重复点只计一次。
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":4.0}]`)
	res = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if res.Series[0].Count != 2 || res.Series[0].Average != 3 {
		t.Fatalf("duplicate must not affect query, got %+v", res.Series)
	}

	// 查询不改变存储：同一查询重复结果一致，后续写入快照也不受影响。
	res2 := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if !reflect.DeepEqual(res, res2) {
		t.Fatalf("repeated query differs: %+v vs %+v", res, res2)
	}
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 2 {
		t.Fatalf("query mutated storage, snapshot = %+v", snap.Series)
	}
}

func TestQueryFiniteMeanOverflow(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1e308},
		{"name":"m","timestamp":2,"value":1e308}
	]`)
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v", res.Series)
	}
	avg := res.Series[0].Average
	if math.IsInf(avg, 0) || math.IsNaN(avg) {
		t.Fatalf("average must be finite, got %v", avg)
	}
	if avg != 1e308 {
		t.Fatalf("average = %v, want 1e308", avg)
	}

	// 反号极值相减（朴素求和与朴素增量都会溢出）也要给出有限结果。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":1e308},
		{"name":"m","timestamp":2,"value":-1e308}
	]`)
	res = mustQuery(t, store2, `{"op":"query","name":"m","start":0,"end":100}`)
	avg = res.Series[0].Average
	if math.IsInf(avg, 0) || math.IsNaN(avg) {
		t.Fatalf("average of opposite extremes must be finite, got %v", avg)
	}

	// 结果与写入顺序无关：反序写入同一点集应得到相同均值。
	store3 := NewMetricStore()
	mustOK(t, store3, `[
		{"name":"m","timestamp":2,"value":-1e308},
		{"name":"m","timestamp":1,"value":1e308}
	]`)
	res3 := mustQuery(t, store3, `{"op":"query","name":"m","start":0,"end":100}`)
	if res3.Series[0].Average != avg {
		t.Fatalf("average depends on write order: %v vs %v", res3.Series[0].Average, avg)
	}
}

func TestQueryAverageExactAfterCancellation(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1e16},
		{"name":"m","timestamp":2,"value":1},
		{"name":"m","timestamp":3,"value":-1e16}
	]`)
	res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":3}`)
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", res.Series)
	}
	s0 := res.Series[0]
	if s0.Count != 3 {
		t.Fatalf("count = %d, want 3", s0.Count)
	}
	// 精确总和为 1，平均值为 1/3 舍入后的最近 float64。
	if s0.Average != 1.0/3.0 {
		t.Fatalf("average = %v, want %v", s0.Average, 1.0/3.0)
	}
	b, _ := json.Marshal(s0.Average)
	if string(b) != "0.3333333333333333" {
		t.Fatalf("average JSON = %s, want 0.3333333333333333", b)
	}
}

func TestQueryAverageOrderAndBatchIndependent(t *testing.T) {
	// 同一组数值：不同批次划分、不同提交顺序、区间内时间戳顺序不同，均值必须一致。
	setups := [][]string{
		{`[{"name":"m","timestamp":1,"value":1e16},{"name":"m","timestamp":2,"value":1},{"name":"m","timestamp":3,"value":-1e16}]`},
		{`[{"name":"m","timestamp":3,"value":-1e16}]`, `[{"name":"m","timestamp":1,"value":1e16},{"name":"m","timestamp":2,"value":1}]`},
		{`[{"name":"m","timestamp":2,"value":1}]`, `[{"name":"m","timestamp":3,"value":-1e16}]`, `[{"name":"m","timestamp":1,"value":1e16}]`},
		{`[{"name":"m","timestamp":1,"value":-1e16},{"name":"m","timestamp":2,"value":1},{"name":"m","timestamp":3,"value":1e16}]`},
	}
	want := 1.0 / 3.0
	for i, batches := range setups {
		store := NewMetricStore()
		for _, line := range batches {
			mustOK(t, store, line)
		}
		res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":3}`)
		if len(res.Series) != 1 || res.Series[0].Count != 3 || res.Series[0].Average != want {
			t.Fatalf("setup %d: got %+v, want count=3 average=%v", i, res.Series, want)
		}
	}
}

func TestQueryAverageSumOverflowStaysFinite(t *testing.T) {
	store := NewMetricStore()
	// 总和 3e308 超出 float64 范围，但平均值必须有限且精确。
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1e308},
		{"name":"m","timestamp":2,"value":1e308},
		{"name":"m","timestamp":3,"value":1e308}
	]`)
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	avg := res.Series[0].Average
	if math.IsInf(avg, 0) || math.IsNaN(avg) || avg != 1e308 {
		t.Fatalf("average = %v, want 1e308", avg)
	}

	// 最大有限值自身重复：平均仍为 MaxFloat64，不溢出为 +Inf。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":1.7976931348623157e308},
		{"name":"m","timestamp":2,"value":1.7976931348623157e308}
	]`)
	res = mustQuery(t, store2, `{"op":"query","name":"m","start":0,"end":10}`)
	if avg = res.Series[0].Average; avg != math.MaxFloat64 {
		t.Fatalf("average = %v, want MaxFloat64", avg)
	}
}

func TestQueryAverageExactZeroIsPositiveZero(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1e308},
		{"name":"m","timestamp":2,"value":-1e308},
		{"name":"m","timestamp":3,"value":5},
		{"name":"m","timestamp":4,"value":-5}
	]`)
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	avg := res.Series[0].Average
	if avg != 0 || math.Signbit(avg) {
		t.Fatalf("exact zero average = %v (signbit=%v), want +0", avg, math.Signbit(avg))
	}
	b, _ := json.Marshal(avg)
	if string(b) != "0" {
		t.Fatalf("zero average JSON = %s, want 0", b)
	}
}

func TestQueryAverageSubnormalAndRounding(t *testing.T) {
	// 最小次正规值参与平均不得被提前冲刷为零。
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":5e-324},
		{"name":"m","timestamp":2,"value":5e-324}
	]`)
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	if avg := res.Series[0].Average; avg != 5e-324 {
		t.Fatalf("average = %v, want 5e-324", avg)
	}

	// 精确平均恰为 2^-1075（0 与最小次正规值正中）：按最近偶数舍入为 0。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":5e-324},
		{"name":"m","timestamp":2,"value":0}
	]`)
	res = mustQuery(t, store2, `{"op":"query","name":"m","start":0,"end":10}`)
	if avg := res.Series[0].Average; avg != 0 {
		t.Fatalf("tie at 2^-1075 must round to even (0), got %v", avg)
	}

	// 单点序列返回该点的值本身。
	store3 := NewMetricStore()
	mustOK(t, store3, `[{"name":"m","timestamp":7,"value":0.1}]`)
	res = mustQuery(t, store3, `{"op":"query","name":"m","start":0,"end":10}`)
	if avg := res.Series[0].Average; avg != 0.1 {
		t.Fatalf("single point average = %v, want 0.1", avg)
	}
}

func TestQueryValidationErrors(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	bad := []string{
		`{"op":"query","name":"","start":0,"end":1}`,                                        // 空 name
		`{"op":"query","name":7,"start":0,"end":1}`,                                         // name 类型错误
		`{"op":"query","name":"m","start":0}`,                                               // 缺 end
		`{"op":"query","name":"m","start":0,"end":1,"extra":1}`,                             // 未知字段
		`{"op":"ingest","name":"m","start":0,"end":1}`,                                      // 未知 op
		`{"op":"write","name":"m","start":0,"end":1}`,                                       // 其他 op
		`{"op":7,"name":"m","start":0,"end":1}`,                                             // op 类型错误
		`{"op":"query","op":"query","name":"m","start":0,"end":1}`,                          // 重复字段
		`{"op":"query","name":"m","start":0.5,"end":1}`,                                     // start 非整数
		`{"op":"query","name":"m","start":"0","end":1}`,                                     // start 字符串
		`{"op":"query","name":"m","start":true,"end":1}`,                                    // start 布尔
		`{"op":"query","name":"m","start":0,"end":9223372036854775808}`,                     // end 超 int64
		`{"op":"query","name":"m","start":2,"end":1}`,                                       // start > end
		`{"op":"query","name":"m","start":-9223372036854775808,"end":-9223372036854775809}`, // end 低于 int64
		`{"op":"query","name":"m","start":0,"end":1,"labels":[]}`,                           // labels 非对象
		`{"op":"query","name":"m","start":0,"end":1,"labels":null}`,                         // labels 为 null
		`{"op":"query","name":"m","start":0,"end":1,"labels":{"":"v"}}`,                     // 空标签键
		`{"op":"query","name":"m","start":0,"end":1,"labels":{"k":1}}`,                      // 标签值非字符串
		`{"op":"query","name":"m","start":0,"end":1,"labels":{"k":"a","k":"b"}}`,            // 重复标签键
		`{"name":"m","start":0,"end":1}`,                                                    // 缺 op
		`{"op":"query","start":0,"end":1}`,                                                  // 缺 name
		`{"op":"query","name":"m","start":0,"end":1} trailing`,                              // 尾随内容
	}
	for _, line := range bad {
		lerr := mustQueryFail(t, store, line)
		if lerr.Index != 0 {
			t.Errorf("line %s: query errors must not carry index, got %d", line, lerr.Index)
		}
		if lerr.Conflict != nil {
			t.Errorf("line %s: query errors must not carry conflict", line)
		}
	}
	// 非法行不改变存储。
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 {
		t.Fatalf("failed queries must not mutate storage, got %+v", res.Series)
	}
}

func TestProcessLineDispatchesArrayAndObject(t *testing.T) {
	store := NewMetricStore()
	r, lerr := store.ProcessLine(`[{"name":"m","timestamp":1,"value":2}]`)
	if lerr != nil {
		t.Fatalf("array line: %+v", lerr)
	}
	if _, ok := r.(*BatchResult); !ok {
		t.Fatalf("array line result type = %T, want *BatchResult", r)
	}
	r, lerr = store.ProcessLine(`{"op":"query","name":"m","start":0,"end":10}`)
	if lerr != nil {
		t.Fatalf("object line: %+v", lerr)
	}
	qr, ok := r.(*QueryResult)
	if !ok {
		t.Fatalf("object line result type = %T, want *QueryResult", r)
	}
	if len(qr.Series) != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("query via ProcessLine = %+v", qr)
	}
	// 标量顶层值仍然是整行错误，且不带 index。
	_, lerr = store.ProcessLine(`"hello"`)
	if lerr == nil || lerr.Index != 0 {
		t.Fatalf("scalar line = %+v, want whole-line error without index", lerr)
	}
	_, lerr = store.ProcessLine(`{}`)
	if lerr == nil {
		t.Fatal("empty object must fail validation")
	}
}
