package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// repeatReader 无限重复输出单个字节 n 次，用于在不分配整块 64 MiB 的情况下
// 构造超长输入，同时让读路径经过大量 64 KiB 缓冲填充。
type repeatReader struct {
	b byte
	n int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	c := len(p)
	if c > r.n {
		c = r.n
	}
	for i := range p[:c] {
		p[i] = r.b
	}
	r.n -= c
	return c, nil
}

// failAfterReader 先透传前 n 个字节，随后返回固定错误，模拟真实读取故障。
type failAfterReader struct {
	data []byte
	n    int
	err  error
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.n >= len(r.data) {
		return 0, r.err
	}
	c := copy(p, r.data[r.n:])
	r.n += c
	if r.n >= len(r.data) {
		return c, r.err
	}
	return c, nil
}

// overlongLine 返回一段“合法 JSON 前缀 + 空白填充”的超长行内容，
// 总长为 maxIngestLineBytes+delta，且不含换行。前缀里的采样点绝不能被写入。
func overlongLine(delta int) io.Reader {
	prefix := `[{"name":"cpu","timestamp":2000,"value":9},`
	return io.MultiReader(
		strings.NewReader(prefix),
		&repeatReader{b: ' ', n: maxIngestLineBytes + delta - len(prefix)},
	)
}

func decodeResultLine(t *testing.T, line string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid JSON result %q: %v", line, err)
	}
	return m
}

func TestRunIngestLineExactlyAtSizeLimitStillProcessed(t *testing.T) {
	// 内容恰好 67,108,864 字节（外层一对方括号之间全部是空白），仍应进入处理。
	input := io.MultiReader(
		strings.NewReader("["),
		&repeatReader{b: ' ', n: maxIngestLineBytes - 2},
		strings.NewReader("]\n"),
	)
	var out bytes.Buffer
	if code := runIngest(input, &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 for a line exactly at the limit", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d output lines, want 1", len(lines))
	}
	m := decodeResultLine(t, lines[0])
	if m["status"] != "ok" || m["added"].(float64) != 0 {
		t.Fatalf("exact-limit line result = %v, want ok empty batch", m)
	}
}

func TestRunIngestOverlongLineIsIsolated(t *testing.T) {
	input := io.MultiReader(
		// 行 1：先成功写入 cpu 的一个点。
		strings.NewReader(`[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]`+"\n"),
		// 行 2：超长批次（超过上限 1 字节），带合法 JSON 前缀，整行失败。
		overlongLine(1),
		strings.NewReader("\n"),
		// 行 3：后续查询必须照常得到结果，且只能看到行 1 的数据。
		strings.NewReader(`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`+"\n"),
	)
	var out bytes.Buffer
	code := runIngest(input, &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because a line exceeded the limit")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d output lines, want 3 (write ok, over-long error, query ok): %v",
			len(lines), lines)
	}

	first := decodeResultLine(t, lines[0])
	if first["status"] != "ok" || first["added"].(float64) != 1 {
		t.Fatalf("line 1 = %v, want successful write", first)
	}

	bad := decodeResultLine(t, lines[1])
	if bad["status"] != "error" || int(bad["line"].(float64)) != 2 {
		t.Fatalf("line 2 = %v, want size-limit error on input line 2", bad)
	}
	if _, hasIndex := bad["index"]; hasIndex {
		t.Fatalf("over-long line error must not carry index: %v", bad)
	}
	if _, hasConflict := bad["conflict"]; hasConflict {
		t.Fatalf("over-long line error must not carry conflict: %v", bad)
	}
	msg, _ := bad["error"].(string)
	if !strings.Contains(msg, "67108864") || !strings.Contains(msg, "single-line size limit") {
		t.Fatalf("error message %q must state the size limit and its byte value", msg)
	}

	q := decodeResultLine(t, lines[2])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("line 3 = %v, want successful query after the over-long line", q)
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %v, want exactly the one committed point", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 0.5 {
		t.Fatalf("query after over-long line = %v, want only the earlier point", s0)
	}
}

func TestRunIngestConsecutiveOverlongLinesReportEachLineNumber(t *testing.T) {
	input := io.MultiReader(
		strings.NewReader("[]\n"),                // 行 1：正常
		overlongLine(1), strings.NewReader("\n"), // 行 2：超长
		overlongLine(5), strings.NewReader("\n"), // 行 3：再次超长（超 5 字节）
		strings.NewReader("[]\n"), // 行 4：仍能正常处理
	)
	var out bytes.Buffer
	code := runIngest(input, &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d output lines, want 4: %v", len(lines), lines)
	}
	if decodeResultLine(t, lines[0])["status"] != "ok" {
		t.Fatalf("line 1 = %v, want ok", lines[0])
	}
	for i, wantLine := range []int{2, 3} {
		m := decodeResultLine(t, lines[1+i])
		if m["status"] != "error" || int(m["line"].(float64)) != wantLine {
			t.Fatalf("output %d = %v, want size error on input line %d", i+1, m, wantLine)
		}
		if _, hasIndex := m["index"]; hasIndex {
			t.Fatalf("over-long error on line %d must not carry index: %v", wantLine, m)
		}
	}
	if decodeResultLine(t, lines[3])["status"] != "ok" {
		t.Fatalf("line 4 = %v, want ok after two over-long lines", lines[3])
	}
}

func TestRunIngestOverlongFinalLineWithoutNewlineReportedOnce(t *testing.T) {
	input := io.MultiReader(
		strings.NewReader("[]\n"), // 行 1：正常
		overlongLine(1),           // 行 2：超长且输入在此直接结束，没有最后一个换行
	)
	var out bytes.Buffer
	code := runIngest(input, &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d output lines, want exactly 2 (ok + one error, no duplicate): %v",
			len(lines), lines)
	}
	if decodeResultLine(t, lines[0])["status"] != "ok" {
		t.Fatalf("line 1 = %v, want ok", lines[0])
	}
	m := decodeResultLine(t, lines[1])
	if m["status"] != "error" || int(m["line"].(float64)) != 2 {
		t.Fatalf("final output = %v, want one error on input line 2", m)
	}
}

func TestRunIngestOverlongWhitespaceLineStillFails(t *testing.T) {
	// 上限以内的空白行不产生结果；超过上限的空白行仍是一条超长失败行。
	input := io.MultiReader(
		strings.NewReader(`{"op":"query","name":"m","start":0,"end":1}`+"\n"),     // 行 1
		&repeatReader{b: ' ', n: maxIngestLineBytes + 1}, strings.NewReader("\n"), // 行 2
		strings.NewReader("   \n"), // 行 3：上限以内空白，只占行号
		strings.NewReader(`{"op":"query","name":"m","start":0,"end":1}`+"\n"), // 行 4
	)
	var out bytes.Buffer
	code := runIngest(input, &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d output lines, want 3 (query ok, size error, query ok): %v",
			len(lines), lines)
	}
	if decodeResultLine(t, lines[0])["status"] != "ok" {
		t.Fatalf("line 1 = %v, want ok", lines[0])
	}
	m := decodeResultLine(t, lines[1])
	if m["status"] != "error" || int(m["line"].(float64)) != 2 {
		t.Fatalf("line 2 = %v, want size error on input line 2", m)
	}
	if decodeResultLine(t, lines[2])["status"] != "ok" {
		t.Fatalf("line 4 = %v, want ok", lines[2])
	}
}

func TestRunIngestReadFailureStillTerminates(t *testing.T) {
	// 首行尚未读到换行时底层读取发生故障：按现有方式终止，返回非零。
	in := &failAfterReader{
		data: []byte(`[{"name":"cpu","timestamp":1000,"value":1}]`),
		err:  io.ErrClosedPipe,
	}
	var out bytes.Buffer
	if code := runIngest(in, &out); code == 0 {
		t.Fatalf("exit code = 0, want non-zero on a real read failure")
	}
	if out.Len() != 0 {
		t.Fatalf("no result may be emitted for a line that never completed, got %q", out.String())
	}
}

func TestRunIngestEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":1}]`, // 行 1：成功
		``,         // 行 2：空白行，无输出但计行号
		`   `,      // 行 3：空白行
		`not json`, // 行 4：整行解析失败
		`[{"name":"cpu","timestamp":1000,"value":2}]`,   // 行 5：冲突（此前数据保留）
		`[{"name":"cpu","timestamp":1000,"value":1.0}]`, // 行 6：重复成功
		`[]`, // 行 7：空批成功
		``,   // 行 8：结尾空白行
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because batches failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d result lines, want 5: %v", len(lines), lines)
	}

	var ok1 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &ok1); err != nil {
		t.Fatal(err)
	}
	if ok1["status"] != "ok" {
		t.Fatalf("line 1 status = %v", ok1["status"])
	}

	var err4 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &err4); err != nil {
		t.Fatal(err)
	}
	if err4["status"] != "error" || int(err4["line"].(float64)) != 4 {
		t.Fatalf("second output line = %v, want parse error on line 4", err4)
	}
	if _, hasIndex := err4["index"]; hasIndex {
		t.Fatalf("whole-line parse error must not carry index: %v", err4)
	}

	var err5 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &err5); err != nil {
		t.Fatal(err)
	}
	if err5["status"] != "error" || int(err5["line"].(float64)) != 5 {
		t.Fatalf("third output line = %v, want conflict error on line 5", err5)
	}
	if int(err5["index"].(float64)) != 1 {
		t.Fatalf("conflict index = %v, want 1", err5["index"])
	}
	conflict := err5["conflict"].(map[string]interface{})
	if conflict["existing"].(float64) != 1 || conflict["submitted"].(float64) != 2 {
		t.Fatalf("conflict values = %v", conflict)
	}

	var ok6 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[3]), &ok6); err != nil {
		t.Fatal(err)
	}
	if ok6["status"] != "ok" || ok6["duplicates"].(float64) != 1 || ok6["added"].(float64) != 0 {
		t.Fatalf("line 6 = %v, want duplicate success", ok6)
	}

	var ok7 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[4]), &ok7); err != nil {
		t.Fatal(err)
	}
	if ok7["status"] != "ok" {
		t.Fatalf("line 7 = %v", ok7)
	}
	// 失败批次均已回滚，最终只有行 1 写入的一条序列。
	series := ok7["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("final series = %v, want exactly 1", series)
	}
}

func TestRunIngestAllSuccessExitsZero(t *testing.T) {
	input := `[{"name":"m","timestamp":1,"value":1}]` + "\n" + `[]` + "\n"
	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if n := len(strings.Split(strings.TrimRight(out.String(), "\n"), "\n")); n != 2 {
		t.Fatalf("got %d output lines, want 2", n)
	}
}

func TestRunIngestEmptyInput(t *testing.T) {
	var out bytes.Buffer
	if code := runIngest(strings.NewReader(""), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Fatalf("empty input must produce no output, got %q", out.String())
	}
}

func TestUsageMentionsIngest(t *testing.T) {
	var b bytes.Buffer
	usage(&b)
	text := b.String()
	for _, want := range []string{"ingest", "standard input", "json array", "added", "duplicates", "non-zero"} {
		if !strings.Contains(strings.ToLower(text), want) {
			t.Errorf("help text missing %q", want)
		}
	}
}

func TestRunIngestInterleavedQuery(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,         // 行 1：写入
		`{"op":"query","name":"cpu","start":1000,"end":1000,"labels":{"host":"a"}}`, // 行 2：查询，avg=2
		``, // 行 3：空白行
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,                  // 行 4：再写入
		`{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`,          // 行 5：avg=(2+4)/2=3
		`{"op":"query","name":"cpu","start":3000,"end":4000}`,                                // 行 6：无点，空数组
		`{"op":"bogus","name":"cpu","start":0,"end":1}`,                                      // 行 7：未知 op，失败
		`{"op":"query","name":"cpu","start":9,"end":1}`,                                      // 行 8：非法区间，失败
		`{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}} trailing`, // 行 9：整行解析失败
		`{"op":"query","name":"cpu","start":1000,"end":2000}`,                                // 行 10：仍可查询，结果不受失败行影响
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d result lines, want 9: %v", len(lines), lines)
	}

	checkQuery := func(idx int, wantCount float64, wantAvg float64) {
		t.Helper()
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(lines[idx]), &m); err != nil {
			t.Fatalf("output line %d: %v", idx, err)
		}
		if m["status"] != "ok" || m["op"] != "query" {
			t.Fatalf("output line %d = %v, want ok query", idx, m)
		}
		series := m["series"].([]interface{})
		if wantCount == 0 {
			if len(series) != 0 {
				t.Fatalf("output line %d series = %v, want empty", idx, series)
			}
			return
		}
		if len(series) != 1 {
			t.Fatalf("output line %d series = %v, want one entry", idx, series)
		}
		s0 := series[0].(map[string]interface{})
		if s0["name"] != "cpu" || s0["count"].(float64) != wantCount || s0["average"].(float64) != wantAvg {
			t.Fatalf("output line %d series[0] = %v", idx, s0)
		}
		labels := s0["labels"].(map[string]interface{})
		if labels["host"] != "a" || len(labels) != 1 {
			t.Fatalf("output line %d labels = %v", idx, labels)
		}
	}

	var line1 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &line1); err != nil {
		t.Fatal(err)
	}
	if line1["status"] != "ok" || line1["added"].(float64) != 1 {
		t.Fatalf("line 1 = %v", line1)
	}
	checkQuery(1, 1, 2)

	var line4 map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &line4); err != nil {
		t.Fatal(err)
	}
	if line4["status"] != "ok" || line4["added"].(float64) != 1 {
		t.Fatalf("line 4 = %v", line4)
	}
	checkQuery(3, 2, 3)
	checkQuery(4, 0, 0)

	for idx, wantLine := range map[int]int{5: 7, 6: 8, 7: 9} {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(lines[idx]), &m); err != nil {
			t.Fatalf("output line %d: %v", idx, err)
		}
		if m["status"] != "error" || int(m["line"].(float64)) != wantLine {
			t.Fatalf("output line %d = %v, want error on input line %d", idx, m, wantLine)
		}
		if _, hasIndex := m["index"]; hasIndex {
			t.Fatalf("query errors must not carry index: %v", m)
		}
	}

	// 失败行之后，此前数据仍可查询。
	checkQuery(8, 2, 3)
}

func TestRunIngestQueryErrorLineNumbers(t *testing.T) {
	input := strings.Join([]string{
		``,    // 行 1：空白
		`   `, // 行 2：空白
		`{"op":"query","name":"m","start":2,"end":1}`, // 行 3：非法区间
		`not json`, // 行 4：整行解析失败
	}, "\n")
	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	for i, wantLine := range []int{3, 4} {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if int(m["line"].(float64)) != wantLine {
			t.Fatalf("output %d = %v, want input line %d", i, m, wantLine)
		}
	}
}
