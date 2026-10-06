package main

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 本文件从命令行入口（一次 ingest 进程内）回归保障用户的跨操作复核流程：在同
// 一份已成功写入的数据上，用相同的指标名、标签条件与起止时间分别发送
// query_windows（额外带合法 step）与 query_points 两行对象行，相邻两条输出
// 必须描述同一组采样，窗口统计能与采样明细逐条核对：
//
//   - 两行 series 的序列身份（指标名+完整标签集合）与排列次序逐一相同，
//     标签条件只筛选序列、结果保留完整标签；同名不同完整标签的序列即使采样
//     时间戳相同也各自统计；没有区间内点的序列两行都不出现；
//   - 复核侧独立按查询起点用浮点精确的整数运算铺窗口（本文件时间戳均远小于
//     2^53，JSON 往返后仍精确），不跟随任一序列的第一个采样点；
//   - 每个窗口的 count 等于明细中落在窗口闭区间内的点数，average 等于这些
//     JSON 往返后实际存储 float64 值的精确算术平均（复用
//     cpDetailMean 的 big.Rat 独立复算）；
//   - 明细中的每个点恰好计入一个窗口：1999 与 2000 上的点分属相邻窗口，
//     end 上的点归入最后一个窗口，不漏不重；只有实际有点的窗口出现并按
//     起点升序，空窗口不补零；
//   - 同一窗口内 1e16、1、-1e16 抵消后的余量保留（均值 JSON 为
//     0.3333333333333333），其他窗口的点不影响它；
//   - 完全未命中时两行都成功输出 "series":[]，不补零窗口或零记录。

// e2eWindow 是复核侧独立铺出的一个非空参考窗口。
type e2eWindow struct {
	index  float64
	start  float64
	end    float64
	points []interface{}
}

// referenceWindowsFromOutputDetail 从 query_points 输出行中的一条明细序列独立
// 铺窗口并分组：k=floor((ts-start)/step)，窗口为
// [start+k*step, min(start+(k+1)*step-1, end)]。只用于远小于 2^53 的时间戳，
// 保证 JSON 往返为 float64 后整数运算仍然精确。
func referenceWindowsFromOutputDetail(t *testing.T, detailPoints []interface{}, start, end, step float64, note string) []e2eWindow {
	t.Helper()
	groups := map[float64][]interface{}{}
	indexes := make([]float64, 0)
	for _, e := range detailPoints {
		pm := e.(map[string]interface{})
		ts := pm["timestamp"].(float64)
		if ts < start || ts > end {
			t.Fatalf("%s: detail point ts=%v is outside the closed range", note, ts)
		}
		k := math.Floor((ts - start) / step)
		if _, seen := groups[k]; !seen {
			indexes = append(indexes, k)
		}
		groups[k] = append(groups[k], e)
	}
	sort.Float64s(indexes)
	out := make([]e2eWindow, 0, len(indexes))
	for _, k := range indexes {
		ws := start + k*step
		we := ws + step - 1
		if we > end {
			we = end
		}
		out = append(out, e2eWindow{index: k, start: ws, end: we, points: groups[k]})
	}
	return out
}

// assertOutputWindowsPointsCorrespond 核对相邻的 query_windows / query_points
// 两条成功输出行逐序列、逐窗口对应。start/end/step 用于失败信息与独立铺窗。
func assertOutputWindowsPointsCorrespond(t *testing.T, windowsLine, pointsLine string, start, end, step float64) {
	t.Helper()
	wm := decodeResultLine(t, windowsLine)
	dm := decodeResultLine(t, pointsLine)
	if wm["status"] != "ok" || wm["op"] != "query_windows" {
		t.Fatalf("[%.0f,%.0f] step %.0f: windows line = %v, want ok/query_windows", start, end, step, wm)
	}
	if dm["status"] != "ok" || dm["op"] != "query_points" {
		t.Fatalf("[%.0f,%.0f] step %.0f: points line = %v, want ok/query_points", start, end, step, dm)
	}
	wSeries := wm["series"].([]interface{})
	pSeries := dm["series"].([]interface{})
	note := fmt.Sprintf("range [%s,%s] step %s", formatE2E(start), formatE2E(end), formatE2E(step))
	if len(wSeries) != len(pSeries) {
		t.Fatalf("%s: series count: query_windows=%d (%v), query_points=%d (%v)",
			note, len(wSeries), cpSigs(wSeries), len(pSeries), cpSigs(pSeries))
	}
	// 身份与排列次序逐一相同。
	for i := range wSeries {
		we := wSeries[i].(map[string]interface{})
		pe := pSeries[i].(map[string]interface{})
		sigW := cpSig(we["name"].(string), we["labels"].(map[string]interface{}))
		sigP := cpSig(pe["name"].(string), pe["labels"].(map[string]interface{}))
		if sigW != sigP {
			t.Fatalf("%s: series[%d] identity/order mismatch: windows=%s points=%s",
				note, i, sigW, sigP)
		}
	}

	for i := range wSeries {
		we := wSeries[i].(map[string]interface{})
		pe := pSeries[i].(map[string]interface{})
		sig := cpSig(we["name"].(string), we["labels"].(map[string]interface{}))
		detailPoints := pe["points"].([]interface{})

		// 明细时间戳严格升序。
		var prev float64
		for j, e := range detailPoints {
			ts := e.(map[string]interface{})["timestamp"].(float64)
			if j > 0 && prev >= ts {
				t.Fatalf("%s: %s: detail points not strictly ascending at %d", note, sig, j)
			}
			prev = ts
		}

		refs := referenceWindowsFromOutputDetail(t, detailPoints, start, end, step, note+" "+sig)
		gotWindows := we["windows"].([]interface{})
		if len(gotWindows) != len(refs) {
			t.Fatalf("%s: %s: query_windows has %d windows but the detail implies %d non-empty windows: %s",
				note, sig, len(gotWindows), len(refs), windowsLine)
		}

		assigned := 0
		var prevEnd float64
		for j, ref := range refs {
			gw := gotWindows[j].(map[string]interface{})
			wStart := gw["start"].(float64)
			wEnd := gw["end"].(float64)
			count := int(gw["count"].(float64))
			average := gw["average"].(float64)
			winNote := fmt.Sprintf("%s: %s: window[%d] [%s,%s]",
				note, sig, j, formatE2E(ref.start), formatE2E(ref.end))
			if j > 0 && wStart <= prevEnd {
				t.Fatalf("%s: windows not in ascending non-overlapping order (previous end %v)",
					winNote, prevEnd)
			}
			// 窗口边界必须从查询起点铺出（不跟随首点移动），且不越界、不倒置。
			if wStart != ref.start || wEnd != ref.end {
				t.Fatalf("%s: bounds = [%v,%v], want detail-derived [%v,%v]",
					winNote, wStart, wEnd, ref.start, ref.end)
			}
			if wStart > wEnd || wStart < start || wEnd > end {
				t.Fatalf("%s: window is inverted or out of range: %+v", winNote, gw)
			}
			// 采样归属：闭区间 [wStart,wEnd] 命中的明细点必须恰好是该参考分组。
			var inBounds []interface{}
			for _, e := range detailPoints {
				ts := e.(map[string]interface{})["timestamp"].(float64)
				if ts >= wStart && ts <= wEnd {
					inBounds = append(inBounds, e)
				}
			}
			if len(inBounds) != len(ref.points) || count != len(ref.points) {
				t.Fatalf("%s: count = %d, %d points are assigned by floor index but %d detail points fall in the window (points=%v)",
					winNote, count, len(ref.points), len(inBounds), detailPoints)
			}
			if count == 0 {
				t.Fatalf("%s: an empty window must not be emitted", winNote)
			}
			// 均值由该窗口自己的明细点独立复算（其他窗口不得影响）。
			wantAvg := cpDetailMean(t, ref.points)
			if average != wantAvg {
				t.Fatalf("%s: average=%v but recomputed from this window's detail = %v (points=%v)",
					winNote, average, wantAvg, ref.points)
			}
			assigned += count
			prevEnd = wEnd
		}
		// 每个点恰好计入一个窗口。
		if assigned != len(detailPoints) {
			t.Fatalf("%s: %s: window counts sum to %d but the detail has %d points (missing or double-counted): %s",
				note, sig, assigned, len(detailPoints), windowsLine)
		}
	}
}

// formatE2E 把区间/窗口坐标格式化为不带小数尾巴的文本，用于失败信息。
func formatE2E(x float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(x, 'f', -1, 64), "0"), ".")
}

// TestRunIngestWindowsAndPointsCorrespondEndToEnd 是命令行端到端核心复核：
// 在同一次 ingest 中写入数据后，成对发送仅 op 不同（windows 多带 step）的
// 查询行，逐对核对身份/次序、每窗口 count/average 与明细的逐条对应，覆盖
// 任务书指定的窗口划分、1999/2000 边界归属、同时间戳不同标签不合并、
// 1e16 抵消余量、每窗口独立统计、空区间两行皆空。
func TestRunIngestWindowsAndPointsCorrespondEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：一次写入全部数据。
		// host=a：窗口 [1000,1999] 三点 1e16/1/-1e16（均值 1/3），
		// 窗口 [2000,2999] 两点 10/20（均值 15），窗口 [3000,3000] 一点 9。
		`[{"name":"cpu","timestamp":1000,"value":10000000000000000,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1500,"value":1,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1900,"value":-10000000000000000,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2000,"value":10,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2500,"value":20,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":3000,"value":9,"labels":{"host":"a"}},` +
			// host=b：与 host=a 在相同时间戳上各有点，值不同，绝不合并。
			`{"name":"cpu","timestamp":1000,"value":100,"labels":{"host":"b"}},` +
			`{"name":"cpu","timestamp":1500,"value":200,"labels":{"host":"b"}},` +
			`{"name":"cpu","timestamp":3000,"value":300,"labels":{"host":"b"}},` +
			// host=e：1999 与 2000 上的点分属相邻窗口。
			`{"name":"cpu","timestamp":1999,"value":5,"labels":{"host":"e"}},` +
			`{"name":"cpu","timestamp":2000,"value":7,"labels":{"host":"e"}},` +
			// host=out：点全部落在查询区间外，两边都不出现。
			`{"name":"cpu","timestamp":500,"value":1,"labels":{"host":"out"}},` +
			// m：子集 zone=x 命中两条，其中一条带额外 host 标签。
			`{"name":"m","timestamp":10,"value":100,"labels":{"zone":"x"}},` +
			`{"name":"m","timestamp":30,"value":200,"labels":{"zone":"x","host":"h"}}]`,
		// 行 2/3：省略 labels，[1000,3000] step 1000。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`,
		`{"op":"query_points","name":"cpu","start":1000,"end":3000}`,
		// 行 4/5：标签子集 host=a。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		`{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
		// 行 6/7：标签子集 host=e，锁定 1999/2000 边界归属。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"e"}}`,
		`{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"e"}}`,
		// 行 8/9：m 的 zone=x 子集，额外标签完整保留。
		`{"op":"query_windows","name":"m","start":0,"end":10000,"step":25,"labels":{"zone":"x"}}`,
		`{"op":"query_points","name":"m","start":0,"end":10000,"labels":{"zone":"x"}}`,
		// 行 10/11：start==end 且 3000 上有点（host=a、host=b 各一点）。
		`{"op":"query_windows","name":"cpu","start":3000,"end":3000,"step":1000}`,
		`{"op":"query_points","name":"cpu","start":3000,"end":3000}`,
		// 行 12/13：区间内没有点，两行都成功输出空数组。
		`{"op":"query_windows","name":"cpu","start":3001,"end":9000,"step":1000}`,
		`{"op":"query_points","name":"cpu","start":3001,"end":9000}`,
	}, "\n")

	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("all lines succeed, exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 13 {
		t.Fatalf("got %d output lines, want 13: %v", len(lines), lines)
	}

	// 行 2/3：省略 labels 全序列复核。
	assertOutputWindowsPointsCorrespond(t, lines[1], lines[2], 1000, 3000, 1000)
	allW := decodeResultLine(t, lines[1])["series"].([]interface{})
	allP := decodeResultLine(t, lines[2])["series"].([]interface{})
	if len(allW) != 3 || len(allP) != 3 {
		t.Fatalf("all cpu series: want a,b,e three entries both sides, got %d/%d", len(allW), len(allP))
	}
	// 期望次序：host=a、host=b、host=e（字典序），host=out 不出现。
	wantSigs := []string{
		cpSig("cpu", map[string]interface{}{"host": "a"}),
		cpSig("cpu", map[string]interface{}{"host": "b"}),
		cpSig("cpu", map[string]interface{}{"host": "e"}),
	}
	if got := cpSigs(allW); !reflect.DeepEqual(got, wantSigs) {
		t.Fatalf("windows series order = %v, want %v", got, wantSigs)
	}
	if got := cpSigs(allP); !reflect.DeepEqual(got, wantSigs) {
		t.Fatalf("points series order = %v, want %v", got, wantSigs)
	}

	bySig := func(series []interface{}) map[string]map[string]interface{} {
		m := make(map[string]map[string]interface{})
		for _, e := range series {
			em := e.(map[string]interface{})
			m[cpSig(em["name"].(string), em["labels"].(map[string]interface{}))] = em
		}
		return m
	}(allW)
	// host=a：三个窗口，第一窗口 3 点均值 1/3，第二窗口不受大数影响为 15。
	aEntry := bySig[wantSigs[0]]
	aWindows := aEntry["windows"].([]interface{})
	if len(aWindows) != 3 {
		t.Fatalf("host=a windows = %v, want 3 non-empty windows", aWindows)
	}
	w0 := aWindows[0].(map[string]interface{})
	if w0["start"].(float64) != 1000 || w0["end"].(float64) != 1999 ||
		int(w0["count"].(float64)) != 3 || w0["average"].(float64) != 1.0/3.0 {
		t.Fatalf("host=a cancellation window = %v, want [1000,1999] count 3 average 1/3", w0)
	}
	if b, _ := json.Marshal(w0["average"]); string(b) != "0.3333333333333333" {
		t.Fatalf("host=a cancellation average JSON = %s, want 0.3333333333333333", b)
	}
	w1 := aWindows[1].(map[string]interface{})
	if w1["start"].(float64) != 2000 || w1["end"].(float64) != 2999 ||
		int(w1["count"].(float64)) != 2 || w1["average"].(float64) != 15 {
		t.Fatalf("host=a second window = %v, want [2000,2999] count 2 average 15 (must not carry the big values)", w1)
	}
	w2 := aWindows[2].(map[string]interface{})
	if w2["start"].(float64) != 3000 || w2["end"].(float64) != 3000 ||
		int(w2["count"].(float64)) != 1 || w2["average"].(float64) != 9 {
		t.Fatalf("host=a last window = %v, want [3000,3000] count 1 average 9", w2)
	}
	// host=b：与 host=a 相同时间戳但独立成窗，不合并。
	bEntry := bySig[wantSigs[1]]
	bWindows := bEntry["windows"].([]interface{})
	if len(bWindows) != 2 {
		t.Fatalf("host=b windows = %v, want 2 (same timestamps as host=a must not merge series)", bWindows)
	}
	bw0 := bWindows[0].(map[string]interface{})
	if bw0["start"].(float64) != 1000 || bw0["end"].(float64) != 1999 ||
		int(bw0["count"].(float64)) != 2 || bw0["average"].(float64) != 150 {
		t.Fatalf("host=b first window = %v, want [1000,1999] count 2 average 150", bw0)
	}

	// 行 4/5：host=a 子集逐窗口核对。
	assertOutputWindowsPointsCorrespond(t, lines[3], lines[4], 1000, 3000, 1000)
	sub := decodeResultLine(t, lines[3])["series"].([]interface{})
	if len(sub) != 1 || cpSigs(sub)[0] != wantSigs[0] {
		t.Fatalf("host=a subset = %v, want exactly host=a", cpSigs(sub))
	}

	// 行 6/7：1999/2000 边界归属，end=3000 收尾窗口不存在（不补零）。
	assertOutputWindowsPointsCorrespond(t, lines[5], lines[6], 1000, 3000, 1000)
	eEntry := decodeResultLine(t, lines[5])["series"].([]interface{})[0].(map[string]interface{})
	eWindows := eEntry["windows"].([]interface{})
	if len(eWindows) != 2 {
		t.Fatalf("host=e windows = %v, want exactly two boundary windows", eWindows)
	}
	ew0 := eWindows[0].(map[string]interface{})
	ew1 := eWindows[1].(map[string]interface{})
	if ew0["start"].(float64) != 1000 || ew0["end"].(float64) != 1999 ||
		int(ew0["count"].(float64)) != 1 || ew0["average"].(float64) != 5 {
		t.Fatalf("1999 point window = %v, want [1000,1999] count 1 average 5", ew0)
	}
	if ew1["start"].(float64) != 2000 || ew1["end"].(float64) != 2999 ||
		int(ew1["count"].(float64)) != 1 || ew1["average"].(float64) != 7 {
		t.Fatalf("2000 point window = %v, want [2000,2999] count 1 average 7", ew1)
	}
	// 明细侧同样只有两个点且按升序归属。
	ePoints := decodeResultLine(t, lines[6])["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(ePoints) != 2 {
		t.Fatalf("host=e detail points = %v, want 2", ePoints)
	}

	// 行 8/9：zone=x 子集，额外 host 标签完整保留，step=25 两个窗口分开。
	assertOutputWindowsPointsCorrespond(t, lines[7], lines[8], 0, 10000, 25)
	mSeries := decodeResultLine(t, lines[7])["series"].([]interface{})
	if len(mSeries) != 2 {
		t.Fatalf("zone=x subset = %v, want 2 m series", cpSigs(mSeries))
	}
	first := mSeries[0].(map[string]interface{})
	if first["labels"].(map[string]interface{})["host"] != "h" ||
		len(first["labels"].(map[string]interface{})) != 2 {
		t.Fatalf("extra host label must be preserved: %v", first["labels"])
	}
	fw := first["windows"].([]interface{})
	if len(fw) != 1 {
		t.Fatalf("host=h series windows = %v, want one window [25,49]", fw)
	}
	if fw[0].(map[string]interface{})["start"].(float64) != 25 ||
		fw[0].(map[string]interface{})["end"].(float64) != 49 ||
		int(fw[0].(map[string]interface{})["count"].(float64)) != 1 ||
		fw[0].(map[string]interface{})["average"].(float64) != 200 {
		t.Fatalf("host=h window = %v, want [25,49] count 1 average 200", fw[0])
	}

	// 行 10/11：start==end，唯一窗口 [3000,3000]，两条序列各一点。
	assertOutputWindowsPointsCorrespond(t, lines[9], lines[10], 3000, 3000, 1000)
	single := decodeResultLine(t, lines[9])["series"].([]interface{})
	if len(single) != 2 {
		t.Fatalf("[3000,3000] series = %v, want host=a and host=b", cpSigs(single))
	}
	for i, wantAvg := range []float64{9, 300} {
		win := single[i].(map[string]interface{})["windows"].([]interface{})
		if len(win) != 1 || win[0].(map[string]interface{})["start"].(float64) != 3000 ||
			win[0].(map[string]interface{})["end"].(float64) != 3000 ||
			int(win[0].(map[string]interface{})["count"].(float64)) != 1 ||
			win[0].(map[string]interface{})["average"].(float64) != wantAvg {
			t.Fatalf("[3000,3000] series[%d] window = %v, want single [3000,3000] average %v",
				i, win, wantAvg)
		}
	}

	// 行 12/13：无命中两行都成功返回空数组（[] 而非 null），不补零窗口。
	for i, raw := range []string{lines[11], lines[12]} {
		op := [...]string{"query_windows", "query_points"}[i]
		m := decodeResultLine(t, raw)
		if m["status"] != "ok" || m["op"] != op {
			t.Fatalf("empty %s line = %v, want ok/%s", op, m, op)
		}
		if series, ok := m["series"].([]interface{}); !ok || len(series) != 0 {
			t.Fatalf("empty %s series = %v, want empty array", op, m["series"])
		}
		if !strings.Contains(raw, `"series":[]`) {
			t.Fatalf("empty %s JSON = %s, want \"series\":[]", op, raw)
		}
		if strings.Contains(raw, `"windows":[]`) && strings.Contains(raw, `"count"`) {
			t.Fatalf("empty %s must not fabricate windows: %s", op, raw)
		}
	}
}

// TestRunIngestWindowsAndPointsSameTimestampsDistinctSeries 专锁“同名但完整
// 标签不同的序列即使采样时间完全相同也不能合并”：两条序列在同样三个时间戳
// 上各写不同值，两边查询逐序列独立，窗口点数相同而均值不同。
func TestRunIngestWindowsAndPointsSameTimestampsDistinctSeries(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"m","timestamp":0,"value":2,"labels":{"h":"a"}},` +
			`{"name":"m","timestamp":10,"value":4,"labels":{"h":"a"}},` +
			`{"name":"m","timestamp":0,"value":20,"labels":{"h":"b"}},` +
			`{"name":"m","timestamp":10,"value":40,"labels":{"h":"b"}}]`,
		`{"op":"query_windows","name":"m","start":0,"end":19,"step":100}`,
		`{"op":"query_points","name":"m","start":0,"end":19}`,
	}, "\n")
	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	assertOutputWindowsPointsCorrespond(t, lines[1], lines[2], 0, 19, 100)
	series := decodeResultLine(t, lines[1])["series"].([]interface{})
	if len(series) != 2 {
		t.Fatalf("series = %v, want h=a and h=b separately", cpSigs(series))
	}
	for i, wantAvg := range []float64{3, 30} {
		win := series[i].(map[string]interface{})["windows"].([]interface{})
		if len(win) != 1 || int(win[0].(map[string]interface{})["count"].(float64)) != 2 ||
			win[0].(map[string]interface{})["average"].(float64) != wantAvg {
			t.Fatalf("series[%d] window = %v, want count 2 average %v (distinct series, same timestamps)",
				i, win, wantAvg)
		}
	}
}
