package darksafe

import (
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 本文件为固定时间窗口统计 query_windows 增加自动回归保障：用户在同一份已成功
// 写入的数据上，用相同的指标名、标签条件与起止时间分别查询 query_windows 与
// query_points（前者额外给出合法 step），两种结果必须描述同一组采样，窗口统计
// 能从采样明细逐条核对：
//
//   - 两边返回的指标名、完整标签集合与序列次序逐一相同；标签条件只筛选序列，
//     结果中保留完整标签集合；同名但完整标签不同的序列各自统计，即使采样时间
//     戳完全相同也绝不合并；区间内无点的序列两边都不出现，完全未命中时两边都
//     成功返回非 nil 空数组；
//   - 窗口从查询起点 start 划分（k 号窗口为
//     [start+k*step, start+(k+1)*step-1]，最后一个截到 end），不跟随各序列
//     的第一个采样点移动；本文件的复核侧用 big.Int 独立按 start 铺窗口，
//     不经过生产代码的窗口划分，能拦住“按首点对齐”这类双侧同源错误；
//   - 每个返回窗口的 count 等于该序列明细中落在窗口闭区间内的点数，average
//     等于这些点实际存储 float64 值的精确算术平均（big.Rat 精确求和、精确
//     相除后按最近偶舍入，复用 query_detail_correspondence_test.go 中与生产
//     舍入实现无关的 detailMean）；
//   - 整段明细中的每个点恰好计入一个窗口：明细点先按大整数参考划分严格分组，
//     再逐组与输出窗口配对，遗漏、跨窗口重复计数、空窗口补零或窗口多出都会被
//     查出；1999 与 2000 上的点分属相邻窗口，end 上的点归入最后一个窗口；
//   - 不同序列可以缺少不同窗口，只有实际有点的窗口出现，并按窗口起点升序；
//   - 窗口统计相互独立：其他窗口中的点不影响本窗口的 count 与 average，
//     同一窗口内 1e16、1、-1e16 抵消后的余量必须保留（三个点均值为
//     0.3333333333333333），切换窗口后数字只反映新窗口自己的采样；
//   - 即使起止时间接近 int64 上下界、step 接近 int64 上界，参考划分仍在
//     big.Int 上完成，窗口不溢出、不倒置、不遗漏采样。
//
// 每条失败信息都指出查询区间（[start,end] 与 step）、完整序列身份
// （指标名+完整标签 JSON 签名）与出问题的窗口，并说明是点数、采样归属、
// 均值、窗口边界还是排列次序不符，便于定位后续修改造成的偏差。

// refWindow 是复核侧独立铺出的一个非空参考窗口：边界来自查询起点 start 与
// step 的大整数铺砌，points 是 query_points 明细中按 floor((ts-start)/step)
// 唯一归入该窗口的全部采样（归属计算与生产代码完全独立）。
type refWindow struct {
	index  uint64
	start  int64
	end    int64
	points []Point
}

// referenceWindowsFromDetail 用 big.Int 从查询起点独立铺窗口并把明细点分组：
// k = floor((ts-start)/step)，窗口为 [start+k*step, min(start+(k+1)*step-1,end)]。
// 起点严格取查询 start，不看任何点的位置，因此生产实现若把窗口跟随某序列首个
// 采样点移动，输出的窗口边界就会与这里不一致。调用方保证明细点都在 [start,end]
// 内且 step > 0；k 最大可达 2^64-1（step=1 跨整个 int64），以 uint64 承载。
func referenceWindowsFromDetail(t *testing.T, points []Point, start, end, step int64, note string) []refWindow {
	t.Helper()
	bStart := big.NewInt(start)
	bEnd := big.NewInt(end)
	bStep := big.NewInt(step)
	bOne := big.NewInt(1)

	groups := map[uint64][]Point{}
	indexes := make([]uint64, 0)
	for _, p := range points {
		if p.Timestamp < start || p.Timestamp > end {
			t.Fatalf("%s: internal: detail point %d lies outside [%d,%d]", note, p.Timestamp, start, end)
		}
		// 非负差整除 == floor；全程大整数，start 为 MinInt64、step 为 MaxInt64
		// 也不溢出。
		k := new(big.Int).Quo(new(big.Int).Sub(big.NewInt(p.Timestamp), bStart), bStep)
		if k.Sign() < 0 {
			t.Fatalf("%s: internal: negative window index for ts=%d", note, p.Timestamp)
		}
		if !k.IsUint64() {
			t.Fatalf("%s: window index %v overflows uint64 for ts=%d", note, k, p.Timestamp)
		}
		ki := k.Uint64()
		if _, seen := groups[ki]; !seen {
			indexes = append(indexes, ki)
		}
		groups[ki] = append(groups[ki], p)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })

	out := make([]refWindow, 0, len(indexes))
	for _, ki := range indexes {
		// ws = start + k*step；we = min(ws+step-1, end)，全程大整数。
		ws := new(big.Int).Add(bStart, new(big.Int).Mul(new(big.Int).SetUint64(ki), bStep))
		we := new(big.Int).Sub(new(big.Int).Add(ws, bStep), bOne)
		if we.Cmp(bEnd) > 0 {
			we.Set(bEnd)
		}
		if !ws.IsInt64() || !we.IsInt64() {
			t.Fatalf("%s: reference window %d bounds overflow int64: [%v,%v]", note, ki, ws, we)
		}
		out = append(out, refWindow{index: ki, start: ws.Int64(), end: we.Int64(), points: groups[ki]})
	}
	return out
}

// assertWindowsDetailCorrespond 在同一份数据上用完全相同的条件执行
// query_windows 与 query_points（仅前者多带 step），逐条核对两边描述同一组
// 采样。返回两边结果供调用方做额外的点名核验。失败信息带区间、序列身份与
// 窗口坐标。labels 为 nil 时省略 labels 字段，否则按子集查询。
func assertWindowsDetailCorrespond(t *testing.T, store *MetricStore, name string, labels map[string]string, start, end, step int64) (*QueryWindowsResult, *QueryPointsResult) {
	t.Helper()
	note := fmt.Sprintf("range [%d,%d] step %d metric %q", start, end, step, name)
	var winLine, pointLine string
	if labels == nil {
		winLine = queryWindowsAll(name, start, end, step)
		pointLine = queryPointsAll(name, start, end)
		note += " labels=<omit>"
	} else {
		winLine = queryWindowsLabels(name, start, end, step, labels)
		pointLine = queryPointsLabels(name, start, end, labels)
		note += " labels=" + jsonLabels(labels)
	}

	wins := mustQueryWindows(t, store, winLine)
	detail := mustQueryPoints(t, store, pointLine)
	if wins.Status != "ok" || wins.Op != "query_windows" {
		t.Fatalf("%s: windows envelope = %q/%q, want ok/query_windows", note, wins.Status, wins.Op)
	}
	if detail.Status != "ok" || detail.Op != "query_points" {
		t.Fatalf("%s: detail envelope = %q/%q, want ok/query_points", note, detail.Status, detail.Op)
	}

	// 序列数量一致：无点序列两边都不补；完全未命中两边都是非 nil 空数组。
	if (wins.Series == nil) != (detail.Series == nil) || len(wins.Series) != len(detail.Series) {
		t.Fatalf("%s: series count differs: query_windows lists %d %s, query_points lists %d %s",
			note, len(wins.Series), windowsIdentitySigs(wins.Series), len(detail.Series), pointsIdentitySigs(detail.Series))
	}
	if len(wins.Series) == 0 {
		if wins.Series == nil || detail.Series == nil {
			t.Fatalf("%s: complete miss must be a non-nil empty array on both sides", note)
		}
		return wins, detail
	}

	// 序列身份（指标名+完整标签集合）与排列次序逐一相同。
	for i := range wins.Series {
		sigW := identitySig(wins.Series[i].Name, wins.Series[i].Labels)
		sigP := identitySig(detail.Series[i].Name, detail.Series[i].Labels)
		if sigW != sigP {
			t.Fatalf("%s: series[%d] identity/order mismatch:\nquery_windows #%d = %s\nquery_points  #%d = %s",
				note, i, i, sigW, i, sigP)
		}
	}

	for i := range wins.Series {
		ws := wins.Series[i]
		ps := detail.Series[i]
		sig := identitySig(ws.Name, ws.Labels)
		// 两边携带的标签必须是同一套完整集合（不缩减成查询条件）。
		if !sameLabels(ws.Labels, ps.Labels) {
			t.Fatalf("%s: %s: labels differ between results: windows=%v points=%v",
				note, sig, ws.Labels, ps.Labels)
		}

		// 明细按时间戳严格升序、两两不同。
		for j, p := range ps.Points {
			if p.Timestamp < start || p.Timestamp > end {
				t.Fatalf("%s: %s: detail point[%d] ts=%d is outside the closed range",
					note, sig, j, p.Timestamp)
			}
			if j > 0 && ps.Points[j-1].Timestamp >= p.Timestamp {
				t.Fatalf("%s: %s: detail not strictly ascending at index %d: %+v",
					note, sig, j, ps.Points)
			}
		}

		// 复核侧独立从 start 铺窗口、把每个明细点唯一分组。
		refs := referenceWindowsFromDetail(t, ps.Points, start, end, step, note+" "+sig)

		// 窗口数量一致：空窗口两边都不能出现，非空窗口不能遗漏。
		if len(ws.Windows) != len(refs) {
			t.Fatalf("%s: %s: query_windows lists %d windows %s, but %d non-empty windows are implied by the detail %s",
				note, sig, len(ws.Windows), windowStartsText(ws.Windows), len(refs), refStartsText(refs))
		}

		var prevEnd int64
		assigned := 0
		for j, ref := range refs {
			got := ws.Windows[j]
			winNote := fmt.Sprintf("%s: %s: window[%d] [%d,%d]", note, sig, j, ref.start, ref.end)
			// 窗口按起点升序、彼此不重叠不遗漏（相邻窗口首尾相接）。
			if j > 0 && got.Start <= prevEnd {
				t.Fatalf("%s: windows are not in ascending non-overlapping order (previous end %d)",
					winNote, prevEnd)
			}
			// 窗口边界必须等于从查询起点铺出的参考边界（不跟随首点移动）。
			if got.Start != ref.start || got.End != ref.end {
				t.Fatalf("%s: bounds = [%d,%d], want detail-derived bounds [%d,%d] (windows must be tiled from the query start, not from this series' first sample)",
					winNote, got.Start, got.End, ref.start, ref.end)
			}
			if got.Start > got.End || got.Start < start || got.End > end {
				t.Fatalf("%s: window is inverted or out of range: %+v", winNote, got)
			}
			// 采样归属：该窗口内的点必须恰好是闭区间 [got.Start,got.End]
			// 命中的那些明细点；逐点点名，边界毫秒（如 1999/2000）归错邻窗
			// 或跨窗重复计数都会在这里暴露。
			var inBounds []Point
			for _, p := range ps.Points {
				if p.Timestamp >= got.Start && p.Timestamp <= got.End {
					inBounds = append(inBounds, p)
				}
			}
			if !reflect.DeepEqual(inBounds, ref.points) {
				t.Fatalf("%s: point assignment mismatch: floor-index group=%+v but closed-interval membership=%+v",
					winNote, ref.points, inBounds)
			}
			// count 必须等于明细中落在窗口闭区间内的点数。
			if got.Count != len(ref.points) {
				t.Fatalf("%s: count = %d, but %d detail points %+v fall in this window",
					winNote, got.Count, len(ref.points), ref.points)
			}
			if got.Count == 0 {
				t.Fatalf("%s: an empty window must not be emitted", winNote)
			}
			// average 必须是这些实际存储值的精确算术平均（最近偶 float64），
			// 由明细独立复算，不经过生产侧的 ratToFloat64NearestEven。
			wantAvg := detailMean(t, ref.points)
			if got.Average != wantAvg {
				t.Fatalf("%s: average = %.17g, but recomputed from this window's detail points %+v = %.17g (other windows must not affect it)",
					winNote, got.Average, ref.points, wantAvg)
			}
			assigned += got.Count
			prevEnd = got.End
		}

		// 整段明细中的每个点恰好计入一个窗口：不漏不重。
		if assigned != len(ps.Points) {
			t.Fatalf("%s: %s: window counts sum to %d but the detail has %d points (some point is missing or double-counted): windows=%+v detail=%+v",
				note, sig, assigned, len(ps.Points), ws.Windows, ps.Points)
		}
	}
	return wins, detail
}

// windowsIdentitySigs / pointsIdentitySigs 在序列数量不一致的失败信息中列出
// 两边全部序列身份。
func windowsIdentitySigs(xs []QueryWindowsSeries) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = identitySig(x.Name, x.Labels)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func pointsIdentitySigs(xs []QueryPointsSeries) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = identitySig(x.Name, x.Labels)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func windowStartsText(ws []Window) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = fmt.Sprintf("[%d,%d](n=%d)", w.Start, w.End, w.Count)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func refStartsText(rs []refWindow) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("[%d,%d](n=%d)", r.start, r.end, len(r.points))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// TestWindowsAndPointsSpecTilingAndBoundaries 任务书指定的划分：查询
// [1000,3000]、step 1000 时边界是 [1000,1999]、[2000,2999]、[3000,3000]；
// 1999 与 2000 上的点分属相邻窗口，3000 上的点归入最后一个窗口；同名不同
// 完整标签的序列即使时间戳完全相同也各自统计，互不合并；没有区间内点的
// 序列两边都不出现。
func TestWindowsAndPointsSpecTilingAndBoundaries(t *testing.T) {
	store := NewMetricStore()
	hostA := map[string]string{"host": "a"}
	hostB := map[string]string{"host": "b"}
	hostC := map[string]string{"host": "c"}
	mustOK(t, store, "["+
		// host=a：三个窗口都有点，1999/2000 边界两侧各放一个点。
		sample("cpu", 1000, 2, hostA)+","+
		sample("cpu", 1999, 3, hostA)+","+
		sample("cpu", 2000, 5, hostA)+","+
		sample("cpu", 2999, 7, hostA)+","+
		sample("cpu", 3000, 11, hostA)+","+
		// host=b：与 host=a 在完全相同的两个边界时间戳上各有点，必须独立成窗。
		sample("cpu", 1999, 100, hostB)+","+
		sample("cpu", 2000, 200, hostB)+","+
		// host=c：唯一一点落在中间窗口，窗口起点必须是 2000（从查询起点铺），
		// 而不能跟随首个采样点变成 2500。
		sample("cpu", 2500, 9, hostC)+","+
		// host=d：所有点都在查询区间外，两边都不出现。
		sample("cpu", 500, 1, map[string]string{"host": "d"})+","+
		sample("cpu", 3500, 1, map[string]string{"host": "d"})+"]")

	wins, detail := assertWindowsDetailCorrespond(t, store, "cpu", nil, 1000, 3000, 1000)

	// 三条有点的序列、统一次序；host=d 不出现。
	if len(wins.Series) != 3 || len(detail.Series) != 3 {
		t.Fatalf("[1000,3000] step 1000: series wins=%s points=%s, want exactly a,b,c",
			windowsIdentitySigs(wins.Series), pointsIdentitySigs(detail.Series))
	}

	want := []struct {
		labels  map[string]string
		windows []Window
	}{
		{hostA, []Window{
			window(1000, 1999, 2, 2.5), // 1999 的点归入前一窗口
			window(2000, 2999, 2, 6),   // 2000 的点归入下一窗口
			window(3000, 3000, 1, 11),  // end 上的点归入最后窗口
		}},
		{hostB, []Window{
			window(1000, 1999, 1, 100),
			window(2000, 2999, 1, 200),
		}},
		{hostC, []Window{
			window(2000, 2999, 1, 9), // 窗口不跟随首点 2500 移动
		}},
	}
	for i, w := range want {
		got := wins.Series[i]
		if !sameLabels(got.Labels, w.labels) {
			t.Fatalf("series[%d] = %s, want %v", i, identitySig(got.Name, got.Labels), w.labels)
		}
		assertWindows(t, got.Windows, w.windows, fmt.Sprintf("cpu%v concrete windows", w.labels))
	}

	// 标签子集 host=b：时间戳与 host=a 相同，但结果仍是 host=b 自己的两个
	// 单窗口，证明同名序列不按时间戳合并。
	wb, _ := assertWindowsDetailCorrespond(t, store, "cpu", hostB, 1000, 3000, 1000)
	if len(wb.Series) != 1 {
		t.Fatalf("host=b subset = %s, want exactly one series", windowsIdentitySigs(wb.Series))
	}
	assertWindows(t, wb.Series[0].Windows, []Window{
		window(1000, 1999, 1, 100),
		window(2000, 2999, 1, 200),
	}, "same timestamps as host=a must not merge series")
}

// TestWindowsAndPointsCancellationIsolatedPerWindow 任务书指定的精度场景：
// 同一窗口内 1e16、1、-1e16 抵消后的余量保留，三点均值 JSON 为
// 0.3333333333333333；其他窗口中的点不能影响它，切换窗口后 count 与均值
// 只反映新窗口自己的采样；另一序列在同一窗口放两个 1e308，朴素求和溢出但
// 窗口均值仍有限，且两条序列互不串算。
func TestWindowsAndPointsCancellationIsolatedPerWindow(t *testing.T) {
	store := NewMetricStore()
	a := map[string]string{"host": "a"}
	b := map[string]string{"host": "b"}
	mustOK(t, store, "["+
		// 窗口 [1000,1999]：正负大数加 1，精确余量 1，均值 1/3。
		sample("cpu", 1000, 1e16, a)+","+
		sample("cpu", 1500, 1, a)+","+
		sample("cpu", 1900, -1e16, a)+","+
		// 窗口 [2000,2999]：两个普通点，均值 15——绝不能混入上一窗口的大数。
		sample("cpu", 2000, 10, a)+","+
		sample("cpu", 2500, 20, a)+","+
		// 窗口 [3000,3000]：单点。
		sample("cpu", 3000, 9, a)+","+
		// host=b：与 host=a 的窗口相同，放两个 1e308，均值有限且独立。
		sample("cpu", 1000, 1e308, b)+","+
		sample("cpu", 1999, 1e308, b)+"]")

	wins, _ := assertWindowsDetailCorrespond(t, store, "cpu", nil, 1000, 3000, 1000)
	if len(wins.Series) != 2 {
		t.Fatalf("series = %s, want host=a and host=b", windowsIdentitySigs(wins.Series))
	}
	wa := wins.Series[findWindowsIndex(t, wins, "cpu", a)]
	assertWindows(t, wa.Windows, []Window{
		window(1000, 1999, 3, 1.0/3.0),
		window(2000, 2999, 2, 15),
		window(3000, 3000, 1, 9),
	}, "per-window independent statistics")
	if wa.Windows[0].Average != 1.0/3.0 {
		t.Fatalf("cancellation window average = %.17g, want 1/3 nearest float64", wa.Windows[0].Average)
	}
	if raw := marshalCompact(t, wa.Windows[0].Average); raw != "0.3333333333333333" {
		t.Fatalf("cancellation window average JSON = %s, want 0.3333333333333333", raw)
	}
	wb := wins.Series[findWindowsIndex(t, wins, "cpu", b)]
	assertWindows(t, wb.Windows, []Window{
		window(1000, 1999, 2, 1e308),
	}, "overflow window stays finite and stays separate from host=a")
}

// TestWindowsAndPointsMissingWindowsDifferPerSeries 不同序列可以缺少不同
// 窗口：只有实际有点的窗口出现，按起点升序，空窗口不补零；区间内完全没有
// 点的序列两边都不出现；未命中任何序列时两边都成功返回空数组（JSON []）。
func TestWindowsAndPointsMissingWindowsDifferPerSeries(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":0,"value":1,"labels":{"h":"a"}},
		{"name":"m","timestamp":20,"value":2,"labels":{"h":"a"}},
		{"name":"m","timestamp":21,"value":4,"labels":{"h":"a"}},
		{"name":"m","timestamp":10,"value":8,"labels":{"h":"b"}},
		{"name":"m","timestamp":99,"value":1,"labels":{"h":"out"}}
	]`)

	// h=a 缺窗口 1，h=b 只有窗口 1，h=out 区间内无点；省略 labels 时仅 a、b。
	wins, detail := assertWindowsDetailCorrespond(t, store, "m", nil, 0, 29, 10)
	if len(wins.Series) != 2 || len(detail.Series) != 2 {
		t.Fatalf("[0,29] series = %s, want exactly h=a,h=b", windowsIdentitySigs(wins.Series))
	}
	assertWindows(t, wins.Series[0].Windows, []Window{
		window(0, 9, 1, 1),
		window(20, 29, 2, 3),
	}, "h=a omits its empty middle window")
	assertWindows(t, wins.Series[1].Windows, []Window{
		window(10, 19, 1, 8),
	}, "h=b has only the middle window")

	// 完全未命中的各种情形：两边都成功、非 nil、空数组、JSON 为 []。
	for _, c := range []struct {
		note   string
		name   string
		labels map[string]string
		start  int64
		end    int64
		step   int64
	}{
		{"unknown metric", "nope", nil, 0, 100, 10},
		{"label value miss", "m", map[string]string{"h": "zzz"}, 0, 100, 10},
		{"label key miss", "m", map[string]string{"zone": "x"}, 0, 100, 10},
		{"empty interior", "m", nil, 30, 39, 10},
		{"empty single timestamp", "m", nil, 5, 5, 10},
		{"series exists but range empty", "m", map[string]string{"h": "out"}, 0, 29, 10},
	} {
		t.Run(c.note, func(t *testing.T) {
			w, p := assertWindowsDetailCorrespond(t, store, c.name, c.labels, c.start, c.end, c.step)
			if w.Series == nil || len(w.Series) != 0 || p.Series == nil || len(p.Series) != 0 {
				t.Fatalf("%s: want non-nil empty lists on both sides, got %+v / %+v",
					c.note, w.Series, p.Series)
			}
			if raw := marshalCompact(t, w); raw != `{"status":"ok","op":"query_windows","series":[]}` {
				t.Fatalf("%s: windows JSON = %s, want empty series []", c.note, raw)
			}
			if raw := marshalCompact(t, p); raw != `{"status":"ok","op":"query_points","series":[]}` {
				t.Fatalf("%s: points JSON = %s, want empty series []", c.note, raw)
			}
		})
	}
}

// TestWindowsAndPointsSubsetFilterKeepsFullLabels 标签条件只筛选序列：按子集
// 命中的多条序列在两边逐序列对应，记录中保留查询条件之外的完整标签，次序与
// query/query_points 一致（先名字后排序标签键值对）。
func TestWindowsAndPointsSubsetFilterKeepsFullLabels(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":10,"value":100,"labels":{"zone":"x"}},
		{"name":"m","timestamp":20,"value":200,"labels":{"zone":"x","host":"h"}},
		{"name":"m","timestamp":30,"value":300,"labels":{"zone":"x","host":"h2"}},
		{"name":"m","timestamp":40,"value":400,"labels":{"zone":"y"}},
		{"name":"m","timestamp":50,"value":500}
	]`)

	// 子集 zone=x 命中三条序列，窗口/明细两边都保留完整标签集合。
	wins, detail := assertWindowsDetailCorrespond(t,
		store, "m", map[string]string{"zone": "x"}, 0, 1000, 25)
	wantLabels := []map[string]string{
		{"host": "h", "zone": "x"},
		{"host": "h2", "zone": "x"},
		{"zone": "x"},
	}
	if len(wins.Series) != 3 || len(detail.Series) != 3 {
		t.Fatalf("zone=x subset = %s, want 3 series", windowsIdentitySigs(wins.Series))
	}
	for i, want := range wantLabels {
		if !sameLabels(wins.Series[i].Labels, want) || !sameLabels(detail.Series[i].Labels, want) {
			t.Fatalf("series[%d] labels wins=%v points=%v, want full set %v (must not shrink to the filter)",
				i, wins.Series[i].Labels, detail.Series[i].Labels, want)
		}
	}

	// 区间只命中三条中的一条：两边保留的是同一个子序列，身份不缩减。
	w, p := assertWindowsDetailCorrespond(t, store, "m",
		map[string]string{"zone": "x"}, 15, 25, 25)
	if len(w.Series) != 1 || len(p.Series) != 1 {
		t.Fatalf("[15,25] zone=x = %s / %s, want exactly host=h,zone=x",
			windowsIdentitySigs(w.Series), pointsIdentitySigs(p.Series))
	}
	if !sameLabels(w.Series[0].Labels, map[string]string{"host": "h", "zone": "x"}) {
		t.Fatalf("partial-range labels = %v, want the full set", w.Series[0].Labels)
	}
}

// TestWindowsAndPointsSingleTimestamp start == end 时唯一窗口包含该时间戳上
// 的点；边界正好压在点上，start 即 end，窗口终点也等于 start。
func TestWindowsAndPointsSingleTimestamp(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", 3000, 2, nil)+","+
		sample("m", 3000, 2.0, nil)+"]") // 等值重复不计入

	wins, _ := assertWindowsDetailCorrespond(t, store, "m", nil, 3000, 3000, 1)
	assertWindows(t, wins.Series[0].Windows, []Window{window(3000, 3000, 1, 2)},
		"start==end step=1")
	wins, _ = assertWindowsDetailCorrespond(t, store, "m", nil, 3000, 3000, tsMaxInt64)
	assertWindows(t, wins.Series[0].Windows, []Window{window(3000, 3000, 1, 2)},
		"start==end huge step still one clipped window")
}

// TestWindowsAndPointsInt64ExtremeTiling 起止时间与 step 接近 int64 上下界时，
// 窗口统计仍能与明细逐条核对：复核侧大整数铺窗覆盖跨整个 int64、step 为
// MaxInt64、step 为 1 与负起点小步划分等组合，窗口不溢出、不倒置、不漏点。
func TestWindowsAndPointsInt64ExtremeTiling(t *testing.T) {
	min, max := tsMinInt64, tsMaxInt64
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", min, 3, nil)+","+
		sample("m", -2, 9, nil)+","+
		sample("m", -1, 1, nil)+","+
		sample("m", max, 5, nil)+"]")

	assertWindowsDetailCorrespond(t, store, "m", nil, min, max, max)
	assertWindowsDetailCorrespond(t, store, "m", nil, min, max, 1)
	assertWindowsDetailCorrespond(t, store, "m", nil, min, -1, max)
	assertWindowsDetailCorrespond(t, store, "m", nil, -2, max, max)
	assertWindowsDetailCorrespond(t, store, "m", nil, min, min, 1)
	assertWindowsDetailCorrespond(t, store, "m", nil, max, max, max)

	// 负起点附近的小步划分：[-5,4]、step=3 → [-5,-3]、[-2,0]、[1,3]、[4,4]。
	store2 := NewMetricStore()
	for ts, v := range map[int64]float64{-5: 1, -4: 2, -3: 3, -2: 4, 0: 5, 1: 6, 3: 7, 4: 8} {
		mustOK(t, store2, "["+sample("m", ts, v, nil)+"]")
	}
	wins, _ := assertWindowsDetailCorrespond(t, store2, "m", nil, -5, 4, 3)
	assertWindows(t, wins.Series[0].Windows, []Window{
		window(-5, -3, 3, 2),
		window(-2, 0, 2, 4.5),
		window(1, 3, 2, 6.5),
		window(4, 4, 1, 8),
	}, "negative-start tiling matches the detail")
}

// TestWindowsAndPointsRangesTable 表格驱动：多条同名不同标签序列、不同区间、
// 不同 step 与标签条件下，窗口统计都能从明细独立核对。刻意让各序列在各窗口
// 的点数与均值非平凡且互不相同，以区分区间过滤错、序列混算错与窗口归属错。
func TestWindowsAndPointsRangesTable(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", 0, 2, map[string]string{"host": "a"})+","+
		sample("m", 9, 4, map[string]string{"host": "a"})+","+
		sample("m", 10, 6, map[string]string{"host": "a"})+","+
		sample("m", 19, 8, map[string]string{"host": "a"})+","+
		sample("m", 20, 10, map[string]string{"host": "a"})+","+
		sample("m", 5, 20, map[string]string{"host": "b"})+","+
		sample("m", 25, 40, map[string]string{"host": "b"})+","+
		sample("m", 15, 7, map[string]string{"dc": "1", "host": "c"})+","+
		sample("other", 10, 999, nil)+"]")

	cases := []struct {
		name   string
		metric string
		start  int64
		end    int64
		step   int64
		labels map[string]string
	}{
		{"all series full range", "m", 0, 29, 10, nil},
		{"step 1 each point", "m", 0, 29, 1, nil},
		{"step wider than range", "m", 0, 29, 100, nil},
		{"boundary endpoints", "m", 9, 20, 10, nil},
		{"single interior ts empty", "m", 1, 1, 10, nil},
		{"host=a full", "m", 0, 29, 10, map[string]string{"host": "a"}},
		{"host=a [9,19]", "m", 9, 19, 10, map[string]string{"host": "a"}},
		{"host=b single window", "m", 0, 29, 10, map[string]string{"host": "b"}},
		{"host=b empty interior", "m", 10, 19, 10, map[string]string{"host": "b"}},
		{"host=c keeps extra label", "m", 0, 29, 10, map[string]string{"host": "c"}},
		{"subset with extra dc", "m", 0, 29, 10, map[string]string{"dc": "1", "host": "c"}},
		{"other metric exact name", "other", 0, 29, 10, nil},
		{"unknown metric", "nope", 0, 29, 10, map[string]string{}},
		{"start==end on a point", "m", 20, 20, 10, map[string]string{"host": "a"}},
		{"start==end empty", "m", 21, 21, 10, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertWindowsDetailCorrespond(t, store, c.metric, c.labels, c.start, c.end, c.step)
		})
	}
}

// TestWindowsAndPointsRandomizedDifferential 随机生成区间、步长与两条序列的
// 采样（含正负抵消、1e308 溢出对、最近偶取偶与负零等取值），每轮都执行两边
// 查询并由通用核对器从明细独立铺窗复核。时间戳在 start-2..end+2 内不重复
// 抽取，区间外点既不入窗也不影响计数。
func TestWindowsAndPointsRandomizedDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(20261007))
	values := []float64{1e16, 1, -1e16, 1e308, 1e308, 0.1, 1, 1.0000000000000002, 7, -5, 0, 2.5, -0.0, 5e-324}
	hosts := []map[string]string{{"host": "a"}, {"host": "b"}}

	for iter := 0; iter < 400; iter++ {
		start := int64(-1000 + rng.Intn(2001))
		end := start + int64(rng.Intn(41)) // 宽度 0..40
		step := int64(1 + rng.Intn(31))    // 1..30

		store := NewMetricStore()
		width := int(end-start) + 5 // start-2 .. end+2
		// 每条序列抽取互不重复的时间戳；两条序列允许在同一时间戳上各有点
		// （不同身份，不能合并）。
		for _, labels := range hosts {
			perm := rng.Perm(width)
			n := 1 + rng.Intn(width)
			var parts []string
			for i := 0; i < n; i++ {
				ts := start - 2 + int64(perm[i])
				v := values[rng.Intn(len(values))]
				parts = append(parts, sample("m", ts, v, labels))
			}
			mustOK(t, store, "["+strings.Join(parts, ",")+"]")
		}
		// 偶尔再加一个不同名指标，验证名称精确选择不受影响。
		mustOK(t, store, "["+sample("other", start, 3.5, nil)+"]")

		switch {
		case rng.Intn(3) == 0:
			assertWindowsDetailCorrespond(t, store, "m", map[string]string{"host": "a"}, start, end, step)
		case rng.Intn(2) == 0:
			assertWindowsDetailCorrespond(t, store, "m", map[string]string{}, start, end, step)
		default:
			assertWindowsDetailCorrespond(t, store, "m", nil, start, end, step)
		}
		assertWindowsDetailCorrespond(t, store, "other", nil, start, end, step)
	}
}
