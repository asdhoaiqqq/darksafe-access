package darksafe

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 本文件回归保障只读操作 query_windows：沿用 query 的查询条件（name 精确、
// labels 子集、[start,end] 闭区间）与序列整理/排列规则，但把区间按 step
// 毫秒从 start 起连续划分成含首尾毫秒的固定窗口，逐序列、逐窗口输出实际有点
// 窗口的 start、end、count、average。核心约定：
//
//   - 窗口为 [start+k*step, start+(k+1)*step-1]，最后一个截到 end；
//     区间 [1000,3000]、step 1000 时窗口为 [1000,1999]、[2000,2999]、[3000,3000]；
//   - 各序列共用同一组边界；每个点只归入一个窗口；start == end 时只有一个窗口；
//   - 只输出实际有点的窗口，按窗口起点升序，空窗口不补零；整个区间没有点的
//     序列不列出；无匹配时 series 为非 nil 空数组；
//   - 每条 series 保留完整指标名与完整标签集合，序列排列次序与另两种查询一致；
//   - count 统计窗口内已成功保存的点；average 沿用 query 的精确有理数平均与
//     居中取偶舍入，依据实际存储的 float64 值；
//   - step 必须是大于零且在 int64 范围内的 JSON 整数，且只用于 query_windows；
//   - 即使 start/end 接近 int64 上下界、step 接近 int64 上界，窗口也不溢出、
//     不倒置、不遗漏采样；
//   - 查询只读；失败是查询错误（无 index、无 conflict、无 series），保留行号，
//     后续输入照常处理；query 与 query_points 的行为保持兼容。

// mustQueryWindows 要求 query_windows 查询成功并返回窗口结果。
func mustQueryWindows(t *testing.T, store *MetricStore, line string) *QueryWindowsResult {
	t.Helper()
	res, err := store.QueryWindowsLine(line)
	if err != nil {
		t.Fatalf("expected query_windows success for %s, got error: %+v", line, err)
	}
	return res
}

// mustQueryWindowsFail 要求 query_windows 查询失败并返回结构化错误。
func mustQueryWindowsFail(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	res, err := store.QueryWindowsLine(line)
	if err == nil {
		t.Fatalf("expected query_windows failure for %s, got result: %+v", line, res)
	}
	return err
}

// queryWindowsAll 构造省略 labels 的 query_windows 对象文本。
func queryWindowsAll(name string, start, end, step int64) string {
	return fmt.Sprintf(`{"op":"query_windows","name":%s,"start":%d,"end":%d,"step":%d}`,
		jsonString(name), start, end, step)
}

// queryWindowsLabels 构造按标签子集查询的 query_windows 对象文本。
func queryWindowsLabels(name string, start, end, step int64, labels map[string]string) string {
	return `{"op":"query_windows","name":` + jsonString(name) +
		fmt.Sprintf(`,"start":%d,"end":%d,"step":%d,"labels":`, start, end, step) +
		jsonLabels(labels) + `}`
}

func window(start, end int64, count int, avg float64) Window {
	return Window{Start: start, End: end, Count: count, Average: avg}
}

// assertWindows 断言一条序列的窗口列表与预期完全一致（含顺序）。
func assertWindows(t *testing.T, got []Window, want []Window, note string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: windows = %+v, want %+v", note, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: window[%d] = %+v, want %+v (full got %+v)", note, i, got[i], want[i], got)
		}
	}
}

// TestQueryWindowsSpecExample 任务书示例：区间 [1000,3000]、step 1000 时窗口为
// [1000,1999]、[2000,2999]、[3000,3000]，空窗口不补零。
func TestQueryWindowsSpecExample(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("cpu", 1000, 2, nil)+","+
		sample("cpu", 1500, 4, nil)+","+
		sample("cpu", 2000, 6, nil)+","+
		sample("cpu", 3000, 8, nil)+"]")

	res := mustQueryWindows(t, store, queryWindowsAll("cpu", 1000, 3000, 1000))
	if res.Status != "ok" || res.Op != "query_windows" {
		t.Fatalf("status/op = %q/%q, want ok/query_windows", res.Status, res.Op)
	}
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", res.Series)
	}
	s0 := res.Series[0]
	if s0.Name != "cpu" {
		t.Fatalf("series name = %q, want cpu", s0.Name)
	}
	assertWindows(t, s0.Windows, []Window{
		window(1000, 1999, 2, 3),
		window(2000, 2999, 1, 6),
		window(3000, 3000, 1, 8),
	}, "spec windows [1000,3000] step 1000")
}

// TestQueryWindowsResultShape 锁定成功结果的 JSON 形状：op 为 query_windows，
// 序列条目只有 name、labels、windows；窗口只有 start、end、count、average；
// 无命中时 series 是空数组而不是 null。
func TestQueryWindowsResultShape(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	res := mustQueryWindows(t, store, queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "a"}))
	raw := marshalCompact(t, res)
	want := `{"status":"ok","op":"query_windows","series":[` +
		`{"name":"cpu","labels":{"host":"a"},"windows":[` +
		`{"start":1000,"end":1999,"count":1,"average":2}]}]}`
	if raw != want {
		t.Fatalf("query_windows JSON = %s, want %s", raw, want)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	entry := decoded["series"].([]any)[0].(map[string]any)
	for _, banned := range []string{"count", "average", "points"} {
		if _, ok := entry[banned]; ok {
			t.Fatalf("query_windows series entry must not carry %q: %v", banned, entry)
		}
	}
	w0 := entry["windows"].([]any)[0].(map[string]any)
	if _, ok := w0["timestamp"]; ok {
		t.Fatalf("window must not carry timestamp: %v", w0)
	}

	// 无命中：成功的空 series 数组（非 null）。
	res = mustQueryWindows(t, store, queryWindowsAll("cpu", 2000, 3000, 1000))
	if res.Series == nil || len(res.Series) != 0 {
		t.Fatalf("miss query_windows = %+v, want non-nil empty series list", res.Series)
	}
	if raw = marshalCompact(t, res); raw != `{"status":"ok","op":"query_windows","series":[]}` {
		t.Fatalf("empty query_windows JSON = %s, want empty series array", raw)
	}
}

// TestQueryWindowsEmptyWindowsOmitted 空窗口不补零：只有实际有点的窗口列出，
// 窗口按起点升序；整条序列区间内没有点时不列出；无匹配序列时 series 为空。
func TestQueryWindowsEmptyWindowsOmitted(t *testing.T) {
	store := NewMetricStore()
	// host=a：窗口 0、2 有点，窗口 1 为空；host=b：整个区间没有点。
	mustOK(t, store, `[
		{"name":"cpu","timestamp":0,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":20,"value":6,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":21,"value":10,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":99,"value":1,"labels":{"host":"b"}}
	]`)

	// labels 子集 host=a：只列窗口 0 与窗口 2，空窗口 1 不补零。
	res := mustQueryWindows(t, store, queryWindowsLabels("cpu", 0, 29, 10, map[string]string{"host": "a"}))
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly host=a", res.Series)
	}
	assertWindows(t, res.Series[0].Windows, []Window{
		window(0, 9, 2, 3),
		window(20, 29, 2, 8),
	}, "empty middle window omitted")

	// 省略 labels 命中全部序列：host=b 在 [0,29] 内没有点，整个序列不列出。
	res = mustQueryWindows(t, store, queryWindowsAll("cpu", 0, 29, 10))
	if len(res.Series) != 1 || res.Series[0].Labels["host"] != "a" {
		t.Fatalf("series = %+v, want only host=a (host=b has no in-range points)", res.Series)
	}

	// 指标不存在 / 标签未命中 / 区间内无点：都是成功的空 series。
	for name, line := range map[string]string{
		"unknown metric":   queryWindowsAll("nope", 0, 29, 10),
		"label miss":       queryWindowsLabels("cpu", 0, 29, 10, map[string]string{"host": "zzz"}),
		"no points in itv": queryWindowsAll("cpu", 30, 40, 10),
	} {
		t.Run(name, func(t *testing.T) {
			r := mustQueryWindows(t, store, line)
			if r.Series == nil || len(r.Series) != 0 {
				t.Fatalf("%s: series = %+v, want empty", name, r.Series)
			}
		})
	}
}

// TestQueryWindowsSingleTimestamp start == end 时区间只有唯一窗口；落在该
// 时间戳上的点全部归入它，窗口终点等于 start。
func TestQueryWindowsSingleTimestamp(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", 5000, 2, nil)+","+
		sample("m", 5000, 2.0, nil)+"]") // 第二个是同值重复，不新增点

	res := mustQueryWindows(t, store, queryWindowsAll("m", 5000, 5000, 1))
	assertWindows(t, res.Series[0].Windows, []Window{window(5000, 5000, 1, 2)},
		"start==end, step=1")

	// step 远大于单点区间时唯一窗口仍是 [start,start]。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 5000, 5000, tsMaxInt64))
	assertWindows(t, res.Series[0].Windows, []Window{window(5000, 5000, 1, 2)},
		"start==end, huge step")

	// 单点区间无点：空 series。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 5001, 5001, 1))
	if len(res.Series) != 0 {
		t.Fatalf("series = %+v, want empty", res.Series)
	}
}

// TestQueryWindowsBoundariesAndAssignment 校验首尾毫秒归属：每个点只归入一个
// 窗口，窗口边界点（start+k*step - 1 与 start+k*step）分别落在相邻窗口。
func TestQueryWindowsBoundariesAndAssignment(t *testing.T) {
	store := NewMetricStore()
	// [10,39]、step=10：窗口 [10,19]、[20,29]、[30,39]，每个关键边界一个点。
	mustOK(t, store, `[
		{"name":"m","timestamp":10,"value":1},
		{"name":"m","timestamp":19,"value":3},
		{"name":"m","timestamp":20,"value":5},
		{"name":"m","timestamp":29,"value":7},
		{"name":"m","timestamp":30,"value":9},
		{"name":"m","timestamp":39,"value":11}
	]`)
	res := mustQueryWindows(t, store, queryWindowsAll("m", 10, 39, 10))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(10, 19, 2, 2),
		window(20, 29, 2, 6),
		window(30, 39, 2, 10),
	}, "each boundary ms belongs to exactly one window")

	// 区间不是 step 的整数倍：最后一个窗口截到 end，而不是 end 对齐的完整窗口。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 10, 35, 10))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(10, 19, 2, 2),
		window(20, 29, 2, 6),
		window(30, 35, 1, 9), // 39 上的点不在区间；窗口截到 35
	}, "last window clipped to end")

	// 区间比 step 宽但不足两个窗口：第一个完整窗口 + 长度为 1 的收尾窗口。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 10, 20, 10))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(10, 19, 2, 2),
		window(20, 20, 1, 5),
	}, "width == step yields two windows touching at the boundary")

	// step 大于区间宽度：唯一窗口就是整个区间（截到 end）。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 10, 39, 100))
	assertWindows(t, res.Series[0].Windows, []Window{window(10, 39, 6, 6)},
		"step wider than the range")
}

// TestQueryWindowsLabelsIdentityAndOrder 名称与标签筛选沿用现有查询：子集匹配、
// 完整标签集合保留、序列排列次序与 query/query_points 一致（先名字后排序标签）。
func TestQueryWindowsLabelsIdentityAndOrder(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":20,"value":2,"labels":{"zone":"x","host":"h"}},
		{"name":"m","timestamp":10,"value":1},
		{"name":"m","timestamp":30,"value":3,"labels":{"zone":"x"}},
		{"name":"other","timestamp":10,"value":9}
	]`)

	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 100, 25))
	if len(res.Series) != 3 {
		t.Fatalf("series = %+v, want 3", res.Series)
	}
	// 与 organizeSeries 的次序一致：无标签在前，其次 host=h,zone=x，最后 zone=x。
	wantIdentity := []struct {
		labels map[string]string
		win    []Window
	}{
		{map[string]string{}, []Window{window(0, 24, 1, 1)}},
		{map[string]string{"host": "h", "zone": "x"}, []Window{window(0, 24, 1, 2)}},
		{map[string]string{"zone": "x"}, []Window{window(25, 49, 1, 3)}},
	}
	for i, wi := range wantIdentity {
		s := res.Series[i]
		if s.Name != "m" || !sameLabels(s.Labels, wi.labels) {
			t.Fatalf("series[%d] = %+v, want identity m%v", i, s, wi.labels)
		}
		assertWindows(t, s.Windows, wi.win, fmt.Sprintf("series[%d]", i))
	}
	// 标签副本独立：修改结果不影响存储，再次查询一致。
	res.Series[0].Labels["injected"] = "y"
	res2 := mustQueryWindows(t, store, queryWindowsAll("m", 0, 100, 25))
	if _, ok := res2.Series[0].Labels["injected"]; ok {
		t.Fatalf("result labels must be an independent copy, got %v", res2.Series[0].Labels)
	}

	// 子集匹配 zone=x：两条序列，返回完整标签集合而不缩减成查询条件。
	res = mustQueryWindows(t, store, `{"op":"query_windows","name":"m","start":0,"end":100,"step":25,"labels":{"zone":"x"}}`)
	if len(res.Series) != 2 {
		t.Fatalf("subset match = %+v, want 2 series", res.Series)
	}
	if !sameLabels(res.Series[0].Labels, map[string]string{"host": "h", "zone": "x"}) ||
		!sameLabels(res.Series[1].Labels, map[string]string{"zone": "x"}) {
		t.Fatalf("full label sets must be preserved: %+v", res.Series)
	}
}

// TestQueryWindowsCountAndAverageRules count 只统计已成功保存的点（等值重复
// 不增加），平均值依据实际存储的 float64、按 query 的精确平均与居中取偶规则。
func TestQueryWindowsCountAndAverageRules(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":0,"value":1},
		{"name":"m","timestamp":1,"value":1.0000000000000002}
	]`)
	// 同值重复：count 不增加。
	mustOK(t, store, `[{"name":"m","timestamp":0,"value":1.0}]`)

	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	// (1 + 1.0000000000000002)/2 恰居两者正中，取偶为 1；count=2（重复点不计）。
	assertWindows(t, res.Series[0].Windows,
		[]Window{window(0, 9, 2, 1)}, "nearest-even average over stored float64")

	// 每个窗口独立统计，窗口间不共享计数。
	mustOK(t, store, `[{"name":"m","timestamp":10,"value":10}]`)
	res = mustQueryWindows(t, store, queryWindowsAll("m", 0, 19, 10))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(0, 9, 2, 1),
		window(10, 19, 1, 10),
	}, "per-window independent counts")
}

// TestQueryWindowsLargeNumberCancellation 同一窗口内正负大数抵消后小余量保留，
// 且窗口内总和超出 float64 范围时平均仍有限——与 query 相同的精确有理数求和。
func TestQueryWindowsLargeNumberCancellation(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"x","timestamp":0,"value":1e16},
		{"name":"x","timestamp":1,"value":1},
		{"name":"x","timestamp":2,"value":-1e16},
		{"name":"x","timestamp":10,"value":1e308},
		{"name":"x","timestamp":11,"value":1e308}
	]`)
	res := mustQueryWindows(t, store, queryWindowsAll("x", 0, 19, 10))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(0, 9, 3, 1.0/3.0), // big-rat 1/3 → nearest float64
		window(10, 19, 2, 1e308), // sum 2e308 overflows float64, mean stays finite
	}, "exact per-window arithmetic")
	if math.IsInf(res.Series[0].Windows[1].Average, 0) || math.IsNaN(res.Series[0].Windows[1].Average) {
		t.Fatalf("average must stay finite, got %v", res.Series[0].Windows[1].Average)
	}

	// 与同数据的整段 query 结果对照：query 同样精确求和，均值有限。
	qr := mustQuery(t, store, `{"op":"query","name":"x","start":0,"end":19}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 5 {
		t.Fatalf("query over same range = %+v, want count 5", qr.Series)
	}
	if math.IsInf(qr.Series[0].Average, 0) {
		t.Fatalf("query average must be finite, got inf")
	}
}

// TestQueryWindowsInt64Boundaries 起止时间与 step 接近 int64 上下界时窗口不
// 溢出、不倒置、不遗漏采样：覆盖跨整个 int64 区间、step 为 MaxInt64、
// start 为 MinInt64 等组合。
func TestQueryWindowsInt64Boundaries(t *testing.T) {
	min := tsMinInt64
	max := tsMaxInt64

	// 四个采样：MinInt64、-2、-1、MaxInt64；区间为整个 int64，step=MaxInt64。
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", min, 3, nil)+","+
		sample("m", -2, 9, nil)+","+
		sample("m", -1, 1, nil)+","+
		sample("m", max, 5, nil)+"]")
	res := mustQueryWindows(t, store, queryWindowsAll("m", min, max, max))
	// 窗口 0：[MinInt64, MinInt64+MaxInt64-1] = [MinInt64, -2]
	// 窗口 1：[-1, -1+MaxInt64-1] = [-1, MaxInt64-2]
	// 窗口 2：[MaxInt64-1, MaxInt64]（截到 end）
	assertWindows(t, res.Series[0].Windows, []Window{
		window(min, -2, 2, 6),
		window(-1, max-2, 1, 1),
		window(max-1, max, 1, 5),
	}, "full int64 range, step MaxInt64, no overflow/inversion")

	// step=1：每个时间戳独立窗口，首尾窗口的边界都精确。
	res = mustQueryWindows(t, store, queryWindowsAll("m", min, max, 1))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(min, min, 1, 3),
		window(-2, -2, 1, 9),
		window(-1, -1, 1, 1),
		window(max, max, 1, 5),
	}, "step 1 across full int64 range")

	// 负起点附近的小步划分：[-5,4]、step=3 → [-5,-3]、[-2,0]、[1,3]、[4,4]。
	store2 := NewMetricStore()
	for ts, v := range map[int64]float64{-5: 1, -3: 3, -2: 5, 0: 9, 1: 7, 3: 1, 4: 2} {
		mustOK(t, store2, "["+sample("m", ts, v, nil)+"]")
	}
	res = mustQueryWindows(t, store2, queryWindowsAll("m", -5, 4, 3))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(-5, -3, 2, 2),
		window(-2, 0, 2, 7),
		window(1, 3, 2, 4),
		window(4, 4, 1, 2),
	}, "negative start tiled without overflow")

	// start 为 MinInt64、end 为 -1（全负半区），step=MaxInt64：第一个窗口的
	// 自然终点 -2 未越过 end，保持完整宽度；第二个窗口从 -1 开始，截到 end=-1。
	res = mustQueryWindows(t, store, queryWindowsAll("m", min, -1, max))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(min, -2, 2, 6),
		window(-1, -1, 1, 1),
	}, "negative half, huge step clips second window to end")
}

// TestQueryWindowsStepValidation step 必须是大于零、int64 范围内的 JSON 整数；
// 缺失、类型不符、值不合法都返回指出 step 的查询错误。
func TestQueryWindowsStepValidation(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2}]`)

	cases := []struct {
		name string
		line string
		want string
	}{
		{"missing step", `{"op":"query_windows","name":"cpu","start":0,"end":1}`, `missing required field "step"`},
		{"step zero", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":0}`, `field "step"`},
		{"step negative", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":-1}`, `field "step"`},
		{"step string", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":"1"}`, `field "step" must be a JSON number`},
		{"step boolean", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":true}`, `field "step" must be a JSON number`},
		{"step null", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":null}`, `field "step" must be a JSON number`},
		{"step float", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":1.5}`, `field "step"`},
		{"step exponent fraction", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":1e0}`, `field "step"`},
		{"step over int64", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":9223372036854775808}`, `field "step"`},
		{"step under int64", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":-9223372036854775809}`, `field "step"`},
		{"step object", `{"op":"query_windows","name":"cpu","start":0,"end":1,"step":{}}`, `field "step" must be a JSON number`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lerr := mustQueryWindowsFail(t, store, tc.line)
			assertQueryError(t, lerr, tc.want)
			// 失败结果没有 series、index 或 conflict。
			m := marshalLineError(t, lerr)
			for _, banned := range []string{"series", "index", "conflict"} {
				if _, ok := m[banned]; ok {
					t.Fatalf("query_windows failure must not carry %q: %v", banned, m)
				}
			}
		})
	}

	// 合法 step 恰好为 int64 上界：成功。
	res := mustQueryWindows(t, store, queryWindowsAll("cpu", 0, tsMaxInt64, tsMaxInt64))
	if res.Op != "query_windows" {
		t.Fatalf("step MaxInt64 must be accepted, got %+v", res)
	}
}

// TestQueryWindowsStepOnlyForNewOp step 只用于 query_windows：query 与
// query_points 携带 step 一律按未知字段拒绝；query_windows 仍可省略 labels。
func TestQueryWindowsStepOnlyForNewOp(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2}]`)

	// step 写在 op 之后：读到 step 立即按未知字段拒绝。
	lerr := mustQueryFail(t, store, `{"op":"query","name":"cpu","start":0,"end":2000,"step":1000}`)
	assertQueryError(t, lerr, `unknown field "step"`)
	lerr = mustQueryPointsFail(t, store, `{"op":"query_points","name":"cpu","start":0,"end":2000,"step":1000}`)
	assertQueryError(t, lerr, `unknown field "step"`)

	// step 写在 op 之前：读完对象再判未知字段，且该判定先于缺字段/区间检查。
	lerr = mustQueryFail(t, store, `{"step":1000,"op":"query","name":"cpu","start":0,"end":2000}`)
	assertQueryError(t, lerr, `unknown field "step"`)

	// step 写在 op 之前时，值是否合法不影响未知字段判定：零、字符串、对象
	// 都按未知字段拒绝，绝不退化成窗口宽度校验。
	for _, line := range []string{
		`{"step":0,"op":"query","name":"cpu","start":0,"end":2000}`,
		`{"step":"1000","op":"query","name":"cpu","start":0,"end":2000}`,
		`{"step":{},"op":"query","name":"cpu","start":0,"end":2000}`,
		`{"step":0,"op":"query_points","name":"cpu","start":0,"end":2000}`,
		`{"step":"1000","op":"query_points","name":"cpu","start":0,"end":2000}`,
		`{"step":{},"op":"query_points","name":"cpu","start":0,"end":2000}`,
	} {
		lerr = mustQueryFail(t, store, line)
		assertQueryError(t, lerr, `unknown field "step"`)
	}

	// 错误仍按字段书写次序选择：step 之前的字段错误先报；把错误字段移到
	// step 之后则先报 step，即使 op 写在它们之后也一样。
	lerr = mustQueryFail(t, store, `{"name":7,"step":1,"op":"query","start":0,"end":1}`)
	assertQueryError(t, lerr, `field "name" must be a string`)
	lerr = mustQueryFail(t, store, `{"step":1,"name":7,"op":"query","start":0,"end":1}`)
	assertQueryError(t, lerr, `unknown field "step"`)

	// step 的未知字段错误先于缺少必填项与区间倒置。
	lerr = mustQueryFail(t, store, `{"step":1,"op":"query"}`)
	assertQueryError(t, lerr, `unknown field "step"`)
	lerr = mustQueryFail(t, store, `{"step":1,"op":"query","name":"cpu","start":5,"end":1}`)
	assertQueryError(t, lerr, `unknown field "step"`)

	// 删掉 step 后原查询正常执行。
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("query compatibility broken: %+v", qr.Series)
	}
	qp := mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":0,"end":2000}`)
	if len(qp.Series) != 1 || len(qp.Series[0].Points) != 1 {
		t.Fatalf("query_points compatibility broken: %+v", qp.Series)
	}

	// op 缺失、类型错误、未知操作或重复出现时沿用既有处理：step 在出现处
	// 按窗口宽度校验，op 自身的问题按书写次序暴露。
	lerr = mustQueryFail(t, store, `{"step":0,"name":"cpu","start":0,"end":1}`)
	assertQueryError(t, lerr, `field "step"`)
	lerr = mustQueryFail(t, store, `{"step":0,"op":5,"name":"cpu","start":0,"end":1}`)
	assertQueryError(t, lerr, `field "step"`)
	lerr = mustQueryFail(t, store, `{"step":0,"op":"ping","name":"cpu","start":0,"end":1}`)
	assertQueryError(t, lerr, `field "step"`)
	lerr = mustQueryFail(t, store, `{"step":5,"op":"query","op":"query","name":"cpu","start":0,"end":1}`)
	assertQueryError(t, lerr, `duplicate field "op"`)
}

// TestQueryWindowsErrorSelection 失败原因的选择沿用查询对象的既有次序：
// 整行解析失败最先；已出现字段按书写顺序；缺字段在已出现字段合法之后；
// query_windows 特有“缺 step”排在四项必填之后、倒置区间之前。
func TestQueryWindowsErrorSelection(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2}]`)

	// 倒置区间 + 缺 step：四项必填齐全后先报缺 step（区间检查在最后）。
	lerr := mustQueryWindowsFail(t, store, `{"op":"query_windows","name":"cpu","start":5,"end":1}`)
	assertQueryError(t, lerr, `missing required field "step"`)

	// 倒置区间 + step 合法：才报区间倒置并给出实际边界。
	lerr = mustQueryWindowsFail(t, store, `{"op":"query_windows","name":"cpu","start":5,"end":1,"step":2}`)
	assertQueryError(t, lerr, `invalid range: "start" must not be greater than "end" (5 > 1)`)

	// 倒置区间 + 未知字段：先报未知字段（书写顺序）。
	lerr = mustQueryWindowsFail(t, store, `{"op":"query_windows","name":"cpu","start":5,"end":1,"step":2,"bogus":1}`)
	assertQueryError(t, lerr, `unknown field "bogus"`)

	// step 非法 + 倒置区间：step 字段问题先于区间检查。
	lerr = mustQueryWindowsFail(t, store, `{"op":"query_windows","name":"cpu","start":5,"end":1,"step":0}`)
	assertQueryError(t, lerr, `field "step"`)

	// 缺多项必填：仍按 op、name、start、end 的顺序；四项齐了才轮到 step。
	for _, tc := range []struct {
		line string
		want string
	}{
		{`{"start":0,"end":1,"step":1}`, `missing required field "op"`},
		{`{"op":"query_windows","start":0,"end":1,"step":1}`, `missing required field "name"`},
		{`{"op":"query_windows","name":"cpu","end":1,"step":1}`, `missing required field "start"`},
		{`{"op":"query_windows","name":"cpu","start":0,"step":1}`, `missing required field "end"`},
	} {
		lerr = mustQueryWindowsFail(t, store, tc.line)
		assertQueryError(t, lerr, tc.want)
	}

	// 未知 op：措辞覆盖三种操作。
	lerr = mustQueryWindowsFail(t, store, `{"op":"ping","name":"cpu","start":0,"end":1,"step":1}`)
	assertQueryError(t, lerr, `unknown op "ping"`)
	assertQueryError(t, lerr, "query_windows")

	// 整行解析失败最先（对象未闭合，即使字段类型已错）。
	lerr = mustQueryWindowsFail(t, store, `{"op":"query_windows","name":7,"step":1`)
	assertQueryError(t, lerr, "invalid JSON")
}

// TestQueryWindowsViaProcessLine 统一入口按 op 分派到 *QueryWindowsResult；
// 三个专用入口交叉使用时各自拒绝不属于自己的 op，且不返回另一类型的结果。
func TestQueryWindowsViaProcessLine(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2}]`)
	line := `{"op":"query_windows","name":"cpu","start":0,"end":2000,"step":1000}`

	r, lerr := store.ProcessLine(line)
	if lerr != nil || r == nil {
		t.Fatalf("ProcessLine query_windows: r=%v lerr=%+v", r, lerr)
	}
	qw, ok := r.(*QueryWindowsResult)
	if !ok {
		t.Fatalf("ProcessLine result type = %T, want *QueryWindowsResult", r)
	}
	if qw.Op != "query_windows" || len(qw.Series) != 1 {
		t.Fatalf("ProcessLine query_windows = %+v", qw)
	}

	// 倒置区间经统一入口失败：真正的 nil 结果。
	r, lerr = store.ProcessLine(`{"op":"query_windows","name":"cpu","start":9,"end":1,"step":2}`)
	if r != nil || lerr == nil || !strings.Contains(lerr.Error, "9 > 1") {
		t.Fatalf("inverted query_windows via ProcessLine: r=%v lerr=%+v", r, lerr)
	}

	// 专用入口交叉使用：各报各的引导信息，不串结果类型。
	if _, lerr = store.QueryLine(line); lerr == nil || !strings.Contains(lerr.Error, "query_windows") {
		t.Fatalf("QueryLine must reject query_windows, got %+v", lerr)
	}
	if _, lerr = store.QueryPointsLine(line); lerr == nil || !strings.Contains(lerr.Error, "query_windows") {
		t.Fatalf("QueryPointsLine must reject query_windows, got %+v", lerr)
	}
	if _, lerr = store.QueryWindowsLine(`{"op":"query","name":"cpu","start":0,"end":1}`); lerr == nil {
		t.Fatalf("QueryWindowsLine must reject op query")
	}
	if _, lerr = store.QueryWindowsLine(`{"op":"query_points","name":"cpu","start":0,"end":1}`); lerr == nil {
		t.Fatalf("QueryWindowsLine must reject op query_points")
	}
}

// TestQueryWindowsReadOnlyAndCommittedOnly query_windows 只读取此前成功提交的
// 数据：失败批次回滚的点不可见、等值重复不计入；查询不改变存储，与写入、
// query、query_points 交替进行互不影响。
func TestQueryWindowsReadOnlyAndCommittedOnly(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":10,"value":2}]`)
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":4}]`)

	baseline := func() {
		t.Helper()
		res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 100, 50))
		want := []Window{window(0, 49, 2, 3)}
		if len(res.Series) != 1 {
			t.Fatalf("baseline series = %+v, want one", res.Series)
		}
		assertWindows(t, res.Series[0].Windows, want, "baseline")
	}
	baseline()

	// 失败批次整批回滚，窗口统计看不到其中任何点。
	mustFail(t, store, `[{"name":"m","timestamp":30,"value":8},{"name":"m","timestamp":10,"value":100}]`)
	baseline()

	// 等值重复不改变 count。
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":4.0}]`)
	baseline()

	// 重复查询结果一致。
	r1 := mustQueryWindows(t, store, queryWindowsAll("m", 0, 100, 50))
	r2 := mustQueryWindows(t, store, queryWindowsAll("m", 0, 100, 50))
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("repeated query_windows differs: %+v vs %+v", r1, r2)
	}

	// 存储未被改变：快照与另两种查询保持原状。
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 2 {
		t.Fatalf("query_windows mutated storage, snapshot = %+v", snap.Series)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("query after query_windows = %+v", qr.Series)
	}

	// 后续写入可见：窗口统计随之增长。
	mustOK(t, store, `[{"name":"m","timestamp":60,"value":9}]`)
	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 100, 50))
	assertWindows(t, res.Series[0].Windows, []Window{
		window(0, 49, 2, 3),
		window(50, 99, 1, 9),
	}, "newly committed point appears in its window")
}

// TestQueryWindowsMultipleSeriesShareBoundaries 各序列共用同一组由 start 与
// step 决定的边界：不同序列的点在相同时间戳上归入相同窗口，输出窗口的
// start/end 一致；序列各自只列自己非空的窗口。
func TestQueryWindowsMultipleSeriesShareBoundaries(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":5,"value":1,"labels":{"h":"a"}},
		{"name":"m","timestamp":25,"value":3,"labels":{"h":"a"}},
		{"name":"m","timestamp":15,"value":7,"labels":{"h":"b"}}
	]`)
	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 39, 20))
	if len(res.Series) != 2 {
		t.Fatalf("series = %+v, want 2", res.Series)
	}
	// h=a 有点的窗口 0 与窗口 1；h=b 只有窗口 0——边界 [0,19]/[20,39] 相同。
	assertWindows(t, res.Series[0].Windows, []Window{
		window(0, 19, 1, 1),
		window(20, 39, 1, 3),
	}, "h=a windows")
	assertWindows(t, res.Series[1].Windows, []Window{
		window(0, 19, 1, 7),
	}, "h=b windows")
}
