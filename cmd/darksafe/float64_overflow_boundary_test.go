package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// seriesByName 从写入成功结果的快照中按指标名取出序列视图；
// 快照按名称排序，多序列时不能假定下标。
func seriesByName(t *testing.T, m map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	for _, s := range m["series"].([]interface{}) {
		sm := s.(map[string]interface{})
		if sm["name"] == name {
			return sm
		}
	}
	t.Fatalf("series %q not found in result: %v", name, m)
	return nil
}

// 端到端回归保障：value 在 float64 有限范围上溢分界两侧的十进制写法被正确
// 区分。仍在边界内的两种写法（1.7976931348623157e308 与
// 1.7976931348623158e308，以及对应负数）转换后都是最大有限 float64：
// 同位置重复时 added=0、duplicates=1，不同时间戳保留两个点，查询数量与均值
// 保持有限且 JSON 输出不出现无穷大或 null。越界写法（末位 9）在字段校验
// 阶段失败：即使采样位置已存有有限值也不报冲突，错误带原始行号与从 1 开始
// 的批内位置、不带 conflict；失败批次整体回滚，此前边界值与采样数量保持
// 原样，失败行之后的合法写入继续处理，命令最终返回非零退出码。
func TestRunIngestFloat64OverflowBoundaryEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：正数边界写法 1（最大有限值）写入 ts=1000。
		`[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`,
		// 行 2：写法 2 原文更大但转换后相同 → 重复，成功忽略。
		`[{"name":"m","timestamp":1000,"value":1.7976931348623158e308}]`,
		// 行 3：两种写法同批同位置 → 只新增一个点。
		`[{"name":"m","timestamp":2000,"value":1.7976931348623157e308},{"name":"m","timestamp":2000,"value":1.7976931348623158e308}]`,
		// 行 4：另一种写法写到新时间戳 ts=3000 → 新增。
		`[{"name":"m","timestamp":3000,"value":1.7976931348623158e308}]`,
		// 行 5：三个点，均值仍是最大有限值。
		`{"op":"query","name":"m","start":0,"end":4000}`,
		// 行 6：采样明细，三个最大有限值原样输出。
		`{"op":"query_points","name":"m","start":0,"end":4000}`,
		// 行 7：同位置提交越界写法 → value 字段错误，不是冲突。
		`[{"name":"m","timestamp":1000,"value":1.7976931348623159e308}]`,
		// 行 8：越界负数 → 同样字段错误。
		`[{"name":"m","timestamp":2000,"value":-1.7976931348623159e308}]`,
		// 行 9：合法新增点在前、越界点在批内第二位 → 整批回滚，位置 2。
		`[{"name":"m","timestamp":5000,"value":1},{"name":"m","timestamp":6000,"value":1.7976931348623159e308}]`,
		// 行 10：失败批次回滚后查询，仍是行 1/3/4 的三个边界点。
		`{"op":"query","name":"m","start":0,"end":9000}`,
		// 行 11：负数边界写法 1 写入序列 neg。
		`[{"name":"neg","timestamp":1000,"value":-1.7976931348623157e308}]`,
		// 行 12：负数写法 2 同位置 → 重复。
		`[{"name":"neg","timestamp":1000,"value":-1.7976931348623158e308}]`,
		// 行 13：负数序列查询，均值为 -MaxFloat64。
		`{"op":"query","name":"neg","start":0,"end":2000}`,
		// 行 14：失败行之后的合法写入继续处理（行 9 被回滚的 ts=5000 重提）。
		`[{"name":"m","timestamp":5000,"value":1}]`,
		// 行 15：m 现在有 4 个点，均值仍有限。
		`{"op":"query","name":"m","start":0,"end":9000}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because overflow lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 15 {
		t.Fatalf("got %d output lines, want 15: %v", len(lines), lines)
	}

	// 行 1：成功新增一个最大有限值点。
	m := decodeResultLine(t, lines[0])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want added=1 duplicates=0", m)
	}

	// 行 2：更大原文但转换后相同，按重复忽略，快照仍是一个最大有限值点。
	m = decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 2 = %v, want added=0 duplicates=1", m)
	}
	points := m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 1 {
		t.Fatalf("line 2 points = %v, want exactly one point", points)
	}
	p0 := points[0].(map[string]interface{})
	if p0["timestamp"].(float64) != 1000 || p0["value"].(float64) != math.MaxFloat64 {
		t.Fatalf("line 2 point = %v, want ts=1000 value=MaxFloat64", p0)
	}

	// 行 3：同批同位置两种写法只新增一个点。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 3 = %v, want added=1 duplicates=1", m)
	}

	// 行 4：新时间戳新增。
	m = decodeResultLine(t, lines[3])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 4 = %v, want added=1 duplicates=0", m)
	}

	// 行 5：三个点，数量为 3，平均值仍是最大有限值（不是无穷大）。
	q := decodeResultLine(t, lines[4])
	s0 := q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 3 || s0["average"].(float64) != math.MaxFloat64 {
		t.Fatalf("line 5 = %v, want count=3 average=MaxFloat64", s0)
	}

	// 行 6：明细查询的原始 JSON 必须是有限数值，不能出现无穷大或 null，
	// 且三个点都渲染为最大有限值的规范写法。
	raw6 := lines[5]
	m6 := decodeResultLine(t, raw6)
	if m6["op"] != "query_points" {
		t.Fatalf("line 6 = %v, want query_points", m6)
	}
	qp := m6["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(qp) != 3 {
		t.Fatalf("line 6 points = %v, want three points", qp)
	}
	for i, pt := range qp {
		p := pt.(map[string]interface{})
		if v, ok := p["value"].(float64); !ok || v != math.MaxFloat64 {
			t.Fatalf("line 6 point %d = %v, want MaxFloat64", i, p)
		}
	}
	if strings.Contains(raw6, "Infinity") || strings.Contains(raw6, "null") {
		t.Fatalf("line 6 raw JSON = %s, want no Infinity or null", raw6)
	}
	if strings.Count(raw6, "1.7976931348623157e+308") != 3 {
		t.Fatalf("line 6 raw JSON = %s, want three MaxFloat64 renderings", raw6)
	}

	// 行 7/8：越界写法（正、负）在 value 字段校验阶段失败，即使行 7 的采样
	// 位置已存有有限值也不报冲突；行号、批内位置正确。
	for _, wantLine := range []int{7, 8} {
		e := decodeResultLine(t, lines[wantLine-1])
		if e["status"] != "error" || int(e["line"].(float64)) != wantLine ||
			int(e["index"].(float64)) != 1 {
			t.Fatalf("line %d = %v, want field error on input line %d index 1", wantLine, e, wantLine)
		}
		msg := e["error"].(string)
		if !strings.Contains(msg, `field "value"`) ||
			!strings.Contains(msg, "must be a finite number representable as float64") {
			t.Fatalf("line %d error = %q, must name field value and the finite-float64 rule", wantLine, msg)
		}
		if _, hasConflict := e["conflict"]; hasConflict {
			t.Fatalf("line %d overflow error must not carry a conflict: %v", wantLine, e)
		}
	}

	// 行 9：合法新增在前、越界在第二位，错误位置为批内第 2 个采样点，
	// 同样不带 conflict。
	e9 := decodeResultLine(t, lines[8])
	if e9["status"] != "error" || int(e9["line"].(float64)) != 9 || int(e9["index"].(float64)) != 2 {
		t.Fatalf("line 9 = %v, want error on input line 9 index 2", e9)
	}
	if _, hasConflict := e9["conflict"]; hasConflict {
		t.Fatalf("line 9 overflow error must not carry a conflict: %v", e9)
	}

	// 行 10：整批回滚后查询仍是三个边界点，平均值有限。
	q = decodeResultLine(t, lines[9])
	s0 = q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 3 || s0["average"].(float64) != math.MaxFloat64 {
		t.Fatalf("line 10 = %v, want count=3 average=MaxFloat64 (rollback intact)", s0)
	}

	// 行 11/12：负数边界两种写法，先新增后重复，负号保留。成功快照列出全部
	// 已知序列（按名称排序，m 在 neg 之前），需按名称找到 neg 序列。
	m = decodeResultLine(t, lines[10])
	if m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 11 = %v, want added=1", m)
	}
	if got := seriesByName(t, m, "neg")["points"].([]interface{})[0].(map[string]interface{})["value"].(float64); got != -math.MaxFloat64 {
		t.Fatalf("line 11 neg value = %v, want -MaxFloat64", got)
	}
	m = decodeResultLine(t, lines[11])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 12 = %v, want added=0 duplicates=1", m)
	}
	if got := seriesByName(t, m, "neg")["points"].([]interface{})[0].(map[string]interface{})["value"].(float64); got != -math.MaxFloat64 {
		t.Fatalf("line 12 neg value = %v, want -MaxFloat64", got)
	}

	// 行 13：负数序列数量为 1、均值为 -MaxFloat64。
	q = decodeResultLine(t, lines[12])
	s0 = q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != -math.MaxFloat64 {
		t.Fatalf("line 13 = %v, want count=1 average=-MaxFloat64", s0)
	}

	// 行 14：失败行之后的合法写入继续处理。
	m = decodeResultLine(t, lines[13])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 14 = %v, want legal write processed after failures, added=1", m)
	}

	// 行 15：四个点，平均值必须仍是有限数值。
	q = decodeResultLine(t, lines[14])
	s0 = q["series"].([]interface{})[0].(map[string]interface{})
	avg := s0["average"].(float64)
	if s0["count"].(float64) != 4 || math.IsInf(avg, 0) {
		t.Fatalf("line 15 = %v, want count=4 finite average", s0)
	}
}

// TestRunIngestFloat64OverflowBoundaryOnlyLegalInputsExitZero 单独验证：
// 只包含分界内合法边界写法（正、负，两种写法）的调用必须全部成功，
// 进程以零退出码结束，输出中不出现错误、无穷大或 null。
func TestRunIngestFloat64OverflowBoundaryOnlyLegalInputsExitZero(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`,
		`[{"name":"m","timestamp":1000,"value":1.7976931348623158e308}]`,
		`[{"name":"m","timestamp":2000,"value":1.7976931348623158e308}]`,
		`[{"name":"n","timestamp":1000,"value":-1.7976931348623157e308}]`,
		`[{"name":"n","timestamp":1000,"value":-1.7976931348623158e308}]`,
		`[{"name":"n","timestamp":2000,"value":-1.7976931348623157e308}]`,
		`{"op":"query","name":"m","start":0,"end":3000}`,
		`{"op":"query","name":"n","start":0,"end":3000}`,
		`{"op":"query_points","name":"m","start":0,"end":3000}`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 for legal boundary-only input", code)
	}
	raw := out.String()
	if strings.Contains(raw, `"status":"error"`) || strings.Contains(raw, "Infinity") ||
		strings.Contains(raw, "null") {
		t.Fatalf("legal boundary input produced bad output: %s", raw)
	}

	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d output lines, want 9: %v", len(lines), lines)
	}
	mq := decodeResultLine(t, lines[6])
	if v := mq["series"].([]interface{})[0].(map[string]interface{}); v["count"].(float64) != 2 ||
		v["average"].(float64) != math.MaxFloat64 {
		t.Fatalf("positive query = %v, want count=2 average=MaxFloat64", v)
	}
	nq := decodeResultLine(t, lines[7])
	if v := nq["series"].([]interface{})[0].(map[string]interface{}); v["count"].(float64) != 2 ||
		v["average"].(float64) != -math.MaxFloat64 {
		t.Fatalf("negative query = %v, want count=2 average=-MaxFloat64", v)
	}
}
