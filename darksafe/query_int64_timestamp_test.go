package darksafe

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件回归保障区间查询在整个 int64 毫秒范围内按真实时间戳精确选点：
//
//   - 9007199254740992（2^53）与 9007199254740993（2^53+1）是相邻毫秒，但
//     2^53+1 已超出 float64 可精确表示的整数范围（舍入后落回 2^53）。两个位置
//     分别保存不同的值后，单点查询各得一个点、平均分别为各自的值，包含两者的
//     区间才得到两个点；任何精度丢失都会把两点合并（写入时即冲突）或把另一位置
//     的值算进单点查询。负的大整数遵循同一规则。
//   - 点按非时间顺序写入时，区间选择仍由实际时间戳决定；查询结果中的指标名与
//     完整标签集合必须对应被统计的那条序列。
//   - -9223372036854775808、0、9223372036854775807 都是合法时间戳：端点相等
//     只统计该端点，最小到最大的区间包含全部点，跨零的较窄区间只含真正落入的
//     点；没有采样的区间返回成功与空 series 数组，不产生 count 为零的序列，
//     更不能把无数据解释成平均值零。
//   - start 或 end 超出 int64 范围时报该字段越界；两者合法但 start > end 时
//     报告区间倒置并保留实际边界值。失败查询不带 index、conflict 与统计结果，
//     不改变已写入数据。

const (
	ts2to53  = 9007199254740992 // 2^53：float64 可精确表示的最大连续整数
	ts2to53p = 9007199254740993 // 2^53+1：作为 float64 会舍入回 2^53
)

// assertSingleSeries 校验查询结果恰好包含一条序列，身份、点数与平均值符合预期。
func assertSingleSeries(t *testing.T, res *QueryResult, note string, wantCount int, wantAvg float64) QuerySeries {
	t.Helper()
	if res.Status != "ok" || res.Op != "query" {
		t.Fatalf("%s: status/op = %q/%q, want ok/query", note, res.Status, res.Op)
	}
	if len(res.Series) != 1 {
		t.Fatalf("%s: series = %+v, want exactly one", note, res.Series)
	}
	s0 := res.Series[0]
	if s0.Count != wantCount || s0.Average != wantAvg {
		t.Fatalf("%s: count/average = %d/%v, want %d/%v", note, s0.Count, s0.Average, wantCount, wantAvg)
	}
	return s0
}

// TestQueryInt64AdjacentMillisecondPrecision 是核心精度保障：同一序列在
// 2^53 与 2^53+1 两个毫秒位置分别保存 2 与 8。若时间戳在任何环节被当成
// 浮点数处理，两个位置会合并为同一个键——写入阶段就会冲突或覆盖，单点查询
// 也会把另一位置的值算进来。两个点必须保持独立可寻址。
func TestQueryInt64AdjacentMillisecondPrecision(t *testing.T) {
	store := NewMetricStore()
	// 两个位置同一批写入：若被合并，第二个点会与第一个冲突，mustOK 直接失败。
	res := mustOK(t, store, `[
		{"name":"cpu","timestamp":9007199254740992,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":9007199254740993,"value":8,"labels":{"host":"a"}}
	]`)
	if res.Added != 2 || res.Duplicates != 0 {
		t.Fatalf("adjacent millisecond writes = added %d duplicates %d, want 2/0 (timestamps must not merge)",
			res.Added, res.Duplicates)
	}
	// 写入快照必须列出两个不同时间戳，且 JSON 输出保留 2^53+1 的精确文本。
	pts := res.Series[0].Points
	if len(pts) != 2 || pts[0].Timestamp != ts2to53 || pts[1].Timestamp != ts2to53p {
		t.Fatalf("snapshot points = %+v, want timestamps %d and %d", pts, ts2to53, ts2to53p)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"timestamp":9007199254740993`) {
		t.Fatalf("snapshot JSON lost the 2^53+1 timestamp: %s", raw)
	}

	// 只查前一个位置：恰好一个点，平均为 2，后一个位置的 8 不得参与。
	s0 := assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740992,"labels":{"host":"a"}}`),
		"query [2^53,2^53]", 1, 2)
	if s0.Name != "cpu" || len(s0.Labels) != 1 || s0.Labels["host"] != "a" {
		t.Fatalf("query [2^53,2^53] identity = %+v, want cpu{host=a}", s0)
	}

	// 只查后一个位置：恰好一个点，平均为 8，前一个位置的 2 不得参与。
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":9007199254740993,"end":9007199254740993,"labels":{"host":"a"}}`),
		"query [2^53+1,2^53+1]", 1, 8)

	// 包含两者的闭区间才得到两个点，平均 (2+8)/2 = 5。
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740993,"labels":{"host":"a"}}`),
		"query [2^53,2^53+1]", 2, 5)

	// 端点外扩一毫秒仍只命中这两个点，不能把区间外的点算进来。
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":9007199254740991,"end":9007199254740994}`),
		"query [2^53-1,2^53+2]", 2, 5)
}

// TestQueryInt64AdjacentMillisecondPrecisionNegative 相同规则适用于负的大整数：
// -2^53 与 -(2^53+1) 同样是两个不同的采样位置，不得因精度丢失合并。
func TestQueryInt64AdjacentMillisecondPrecisionNegative(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"cpu","timestamp":-9007199254740993,"value":8},
		{"name":"cpu","timestamp":-9007199254740992,"value":2}
	]`)
	if res.Added != 2 {
		t.Fatalf("negative adjacent writes = added %d, want 2 (timestamps must not merge)", res.Added)
	}
	// 快照按时间戳升序：-(2^53+1) 在前。
	pts := res.Series[0].Points
	if len(pts) != 2 || pts[0].Timestamp != -ts2to53p || pts[1].Timestamp != -ts2to53 {
		t.Fatalf("snapshot points = %+v, want timestamps %d and %d", pts, -ts2to53p, -ts2to53)
	}

	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":-9007199254740993,"end":-9007199254740993}`),
		"query [-(2^53+1),-(2^53+1)]", 1, 8)
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":-9007199254740992,"end":-9007199254740992}`),
		"query [-2^53,-2^53]", 1, 2)
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":-9007199254740993,"end":-9007199254740992}`),
		"query [-(2^53+1),-2^53]", 2, 5)
}

// TestQueryInt64OutOfOrderWritesSelectByTimestamp 点按非时间顺序写入（跨批次、
// 先大后小）时，区间选择仍由实际时间戳决定；返回的指标名与完整标签集合对应
// 被统计的序列，相邻毫秒上其他序列的点不混入。
func TestQueryInt64OutOfOrderWritesSelectByTimestamp(t *testing.T) {
	store := NewMetricStore()
	// 先写 2^53+1，再写 2^53，再写 2^53-1：写入顺序与时间顺序相反。
	mustOK(t, store, `[{"name":"cpu","timestamp":9007199254740993,"value":8,"labels":{"host":"a","zone":"z1"}}]`)
	mustOK(t, store, `[{"name":"cpu","timestamp":9007199254740992,"value":2,"labels":{"host":"a","zone":"z1"}}]`)
	mustOK(t, store, `[{"name":"cpu","timestamp":9007199254740991,"value":4,"labels":{"host":"a","zone":"z1"}}]`)
	// 同名指标、相邻毫秒上的另一条序列（标签不同）：不得混入 host=a 的统计。
	mustOK(t, store, `[{"name":"cpu","timestamp":9007199254740992,"value":100,"labels":{"host":"b"}}]`)

	// 中间位置的单点查询：只命中 2^53 上的 2，与写入先后无关。
	s0 := assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740992,"labels":{"host":"a"}}`),
		"out-of-order point query", 1, 2)
	if s0.Name != "cpu" || len(s0.Labels) != 2 || s0.Labels["host"] != "a" || s0.Labels["zone"] != "z1" {
		t.Fatalf("out-of-order point query identity = %+v, want cpu{host=a,zone=z1}", s0)
	}

	// 全区间：host=a 序列三个点平均 (4+2+8)/3 = 14/3；host=b 序列独立成行。
	res := mustQuery(t, store, `{"op":"query","name":"cpu","start":9007199254740991,"end":9007199254740993}`)
	if len(res.Series) != 2 {
		t.Fatalf("full range series = %+v, want two series", res.Series)
	}
	// 序列次序沿用 snapshot 排序：{host=a,zone=z1} 的标签对排在 {host=b} 前。
	if res.Series[0].Labels["host"] != "a" || res.Series[0].Count != 3 || res.Series[0].Average != 14.0/3.0 {
		t.Fatalf("host=a series = %+v, want count 3 average 14/3", res.Series[0])
	}
	if res.Series[1].Labels["host"] != "b" || res.Series[1].Count != 1 || res.Series[1].Average != 100 {
		t.Fatalf("host=b series = %+v, want count 1 average 100", res.Series[1])
	}

	// 只覆盖后写入的两个位置：先写的 2^53+1 与后写的 2^53-1 都在区间内，
	// 与写入顺序无关地各计一次。
	s0 = assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"cpu","start":9007199254740991,"end":9007199254740992,"labels":{"host":"a","zone":"z1"}}`),
		"out-of-order [2^53-1,2^53]", 2, 3)
	if len(s0.Labels) != 2 {
		t.Fatalf("subset-label query must return the full label set, got %+v", s0.Labels)
	}
}

// TestQueryInt64ExtremeTimestamps 合法时间戳覆盖整个 int64 范围：
// -9223372036854775808、0、9223372036854775807 都可写入并按闭区间选择。
func TestQueryInt64ExtremeTimestamps(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"m","timestamp":-9223372036854775808,"value":1},
		{"name":"m","timestamp":0,"value":2},
		{"name":"m","timestamp":9223372036854775807,"value":3}
	]`)
	if res.Added != 3 {
		t.Fatalf("extreme timestamp writes = added %d, want 3", res.Added)
	}
	pts := res.Series[0].Points
	if len(pts) != 3 || pts[0].Timestamp != -9223372036854775808 || pts[1].Timestamp != 0 ||
		pts[2].Timestamp != 9223372036854775807 {
		t.Fatalf("extreme points = %+v, want min/0/max in ascending order", pts)
	}

	// 端点相等：只统计该端点一个点。
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"m","start":-9223372036854775808,"end":-9223372036854775808}`),
		"point query at int64 min", 1, 1)
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":0}`),
		"point query at zero", 1, 2)
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"m","start":9223372036854775807,"end":9223372036854775807}`),
		"point query at int64 max", 1, 3)

	// 最小值到最大值的区间包含全部三个点，平均 (1+2+3)/3 = 2。
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"m","start":-9223372036854775808,"end":9223372036854775807}`),
		"full int64 range", 3, 2)

	// 跨过零的较窄区间只包含真正落在范围内的点（此处只有时间戳 0）。
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"m","start":-100,"end":100}`),
		"narrow range across zero", 1, 2)

	// 端点紧邻极值但不含极值本身：[-min+1, max-1] 内同样只有 0。
	assertSingleSeries(t,
		mustQuery(t, store, `{"op":"query","name":"m","start":-9223372036854775807,"end":9223372036854775806}`),
		"range just inside the extremes", 1, 2)
}

// TestQueryInt64EmptyRangeReturnsEmptySeries 没有采样的区间返回成功与空 series
// 数组：不生成 count 为零的序列，也不能把无数据解释成平均值零。
func TestQueryInt64EmptyRangeReturnsEmptySeries(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":-9223372036854775808,"value":1},
		{"name":"m","timestamp":9007199254740993,"value":8}
	]`)

	emptyQueries := []struct {
		note string
		line string
	}{
		// 两个已存点之间的空档。
		{"gap between stored points", `{"op":"query","name":"m","start":-9007199254740993,"end":9007199254740992}`},
		// 紧邻已存点之外。
		{"just above the largest point", `{"op":"query","name":"m","start":9007199254740994,"end":9223372036854775807}`},
		// 指标名不存在。
		{"unknown metric", `{"op":"query","name":"nope","start":-9223372036854775808,"end":9223372036854775807}`},
		// 标签条件不命中。
		{"unmatched labels", `{"op":"query","name":"m","start":-9223372036854775808,"end":9223372036854775807,"labels":{"host":"a"}}`},
	}
	for _, tc := range emptyQueries {
		res := mustQuery(t, store, tc.line)
		if res.Status != "ok" || res.Op != "query" {
			t.Fatalf("%s: status/op = %q/%q, want ok/query", tc.note, res.Status, res.Op)
		}
		if res.Series == nil || len(res.Series) != 0 {
			t.Fatalf("%s: series = %+v, want a non-nil empty array (no count=0 entries)", tc.note, res.Series)
		}
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"status":"ok","op":"query","series":[]}` {
			t.Fatalf("%s: JSON = %s, want an empty series array, not null and no zero-average entry", tc.note, raw)
		}
	}
}

// TestQueryInt64BoundaryFailures 保护查询边界失败的原有结果：
// start/end 超出 int64 范围时报该字段越界；两者合法但 start > end 时报告
// 区间倒置并保留实际边界值；失败查询不带 index、conflict 与统计结果，
// 不改变已写入数据，后续合法查询仍得到原来的统计。
func TestQueryInt64BoundaryFailures(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":9007199254740992,"value":2},
		{"name":"m","timestamp":9007199254740993,"value":8}
	]`)

	assertBaseline := func() {
		t.Helper()
		s0 := assertSingleSeries(t,
			mustQuery(t, store, `{"op":"query","name":"m","start":9007199254740992,"end":9007199254740993}`),
			"baseline after failures", 2, 5)
		if s0.Name != "m" || len(s0.Labels) != 0 {
			t.Fatalf("baseline identity = %+v, want m{}", s0)
		}
	}
	assertBaseline()

	// start 越界（低于 int64 最小值）：原因指出 start 字段越界。
	lerr := mustQueryFail(t, store, `{"op":"query","name":"m","start":-9223372036854775809,"end":0}`)
	assertQueryError(t, lerr, `"start"`)
	assertQueryError(t, lerr, "int64 range")

	// end 越界（高于 int64 最大值）：原因指出 end 字段越界。
	lerr = mustQueryFail(t, store, `{"op":"query","name":"m","start":0,"end":9223372036854775808}`)
	assertQueryError(t, lerr, `"end"`)
	assertQueryError(t, lerr, "int64 range")

	// 两者都合法但 start > end：报告区间倒置并保留实际边界值，
	// 大整数边界必须按原值呈现，不能被精度丢失改写。
	lerr = mustQueryFail(t, store, `{"op":"query","name":"m","start":9007199254740993,"end":9007199254740992}`)
	assertQueryError(t, lerr, "invalid range")
	assertQueryError(t, lerr, "9007199254740993 > 9007199254740992")

	// int64 两极构成的倒置区间：实际边界完整保留在原因中。
	lerr = mustQueryFail(t, store, `{"op":"query","name":"m","start":9223372036854775807,"end":-9223372036854775808}`)
	assertQueryError(t, lerr, "invalid range")
	assertQueryError(t, lerr, "9223372036854775807 > -9223372036854775808")

	// 越界与倒置同时存在时按字段书写顺序先报越界（end 的越界写在前面）。
	lerr = mustQueryFail(t, store, `{"op":"query","name":"m","end":9223372036854775808,"start":5}`)
	assertQueryError(t, lerr, `"end"`)
	assertQueryError(t, lerr, "int64 range")

	// 全部失败之后，已写入数据不变，合法查询仍得到原来的统计。
	assertBaseline()
}

// TestQueryInt64BoundaryFailureCarriesNoResult 失败查询只返回错误：
// 不附带 index、conflict，也不返回任何统计结果。
func TestQueryInt64BoundaryFailureCarriesNoResult(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":9007199254740992,"value":2}]`)

	for _, line := range []string{
		`{"op":"query","name":"m","start":-9223372036854775809,"end":0}`,
		`{"op":"query","name":"m","start":0,"end":9223372036854775808}`,
		`{"op":"query","name":"m","start":9007199254740993,"end":9007199254740992}`,
	} {
		res, lerr := store.QueryLine(line)
		if lerr == nil {
			t.Fatalf("line %s: expected failure, got result %+v", line, res)
		}
		if res != nil {
			t.Fatalf("line %s: failed query must not return a result, got %+v", line, res)
		}
		if lerr.Index != 0 || lerr.Conflict != nil {
			t.Fatalf("line %s: query failure must not carry index/conflict, got %+v", line, lerr)
		}
	}
}

// TestQueryInt64TimestampJSONRoundTrip 查询结果经 JSON 序列化再解析后，
// 大整数时间戳相关的统计（count、average）与序列身份保持不变。
func TestQueryInt64TimestampJSONRoundTrip(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":9007199254740992,"value":2,"labels":{"host":"a"}},
		{"name":"m","timestamp":9007199254740993,"value":8,"labels":{"host":"a"}}
	]`)
	res := mustQuery(t, store, `{"op":"query","name":"m","start":9007199254740992,"end":9007199254740993}`)
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var round QueryResult
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if len(round.Series) != 1 || round.Series[0].Count != 2 || round.Series[0].Average != 5 {
		t.Fatalf("round-tripped result = %+v (JSON %s), want count 2 average 5", round.Series, raw)
	}
	if round.Series[0].Name != "m" || round.Series[0].Labels["host"] != "a" {
		t.Fatalf("round-tripped identity = %+v", round.Series[0])
	}
}
