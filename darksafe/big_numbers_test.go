package darksafe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Big JSON numbers (e.g. 1e400) are syntactically valid but overflow float64.
// In fields that take no part in release rules they must not affect parsing,
// filtering, or batching.

func TestParse_BigNumberInUnknownFieldsAccepted(t *testing.T) {
	base := `{
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
	// Same document plus big numbers at the top level, in a nested object,
	// and inside an array of an unknown field.
	withBig := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [
			{"id": "c3", "tags": {"env": "prod"}},
			{"id": "c1", "tags": {"env": "dev"}},
			{"id": "c2", "disabled": true, "tags": {"env": "prod"}},
			{"id": "c4", "tags": {"env": "prod", "region": "us"}}
		],
		"include": [{"env": "prod"}],
		"exclude": [{"region": "us"}],
		"quota": 1e400,
		"meta": {"limit": 1e400, "history": [1e400, {"peak": 1e400}]}
	}`
	want, err := MakeReleasePlan(parsePlan(t, base))
	if err != nil {
		t.Fatal(err)
	}
	got, err := MakeReleasePlan(parsePlan(t, withBig))
	if err != nil {
		t.Fatalf("big numbers in unknown fields must be accepted: %v", err)
	}
	if len(got.Batches) != len(want.Batches) || len(got.Excluded) != len(want.Excluded) ||
		got.App != want.App {
		t.Fatalf("plans differ:\n with: %+v\n without: %+v", got, want)
	}
	for i := range want.Batches {
		if strings.Join(got.Batches[i].Clusters, ",") != strings.Join(want.Batches[i].Clusters, ",") {
			t.Fatalf("batch %d differs: %+v vs %+v", i, got.Batches[i], want.Batches[i])
		}
	}
	for i := range want.Excluded {
		if got.Excluded[i] != want.Excluded[i] {
			t.Fatalf("excluded %d differs: %+v vs %+v", i, got.Excluded[i], want.Excluded[i])
		}
	}
}

func TestParse_BigNumberInBatchSizeIsFieldError(t *testing.T) {
	doc := `{"app":"a","revision":"r","image":"i","batchSize":1e400,"clusters":[{"id":"c1"}]}`
	_, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatal("expected batchSize error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "batchSize") || !strings.Contains(msg, "正整数") {
		t.Fatalf("expected positive-integer batchSize error, got %q", msg)
	}
	if strings.Contains(msg, "JSON 格式错误") {
		t.Fatalf("must not be reported as JSON syntax error, got %q", msg)
	}
}

func TestParse_BigNumberInTagOrConditionIsTypeError(t *testing.T) {
	cases := map[string]string{
		"tag value":       `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x","tags":{"env":1e400}}]}`,
		"include value":   `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"include":[{"env":1e400}]}`,
		"exclude value":   `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"exclude":[{"env":1e400}]}`,
		"plain tag value": `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x","tags":{"env":2}}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseReleaseInput([]byte(doc))
			if err == nil {
				t.Fatal("expected type error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "必须是字符串") {
				t.Fatalf("expected string-type error, got %q", msg)
			}
			if strings.Contains(msg, "JSON 格式错误") {
				t.Fatalf("must not be reported as JSON syntax error, got %q", msg)
			}
		})
	}
}

func TestParse_DuplicateWithBigNumberFirstValue(t *testing.T) {
	// The duplicate must be reported even though the first value overflows
	// float64; the walk must not stop at the big number.
	doc := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],` +
		`"meta":{"limit":1e400,"limit":2}}`
	err := parseDupErr(t, doc)
	msg := err.Error()
	if !strings.Contains(msg, `"limit"`) || !strings.Contains(msg, "$.meta") {
		t.Fatalf("expected limit duplicate at $.meta, got %q", msg)
	}

	// Duplicate beats a missing-app business error even with a big number.
	doc = `{"revision":"r","image":"i","batchSize":1,"clusters":[],` +
		`"meta":{"limit":1e400,"limit":2}}`
	err = parseDupErr(t, doc)
	if !strings.Contains(err.Error(), `"limit"`) {
		t.Fatalf("expected duplicate error before business error, got %q", err)
	}
}

func TestParse_InvalidNumberStillRejected(t *testing.T) {
	for _, doc := range []string{
		`{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"meta":{"limit":1e}}`,
		`{"app":"a","revision":"r","image":"i","batchSize":1e,"clusters":[]}`,
		`{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"extra":[1e]}`,
	} {
		_, err := ParseReleaseInput([]byte(doc))
		if err == nil {
			t.Fatalf("expected JSON syntax error for %s", doc)
		}
		if !strings.Contains(err.Error(), "JSON 格式错误") {
			t.Fatalf("expected JSON 格式错误 for %s, got %q", doc, err)
		}
	}
}

func TestPlanCLI_BigNumberInUnknownFieldsPlans(t *testing.T) {
	dir := t.TempDir()
	withBig := filepath.Join(dir, "with.json")
	plain := filepath.Join(dir, "plain.json")
	common := `"app":"payments","revision":"v1","image":"img","batchSize":2,
		"clusters":[{"id":"c3","tags":{"env":"prod"}},{"id":"c1","tags":{"env":"dev"}},
		{"id":"c2","disabled":true,"tags":{"env":"prod"}}],
		"include":[{"env":"prod"}]`
	if err := os.WriteFile(withBig, []byte(`{`+common+`,"quota":1e400,"meta":{"limit":1e400},"list":[1e400]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte(`{`+common+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", withBig)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	code2, stdout2, _ := runCLI(t, "plan", plain)
	if code2 != 0 {
		t.Fatalf("baseline failed: %q", stdout2)
	}
	if stdout != stdout2 {
		t.Fatalf("plan with big-number extras differs:\n with: %s\n without: %s", stdout, stdout2)
	}
	if !strings.Contains(stdout, `"c3"`) || !strings.Contains(stdout, ReasonDisabled) {
		t.Fatalf("expected full plan on stdout, got %q", stdout)
	}
}

func TestPlanCLI_BigNumberInBatchSizeFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"a","revision":"r","image":"i","batchSize":1e400,"clusters":[{"id":"c1"}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q", stdout)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "batchSize") || !strings.Contains(stderr, "正整数") {
		t.Fatalf("stderr should explain batchSize must be a positive integer, got %q", stderr)
	}
	if strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("stderr must not blame JSON syntax, got %q", stderr)
	}
}
