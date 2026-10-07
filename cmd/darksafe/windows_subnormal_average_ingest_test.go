package main

import (
	"strings"
	"testing"
)

// 本文件从命令行入口回归保障 ingest 的 query_windows 在极小数均值上的行为：
// 用户通过 ingest 写入采样、再用 query_windows 查看各窗口的 count 与 average
// 时，即使平均值接近零，结果也遵守精确平均与最近偶数舍入规则——依据是已保存
// 的有限 float64 值，不能把微小非零值提前当成零，也不能按十进制小数位截断。
// 重点锁定用户实际取得的逐行 JSON 数值表示：正零输出 0，负零输出 -0，
// 5e-324 不被冲刷成 0；均值为零的窗口保留真实点数，真正无点的窗口不出现。

// TestRunIngestQueryWindowsSubnormalAverageEndToEnd 同一次 ingest 内写入零附近
// 的采样并逐窗查询：两点中点（5e-324 与 0 → 正零；-5e-324 与 0 → 负零）、
// 三点中点两侧（两个 5e-324 加一个 0 → 5e-324；一个 5e-324 加两个 0 → 正零）、
// 等值反向抵消（正零），以及相邻窗口互不混入、区间外采样不参与。
func TestRunIngestQueryWindowsSubnormalAverageEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：窗口 [0,9] 保存 5e-324 与 0（两个时间戳）。
		`[{"name":"m","timestamp":1,"value":5e-324},{"name":"m","timestamp":2,"value":0}]`,
		// 行 2：窗口 [10,19] 保存 -5e-324 与 0。
		`[{"name":"m","timestamp":11,"value":-5e-324},{"name":"m","timestamp":12,"value":0}]`,
		// 行 3：窗口 [20,29] 保存两个 5e-324 与一个 0（中点靠上一侧）。
		`[{"name":"m","timestamp":21,"value":5e-324},{"name":"m","timestamp":22,"value":5e-324},{"name":"m","timestamp":23,"value":0}]`,
		// 行 4：窗口 [30,39] 保存一个 -5e-324 与两个 0（中点靠下一侧，负）。
		`[{"name":"m","timestamp":31,"value":-5e-324},{"name":"m","timestamp":32,"value":0},{"name":"m","timestamp":33,"value":0}]`,
		// 行 5：窗口 [40,49] 保存 5e-324 与 -5e-324（精确抵消）。
		`[{"name":"m","timestamp":41,"value":5e-324},{"name":"m","timestamp":42,"value":-5e-324}]`,
		// 行 6：区间外采样（ts=55），不参与 [0,49] 的任何窗口。
		`[{"name":"m","timestamp":55,"value":1e300}]`,
		// 行 7：同一位置（ts=1）等值重复，被忽略，不改变窗口点数与均值。
		`[{"name":"m","timestamp":1,"value":5e-324}]`,
		// 行 8：逐窗查询 [0,49]、step 10。
		`{"op":"query_windows","name":"m","start":0,"end":49,"step":10}`,
	}, "\n")

	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 (all lines succeed)", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("got %d result lines, want 8: %v", len(lines), lines)
	}

	// 行 7 的写入结果：等值重复计 duplicates，不新增点。
	m := decodeResultLine(t, lines[6])
	if m["status"] != "ok" || int(m["added"].(float64)) != 0 || int(m["duplicates"].(float64)) != 1 {
		t.Fatalf("line 7 = %v, want ok added 0 duplicates 1", m)
	}

	// 行 8 的查询结果：五个窗口按起点升序，各自只反映自己的点。
	got := strings.TrimRight(lines[7], "\n")
	want := `{"status":"ok","op":"query_windows","series":[{"name":"m","labels":{},"windows":[` +
		`{"start":0,"end":9,"count":2,"average":0},` +
		`{"start":10,"end":19,"count":2,"average":-0},` +
		`{"start":20,"end":29,"count":3,"average":5e-324},` +
		`{"start":30,"end":39,"count":3,"average":-0},` +
		`{"start":40,"end":49,"count":2,"average":0}` +
		`]}]}`
	if got != want {
		t.Fatalf("query_windows JSON =\n%s\nwant\n%s", got, want)
	}
	// 区间外的 1e300 不出现在任何窗口均值中。
	if strings.Contains(got, "1e+300") {
		t.Fatalf("out-of-range sample leaked into windows: %s", got)
	}
}

// TestRunIngestQueryWindowsSubnormalZeroAverageWindowPresent 均值为零的窗口仍是
// 有数据的成功结果：查询结果中出现且保留真实点数；真正没有采样的窗口不出现。
func TestRunIngestQueryWindowsSubnormalZeroAverageWindowPresent(t *testing.T) {
	input := strings.Join([]string{
		// 窗口 [0,9]：5e-324 与 0，均值舍入为正零；窗口 [10,19] 无点；
		// 窗口 [20,29]：5e-324 与 -5e-324，精确抵消为正零。
		`[{"name":"m","timestamp":1,"value":5e-324},{"name":"m","timestamp":2,"value":0},` +
			`{"name":"m","timestamp":21,"value":5e-324},{"name":"m","timestamp":22,"value":-5e-324}]`,
		`{"op":"query_windows","name":"m","start":0,"end":29,"step":10}`,
	}, "\n")

	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d result lines, want 2: %v", len(lines), lines)
	}
	want := `{"status":"ok","op":"query_windows","series":[{"name":"m","labels":{},"windows":[` +
		`{"start":0,"end":9,"count":2,"average":0},` +
		`{"start":20,"end":29,"count":2,"average":0}` +
		`]}]}`
	if lines[1] != want {
		t.Fatalf("query_windows JSON =\n%s\nwant\n%s", lines[1], want)
	}
}
