package darksafe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// parseDupErr parses data and returns the error; it fails the test if no
// error is returned.
func parseDupErr(t *testing.T, data string) error {
	t.Helper()
	_, err := ParseReleaseInput([]byte(data))
	if err == nil {
		t.Fatalf("expected duplicate-member error for %s", data)
	}
	return err
}

func TestParse_DuplicateMemberRejectedEverywhere(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name      string
		body      string // JSON body without outer braces
		wantField string
		wantPath  string
	}{
		{
			name:      "top-level app",
			body:      `"app":"a","app":"b","revision":"r","image":"i","batchSize":1,"clusters":[]`,
			wantField: "app",
			wantPath:  "$",
		},
		{
			name:      "top-level batchSize",
			body:      `"app":"a","revision":"r","image":"i","batchSize":1,"batchSize":2,"clusters":[]`,
			wantField: "batchSize",
			wantPath:  "$",
		},
		{
			name:      "cluster disabled",
			body:      base + `,"clusters":[{"id":"x","disabled":true,"disabled":false}]`,
			wantField: "disabled",
			wantPath:  "$.clusters[0]",
		},
		{
			name:      "cluster id",
			body:      base + `,"clusters":[{"id":"x","id":"y"}]`,
			wantField: "id",
			wantPath:  "$.clusters[0]",
		},
		{
			name:      "second cluster",
			body:      base + `,"clusters":[{"id":"a"},{"id":"b","disabled":false,"disabled":true}]`,
			wantField: "disabled",
			wantPath:  "$.clusters[1]",
		},
		{
			name:      "tags",
			body:      base + `,"clusters":[{"id":"x","tags":{"env":"prod","env":"dev"}}]`,
			wantField: "env",
			wantPath:  "$.clusters[0].tags",
		},
		{
			name:      "include condition",
			body:      base + `,"clusters":[],"include":[{"env":"prod","env":"dev"}]`,
			wantField: "env",
			wantPath:  "$.include[0]",
		},
		{
			name:      "exclude condition",
			body:      base + `,"clusters":[],"exclude":[{"region":"us","region":"eu"}]`,
			wantField: "region",
			wantPath:  "$.exclude[0]",
		},
		{
			name:      "second include condition",
			body:      base + `,"clusters":[],"include":[{"env":"prod"},{"env":"dev","env":"staging"}]`,
			wantField: "env",
			wantPath:  "$.include[1]",
		},
		{
			name:      "unknown field nested object",
			body:      base + `,"clusters":[],"meta":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  "$.meta",
		},
		{
			name:      "unknown field array of objects",
			body:      base + `,"clusters":[],"items":[{"y":1,"y":2}]`,
			wantField: "y",
			wantPath:  "$.items[0]",
		},
		{
			name:      "unknown field deep nesting",
			body:      base + `,"clusters":[],"a":{"b":{"c":[{"z":1,"z":2}]}}`,
			wantField: "z",
			wantPath:  "$.a.b.c[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseDupErr(t, "{"+tc.body+"}")
			msg := err.Error()
			if !strings.Contains(msg, tc.wantField) {
				t.Fatalf("error %q does not name field %q", msg, tc.wantField)
			}
			if !strings.Contains(msg, tc.wantPath) {
				t.Fatalf("error %q does not contain path %q", msg, tc.wantPath)
			}
		})
	}
}

func TestParse_DuplicateRejectedRegardlessOfValues(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// Same value twice must still be rejected.
	for _, val := range []string{`true`, `false`, `null`, `1`, `1.5`, `"str"`, `[1,2]`, `{"k":"v"}`} {
		t.Run("same value "+val, func(t *testing.T) {
			body := `{"id":"x","tags":{"env":` + val + `,"env":` + val + `}}`
			err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
			if !strings.Contains(err.Error(), "env") {
				t.Fatalf("expected field name in %q", err)
			}
		})
	}
	// Different value types on either side must be rejected.
	pairs := [][2]string{
		{`1`, `true`},
		{`"a"`, `null`},
		{`[]`, `{}`},
		{`false`, `"false"`},
	}
	for _, p := range pairs {
		t.Run("mixed "+p[0]+" "+p[1], func(t *testing.T) {
			body := `{"id":"x","tags":{"env":` + p[0] + `,"env":` + p[1] + `}}`
			err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
			if !strings.Contains(err.Error(), "env") {
				t.Fatalf("expected field name in %q", err)
			}
		})
	}
}

func TestParse_DuplicateNamesComparedAfterDecoding(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// "env" written directly vs. with its "v" written as a \u escape are the
	// same name once decoded.
	body := `{"id":"x","tags":{"env":"prod","env":"dev"}}`
	err := parseDupErr(t, `{`+base+`,"clusters":[`+body+`]}`)
	if !strings.Contains(err.Error(), `"env"`) {
		t.Fatalf("expected decoded field name \"env\" in %q", err)
	}

	// Case differences are different names.
	valid := `{` + base + `,"clusters":[{"id":"x","tags":{"env":"prod","ENV":"dev"}}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("case-different names must not be duplicates: %v", err)
	}
	// Spacing differences are different names (no trimming).
	valid = `{` + base + `,"clusters":[{"id":"x","tags":{" env":"prod","env":"dev"}}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("spacing-different names must not be duplicates: %v", err)
	}
}

func TestParse_DuplicateCheckIsPerObject(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// Two clusters may both have an "env" tag.
	valid := `{` + base + `,"clusters":[` +
		`{"id":"a","tags":{"env":"prod"}},` +
		`{"id":"b","tags":{"env":"dev"}}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("two clusters sharing a tag key must be valid: %v", err)
	}
	// Two conditions may both contain "region".
	valid = `{` + base + `,"clusters":[],` +
		`"include":[{"region":"us"},{"region":"eu"}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("two conditions sharing a key must be valid: %v", err)
	}
	// Repeated identical conditions are not duplicates.
	valid = `{` + base + `,"clusters":[],` +
		`"include":[{"env":"prod"},{"env":"prod"}]}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("repeated identical conditions must be valid: %v", err)
	}
	// Same key in parent object and nested object is fine.
	valid = `{` + base + `,"clusters":[],"meta":{"env":"x","nested":{"env":"y"}}}`
	if _, err := ParseReleaseInput([]byte(valid)); err != nil {
		t.Fatalf("same key in parent and nested object must be valid: %v", err)
	}
}

func TestParse_FirstDuplicateInFileOrderWins(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	// Cluster 1 has a duplicate disabled, cluster 0 has a duplicate id:
	// cluster 0's duplicate appears earlier in the file.
	doc := `{` + base + `,"clusters":[` +
		`{"id":"a","id":"b"},` +
		`{"id":"c","disabled":true,"disabled":false}]}`
	err := parseDupErr(t, doc)
	msg := err.Error()
	if !strings.Contains(msg, `"id"`) || !strings.Contains(msg, "$.clusters[0]") {
		t.Fatalf("expected clusters[0] id duplicate first, got %q", msg)
	}

	// Duplicate inside tags of cluster 0 appears before duplicate disabled
	// of cluster 1.
	doc = `{` + base + `,"clusters":[` +
		`{"id":"a","tags":{"env":1,"env":2}},` +
		`{"id":"c","disabled":true,"disabled":false}]}`
	err = parseDupErr(t, doc)
	msg = err.Error()
	if !strings.Contains(msg, `"env"`) || !strings.Contains(msg, "$.clusters[0].tags") {
		t.Fatalf("expected clusters[0].tags env duplicate first, got %q", msg)
	}

	// Duplicate in an earlier include condition beats a later cluster dup.
	doc = `{` + base + `,` +
		`"include":[{"env":"prod","env":"dev"}],` +
		`"clusters":[{"id":"a","disabled":true,"disabled":false}]}`
	err = parseDupErr(t, doc)
	msg = err.Error()
	if !strings.Contains(msg, `"env"`) || !strings.Contains(msg, "$.include[0]") {
		t.Fatalf("expected include[0] env duplicate first, got %q", msg)
	}
}

func TestParse_DuplicateBeatsBusinessErrors(t *testing.T) {
	// Duplicate member plus missing app: duplicate error must come first.
	doc := `{"revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","disabled":true,"disabled":false}]}`
	err := parseDupErr(t, doc)
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected duplicate error before business error, got %q", err)
	}

	// Duplicate plus empty tag key: duplicate wins.
	doc = `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","tags":{"env":1,"env":2,"":3}}]}`
	err = parseDupErr(t, doc)
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("expected duplicate error before tag-key error, got %q", err)
	}

	// With no duplicate, business errors are still reported as before.
	doc = `{"revision":"r","image":"i","batchSize":1,"clusters":[]}`
	if _, err := ParseReleaseInput([]byte(doc)); err == nil ||
		!strings.Contains(err.Error(), "app") {
		t.Fatalf("expected missing app error, got %v", err)
	}
}

func TestParse_ValidConfigsStillPlan(t *testing.T) {
	raw := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3", "tags": {"env": "prod"}},
			{"id": "c1", "tags": {"env": "dev"}},
			{"id": "c2", "disabled": true, "tags": {"env": "prod"}},
			{"id": "c4", "tags": {"env": "prod", "region": "us"}}
		],
		"include": [{"env": "prod"}],
		"exclude": [{"region": "us"}]
	}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	// c1 is dev -> include-not-matched; c2 disabled; c4 prod+us -> excluded;
	// c3 selected.
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c3" {
		t.Fatalf("unexpected plan: %+v", plan.Batches)
	}
	if len(plan.Excluded) != 3 {
		t.Fatalf("expected 3 excluded, got %+v", plan.Excluded)
	}
}

// runCLI invokes the darksafe command with the given arguments and returns
// exit code, stdout and stderr.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "./cmd/darksafe"}, args...)...)
	cmd.Dir = ".."
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

func TestPlanCLI_DuplicateMemberFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c1", "disabled": true, "disabled": false},
			{"id": "c2"}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "disabled") || !strings.Contains(stderr, "$.clusters[0]") {
		t.Fatalf("stderr should name field and position, got %q", stderr)
	}
}

func TestPlanCLI_ValidFileStillPlans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [{"id": "c2"}, {"id": "c1"}]
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, `"c1"`) || !strings.Contains(stdout, `"c2"`) {
		t.Fatalf("expected plan on stdout, got %q", stdout)
	}
}
