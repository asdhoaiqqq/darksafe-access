package darksafe

import "strings"

// This file renders cluster IDs in the all-candidates-rejected failure
// report (see MakeReleasePlan in plan.go). The report is a line-oriented
// message: one "  <id>：<reason>" line per candidate. A legal ID is kept and
// compared exactly as written — identity rules live in clusterid.go — but a
// legal ID may itself contain characters that break that line-oriented
// display: a real newline or a line/paragraph separator splits one candidate
// into several lines, a carriage return rewrites the line it is on, and a
// tab or another control character produces a control effect. Rendering is
// therefore display-only: the original string is never trimmed, replaced,
// rejected, or used in escaped form for sorting or any identity comparison.
//
// An ID made solely of ordinary text is written verbatim, so the established
// line keeps its exact shape. An ID that contains a double quote, a
// backslash, a Unicode control character (Cc: U+0000–U+001F and
// U+007F–U+009F), or one of the separators U+2028/U+2029 is rendered as one
// complete double-quoted JSON string. Every control character and the two
// separators are escaped (\b, \t, \n, \f, \r or \uXXXX), so the rendered ID
// can never produce a real line break, carriage return, tab or any other
// control effect; quotes and backslashes are escaped as well, so the ID's
// own quote can never be mistaken for the wrapping quote. The rendered form
// is a valid JSON string and decodes back to the exact original ID — e.g.
// the ID c-a<newline>c-b renders as "c-a\nc-b" (one record), which stays
// visibly different from the ID c-a\nc-b (a backslash followed by n),
// rendered as "c-a\\nc-b". Ordinary visible characters, including Chinese,
// are written as themselves and stay directly readable.

// clusterIDForReport renders one candidate's exact ID for one line of the
// all-rejected failure report. Plain IDs pass through unchanged; any ID that
// needs protection is returned as one quoted, decodable JSON string.
func clusterIDForReport(id string) string {
	for _, r := range id {
		if r == '"' || r == '\\' || isReportControlRune(r) {
			return quoteClusterIDForReport(id)
		}
	}
	return id
}

// quoteClusterIDForReport writes id wrapped in double quotes with every
// double quote, backslash, control character and U+2028/U+2029 escaped.
// encoding/json already escapes the C0 controls (U+0000–U+001F) and the two
// separators, but it passes the C1 controls U+007F–U+009F through
// literally; those are control characters too (U+0085 is a line
// separator), so the escaping is done here for the full control range.
func quoteClusterIDForReport(id string) string {
	var b strings.Builder
	b.Grow(len(id) + 2)
	b.WriteByte('"')
	for _, r := range id {
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
			if isReportControlRune(r) {
				writeUnicodeEscape(&b, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// isReportControlRune reports whether r must be escaped inside a quoted ID:
// every Unicode control character of category Cc (U+0000–U+001F and
// U+007F–U+009F) and the line and paragraph separators U+2028/U+2029.
func isReportControlRune(r rune) bool {
	return r <= 0x1F || (r >= 0x7F && r <= 0x9F) || r == 0x2028 || r == 0x2029
}

// writeUnicodeEscape appends the six-character \uXXXX spelling of r. Every
// rune it is called for fits in four hex digits (C1 controls and the
// U+2028/U+2029 separators); the spelling matches encoding/json's lowercase
// hex style and decodes back to r.
func writeUnicodeEscape(b *strings.Builder, r rune) {
	const hex = "0123456789abcdef"
	b.WriteString(`\u`)
	b.WriteByte(hex[(r>>12)&0xF])
	b.WriteByte(hex[(r>>8)&0xF])
	b.WriteByte(hex[(r>>4)&0xF])
	b.WriteByte(hex[r&0xF])
}
