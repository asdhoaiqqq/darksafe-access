package darksafe

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// This file covers the display rule for cluster IDs in the all-rejected
// failure report (rejectreport.go): the report is a line-based list, so an
// ID containing a quote, backslash, control character or U+2028/U+2029 is
// rendered as a quoted JSON string with those characters escaped — one real
// candidate always occupies exactly one record line, and decoding the
// quoted text restores the original ID exactly. Plain IDs keep the
// unquoted two-space-indent spelling. Identity itself is untouched:
// records are still sorted by the original ID and each candidate appears
// exactly once.

// rejectReportIDs extracts the displayed ID token of every record line of
// an all-rejected error: the summary line is line 0, each following line is
// "  <display>：<reason>".
func rejectReportLines(t *testing.T, msg string) []string {
	t.Helper()
	lines := strings.Split(msg, "\n")
	if lines[0] != "没有符合规则的可用集群，各候选集群未入选原因：" {
		t.Fatalf("unexpected summary line %q in %q", lines[0], msg)
	}
	return lines[1:]
}

// assertRecordLine checks one record line against the expected display
// token and reason, and — when the display is a quoted JSON string — that
// decoding it restores the original ID.
func assertRecordLine(t *testing.T, line, display, reason, originalID string) {
	t.Helper()
	want := "  " + display + "：" + reason
	if line != want {
		t.Fatalf("record line mismatch:\n got %q\nwant %q", line, want)
	}
	if strings.HasPrefix(display, "\"") {
		var got string
		if err := json.Unmarshal([]byte(display), &got); err != nil {
			t.Fatalf("display %q is not a decodable JSON string: %v", display, err)
		}
		if got != originalID {
			t.Fatalf("display %q decodes to %q, want original ID %q", display, got, originalID)
		}
	} else if display != originalID {
		t.Fatalf("plain display %q must equal the original ID %q", display, originalID)
	}
}

// assertNoControlEffect checks that the report text carries no control
// character beyond the newlines separating records, and no U+2028/U+2029
// line separators: every special character of an ID must have been escaped.
func assertNoControlEffect(t *testing.T, msg string) {
	t.Helper()
	for _, r := range msg {
		if r == '\n' {
			continue
		}
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			t.Fatalf("report contains unescaped control/separator %#U: %q", r, msg)
		}
	}
}

// TestMakeReleasePlan_AllRejectedQuotesNewlineAndBackslashIDs is the core
// regression: an ID made of "c-a", a real newline and "c-b" is one
// candidate and must stay one record, displayed as the JSON string
// "c-a\nc-b"; an ID made of "c-a", a backslash, "n" and "c-b" is a
// different candidate and must display differently — "c-a\\nc-b". Records
// remain sorted by the original IDs (a newline sorts before a backslash).
func TestMakeReleasePlan_AllRejectedQuotesNewlineAndBackslashIDs(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		Clusters: []Cluster{
			{ID: `c-a\nc-b`, Tags: map[string]string{"env": "temp"}}, // backslash + n
			{ID: "c-a\nc-b", Disabled: true},                         // real newline
			{ID: "plain"},
		},
		Exclude: []LabelCondition{{"env": "temp"}},
		Include: []LabelCondition{{"env": "prod"}},
	}
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error when every candidate is filtered out")
	}
	assertZeroReleasePlan(t, plan)

	lines := rejectReportLines(t, err.Error())
	if len(lines) != 3 {
		t.Fatalf("3 candidates must produce exactly 3 record lines, got %d: %q", len(lines), err.Error())
	}
	assertRecordLine(t, lines[0], `"c-a\nc-b"`, ReasonDisabled, "c-a\nc-b")
	assertRecordLine(t, lines[1], `"c-a\\nc-b"`, ReasonExcludeMatched, `c-a\nc-b`)
	assertRecordLine(t, lines[2], "plain", ReasonIncludeNotMatched, "plain")
	assertNoControlEffect(t, err.Error())
}

// TestMakeReleasePlan_AllRejectedQuotesSpecialIDs exercises every trigger
// of the quoting rule — double quote, backslash, tab, carriage return, ESC,
// DEL and the U+2028/U+2029 separators — plus the visible characters that
// must stay plain (Chinese text, and "<>&" which JSON's HTML escaping must
// not rewrite). Each display decodes back to the original ID.
func TestMakeReleasePlan_AllRejectedQuotesSpecialIDs(t *testing.T) {
	cases := []struct {
		id      string
		display string
	}{
		{`q"x`, `"q\"x"`},
		{`back\slash`, `"back\\slash"`},
		{"tab\there", `"tab\there"`},
		{"cr\rhere", `"cr\rhere"`},
		{"esc\x1b[0m", `"esc\u001b[0m"`},
		{"del\x7f", `"del\u007f"`},
		{"sep\u2028line", `"sep\u2028line"`},
		{"para\u2029line", `"para\u2029line"`},
		{"中文集群", "中文集群"},
		{"a<b>&c", "a<b>&c"},
	}
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 2,
		Include: []LabelCondition{{"env": "prod"}},
	}
	// Every candidate is disabled, so the reason is uniform and the sorted
	// order is just the ascending original IDs.
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		in.Clusters = append(in.Clusters, Cluster{ID: c.id, Disabled: true})
		ids = append(ids, c.id)
	}
	sort.Strings(ids)

	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error when every candidate is filtered out")
	}
	assertZeroReleasePlan(t, plan)
	msg := err.Error()
	lines := rejectReportLines(t, msg)
	if len(lines) != len(cases) {
		t.Fatalf("expected %d record lines, got %d: %q", len(cases), len(lines), msg)
	}
	displayByID := make(map[string]string, len(cases))
	for _, c := range cases {
		displayByID[c.id] = c.display
	}
	for i, id := range ids {
		assertRecordLine(t, lines[i], displayByID[id], ReasonDisabled, id)
	}
	assertNoControlEffect(t, msg)
}

// TestMakeReleasePlan_AllRejectedQuotedIDNotConfusedWithQuoting makes sure
// an ID that itself contains quotes cannot be mistaken for the quoting the
// report adds: `a"b` displays as "a\"b", whose outer quotes are the
// report's and whose inner \" decodes back to the real quote.
func TestMakeReleasePlan_AllRejectedQuotedIDNotConfusedWithQuoting(t *testing.T) {
	in := ReleasePlanInput{
		App: "app", Revision: "r1", Image: "img", BatchSize: 1,
		Clusters: []Cluster{
			{ID: `a"b`, Disabled: true},
			{ID: `a"b"c`, Disabled: true},
		},
	}
	_, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	lines := rejectReportLines(t, err.Error())
	if len(lines) != 2 {
		t.Fatalf("expected 2 record lines, got %d: %q", len(lines), err.Error())
	}
	assertRecordLine(t, lines[0], `"a\"b"`, ReasonDisabled, `a"b`)
	assertRecordLine(t, lines[1], `"a\"b\"c"`, ReasonDisabled, `a"b"c`)
}

// TestPlanCLI_AllRejectedSpecialIDsMatchesLibrary drives the same
// special-ID configuration through the command line: exit code 1, empty
// stdout, and stderr carrying the same per-candidate reasons the library
// error reports (plus the newline Fprintln appends). The JSON document
// supplies the real newline via a \n escape and the literal backslash-n via
// a \\ escape, so both IDs are exactly the library test's.
func TestPlanCLI_AllRejectedSpecialIDsMatchesLibrary(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2,
  "include": [{"env": "prod"}],
  "exclude": [{"env": "temp"}],
  "clusters": [
    {"id": "c-a\nc-b", "disabled": true},
    {"id": "c-a\\nc-b", "tags": {"env": "temp"}},
    {"id": "plain"}
  ]
}`
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
	want := allRejectedStderr(
		`"c-a\nc-b"：`+ReasonDisabled,
		`"c-a\\nc-b"：`+ReasonExcludeMatched,
		"plain："+ReasonIncludeNotMatched,
	)
	assertAllRejected(t, code, stdout, stderr, want)

	// The library error for the same configuration must carry the same
	// reason content, differing only by the trailing newline the CLI adds.
	in, perr := ParseReleaseInput([]byte(doc))
	if perr != nil {
		t.Fatal(perr)
	}
	_, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected a non-nil error from the library path")
	}
	if stderr != err.Error()+"\n" {
		t.Fatalf("CLI and library reports differ:\n cli: %q\n lib: %q", stderr, err.Error())
	}
}

// TestPlanCLI_AllRejectedSpreadBySpecialIDs keeps the fault-domain
// interaction honest for quoted IDs: with spreadBy enabled and every
// candidate filtered out (none carrying the spread tag), the report still
// lists each candidate's filter reason — quoted where needed — and never
// turns into a missing-fault-domain-tag error.
func TestPlanCLI_AllRejectedSpreadBySpecialIDs(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "zone",
  "include": [{"env": "prod"}],
  "clusters": [
    {"id": "c-off\toff", "disabled": true},
    {"id": "c-dev"}
  ]
}`
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
	want := allRejectedStderr(
		"c-dev："+ReasonIncludeNotMatched,
		`"c-off\toff"：`+ReasonDisabled,
	)
	assertAllRejected(t, code, stdout, stderr, want)
	if strings.Contains(stderr, "故障域") {
		t.Fatalf("filtered-out clusters must not trigger the fault-domain check, got %q", stderr)
	}
}
