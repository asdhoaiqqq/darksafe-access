package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件从命令行入口回归保障 ingest 的 query_windows 操作：对象行 op 写为
// query_windows 时按 step 毫秒把 [start,end] 闭区间连续划成含首尾毫秒的
// 固定窗口，逐序列输出实际有点窗口的 start/end/count/average；失败行保留
// 原始行号（不带 series、index、conflict），后续输入照常处理，并最终返回
// 非零退出码。

// TestRunIngestQueryWindowsEndToEnd 写入与三种查询交替进入同一次 ingest：
// 验证任务书的窗口划分、空窗口不补零、start==end、标签筛选与序列次序、
// step 缺失/非法、step 对旧操作是未知字段、倒置区间指出实际边界。
func TestRunIngestQueryWindowsEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：写入 cpu host=a 的点（含空窗口场景）与 host=b 的一个点。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2500,"value":6,"labels":{"host":"b"}}]`,
		// 行 2：任务书示例 [1000,3000] step 1000：
		// [1000,1999] 两点均值 3；[2000,2999] 空窗不补零；[3000,3000] 一点值 8。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		// 行 3：start == end，唯一窗口 [3000,3000]。
		`{"op":"query_windows","name":"cpu","start":3000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		// 行 4：省略 labels 命中两条序列，沿用既有排列次序（host=a 在前），
		// 各序列只列自己非空的窗口，边界共用。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`,
		// 行 5：区间内没有点（host=zzz 未命中），成功返回空 series。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"zzz"}}`,
		// 行 6：缺 step：查询错误并指出 step，保留原始行号。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000}`,
		// 行 7：step 非正整数：查询错误。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":0}`,
		// 行 8：step 类型不对：查询错误。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":"1000"}`,
		// 行 9：step 超出 int64：查询错误。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":9223372036854775808}`,
		// 行 10：step 只用于新操作：旧 query 带 step 是未知字段。
		`{"op":"query","name":"cpu","start":1000,"end":3000,"step":1000}`,
		// 行 11：空白行（在输入文本中显式占一行号）。
		``,
		// 行 12：倒置区间且 step 合法：指出实际边界，不带 series/index/conflict。
		`{"op":"query_windows","name":"cpu","start":3000,"end":1000,"step":1000}`,
		// 行 13：失败行之后合法查询照常处理，数据未被改变。
		`{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
	}, "\n")

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 12 {
		t.Fatalf("got %d result lines, want 12 (blank line 11 emits nothing): %v", len(lines), lines)
	}

	assertWindow := func(w map[string]interface{}, start, end float64, count int, avg float64) {
		t.Helper()
		if w["start"].(float64) != start || w["end"].(float64) != end ||
			int(w["count"].(float64)) != count || w["average"].(float64) != avg {
			t.Fatalf("window = %v, want [%v,%v] count %d average %v", w, start, end, count, avg)
		}
	}

	// 行 2：两个非空窗口，空窗口不补零，按起点升序。
	m := decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("line 2 = %v, want ok query_windows", m)
	}
	series := m["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 2 series = %v, want one", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["labels"].(map[string]interface{})["host"] != "a" {
		t.Fatalf("line 2 identity = %v, want cpu{host=a}", s0)
	}
	if _, ok := s0["count"]; ok {
		t.Fatalf("series entry must not carry count: %v", s0)
	}
	if _, ok := s0["points"]; ok {
		t.Fatalf("series entry must not carry points: %v", s0)
	}
	windows := s0["windows"].([]interface{})
	if len(windows) != 2 {
		t.Fatalf("line 2 windows = %v, want 2 non-empty windows", windows)
	}
	assertWindow(windows[0].(map[string]interface{}), 1000, 1999, 2, 3)
	assertWindow(windows[1].(map[string]interface{}), 3000, 3000, 1, 8)

	// 行 3：start == end 唯一窗口。
	m = decodeResultLine(t, lines[2])
	windows = m["series"].([]interface{})[0].(map[string]interface{})["windows"].([]interface{})
	if len(windows) != 1 {
		t.Fatalf("line 3 windows = %v, want exactly one", windows)
	}
	assertWindow(windows[0].(map[string]interface{}), 3000, 3000, 1, 8)

	// 行 4：两条序列共用边界；host=a 两个窗口，host=b 一个窗口（点在 2500）。
	m = decodeResultLine(t, lines[3])
	series = m["series"].([]interface{})
	if len(series) != 2 {
		t.Fatalf("line 4 series = %v, want host=a and host=b", series)
	}
	first := series[0].(map[string]interface{})
	second := series[1].(map[string]interface{})
	if first["labels"].(map[string]interface{})["host"] != "a" ||
		second["labels"].(map[string]interface{})["host"] != "b" {
		t.Fatalf("line 4 series order = %v, want host=a then host=b", series)
	}
	fw := first["windows"].([]interface{})
	sw := second["windows"].([]interface{})
	if len(fw) != 2 || len(sw) != 1 {
		t.Fatalf("line 4 windows = %v / %v, want 2 then 1", fw, sw)
	}
	assertWindow(sw[0].(map[string]interface{}), 2000, 2999, 1, 6)

	// 行 5：无命中，成功的空 series 数组。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("line 5 = %v, want ok", m)
	}
	if len(m["series"].([]interface{})) != 0 {
		t.Fatalf("line 5 series = %v, want empty array", m["series"])
	}

	// 行 6-10：各类查询错误，行号逐行保留，且不带 series/index/conflict。
	badLines := []struct {
		idx  int
		want string
	}{
		{5, `missing required field "step"`},
		{6, `field "step"`},
		{7, `field "step" must be a JSON number`},
		{8, `field "step"`},
		{9, `unknown field "step"`},
	}
	for _, bl := range badLines {
		m = decodeResultLine(t, lines[bl.idx])
		if m["status"] != "error" {
			t.Fatalf("line %d = %v, want error", bl.idx+1, m)
		}
		if int(m["line"].(float64)) != bl.idx+1 {
			t.Fatalf("line %d reports line %v, want original line number", bl.idx+1, m["line"])
		}
		if !strings.Contains(m["error"].(string), bl.want) {
			t.Fatalf("line %d error = %q, want substring %q", bl.idx+1, m["error"], bl.want)
		}
		for _, banned := range []string{"series", "index", "conflict"} {
			if _, ok := m[banned]; ok {
				t.Fatalf("line %d error must not carry %q: %v", bl.idx+1, banned, m)
			}
		}
	}

	// 行 12：倒置区间指出实际边界。
	m = decodeResultLine(t, lines[10])
	if m["status"] != "error" || int(m["line"].(float64)) != 12 {
		t.Fatalf("line 12 = %v, want error on input line 12", m)
	}
	if !strings.Contains(m["error"].(string), "3000 > 1000") {
		t.Fatalf("line 12 error = %q, want actual bounds 3000 > 1000", m["error"])
	}

	// 行 13：失败之后合法 query 照常，host=a 三个点均值 14/3。
	m = decodeResultLine(t, lines[11])
	if m["status"] != "ok" || m["op"] != "query" {
		t.Fatalf("line 13 = %v, want ok query", m)
	}
	s0 = m["series"].([]interface{})[0].(map[string]interface{})
	if int(s0["count"].(float64)) != 3 || s0["average"].(float64) != 14.0/3.0 {
		t.Fatalf("line 13 = %v, want count 3 average 14/3", s0)
	}
}

// TestRunIngestQueryWindowsAllSuccessExitsZero 全部成功的 query_windows 输入
// （含空结果）退出码为 0，空 series 序列化为 [] 而不是 null。
func TestRunIngestQueryWindowsAllSuccessExitsZero(t *testing.T) {
	input := `{"op":"query_windows","name":"m","start":0,"end":100,"step":10}` + "\n"
	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimRight(out.String(), "\n")
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("invalid JSON result %q: %v", got, err)
	}
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("result = %v, want ok query_windows", m)
	}
	series, ok := m["series"].([]interface{})
	if !ok || len(series) != 0 {
		t.Fatalf("series = %v, want empty array (not null)", m["series"])
	}
}
