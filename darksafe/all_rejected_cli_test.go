package darksafe

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// planBinOnce builds the darksafe command once per test run; running the
// real binary (instead of `go run`, which appends its own "exit status 1"
// line to stderr) lets the tests assert the exact stderr a user receives.
var (
	planBinOnce sync.Once
	planBinPath string
)

// planBinary returns the path of the built darksafe command.

func planBinary(t *testing.T) string {
	t.Helper()
	planBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "darksafe-bin")
		if err != nil {
			t.Fatal(err)
		}
		planBinPath = filepath.Join(dir, "darksafe")
		cmd := exec.Command("go", "build", "-o", planBinPath, "./cmd/darksafe")
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to build darksafe: %v\n%s", err, out)
		}
	})
	return planBinPath
}

// runPlanCLI invokes the built darksafe binary with the given arguments and
// returns exit code, stdout and stderr — the exact streams a user sees.
func runPlanCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(planBinary(t), args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run command: %v", err)
		}
	}
	return code, stdout.String(), stderr.String()
}

// This file covers the command-line contract of the "no candidate passed
// filtering" failure: the library rules already have unit tests in
// plan_test.go (TestPlan_AllExcludedFailsWithReasons); here the same failure
// is observed through `darksafe plan <file>` exactly as a user drives it — a
// config file on disk, an empty stdout, the full per-candidate reason list on
// stderr and exit code 1. Everything runs offline: the binary never connects
// to a cluster or performs a release.

// allRejectedStderr renders the exact stderr contract for a fully rejected
// plan: the summary line followed by one "  <id>：<reason>" line per
// candidate, ascending by ID, terminated by the newline Fprintln appends.
func allRejectedStderr(entries ...string) string {
	var b strings.Builder
	b.WriteString("没有符合规则的可用集群，各候选集群未入选原因：")
	for _, e := range entries {
		b.WriteString("\n  ")
		b.WriteString(e)
	}
	b.WriteString("\n")
	return b.String()
}

// assertAllRejected checks the common failure shape: exit code 1, a
// completely empty stdout (no success plan, no partial batches) and stderr
// equal to want.
func assertAllRejected(t *testing.T, code int, stdout, stderr, want string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be completely empty when no cluster is selected, got %q", stdout)
	}
	if stderr != want {
		t.Fatalf("stderr mismatch:\n got %q\nwant %q", stderr, want)
	}
}

// TestPlanCLI_AllRejectedFailsWithFullReasons drives the complete failure
// contract through the CLI with a valid config whose candidates all lose the
// selection. The candidates exercise every rejection reason and its priority
// in one shared output: a disabled cluster that also matches an exclude
// condition stays 集群已停用; a cluster matching both include and exclude
// stays 命中排除条件; clusters matching no include condition are
// 未命中包含条件 — including one that satisfies only part of a multi-key
// include condition (all keys of a condition must match) while several
// conditions are available (only matching none of them rejects). The reason
// list is ascending by cluster ID with each candidate exactly once, and it
// must not depend on the order candidates are written in the file.
func TestPlanCLI_AllRejectedFailsWithFullReasons(t *testing.T) {
	// Sorted by ID the candidates are:
	//   m-partial  tags env=prod (only half of the first include condition)
	//              and tier=web (not edge): matches no condition fully.
	//   m-plain    matches no include condition at all.
	//   off-prio   disabled and also matches exclude {"env": "temp"}:
	//              disabled wins.
	//   x-both     matches include {"env": "prod", "region": "us"} and
	//              exclude {"quarantine": "true"}: exclude wins.
	//   x-temp     matches exclude {"env": "temp"} only.
	want := allRejectedStderr(
		"m-partial："+ReasonIncludeNotMatched,
		"m-plain："+ReasonIncludeNotMatched,
		"off-prio："+ReasonDisabled,
		"x-both："+ReasonExcludeMatched,
		"x-temp："+ReasonExcludeMatched,
	)

	template := `{
  "app": "payments",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments:2026.10.0-r3",
  "batchSize": 2,
  "include": [%s],
  "exclude": [%s],
  "clusters": [%s]
}`
	includeA := `{"env": "prod", "region": "us"}, {"tier": "edge"}`
	excludeA := `{"quarantine": "true"}, {"env": "temp"}`
	clustersA := `
    {"id": "x-temp", "tags": {"env": "temp"}},
    {"id": "m-partial", "tags": {"env": "prod", "tier": "web"}},
    {"id": "off-prio", "disabled": true, "tags": {"env": "temp"}},
    {"id": "x-both", "tags": {"env": "prod", "region": "us", "quarantine": "true"}},
    {"id": "m-plain", "tags": {"env": "dev"}}`
	// The same configuration with candidates and conditions written in a
	// different order; the failure output must be identical.
	includeB := `{"tier": "edge"}, {"region": "us", "env": "prod"}`
	excludeB := `{"env": "temp"}, {"quarantine": "true"}`
	clustersB := `
    {"id": "m-plain", "tags": {"env": "dev"}},
    {"id": "x-both", "tags": {"quarantine": "true", "region": "us", "env": "prod"}},
    {"id": "off-prio", "tags": {"env": "temp"}, "disabled": true},
    {"id": "m-partial", "tags": {"tier": "web", "env": "prod"}},
    {"id": "x-temp", "tags": {"env": "temp"}}`

	for name, doc := range map[string]string{
		"order-a": fmt.Sprintf(template, includeA, excludeA, clustersA),
		"order-b": fmt.Sprintf(template, includeB, excludeB, clustersB),
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
			assertAllRejected(t, code, stdout, stderr, want)
		})
	}
}

// TestPlanCLI_AllRejectedSpreadByKeepsFilterReasons protects the ordering
// between selection filters and the fault-domain check on the failure path:
// with spreadBy enabled, candidates that are all filtered out and also lack
// the spreadBy tag must still produce the full per-candidate rejection
// reasons — the failure must not turn into a missing-fault-domain-tag error.
func TestPlanCLI_AllRejectedSpreadByKeepsFilterReasons(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 2, "spreadBy": "zone",
  "include": [{"env": "prod"}],
  "exclude": [{"env": "temp"}],
  "clusters": [
    {"id": "c-temp", "tags": {"env": "temp"}},
    {"id": "c-off", "disabled": true},
    {"id": "c-dev", "tags": {"env": "dev"}}
  ]
}`
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
	want := allRejectedStderr(
		"c-dev："+ReasonIncludeNotMatched,
		"c-off："+ReasonDisabled,
		"c-temp："+ReasonExcludeMatched,
	)
	assertAllRejected(t, code, stdout, stderr, want)
	if strings.Contains(stderr, "故障域") || strings.Contains(stderr, "zone") {
		t.Fatalf("filtered-out clusters must not trigger the fault-domain tag check, got %q", stderr)
	}
}

// TestPlanCLI_AllRejectedSingleDisabledCandidate covers the minimal case:
// exactly one candidate, disabled. The failure names that candidate with the
// disabled reason, exits 1 and leaves stdout empty.
func TestPlanCLI_AllRejectedSingleDisabledCandidate(t *testing.T) {
	doc := `{
  "app": "app", "revision": "r1", "image": "img",
  "batchSize": 1,
  "clusters": [
    {"id": "only", "disabled": true}
  ]
}`
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, doc))
	assertAllRejected(t, code, stdout, stderr, allRejectedStderr("only："+ReasonDisabled))
}
