package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
)

// 本文件在命令行端到端（stdin → runIngest → JSON 结果/退出码/标准错误）层面回归保障
// ingest 在“读取输入本身发生故障”时的既有行为。业务输入错误（非法 JSON、冲突等）
// 仍按行产出 JSON 结果并继续处理后续行；只有底层读取返回非 EOF 故障才终止命令。
// 固定的既有约定：
//   - 故障出现前已经收到换行符的完整输入行，仍按原始顺序各处理一次：成功的写入/查询
//     结果与失败行的业务错误（含原始行号、index、conflict）都原样保留，不消失、不重复，
//     也不会被改写成读取故障诊断；
//   - 读取端在同一次 Read 中交付一段数据并同时报告故障时，该段中已经以换行结束的行
//     不能被一起丢弃；故障所在的末行若还没有换行符，则既不提交写入、也不执行查询，
//     也不为其已收到的文字（即使文字本身恰好是完整合法的写入数组或查询对象）产出任何
//     业务 JSON 结果；
//   - 读取片段边界（包括 64 KiB 读缓冲边界）永远不是输入行边界，跨边界时处理范围一致；
//   - 读取故障最终退出码为 1，标准错误恰好输出一行以 "ingest: read error: " 开头、
//     包含实际失败原因的纯文本诊断；标准输出只保留此前完整行的结果，不追加任何带
//     status/line/index/conflict 的读取错误记录；输入一开始就失败时标准输出为空；
//   - 与正常结束相区别：没有末尾换行的合法末行在正常 EOF 时照常处理一次，同样的字节
//     若以读取故障结束则完全不处理。
//
// 故障全部离线注入（io.Reader 返回固定错误），不依赖真实管道关闭或磁盘错误。

// errSimulatedReadFailure 是离线注入的底层读取故障原因，必须原样出现在
// "ingest: read error: ..." 诊断中。
var errSimulatedReadFailure = errors.New("simulated ingest input read failure")

// failingSegmentedReader 先按 cuts 指定的绝对字节边界把 data 分段送达，并在“最后一段
// 数据交付的同一次 Read 调用”返回故障 err，精确模拟读取端“交付一段数据的同时报告
// 故障”。最后一段之后的 Read 只返回同一故障、不再给出任何字节。cuts 的语义与
// segmentedReader 相同（落在 (0, len(data)) 内、排序去重）；cuts 为空时整份 data
// 在最后一次物理 Read 中与故障一起交付（bufio 的 64 KiB 缓冲可能把它拆成多次填充，
// 故障随最后一次填充到达）。
type failingSegmentedReader struct {
	data []byte
	pos  int
	cuts []int
	idx  int
	err  error
}

func newFailingSegmentedReader(data []byte, err error, cuts ...int) *failingSegmentedReader {
	kept := cuts[:0:0]
	for _, c := range cuts {
		if c > 0 && c < len(data) {
			kept = append(kept, c)
		}
	}
	sort.Ints(kept)
	out := kept[:0:0]
	for i, c := range kept {
		if i == 0 || c != out[len(out)-1] {
			out = append(out, c)
		}
	}
	return &failingSegmentedReader{data: data, cuts: out, err: err}
}

func (r *failingSegmentedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= len(r.data) {
		return 0, r.err
	}
	for r.idx < len(r.cuts) && r.cuts[r.idx] <= r.pos {
		r.idx++
	}
	end := len(r.data)
	if r.idx < len(r.cuts) {
		end = r.cuts[r.idx]
		r.idx++
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	if r.pos < end {
		// p 小于当前段（整份数据一次交付且超过 64 KiB 读缓冲时会发生）：
		// 保留边界位置，下次继续从同一段交付，边界不丢失。
		if r.idx < len(r.cuts) {
			r.idx--
		}
		return n, nil
	}
	// 最后一段（含末行片段）与故障在同一次 Read 中交付。
	if r.pos >= len(r.data) {
		return n, r.err
	}
	return n, nil
}

// runIngestCaptureStderr 执行一次 ingest，返回退出码以及标准输出、标准错误的原始字节。
// runIngest 同步执行，返回前所有 stderr 写入都已完成。
func runIngestCaptureStderr(t *testing.T, in io.Reader) (int, string, string) {
	t.Helper()
	var out bytes.Buffer
	code, stderr := captureStderr(t, func() int { return runIngest(in, &out) })
	return code, out.String(), stderr
}

// decodeStdoutResults 把标准输出按行解码为 JSON 结果；每条记录必须以 '\n' 结尾，
// 空输出对应零条记录——因此末行片段若泄漏成半截输出，会在此处直接失败。
func decodeStdoutResults(t *testing.T, stdout string) []map[string]interface{} {
	t.Helper()
	if stdout == "" {
		return nil
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout must consist of newline-terminated records, got %q", stdout)
	}
	var res []map[string]interface{}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		res = append(res, decodeResultLine(t, line))
	}
	return res
}

// assertReadFailureDiagnostic 固定读取故障的标准错误形态：恰好一行（以 '\n' 结尾），
// 逐字等于 "ingest: read error: <实际原因>"，且不是带 status/line/index/conflict
// 的业务 JSON 记录。
func assertReadFailureDiagnostic(t *testing.T, stderr string) {
	t.Helper()
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("stderr = %q, want exactly one newline-terminated diagnostic line", stderr)
	}
	diagnostic := strings.TrimSuffix(stderr, "\n")
	want := "ingest: read error: " + errSimulatedReadFailure.Error()
	if diagnostic != want {
		t.Fatalf("stderr = %q, want exactly %q", stderr, want)
	}
	for _, banned := range []string{`"status"`, `"line"`, `"index"`, `"conflict"`} {
		if strings.Contains(diagnostic, banned) {
			t.Fatalf("read diagnostic must not be a business JSON record, contains %s: %q",
				banned, diagnostic)
		}
	}
}

// referenceStdout 以正常 EOF 结束跑一份输入，返回退出码与标准输出字节，
// 作为“此前完整行应有的结果”的逐字节基准。
func referenceStdout(t *testing.T, data []byte) (int, []byte) {
	t.Helper()
	var ref bytes.Buffer
	code := runIngest(bytes.NewReader(data), &ref)
	return code, ref.Bytes()
}

// TestRunIngestReadFailureKeepsCompletedLines 是任务描述中的核心场景：先写入 cpu 在
// 时间戳 1000 的值 2，再查询包含该时间戳的区间；读取端在同一次 Read 交付这些字节的
// 同时报告故障，且故障末行（无换行）本身是一段完整合法的写入数组。两条完整行的结果
// 不得消失或重复（查询仍返回一个点、均值 2），末行片段不得产生结果，退出码为 1，
// 标准输出逐字节等于这些完整行在正常结束时的输出。
func TestRunIngestReadFailureKeepsCompletedLines(t *testing.T) {
	writeLine := `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`
	queryLine := `{"op":"query","name":"cpu","start":1000,"end":1000,"labels":{"host":"a"}}`
	completed := writeLine + "\n" + queryLine + "\n"
	// 故障所在末行：没有换行；文字本身是完整合法的写入数组，也绝不处理。
	unfinished := `[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`
	data := []byte(completed + unfinished)

	refCode, refBytes := referenceStdout(t, []byte(completed))
	if refCode != 0 {
		t.Fatalf("reference exit code = %d, want 0", refCode)
	}

	in := &failAfterReader{data: data, err: errSimulatedReadFailure}
	code, stdout, stderr := runIngestCaptureStderr(t, in)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 on a real read failure", code)
	}
	assertReadFailureDiagnostic(t, stderr)

	// 完整行的正常结果逐字节保留，不被读取故障丢弃或追加任何记录。
	if stdout != string(refBytes) {
		t.Fatalf("stdout = %q\nwant %q (completed lines must survive byte-for-byte)",
			stdout, string(refBytes))
	}

	res := decodeStdoutResults(t, stdout)
	if len(res) != 2 {
		t.Fatalf("%d results, want exactly 2 (one write, one query): %s",
			len(res), mustJSON(res))
	}
	if res[0]["status"] != "ok" || res[0]["added"].(float64) != 1 {
		t.Fatalf("write result = %s, want one added point exactly once", mustJSON(res[0]))
	}
	q := res[1]
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("query result = %s, want an ok query", mustJSON(q))
	}
	series := q["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("query series = %s, want exactly one point", mustJSON(q))
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 1 || s0["average"].(float64) != 2 {
		t.Fatalf("query = %s, want count=1 average=2 despite the following read failure",
			mustJSON(s0))
	}
}

// TestRunIngestReadFailureKeepsBusinessErrorRecords 确认故障前完整行上的业务错误同样
// 保留：解析失败（无 index/conflict）与写入冲突（带 index 与 conflict）的 JSON 结果
// 和原始行号都不得被读取故障诊断替代；末行合法查询片段不执行、不产出记录。
func TestRunIngestReadFailureKeepsBusinessErrorRecords(t *testing.T) {
	good := `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`
	unfinishedQuery := `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`

	cases := []struct {
		name    string
		badLine string
		check   func(t *testing.T, m map[string]interface{})
	}{
		{
			name:    "parse-error",
			badLine: `not json`,
			check: func(t *testing.T, m map[string]interface{}) {
				if m["status"] != "error" || int(m["line"].(float64)) != 2 {
					t.Fatalf("business error = %s, want parse error on line 2", mustJSON(m))
				}
				if _, has := m["index"]; has {
					t.Fatalf("parse error must not carry index: %s", mustJSON(m))
				}
				if _, has := m["conflict"]; has {
					t.Fatalf("parse error must not carry conflict: %s", mustJSON(m))
				}
				if !strings.Contains(m["error"].(string), "invalid JSON") {
					t.Fatalf("error = %q, want the business invalid-JSON reason preserved",
						m["error"])
				}
			},
		},
		{
			name:    "conflict",
			badLine: `[{"name":"cpu","timestamp":1000,"value":4,"labels":{"host":"a"}}]`,
			check: func(t *testing.T, m map[string]interface{}) {
				if m["status"] != "error" || int(m["line"].(float64)) != 2 ||
					int(m["index"].(float64)) != 1 {
					t.Fatalf("business error = %s, want conflict on line 2 index 1", mustJSON(m))
				}
				c := m["conflict"].(map[string]interface{})
				if c["existing"].(float64) != 2 || c["submitted"].(float64) != 4 {
					t.Fatalf("conflict = %s, want existing=2 submitted=4 preserved", mustJSON(c))
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			completed := strings.Join([]string{good, tc.badLine, `[]`}, "\n") + "\n"
			data := []byte(completed + unfinishedQuery)

			refCode, refBytes := referenceStdout(t, []byte(completed))
			if refCode != 1 {
				t.Fatalf("reference exit code = %d, want 1 because of the business error",
					refCode)
			}

			in := &failAfterReader{data: data, err: errSimulatedReadFailure}
			code, stdout, stderr := runIngestCaptureStderr(t, in)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			// 业务错误结果只随完整行留在 stdout；stderr 上只有读取诊断，
			// stdout 与“仅有完整行、正常结束”的基准逐字节一致（不增、不减、不改写）。
			if stdout != string(refBytes) {
				t.Fatalf("stdout = %q\nwant %q", stdout, string(refBytes))
			}
			assertReadFailureDiagnostic(t, stderr)
			if strings.Contains(stderr, "invalid JSON") {
				t.Fatalf("business reason must stay on stdout, stderr = %q", stderr)
			}

			res := decodeStdoutResults(t, stdout)
			if len(res) != 3 {
				t.Fatalf("%d results, want 3 (write, business error, empty batch); "+
					"the unfinished query must not run: %s", len(res), mustJSON(res))
			}
			if res[0]["status"] != "ok" || res[0]["added"].(float64) != 1 {
				t.Fatalf("first result = %s, want the committed write", mustJSON(res[0]))
			}
			tc.check(t, res[1])
			if res[2]["status"] != "ok" {
				t.Fatalf("third result = %s, want the later completed line still processed",
					mustJSON(res[2]))
			}
		})
	}
}

// TestRunIngestReadFailureSameDeliveryAcrossFragmentBoundaries 穷举片段切分位置：
// 多条完整行与下一行的一部分可能在同一次随故障交付的 Read 中，也可能分散在多次 Read
// 中；无论切分点落在何处（行内、换行符前后、未结束片段内部），输出都只能包含此前
// 完整行的结果，且与正常结束基准逐字节相同。空白行只占行号，业务错误保留真实行号。
func TestRunIngestReadFailureSameDeliveryAcrossFragmentBoundaries(t *testing.T) {
	writeLine := `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`
	badLine := `not json`
	queryLine := `{"op":"query","name":"cpu","start":1000,"end":1000,"labels":{"host":"a"}}`
	// 行 1 写入、行 2 空白、行 3 业务解析失败、行 4 查询；随后是未结束的行 5 片段。
	completed := strings.Join([]string{writeLine, "", badLine, queryLine}, "\n") + "\n"
	unfinished := `[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`
	data := []byte(completed + unfinished)

	_, refBytes := referenceStdout(t, []byte(completed))
	wantRes := decodeStdoutResults(t, string(refBytes))
	if len(wantRes) != 3 {
		t.Fatalf("reference = %d results, want 3 (blank line counts only as a line number)",
			len(wantRes))
	}
	if wantRes[1]["status"] != "error" || int(wantRes[1]["line"].(float64)) != 3 {
		t.Fatalf("reference business error = %s, want line 3", mustJSON(wantRes[1]))
	}
	q := wantRes[2]["series"].([]interface{})
	if len(q) != 1 || q[0].(map[string]interface{})["average"].(float64) != 2 {
		t.Fatalf("reference query = %s, want the one committed point average 2",
			mustJSON(wantRes[2]))
	}

	runAndCompare := func(t *testing.T, label string, in io.Reader) {
		t.Helper()
		code, stdout, stderr := runIngestCaptureStderr(t, in)
		if code != 1 {
			t.Fatalf("%s: exit code = %d, want 1", label, code)
		}
		if stdout != string(refBytes) {
			t.Fatalf("%s: stdout = %q\nwant %q (fragment boundaries are not line boundaries)",
				label, stdout, string(refBytes))
		}
		assertReadFailureDiagnostic(t, stderr)
	}

	// 没有切分：整份数据（两条以上完整行 + 未结束片段）在随故障交付的同一次 Read 中。
	t.Run("all-bytes-with-faulty-read", func(t *testing.T) {
		runAndCompare(t, "all bytes in one faulty read",
			newFailingSegmentedReader(data, errSimulatedReadFailure))
	})
	// 故障段恰好包含两条完整行（行 3、行 4）加未结束片段。
	t.Run("faulty-read-holds-two-lines-and-partial", func(t *testing.T) {
		runAndCompare(t, "two complete lines plus partial in the faulty read",
			newFailingSegmentedReader(data, errSimulatedReadFailure, len(writeLine)+1))
	})
	// 逐字节穷举每个切分点。
	for c := 1; c < len(data); c++ {
		c := c
		t.Run(fmt.Sprintf("cut@%d", c), func(t *testing.T) {
			runAndCompare(t, fmt.Sprintf("cut at byte %d", c),
				newFailingSegmentedReader(data, errSimulatedReadFailure, c))
		})
	}
	// 所有位置同时切开（逐字节送达，故障随最后一个字节到达）。
	allCuts := make([]int, 0, len(data)-1)
	for c := 1; c < len(data); c++ {
		allCuts = append(allCuts, c)
	}
	t.Run("1-byte", func(t *testing.T) {
		runAndCompare(t, "one byte at a time",
			newFailingSegmentedReader(data, errSimulatedReadFailure, allCuts...))
	})
	// 若干多点切分：换行处、完整部分与片段交界处、片段内部。
	t.Run("multi-cut", func(t *testing.T) {
		runAndCompare(t, "multiple cuts",
			newFailingSegmentedReader(data, errSimulatedReadFailure,
				1, len(writeLine)+1, len(completed)-1, len(completed), len(completed)+1))
	})
}

// TestRunIngestReadFailureAcrossReadBufferBoundary 固定跨越 64 KiB 读缓冲边界时的处理
// 范围：两条约 40 KiB 的完整写入行与一条查询行（总长超过 64 KiB，行首行尾落在缓冲
// 边界两侧）必须各处理一次，查询看到两个点、均值 3；随后未结束的合法写入数组片段
// （本身是完整 JSON）不得被缓冲边界拼成额外请求。输出与正常结束基准逐字节相同。
func TestRunIngestReadFailureAcrossReadBufferBoundary(t *testing.T) {
	pad := strings.Repeat("x", 40000)
	big1 := `[{"name":"big","timestamp":0,"value":2,"labels":{"pad":"` + pad + `"}}]`
	big2 := `[{"name":"big","timestamp":1000,"value":4,"labels":{"pad":"` + pad + `"}}]`
	queryLine := `{"op":"query","name":"big","start":0,"end":1000}`
	completed := big1 + "\n" + big2 + "\n" + queryLine + "\n"
	// 约 30 KiB、文字本身是完整合法写入数组的未结束片段。
	unfinished := `[{"name":"big","timestamp":2000,"value":9,"labels":{"pad":"` +
		strings.Repeat("y", 30000) + `"}}]`
	data := []byte(completed + unfinished)

	if len(data) <= 64*1024 {
		t.Fatalf("test setup: input only %d bytes, want it to span the 64 KiB read buffer",
			len(data))
	}

	_, refBytes := referenceStdout(t, []byte(completed))
	wantRes := decodeStdoutResults(t, string(refBytes))
	if len(wantRes) != 3 {
		t.Fatalf("reference = %d results, want 3 (two big writes + one query)", len(wantRes))
	}
	if wantRes[0]["added"].(float64) != 1 || wantRes[1]["added"].(float64) != 1 {
		t.Fatalf("reference writes = %s, %s", mustJSON(wantRes[0]), mustJSON(wantRes[1]))
	}
	series := wantRes[2]["series"].([]interface{})
	if len(series) != 1 {
		t.Fatalf("reference query = %s, want one series", mustJSON(wantRes[2]))
	}
	s0 := series[0].(map[string]interface{})
	if s0["count"].(float64) != 2 || s0["average"].(float64) != 3 {
		t.Fatalf("reference query = %s, want count=2 average=3", mustJSON(s0))
	}

	// 切分点：对齐 64 KiB、错开 1 字节、64 KiB 前后一个字节，以及第二行行首附近。
	var cutSet [][]int
	for k := 64 * 1024; k < len(data); k += 64 * 1024 {
		cutSet = append(cutSet, []int{k})
	}
	for k := 1; k < len(data); k += 64 * 1024 {
		cutSet = append(cutSet, []int{k})
	}
	for _, k := range []int{64*1024 - 1, 64 * 1024, 64*1024 + 1, len(big1) + 1} {
		if k > 0 && k < len(data) {
			cutSet = append(cutSet, []int{k})
		}
	}
	for i, cuts := range cutSet {
		cuts := cuts
		t.Run(fmt.Sprintf("cuts%v@%d", cuts, i), func(t *testing.T) {
			in := newFailingSegmentedReader(data, errSimulatedReadFailure, cuts...)
			code, stdout, stderr := runIngestCaptureStderr(t, in)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if stdout != string(refBytes) {
				t.Fatalf("stdout differs from the completed-lines reference:\n got %d bytes\n"+
					"want %d bytes (read-buffer edges must not become line edges)",
					len(stdout), len(refBytes))
			}
			assertReadFailureDiagnostic(t, stderr)
		})
	}

	// 整份输入随故障一起开始送达（无切分）：故障在最后一次缓冲填充时到达，
	// 结论必须与逐段送达一致。
	t.Run("whole-input-faulty", func(t *testing.T) {
		in := newFailingSegmentedReader(data, errSimulatedReadFailure)
		code, stdout, stderr := runIngestCaptureStderr(t, in)
		if code != 1 || stdout != string(refBytes) {
			t.Fatalf("code=%d stdout=%d bytes, want code 1 and %d reference bytes",
				code, len(stdout), len(refBytes))
		}
		assertReadFailureDiagnostic(t, stderr)
	})
}

// TestRunIngestReadFailureAfterFinalNewlineKeepsEveryResult 故障发生在最后一个换行符
// 之后（没有未结束片段）：所有行都已完整，结果一条不丢；与正常 EOF 的区别只在退出码
// 与标准错误诊断。
func TestRunIngestReadFailureAfterFinalNewlineKeepsEveryResult(t *testing.T) {
	data := []byte("[]\n" + `{"op":"query","name":"m","start":0,"end":1}` + "\n")
	refCode, refBytes := referenceStdout(t, data)
	if refCode != 0 {
		t.Fatalf("reference exit code = %d, want 0", refCode)
	}

	in := &failAfterReader{data: data, err: errSimulatedReadFailure}
	code, stdout, stderr := runIngestCaptureStderr(t, in)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 even though every line completed", code)
	}
	if stdout != string(refBytes) {
		t.Fatalf("stdout = %q, want all completed results %q", stdout, string(refBytes))
	}
	assertReadFailureDiagnostic(t, stderr)
}

// TestRunIngestReadFailureAtStartProducesNoOutput 输入一开始就失败，或只能交付一个
// 从未结束的首行片段（即使片段本身是合法 JSON，或是“空白完整行 + 未结束片段”）：
// 标准输出必须为空，退出码 1，标准错误只有一行读取诊断。
func TestRunIngestReadFailureAtStartProducesNoOutput(t *testing.T) {
	validWriteFragment := []byte(`[{"name":"cpu","timestamp":1000,"value":2}]`)
	validQueryFragment := []byte(`{"op":"query","name":"cpu","start":0,"end":1}`)

	cases := []struct {
		name string
		data []byte
	}{
		{"immediate-failure", nil},
		{"unterminated-valid-write-array", validWriteFragment},
		{"unterminated-valid-query-object", validQueryFragment},
		{"blank-line-then-unterminated-fragment", bytesJoin([]byte("\n"), validWriteFragment)},
		{"blank-line-only", []byte("\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &failAfterReader{data: tc.data, err: errSimulatedReadFailure}
			code, stdout, stderr := runIngestCaptureStderr(t, in)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty when no non-empty line completed", stdout)
			}
			assertReadFailureDiagnostic(t, stderr)
		})
	}
}

func bytesJoin(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// TestRunIngestReadFailureDistinctFromCleanEOF 固定正常结束与读取故障的区别：同样的
// 字节——没有末尾换行的合法末行——在正常 EOF 时照常处理一次，在读取故障结束时完全
// 不处理；此前完整行的结果在两种结束方式下逐字节相同。
func TestRunIngestReadFailureDistinctFromCleanEOF(t *testing.T) {
	writeLine := `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`
	lastQuery := `{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`
	script := []byte(writeLine + "\n" + lastQuery) // 末行合法但没有换行

	// 正常 EOF：末行查询处理一次，退出码 0，无标准错误。
	cleanCode, cleanStdout, cleanStderr := runIngestCaptureStderr(t, bytes.NewReader(script))
	if cleanCode != 0 {
		t.Fatalf("clean EOF exit code = %d, want 0", cleanCode)
	}
	if cleanStderr != "" {
		t.Fatalf("clean EOF stderr = %q, want empty", cleanStderr)
	}
	cleanRes := decodeStdoutResults(t, cleanStdout)
	if len(cleanRes) != 2 {
		t.Fatalf("clean EOF %d results, want 2 (final newline-less line processed once): %s",
			len(cleanRes), mustJSON(cleanRes))
	}
	cleanQuery := cleanRes[1]["series"].([]interface{})
	if len(cleanQuery) != 1 || cleanQuery[0].(map[string]interface{})["average"].(float64) != 2 {
		t.Fatalf("clean EOF query = %s, want the committed point average 2",
			mustJSON(cleanRes[1]))
	}

	// 读取故障结束：同样的字节，末行查询不执行，只剩第一条完整写入行的结果。
	failIn := &failAfterReader{data: script, err: errSimulatedReadFailure}
	failCode, failStdout, failStderr := runIngestCaptureStderr(t, failIn)
	if failCode != 1 {
		t.Fatalf("read-failure exit code = %d, want 1", failCode)
	}
	assertReadFailureDiagnostic(t, failStderr)
	failRes := decodeStdoutResults(t, failStdout)
	if len(failRes) != 1 {
		t.Fatalf("read failure %d results, want only the one completed write line: %s",
			len(failRes), mustJSON(failRes))
	}
	if failRes[0]["status"] != "ok" || failRes[0]["added"].(float64) != 1 {
		t.Fatalf("read failure result = %s, want the completed write retained",
			mustJSON(failRes[0]))
	}
	// 两种结束方式下，第一条完整行的结果逐字节相同。
	if !strings.HasPrefix(cleanStdout, failStdout) {
		t.Fatalf("completed-line output must be shared:\n clean=%q\n fault=%q",
			cleanStdout, failStdout)
	}

	// 只有一条无换行合法末行、此前没有任何完整行：EOF 处理一次，故障完全不处理。
	solo := []byte(`[]`)
	if soloCode, soloStdout, _ := runIngestCaptureStderr(t, bytes.NewReader(solo)); soloCode != 0 {
		t.Fatalf("clean EOF solo exit code = %d, want 0", soloCode)
	} else if len(decodeStdoutResults(t, soloStdout)) != 1 {
		t.Fatalf("clean EOF solo = %q, want one result", soloStdout)
	}
	soloFail := &failAfterReader{data: solo, err: errSimulatedReadFailure}
	if code, stdout, stderr := runIngestCaptureStderr(t, soloFail); code != 1 || stdout != "" {
		t.Fatalf("faulty solo: code=%d stdout=%q, want code 1 and empty stdout", code, stdout)
	} else {
		assertReadFailureDiagnostic(t, stderr)
	}
}
