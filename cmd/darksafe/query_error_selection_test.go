package main

import (
	"bytes"
	"strings"
	"testing"
)

// 端到端回归保障：同一条查询请求包含多个问题时，ingest 按行返回的错误原因
// 能让用户逐步修正输入；失败行保留从 1 开始的原始行号（空白行仍计入行号），
// 失败查询不返回结果、不带 index 与 conflict；此前写入的多时间戳序列在
// 失败之后用合法查询读取相同范围，完整标签、点数和平均值与失败前一致；
// 出现过失败的输入最终返回非零退出码。
func TestRunIngestQueryErrorSelectionEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：写入一条含三个时间戳、两个标签的序列。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a","zone":"z1"}},` +
			`{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a","zone":"z1"}},` +
			`{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a","zone":"z1"}}]`,
		// 行 2：合法查询基线，count=3、average=4。
		`{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`,
		``,    // 行 3：空白行，只占行号
		`   `, // 行 4：空白行，只占行号
		// 行 5：未知字段与类型错误同时存在，先出现的未知字段获选。
		`{"op":"query","bogus":1,"name":7,"start":0,"end":4000}`,
		// 行 6：去掉未知字段后，字段类型错误成为当前问题。
		`{"op":"query","name":7,"start":0,"end":4000}`,
		// 行 7：已出现字段都合法时，才报告缺少必填字段 name。
		`{"op":"query","start":0,"end":4000}`,
		// 行 8：倒置区间与未知字段同时存在，先报告未知字段。
		`{"op":"query","name":"cpu","start":4000,"end":0,"extra":1}`,
		// 行 9：去掉未知字段后，同一请求报告倒置区间。
		`{"op":"query","name":"cpu","start":4000,"end":0}`,
		// 行 10：对象未闭合，即使前面字段已有类型错误也报整行解析失败。
		`{"op":"query","name":7,"start":0`,
		// 行 11：修正区间后，合法查询得到与失败前一致的结果。
		`{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because query lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	// 空白行（3、4）不产生结果：共 9 行输出。
	if len(lines) != 9 {
		t.Fatalf("got %d output lines, want 9: %v", len(lines), lines)
	}

	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" || m["added"].(float64) != 3 {
		t.Fatalf("line 1 = %v, want successful write of 3 points", m)
	}

	// 合法查询（行 2 与行 11）必须给出完全一致的结果：
	// 完整标签、点数 3、平均值 4。
	checkBaselineQuery := func(idx int) {
		t.Helper()
		m := decodeResultLine(t, lines[idx])
		if m["status"] != "ok" || m["op"] != "query" {
			t.Fatalf("output %d = %v, want ok query", idx, m)
		}
		series := m["series"].([]interface{})
		if len(series) != 1 {
			t.Fatalf("output %d series = %v, want exactly one", idx, series)
		}
		s0 := series[0].(map[string]interface{})
		if s0["name"] != "cpu" || s0["count"].(float64) != 3 || s0["average"].(float64) != 4 {
			t.Fatalf("output %d series[0] = %v, want cpu count=3 average=4", idx, s0)
		}
		labels := s0["labels"].(map[string]interface{})
		if len(labels) != 2 || labels["host"] != "a" || labels["zone"] != "z1" {
			t.Fatalf("output %d labels = %v, want full label set host=a,zone=z1", idx, labels)
		}
	}
	checkBaselineQuery(1)

	// 失败查询：输出只含错误，保留原始行号，不带 index 与 conflict，
	// 错误原因按修正步骤逐步变化、彼此可区分。
	wantErr := []struct {
		outIdx int
		line   int
		substr string
	}{
		{2, 5, `unknown field "bogus"`},
		{3, 6, `"name" must be a string`},
		{4, 7, `missing required field "name"`},
		{5, 8, `unknown field "extra"`},
		{6, 9, "invalid range"},
		{7, 10, "invalid JSON"},
	}
	for _, w := range wantErr {
		m := decodeResultLine(t, lines[w.outIdx])
		if m["status"] != "error" || int(m["line"].(float64)) != w.line {
			t.Fatalf("output %d = %v, want error on input line %d", w.outIdx, m, w.line)
		}
		if _, has := m["index"]; has {
			t.Fatalf("line %d query error must not carry index: %v", w.line, m)
		}
		if _, has := m["conflict"]; has {
			t.Fatalf("line %d query error must not carry conflict: %v", w.line, m)
		}
		msg, _ := m["error"].(string)
		if !strings.Contains(msg, w.substr) {
			t.Fatalf("line %d error = %q, want substring %q", w.line, msg, w.substr)
		}
	}

	// 全部失败之后，相同范围的合法查询结果与失败前一致。
	checkBaselineQuery(8)
}
