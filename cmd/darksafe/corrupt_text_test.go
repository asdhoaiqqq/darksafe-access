package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件覆盖损坏文本处理在 ingest 命令端到端路径上的行为：
// 非法 UTF-8 字节与未配对代理项转义都是整行失败（保留原始行号、无 index、
// 无 conflict、两类原因可区分），不提交该行任何点、查询不返回替换匹配；
// 此前数据保留、后续行继续处理、进程以非零退出码结束。

// decodeOutputLines 把 ingest 输出拆成已解码的结果行。
func decodeOutputLines(t *testing.T, out string) []map[string]interface{} {
	t.Helper()
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	got := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(row), &m); err != nil {
			t.Fatalf("invalid JSON result %q: %v", row, err)
		}
		got = append(got, m)
	}
	return got
}

func TestRunIngestCorruptTextEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：合法写入一个名字含 U+FFFD 的指标。
		`[{"name":"m�","timestamp":1,"value":1,"labels":{"host":"a"}}]`,
		// 行 2：指标名含未配对高代理项（若被修补会与行 1 重复）——整行失败。
		`[{"name":"m\uD800","timestamp":1,"value":1}]`,
		// 行 3：空白行仍只占行号、无输出。
		``,
		// 行 4：数组第一个点合法，第二个点标签值含原始非法 UTF-8 字节——整行失败，
		// 第一个点也不得提交。
		`[{"name":"new","timestamp":2,"value":2},{"name":"m","timestamp":3,"value":3,"labels":{"k":"v` + "\xff" + `"}}]`,
		// 行 5：损坏查询（标签值代理项），不得返回替换后的匹配。
		`{"op":"query","name":"m�","start":0,"end":10,"labels":{"host":"a\uDC00"}}`,
		// 行 6：合法查询，行 1 的数据仍在。
		`{"op":"query","name":"m�","start":0,"end":10,"labels":{"host":"a"}}`,
		// 行 7：低代理项查询，整行失败。
		`{"op":"query","name":"m\uDE00","start":0,"end":10}`,
		// 行 8：合法写入仍按顺序继续处理。
		`[{"name":"other","timestamp":9,"value":5}]`,
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero because corrupt lines failed")
	}

	rows := decodeOutputLines(t, out.String())
	// 输出对应输入行：1(ok) 2(err) 4(err) 5(err) 6(ok) 7(err) 8(ok)，共 7 条。
	if len(rows) != 7 {
		t.Fatalf("got %d result rows, want 7: %v", len(rows), rows)
	}

	if rows[0]["status"] != "ok" || rows[0]["added"].(float64) != 1 {
		t.Fatalf("row 1 = %v, want successful write", rows[0])
	}

	err2 := rows[1]
	if err2["status"] != "error" || int(err2["line"].(float64)) != 2 {
		t.Fatalf("row 2 = %v, want error on input line 2", err2)
	}
	if _, has := err2["index"]; has {
		t.Fatalf("corrupt-line error must not carry index: %v", err2)
	}
	if _, has := err2["conflict"]; has {
		t.Fatalf("corrupt-line error must not carry conflict: %v", err2)
	}
	msg2, _ := err2["error"].(string)
	if !strings.Contains(msg2, "surrogate") || strings.Contains(msg2, "valid UTF-8 text") {
		t.Fatalf("line 2 error = %q, want a surrogate-specific reason distinct from UTF-8", msg2)
	}

	err4 := rows[2]
	if err4["status"] != "error" || int(err4["line"].(float64)) != 4 {
		t.Fatalf("row 3 = %v, want error on input line 4", err4)
	}
	if _, has := err4["index"]; has {
		t.Fatalf("UTF-8 corrupt line must not carry index (whole line rejected): %v", err4)
	}
	if _, has := err4["conflict"]; has {
		t.Fatalf("UTF-8 corrupt line must not carry conflict: %v", err4)
	}
	msg4, _ := err4["error"].(string)
	if !strings.Contains(msg4, "valid UTF-8") || strings.Contains(msg4, "surrogate") {
		t.Fatalf("line 4 error = %q, want a UTF-8-specific reason distinct from surrogate", msg4)
	}

	err5 := rows[3]
	if err5["status"] != "error" || int(err5["line"].(float64)) != 5 {
		t.Fatalf("row 4 = %v, want error on input line 5", err5)
	}

	q6 := rows[4]
	if q6["status"] != "ok" || q6["op"] != "query" {
		t.Fatalf("row 5 = %v, want successful query", q6)
	}
	series := q6["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query after corrupt lines = %v, want exactly the one committed point", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 1 {
		t.Fatalf("post-corruption query point = %v, want count 1 average 1", s0)
	}

	err7 := rows[5]
	if err7["status"] != "error" || int(err7["line"].(float64)) != 7 {
		t.Fatalf("row 6 = %v, want error on input line 7", err7)
	}
	if _, has := err7["index"]; has {
		t.Fatalf("query text errors must not carry index: %v", err7)
	}

	ok8 := rows[6]
	if ok8["status"] != "ok" || ok8["added"].(float64) != 1 {
		t.Fatalf("row 7 = %v, want processing to continue with a successful write", ok8)
	}
}

func TestRunIngestSurrogateTypesAndEscapedBackslash(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"a\\uD800plain","timestamp":1,"value":4}]`,      // 行 1：转义反斜杠 + uD800 是普通文本，合法
		`[{"name":"a😀","timestamp":1,"value":5}]`,                 // 行 2：合法代理对（emoji），合法
		`[{"name":"x\uDBFF","timestamp":1,"value":1}]`,            // 行 3：单独高代理项，失败
		`[{"name":"x\uDC00","timestamp":1,"value":1}]`,            // 行 4：单独低代理项，失败
		`{"op":"query","name":"a\\uD800plain","start":0,"end":2}`, // 行 5：普通文本可查到
	}, "\n")

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	rows := decodeOutputLines(t, out.String())
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5: %v", len(rows), rows)
	}
	if rows[0]["status"] != "ok" {
		t.Fatalf("escaped-backslash text line = %v, want ok", rows[0])
	}
	if rows[1]["status"] != "ok" {
		t.Fatalf("valid surrogate-pair line = %v, want ok", rows[1])
	}
	for i, wantLine := range []int{3, 4} {
		m := rows[2+i]
		if m["status"] != "error" || int(m["line"].(float64)) != wantLine {
			t.Fatalf("row %d = %v, want surrogate error on line %d", i+2, m, wantLine)
		}
		if !strings.Contains(m["error"].(string), "surrogate") {
			t.Fatalf("row %d error = %q, want surrogate reason", i+2, m["error"])
		}
	}
	q := rows[4]
	if q["status"] != "ok" {
		t.Fatalf("plain-text query = %v, want ok", q)
	}
	if n := len(q["series"].([]interface{})); n != 1 {
		t.Fatalf("plain-text query series = %v, want 1 match", q["series"])
	}
}
