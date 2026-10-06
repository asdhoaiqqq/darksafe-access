package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件从命令行入口（一次 ingest 进程内）回归保障用户的窗口复核流程：在同一份
// 已成功写入的数据上，用相同的指标名、标签条件和起止时间分别发送 op 为
// "query_windows"（额外给出合法 step）与 "query_points" 的对象行。两条输出
// 必须描述同一组采样：
//
//   - 两行 series 的序列身份（指标名+完整标签集合）与排列次序逐一相同；
//   - 窗口从查询起点 start 起按 step 平铺，边界为 [start+k*step,
//     min(start+(k+1)*step-1, end)]，不跟随各序列的第一个采样点移动；
//   - 每个返回窗口的 count 等于明细行对应序列中落在窗口闭区间内的点数，
//     average 等于这些点（JSON 往返后的 float64 存储值）的精确平均（最近偶）；
//   - 明细中的每个点恰好计入一个窗口：计数总和等于明细点数，既不遗漏，
//     也不在相邻窗口重复计数；
//   - 只有实际有点的窗口出现，按起点升序；完全未命中时两行都成功输出
//     空 series 数组（[]）。
//
// 与 darksafe 包内的 Go 级对应测试互补：这里数据真正经过 stdin 逐行写入、
// JSON 编码输出与退出码路径，保证用户在命令行上做的窗口复核自洽。

// assertWindowDetailLinesCorrespond 核对一对 query_windows / query_points
// 成功输出行逐序列、逐窗口对应。start/end/step 用于失败信息指出区间与步长，
// 并独立复核窗口边界与采样归属。
func assertWindowDetailLinesCorrespond(t *testing.T, windowsLine, detailLine string, start, end, step int64) {
	t.Helper()
	wm := decodeResultLine(t, windowsLine)
	dm := decodeResultLine(t, detailLine)
	if wm["status"] != "ok" || wm["op"] != "query_windows" {
		t.Fatalf("[%d,%d] step=%d: windows line = %v, want ok/query_windows", start, end, step, wm)
	}
	if dm["status"] != "ok" || dm["op"] != "query_points" {
		t.Fatalf("[%d,%d] step=%d: detail line = %v, want ok/query_points", start, end, step, dm)
	}
	wSeries := wm["series"].([]interface{})
	dSeries := dm["series"].([]interface{})
	if len(wSeries) != len(dSeries) {
		t.Fatalf("[%d,%d] step=%d: series count: query_windows=%d (%v), query_points=%d (%v)",
			start, end, step, len(wSeries), cpSigs(wSeries), len(dSeries), cpSigs(dSeries))
	}
	for i := range wSeries {
		we := wSeries[i].(map[string]interface{})
		de := dSeries[i].(map[string]interface{})
		sig := cpSig(we["name"].(string), we["labels"].(map[string]interface{}))
		if sig != cpSig(de["name"].(string), de["labels"].(map[string]interface{})) {
			t.Fatalf("[%d,%d] step=%d: series[%d] identity/order mismatch: windows=%s detail=%s",
				start, end, step, i, sig,
				cpSig(de["name"].(string), de["labels"].(map[string]interface{})))
		}
		points := de["points"].([]interface{})
		windows := we["windows"].([]interface{})
		if len(points) == 0 {
			t.Fatalf("[%d,%d] step=%d: %s: zero-point entry must not appear in either line",
				start, end, step, sig)
		}

		// 明细点严格升序且都在闭区间内。
		var prevTS float64
		for j, pe := range points {
			ts := pe.(map[string]interface{})["timestamp"].(float64)
			if ts < float64(start) || ts > float64(end) {
				t.Fatalf("[%d,%d] step=%d: %s: detail point[%d] ts=%v is outside the closed range",
					start, end, step, sig, j, ts)
			}
			if j > 0 && prevTS >= ts {
				t.Fatalf("[%d,%d] step=%d: %s: detail points not strictly ascending at %d",
					start, end, step, sig, j)
			}
			prevTS = ts
		}

		// 逐窗口核对：边界从查询起点平铺、count 与窗口内明细点数一致、
		// average 与窗口内明细独立复算一致。
		total := 0
		var prevWStart float64
		for k, wve := range windows {
			w := wve.(map[string]interface{})
			ws := int64(w["start"].(float64))
			we2 := int64(w["end"].(float64))
			// 边界必须从 start 起按 step 平铺，最后一个窗口截到 end。
			if (ws-start)%step != 0 || ws < start {
				t.Fatalf("[%d,%d] step=%d: %s: window[%d] start=%d is not start+k*step",
					start, end, step, sig, k, ws)
			}
			wantEnd := ws + step - 1
			if wantEnd > end {
				wantEnd = end
			}
			if we2 != wantEnd {
				t.Fatalf("[%d,%d] step=%d: %s: window[%d] = [%d,%d], want end %d (tiling from the query start)",
					start, end, step, sig, k, ws, we2, wantEnd)
			}
			if k > 0 && prevWStart >= w["start"].(float64) {
				t.Fatalf("[%d,%d] step=%d: %s: windows not ascending by start at %d",
					start, end, step, sig, k)
			}
			prevWStart = w["start"].(float64)

			// 收集落在该窗口闭区间内的明细点。
			var inWindow []interface{}
			for _, pe := range points {
				ts := int64(pe.(map[string]interface{})["timestamp"].(float64))
				if ts >= ws && ts <= we2 {
					inWindow = append(inWindow, pe)
				}
			}
			count := int(w["count"].(float64))
			if count != len(inWindow) {
				t.Fatalf("[%d,%d] step=%d: %s: window [%d,%d]: count=%d but %d detail points fall inside",
					start, end, step, sig, ws, we2, count, len(inWindow))
			}
			if count == 0 {
				t.Fatalf("[%d,%d] step=%d: %s: window [%d,%d]: empty window must not be emitted",
					start, end, step, sig, ws, we2)
			}
			wantAvg := cpDetailMean(t, inWindow)
			if got := w["average"].(float64); got != wantAvg {
				t.Fatalf("[%d,%d] step=%d: %s: window [%d,%d]: average=%v but recomputed from the detail = %v",
					start, end, step, sig, ws, we2, got, wantAvg)
			}
			total += count
		}
		// 划分性：每个明细点恰好计入一个窗口。
		if total != len(points) {
			t.Fatalf("[%d,%d] step=%d: %s: window counts sum to %d but the detail lists %d points; every point must be counted in exactly one window",
				start, end, step, sig, total, len(points))
		}
	}
}

// TestRunIngestWindowsAndPointsCorrespondEndToEnd 是命令行端到端窗口复核：
// 一次写入多条序列（含相同时间戳的不同标签序列、区间外点），随后成对发送
// 条件相同、仅 op/step 不同的 query_windows 与 query_points，逐对核对
// 身份/次序/窗口边界/点数/均值/划分性。
func TestRunIngestWindowsAndPointsCorrespondEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：一次写入全部数据。
		// cpu host=a：区间外 500/4000；1999 与 2000 分属相邻窗口；3000 入收尾窗口。
		`[{"name":"cpu","timestamp":500,"value":77,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1999,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2000,"value":6,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":4000,"value":88,"labels":{"host":"a"}},` +
			// cpu host=b：与 host=a 在 1000/2000 同时间戳采样，值不同；
			// 另有 host=a 缺少的窗口 [2000,2999] 内的 2500。
			`{"name":"cpu","timestamp":1000,"value":200,"labels":{"host":"b"}},` +
			`{"name":"cpu","timestamp":2000,"value":600,"labels":{"host":"b"}},` +
			`{"name":"cpu","timestamp":2500,"value":9,"labels":{"host":"b"}},` +
			// 指标 m：标签子集命中两条，其中一条带查询条件之外的额外标签；
			// 同窗口内 1e16、1、-1e16 抵消后余量必须保留。
			`{"name":"m","timestamp":10,"value":10000000000000000,"labels":{"zone":"x","host":"h"}},` +
			`{"name":"m","timestamp":20,"value":1,"labels":{"zone":"x","host":"h"}},` +
			`{"name":"m","timestamp":30,"value":-10000000000000000,"labels":{"zone":"x","host":"h"}},` +
			`{"name":"m","timestamp":40,"value":400,"labels":{"zone":"x"}}]`,
		// 行 2/3：host=a 的 [1000,3000]、step 1000。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		`{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
		// 行 4/5：省略 labels，两条 cpu 序列共用边界、各自统计。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`,
		`{"op":"query_points","name":"cpu","start":1000,"end":3000}`,
		// 行 6/7：标签子集 zone=x 命中两条 m，额外 host 标签必须保留；
		// 窗口 [0,99] 内 1e16、1、-1e16 的均值是 0.3333333333333333。
		`{"op":"query_windows","name":"m","start":0,"end":99,"step":100,"labels":{"zone":"x"}}`,
		`{"op":"query_points","name":"m","start":0,"end":99,"labels":{"zone":"x"}}`,
		// 行 8/9：start==end，唯一窗口 [2000,2000]。
		`{"op":"query_windows","name":"cpu","start":2000,"end":2000,"step":1000,"labels":{"host":"a"}}`,
		`{"op":"query_points","name":"cpu","start":2000,"end":2000,"labels":{"host":"a"}}`,
		// 行 10/11：区间内没有任何点，两行都成功返回空数组。
		`{"op":"query_windows","name":"cpu","start":4001,"end":9000,"step":1000}`,
		`{"op":"query_points","name":"cpu","start":4001,"end":9000}`,
	}, "\n")

	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("all lines succeed, exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 11 {
		t.Fatalf("got %d output lines, want 11: %v", len(lines), lines)
	}

	// 行 2/3：任务书边界示例——[1000,1999] 两点、[2000,2999] 一点、
	// [3000,3000] 一点，逐窗口与明细对应。
	assertWindowDetailLinesCorrespond(t, lines[1], lines[2], 1000, 3000, 1000)
	wSeries := decodeResultLine(t, lines[1])["series"].([]interface{})
	if len(wSeries) != 1 {
		t.Fatalf("host=a [1000,3000] windows series = %v, want exactly 1", wSeries)
	}
	wins := wSeries[0].(map[string]interface{})["windows"].([]interface{})
	wantWins := []map[string]float64{
		{"start": 1000, "end": 1999, "count": 2, "average": 3},
		{"start": 2000, "end": 2999, "count": 1, "average": 6},
		{"start": 3000, "end": 3000, "count": 1, "average": 8},
	}
	if len(wins) != len(wantWins) {
		t.Fatalf("host=a windows = %v, want %v", wins, wantWins)
	}
	for i, want := range wantWins {
		got := wins[i].(map[string]interface{})
		for _, key := range []string{"start", "end", "count", "average"} {
			if got[key].(float64) != want[key] {
				t.Fatalf("host=a window[%d] = %v, want %v (1999/2000 split across adjacent windows, 3000 in the last)",
					i, got, want)
			}
		}
	}

	// 行 4/5：两条 cpu 序列各自统计——同时间戳的 host=a/host=b 不合并，
	// host=b 独有窗口 [2000,2999]（2500 上的点）。
	assertWindowDetailLinesCorrespond(t, lines[3], lines[4], 1000, 3000, 1000)
	allWin := decodeResultLine(t, lines[3])["series"].([]interface{})
	if len(allWin) != 2 {
		t.Fatalf("all cpu series: want 2 entries, got %v", cpSigs(allWin))
	}
	winByHost := make(map[string]map[string]interface{})
	for _, e := range allWin {
		em := e.(map[string]interface{})
		winByHost[em["labels"].(map[string]interface{})["host"].(string)] = em
	}
	aWins := winByHost["a"]["windows"].([]interface{})
	if len(aWins) != 3 {
		t.Fatalf("host=a windows = %v, want 3 (its own samples only)", aWins)
	}
	bWins := winByHost["b"]["windows"].([]interface{})
	if len(bWins) != 2 {
		t.Fatalf("host=b windows = %v, want 2 ([1000,1999] with 1000, [2000,2999] with 2000+2500)", bWins)
	}
	bSecond := bWins[1].(map[string]interface{})
	if bSecond["start"].(float64) != 2000 || bSecond["count"].(float64) != 2 ||
		bSecond["average"].(float64) != 304.5 {
		t.Fatalf("host=b window [2000,2999] = %v, want count=2 average=304.5 (2000:600 and 2500:9)",
			bSecond)
	}

	// 行 6/7：子集 zone=x 命中两条 m；带额外标签的序列窗口内 1e16、1、-1e16
	// 抵消后均值是 0.3333333333333333；额外标签完整保留。
	assertWindowDetailLinesCorrespond(t, lines[5], lines[6], 0, 99, 100)
	mWin := decodeResultLine(t, lines[5])["series"].([]interface{})
	if len(mWin) != 2 {
		t.Fatalf("zone=x subset: want 2 m series, got %v", cpSigs(mWin))
	}
	first := mWin[0].(map[string]interface{})
	firstLabels := first["labels"].(map[string]interface{})
	if _, hasHost := firstLabels["host"]; !hasHost || len(firstLabels) != 2 {
		t.Fatalf("extra label dropped from windows record: %v", firstLabels)
	}
	firstWins := first["windows"].([]interface{})
	if len(firstWins) != 1 {
		t.Fatalf("host=h windows = %v, want the single window [0,99]", firstWins)
	}
	fw := firstWins[0].(map[string]interface{})
	if fw["count"].(float64) != 3 || fw["average"].(float64) != 1.0/3.0 {
		t.Fatalf("host=h window [0,99] = %v, want count=3 average=1/3 (middle 1 must survive cancellation)", fw)
	}
	if b, _ := json.Marshal(fw["average"]); string(b) != "0.3333333333333333" {
		t.Fatalf("host=h window average JSON = %s, want 0.3333333333333333", b)
	}

	// 行 8/9：start==end 单点窗口。
	assertWindowDetailLinesCorrespond(t, lines[7], lines[8], 2000, 2000, 1000)
	oneWin := decodeResultLine(t, lines[7])["series"].([]interface{})
	oneW := oneWin[0].(map[string]interface{})["windows"].([]interface{})[0].(map[string]interface{})
	if oneW["start"].(float64) != 2000 || oneW["end"].(float64) != 2000 ||
		oneW["count"].(float64) != 1 || oneW["average"].(float64) != 6 {
		t.Fatalf("[2000,2000] window = %v, want [2000,2000] count=1 average=6", oneW)
	}

	// 行 10/11：无命中两行都成功返回空数组（[] 而非 null），不补零窗口。
	for i, line := range []string{lines[9], lines[10]} {
		m := decodeResultLine(t, line)
		op := [...]string{"query_windows", "query_points"}[i]
		if m["status"] != "ok" || m["op"] != op {
			t.Fatalf("empty %s line = %v, want ok/%s", op, m, op)
		}
		if series, ok := m["series"].([]interface{}); !ok || len(series) != 0 {
			t.Fatalf("empty %s series = %v, want empty array []", op, m["series"])
		}
		if !strings.Contains(line, `"series":[]`) {
			t.Fatalf("empty %s JSON = %s, want \"series\":[]", op, line)
		}
	}
}

// TestRunIngestWindowsPointsAnchoredAtQueryStart 命令行层面锁定“窗口从查询
// 起点划分”：同一组采样、同一 step，查询起点不同的两行结果边界不同；序列
// 第一个采样点之前的空窗口不补零，窗口起点始终是 start+k*step。
func TestRunIngestWindowsPointsAnchoredAtQueryStart(t *testing.T) {
	input := strings.Join([]string{
		// 第一个采样点在 1500，刻意不在任何窗口边界上。
		`[{"name":"m","timestamp":1500,"value":2},` +
			`{"name":"m","timestamp":1600,"value":4},` +
			`{"name":"m","timestamp":2500,"value":6}]`,
		`{"op":"query_windows","name":"m","start":1000,"end":3000,"step":1000}`,
		`{"op":"query_points","name":"m","start":1000,"end":3000}`,
		`{"op":"query_windows","name":"m","start":1200,"end":3000,"step":1000}`,
		`{"op":"query_points","name":"m","start":1200,"end":3000}`,
	}, "\n")

	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("all lines succeed, exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d output lines, want 5: %v", len(lines), lines)
	}

	// 起点 1000：窗口 [1000,1999]（1500 与 1600）、[2000,2999]（2500）。
	assertWindowDetailLinesCorrespond(t, lines[1], lines[2], 1000, 3000, 1000)
	w1 := decodeResultLine(t, lines[1])["series"].([]interface{})[0].(map[string]interface{})["windows"].([]interface{})
	if got := w1[0].(map[string]interface{})["start"].(float64); got != 1000 {
		t.Fatalf("first window starts at %v, want 1000 (query start), not the first sample 1500", got)
	}

	// 起点 1200：同一组采样重新平铺——[1200,2199]（1500 与 1600）、
	// [2200,3000]（2500，截到 end）。
	assertWindowDetailLinesCorrespond(t, lines[3], lines[4], 1200, 3000, 1000)
	w2 := decodeResultLine(t, lines[3])["series"].([]interface{})[0].(map[string]interface{})["windows"].([]interface{})
	second := w2[1].(map[string]interface{})
	if second["start"].(float64) != 2200 || second["end"].(float64) != 3000 {
		t.Fatalf("re-tiled last window = %v, want [2200,3000] clipped to end", second)
	}
}
