package darksafe

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 本文件回归保障只读操作 query_windows：在 query 的查询条件（name 精确、
// labels 子集、[start,end] 闭区间）之上按 step 毫秒把区间从 start 起连续
// 划分为固定宽度闭窗口，逐序列统计每个实际有点窗口的 count 与精确 average。
//
// 已覆盖的核心场景：
//   - 窗口为闭区间且首尾相接：[1000,3000]、step 1000 划成
//     [1000,1999]、[2000,2999]、[3000,3000]，每个点只归入一个窗口；
//   - 各序列共用同一套窗口边界；只输出实际有点的窗口，按窗口起点升序，
//     空窗口不补零，整个区间没有点的序列不列出，无命中时 series 为 []；
//   - start == end 仍可查询，命中点归入唯一窗口；
//   - start/end 接近 int64 上下界时窗口不溢出、不倒置、不漏采样；
//   - count 只计已成功保存的点；average 沿用 query 的精确有理数平均与
//     最近偶数舍入，窗口内大数抵消或总和超出 float64 时仍为有限结果；
//   - 名称、标签筛选与序列排序沿用 query；操作只读；
//   - step 必须是大于零且在 int64 范围内的 JSON 整数：缺失、类型不符、
//     非整数、零、负数、超界都返回指出 step 的查询错误；step 只用于
//     query_windows；倒置区间指出实际起止值；失败结果保留查询错误约定
//     （没有 series、index 或 conflict），失败后存储不变、后续输入照常。

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

// assertWindow 断言一个窗口条目的边界、点数与均值完全一致。
func assertWindow(t *testing.T, got WindowStat, want WindowStat, note string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: window = %+v, want %+v", note, got, want)
	}
}

// TestQueryWindowsSpecExample 是任务书示例：区间 [1000,3000]、step 1000
// 划成三个窗口 [1000,1999]、[2000,2999]、[3000,3000]；只有实际有点的
// 窗口出现，窗口内 count/average 正确，窗口按起点升序。
func TestQueryWindowsSpecExample(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "a"}
	mustOK(t, store, "["+
		sample("cpu", 1000, 2, labels)+","+
		sample("cpu", 1500, 4, labels)+","+
		sample("cpu", 2000, 8, labels)+","+
		sample("cpu", 2999, 0, labels)+","+
		sample("cpu", 3000, 6, labels)+"]")

	res := mustQueryWindows(t, store, queryWindowsLabels("cpu", 1000, 3000, 1000, labels))
	if res.Status != "ok" || res.Op != "query_windows" {
		t.Fatalf("status/op = %q/%q, want ok/query_windows", res.Status, res.Op)
	}
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", res.Series)
	}
	s0 := res.Series[0]
	if s0.Name != "cpu" || !sameLabels(s0.Labels, labels) {
		t.Fatalf("series identity = %+v, want cpu{host=a}", s0)
	}
	want := []WindowStat{
		{Start: 1000, End: 1999, Count: 2, Average: 3}, // (2+4)/2
		{Start: 2000, End: 2999, Count: 2, Average: 4}, // (8+0)/2
		{Start: 3000, End: 3000, Count: 1, Average: 6},
	}
	if len(s0.Windows) != len(want) {
		t.Fatalf("windows = %+v, want %d windows", s0.Windows, len(want))
	}
	for i := range want {
		assertWindow(t, s0.Windows[i], want[i], fmt.Sprintf("window %d", i))
	}
}

// TestQueryWindowsPointFallsInExactlyOneWindow 窗口首尾相接且都是闭区间：
// 边界两侧的毫秒（如 1999 与 2000）分属相邻窗口，绝不同属或遗漏；
// 末窗口截到 end 时 end 上的点仍命中。
func TestQueryWindowsPointFallsInExactlyOneWindow(t *testing.T) {
	store := NewMetricStore()
	// start=-5、end=6、step=4：窗口 [-5,-2]、[-1,2]、[3,6]。
	for i, v := range []float64{10, 20, 30, 40, 50, 60} {
		ts := []int64{-5, -2, -1, 2, 3, 6}[i]
		mustOK(t, store, fmt.Sprintf(`[{"name":"m","timestamp":%d,"value":%v}]`, ts, v))
	}
	res := mustQueryWindows(t, store, queryWindowsAll("m", -5, 6, 4))
	want := []WindowStat{
		{Start: -5, End: -2, Count: 2, Average: 15},
		{Start: -1, End: 2, Count: 2, Average: 35},
		{Start: 3, End: 6, Count: 2, Average: 55},
	}
	got := res.Series[0].Windows
	if len(got) != len(want) {
		t.Fatalf("windows = %+v, want %d", got, len(want))
	}
	for i := range want {
		assertWindow(t, got[i], want[i], fmt.Sprintf("window %d", i))
	}
}

// TestQueryWindowsSingleTimestamp start == end 时只有唯一窗口 [start,start]；
// 该时间戳上的点归入其中，step 大小不影响唯一窗口的边界。
func TestQueryWindowsSingleTimestamp(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1500,"value":2}]`)

	for _, step := range []int64{1, 1000, 9223372036854775807} {
		res := mustQueryWindows(t, store, queryWindowsAll("m", 1500, 1500, step))
		got := res.Series
		if len(got) != 1 || len(got[0].Windows) != 1 {
			t.Fatalf("step %d: %+v, want one single-millisecond window", step, got)
		}
		assertWindow(t, got[0].Windows[0],
			WindowStat{Start: 1500, End: 1500, Count: 1, Average: 2},
			fmt.Sprintf("step %d", step))
	}
}

// TestQueryWindowsEmptyWindowsAndSeriesOmitted 空窗口不补零、没有点的序列
// 不列出；各序列共用边界，只输出各自命中的窗口子集。
func TestQueryWindowsEmptyWindowsAndSeriesOmitted(t *testing.T) {
	store := NewMetricStore()
	// host=a 只在第一窗口有点；host=b 只在第二窗口有点；host=c 区间内无点。
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2500,"value":8,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":9000,"value":1,"labels":{"host":"c"}}
	]`)

	res := mustQueryWindows(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	if len(res.Series) != 2 {
		t.Fatalf("series = %+v, want only the two series with in-range points", res.Series)
	}
	// 序列次序沿用 query/query_points：host=a 在前、host=b 在后。
	if !sameLabels(res.Series[0].Labels, map[string]string{"host": "a"}) ||
		!sameLabels(res.Series[1].Labels, map[string]string{"host": "b"}) {
		t.Fatalf("series order = %+v", res.Series)
	}
	// 每条序列只输出自己命中的窗口，窗口边界是各序列共用的同一套边界。
	assertWindow(t, res.Series[0].Windows[0],
		WindowStat{Start: 1000, End: 1999, Count: 1, Average: 2}, "host=a window")
	assertWindow(t, res.Series[1].Windows[0],
		WindowStat{Start: 2000, End: 2999, Count: 1, Average: 8}, "host=b window")

	// 无命中：成功的空 series 数组而不是 null。
	miss := mustQueryWindows(t, store, queryWindowsAll("cpu", 4000, 8000, 1000))
	if miss.Series == nil || len(miss.Series) != 0 {
		t.Fatalf("miss = %+v, want non-nil empty series", miss.Series)
	}
	if raw := marshalCompact(t, miss); raw != `{"status":"ok","op":"query_windows","series":[]}` {
		t.Fatalf("empty JSON = %s, want empty series array", raw)
	}
}

// TestQueryWindowsResultShape 锁定成功结果 JSON 形状：op 为 query_windows，
// 序列条目只有 name、labels、windows，窗口只有 start、end、count、average；
// 无标签序列 labels 为 {}；不携带 query 的顶层 count/average。
func TestQueryWindowsResultShape(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":0,"value":1},{"name":"m","timestamp":2,"value":3}]`)
	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 2, 2))
	raw := marshalCompact(t, res)
	want := `{"status":"ok","op":"query_windows","series":[` +
		`{"name":"m","labels":{},"windows":[` +
		`{"start":0,"end":1,"count":1,"average":1},` +
		`{"start":2,"end":2,"count":1,"average":3}]}]}`
	if raw != want {
		t.Fatalf("query_windows JSON = %s\nwant %s", raw, want)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	entry := decoded["series"].([]any)[0].(map[string]any)
	for _, banned := range []string{"points", "count", "average"} {
		if _, ok := entry[banned]; ok {
			t.Fatalf("series entry must not carry %q: %v", banned, entry)
		}
	}
	win := entry["windows"].([]any)[0].(map[string]any)
	for _, key := range []string{"start", "end", "count", "average"} {
		if _, ok := win[key]; !ok {
			t.Fatalf("window entry must carry %q: %v", key, win)
		}
	}
}

// TestQueryWindowsStepLargerThanRange step 宽于区间时只有一个截到 end 的窗口。
func TestQueryWindowsStepLargerThanRange(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1000,"value":2},
		{"name":"m","timestamp":1500,"value":4}
	]`)
	res := mustQueryWindows(t, store, queryWindowsAll("m", 1000, 1500, 1_000_000))
	got := res.Series[0].Windows
	if len(got) != 1 {
		t.Fatalf("windows = %+v, want a single clipped window", got)
	}
	assertWindow(t, got[0], WindowStat{Start: 1000, End: 1500, Count: 2, Average: 3}, "clipped window")
}

// TestQueryWindowsLabelSelection 名称与标签筛选沿用 query：省略 labels 或 {}
// 命中全部序列且保留完整标签集合；子集匹配额外标签；无标签 labels 为 {}。
func TestQueryWindowsLabelSelection(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":1,"value":4,"labels":{"zone":"x","host":"h"}},
		{"name":"m","timestamp":1,"value":3,"labels":{"zone":"x"}},
		{"name":"other","timestamp":1,"value":9}
	]`)

	// 省略 labels：该指标全部序列，次序与 query 一致。
	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 1, 10))
	if len(res.Series) != 3 {
		t.Fatalf("all series = %+v, want 3", res.Series)
	}
	wantLabels := []map[string]string{
		{},
		{"host": "h", "zone": "x"},
		{"zone": "x"},
	}
	for i, want := range wantLabels {
		if !sameLabels(res.Series[i].Labels, want) {
			t.Fatalf("series[%d] labels = %v, want %v", i, res.Series[i].Labels, want)
		}
		if raw := marshalCompact(t, res.Series[0].Labels); i == 0 && raw != `{}` {
			t.Fatalf("unlabeled labels JSON = %s, want {}", raw)
		}
	}

	// 空对象等价省略；子集匹配；额外约束；空字符串值；未命中；不同指标。
	res = mustQueryWindows(t, store, `{"op":"query_windows","name":"m","start":0,"end":1,"step":10,"labels":{}}`)
	if len(res.Series) != 3 {
		t.Fatalf("empty labels = %+v, want all 3", res.Series)
	}
	res = mustQueryWindows(t, store, queryWindowsLabels("m", 0, 1, 10, map[string]string{"zone": "x"}))
	if len(res.Series) != 2 {
		t.Fatalf("subset match = %+v, want 2 series", res.Series)
	}
	res = mustQueryWindows(t, store, queryWindowsLabels("m", 0, 1, 10, map[string]string{"zone": "x", "host": "h"}))
	if len(res.Series) != 1 || res.Series[0].Windows[0].Average != 4 {
		t.Fatalf("fully specified match = %+v, want average 4", res.Series)
	}
	res = mustQueryWindows(t, store, queryWindowsAll("nope", 0, 1, 10))
	if len(res.Series) != 0 {
		t.Fatalf("unknown metric = %+v, want empty", res.Series)
	}
}

// TestQueryWindowsInt64Boundaries 区间与 step 触及 int64 上下界时，窗口边界
// 不溢出、不倒置，MinInt64/MaxInt64 上的采样都不遗漏。
func TestQueryWindowsInt64Boundaries(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", tsMinInt64, 1, nil)+","+
		sample("m", 0, 3, nil)+","+
		sample("m", tsMaxInt64, 2, nil)+"]")

	// step=1：两端各自独立成单毫秒窗口，end 绝不越过 MaxInt64、start 不越下界。
	res := mustQueryWindows(t, store, queryWindowsAll("m", tsMinInt64, tsMaxInt64, 1))
	got := res.Series[0].Windows
	if len(got) != 3 {
		t.Fatalf("step 1 windows = %+v, want 3", got)
	}
	assertWindow(t, got[0], WindowStat{Start: tsMinInt64, End: tsMinInt64, Count: 1, Average: 1}, "min window")
	assertWindow(t, got[1], WindowStat{Start: 0, End: 0, Count: 1, Average: 3}, "zero window")
	assertWindow(t, got[2], WindowStat{Start: tsMaxInt64, End: tsMaxInt64, Count: 1, Average: 2}, "max window")

	// step=MaxInt64 跨满整个 int64：常规窗口宽度 step-1，末窗口截到 MaxInt64，
	// 起点由时间戳反推，不能做 start+idx*step 的 int64 加法。
	res = mustQueryWindows(t, store, queryWindowsAll("m", tsMinInt64, tsMaxInt64, tsMaxInt64))
	got = res.Series[0].Windows
	if len(got) != 3 {
		t.Fatalf("max-step windows = %+v, want 3", got)
	}
	assertWindow(t, got[0], WindowStat{Start: tsMinInt64, End: -2, Count: 1, Average: 1}, "first wide window")
	assertWindow(t, got[1], WindowStat{Start: -1, End: tsMaxInt64 - 2, Count: 1, Average: 3}, "middle wide window")
	assertWindow(t, got[2], WindowStat{Start: tsMaxInt64 - 1, End: tsMaxInt64, Count: 1, Average: 2}, "clipped last window")

	// 上界附近的小区间：常规终点本应越过 MaxInt64，截到 end 而不溢出。
	res = mustQueryWindows(t, store, queryWindowsAll("m", tsMaxInt64-1, tsMaxInt64, tsMaxInt64))
	got = res.Series[0].Windows
	if len(got) != 1 {
		t.Fatalf("top-edge windows = %+v, want 1", got)
	}
	assertWindow(t, got[0], WindowStat{Start: tsMaxInt64 - 1, End: tsMaxInt64, Count: 1, Average: 2}, "top edge")

	// 下界附近的小区间：起点与终点都保持在合法范围内。
	res = mustQueryWindows(t, store, queryWindowsAll("m", tsMinInt64, tsMinInt64+1, tsMaxInt64))
	got = res.Series[0].Windows
	if len(got) != 1 {
		t.Fatalf("bottom-edge windows = %+v, want 1", got)
	}
	assertWindow(t, got[0], WindowStat{Start: tsMinInt64, End: tsMinInt64 + 1, Count: 1, Average: 1}, "bottom edge")
}

// TestQueryWindowsExactAveragePerWindow 每个窗口各自在有理数上精确求和、精确
// 除以窗口内点数后按最近偶数舍入：窗口内正负大数抵消留下小余量、窗口总和
// 超出 float64 范围、精确零输出 +0、次正规居中取偶，都与 query 规则一致。
func TestQueryWindowsExactAveragePerWindow(t *testing.T) {
	// 同一窗口内 1e16、1、-1e16：精确均值 1/3。
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1e16},
		{"name":"m","timestamp":2,"value":1},
		{"name":"m","timestamp":3,"value":-1e16}
	]`)
	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	win := res.Series[0].Windows[0]
	if win.Count != 3 || win.Average != 1.0/3.0 {
		t.Fatalf("cancellation window = %+v, want count 3 average 1/3", win)
	}

	// 同窗口三个 1e308：总和约 3e308 超出 float64，均值仍有限且为 1e308。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":1e308},
		{"name":"m","timestamp":2,"value":1e308},
		{"name":"m","timestamp":3,"value":1e308}
	]`)
	res = mustQueryWindows(t, store2, queryWindowsAll("m", 0, 9, 10))
	win = res.Series[0].Windows[0]
	if math.IsInf(win.Average, 0) || math.IsNaN(win.Average) || win.Average != 1e308 {
		t.Fatalf("overflow window = %+v, want finite 1e308", win)
	}

	// 精确零输出正零。
	store3 := NewMetricStore()
	mustOK(t, store3, `[
		{"name":"m","timestamp":1,"value":1e308},
		{"name":"m","timestamp":2,"value":-1e308}
	]`)
	res = mustQueryWindows(t, store3, queryWindowsAll("m", 0, 9, 10))
	win = res.Series[0].Windows[0]
	if win.Average != 0 || math.Signbit(win.Average) {
		t.Fatalf("exact-zero window = %v, want +0", win.Average)
	}

	// 窗口各自独立统计：两个窗口分别为 [5e-324, 0]（居中取偶为 0）与单点
	// 5e-324（保留最小次正规值）。
	store4 := NewMetricStore()
	mustOK(t, store4, `[
		{"name":"m","timestamp":0,"value":5e-324},
		{"name":"m","timestamp":1,"value":0},
		{"name":"m","timestamp":10,"value":5e-324}
	]`)
	res = mustQueryWindows(t, store4, queryWindowsAll("m", 0, 20, 10))
	wins := res.Series[0].Windows
	if len(wins) != 2 {
		t.Fatalf("subnormal windows = %+v, want 2", wins)
	}
	if wins[0].Average != 0 || math.Signbit(wins[0].Average) {
		t.Fatalf("tie window average = %v, want +0 (round to even)", wins[0].Average)
	}
	if wins[1].Average != 5e-324 {
		t.Fatalf("single-subnormal window = %v, want 5e-324", wins[1].Average)
	}
}

// TestQueryWindowsCountsOnlyCommittedPointsAndReadOnly 失败批次回滚的点不计入
// 任何窗口，等值重复不增加 count；查询只读，重复查询结果一致且存储不变。
func TestQueryWindowsCountsOnlyCommittedPointsAndReadOnly(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":2}]`)
	mustOK(t, store, `[{"name":"m","timestamp":2,"value":4}]`)

	baseline := func() {
		t.Helper()
		res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
		assertWindow(t, res.Series[0].Windows[0],
			WindowStat{Start: 0, End: 9, Count: 2, Average: 3}, "baseline window")
	}
	baseline()

	// 失败批次整批回滚：其中任何点都不能出现在窗口里。
	mustFail(t, store, `[{"name":"m","timestamp":3,"value":8},{"name":"m","timestamp":1,"value":100}]`)
	baseline()

	// 等值重复被忽略，不增加 count。
	mustOK(t, store, `[{"name":"m","timestamp":2,"value":4.0}]`)
	baseline()

	// 只读：重复查询一致，写入快照与 query 结果不变。
	r1 := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	r2 := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("repeated query_windows differs: %+v vs %+v", r1, r2)
	}
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 2 {
		t.Fatalf("query_windows mutated storage, snapshot = %+v", snap.Series)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":9}`)
	if qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("query after query_windows = %+v", qr.Series)
	}
}

// TestQueryWindowsStepValidation step 必须是大于零且在 int64 范围内的 JSON
// 整数；缺失、类型不符、非整数、零、负数、超界都返回指出 step 的查询错误。
func TestQueryWindowsStepValidation(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	cases := []struct {
		name string
		line string
		want string
	}{
		{"missing", `{"op":"query_windows","name":"m","start":0,"end":1}`,
			`missing required field "step"`},
		{"zero", `{"op":"query_windows","name":"m","start":0,"end":1,"step":0}`,
			`field "step": must be a JSON integer greater than zero`},
		{"negative", `{"op":"query_windows","name":"m","start":0,"end":1,"step":-1}`,
			`field "step": must be a JSON integer greater than zero`},
		{"float", `{"op":"query_windows","name":"m","start":0,"end":1,"step":0.5}`,
			`field "step"`},
		{"integer-looking float", `{"op":"query_windows","name":"m","start":0,"end":1,"step":1.0}`,
			`field "step"`},
		{"string", `{"op":"query_windows","name":"m","start":0,"end":1,"step":"1"}`,
			`field "step" must be a JSON number`},
		{"boolean", `{"op":"query_windows","name":"m","start":0,"end":1,"step":true}`,
			`field "step" must be a JSON number`},
		{"null", `{"op":"query_windows","name":"m","start":0,"end":1,"step":null}`,
			`field "step" must be a JSON number`},
		{"beyond int64", `{"op":"query_windows","name":"m","start":0,"end":1,"step":9223372036854775808}`,
			`field "step"`},
		{"below int64", `{"op":"query_windows","name":"m","start":0,"end":1,"step":-9223372036854775809}`,
			`field "step"`},
		{"duplicate", `{"op":"query_windows","name":"m","start":0,"end":1,"step":1,"step":2}`,
			`duplicate field "step"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lerr := mustQueryWindowsFail(t, store, tc.line)
			assertQueryError(t, lerr, tc.want)
		})
	}
}

// TestQueryWindowsStepRejectedByOtherOps step 只属于 query_windows：query 与
// query_points 上出现 step 一律按未知字段拒绝，原操作输入规则保持兼容。
func TestQueryWindowsStepRejectedByOtherOps(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	for _, line := range []string{
		`{"op":"query","name":"m","start":0,"end":1,"step":1}`,
		`{"op":"query_points","name":"m","start":0,"end":1,"step":1}`,
		// op 写在 step 之后时，解析结束后仍要指出 step 对该 op 是未知字段。
		`{"step":1,"op":"query","name":"m","start":0,"end":1}`,
	} {
		lerr := mustQueryFail(t, store, line)
		assertQueryError(t, lerr, `unknown field "step"`)
	}

	// 原有操作不带 step 时行为完全不变。
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":1}`)
	if len(res.Series) != 1 || res.Series[0].Average != 1 {
		t.Fatalf("query without step changed: %+v", res.Series)
	}
}

// TestQueryWindowsOtherFieldErrorsAndOrder 其他字段沿用 query 的校验，且结构
// 完整对象按字段书写顺序定原因：step 类型错误与未知字段谁在前就报谁。
func TestQueryWindowsOtherFieldErrorsAndOrder(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	cases := []struct {
		name string
		line string
		want string
	}{
		{"name wrong type", `{"op":"query_windows","name":7,"start":0,"end":1,"step":1}`,
			`field "name" must be a string`},
		{"missing name", `{"op":"query_windows","start":0,"end":1,"step":1}`,
			`missing required field "name"`},
		{"missing start", `{"op":"query_windows","name":"m","end":1,"step":1}`,
			`missing required field "start"`},
		{"missing end", `{"op":"query_windows","name":"m","start":0,"step":1}`,
			`missing required field "end"`},
		{"missing op", `{"name":"m","start":0,"end":1,"step":1}`,
			`missing required field "op"`},
		{"unknown op", `{"op":"ping","name":"m","start":0,"end":1,"step":1}`,
			`unknown op "ping"`},
		{"labels wrong type", `{"op":"query_windows","name":"m","start":0,"end":1,"step":1,"labels":[]}`,
			`field "labels" must be an object`},
		{"step bad type before unknown", `{"op":"query_windows","name":"m","start":0,"end":1,"step":"x","bogus":1}`,
			`field "step" must be a JSON number`},
		{"unknown before step bad type", `{"op":"query_windows","name":"m","start":0,"end":1,"bogus":1,"step":"x"}`,
			`unknown field "bogus"`},
		{"inverted range", `{"op":"query_windows","name":"m","start":5,"end":1,"step":1}`,
			`invalid range: "start" must not be greater than "end" (5 > 1)`},
		{"inverted range at extremes", queryWindowsAll("m", tsMaxInt64, tsMinInt64, tsMaxInt64),
			fmt.Sprintf("%d > %d", tsMaxInt64, tsMinInt64)},
		{"trailing content", `{"op":"query_windows","name":"m","start":0,"end":1,"step":1} trailing`,
			"invalid JSON"},
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

	// 缺 step 与倒置区间同时存在时，字段/必填检查先于区间检查，先报 step。
	lerr := mustQueryWindowsFail(t, store, `{"op":"query_windows","name":"m","start":5,"end":1}`)
	assertQueryError(t, lerr, `missing required field "step"`)

	// 所有失败之后存储不变。
	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	assertWindow(t, res.Series[0].Windows[0],
		WindowStat{Start: 0, End: 9, Count: 1, Average: 1}, "data after failures")
}

// TestQueryWindowsViaProcessLine 统一入口对 query_windows 返回 *QueryWindowsResult；
// 专用入口交叉使用时拒绝其他 op；倒置区间经统一入口仍是 nil 结果 + 错误。
func TestQueryWindowsViaProcessLine(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	r, lerr := store.ProcessLine(`{"op":"query_windows","name":"cpu","start":0,"end":2000,"step":1000}`)
	if lerr != nil || r == nil {
		t.Fatalf("ProcessLine query_windows: r=%v lerr=%+v", r, lerr)
	}
	qw, ok := r.(*QueryWindowsResult)
	if !ok {
		t.Fatalf("result type = %T, want *QueryWindowsResult", r)
	}
	if qw.Op != "query_windows" || len(qw.Series) != 1 || len(qw.Series[0].Windows) != 1 {
		t.Fatalf("ProcessLine query_windows = %+v", qw)
	}

	r, lerr = store.ProcessLine(`{"op":"query_windows","name":"cpu","start":9,"end":1,"step":1}`)
	if r != nil || lerr == nil || !strings.Contains(lerr.Error, "9 > 1") {
		t.Fatalf("inverted via ProcessLine: r=%v lerr=%+v", r, lerr)
	}

	// 交叉使用专用入口：都报明确错误，不返回另一类型结果。
	if _, lerr = store.QueryWindowsLine(`{"op":"query","name":"cpu","start":0,"end":1}`); lerr == nil {
		t.Fatalf("QueryWindowsLine must reject op query")
	}
	if _, lerr = store.QueryWindowsLine(`{"op":"query_points","name":"cpu","start":0,"end":1}`); lerr == nil {
		t.Fatalf("QueryWindowsLine must reject op query_points")
	}
	if _, lerr = store.QueryLine(`{"op":"query_windows","name":"cpu","start":0,"end":1,"step":1}`); lerr == nil {
		t.Fatalf("QueryLine must reject op query_windows")
	}
	if _, lerr = store.QueryPointsLine(`{"op":"query_windows","name":"cpu","start":0,"end":1,"step":1}`); lerr == nil {
		t.Fatalf("QueryPointsLine must reject op query_windows")
	}
}

// TestQueryWindowsWindowOrderIndependentOfMapTraversal 点在 map 中的遍历次序
// 随机，但窗口必须稳定地按窗口起点升序输出：多点、多窗口下反复构造结果一致。
func TestQueryWindowsWindowOrderIndependentOfMapTraversal(t *testing.T) {
	store := NewMetricStore()
	var b strings.Builder
	b.WriteByte('[')
	// 0、100、…、2500 共 26 个点：恰好让三个窗口都有点，末窗口截到 2500。
	for i := int64(0); i < 26; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"m","timestamp":%d,"value":%d}`, i*100, i)
	}
	b.WriteByte(']')
	mustOK(t, store, b.String())

	wantStarts := []int64{0, 1000, 2000}
	wantEnds := []int64{999, 1999, 2500}
	for iter := 0; iter < 5; iter++ {
		res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 2500, 1000))
		wins := res.Series[0].Windows
		if len(wins) != len(wantStarts) {
			t.Fatalf("iter %d: windows = %+v", iter, wins)
		}
		for i, ws := range wantStarts {
			if wins[i].Start != ws {
				t.Fatalf("iter %d: window %d start = %d, want %d (windows out of order)",
					iter, i, wins[i].Start, ws)
			}
			if wins[i].End != wantEnds[i] {
				t.Fatalf("iter %d: window %d end = %d, want %d", iter, i, wins[i].End, wantEnds[i])
			}
		}
	}
}
