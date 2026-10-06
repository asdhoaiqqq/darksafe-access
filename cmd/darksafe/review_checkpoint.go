// Checkpoint-file reading for `darksafe review --checkpoint FILE`. The
// separately retained checkpoint is the same small plain-text document the
// offline_review example writes: one key=value item per line, three keys
// (org, end_seq, fingerprint) each exactly once, in any order, with one
// optional trailing newline.
//
// Byte fidelity matters most: the org line uses Go-style double quoting
// (the same spelling fmt.Sprintf("org=%q", ...) emits), and reading it
// back must restore the organization's ORIGINAL bytes — Chinese, leading
// and trailing spaces, escaped control characters and non-UTF-8 bytes
// (0xff and 0xfe written as \xff/\xfe, never folded onto U+FFFD).
//
// Every shape deviation is an invocation error the review command reports
// with exit code 2: the file cannot be read, a field is missing or
// repeated, an unknown key or non-empty stray line appears, the quoted
// organization is empty or mis-escaped, the end sequence is not a
// non-negative decimal integer the process's int type can represent, or
// the fingerprint is not 64 hexadecimal characters.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"unicode/utf8"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// readCheckpointFile reads and strictly parses one independently saved
// checkpoint document. A read failure names the offending file; a shape
// failure names the file and the specific line/item at fault.
func readCheckpointFile(path string) (darksafe.Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return darksafe.Checkpoint{}, fmt.Errorf("cannot read checkpoint %q: %v", path, err)
	}
	return parseCheckpointDocument(path, data)
}

// parseCheckpointDocument validates the document shape and decodes the
// three items. Splitting on '\n' means no item value can smuggle a newline
// across a line boundary; the single trailing newline the format allows is
// consumed by trimming exactly one '\n' from the tail — a second trailing
// newline becomes an empty line and fails like any other extra line.
func parseCheckpointDocument(path string, data []byte) (darksafe.Checkpoint, error) {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}

	var cp darksafe.Checkpoint
	var seenOrg, seenEndSeq, seenFingerprint bool
	for lineNo, rawLine := range bytes.Split(data, []byte{'\n'}) {
		lineNo := lineNo + 1
		eq := bytes.IndexByte(rawLine, '=')
		if eq <= 0 {
			// eq < 0: no '=' on the line; eq == 0: an empty key. Both are
			// non-empty miscellany (a wholly empty line too: the format's
			// only permitted emptiness is the one trailing newline already
			// trimmed above).
			return darksafe.Checkpoint{}, fmt.Errorf(
				"checkpoint %q line %d: expected one of org=..., end_seq=..., fingerprint=..., got %q",
				path, lineNo, rawLine)
		}
		key := string(rawLine[:eq])
		value := rawLine[eq+1:]

		switch key {
		case "org":
			if seenOrg {
				return darksafe.Checkpoint{}, fmt.Errorf(
					"checkpoint %q line %d: duplicate field org", path, lineNo)
			}
			seenOrg = true
			org, err := parseCheckpointOrg(path, lineNo, value)
			if err != nil {
				return darksafe.Checkpoint{}, err
			}
			cp.Org = org
		case "end_seq":
			if seenEndSeq {
				return darksafe.Checkpoint{}, fmt.Errorf(
					"checkpoint %q line %d: duplicate field end_seq", path, lineNo)
			}
			seenEndSeq = true
			n, err := parseCheckpointEndSeq(path, lineNo, value)
			if err != nil {
				return darksafe.Checkpoint{}, err
			}
			cp.EndSeq = n
		case "fingerprint":
			if seenFingerprint {
				return darksafe.Checkpoint{}, fmt.Errorf(
					"checkpoint %q line %d: duplicate field fingerprint", path, lineNo)
			}
			seenFingerprint = true
			if !isHexFingerprint(value) {
				return darksafe.Checkpoint{}, fmt.Errorf(
					"checkpoint %q line %d: invalid fingerprint %q: must be 64 hexadecimal characters",
					path, lineNo, value)
			}
			cp.Fingerprint = string(value)
		default:
			return darksafe.Checkpoint{}, fmt.Errorf(
				"checkpoint %q line %d: unknown field %q", path, lineNo, key)
		}
	}

	if !seenOrg {
		return darksafe.Checkpoint{}, fmt.Errorf("checkpoint %q: missing field org", path)
	}
	if !seenEndSeq {
		return darksafe.Checkpoint{}, fmt.Errorf("checkpoint %q: missing field end_seq", path)
	}
	if !seenFingerprint {
		return darksafe.Checkpoint{}, fmt.Errorf("checkpoint %q: missing field fingerprint", path)
	}
	return cp, nil
}

// parseCheckpointOrg decodes the Go-style double-quoted organization value
// back to its exact original bytes.
//
// strconv.Unquote alone is not sufficient: it also accepts single-quoted
// rune and back-quoted raw literals, and when the literal carries a raw
// byte that is not valid UTF-8 it replaces that byte with U+FFFD, which
// would fold genuinely different bytes (a lone 0xFF and a lone 0xFE) onto
// the same organization. The format pins the double-quoted shape and
// requires control characters and non-UTF-8 bytes to be ESCAPED (exactly
// what %q writes), so the literal between its quotes must be clean UTF-8
// with no raw control bytes; only then is Unquote allowed to interpret
// escapes — and escaped \x/\ooo sequences still restore any byte,
// including 0xFF and 0xFE, distinctly.
func parseCheckpointOrg(path string, lineNo int, value []byte) (string, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf(
			"checkpoint %q line %d: org must be a Go-style double-quoted string, got %q",
			path, lineNo, value)
	}
	inner := value[1 : len(value)-1]
	if !utf8.Valid(inner) || hasRawControl(inner) {
		return "", fmt.Errorf(
			"checkpoint %q line %d: org has a raw control character or non-UTF-8 byte; it must be Go-escaped (e.g. \\t, \\xff)",
			path, lineNo)
	}
	org, err := strconv.Unquote(string(value))
	if err != nil {
		return "", fmt.Errorf(
			"checkpoint %q line %d: invalid quoted org %q: %v", path, lineNo, value, err)
	}
	if org == "" {
		return "", fmt.Errorf("checkpoint %q line %d: org must not be empty", path, lineNo)
	}
	return org, nil
}

// hasRawControl reports whether b contains an unescaped ASCII control
// byte (C0 range or DEL); inside the quoted literal those are only legal
// in escaped form (\t, \x00, …).
func hasRawControl(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// parseCheckpointEndSeq decodes a non-negative decimal integer that fits in
// the process's int type (the same int Checkpoint.EndSeq and the archive
// framing use). A sign, a non-digit or a value beyond int range is a
// checkpoint format error rather than being truncated.
func parseCheckpointEndSeq(path string, lineNo int, value []byte) (int, error) {
	if len(value) == 0 {
		return 0, fmt.Errorf(
			"checkpoint %q line %d: invalid end_seq: must be a non-negative decimal integer",
			path, lineNo)
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf(
				"checkpoint %q line %d: invalid end_seq %q: must be a non-negative decimal integer",
				path, lineNo, value)
		}
	}
	n, err := strconv.Atoi(string(value))
	if err != nil {
		return 0, fmt.Errorf(
			"checkpoint %q line %d: end_seq %q is out of the representable range: %v",
			path, lineNo, value, err)
	}
	return n, nil
}

// isHexFingerprint reports whether value is exactly 64 hexadecimal
// characters (uppercase accepted as shape; archive comparison later
// rejects anything that does not match the chain).
func isHexFingerprint(value []byte) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
