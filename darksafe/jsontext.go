package darksafe

import (
	"fmt"
	"unicode/utf8"
)

// This file enforces the JSON text rule that encoding/json deliberately does
// not: a document's strings must carry the user's text verbatim, never a
// character substituted for broken input. encoding/json replaces two kinds of
// broken text with the Unicode replacement character U+FFFD and keeps going:
//
//   - a raw byte sequence that is not valid UTF-8 inside a JSON string, and
//   - a \uXXXX escape that is an unpaired UTF-16 surrogate: a high surrogate
//     (U+D800–U+DBFF) not immediately followed by a low surrogate
//     (U+DC00–U+DFFF), or a low surrogate not preceded by a high one.
//
// Both are rejected by the structural walk in plan.go, which visits every
// JSON value — member names and string values alike, in known fields and in
// unknown extra fields, however deeply their objects and arrays nest — so a
// text error is found before any business-field validation or release
// planning. A U+FFFD the user actually wrote (directly in valid UTF-8 or as
// the valid escape \uFFFD) is itself a legal character and is accepted;
// "\uD800" with the backslash itself escaped ("\\uD800") is ordinary literal
// text, not a Unicode escape, and is accepted as well.

// Text-error markers used by invalidTextError and checkJSONString.
const (
	invalidTextUTF8      = "utf8"
	invalidTextSurrogate = "surrogate"
)

// invalidTextError reports a JSON string whose decoded form cannot be trusted
// to preserve the user's text. path locates the string the same way
// duplicateMemberError does: "$.clusters[0].id" for a member value,
// "$.clusters[1]" for a string array element, and "$.clusters[0].tags" for a
// broken member name (reported against the object that owns the name).
type invalidTextError struct {
	kind string
	path string
}

func (e *invalidTextError) Error() string {
	switch e.kind {
	case invalidTextUTF8:
		return fmt.Sprintf("JSON 文本包含无效 UTF-8 字节: %s", e.path)
	default:
		return fmt.Sprintf("JSON 文本包含未配对的 Unicode 代理项转义 (\\uXXXX): %s", e.path)
	}
}

// rawTokenBytes bounds one token's exact bytes from decoder offsets. A
// Token() call consumes the insignificant text before the token as well —
// whitespace, and, depending on context, the single ',' separating an array
// element or the ':' separating a member name from its value — so those
// separator bytes are skipped to reach the token itself.
func rawTokenBytes(data []byte, start, end int64) []byte {
	i := int(start)
	for i < int(end) && isTokenSeparator(data[i]) {
		i++
	}
	return data[i:int(end)]
}

func isTokenSeparator(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == ',' || b == ':'
}

// checkJSONStringToken inspects one string token encoding/json has already
// accepted: raw is the token's exact bytes in the document (the surrounding
// double quotes included) and decoded is the string the decoder produced.
// It flags invalid raw UTF-8 and unpaired surrogate escapes. A genuinely
// written U+FFFD is indistinguishable from a substitution only after decoding,
// which is why the raw token — not the decoded string — is examined.
func checkJSONStringToken(raw []byte, _ string) *invalidTextError {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		// The token was read as a string, so its span must be quoted; if our
		// offset bookkeeping ever says otherwise, do not second-guess the
		// document on a hunch.
		return nil
	}
	body := raw[1 : len(raw)-1]
	if !utf8.Valid(body) {
		return &invalidTextError{kind: invalidTextUTF8}
	}
	if hasUnpairedSurrogate(body) {
		return &invalidTextError{kind: invalidTextSurrogate}
	}
	return nil
}

// hasUnpairedSurrogate scans the body of a raw JSON string token (quotes
// excluded) for a \uXXXX escape that is an unpaired UTF-16 surrogate. A high
// surrogate escape must be immediately followed by a low surrogate escape and
// a low must follow a high; anything in between — another escape, a literal
// character, end of string — leaves the surrogate unpaired. A doubled
// backslash is an escaped backslash ("\\"), so "\\uD800" leaves the seven
// ordinary characters u, D, 8, 0, 0 after consuming the paired backslashes
// and is not read as a Unicode escape.
func hasUnpairedSurrogate(body []byte) bool {
	pendingHigh := false
	for i := 0; i < len(body); {
		if body[i] != '\\' {
			if pendingHigh {
				return true // high surrogate was not immediately followed by its low
			}
			_, size := utf8.DecodeRune(body[i:])
			if size == 0 {
				size = 1
			}
			i += size
			continue
		}
		// body[i] is a backslash introducing (or, for "\\", pairing with) an
		// escape. A non-unicode escape consumes two bytes total; "\\uD800" is
		// therefore the two-byte escaped backslash plus literal "uD800".
		if i+1 >= len(body) {
			return pendingHigh
		}
		if body[i+1] != 'u' {
			if pendingHigh {
				return true
			}
			i += 2
			continue
		}
		r, ok := hex4Rune(body[i+2:])
		if !ok {
			// encoding/json already accepted this token, so the four hex
			// digits must be present; treat a malformed span defensively as a
			// non-surrogate escape rather than a false positive.
			if pendingHigh {
				return true
			}
			i += 2
			continue
		}
		switch {
		case isHighSurrogate(r):
			if pendingHigh {
				return true // the previous high never received its low
			}
			pendingHigh = true
		case isLowSurrogate(r):
			if !pendingHigh {
				return true // low surrogate without a preceding high
			}
			pendingHigh = false // a complete surrogate pair: legal
		default:
			if pendingHigh {
				return true
			}
		}
		i += 6 // backslash, 'u' and four hex digits
	}
	return pendingHigh
}

// hex4Rune decodes exactly four hexadecimal digits at s[0:4].
func hex4Rune(s []byte) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	var r rune
	for k := 0; k < 4; k++ {
		var d rune
		switch {
		case '0' <= s[k] && s[k] <= '9':
			d = rune(s[k] - '0')
		case 'a' <= s[k] && s[k] <= 'f':
			d = rune(s[k]-'a') + 10
		case 'A' <= s[k] && s[k] <= 'F':
			d = rune(s[k]-'A') + 10
		default:
			return 0, false
		}
		r = r<<4 | d
	}
	return r, true
}

func isHighSurrogate(r rune) bool { return 0xD800 <= r && r <= 0xDBFF }
func isLowSurrogate(r rune) bool  { return 0xDC00 <= r && r <= 0xDFFF }
