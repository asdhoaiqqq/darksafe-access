package main

import (
	"math"
	"strings"
	"testing"
)

// 本文件从命令行入口（runIngest，与用户 `... | darksafe ingest` 完全同一条
// 输出路径）回归保障 query_windows 的极小数均值：用户写入 5e-324（最小的可
// 表示非零 float64）量级的采样后，查询各窗口的 count/average 时，结果必须
// 遵守已有的“精确算术平均 + 最近偶数舍入”规则，并且用户实际收到的 JSON
// 数值文本要忠实保留零附近的区别：
//
//   - 正的微小均值舍入为正零，文本是 "average":0；
//   - 负的微小均值舍入为负零，文本必须是 "average":-0（不能把 -0 写成 0）；
//   - 大小相等符号相反的非零采样精确抵消，平均为正零；
//   - 越过中点的微小均值仍是可表示的非零次正规值，文本为 5e-324 / -5e-324，
//     既不能提前清零，也不能按十进制小数位截断；
//   - 零均值窗口是“有数据”的成功结果（带真实 count），不被省略；
//     真正无点的窗口不出现；区间外采样不参与；
//   - 同时间戳的等值重复（即便用不同十进制文本）仍按既有规则忽略，不改变
//     点数，也不把均值推过中点；查询只读。

// asFloat 从已解码的 JSON 结果里取一个 number 字段的 float64。
func asFloat(t *testing.T, m map[string]interface{}, key, where string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s: field %q = %#v, want a JSON number", where, key, m[key])
	}
	return v
}

// windowsOf 取出一条已解码序列的 windows 列表。
func windowsOf(t *testing.T, series map[string]interface{}, where string) []interface{} {
	t.Helper()
	w, ok := series["windows"].([]interface{})
	if !ok {
		t.Fatalf("%s: series has no windows array: %v", where, series)
	}
	return w
}

// TestRunIngestQueryWindowsTinyAverages 是用户视角的端到端保障：写入 → 等值
// 重复 → 多个 query_windows 查询 → 快照，断言输出行的原始 JSON 文本与解码值。
func TestRunIngestQueryWindowsTinyAverages(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：无标签序列 m 的极小数采样，step=10 时分布为：
		//   [0,9]   ts0=5e-324, ts1=0             → count 2，正零
		//   [10,19] ts10=-5e-324, ts11=0          → count 2，负零
		//   [20,29] ts20=5e-324, ts21=-5e-324     → count 2，精确抵消，正零
		//   [30,39] ts30=5e-324, ts31=5e-324, ts32=0 → count 3，5e-324
		//   [40,49] ts40=-5e-324, ts41=0, ts42=0  → count 3，负零
		//   [50,59] 无点（空窗应省略）
		//   ts100=5e-324 在后续查询区间之外，不得参与。
		// 另有标签序列 m{host=a}：ts0/ts1 两个 -5e-324 与 ts2 的 0
		//   → [0,9] count 3，-5e-324。
		`[{"name":"m","timestamp":0,"value":5e-324},` +
			`{"name":"m","timestamp":1,"value":0},` +
			`{"name":"m","timestamp":10,"value":-5e-324},` +
			`{"name":"m","timestamp":11,"value":0},` +
			`{"name":"m","timestamp":20,"value":5e-324},` +
			`{"name":"m","timestamp":21,"value":-5e-324},` +
			`{"name":"m","timestamp":30,"value":5e-324},` +
			`{"name":"m","timestamp":31,"value":5e-324},` +
			`{"name":"m","timestamp":32,"value":0},` +
			`{"name":"m","timestamp":40,"value":-5e-324},` +
			`{"name":"m","timestamp":41,"value":0},` +
			`{"name":"m","timestamp":42,"value":0},` +
			`{"name":"m","timestamp":100,"value":5e-324},` +
			`{"name":"m","timestamp":0,"value":-5e-324,"labels":{"host":"a"}},` +
			`{"name":"m","timestamp":1,"value":-5e-324,"labels":{"host":"a"}},` +
			`{"name":"m","timestamp":2,"value":0,"labels":{"host":"a"}}]`,
		// 行 2：同一位置（无标签 m，ts0）用另一种十进制文本重提同一个
		// float64——等值重复，必须被忽略，窗口 [0,9] 仍是 2 个点、正零，
		// 不能因“再来一个 5e-324”把均值推过中点变成 5e-324。
		`[{"name":"m","timestamp":0,"value":4.9406564584124654e-324}]`,
		// 行 3：同一位置 ts1 用 -0 重提已存的 0，同样只是等值重复。
		`[{"name":"m","timestamp":1,"value":-0}]`,
		// 行 4：标签子集查询 host=a，只返回其完整身份与唯一窗口 -5e-324。
		`{"op":"query_windows","name":"m","start":0,"end":9,"step":10,"labels":{"host":"a"}}`,
		// 行 5：省略 labels 命中两条序列；无标签序列在前，五个非空窗口按
		// 起点升序，空窗 [50,59] 不补零。
		`{"op":"query_windows","name":"m","start":0,"end":59,"step":10}`,
		// 行 6：区间 [60,99] 无点（ts100 在区间外）：成功的空 series。
		`{"op":"query_windows","name":"m","start":60,"end":99,"step":10}`,
		// 行 7：空批快照验证查询只读、重复未写入：点数仍是 13 + 3。
		`[]`,
	}, "\n")

	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 for all-successful input\noutput:\n%s", code, out.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d output lines, want 7: %v", len(lines), lines)
	}

	// 行 1：16 个采样全部为新增。
	first := decodeResultLine(t, lines[0])
	if first["status"] != "ok" || first["added"].(float64) != 16 || first["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want 16 added / 0 duplicates", first)
	}
	// 行 2、行 3：等值重复均忽略（added 0、duplicates 1）。
	for i := 1; i <= 2; i++ {
		m := decodeResultLine(t, lines[i])
		if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
			t.Fatalf("line %d = %v, want an ignored equal-value duplicate (0/1)", i+1, m)
		}
	}

	// 行 4：host=a 的唯一窗口 [0,9] count 3、average -5e-324。
	m := decodeResultLine(t, lines[3])
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("line 4 = %v, want ok query_windows", m)
	}
	labeledSeries := m["series"].([]interface{})
	if len(labeledSeries) != 1 {
		t.Fatalf("line 4 series = %v, want exactly host=a", labeledSeries)
	}
	sLabeled := labeledSeries[0].(map[string]interface{})
	labels := sLabeled["labels"].(map[string]interface{})
	if sLabeled["name"] != "m" || len(labels) != 1 || labels["host"] != "a" {
		t.Fatalf("line 4 identity = %v, want m{host=a} with the full label set", sLabeled)
	}
	lw := windowsOf(t, sLabeled, "line 4")
	if len(lw) != 1 {
		t.Fatalf("line 4 windows = %v, want one", lw)
	}
	w0 := lw[0].(map[string]interface{})
	if int(asFloat(t, w0, "count", "line 4")) != 3 {
		t.Fatalf("line 4 count = %v, want 3", w0["count"])
	}
	if got := math.Float64bits(asFloat(t, w0, "average", "line 4")); got != 0x8000000000000001 {
		t.Fatalf("line 4 average bits = %016x, want 8000000000000001 (-5e-324)", got)
	}
	if !strings.Contains(lines[3], `"average":-5e-324`) {
		t.Fatalf("line 4 raw JSON %q must preserve \"average\":-5e-324", lines[3])
	}

	// 行 5：两条序列、窗口升序、零均值窗保留而空窗省略。
	raw5 := lines[4]
	m = decodeResultLine(t, raw5)
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("line 5 = %v, want ok query_windows", m)
	}
	allSeries := m["series"].([]interface{})
	if len(allSeries) != 2 {
		t.Fatalf("line 5 series = %v, want unlabeled and host=a", allSeries)
	}
	sUnlabeled := allSeries[0].(map[string]interface{})
	sSecond := allSeries[1].(map[string]interface{})
	if len(sUnlabeled["labels"].(map[string]interface{})) != 0 {
		t.Fatalf("line 5 first series must be the unlabeled one, got %v", sUnlabeled["labels"])
	}
	if sSecond["labels"].(map[string]interface{})["host"] != "a" {
		t.Fatalf("line 5 second series must be host=a, got %v", sSecond["labels"])
	}

	type wantWindow struct {
		start, end float64
		count      int
		average    uint64 // 期望的 float64 位模式
	}
	posZeroBits := uint64(0x0000000000000000)
	negZeroBits := uint64(0x8000000000000000)
	minSubBits := uint64(0x0000000000000001)
	wantWindows := []wantWindow{
		{0, 9, 2, posZeroBits},
		{10, 19, 2, negZeroBits},
		{20, 29, 2, posZeroBits},
		{30, 39, 3, minSubBits},
		{40, 49, 3, negZeroBits},
	}
	gotWindows := windowsOf(t, sUnlabeled, "line 5 unlabeled")
	if len(gotWindows) != len(wantWindows) {
		t.Fatalf("line 5 windows = %v, want %d non-empty windows (empty [50,59] omitted)",
			gotWindows, len(wantWindows))
	}
	for i, want := range wantWindows {
		g := gotWindows[i].(map[string]interface{})
		if asFloat(t, g, "start", "line 5") != want.start || asFloat(t, g, "end", "line 5") != want.end {
			t.Fatalf("window %d bounds = [%v,%v], want [%v,%v]",
				i, g["start"], g["end"], want.start, want.end)
		}
		if int(asFloat(t, g, "count", "line 5")) != want.count {
			t.Fatalf("window %d count = %v, want %d", i, g["count"], want.count)
		}
		avg := asFloat(t, g, "average", "line 5")
		if math.Float64bits(avg) != want.average {
			t.Fatalf("window [%v,%v] average bits = %016x, want %016x",
				want.start, want.end, math.Float64bits(avg), want.average)
		}
	}

	// 用户收到的原始 JSON 文本必须按窗口升序忠实呈现 0 / -0 / 0 /
	// 5e-324 / -0（正零与负零文本不同，非零次正规值不被清零或截断）。
	wantFields := []string{`"average":0`, `"average":-0`, `"average":0`, `"average":5e-324`, `"average":-0`}
	pos := 0
	for _, field := range wantFields {
		idx := strings.Index(raw5[pos:], field)
		if idx < 0 {
			t.Fatalf("line 5 raw JSON missing ordered field %s after offset %d:\n%s", field, pos, raw5)
		}
		pos += idx + len(field)
	}
	// 负零必须真的写成 -0：行 5 至少要有两处 "average":-0（窗口 [10,19] 与 [40,49]）。
	if strings.Count(raw5, `"average":-0`) < 2 {
		t.Fatalf("line 5 raw JSON must keep both negative-zero windows as -0:\n%s", raw5)
	}
	// host=a 序列在同一条输出里仍是 -5e-324。
	if !strings.Contains(raw5, `"average":-5e-324`) {
		t.Fatalf("line 5 raw JSON must preserve host=a average -5e-324:\n%s", raw5)
	}

	// 行 6：区间无点，成功的空 series 数组；ts100 的点在区间外不参与。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("line 6 = %v, want ok query_windows", m)
	}
	if len(m["series"].([]interface{})) != 0 {
		t.Fatalf("line 6 series = %v, want empty array", m["series"])
	}

	// 行 7：查询只读、重复未写入。无标签序列 13 个点（含区间外 ts100），
	// host=a 序列 3 个点。
	snap := decodeResultLine(t, lines[6])
	if snap["status"] != "ok" {
		t.Fatalf("line 7 = %v, want ok snapshot", snap)
	}
	pointsByLabels := map[int]int{}
	for _, e := range snap["series"].([]interface{}) {
		sm := e.(map[string]interface{})
		pointsByLabels[len(sm["labels"].(map[string]interface{}))] = len(sm["points"].([]interface{}))
	}
	if pointsByLabels[0] != 13 || pointsByLabels[1] != 3 {
		t.Fatalf("snapshot point counts = %v, want unlabeled=13 host=a=3 (queries read-only, duplicates ignored)",
			pointsByLabels)
	}
}
