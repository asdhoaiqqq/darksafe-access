package darksafe

import (
	"strings"
	"unicode"
)

// This file holds the display rule for cluster IDs in the all-rejected
// failure report of MakeReleasePlan. The report is a line-based list —
// "  <id>：<reason>" per candidate — so one rendered record must always
// occupy exactly one line and must let the reader recover the original ID
// exactly. A cluster ID is legal with almost any character in it (only
// empty or whitespace-only IDs are rejected, and valid UTF-8 is required);
// an ID containing a real newline or carriage return would otherwise spill
// one candidate across several displayed records or rewrite the current
// line, making it impossible to tell which cluster each reason belongs to.
//
// The rule is display-only: an ID that contains a double quote, a
// backslash, a Unicode control character, or the U+2028/U+2029 separators
// is rendered as a double-quoted JSON string under the shared escaping rule
// (jsonstring.go), so the report never gains a real line break, tab or
// other control effect and decoding the quoted text restores the original
// ID byte for byte. This display keeps the report's own convention on the
// one choice the shared rule leaves open: the visible characters '<', '>'
// and '&' stay as themselves, even in an ID that needs quoting for other
// characters. Every other ID keeps the existing plain two-space-indent
// spelling, so ordinary IDs — including Chinese and other visible text —
// stay directly readable. The original ID is never trimmed, replaced or
// rejected, and the escaped text is never used for identity comparison:
// sorting, deduplication and label matching all keep working on the
// original string.

// reportClusterID renders one cluster ID for the all-rejected failure
// report. Plain IDs are returned unchanged; an ID needing escapes is
// returned as a JSON string literal (surrounding quotes included).
func reportClusterID(id string) string {
	if !needsQuotedReportID(id) {
		return id
	}
	var b strings.Builder
	b.Grow(len(id) + 2)
	escapeJSONString(&b, id, false)
	return b.String()
}

// needsQuotedReportID reports whether an ID contains any character that
// could break the line-based report or be mistaken for the quoting itself:
// a double quote, a backslash, a Unicode control character, or U+2028 /
// U+2029. Anything else — visible text of any script — stays plain.
func needsQuotedReportID(id string) bool {
	for _, r := range id {
		if r == '"' || r == '\\' || r == '\u2028' || r == '\u2029' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}
