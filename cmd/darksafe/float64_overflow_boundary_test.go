package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// 端到端回归保障 value 在 float64 有限范围上界两侧的实际处理，
// 贯穿 ingest 的写入、重复判定、整批原子性与后续查询：
//
//   - 1.7976931348623157e308 与 1.7976931348623158e308 都成功保存为
//     最大有限 float64；同序列同时间戳先后提交、同批提交都算重复，
//     不同时间戳保留多个点，查询 count 与有限 average 正确。
//   - 1.7976931348623159e308 已溢出：作为 value 字段校验失败拒绝，
//     给出原始行号与从 1 开始的批内位置，不携带 conflict，即使该位置
//     已存有有限值也不报冲突；失败点之前的合法新增点不提交，先前
//     成功保存的边界值与数量保持原样，后续合法写入继续处理，
//     但本次命令最终非零退出。
//
// 所有输出中不得出现无穷大或把数值写成 null。

// finiteOutput 断言一段原始 ingest 输出不含无穷大或 null 数值。
func finiteOutput(t *testing.T, raw string) {
	t.Helper()
	if strings.Contains(raw, "Inf") {
		t.Fatalf("output must not contain infinity: %q", raw)
	}
	if strings.Contains(raw, "null") {
		t.Fatalf("output must not turn values into null: %q", raw)
	}
}

func TestRunIngestFloat64OverflowBoundaryEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：...157e308 写入最大有限 float64。
		`[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`,
		// 行 2：...158e308 字面上更大但舍入到同一最大有限值 → 重复。
		`[{"name":"m","timestamp":1000,"value":1.7976931348623158e308}]`,
		// 行 3：两种写法同批提交到另一个时间戳 → 只新增一个点。
		`[{"name":"m","timestamp":2000,"value":1.7976931348623157e308},{"name":"m","timestamp":2000,"value":1.7976931348623158e308}]`,
		// 行 4：查询两个时间戳，count=2，average 仍是最大有限值。
		`{"op":"query","name":"m","start":0,"end":3000}`,
		// 行 5：...159e308 溢出，且该位置已存有有限值 → 字段错误而非冲突。
		`[{"name":"m","timestamp":1000,"value":1.7976931348623159e308}]`,
		// 行 6：合法新增点在前、越界点在后 → 整批不提交，index 为 2。
		`[{"name":"m","timestamp":3000,"value":1.7976931348623157e308},{"name":"m","timestamp":1000,"value":1.7976931348623159e308}]`,
		// 行 7：失败批次回滚后，先前的两个边界点仍可查询。
		`{"op":"query","name":"m","start":0,"end":3000}`,
		// 行 8：失败行之后的合法写入继续处理。
		`[{"name":"m","timestamp":3000,"value":1.7976931348623157e308}]`,
		// 行 9：三个点全部保留，average 仍是有限的最大有限值。
		`{"op":"query","name":"m","start":0,"end":3000}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because overflow lines failed")
	}
	finiteOutput(t, out.String())

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d output lines, want 9: %v", len(lines), lines)
	}

	// 行 1：成功新增一个最大有限值点。
	m := decodeResultLine(t, lines[0])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want added=1 duplicates=0", m)
	}
	if v := m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})[0].(map[string]interface{})["value"].(float64); v != math.MaxFloat64 {
		t.Fatalf("line 1 value = %v, want MaxFloat64", v)
	}

	// 行 2：...158 舍入到同一存储值，重复；快照仍只有一个点且为最大有限值。
	m = decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 2 = %v, want added=0 duplicates=1", m)
	}
	points := m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 1 || points[0].(map[string]interface{})["value"].(float64) != math.MaxFloat64 {
		t.Fatalf("line 2 points = %v, want one MaxFloat64 point", points)
	}

	// 行 3：同批两种写法只新增一个点。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 3 = %v, want added=1 duplicates=1", m)
	}

	// 行 4：count=2、average=MaxFloat64（有限，不是无穷大）。
	q := decodeResultLine(t, lines[3])
	s0 := q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 2 || s0["average"].(float64) != math.MaxFloat64 {
		t.Fatalf("line 4 = %v, want count=2 average=MaxFloat64", s0)
	}

	// 行 5：越界在已存有有限值的位置上也只是字段错误：line=5、index=1、无 conflict。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "error" || int(m["line"].(float64)) != 5 || int(m["index"].(float64)) != 1 {
		t.Fatalf("line 5 = %v, want field error on input line 5 index 1", m)
	}
	if _, hasConflict := m["conflict"]; hasConflict {
		t.Fatalf("line 5 overflow must not carry conflict: %v", m)
	}
	if msg := m["error"].(string); !strings.Contains(msg, `field "value"`) ||
		!strings.Contains(msg, "finite number representable as float64") {
		t.Fatalf("line 5 error = %q, must say value must convert to finite float64", msg)
	}

	// 行 6：合法新增点在前也随整批丢弃；index 指向第 2 个点，无 conflict。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "error" || int(m["line"].(float64)) != 6 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 6 = %v, want field error on input line 6 index 2", m)
	}
	if _, hasConflict := m["conflict"]; hasConflict {
		t.Fatalf("line 6 overflow must not carry conflict: %v", m)
	}

	// 行 7：回滚后仍是行 1、行 3 提交的两个点。
	q = decodeResultLine(t, lines[6])
	s0 = q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 2 || s0["average"].(float64) != math.MaxFloat64 {
		t.Fatalf("line 7 = %v, want count=2 average=MaxFloat64 (rolled back)", s0)
	}

	// 行 8：失败行之后的合法写入照常提交。
	m = decodeResultLine(t, lines[7])
	if m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 8 = %v, want continued processing with added=1", m)
	}

	// 行 9：三个点全部可见，average 有限。
	q = decodeResultLine(t, lines[8])
	s0 = q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 3 || s0["average"].(float64) != math.MaxFloat64 {
		t.Fatalf("line 9 = %v, want count=3 average=MaxFloat64", s0)
	}
}

// TestRunIngestFloat64OverflowBoundaryNegativeSignEndToEnd 确认负数在对应
// 边界遵循同样规则：-...157 与 -...158 都保存为 -MaxFloat64 且保留负号，
// -...159 溢出被拒绝（字段错误、无 conflict），失败不影响先前数据，
// 命令最终非零退出，输出中无无穷大或 null。
func TestRunIngestFloat64OverflowBoundaryNegativeSignEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"n","timestamp":1000,"value":-1.7976931348623157e308}]`,
		// 更负的 -...158 舍入到同一 -MaxFloat64：重复而非冲突。
		`[{"name":"n","timestamp":1000,"value":-1.7976931348623158e308}]`,
		// 不同时间戳保留两个负的最大有限值。
		`[{"name":"n","timestamp":2000,"value":-1.7976931348623158e308}]`,
		`{"op":"query","name":"n","start":0,"end":3000}`,
		// 负方向越界：字段错误，无 conflict。
		`[{"name":"n","timestamp":1000,"value":-1.7976931348623159e308}]`,
		`{"op":"query","name":"n","start":0,"end":3000}`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero due to overflow line")
	}
	finiteOutput(t, out.String())

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d output lines, want 6: %v", len(lines), lines)
	}

	m := decodeResultLine(t, lines[0])
	if v := m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})[0].(map[string]interface{})["value"].(float64); v != -math.MaxFloat64 {
		t.Fatalf("line 1 value = %v, want -MaxFloat64", v)
	}
	m = decodeResultLine(t, lines[1])
	if m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 2 = %v, want duplicate of -MaxFloat64", m)
	}
	m = decodeResultLine(t, lines[2])
	if m["added"].(float64) != 1 {
		t.Fatalf("line 3 = %v, want one new point at a different timestamp", m)
	}
	q := decodeResultLine(t, lines[3])
	s0 := q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 2 || s0["average"].(float64) != -math.MaxFloat64 {
		t.Fatalf("line 4 = %v, want count=2 average=-MaxFloat64", s0)
	}
	m = decodeResultLine(t, lines[4])
	if m["status"] != "error" || int(m["line"].(float64)) != 5 || int(m["index"].(float64)) != 1 {
		t.Fatalf("line 5 = %v, want field error on line 5 index 1", m)
	}
	if _, hasConflict := m["conflict"]; hasConflict {
		t.Fatalf("line 5 must not carry conflict: %v", m)
	}
	q = decodeResultLine(t, lines[5])
	s0 = q["series"].([]interface{})[0].(map[string]interface{})
	if s0["count"].(float64) != 2 || s0["average"].(float64) != -math.MaxFloat64 {
		t.Fatalf("line 6 = %v, want original two negative points intact", s0)
	}
}

// TestRunIngestFloat64BoundaryOnlyLegalInputsExitsZero 确认只含合法边界输入
// （含正、负两种符号的 ...157 与 ...158）的调用正常成功、退出码为零，
// 原始输出里既没有无穷大也没有 null。
func TestRunIngestFloat64BoundaryOnlyLegalInputsExitsZero(t *testing.T) {
	input := strings.Join([]string{
		// 正数两种写法写到 p 的两个时间戳，负数两种写法写到 n 的两个时间戳。
		`[{"name":"p","timestamp":1000,"value":1.7976931348623157e308},{"name":"p","timestamp":2000,"value":1.7976931348623158e308},{"name":"n","timestamp":1000,"value":-1.7976931348623157e308},{"name":"n","timestamp":2000,"value":-1.7976931348623158e308}]`,
		`{"op":"query","name":"p","start":0,"end":3000}`,
		`{"op":"query","name":"n","start":0,"end":3000}`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 for legal boundary-only inputs", code)
	}
	finiteOutput(t, out.String())

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d output lines, want 3: %v", len(lines), lines)
	}
	m := decodeResultLine(t, lines[0])
	if m["status"] != "ok" || m["added"].(float64) != 4 {
		t.Fatalf("line 1 = %v, want 4 finite boundary points added", m)
	}
	pos := decodeResultLine(t, lines[1])["series"].([]interface{})[0].(map[string]interface{})
	if pos["count"].(float64) != 2 || pos["average"].(float64) != math.MaxFloat64 {
		t.Fatalf("positive query = %v, want count=2 average=MaxFloat64", pos)
	}
	neg := decodeResultLine(t, lines[2])["series"].([]interface{})[0].(map[string]interface{})
	if neg["count"].(float64) != 2 || neg["average"].(float64) != -math.MaxFloat64 {
		t.Fatalf("negative query = %v, want count=2 average=-MaxFloat64", neg)
	}
}
