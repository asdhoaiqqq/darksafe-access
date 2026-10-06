package main

import (
	"strings"
	"testing"
)

// 本文件从命令行入口回归保障 ingest 的 query_windows 操作：对象行 op 写为
// query_windows 并附带正整数 step 时，沿用 name/start/end/labels 查询条件，
// 把区间从 start 起按 step 毫秒连续划分窗口，逐序列输出实际有点窗口的
// start/end/count/average；失败行保留原始行号、不带 series/index/conflict，
// 之后各行照常处理，整次调用因出现失败行而返回非零退出码。

// TestRunIngestQueryWindowsEndToEnd 在同一次 ingest 中交替写入、窗口查询、
// 倒置区间失败与原 query 复核，逐行核对输出形状与数值。
func TestRunIngestQueryWindowsEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：cpu host=a 在 1000/1500/3000 各有点，host=b 在 2000 有一个点。
		`[{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2000,"value":10,"labels":{"host":"b"}}]`,
		// 行 2：任务书示例划分 [1000,3000] step 1000，只看 host=a：
		// 命中窗口 [1000,1999]（2、4 两点，均值 3）与 [3000,3000]（6）。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		// 行 3：start == end，唯一窗口 [3000,3000]。
		`{"op":"query_windows","name":"cpu","start":3000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		// 行 4：省略 labels 命中全部序列，各序列共用边界、按序列次序输出。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`,
		// 行 5：区间无点，空 series。
		`{"op":"query_windows","name":"cpu","start":4000,"end":5000,"step":1000}`,
		// 行 6：缺 step，查询错误，指出 step。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000}`,
		// 行 7：step 为零，查询错误。
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":0}`,
		// 行 8：query 不接受 step。
		`{"op":"query","name":"cpu","start":1000,"end":3000,"step":1000}`,
		// 行 9：倒置区间指出实际边界。
		`{"op":"query_windows","name":"cpu","start":3000,"end":1000,"step":1000}`,
		// 行 10：失败行之后原 query 照常工作，数据未被改变。
		`{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
	}, "\n")

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because some lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 10 {
		t.Fatalf("got %d result lines, want 10: %v", len(lines), lines)
	}

	// 行 2：host=a 只命中第一窗口与第三窗口，空窗口不补零，按起点升序。
	m := decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("line 2 = %v, want ok query_windows", m)
	}
	s0 := m["series"].([]interface{})[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["labels"].(map[string]interface{})["host"] != "a" {
		t.Fatalf("line 2 identity = %v, want cpu{host=a}", s0)
	}
	if _, has := s0["count"]; has {
		t.Fatalf("query_windows series entry must not carry count: %v", s0)
	}
	wins := s0["windows"].([]interface{})
	if len(wins) != 2 {
		t.Fatalf("line 2 windows = %v, want two non-empty windows", wins)
	}
	w0 := wins[0].(map[string]interface{})
	w2 := wins[1].(map[string]interface{})
	if w0["start"].(float64) != 1000 || w0["end"].(float64) != 1999 ||
		w0["count"].(float64) != 2 || w0["average"].(float64) != 3 {
		t.Fatalf("line 2 first window = %v, want [1000,1999] count 2 average 3", w0)
	}
	if w2["start"].(float64) != 3000 || w2["end"].(float64) != 3000 ||
		w2["count"].(float64) != 1 || w2["average"].(float64) != 6 {
		t.Fatalf("line 2 last window = %v, want [3000,3000] count 1 average 6", w2)
	}

	// 行 3：start == end 唯一窗口。
	m = decodeResultLine(t, lines[2])
	wins = m["series"].([]interface{})[0].(map[string]interface{})["windows"].([]interface{})
	if len(wins) != 1 {
		t.Fatalf("line 3 windows = %v, want one", wins)
	}
	w := wins[0].(map[string]interface{})
	if w["start"].(float64) != 3000 || w["end"].(float64) != 3000 || w["average"].(float64) != 6 {
		t.Fatalf("line 3 window = %v, want [3000,3000] average 6", w)
	}

	// 行 4：省略 labels 命中两条序列，次序 host=a 在前、host=b 在后，边界共用。
	m = decodeResultLine(t, lines[3])
	series := m["series"].([]interface{})
	if len(series) != 2 {
		t.Fatalf("line 4 series = %v, want both hosts", series)
	}
	first := series[0].(map[string]interface{})
	second := series[1].(map[string]interface{})
	if first["labels"].(map[string]interface{})["host"] != "a" ||
		second["labels"].(map[string]interface{})["host"] != "b" {
		t.Fatalf("line 4 series order = %v, want host=a then host=b", series)
	}
	fw := first["windows"].([]interface{})
	if len(fw) != 2 {
		t.Fatalf("line 4 host=a windows = %v, want two", fw)
	}
	sw := second["windows"].([]interface{})
	if len(sw) != 1 {
		t.Fatalf("line 4 host=b windows = %v, want one", sw)
	}
	bOnly := sw[0].(map[string]interface{})
	if bOnly["start"].(float64) != 2000 || bOnly["end"].(float64) != 2999 ||
		bOnly["count"].(float64) != 1 || bOnly["average"].(float64) != 10 {
		t.Fatalf("line 4 host=b window = %v, want [2000,2999] average 10", bOnly)
	}

	// 行 5：空 series。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "ok" || m["op"] != "query_windows" || len(m["series"].([]interface{})) != 0 {
		t.Fatalf("line 5 = %v, want ok query_windows with empty series", m)
	}

	// 行 6-9：各类查询错误保留行号且不带 series、index、conflict。
	for i, want := range map[int]string{
		5: `missing required field "step"`,
		6: `"step"`,
		7: `unknown field "step"`,
		8: "3000 > 1000",
	} {
		m = decodeResultLine(t, lines[i])
		if m["status"] != "error" || int(m["line"].(float64)) != i+1 {
			t.Fatalf("line %d = %v, want error on input line %d", i+1, m, i+1)
		}
		msg, _ := m["error"].(string)
		if !strings.Contains(msg, want) {
			t.Fatalf("line %d error = %q, want substring %q", i+1, msg, want)
		}
		for _, banned := range []string{"series", "index", "conflict"} {
			if _, ok := m[banned]; ok {
				t.Fatalf("line %d must not carry %q: %v", i+1, banned, m)
			}
		}
	}

	// 行 10：失败行之后 query 照常，host=a 三个点均值 4。
	m = decodeResultLine(t, lines[9])
	if m["status"] != "ok" || m["op"] != "query" {
		t.Fatalf("line 10 = %v, want ok query", m)
	}
	s0 = m["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 3 || s0["average"].(float64) != 4 {
		t.Fatalf("line 10 = %v, want count 3 average 4", s0)
	}
}

// TestRunIngestQueryWindowsAllSuccessExitsZero 全成功（含无命中）时退出码为 0，
// 空结果序列化为 []。
func TestRunIngestQueryWindowsAllSuccessExitsZero(t *testing.T) {
	input := `{"op":"query_windows","name":"m","start":0,"end":100,"step":10}` + "\n"
	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimRight(out.String(), "\n")
	if got != `{"status":"ok","op":"query_windows","series":[]}` {
		t.Fatalf("empty result = %s, want empty series array", got)
	}
}
