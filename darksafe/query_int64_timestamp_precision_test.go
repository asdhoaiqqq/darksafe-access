package darksafe

import (
	"encoding/json"
	"fmt"
	"testing"
)

// 本文件为区间查询在整个 int64 毫秒时间戳范围内的精确选择补充回归保障。
// 时间戳以 int64 保存，JSON 经 json.Number 解析，不能退化为 float64：
// 在 2^53=9007199254740992 附近 float64 的间距已是 1 毫秒，任何把时间戳
// 当成 float64 的实现都会把 2^53 与 2^53+1 两个不同采样点合并成一个位置，
// 并让另一个位置的值污染单点查询。
//
// 已覆盖的核心场景：
//   - 同一序列在 2^53 和 2^53+1 分别存值 2 和 8（先写 2^53+1 后写 2^53，
//     点按非时间顺序写入）：单独查任一端点只得到该位置的一个点，含两端的
//     闭区间才得到两个点、平均值 5；
//   - 同样的规则适用于负的大整数对 -2^53-1/-2^53；
//   - 合法时间戳 MinInt64、0、MaxInt64：start==end 只统计该端点，全范围
//     [MinInt64,MaxInt64] 包含全部点，跨零窄区间只包含真正落在范围内的点；
//   - 没有采样的区间返回成功与空 series 数组，不生成 count 为零的序列，
//     更不能把无数据解释为平均值零；
//   - 查询返回的指标名与完整标签集合必须对应被统计的序列；
//   - start/end 超出 int64 时返回指出该字段越界的 error；start>end 报告
//     区间倒置并保留实际边界值；失败查询无 index、无 conflict、无统计结果，
//     不改变已写入的数据。

const (
	tsTwoPow53     int64 = 9007199254740992
	tsTwoPow53Plus int64 = 9007199254740993
	tsNegTwoPow53  int64 = -9007199254740992
	tsNegPairFar   int64 = -9007199254740993
	tsMinInt64     int64 = -9223372036854775808
	tsMaxInt64     int64 = 9223372036854775807
)

// assertSeriesStats 断言查询恰好命中一条序列，且名称、完整标签集合、count、
// average 全部等于预期；无命中时调用 mustQuery 后检查 len(Series)==0。
func assertSeriesStats(t *testing.T, s QuerySeries, name string, labels map[string]string, count int, average float64, note string) {
	t.Helper()
	if s.Name != name {
		t.Fatalf("%s: name = %q, want %q", note, s.Name, name)
	}
	if !sameLabels(s.Labels, labels) {
		t.Fatalf("%s: labels = %v, want full set %v", note, s.Labels, labels)
	}
	if s.Count != count {
		t.Fatalf("%s: count = %d, want %d", note, s.Count, count)
	}
	if s.Average != average {
		t.Fatalf("%s: average = %v, want %v", note, s.Average, average)
	}
}

// sameLabels 比较两个标签集合完全相同（nil 与空 map 等价）。
func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestQuerySeparatesAdjacentTimestampsAtTwoPow53 是核心精度回归：
// 2^53 与 2^53+1 两个毫秒位置必须是两个不同采样点。点按非时间顺序写入
// （先 2^53+1 值 8，后 2^53 值 2），区间选择只由实际时间戳决定。
func TestQuerySeparatesAdjacentTimestampsAtTwoPow53(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "a"}
	// 非时间顺序写入：先存值 8 于 2^53+1，再存值 2 于 2^53。
	mustOK(t, store, "["+
		sample("cpu", tsTwoPow53Plus, 8, labels)+","+
		sample("cpu", tsTwoPow53, 2, labels)+"]")

	// 单独查 2^53：只拿到该位置的值 2，不能把邻近的 8 算进来。
	s := queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53, labels))
	assertSeriesStats(t, s, "cpu", labels, 1, 2, "single-point query at 2^53")

	// 单独查 2^53+1：只拿到该位置的值 8。
	s = queryOne(t, store, queryLabels("cpu", tsTwoPow53Plus, tsTwoPow53Plus, labels))
	assertSeriesStats(t, s, "cpu", labels, 1, 8, "single-point query at 2^53+1")

	// 含两端的闭区间 [2^53, 2^53+1] 才统计两个点，平均 (2+8)/2 = 5。
	s = queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53Plus, labels))
	assertSeriesStats(t, s, "cpu", labels, 2, 5, "range covering both adjacent positions")

	// 紧邻位置对 + 一个区间外点（同一序列 2^53-1 值 100）：
	// 单点查询仍只统计端点本身；[2^53, 2^53+1] 仍只统计这两个点，
	// 相邻外点不能因精度丢失被并入区间。
	mustOK(t, store, "["+sample("cpu", tsTwoPow53-1, 100, labels)+"]")
	s = queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53, labels))
	assertSeriesStats(t, s, "cpu", labels, 1, 2, "single-point query with neighbor below")
	s = queryOne(t, store, queryLabels("cpu", tsTwoPow53Plus, tsTwoPow53Plus, labels))
	assertSeriesStats(t, s, "cpu", labels, 1, 8, "single-point query with neighbor below")
	s = queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53Plus, labels))
	assertSeriesStats(t, s, "cpu", labels, 2, 5, "two-point range stays two points")

	// 只覆盖 2^53-1 的窄区间：count=1、平均 100。
	s = queryOne(t, store, queryLabels("cpu", tsTwoPow53-1, tsTwoPow53-1, labels))
	assertSeriesStats(t, s, "cpu", labels, 1, 100, "neighbor-below point query")

	// 快照中两个紧邻位置各自独立列出、按时间戳升序排列，值一一对应。
	snap := mustOK(t, store, `[]`)
	pts := snap.Series[0].Points
	if len(pts) != 3 {
		t.Fatalf("snapshot points = %+v, want 3 distinct timestamps", pts)
	}
	wantTS := []int64{tsTwoPow53 - 1, tsTwoPow53, tsTwoPow53Plus}
	wantVal := []float64{100, 2, 8}
	for i := range wantTS {
		if pts[i].Timestamp != wantTS[i] || pts[i].Value != wantVal[i] {
			t.Fatalf("snapshot point %d = (%d,%v), want (%d,%v)",
				i, pts[i].Timestamp, pts[i].Value, wantTS[i], wantVal[i])
		}
	}
}

// TestQuerySeparatesAdjacentNegativeLargeTimestamps 负的大整数对同样适用：
// -2^53-1 与 -2^53 是不同采样点，单点查询各自只统计该位置，闭区间包含
// 两者时 count=2、平均 5。点同样按非时间顺序写入。
func TestQuerySeparatesAdjacentNegativeLargeTimestamps(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "n"}
	// 非时间顺序写入：先写较大（靠近零）的 -2^53 值 2，再写 -2^53-1 值 8。
	mustOK(t, store, "["+
		sample("neg", tsNegTwoPow53, 2, labels)+","+
		sample("neg", tsNegPairFar, 8, labels)+"]")

	s := queryOne(t, store, queryLabels("neg", tsNegPairFar, tsNegPairFar, labels))
	assertSeriesStats(t, s, "neg", labels, 1, 8, "single-point query at -2^53-1")
	s = queryOne(t, store, queryLabels("neg", tsNegTwoPow53, tsNegTwoPow53, labels))
	assertSeriesStats(t, s, "neg", labels, 1, 2, "single-point query at -2^53")
	s = queryOne(t, store, queryLabels("neg", tsNegPairFar, tsNegTwoPow53, labels))
	assertSeriesStats(t, s, "neg", labels, 2, 5, "range covering both negative adjacent positions")
}

// TestQueryInt64ExtremeTimestampsAddressable 覆盖合法时间戳的两个端点与零：
// MinInt64(-9223372036854775808)、0、MaxInt64(9223372036854775807)
// 都能精确写入与寻址。
func TestQueryInt64ExtremeTimestampsAddressable(t *testing.T) {
	store := NewMetricStore()
	// 故意不按时间顺序写入三个端点，值分别取可区分的 2、5、8（均值也为 5）。
	mustOK(t, store, "["+
		sample("m", 0, 5, nil)+","+
		sample("m", tsMaxInt64, 8, nil)+","+
		sample("m", tsMinInt64, 2, nil)+"]")

	// 端点相等时只统计该端点。
	s := queryOne(t, store, queryAll("m", tsMinInt64, tsMinInt64))
	assertSeriesStats(t, s, "m", map[string]string{}, 1, 2, "MinInt64 point query")
	s = queryOne(t, store, queryAll("m", 0, 0))
	assertSeriesStats(t, s, "m", map[string]string{}, 1, 5, "zero point query")
	s = queryOne(t, store, queryAll("m", tsMaxInt64, tsMaxInt64))
	assertSeriesStats(t, s, "m", map[string]string{}, 1, 8, "MaxInt64 point query")

	// 最小值到最大值的全范围包含全部三个点，平均 (2+5+8)/3 = 5。
	s = queryOne(t, store, queryAll("m", tsMinInt64, tsMaxInt64))
	assertSeriesStats(t, s, "m", map[string]string{}, 3, 5, "full int64 range")

	// 跨过零的较窄区间 [-1,1] 只包含真正落在范围内的 0 点。
	s = queryOne(t, store, queryAll("m", -1, 1))
	assertSeriesStats(t, s, "m", map[string]string{}, 1, 5, "narrow range crossing zero")

	// 极小邻域与极大邻域：不生成 count=0 的序列（注意 MaxInt64 附近 float64
	// 无法表示 ts-1，但这里用 int64 常量构造，不经过 float64）。
	if qr := mustQuery(t, store, queryAll("m", tsMinInt64+1, -1)); len(qr.Series) != 0 {
		t.Fatalf("negative gap range = %+v, want empty series", qr.Series)
	}
	if qr := mustQuery(t, store, queryAll("m", 1, tsMaxInt64-1)); len(qr.Series) != 0 {
		t.Fatalf("positive gap range = %+v, want empty series", qr.Series)
	}
}

// TestQueryEmptyRangeStaysSuccessWithEmptySeries 没有采样的区间仍返回成功和
// 空 series 数组：不生成 count 为零的序列，更不能把无数据解释为平均值零。
func TestQueryEmptyRangeStaysSuccessWithEmptySeries(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("cpu", tsTwoPow53, 2, map[string]string{"host": "a"})+","+
		sample("cpu", tsTwoPow53Plus, 8, map[string]string{"host": "b"})+"]")

	// 2^53+2..2^53+10 没有任何点。
	qr := mustQuery(t, store, queryAll("cpu", tsTwoPow53+2, tsTwoPow53+10))
	if qr.Status != "ok" || qr.Op != "query" {
		t.Fatalf("status/op = %q/%q, want ok/query", qr.Status, qr.Op)
	}
	if len(qr.Series) != 0 {
		t.Fatalf("empty range series = %+v, want [] (no zero-count series)", qr.Series)
	}
	// JSON 必须是空数组而不是 null，也没有任何 count=0/avg=0 的条目。
	if raw := marshalCompact(t, qr); raw != `{"status":"ok","op":"query","series":[]}` {
		t.Fatalf("empty range JSON = %s, want empty-array success", raw)
	}

	// 指标名存在但标签没有任何序列匹配：同样空数组。
	qr = mustQuery(t, store, queryLabels("cpu", tsMinInt64, tsMaxInt64, map[string]string{"host": "zzz"}))
	if len(qr.Series) != 0 {
		t.Fatalf("unmatched label series = %+v, want []", qr.Series)
	}
	// 指标名完全不存在：空数组。
	qr = mustQuery(t, store, queryAll("nope", tsMinInt64, tsMaxInt64))
	if len(qr.Series) != 0 {
		t.Fatalf("unknown metric series = %+v, want []", qr.Series)
	}
}

// TestQueryPrecisionSelectionPerSeriesIdentity 查询选择只作用于真正命中的
// 序列：同一指标名下不同完整标签集合的两条序列在相同的两个紧邻时间戳上
// 各存各的值，查询必须返回被统计序列的指标名与完整标签，统计值不得串换。
func TestQueryPrecisionSelectionPerSeriesIdentity(t *testing.T) {
	store := NewMetricStore()
	hostA := map[string]string{"host": "a", "zone": "x"}
	hostB := map[string]string{"host": "b", "zone": "x"}
	mustOK(t, store, "["+
		// host=a：2^53=2、2^53+1=8，平均 5。
		sample("cpu", tsTwoPow53Plus, 8, hostA)+","+
		sample("cpu", tsTwoPow53, 2, hostA)+","+
		// host=b：同样两个时间戳但值不同（20/40，平均 30），证明不是按时间戳合并。
		sample("cpu", tsTwoPow53, 20, hostB)+","+
		sample("cpu", tsTwoPow53Plus, 40, hostB)+"]")

	// 子集 host=a 只命中 host=a 一条，完整标签（含额外的 zone=x）原样返回。
	s := queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53Plus, map[string]string{"host": "a"}))
	assertSeriesStats(t, s, "cpu", hostA, 2, 5, "host=a adjacent range")

	// host=b 是另一条序列：同样两个时间戳统计各自的值。
	s = queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53Plus, map[string]string{"host": "b"}))
	assertSeriesStats(t, s, "cpu", hostB, 2, 30, "host=b adjacent range")

	// 省略标签命中全部序列：两条都列出，count/平均与完整标签逐条对应不串换。
	qr := mustQuery(t, store, queryAll("cpu", tsTwoPow53, tsTwoPow53Plus))
	if len(qr.Series) != 2 {
		t.Fatalf("all-series query = %+v, want 2 series", qr.Series)
	}
	assertSeriesStats(t, qr.Series[0], "cpu", hostA, 2, 5, "all-series entry 0")
	assertSeriesStats(t, qr.Series[1], "cpu", hostB, 2, 30, "all-series entry 1")

	// 只在 2^53 这一个端点：两条序列各自 count=1，值分别为 2 和 20。
	qr = mustQuery(t, store, queryAll("cpu", tsTwoPow53, tsTwoPow53))
	assertSeriesStats(t, qr.Series[0], "cpu", hostA, 1, 2, "single endpoint host=a")
	assertSeriesStats(t, qr.Series[1], "cpu", hostB, 1, 20, "single endpoint host=b")
}

// TestQueryBoundsOutsideInt64Rejected start 或 end 超出 int64 范围时返回能
// 指出该字段越界的 error；一次只报一个原因，失败无 index、无 conflict、
// 无统计结果，也不改变已写入的数据。
func TestQueryBoundsOutsideInt64Rejected(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "a"}
	mustOK(t, store, "["+
		sample("cpu", tsTwoPow53, 2, labels)+","+
		sample("cpu", tsTwoPow53Plus, 8, labels)+"]")

	assertBaseline := func() {
		t.Helper()
		s := queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53Plus, labels))
		assertSeriesStats(t, s, "cpu", labels, 2, 5, "baseline after failed queries")
	}
	assertBaseline()

	cases := []struct {
		name string
		line string
		want string
	}{
		{"start one above MaxInt64",
			`{"op":"query","name":"cpu","start":9223372036854775808,"end":9223372036854775807}`,
			`field "start"`},
		{"end one above MaxInt64",
			`{"op":"query","name":"cpu","start":0,"end":9223372036854775808}`,
			`field "end"`},
		{"start one below MinInt64",
			`{"op":"query","name":"cpu","start":-9223372036854775809,"end":0}`,
			`field "start"`},
		{"end one below MinInt64",
			`{"op":"query","name":"cpu","start":-9223372036854775808,"end":-9223372036854775809}`,
			`field "end"`},
		{"start far above MaxInt64",
			`{"op":"query","name":"cpu","start":99999999999999999999999999,"end":9223372036854775807}`,
			`field "start"`},
		{"end far below MinInt64",
			`{"op":"query","name":"cpu","start":0,"end":-99999999999999999999999999}`,
			`field "end"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lerr := mustQueryFail(t, store, tc.line)
			assertQueryError(t, lerr, "must be within int64 range")
			assertQueryError(t, lerr, tc.want)
			if lerr.Line != 0 {
				t.Fatalf("package-level failure must not preset line, got line=%d", lerr.Line)
			}
		})
	}

	// 所有失败查询之后，此前写入的数据原样可见。
	assertBaseline()
}

// TestQueryInvertedRangeReportsActualBounds 两者都合法但 start 大于 end 时，
// 报告区间倒置并在原因中保留两个实际边界值（包括 int64 端点处的倒置）；
// 失败无 index、无 conflict、无统计结果，不改变存储。
func TestQueryInvertedRangeReportsActualBounds(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "a"}
	mustOK(t, store, "["+
		sample("cpu", tsTwoPow53, 2, labels)+","+
		sample("cpu", tsTwoPow53Plus, 8, labels)+"]")

	cases := []struct {
		name      string
		start     int64
		end       int64
		wantInMsg string
	}{
		{"ordinary inversion", 5000, 1000, "5000 > 1000"},
		{"adjacent positions inverted", tsTwoPow53Plus, tsTwoPow53,
			fmt.Sprintf("%d > %d", tsTwoPow53Plus, tsTwoPow53)},
		{"extremes inverted", tsMaxInt64, tsMinInt64,
			fmt.Sprintf("%d > %d", tsMaxInt64, tsMinInt64)},
		{"zero over negative", 0, tsNegPairFar, fmt.Sprintf("0 > %d", tsNegPairFar)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := queryLabels("cpu", tc.start, tc.end, labels)
			lerr := mustQueryFail(t, store, line)
			assertQueryError(t, lerr, "invalid range")
			assertQueryError(t, lerr, `"start"`)
			assertQueryError(t, lerr, `"end"`)
			assertQueryError(t, lerr, tc.wantInMsg)
		})
	}

	// 失败之后合法查询仍得到原来的统计。
	s := queryOne(t, store, queryLabels("cpu", tsTwoPow53, tsTwoPow53Plus, labels))
	assertSeriesStats(t, s, "cpu", labels, 2, 5, "stats unchanged after inverted-range failures")
}

// TestQueryFailuresDoNotMutateAndNoStats 失败查询不附带任何统计结果：
// 返回的是 *LineError 而非成功结果；随后的写入快照与合法查询都保持原状。
func TestQueryFailuresDoNotMutateAndNoStats(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", tsMinInt64, 2, nil)+","+
		sample("m", 0, 5, nil)+","+
		sample("m", tsMaxInt64, 8, nil)+"]")

	failures := []string{
		`{"op":"query","name":"m","start":9223372036854775808,"end":9223372036854775807}`,
		`{"op":"query","name":"m","start":-9223372036854775809,"end":0}`,
		`{"op":"query","name":"m","start":9223372036854775807,"end":-9223372036854775808}`,
		`{"op":"query","name":"m","start":0.5,"end":1}`,
	}
	for i, line := range failures {
		lerr := mustQueryFail(t, store, line)
		if lerr.Index != 0 || lerr.Conflict != nil {
			t.Fatalf("failure %d must carry neither index nor conflict, got %+v", i, lerr)
		}
	}

	// 存储未被失败查询改变：仍是三个点；全范围统计 count=3、平均 5。
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 3 {
		t.Fatalf("storage mutated by failed queries: %+v", snap.Series)
	}
	s := queryOne(t, store, queryAll("m", tsMinInt64, tsMaxInt64))
	assertSeriesStats(t, s, "m", map[string]string{}, 3, 5, "full range after failures")
}

// marshalCompact 把查询结果编码为紧凑 JSON 文本，用于锁定空结果形状。
func marshalCompact(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
