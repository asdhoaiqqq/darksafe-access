package darksafe

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
// The escaping itself is the shared rule in quotedtext.go; this file keeps
// only this report's own display convention. A "normal" ID is shown
// unchanged; an ID that contains a double quote, a backslash, a Unicode
// control character, or the U+2028/U+2029 separators is rendered as a
// double-quoted JSON string in which every such character is escaped, so
// the report never gains a real line break, tab or other control effect and
// decoding the quoted text restores the original ID byte for byte. The
// visible characters less-than, greater-than and ampersand stay raw even
// inside a quoted ID — the one spelling this report keeps different from a
// field location. Every other ID keeps the existing plain two-space-indent
// spelling, so ordinary IDs — including Chinese and other visible text —
// stay directly readable. The original ID is never trimmed, replaced or
// rejected, and the escaped text is never used for identity comparison:
// sorting, deduplication and label matching all keep working on the
// original string.

// reportClusterID renders one cluster ID for the all-rejected failure
// report. Plain IDs are returned unchanged; an ID needing escapes is
// returned as a JSON string literal (surrounding quotes included). The
// quote-needed test and the escaping both live in quotedtext.go, shared
// with the field-location renderer.
func reportClusterID(id string) string {
	return quoteReportText.render(id)
}
