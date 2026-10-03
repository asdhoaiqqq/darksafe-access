package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// errReader 在任何读取时都返回固定错误，用于模拟真实输入故障。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("simulated read failure") }

// decodeOutputLines 把逐行 JSON 输出切分为通用 map，要求每行都是合法 JSON。
func decodeOutputLines(t *testing.T, out string) []map[string]interface{} {
	t.Helper()
	text := strings.TrimRight(out, "\n")
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	results := make([]map[string]interface{}, 0, len(parts))
	for i, p := range parts {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(p), &m); err != nil {
			t.Fatalf("output line %d is not JSON: %v (%q)", i+1, err, p)
		}
		results = append(results, m)
	}
	return results
}

// paddedJSONLine 构造一条恰好 size 字节的合法写入行：前缀是一个完整采样点，
// 其后填充空白，最后以 ']' 收尾；JSON 允许对象与 ']' 之间存在任意空白。
func paddedJSONLine(size int) string {
	prefix := `[{"name":"cpu","timestamp":1,"value":1}`
	suffix := "]"
	if len(prefix)+len(suffix) > size {
		panic("line too short for padding")
	}
	return prefix + strings.Repeat(" ", size-len(prefix)-len(suffix)) + suffix
}

// oversizedLine 返回一条 size 字节的非法超长行：开头是合法 JSON 采样点前缀，
// 随后用 'x' 填充。整行不是合法 JSON，但超长时根本不应被解析。
func oversizedLine(size int) string {
	prefix := `[{"name":"cpu","timestamp":2,"value":2}`
	if len(prefix) > size {
		panic("line too short for prefix")
	}
	return prefix + strings.Repeat("x", size-len(prefix))
}

func TestRunIngestLineExactlyAtLimitSucceeds(t *testing.T) {
	limit := darksafe.MaxLineBytes
	// 恰好达到上限：带 '\n' 与带 '\r\n' 两种结尾都必须正常写入。
	input := paddedJSONLine(limit) + "\n" + paddedJSONLine(limit) + "\r\n" +
		`{"op":"query","name":"cpu","start":0,"end":10}` + "\n"

	var out bytes.Buffer
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 for lines at the size limit", code)
	}
	results := decodeOutputLines(t, out.String())
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: %v", len(results), results)
	}
	for i, want := range []map[string]float64{
		{"added": 1, "duplicates": 0}, // 第一条点写入
		{"added": 0, "duplicates": 1}, // 完全相同的点：重复，成功忽略
	} {
		if results[i]["status"] != "ok" ||
			results[i]["added"].(float64) != want["added"] ||
			results[i]["duplicates"].(float64) != want["duplicates"] {
			t.Fatalf("write at limit #%d = %v, want %v", i+1, results[i], want)
		}
	}
	q := results[2]
	series := q["series"].([]interface{})
	if len(series) != 1 || series[0].(map[string]interface{})["count"].(float64) != 1 {
		t.Fatalf("query after at-limit writes = %v, want one point", series)
	}
}

func TestRunIngestLineExactlyAtLimitWithoutTrailingNewline(t *testing.T) {
	limit := darksafe.MaxLineBytes
	var out bytes.Buffer
	// 输入在超长判定边界处直接结束、没有最后一个换行。
	if code := runIngest(strings.NewReader(paddedJSONLine(limit)), &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	results := decodeOutputLines(t, out.String())
	if len(results) != 1 || results[0]["status"] != "ok" {
		t.Fatalf("results = %v", results)
	}
}

func TestRunIngestOversizedLineIsContained(t *testing.T) {
	limit := darksafe.MaxLineBytes
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1,"value":1}]`,       // 行 1：成功写入
		oversizedLine(limit + 1),                         // 行 2：超长（合法前缀不得被当成完整请求）
		`{"op":"query","name":"cpu","start":0,"end":10}`, // 行 3：只能看到行 1 的数据
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because a line exceeded the limit")
	}
	results := decodeOutputLines(t, out.String())
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 (ok, size error, query ok)", len(results))
	}

	if results[0]["status"] != "ok" || results[0]["added"].(float64) != 1 {
		t.Fatalf("line 1 = %v, want successful write", results[0])
	}

	err2 := results[1]
	if err2["status"] != "error" {
		t.Fatalf("line 2 = %v, want status error", err2)
	}
	if int(err2["line"].(float64)) != 2 {
		t.Fatalf("size error line = %v, want 2", err2["line"])
	}
	if _, hasIndex := err2["index"]; hasIndex {
		t.Fatalf("size error must not carry index: %v", err2)
	}
	if _, hasConflict := err2["conflict"]; hasConflict {
		t.Fatalf("size error must not carry conflict: %v", err2)
	}
	msg, _ := err2["error"].(string)
	if !strings.Contains(msg, "67108864") || !strings.Contains(strings.ToLower(msg), "limit") {
		t.Fatalf("size error message must state the limit value 67108864: %q", msg)
	}

	series := results[2]["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %v, want exactly the one point from line 1", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 1 {
		t.Fatalf("query saw data from the failed over-long line: %v", s0)
	}
}

func TestRunIngestOversizedLineAtEOFWithoutNewline(t *testing.T) {
	limit := darksafe.MaxLineBytes
	// 行 1 正常，行 2 超长且输入直接结束、没有最后一个换行。
	input := `[]` + "\n" + oversizedLine(limit+1)

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	results := decodeOutputLines(t, out.String())
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (ok then one size error): %v", len(results), results)
	}
	if results[0]["status"] != "ok" {
		t.Fatalf("line 1 = %v", results[0])
	}
	if results[1]["status"] != "error" || int(results[1]["line"].(float64)) != 2 {
		t.Fatalf("line 2 = %v, want one size error on input line 2", results[1])
	}
}

func TestRunIngestTwoConsecutiveOversizedLines(t *testing.T) {
	limit := darksafe.MaxLineBytes
	input := strings.Join([]string{
		oversizedLine(limit + 1),                 // 行 1：超长
		"   ",                                    // 行 2：空白，只占行号
		oversizedLine(limit + 1),                 // 行 3：超长
		oversizedLine(limit + 10),                // 行 4：超长（CRLF 结尾）
		`[{"name":"m","timestamp":1,"value":7}]`, // 行 5：正常写入
	}, "\n") + "\r\n"

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	results := decodeOutputLines(t, out.String())
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4 (3 size errors + 1 ok): %v", len(results), results)
	}
	for i, wantLine := range []int{1, 3, 4} {
		m := results[i]
		if m["status"] != "error" || int(m["line"].(float64)) != wantLine {
			t.Fatalf("result %d = %v, want size error on input line %d", i, m, wantLine)
		}
		if _, hasIndex := m["index"]; hasIndex {
			t.Fatalf("result %d must not carry index", i)
		}
	}
	if results[3]["status"] != "ok" || results[3]["added"].(float64) != 1 {
		t.Fatalf("valid line after oversized lines = %v, want successful write", results[3])
	}
}

func TestRunIngestReadFailureStillTerminates(t *testing.T) {
	// 先给出一行正常输入，随后底层读取真正故障：必须输出已有结果并以非零终止，
	// 而不是把故障当成普通失败行继续。
	in := io.MultiReader(strings.NewReader(`[]`+"\n"), errReader{})
	var out bytes.Buffer
	if code := runIngest(in, &out); code != 1 {
		t.Fatalf("exit code = %d, want 1 on read failure", code)
	}
	results := decodeOutputLines(t, out.String())
	if len(results) != 1 || results[0]["status"] != "ok" {
		t.Fatalf("results = %v, want the one line read before the failure", results)
	}
}

func TestLineReaderSplitsLikeScanner(t *testing.T) {
	input := "a\nb\r\nc\r\n\nd" // a, b(CRLF), c(CRLF), 空行, d(无尾换行)
	r := newLineReader(strings.NewReader(input))
	got := []string{}
	for {
		line, tooLong, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tooLong {
			t.Fatalf("short input must never be reported too long")
		}
		got = append(got, line)
	}
	want := []string{"a", "b", "c", "", "d"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

func TestLineReaderEmptyAndTrailingNewline(t *testing.T) {
	for _, input := range []string{"", "\n", "\r\n"} {
		r := newLineReader(strings.NewReader(input))
		var calls int
		for {
			_, _, err := r.Next()
			if err == io.EOF {
				break
			}
			calls++
			if err != nil {
				t.Fatalf("input %q: unexpected error %v", input, err)
			}
		}
		if input == "" && calls != 0 {
			t.Fatalf("empty input produced %d lines", calls)
		}
		if input != "" && calls != 1 {
			t.Fatalf("input %q produced %d lines, want exactly 1 blank line", input, calls)
		}
	}
}

// chunkReader 每次最多返回 n 个字节，模拟管道等会把输入切碎的来源。
type chunkReader struct {
	data []byte
	n    int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(c.data) {
		n = len(c.data)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func TestLineReaderFragmentedReads(t *testing.T) {
	// '\r\n' 与各分隔符故意落在读取块边界两侧，最后一行无尾换行。
	input := "ab\r\ncd\nef\r\ng"
	r := newLineReader(&chunkReader{data: []byte(input), n: 3})
	want := []string{"ab", "cd", "ef", "g"}
	var got []string
	for {
		line, tooLong, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tooLong {
			t.Fatalf("short fragmented input must not be too long")
		}
		got = append(got, line)
	}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q (got all %q)", i+1, got[i], want[i], got)
		}
	}
}

// 超长行以固定大小块到达：'\n' 无法进入受限缓冲，必须经由 drainLine 跨块
// 扫描找到分隔符，并完整保留同一块中分隔符之后属于下一行的字节。
func TestRunIngestOversizedLineFragmentedAcrossChunks(t *testing.T) {
	limit := darksafe.MaxLineBytes
	payload := make([]byte, 0, limit+100+1+3)
	payload = append(payload, strings.Repeat("x", limit+100)...) // 行 1：超长
	payload = append(payload, '\n')
	payload = append(payload, "[]\n"...) // 行 2：正常空批

	var out bytes.Buffer
	code := runIngest(&chunkReader{data: payload, n: 64 * 1024}, &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	results := decodeOutputLines(t, out.String())
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2: %v", len(results), results)
	}
	if results[0]["status"] != "error" || int(results[0]["line"].(float64)) != 1 {
		t.Fatalf("first result = %v, want size error on line 1", results[0])
	}
	if results[1]["status"] != "ok" {
		t.Fatalf("second result = %v, want successful empty batch on line 2", results[1])
	}
}
