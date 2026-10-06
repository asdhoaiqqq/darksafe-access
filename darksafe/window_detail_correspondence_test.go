package darksafe

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 本文件回归保障固定时间窗口统计与采样明细描述的是同一组采样：用户在同一份
// 已成功写入的数据上，用相同的指标名、标签条件和起止时间分别发送 op 为
// "query_windows"（额外给出合法 step）与 "query_points" 的查询，两种结果
// 必须能逐条核对：
//
//   - 两种结果的序列身份（指标名+完整标签集合）与排列次序完全一致；标签条件
//     只筛选序列，返回的记录仍携带完整标签集合，不缩减成查询条件；
//   - 同名但完整标签不同的序列各自统计，即使采样时间戳完全相同也不合并；
//   - 窗口从查询起点 start 起按 step 连续划分（第 k 个窗口为
//     [start+k*step, start+(k+1)*step-1]，最后一个截到 end），不跟随各序列
//     的第一个采样点移动；不同序列可以缺少不同窗口，只有实际有点的窗口出现，
//     按窗口起点升序；
//   - 对每个返回窗口，count 等于该序列明细中落在窗口闭区间内的点数，average
//     等于这些点实际存储值的算术平均——测试侧从 query_points 明细出发，用
//     big.Int/big.Rat 独立重算窗口归属、边界与均值（不经过生产代码的窗口
//     划分与舍入实现），保证统计侧与复核侧不共用同一份可能出错的逻辑；
//   - 整段明细中的每个点恰好计入一个窗口：既不遗漏，也不在相邻窗口重复计数；
//   - 没有区间内采样的序列在两边都不出现；完全未命中时两边都成功返回空数组。
//
// 失败信息一律指出查询区间、step、完整序列身份与出问题的窗口，说明是点数、
// 采样归属还是均值不符，便于定位后续修改造成的窗口统计偏差。

// windowPair 是除 op 与 step 外条件完全相同的一对查询文本：用户复核时只把
// op 从 query_points 换成 query_windows 并补上 step，其余条件（指标名、
// 标签条件、起止时间）逐字保持相同。
type windowPair struct {
	note    string // 区间/场景的人类可读说明
	windows string // op 为 "query_windows"，带 step
	points  string // op 为 "query_points"
	start   int64  // 用于失败信息指明是哪个区间，并独立重算窗口归属
	end     int64
	step    int64
}

// windowPairsFor 用同一组条件构造一对查询文本。labels 为 nil 时省略 labels
// 字段（匹配该指标全部序列）；非 nil 时显式带 labels（空 map 即 {}）。
func windowPairsFor(name string, start, end, step int64, labels map[string]string) windowPair {
	note := fmt.Sprintf("metric=%q range=[%d,%d] step=%d", name, start, end, step)
	if labels == nil {
		return windowPair{
			note:    note + " labels=<omit>",
			windows: queryWindowsAll(name, start, end, step),
			points:  queryPointsAll(name, start, end),
			start:   start,
			end:     end,
			step:    step,
		}
	}
	return windowPair{
		note:    note + " labels=" + jsonLabels(labels),
		windows: queryWindowsLabels(name, start, end, step, labels),
		points:  queryPointsLabels(name, start, end, labels),
		start:   start,
		end:     end,
		step:    step,
	}
}

// windowExpect 是从明细独立重算出的一个应出现的窗口：闭区间边界与落入其中的
// 明细点（按明细原有次序）。
type windowExpect struct {
	start  int64
	end    int64
	points []Point
}

// expectWindowsFromDetail 从 query_points 明细独立重算应出现的窗口列表：
// 每个点的窗口下标为 floor((ts-start)/step)，窗口边界为
// [start+idx*step, min(start+(idx+1)*step-1, end)]。全部用 big.Int 运算，
// 不调用生产代码的窗口划分（runQueryWindows/buildWindow/int64AtOffset），
// 因此即使起止时间接近 int64 上下界、step 接近 int64 上界，复核侧也不会
// 与统计侧共享同一个溢出或划分错误。points 必须全部落在 [start,end] 内
// （调用方已校验），因此下标非负，Quo 即向下取整。返回按窗口起点升序。
func expectWindowsFromDetail(t *testing.T, points []Point, start, end, step int64) []windowExpect {
	t.Helper()
	bigStart := big.NewInt(start)
	bigStep := big.NewInt(step)
	bigEnd := big.NewInt(end)
	one := big.NewInt(1)

	byIdx := make(map[string][]Point)
	seen := make(map[string]*big.Int)
	var idxs []*big.Int
	for _, p := range points {
		idx := new(big.Int).Sub(big.NewInt(p.Timestamp), bigStart)
		idx.Quo(idx, bigStep)
		key := idx.String()
		if _, ok := seen[key]; !ok {
			seen[key] = idx
			idxs = append(idxs, idx)
		}
		byIdx[key] = append(byIdx[key], p)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i].Cmp(idxs[j]) < 0 })

	out := make([]windowExpect, 0, len(idxs))
	for _, idx := range idxs {
		ws := new(big.Int).Add(bigStart, new(big.Int).Mul(idx, bigStep))
		we := new(big.Int).Sub(new(big.Int).Add(ws, bigStep), one)
		if we.Cmp(bigEnd) > 0 {
			we.Set(bigEnd)
		}
		if !ws.IsInt64() || !we.IsInt64() {
			t.Fatalf("internal: recomputed window bounds out of int64: [%s,%s]", ws, we)
		}
		out = append(out, windowExpect{
			start:  ws.Int64(),
			end:    we.Int64(),
			points: byIdx[idx.String()],
		})
	}
	return out
}

// windowsIdentitySigs 在序列数量不一致的失败信息中列出窗口结果全部序列身份。
func windowsIdentitySigs(xs []QueryWindowsSeries) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = identitySig(x.Name, x.Labels)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// assertWindowPointsCorrespond 对同一区间执行一对查询（query_windows 与
// query_points，其余条件相同），断言两种结果描述同一组采样：
//   - 双方都成功，status/op 正确；
//   - 序列身份（name+完整标签）与排列次序逐一相同，两侧标签集合相同；
//   - 明细点时间戳严格升序、两两不同，且都落在闭区间 [start,end] 内；
//   - 返回的窗口与从明细独立重算的窗口逐一相同：边界、count（窗口闭区间内
//     明细点数）、average（这些点实际存储值的精确平均，最近偶 float64）；
//   - 窗口按起点升序、不含空窗口；每个明细点恰好计入一个窗口（计数总和
//     等于明细点数，既不遗漏也不在相邻窗口重复）；
//   - 两侧都不出现没有点的空记录；空结果是成功的非 nil 空数组。
//
// 每个失败信息都带查询区间、step 与序列身份签名，窗口级失败再指出具体窗口。
func assertWindowPointsCorrespond(t *testing.T, store *MetricStore, p windowPair) {
	t.Helper()
	win := mustQueryWindows(t, store, p.windows)
	det := mustQueryPoints(t, store, p.points)
	if win.Status != "ok" || win.Op != "query_windows" {
		t.Fatalf("[%d,%d] step=%d %s: windows envelope = %q/%q, want ok/query_windows",
			p.start, p.end, p.step, p.note, win.Status, win.Op)
	}
	if det.Status != "ok" || det.Op != "query_points" {
		t.Fatalf("[%d,%d] step=%d %s: detail envelope = %q/%q, want ok/query_points",
			p.start, p.end, p.step, p.note, det.Status, det.Op)
	}

	// 序列数量、身份与排列次序逐一相同。
	if len(win.Series) != len(det.Series) {
		t.Fatalf("[%d,%d] step=%d %s: query_windows returned %d series but query_points returned %d:\nwindows: %s\ndetail:  %s",
			p.start, p.end, p.step, p.note, len(win.Series), len(det.Series),
			windowsIdentitySigs(win.Series), detailIdentitySigs(det.Series))
	}
	for i := range win.Series {
		sigW := identitySig(win.Series[i].Name, win.Series[i].Labels)
		sigD := identitySig(det.Series[i].Name, det.Series[i].Labels)
		if sigW != sigD {
			t.Fatalf("[%d,%d] step=%d %s: series[%d] identity/order mismatch:\nquery_windows #%d = %s\nquery_points  #%d = %s",
				p.start, p.end, p.step, p.note, i, i, sigW, i, sigD)
		}
	}

	// 逐条序列核对窗口统计与明细的对应关系。
	for i := range win.Series {
		ws := win.Series[i]
		ds := det.Series[i]
		sig := identitySig(ws.Name, ws.Labels)

		// 两种结果携带的标签集合必须相同（且为完整集合，由各场景另行核对）。
		if !sameLabels(ws.Labels, ds.Labels) {
			t.Fatalf("[%d,%d] step=%d %s: %s: labels differ between results: windows=%v detail=%v",
				p.start, p.end, p.step, p.note, sig, ws.Labels, ds.Labels)
		}
		// 没有点的序列不能被任一侧补成空记录。
		if len(ds.Points) == 0 {
			t.Fatalf("[%d,%d] step=%d %s: %s: zero-point entry must not be emitted by either query",
				p.start, p.end, p.step, p.note, sig)
		}

		// 明细按时间戳严格升序，且每个点都在闭区间内。
		for j, pt := range ds.Points {
			if pt.Timestamp < p.start || pt.Timestamp > p.end {
				t.Fatalf("[%d,%d] step=%d %s: %s: detail point[%d] ts=%d is outside the closed range",
					p.start, p.end, p.step, p.note, sig, j, pt.Timestamp)
			}
			if j > 0 && ds.Points[j-1].Timestamp >= pt.Timestamp {
				t.Fatalf("[%d,%d] step=%d %s: %s: detail not strictly ascending at %d: %+v",
					p.start, p.end, p.step, p.note, sig, j, ds.Points)
			}
		}

		// 从明细独立重算应出现的窗口，与返回窗口逐一比对。
		want := expectWindowsFromDetail(t, ds.Points, p.start, p.end, p.step)
		got := ws.Windows
		if len(got) != len(want) {
			t.Fatalf("[%d,%d] step=%d %s: %s: returned %d windows %+v, but the detail implies %d non-empty windows %+v",
				p.start, p.end, p.step, p.note, sig, len(got), got, len(want), want)
		}
		total := 0
		for k := range got {
			g := got[k]
			w := want[k]
			wsig := fmt.Sprintf("window [%d,%d]", w.start, w.end)
			if k > 0 && got[k-1].Start >= g.Start {
				t.Fatalf("[%d,%d] step=%d %s: %s: windows not ascending by start at %d: %+v",
					p.start, p.end, p.step, p.note, sig, k, got)
			}
			if g.Start != w.start || g.End != w.end {
				t.Fatalf("[%d,%d] step=%d %s: %s: %s: returned bounds [%d,%d], window tiling from the query start is off",
					p.start, p.end, p.step, p.note, sig, wsig, g.Start, g.End)
			}
			if g.Count != len(w.points) {
				t.Fatalf("[%d,%d] step=%d %s: %s: %s: count=%d but %d detail points fall inside: %+v",
					p.start, p.end, p.step, p.note, sig, wsig, g.Count, len(w.points), w.points)
			}
			if g.Count == 0 {
				t.Fatalf("[%d,%d] step=%d %s: %s: %s: empty window must not be emitted",
					p.start, p.end, p.step, p.note, sig, wsig)
			}
			wantAvg := detailMean(t, w.points)
			if g.Average != wantAvg {
				t.Fatalf("[%d,%d] step=%d %s: %s: %s: average=%v but recomputed from the detail points in this window = %v (points=%+v)",
					p.start, p.end, p.step, p.note, sig, wsig, g.Average, wantAvg, w.points)
			}
			total += g.Count
		}
		// 划分性：每个明细点恰好计入一个窗口——计数总和等于明细点数，
		// 既不遗漏，也不在相邻窗口重复计数。
		if total != len(ds.Points) {
			t.Fatalf("[%d,%d] step=%d %s: %s: window counts sum to %d but the detail lists %d points; every point must be counted in exactly one window",
				p.start, p.end, p.step, p.note, sig, total, len(ds.Points))
		}
	}
}

// TestWindowsPointsSpecBoundariesCorrespond 任务书指定的窗口划分：查询
// [1000,3000]、step 1000 时边界是 [1000,1999]、[2000,2999]、[3000,3000]——
// 1999 与 2000 上的点分属相邻窗口，3000 上的点归入最后一个窗口。逐窗口与
// query_points 明细核对点数与均值。
func TestWindowsPointsSpecBoundariesCorrespond(t *testing.T) {
	store := NewMetricStore()
	labelsA := map[string]string{"host": "a"}
	mustOK(t, store, "["+
		sample("cpu", 500, 77, labelsA)+","+ // 区间下方之外
		sample("cpu", 1000, 2, labelsA)+","+
		sample("cpu", 1999, 4, labelsA)+","+ // 第一个窗口的最后一毫秒
		sample("cpu", 2000, 6, labelsA)+","+ // 第二个窗口的第一毫秒
		sample("cpu", 2999, 8, labelsA)+","+ // 第二个窗口的最后一毫秒
		sample("cpu", 3000, 10, labelsA)+","+ // 收尾窗口 [3000,3000]
		sample("cpu", 4000, 88, labelsA)+"]") // 区间上方之外

	pair := windowPairsFor("cpu", 1000, 3000, 1000, labelsA)
	assertWindowPointsCorrespond(t, store, pair)

	// 明细侧：恰好区间内 5 个点，按时间升序，区间外的 500/4000 不出现。
	det := mustQueryPoints(t, store, pair.points)
	if len(det.Series) != 1 {
		t.Fatalf("[1000,3000] host=a: detail series = %s, want exactly 1",
			detailIdentitySigs(det.Series))
	}
	wantPoints := []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 1999, Value: 4},
		{Timestamp: 2000, Value: 6},
		{Timestamp: 2999, Value: 8},
		{Timestamp: 3000, Value: 10},
	}
	if !reflect.DeepEqual(det.Series[0].Points, wantPoints) {
		t.Fatalf("[1000,3000] %s: detail points = %+v, want %+v",
			identitySig("cpu", labelsA), det.Series[0].Points, wantPoints)
	}

	// 窗口侧：三个窗口的边界、点数、均值与明细逐条对应。
	win := mustQueryWindows(t, store, pair.windows)
	if len(win.Series) != 1 {
		t.Fatalf("[1000,3000] host=a: windows series = %s, want exactly 1",
			windowsIdentitySigs(win.Series))
	}
	assertWindows(t, win.Series[0].Windows, []Window{
		window(1000, 1999, 2, 3), // 1000 与 1999
		window(2000, 2999, 2, 7), // 2000 与 2999
		window(3000, 3000, 1, 10),
	}, "spec tiling [1000,3000] step 1000")
}

// TestWindowsPointsAnchoredAtQueryStart 窗口从查询起点划分，不跟随各序列的
// 第一个采样点移动：同一组采样在不同查询起点下入窗不同；序列第一个采样点
// 之前可以有空窗口（不补零），窗口起点始终是 start+k*step。
func TestWindowsPointsAnchoredAtQueryStart(t *testing.T) {
	store := NewMetricStore()
	// 第一个采样点在 1500，刻意不在任何窗口边界上。
	mustOK(t, store, "["+
		sample("m", 1500, 2, nil)+","+
		sample("m", 1600, 4, nil)+","+
		sample("m", 2500, 6, nil)+"]")

	// 查询 [1000,3000]、step 1000：窗口从 1000 起划分，不是从 1500 起；
	// 1500/1600 同入 [1000,1999]，2500 入 [2000,2999]。
	pair := windowPairsFor("m", 1000, 3000, 1000, nil)
	assertWindowPointsCorrespond(t, store, pair)
	win := mustQueryWindows(t, store, pair.windows)
	assertWindows(t, win.Series[0].Windows, []Window{
		window(1000, 1999, 2, 3),
		window(2000, 2999, 1, 6),
	}, "windows anchored at query start 1000, not at first sample 1500")

	// 同一组采样、查询起点改为 1200：边界整体平移，1500 入 [1200,2199]，
	// 1600 仍与 1500 同窗，2500 入 [2200,3000]（截到 end）。
	pair2 := windowPairsFor("m", 1200, 3000, 1000, nil)
	assertWindowPointsCorrespond(t, store, pair2)
	win2 := mustQueryWindows(t, store, pair2.windows)
	assertWindows(t, win2.Series[0].Windows, []Window{
		window(1200, 2199, 2, 3),
		window(2200, 3000, 1, 6),
	}, "same samples re-tiled when the query start moves to 1200")

	// 起点对齐到采样点本身：1500 成为窗口起点。
	pair3 := windowPairsFor("m", 1500, 3000, 1000, nil)
	assertWindowPointsCorrespond(t, store, pair3)
	win3 := mustQueryWindows(t, store, pair3.windows)
	assertWindows(t, win3.Series[0].Windows, []Window{
		window(1500, 2499, 2, 3),
		window(2500, 3000, 1, 6),
	}, "start aligned with the first sample")
}

// TestWindowsPointsMultiSeriesIndependent 同名但完整标签不同的序列各自统计：
// 即使采样时间戳完全相同也不合并；标签条件只筛选序列，结果仍保留完整标签
// 集合；两种结果的序列次序一致；不同序列可以缺少不同窗口。
func TestWindowsPointsMultiSeriesIndependent(t *testing.T) {
	store := NewMetricStore()
	labelsA := map[string]string{"host": "a"}
	labelsB := map[string]string{"host": "b"}
	labelsCX := map[string]string{"dc": "1", "host": "c"}
	mustOK(t, store, "["+
		// host=a 与 host=b 在完全相同的时间戳上采样，值刻意不同：
		// 一旦合并统计，两条序列的 count/average 都会变。
		sample("m", 10, 2, labelsA)+","+
		sample("m", 20, 4, labelsA)+","+
		sample("m", 10, 200, labelsB)+","+
		sample("m", 20, 400, labelsB)+","+
		sample("m", 35, 7, labelsCX)+","+ // 只在窗口 [30,39] 有点
		sample("m", 45, 9, labelsB)+"]") // host=b 独有的窗口 [40,49]

	// 省略 labels：三条序列都返回，次序两侧一致。规范次序按排序后标签键值对
	// 逐对比较：{dc:1,host:c} 的首对键 dc 排在 host 之前，因此 host=c 在最前。
	pair := windowPairsFor("m", 0, 49, 10, nil)
	assertWindowPointsCorrespond(t, store, pair)
	win := mustQueryWindows(t, store, pair.windows)
	det := mustQueryPoints(t, store, pair.points)
	if len(win.Series) != 3 || len(det.Series) != 3 {
		t.Fatalf("[0,49] all-labels: series count windows=%d detail=%d, want 3",
			len(win.Series), len(det.Series))
	}
	wantOrder := []map[string]string{labelsCX, labelsA, labelsB}
	for i, want := range wantOrder {
		if !sameLabels(win.Series[i].Labels, want) || !sameLabels(det.Series[i].Labels, want) {
			t.Fatalf("[0,49] series[%d] identity = %v / %v, want full set %v",
				i, win.Series[i].Labels, det.Series[i].Labels, want)
		}
	}
	// host=c：只在自己有点的窗口出现，完整标签集合（含 dc）保留。
	assertWindows(t, win.Series[0].Windows, []Window{
		window(30, 39, 1, 7),
	}, "host=c appears only in its own window")
	// host=a：两个窗口各一点，统计不受同时间戳的 host=b 影响。
	assertWindows(t, win.Series[1].Windows, []Window{
		window(10, 19, 1, 2),
		window(20, 29, 1, 4),
	}, "host=a counted separately despite identical timestamps")
	// host=b：三个窗口（独有 [40,49]），均值是自己的大值。
	assertWindows(t, win.Series[2].Windows, []Window{
		window(10, 19, 1, 200),
		window(20, 29, 1, 400),
		window(40, 49, 1, 9),
	}, "host=b has its own windows, including the one host=a lacks")

	// 标签子集 host=c：只筛选序列，结果仍带完整标签 {"dc":"1","host":"c"}。
	sub := windowPairsFor("m", 0, 49, 10, map[string]string{"host": "c"})
	assertWindowPointsCorrespond(t, store, sub)
	subWin := mustQueryWindows(t, store, sub.windows)
	if len(subWin.Series) != 1 || !sameLabels(subWin.Series[0].Labels, labelsCX) {
		t.Fatalf("subset host=c = %+v, want the single series with its full label set %v",
			subWin.Series, labelsCX)
	}
}

// TestWindowsPointsCancellationWithinOneWindow 同一窗口内的 1e16、1、-1e16
// 保留抵消后的余量：均值为 0.3333333333333333；其他窗口中的点不能影响它——
// 切换窗口后点数与均值都只反映新窗口自己的采样。
func TestWindowsPointsCancellationWithinOneWindow(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("cpu", 1000, 1e16, nil)+","+
		sample("cpu", 1500, 1, nil)+","+
		sample("cpu", 1999, -1e16, nil)+","+
		// 相邻窗口的点：若泄漏进上一窗口，点数与均值都会变。
		sample("cpu", 2000, 1e308, nil)+","+
		sample("cpu", 2500, 1e308, nil)+"]")

	pair := windowPairsFor("cpu", 1000, 2999, 1000, nil)
	assertWindowPointsCorrespond(t, store, pair)
	win := mustQueryWindows(t, store, pair.windows)
	if len(win.Series) != 1 || len(win.Series[0].Windows) != 2 {
		t.Fatalf("windows = %+v, want one series with two windows", win.Series)
	}
	w0 := win.Series[0].Windows[0]
	if w0.Count != 3 || w0.Average != 1.0/3.0 {
		t.Fatalf("[1000,2999] step=1000 %s: window [1000,1999] = %+v, want count=3 average=1/3; the middle 1 must survive cancellation",
			identitySig("cpu", nil), w0)
	}
	b, _ := json.Marshal(w0.Average)
	if string(b) != "0.3333333333333333" {
		t.Fatalf("[1000,2999] step=1000 %s: window [1000,1999] average JSON = %s, want 0.3333333333333333",
			identitySig("cpu", nil), b)
	}
	// 第二个窗口只反映自己的两个 1e308：总和超出 float64 范围，均值仍有限。
	w1 := win.Series[0].Windows[1]
	if w1.Start != 2000 || w1.End != 2999 || w1.Count != 2 || w1.Average != 1e308 {
		t.Fatalf("[1000,2999] step=1000 %s: window [2000,2999] = %+v, want count=2 average=1e308 (only its own samples)",
			identitySig("cpu", nil), w1)
	}

	// 改一步长使三点分属不同窗口：各自的均值就是各自的存储值，互不干扰。
	pair2 := windowPairsFor("cpu", 1000, 2999, 500, nil)
	assertWindowPointsCorrespond(t, store, pair2)
	win2 := mustQueryWindows(t, store, pair2.windows)
	assertWindows(t, win2.Series[0].Windows, []Window{
		window(1000, 1499, 1, 1e16),
		window(1500, 1999, 2, -5e15), // (1 + -1e16)/2 精确平均
		window(2000, 2499, 1, 1e308),
		window(2500, 2999, 1, 1e308),
	}, "re-tiling isolates the cancellation points into their own windows")
}

// TestWindowsPointsNoMatchBothEmpty 没有区间内采样的序列在两边都不出现；
// 完全未命中时两种查询都成功返回空 series 数组（非 nil，序列化为 []）。
func TestWindowsPointsNoMatchBothEmpty(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("cpu", 1000, 2, map[string]string{"host": "a"})+","+
		sample("cpu", 2000, 4, map[string]string{"host": "a"})+","+
		sample("cpu", 2000, 5, map[string]string{"host": "b"})+"]")

	cases := []windowPair{
		windowPairsFor("nope", 0, 5000, 1000, nil),                              // 指标不存在（省略 labels）
		windowPairsFor("nope", 0, 5000, 1000, map[string]string{}),              // 指标不存在（显式 {}）
		windowPairsFor("cpu", 0, 5000, 1000, map[string]string{"host": "zzz"}),  // 标签值不匹配
		windowPairsFor("cpu", 0, 5000, 1000, map[string]string{"zone": "x"}),    // 缺少该标签键
		windowPairsFor("cpu", 1001, 1999, 100, nil),                             // 区间内没有点
		windowPairsFor("cpu", 2001, 9000, 1000, map[string]string{"host": "a"}), // 序列存在但区间在点之后
		windowPairsFor("cpu", 1000, 1000, 1, map[string]string{"host": "b"}),    // 标签命中但该时刻无点
		windowPairsFor("cpu", 1500, 1500, 1, nil),                               // start==end 落在空位置
	}
	for _, p := range cases {
		t.Run(p.note, func(t *testing.T) {
			assertWindowPointsCorrespond(t, store, p)
			win := mustQueryWindows(t, store, p.windows)
			det := mustQueryPoints(t, store, p.points)
			if win.Series == nil || len(win.Series) != 0 {
				t.Fatalf("windows series = %+v, want non-nil empty list", win.Series)
			}
			if det.Series == nil || len(det.Series) != 0 {
				t.Fatalf("detail series = %+v, want non-nil empty list", det.Series)
			}
			if raw := marshalCompact(t, win); raw != `{"status":"ok","op":"query_windows","series":[]}` {
				t.Fatalf("windows JSON = %s, want empty series []", raw)
			}
			if raw := marshalCompact(t, det); raw != `{"status":"ok","op":"query_points","series":[]}` {
				t.Fatalf("detail JSON = %s, want empty series []", raw)
			}
		})
	}
}

// TestWindowsPointsPartitionTable 表格驱动覆盖多种区间与步长组合：每个明细点
// 恰好计入一个窗口（划分性由通用核对里的计数总和保证），窗口边界始终从查询
// 起点平铺。包含负起点、start==end、step 大于区间宽度、step 为 1 等情形，
// 以及多条序列在同一组边界下各自缺少不同窗口的场景。
func TestWindowsPointsPartitionTable(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", -7, 1, map[string]string{"host": "a"})+","+
		sample("m", -3, 3, map[string]string{"host": "a"})+","+
		sample("m", 0, 5, map[string]string{"host": "a"})+","+
		sample("m", 4, 7, map[string]string{"host": "a"})+","+
		sample("m", 9, 9, map[string]string{"host": "a"})+","+
		sample("m", 12, 11, map[string]string{"host": "a"})+","+
		sample("m", -3, 30, map[string]string{"host": "b"})+","+ // 与 host=a 同时间戳
		sample("m", 8, 80, map[string]string{"host": "b"})+","+
		sample("other", 0, 999, nil)+"]")

	cases := []struct {
		name   string
		metric string
		start  int64
		end    int64
		step   int64
		labels map[string]string // nil = 省略 labels
	}{
		{"all labels full range step 5", "m", -10, 14, 5, nil},
		{"all labels step 1", "m", -7, 12, 1, nil},
		{"all labels step wider than range", "m", -10, 14, 1000, nil},
		{"host=a negative start step 3", "m", -8, 12, 3, map[string]string{"host": "a"}},
		{"host=a start==end on a sample", "m", 0, 0, 7, map[string]string{"host": "a"}},
		{"host=a start==end off samples", "m", 1, 1, 7, map[string]string{"host": "a"}},
		{"host=b shared tiling", "m", -10, 14, 5, map[string]string{"host": "b"}},
		{"host=a partial interior", "m", -3, 9, 4, map[string]string{"host": "a"}},
		{"other metric exact name", "other", -5, 5, 2, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertWindowPointsCorrespond(t, store,
				windowPairsFor(c.metric, c.start, c.end, c.step, c.labels))
		})
	}

	// 点名核验一组非平凡窗口：[-10,14]、step 5 时边界为
	// [-10,-6]、[-5,-1]、[0,4]、[5,9]、[10,14]；host=a 的点 -7/-3/0/4/9/12
	// 依次落入窗口 0/1/2/2/3/4。
	win := mustQueryWindows(t, store, queryWindowsLabels("m", -10, 14, 5, map[string]string{"host": "a"}))
	assertWindows(t, win.Series[0].Windows, []Window{
		window(-10, -6, 1, 1),
		window(-5, -1, 1, 3),
		window(0, 4, 2, 6),
		window(5, 9, 1, 9),
		window(10, 14, 1, 11),
	}, "host=a tiling over a negative start")
}

// TestWindowsPointsInt64ExtremesCorrespond 起止时间与 step 接近 int64 上下界
// 时，窗口统计仍与明细逐条对应：复核侧用 big.Int 独立重算归属与边界，
// 不与统计侧共用任何溢出风险路径。
func TestWindowsPointsInt64ExtremesCorrespond(t *testing.T) {
	min := tsMinInt64
	max := tsMaxInt64
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("m", min, 3, nil)+","+
		sample("m", -2, 9, nil)+","+
		sample("m", -1, 1, nil)+","+
		sample("m", 0, 7, nil)+","+
		sample("m", max, 5, nil)+"]")

	cases := []windowPair{
		// 整个 int64 区间、step 为 MaxInt64：三个窗口，最后一个截到 end。
		windowPairsFor("m", min, max, max, nil),
		// step 为 1：每个时间戳独立窗口。
		windowPairsFor("m", min, max, 1, nil),
		// 负半区、巨大 step。
		windowPairsFor("m", min, -1, max, nil),
		// 跨零小区间、小步长。
		windowPairsFor("m", -2, 1, 1, nil),
		// start==end 在 int64 下界。
		windowPairsFor("m", min, min, max, nil),
		// start==end 在 int64 上界。
		windowPairsFor("m", max, max, max, nil),
	}
	for _, p := range cases {
		t.Run(p.note, func(t *testing.T) {
			assertWindowPointsCorrespond(t, store, p)
		})
	}

	// 点名核验极端组合的窗口边界：step=MaxInt64 时窗口为
	// [MinInt64,-2]、[-1,MaxInt64-2]、[MaxInt64-1,MaxInt64]。
	win := mustQueryWindows(t, store, queryWindowsAll("m", min, max, max))
	assertWindows(t, win.Series[0].Windows, []Window{
		window(min, -2, 2, 6),
		window(-1, max-2, 2, 4), // -1 上的 1 与 0 上的 7
		window(max-1, max, 1, 5),
	}, "full int64 range tiled with step MaxInt64")
}
