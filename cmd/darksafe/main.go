// Command darksafe is the 零信任身份与授权决策平台 entry point.
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(dispatch(os.Args, os.Stdout, os.Stderr))
}

// dispatch selects one subcommand from the full argument vector
// (args[0] is the program name). With no arguments it runs the demo,
// preserving the original no-argument behavior.
func dispatch(args []string, stdout, stderr io.Writer) int {
	command := "demo"
	if len(args) > 1 {
		command = args[1]
	}
	switch command {
	case "review":
		return runReview(args[1:], stdout, stderr)
	case "demo":
		runDemo(stdout)
		return 0
	case "version":
		fmt.Fprintln(stdout, "darksafe 0.1.0")
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: darksafe [demo|version|review|help]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  demo      run the built-in no-argument demonstration (default)")
	fmt.Fprintln(w, "  version   print the version")
	fmt.Fprintln(w, "  review    review one archived access decision offline;")
	fmt.Fprintln(w, "            run `darksafe review --help` for its arguments and an example")
	fmt.Fprintln(w, "  help      show this help")
}
