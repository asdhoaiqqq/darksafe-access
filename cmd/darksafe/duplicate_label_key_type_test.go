package main

import (
	"strings"
	"testing"
)

// 端到端回归：labels 中先出现合法字符串值的标签键、再次出现时第二次的值是
// 数字/布尔/null/对象/数组，错误必须是重复标签键而不是“标签值必须是字符串”。
// 写入批次失败带从 1 开始的采样点 index、不带 conflict，整批新增点不提交；
// 查询失败不带 index/conflict 也不返回结果。原始行号保留，失败后后续行继续
// 处理，出现过失败行时退出码非零。
func TestRunIngestDuplicateLabelKeyNotMaskedByValueType(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：基线 cpu{host=a} 时间戳 1000 值 2。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：先放入同序列时间戳 2000 值 4 的新增点，再放标签键重复（第二次值是数字）的点。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":3000,"value":5,"labels":{"host":"a","host":1}}]`,
		// 行 3：第二次标签值为布尔值。
		`[{"name":"cpu","timestamp":4000,"value":6,"labels":{"host":"a","host":true}}]`,
		// 行 4：查询标签条件重复，第二次值是 null。
		`{"op":"query","name":"cpu","start":0,"end":5000,"labels":{"host":"a","host":null}}`,
		// 行 5：空白行不产生结果，但仍占行号。
		``,
		// 行 6：此前数据仍是原来的一个点，count=1、average=2。
		`{"op":"query","name":"cpu","start":0,"end":5000,"labels":{"host":"a"}}`,
		// 行 7：失败行之后的合法写入照常处理。
		`[{"name":"tail","timestamp":1,"value":9}]`,
		// 行 8：新写入的数据可查询。
		`{"op":"query","name":"tail","start":0,"end":10}`,
	}, "\n")

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because duplicate-label-key lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d output lines, want 7 (blank line produces none): %v", len(lines), lines)
	}

	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 1 = %v, want ok added 1", m)
	}

	// 行 2：写入失败 index=2，原因是重复标签键，不带 conflict（不能是类型错误）。
	m := decodeResultLine(t, lines[1])
	if m["status"] != "error" || int(m["line"].(float64)) != 2 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 2 = %v, want error line=2 index=2", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 2 must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate label key "host"`) {
		t.Fatalf("line 2 error = %q, want duplicate label key host, not a type error", msg)
	}

	// 行 3：第二次值是布尔值同样报重复标签键。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "error" || int(m["line"].(float64)) != 3 || int(m["index"].(float64)) != 1 {
		t.Fatalf("line 3 = %v, want error line=3 index=1", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 3 must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate label key "host"`) {
		t.Fatalf("line 3 error = %q, want duplicate label key host", msg)
	}

	// 行 4：查询失败，无 index、无 conflict、无结果序列。
	m = decodeResultLine(t, lines[3])
	if m["status"] != "error" || int(m["line"].(float64)) != 4 {
		t.Fatalf("line 4 = %v, want error on input line 4", m)
	}
	if _, has := m["index"]; has {
		t.Fatalf("line 4 query error must not carry index: %v", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 4 query error must not carry conflict: %v", m)
	}
	if _, has := m["series"]; has {
		t.Fatalf("line 4 failed query must not return series: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate label key "host"`) {
		t.Fatalf("line 4 error = %q, want duplicate label key host", msg)
	}

	// 行 6：行 2 的时间戳 2000 新增点没有提交；只有基线一个点，count=1、average=2。
	q := decodeResultLine(t, lines[4])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 6 = %v, want ok query", q)
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 6 series = %v, want only the baseline point", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["count"].(float64) != 1 || s0["average"].(float64) != 2 {
		t.Fatalf("line 6 series[0] = %v, want cpu count=1 average=2", s0)
	}

	// 行 7：失败行之后的合法写入成功。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 7 = %v, want ok added=1", m)
	}

	// 行 8：tail 可查询。
	q = decodeResultLine(t, lines[6])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 8 = %v, want ok query", q)
	}
	series = q["series"].([]interface{})
	if len(series) != 1 || series[0].(map[string]interface{})["average"].(float64) != 9 {
		t.Fatalf("line 8 series = %v, want exactly tail average 9", series)
	}
}
