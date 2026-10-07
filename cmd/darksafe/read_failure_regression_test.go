package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// 本文件回归保障 ingest 在“读取输入本身发生故障”时的既有行为，与业务输入
// 错误（产生 JSON 结果并继续）相区分：
//
//   - 故障出现前已经收到换行符的完整行，仍按原始顺序各处理一次、各输出一条
//     结果；尤其当读取端在同一次 Read 里既交付数据又报告故障时，这段数据中
//     完整结束的行不能被一起丢弃，其结果不能消失或重复。
//   - 故障所在的末行如果没有换行符，则不提交写入、不执行查询、不产生业务
//     JSON 结果——即使已收到的文字恰好是完整合法的写入数组或查询对象。
//     同一段故障数据里多条完整行加下一行的一部分时，只有完整行有结果。
//     该处理范围在跨过 64 KiB 读缓冲边界时保持一致，片段边界不是行边界。
//   - 读取故障最终返回退出码 1；标准错误只输出一次以 "ingest: read error: "
//     开头、包含实际故障原因的诊断；标准输出保留此前完整行的结果，不追加
//     带 status、line 或 conflict 的读取错误记录。输入一开始就失败时标准
//     输出为空。
//   - 与正常输入结束的区别：没有最后一个换行符的合法末行在正常结束时照常
//     处理一次；相同文字若因读取故障结束则不处理。

// errSimulatedReadFailure 是测试中模拟的底层读取故障，文本须有辨识度，
// 以便断言它只出现在标准错误、而不混入标准输出的任何记录。
var errSimulatedReadFailure = errors.New("simulated read failure: underlying device vanished")

// readStep 描述脚本化读取的一步：data 是这一步交付的字节；err 非空时表示
// 底层故障，随这段数据的最后一批字节在同一次 Read 中一起报告（data 为空时
// 则单独报告故障）。err 为 nil 表示正常交付。
type readStep struct {
	data string
	err  error
}

// scriptedReader 按 steps 逐段交付输入：每段数据可能被 64 KiB 读缓冲拆成
// 多次 Read 返回；当某一步的字节全部送出且该步带故障时，故障与最后一批
// 字节在同一次 Read 返回（n>0, err!=nil），模拟真实读取端“数据与故障同时
// 到达”的情形。所有步骤用完后返回 io.EOF。
type scriptedReader struct {
	steps []readStep
	idx   int
	off   int // 当前步骤已送出的字节数
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	for r.idx < len(r.steps) && r.off >= len(r.steps[r.idx].data) {
		if err := r.steps[r.idx].err; err != nil {
			r.idx++
			r.off = 0
			return 0, err
		}
		r.idx++
		r.off = 0
	}
	if r.idx >= len(r.steps) {
		return 0, io.EOF
	}
	s := r.steps[r.idx]
	n := copy(p, s.data[r.off:])
	r.off += n
	if r.off >= len(s.data) && s.err != nil {
		err := s.err
		r.idx++
		r.off = 0
		return n, err
	}
	return n, nil
}

// runIngestCaptureStderr 执行一次 ingest，同时捕获进程级标准错误
// （runIngest 的读取故障诊断直接写 os.Stderr）。本包测试不并行运行，
// 临时替换 os.Stderr 是安全的。
func runIngestCaptureStderr(t *testing.T, in io.Reader) (code int, stdout, stderr string) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	var out bytes.Buffer
	code = runIngest(in, &out)

	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("close stderr pipe: %v", err)
	}
	errText, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	r.Close()
	return code, out.String(), string(errText)
}

// assertReadFailureDiagnostic 断言标准错误恰好包含一行诊断：以
// "ingest: read error: " 开头，并包含底层故障的实际原因。
func assertReadFailureDiagnostic(t *testing.T, stderr string, cause error) {
	t.Helper()
	if !strings.HasPrefix(stderr, "ingest: read error: ") {
		t.Fatalf("stderr = %q, want a single diagnostic starting with %q",
			stderr, "ingest: read error: ")
	}
	if !strings.Contains(stderr, cause.Error()) {
		t.Fatalf("stderr = %q, want it to name the underlying cause %q", stderr, cause.Error())
	}
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("stderr = %q, want exactly one diagnostic line, no repeats", stderr)
	}
}

// stdoutResultLines 把标准输出拆成逐条结果行；空输出返回 nil，
// 每条结果都必须以换行结束。
func stdoutResultLines(t *testing.T, stdout string) []string {
	t.Helper()
	if stdout == "" {
		return nil
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout = %q, every result line must end with a newline", stdout)
	}
	return strings.Split(strings.TrimRight(stdout, "\n"), "\n")
}

// assertNoReadFailureRecord 断言标准输出没有追加任何读取故障记录：
// 既不出现诊断文本，也不出现底层故障原因。
func assertNoReadFailureRecord(t *testing.T, stdout string) {
	t.Helper()
	if strings.Contains(stdout, "read error") || strings.Contains(stdout, errSimulatedReadFailure.Error()) {
		t.Fatalf("stdout = %q, must not carry any read-failure record", stdout)
	}
}

// TestRunIngestReadFailureKeepsCompletedLines 是核心场景：先写入 cpu 在
// 时间戳 1000 的值 2，再查询包含该时间戳的区间。两行完整输入之后（或同一段
// 故障数据之中）读取失败：两条结果必须各出现一次、按原顺序保留——查询仍返回
// 一个点、均值 2——退出码为 1，标准错误只有一条读取故障诊断。
func TestRunIngestReadFailureKeepsCompletedLines(t *testing.T) {
	write := `[{"name":"cpu","timestamp":1000,"value":2}]`
	query := `{"op":"query","name":"cpu","start":1000,"end":1000}`

	cases := []struct {
		name  string
		steps []readStep
	}{
		// 数据与故障在同一次 Read 中一起交付：完整结束的行不能被一起丢弃。
		{"data-and-error-in-one-read", []readStep{
			{data: write + "\n" + query + "\n", err: errSimulatedReadFailure},
		}},
		// 数据先全部送达，故障在随后一次 Read 单独报告。
		{"error-in-later-read", []readStep{
			{data: write + "\n" + query + "\n"},
			{err: errSimulatedReadFailure},
		}},
		// 故障随第二段数据一起到达：第一段完整行同样保留。
		{"error-arrives-with-second-chunk", []readStep{
			{data: write + "\n"},
			{data: query + "\n", err: errSimulatedReadFailure},
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runIngestCaptureStderr(t, &scriptedReader{steps: tc.steps})
			if code != 1 {
				t.Fatalf("exit code = %d, want exactly 1 on read failure", code)
			}
			assertReadFailureDiagnostic(t, stderr, errSimulatedReadFailure)

			lines := stdoutResultLines(t, stdout)
			if len(lines) != 2 {
				t.Fatalf("stdout = %q, want exactly 2 results (write ok, query ok), "+
					"each completed line exactly once", stdout)
			}
			w := decodeResultLine(t, lines[0])
			if w["status"] != "ok" || w["added"].(float64) != 1 {
				t.Fatalf("write result = %v, want ok added=1", w)
			}
			q := decodeResultLine(t, lines[1])
			if q["status"] != "ok" || q["op"] != "query" {
				t.Fatalf("query result = %v, want ok query", q)
			}
			series := q["series"].([]interface{})
			if len(series) != 1 {
				t.Fatalf("query series = %v, want the one committed point", series)
			}
			s0 := series[0].(map[string]interface{})
			if s0["count"].(float64) != 1 || s0["average"].(float64) != 2 {
				t.Fatalf("query series[0] = %v, want count=1 average=2", s0)
			}
			assertNoReadFailureRecord(t, stdout)
		})
	}
}

// TestRunIngestReadFailureDropsUnterminatedTail 固定末行处理范围：故障所在
// 的末行没有换行符，就不提交、不执行、不产生业务 JSON 结果——即使已收到的
// 文字恰好是完整合法的写入数组或查询对象。同一段故障数据里只有完整结束的
// 行才有结果，未结束的部分不被拼成额外请求。
func TestRunIngestReadFailureDropsUnterminatedTail(t *testing.T) {
	committed := `[{"name":"cpu","timestamp":1000,"value":2}]`
	validWriteTail := `[{"name":"cpu","timestamp":2000,"value":9}]`
	validQueryTail := `{"op":"query","name":"cpu","start":0,"end":3000}`

	cases := []struct {
		name        string
		steps       []readStep
		wantResults int
	}{
		// 故障段带来一条完整行和一段未结束文字；未结束文字本身是一个完整
		// 合法的写入数组，也不得提交。
		{"valid-write-array-tail", []readStep{
			{data: committed + "\n" + validWriteTail, err: errSimulatedReadFailure},
		}, 1},
		// 未结束文字是一个完整合法的查询对象：不得执行，也不产生结果。
		{"valid-query-object-tail", []readStep{
			{data: committed + "\n" + validQueryTail, err: errSimulatedReadFailure},
		}, 1},
		// 同一段故障数据含多条完整行加下一行的一部分：只有前面完整行有结果。
		{"complete-lines-plus-partial", []readStep{
			{data: committed + "\n" + validWriteTail + "\n" + validQueryTail[:20],
				err: errSimulatedReadFailure},
		}, 2},
		// 故障在末行文字送达后的下一次 Read 才报告：未结束文字同样被丢弃。
		{"error-after-tail-bytes", []readStep{
			{data: committed + "\n" + validWriteTail},
			{err: errSimulatedReadFailure},
		}, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runIngestCaptureStderr(t, &scriptedReader{steps: tc.steps})
			if code != 1 {
				t.Fatalf("exit code = %d, want exactly 1 on read failure", code)
			}
			assertReadFailureDiagnostic(t, stderr, errSimulatedReadFailure)

			lines := stdoutResultLines(t, stdout)
			if len(lines) != tc.wantResults {
				t.Fatalf("stdout = %q, want exactly %d result(s) for the completed "+
					"line(s) only; the unterminated tail must produce none", stdout, tc.wantResults)
			}
			first := decodeResultLine(t, lines[0])
			if first["status"] != "ok" || first["added"].(float64) != 1 {
				t.Fatalf("first result = %v, want ok added=1 for the committed line", first)
			}
			if tc.wantResults == 2 {
				// 第二条完整行（带换行的写入）正常提交；其后的查询片段未执行。
				second := decodeResultLine(t, lines[1])
				if second["status"] != "ok" || second["added"].(float64) != 1 {
					t.Fatalf("second result = %v, want ok added=1 for the completed write", second)
				}
			}
			assertNoReadFailureRecord(t, stdout)
		})
	}
}

// TestRunIngestReadFailureAtStartProducesEmptyStdout 输入一开始就失败时，
// 标准输出为空，退出码为 1，标准错误只有一条诊断。
func TestRunIngestReadFailureAtStartProducesEmptyStdout(t *testing.T) {
	code, stdout, stderr := runIngestCaptureStderr(t, &scriptedReader{steps: []readStep{
		{err: errSimulatedReadFailure},
	}})
	if code != 1 {
		t.Fatalf("exit code = %d, want exactly 1 on read failure", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty output when the very first read fails", stdout)
	}
	assertReadFailureDiagnostic(t, stderr, errSimulatedReadFailure)
}

// TestRunIngestReadFailureAcrossReadBufferBoundary 输入跨过 64 KiB 读缓冲
// 边界时，完整行与未结束末行的处理范围保持一致：带换行结束的长行（内容跨
// 多个读缓冲）在故障前完整处理一次；同样的长行若没有换行结束，即使文字
// 本身完整合法也整体丢弃，片段边界不会被当作行边界。
func TestRunIngestReadFailureAcrossReadBufferBoundary(t *testing.T) {
	longVal := strings.Repeat("y", 100*1024) // 行内容约 100 KiB，跨过多个 64 KiB 读缓冲
	bigLine := `[{"name":"big","timestamp":1,"value":1,"labels":{"pad":"` + longVal + `"}}]`
	if len(bigLine) <= 64*1024 {
		t.Fatalf("test setup: line only %d bytes, want > 64 KiB", len(bigLine))
	}
	if len(bigLine) >= maxIngestLineBytes {
		t.Fatalf("test setup: line %d bytes must stay under the 64 MiB limit", len(bigLine))
	}
	cut := 70000 // 落在 64 KiB 读缓冲边界之后

	t.Run("terminated-big-line-survives", func(t *testing.T) {
		code, stdout, stderr := runIngestCaptureStderr(t, &scriptedReader{steps: []readStep{
			{data: bigLine[:cut]},
			{data: bigLine[cut:] + "\n", err: errSimulatedReadFailure},
		}})
		if code != 1 {
			t.Fatalf("exit code = %d, want exactly 1 on read failure", code)
		}
		assertReadFailureDiagnostic(t, stderr, errSimulatedReadFailure)
		lines := stdoutResultLines(t, stdout)
		if len(lines) != 1 {
			t.Fatalf("stdout = %q, want exactly 1 result: the newline-terminated big "+
				"line processed once, buffer boundaries must not split it", stdout)
		}
		m := decodeResultLine(t, lines[0])
		if m["status"] != "ok" || m["added"].(float64) != 1 {
			t.Fatalf("big-line result = %v, want ok added=1", m)
		}
	})

	t.Run("unterminated-big-line-dropped", func(t *testing.T) {
		code, stdout, stderr := runIngestCaptureStderr(t, &scriptedReader{steps: []readStep{
			{data: bigLine[:cut]},
			{data: bigLine[cut:], err: errSimulatedReadFailure},
		}})
		if code != 1 {
			t.Fatalf("exit code = %d, want exactly 1 on read failure", code)
		}
		assertReadFailureDiagnostic(t, stderr, errSimulatedReadFailure)
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty: the big line never saw a newline before "+
				"the failure, so it must not be committed even though its text is valid", stdout)
		}
	})
}

// TestRunIngestReadFailurePreservesEarlierBusinessResults 故障前完整行的
// 业务结果（包括业务错误及其原始行号、index、conflict）原样保留，不会被
// 读取故障的诊断取代，也不会在标准输出追加任何读取错误记录。
func TestRunIngestReadFailurePreservesEarlierBusinessResults(t *testing.T) {
	write := `[{"name":"cpu","timestamp":1000,"value":2}]`    // 行 1：成功
	bad := `not json`                                         // 行 2：整行解析失败
	conflict := `[{"name":"cpu","timestamp":1000,"value":4}]` // 行 3：与行 1 冲突
	tail := `[{"name":"cpu","timestamp":2000,"value":9}`      // 行 4：未闭合且无换行

	code, stdout, stderr := runIngestCaptureStderr(t, &scriptedReader{steps: []readStep{
		{data: write + "\n" + bad + "\n" + conflict + "\n" + tail, err: errSimulatedReadFailure},
	}})
	if code != 1 {
		t.Fatalf("exit code = %d, want exactly 1 on read failure", code)
	}
	assertReadFailureDiagnostic(t, stderr, errSimulatedReadFailure)

	lines := stdoutResultLines(t, stdout)
	if len(lines) != 3 {
		t.Fatalf("stdout = %q, want exactly 3 results (ok, parse error, conflict); "+
			"the unterminated tail and the read failure itself add none", stdout)
	}

	ok := decodeResultLine(t, lines[0])
	if ok["status"] != "ok" || ok["added"].(float64) != 1 {
		t.Fatalf("result 1 = %v, want ok added=1", ok)
	}

	parseErr := decodeResultLine(t, lines[1])
	if parseErr["status"] != "error" || int(parseErr["line"].(float64)) != 2 {
		t.Fatalf("result 2 = %v, want the original parse error on input line 2", parseErr)
	}
	if _, has := parseErr["index"]; has {
		t.Fatalf("whole-line parse error must not carry index: %v", parseErr)
	}
	if _, has := parseErr["conflict"]; has {
		t.Fatalf("whole-line parse error must not carry conflict: %v", parseErr)
	}

	conflictErr := decodeResultLine(t, lines[2])
	if conflictErr["status"] != "error" || int(conflictErr["line"].(float64)) != 3 ||
		int(conflictErr["index"].(float64)) != 1 {
		t.Fatalf("result 3 = %v, want the original conflict on input line 3, index 1", conflictErr)
	}
	c := conflictErr["conflict"].(map[string]interface{})
	if c["existing"].(float64) != 2 || c["submitted"].(float64) != 4 {
		t.Fatalf("conflict values = %v, want existing=2 submitted=4", c)
	}

	assertNoReadFailureRecord(t, stdout)
}

// TestRunIngestUnterminatedFinalLineNormalEndVsReadFailure 固定正常结束与
// 读取故障的区别：没有最后一个换行符的合法末行，在正常结束时照常处理一次；
// 相同文字若因读取故障结束则不处理、不产生任何结果。
func TestRunIngestUnterminatedFinalLineNormalEndVsReadFailure(t *testing.T) {
	text := `[]` // 合法空批，无结尾换行

	t.Run("normal-eof-processes-final-line", func(t *testing.T) {
		code, stdout, stderr := runIngestCaptureStderr(t, strings.NewReader(text))
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 for a clean input ending", code)
		}
		if stderr != "" {
			t.Fatalf("stderr = %q, want empty on a clean run", stderr)
		}
		lines := stdoutResultLines(t, stdout)
		if len(lines) != 1 {
			t.Fatalf("stdout = %q, want exactly 1 result for the unterminated final line", stdout)
		}
		if m := decodeResultLine(t, lines[0]); m["status"] != "ok" {
			t.Fatalf("final-line result = %v, want ok", m)
		}
	})

	t.Run("read-failure-drops-same-text", func(t *testing.T) {
		code, stdout, stderr := runIngestCaptureStderr(t, &scriptedReader{steps: []readStep{
			{data: text, err: errSimulatedReadFailure},
		}})
		if code != 1 {
			t.Fatalf("exit code = %d, want exactly 1 on read failure", code)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty: the same text ends by read failure, "+
				"so the unterminated line is not processed", stdout)
		}
		assertReadFailureDiagnostic(t, stderr, errSimulatedReadFailure)
	})
}
