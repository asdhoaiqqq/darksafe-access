package darksafe

import (
	"fmt"
	"strings"
	"testing"
)

// This file pins down the command-line contract of the "every candidate was
// rejected" plan failure as a user actually receives it from
// `darksafe plan <file>`: a legal config with at least one candidate where no
// cluster passes the filters must exit 1, print nothing to stdout (never a
// success plan or partial batches), and print the rejection header on stderr
// followed by one line per candidate — every candidate exactly once, ascending
// by ID, regardless of file order — carrying the same fixed reason strings a
// successful plan uses. Everything runs offline: no cluster is contacted and
// no release is performed.
//
// The library-level error is covered in plan_test.go; here the complete
// multi-reason listing and the stdout/stderr/exit-code convention are observed
// through the real command entry point.

// allRejectedHeader is the exact first stderr line of the all-rejected
// failure; candidate lines follow as "  <id>：<reason>" (two-space indent,
// full-width colon).
const allRejectedHeader = "没有符合规则的可用集群，各候选集群未入选原因："

// goRunStatusSuffix is what `go run` appends to stderr after a child exits
// non-zero; runCLI drives the command through `go run`, so it trails the
// command's own stderr output.
const goRunStatusSuffix = "exit status 1\n"

// assertAllRejectedFailure verifies the whole failure contract for one run:
// exit code 1, completely empty stdout, and stderr consisting of exactly the
// rejection header plus one line per wanted entry (want must already be
// ascending), followed only by go run's own status line. It also parses the
// candidate lines to prove every ID appears exactly once in ascending order,
// and that the failure was the all-rejected listing rather than a fault-domain
// tag error (filtering precedes the spreadBy tag check).
func assertAllRejectedFailure(t *testing.T, code int, stdout, stderr string, want []cliExcluded) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be completely empty on full rejection, got %q", stdout)
	}
	if strings.Contains(stderr, "缺少故障域标签") {
		t.Fatalf("rejected clusters must not be checked for the spreadBy tag; stderr=%q", stderr)
	}

	var b strings.Builder
	b.WriteString(allRejectedHeader)
	for _, w := range want {
		fmt.Fprintf(&b, "\n  %s：%s", w.ID, w.Reason)
	}
	wantBlock := b.String() + "\n"
	if stderr != wantBlock+goRunStatusSuffix {
		t.Fatalf("stderr mismatch:\n got=%q\nwant=%q", stderr, wantBlock+goRunStatusSuffix)
	}

	// Independently parse the listing to protect its line convention: one
	// unique candidate per line, ascending by ID, reasons matching want.
	lines := strings.Split(strings.TrimSuffix(wantBlock, "\n"), "\n")
	if lines[0] != allRejectedHeader {
		t.Fatalf("first line = %q, want %q", lines[0], allRejectedHeader)
	}
	if len(lines)-1 != len(want) {
		t.Fatalf("got %d candidate lines, want %d", len(lines)-1, len(want))
	}
	seen := map[string]int{}
	prevID := ""
	for i, line := range lines[1:] {
		body, ok := strings.CutPrefix(line, "  ")
		if !ok {
			t.Fatalf("candidate line %d is not indented by two spaces: %q", i+1, line)
		}
		id, reason, ok := strings.Cut(body, "：")
		if !ok || id == "" || reason == "" || strings.ContainsRune(id, '：') || strings.ContainsRune(reason, '：') {
			t.Fatalf("candidate line %d must be \"  <id>：<reason>\": %q", i+1, line)
		}
		if lineN, dup := seen[id]; dup {
			t.Fatalf("candidate %q listed on line %d and again on line %d", id, lineN, i+1)
		}
		seen[id] = i + 1
		if i > 0 && id <= prevID {
			t.Fatalf("candidate lines not ascending: %q followed by %q", prevID, id)
		}
		prevID = id
		if w := want[i]; id != w.ID || reason != w.Reason {
			t.Fatalf("line %d = %q：%q, want %q：%q", i+1, id, reason, w.ID, w.Reason)
		}
	}
}

// allRejectedConfig is one legal configuration in which every candidate is
// filtered out, exercising the full reason priority chain at once:
//
//   - d-off is disabled AND matches an exclude condition -> 集群已停用
//     (disabled outranks exclude).
//   - a-exc matches include condition 1 (env=prod) AND an exclude condition
//     (quarantine=true) -> 命中排除条件 (exclude outranks include).
//   - e-tmp matches an exclude condition only -> 命中排除条件.
//   - b-inc matches neither include condition and no exclude condition
//     (env=dev; condition 1 needs env=prod, condition 2 needs tier+scope)
//     -> 未命中包含条件.
//   - c-partial carries tier=edge but is missing the scope label that
//     include condition 2 requires (every key in a condition must match), so
//     it satisfies neither condition -> 未命中包含条件; it must not be
//     selected just because one key matches.
//
// spreadBy=zone is enabled and none of the candidates carry a zone tag: the
// filter result must still be the rejection listing, never a missing
// fault-domain-tag failure. clusterOrder is spliced into the clusters array so
// the same candidates can be presented in different file orders.
func allRejectedConfig(clusterOrder string) string {
	return fmt.Sprintf(`{
  "app": "payments",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments:2026.10.0-r3",
  "batchSize": 2,
  "spreadBy": "zone",
  "include": [{"env": "prod"}, {"tier": "edge", "scope": "public"}],
  "exclude": [{"env": "temp"}, {"quarantine": "true"}],
  "clusters": [%s]
}`, clusterOrder)
}

// allRejectedWant is the complete, ascending listing the config must produce;
// all three fixed reasons are visible together in this single failure output.
var allRejectedWant = []cliExcluded{
	{ID: "a-exc", Reason: ReasonExcludeMatched},
	{ID: "b-inc", Reason: ReasonIncludeNotMatched},
	{ID: "c-partial", Reason: ReasonIncludeNotMatched},
	{ID: "d-off", Reason: ReasonDisabled},
	{ID: "e-tmp", Reason: ReasonExcludeMatched},
}

// TestPlanCLI_AllRejectedFullReasons drives the failure through the real
// command and checks exit code 1, empty stdout, and the exact stderr listing:
// header plus every candidate exactly once, ascending by ID, with the three
// fixed reasons reflecting the disabled -> exclude -> include priority, even
// though the candidates are written out of order and none carry the spreadBy
// tag.
func TestPlanCLI_AllRejectedFullReasons(t *testing.T) {
	orderA := `
    {"id": "e-tmp", "tags": {"env": "temp"}},
    {"id": "c-partial", "tags": {"tier": "edge"}},
    {"id": "a-exc", "tags": {"env": "prod", "quarantine": "true"}},
    {"id": "d-off", "disabled": true, "tags": {"env": "temp"}},
    {"id": "b-inc", "tags": {"env": "dev"}}`

	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, allRejectedConfig(orderA)))
	assertAllRejectedFailure(t, code, stdout, stderr, allRejectedWant)

	// All three fixed reasons must coexist in this one failure message rather
	// than only appearing across separate per-cluster errors.
	for _, reason := range []string{ReasonDisabled, ReasonExcludeMatched, ReasonIncludeNotMatched} {
		if !strings.Contains(stderr, "："+reason+"\n") {
			t.Fatalf("failure output must show reason %q for some candidate: %q", reason, stderr)
		}
	}
	// No candidate may be missing and no plan-like output may leak through.
	for _, w := range allRejectedWant {
		if !strings.Contains(stderr, "\n  "+w.ID+"：") {
			t.Fatalf("failure output must list candidate %q: %q", w.ID, stderr)
		}
	}
}

// TestPlanCLI_AllRejectedIndependentOfFileOrder presents the same candidates
// in two deliberately different orders and requires the failure result
// (exit code, empty stdout, complete reason listing) to be byte-identical.
func TestPlanCLI_AllRejectedIndependentOfFileOrder(t *testing.T) {
	orderA := `
    {"id": "e-tmp", "tags": {"env": "temp"}},
    {"id": "c-partial", "tags": {"tier": "edge"}},
    {"id": "a-exc", "tags": {"env": "prod", "quarantine": "true"}},
    {"id": "d-off", "disabled": true, "tags": {"env": "temp"}},
    {"id": "b-inc", "tags": {"env": "dev"}}`
	orderB := `
    {"id": "b-inc", "tags": {"env": "dev"}},
    {"id": "d-off", "disabled": true, "tags": {"env": "temp"}},
    {"id": "a-exc", "tags": {"quarantine": "true", "env": "prod"}},
    {"id": "e-tmp", "tags": {"env": "temp"}},
    {"id": "c-partial", "tags": {"tier": "edge"}}`

	codeA, stdoutA, stderrA := runCLI(t, "plan", writePlanDoc(t, allRejectedConfig(orderA)))
	codeB, stdoutB, stderrB := runCLI(t, "plan", writePlanDoc(t, allRejectedConfig(orderB)))
	assertAllRejectedFailure(t, codeA, stdoutA, stderrA, allRejectedWant)
	assertAllRejectedFailure(t, codeB, stdoutB, stderrB, allRejectedWant)
	if stderrA != stderrB {
		t.Fatalf("file order changed the failure listing:\n%s\n----\n%s", stderrA, stderrB)
	}
}

// TestPlanCLI_AllRejectedSingleDisabledCandidate covers the minimal shape:
// exactly one candidate, disabled (and also matching an exclude condition),
// with spreadBy enabled and no zone tag. The failure must still list that one
// candidate with 集群已停用 — not a missing fault-domain-tag error — and emit
// no plan.
func TestPlanCLI_AllRejectedSingleDisabledCandidate(t *testing.T) {
	doc := `{
  "app": "app",
  "revision": "r1",
  "image": "img",
  "batchSize": 1,
  "spreadBy": "zone",
  "exclude": [{"env": "temp"}],
  "clusters": [
    {"id": "solo", "disabled": true, "tags": {"env": "temp"}}
  ]
}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	assertAllRejectedFailure(t, code, stdout, stderr, []cliExcluded{
		{ID: "solo", Reason: ReasonDisabled},
	})
}
