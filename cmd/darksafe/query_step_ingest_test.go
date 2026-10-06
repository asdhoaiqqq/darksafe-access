package main

import (
	"strings"
	"testing"
)

// 本文件从命令行入口回归保障 ingest 中查询对象的 step 字段判定：step 只是
// query_windows 的窗口宽度，普通均值查询与采样明细查询携带它时——无论写在
// op 前后、值是正整数、零、字符串还是对象——只输出一条 unknown field "step"
// 错误（带原始行号，不带 series、index、conflict），已写入的采样点不变，
// 后续合法输入继续处理，整个调用以非零退出码结束。

// TestRunIngestQueryStepUnknownFieldEndToEnd step 为零写在最前的普通查询被
// 明确拒绝；删掉 step 后同一查询正常执行，失败行不影响已保存的数据。
func TestRunIngestQueryStepUnknownFieldEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：写入基线数据。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：step 为零写在最前的普通均值查询：未知字段，不提示窗口宽度。
		`{"step":0,"op":"query","name":"cpu","start":0,"end":2000}`,
		// 行 3：step 为字符串、写在 op 之前的采样明细查询：同样是未知字段。
		`{"step":"1000","op":"query_points","name":"cpu","start":0,"end":2000}`,
		// 行 4：step 为对象、写在 op 之后：仍是未知字段。
		`{"op":"query","name":"cpu","start":0,"end":2000,"step":{}}`,
		// 行 5：删掉 step 后原查询正常执行，行 1 的数据原样可见。
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`,
		// 行 6：窗口查询携带合法 step 照常工作。
		`{"step":1000,"op":"query_windows","name":"cpu","start":0,"end":2000}`,
	}, "\n")

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d result lines, want 6: %v", len(lines), lines)
	}

	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" {
		t.Fatalf("line 1 = %v, want ok write", m)
	}

	// 行 2-4：各自只输出一条带原始行号的 unknown field "step"，
	// 不带 series、index、conflict。
	for _, wantLine := range []int{2, 3, 4} {
		m := decodeResultLine(t, lines[wantLine-1])
		if m["status"] != "error" {
			t.Fatalf("line %d = %v, want error", wantLine, m)
		}
		if int(m["line"].(float64)) != wantLine {
			t.Fatalf("line %d reports line %v, want original line number", wantLine, m["line"])
		}
		if m["error"].(string) != `unknown field "step"` {
			t.Fatalf("line %d error = %q, want exactly %q", wantLine, m["error"], `unknown field "step"`)
		}
		for _, banned := range []string{"series", "index", "conflict"} {
			if _, ok := m[banned]; ok {
				t.Fatalf("line %d error must not carry %q: %v", wantLine, banned, m)
			}
		}
	}

	// 行 5：删掉 step 的同一查询成功，此前保存的采样点未被失败行改变。
	m := decodeResultLine(t, lines[4])
	if m["status"] != "ok" || m["op"] != "query" {
		t.Fatalf("line 5 = %v, want ok query", m)
	}
	s0 := m["series"].([]interface{})[0].(map[string]interface{})
	if int(s0["count"].(float64)) != 1 || s0["average"].(float64) != 2 {
		t.Fatalf("line 5 = %v, want count 1 average 2", s0)
	}

	// 行 6：窗口查询的合法 step 写在 op 之前仍被接受。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "ok" || m["op"] != "query_windows" {
		t.Fatalf("line 6 = %v, want ok query_windows", m)
	}
}
