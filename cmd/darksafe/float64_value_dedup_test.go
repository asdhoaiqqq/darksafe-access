package main

import (
	"bytes"
	"strings"
	"testing"
)

// 端到端回归保障：数值去重与冲突判定以实际存储的有限 float64 值为准，
// 不按 JSON 数字的原始文本比较，也不把数值接近当成相等。
//
//   - 9007199254740993 等写法转换后与已存的 9007199254740992 相同：
//     成功忽略，added 为 0、duplicates 为 1，快照仍只有原来的一个点，
//     查询的 count 与 average 不变。
//   - 9007199254740994 是相邻可表示值：整批冲突拒绝，错误原因与 conflict
//     中的 existing、submitted 都是转换后的实际存储值。
//   - 次正规值同理：5e-324 与 4.9406564584124654e-324 相等（重复），
//     1e-323 不同（冲突），不能因为数值很小就合并。
//
// 输入中出现过失败行，命令最终以非零退出码结束；失败批次后的查询仍看到
// 此前的原值，后续合法输入继续处理。
func TestRunIngestFloat64ValueDedupEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：写入 2^53（精确可表示）。
		`[{"name":"m","timestamp":1000,"value":9007199254740992}]`,
		// 行 2：9007199254740993 转换后与已存值相同 → 重复，成功忽略。
		`[{"name":"m","timestamp":1000,"value":9007199254740993}]`,
		// 行 3：9007199254740994 是相邻可表示值 → 冲突，整批拒绝。
		`[{"name":"m","timestamp":1000,"value":9007199254740994}]`,
		// 行 4：查询仍只见原值，count=1、average=9007199254740992。
		`{"op":"query","name":"m","start":0,"end":2000}`,
		// 行 5：次正规值的两种等价写法同批出现，只新增一个点。
		`[{"name":"m","timestamp":2000,"value":5e-324},{"name":"m","timestamp":2000,"value":4.9406564584124654e-324}]`,
		// 行 6：1e-323 与已存的 5e-324 是相邻可表示值 → 冲突。
		`[{"name":"m","timestamp":2000,"value":1e-323}]`,
		// 行 7：查询两个点，次正规值不被合并也不被冲刷。
		`{"op":"query","name":"m","start":0,"end":2000}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because conflict lines occurred")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d output lines, want 7: %v", len(lines), lines)
	}

	// 行 1：成功新增一个点。
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" ||
		m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want added=1 duplicates=0", m)
	}

	// 行 2：等价写法被忽略，快照仍只有原来的一个点，值是存储的 9007199254740992。
	m := decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 2 = %v, want added=0 duplicates=1", m)
	}
	series := m["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 2 series = %v, want one series", series)
	}
	points := series[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 1 {
		t.Fatalf("line 2 points = %v, want still exactly one point", points)
	}
	p0 := points[0].(map[string]interface{})
	if p0["timestamp"].(float64) != 1000 || p0["value"].(float64) != 9007199254740992.0 {
		t.Fatalf("line 2 stored point = %v, want ts=1000 value=9007199254740992", p0)
	}

	// 行 3：相邻可表示值冲突；existing、submitted 都是实际存储值。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "error" || int(m["line"].(float64)) != 3 || int(m["index"].(float64)) != 1 {
		t.Fatalf("line 3 = %v, want conflict error on input line 3 index 1", m)
	}
	conflict, ok := m["conflict"].(map[string]interface{})
	if !ok {
		t.Fatalf("line 3 must carry conflict detail: %v", m)
	}
	if conflict["timestamp"].(float64) != 1000 ||
		conflict["existing"].(float64) != 9007199254740992.0 ||
		conflict["submitted"].(float64) != 9007199254740994.0 {
		t.Fatalf("line 3 conflict = %v, want existing=9007199254740992 submitted=9007199254740994", conflict)
	}
	if !strings.Contains(m["error"].(string), "9.007199254740992e+15") ||
		!strings.Contains(m["error"].(string), "9.007199254740994e+15") {
		t.Fatalf("line 3 error must name the stored values, got %v", m["error"])
	}

	// 行 4：冲突批次回滚，查询仍只有原值。
	q := decodeResultLine(t, lines[3])
	qs := q["series"].([]interface{})
	if len(qs) != 1 {
		t.Fatalf("line 4 series = %v, want one series", qs)
	}
	s0 := qs[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 9007199254740992.0 {
		t.Fatalf("line 4 = %v, want count=1 average=9007199254740992", s0)
	}

	// 行 5：5e-324 与 4.9406564584124654e-324 转换后相等，同批只新增一个点。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 5 = %v, want added=1 duplicates=1", m)
	}

	// 行 6：1e-323 与已存的 5e-324 是相邻可表示值，冲突；existing 是 5e-324。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "error" || int(m["line"].(float64)) != 6 || int(m["index"].(float64)) != 1 {
		t.Fatalf("line 6 = %v, want conflict error on input line 6 index 1", m)
	}
	conflict, ok = m["conflict"].(map[string]interface{})
	if !ok || conflict["existing"].(float64) != 5e-324 || conflict["submitted"].(float64) != 1e-323 {
		t.Fatalf("line 6 conflict = %v, want existing=5e-324 submitted=1e-323", m["conflict"])
	}

	// 行 7：两个点都保留；次正规点的值不因数值很小而被合并或冲刷。
	q = decodeResultLine(t, lines[6])
	qs = q["series"].([]interface{})
	if len(qs) != 1 {
		t.Fatalf("line 7 series = %v, want one series", qs)
	}
	s0 = qs[0].(map[string]interface{})
	if s0["count"].(float64) != 2 {
		t.Fatalf("line 7 count = %v, want 2", s0["count"])
	}
	// (9007199254740992 + 5e-324) / 2 的精确值舍入到最近 float64 为 4503599627370496。
	if s0["average"].(float64) != 4503599627370496.0 {
		t.Fatalf("line 7 average = %v, want 4503599627370496", s0["average"])
	}
}
