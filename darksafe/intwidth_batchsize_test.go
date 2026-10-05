package darksafe

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// batchSizeDoc builds a plan document with three available clusters and no
// spreadBy; size is substituted as the raw JSON integer literal.
func batchSizeDoc(size string) string {
	return `{"app":"app","revision":"r1","image":"img","batchSize":` + size +
		`,"clusters":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`
}

// A batchSize literal must be kept exactly as written whenever the running
// environment's Go int can represent it, and rejected with the field error —
// never narrowed — when it cannot. The boundary therefore follows the actual
// integer width rather than one hard-coded 32-bit ceiling: on a 32-bit
// environment 2147483647 is the largest legal value and
// 2147483648/4294967297 are configuration errors; on a 64-bit environment
// those same values plan normally, up to the largest positive int itself.
func TestParse_BatchSizeNativeIntBoundary(t *testing.T) {
	// 2147483647 is legal in every supported environment and keeps its value.
	in, err := ParseReleaseInput([]byte(batchSizeDoc("2147483647")))
	if err != nil {
		t.Fatalf("2147483647 must be representable everywhere, got %v", err)
	}
	if in.BatchSize != 2147483647 {
		t.Fatalf("batchSize = %d, want 2147483647", in.BatchSize)
	}

	// The largest positive int of this environment is legal and preserved.
	maxInt := int(^uint(0) >> 1)
	in, err = ParseReleaseInput([]byte(batchSizeDoc(strconv.Itoa(maxInt))))
	if err != nil {
		t.Fatalf("max int %d must be legal, got %v", maxInt, err)
	}
	if in.BatchSize != maxInt {
		t.Fatalf("batchSize = %d, want %d", in.BatchSize, maxInt)
	}

	switch strconv.IntSize {
	case 32:
		// Values beyond the 32-bit ceiling are all configuration errors, even
		// though they narrow into other ints: 4294967297 would silently
		// become 1 and 2147483648 a negative number. Neither may plan.
		for _, lit := range []string{"2147483648", "4294967297"} {
			zero, perr := ParseReleaseInput([]byte(batchSizeDoc(lit)))
			if perr == nil {
				t.Fatalf("batchSize %s must be rejected on a 32-bit environment, got %+v", lit, zero)
			}
			if want := `字段 "batchSize" 必须是正整数`; perr.Error() != want {
				t.Fatalf("batchSize %s: error = %q, want %q", lit, perr.Error(), want)
			}
			if strings.Contains(perr.Error(), "JSON 格式错误") {
				t.Fatalf("batchSize %s is a legal JSON number, not a format error: %v", lit, perr)
			}
			if !reflect.DeepEqual(zero, ReleasePlanInput{}) {
				t.Fatalf("rejected parse must return the zero-value config, got %+v", zero)
			}
		}
	case 64:
		// The exact values the 32-bit environment cannot hold stay usable here
		// with their written values; the fix must not clamp every environment
		// to the 32-bit ceiling.
		for _, lit := range []string{"2147483648", "4294967297"} {
			wide, perr := ParseReleaseInput([]byte(batchSizeDoc(lit)))
			if perr != nil {
				t.Fatalf("batchSize %s must be legal on a 64-bit environment, got %v", lit, perr)
			}
			want, _ := strconv.ParseInt(lit, 10, 64)
			if int64(wide.BatchSize) != want {
				t.Fatalf("batchSize = %d, want %s (must not be truncated or clamped)", wide.BatchSize, lit)
			}
		}
	default:
		t.Fatalf("unexpected int size %d", strconv.IntSize)
	}
}

// Through the public parse entry an out-of-range capacity fails before any
// filtering or batching: the returned config is the zero value and the cause
// is the existing field error. On a 32-bit environment the document carrying
// 4294967297 and three available clusters previously narrowed to batchSize 1
// and produced three single-cluster batches; now nothing is planned.
func TestParse_BatchSizeOutOfRangePlansNothing(t *testing.T) {
	if strconv.IntSize != 32 {
		// 4294967297 overflows only on a 32-bit int; the 64-bit preservation
		// contract is covered by TestParse_BatchSizeNativeIntBoundary.
		return
	}
	in, err := ParseReleaseInput([]byte(batchSizeDoc("4294967297")))
	if err == nil {
		t.Fatalf("4294967297 must be rejected on a 32-bit environment, got %+v", in)
	}
	if want := `字段 "batchSize" 必须是正整数`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("expected the zero-value config on rejection, got %+v", in)
	}
	// The zero-value config never reaches batching (it fails field validation
	// on app before batchSize); in particular there is no plan whose three
	// single-cluster batches would reveal the narrowed value 1.
	if plan, perr := MakeReleasePlan(in); perr == nil {
		t.Fatalf("planning the rejected config must fail, got %+v", plan)
	}
}

// CLI contract at the integer-width boundary. On a 32-bit environment the
// document with batchSize 4294967297 and three available clusters must exit
// 1 with empty stdout and the field error on stderr — it must not print three
// single-cluster batches as if batchSize were 1. On a 64-bit environment the
// same document plans normally: one batch holding all three clusters.
func TestPlanCLI_BatchSizeNativeIntBoundary(t *testing.T) {
	code, stdout, stderr := runCLI(t, "plan", writePlanDoc(t, batchSizeDoc("4294967297")))
	switch strconv.IntSize {
	case 32:
		if code != 1 {
			t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty on rejection, got %q", stdout)
		}
		if want := `字段 "batchSize" 必须是正整数`; !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
		if strings.Contains(stderr, "JSON 格式错误") {
			t.Fatalf("4294967297 is a legal JSON number, stderr must not call it malformed: %q", stderr)
		}
	case 64:
		if code != 0 {
			t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
		}
		if stderr != "" {
			t.Fatalf("unexpected stderr: %q", stderr)
		}
		p := decodeCLIPlan(t, stdout)
		if len(p.Batches) != 1 ||
			strings.Join(p.Batches[0].Clusters, ",") != "c1,c2,c3" {
			t.Fatalf("expected one batch [c1 c2 c3], got %+v", p.Batches)
		}
	}
}

// A capacity within native int range is still only an upper bound per batch:
// enabling fault-domain spreading keeps same-domain clusters in separate
// batches even when the capacity is 4294967297 (a value legal only where the
// native int can hold it).
func TestPlan_BatchSizeBoundaryStillRespectsSpreadBy(t *testing.T) {
	lit := "4294967297"
	if strconv.IntSize == 32 {
		// Use the largest value this environment can represent; the spreadBy
		// guarantee must hold at the ceiling regardless of int width.
		lit = "2147483647"
	}
	raw := `{"app":"app","revision":"r1","image":"img","batchSize":` + lit + `,"spreadBy":"zone",` +
		`"clusters":[` +
		`{"id":"c1","tags":{"zone":"east"}},` +
		`{"id":"c2","tags":{"zone":"east"}},` +
		`{"id":"c3","tags":{"zone":"east"}}]}`
	in := parsePlan(t, raw)
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchIDs(plan); strings.Join(got, "|") != "c1|c2|c3" {
		t.Fatalf("huge in-range capacity must not merge one domain into a batch, got %v", got)
	}
}
