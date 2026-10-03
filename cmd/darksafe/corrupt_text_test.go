package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// 含非法 UTF-8 字节的输入行（name 中夹一个 0xFF）。
var rawBadUTF8Write = []byte(`[{"name":"cpu` + "\xff" + `","timestamp":1000,"value":1}]`)

// TestRunIngestCorruptTextIsWholeLineFailure 覆盖端到端语义：
// 损坏行只产出一条带原始行号、无 index/conflict 的 error；此前数据保留，
// 后续写入与查询照常；损坏查询不返回替换后的匹配；进程非零退出。
func TestRunIngestCorruptTextIsWholeLineFailure(t *testing.T) {
	input := io.MultiReader(
		// 行 1：先成功写入一个真实的 cpu 序列与一个显式 U+FFFD 序列。
		strings.NewReader(`[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]`+"\n"),
		strings.NewReader(`[{"name":"m�","timestamp":1,"value":7}]`+"\n"),
		// 行 3：非法 UTF-8 字节的写入，整行失败。
		bytes.NewReader(append(append([]byte(nil), rawBadUTF8Write...), '\n')),
		// 行 4：孤立代理项转义的写入，整行失败，且不得与 U+FFFD 序列合并。
		strings.NewReader(`[{"name":"m\uD800","timestamp":1,"value":9}]`+"\n"),
		// 行 5：孤立代理项转义的查询，失败，不得经替换命中真实 U+FFFD 序列。
		strings.NewReader(`{"op":"query","name":"m\uD800","start":0,"end":10}`+"\n"),
		// 行 6：非法字节查询，失败。
		bytes.NewReader([]byte(`{"op":"query","name":"cpu`+"\xff"+`","start":0,"end":10}`+"\n")),
		// 行 7：合法查询此前数据，仍应正常得到行 1 的点。
		strings.NewReader(`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`+"\n"),
		// 行 8：合法补充平面写入，确认后续输入按序处理且行为不变。
		strings.NewReader(`[{"name":"😀","timestamp":1,"value":1}]`+"\n"),
	)

	var out bytes.Buffer
	if code := runIngest(input, &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero because corrupt lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("got %d output lines, want 8 (3 writes ok, 4 errors, 1 query ok): %v", len(lines), lines)
	}

	// 行 1、行 2 成功。
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" {
		t.Fatalf("line 1 = %v, want ok", m)
	}
	if m := decodeResultLine(t, lines[1]); m["status"] != "ok" {
		t.Fatalf("line 2 = %v, want ok", m)
	}

	// 错误行：行号 3-6 全部为整行错误，无 index、无 conflict，原因可区分两类。
	wantErr := []struct {
		line   int
		substr string
	}{
		{3, "UTF-8"},
		{4, "surrogate"},
		{5, "surrogate"},
		{6, "UTF-8"},
	}
	for i, w := range wantErr {
		m := decodeResultLine(t, lines[2+i])
		if m["status"] != "error" || int(m["line"].(float64)) != w.line {
			t.Fatalf("output %d = %v, want error on input line %d", i+2, m, w.line)
		}
		if _, has := m["index"]; has {
			t.Fatalf("line %d error must not carry index: %v", w.line, m)
		}
		if _, has := m["conflict"]; has {
			t.Fatalf("line %d error must not carry conflict: %v", w.line, m)
		}
		msg, _ := m["error"].(string)
		if !strings.Contains(msg, w.substr) {
			t.Fatalf("line %d error = %q, want substring %q", w.line, msg, w.substr)
		}
	}

	// 行 7：此前提交的数据仍可查询，只有行 1 的一个点（平均 0.5）。
	q := decodeResultLine(t, lines[6])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 7 = %v, want ok query after corrupt lines", q)
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 7 series = %v, want exactly the earlier cpu point", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 0.5 {
		t.Fatalf("line 7 series[0] = %v, want count=1 average=0.5", s0)
	}

	// 行 8：合法补充平面写入仍被正常处理。
	if m := decodeResultLine(t, lines[7]); m["status"] != "ok" {
		t.Fatalf("line 8 = %v, want ok supplementary-plane write after failures", m)
	}
}

// TestRunIngestCorruptLineDoesNotCommitValidPrefix 即使数组前面的点完全合法，
// 含损坏文本的整行也不得提交任何点；命令非零退出，且后续空批快照不含该序列。
func TestRunIngestCorruptLineDoesNotCommitValidPrefix(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"old","timestamp":1,"value":9}]`,
		`[{"name":"brandnew","timestamp":1,"value":1},{"name":"bad\uD800","timestamp":2,"value":2}]`,
		`[]`,
	}, "\n")
	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %v", len(lines), lines)
	}
	mid := decodeResultLine(t, lines[1])
	if mid["status"] != "error" || int(mid["line"].(float64)) != 2 {
		t.Fatalf("line 2 = %v, want error on line 2", mid)
	}
	if _, has := mid["index"]; has {
		t.Fatalf("corrupt line must fail as a whole, no index: %v", mid)
	}
	snap := decodeResultLine(t, lines[2])
	gotSeries := snap["series"].([]interface{})
	if len(gotSeries) != 1 {
		t.Fatalf("snapshot = %v, want only old series, no brandnew commit", gotSeries)
	}
	name := gotSeries[0].(map[string]interface{})["name"].(string)
	if name != "old" {
		t.Fatalf("committed series name = %q, want old", name)
	}
}
