package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// 端到端回归：重复字段以 JSON uXXXX 转义书写、还原后同名时，
// 写入批次失败（index 指向出错采样点、无 conflict、整批不提交），
// 查询对象失败（无 index/conflict、不返回成功结果）；
// 原始行号保留、失败行之后的合法请求继续处理、退出码非零。

// escapedJSONKeys 把每个键的每个 ASCII 字节改写成 JSON 的 uXXXX 转义写法
// （反斜杠用字节 92 生成，避免源码转义歧义），如 "host" 改写为转义形式的同名键。
func escapedJSONKeys(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteByte(92)
		fmt.Fprintf(&b, "u%04x", s[i])
	}
	return b.String()
}

func TestRunIngestEscapedDuplicateKeys(t *testing.T) {
	escName := escapedJSONKeys("name")
	escHost := escapedJSONKeys("host")

	input := strings.Join([]string{
		// 行 1：成功写入一个 cpu 点。
		`[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]`,
		// 行 2：空白行，不产生结果但计入行号。
		``,
		// 行 3：第一个点合法（本批新增），第二个点的 labels 以转义写法重复 host：
		// 整批失败、index=2、无 conflict，第一个点也不得提交。
		`[{"name":"new","timestamp":1,"value":1},{"name":"new","timestamp":2,"value":2,"labels":{"host":"a","` + escHost + `":"b"}}]`,
		// 行 4：查询对象顶层以转义写法重复 name：查询错误，无 index/conflict。
		`{"op":"query","name":"cpu","` + escName + `":"cpu","start":0,"end":3000}`,
		// 行 5：查询对象的标签条件以转义写法重复 host：同样查询错误。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a","` + escHost + `":"b"}}`,
		// 行 6：合法查询：此前数据不变（仍只有行 1 的一个点，均值 0.5），
		// 且行 3 批次的第一个点未随失败批次提交。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`,
		// 行 7：只出现一次的转义字段名仍被接受，写入照常成功。
		`[{"` + escapedJSONKeys("name") + `":"m","` + escapedJSONKeys("timestamp") +
			`":1,"` + escapedJSONKeys("value") + `":3}]`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero because duplicate-key lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d result lines, want 6 (write ok, 3 errors, query ok, write ok): %v",
			len(lines), lines)
	}

	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" {
		t.Fatalf("line 1 = %v, want successful write", m)
	}

	// 行 3：批次错误，带原始行号与采样点 index=2，原因点名 host，且无 conflict。
	bad := decodeResultLine(t, lines[1])
	if bad["status"] != "error" || int(bad["line"].(float64)) != 3 {
		t.Fatalf("second output = %v, want error on input line 3", bad)
	}
	if int(bad["index"].(float64)) != 2 {
		t.Fatalf("line 3 index = %v, want 2 (1-based sample position)", bad["index"])
	}
	if _, has := bad["conflict"]; has {
		t.Fatalf("a duplicate key failure must not carry conflict: %v", bad)
	}
	if msg, _ := bad["error"].(string); !strings.Contains(msg, "host") ||
		!strings.Contains(msg, "duplicate") {
		t.Fatalf("line 3 error = %q, want it to report duplicate key host", msg)
	}

	// 行 4、行 5：查询错误，保留原始行号，无 index、无 conflict。
	for i, wantLine := range []int{4, 5} {
		m := decodeResultLine(t, lines[2+i])
		if m["status"] != "error" || int(m["line"].(float64)) != wantLine {
			t.Fatalf("output %d = %v, want query error on input line %d", i+2, m, wantLine)
		}
		if _, has := m["index"]; has {
			t.Fatalf("line %d query error must not carry index: %v", wantLine, m)
		}
		if _, has := m["conflict"]; has {
			t.Fatalf("line %d query error must not carry conflict: %v", wantLine, m)
		}
		if msg, _ := m["error"].(string); !strings.Contains(msg, "duplicate") {
			t.Fatalf("line %d error = %q, want a duplicate-key reason", wantLine, msg)
		}
	}

	// 行 6：失败批次之后查询仍成功；只命中行 1 的点，count=1、average=0.5。
	q := decodeResultLine(t, lines[4])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 6 = %v, want successful query after failed lines", q)
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("line 6 series = %v, want exactly the earlier committed point", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 0.5 {
		t.Fatalf("line 6 series[0] = %v, want count=1 average=0.5", s0)
	}

	// 行 7：只含转义字段名的合法写入成功（行 3 失败批次的 new 未被提交，
	// 这里的 m 与它无关，故 added=1）。
	last := decodeResultLine(t, lines[5])
	if last["status"] != "ok" || last["added"].(float64) != 1 {
		t.Fatalf("line 7 = %v, want successful write with one added point", last)
	}
}
