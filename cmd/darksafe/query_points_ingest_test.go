package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件从命令行入口回归保障 ingest 的 query_points 操作：对象行 op 写为
// query_points 时沿用查询条件返回区间内原始采样明细；失败行之后继续处理
// 后续行，并最终返回非零退出码。

// TestRunIngestQueryPointsEndToEnd 写入、均值查询与明细查询交替进入同一次
// ingest：query_points 逐行输出 status 为 ok、op 为 query_points 的结果，
// 序列保留完整标签，points 按时间戳升序；倒置区间失败不影响后续行。
func TestRunIngestQueryPointsEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：写入 cpu host=a 的三个点（故意乱序）与 host=b 的一个点。
		`[{"name":"cpu","timestamp":3000,"value":9,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1500,"value":7,"labels":{"host":"b"}}]`,
		// 行 2：明细查询 [1000,2000] host=a：只列出前两个点，3000 不出现。
		`{"op":"query_points","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`,
		// 行 3：start == end，只返回该时间戳上的采样。
		`{"op":"query_points","name":"cpu","start":3000,"end":3000,"labels":{"host":"a"}}`,
		// 行 4：省略 labels，命中该指标全部序列，沿用既有排列次序。
		`{"op":"query_points","name":"cpu","start":0,"end":4000}`,
		// 行 5：区间内没有点，成功返回空 series 数组。
		`{"op":"query_points","name":"cpu","start":4000,"end":5000}`,
		// 行 6：倒置区间，失败并指出实际边界。
		`{"op":"query_points","name":"cpu","start":5000,"end":1000}`,
		// 行 7：失败行之后均值查询照常工作，数据未被改变。
		`{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
	}, "\n")

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because a line failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d result lines, want 7: %v", len(lines), lines)
	}

	// 行 2：[1000,2000] 只含 (1000,2) 与 (2000,4)，按时间戳升序。
	m := decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["op"] != "query_points" {
		t.Fatalf("line 2 = %v, want ok query_points", m)
	}
	series := m["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 2 series = %v, want one entry", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["labels"].(map[string]interface{})["host"] != "a" {
		t.Fatalf("line 2 series identity = %v, want cpu{host=a}", s0)
	}
	if _, hasCount := s0["count"]; hasCount {
		t.Fatalf("query_points entry must not carry count: %v", s0)
	}
	if _, hasAvg := s0["average"]; hasAvg {
		t.Fatalf("query_points entry must not carry average: %v", s0)
	}
	points := s0["points"].([]interface{})
	if len(points) != 2 {
		t.Fatalf("line 2 points = %v, want the two in-range points", points)
	}
	p0 := points[0].(map[string]interface{})
	p1 := points[1].(map[string]interface{})
	if p0["timestamp"].(float64) != 1000 || p0["value"].(float64) != 2 ||
		p1["timestamp"].(float64) != 2000 || p1["value"].(float64) != 4 {
		t.Fatalf("line 2 points = %v, want (1000,2) then (2000,4)", points)
	}

	// 行 3：start == end 只列出 3000 上的点。
	m = decodeResultLine(t, lines[2])
	points = m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 1 || points[0].(map[string]interface{})["value"].(float64) != 9 {
		t.Fatalf("line 3 points = %v, want only (3000,9)", points)
	}

	// 行 4：省略 labels 命中全部序列，host=a 在前、host=b 在后；
	// host=a 的三个点按时间戳升序。
	m = decodeResultLine(t, lines[3])
	series = m["series"].([]interface{})
	if len(series) != 2 {
		t.Fatalf("line 4 series = %v, want both host=a and host=b", series)
	}
	first := series[0].(map[string]interface{})
	second := series[1].(map[string]interface{})
	if first["labels"].(map[string]interface{})["host"] != "a" ||
		second["labels"].(map[string]interface{})["host"] != "b" {
		t.Fatalf("line 4 series order = %v, want host=a then host=b", series)
	}
	points = first["points"].([]interface{})
	wantTS := []float64{1000, 2000, 3000}
	if len(points) != 3 {
		t.Fatalf("line 4 host=a points = %v, want 3 ascending points", points)
	}
	for i, ts := range wantTS {
		if points[i].(map[string]interface{})["timestamp"].(float64) != ts {
			t.Fatalf("line 4 point %d = %v, want timestamp %v", i, points[i], ts)
		}
	}

	// 行 5：区间内没有点，成功的空 series 数组。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "ok" || m["op"] != "query_points" {
		t.Fatalf("line 5 = %v, want ok query_points", m)
	}
	if len(m["series"].([]interface{})) != 0 {
		t.Fatalf("line 5 series = %v, want empty array", m["series"])
	}

	// 行 6：倒置区间失败，指出实际边界，不带 series、index、conflict。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "error" || int(m["line"].(float64)) != 6 {
		t.Fatalf("line 6 = %v, want error on input line 6", m)
	}
	msg, _ := m["error"].(string)
	if !strings.Contains(msg, "5000 > 1000") {
		t.Fatalf("line 6 error = %q, want actual bounds 5000 > 1000", msg)
	}
	for _, banned := range []string{"series", "index", "conflict"} {
		if _, ok := m[banned]; ok {
			t.Fatalf("line 6 error must not carry %q: %v", banned, m)
		}
	}

	// 行 7：失败行之后均值查询照常，数据未被前面的操作改变。
	m = decodeResultLine(t, lines[6])
	if m["status"] != "ok" || m["op"] != "query" {
		t.Fatalf("line 7 = %v, want ok query", m)
	}
	s0 = m["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 3 || s0["average"].(float64) != 5 {
		t.Fatalf("line 7 = %v, want count 3 average 5", s0)
	}
}

// TestRunIngestQueryPointsAllSuccessExitsZero 全部行为成功的 query_points
// 时退出码为 0；空结果也序列化为 []。
func TestRunIngestQueryPointsAllSuccessExitsZero(t *testing.T) {
	input := `{"op":"query_points","name":"m","start":0,"end":100}` + "\n"
	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimRight(out.String(), "\n")
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("invalid JSON result %q: %v", got, err)
	}
	if m["status"] != "ok" || m["op"] != "query_points" {
		t.Fatalf("result = %v, want ok query_points", m)
	}
	series, ok := m["series"].([]interface{})
	if !ok || len(series) != 0 {
		t.Fatalf("series = %v, want empty array (not null)", m["series"])
	}
}
