package main

import (
	"strings"
	"testing"
)

// 端到端回归：转义还原后重复的字段名/标签键必须让对应行失败。
// 写入批次失败带从 1 开始的采样点 index、不带 conflict，整批不提交；
// 查询失败不带 index、不带 conflict，也不返回成功结果。命令保留从 1 开始的
// 原始输入行号，失败行之后的合法请求继续处理，出现过失败行时退出码非零。
func TestRunIngestEscapedDuplicateKeysAreLineFailures(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：基线写入成功。
		`[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]`,
		// 行 2：第二个采样点的指标名字段转义还原后重复（两个值还相同），整批失败。
		`[{"name":"mem","timestamp":1,"value":1},{"name":"mem","\u006eame":"mem","timestamp":2,"value":2}]`,
		// 行 3：查询对象顶层字段转义还原后重复，查询错误。
		`{"op":"query","name":"cpu","\u006eame":"cpu","start":0,"end":3000}`,
		// 行 4：第二个采样点的标签键转义还原后重复（两个标签值不同），整批失败。
		`[{"name":"net","timestamp":1,"value":1},{"name":"net","timestamp":2,"value":2,"labels":{"host":"a","\u0068ost":"b"}}]`,
		// 行 5：查询标签条件里的键转义还原后重复，查询错误。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a","\u0068ost":"b"}}`,
		// 行 6：空白行不产生结果，但仍占用行号。
		``,
		// 行 7：此前成功写入的基线仍可查询到原来的数量与均值（只有一个点，0.5）。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
		// 行 8：同一序列同一时间戳同值重提是合法重复采样点，与字段重复失败相区别。
		`[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]`,
		// 行 9：失败行之后的合法写入照常处理。
		`[{"name":"tail","timestamp":1,"value":9}]`,
		// 行 10：新写入的数据可查询。
		`{"op":"query","name":"tail","start":0,"end":10}`,
	}, "\n")

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because duplicate-key lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d output lines, want 9 (blank line produces none): %v", len(lines), lines)
	}

	// 行 1 成功。
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 1 = %v, want ok added 1", m)
	}

	// 行 2：写入批次失败，index=2，原因指出重复字段，无 conflict。
	m := decodeResultLine(t, lines[1])
	if m["status"] != "error" || int(m["line"].(float64)) != 2 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 2 = %v, want error line=2 index=2", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 2 duplicate field must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate field "name"`) {
		t.Fatalf("line 2 error = %q, want duplicate field name", msg)
	}

	// 行 3：查询失败，无 index、无 conflict。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "error" || int(m["line"].(float64)) != 3 {
		t.Fatalf("line 3 = %v, want error on input line 3", m)
	}
	if _, has := m["index"]; has {
		t.Fatalf("line 3 query error must not carry index: %v", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 3 query error must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate field "name"`) {
		t.Fatalf("line 3 error = %q, want duplicate field name", msg)
	}

	// 行 4：写入批次失败，index=2，原因指出重复标签键 host，无 conflict。
	m = decodeResultLine(t, lines[3])
	if m["status"] != "error" || int(m["line"].(float64)) != 4 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 4 = %v, want error line=4 index=2", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 4 duplicate label key must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate label key "host"`) {
		t.Fatalf("line 4 error = %q, want duplicate label key host", msg)
	}

	// 行 5：查询标签键重复失败，无 index、无 conflict。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "error" || int(m["line"].(float64)) != 5 {
		t.Fatalf("line 5 = %v, want error on input line 5", m)
	}
	if _, has := m["index"]; has {
		t.Fatalf("line 5 query error must not carry index: %v", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 5 query error must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate label key "host"`) {
		t.Fatalf("line 5 error = %q, want duplicate label key host", msg)
	}

	// 行 7：失败批次的新增点没有提交；基线仍是一个点、均值 0.5。
	q := decodeResultLine(t, lines[5])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 7 = %v, want ok query", q)
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 7 series = %v, want only the baseline point (mem/net not committed)", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["count"].(float64) != 1 || s0["average"].(float64) != 0.5 {
		t.Fatalf("line 7 series[0] = %v, want cpu count=1 average=0.5", s0)
	}

	// 行 8：同值重提按重复采样点成功忽略，计入 duplicates 而不是失败。
	m = decodeResultLine(t, lines[6])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 8 = %v, want ok added=0 duplicates=1", m)
	}

	// 行 9：合法新写入成功，快照只有 cpu 与 tail 两条序列（mem/net 从未提交）。
	m = decodeResultLine(t, lines[7])
	if m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 9 = %v, want ok added=1", m)
	}
	gotSeries := m["series"].([]interface{})
	if len(gotSeries) != 2 {
		t.Fatalf("line 9 snapshot = %v, want exactly cpu and tail series", gotSeries)
	}

	// 行 10：tail 可查询。
	q = decodeResultLine(t, lines[8])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 10 = %v, want ok query", q)
	}
	series = q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 10 series = %v, want exactly tail", series)
	}
	s0 = series[0].(map[string]interface{})
	if s0["name"] != "tail" || s0["count"].(float64) != 1 || s0["average"].(float64) != 9 {
		t.Fatalf("line 10 series[0] = %v, want tail count=1 average=9", s0)
	}
}

// 只出现一次的转义字段名/标签键与直接书写同名同义：端到端可正常写入与查询。
func TestRunIngestEscapedKeyAppearingOnceStillWorks(t *testing.T) {
	input := strings.Join([]string{
		`[{"\u006eame":"m","timestamp":1,"value":7,"labels":{"\u0068ost":"a"}}]`,
		`{"op":"query","\u006eame":"m","start":0,"end":10,"labels":{"\u0068ost":"a"}}`,
	}, "\n")
	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 for valid escaped-once keys", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("write = %v, want ok added=1", m)
	}
	q := decodeResultLine(t, lines[1])
	series := q["series"].([]interface{})
	if len(series) != 1 || series[0].(map[string]interface{})["average"].(float64) != 7 {
		t.Fatalf("query = %v, want one series average 7", q)
	}
}
