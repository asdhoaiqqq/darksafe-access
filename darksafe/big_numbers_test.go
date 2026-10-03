package darksafe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A legal JSON number outside float64 range (e.g. 1e400) must not be mistaken
// for a JSON syntax error. In a field that does not participate in release
// rules — at top level, inside a nested object, or inside an array — it is
// ignored just like any other unknown field, and the plan is identical to
// the document with that field removed.
func TestParse_HugeNumberInExtraFieldAccepted(t *testing.T) {
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
	withExtra := []string{
		// top-level extra field
		`{"app":"payments","revision":"v1.2.3","image":"reg/payments:v1.2.3","batchSize":2,` +
			`"clusters":[{"id":"c3","tags":{"env":"prod"}},{"id":"c1","tags":{"env":"dev"}},{"id":"c2","disabled":true,"tags":{"env":"prod"}},{"id":"c4","tags":{"env":"prod","region":"us"}}],` +
			`"include":[{"env":"prod"}],"exclude":[{"region":"us"}],"limit":1e400}`,
		// nested inside an extra object
		`{"app":"payments","revision":"v1.2.3","image":"reg/payments:v1.2.3","batchSize":2,` +
			`"clusters":[{"id":"c3","tags":{"env":"prod"}},{"id":"c1","tags":{"env":"dev"}},{"id":"c2","disabled":true,"tags":{"env":"prod"}},{"id":"c4","tags":{"env":"prod","region":"us"}}],` +
			`"include":[{"env":"prod"}],"exclude":[{"region":"us"}],"meta":{"limit":1e400}}`,
		// inside an array and a deeply nested object
		`{"app":"payments","revision":"v1.2.3","image":"reg/payments:v1.2.3","batchSize":2,` +
			`"clusters":[{"id":"c3","tags":{"env":"prod"}},{"id":"c1","tags":{"env":"dev"}},{"id":"c2","disabled":true,"tags":{"env":"prod"}},{"id":"c4","tags":{"env":"prod","region":"us"}}],` +
			`"include":[{"env":"prod"}],"exclude":[{"region":"us"}],"notes":[1,{"deep":[1e400]},"x"]}`,
		// extra field inside a cluster object
		`{"app":"payments","revision":"v1.2.3","image":"reg/payments:v1.2.3","batchSize":2,` +
			`"clusters":[{"id":"c1","w":1e400},{"id":"c2"}]}`,
	}
	basePlan, err := MakeReleasePlan(parsePlan(t, base))
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range withExtra {
		in, err := ParseReleaseInput([]byte(raw))
		if err != nil {
			t.Fatalf("case %d: extra huge number must be accepted, got %v", i, err)
		}
		plan, err := MakeReleasePlan(in)
		if err != nil {
			t.Fatalf("case %d: plan failed: %v", i, err)
		}
		if i < 3 && planSummary(plan) != planSummary(basePlan) {
			t.Fatalf("case %d: plan differs from the document without the extra field:\n got %+v\nwant %+v", i, plan, basePlan)
		}
	}
}

func planSummary(p ReleasePlan) string {
	var b strings.Builder
	b.WriteString(p.App.Name + "/" + p.App.Revision + "/" + p.App.Image)
	for _, batch := range p.Batches {
		b.WriteString("|b" + strings.Join(batch.Clusters, ","))
	}
	for _, e := range p.Excluded {
		b.WriteString("|x" + e.ID + ":" + e.Reason)
	}
	return b.String()
}

// A huge number in a business field is judged by that field's own rules:
// batchSize must stay a positive integer, tag/condition values must stay
// strings — never reported as a JSON format error, never truncated or
// replaced by a default.
func TestParse_HugeNumberInBusinessFieldGivesFieldError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "batchSize 1e400",
			raw:  `{"app":"a","revision":"r","image":"i","batchSize":1e400,"clusters":[{"id":"x"}]}`,
			want: `字段 "batchSize" 必须是正整数`,
		},
		{
			name: "batchSize huge integer literal",
			raw:  `{"app":"a","revision":"r","image":"i","batchSize":99999999999999999999,"clusters":[{"id":"x"}]}`,
			want: `字段 "batchSize" 必须是正整数`,
		},
		{
			name: "batchSize tiny 1e-400",
			raw:  `{"app":"a","revision":"r","image":"i","batchSize":1e-400,"clusters":[{"id":"x"}]}`,
			want: `字段 "batchSize" 必须是正整数`,
		},
		{
			name: "tag value huge number",
			raw:  `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x","tags":{"env":1e400}}]}`,
			want: `集群 "x" 的标签 "env" 值必须是字符串`,
		},
		{
			name: "include value huge number",
			raw:  `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"include":[{"env":1e400}]}`,
			want: `include[0] 的标签 "env" 值必须是字符串`,
		},
		{
			name: "exclude value huge number",
			raw:  `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"exclude":[{"env":1e400}]}`,
			want: `exclude[0] 的标签 "env" 值必须是字符串`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReleaseInput([]byte(tc.raw))
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), "JSON 格式错误") {
				t.Fatalf("must not be reported as a JSON format error, got %v", err)
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// Integer batch sizes keep their original value; big valid integers are not
// silently rounded down to some default or float-representable value.
func TestParse_IntegerBatchSizePreserved(t *testing.T) {
	in := parsePlan(t, `{"app":"a","revision":"r","image":"i","batchSize":7,`+
		`"clusters":[{"id":"c1"},{"id":"c2"},{"id":"c3"}],"meta":1e400}`)
	if in.BatchSize != 7 {
		t.Fatalf("batchSize = %d, want 7", in.BatchSize)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c1,c2,c3" {
		t.Fatalf("all three clusters should fit one batch of 7, got %+v", plan.Batches)
	}
}

// Duplicate-member detection runs before business validation and must not be
// derailed by a huge first value: the duplicate name and its object position
// are reported even when the app field is also missing, and even when the
// first value cannot fit in float64.
func TestParse_DuplicateMemberWithHugeFirstValue(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantField string
		wantPath  string
	}{
		{
			name:      "unknown extra object, app missing",
			raw:       `{"revision":"r","image":"i","batchSize":1,"clusters":[],"extra":{"limit":1e400,"limit":2}}`,
			wantField: "limit",
			wantPath:  "$.extra",
		},
		{
			name:      "nested object",
			raw:       `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"meta":{"limit":1e400,"limit":2}}`,
			wantField: "limit",
			wantPath:  "$.meta",
		},
		{
			name:      "inside array element",
			raw:       `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"items":[{"limit":1e400,"limit":2}]}`,
			wantField: "limit",
			wantPath:  "$.items[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseDupErr(t, tc.raw)
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

// The first duplicate in document order is still the one reported when
// several objects have duplicates, regardless of their values.
func TestParse_DuplicateOrderWithHugeValues(t *testing.T) {
	raw := `{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[` +
		`{"id":"a","w":1e400,"w":2},` +
		`{"id":"c","tags":{"env":1e400,"env":2}}]}`
	err := parseDupErr(t, raw)
	msg := err.Error()
	if !strings.Contains(msg, `"w"`) || !strings.Contains(msg, "$.clusters[0]") {
		t.Fatalf("expected earliest duplicate w at clusters[0], got %q", msg)
	}
}

// Genuine JSON syntax errors stay fatal even though huge legal numbers are
// now accepted.
func TestParse_MalformedNumbersStillRejected(t *testing.T) {
	for _, raw := range []string{
		`{"x":1e}`,
		`{"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[],"x":1e}`,
		`{"x":1E+}`,
		`{"x":0.}`,
	} {
		_, err := ParseReleaseInput([]byte(raw))
		if err == nil {
			t.Fatalf("expected JSON format error for %s", raw)
		}
		if !strings.Contains(err.Error(), "JSON 格式错误") {
			t.Fatalf("expected JSON 格式错误 for %s, got %v", raw, err)
		}
	}
}

// Library callers get the same accept/reject decision and attribution as the
// CLI: the returned error message names the same cause.
func TestParse_LibraryAndCLIAgreeOnHugeNumbers(t *testing.T) {
	accepted := `{"app":"a","revision":"r","image":"i","batchSize":1,` +
		`"clusters":[{"id":"x"}],"meta":{"limit":1e400}}`
	if _, err := ParseReleaseInput([]byte(accepted)); err != nil {
		t.Fatalf("library path should accept huge number in extra field: %v", err)
	}
	rejected := `{"app":"a","revision":"r","image":"i","batchSize":1e400,"clusters":[{"id":"x"}]}`
	_, err := ParseReleaseInput([]byte(rejected))
	if err == nil {
		t.Fatal("library path should reject huge batchSize")
	}
	if want := `字段 "batchSize" 必须是正整数`; err.Error() != want {
		t.Fatalf("library error = %q, want %q", err.Error(), want)
	}
}

// CLI contract: a valid document carrying a huge number in an extra field
// prints the full plan and exits 0; a huge batchSize exits non-zero with an
// empty stdout and the field error on stderr.
func TestPlanCLI_HugeNumberInExtraFieldPlans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{
		"app": "payments", "revision": "v1.2.3", "image": "reg/payments:v1.2.3",
		"batchSize": 2,
		"clusters": [{"id": "c2"}, {"id": "c1"}],
		"meta": {"limit": 1e400, "notes": [1e400, {"deep": [1e400]}]}
	}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, `"c1"`) || !strings.Contains(stdout, `"c2"`) {
		t.Fatalf("expected full plan on stdout, got %q", stdout)
	}
}

func TestPlanCLI_HugeBatchSizeFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"payments","revision":"v1","image":"img","batchSize":1e400,"clusters":[{"id":"c1"}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "batchSize") || strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("stderr should attribute the batchSize problem, got %q", stderr)
	}
}

func TestPlanCLI_MalformedNumberFailsAsJSONError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	doc := `{"app":"payments","revision":"v1","image":"img","batchSize":1,"clusters":[],"x":1e}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "plan", path)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "JSON 格式错误") {
		t.Fatalf("stderr should report a JSON format error, got %q", stderr)
	}
}
