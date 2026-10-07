package darksafe

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

// This file covers the shared display rule in quotedtext.go: the one JSON
// escaping used by both the all-rejected candidate report and a
// configuration error's field location. The two displays must render every
// quote/backslash/control character identically and decodably; the only
// things a display is allowed to own are whether ordinary text gets quotes
// at all and the spelling of the three visible characters less-than,
// greater-than and ampersand.

// decodeQuoted JSON-decodes a quoted display back to its original text.
func decodeQuoted(t *testing.T, display string) string {
	t.Helper()
	var got string
	if err := json.Unmarshal([]byte(display), &got); err != nil {
		t.Fatalf("display %q is not a decodable JSON string: %v", display, err)
	}
	return got
}

// TestSharedQuoteRule_ControlAndShortEscapesAreIdentical proves the two
// styles escape the same characters with the same spellings and that every
// quoted result decodes back to its source.
func TestSharedQuoteRule_ControlAndShortEscapesAreIdentical(t *testing.T) {
	cases := []struct {
		in   string
		want string // expected spelling for BOTH styles (both are quoted)
	}{
		{`q"x`, `"q\"x"`},
		{`back\slash`, `"back\\slash"`},
		{"back\bform", `"back\bform"`},
		{"form\ffeed", `"form\ffeed"`},
		{"line\nfeed", `"line\nfeed"`},
		{"carriage\rreturn", `"carriage\rreturn"`},
		{"tab\tstop", `"tab\tstop"`},
		{"nul\x00end", `"nul\u0000end"`},
		{"unit\x1fsep", `"unit\u001fsep"`},
		{"esc\x1b[0m", `"esc\u001b[0m"`},
		{"del\x7f", `"del\u007f"`},
		{"first-c1\u0080", `"first-c1\u0080"`},
		{"nel\u0085", `"nel\u0085"`},
		{"last-c1\u009f", `"last-c1\u009f"`},
		{"line-sep\u2028x", `"line-sep\u2028x"`},
		{"para-sep\u2029x", `"para-sep\u2029x"`},
	}
	for _, tc := range cases {
		gotReport := quoteReportText.render(tc.in)
		gotPath := quotePathText.render(tc.in)
		if gotReport != tc.want || gotPath != tc.want {
			t.Errorf("input %q:\n report=%q\n path  =%q\n want  =%q",
				tc.in, gotReport, gotPath, tc.want)
		}
		if decodeQuoted(t, gotReport) != tc.in {
			t.Errorf("report spelling %q does not restore %q", gotReport, tc.in)
		}
		if decodeQuoted(t, gotPath) != tc.in {
			t.Errorf("path spelling %q does not restore %q", gotPath, tc.in)
		}
		if strings.Contains(gotReport, `\x`) || strings.Contains(gotPath, `\x`) {
			t.Errorf("input %q produced a JSON-illegal \\xNN spelling", tc.in)
		}
	}
}

// TestSharedQuoteRule_PlainTextAndQuotingPolicy checks the part each display
// owns: ordinary visible text is bare in the candidate report but always
// quoted in a field location, while the contents stay identical.
func TestSharedQuoteRule_PlainTextAndQuotingPolicy(t *testing.T) {
	for _, in := range []string{"", "plain", "中文集群", "😀 cluster", `a<b>&c`} {
		if got := quoteReportText.render(in); got != in {
			t.Errorf("report must show plain text %q unchanged, got %q", in, got)
		}
		gotPath := quotePathText.render(in)
		if !strings.HasPrefix(gotPath, `"`) || !strings.HasSuffix(gotPath, `"`) {
			t.Errorf("path must always quote %q, got %q", in, gotPath)
		}
		if decodeQuoted(t, gotPath) != in {
			t.Errorf("path spelling %q does not restore %q", gotPath, in)
		}
	}
	// The empty name keeps its established quoted spelling in a location.
	if got := quotePathText.render(""); got != `""` {
		t.Errorf("empty path name = %q, want \"\"", got)
	}
}

// TestSharedQuoteRule_HTMLSignsAreTheOnlyDifference is the one convention
// the two displays keep apart: less-than, greater-than and ampersand stay
// raw in the candidate report — even when another character forces the
// quotes on — but keep their HTML-safe \uXXXX spelling in a field location.
func TestSharedQuoteRule_HTMLSignsAreTheOnlyDifference(t *testing.T) {
	// Alone they are ordinary visible text to the report and quoted raw in
	// the path style.
	for in, wantPath := range map[string]string{
		"a<b": `"a\u003cb"`,
		"a>b": `"a\u003eb"`,
		"a&b": `"a\u0026b"`,
	} {
		if got := quoteReportText.render(in); got != in {
			t.Errorf("report must leave %q plain, got %q", in, got)
		}
		if got := quotePathText.render(in); got != wantPath {
			t.Errorf("path spelling of %q = %q, want %q", in, got, wantPath)
		}
		if decodeQuoted(t, wantPath) != in {
			t.Errorf("path spelling %q does not restore %q", wantPath, in)
		}
	}
	// With a control character present the report must quote, yet keep the
	// three visible characters raw inside the quoted string.
	in := "a<b>&c\nd"
	wantReport := `"a<b>&c\nd"`
	if got := quoteReportText.render(in); got != wantReport {
		t.Errorf("quoted report spelling = %q, want %q", got, wantReport)
	}
	if decodeQuoted(t, wantReport) != in {
		t.Errorf("quoted report %q does not restore %q", wantReport, in)
	}
	wantPath := `"a\u003cb\u003e\u0026c\nd"`
	if got := quotePathText.render(in); got != wantPath {
		t.Errorf("quoted path spelling = %q, want %q", got, wantPath)
	}
	if decodeQuoted(t, wantPath) != in {
		t.Errorf("quoted path %q does not restore %q", wantPath, in)
	}
}

// TestSharedQuoteRule_RealNewlineDistinctFromLiteralN is the identity rule:
// a real newline and a backslash followed by the letter n render
// differently in both styles, and each spelling restores its own source.
func TestSharedQuoteRule_RealNewlineDistinctFromLiteralN(t *testing.T) {
	real := "c-a\nc-b"
	literal := `c-a\nc-b`
	for _, st := range []quoteStyle{quoteReportText, quotePathText} {
		gotReal := st.render(real)
		gotLiteral := st.render(literal)
		if gotReal == gotLiteral {
			t.Fatalf("real newline and literal \\n merged into %q", gotReal)
		}
		if decodeQuoted(t, gotReal) != real {
			t.Errorf("%q does not restore real-newline source", gotReal)
		}
		if decodeQuoted(t, gotLiteral) != literal {
			t.Errorf("%q does not restore literal-backslash source", gotLiteral)
		}
	}
	// The established concrete spellings.
	if got := quoteReportText.render(real); got != `"c-a\nc-b"` {
		t.Errorf("real newline report spelling = %q", got)
	}
	if got := quoteReportText.render(literal); got != `"c-a\\nc-b"` {
		t.Errorf("literal \\n report spelling = %q", got)
	}
}

// TestSharedQuoteRule_NeverEmitsControlEffect checks the rendered text of
// either style carries no raw control code point or line/paragraph
// separator (the wrapping quotes and short-escape backslashes are text).
func TestSharedQuoteRule_NeverEmitsControlEffect(t *testing.T) {
	probe := "\"\\\b\f\n\r\t\x00\x1b\x7f\u0080\u0085\u009f\u2028\u2029a<b>&c中😀"
	for _, st := range []quoteStyle{quoteReportText, quotePathText} {
		out := st.render(probe)
		for _, r := range out {
			if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
				t.Errorf("style rendered raw control %#U in %q", r, out)
			}
		}
		if decodeQuoted(t, out) != probe {
			t.Errorf("rendering %q does not restore source", out)
		}
	}
}

// TestSharedQuoteRule_PowersBothDisplays ties the shared rule to the two
// entry points callers actually use, so a change to one cannot silently
// diverge from the other.
func TestSharedQuoteRule_PowersBothDisplays(t *testing.T) {
	id := "zone\tA<b>"
	if got, want := reportClusterID(id), quoteReportText.render(id); got != want {
		t.Errorf("reportClusterID(%q) = %q, want shared report spelling %q", id, got, want)
	}
	name := "zone\tA<b>"
	if got, want := jsonEncodePathString(name), quotePathText.render(name); got != want {
		t.Errorf("jsonEncodePathString(%q) = %q, want shared path spelling %q", name, got, want)
	}
}
