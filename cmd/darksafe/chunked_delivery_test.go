package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// chunkReader 按 sizes 循环给出每次 Read 的最大字节数，把同一份输入
// 拆成大小不一的片段送达，模拟标准输入分段到达。片段边界不携带任何
// 行结束含义：行只能以 '\n' 或输入结束为界。
type chunkReader struct {
	data  []byte
	sizes []int
	pos   int
	next  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.sizes[r.next%len(r.sizes)]
	r.next++
	if n < 1 {
		n = 1
	}
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// runIngestChunked 以给定片段大小分段送达输入，返回输出文本与退出码。
func runIngestChunked(t *testing.T, input []byte, sizes ...int) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := runIngest(&chunkReader{data: input, sizes: sizes}, &out)
	return out.String(), code
}

// 常用分段档位：1 字节覆盖所有可能的切分点（多字节 UTF-8 字符内部、
// 转义反斜杠之后、成对代理项中间），64 KiB 附近覆盖读取缓冲边界。
var chunkSizes = []int{1, 2, 3, 5, 7, 64, 4096, 65535, 65536, 65537, 1 << 20}

// TestRunIngestChunkedDeliveryMatchesWholeInput 同一份输入无论怎样分段，
// 输出字节与退出码都必须与一次性送达完全一致：片段结束不是行结束。
func TestRunIngestChunkedDeliveryMatchesWholeInput(t *testing.T) {
	input := []byte(strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`,
		``, // 空白行：无输出但占行号
		`[{"name":"指标😀","timestamp":1000,"value":1,"labels":{"环境":"生产"}}]`,
		// 与上一行同一序列的合法转义书写，等值同时间戳：重复。
		`[{"name":"\u6307\u6807\uD83D\uDE00","timestamp":1000,"value":1.0,"labels":{"\u73af\u5883":"\u751f\u4ea7"}}]`,
		// 用转义身份查询直接书写的序列。
		`{"op":"query","name":"\u6307\u6807\uD83D\uDE00","start":0,"end":2000,"labels":{"\u73af\u5883":"\u751f\u4ea7"}}`,
		`{"op":"query","name":"cpu","start":1000,"end":1000,"labels":{"host":"a"}}`,
		`   `, // 空白行
	}, "\n") + "\n")

	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode != 0 {
		t.Fatalf("whole-input exit code = %d, want 0", wantCode)
	}
	for _, size := range chunkSizes {
		gotOut, gotCode := runIngestChunked(t, input, size)
		if gotCode != wantCode {
			t.Fatalf("chunk size %d: exit code = %d, want %d", size, gotCode, wantCode)
		}
		if gotOut != wantOut {
			t.Fatalf("chunk size %d: output differs from whole-input delivery\n got: %q\nwant: %q",
				size, gotOut, wantOut)
		}
	}

	lines := strings.Split(strings.TrimRight(wantOut, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d output lines, want 5 (blank lines produce none): %v", len(lines), lines)
	}
	// 转义书写的等值重提计为重复，不产生新序列。
	dup := decodeResultLine(t, lines[2])
	if dup["status"] != "ok" || dup["added"].(float64) != 0 || dup["duplicates"].(float64) != 1 {
		t.Fatalf("escaped duplicate write = %v, want added=0 duplicates=1", dup)
	}
	if series := dup["series"].([]interface{}); len(series) != 2 {
		t.Fatalf("series after duplicate = %v, want exactly 2 series (no extra from escaping)", series)
	}
	// 查询结果保留还原后的真实名称与完整标签，无替换字符。
	q := decodeResultLine(t, lines[3])
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %v, want 1", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["name"] != "指标😀" {
		t.Fatalf("query series name = %q, want 指标😀 restored verbatim", s0["name"])
	}
	labels := s0["labels"].(map[string]interface{})
	if len(labels) != 1 || labels["环境"] != "生产" {
		t.Fatalf("query series labels = %v, want {环境:生产}", labels)
	}
	if strings.ContainsRune(lines[3], '�') {
		t.Fatalf("query output must not contain U+FFFD replacement: %q", lines[3])
	}
}

// TestRunIngestChunkedEveryTwoWaySplit 对同一份输入枚举每一个切分位置，
// 切成两段送达；任何切分点的输出都必须与整体送达一致。输入同时包含
// 中文、补充平面字符（直接书写与成对代理项转义）和转义反斜杠。
func TestRunIngestChunkedEveryTwoWaySplit(t *testing.T) {
	input := []byte(strings.Join([]string{
		`[{"name":"指😀","timestamp":1,"value":2,"labels":{"键":"值😀"}}]`,
		`[{"name":"\u6307\uD83D\uDE00","timestamp":1,"value":2.0,"labels":{"\u952E":"\u503C\uD83D\uDE00"}}]`,
		`{"op":"query","name":"指😀","start":0,"end":10}`,
	}, "\n") + "\n")

	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode != 0 {
		t.Fatalf("whole-input exit code = %d, want 0", wantCode)
	}
	for i := 0; i <= len(input); i++ {
		gotOut, gotCode := runIngestChunked(t, input, i, len(input)-i)
		if gotCode != wantCode || gotOut != wantOut {
			t.Fatalf("split at byte %d: (code %d, out %q), want (code %d, out %q)",
				i, gotCode, gotOut, wantCode, wantOut)
		}
	}
}

// TestRunIngestChunkedLineCrossingReadBuffer 一条超过 64 KiB 读取缓冲、
// 但远低于 64 MiB 上限的合法行，跨缓冲边界后仍作为一条请求处理。
func TestRunIngestChunkedLineCrossingReadBuffer(t *testing.T) {
	longName := "长" + strings.Repeat("a", 70000) + "😀"
	line := `[{"name":"` + longName + `","timestamp":1,"value":5}]`
	if len(line) <= 64*1024 {
		t.Fatalf("test line must exceed the 64 KiB read buffer, got %d bytes", len(line))
	}
	input := []byte(line + "\n" + `{"op":"query","name":"` + longName + `","start":0,"end":10}` + "\n")

	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode != 0 {
		t.Fatalf("whole-input exit code = %d, want 0", wantCode)
	}
	for _, size := range []int{1, 4096, 65535, 65536, 65537} {
		gotOut, gotCode := runIngestChunked(t, input, size)
		if gotCode != 0 || gotOut != wantOut {
			t.Fatalf("chunk size %d: (code %d, out prefix %.200q), want (code 0, out prefix %.200q)",
				size, gotCode, gotOut, wantOut)
		}
	}

	lines := strings.Split(strings.TrimRight(wantOut, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d output lines, want 2 (one write, one query)", len(lines))
	}
	w := decodeResultLine(t, lines[0])
	if w["status"] != "ok" || w["added"].(float64) != 1 {
		t.Fatalf("long line write = %v, want ok added=1 as a single request", w)
	}
	q := decodeResultLine(t, lines[1])
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %v, want the long-named series", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["name"] != longName || s0["count"].(float64) != 1 || s0["average"].(float64) != 5 {
		t.Fatalf("query series[0] name/count/average mismatch: name len %d, count %v, avg %v",
			len(s0["name"].(string)), s0["count"], s0["average"])
	}
}

// TestRunIngestChunkedWriteDuplicateQueryStats 同一序列先后写入 2 和 4，
// 再等值重提其中一点，随后查询闭区间：count 为 2、average 为 3；
// 这些统计在任何分段下都一致。
func TestRunIngestChunkedWriteDuplicateQueryStats(t *testing.T) {
	input := []byte(strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":2}]`,
		`[{"name":"cpu","timestamp":2000,"value":4}]`,
		`[{"name":"cpu","timestamp":1000,"value":2.0}]`, // 等值重复
		`{"op":"query","name":"cpu","start":1000,"end":2000}`,
	}, "\n") + "\n")

	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode != 0 {
		t.Fatalf("whole-input exit code = %d, want 0", wantCode)
	}
	for _, size := range chunkSizes {
		gotOut, gotCode := runIngestChunked(t, input, size)
		if gotCode != 0 || gotOut != wantOut {
			t.Fatalf("chunk size %d: (code %d, out %q), want (code 0, out %q)",
				size, gotCode, gotOut, wantOut)
		}
	}

	lines := strings.Split(strings.TrimRight(wantOut, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d output lines, want 4", len(lines))
	}
	for i, wantAdded := range []float64{1, 1, 0} {
		m := decodeResultLine(t, lines[i])
		if m["status"] != "ok" || m["added"].(float64) != wantAdded {
			t.Fatalf("write %d = %v, want ok added=%v", i+1, m, wantAdded)
		}
	}
	dup := decodeResultLine(t, lines[2])
	if dup["duplicates"].(float64) != 1 {
		t.Fatalf("third write = %v, want duplicates=1", dup)
	}
	q := decodeResultLine(t, lines[3])
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %v, want 1", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 2 || s0["average"].(float64) != 3 {
		t.Fatalf("query = %v, want count=2 average=3", s0)
	}
}

// TestRunIngestChunkedDelimiterSplit 换行分隔符单独送达、回车与换行
// 分开送达时，结果条数与行号约定与整体送达一致。
func TestRunIngestChunkedDelimiterSplit(t *testing.T) {
	// 行 1 "[]\r"（\r 属于行内容，是合法 JSON 空白）；行 2 "\r" 为空白行，
	// 只占行号；行 3 "[]"。
	input := []byte("[]\r\n\r\n[]\n")
	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode != 0 {
		t.Fatalf("whole-input exit code = %d, want 0", wantCode)
	}
	lines := strings.Split(strings.TrimRight(wantOut, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d output lines, want 2 (the \\r-only line is blank): %v", len(lines), lines)
	}

	// 分段档位覆盖：'\n' 独占一个片段、'\r' 与 '\n' 分属两个片段。
	for _, sizes := range [][]int{{1}, {2}, {3}, {1, 4}, {5, 1, 5}} {
		gotOut, gotCode := runIngestChunked(t, input, sizes...)
		if gotCode != 0 || gotOut != wantOut {
			t.Fatalf("chunk sizes %v: (code %d, out %q), want (code 0, out %q)",
				sizes, gotCode, gotOut, wantOut)
		}
	}
}

// TestRunIngestChunkedFinalLineWithoutNewline 输入正常结束时，最后一条
// 完整请求即使没有结尾换行也处理一次，且只处理一次。
func TestRunIngestChunkedFinalLineWithoutNewline(t *testing.T) {
	input := []byte(`[{"name":"m","timestamp":1,"value":1}]` + "\n" +
		`{"op":"query","name":"m","start":0,"end":10}`) // 末行无换行
	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode != 0 {
		t.Fatalf("whole-input exit code = %d, want 0", wantCode)
	}
	lines := strings.Split(strings.TrimRight(wantOut, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d output lines, want exactly 2 (final line processed once)", len(lines))
	}
	q := decodeResultLine(t, lines[1])
	series := q["series"].([]interface{})
	if len(series) != 1 || series[0].(map[string]interface{})["count"].(float64) != 1 {
		t.Fatalf("final query = %v, want the one committed point", q)
	}
	for _, size := range chunkSizes {
		gotOut, gotCode := runIngestChunked(t, input, size)
		if gotCode != 0 || gotOut != wantOut {
			t.Fatalf("chunk size %d: (code %d, out %q), want (code 0, out %q)",
				size, gotCode, gotOut, wantOut)
		}
	}
}

// TestRunIngestChunkedUnterminatedFinalLine 末行 JSON 未闭合时只报告该行
// 解析失败：不带 index/conflict，不返回成功写入结果；此前数据保留。
func TestRunIngestChunkedUnterminatedFinalLine(t *testing.T) {
	input := []byte(`[{"name":"m","timestamp":1,"value":1}]` + "\n" +
		`[{"name":"m","timestamp":2,"value":2}`) // 未闭合，无换行
	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode == 0 {
		t.Fatalf("whole-input exit code = 0, want non-zero for an unterminated final line")
	}
	lines := strings.Split(strings.TrimRight(wantOut, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d output lines, want 2 (ok + one parse error): %v", len(lines), lines)
	}
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" {
		t.Fatalf("line 1 = %v, want ok", m)
	}
	m := decodeResultLine(t, lines[1])
	if m["status"] != "error" || int(m["line"].(float64)) != 2 {
		t.Fatalf("final line = %v, want parse error on input line 2", m)
	}
	if _, has := m["index"]; has {
		t.Fatalf("unterminated final line must not carry index: %v", m)
	}
	if _, has := m["conflict"]; has {
		t.Fatalf("unterminated final line must not carry conflict: %v", m)
	}
	for _, size := range chunkSizes {
		gotOut, gotCode := runIngestChunked(t, input, size)
		if gotCode != wantCode || gotOut != wantOut {
			t.Fatalf("chunk size %d: (code %d, out %q), want (code %d, out %q)",
				size, gotCode, gotOut, wantCode, wantOut)
		}
	}
}

// TestRunIngestChunkedCorruptTextStillFails 分段送达不能使损坏文本被接受：
// 非法 UTF-8 字节与孤立代理项仍整行失败，此前数据保留，后续合法行继续，
// 最终非零退出。
func TestRunIngestChunkedCorruptTextStillFails(t *testing.T) {
	input := []byte(`[{"name":"cpu","timestamp":1000,"value":2}]` + "\n" +
		`[{"name":"cp` + "\xff" + `u","timestamp":2000,"value":4}]` + "\n" +
		`[{"name":"m\uD800","timestamp":1,"value":9}]` + "\n" + // 孤立高代理项转义
		`[{"name":"later","timestamp":1,"value":1}]` + "\n" +
		`{"op":"query","name":"cpu","start":0,"end":3000}` + "\n")

	wantOut, wantCode := runIngestChunked(t, input, len(input))
	if wantCode == 0 {
		t.Fatalf("whole-input exit code = 0, want non-zero because corrupt lines failed")
	}
	for _, size := range chunkSizes {
		gotOut, gotCode := runIngestChunked(t, input, size)
		if gotCode != wantCode || gotOut != wantOut {
			t.Fatalf("chunk size %d: (code %d, out %q), want (code %d, out %q)",
				size, gotCode, gotOut, wantCode, wantOut)
		}
	}

	lines := strings.Split(strings.TrimRight(wantOut, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d output lines, want 5: %v", len(lines), lines)
	}
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" {
		t.Fatalf("line 1 = %v, want ok", m)
	}
	for i, w := range []struct {
		line   int
		substr string
	}{{2, "UTF-8"}, {3, "surrogate"}} {
		m := decodeResultLine(t, lines[1+i])
		if m["status"] != "error" || int(m["line"].(float64)) != w.line {
			t.Fatalf("output %d = %v, want error on input line %d", i+1, m, w.line)
		}
		if _, has := m["index"]; has {
			t.Fatalf("corrupt line %d must not carry index: %v", w.line, m)
		}
		if _, has := m["conflict"]; has {
			t.Fatalf("corrupt line %d must not carry conflict: %v", w.line, m)
		}
		if msg, _ := m["error"].(string); !strings.Contains(msg, w.substr) {
			t.Fatalf("line %d error = %q, want substring %q", w.line, msg, w.substr)
		}
	}
	// 后续合法行继续处理。
	if m := decodeResultLine(t, lines[3]); m["status"] != "ok" || m["added"].(float64) != 1 {
		t.Fatalf("line 4 = %v, want ok added=1 after corrupt lines", m)
	}
	// 此前成功写入的数据保留：cpu 仍只有行 1 的一个点。
	q := decodeResultLine(t, lines[4])
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %v, want exactly the retained cpu point", series)
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 2 {
		t.Fatalf("query = %v, want count=1 average=2 (corrupt writes committed nothing)", s0)
	}
}
