package darksafe

import (
	"encoding/json"
	"math"
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
