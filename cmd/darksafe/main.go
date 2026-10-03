// Command darksafe is the 零信任身份与授权决策平台 entry point.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "ingest":
		os.Exit(runIngest(os.Stdin, os.Stdout))
	case "version":
		fmt.Println("darksafe 0.1.0")
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: darksafe [demo|version|ingest|help]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  demo     run the bundled access-decision demo")
	fmt.Fprintln(w, "  version  print the version")
	fmt.Fprintln(w, "  ingest   batch-ingest metric samples from standard input")
	fmt.Fprintln(w, "  help     show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "ingest input:")
	fmt.Fprintln(w, "  Standard input is read line by line. Writes and queries may alternate;")
	fmt.Fprintln(w, "  queries only read data committed by earlier successful write lines and")
	fmt.Fprintln(w, "  never change storage. Blank lines produce no result but still count toward")
	fmt.Fprintln(w, "  line numbers. Accepted data is held in memory for this process only and is")
	fmt.Fprintln(w, "  never written to disk.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  A single input line may contain at most 67,108,864 raw bytes (64 MiB),")
	fmt.Fprintln(w, "  excluding the line separator; a line exactly at the limit is still")
	fmt.Fprintln(w, "  processed normally. A longer line fails as one whole input: no point on it")
	fmt.Fprintln(w, "  is written and no query on it runs, it reports one error result carrying")
	fmt.Fprintln(w, "  its input line number (with no index or conflict), and later lines keep")
	fmt.Fprintln(w, "  being processed in order.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Write line: a JSON array of sample objects; each line is one batch.")
	fmt.Fprintln(w, "  A sample is {\"name\": string, \"timestamp\": <int64 milliseconds>,")
	fmt.Fprintln(w, "  \"value\": <finite number>} and may optionally include")
	fmt.Fprintln(w, "  \"labels\": {\"key\": \"value\"}. Empty label values are preserved verbatim;")
	fmt.Fprintln(w, "  omitting labels and passing an empty labels object both mean no labels.")
	fmt.Fprintln(w, "  Missing fields, wrong types, unknown fields, duplicate keys, illegal")
	fmt.Fprintln(w, "  numbers, or input that is not a JSON array fail the whole batch.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  A series is identified by metric name plus its full label set; label order does")
	fmt.Fprintln(w, "  not matter. Submitting the same series/timestamp with the same value is a")
	fmt.Fprintln(w, "  duplicate (1 and 1.0 are equal) and is ignored; a different value rejects the")
	fmt.Fprintln(w, "  entire batch without overwriting anything. Any failure in a batch discards all")
	fmt.Fprintln(w, "  new points from that line; earlier successful batches remain in effect.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Query line: a JSON object {\"op\":\"query\",\"name\":\"cpu\",")
	fmt.Fprintln(w, "  \"start\":1000,\"end\":2000,\"labels\":{\"host\":\"a\"}}. op, name, start and")
	fmt.Fprintln(w, "  end are required; name is an exact, non-empty metric-name match and start/end")
	fmt.Fprintln(w, "  are inclusive int64 millisecond bounds (start == end queries that one")
	fmt.Fprintln(w, "  timestamp; start > end is rejected). labels is optional: omit it or pass {}")
	fmt.Fprintln(w, "  to match every series of the metric; otherwise every given key must exist with")
	fmt.Fprintln(w, "  exactly the same value (extra series labels are allowed, and an empty string")
	fmt.Fprintln(w, "  only matches a label that actually exists with that empty value). Names and")
	fmt.Fprintln(w, "  labels are matched verbatim without trimming or case folding.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "ingest output (one JSON line per non-empty input line, in input order):")
	fmt.Fprintln(w, "  write ok: {\"status\":\"ok\",\"added\":N,\"duplicates\":N,\"series\":[...]}")
	fmt.Fprintln(w, "           where every known series is listed with name, labels and timestamp-")
	fmt.Fprintln(w, "           sorted points; series are ordered by name and then by sorted labels.")
	fmt.Fprintln(w, "  query ok: {\"status\":\"ok\",\"op\":\"query\",\"series\":[{\"name\":...,")
	fmt.Fprintln(w, "           \"labels\":{},\"count\":N,\"average\":N.N}]} — one entry per series")
	fmt.Fprintln(w, "           having points in range, in the same series order, with the full name")
	fmt.Fprintln(w, "           and label set; an empty match yields []. The arithmetic mean is taken")
	fmt.Fprintln(w, "           over stored float64 values and is always finite for finite inputs.")
	fmt.Fprintln(w, "  failure: {\"status\":\"error\",\"line\":N,\"index\":M,\"error\":\"...\",")
	fmt.Fprintln(w, "           \"conflict\":{...}} — index is the 1-based sample position and is")
	fmt.Fprintln(w, "           omitted when the whole line cannot be parsed, for query lines, or")
	fmt.Fprintln(w, "           for a line exceeding the 64 MiB line-size limit; the size error")
	fmt.Fprintln(w, "           likewise carries no conflict. Otherwise conflict reports the")
	fmt.Fprintln(w, "           series, timestamp, existing and submitted values.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Processing continues after a failed line. The command exits non-zero if any")
	fmt.Fprintln(w, "  line failed, and zero when every line succeeded.")
}

// runIngest 逐行处理标准输入，逐行输出 JSON 结果。返回进程退出码。
func runIngest(in io.Reader, out io.Writer) int {
	store := darksafe.NewMetricStore()
	reader := newLineReader(in)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	emit := func(v any) bool {
		if err := enc.Encode(v); err != nil {
			fmt.Fprintln(os.Stderr, "ingest: write error:", err)
			return false
		}
		return true
	}

	exitCode := 0
	lineNo := 0
	for {
		line, tooLong, err := reader.Next()
		if err == io.EOF {
			return exitCode
		}
		if err != nil {
			// 输入读取真正发生故障：按现有方式报告并立即终止。
			fmt.Fprintln(os.Stderr, "ingest: read error:", err)
			return 1
		}
		lineNo++
		if tooLong {
			// 整条超长行（从行首到下一个行分隔符）都属于这一条失败输入：
			// 不解析、不写入任何点、不执行任何查询，只输出一次行错误。
			le := darksafe.NewLineSizeError()
			le.Line = lineNo
			if !emit(le) {
				return 1
			}
			exitCode = 1
			continue
		}
		text := line // 已固化的行内容，缓冲移动不影响该字符串
		if len(strings.TrimSpace(text)) == 0 {
			continue // 空白行不产生结果，但已计入行号
		}
		result, lineErr := store.ProcessLine(text)
		if lineErr != nil {
			lineErr.Line = lineNo
			if !emit(lineErr) {
				return 1
			}
			exitCode = 1
			continue
		}
		if !emit(result) {
			return 1
		}
	}
}

// maxLineBuffer 是查找行分隔符时允许缓冲的最大字节数：合法行最多
// MaxLineBytes 字节，另加行末可能紧跟 '\n' 的单个 '\r' 与 '\n' 本身。
const maxLineBuffer = darksafe.MaxLineBytes + 2

// lineReader 按行从底层 reader 读取，并对单行原始字节数施加上限。
// 它沿用 bufio.Scanner 的换行识别方式：'\n' 为行分隔符，行尾紧跟的
// 单个 '\r' 一并去掉，分隔符不计入行大小。与 Scanner 不同的是：
// 超过上限的行作为一条完整输入被跳过（读取并丢弃直到下一个 '\n'，
// 含末尾无换行直接结束的情况），之后的行仍可继续读取。
type lineReader struct {
	r   io.Reader
	buf []byte
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: r, buf: make([]byte, 0, 64*1024)}
}

// Next 返回下一行（不含行分隔符）。tooLong 为 true 表示该行去掉行末
// '\r' 后仍严格超过 MaxLineBytes：本次调用已把该行剩余部分排干到下一个
// 行分隔符（或输入末尾），调用方不得解析其内容。输入正常结束且没有更多
// 字节时返回 io.EOF；结尾恰好是一个分隔符不会产生额外的空行。
//
// 不变量：lr.buf 始终保存尚未消费的输入字节；正常返回的字符串在移动缓冲
// 之前复制完成，与缓冲后续内容无关。
func (lr *lineReader) Next() (line string, tooLong bool, err error) {
	for {
		if i := bytes.IndexByte(lr.buf, '\n'); i >= 0 {
			token := dropTrailingCR(lr.buf[:i])
			over := len(token) > darksafe.MaxLineBytes
			// 先判定大小再决定是否复制内容；无论成败都要把分隔符后的字节
			// 移到缓冲开头（in-place 前移会覆盖 token，故合法行先转成 string）。
			if !over {
				line = string(token)
			}
			n := copy(lr.buf, lr.buf[i+1:])
			lr.buf = lr.buf[:n]
			if over {
				return "", true, nil
			}
			return line, false, nil
		}
		if len(lr.buf) == cap(lr.buf) {
			if cap(lr.buf) < maxLineBuffer {
				lr.grow()
				continue
			}
			// 已缓冲 maxLineBuffer 字节仍无 '\n'：即使行末带 '\r'，
			// 去掉后也至少有 MaxLineBytes+1 字节，必然超限。排干该行，
			// 让下一次 Next 从下一行的行首开始。
			rest, derr := lr.drainLine()
			if derr != nil && derr != io.EOF {
				return "", false, derr
			}
			lr.buf = rest // 可能为 nil（超长行一直到输入末尾）
			return "", true, nil
		}
		n, rerr := lr.r.Read(lr.buf[len(lr.buf):cap(lr.buf)])
		lr.buf = lr.buf[:len(lr.buf)+n]
		if rerr != nil && rerr != io.EOF {
			return "", false, rerr
		}
		if rerr == io.EOF {
			if len(lr.buf) == 0 {
				return "", false, io.EOF
			}
			// 末尾无换行：剩余内容作为最后一行；清空缓冲，避免下一次
			// 调用（读到 EOF）重复返回同一行。
			token := dropTrailingCR(lr.buf)
			lr.buf = lr.buf[:0]
			if len(token) > darksafe.MaxLineBytes {
				return "", true, nil
			}
			return string(token), false, nil
		}
	}
}

// drainLine 读取并丢弃直到下一个 '\n'（含）为止的字节，返回 '\n'
// 之后已读到的字节（属于下一行）；输入直接结束时返回 io.EOF。
// '\n' 位于已缓冲内容中时不产生读取。
func (lr *lineReader) drainLine() (rest []byte, err error) {
	if i := bytes.IndexByte(lr.buf, '\n'); i >= 0 {
		return append([]byte(nil), lr.buf[i+1:]...), nil
	}
	tmp := make([]byte, 32*1024)
	for {
		n, rerr := lr.r.Read(tmp)
		if i := bytes.IndexByte(tmp[:n], '\n'); i >= 0 {
			return append([]byte(nil), tmp[i+1:n]...), nil
		}
		if rerr != nil {
			return nil, rerr
		}
	}
}

// dropTrailingCR 去掉行末紧跟 '\n'（或 EOF）的单个 '\r'，其余 '\r' 原样保留。
func dropTrailingCR(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\r' {
		return line[:len(line)-1]
	}
	return line
}

// grow 倍增行缓冲，上限为 maxLineBuffer；正常行远小于该值，
// 只有逼近单行上限的输入才会持续扩容。
func (lr *lineReader) grow() {
	size := cap(lr.buf) * 2
	if size < 64*1024 {
		size = 64 * 1024
	}
	if size > maxLineBuffer {
		size = maxLineBuffer
	}
	grown := make([]byte, len(lr.buf), size)
	copy(grown, lr.buf)
	lr.buf = grown
}

func runDemo() {
	subjects := []darksafe.Subject{
		{ID: "u-1001", Kind: "user", Roles: []string{"org/payments/ledger:read"}},
		{ID: "svc-batch", Kind: "service", Roles: []string{"owner"}},
		{ID: "u-1002", Kind: "user", Disabled: true},
	}
	resource := darksafe.Resource{ID: "ledger-main", Scope: "org/payments/ledger"}
	allowed := 0
	for _, subject := range subjects {
		decision := darksafe.Access(subject, resource, "read")
		if decision.Allowed {
			allowed++
		}
		fmt.Printf("subject=%s allowed=%v reason=%s\n", subject.ID, decision.Allowed, decision.Reason)
	}
	fmt.Printf("summary: %d of %d subjects allowed\n", allowed, len(subjects))
}
