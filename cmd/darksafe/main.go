// Command darksafe is the 零信任身份与授权决策平台 entry point.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
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
	case "review":
		runReview(os.Args[2:])
	case "version":
		fmt.Println("darksafe 0.1.0")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`usage: darksafe [demo|review|version|help]

Commands:
  demo      run the built-in access-decision demonstration (default)
  review    re-check one audited access decision from a saved audit archive,
            after the service instance that produced it has ended
  version   print the version
  help      show this message

Run "darksafe review -h" for the review command's inputs and an example.`)
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

// reviewUsage describes every input the offline review needs and shows one
// complete invocation.
func reviewUsage() {
	fmt.Fprintln(os.Stderr, `usage: darksafe review -archive FILE -org ORG -seq N -end-seq M -fingerprint HEX

Re-check one audited access decision entirely offline, from a saved audit
archive. No service instance is contacted and no policy store is created or
restored; the review re-evaluates the request preserved in the decision
record against the policy version that record actually used, never a newer
published or rolled-back version. The command is read-only: it neither
rewrites the archive or checkpoint nor appends audit records.

Inputs (all required):
  -archive FILE
        path to the audit archive file saved when the service ended
        (produced by darksafe.EncodeAuditArchive)
  -org ORG
        organization name the archive belongs to; quote it if it
        contains spaces or control characters
  -seq N
        sequence number of the decision record to review (positive
        integer); the record must be a decision record, not a policy
        change
  -end-seq M
        end sequence of the separately retained checkpoint (zero or
        positive integer) that pins the exported records
  -fingerprint HEX
        fingerprint of the separately retained checkpoint

The checkpoint (-end-seq with -fingerprint) must be the one retained
independently of the archive; the checkpoint embedded in the archive is
informational only and never substitutes for it. The whole archive is
validated against the supplied checkpoint before any result is shown, so
corruption in any record, even after the target, fails the review.

Output: the target sequence, the original recorded decision and the
recomputed decision (allowance, reason, matched policies, policy version
each), and whether the two agree. Strings are printed quoted so spaces,
control characters and non-UTF-8 bytes stay distinguishable. A complete
review result, including an inconsistent one, exits 0; unreadable files,
missing or invalid inputs, archive validation failures, a missing or
non-decision target, and an unobtainable historical policy version print
the reason to stderr and exit 2.

Example:
  darksafe review -archive audit-acme.bin -org acme -seq 2 \
      -end-seq 4 -fingerprint 9b74c9897bac770ffc029102a200c5de`)
}

// failReview reports a review failure on stderr and exits with code 2.
func failReview(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "review: "+format+"\n", args...)
	os.Exit(2)
}

// parseReviewInt parses one required integer flag, reporting the flag name
// in the error so the failing input is identifiable.
func parseReviewInt(flagName, raw string) int {
	v, err := strconv.Atoi(raw)
	if err != nil {
		failReview("%s %q is not an integer", flagName, raw)
	}
	return v
}

// quoteList renders a string list with each element quoted, so entries
// carrying spaces, control characters or invalid UTF-8 bytes remain
// distinguishable from one another (a raw 0xFF prints as "\xff", a raw
// 0xFE as "\xfe", a genuine U+FFFD as "�").
func quoteList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("%s: allowed=%v reason=%s matched=%s version=%d\n",
		label, d.Allowed, strconv.Quote(d.Reason), quoteList(d.Matched), d.Version)
}

func runReview(args []string) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = reviewUsage
	archivePath := fs.String("archive", "", "path to the saved audit archive file")
	org := fs.String("org", "", "organization name the archive belongs to")
	seqRaw := fs.String("seq", "", "sequence number of the decision record to review")
	endSeqRaw := fs.String("end-seq", "", "end sequence of the separately retained checkpoint")
	fingerprint := fs.String("fingerprint", "", "fingerprint of the separately retained checkpoint")
	if err := fs.Parse(args); err != nil {
		// The parse error and usage were already written to stderr.
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		failReview("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}

	required := []struct {
		name  string
		value string
	}{
		{"archive", *archivePath},
		{"org", *org},
		{"seq", *seqRaw},
		{"end-seq", *endSeqRaw},
		{"fingerprint", *fingerprint},
	}
	missing := []string{}
	for _, input := range required {
		if input.value == "" {
			missing = append(missing, "-"+input.name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "review: missing required inputs: %s\n", strings.Join(missing, ", "))
		reviewUsage()
		os.Exit(2)
	}

	seq := parseReviewInt("-seq", *seqRaw)
	if seq < 1 {
		failReview("target sequence must be positive, got %d", seq)
	}
	endSeq := parseReviewInt("-end-seq", *endSeqRaw)
	if endSeq < 0 {
		failReview("checkpoint end sequence must not be negative, got %d", endSeq)
	}

	archive, err := os.ReadFile(*archivePath)
	if err != nil {
		failReview("cannot read archive %s: %v", strconv.Quote(*archivePath), err)
	}

	// The checkpoint comes exclusively from the command line; the archive's
	// embedded checkpoint is never consulted as evidence. Decoding validates
	// the complete material, including records after the target.
	cp := darksafe.Checkpoint{Org: *org, EndSeq: endSeq, Fingerprint: *fingerprint}
	records, err := darksafe.DecodeAuditArchive(archive, *org, cp)
	if err != nil {
		failReview("archive validation failed: %v", err)
	}
	review, err := darksafe.RecheckDecisionOffline(*org, records, cp, seq)
	if err != nil {
		failReview("%v", err)
	}

	fmt.Printf("target sequence: %d\n", review.Seq)
	printDecision("original decision ", review.Original)
	printDecision("recomputed decision", review.Recomputed)
	fmt.Printf("consistent: %v\n", review.Consistent)
}
