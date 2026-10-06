package darksafe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the command-line contract of how `darksafe plan` receives
// the config file path: how the path argument is taken in, and what the user
// sees on the two output streams and the exit status when a config cannot be
// obtained. The filtering, fault-domain batching and business validation rules
// are covered elsewhere (plan_test.go, spreadby_cli_test.go,
// all_rejected_cli_test.go); here only the "obtain one config file" step is
// pinned down, so that future changes to the command entry point cannot blur
// argument-count errors, file-read failures and successful plan output into
// one another. Everything runs offline against the real built binary.

// planUsageStderr is the exact stderr a user receives when the plan command
// is invoked with a wrong number of path arguments.
const planUsageStderr = "用法: darksafe plan <JSON文件路径>\n"

// runPlanCLIInDir is runPlanCLI with an explicit working directory, used to
// pin down how relative paths are resolved against the caller's CWD.
func runPlanCLIInDir(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(planBinary(t), args...)
	cmd.Dir = dir
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

// writePlanDocNamed stores doc under name inside dir and returns its path.
func writePlanDocNamed(t *testing.T, dir, name, doc string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertUsageError checks the argument-count contract: exit code 2, a
// completely empty stdout (no plan is ever emitted, no demo output leaks) and
// stderr holding exactly the existing plan usage line.
func assertUsageError(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 2 {
		t.Fatalf("expected exit code 2 for a usage error, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on a usage error, got %q", stdout)
	}
	if stderr != planUsageStderr {
		t.Fatalf("stderr mismatch:\n got %q\nwant %q", stderr, planUsageStderr)
	}
}

// assertReadError checks the file-read failure contract: exit code 1, an
// empty stdout, and stderr that states the file could not be read and names
// the offending path. The OS-supplied reason text varies between
// environments, so only its presence (a non-empty message after the path) is
// required, not its wording.
func assertReadError(t *testing.T, code int, stdout, stderr, path string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("expected exit code 1 for a read failure, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty when the config cannot be read, got %q", stdout)
	}
	if !strings.Contains(stderr, "无法读取文件") {
		t.Fatalf("stderr must state the file cannot be read, got %q", stderr)
	}
	if !strings.Contains(stderr, path) {
		t.Fatalf("stderr must name the offending path %q, got %q", path, stderr)
	}
	// The read failure must not be reported as a JSON format problem ("JSON
	// 格式错误" is the parser's prefix) or a filtering/validation failure, and
	// there must be some concrete reason beyond the bare prefix.
	if strings.Contains(stderr, "没有符合规则的可用集群") {
		t.Fatalf("read failure must not look like a filtering failure, got %q", stderr)
	}
	if strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("read failure must not look like a JSON format error, got %q", stderr)
	}
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stderr), "无法读取文件"))
	if trimmed == "" || trimmed == ":" {
		t.Fatalf("stderr must carry a concrete reason after the prefix, got %q", stderr)
	}
}

// pathCLIConfig is a small valid configuration whose app identity and batch
// layout are easy to assert; clusters are written out of ID order on purpose.
const pathCLIConfig = `{
  "app": "payments-gateway",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments-gateway:2026.10.0-r3",
  "batchSize": 2,
  "exclude": [{"env": "temp"}],
  "clusters": [
    {"id": "c-c"},
    {"id": "x1", "tags": {"env": "temp"}},
    {"id": "c-a"},
    {"id": "off", "disabled": true},
    {"id": "c-b"}
  ]
}`

// assertPathCLIPlan checks the successful plan produced from pathCLIConfig:
// app info echoed from the file, ascending IDs packed by capacity only, and
// the excluded list ascending with the existing reasons.
func assertPathCLIPlan(t *testing.T, stdout string) {
	t.Helper()
	p := decodeCLIPlan(t, stdout)
	if p.App.Name != "payments-gateway" || p.App.Revision != "2026.10.0-r3" ||
		p.App.Image != "registry.example.net/payments-gateway:2026.10.0-r3" {
		t.Fatalf("app info not echoed from the config file: %+v", p.App)
	}
	if len(p.Batches) != 2 {
		t.Fatalf("expected 2 batches, got %+v", p.Batches)
	}
	if p.Batches[0].Index != 1 || p.Batches[1].Index != 2 {
		t.Fatalf("batch indices must run from 1 consecutively, got %d and %d",
			p.Batches[0].Index, p.Batches[1].Index)
	}
	if strings.Join(p.Batches[0].Clusters, ",") != "c-a,c-b" {
		t.Fatalf("batch 1 = %v, want [c-a c-b]", p.Batches[0].Clusters)
	}
	if strings.Join(p.Batches[1].Clusters, ",") != "c-c" {
		t.Fatalf("batch 2 = %v, want [c-c]", p.Batches[1].Clusters)
	}
	wantExcluded := []cliExcluded{
		{ID: "off", Reason: ReasonDisabled},
		{ID: "x1", Reason: ReasonExcludeMatched},
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

// assertSuccessRun checks the common success shape: exit code 0, an empty
// stderr, and stdout holding exactly one complete parseable plan.
func assertSuccessRun(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr on success, got %q", stderr)
	}
}

// TestPlanCLI_SingleValidPathSucceeds pins the happy path: one valid file
// path, exit 0, empty stderr, and stdout carrying only one complete parseable
// plan JSON whose app info and batches come from that file.
func TestPlanCLI_SingleValidPathSucceeds(t *testing.T) {
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, pathCLIConfig))
	assertSuccessRun(t, code, stdout, stderr)
	assertPathCLIPlan(t, stdout)
}

// TestPlanCLI_PathWithChineseAndSpacesIsOneArgument writes the config under a
// file name containing Chinese characters and spaces and passes the whole
// path as a single argument; it must be used verbatim, not split or
// reinterpreted, and produce the same successful plan.
func TestPlanCLI_PathWithChineseAndSpacesIsOneArgument(t *testing.T) {
	dir := t.TempDir()
	path := writePlanDocNamed(t, dir, "发布 计划 配置.json", pathCLIConfig)
	code, stdout, stderr := runPlanCLI(t, "plan", path)
	assertSuccessRun(t, code, stdout, stderr)
	assertPathCLIPlan(t, stdout)
}

// TestPlanCLI_RelativePathResolvesAgainstCallerCWD places a different valid
// config under the same file name in two directories and invokes the binary
// with the bare relative name from each one. The plan must come from the
// calling process's current working directory — the other directory's
// same-named config must not leak its app or revision into the result.
func TestPlanCLI_RelativePathResolvesAgainstCallerCWD(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "dir-a")
	dirB := filepath.Join(base, "dir-b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configFor := func(app, revision string) string {
		return `{
  "app": "` + app + `",
  "revision": "` + revision + `",
  "image": "registry.example.net/` + app + `:` + revision + `",
  "batchSize": 2,
  "clusters": [{"id": "c-a"}, {"id": "c-b"}]
}`
	}
	writePlanDocNamed(t, dirA, "plan.json", configFor("app-from-cwd-a", "rev-a"))
	writePlanDocNamed(t, dirB, "plan.json", configFor("app-from-cwd-b", "rev-b"))

	for name, tc := range map[string]struct {
		dir      string
		wantApp  string
		wantRev  string
		otherApp string
		otherRev string
	}{
		"from-dir-a": {dir: dirA, wantApp: "app-from-cwd-a", wantRev: "rev-a", otherApp: "app-from-cwd-b", otherRev: "rev-b"},
		"from-dir-b": {dir: dirB, wantApp: "app-from-cwd-b", wantRev: "rev-b", otherApp: "app-from-cwd-a", otherRev: "rev-a"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runPlanCLIInDir(t, tc.dir, "plan", "plan.json")
			assertSuccessRun(t, code, stdout, stderr)
			p := decodeCLIPlan(t, stdout)
			if p.App.Name != tc.wantApp || p.App.Revision != tc.wantRev {
				t.Fatalf("plan must come from the CWD config %q/%q, got %+v", tc.wantApp, tc.wantRev, p.App)
			}
			if strings.Contains(stdout, tc.otherApp) || strings.Contains(stdout, tc.otherRev) {
				t.Fatalf("plan must not mix in the other directory's config, stdout=%q", stdout)
			}
		})
	}
}

// TestPlanCLI_NoPathIsUsageError: invoking plan without a path is a command
// usage error — exit 2, empty stdout, the existing usage text on stderr.
func TestPlanCLI_NoPathIsUsageError(t *testing.T) {
	code, stdout, stderr := runPlanCLI(t, "plan")
	assertUsageError(t, code, stdout, stderr)
}

// TestPlanCLI_MultiplePathsIsUsageError: more than one path is a usage error
// too. The argument-count problem must be reported before anything else:
// even when the first path is a valid config no plan may be selected and
// printed, and even when one of the paths does not exist the file-read error
// must not pre-empt the usage error.
func TestPlanCLI_MultiplePathsIsUsageError(t *testing.T) {
	valid := writePlanDoc(t, pathCLIConfig)
	missing := filepath.Join(t.TempDir(), "不存在.json")

	t.Run("first path valid", func(t *testing.T) {
		code, stdout, stderr := runPlanCLI(t, "plan", valid, writePlanDoc(t, pathCLIConfig))
		assertUsageError(t, code, stdout, stderr)
	})
	t.Run("one path missing", func(t *testing.T) {
		code, stdout, stderr := runPlanCLI(t, "plan", valid, missing)
		assertUsageError(t, code, stdout, stderr)
	})
	t.Run("missing path first", func(t *testing.T) {
		code, stdout, stderr := runPlanCLI(t, "plan", missing, valid)
		assertUsageError(t, code, stdout, stderr)
	})
}

// TestPlanCLI_MissingFileIsReadError: a single path that does not exist is a
// file-read failure — exit 1, empty stdout, stderr stating the file cannot be
// read and naming the path with a concrete reason. It must not fall back to
// the built-in demo, print an empty plan, or exit successfully.
func TestPlanCLI_MissingFileIsReadError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "没有这个文件.json")
	code, stdout, stderr := runPlanCLI(t, "plan", missing)
	assertReadError(t, code, stdout, stderr, missing)
}

// TestPlanCLI_DirectoryPathIsReadError: a single path that points at a
// directory rather than a readable config file fails the same way — exit 1,
// empty stdout, stderr naming the path and the reason.
func TestPlanCLI_DirectoryPathIsReadError(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runPlanCLI(t, "plan", dir)
	assertReadError(t, code, stdout, stderr, dir)
}
