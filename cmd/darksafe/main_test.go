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
