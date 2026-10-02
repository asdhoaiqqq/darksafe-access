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
	fmt.Fprintln(w, "  ingest   batch-ingest metric samples and run range queries on standard input")
	fmt.Fprintln(w, "  help     show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "ingest input:")
	fmt.Fprintln(w, "  Standard input is read line by line. Blank lines produce no result but still")
	fmt.Fprintln(w, "  count toward line numbers. Accepted data is held in memory for this process")
	fmt.Fprintln(w, "  only and is never written to disk. Write batches and query objects may be")
	fmt.Fprintln(w, "  interleaved freely.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  A write line is a JSON array of sample objects; each line is one batch.")
	fmt.Fprintln(w, "  A sample is {\"name\": string, \"timestamp\": <int64 milliseconds>,")
	fmt.Fprintln(w, "  \"value\": <finite number>} and may optionally include")
	fmt.Fprintln(w, "  \"labels\": {\"key\": \"value\"}. Empty label values are preserved verbatim;")
	fmt.Fprintln(w, "  omitting labels and passing an empty labels object both mean no labels.")
	fmt.Fprintln(w, "  Missing fields, wrong types, unknown fields, illegal numbers, or input that is")
	fmt.Fprintln(w, "  not a JSON array fail the whole batch.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  A series is identified by metric name plus its full label set; label order does")
	fmt.Fprintln(w, "  not matter. Submitting the same series/timestamp with the same value is a")
	fmt.Fprintln(w, "  duplicate (1 and 1.0 are equal) and is ignored; a different value rejects the")
	fmt.Fprintln(w, "  entire batch without overwriting anything. Any failure in a batch discards all")
	fmt.Fprintln(w, "  new points from that line; earlier successful batches remain in effect.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  A query line is a JSON object: {\"op\":\"query\",\"name\":\"cpu\",")
	fmt.Fprintln(w, "  \"start\":1000,\"end\":2000,\"labels\":{\"host\":\"a\"}}. start and end are")
	fmt.Fprintln(w, "  int64 milliseconds and both endpoints are inclusive; start==end queries that")
	fmt.Fprintln(w, "  single timestamp, and start>end is rejected. name matches the metric name")
	fmt.Fprintln(w, "  exactly. labels are optional: omitted or empty matches every series of the")
	fmt.Fprintln(w, "  metric; given labels require those keys to exist with equal values while series")
	fmt.Fprintln(w, "  may carry extra labels (an empty string matches only an actual empty label, not")
	fmt.Fprintln(w, "  a missing one). Queries only read previously committed data and never mutate")
	fmt.Fprintln(w, "  the store; points from failed batches are not visible.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "ingest output (one JSON line per non-empty input line, in input order):")
	fmt.Fprintln(w, "  write success: {\"status\":\"ok\",\"added\":N,\"duplicates\":N,\"series\":[...]}")
	fmt.Fprintln(w, "           where every known series is listed with name, labels and timestamp-")
	fmt.Fprintln(w, "           sorted points; series are ordered by name and then by sorted labels.")
	fmt.Fprintln(w, "  query success: {\"status\":\"ok\",\"op\":\"query\",\"series\":[...]} where each")
	fmt.Fprintln(w, "           item has the full name and labels ({} when none), the count of points")
	fmt.Fprintln(w, "           in the inclusive range and their arithmetic average; only series with")
	fmt.Fprintln(w, "           at least one point in range are listed, in the same series order.")
	fmt.Fprintln(w, "  failure: {\"status\":\"error\",\"line\":N,\"index\":M,\"error\":\"...\",")
	fmt.Fprintln(w, "           \"conflict\":{...}} — index is the 1-based sample position and is")
	fmt.Fprintln(w, "           omitted for query lines and when the whole line cannot be parsed;")
	fmt.Fprintln(w, "           conflict reports the series, timestamp, existing and submitted values.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Processing continues after a failed line. The command exits non-zero if any")
	fmt.Fprintln(w, "  line failed, and zero when every line succeeded.")
}

// runIngest 逐行处理标准输入，逐行输出 JSON 结果。返回进程退出码。
func runIngest(in io.Reader, out io.Writer) int {
	store := darksafe.NewMetricStore()
	scanner := bufio.NewScanner(in)
	// 单行可能包含任意大的批次，提高缓冲上限。
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	exitCode := 0
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if len(strings.TrimSpace(line)) == 0 {
			continue // 空白行不产生结果，但已计入行号
		}
		result, lineErr := store.ProcessLine(line)
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
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "ingest: read error:", err)
		return 1
	}
	return exitCode
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
