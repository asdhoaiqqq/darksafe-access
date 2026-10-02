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
	for _, want := range []string{"ingest", "standard input", "json array", "added", "duplicates", "non-zero"} {
		if !strings.Contains(strings.ToLower(text), want) {
			t.Errorf("help text missing %q", want)
		}
	}
}

func TestRunIngestInterleavedQuery(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,         // 行 1：写入
		`{"op":"query","name":"cpu","start":1000,"end":1000,"labels":{"host":"a"}}`, // 行 2：查询，avg=2
		``, // 行 3：空白行
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,                  // 行 4：再写入
		`{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`,          // 行 5：avg=(2+4)/2=3
		`{"op":"query","name":"cpu","start":3000,"end":4000}`,                                // 行 6：无点，空数组
		`{"op":"bogus","name":"cpu","start":0,"end":1}`,                                      // 行 7：未知 op，失败
		`{"op":"query","name":"cpu","start":9,"end":1}`,                                      // 行 8：非法区间，失败
		`{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}} trailing`, // 行 9：整行解析失败
		`{"op":"query","name":"cpu","start":1000,"end":2000}`,                                // 行 10：仍可查询，结果不受失败行影响
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d result lines, want 9: %v", len(lines), lines)
	}

	checkQuery := func(idx int, wantCount float64, wantAvg float64) {
		t.Helper()
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(lines[idx]), &m); err != nil {
			t.Fatalf("output line %d: %v", idx, err)
		}
		if m["status"] != "ok" || m["op"] != "query" {
			t.Fatalf("output line %d = %v, want ok query", idx, m)
		}
		series := m["series"].([]interface{})
		if wantCount == 0 {
			if len(series) != 0 {
				t.Fatalf("output line %d series = %v, want empty", idx, series)
			}
			return
		}
		if len(series) != 1 {
			t.Fatalf("output line %d series = %v, want one entry", idx, series)
		}
		s0 := series[0].(map[string]interface{})
		if s0["name"] != "cpu" || s0["count"].(float64) != wantCount || s0["average"].(float64) != wantAvg {
			t.Fatalf("output line %d series[0] = %v", idx, s0)
		}
		labels := s0["labels"].(map[string]interface{})
		if labels["host"] != "a" || len(labels) != 1 {
			t.Fatalf("output line %d labels = %v", idx, labels)
		}
	}

	var line1 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &line1); err != nil {
		t.Fatal(err)
	}
	if line1["status"] != "ok" || line1["added"].(float64) != 1 {
		t.Fatalf("line 1 = %v", line1)
	}
	checkQuery(1, 1, 2)

	var line4 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &line4); err != nil {
		t.Fatal(err)
	}
	if line4["status"] != "ok" || line4["added"].(float64) != 1 {
		t.Fatalf("line 4 = %v", line4)
	}
	checkQuery(3, 2, 3)
	checkQuery(4, 0, 0)

	for idx, wantLine := range map[int]int{5: 7, 6: 8, 7: 9} {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(lines[idx]), &m); err != nil {
			t.Fatalf("output line %d: %v", idx, err)
		}
		if m["status"] != "error" || int(m["line"].(float64)) != wantLine {
			t.Fatalf("output line %d = %v, want error on input line %d", idx, m, wantLine)
		}
		if _, hasIndex := m["index"]; hasIndex {
			t.Fatalf("query errors must not carry index: %v", m)
		}
	}

	// 失败行之后，此前数据仍可查询。
	checkQuery(8, 2, 3)
}

func TestRunIngestQueryErrorLineNumbers(t *testing.T) {
	input := strings.Join([]string{
		``,    // 行 1：空白
		`   `, // 行 2：空白
		`{"op":"query","name":"m","start":2,"end":1}`, // 行 3：非法区间
		`not json`, // 行 4：整行解析失败
	}, "\n")
	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	for i, wantLine := range []int{3, 4} {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if int(m["line"].(float64)) != wantLine {
			t.Fatalf("output %d = %v, want input line %d", i, m, wantLine)
		}
	}
}
