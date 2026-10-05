package main

import (
	"strings"
	"testing"
)

// ingest 层面的 query_points 行为：成功行输出 op 为 query_points 的明细结果；
// 失败行（倒置区间）不带 series、index、conflict，后续行继续处理，
// 命令最终以非零退出码结束。
func TestRunIngestQueryPointsEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":3000,"value":9,"labels":{"host":"a"}}]`,
		`{"op":"query_points","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`,
		`{"op":"query_points","name":"cpu","start":5000,"end":1000}`,
		`{"op":"query_points","name":"cpu","start":3000,"end":3000}`,
		`{"op":"query_points","name":"mem","start":0,"end":9000}`,
		`{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
	}, "\n") + "\n"

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero after a failed line")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("output lines = %d, want 6:\n%s", len(lines), out.String())
	}

	// 区间内只列出 1000、2000 两个点，3000 上的点不出现；无 count/average。
	got := decodeResultLine(t, lines[1])
	if got["status"] != "ok" || got["op"] != "query_points" {
		t.Fatalf("line 2 = %v, want ok query_points", got)
	}
	series := got["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 2 series = %v, want one series", series)
	}
	s0 := series[0].(map[string]interface{})
	if _, has := s0["count"]; has {
		t.Fatalf("line 2 series entry must not carry count: %v", s0)
	}
	if _, has := s0["average"]; has {
		t.Fatalf("line 2 series entry must not carry average: %v", s0)
	}
	pts := s0["points"].([]interface{})
	if len(pts) != 2 {
		t.Fatalf("line 2 points = %v, want the two in-range points", pts)
	}
	p0 := pts[0].(map[string]interface{})
	p1 := pts[1].(map[string]interface{})
	if p0["timestamp"] != float64(1000) || p0["value"] != float64(2) ||
		p1["timestamp"] != float64(2000) || p1["value"] != float64(4) {
		t.Fatalf("line 2 points = %v, want (1000,2) and (2000,4)", pts)
	}

	// 倒置区间：错误指出实际边界，不带 series、index、conflict。
	got = decodeResultLine(t, lines[2])
	if got["status"] != "error" || got["line"] != float64(3) {
		t.Fatalf("line 3 = %v, want error on line 3", got)
	}
	if !strings.Contains(got["error"].(string), "5000 > 1000") {
		t.Fatalf("line 3 error = %v, want actual bounds", got["error"])
	}
	for _, key := range []string{"series", "index", "conflict"} {
		if _, has := got[key]; has {
			t.Fatalf("line 3 failure must not carry %q: %v", key, got)
		}
	}

	// 失败行之后继续处理：start == end 只返回该时间戳上的点。
	got = decodeResultLine(t, lines[3])
	s0 = got["series"].([]interface{})[0].(map[string]interface{})
	pts = s0["points"].([]interface{})
	if len(pts) != 1 || pts[0].(map[string]interface{})["timestamp"] != float64(3000) {
		t.Fatalf("line 4 points = %v, want only the point at 3000", pts)
	}

	// 指标不存在：成功返回空 series 数组。
	got = decodeResultLine(t, lines[4])
	if got["status"] != "ok" || len(got["series"].([]interface{})) != 0 {
		t.Fatalf("line 5 = %v, want ok with empty series", got)
	}

	// 原有 query 的格式与均值语义不变。
	got = decodeResultLine(t, lines[5])
	s0 = got["series"].([]interface{})[0].(map[string]interface{})
	if got["op"] != "query" || s0["count"] != float64(3) || s0["average"] != float64(5) {
		t.Fatalf("line 6 = %v, want query count 3 average 5", got)
	}
}
