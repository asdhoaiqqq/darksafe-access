package darksafe

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file covers the command-line contract of fault-domain spreading
// (spreadBy): the library rules already have unit tests in plan_test.go; here
// the same guarantees are observed through `darksafe plan <file>` exactly as a
// user drives it — a config file on disk, JSON on stdout, errors on stderr and
// process exit codes. Everything runs offline: the binary never connects to a
// cluster or performs a release.

// cliPlan mirrors the JSON shape of a printed ReleasePlan.
type cliPlan struct {
	App      cliApp        `json:"app"`
	Batches  []cliBatch    `json:"batches"`
	Excluded []cliExcluded `json:"excluded"`
}

type cliApp struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Image    string `json:"image"`
}

type cliBatch struct {
	Index    int      `json:"index"`
	Clusters []string `json:"clusters"`
}

type cliExcluded struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// writePlanDoc stores doc in a fresh temp directory and returns its path.
func writePlanDoc(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// decodeCLIPlan requires stdout to hold exactly one complete JSON plan and
// nothing after it: success means stdout contains only the plan.
func decodeCLIPlan(t *testing.T, stdout string) cliPlan {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var p cliPlan
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("stdout is not a parseable JSON plan: %v; stdout=%q", err, stdout)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("stdout carries data after the plan: %q", stdout)
	}
	return p
}

// assertBatches checks batch numbering, size, ascending order, domain
// uniqueness per batch and that every selected ID appears exactly once.
func assertSpreadBatches(t *testing.T, p cliPlan, want [][]string, domains map[string]string, batchSize int) {
	t.Helper()
	if len(p.Batches) != len(want) {
		t.Fatalf("got %d batches %v, want %d batches %v", len(p.Batches), p.Batches, len(want), want)
	}
	seen := map[string]int{}
	for i, w := range want {
		b := p.Batches[i]
		if b.Index != i+1 {
			t.Fatalf("batch %d has index %d, want %d", i, b.Index, i+1)
		}
		if len(b.Clusters) > batchSize {
			t.Fatalf("batch %d holds %d clusters, batchSize is %d", i+1, len(b.Clusters), batchSize)
		}
		if strings.Join(b.Clusters, ",") != strings.Join(w, ",") {
			t.Fatalf("batch %d = %v, want %v", i+1, b.Clusters, w)
		}
		if !sort.StringsAreSorted(b.Clusters) {
			t.Fatalf("batch %d is not ascending: %v", i+1, b.Clusters)
		}
		usedDomain := map[string]string{}
		for _, id := range b.Clusters {
			if prev, dup := seen[id]; dup {
				t.Fatalf("cluster %q appears in batch %d and again in batch %d", id, prev, i+1)
			}
			seen[id] = i + 1
			d, known := domains[id]
			if !known {
				t.Fatalf("cluster %q in plan but no expected domain", id)
			}
			if other, conflict := usedDomain[d]; conflict {
				t.Fatalf("batch %d contains two clusters of domain %q: %q and %q", i+1, d, other, id)
			}
			usedDomain[d] = id
		}
	}
}

// TestPlanCLI_SpreadByFullPlan drives the whole contract through the CLI:
// complete parseable JSON plan, app/revision/image echoed from the input,
// fault-domain-aware batching, ascending excluded list with existing reasons,
// exit code 0, plan-only stdout and an empty stderr. Candidates are listed out
// of order in the file; that must not change any of the results.
func TestPlanCLI_SpreadByFullPlan(t *testing.T) {
	doc := `{
  "app": "payments",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments:2026.10.0-r3",
  "batchSize": 3,
  "spreadBy": "zone",
  "include": [{"zone": "z1"}, {"zone": "z2"}, {"zone": "z3"}],
  "exclude": [{"env": "temp"}],
  "clusters": [
    {"id": "c-d", "tags": {"zone": "z3"}},
    {"id": "x2", "tags": {"env": "temp"}},
    {"id": "c-b", "tags": {"zone": "z1"}},
    {"id": "x1", "tags": {"env": "dev"}},
    {"id": "off", "disabled": true},
    {"id": "c-e", "tags": {"zone": "z2"}},
    {"id": "c-a", "tags": {"zone": "z1"}},
    {"id": "c-c", "tags": {"zone": "z2"}}
  ]
}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr on success, got %q", stderr)
	}

	p := decodeCLIPlan(t, stdout)
	if p.App.Name != "payments" || p.App.Revision != "2026.10.0-r3" ||
		p.App.Image != "registry.example.net/payments:2026.10.0-r3" {
		t.Fatalf("app info not echoed from input: %+v", p.App)
	}

	// Sorted selection is c-a,c-b (z1), c-c,c-e (z2), c-d (z3). Batch 1 takes
	// the smallest IDs that fit: c-a(z1), c-c(z2), c-d(z3); c-b and c-e are
	// deferred by domain conflicts and form batch 2.
	domains := map[string]string{
		"c-a": "z1", "c-b": "z1",
		"c-c": "z2", "c-e": "z2",
		"c-d": "z3",
	}
	assertSpreadBatches(t, p,
		[][]string{{"c-a", "c-c", "c-d"}, {"c-b", "c-e"}},
		domains, 3)

	wantExcluded := []cliExcluded{
		{ID: "off", Reason: ReasonDisabled},
		{ID: "x1", Reason: ReasonIncludeNotMatched},
		{ID: "x2", Reason: ReasonExcludeMatched},
	}
	if len(p.Excluded) != len(wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", p.Excluded, wantExcluded)
	}
	for i, w := range wantExcluded {
		if p.Excluded[i] != w {
			t.Fatalf("excluded[%d] = %+v, want %+v", i, p.Excluded[i], w)
		}
	}
}

// TestPlanCLI_SpreadByConflictDefersButDoesNotBlock isolates the core rule:
// a same-domain conflict defers that cluster to a later batch, but later
// clusters of other domains still enter the current batch, and each batch is
// filled from the smallest still-unscheduled ID that can enter it.
func TestPlanCLI_SpreadByConflictDefersButDoesNotBlock(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "zone",
  "clusters": [
    {"id": "d", "tags": {"zone": "z2"}},
    {"id": "a", "tags": {"zone": "z1"}},
    {"id": "b", "tags": {"zone": "z1"}},
    {"id": "c", "tags": {"zone": "z2"}}
  ]
}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	domains := map[string]string{"a": "z1", "b": "z1", "c": "z2", "d": "z2"}
	// b conflicts with a and is deferred; c (other domain) still joins batch 1.
	assertSpreadBatches(t, p, [][]string{{"a", "c"}, {"b", "d"}}, domains, 2)
}

// TestPlanCLI_SpreadByIndependentOfFileOrder runs the same configuration with
// candidates in two different orders and requires byte-identical plans, plus
// the expected fault-domain schedule.
func TestPlanCLI_SpreadByIndependentOfFileOrder(t *testing.T) {
	template := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "zone",
  "clusters": [%s]
}`
	orderA := `
    {"id": "a", "tags": {"zone": "z1"}},
    {"id": "b", "tags": {"zone": "z1"}},
    {"id": "c", "tags": {"zone": "z2"}},
    {"id": "d", "tags": {"zone": "z2"}},
    {"id": "e", "tags": {"zone": "z3"}}`
	orderB := `
    {"id": "e", "tags": {"zone": "z3"}},
    {"id": "c", "tags": {"zone": "z2"}},
    {"id": "a", "tags": {"zone": "z1"}},
    {"id": "d", "tags": {"zone": "z2"}},
    {"id": "b", "tags": {"zone": "z1"}}`

	code1, out1, err1 := runCLI(t, "plan", writePlanDoc(t, fmt.Sprintf(template, orderA)))
	code2, out2, err2 := runCLI(t, "plan", writePlanDoc(t, fmt.Sprintf(template, orderB)))
	if code1 != 0 || code2 != 0 {
		t.Fatalf("expected exit 0 both runs, got %d (%q) and %d (%q)", code1, err1, code2, err2)
	}
	if err1 != "" || err2 != "" {
		t.Fatalf("unexpected stderr: %q / %q", err1, err2)
	}
	if strings.TrimSpace(out1) != strings.TrimSpace(out2) {
		t.Fatalf("file order changed the plan:\n%s\n----\n%s", out1, out2)
	}
	p := decodeCLIPlan(t, out1)
	domains := map[string]string{"a": "z1", "b": "z1", "c": "z2", "d": "z2", "e": "z3"}
	assertSpreadBatches(t, p, [][]string{{"a", "c"}, {"b", "d"}, {"e"}}, domains, 2)
}

// TestPlanCLI_SpreadByFilteringPrecedesTagCheck protects the ordering between
// selection filters and the fault-domain check: clusters that are disabled or
// removed by include/exclude need no spreadBy tag at all — their missing tag
// must not fail a plan that otherwise works. They still appear in the excluded
// list, ascending by ID, with their existing reasons. Exclude wins when a
// cluster matches both include and exclude.
func TestPlanCLI_SpreadByFilteringPrecedesTagCheck(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "zone",
  "include": [{"env": "prod"}],
  "exclude": [{"env": "temp"}, {"quarantine": "true"}],
  "clusters": [
    {"id": "s1", "tags": {"env": "prod", "zone": "z1"}},
    {"id": "tmp", "tags": {"env": "temp"}},
    {"id": "off", "disabled": true},
    {"id": "s2", "tags": {"env": "prod", "zone": "z2"}},
    {"id": "dev", "tags": {"env": "dev"}},
    {"id": "both", "tags": {"env": "prod", "quarantine": "true"}}
  ]
}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code != 0 {
		t.Fatalf("filtered-out clusters must not require the zone tag; exit=%d stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	domains := map[string]string{"s1": "z1", "s2": "z2"}
	assertSpreadBatches(t, p, [][]string{{"s1", "s2"}}, domains, 2)

	wantExcluded := []cliExcluded{
		{ID: "both", Reason: ReasonExcludeMatched},
		{ID: "dev", Reason: ReasonIncludeNotMatched},
		{ID: "off", Reason: ReasonDisabled},
		{ID: "tmp", Reason: ReasonExcludeMatched},
	}
	if len(p.Excluded) != len(wantExcluded) {
		t.Fatalf("excluded = %+v, want %+v", p.Excluded, wantExcluded)
	}
	for i, w := range wantExcluded {
		if p.Excluded[i] != w {
			t.Fatalf("excluded[%d] = %+v, want %+v (full list: %+v)", i, p.Excluded[i], w, p.Excluded)
		}
	}
}

// TestPlanCLI_SpreadBySelectedMissingTagFails covers the failure contract: if
// any selected cluster lacks the spreadBy tag, planning fails with a non-zero
// exit, stdout is completely empty (no partial batches are ever emitted) and
// stderr names both the smallest offending cluster and the tag.
func TestPlanCLI_SpreadBySelectedMissingTagFails(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "zone",
  "clusters": [
    {"id": "c-a", "tags": {"zone": "east"}},
    {"id": "c-c", "tags": {"region": "us"}},
    {"id": "c-b"}
  ]
}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q", stdout)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "c-b") {
		t.Fatalf("stderr must name the smallest offending cluster c-b, got %q", stderr)
	}
	if !strings.Contains(stderr, "zone") {
		t.Fatalf("stderr must name the missing tag zone, got %q", stderr)
	}
	if strings.Contains(stderr, "c-c") {
		t.Fatalf("with multiple offenders stderr must point at the smallest c-b, got %q", stderr)
	}
}

// TestPlanCLI_SpreadByEmptyTagValueIsDomain verifies through the CLI that a
// tag present with an empty string value is a legal fault domain, distinct
// from a missing tag: empty-valued clusters avoid each other but still share
// batches with other domains.
func TestPlanCLI_SpreadByEmptyTagValueIsDomain(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "zone",
  "clusters": [
    {"id": "c", "tags": {"zone": "east"}},
    {"id": "a", "tags": {"zone": ""}},
    {"id": "b", "tags": {"zone": ""}}
  ]
}`
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	domains := map[string]string{"a": "", "b": "", "c": "east"}
	// a and b share the empty domain and must not share a batch; c fills batch 1.
	assertSpreadBatches(t, p, [][]string{{"a", "c"}, {"b"}}, domains, 2)
}

// TestPlanCLI_NoSpreadByKeepsLegacyBatching verifies that omitting spreadBy
// or setting it to "" preserves the original behavior at the CLI level:
// ascending IDs split purely by capacity, so clusters of the same domain may
// share a batch. Both spellings must produce identical output.
func TestPlanCLI_NoSpreadByKeepsLegacyBatching(t *testing.T) {
	docs := map[string]string{
		"absent": `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2,
  "clusters": [
    {"id": "c3"},
    {"id": "c1", "tags": {"zone": "east"}},
    {"id": "c2", "tags": {"zone": "east"}}
  ]
}`,
		"empty": `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "",
  "clusters": [
    {"id": "c2", "tags": {"zone": "east"}},
    {"id": "c3"},
    {"id": "c1", "tags": {"zone": "east"}}
  ]
}`,
	}
	outputs := map[string]string{}
	for name, doc := range docs {
		code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, doc))
		if code != 0 {
			t.Fatalf("%s: expected exit 0, got %d; stderr=%q", name, code, stderr)
		}
		if stderr != "" {
			t.Fatalf("%s: unexpected stderr: %q", name, stderr)
		}
		outputs[name] = strings.TrimSpace(stdout)
	}
	if outputs["absent"] != outputs["empty"] {
		t.Fatalf("absent and empty spreadBy differ:\n%s\n----\n%s", outputs["absent"], outputs["empty"])
	}
	p := decodeCLIPlan(t, outputs["absent"])
	// Legacy packing ignores domains: the two same-domain clusters c1 and c2
	// share batch 1, ordered ascending; c3 takes batch 2.
	if len(p.Batches) != 2 {
		t.Fatalf("expected 2 batches, got %+v", p.Batches)
	}
	if strings.Join(p.Batches[0].Clusters, ",") != "c1,c2" {
		t.Fatalf("batch 1 = %v, want [c1 c2] (same domain may share a batch)", p.Batches[0].Clusters)
	}
	if p.Batches[0].Index != 1 || p.Batches[1].Index != 2 {
		t.Fatalf("batch indices must run from 1 consecutively, got %d and %d", p.Batches[0].Index, p.Batches[1].Index)
	}
	if strings.Join(p.Batches[1].Clusters, ",") != "c3" {
		t.Fatalf("batch 2 = %v, want [c3]", p.Batches[1].Clusters)
	}
}
