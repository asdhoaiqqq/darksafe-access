package darksafe

import (
	"fmt"
	"strings"
)

// This file holds the one JSON-string display rule shared by the two error
// texts that must render arbitrary user text recoverably: the JSONPath-style
// field location in configuration errors (duplicate members, invalid text —
// see jsonwalk.go) and the cluster-ID display in the all-rejected failure
// report (see rejectreport.go). Both displays show a name either as plain
// text or as a double-quoted JSON string whose decoding restores the
// original name byte for byte; the escaping inside the quoted form is the
// shared rule and lives here exactly once, so the two displays can never
// drift apart in how a character is escaped.
//
// The shared rule:
//   - The JSON short escapes keep their established spellings: \" \\ \b \f
//     \n \r \t.
//   - Every code point with a control effect is written as a real \uXXXX
//     JSON escape — the remaining C0 controls, DEL (U+007F) and the C1
//     controls (U+0080–U+009F), which encoding/json leaves raw — so the
//     display can neither act on a terminal nor hide part of a name. The
//     escape decodes back to the character itself and stays distinct from
//     the ordinary text of a backslash followed by "u007f"; no JSON-
//     undecodable \xNN form is ever produced.
//   - U+2028 and U+2029 are legal in JSON strings but are line separators
//     to many renderers, so they keep their \u2028/\u2029 spellings.
//   - Every other character — visible text of any script, including Chinese
//     and supplementary-plane characters — is written as itself.
//
// Exactly one choice remains display-specific and is the caller's: whether
// the visible characters '<', '>' and '&' are also \uXXXX-escaped. The
// field-location display passes escapeHTML=true to preserve encoding/json's
// HTML-safe spelling, so a location segment stays identical to the
// established rendering; the all-rejected report passes escapeHTML=false
// and shows those three characters as themselves, even in an ID that needs
// quoting for other characters.
//
// The input is always a Go string built from decoded, validated text, so it
// is valid UTF-8 and rune-range iteration visits exactly its code points.

// escapeJSONString writes s to b as a complete double-quoted JSON string
// under the shared rule above; decoding the written text restores s
// exactly. escapeHTML selects the one display-specific choice: whether
// '<', '>' and '&' are written as &/</> (true) or kept as
// the visible characters themselves (false).
func escapeJSONString(b *strings.Builder, s string, escapeHTML bool) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case escapeHTML && (r == '<' || r == '>' || r == '&'):
				// encoding/json's HTML-safe spelling, kept only by the
				// field-location display.
				fmt.Fprintf(b, `\u%04x`, r)
			case r < 0x20 || r == 0x7f || 0x80 <= r && r <= 0x9f ||
				r == 0x2028 || r == 0x2029:
				// A code point with a control effect (C0, DEL, C1 — exactly
				// the set unicode.IsControl reports) or a line separator:
				// always a real \uXXXX escape, never a raw character.
				fmt.Fprintf(b, `\u%04x`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jsonEncodePathString renders key as a legal JSON string (including the
// surrounding quotes) for a JSONPath-style field location; decoding the
// result restores key exactly, including empty names and names containing
// quotes, backslashes, newlines or other control characters. It is the
// shared escaping rule with the location display's own convention: '<',
// '>' and '&' keep encoding/json's HTML-safe \uXXXX spelling.
func jsonEncodePathString(key string) string {
	var b strings.Builder
	b.Grow(len(key) + 2)
	escapeJSONString(&b, key, true)
	return b.String()
}
