// Parsing for the independently saved checkpoint file consumed by
// `darksafe review --checkpoint`. The file is the small plain-text form the
// offline review example writes: one key=value item per line, keys org,
// end_seq and fingerprint each exactly once in any order, with one optional
// trailing newline. Nothing else is accepted: no additional fields, no
// blank or junk lines, no comments.
//
// The org value is a Go-style double-quoted string and is unquoted with
// strconv.Unquote, so the exact retained bytes are recovered: Chinese,
// leading and trailing spaces, escaped control characters and escaped
// non-UTF-8 bytes all survive, and distinct invalid bytes (a lone 0xFF and
// a lone 0xFE) never collapse onto one replacement glyph. end_seq is a
// non-negative decimal integer that fits the platform int (0 still means a
// checkpoint of an organization with no records); fingerprint is exactly 64
// hexadecimal characters.
//
// A parse failure is a usage-level failure (exit 2): the file is malformed
// evidence handling rather than a mismatch with a validly read archive.
package main

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// parseCheckpointData parses checkpoint file bytes. It takes the bytes
// rather than a path so read failures (reported separately by the caller)
// stay distinguishable from malformed contents.
func parseCheckpointData(data []byte) (darksafe.Checkpoint, error) {
	// Split on '\n' explicitly: no UTF-8 decoding or trimming is ever done,
	// so a byte that merely looks like whitespace cannot reshape the file.
	lines := bytes.Split(data, []byte{'\n'})
	// One trailing newline is permitted: the split then ends in one empty
	// element. Only that single final empty element is dropped; a blank line
	// anywhere else, including a second trailing newline, stays and fails.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}

	var cp darksafe.Checkpoint
	seen := make(map[string]int, 3) // key -> 1-based line of its first occurrence
	for i, line := range lines {
		lineNo := i + 1
		if len(line) == 0 {
			return darksafe.Checkpoint{}, fmt.Errorf("line %d: empty line", lineNo)
		}
		key, value, ok := bytes.Cut(line, []byte{'='})
		if !ok {
			return darksafe.Checkpoint{}, fmt.Errorf("line %d: expected key=value, got %q", lineNo, string(line))
		}
		field := string(key)
		if field != "org" && field != "end_seq" && field != "fingerprint" {
			return darksafe.Checkpoint{}, fmt.Errorf("line %d: unknown checkpoint field %q", lineNo, field)
		}
		if first, dup := seen[field]; dup {
			return darksafe.Checkpoint{}, fmt.Errorf("line %d: duplicate %s entry (first seen on line %d)", lineNo, field, first)
		}
		seen[field] = lineNo

		switch field {
		case "org":
			org, err := parseCheckpointOrg(value, lineNo)
			if err != nil {
				return darksafe.Checkpoint{}, err
			}
			cp.Org = org
		case "end_seq":
			n, err := parseCheckpointEndSeq(value, lineNo)
			if err != nil {
				return darksafe.Checkpoint{}, err
			}
			cp.EndSeq = n
		case "fingerprint":
			fp, err := parseCheckpointFingerprint(value, lineNo)
			if err != nil {
				return darksafe.Checkpoint{}, err
			}
			cp.Fingerprint = fp
		}
	}

	// Report missing items in a fixed order independent of the file's line
	// order, so the reason stays comparable across differently ordered files.
	for _, field := range []string{"org", "end_seq", "fingerprint"} {
		if _, ok := seen[field]; !ok {
			return darksafe.Checkpoint{}, fmt.Errorf("missing %s entry", field)
		}
	}
	return cp, nil
}

// parseCheckpointOrg decodes the Go-style double-quoted org value. The
// quote characters are required explicitly: strconv.Unquote alone would
// also accept backtick raw strings and single-quoted rune literals, neither
// of which is the documented format. On success the returned string holds
// the value's exact bytes, including invalid UTF-8 produced by byte
// escapes.
func parseCheckpointOrg(value []byte, lineNo int) (string, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("org entry on line %d: must be a Go-style double-quoted string, got %q",
			lineNo, string(value))
	}
	org, err := strconv.Unquote(string(value))
	if err != nil {
		return "", fmt.Errorf("org entry on line %d: invalid Go quoting: %v", lineNo, err)
	}
	if org == "" {
		return "", fmt.Errorf("org entry on line %d: organization is empty", lineNo)
	}
	return org, nil
}

// parseCheckpointEndSeq decodes a non-negative decimal integer and proves
// it fits the platform int before it can be used. Signs, spaces, underscore
// separators and any other non-digit byte are rejected outright; a leading
// zero does not change the value.
func parseCheckpointEndSeq(value []byte, lineNo int) (int, error) {
	if len(value) == 0 {
		return 0, fmt.Errorf("end_seq entry on line %d: value is empty", lineNo)
	}
	var n int
	for _, c := range value {
		d := int(c - '0')
		if d < 0 || d > 9 {
			return 0, fmt.Errorf("end_seq entry on line %d: must be a non-negative decimal integer, got %q",
				lineNo, string(value))
		}
		if n > (math.MaxInt-d)/10 {
			return 0, fmt.Errorf("end_seq entry on line %d: value %q is out of representable range",
				lineNo, string(value))
		}
		n = n*10 + d
	}
	return n, nil
}

// parseCheckpointFingerprint validates exactly 64 hexadecimal characters.
// Both upper and lower case are syntactically legal; the retained string is
// not normalized, so an uppercase spellout still compares byte-for-byte
// against the archive's lowercase fingerprint at validation time (exit 1,
// the material-mismatch family, rather than being silently rewritten).
func parseCheckpointFingerprint(value []byte, lineNo int) (string, error) {
	if len(value) != 64 {
		return "", fmt.Errorf("fingerprint entry on line %d: must be exactly 64 hexadecimal characters, got %d",
			lineNo, len(value))
	}
	for _, c := range value {
		hex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !hex {
			return "", fmt.Errorf("fingerprint entry on line %d: non-hexadecimal character %q",
				lineNo, string(c))
		}
	}
	return string(value), nil
}
