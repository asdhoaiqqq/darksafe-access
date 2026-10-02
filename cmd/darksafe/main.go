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

const version = "0.1.0"

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("darksafe " + version)
	case "ingest":
		os.Exit(runIngest(os.Stdin, os.Stdout, os.Stderr))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`usage: darksafe [demo|version|ingest|help]

commands:
  demo     run a sample access decision
  version  print the version
  ingest   read metric batches from standard input, one JSON array of points
           per line. Each point has:
             name       non-empty string
             timestamp  int64-range JSON integer in milliseconds
             value      JSON number representable as a finite float64
             labels     optional object of string to string; omitted and {}
                        both mean no labels; empty label values are preserved
           Results are written to standard output as one JSON object per input
           line, in input order. A successful result reports the number of new
           points, the number of duplicates, and a snapshot of every series
           with its points. A failed result reports the line number, the
           1-based point position (for point errors), and the reason; a value
           conflict additionally reports the series, timestamp, existing
           value, and submitted value. A failed batch applies none of its
           points, but earlier batches remain. The command exits non-zero if
           any batch failed.
  help     show this help
`)
}

// runIngest reads batches line by line. Blank lines are counted but produce
// no result. Every non-empty line yields exactly one JSON result line.
func runIngest(in io.Reader, out, errOut io.Writer) int {
	eng := darksafe.NewEngine()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)

	lineNo := 0
	failed := false
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		result := eng.IngestLine(line, lineNo)
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(errOut, "write error: %v\n", err)
			return 1
		}
		if !result.OK {
			failed = true
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(errOut, "read error: %v\n", err)
		return 1
	}
	if failed {
		return 1
	}
	return 0
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
