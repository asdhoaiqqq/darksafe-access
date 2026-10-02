package darksafe

import (
	"encoding/json"
	"fmt"
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
	// 非标量 JSON 值与残缺输入：整行失败，不带 index，且原因属于 JSON 语法层面。
	for _, line := range []string{
		`null`,         // null
		`"hello"`,      // 字符串
		`123`,          // 数字
		`[`,            // 残缺数组
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
	// 对象行现在是查询语法：缺字段的查询对象仍失败（查询校验错误），但同样不带 index。
	for _, line := range []string{
		`{}`,             // 空对象
		`{"name":"m"}`,   // 缺 op/start/end
	} {
		store := NewMetricStore()
		lerr := mustFail(t, store, line)
		if lerr.Index != 0 {
			t.Errorf("line %q: query validation failure must not carry index, got %d", line, lerr.Index)
		}
		if lerr.Error == "" || strings.Contains(lerr.Error, "invalid JSON") {
			t.Errorf("line %q: error = %q, want a query validation reason", line, lerr.Error)
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

// mustQuery 执行一行查询并返回结果；失败时直接令测试失败。
func mustQuery(t *testing.T, store *MetricStore, line string) *QueryResult {
	t.Helper()
	res, lerr := store.ProcessLine(line)
	if lerr != nil {
		t.Fatalf("expected query success for %s, got error: %+v", line, lerr)
	}
	qr, ok := res.(*QueryResult)
	if !ok {
		t.Fatalf("line %s: result type %T, want *QueryResult", line, res)
	}
	return qr
}

func TestQueryBasicAverageAndOrder(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":3,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":10,"labels":{"host":"b"}},
		{"name":"mem","timestamp":1000,"value":100}
	]`)

	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":2000}`)
	if qr.Status != "ok" || qr.Op != "query" {
		t.Fatalf("query envelope = %+v, want ok/query", qr)
	}
	if len(qr.Series) != 2 {
		t.Fatalf("series count = %d, want 2: %+v", len(qr.Series), qr.Series)
	}
	// 沿用现有排序：同指标名按标签字典序 host=a 在 host=b 前。
	a := qr.Series[0]
	if a.Name != "cpu" || a.Labels["host"] != "a" || a.Count != 2 || a.Average != 2 {
		t.Fatalf("series[0] = %+v, want cpu host=a count=2 avg=2", a)
	}
	b := qr.Series[1]
	if b.Name != "cpu" || b.Labels["host"] != "b" || b.Count != 1 || b.Average != 10 {
		t.Fatalf("series[1] = %+v, want cpu host=b count=1 avg=10", b)
	}
}

func TestQueryInclusiveBoundariesAndSingleTimestamp(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1000,"value":1},
		{"name":"m","timestamp":2000,"value":2},
		{"name":"m","timestamp":3000,"value":3}
	]`)

	// 两端点都包含：三个点。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":1000,"end":3000}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 3 || qr.Series[0].Average != 2 {
		t.Fatalf("full range = %+v, want count 3 avg 2", qr.Series)
	}
	// start==end 只查该时间戳。
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":2000,"end":2000}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("single timestamp = %+v, want count 1 avg 2", qr.Series)
	}
	// 区间内无点：空数组。
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":1500,"end":1999}`)
	if len(qr.Series) != 0 {
		t.Fatalf("empty range = %+v, want empty series", qr.Series)
	}
}

func TestQueryLabelsMatchSemantics(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","zone":"x"}},
		{"name":"m","timestamp":1,"value":2,"labels":{"host":"a"}},
		{"name":"m","timestamp":1,"value":3,"labels":{"host":"b"}},
		{"name":"m","timestamp":1,"value":4,"labels":{"zone":""}},
		{"name":"m","timestamp":1,"value":5}
	]`)

	// 只给 host=a：命中前两条（额外标签不影响）。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1,"labels":{"host":"a"}}`)
	if len(qr.Series) != 2 {
		t.Fatalf("host=a matches %d series, want 2: %+v", len(qr.Series), qr.Series)
	}
	// 标签顺序不影响匹配：同一标签集合换序后命中相同序列。
	qr2a := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1,"labels":{"host":"a","zone":"x"}}`)
	qr2b := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1,"labels":{"zone":"x","host":"a"}}`)
	if len(qr2a.Series) != 1 || len(qr2b.Series) != 1 {
		t.Fatalf("reordered labels matches %d/%d series, want 1/1", len(qr2a.Series), len(qr2b.Series))
	}
	// 空字符串只匹配实际存在的空值标签，不匹配缺少该标签的序列。
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1,"labels":{"zone":""}}`)
	if len(qr.Series) != 1 || qr.Series[0].Labels["zone"] != "" {
		t.Fatalf("zone= empty matches %+v, want only the actual empty-label series", qr.Series)
	}
	// 省略 labels 与空对象都匹配该指标全部序列。
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1}`)
	if len(qr.Series) != 5 {
		t.Fatalf("omitted labels matches %d series, want 5", len(qr.Series))
	}
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1,"labels":{}}`)
	if len(qr.Series) != 5 {
		t.Fatalf("empty labels matches %d series, want 5", len(qr.Series))
	}
	// 无标签序列输出空对象（在排序中位于最前）。
	var noLabel *QuerySeries
	for i := range qr.Series {
		if len(qr.Series[i].Labels) == 0 {
			noLabel = &qr.Series[i]
			break
		}
	}
	if noLabel == nil {
		t.Fatalf("no-label series not found in %+v", qr.Series)
	}
}

func TestQueryNoMatchReturnsEmptyArray(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	qr := mustQuery(t, store, `{"op":"query","name":"other","start":1,"end":1}`)
	if qr.Series == nil || len(qr.Series) != 0 {
		t.Fatalf("no-match result = %+v, want empty non-nil series", qr.Series)
	}
}

func TestQueryLargeValuesFiniteMean(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1.7976931348623157e308},
		{"name":"m","timestamp":2,"value":1.7976931348623157e308}
	]`)
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if len(qr.Series) != 1 {
		t.Fatalf("series = %+v", qr.Series)
	}
	avg := qr.Series[0].Average
	if math.IsInf(avg, 0) || math.IsNaN(avg) {
		t.Fatalf("mean of two 1e308 values = %v, must be finite", avg)
	}
	if avg != 1.7976931348623157e308 {
		t.Fatalf("mean = %v, want 1.7976931348623157e308", avg)
	}

	// 异号大值：均值为 0，不得因中间求和溢出。
	mustOK(t, store, `[
		{"name":"n","timestamp":1,"value":-1.7976931348623157e308},
		{"name":"n","timestamp":2,"value":1.7976931348623157e308}
	]`)
	qr = mustQuery(t, store, `{"op":"query","name":"n","start":1,"end":2}`)
	avg = qr.Series[0].Average
	if math.IsInf(avg, 0) || math.IsNaN(avg) || avg != 0 {
		t.Fatalf("mean of opposite max values = %v, want 0", avg)
	}
}

func TestQueryDoesNotMutateStore(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	before := mustOK(t, store, `[]`)
	mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1}`)
	after := mustOK(t, store, `[]`)
	if len(after.Series) != len(before.Series) {
		t.Fatalf("query mutated series count: %d -> %d", len(before.Series), len(after.Series))
	}
	if after.Series[0].Points[0].Value != 1 {
		t.Fatalf("query mutated stored value: %+v", after.Series[0].Points)
	}
}

func TestQueryDeterministicRegardlessOfWriteOrder(t *testing.T) {
	values := []float64{1, 3, 5, 7, 9}
	build := func(order []int) *MetricStore {
		store := NewMetricStore()
		for _, i := range order {
			line := fmt.Sprintf(`[{"name":"m","timestamp":%d,"value":%v}]`, i+1, values[i])
			mustOK(t, store, line)
		}
		return store
	}
	q := `{"op":"query","name":"m","start":1,"end":5}`
	qr1 := mustQuery(t, build([]int{0, 1, 2, 3, 4}), q)
	qr2 := mustQuery(t, build([]int{4, 3, 2, 1, 0}), q)
	qr3 := mustQuery(t, build([]int{2, 0, 4, 1, 3}), q)
	if qr1.Series[0].Average != qr2.Series[0].Average || qr1.Series[0].Average != qr3.Series[0].Average {
		t.Fatalf("averages differ by write order: %v %v %v",
			qr1.Series[0].Average, qr2.Series[0].Average, qr3.Series[0].Average)
	}
	if qr1.Series[0].Count != 5 || qr1.Series[0].Average != 5 {
		t.Fatalf("avg = %v, want 5", qr1.Series[0].Average)
	}
}

func TestQueryFailedBatchNotVisible(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	// 整批失败：本批点不得参与查询。
	mustFail(t, store, `[{"name":"m","timestamp":2,"value":2},{"name":"m","timestamp":1,"value":999}]`)
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 1 {
		t.Fatalf("failed batch must not be visible: %+v", qr.Series)
	}
}

func TestQueryRejectsInvalidObjects(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{`{"name":"m","start":1,"end":2}`, `missing required field "op"`},
		{`{"op":"query","start":1,"end":2}`, `missing required field "name"`},
		{`{"op":"query","name":"m","end":2}`, `missing required field "start"`},
		{`{"op":"query","name":"m","start":1}`, `missing required field "end"`},
		{`{"op":"query","name":"","start":1,"end":2}`, `non-empty string`},
		{`{"op":"ingest","name":"m","start":1,"end":2}`, `unknown op`},
		{`{"op":"query","name":"m","start":2,"end":1}`, `invalid range`},
		{`{"op":"query","name":"m","start":1,"end":2,"extra":1}`, `unknown field`},
		{`{"op":"query","op":"query","name":"m","start":1,"end":2}`, `duplicate field`},
		{`{"op":1,"name":"m","start":1,"end":2}`, `field "op" must be a string`},
		{`{"op":"query","name":7,"start":1,"end":2}`, `field "name" must be a string`},
		{`{"op":"query","name":"m","start":"1","end":2}`, `field "start"`},
		{`{"op":"query","name":"m","start":1.5,"end":2}`, `field "start"`},
		{`{"op":"query","name":"m","start":1,"end":true}`, `field "end"`},
		{`{"op":"query","name":"m","start":1,"end":2,"labels":[]}`, `field "labels"`},
		{`{"op":"query","name":"m","start":1,"end":2,"labels":null}`, `field "labels"`},
		{`{"op":"query","name":"m","start":1,"end":2,"labels":{"": "x"}}`, `label keys must be non-empty`},
		{`{"op":"query","name":"m","start":1,"end":2,"labels":{"k": 2}}`, `must be a string`},
		{`{"op":"query","name":"m","start":1,"end":2,"labels":{"k":"1","k":"2"}}`, `duplicate label key`},
		{`{"op":"query","name":"m","start":9223372036854775808,"end":2}`, `field "start"`},
	}
	for _, tc := range cases {
		store := NewMetricStore()
		lerr := mustFail(t, store, tc.line)
		if lerr.Index != 0 {
			t.Errorf("line %s: query error must not carry index, got %d", tc.line, lerr.Index)
		}
		if !strings.Contains(lerr.Error, tc.want) {
			t.Errorf("line %s: error = %q, want substring %q", tc.line, lerr.Error, tc.want)
		}
	}
}

func TestQueryResultShape(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1}`)
	b, err := json.Marshal(qr)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "ok" || got["op"] != "query" {
		t.Fatalf("envelope = %v", got)
	}
	s0 := got["series"].([]interface{})[0].(map[string]interface{})
	if s0["labels"].(map[string]interface{}) == nil {
		t.Fatal("labels should serialize as {} not null")
	}
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 1 {
		t.Fatalf("count/average = %v/%v", s0["count"], s0["average"])
	}
}
