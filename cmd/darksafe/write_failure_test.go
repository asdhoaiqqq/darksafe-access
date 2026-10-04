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

// 结果输出故障回归保障（本机离线、确定性复现，不依赖真实磁盘写满或外部进程故障）。
//
// 固定 runIngest 的现有行为：结果输出目标在接收某条结果时返回错误后——
//   - 立即以非零退出码结束，不重试当前结果，剩余输入行（即使是合法写入或查询）
//     也不再产生任何结果；
//   - 诊断只进入标准错误，恰为一行 "ingest: write error: <底层错误原因>"，
//     不再向已经失败的标准输出追加业务 JSON，也不把输出故障包装成带
//     line/index/conflict 的采样点错误；
//   - 此前已完整交付的结果按原顺序保留、不重发；当前结果若只有一部分字节被
//     接收，留下的就是一个不可解析的片段，命令同样立即结束、不补齐这条 JSON。
//
// 该规则对成功结果（合法写入、有效查询）与失败结果（输入校验失败、数值冲突、
// 超过单行字节上限）同样适用；输出正常时按输入顺序响应、失败行后继续处理、
// 最终退出码约定等既有行为不在此文件改动范围内。

// errResultSinkBroken 是注入给结果输出目标的确定性故障原因。
var errResultSinkBroken = errors.New("synthetic result sink broken")

// faultingResultWriter 模拟“结果保存目标”在接收结果时报错。
// 前 failAt-1 次 Write（json.Encoder 每条记录恰好一次 Write）完整收进内存；
// 第 failAt 次 Write 只接收 accept 个字节（可为 0）后返回固定错误，用于分别
// 复现“第一条结果即被拒绝”“若干条完整交付后才报错”和“当前记录只接收了
// 部分字节”。此后任何 Write 仍直接失败，作为“故障后继续写/重试”的兜底：
// 正确实现不应再产生一次 Write。records 记录实际 Write 调用次数。
type faultingResultWriter struct {
	buf     bytes.Buffer
	failAt  int
	accept  int
	records int
	failed  bool
}

func (w *faultingResultWriter) Write(p []byte) (int, error) {
	w.records++
	if w.records < w.failAt {
		return w.buf.Write(p)
	}
	if !w.failed {
		w.failed = true
		n := w.accept
		if n > len(p) {
			n = len(p)
		}
		if n > 0 {
			w.buf.Write(p[:n])
		}
		return n, errResultSinkBroken
	}
	// 故障之后的任何写入都不允许出现：继续报错以便测试暴露重试/续处理。
	return 0, errResultSinkBroken
}

// captureStderr 临时把进程级 os.Stderr 接到管道上执行 run，返回写出的全部诊断。
// 本包测试均不使用 t.Parallel，串行执行下替换是安全的。
func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	run()
	if err := w.Close(); err != nil {
		t.Fatalf("close stderr pipe: %v", err)
	}
	os.Stderr = orig
	return <-done
}

// checkWriteFaultDiagnostic 固定输出故障的诊断契约：恰好一行、只进 stderr、
// 带固定前缀与底层原因，且是纯文本而不是带 line/index/conflict 的业务 JSON。
func checkWriteFaultDiagnostic(t *testing.T, stderr string) {
	t.Helper()
	diagnostic := strings.TrimSuffix(stderr, "\n")
	if stderr == "" || !strings.HasSuffix(stderr, "\n") || strings.Contains(diagnostic, "\n") {
		t.Fatalf("diagnostic must be exactly one newline-terminated stderr line, got %q", stderr)
	}
	if !strings.HasPrefix(diagnostic, "ingest: write error: ") {
		t.Fatalf("stderr = %q, want prefix %q", stderr, "ingest: write error: ")
	}
	if !strings.Contains(diagnostic, errResultSinkBroken.Error()) {
		t.Fatalf("stderr = %q, want it to carry the underlying reason %q",
			stderr, errResultSinkBroken)
	}
	var v any
	if json.Unmarshal([]byte(diagnostic), &v) == nil {
		t.Fatalf("write-fault diagnostic must stay plain text, not a business JSON record: %q", stderr)
	}
	for _, banned := range []string{`"line"`, `"index"`, `"conflict"`} {
		if strings.Contains(diagnostic, banned) {
			t.Fatalf("write-fault diagnostic must not be wrapped as a sample-point error carrying %s: %q",
				banned, stderr)
		}
	}
}

// splitDelivered 把输出拆成“已完整交付的记录”（按顺序，每行一条 JSON）
// 与最后一个换行之后的尾部字节。故障发生在一条记录中途时，尾部就是该记录
// 未补齐的片段，必须与此前完整交付的记录区分开。
func splitDelivered(t *testing.T, out string) ([]string, string) {
	t.Helper()
	idx := strings.LastIndexByte(out, '\n')
	if idx < 0 {
		return nil, out // 没有任何完整记录；out 为空或只是片段
	}
	head := out[:idx]
	tail := out[idx+1:]
	if head == "" {
		return nil, tail
	}
	return strings.Split(head, "\n"), tail
}

// writeFaultSequenceInput 构造一条 8 行、8 条结果的混合序列：
// 合法写入、合法写入、有效查询、数值冲突错误、整行解析错误、字段校验错误、
// 合法查询、合法写入。后两条合法结果用于证明输出故障后“剩余合法行不再产出”。
func writeFaultSequenceInput() io.Reader {
	lines := []string{
		`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,      // 1 写入成功
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,      // 2 写入成功
		`{"op":"query","name":"cpu","start":0,"end":9000,"labels":{"host":"a"}}`, // 3 查询成功
		`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,      // 4 数值冲突错误
		`not json`, // 5 整行解析错误
		`[{"name":"cpu","timestamp":"soon","value":3}]`,                          // 6 字段校验错误
		`{"op":"query","name":"cpu","start":0,"end":9000,"labels":{"host":"a"}}`, // 7 合法查询
		`[{"name":"cpu","timestamp":3000,"value":5,"labels":{"host":"a"}}]`,      // 8 合法写入
	}
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// runFaultSequenceControl 在输出正常时跑一遍同一输入，作为“本应交付的结果”
// 参照：既验证各类结果的既有形状，也为故障场景提供逐字节前缀基准。
func runFaultSequenceControl(t *testing.T) []string {
	t.Helper()
	var control bytes.Buffer
	if code := runIngest(writeFaultSequenceInput(), &control); code == 0 {
		t.Fatalf("control run: input contains failed lines, want non-zero exit code")
	}
	lines := strings.Split(strings.TrimRight(control.String(), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("control run: got %d result lines, want 8: %v", len(lines), lines)
	}
	for _, line := range lines {
		decodeResultLine(t, line) // 参照输出本身必须全部是完整 JSON
	}
	conflict := decodeResultLine(t, lines[3])
	if conflict["status"] != "error" || int(conflict["line"].(float64)) != 4 ||
		int(conflict["index"].(float64)) != 1 {
		t.Fatalf("control line 4 = %v, want conflict error with line 4 index 1", conflict)
	}
	if _, has := conflict["conflict"]; !has {
		t.Fatalf("control line 4 = %v, want conflict payload", conflict)
	}
	parseErr := decodeResultLine(t, lines[4])
	if parseErr["status"] != "error" || int(parseErr["line"].(float64)) != 5 {
		t.Fatalf("control line 5 = %v, want parse error on input line 5", parseErr)
	}
	if _, has := parseErr["index"]; has {
		t.Fatalf("control line 5 = %v, parse error must not carry index", parseErr)
	}
	fieldErr := decodeResultLine(t, lines[5])
	if fieldErr["status"] != "error" || int(fieldErr["line"].(float64)) != 6 ||
		int(fieldErr["index"].(float64)) != 1 {
		t.Fatalf("control line 6 = %v, want field-validation error with line 6 index 1", fieldErr)
	}
	if _, has := fieldErr["conflict"]; has {
		t.Fatalf("control line 6 = %v, field-validation error must not carry conflict", fieldErr)
	}
	return lines
}

// TestRunIngestWriteFailureTerminatesSequence 覆盖同一条成功/失败混合序列上的
// 三种故障时机：第一条结果即被拒绝、若干条完整交付后拒绝、当前记录只接收部分字节。
func TestRunIngestWriteFailureTerminatesSequence(t *testing.T) {
	control := runFaultSequenceControl(t)

	t.Run("rejects_first_result_and_emits_nothing", func(t *testing.T) {
		w := &faultingResultWriter{failAt: 1}
		var code int
		stderr := captureStderr(t, func() {
			code = runIngest(writeFaultSequenceInput(), w)
		})
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero when the first result cannot be written")
		}
		if w.records != 1 {
			t.Fatalf("writer saw %d Write calls, want exactly 1 (no retry, no further lines)", w.records)
		}
		if w.buf.Len() != 0 {
			t.Fatalf("no result bytes may remain when the first write is rejected, got %q", w.buf.String())
		}
		checkWriteFaultDiagnostic(t, stderr)
	})

	t.Run("keeps_delivered_results_then_stops_after_hard_fault", func(t *testing.T) {
		// 前 3 条结果（2 条写入成功 + 1 条查询成功）完整交付；第 4 条（冲突错误）
		// 被整条拒绝。错误结果写不出去时按输出故障结束，不能沿用“失败行后继续
		// 处理后续行”的规则。
		w := &faultingResultWriter{failAt: 4}
		var code int
		stderr := captureStderr(t, func() {
			code = runIngest(writeFaultSequenceInput(), w)
		})
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero on result-write failure")
		}
		if w.records != 4 {
			t.Fatalf("writer saw %d Write calls, want exactly 4 (no retry, no results for remaining lines)",
				w.records)
		}
		want := strings.Join(control[:3], "\n") + "\n"
		if got := w.buf.String(); got != want {
			t.Fatalf("delivered output = %q, want exactly the first 3 control records unchanged, not resent",
				got)
		}
		complete, tail := splitDelivered(t, w.buf.String())
		if tail != "" {
			t.Fatalf("hard fault must leave no partial bytes, tail = %q", tail)
		}
		if len(complete) != 3 {
			t.Fatalf("got %d complete records, want 3: %v", len(complete), complete)
		}
		for i, line := range complete {
			if line != control[i] {
				t.Fatalf("delivered record %d = %q, want control %q (order and content preserved)",
					i+1, line, control[i])
			}
		}
		if strings.Contains(w.buf.String(), control[3]) {
			t.Fatalf("the failed conflict result must not appear in business output")
		}
		if strings.Contains(w.buf.String(), "write error") {
			t.Fatalf("write-fault diagnostic must not be appended to the failed business output")
		}
		checkWriteFaultDiagnostic(t, stderr)
	})

	t.Run("partial_record_is_left_as_distinguishable_fragment", func(t *testing.T) {
		// 第 4 条结果（冲突错误）只被接收前 8 个字节即报错：立即结束，不补齐
		// 这条 JSON；前 3 条完整记录保留，片段不能被当成正常响应。
		const fragmentBytes = 8
		w := &faultingResultWriter{failAt: 4, accept: fragmentBytes}
		var code int
		stderr := captureStderr(t, func() {
			code = runIngest(writeFaultSequenceInput(), w)
		})
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero after a partial result write")
		}
		if w.records != 4 {
			t.Fatalf("writer saw %d Write calls, want exactly 4 (no retry, no completion attempt)",
				w.records)
		}
		fullRecord := control[3] + "\n"
		if len(fullRecord) <= fragmentBytes {
			t.Fatalf("test setup invalid: faulting record only %d bytes", len(fullRecord))
		}
		fragment := fullRecord[:fragmentBytes]
		want := strings.Join(control[:3], "\n") + "\n" + fragment
		if got := w.buf.String(); got != want {
			t.Fatalf("output = %q, want 3 intact records plus the exact %d-byte fragment", got, fragmentBytes)
		}
		complete, tail := splitDelivered(t, w.buf.String())
		if len(complete) != 3 {
			t.Fatalf("got %d complete records, want 3: %v", len(complete), complete)
		}
		for i, line := range complete {
			if line != control[i] {
				t.Fatalf("delivered record %d changed: %q vs control %q", i+1, line, control[i])
			}
			var v any
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Fatalf("delivered record %d must remain valid JSON: %v", i+1, err)
			}
		}
		if tail != fragment {
			t.Fatalf("tail = %q, want the exact partial fragment %q", tail, fragment)
		}
		if !strings.HasPrefix(fullRecord, tail) || tail == fullRecord {
			t.Fatalf("fragment %q must be a strict prefix of the undelivered record %q", tail, fullRecord)
		}
		var v any
		if json.Unmarshal([]byte(tail), &v) == nil {
			t.Fatalf("partial fragment %q must not parse as a normal response", tail)
		}
		checkWriteFaultDiagnostic(t, stderr)
	})
}

// TestRunIngestWriteFailureCoversEveryResultKind 固定终止规则对每一类“本应输出
// 的结果”都生效：合法写入/有效查询的成功结果，以及冲突、整行解析、字段校验、
// 超长行的失败结果。写不出去的 error 结果按输出故障结束，不继续后续行。
func TestRunIngestWriteFailureCoversEveryResultKind(t *testing.T) {
	validQuery := `{"op":"query","name":"cpu","start":0,"end":9000,"labels":{"host":"a"}}`

	// checkUndelivered 校验“本应交付但写不出去”的那条参照记录的形状，
	// 证明输出故障规则覆盖的是正确的那一业务结果种类。
	type row struct {
		name             string
		newInput         func() io.Reader
		failAt           int
		wantDelivered    int
		checkUndelivered func(t *testing.T, m map[string]any)
	}
	rows := []row{
		{
			name: "successful_query_rejected_on_first_result",
			newInput: func() io.Reader {
				return strings.NewReader(strings.Join([]string{
					`{"op":"query","name":"cpu","start":0,"end":9}`,
					`[{"name":"cpu","timestamp":1,"value":1}]`,
				}, "\n") + "\n")
			},
			failAt:           1,
			wantDelivered:    0,
			checkUndelivered: expectOKQuery,
		},
		{
			name: "successful_query_after_delivered_write",
			newInput: func() io.Reader {
				return strings.NewReader(strings.Join([]string{
					`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,
					validQuery,
					`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,
				}, "\n") + "\n")
			},
			failAt:           2,
			wantDelivered:    1,
			checkUndelivered: expectOKQuery,
		},
		{
			name: "successful_write_after_other_results",
			newInput: func() io.Reader {
				return strings.NewReader(strings.Join([]string{
					`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,
					validQuery,
					`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,
				}, "\n") + "\n")
			},
			failAt:        3,
			wantDelivered: 2,
			checkUndelivered: func(t *testing.T, m map[string]any) {
				t.Helper()
				if m["status"] != "ok" || m["added"].(float64) != 1 {
					t.Fatalf("undelivered record = %v, want a successful write with added 1", m)
				}
			},
		},
		{
			name: "conflict_error_result_cannot_be_written",
			newInput: func() io.Reader {
				return strings.NewReader(strings.Join([]string{
					`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,
					`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,
					validQuery,
				}, "\n") + "\n")
			},
			failAt:        2,
			wantDelivered: 1,
			checkUndelivered: func(t *testing.T, m map[string]any) {
				t.Helper()
				if m["status"] != "error" || int(m["line"].(float64)) != 2 ||
					int(m["index"].(float64)) != 1 {
					t.Fatalf("undelivered record = %v, want conflict error with line 2 index 1", m)
				}
				conflict, ok := m["conflict"].(map[string]any)
				if !ok {
					t.Fatalf("undelivered record = %v, want conflict payload", m)
				}
				if conflict["existing"].(float64) != 1 || conflict["submitted"].(float64) != 9 {
					t.Fatalf("undelivered conflict payload = %v", conflict)
				}
			},
		},
		{
			name: "whole_line_parse_error_result_cannot_be_written",
			newInput: func() io.Reader {
				return strings.NewReader(strings.Join([]string{
					`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,
					`not json`,
					validQuery,
				}, "\n") + "\n")
			},
			failAt:        2,
			wantDelivered: 1,
			checkUndelivered: func(t *testing.T, m map[string]any) {
				t.Helper()
				if m["status"] != "error" || int(m["line"].(float64)) != 2 {
					t.Fatalf("undelivered record = %v, want parse error on input line 2", m)
				}
				if _, has := m["index"]; has {
					t.Fatalf("undelivered parse error = %v, must not carry index", m)
				}
				if _, has := m["conflict"]; has {
					t.Fatalf("undelivered parse error = %v, must not carry conflict", m)
				}
			},
		},
		{
			name: "field_validation_error_result_cannot_be_written",
			newInput: func() io.Reader {
				return strings.NewReader(strings.Join([]string{
					`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`,
					`[{"name":"cpu","timestamp":"soon","value":3}]`,
					validQuery,
				}, "\n") + "\n")
			},
			failAt:        2,
			wantDelivered: 1,
			checkUndelivered: func(t *testing.T, m map[string]any) {
				t.Helper()
				if m["status"] != "error" || int(m["line"].(float64)) != 2 ||
					int(m["index"].(float64)) != 1 {
					t.Fatalf("undelivered record = %v, want field error with line 2 index 1", m)
				}
				if _, has := m["conflict"]; has {
					t.Fatalf("undelivered field error = %v, must not carry conflict", m)
				}
			},
		},
		{
			name: "overlong_line_error_result_cannot_be_written",
			newInput: func() io.Reader {
				// 行 1 合法写入；行 2 超长（整行失败、无 index/conflict）；行 3 合法查询。
				return io.MultiReader(
					strings.NewReader(`[{"name":"m","timestamp":1,"value":1}]`+"\n"),
					overlongLine(1),
					strings.NewReader("\n"+validQueryForM()+"\n"),
				)
			},
			failAt:        2,
			wantDelivered: 1,
			checkUndelivered: func(t *testing.T, m map[string]any) {
				t.Helper()
				if m["status"] != "error" || int(m["line"].(float64)) != 2 {
					t.Fatalf("undelivered record = %v, want size-limit error on input line 2", m)
				}
				if _, has := m["index"]; has {
					t.Fatalf("undelivered over-long error = %v, must not carry index", m)
				}
				if _, has := m["conflict"]; has {
					t.Fatalf("undelivered over-long error = %v, must not carry conflict", m)
				}
				msg, _ := m["error"].(string)
				if !strings.Contains(msg, "67108864") || !strings.Contains(msg, "single-line size limit") {
					t.Fatalf("undelivered over-long error message = %q", msg)
				}
			},
		},
		{
			name: "overlong_line_error_rejected_on_first_result",
			newInput: func() io.Reader {
				// 第一行即超长，随后还有一条合法查询：故障时不得有任何输出。
				return io.MultiReader(
					overlongLine(1),
					strings.NewReader("\n"+validQueryForM()+"\n"),
				)
			},
			failAt:        1,
			wantDelivered: 0,
			checkUndelivered: func(t *testing.T, m map[string]any) {
				t.Helper()
				if m["status"] != "error" || int(m["line"].(float64)) != 1 {
					t.Fatalf("undelivered record = %v, want size-limit error on input line 1", m)
				}
				if _, has := m["index"]; has {
					t.Fatalf("undelivered over-long error = %v, must not carry index", m)
				}
				if _, has := m["conflict"]; has {
					t.Fatalf("undelivered over-long error = %v, must not carry conflict", m)
				}
			},
		},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			// 参照运行：输出目标正常时，每条输入行本应得到的完整 JSON 结果。
			var control bytes.Buffer
			controlCode := runIngest(tc.newInput(), &control)
			controlLines := strings.Split(strings.TrimRight(control.String(), "\n"), "\n")
			if len(controlLines) < tc.failAt {
				t.Fatalf("control run only produced %d records, cannot fail at %d: %v",
					len(controlLines), tc.failAt, controlLines)
			}
			if controlCode == 0 && strings.Contains(controlLines[tc.failAt-1], `"error"`) {
				t.Fatalf("control setup inconsistent: record %d looks like an error but exit code was 0",
					tc.failAt)
			}
			tc.checkUndelivered(t, decodeResultLine(t, controlLines[tc.failAt-1]))

			// 故障运行：在目标记录处整条拒绝（不接收任何字节）。
			w := &faultingResultWriter{failAt: tc.failAt}
			var code int
			stderr := captureStderr(t, func() {
				code = runIngest(tc.newInput(), w)
			})
			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero when result record %d cannot be delivered",
					tc.failAt)
			}
			if w.records != tc.failAt {
				t.Fatalf("writer saw %d Write calls, want exactly %d (no retry; remaining lines produce nothing)",
					w.records, tc.failAt)
			}

			complete, tail := splitDelivered(t, w.buf.String())
			if tail != "" {
				t.Fatalf("hard rejection must leave no partial record, tail = %q", tail)
			}
			if len(complete) != tc.wantDelivered {
				t.Fatalf("got %d delivered records, want %d: %v",
					len(complete), tc.wantDelivered, complete)
			}
			want := ""
			if tc.wantDelivered > 0 {
				want = strings.Join(controlLines[:tc.wantDelivered], "\n") + "\n"
			}
			if got := w.buf.String(); got != want {
				t.Fatalf("business output = %q, want exactly the %d previously delivered control record(s), unchanged",
					got, tc.wantDelivered)
			}
			for i := 0; i < tc.wantDelivered; i++ {
				if complete[i] != controlLines[i] {
					t.Fatalf("delivered record %d = %q, want control %q (preserved in order, not resent)",
						i+1, complete[i], controlLines[i])
				}
			}
			// 未交付的目标记录以及后续任何记录都不得出现在业务输出中。
			for _, line := range controlLines[tc.failAt-1:] {
				if strings.Contains(w.buf.String(), line) {
					t.Fatalf("business output must not contain the undelivered/later record %q", line)
				}
			}
			if strings.Contains(w.buf.String(), "write error") {
				t.Fatalf("write-fault diagnostic must stay on stderr, not in business output")
			}
			checkWriteFaultDiagnostic(t, stderr)
		})
	}
}

// expectOKQuery 校验一条成功查询结果的形状。
func expectOKQuery(t *testing.T, m map[string]any) {
	t.Helper()
	if m["status"] != "ok" || m["op"] != "query" {
		t.Fatalf("record = %v, want a successful query result", m)
	}
}

// validQueryForM 返回一条针对指标 m 的有效查询，供超长行之后的“后续合法行”使用。
func validQueryForM() string {
	return `{"op":"query","name":"m","start":0,"end":1}`
}
