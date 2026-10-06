package darksafe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the regression net for the single step of OBTAINING the
// release configuration file at the command line — how `darksafe plan`
// receives the user-supplied path and what the user observes when no
// configuration can be obtained. The filtering, fault-domain batching and
// business-validation rules keep their own tests; here the same contract is
// observed through the real binary exactly as a user drives it: exit code,
// stdout and stderr. Everything runs offline.
//
// The contract pinned here:
//   - one valid path that yields a plan: exit 0, empty stderr, stdout holds
//     exactly one complete parseable plan JSON whose app info and batches
//     match that file;
//   - a name containing Chinese characters or spaces stays one argv element;
//   - a relative path is looked up against the invoking process's working
//     directory — a same-named valid file in another directory must never
//     leak its app or revision into the result;
//   - no path or more than one path is a usage error: exit 2, empty stdout,
//     the existing plan usage line on stderr, checked BEFORE any file is
//     read (a valid first path or a missing other path changes nothing);
//   - one path that cannot be read (missing, or a directory) is a file
//     error: exit 1, empty stdout, stderr says the file cannot be read and
//     carries the offending path and the environment's concrete reason,
//     never a JSON-format or filtering error, a built-in demo, an empty
//     plan or a success status.

// planUsageStderr is the existing one-line usage of the plan subcommand,
// including the newline Fprintln appends.
const planUsageStderr = "用法: darksafe plan <JSON文件路径>\n"

// runPlanCLIIn is runPlanCLI with an explicit working directory for the
// child process; an empty dir means the current directory. A relative path
// argument is then resolved by the child against dir exactly as a shell
// invocation from that directory would resolve it.
func runPlanCLIIn(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(planBinary(t), args...)
	if dir != "" {
		cmd.Dir = dir
	}
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

// assertPlanUsageError pins the usage-failure shape shared by the wrong
// argument count cases: exit code 2, completely empty stdout (no plan is
// ever selected from one of the paths) and exactly the existing usage line.
func assertPlanUsageError(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 2 {
		t.Fatalf("a wrong argument count must exit 2, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on a usage error, got %q", stdout)
	}
	if stderr != planUsageStderr {
		t.Fatalf("stderr must be the existing plan usage:\n got %q\nwant %q", stderr, planUsageStderr)
	}
}

// assertPlanReadFailure pins the cannot-obtain-file shape: exit code 1,
// empty stdout (no JSON error, no empty plan, no demo fallback), and stderr
// that states the file cannot be read and quotes the offending path
// followed by a concrete environment-supplied reason. The reason wording is
// allowed to vary, but it must be present and the failure must not be
// relabeled as a JSON or filtering problem.
func assertPlanReadFailure(t *testing.T, code int, stdout, stderr, path string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("an unreadable config path must exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty when the file cannot be read, got %q", stdout)
	}
	body := strings.TrimSpace(stderr)
	if !strings.HasPrefix(body, "无法读取文件") {
		t.Fatalf("stderr must state the file cannot be read, got %q", stderr)
	}
	if !strings.Contains(stderr, path) {
		t.Fatalf("stderr must carry the offending path %q, got %q", path, stderr)
	}
	// Everything after the echoed path is the environment's reason, e.g.
	// ": no such file or directory" (missing) or ": is a directory". Its
	// wording is environment-specific, but it must not be empty.
	at := strings.Index(stderr, path)
	reason := strings.TrimSpace(strings.TrimLeft(stderr[at+len(path):], ": "))
	if reason == "" {
		t.Fatalf("stderr must include the concrete read failure reason after the path, got %q", stderr)
	}
	for _, mislabeled := range []string{
		"JSON 格式错误",
		"没有符合规则的可用集群",
		"没有可发布的集群",
		planUsageStderr,
	} {
		if strings.Contains(stderr, mislabeled) {
			t.Fatalf("a read failure must not be reported as %q; stderr=%q", mislabeled, stderr)
		}
	}
}

// configAcquisitionDoc is a valid config whose candidates are deliberately
// out of order, so a successful read can be checked end to end: app info
// echoed verbatim and batches sorted and split by batchSize.
const configAcquisitionDoc = `{
  "app": "payments",
  "revision": "2026.10.0-r7",
  "image": "registry.example.net/payments:2026.10.0-r7",
  "batchSize": 2,
  "clusters": [
    {"id": "c3"},
    {"id": "c1"},
    {"id": "c2"}
  ]
}`

// assertAcquisitionPlan verifies stdout is exactly one complete plan JSON
// matching configAcquisitionDoc, on the success channels: callers first
// assert exit 0 and empty stderr.
func assertAcquisitionPlan(t *testing.T, stdout string) cliPlan {
	t.Helper()
	p := decodeCLIPlan(t, stdout)
	if p.App.Name != "payments" || p.App.Revision != "2026.10.0-r7" ||
		p.App.Image != "registry.example.net/payments:2026.10.0-r7" {
		t.Fatalf("app info must come from the supplied file, got %+v", p.App)
	}
	if len(p.Batches) != 2 ||
		p.Batches[0].Index != 1 || strings.Join(p.Batches[0].Clusters, ",") != "c1,c2" ||
		p.Batches[1].Index != 2 || strings.Join(p.Batches[1].Clusters, ",") != "c3" {
		t.Fatalf("batches must match the file's sorted candidates (batchSize 2), got %+v", p.Batches)
	}
	if len(p.Excluded) != 0 {
		t.Fatalf("all candidates are selected, unexpected excluded list: %+v", p.Excluded)
	}
	return p
}

// TestPlanCLI_OneValidPathPrintsPlan is the success contract for obtaining a
// configuration file: exit 0, empty stderr, and stdout holding exactly one
// complete parseable plan whose app info and batches match the file.
func TestPlanCLI_OneValidPathPrintsPlan(t *testing.T) {
	code, stdout, stderr := runPlanCLI(t, "plan", writePlanDoc(t, configAcquisitionDoc))
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr must be empty on success, got %q", stderr)
	}
	assertAcquisitionPlan(t, stdout)
}

// TestPlanCLI_ChineseAndSpacedFilenameStaysSingleArg proves a path
// containing Chinese characters and spaces is received as exactly one
// argument: splitting it would arrive as multiple paths and fail as a usage
// error, while here the file is read whole and produces its plan.
func TestPlanCLI_ChineseAndSpacedFilenameStaysSingleArg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "发布 计划.json")
	if err := os.WriteFile(path, []byte(configAcquisitionDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runPlanCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("a Chinese-and-space file name must be one usable path, exit=%d stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr must be empty on success, got %q", stderr)
	}
	assertAcquisitionPlan(t, stdout)
}

// TestPlanCLI_RelativePathResolvesFromProcessCwd proves a bare file name is
// resolved against the invoking process's working directory and nowhere
// else: the same-named valid config in another directory must never
// contribute its app, revision or clusters; and an unrelated working
// directory without the file fails instead of finding that other copy.
func TestPlanCLI_RelativePathResolvesFromProcessCwd(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	other := filepath.Join(root, "other")
	for _, dir := range []string{work, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	workDoc := `{
  "app": "cwd-app", "revision": "cwd-r1", "image": "img-cwd",
  "batchSize": 1,
  "clusters": [{"id": "c1"}]
}`
	otherDoc := `{
  "app": "other-app", "revision": "other-r2", "image": "img-other",
  "batchSize": 1,
  "clusters": [{"id": "z9"}]
}`
	if err := os.WriteFile(filepath.Join(work, "release.json"), []byte(workDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "release.json"), []byte(otherDoc), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runPlanCLIIn(t, work, "plan", "release.json")
	if code != 0 {
		t.Fatalf("from work: expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("from work: stderr must be empty, got %q", stderr)
	}
	p := decodeCLIPlan(t, stdout)
	if p.App != (cliApp{Name: "cwd-app", Revision: "cwd-r1", Image: "img-cwd"}) {
		t.Fatalf("from work: plan must come from the cwd file, got %+v", p.App)
	}
	if strings.Join(p.Batches[0].Clusters, ",") != "c1" {
		t.Fatalf("from work: batches must be cwd file's [c1], got %+v", p.Batches)
	}
	for _, foreign := range []string{"other-app", "other-r2", "img-other", "z9"} {
		if strings.Contains(stdout, foreign) {
			t.Fatalf("from work: result must not mix in the other directory's config, stdout contains %q", foreign)
		}
	}

	// The inverse direction: from the other directory the same bare name
	// must yield the other file, never work's.
	code, stdout, stderr = runPlanCLIIn(t, other, "plan", "release.json")
	if code != 0 {
		t.Fatalf("from other: expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("from other: stderr must be empty, got %q", stderr)
	}
	p = decodeCLIPlan(t, stdout)
	if p.App != (cliApp{Name: "other-app", Revision: "other-r2", Image: "img-other"}) {
		t.Fatalf("from other: plan must come from the cwd file, got %+v", p.App)
	}
	if strings.Join(p.Batches[0].Clusters, ",") != "z9" {
		t.Fatalf("from other: batches must be other file's [z9], got %+v", p.Batches)
	}
	for _, foreign := range []string{"cwd-app", "cwd-r1", "img-cwd"} {
		if strings.Contains(stdout, foreign) {
			t.Fatalf("from other: result must not mix in the work directory's config, stdout contains %q", foreign)
		}
	}

	// A third directory holding no such file must fail: the existence of
	// same-named valid configs elsewhere must not act as a search path.
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runPlanCLIIn(t, empty, "plan", "release.json")
	assertPlanReadFailure(t, code, stdout, stderr, "release.json")
}

// TestPlanCLI_NoPathIsUsageError: with no path the command is a usage error
// checked before any file access — exit 2, empty stdout, the usage line.
func TestPlanCLI_NoPathIsUsageError(t *testing.T) {
	code, stdout, stderr := runPlanCLI(t, "plan")
	assertPlanUsageError(t, code, stdout, stderr)
}

// TestPlanCLI_MultiplePathsIsUsageError covers both "do not pick the valid
// first path" and "a missing other path must not be reported first": the
// argument count is wrong, so the usage error wins and no file is read.
func TestPlanCLI_MultiplePathsIsUsageError(t *testing.T) {
	first := writePlanDoc(t, configAcquisitionDoc)
	second := writePlanDoc(t, `{
  "app": "second", "revision": "r2", "image": "img2",
  "batchSize": 1, "clusters": [{"id": "q9"}]
}`)
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")

	cases := map[string][]string{
		"two valid paths, first would work": {first, second},
		"valid first path, second missing":  {first, missing},
		"same valid path twice":             {first, first},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runPlanCLI(t, append([]string{"plan"}, paths...)...)
			assertPlanUsageError(t, code, stdout, stderr)
			for _, leaked := range []string{"payments", "second", "无法读取文件"} {
				if strings.Contains(stderr, leaked) || strings.Contains(stdout, leaked) {
					t.Fatalf("no path may be read or planned on a count error; leaked %q (stdout=%q stderr=%q)", leaked, stdout, stderr)
				}
			}
		})
	}
}

// TestPlanCLI_MissingFileFailsWithPathAndReason: one path that does not
// exist is a file error — exit 1, empty stdout, stderr names the failure,
// the exact path (Chinese characters and spaces kept whole) and a concrete
// reason. It must never look like a JSON or filtering failure.
func TestPlanCLI_MissingFileFailsWithPathAndReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "不存在 的目录", "发布计划.json")
	code, stdout, stderr := runPlanCLI(t, "plan", path)
	assertPlanReadFailure(t, code, stdout, stderr, path)
}

// TestPlanCLI_DirectoryPathFailsWithPathAndReason: a path naming a directory
// is not a readable configuration file — the same file-error contract as a
// missing path, with the directory-specific reason, never an empty plan or
// success.
func TestPlanCLI_DirectoryPathFailsWithPathAndReason(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runPlanCLI(t, "plan", dir)
	assertPlanReadFailure(t, code, stdout, stderr, dir)
}
