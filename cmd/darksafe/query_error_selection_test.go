package main

import (
	"bytes"
	"strings"
	"testing"
)

// 通过 ingest 按行使用查询时：失败行保留从一开始的原始行号（空白行仍计入行号），
// 失败只产生错误结果（不带 index 与 conflict），后续合法查询继续得到正常结果，
// 出现过失败的输入最终返回非零退出码。
func TestRunIngestQueryFailuresKeepLineNumbersAndExitCode(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`, // 行 1：写入两个点
		``, // 行 2：空白行，无输出但计行号
		`{"op":"query","name":"cpu","start":0,"end":3000,"bogus":1}`, // 行 3：未知字段
		`   `, // 行 4：空白行
		`{"op":"query","name":"cpu","start":0,"end":3000} trailing`,              // 行 5：整行解析失败
		`{"op":"query","name":"cpu","start":3000,"end":0}`,                       // 行 6：倒置区间
		`{"op":"query","name":"cpu","start":0}`,                                  // 行 7：缺少必填字段 end
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`, // 行 8：合法查询
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because query lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d result lines, want 6 (write ok, 4 errors, query ok): %v", len(lines), lines)
	}

	first := decodeResultLine(t, lines[0])
	if first["status"] != "ok" || first["added"].(float64) != 2 {
		t.Fatalf("line 1 = %v, want successful write of two points", first)
	}

	// 四条失败查询：行号从 1 开始连续计数（含空白行），错误原因区分各类问题，
	// 且不带 index 与 conflict。
	wantErrors := []struct {
		line int
		want string
	}{
		{3, `unknown field "bogus"`},
		{5, "invalid JSON"},
		{6, "invalid range"},
		{7, `missing required field "end"`},
	}
	for i, we := range wantErrors {
		m := decodeResultLine(t, lines[1+i])
		if m["status"] != "error" || int(m["line"].(float64)) != we.line {
			t.Fatalf("output %d = %v, want error on input line %d", i+1, m, we.line)
		}
		msg, _ := m["error"].(string)
		if !strings.Contains(msg, we.want) {
			t.Fatalf("output %d error = %q, want substring %q", i+1, msg, we.want)
		}
		if _, hasIndex := m["index"]; hasIndex {
			t.Fatalf("query error on line %d must not carry index: %v", we.line, m)
		}
		if _, hasConflict := m["conflict"]; hasConflict {
			t.Fatalf("query error on line %d must not carry conflict: %v", we.line, m)
		}
	}

	// 失败行之后，合法查询仍得到正常结果：完整标签、点数与平均值不受失败影响。
	q := decodeResultLine(t, lines[5])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 8 = %v, want successful query after failed lines", q)
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %v, want exactly one series", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 2 || s0["average"].(float64) != 3 {
		t.Fatalf("query after failed lines = %v, want count=2 average=3", s0)
	}
	labels := s0["labels"].(map[string]interface{})
	if len(labels) != 1 || labels["host"] != "a" {
		t.Fatalf("query labels = %v, want exactly host=a", labels)
	}
}
