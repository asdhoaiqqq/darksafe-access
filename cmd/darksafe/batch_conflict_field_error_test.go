package main

import (
	"bytes"
	"strings"
	"testing"
)

// 本文件在命令行端固定 ingest 批量写入中“数值冲突与字段错误同时出现”时的
// 用户可观察行为：每个失败批次只输出一条带原始输入行号（从 1 开始）的
// error，不夹带成功结果或部分统计（无 added/duplicates/series）；失败批次
// 前面的新增点不留存、已存原值不被覆盖；随后查询与恢复批次如实反映数据；
// 输入中出现过失败行时命令以非零退出码结束。

// assertSingleFailureRecord 校验一条失败结果：恰好是一个 error 对象，
// 带原始行号，不带任何成功统计字段；wantIndex 为 0 时要求没有 index，
// 否则 index 必须等于该值；wantConflict 为 false 时要求没有 conflict。
func assertSingleFailureRecord(t *testing.T, raw string, wantLine, wantIndex int,
	wantReason string, wantConflict bool) map[string]interface{} {
	t.Helper()
	m := decodeResultLine(t, raw)
	if m["status"] != "error" {
		t.Fatalf("record %s = %v, want status error", raw, m)
	}
	if int(m["line"].(float64)) != wantLine {
		t.Fatalf("record %s line = %v, want input line %d", raw, m["line"], wantLine)
	}
	if wantIndex == 0 {
		if _, has := m["index"]; has {
			t.Fatalf("line %d failure must not carry index: %v", wantLine, m)
		}
	} else if int(m["index"].(float64)) != wantIndex {
		t.Fatalf("line %d index = %v, want %d", wantLine, m["index"], wantIndex)
	}
	if !strings.Contains(m["error"].(string), wantReason) {
		t.Fatalf("line %d error = %q, want substring %q", wantLine, m["error"], wantReason)
	}
	_, hasConflict := m["conflict"]
	if hasConflict != wantConflict {
		t.Fatalf("line %d conflict presence = %v, want %v (record %v)", wantLine, hasConflict, wantConflict, m)
	}
	// 失败结果不得夹带成功结果或部分统计。
	for _, banned := range []string{"added", "duplicates", "series", "op"} {
		if _, has := m[banned]; has {
			t.Fatalf("line %d failure must not carry success field %q: %v", wantLine, banned, m)
		}
	}
	return m
}

// assertCPUHostAQueryRecord 校验一条查询成功结果：恰好一条 cpu host=a 序列，
// count 与 average 符合预期。
func assertCPUHostAQueryRecord(t *testing.T, raw string, wantCount int, wantAverage float64) {
	t.Helper()
	m := decodeResultLine(t, raw)
	if m["status"] != "ok" || m["op"] != "query" {
		t.Fatalf("record %s = %v, want ok query", raw, m)
	}
	all := m["series"].([]interface{})
	if len(all) != 1 {
		t.Fatalf("query %s series = %v, want exactly host=a", raw, all)
	}
	s0 := all[0].(map[string]interface{})
	labels := s0["labels"].(map[string]interface{})
	if s0["name"] != "cpu" || labels["host"] != "a" || len(labels) != 1 ||
		int(s0["count"].(float64)) != wantCount || s0["average"].(float64) != wantAverage {
		t.Fatalf("query %s series[0] = %v, want cpu host=a count=%d average=%v",
			raw, s0, wantCount, wantAverage)
	}
}

// TestRunIngestConflictWinsOverLaterTypeError 复现任务主链路：
// 先写入 cpu/host=a/ts1000/value=2；随后一批三个点——第一个是 ts2000=4 的
// 新增，第二个把 ts1000 提交为 9（冲突），第三个 name 是数字。只报第二个点
// 的数值冲突且细节完整，后面的类型错误不能替换原因。交换后两个点后只报
// 第二个点的 name 类型错误，不带 conflict。每次失败整批不留痕，查询始终
// 只有一个点、均值 2；合法批次恢复后为两个点、均值 3；退出码非零。
func TestRunIngestConflictWinsOverLaterTypeError(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：基线写入。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：新增在前、冲突在第二、类型错误在第三。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}},{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}}]`,
		// 行 3：交换后两个点：类型错误在第二，冲突在第三。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}},{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,
		// 行 4：失败后查询，仍只有基线点。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
		// 行 5：提交 ts2000=4 的合法批次。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,
		// 行 6：恢复后查询，两个点、均值 3。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero because failed lines occurred")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d output lines, want exactly 6 (one per non-empty input line): %v",
			len(lines), lines)
	}

	// 行 1：成功写入基线一个点。
	first := decodeResultLine(t, lines[0])
	if first["status"] != "ok" || int(first["added"].(float64)) != 1 ||
		int(first["duplicates"].(float64)) != 0 {
		t.Fatalf("line 1 = %v, want ok added=1 duplicates=0", first)
	}

	// 行 2：只报第二个点的冲突，conflict 细节完整对应这个点。
	err2 := assertSingleFailureRecord(t, lines[1], 2, 2,
		"conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9", true)
	if strings.Contains(err2["error"].(string), "must be a string") {
		t.Fatalf("later sample's type error must not replace the conflict reason: %v", err2)
	}
	conflict := err2["conflict"].(map[string]interface{})
	series := conflict["series"].(map[string]interface{})
	labels := series["labels"].(map[string]interface{})
	if series["name"] != "cpu" || labels["host"] != "a" || len(labels) != 1 ||
		int(conflict["timestamp"].(float64)) != 1000 ||
		conflict["existing"].(float64) != 2 || conflict["submitted"].(float64) != 9 {
		t.Fatalf("line 2 conflict detail = %v, want cpu{host=a} ts1000 existing=2 submitted=9", conflict)
	}

	// 行 3：交换后只报第二个点的 name 类型错误，不带 conflict、不列出后面的冲突。
	err3 := assertSingleFailureRecord(t, lines[2], 3, 2,
		`field "name" must be a string`, false)
	if strings.Contains(err3["error"].(string), "conflict") {
		t.Fatalf("line 3 must not also list the later conflict: %v", err3)
	}

	// 行 4：两批失败均未留痕——ts2000 新增点不存在、ts1000 原值 2 未覆盖。
	assertCPUHostAQueryRecord(t, lines[3], 1, 2)

	// 行 5：合法批次正常新增。
	recovery := decodeResultLine(t, lines[4])
	if recovery["status"] != "ok" || int(recovery["added"].(float64)) != 1 ||
		int(recovery["duplicates"].(float64)) != 0 {
		t.Fatalf("line 5 = %v, want ok added=1", recovery)
	}

	// 行 6：两个点、均值 3。
	assertCPUHostAQueryRecord(t, lines[5], 2, 3)
}

// TestRunIngestUnknownFieldOnConflictingSampleBeatsConflict 固定同一点的对照：
// 与已存点同序列、同时间戳且值不同但携带未知字段 bogus 时，返回未知字段错误
// 而不是数值冲突；name/timestamp/value 写在 bogus 前后结果相同；删去 bogus、
// 其余内容不变后才报告真正的数值冲突。数据始终保持基线一个点，退出码非零。
func TestRunIngestUnknownFieldOnConflictingSampleBeatsConflict(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：基线写入。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：name/timestamp/value 都写在 bogus 前面：仍报未知字段。
		`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"},"bogus":1}]`,
		// 行 3：bogus 写在最前面：结果相同。
		`[{"bogus":1,"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,
		// 行 4：删去 bogus、其余内容不变：真正的数值冲突。
		`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,
		// 行 5：查询确认原值 2 从未被覆盖。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero because failed lines occurred")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d output lines, want 5: %v", len(lines), lines)
	}

	assertSingleFailureRecord(t, lines[1], 2, 1, `unknown field "bogus"`, false)
	assertSingleFailureRecord(t, lines[2], 3, 1, `unknown field "bogus"`, false)

	err4 := assertSingleFailureRecord(t, lines[3], 4, 1,
		"conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9", true)
	conflict := err4["conflict"].(map[string]interface{})
	if conflict["existing"].(float64) != 2 || conflict["submitted"].(float64) != 9 ||
		int(conflict["timestamp"].(float64)) != 1000 {
		t.Fatalf("line 4 conflict detail = %v, want ts1000 existing=2 submitted=9", conflict)
	}

	assertCPUHostAQueryRecord(t, lines[4], 1, 2)
}

// TestRunIngestUnclosedArrayBeatsConflictCondition 固定边界：数组未闭合时，
// 即使前面的点已经具备冲突条件（第一个点是合法新增、第二个点对象完整且
// 与已存点冲突），也只返回整行解析错误，不带 index 或 conflict；行内新增点
// 不留存，查询仍只有基线点；退出码非零。
func TestRunIngestUnclosedArrayBeatsConflictCondition(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：基线写入。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：数组未闭合，但第二个点已满足冲突条件。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}`,
		// 行 3：失败后查询。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero because the unclosed line failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d output lines, want 3: %v", len(lines), lines)
	}

	assertSingleFailureRecord(t, lines[1], 2, 0, "invalid JSON", false)
	assertCPUHostAQueryRecord(t, lines[2], 1, 2)
}
