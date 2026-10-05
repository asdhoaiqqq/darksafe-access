package main

import (
	"bytes"
	"strings"
	"testing"
)

// 端到端回归保障：同一进程内先成功写入基线点（cpu、host=a、ts=1000、value=2），
// 随后各失败批次的“用户收到的失败原因”与“数据状态”必须同时正确：
//
//   - 数值冲突与字段类型错误出现在不同采样点时，只报告数组位置更靠前的那一个；
//     冲突结果带完整 conflict（指标名、完整标签、时间戳、existing/submitted），
//     字段错误不附带 conflict，交换两类问题点的位置后原因随之切换。
//   - 同一采样点同时具备冲突条件与未知字段 bogus 时，无论 bogus 写在何处，
//     都报未知字段而非冲突。
//   - 数组未闭合只产生整行解析错误，不带 index 与 conflict，即使行内前面的
//     采样点已具备冲突条件。
//
// 每个失败批次只输出一条带原始输入行号的 error（index 从 1 开始），不夹带
// 成功结果或任何部分统计；失败批次前面的新增点不留下、已存值不被覆盖，
// 失败后查询仍只见基线点；之后的合法批次正常新增。输入中出现过失败行时，
// 命令最终返回非零退出码。
func TestRunIngestBatchConflictSelectionEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：先成功写入基线点。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：第一个点是 ts=2000 的合法新增，第二个点在 ts=1000 冲突，
		// 第三个点 name 为数字。只报告第二个点的冲突。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}},` +
			`{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}}]`,
		// 行 3：交换后两个点：第二个点 name 类型错误，第三个点才是冲突。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
			`{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,
		// 行 4：同一采样点既冲突又带未知字段 bogus（bogus 在所有已知字段之后）。
		`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"},"bogus":1}]`,
		// 行 5：同样的采样点，name/timestamp/value 都写在 bogus 前面，labels 在其后。
		`[{"name":"cpu","timestamp":1000,"value":9,"bogus":1,"labels":{"host":"a"}}]`,
		// 行 6：数组未闭合，前面的点已具备冲突条件：只能是整行解析错误。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}`,
		// 行 7：失败批次均回滚后，查询覆盖两个时间戳仍只见基线点。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
		// 行 8：提交 ts=2000、value=4 的合法批次，正常新增。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,
		// 行 9：再次查询得到两个点、均值 3。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because failure lines occurred")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d output lines, want 9 (one result per non-blank input line): %v",
			len(lines), lines)
	}

	// assertBareError 校验失败记录：恰好一条 error、原始行号正确、index 期望，
	// 且不夹带任何成功结果或部分统计（added/duplicates/series/op 均不得出现）。
	assertBareError := func(idx, wantLine, wantIndex int) map[string]interface{} {
		t.Helper()
		m := decodeResultLine(t, lines[idx])
		if m["status"] != "error" {
			t.Fatalf("output %d = %v, want status error", idx, m)
		}
		if int(m["line"].(float64)) != wantLine {
			t.Fatalf("output %d line = %v, want input line %d", idx, m["line"], wantLine)
		}
		if wantIndex == 0 {
			if _, has := m["index"]; has {
				t.Fatalf("output %d must not carry index: %v", idx, m)
			}
		} else if int(m["index"].(float64)) != wantIndex {
			t.Fatalf("output %d index = %v, want %d", idx, m["index"], wantIndex)
		}
		for _, k := range []string{"added", "duplicates", "series", "op"} {
			if _, has := m[k]; has {
				t.Fatalf("failed batch output %d must not carry success/stat field %q: %v", idx, k, m)
			}
		}
		return m
	}

	// 行 1：成功写入基线点。
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" ||
		m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want successful write of the single baseline point", m)
	}

	// 行 2：只报第二个采样点的数值冲突，完整 conflict 细节对应这个点。
	m := assertBareError(1, 2, 2)
	if m["error"] != "conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9" {
		t.Fatalf("line 2 error = %v", m["error"])
	}
	conflict, ok := m["conflict"].(map[string]interface{})
	if !ok {
		t.Fatalf("line 2 must carry conflict detail: %v", m)
	}
	cSeries := conflict["series"].(map[string]interface{})
	if cSeries["name"] != "cpu" {
		t.Fatalf("line 2 conflict series name = %v, want cpu", cSeries["name"])
	}
	cLabels := cSeries["labels"].(map[string]interface{})
	if len(cLabels) != 1 || cLabels["host"] != "a" {
		t.Fatalf("line 2 conflict labels = %v, want exactly host=a", cLabels)
	}
	if conflict["timestamp"].(float64) != 1000 ||
		conflict["existing"].(float64) != 2 || conflict["submitted"].(float64) != 9 {
		t.Fatalf("line 2 conflict detail = %v, want ts=1000 existing=2 submitted=9", conflict)
	}

	// 行 3：交换后只报第二个点的 name 类型错误，不附带 conflict。
	m = assertBareError(2, 3, 2)
	if !strings.Contains(m["error"].(string), `field "name" must be a string`) {
		t.Fatalf("line 3 error = %v, want name type error", m["error"])
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 3 field error must not carry conflict: %v", m)
	}

	// 行 4、行 5：同一采样点的未知字段错误压过数值冲突，均无 conflict。
	for idx, wantLine := range []int{4, 5} {
		m := assertBareError(3+idx, wantLine, 1)
		if m["error"] != `unknown field "bogus"` {
			t.Fatalf("line %d error = %v, want unknown field bogus", wantLine, m["error"])
		}
		if _, has := m["conflict"]; has {
			t.Fatalf("line %d field error must not carry conflict: %v", wantLine, m)
		}
	}

	// 行 6：数组未闭合 → 整行解析错误，无 index、无 conflict。
	m = assertBareError(5, 6, 0)
	if !strings.Contains(m["error"].(string), "invalid JSON") {
		t.Fatalf("line 6 error = %v, want invalid JSON parse error", m["error"])
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 6 parse error must not carry conflict: %v", m)
	}

	// 行 7：失败批次前的新增点未留下、原值未覆盖——只剩 host=a 的一个点。
	q := decodeResultLine(t, lines[6])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 7 = %v, want ok query", q)
	}
	qSeries := q["series"].([]interface{})
	if len(qSeries) != 1 {
		t.Fatalf("line 7 series = %v, want exactly host=a", qSeries)
	}
	s0 := qSeries[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["labels"].(map[string]interface{})["host"] != "a" ||
		s0["count"].(float64) != 1 || s0["average"].(float64) != 2 {
		t.Fatalf("line 7 series[0] = %v, want cpu{host=a} count=1 average=2", s0)
	}

	// 行 8：合法批次正常新增。
	if m := decodeResultLine(t, lines[7]); m["status"] != "ok" ||
		m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 8 = %v, want added=1 duplicates=0", m)
	}

	// 行 9：两个点、均值 (2+4)/2 = 3。
	q = decodeResultLine(t, lines[8])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 9 = %v, want ok query", q)
	}
	qSeries = q["series"].([]interface{})
	if len(qSeries) != 1 {
		t.Fatalf("line 9 series = %v, want exactly host=a", qSeries)
	}
	s0 = qSeries[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["labels"].(map[string]interface{})["host"] != "a" ||
		s0["count"].(float64) != 2 || s0["average"].(float64) != 3 {
		t.Fatalf("line 9 series[0] = %v, want cpu{host=a} count=2 average=3", s0)
	}
}
