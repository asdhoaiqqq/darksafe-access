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
//   - 0: a full review was produced and the complete report was written
//     without error, including a legitimate archive whose original and
//     recomputed decisions disagree.
//   - 2: the invocation itself is wrong (missing/invalid arguments, the
//     archive file cannot be read).
//   - 1: the material or target cannot support a review (an invalid or
//     chain-broken archive, a missing or non-decision target, a historical
//     policy version that cannot be obtained), or the review report could
//     not be written out in full.
const (
	reviewExitOK     = 0
	reviewExitFailed = 1
	reviewExitUsage  = 2
)

// reviewInputs holds everything the review command needs from the caller.
//
// The checkpoint fields arrive in exactly one of two ways and never a mix:
// --org/--end-seq/--fingerprint typed on the command line, or one
// --checkpoint FILE that carries all three. parseReviewArgs rejects a
// combination of the two forms even when the repeated values are equal.
type reviewInputs struct {
	archivePath    string
	checkpointPath string
	org            string
	seq            int
	endSeq         int
	endSeqRaw      string
	fingerprint    string
	// json selects machine-readable JSON output instead of the default text
	// report. The review itself is identical either way: the same archive
	// validation and the same historical recheck, never a re-submitted
	// request.
	json bool
}

func reviewHelp(w io.Writer) {
	fmt.Fprint(w, `usage: darksafe review --archive FILE --seq N
                      ( --checkpoint FILE
                        | --org ORG --end-seq N --fingerprint HEX )

Review one access decision saved in an audit archive, long after the
service instance that made the decision has ended. No running service is
contacted and no policy store is created or restored; the decision is
recomputed offline from the archived material alone. The archived decision
is never re-submitted as a new access request.

Required inputs:
  --archive FILE       path to the audit archive written by
                       EncodeAuditArchive (the complete export, not a
                       subject-filtered page or a fragment)
  --seq N              positive sequence number of the decision record to
                       review (organization-local, starting at 1)

The retained checkpoint comes in exactly one of two forms:

  --checkpoint FILE    read the independently saved checkpoint from FILE
                       instead of typing its fields. The file is the small
                       text document produced by examples/offline_review:
                       one key=value item per line, each of org, end_seq
                       and fingerprint exactly once, lines in any order,
                       with one optional trailing newline; org is a Go-style
                       double-quoted string (escapes restore the original
                       bytes, including spaces, control characters and
                       non-UTF-8 bytes), end_seq is a non-negative decimal
                       integer (0 for an organization with no records), and
                       fingerprint is 64 hexadecimal characters.

  --org ORG            organization name the archive belongs to; it must
                       match the organization bound into the records
  --end-seq N          end sequence of the checkpoint retained separately
                       from the archive (0 for an organization with no
                       records); must not be negative
  --fingerprint HEX    checkpoint fingerprint retained separately from the
                       archive (64 hexadecimal SHA-256 characters)

  --checkpoint is mutually exclusive with --org, --end-seq and
  --fingerprint: naming --checkpoint together with any of those flags, in
  either order and even with the same value, is an argument conflict and
  fails as a usage error rather than one input overriding the other.

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
                       matched list both render as []. A failure before the
                       report is written leaves stdout empty and the reason on
                       stderr only; if stdout refuses the report or accepts
                       only part of it, any accepted prefix stays in place,
                       the command exits 1 and the output failure is reported
                       on stderr, never retried in another format.

The checkpoint carried inside the archive is informational only: validation
always uses the retained checkpoint, whether it is supplied via
--checkpoint or via --org/--end-seq/--fingerprint. Even when --seq points
at an early record, every archived record is checked; a corrupted or
missing later record, or any mismatch with the retained checkpoint, fails
the whole review with no partial output. A well-formed --checkpoint file
whose organization or checkpoint does not match the archive is a material
failure (exit 1), never an occasion to fall back to the checkpoint embedded
in the archive.

The request is replayed strictly against the policy version the recorded
decision actually used; later publishes or rollbacks present in the archive
are never substituted. A chain that is valid but whose original and
recomputed decisions disagree is still printed in full and exits 0; the
disagreement is shown explicitly rather than reported as file damage.

Strings may contain spaces, control characters or non-UTF-8 bytes; the text
report uses Go-style quoting, while --json uses the JSON string /
{"base64": ...} rule described above, and both keep different raw bytes
distinguishable. This command only reads the named archive and checkpoint:
it neither rewrites either file nor appends audit records.

Exit codes: 0 complete report written without error (decisions may
disagree), 1 archive or target invalid / checkpoint does not match the
archive / historical version unavailable / review report could not be
written out in full, 2 bad arguments (including a missing, unreadable or
malformed --checkpoint file). --json changes none of these.

Examples:
  (materials produced by examples/offline_review; seq 1 is the
  policy publish record, seq 2 is the read decision being reviewed)

  checkpoint typed on the command line:
  darksafe review --archive acme-factory.audit \
      --org 'acme factory' --seq 2 --end-seq 2 \
      --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299

  same review with the independently saved checkpoint file:
  darksafe review --archive acme-factory.audit --seq 2 \
      --checkpoint acme-factory.checkpoint

  add --json to get the same review as one JSON object:
  darksafe review --json --archive acme-factory.audit --seq 2 \
      --checkpoint acme-factory.checkpoint
`)
}

// parseReviewArgs parses review arguments. flags is an injectable args slice
// so tests can drive the command directly. The first element of args is the
// subcommand name ("review"). --help/-h/-help set wantHelp.
func parseReviewArgs(args []string) (in reviewInputs, wantHelp bool, err error) {
	haveSeq, haveEndSeq := false, false
	haveOrg, haveFingerprint, haveCheckpoint := false, false, false
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
		case "--checkpoint":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			if haveCheckpoint {
				return reviewInputs{}, false, errors.New("--checkpoint given more than once")
			}
			in.checkpointPath = v
			haveCheckpoint = true
		case "--org":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			in.org = v
			haveOrg = true
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
			// Only record presence and the raw token here. The conflict is
			// structural and order-independent, so a --checkpoint appearing
			// later must still reject this flag; parsing the integer is
			// therefore deferred until the mode is known after the loop.
			in.endSeqRaw = v
			haveEndSeq = true
		case "--fingerprint":
			v, err := take()
			if err != nil {
				return reviewInputs{}, false, err
			}
			in.fingerprint = v
			haveFingerprint = true
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
	// One structural conflict check after the whole command line has been
	// seen, so the verdict never depends on which flag came first nor on
	// whether the repeated values are equal.
	if haveCheckpoint && (haveOrg || haveEndSeq || haveFingerprint) {
		return reviewInputs{}, false, errCheckpointConflict
	}
	if !haveSeq {
		return reviewInputs{}, false, errors.New("--seq is required")
	}
	if in.seq <= 0 {
		return reviewInputs{}, false, fmt.Errorf("--seq must be a positive integer, got %d", in.seq)
	}
	if haveCheckpoint {
		// In file mode the other three checkpoint fields are never taken
		// from the command line — the conflict check above just ruled that
		// out — and their absence is required, not an error.
		if in.checkpointPath == "" {
			return reviewInputs{}, false, errors.New("--checkpoint requires a file path")
		}
		return in, false, nil
	}
	if in.org == "" {
		return reviewInputs{}, false, errors.New("--org is required (or supply --checkpoint FILE)")
	}
	if !haveEndSeq {
		return reviewInputs{}, false, errors.New("--end-seq is required (or supply --checkpoint FILE)")
	}
	// Parsed only now, in flag mode: in file mode a malformed token would
	// already have failed as the structural conflict above.
	n, perr := strconv.Atoi(in.endSeqRaw)
	if perr != nil {
		return reviewInputs{}, false, fmt.Errorf("--end-seq must be an integer, got %q", in.endSeqRaw)
	}
	in.endSeq = n
	if in.endSeq < 0 {
		return reviewInputs{}, false, fmt.Errorf("--end-seq must not be negative, got %d", in.endSeq)
	}
	if in.fingerprint == "" {
		return reviewInputs{}, false, errors.New("--fingerprint is required (or supply --checkpoint FILE)")
	}
	return in, false, nil
}

// errCheckpointConflict is returned whenever --checkpoint appears together
// with --org, --end-seq or --fingerprint. The conflict never depends on
// the values (equal values still conflict) nor on argument order.
var errCheckpointConflict = errors.New(
	"--checkpoint is mutually exclusive with --org, --end-seq and --fingerprint; " +
		"supply the checkpoint either as a file or as those three flags, not both")

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

	// The retained checkpoint either arrives whole from --checkpoint FILE
	// or field by field from the command line; the parser made the two
	// forms mutually exclusive, so a missing/unreadable/malformed file is
	// reported here before the archive is even opened.
	cp := darksafe.Checkpoint{Org: in.org, EndSeq: in.endSeq, Fingerprint: in.fingerprint}
	if in.checkpointPath != "" {
		fileCP, err := readCheckpointFile(in.checkpointPath)
		if err != nil {
			fmt.Fprintf(stderr, "review: %s\n", err)
			return reviewExitUsage
		}
		cp = fileCP
	}

	archive, err := os.ReadFile(in.archivePath)
	if err != nil {
		fmt.Fprintf(stderr, "review: cannot read archive %q: %v\n", in.archivePath, err)
		return reviewExitUsage
	}

	// Decode and chain validation are one stage: the archive is checked
	// against the retained checkpoint exactly once here, and the review
	// below replays the target from that already-verified material rather
	// than walking the whole chain a second time. With --checkpoint the
	// file's organization and checkpoint are authoritative; the checkpoint
	// embedded in the archive is never allowed to stand in for them.
	material, err := darksafe.DecodeVerifiedAuditArchive(archive, cp.Org, cp)
	if err != nil {
		fmt.Fprintf(stderr, "review: archive validation failed: %v\n", err)
		return reviewExitFailed
	}
	review, err := darksafe.RecheckVerifiedDecisionOffline(material, in.seq)
	if err != nil {
		fmt.Fprintf(stderr, "review: %v\n", err)
		return reviewExitFailed
	}

	// Render both formats fully before touching stdout: a render failure must
	// not leave partial output, and every earlier failure path already
	// returned with stdout untouched.
	var payload []byte
	if in.json {
		payload, err = renderReviewJSON(review)
		if err != nil {
			fmt.Fprintf(stderr, "review: cannot encode result as JSON: %v\n", err)
			return reviewExitFailed
		}
	} else {
		var buf strings.Builder
		printReview(&buf, review)
		payload = []byte(buf.String())
	}

	// Success requires the WHOLE report to actually land. A receiver may
	// accept some bytes and then refuse the rest, or report a short write
	// with no error; neither may be mistaken for a completed review merely
	// because the archive validated and the decision recomputed. Whatever
	// prefix the receiver already accepted stays there (it cannot be
	// unwritten), but nothing more is appended afterwards.
	n, werr := stdout.Write(payload)
	if werr != nil {
		fmt.Fprintf(stderr, "review: failed to write review report: %v\n", werr)
		return reviewExitFailed
	}
	if n < len(payload) {
		fmt.Fprintf(stderr,
			"review: review report output incomplete: wrote %d of %d bytes\n",
			n, len(payload))
		return reviewExitFailed
	}
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
