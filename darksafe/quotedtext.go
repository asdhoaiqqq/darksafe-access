package darksafe

import (
	"fmt"
	"strings"
	"unicode"
)

// This file holds the single display rule shared by the two kinds of error
// text MakeReleasePlan and its parsing layer produce:
//
//   - The all-rejected candidate report (rejectreport.go) shows a cluster ID
//     on a fixed one-line record. A "normal" ID is shown unchanged; only an
//     ID that would break the line layout or be mistaken for quoting gets
//     wrapped in a JSON string.
//   - A configuration error's field location (jsonwalk.go) shows every
//     non-simple member name inside brackets around a JSON string, so its
//     symbols can never be read as path hierarchy or an array index.
//
// Both displays must let a reader recover the original text exactly, must
// never emit a real control effect (a line break, tab or other control
// character that would act on the terminal), and must not fabricate a
// JSON-undecodable escape: a real newline shows as the two-character \n
// spelling while ordinary text made of a backslash and the letter n shows
// as \\n, so the two never merge into one identity. Those rules live here
// once; each display keeps only its own quoting policy and the one spelling
// difference it is allowed to make for the visible characters less-than,
// greater-than and ampersand.
//
// The encoders do not use encoding/json because its HTMLEscape (and the
// raw DEL/C1 output) would decide the less-than/greater-than/ampersand
// spelling for both displays at once; the shared rule keeps that decision
// per caller while escaping every code point with a control effect as
// \uXXXX, a real JSON escape that decodes back to the rune itself and is
// distinct from ordinary text containing "\u007f". No \xNN form is ever
// produced, since JSON cannot decode one. The input is a Go string — a
// decoded member name or a candidate's exact ID — so it is always valid
// UTF-8; rune-range iteration therefore visits exactly its code points.

// quoteStyle selects how the shared encoder renders one piece of text.
// Everything in the escaping rule is identical between the two displays;
// the style carries only the quoting policy and their single allowed
// spelling difference (the visible characters less-than, greater-than and
// ampersand).
type quoteStyle struct {
	// escapeHTMLSigns, true for a field location, writes the three visible
	// characters less-than, greater-than and ampersand as four-hex Unicode
	// escapes: the HTML-safe spelling that encoding/json produces with
	// HTMLEscape on. The candidate report leaves those three characters raw.
	escapeHTMLSigns bool
	// alwaysQuote forces the surrounding double quotes even for ordinary
	// text. Field locations always quote (they are embedded inside
	// brackets); the candidate report only quotes text needing escapes.
	alwaysQuote bool
}

var (
	// quoteReportText is the candidate-report style: conditional quoting
	// (ordinary visible text stays directly readable), and the three visible
	// characters less-than, greater-than and ampersand are kept as-is even
	// when another character forces the quotes on.
	quoteReportText = quoteStyle{escapeHTMLSigns: false, alwaysQuote: false}

	// quotePathText is the field-location style: always wrapped in a JSON
	// string (the segment sits inside [...]), and less-than/greater-than/
	// ampersand keep their established HTML-safe \uXXXX spellings.
	quotePathText = quoteStyle{escapeHTMLSigns: true, alwaysQuote: true}
)

// appendJSONString writes s as a JSON string body between — but not
// including — the surrounding quotes, following the shared rule:
//   - the JSON short escapes keep their established spellings:
//     ", \, \b, \f, \n, \r, \t;
//   - every code point with a control effect is written as a real \uXXXX
//     JSON escape, including DEL (U+007F) and the C1 controls
//     (U+0080–U+009F), which encoding/json leaves raw, as well as the
//     U+2028/U+2029 line/paragraph separators;
//   - less-than, greater-than and ampersand get the same treatment only
//     when st.escapeHTMLSigns is set;
//   - every other code point — visible text of any script, including
//     Chinese, emoji and the rest — is written directly.
//
// The spelling decodes back to s byte for byte: a backslash in the original
// text shows as \\ and a real control rune as \uXXXX, so a displayed string
// can never hide extra hierarchy or merge two originals.
func (st quoteStyle) appendJSONString(b *strings.Builder, s string) {
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
			case st.escapeHTMLSigns && (r == '<' || r == '>' || r == '&'):
				// Keep the HTML-safe spelling for field locations; the
				// candidate report leaves these three characters visible.
				fallthrough
			case r < 0x20, r == 0x7f, 0x80 <= r && r <= 0x9f,
				r == 0x2028, r == 0x2029:
				// Every code point with a control effect is written as a real
				// \uXXXX JSON escape — including DEL and the C1 controls,
				// which encoding/json leaves raw — so the text can neither
				// act on the terminal nor hide part of the name. The
				// spelling decodes back to the character itself and is
				// distinct from ordinary text containing "\u007f"; no
				// JSON-illegal \xNN form is ever emitted.
				fmt.Fprintf(b, `\u%04x`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
}

// render writes s under the style's quoting policy: always wrapped in a
// double-quoted JSON string for field locations, or — for the candidate
// report — returned unchanged when it has no character the shared rule
// escapes and no character that could be mistaken for the quoting itself.
func (st quoteStyle) render(s string) string {
	if !st.alwaysQuote && !needsJSONQuoting(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	st.appendJSONString(&b, s)
	b.WriteByte('"')
	return b.String()
}

// needsJSONQuoting reports whether s contains any character the shared
// escaping rule rewrites or any character that could be mistaken for the
// quoting the display adds: a double quote, a backslash, a Unicode control
// character (covering the C0 controls, DEL and the C1 controls), or
// U+2028/U+2029. Anything else — visible text of any script, including
// less-than, greater-than and ampersand — stays plain.
func needsJSONQuoting(s string) bool {
	for _, r := range s {
		if r == '"' || r == '\\' || r == '\u2028' || r == '\u2029' ||
			unicode.IsControl(r) {
			return true
		}
	}
	return false
}
