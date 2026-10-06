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
	"unicode/utf8"

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
	// jsonOutput selects the machine-readable single-JSON-object report on
	// stdout instead of the default human-readable text report.
	jsonOutput bool
}

func reviewHelp(w io.Writer) {
	fmt.Fprint(w, `usage: darksafe review --archive FILE --org ORG --seq N \
                      --end-seq N --fingerprint HEX [--json]

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

Optional:
  --json[=true|false]  emit the report as one complete JSON object on
                       stdout and nothing else (no title, prose or text
                       report). Without --json the default text report is
                       printed, byte-for-byte as before. The bare flag
                       means true; --json=false restores the text report.

The checkpoint carried inside the archive is informational only: validation
always uses --end-seq/--fingerprint supplied here. Even when --seq points at
an early record, every archived record is checked; a corrupted or missing
later record, or any mismatch with the retained checkpoint, fails the whole
review with no partial output (in --json mode stdout stays empty and the
reason goes to stderr only).

The request is replayed strictly against the policy version the recorded
decision actually used; later publishes or rollbacks present in the archive
are never substituted. A chain that is valid but whose original and
recomputed decisions disagree is still printed in full and exits 0; the
disagreement is shown explicitly ("consistent": false) rather than reported
as file damage. A nil matched-policy list and an empty one are both encoded
as [] and remain the same decision for consistency purposes.

JSON report shape (field meanings mirror the text report one-for-one):
  {
    "target_seq": <integer>,
    "original":   {"allowed": <bool>, "reason": "...",
                   "matched_policies": ["..."], "policy_version": <int>},
    "recomputed": {"allowed": <bool>, "reason": "...",
                   "matched_policies": ["..."], "policy_version": <int>},
    "consistent": <bool>
  }
target_seq is the audited record's sequence; policy_version is separate,
inside each decision. matched_policies keeps its original order; it is []
for both a nil and an empty hit list.

String representation in JSON: every string is a JSON string whose decoded
bytes reproduce the original string byte-for-byte. Ordinary printable
Unicode (including Chinese) is written literally and stays readable;
double quote, backslash and control characters use standard JSON escapes
(\" \\ \n \r \t \b \f and \u00XX for other control bytes); bytes that are
not valid UTF-8 are each emitted as \u00XX carrying that exact byte value,
so a single 0xFF appears as \u00FF, 0xFE as \u00FE and a genuine
U+FFFD character as the three UTF-8 bytes EF BF BD — the three never
collapse together. A standard JSON parser therefore recovers, per string,
exactly the bytes the report held. Control characters can never break the
object boundary.

Strings may contain spaces, control characters or non-UTF-8 bytes; both the
text and JSON output keep different raw bytes distinguishable. This command
only reads the named archive: it neither rewrites it nor appends audit
records, regardless of the selected output format.

Exit codes: 0 review complete (decisions may disagree), 1 archive or target
invalid / historical version unavailable, 2 bad arguments or unreadable
file. In --json mode a non-zero exit still leaves stdout completely empty.

Example:
  (materials produced by examples/offline_review; seq 1 is the
  policy publish record, seq 2 is the read decision being reviewed)
  darksafe review --archive acme-factory.audit \
      --org 'acme factory' --seq 2 --end-seq 2 \
      --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299

  darksafe review --archive acme-factory.audit \
      --org 'acme factory' --seq 2 --end-seq 2 \
      --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299 \
      --json
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
		case arg == "--json":
			// Bare boolean flag. --json=true/false is handled below through
			// the --flag=value path.
			in.jsonOutput = true
			continue
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
			// Reached only via --json=value (the bare flag was consumed
			// above). Parse strictly so a typo cannot silently select JSON.
			switch value {
			case "true", "1":
				in.jsonOutput = true
			case "false", "0":
				in.jsonOutput = false
			default:
				return reviewInputs{}, false, fmt.Errorf("--json must be true or false, got %q", value)
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

	if in.jsonOutput {
		// Build the whole object before touching stdout so a rendering
		// surprise can never deliver a partial JSON document; every failure
		// path above has already returned without writing to stdout.
		var buf strings.Builder
		writeReviewJSON(&buf, review)
		buf.WriteByte('\n')
		fmt.Fprint(stdout, buf.String())
		return reviewExitOK
	}

	printReview(stdout, review)
	return reviewExitOK
}

// writeReviewJSON renders the review as exactly one JSON object. Field
// meanings mirror the text report: "target_seq" is the audited record's
// sequence (kept separate from each decision's "policy_version"), each
// decision carries "allowed", "reason", "matched_policies" and
// "policy_version", and "consistent" is the explicit agreement verdict.
// matched_policies is written in its recorded order and is [] for both a
// nil and an empty hit list, matching the library's nil/empty equivalence.
func writeReviewJSON(buf *strings.Builder, r darksafe.OfflineDecisionReview) {
	buf.WriteString(`{"target_seq":`)
	buf.WriteString(strconv.Itoa(r.Seq))
	buf.WriteString(`,"original":`)
	writeDecisionJSON(buf, r.Original)
	buf.WriteString(`,"recomputed":`)
	writeDecisionJSON(buf, r.Recomputed)
	buf.WriteString(`,"consistent":`)
	if r.Consistent {
		buf.WriteString("true")
	} else {
		buf.WriteString("false")
	}
	buf.WriteByte('}')
}

func writeDecisionJSON(buf *strings.Builder, d darksafe.Decision) {
	buf.WriteString(`{"allowed":`)
	if d.Allowed {
		buf.WriteString("true")
	} else {
		buf.WriteString("false")
	}
	buf.WriteString(`,"reason":`)
	writeJSONString(buf, d.Reason)
	buf.WriteString(`,"matched_policies":[`)
	for i, id := range d.Matched { // original order, no sorting or dedup
		if i > 0 {
			buf.WriteByte(',')
		}
		writeJSONString(buf, id)
	}
	buf.WriteString(`],"policy_version":`)
	buf.WriteString(strconv.Itoa(d.Version))
	buf.WriteByte('}')
}

// jsonHex is the lowercase hexadecimal alphabet used in \uXXXX escapes.
const jsonHex = "0123456789abcdef"

// writeJSONString appends s as a JSON string literal whose decoded bytes
// reproduce s byte-for-byte, even when s is not valid UTF-8:
//
//   - Valid printable UTF-8 runes (ordinary text, including Chinese) are
//     emitted literally, so readable content stays readable.
//   - '"' and '\\' use \" and \\\\; the JSON short escapes \b \f \n \r \t
//     cover their control bytes; every other ASCII control byte (< 0x20)
//     uses \u00XX, so a control character can never leave the string or
//     break the object boundary.
//   - Each byte that is not valid UTF-8 on its own is emitted as \u00XX
//     carrying that exact byte value. Decoding a JSON \u00XX escape yields
//     U+00XX, which UTF-8 encodes back to the single byte 0xXX, so the
//     receiver restores the exact original byte: a lone 0xFF (\u00FF)
//     and a lone 0xFE (\u00FE) never collapse onto a genuine U+FFFD
//     (emitted literally as the three bytes EF BF BD).
func writeJSONString(buf *strings.Builder, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		b := s[i]
		switch {
		case b == '"':
			buf.WriteString(`\"`)
			i++
		case b == '\\':
			buf.WriteString(`\\`)
			i++
		case b == '\n':
			buf.WriteString(`\n`)
			i++
		case b == '\r':
			buf.WriteString(`\r`)
			i++
		case b == '\t':
			buf.WriteString(`\t`)
			i++
		case b == '\b':
			buf.WriteString(`\b`)
			i++
		case b == '\f':
			buf.WriteString(`\f`)
			i++
		case b < 0x20:
			// Any other ASCII control byte: explicit \u00XX.
			buf.WriteString(`\u00`)
			buf.WriteByte(jsonHex[b>>4])
			buf.WriteByte(jsonHex[b&0x0f])
			i++
		case b < 0x80:
			// Ordinary printable ASCII byte.
			buf.WriteByte(b)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				// One byte that is not valid UTF-8. Encode its value as
				// \u00XX so the exact byte round-trips and stays distinct
				// from a real U+FFFD written elsewhere in the string.
				buf.WriteString(`\u00`)
				buf.WriteByte(jsonHex[b>>4])
				buf.WriteByte(jsonHex[b&0x0f])
				i++
				continue
			}
			// A whole valid rune (multi-byte UTF-8) stays literal: a real
			// U+FFFD is its three bytes EF BF BD, never �.
			buf.WriteString(s[i : i+size])
			i += size
		}
	}
	buf.WriteByte('"')
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
