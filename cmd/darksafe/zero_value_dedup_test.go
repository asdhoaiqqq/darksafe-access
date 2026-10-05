package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// 端到端回归保障：指标写入的零值去重以转换后的有限 float64 存储值为准。
//
//   - 0、-0、0.0、-0.0 是同一数值，同一序列同一时间戳先后提交时只新增一个
//     点，其余成功忽略并计入本批 duplicates；跨批与同批都一致。
//   - 1e-400 与 -1e-400 写入时分别舍入成正零与负零，与已存零值相遇同样算
//     重复，原始文本非零或符号不同都不导致冲突。
//   - 被忽略的重复不改变首次接受的存储值：先写正零，快照里始终是正零；
//     先写由 -1e-400 舍入成的负零，该点就保留负号（输出 -0）。
//   - 5e-324 仍是可区分的非零值：与已存零冲突时整批拒绝，错误指出采样
//     位置，conflict 给出序列、时间戳、判定冲突时的零值与提交的极小值；
//     批内前面的合法新增点不提交，原零点不被替换。
//   - 查询按已提交点计数：重复不增加 count，零值在不同时间戳是不同点，
//     精确均值为零时 average 输出正零。
//
// 输入中出现失败行，命令最终以非零退出码结束；冲突后的查询只含此前成功
// 写入的点。
func TestRunIngestZeroValueDedupEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：ts=1000 先写入正零。
		`[{"name":"m","timestamp":1000,"value":0}]`,
		// 行 2：-0、0.0、-0.0 与已存零等值，三次提交全部忽略。
		`[{"name":"m","timestamp":1000,"value":-0},{"name":"m","timestamp":1000,"value":0.0},{"name":"m","timestamp":1000,"value":-0.0}]`,
		// 行 3：1e-400 舍入为正零、-1e-400 舍入为负零，都与已存零重复。
		`[{"name":"m","timestamp":1000,"value":1e-400},{"name":"m","timestamp":1000,"value":-1e-400}]`,
		// 行 4：此前不存在的 ts=2000，本批先写 0 再写 -0.0：只新增一个点。
		`[{"name":"m","timestamp":2000,"value":0},{"name":"m","timestamp":2000,"value":-0.0}]`,
		// 行 5：ts=3000 的合法新增在前，ts=1000 提交 5e-324 与已存零冲突在后，
		// 整批拒绝，前面的新增点也不提交。
		`[{"name":"m","timestamp":3000,"value":7},{"name":"m","timestamp":1000,"value":5e-324}]`,
		// 行 6：冲突回滚后查询，只见 ts=1000、ts=2000 两个零点。
		`{"op":"query","name":"m","start":0,"end":5000}`,
		// 行 7：新位置 ts=4000 先写舍入成负零的 -1e-400，再提交正零文本：
		// 新增一个带负号的零点，第二次提交计为重复且不改写符号。
		`[{"name":"m","timestamp":4000,"value":-1e-400},{"name":"m","timestamp":4000,"value":0}]`,
		// 行 8：三个不同时间戳的零点各计一次，精确均值为零，输出正零。
		`{"op":"query","name":"m","start":0,"end":5000}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because a conflict line occurred")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("got %d output lines, want 8: %v", len(lines), lines)
	}

	// 行 1：成功新增一个正零点。
	m := decodeResultLine(t, lines[0])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want added=1 duplicates=0", m)
	}

	// 行 2：三种零值写法全部按重复忽略；快照仍只有 ts=1000 一个正零点，
	// 后写的负零不能覆盖首次接受的符号。
	m = decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 3 {
		t.Fatalf("line 2 = %v, want added=0 duplicates=3", m)
	}
	points := m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 1 {
		t.Fatalf("line 2 points = %v, want still exactly one point", points)
	}
	v1000 := points[0].(map[string]interface{})["value"].(float64)
	if points[0].(map[string]interface{})["timestamp"].(float64) != 1000 ||
		v1000 != 0 || math.Signbit(v1000) {
		t.Fatalf("line 2 stored point = %v, want ts=1000 value=+0", points[0])
	}

	// 行 3：舍入成正零、负零的非零文本同样按重复忽略。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 2 {
		t.Fatalf("line 3 = %v, want added=0 duplicates=2", m)
	}
	points = m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if v := points[0].(map[string]interface{})["value"].(float64); v != 0 || math.Signbit(v) {
		t.Fatalf("line 3 stored zero = %v, want first-written +0 preserved", v)
	}

	// 行 4：同批新位置只新增一个零点，另一个写法计为重复。
	m = decodeResultLine(t, lines[3])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 4 = %v, want added=1 duplicates=1", m)
	}
	points = m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 2 {
		t.Fatalf("line 4 points = %v, want exactly two points (ts=1000,2000)", points)
	}

	// 行 5：5e-324 与已存正零冲突；index 指向批内第二个采样点，
	// conflict 给出序列、时间戳、判定时的零值与提交的极小值。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "error" || int(m["line"].(float64)) != 5 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 5 = %v, want conflict error on input line 5 index 2", m)
	}
	conflict, ok := m["conflict"].(map[string]interface{})
	if !ok {
		t.Fatalf("line 5 must carry conflict detail: %v", m)
	}
	seriesRef := conflict["series"].(map[string]interface{})
	if seriesRef["name"] != "m" {
		t.Fatalf("line 5 conflict series = %v, want name m", seriesRef)
	}
	if conflict["timestamp"].(float64) != 1000 {
		t.Fatalf("line 5 conflict timestamp = %v, want 1000", conflict["timestamp"])
	}
	if existing := conflict["existing"].(float64); existing != 0 || math.Signbit(existing) {
		t.Fatalf("line 5 conflict existing = %v, want the stored +0", existing)
	}
	if conflict["submitted"].(float64) != 5e-324 {
		t.Fatalf("line 5 conflict submitted = %v, want 5e-324", conflict["submitted"])
	}
	if !strings.Contains(m["error"].(string), "already has value 0") ||
		!strings.Contains(m["error"].(string), "submitted 5e-324") {
		t.Fatalf("line 5 error must name the zero and 5e-324, got %v", m["error"])
	}

	// 行 6：整批回滚——ts=3000 的新增点没有提交，只有此前两个零点；
	// 精确均值为零时 average 必须是正零。
	q := decodeResultLine(t, lines[5])
	qs := q["series"].([]interface{})
	if len(qs) != 1 {
		t.Fatalf("line 6 series = %v, want one series", qs)
	}
	s0 := qs[0].(map[string]interface{})
	if s0["count"].(float64) != 2 {
		t.Fatalf("line 6 count = %v, want 2", s0["count"])
	}
	if avg := s0["average"].(float64); avg != 0 || math.Signbit(avg) {
		t.Fatalf("line 6 average = %v, want +0", avg)
	}

	// 行 7：新位置先接受由 -1e-400 舍入成的负零，随后的正零按重复忽略、
	// 不改写符号：快照中该点输出 -0，ts=1000 仍是正零。
	m = decodeResultLine(t, lines[6])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 7 = %v, want added=1 duplicates=1", m)
	}
	points = m["series"].([]interface{})[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 3 {
		t.Fatalf("line 7 points = %v, want three points (ts=1000,2000,4000)", points)
	}
	first := points[0].(map[string]interface{})["value"].(float64)
	last := points[2].(map[string]interface{})["value"].(float64)
	if first != 0 || math.Signbit(first) {
		t.Fatalf("line 7 ts=1000 value = %v, want +0 unchanged", first)
	}
	if last != 0 || !math.Signbit(last) {
		t.Fatalf("line 7 ts=4000 value = %v, want first-written -0", last)
	}

	// 行 8：三个不同时间戳的零点各参与一次计数，均值为正零。
	q = decodeResultLine(t, lines[7])
	qs = q["series"].([]interface{})
	if len(qs) != 1 {
		t.Fatalf("line 8 series = %v, want one series", qs)
	}
	s0 = qs[0].(map[string]interface{})
	if s0["count"].(float64) != 3 {
		t.Fatalf("line 8 count = %v, want 3 (zeros at distinct timestamps are distinct points)",
			s0["count"])
	}
	if avg := s0["average"].(float64); avg != 0 || math.Signbit(avg) {
		t.Fatalf("line 8 average = %v, want +0", avg)
	}
}
