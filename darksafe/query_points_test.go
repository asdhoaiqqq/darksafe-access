package darksafe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 本文件回归保障只读操作 query_points：沿用 query 的查询条件（name 精确、
// labels 子集、[start,end] 闭区间），但返回区间内的原始采样明细而不是点数
// 与均值，使用户在发现均值异常后能定位到具体时间戳上存储的值。
//
// 已覆盖的核心场景：
//   - 区间包含两个端点；start == end 时只返回该时间戳上的采样；
//   - 每条命中序列保留完整指标名与完整标签集合（不缩减成查询条件），points
//     按时间戳升序、只含 timestamp 与 value，不附加 count 或 average；
//   - 序列排列次序与 query 成功结果一致；只列出区间内有点的序列；
//   - 指标不存在、标签条件未命中、区间内没有点都成功返回空 series 数组；
//   - 时间戳在 2^53 附近仍按 int64 精确选择，value 是实际存储的 float64；
//   - 查询只读：不改变已存数据，也只读到此前成功提交的点；
//   - 输入校验与失败原因的选择与 query 一致：倒置区间指出实际边界，
//     失败结果没有 series、index 或 conflict。

// mustQueryPoints 要求 query_points 查询成功并返回明细结果。
func mustQueryPoints(t *testing.T, store *MetricStore, line string) *QueryPointsResult {
	t.Helper()
	res, err := store.QueryPointsLine(line)
	if err != nil {
		t.Fatalf("expected query_points success for %s, got error: %+v", line, err)
	}
	return res
}

// mustQueryPointsFail 要求 query_points 查询失败并返回结构化错误。
func mustQueryPointsFail(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	res, err := store.QueryPointsLine(line)
	if err == nil {
		t.Fatalf("expected query_points failure for %s, got result: %+v", line, res)
	}
	return err
}

// queryPointsAll 构造省略 labels 的 query_points 对象文本。
func queryPointsAll(name string, start, end int64) string {
	return fmt.Sprintf(`{"op":"query_points","name":%s,"start":%d,"end":%d}`,
		jsonString(name), start, end)
}

// queryPointsLabels 构造按标签子集查询的 query_points 对象文本。
func queryPointsLabels(name string, start, end int64, labels map[string]string) string {
	return `{"op":"query_points","name":` + jsonString(name) +
		fmt.Sprintf(`,"start":%d,"end":%d,"labels":`, start, end) + jsonLabels(labels) + `}`
}

// assertPoints 断言一条明细序列的点与预期的 (timestamp, value) 序列完全一致。
func assertPoints(t *testing.T, s QueryPointsSeries, want []Point, note string) {
	t.Helper()
	if !reflect.DeepEqual(s.Points, want) {
		t.Fatalf("%s: points = %+v, want %+v", note, s.Points, want)
	}
}

// TestQueryPointsSpecExample 是任务书示例：cpu、host=a 在 1000/2000/3000 上
// 分别存 2、4、9，查询 [1000,2000] 只列出前两个点，3000 上的点不出现。
func TestQueryPointsSpecExample(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "a"}
	mustOK(t, store, "["+
		sample("cpu", 1000, 2, labels)+","+
		sample("cpu", 2000, 4, labels)+","+
		sample("cpu", 3000, 9, labels)+"]")

	res := mustQueryPoints(t, store, queryPointsLabels("cpu", 1000, 2000, labels))
	if res.Status != "ok" || res.Op != "query_points" {
		t.Fatalf("status/op = %q/%q, want ok/query_points", res.Status, res.Op)
	}
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", res.Series)
	}
	s0 := res.Series[0]
	if s0.Name != "cpu" || !sameLabels(s0.Labels, labels) {
		t.Fatalf("series identity = %+v, want cpu{host=a}", s0)
	}
	assertPoints(t, s0, []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}},
		"range [1000,2000] excludes the point at 3000")

	// start == end：只返回该时间戳上的采样。
	res = mustQueryPoints(t, store, queryPointsLabels("cpu", 3000, 3000, labels))
	assertPoints(t, res.Series[0], []Point{{Timestamp: 3000, Value: 9}}, "single timestamp 3000")
	res = mustQueryPoints(t, store, queryPointsLabels("cpu", 1000, 1000, labels))
	assertPoints(t, res.Series[0], []Point{{Timestamp: 1000, Value: 2}}, "single timestamp 1000")

	// 两个端点都包含：闭区间 [1000,3000] 列出全部三个点。
	res = mustQueryPoints(t, store, queryPointsLabels("cpu", 1000, 3000, labels))
	assertPoints(t, res.Series[0], []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 2000, Value: 4},
		{Timestamp: 3000, Value: 9},
	}, "inclusive range [1000,3000]")
}

// TestQueryPointsResultShape 锁定成功结果的 JSON 形状：op 为 query_points，
// 序列条目只有 name、labels、points（没有 count/average），点只有
// timestamp 与 value；无命中时 series 是空数组而不是 null。
func TestQueryPointsResultShape(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	res := mustQueryPoints(t, store, queryPointsAll("cpu", 0, 2000))
	raw := marshalCompact(t, res)
	want := `{"status":"ok","op":"query_points","series":[` +
		`{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":2}]}]}`
	if raw != want {
		t.Fatalf("query_points JSON = %s, want %s", raw, want)
	}
	// 序列化后的条目不得携带均值查询的统计字段。
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	entry := decoded["series"].([]any)[0].(map[string]any)
	for _, banned := range []string{"count", "average"} {
		if _, ok := entry[banned]; ok {
			t.Fatalf("query_points entry must not carry %q: %v", banned, entry)
		}
	}

	// 无命中：成功的空 series 数组。
	res = mustQueryPoints(t, store, queryPointsAll("cpu", 2000, 3000))
	if res.Series == nil || len(res.Series) != 0 {
		t.Fatalf("miss query_points = %+v, want non-nil empty series list", res.Series)
	}
	if raw = marshalCompact(t, res); raw != `{"status":"ok","op":"query_points","series":[]}` {
		t.Fatalf("empty query_points JSON = %s, want empty series array", raw)
	}
}

// TestQueryPointsEmptySuccesses 指标不存在、标签条件未命中、区间内没有点，
// 都成功返回空 series 数组，与失败（nil 结果 + LineError）明确区分。
func TestQueryPointsEmptySuccesses(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	cases := []struct {
		name string
		line string
	}{
		{"unknown metric", queryPointsAll("nope", 0, 2000)},
		{"label miss", queryPointsLabels("cpu", 0, 2000, map[string]string{"host": "zzz"})},
		{"missing label key", queryPointsLabels("cpu", 0, 2000, map[string]string{"zone": "x"})},
		{"no points in range", queryPointsAll("cpu", 1001, 2000)},
		{"empty store range", queryPointsAll("other", tsMinInt64, tsMaxInt64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mustQueryPoints(t, store, tc.line)
			if res.Status != "ok" || res.Op != "query_points" {
				t.Fatalf("status/op = %q/%q, want ok/query_points", res.Status, res.Op)
			}
			if res.Series == nil || len(res.Series) != 0 {
				t.Fatalf("series = %+v, want non-nil empty list", res.Series)
			}
			if raw := marshalCompact(t, res); raw != `{"status":"ok","op":"query_points","series":[]}` {
				t.Fatalf("JSON = %s, want empty series array", raw)
			}
		})
	}
}

// TestQueryPointsLabelSelectionAndOrder 标签仍按子集规则匹配：省略或 {}
// 查该指标全部序列，返回记录保留完整标签集合而不缩减成查询条件；序列之间
// 沿用与 query 成功结果一致的排列次序，每条序列的点按时间戳升序；
// 无标签序列的 labels 为 {}。
func TestQueryPointsLabelSelectionAndOrder(t *testing.T) {
	store := NewMetricStore()
	// 故意乱序写入时间戳与序列，验证输出次序与输入顺序无关。
	mustOK(t, store, `[
		{"name":"m","timestamp":20,"value":2,"labels":{"zone":"x","host":"h"}},
		{"name":"m","timestamp":10,"value":1},
		{"name":"m","timestamp":30,"value":3,"labels":{"zone":"x"}},
		{"name":"m","timestamp":10,"value":4,"labels":{"zone":"x","host":"h"}},
		{"name":"m","timestamp":30,"value":5},
		{"name":"m","timestamp":20,"value":6,"labels":{"zone":"x"}},
		{"name":"other","timestamp":10,"value":9}
	]`)

	// 省略 labels：该指标全部序列，次序与写入快照/query 一致
	// （无标签在前，同指标名按排序后标签键值对字典序）。
	res := mustQueryPoints(t, store, queryPointsAll("m", 0, 100))
	if len(res.Series) != 3 {
		t.Fatalf("all series = %+v, want 3", res.Series)
	}
	wantLabels := []map[string]string{
		{},
		{"host": "h", "zone": "x"},
		{"zone": "x"},
	}
	wantPoints := [][]Point{
		{{Timestamp: 10, Value: 1}, {Timestamp: 30, Value: 5}},
		{{Timestamp: 10, Value: 4}, {Timestamp: 20, Value: 2}},
		{{Timestamp: 20, Value: 6}, {Timestamp: 30, Value: 3}},
	}
	for i := range wantLabels {
		s := res.Series[i]
		if s.Name != "m" || !sameLabels(s.Labels, wantLabels[i]) {
			t.Fatalf("series[%d] identity = %+v, want m%v", i, s, wantLabels[i])
		}
		assertPoints(t, s, wantPoints[i], fmt.Sprintf("series[%d] ascending points", i))
	}
	// 无标签序列的 labels 序列化为 {} 而不是 null。
	if raw := marshalCompact(t, res.Series[0].Labels); raw != `{}` {
		t.Fatalf("unlabeled series labels JSON = %s, want {}", raw)
	}

	// 空对象等价于省略。
	res = mustQueryPoints(t, store, `{"op":"query_points","name":"m","start":0,"end":100,"labels":{}}`)
	if len(res.Series) != 3 {
		t.Fatalf("empty labels = %+v, want all 3 series", res.Series)
	}

	// 子集匹配 zone=x 命中两条序列，返回的完整标签集合不缩减成查询条件。
	res = mustQueryPoints(t, store, queryPointsLabels("m", 0, 100, map[string]string{"zone": "x"}))
	if len(res.Series) != 2 {
		t.Fatalf("subset match = %+v, want 2 series", res.Series)
	}
	if !sameLabels(res.Series[0].Labels, map[string]string{"host": "h", "zone": "x"}) {
		t.Fatalf("full labels must be preserved, got %v", res.Series[0].Labels)
	}
	if !sameLabels(res.Series[1].Labels, map[string]string{"zone": "x"}) {
		t.Fatalf("full labels must be preserved, got %v", res.Series[1].Labels)
	}

	// 区间过滤逐序列独立：只覆盖部分时间戳时，区间内没有点的序列不列出。
	res = mustQueryPoints(t, store, queryPointsAll("m", 15, 25))
	if len(res.Series) != 2 {
		t.Fatalf("range [15,25] = %+v, want the two series with points inside", res.Series)
	}
	assertPoints(t, res.Series[0], []Point{{Timestamp: 20, Value: 2}}, "host=h series in [15,25]")
	assertPoints(t, res.Series[1], []Point{{Timestamp: 20, Value: 6}}, "zone=x series in [15,25]")

	// name 精确匹配：不同指标不返回。
	res = mustQueryPoints(t, store, queryPointsAll("other", 0, 100))
	if len(res.Series) != 1 || res.Series[0].Name != "other" {
		t.Fatalf("other metric = %+v", res.Series)
	}
}

// TestQueryPointsInt64TimestampPrecision 范围判断与输出保留时间戳的整数
// 精度：2^53 与 2^53+1 是两个不同的采样位置，单点查询各自只列出该位置；
// int64 两个端点也能精确寻址。
func TestQueryPointsInt64TimestampPrecision(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "a"}
	// 非时间顺序写入：先 2^53+1 值 8，再 2^53 值 2。
	mustOK(t, store, "["+
		sample("cpu", tsTwoPow53Plus, 8, labels)+","+
		sample("cpu", tsTwoPow53, 2, labels)+"]")

	res := mustQueryPoints(t, store, queryPointsLabels("cpu", tsTwoPow53, tsTwoPow53, labels))
	assertPoints(t, res.Series[0], []Point{{Timestamp: tsTwoPow53, Value: 2}}, "only 2^53")
	res = mustQueryPoints(t, store, queryPointsLabels("cpu", tsTwoPow53Plus, tsTwoPow53Plus, labels))
	assertPoints(t, res.Series[0], []Point{{Timestamp: tsTwoPow53Plus, Value: 8}}, "only 2^53+1")
	// 闭区间包含两者，按时间戳升序输出（与写入顺序相反）。
	res = mustQueryPoints(t, store, queryPointsLabels("cpu", tsTwoPow53, tsTwoPow53Plus, labels))
	assertPoints(t, res.Series[0], []Point{
		{Timestamp: tsTwoPow53, Value: 2},
		{Timestamp: tsTwoPow53Plus, Value: 8},
	}, "both adjacent positions ascending")

	// int64 端点：MinInt64 与 MaxInt64 上的点可精确寻址，输出的时间戳整数不丢精度。
	mustOK(t, store, "["+
		sample("m", tsMaxInt64, 8, nil)+","+
		sample("m", tsMinInt64, 2, nil)+"]")
	res = mustQueryPoints(t, store, queryPointsAll("m", tsMinInt64, tsMaxInt64))
	assertPoints(t, res.Series[0], []Point{
		{Timestamp: tsMinInt64, Value: 2},
		{Timestamp: tsMaxInt64, Value: 8},
	}, "full int64 range")
	raw := marshalCompact(t, res)
	if !strings.Contains(raw, "9223372036854775807") || !strings.Contains(raw, "-9223372036854775808") {
		t.Fatalf("int64 boundary timestamps must serialize exactly, got %s", raw)
	}
}

// TestQueryPointsStoredFloat64Values value 展示实际存储的 float64 值：
// 十进制写法不保留、只保留转换后的二进制值；特殊值 -0 与极大值原样呈现。
func TestQueryPointsStoredFloat64Values(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":0.1},
		{"name":"m","timestamp":2,"value":1.0000000000000002},
		{"name":"m","timestamp":3,"value":1e308}
	]`)
	res := mustQueryPoints(t, store, queryPointsAll("m", 0, 10))
	want := []Point{
		{Timestamp: 1, Value: 0.1},
		{Timestamp: 2, Value: 1.0000000000000002},
		{Timestamp: 3, Value: 1e308},
	}
	assertPoints(t, res.Series[0], want, "stored float64 values")
	raw := marshalCompact(t, res)
	if !strings.Contains(raw, `"value":1e+308`) {
		t.Fatalf("large value must serialize as stored float64, got %s", raw)
	}
}

// TestQueryPointsReadOnlyAndCommittedOnly query_points 只读取此前成功提交
// 的数据：失败批次回滚的点不可见，等值重复不新增点；查询本身不改变存储，
// 重复查询结果一致，随后的写入快照与均值查询保持原状。
func TestQueryPointsReadOnlyAndCommittedOnly(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":10,"value":2}]`)
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":4}]`)

	baseline := func() {
		t.Helper()
		res := mustQueryPoints(t, store, queryPointsAll("m", 0, 100))
		if len(res.Series) != 1 {
			t.Fatalf("baseline series = %+v, want one", res.Series)
		}
		assertPoints(t, res.Series[0], []Point{
			{Timestamp: 10, Value: 2},
			{Timestamp: 20, Value: 4},
		}, "baseline points")
	}
	baseline()

	// 失败批次整批回滚：其中任何点都不能被 query_points 看到。
	mustFail(t, store, `[{"name":"m","timestamp":30,"value":8},{"name":"m","timestamp":10,"value":100}]`)
	baseline()

	// 等值重复被忽略，不新增点。
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":4.0}]`)
	baseline()

	// 查询不改变存储：重复查询一致，写入快照与均值查询结果不变。
	res1 := mustQueryPoints(t, store, queryPointsAll("m", 0, 100))
	res2 := mustQueryPoints(t, store, queryPointsAll("m", 0, 100))
	if !reflect.DeepEqual(res1, res2) {
		t.Fatalf("repeated query_points differs: %+v vs %+v", res1, res2)
	}
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 2 {
		t.Fatalf("query_points mutated storage, snapshot = %+v", snap.Series)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("query after query_points = %+v, want count 2 average 3", qr.Series)
	}
}

// TestQueryPointsFailuresFollowQueryConventions 输入校验与失败原因的选择
// 沿用 query 的约定：字段错误、缺字段、未知 op、倒置区间（指出实际边界）
// 都返回结构化错误，不带 index 与 conflict，也不返回 series；失败不改变存储。
func TestQueryPointsFailuresFollowQueryConventions(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	cases := []struct {
		name string
		line string
		want string
	}{
		{"name wrong type", `{"op":"query_points","name":7,"start":0,"end":1}`, `field "name" must be a string`},
		{"empty name", `{"op":"query_points","name":"","start":0,"end":1}`, `field "name" must be a non-empty string`},
		{"start wrong type", `{"op":"query_points","name":"cpu","start":"0","end":1}`, `field "start" must be a JSON number`},
		{"start not integer", `{"op":"query_points","name":"cpu","start":0.5,"end":1}`, `field "start"`},
		{"end beyond int64", `{"op":"query_points","name":"cpu","start":0,"end":9223372036854775808}`,
			`field "end"`},
		{"missing end", `{"op":"query_points","name":"cpu","start":0}`, `missing required field "end"`},
		{"missing op", `{"name":"cpu","start":0,"end":1}`, `missing required field "op"`},
		{"unknown field", `{"op":"query_points","name":"cpu","start":0,"end":1,"extra":1}`, `unknown field "extra"`},
		{"duplicate field", `{"op":"query_points","name":"cpu","start":0,"start":1,"end":2}`, `duplicate field "start"`},
		{"labels wrong type", `{"op":"query_points","name":"cpu","start":0,"end":1,"labels":[]}`,
			`field "labels" must be an object`},
		{"duplicate label key", `{"op":"query_points","name":"cpu","start":0,"end":1,"labels":{"k":"a","k":"b"}}`,
			`duplicate label key "k"`},
		{"unknown op", `{"op":"ping","name":"cpu","start":0,"end":1}`, `unknown op "ping"`},
		{"inverted range", `{"op":"query_points","name":"cpu","start":5000,"end":1000}`,
			`invalid range: "start" must not be greater than "end" (5000 > 1000)`},
		{"inverted range at extremes",
			queryPointsAll("cpu", tsMaxInt64, tsMinInt64),
			fmt.Sprintf("%d > %d", tsMaxInt64, tsMinInt64)},
		{"trailing content", `{"op":"query_points","name":"cpu","start":0,"end":1} trailing`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lerr := mustQueryPointsFail(t, store, tc.line)
			assertQueryError(t, lerr, tc.want)
			// 失败结果没有 series、index 或 conflict。
			m := marshalLineError(t, lerr)
			for _, banned := range []string{"series", "index", "conflict"} {
				if _, ok := m[banned]; ok {
					t.Fatalf("query_points failure must not carry %q: %v", banned, m)
				}
			}
		})
	}

	// 所有失败之后，已存数据原样可见。
	res := mustQueryPoints(t, store, queryPointsAll("cpu", 0, 2000))
	if len(res.Series) != 1 {
		t.Fatalf("stored data changed after failures: %+v", res.Series)
	}
	assertPoints(t, res.Series[0], []Point{{Timestamp: 1000, Value: 2}}, "baseline after failures")
}

// TestQueryPointsViaProcessLine 统一入口对 query_points 对象返回非 nil 的
// *QueryPointsResult；失败仍是真正的 nil 结果。专用入口分工：QueryLine 只
// 执行 op "query"，QueryPointsLine 只执行 op "query_points"，交叉使用报错。
func TestQueryPointsViaProcessLine(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	r, lerr := store.ProcessLine(`{"op":"query_points","name":"cpu","start":0,"end":2000}`)
	if lerr != nil || r == nil {
		t.Fatalf("ProcessLine query_points: r=%v lerr=%+v", r, lerr)
	}
	qp, ok := r.(*QueryPointsResult)
	if !ok {
		t.Fatalf("ProcessLine query_points result type = %T, want *QueryPointsResult", r)
	}
	if qp.Op != "query_points" || len(qp.Series) != 1 {
		t.Fatalf("ProcessLine query_points = %+v", qp)
	}

	// 倒置区间经统一入口失败：真正的 nil 结果 + 结构化错误。
	r, lerr = store.ProcessLine(`{"op":"query_points","name":"cpu","start":9,"end":1}`)
	if r != nil || lerr == nil {
		t.Fatalf("inverted query_points via ProcessLine: r=%v lerr=%+v", r, lerr)
	}
	if !strings.Contains(lerr.Error, "9 > 1") {
		t.Fatalf("inverted range error = %q, want actual bounds", lerr.Error)
	}

	// 专用入口交叉使用：op 与入口不匹配时给出明确错误，不返回另一类型的结果。
	if _, lerr = store.QueryLine(`{"op":"query_points","name":"cpu","start":0,"end":1}`); lerr == nil {
		t.Fatalf("QueryLine must reject op query_points")
	}
	if _, lerr = store.QueryPointsLine(`{"op":"query","name":"cpu","start":0,"end":1}`); lerr == nil {
		t.Fatalf("QueryPointsLine must reject op query")
	}
}

// TestQueryPointsInterleavedWithWritesAndQueries 写入、均值查询与明细查询在
// 同一存储上交替进行：各自只读/原子写入的语义互不影响。
func TestQueryPointsInterleavedWithWritesAndQueries(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	res := mustQueryPoints(t, store, queryPointsLabels("cpu", 0, 3000, map[string]string{"host": "a"}))
	assertPoints(t, res.Series[0], []Point{{Timestamp: 1000, Value: 2}}, "after first write")

	mustOK(t, store, `[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`)
	res = mustQueryPoints(t, store, queryPointsLabels("cpu", 0, 3000, map[string]string{"host": "a"}))
	assertPoints(t, res.Series[0], []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 2000, Value: 4},
	}, "after second write")

	// 均值查询不受明细查询影响，仍按原语义统计。
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("query = %+v, want count 2 average 3", qr.Series)
	}
}
