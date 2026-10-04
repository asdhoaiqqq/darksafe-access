package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// 本文件固定 ingest 的“结果输出故障”行为：当输出目标在接收当前结果时返回错误，
// runIngest 立即以退出码 1 结束，诊断只写入标准错误（ingest: write error: <原因>），
// 不向已失败的标准输出追加业务 JSON，不把它包装成带 line/index/conflict 的采样点错误，
// 不重试当前结果，也不再为剩余输入行（即使合法）产出任何结果。
// 这些测试通过向 io.Writer 注入故障在本机离线复现，不依赖真实磁盘写满或外部进程。

// errResultSink 是离线注入的结果输出故障原因，必须原样出现在 stderr 诊断里。
var errResultSink = errors.New("simulated ingest result sink failure")

// failingResultWriter 模拟结果输出目标在某条结果写出时返回错误。
// json.Encoder.Encode 对每条结果只发起一次 Write（JSON 与结尾 '\n' 在同一批字节中），
// runIngest 的输出路径上也没有其它缓冲，因此一次 Write 调用恰好对应一条待交付结果：
//   - 前 deliver 条结果完整接收（原样留存字节）；
//   - 下一条结果：partial==0 时整条拒绝（n=0, err），partial>0 时只接收其前 partial 字节，
//     用来模拟“当前结果只有一部分字节被接收就发生错误”；
//   - 按约定 runIngest 必须在首次故障后立即返回，因此 faultCalls 必须恒为 1。
type failingResultWriter struct {
	buf        bytes.Buffer
	deliver    int
	partial    int
	faultCalls int
}

func (w *failingResultWriter) Write(p []byte) (int, error) {
	if w.deliver > 0 {
		w.deliver--
		w.buf.Write(p)
		return len(p), nil
	}
	w.faultCalls++
	if w.partial > 0 {
		n := w.partial
		if n > len(p) {
			n = len(p)
		}
		w.buf.Write(p[:n])
		return n, errResultSink
	}
	return 0, errResultSink
}

// captureStderr 临时把 os.Stderr 接到管道，同步执行 fn（runIngest 不启动 goroutine，
// 返回前所有 stderr 写入都已完成），返回退出码与捕获到的标准错误内容。
func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	code := fn()
	os.Stderr = old
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return code, string(b)
}

// splitRecordLines 把参考运行的输出切成逐条结果（每条都以 '\n' 结尾），保留原始字节。
func splitRecordLines(t *testing.T, b []byte) [][]byte {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("reference run produced no output")
	}
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			t.Fatalf("reference output has an unterminated record: %q", b)
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

func joinedLines(lines ...string) io.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// overlongThen 先放一条超长行（无结尾换行由调用方补），再接若干正常行。
func overlongThen(tail ...string) io.Reader {
	return io.MultiReader(overlongLine(1),
		strings.NewReader("\n"+strings.Join(tail, "\n")+"\n"))
}

// overlongAfter 先放若干正常行，再放一条超长行，最后再接若干正常行。
func overlongAfter(head, tail []string) io.Reader {
	return io.MultiReader(
		strings.NewReader(strings.Join(head, "\n")+"\n"),
		overlongLine(1),
		strings.NewReader("\n"+strings.Join(tail, "\n")+"\n"),
	)
}

// assertTargetKind 校验参考输出中第 target 条结果确实是用例声明的类型，
// 保证“写不出去的那条”本身就是要覆盖的成功或失败结果，而不是测试构造失误。
func assertTargetKind(t *testing.T, kind string, raw []byte) {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("target record %q is not valid JSON: %v", raw, err)
	}
	switch kind {
	case "write-ok":
		if m["status"] != "ok" || m["added"] == nil {
			t.Fatalf("target = %v, want a write success carrying added", m)
		}
	case "query-ok":
		if m["status"] != "ok" || m["op"] != "query" {
			t.Fatalf("target = %v, want a query success", m)
		}
	case "parse-error":
		assertErrorRecordShape(t, m, false, false)
		if !strings.Contains(m["error"].(string), "invalid JSON") {
			t.Fatalf("target error %v is not a whole-line parse error", m)
		}
	case "field-error":
		assertErrorRecordShape(t, m, true, false)
	case "query-error":
		// 查询校验失败：普通输入失败，无 index、无 conflict，也不是整行解析失败。
		assertErrorRecordShape(t, m, false, false)
		if strings.Contains(m["error"].(string), "invalid JSON") {
			t.Fatalf("target error %v must be a query validation error, not a parse error", m)
		}
	case "conflict-error":
		assertErrorRecordShape(t, m, true, true)
	case "overlong-error":
		assertErrorRecordShape(t, m, false, false)
		msg, _ := m["error"].(string)
		if !strings.Contains(msg, "67108864") || !strings.Contains(msg, "single-line size limit") {
			t.Fatalf("target error %q is not the size-limit error", msg)
		}
	default:
		t.Fatalf("unknown case kind %q", kind)
	}
}

func assertErrorRecordShape(t *testing.T, m map[string]interface{}, wantIndex, wantConflict bool) {
	t.Helper()
	if m["status"] != "error" {
		t.Fatalf("record = %v, want status error", m)
	}
	_, hasIndex := m["index"]
	if hasIndex != wantIndex {
		t.Fatalf("record %v index presence = %v, want %v", m, hasIndex, wantIndex)
	}
	_, hasConflict := m["conflict"]
	if hasConflict != wantConflict {
		t.Fatalf("record %v conflict presence = %v, want %v", m, hasConflict, wantConflict)
	}
}

type writeFaultCase struct {
	name     string
	kind     string           // 写不出去的目标结果类型
	newInput func() io.Reader // 参考运行与故障运行各调用一次，保证输入一致
	target   int              // 目标结果是第几条（按输出顺序，从 1 开始）
	records  int              // 参考运行应产出的结果总数
	partial  bool             // false：目标结果整条被拒绝；true：只接收前半截字节
}

func writeFaultCases() []writeFaultCase {
	const (
		w1 = `[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`
		w2 = `[{"name":"cpu","timestamp":2000,"value":2,"labels":{"host":"a"}}]`
		w3 = `[{"name":"cpu","timestamp":3000,"value":3,"labels":{"host":"a"}}]`
		q1 = `{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`

		badParse = `not json`
		badField = `[{"name":"cpu","timestamp":"soon","value":1}]`
		// 查询校验失败：op 非法。无 index、无 conflict，是“普通输入失败”。
		badQuery = `{"op":"bogus","name":"cpu","start":0,"end":1}`
		// 与 w1 同序列同时间戳但值不同：数值冲突结果（带 index 与 conflict）。
		badConflict = `[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`
	)

	return []writeFaultCase{
		// 第一条结果到达时即被整条拒绝：成功结果（写入、查询）与各类失败结果适用同一条终止规则。
		{"first write-ok rejected wholesale", "write-ok",
			func() io.Reader { return joinedLines(w1, w2, q1) }, 1, 3, false},
		{"first query-ok rejected wholesale", "query-ok",
			// 首行为空白行：不产生结果但占行号，因此第一条“结果”就是查询成功。
			func() io.Reader { return joinedLines("", q1, w2) }, 1, 2, false},
		{"first parse-error rejected wholesale", "parse-error",
			func() io.Reader { return joinedLines(badParse, w1, q1) }, 1, 3, false},
		{"first field-error rejected wholesale", "field-error",
			func() io.Reader { return joinedLines(badField, w1, q1) }, 1, 3, false},
		{"first query-error rejected wholesale", "query-error",
			func() io.Reader { return joinedLines(badQuery, w1, q1) }, 1, 3, false},
		{"first overlong-error rejected wholesale", "overlong-error",
			func() io.Reader { return overlongThen(w1, q1) }, 1, 3, false},

		// 若干条结果已完整交付后目标才被整条拒绝：此前结果保留、顺序不变、不重发；
		// 合法写入批次与有效查询同样立即终止；解析/字段/查询/冲突/超长行原本要返回的 error
		// 结果写不出去时，也按输出故障结束，而不是沿用“失败行后继续处理后续行”的规则。
		{"write-ok rejected after one delivered", "write-ok",
			func() io.Reader { return joinedLines(w1, w2, q1) }, 2, 3, false},
		{"query-ok rejected after two delivered", "query-ok",
			func() io.Reader { return joinedLines(w1, w2, q1, w3) }, 3, 4, false},
		{"parse-error rejected after two delivered", "parse-error",
			func() io.Reader { return joinedLines(w1, w2, badParse, w3, q1) }, 3, 5, false},
		{"field-error rejected after two delivered", "field-error",
			func() io.Reader { return joinedLines(w1, w2, badField, w3, q1) }, 3, 5, false},
		{"query-error rejected after two delivered", "query-error",
			func() io.Reader { return joinedLines(w1, w2, badQuery, w3, q1) }, 3, 5, false},
		{"conflict-error rejected after two delivered", "conflict-error",
			func() io.Reader { return joinedLines(w1, w2, badConflict, w3, q1) }, 3, 5, false},
		{"overlong-error rejected after two delivered", "overlong-error",
			func() io.Reader { return overlongAfter([]string{w1, w2}, []string{w3, q1}) }, 3, 5, false},

		// 目标结果只有一部分字节被接收就报错：不得补齐这条 JSON，片段不得被当成正常响应。
		{"first write-ok partially received", "write-ok",
			func() io.Reader { return joinedLines(w1, w2, q1) }, 1, 3, true},
		{"first query-ok partially received", "query-ok",
			func() io.Reader { return joinedLines("", q1, w2) }, 1, 2, true},
		{"first query-error partially received", "query-error",
			func() io.Reader { return joinedLines(badQuery, w1, q1) }, 1, 3, true},
		{"write-ok partially received after two", "write-ok",
			func() io.Reader { return joinedLines(w1, w2, w3, q1) }, 3, 4, true},
		{"query-ok partially received after two", "query-ok",
			func() io.Reader { return joinedLines(w1, w2, q1, w3) }, 3, 4, true},
		{"parse-error partially received after two", "parse-error",
			func() io.Reader { return joinedLines(w1, w2, badParse, w3, q1) }, 3, 5, true},
		{"field-error partially received after two", "field-error",
			func() io.Reader { return joinedLines(w1, w2, badField, w3, q1) }, 3, 5, true},
		{"query-error partially received after two", "query-error",
			func() io.Reader { return joinedLines(w1, w2, badQuery, w3, q1) }, 3, 5, true},
		{"conflict-error partially received after two", "conflict-error",
			func() io.Reader { return joinedLines(w1, w2, badConflict, w3, q1) }, 3, 5, true},
		{"overlong-error partially received after two", "overlong-error",
			func() io.Reader { return overlongAfter([]string{w1, w2}, []string{w3, q1}) }, 3, 5, true},
	}
}

// TestRunIngestResultWriteFailureTerminates 固定结果写出故障的全部终止语义。
func TestRunIngestResultWriteFailureTerminates(t *testing.T) {
	for _, tc := range writeFaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			// 参考运行：输出目标永不失败，取得每条结果确定的字节形态（均以 '\n' 结尾）。
			var ref bytes.Buffer
			runIngest(tc.newInput(), &ref)
			records := splitRecordLines(t, ref.Bytes())
			if len(records) != tc.records {
				t.Fatalf("reference run produced %d records, want %d", len(records), tc.records)
			}
			assertTargetKind(t, tc.kind, records[tc.target-1])
			// 用例设计自检：目标之后必须还有可产出结果的输入行，
			// 否则无法证明“剩余输入行不再产生结果”。
			if tc.target >= len(records) {
				t.Fatalf("case design: target %d must leave later records, only have %d",
					tc.target, len(records))
			}

			sink := &failingResultWriter{deliver: tc.target - 1}
			cut := 0
			if tc.partial {
				// 在目标结果的正中间截断，保证切在 JSON 内部且不会切到结尾的 '\n'。
				cut = len(records[tc.target-1]) / 2
				if cut < 1 || cut >= len(records[tc.target-1])-1 {
					t.Fatalf("partial cut %d invalid for %d-byte record", cut, len(records[tc.target-1]))
				}
				sink.partial = cut
			}

			code, stderr := captureStderr(t, func() int {
				return runIngest(tc.newInput(), sink)
			})

			// 退出码非零；底层输出错误必须发生且只发生一次：不重试当前结果，
			// 后续行（包括末尾合法写入/查询）也不再尝试写出。
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 when result delivery fails", code)
			}
			if sink.faultCalls != 1 {
				t.Fatalf("sink faulted %d time(s), want exactly 1 (no retry, no later results)",
					sink.faultCalls)
			}

			// 已完整交付的只能是目标之前的记录（参考输出的严格字节前缀：顺序、内容一致，不重发）；
			// 部分接收时再多出目标记录的前 cut 个字节，且不得包含结尾 '\n'。
			want := bytes.Join(records[:tc.target-1], nil)
			var fragment []byte
			if tc.partial {
				fragment = records[tc.target-1][:cut]
				want = append(append(make([]byte, 0, len(want)+len(fragment)), want...), fragment...)
			}
			if got := sink.buf.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("delivered stdout bytes mismatch:\n got %q\nwant %q", got, want)
			}

			// 此前完整交付的旧记录仍可各自解析为 JSON，且与参考输出逐字节一致。
			for i, raw := range records[:tc.target-1] {
				var m map[string]interface{}
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatalf("delivered record %d %q stopped being valid JSON: %v", i+1, raw, err)
				}
			}

			// 片段必须能与正常响应区分：没有行分隔符、本身不是合法 JSON，
			// 消费者不能把它当成一条完整响应。
			if tc.partial {
				if bytes.HasSuffix(fragment, []byte("\n")) {
					t.Fatalf("fragment %q must not be terminated like a complete record", fragment)
				}
				var v interface{}
				if err := json.Unmarshal(fragment, &v); err == nil {
					t.Fatalf("fragment %q parses as JSON; it must not be mistaken for a result", fragment)
				}
			} else if tc.target == 1 && sink.buf.Len() != 0 {
				t.Fatalf("first result rejected wholesale but stdout received %q", sink.buf.Bytes())
			}

			// 诊断只进入标准错误：唯一一行、固定前缀、带底层原因；
			// 它是纯文本，不是带 line/index/conflict 的业务 JSON。
			diagnostic := strings.TrimSuffix(stderr, "\n")
			if strings.Contains(diagnostic, "\n") {
				t.Fatalf("want exactly one diagnostic line, stderr = %q", stderr)
			}
			wantDiagnostic := "ingest: write error: " + errResultSink.Error()
			if diagnostic != wantDiagnostic {
				t.Fatalf("stderr = %q, want %q", stderr, wantDiagnostic)
			}
			for _, banned := range []string{`"status"`, `"line"`, `"index"`, `"conflict"`} {
				if strings.Contains(diagnostic, banned) {
					t.Fatalf("diagnostic must not be a business JSON record, contains %s: %q",
						banned, diagnostic)
				}
			}
		})
	}
}

// TestRunIngestWriteFailureDoesNotAppendBusinessError 单独锁定一个易回归点：
// 当一条 error 结果本身写不出去时，标准输出上除已接收的片段外不得再多出任何字节，
// 尤其不能出现第二条以 {"status":"error" 开头的记录。
func TestRunIngestWriteFailureDoesNotAppendBusinessError(t *testing.T) {
	input := joinedLines(
		`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,
		`not json`, // 目标：解析失败的 error 结果在输出时整条被拒
		`[{"name":"cpu","timestamp":2000,"value":2,"labels":{"host":"a"}}]`,
	)
	var ref bytes.Buffer
	runIngest(input, &ref)
	records := splitRecordLines(t, ref.Bytes())
	if len(records) != 3 {
		t.Fatalf("reference records = %d, want 3", len(records))
	}

	sink := &failingResultWriter{deliver: 1} // 第一条成功交付，第二条（error 结果）被拒
	code, stderr := captureStderr(t, func() int {
		return runIngest(joinedLines(
			`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,
			`not json`,
			`[{"name":"cpu","timestamp":2000,"value":2,"labels":{"host":"a"}}]`,
		), sink)
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got, want := sink.buf.String(), string(records[0]); got != want {
		t.Fatalf("stdout = %q, want only the first delivered record %q", got, want)
	}
	if sink.faultCalls != 1 {
		t.Fatalf("sink faulted %d time(s), want 1", sink.faultCalls)
	}
	if !strings.HasPrefix(stderr, "ingest: write error: ") {
		t.Fatalf("stderr = %q, want the write-error diagnostic on stderr", stderr)
	}
}
