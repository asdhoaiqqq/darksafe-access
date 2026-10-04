package main

import (
	"fmt"
	"strings"
	"testing"
)

// 端到端回归：labels 中合法字符串值的标签键再次出现时，第二次值的类型
// （数字、数组等）不能掩盖重复标签键错误。写入批次错误带从 1 开始的采样点
// index、不带 conflict，整批新增点不提交；查询错误不带 index、不带 conflict，
// 也不返回结果。命令保留从 1 开始的原始行号（含空白行占位），失败行之后的
// 合法请求继续处理，出现失败行时退出码非零。

// jsonHexFirst 把键的首字符改写为 JSON 十六进制转义（host → 反斜杠 u0068ost）。
// 反斜杠用 rune(92) 构造，源码里无需直接书写反斜杠转义序列。
func jsonHexFirst(s string) string {
	return string(rune(92)) + "u" + fmt.Sprintf("%04x", s[0]) + s[1:]
}

func TestRunIngestDuplicateLabelKeyNotMaskedBySecondValueType(t *testing.T) {
	escHost := jsonHexFirst("host") // 还原后仍为 host
	input := strings.Join([]string{
		// 行 1：基线写入成功，cpu/host=a 在 1000ms 有一个值为 2 的点。
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：同序列先放入 2000ms 值 4 的新点，第二个采样点的标签键 host
		// 第二次出现且值是数字——整批失败，index 指向采样点 2。
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":3000,"value":5,"labels":{"host":"a","host":7}}]`,
		// 行 3：查询标签条件里 host 第二次出现且值是数字——查询错误。
		`{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a","host":7}}`,
		// 行 4：空白行不产生结果，但仍占用行号。
		``,
		// 行 5：转义书写在前（值合法）、直接书写在后且第二次值为数组——查询错误。
		`{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"` + escHost + `":"a","host":[]}}`,
		// 行 6：此前只成功写入基线一个点：count=1、average=2（2000ms 的点未提交）。
		`{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`,
		// 行 7：失败行之后的合法写入照常处理。
		`[{"name":"tail","timestamp":1,"value":9}]`,
		// 行 8：新写入的数据可查询。
		`{"op":"query","name":"tail","start":0,"end":10}`,
	}, "\n")

	var out strings.Builder
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because duplicate-label-key lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d output lines, want 7 (blank line produces none): %v", len(lines), lines)
	}

	// 行 1 成功。
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 1 = %v, want ok added 1", m)
	}

	// 行 2：写入批次失败，line=2、index=2，原因指出还原后的重复标签键 host，
	// 无 conflict，且不能是第二次值的类型错误。
	m := decodeResultLine(t, lines[1])
	if m["status"] != "error" || int(m["line"].(float64)) != 2 || int(m["index"].(float64)) != 2 {
		t.Fatalf("line 2 = %v, want error line=2 index=2", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 2 duplicate label key must not carry conflict: %v", m)
	}
	msg, _ := m["error"].(string)
	if !strings.Contains(msg, `duplicate label key "host"`) {
		t.Fatalf("line 2 error = %q, want duplicate label key host", msg)
	}
	if strings.Contains(msg, "must be a string") {
		t.Fatalf("line 2 duplicate key must not be masked by value type: %q", msg)
	}

	// 行 3：查询失败，line=3，无 index、无 conflict、无结果。
	m = decodeResultLine(t, lines[2])
	if m["status"] != "error" || int(m["line"].(float64)) != 3 {
		t.Fatalf("line 3 = %v, want error on input line 3", m)
	}
	if _, has := m["index"]; has {
		t.Fatalf("line 3 query error must not carry index: %v", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 3 query error must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate label key "host"`) ||
		strings.Contains(msg, "must be a string") {
		t.Fatalf("line 3 error = %q, want unmasked duplicate label key host", msg)
	}

	// 行 5（空白行占位为行 4）：转义在前、直接在后的重复标签键，查询错误。
	m = decodeResultLine(t, lines[3])
	if m["status"] != "error" || int(m["line"].(float64)) != 5 {
		t.Fatalf("line 5 = %v, want error on input line 5 (blank line keeps its number)", m)
	}
	if _, has := m["index"]; has {
		t.Fatalf("line 5 query error must not carry index: %v", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("line 5 query error must not carry conflict: %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, `duplicate label key "host"`) ||
		strings.Contains(msg, "must be a string") {
		t.Fatalf("line 5 error = %q, want unmasked duplicate label key host", msg)
	}

	// 行 6：失败批次的新增点没有提交，基线仍是唯一的点，count=1、average=2。
	q := decodeResultLine(t, lines[4])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 6 = %v, want ok query", q)
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 6 series = %v, want only the baseline point (2000ms not committed)", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["name"] != "cpu" || s0["count"].(float64) != 1 || s0["average"].(float64) != 2 {
		t.Fatalf("line 6 series[0] = %v, want cpu count=1 average=2", s0)
	}

	// 行 7：失败行之后的合法写入照常处理。
	m = decodeResultLine(t, lines[5])
	if m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 7 = %v, want ok added=1", m)
	}

	// 行 8：tail 可查询。
	q = decodeResultLine(t, lines[6])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 8 = %v, want ok query", q)
	}
	series = q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 8 series = %v, want exactly tail", series)
	}
	s0 = series[0].(map[string]interface{})
	if s0["name"] != "tail" || s0["count"].(float64) != 1 || s0["average"].(float64) != 9 {
		t.Fatalf("line 8 series[0] = %v, want tail count=1 average=9", s0)
	}
}
