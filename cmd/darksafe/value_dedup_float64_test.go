package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 端到端回归保障：数值去重以实际存储的有限 float64 值为准，而不是 JSON 数字的
// 原始文本、也不是十进制下的“数值接近”。通过 ingest 逐行写入与随后查询验证：
//
//   - 9007199254740993 与 9007199254740992 存为同一个 float64（2^53），
//     5e-324 与 4.9406564584124654e-324 都舍入为最小次正规值：重复，成功忽略，
//     added 为 0、duplicates 按被忽略的提交次数计，快照仍只有原来的一个点。
//   - 9007199254740994 是 2^53 的下一个可表示值，1e-323 是最小次正规值的两倍：
//     与已存值不同，必须冲突；conflict 的 existing/submitted 对应实际存储值。
//   - 规则在同一数组内与跨成功批次一致；失败批次整批回滚（批内排在前面的
//     合法新增点也不提交，已存原值保留），失败后查询仍见原值，后续合法输入
//     继续处理；被忽略的重复不改变查询的 count 与 average。
//
// 输入含失败行，命令最终以非零退出码结束。
func TestRunIngestValueDedupByStoredFloat64EndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：写入 2^53。
		`[{"name":"m","timestamp":1,"value":9007199254740992}]`,
		// 行 2：9007199254740993 不可表示，舍入后与已存值相同 → 重复忽略。
		`[{"name":"m","timestamp":1,"value":9007199254740993}]`,
		// 行 3：9007199254740994 是相邻可表示值 → 冲突，existing/submitted 为存储值。
		`[{"name":"m","timestamp":1,"value":9007199254740994}]`,
		// 行 4：查询确认冲突未覆盖原值，重复未增加 count。
		`{"op":"query","name":"m","start":0,"end":10}`,
		// 行 5：同一数组内新位置先出现、随后以等值的另一种写法出现 → 只新增一个点。
		`[{"name":"t","timestamp":1,"value":5e-324},{"name":"t","timestamp":1,"value":4.9406564584124654e-324}]`,
		// 行 6：1e-323 是最小次正规值的两倍 → 与已存值冲突；批内前面的合法新增点
		// （u 序列）也不提交。
		`[{"name":"u","timestamp":1,"value":1},{"name":"t","timestamp":1,"value":1e-323}]`,
		// 行 7：查询确认 t 仍只有最小次正规值一个点，u 未留下。
		`{"op":"query","name":"u","start":0,"end":10}`,
		// 行 8：同一数组内新位置先出现、随后以不同存储值出现 → 整批拒绝，
		// existing 是本批较早的值，两个点都不保留。
		`[{"name":"v","timestamp":1,"value":9007199254740992},{"name":"v","timestamp":1,"value":9007199254740994}]`,
		// 行 9：查询确认 v 的两个点都不存在。
		`{"op":"query","name":"v","start":0,"end":10}`,
		// 行 10：后续合法输入继续处理。
		`[{"name":"m","timestamp":2,"value":1}]`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because conflict lines occurred")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 10 {
		t.Fatalf("got %d output lines, want 10: %v", len(lines), lines)
	}

	// seriesPoints 从成功写入结果中取出指定序列的 points 数组。
	seriesPoints := func(m map[string]interface{}, name string) []interface{} {
		t.Helper()
		for _, s := range m["series"].([]interface{}) {
			sv := s.(map[string]interface{})
			if sv["name"] == name {
				return sv["points"].([]interface{})
			}
		}
		return nil
	}
	// floatBits 把结果 JSON 中的数值按 float64 位模式取出，按位比较存储值。
	floatBits := func(v interface{}) uint64 {
		t.Helper()
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("value %v (%T) is not a JSON number", v, v)
		}
		return math.Float64bits(f)
	}
	const (
		bits2Pow53      = 0x4340000000000000 // 9007199254740992
		bits2Pow53Plus2 = 0x4340000000000001 // 9007199254740994
		bitsMinSub      = 0x0000000000000001 // 5e-324
		bitsTwoMinSub   = 0x0000000000000002 // 1e-323
	)

	// 行 1：新增 2^53。
	m := decodeResultLine(t, lines[0])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want added=1 duplicates=0", m)
	}

	// 行 2：等值的另一种写法 → added 0、duplicates 1，快照仍只有原来的一个点。
	m = decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 2 = %v, want added=0 duplicates=1", m)
	}
	pts := seriesPoints(m, "m")
	if len(pts) != 1 || floatBits(pts[0].(map[string]interface{})["value"]) != bits2Pow53 {
		t.Fatalf("line 2 snapshot points = %v, want single point 2^53", pts)
	}

	// 行 3：相邻可表示值 → 冲突；existing/submitted 对应实际存储值。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "error" || int(m["line"].(float64)) != 3 || int(m["index"].(float64)) != 1 {
		t.Fatalf("line 3 = %v, want conflict error on input line 3 index 1", m)
	}
	conflict, ok := m["conflict"].(map[string]interface{})
	if !ok {
		t.Fatalf("line 3 must carry conflict detail: %v", m)
	}
	if conflict["timestamp"].(float64) != 1 ||
		floatBits(conflict["existing"]) != bits2Pow53 ||
		floatBits(conflict["submitted"]) != bits2Pow53Plus2 {
		t.Fatalf("line 3 conflict = %v, want ts=1 existing=2^53 submitted=2^53+2", conflict)
	}

	// 行 4：查询仍只有原值一个点，average 为 2^53。
	q := decodeResultLine(t, lines[3])
	qs := q["series"].([]interface{})
	if len(qs) != 1 {
		t.Fatalf("line 4 series = %v, want exactly m", qs)
	}
	s0 := qs[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || floatBits(s0["average"]) != bits2Pow53 {
		t.Fatalf("line 4 series[0] = %v, want count=1 average=2^53", s0)
	}

	// 行 5：批内等值两种写法 → added 1、duplicates 1，只有一个点。
	m = decodeResultLine(t, lines[4])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 5 = %v, want added=1 duplicates=1", m)
	}
	pts = seriesPoints(m, "t")
	if len(pts) != 1 || floatBits(pts[0].(map[string]interface{})["value"]) != bitsMinSub {
		t.Fatalf("line 5 snapshot points = %v, want single point 5e-324", pts)
	}

	// 行 6：1e-323 与已存最小次正规值冲突，index 为 2。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "error" || int(m["line"].(float64)) != 6 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 6 = %v, want conflict error on input line 6 index 2", m)
	}
	conflict, ok = m["conflict"].(map[string]interface{})
	if !ok {
		t.Fatalf("line 6 must carry conflict detail: %v", m)
	}
	if floatBits(conflict["existing"]) != bitsMinSub ||
		floatBits(conflict["submitted"]) != bitsTwoMinSub {
		t.Fatalf("line 6 conflict = %v, want existing=5e-324 submitted=1e-323", conflict)
	}

	// 行 7：u 序列的点（排在冲突点前面的合法新增）未提交 → 空结果。
	q = decodeResultLine(t, lines[6])
	if len(q["series"].([]interface{})) != 0 {
		t.Fatalf("line 7 = %v, want empty series (rolled-back point must not exist)", q)
	}

	// 行 8：批内新位置先 2^53 后 2^53+2 → 整批拒绝，existing 是批内较早的值。
	m = decodeResultLine(t, lines[7])
	if m["status"] != "error" || int(m["line"].(float64)) != 8 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 8 = %v, want conflict error on input line 8 index 2", m)
	}
	conflict, ok = m["conflict"].(map[string]interface{})
	if !ok {
		t.Fatalf("line 8 must carry conflict detail: %v", m)
	}
	if floatBits(conflict["existing"]) != bits2Pow53 ||
		floatBits(conflict["submitted"]) != bits2Pow53Plus2 {
		t.Fatalf("line 8 conflict = %v, want existing=2^53 submitted=2^53+2", conflict)
	}

	// 行 9：v 的两个点都不存在 → 空结果。
	q = decodeResultLine(t, lines[8])
	if len(q["series"].([]interface{})) != 0 {
		t.Fatalf("line 9 = %v, want empty series (both conflicting points dropped)", q)
	}

	// 行 10：后续合法批次正常新增，计数只统计本批。
	m = decodeResultLine(t, lines[9])
	if m["status"] != "ok" || m["added"].(float64) != 1 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 10 = %v, want added=1 duplicates=0", m)
	}
	// 成功快照仍展示全部已提交数据：m 序列两个点、最小次正规值序列一个点。
	pts = seriesPoints(m, "m")
	if len(pts) != 2 {
		t.Fatalf("line 10 snapshot m points = %v, want 2 points", pts)
	}
	pts = seriesPoints(m, "t")
	if len(pts) != 1 || floatBits(pts[0].(map[string]interface{})["value"]) != bitsMinSub {
		t.Fatalf("line 10 snapshot t points = %v, want single point 5e-324", pts)
	}
}

// 负数与接近零的值遵循同一规则的端到端对照：-9007199254740993 与
// -9007199254740992 存为同一个值（重复忽略），-9007199254740994 冲突。
// 全部行成功时退出码为 0。
func TestRunIngestValueDedupNegativeEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"m","timestamp":1,"value":-9007199254740992}]`,
		`[{"name":"m","timestamp":1,"value":-9007199254740993}]`,
		`{"op":"query","name":"m","start":0,"end":10}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (all lines succeed)", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d output lines, want 3: %v", len(lines), lines)
	}

	m := decodeResultLine(t, lines[1])
	if m["status"] != "ok" || m["added"].(float64) != 0 || m["duplicates"].(float64) != 1 {
		t.Fatalf("line 2 = %v, want added=0 duplicates=1", m)
	}

	q := decodeResultLine(t, lines[2])
	s0 := q["series"].([]interface{})[0].(map[string]interface{})
	bits := math.Float64bits(s0["average"].(float64))
	if s0["count"].(float64) != 1 || bits != 0xc340000000000000 {
		t.Fatalf("line 3 series[0] = %v, want count=1 average=-2^53", s0)
	}

	// 序列化后的 average 文本必须能解析回 -2^53（JSON 精度不丢失相邻值差异）。
	var probe struct {
		Series []struct {
			Average json.Number `json:"average"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &probe); err != nil {
		t.Fatalf("re-parse line 3: %v", err)
	}
	f, err := probe.Series[0].Average.Float64()
	if err != nil || math.Float64bits(f) != 0xc340000000000000 {
		t.Fatalf("average text %q round-trips to %v, want -2^53", probe.Series[0].Average, f)
	}
}
