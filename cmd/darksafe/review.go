// The review subcommand re-evaluates one decision preserved in an audit
// archive after the service instance that decided it has ended. It never
// talks to a running service and never creates or restores a Store: the
// decision is recomputed purely from the saved record's request, the
// historical policy version carried in records before it, and a checkpoint
// the caller retained separately from the archive.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// Exit codes for the review command:
//
//   - 0: a full review was produced, including a legitimate archive whose
//     original and recomputed decisions disagree.
//   - 2: the invocation itself is wrong (missing/invalid arguments, the
//     archive file cannot be read).
//   - 1: the material or target cannot support a review (an invalid or
//     chain-broken archive, a missing or non-decision target, a historical
//     policy version that cannot be obtained).
const (
	reviewExitOK     = 0
	reviewExitFailed = 1
	reviewExitUsage  = 2
)

// reviewInputs holds everything the review command needs from the caller.
type reviewInputs struct {
	archivePath string
	org         string
	seq         int
	endSeq      int
	fingerprint string
	// json selects machine-readable JSON output instead of the default text
	// report. The review itself is identical either way: the same archive
	// validation and the same historical recheck, never a re-submitted
	// request.
	json bool
}

func reviewHelp(w io.Writer) {
	fmt.Fprint(w, `usage: darksafe review --archive FILE --org ORG --seq N \
                      --end-seq N --fingerprint HEX

Review one access decision saved in an audit archive, long after the
service instance that made the decision has ended. No running service is
contacted and no policy store is created or restored; the decision is
recomputed offline from the archived material alone. The archived decision
is never re-submitted as a new access request.

Required inputs:
  --archive FILE       path to the audit archive written by
                       EncodeAuditArchive (the complete export, not a
                       subject-filtered page or a fragment)
  --org ORG            organization name the archive belongs to; it must
                       match the organization bound into the records
  --seq N              positive sequence number of the decision record to
                       review (organization-local, starting at 1)
  --end-seq N          end sequence of the checkpoint retained separately
                       from the archive (0 for an organization with no
                       records); must not be negative
  --fingerprint HEX    checkpoint fingerprint retained separately from the
                       archive (64 hexadecimal SHA-256 characters)
  --json               emit the review as one complete JSON object on
                       stdout instead of the text report. The object gives
                       "target_sequence", "original" and "recomputed"
                       decisions (each with "allowed", "reason",
                       "matched_policies" in original order, and
                       "policy_version"), and "consistent". A valid UTF-8
                       string is a JSON string; a string that is not valid
                       UTF-8 is the object {"base64": "<standard base64 of
                       the exact bytes>"}, so a lone 0xFF, a lone 0xFE and a
                       real U+FFFD never collapse together and every string's
                       original bytes can be recovered. A nil and an empty
                       matched list both render as []. On any failure stdout
                       stays empty and the reason goes to stderr only.

The checkpoint carried inside the archive is informational only: validation
always uses --end-seq/--fingerprint supplied here. Even when --seq points at
an early record, every archived record is checked; a corrupted or missing
later record, or any mismatch with the retained checkpoint, fails the whole
review with no partial output.

The request is replayed strictly against the policy version the recorded
decision actually used; later publishes or rollbacks present in the archive
are never substituted. A chain that is valid but whose original and
recomputed decisions disagree is still printed in full and exits 0; the
disagreement is shown explicitly rather than reported as file damage.

Strings may contain spaces, control characters or non-UTF-8 bytes; the text
report uses Go-style quoting, while --json uses the JSON string /
{"base64": ...} rule described above, and both keep different raw bytes
distinguishable. This command only reads the named archive: it neither
rewrites it nor appends audit records.

Exit codes: 0 review complete (decisions may disagree), 1 archive or target
invalid / historical version unavailable, 2 bad arguments or unreadable
file. --json changes none of these.

Example:
  (materials produced by examples/offline_review; seq 1 is the
  policy publish record, seq 2 is the read decision being reviewed)
  darksafe review --archive acme-factory.audit \
      --org 'acme factory' --seq 2 --end-seq 2 \
      --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299

  add --json to get the same review as one JSON object:
  darksafe review --json --archive acme-factory.audit \
      --org 'acme factory' --seq 2 --end-seq 2 \
      --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299
`)
}

// parseReviewArgs parses review arguments. flags is an injectable args slice
// so tests can drive the command directly. The first element of args is the
// subcommand name ("review"). --help/-h/-help set wantHelp.
func parseReviewArgs(args []string) (in reviewInputs, wantHelp bool, err error) {
	haveSeq, haveEndSeq := false, false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		value := ""
		hasValue := false
		switch {
		case arg == "--help" || arg == "-h" || arg == "-help":
			return reviewInputs{}, true, nil
		case len(arg) > 2 && arg[:2] == "--":
			// Accept both --flag value and --flag=value.
			if eq := strings.IndexByte(arg, '='); eq >= 0 {
				value = arg[eq+1:]
				hasValue = true
				arg = arg[:eq]
			}
		default:
			return reviewInputs{}, false, fmt.Errorf("unexpected argument %q", arg)
		}

		take := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s requires a value", arg)
			}
			i++
			return args[i], nil
		}

		switch arg {
		case "--archive":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			in.archivePath = v
		case "--org":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			in.org = v
		case "--seq":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			n, perr := strconv.Atoi(v)
			if perr != nil {
				return reviewInputs{}, false, fmt.Errorf("--seq must be an integer, got %q", v)
			}
			in.seq = n
			haveSeq = true
		case "--end-seq":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			n, perr := strconv.Atoi(v)
			if perr != nil {
				return reviewInputs{}, false, fmt.Errorf("--end-seq must be an integer, got %q", v)
			}
			in.endSeq = n
			haveEndSeq = true
		case "--fingerprint":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			in.fingerprint = v
		case "--json":
			// Bare --json enables JSON output; --json=value accepts the
			// usual boolean spellings. No value is ever consumed from the
			// following token, so "--json --seq 2" keeps working.
			switch {
			case !hasValue:
				in.json = true
			default:
				b, perr := strconv.ParseBool(value)
				if perr != nil {
					return reviewInputs{}, false, fmt.Errorf("--json must be a boolean, got %q", value)
				}
				in.json = b
			}
		default:
			return reviewInputs{}, false, fmt.Errorf("unknown review flag %q", arg)
		}
	}

	if in.archivePath == "" {
		return reviewInputs{}, false, errors.New("--archive is required")
	}
	if in.org == "" {
		return reviewInputs{}, false, errors.New("--org is required")
	}
	if !haveSeq {
		return reviewInputs{}, false, errors.New("--seq is required")
	}
	if in.seq <= 0 {
		return reviewInputs{}, false, fmt.Errorf("--seq must be a positive integer, got %d", in.seq)
	}
	if !haveEndSeq {
		return reviewInputs{}, false, errors.New("--end-seq is required")
	}
	if in.endSeq < 0 {
		return reviewInputs{}, false, fmt.Errorf("--end-seq must not be negative, got %d", in.endSeq)
	}
	if in.fingerprint == "" {
		return reviewInputs{}, false, errors.New("--fingerprint is required")
	}
	return in, false, nil
}

// runReview executes one offline review and returns the process exit code.
// stdout/stderr are injectable so the command stays testable; args is the
// full argument vector including the leading "review".
func runReview(args []string, stdout, stderr io.Writer) int {
	in, wantHelp, err := parseReviewArgs(args)
	if wantHelp {
		reviewHelp(stdout)
		return reviewExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "review: %s\n", err)
		return reviewExitUsage
	}

	archive, err := os.ReadFile(in.archivePath)
	if err != nil {
		fmt.Fprintf(stderr, "review: cannot read archive %q: %v\n", in.archivePath, err)
		return reviewExitUsage
	}

	cp := darksafe.Checkpoint{Org: in.org, EndSeq: in.endSeq, Fingerprint: in.fingerprint}
	records, err := darksafe.DecodeAuditArchive(archive, in.org, cp)
	if err != nil {
		fmt.Fprintf(stderr, "review: archive validation failed: %v\n", err)
		return reviewExitFailed
	}
	review, err := darksafe.RecheckDecisionOffline(in.org, records, cp, in.seq)
	if err != nil {
		fmt.Fprintf(stderr, "review: %v\n", err)
		return reviewExitFailed
	}

	if in.json {
		// Render fully before touching stdout: a marshal failure must not
		// leave a partial object, and every earlier failure path already
		// returned with stdout untouched.
		payload, err := renderReviewJSON(review)
		if err != nil {
			fmt.Fprintf(stderr, "review: cannot encode result as JSON: %v\n", err)
			return reviewExitFailed
		}
		if _, err := stdout.Write(payload); err != nil {
			fmt.Fprintf(stderr, "review: cannot write JSON result: %v\n", err)
			return reviewExitFailed
		}
		return reviewExitOK
	}

	printReview(stdout, review)
	return reviewExitOK
}

// printReview renders the target sequence, both full decisions and the
// consistency verdict. Every potentially non-UTF-8 string is emitted with
// %q (Go quoting): a lone 0xFF renders as "\xff", 0xFE as "\xfe" and a real
// U+FFFD as "�", so distinct raw bytes never collapse onto one glyph.
func printReview(w io.Writer, r darksafe.OfflineDecisionReview) {
	fmt.Fprintf(w, "target sequence: %d\n", r.Seq)
	fmt.Fprintln(w, "original decision:")
	printDecision(w, r.Original)
	fmt.Fprintln(w, "recomputed decision:")
	printDecision(w, r.Recomputed)
	if r.Consistent {
		fmt.Fprintln(w, "consistent: yes")
	} else {
		fmt.Fprintln(w, "consistent: no (original and recomputed decisions differ)")
	}
}

func printDecision(w io.Writer, d darksafe.Decision) {
	fmt.Fprintf(w, "  allowed: %v\n", d.Allowed)
	fmt.Fprintf(w, "  reason: %q\n", d.Reason)
	fmt.Fprintf(w, "  matched policies: %s\n", formatMatched(d.Matched))
	fmt.Fprintf(w, "  policy version: %d\n", d.Version)
}

// formatMatched quotes each matched policy identifier byte-for-byte. A nil
// and an empty list mean the same set of hits but keep distinguishable
// spellings, mirroring the library's own nil-versus-empty preservation.
func formatMatched(matched []string) string {
	if matched == nil {
		return "[]"
	}
	out := "["
	for i, id := range matched {
		if i > 0 {
			out += ", "
		}
		out += strconv.Quote(id)
	}
	return out + "]"
}
