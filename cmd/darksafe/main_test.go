package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunIngestEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":1}]`, // 行 1：成功
		``,         // 行 2：空白行，无输出但计行号
		`   `,      // 行 3：空白行
		`not json`, // 行 4：整行解析失败
		`[{"name":"cpu","timestamp":1000,"value":2}]`,   // 行 5：冲突（此前数据保留）
		`[{"name":"cpu","timestamp":1000,"value":1.0}]`, // 行 6：重复成功
		`[]`, // 行 7：空批成功
		``,   // 行 8：结尾空白行
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because batches failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d result lines, want 5: %v", len(lines), lines)
	}

	var ok1 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &ok1); err != nil {
		t.Fatal(err)
	}
	if ok1["status"] != "ok" {
		t.Fatalf("line 1 status = %v", ok1["status"])
	}

	var err4 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &err4); err != nil {
		t.Fatal(err)
	}
	if err4["status"] != "error" || int(err4["line"].(float64)) != 4 {
		t.Fatalf("second output line = %v, want parse error on line 4", err4)
	}
	if _, hasIndex := err4["index"]; hasIndex {
		t.Fatalf("whole-line parse error must not carry index: %v", err4)
	}

	var err5 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &err5); err != nil {
		t.Fatal(err)
	}
	if err5["status"] != "error" || int(err5["line"].(float64)) != 5 {
		t.Fatalf("third output line = %v, want conflict error on line 5", err5)
	}
	if int(err5["index"].(float64)) != 1 {
		t.Fatalf("conflict index = %v, want 1", err5["index"])
	}
	conflict := err5["conflict"].(map[string]interface{})
	if conflict["existing"].(float64) != 1 || conflict["submitted"].(float64) != 2 {
		t.Fatalf("conflict values = %v", conflict)
	}

	var ok6 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[3]), &ok6); err != nil {
		t.Fatal(err)
	}
	if ok6["status"] != "ok" || ok6["duplicates"].(float64) != 1 || ok6["added"].(float64) != 0 {
		t.Fatalf("line 6 = %v, want duplicate success", ok6)
	}

	var ok7 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[4]), &ok7); err != nil {
		t.Fatal(err)
	}
	if ok7["status"] != "ok" {
		t.Fatalf("line 7 = %v", ok7)
	}
	// 失败批次均已回滚，最终只有行 1 写入的一条序列。
	series := ok7["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("final series = %v, want exactly 1", series)
	}
}

func TestRunIngestAllSuccessExitsZero(t *testing.T) {
	input := `[{"name":"m","timestamp":1,"value":1}]` + "\n" + `[]` + "\n"
	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if n := len(strings.Split(strings.TrimRight(out.String(), "\n"), "\n")); n != 2 {
		t.Fatalf("got %d output lines, want 2", n)
	}
}

func TestRunIngestEmptyInput(t *testing.T) {
	var out bytes.Buffer
	if code := runIngest(strings.NewReader(""), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Fatalf("empty input must produce no output, got %q", out.String())
	}
}

func TestUsageMentionsIngest(t *testing.T) {
	var b bytes.Buffer
	usage(&b)
	text := b.String()
	for _, want := range []string{"ingest", "standard input", "json array", "added", "duplicates", "non-zero", "query", "op", "start", "end", "average"} {
		if !strings.Contains(strings.ToLower(text), want) {
			t.Errorf("help text missing %q", want)
		}
	}
}

func TestRunIngestInterleavesWritesAndQueries(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`, // 行 1：写入
		`{"op":"query","name":"cpu","start":1000,"end":1000}`,               // 行 2：查询
		``,                                                                    // 行 3：空白
		`{"op":"query","name":"cpu","start":1000,"end":1000}`,               // 行 4：连续查询一致
		`[{"name":"cpu","timestamp":2000,"value":3,"labels":{"host":"a"}}]`, // 行 5：再写入
		`{"op":"query","name":"cpu","start":1000,"end":2000}`,               // 行 6：区间均值
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d output lines, want 5: %v", len(lines), lines)
	}

	var q2 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &q2); err != nil {
		t.Fatal(err)
	}
	if q2["status"] != "ok" || q2["op"] != "query" {
		t.Fatalf("line 2 = %v, want query ok", q2)
	}
	s2 := q2["series"].([]interface{})
	if len(s2) != 1 {
		t.Fatalf("line 2 series = %v, want 1", s2)
	}
	avg2 := s2[0].(map[string]interface{})["average"].(float64)
	if avg2 != 1 {
		t.Fatalf("line 2 average = %v, want 1", avg2)
	}

	// 连续查询结果一致。
	var q4 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &q4); err != nil {
		t.Fatal(err)
	}
	avg4 := q4["series"].([]interface{})[0].(map[string]interface{})["average"].(float64)
	if avg4 != avg2 {
		t.Fatalf("consecutive query averages differ: %v vs %v", avg4, avg2)
	}

	var q6 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[4]), &q6); err != nil {
		t.Fatal(err)
	}
	s6 := q6["series"].([]interface{})[0].(map[string]interface{})
	if s6["count"].(float64) != 2 || s6["average"].(float64) != 2 {
		t.Fatalf("line 6 = %v, want count 2 average 2", s6)
	}
}

func TestRunIngestQueryErrorsCarryLineAndNoIndex(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"m","timestamp":1,"value":1}]`, // 行 1
		`{"op":"query","name":"m","start":2,"end":1}`, // 行 2：非法区间
		`{"op":"query","name":"m","start":1,"end":1}`, // 行 3：正常
		`{"op":"bogus","name":"m","start":1,"end":1}`, // 行 4：未知 op
		`{"op":"query","name":"m","start":1,"end":1,"bad":1}`, // 行 5：未知字段
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because query lines failed")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d output lines, want 5: %v", len(lines), lines)
	}

	checkErr := func(idx, wantLine int, wantSub string) {
		t.Helper()
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(lines[idx]), &m); err != nil {
			t.Fatal(err)
		}
		if m["status"] != "error" {
			t.Fatalf("line %d: status = %v, want error", wantLine, m["status"])
		}
		if int(m["line"].(float64)) != wantLine {
			t.Fatalf("line %d: line number = %v", wantLine, m["line"])
		}
		if _, hasIndex := m["index"]; hasIndex {
			t.Fatalf("line %d: query error must not carry index: %v", wantLine, m)
		}
		if !strings.Contains(m["error"].(string), wantSub) {
			t.Fatalf("line %d: error = %q, want substring %q", wantLine, m["error"], wantSub)
		}
	}
	checkErr(1, 2, "invalid range")
	checkErr(3, 4, "unknown op")
	checkErr(4, 5, "unknown field")

	// 行 3 是夹在失败之间的成功查询，证明失败后继续处理。
	var q3 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &q3); err != nil {
		t.Fatal(err)
	}
	if q3["status"] != "ok" || q3["op"] != "query" {
		t.Fatalf("line 3 = %v, want query ok", q3)
	}
}

func TestRunIngestQueryNoMatchAndEmptyLabels(t *testing.T) {
	input := `{"op":"query","name":"ghost","start":1,"end":1}` + "\n"
	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var qr map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &qr); err != nil {
		t.Fatal(err)
	}
	if qr["status"] != "ok" || qr["op"] != "query" {
		t.Fatalf("envelope = %v", qr)
	}
	series := qr["series"].([]interface{})
	if len(series) != 0 {
		t.Fatalf("series = %v, want empty array", series)
	}
}
