package darksafe

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// This file drives the all-candidates-rejected report end to end when legal
// candidate IDs carry display-hostile characters: real newlines, carriage
// returns, tabs, NUL/DEL/C1 controls, U+2028/U+2029, quotes and backslashes.
// The unit tests in reporttext_test.go cover the rendering itself; here the
// same candidates are observed through both entries — MakeReleasePlan on an
// in-memory config and `darksafe plan` on a JSON file — and the report must
// keep one recoverable record per candidate through both, while ordinary IDs
// keep the plain "  id：reason" shape.

// rejectionReportCandidate is one test candidate paired with the rejection
// reason the fixed disabled → exclude → include priority must assign.
type rejectionReportCandidate struct {
	id     string
	reason string
	doc    jsonCluster
}

// rejectionReportCandidates builds the candidate set. Every candidate is
// rejected, and the IDs deliberately include characters that used to break
// the line-oriented report:
//   - "c-a<newline>c-b" (disabled) vs "c-a\<n>c-b" (backslash + n, excluded):
//     distinct IDs that must render as distinct single-line records;
//   - quotes, tab, CR, DEL, the C1 line separator U+0085, U+2028/U+2029 and
//     ordinary Chinese/plain IDs to cover every quoting trigger and the
//     unchanged plain format.
func rejectionReportCandidates() []rejectionReportCandidate {
	return []rejectionReportCandidate{
		{id: "c-a\nc-b", reason: ReasonDisabled, doc: jsonCluster{ID: "c-a\nc-b", Disabled: true}},
		{id: `c-a\nc-b`, reason: ReasonExcludeMatched, doc: jsonCluster{ID: `c-a\nc-b`, Tags: map[string]string{"env": "temp"}}},
		{id: "d\x7fd", reason: ReasonExcludeMatched, doc: jsonCluster{ID: "d\x7fd", Tags: map[string]string{"env": "temp"}}},
		{id: "l l", reason: ReasonIncludeNotMatched, doc: jsonCluster{ID: "l l", Tags: map[string]string{"env": "dev"}}},
		{id: "plain", reason: ReasonIncludeNotMatched, doc: jsonCluster{ID: "plain", Tags: map[string]string{"env": "dev"}}},
		{id: `q"q`, reason: ReasonIncludeNotMatched, doc: jsonCluster{ID: `q"q`, Tags: map[string]string{"env": "dev"}}},
		{id: "r\rr", reason: ReasonExcludeMatched, doc: jsonCluster{ID: "r\rr", Tags: map[string]string{"env": "temp"}}},
		{id: "t\t1", reason: ReasonDisabled, doc: jsonCluster{ID: "t\t1", Disabled: true}},
		{id: "x\xc2\x85y", reason: ReasonIncludeNotMatched, doc: jsonCluster{ID: "x\xc2\x85y"}},
		{id: "中文", reason: ReasonDisabled, doc: jsonCluster{ID: "中文", Disabled: true}},
	}
}

// jsonCluster is the on-disk shape of one candidate.
type jsonCluster struct {
	ID       string            `json:"id"`
	Disabled bool              `json:"disabled,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// rejectionReportInput builds the in-memory config for the candidate set.
// include requires env=prod and exclude matches env=temp; together with the
// disabled flag this yields one of each rejection reason.
func rejectionReportInput(cands []rejectionReportCandidate, spreadBy string) ReleasePlanInput {
	in := ReleasePlanInput{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  spreadBy,
		Include:   []LabelCondition{{"env": "prod"}},
		Exclude:   []LabelCondition{{"env": "temp"}},
	}
	for _, c := range cands {
		in.Clusters = append(in.Clusters, Cluster{ID: c.id, Disabled: c.doc.Disabled, Tags: c.doc.Tags})
	}
	return in
}

// rejectionReportDoc marshals the same configuration as a JSON file, so the
// CLI parses exactly the configuration the library gets in memory. The
// encoder escapes C0 controls and U+2028/U+2029 but passes U+0085 through
// literally, which is legal JSON and exercises that path too.
func rejectionReportDoc(t *testing.T, cands []rejectionReportCandidate, spreadBy string) string {
	t.Helper()
	doc := struct {
		App       string           `json:"app"`
		Revision  string           `json:"revision"`
		Image     string           `json:"image"`
		BatchSize int              `json:"batchSize"`
		SpreadBy  string           `json:"spreadBy,omitempty"`
		Include   []LabelCondition `json:"include"`
		Exclude   []LabelCondition `json:"exclude"`
		Clusters  []jsonCluster    `json:"clusters"`
	}{
		App:       "app",
		Revision:  "r1",
		Image:     "img",
		BatchSize: 2,
		SpreadBy:  spreadBy,
		Include:   []LabelCondition{{"env": "prod"}},
		Exclude:   []LabelCondition{{"env": "temp"}},
	}
	for _, c := range cands {
		doc.Clusters = append(doc.Clusters, c.doc)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return writePlanDoc(t, string(data))
}

// expectedRejectionError renders the exact library error from the candidate
// set: the summary plus one formatted record per candidate in ascending
// original-ID order.
func expectedRejectionError(cands []rejectionReportCandidate) string {
	sorted := append([]rejectionReportCandidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })
	entries := make([]string, len(sorted))
	for i, c := range sorted {
		entries[i] = clusterIDForReport(c.id) + "：" + c.reason
	}
	return allRejectedLibraryError(entries...)
}

// parseRejectionRecord recovers the original ID and reason from one report
// line ("  <rendered-id>：<reason>"). The reason is matched as a known
// suffix because a quoted ID may itself contain a Chinese colon.
func parseRejectionRecord(t *testing.T, line string) (string, string) {
	t.Helper()
	if !strings.HasPrefix(line, "  ") {
		t.Fatalf("record line must start with two spaces: %q", line)
	}
	body := line[2:]
	for _, reason := range []string{ReasonDisabled, ReasonExcludeMatched, ReasonIncludeNotMatched} {
		suffix := "：" + reason
		if strings.HasSuffix(body, suffix) {
			rendered := body[:len(body)-len(suffix)]
			id := rendered
			if strings.HasPrefix(rendered, `"`) {
				if err := json.Unmarshal([]byte(rendered), &id); err != nil {
					t.Fatalf("record ID is not a decodable JSON string: %v (%q)", err, rendered)
				}
			}
			return id, reason
		}
	}
	t.Fatalf("record line does not end with a known reason: %q", line)
	return "", ""
}

// assertRejectionReportShape checks the report's structural contract:
//   - exactly one physical line per candidate plus the summary line, so no
//     candidate can appear as several records;
//   - no record line carries any control character or U+2028/U+2029;
//   - records decode back to the candidate set exactly once each, in
//     ascending original-ID order, with the priority-assigned reasons;
//   - the two lookalike IDs render as distinct one-line records.
func assertRejectionReportShape(t *testing.T, msg string, cands []rejectionReportCandidate) {
	t.Helper()
	lines := strings.Split(msg, "\n")
	if want := len(cands) + 1; len(lines) != want {
		t.Fatalf("report must occupy exactly %d physical lines (summary + one per candidate), got %d: %q", want, len(lines), msg)
	}
	if lines[0] != "没有符合规则的可用集群，各候选集群未入选原因：" {
		t.Fatalf("unexpected summary line: %q", lines[0])
	}

	sorted := append([]rejectionReportCandidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })
	got := make(map[string]int)
	for i, c := range sorted {
		line := lines[i+1]
		for _, r := range line {
			if r <= 0x1F || (r >= 0x7F && r <= 0x9F) || r == 0x2028 || r == 0x2029 {
				t.Fatalf("record %d carries a raw control/separator rune %q: %q", i+1, r, line)
			}
		}
		id, reason := parseRejectionRecord(t, line)
		if id != c.id {
			t.Fatalf("record %d recovered ID %q, want %q (line %q)", i+1, id, c.id, line)
		}
		if reason != c.reason {
			t.Fatalf("record %d (%q) reason = %q, want %q", i+1, id, reason, c.reason)
		}
		got[id]++
	}
	if len(got) != len(cands) {
		t.Fatalf("every candidate must appear exactly once, got %d distinct of %d", len(got), len(cands))
	}
	for _, c := range cands {
		if got[c.id] != 1 {
			t.Fatalf("candidate %q appeared %d times", c.id, got[c.id])
		}
	}

	// Pinned lookalike lines: real newline vs backslash-n must differ and
	// stay one record each; plain and Chinese IDs stay unquoted.
	wantLines := map[string]string{
		"c-a\nc-b":   `  "c-a\nc-b"：` + ReasonDisabled,
		`c-a\nc-b`:   `  "c-a\\nc-b"：` + ReasonExcludeMatched,
		"plain":      "  plain：" + ReasonIncludeNotMatched,
		"中文":         "  中文：" + ReasonDisabled,
		"r\rr":       `  "r\rr"：` + ReasonExcludeMatched,
		"t\t1":       `  "t\t1"：` + ReasonDisabled,
		"x\xc2\x85y": `  "x\u0085y"：` + ReasonIncludeNotMatched,
	}
	for _, line := range lines[1:] {
		id, _ := parseRejectionRecord(t, line)
		if want, ok := wantLines[id]; ok && line != want {
			t.Fatalf("record for %q:\n got %q\nwant %q", id, line, want)
		}
	}
}

// TestMakeReleasePlan_RejectedReportWithControlIDs covers the library entry:
// the zero plan comes back with an error whose per-candidate records survive
// newlines, carriage returns, tabs, C1 controls and the Unicode separators,
// each decodable to the exact original ID.
func TestMakeReleasePlan_RejectedReportWithControlIDs(t *testing.T) {
	cands := rejectionReportCandidates()
	in := rejectionReportInput(cands, "")
	plan, err := MakeReleasePlan(in)
	if err == nil {
		t.Fatal("expected an all-rejected error")
	}
	assertZeroReleasePlan(t, plan)
	msg := err.Error()
	if msg != expectedRejectionError(cands) {
		t.Fatalf("library error mismatch:\n got %q\nwant %q", msg, expectedRejectionError(cands))
	}
	assertRejectionReportShape(t, msg, cands)

	// Rendering is display-only: the input keeps the original IDs, untrimmed.
	var ids []string
	for _, c := range in.Clusters {
		ids = append(ids, c.ID)
	}
	wantIDs := make([]string, len(cands))
	for i, c := range rejectionReportCandidates() {
		wantIDs[i] = c.id
	}
	if strings.Join(ids, "|") != strings.Join(wantIDs, "|") {
		t.Fatalf("input IDs must be untouched: %q", ids)
	}
}

// TestPlanCLI_RejectedReportMatchesLibrary drives the same configuration
// through the command line: exit code 1, empty stdout, and stderr identical
// to the library error apart from the single trailing newline Fprintln adds.
func TestPlanCLI_RejectedReportMatchesLibrary(t *testing.T) {
	cands := rejectionReportCandidates()
	libPlan, libErr := MakeReleasePlan(rejectionReportInput(cands, ""))
	if libErr == nil {
		t.Fatal("library call must fail")
	}
	assertZeroReleasePlan(t, libPlan)

	code, stdout, stderr := runPlanCLI(t, "plan", rejectionReportDoc(t, cands, ""))
	assertAllRejected(t, code, stdout, stderr, libErr.Error()+"\n")
	assertRejectionReportShape(t, strings.TrimSuffix(stderr, "\n"), cands)
}

// TestRejectedReport_SpreadByKeepsFilterReasonsWithControlIDs pins the
// filter-vs-fault-domain ordering with the hostile IDs: even with spreadBy
// enabled and none of the rejected clusters carrying that tag, the report is
// the same per-candidate filter-reason list — the missing-tag error must not
// replace it through either entry.
func TestRejectedReport_SpreadByKeepsFilterReasonsWithControlIDs(t *testing.T) {
	cands := rejectionReportCandidates()
	plain, err := MakeReleasePlan(rejectionReportInput(cands, ""))
	if err == nil {
		t.Fatal("plain config must fail")
	}
	assertZeroReleasePlan(t, plain)

	spread, errSpread := MakeReleasePlan(rejectionReportInput(cands, "zone"))
	if errSpread == nil {
		t.Fatal("spreadBy config must still fail")
	}
	assertZeroReleasePlan(t, spread)
	if errSpread.Error() != err.Error() {
		t.Fatalf("spreadBy must not change the all-rejected report:\n plain:  %q\nspread: %q", err, errSpread)
	}
	if strings.Contains(errSpread.Error(), "故障域") {
		t.Fatalf("filtered-out clusters need no fault-domain tag: %q", errSpread)
	}

	code, stdout, stderr := runPlanCLI(t, "plan", rejectionReportDoc(t, cands, "zone"))
	assertAllRejected(t, code, stdout, stderr, errSpread.Error()+"\n")
}

// TestRejectedReport_SingleCandidateWithNewlineIsOneRecord is the minimal
// CLI case: one disabled candidate whose ID is a real multi-line string must
// still produce exactly one record line, exit 1 and empty stdout.
func TestRejectedReport_SingleCandidateWithNewlineIsOneRecord(t *testing.T) {
	cands := []rejectionReportCandidate{
		{id: "only\nhost", reason: ReasonDisabled, doc: jsonCluster{ID: "only\nhost", Disabled: true}},
	}
	code, stdout, stderr := runPlanCLI(t, "plan", rejectionReportDoc(t, cands, ""))
	assertAllRejected(t, code, stdout, stderr,
		`没有符合规则的可用集群，各候选集群未入选原因：`+"\n"+
			`  "only\nhost"：`+ReasonDisabled+"\n")
	assertRejectionReportShape(t, strings.TrimSuffix(stderr, "\n"), cands)
}
