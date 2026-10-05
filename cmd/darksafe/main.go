// Command darksafe is the 零信任身份与授权决策平台 entry point.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// maxIngestLineBytes 是 ingest 单行内容的原始字节上限：64 MiB。
// 行分隔符 '\n' 不计入；内容恰好达到上限仍可进入写入或查询处理，只有超过才失败。
const maxIngestLineBytes = 64 * 1024 * 1024 // 67,108,864

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
	fmt.Fprintln(w, "  never written to disk. A single line may contain at most 67,108,864 raw")
	fmt.Fprintln(w, "  bytes (64 MiB); the line separator is not counted, so a line of exactly")
	fmt.Fprintln(w, "  that length is still processed. A longer line always fails as one whole")
	fmt.Fprintln(w, "  line: it is never split into multiple inputs and even a valid JSON prefix")
	fmt.Fprintln(w, "  is never treated as a complete request, so it writes no points and runs no")
	fmt.Fprintln(w, "  query. It still produces one error result, data committed by earlier lines")
	fmt.Fprintln(w, "  is retained, and later lines continue to be processed in input order.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Write line: a JSON array of sample objects; each line is one batch.")
	fmt.Fprintln(w, "  A sample is {\"name\": string, \"timestamp\": <int64 milliseconds>,")
	fmt.Fprintln(w, "  \"value\": <finite number>} and may optionally include")
	fmt.Fprintln(w, "  \"labels\": {\"key\": \"value\"}. Empty label values are preserved verbatim;")
	fmt.Fprintln(w, "  omitting labels and passing an empty labels object both mean no labels.")
	fmt.Fprintln(w, "  Missing fields, wrong types, unknown fields, duplicate keys, illegal")
	fmt.Fprintln(w, "  numbers, or input that is not a JSON array fail the whole batch.")
	fmt.Fprintln(w, "  Duplicate keys are compared after JSON unescaping, so a field written")
	fmt.Fprintln(w, "  literally and the same field written with its first letter as a")
	fmt.Fprintln(w, "  \\uXXXX escape collide even when both values are identical; the same")
	fmt.Fprintln(w, "  rule applies to label keys and to query objects. The check is scoped to")
	fmt.Fprintln(w, "  a single object: fields in different samples, and a sample field sharing")
	fmt.Fprintln(w, "  a name with one of its own label keys, are both legal. A write failure")
	fmt.Fprintln(w, "  names the offending sample via its 1-based index and never carries a")
	fmt.Fprintln(w, "  conflict; a query failure carries neither index nor conflict.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Every string (metric names, label keys and label values, in both write")
	fmt.Fprintln(w, "  arrays and query objects) must be valid text: the raw bytes must be valid")
	fmt.Fprintln(w, "  UTF-8, and a \\uXXXX Unicode escape must denote a real character. A lone")
	fmt.Fprintln(w, "  high surrogate, a lone low surrogate, or a high surrogate not followed")
	fmt.Fprintln(w, "  immediately by a valid low surrogate is rejected; it is never repaired into")
	fmt.Fprintln(w, "  U+FFFD. (A backslash-escaped as \\\\uD800 is ordinary text, not an escape.)")
	fmt.Fprintln(w, "  Chinese text, supplementary-plane characters written literally or as a")
	fmt.Fprintln(w, "  matched surrogate pair, and a deliberately typed U+FFFD or \\uFFFD are all")
	fmt.Fprintln(w, "  valid. Corrupt text fails the whole line as a parse error with no index and")
	fmt.Fprintln(w, "  no conflict even if earlier samples in the array were valid: no point from")
	fmt.Fprintln(w, "  that line is committed and a corrupt query returns no matches. The error")
	fmt.Fprintln(w, "  distinguishes invalid UTF-8 bytes from invalid surrogate escapes.")
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
	fmt.Fprintln(w, "  Point-listing line: same object shape with \"op\":\"query_points\". It uses")
	fmt.Fprintln(w, "  the same conditions and validation as a query but is read-only detail: each")
	fmt.Fprintln(w, "  matching series lists its raw stored samples in the range instead of count")
	fmt.Fprintln(w, "  and average. It never changes stored values or write counts.")
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
	fmt.Fprintln(w, "  query_points ok: {\"status\":\"ok\",\"op\":\"query_points\",\"series\":[{\"name\":...,")
	fmt.Fprintln(w, "           \"labels\":{},\"points\":[{\"timestamp\":N,\"value\":N.N}]}]} — one entry")
	fmt.Fprintln(w, "           per series having points in range, in the same series order, with the")
	fmt.Fprintln(w, "           full name and label set; points ascend by timestamp and carry the")
	fmt.Fprintln(w, "           stored float64 values, with no count or average attached; an empty")
	fmt.Fprintln(w, "           match yields [].")
	fmt.Fprintln(w, "  failure: {\"status\":\"error\",\"line\":N,\"index\":M,\"error\":\"...\",")
	fmt.Fprintln(w, "           \"conflict\":{...}} — index is the 1-based sample position and is")
	fmt.Fprintln(w, "           omitted when the whole line cannot be parsed or for query lines;")
	fmt.Fprintln(w, "           conflict reports the series, timestamp, existing and submitted values.")
	fmt.Fprintln(w, "           In the error text the series is rendered as name{k=v,...} with labels")
	fmt.Fprintln(w, "           sorted by key; a name, key or value containing commas, equals signs,")
	fmt.Fprintln(w, "           braces, quotes, backslashes, leading/trailing spaces or control")
	fmt.Fprintln(w, "           characters (or an empty string) is shown as a quoted string literal")
	fmt.Fprintln(w, "           (newlines as \\n, tabs as \\t), so content characters are never")
	fmt.Fprintln(w, "           mistaken for label boundaries and one reason stays one record.")
	fmt.Fprintln(w, "           An over-long line reports the size limit as a whole-line error with")
	fmt.Fprintln(w, "           no index and no conflict.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Processing continues after a failed line. The command exits non-zero if any")
	fmt.Fprintln(w, "  line failed, and zero when every line succeeded.")
}

// runIngest 逐行处理标准输入，逐行输出 JSON 结果。返回进程退出码。
func runIngest(in io.Reader, out io.Writer) int {
	store := darksafe.NewMetricStore()
	r := bufio.NewReaderSize(in, 64*1024)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	exitCode := 0
	lineNo := 0
	for {
		line, err := readIngestLine(r)
		if err == io.EOF {
			return exitCode
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "ingest: read error:", err)
			return 1
		}
		lineNo++
		if line.tooLong {
			// 超长行从开头到下一个行分隔符都属于同一条失败输入：
			// 内容已整体排空，不解析、不写入任何点、不执行任何查询。
			lineErr := &darksafe.LineError{
				Status: "error",
				Line:   lineNo,
				Error: fmt.Sprintf("line exceeds the %d-byte (64 MiB) single-line size limit",
					maxIngestLineBytes),
			}
			if err := enc.Encode(lineErr); err != nil {
				fmt.Fprintln(os.Stderr, "ingest: write error:", err)
				return 1
			}
			exitCode = 1
			continue
		}
		if len(strings.TrimSpace(line.text)) == 0 {
			continue // 空白行不产生结果，但已计入行号
		}
		result, lineErr := store.ProcessLine(line.text)
		if lineErr != nil {
			lineErr.Line = lineNo
			if err := enc.Encode(lineErr); err != nil {
				fmt.Fprintln(os.Stderr, "ingest: write error:", err)
				return 1
			}
			exitCode = 1
			continue
		}
		if err := enc.Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, "ingest: write error:", err)
			return 1
		}
	}
}

// ingestLine 是读到的一行内容，不含结尾的 '\n' 分隔符（分隔符不计入行大小）。
// tooLong 为 true 时表示该行内容超过 maxIngestLineBytes，text 不保留其内容；
// 该行从开头到下一个 '\n' 的全部字节都已被排空，不会拆成多条输入。
type ingestLine struct {
	text    string
	tooLong bool
}

// readIngestLine 读取下一行输入。沿用 bufio.Scanner 的换行识别方式：
// 以 '\n' 分隔，结尾的 '\n' 不属于行内容（"foo\r\n" 的内容仍为 "foo\r"，
// 因此空白判定与现有行为一致）。行内容不超过上限时累积到 text；
// 一旦超过上限，停止累积并继续排空到下一个 '\n' 或输入结束，返回 tooLong。
// 输入直接结束且没有任何字节时返回 io.EOF；超长行在末尾无换行结束时
// 同样作为一条失败行返回，下一次调用才返回 io.EOF，不会丢失或重复。
// 除 io.EOF 外的非空 error 均为底层输入读取故障，调用方应终止处理。
func readIngestLine(r *bufio.Reader) (ingestLine, error) {
	var b strings.Builder
	size := 0
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			if chunk[len(chunk)-1] == '\n' {
				chunk = chunk[:len(chunk)-1]
			}
			size += len(chunk)
			if size > maxIngestLineBytes {
				tooLong = true
				b = strings.Builder{} // 内容已注定失败，排空后续片段时不再保留此前累积
			}
			if !tooLong {
				b.Write(chunk)
			}
		}
		switch err {
		case nil:
			if tooLong {
				return ingestLine{tooLong: true}, nil
			}
			return ingestLine{text: b.String()}, nil
		case bufio.ErrBufferFull:
			// 行尚未结束且本次缓冲已耗尽，继续读取后续片段。
			continue
		case io.EOF:
			// 最后一行没有结尾换行；完全没有读到字节才表示输入结束，
			// 否则最后一行（含超长行）在本次返回，下次调用才得到 io.EOF。
			if size == 0 {
				return ingestLine{}, io.EOF
			}
			if tooLong {
				return ingestLine{tooLong: true}, nil
			}
			return ingestLine{text: b.String()}, nil
		default:
			return ingestLine{}, err
		}
	}
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
