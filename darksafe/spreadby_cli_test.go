package darksafe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the spreadBy fault-domain behaviour through the real
// `darksafe plan <file>` command: a config file on disk, a process exit code,
// and raw stdout/stderr. The library rules are covered in plan_test.go; here
// the contract under regression is what a user actually observes when the
// fault-domain plan arrives from a configuration file. Everything runs
// offline — the command never connects to a cluster or performs a release.

// cliPlan mirrors the JSON document printed by `darksafe plan`. It is
// deliberately decoupled from the library structs so that a drift in either
// the JSON schema or the planning rules is visible at the command boundary.
type cliPlan struct {
	App struct {
		Name     string `json:"name"`
		Revision string `json:"revision"`
		Image    string `json:"image"`
	} `json:"app"`
	Batches []struct {
		Index    int      `json:"index"`
		Clusters []string `json:"clusters"`
	} `json:"batches"`
	Excluded []struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	} `json:"excluded"`
}

// spreadPlanDoc is the reference success document: five selectable clusters
// in two fault domains plus one disabled, one include-filtered and one
// exclude-filtered candidate (the exclude match wins over the include match).
const spreadPlanDoc = `{
	"app": "payments",
	"revision": "v1.2.3",
	"image": "reg/payments:v1.2.3",
	"batchSize": 2,
	"spreadBy": "zone",
	"clusters": [
		{"id": "a", "tags": {"zone": "east"}},
		{"id": "b", "tags": {"zone": "east"}},
		{"id": "c", "tags": {"zone": "west"}},
		{"id": "d", "tags": {"zone": "west"}},
		{"id": "e", "tags": {"zone": "east"}},
		{"id": "f", "disabled": true},
		{"id": "g", "tags": {"env": "dev"}},
		{"id": "h", "tags": {"zone": "north", "env": "prod"}}
	],
	"include": [{"env": "prod"}, {"zone": "east"}, {"zone": "west"}],
	"exclude": [{"env": "prod"}]
}`

// spreadPlanDocShuffled is the same configuration with clusters, tag keys and
// include/exclude conditions reordered in the file. The emitted plan must be
// byte-for-byte identical to the one for spreadPlanDoc.
const spreadPlanDocShuffled = `{
	"include": [{"zone": "west"}, {"env": "prod"}, {"zone": "east"}],
	"spreadBy": "zone",
	"clusters": [
		{"tags": {"zone": "east"}, "id": "e"},
		{"tags": {"env": "prod", "zone": "north"}, "id": "h"},
		{"disabled": true, "id": "f"},
		{"tags": {"zone": "west"}, "id": "d"},
		{"tags": {"env": "dev"}, "id": "g"},
		{"id": "c", "tags": {"zone": "west"}},
		{"tags": {"zone": "east"}, "id": "b"},
		{"id": "a", "tags": {"zone": "east"}}
	],
	"exclude": [{"env": "prod"}],
	"batchSize": 2,
	"image": "reg/payments:v1.2.3",
	"revision": "v1.2.3",
	"app": "payments"
}`

// runPlanCLI writes doc to a temporary config file and runs `darksafe plan`.
func runPlanCLI(t *testing.T, doc string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return runCLI(t, "plan", path)
}

// parseCLIPlan requires stdout to be one complete JSON document (with nothing
// after it) and decodes it into a cliPlan.
func parseCLIPlan(t *testing.T, stdout string) cliPlan {
	t.Helper()
	var plan cliPlan
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&plan); err != nil {
		t.Fatalf("stdout is not a parseable release plan: %v\nstdout=%q", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout contains trailing data after the plan: %q", stdout)
	}
	return plan
}

// assertPlanShape checks the batch-level contract: batches numbered from one
// with no gaps, at most batchSize clusters each, ascending IDs inside a batch,
// each selectable ID appearing exactly once, and at most one cluster per fault
// domain in the same batch.
func assertPlanShape(t *testing.T, plan cliPlan, batchSize int, domains map[string]string) {
	t.Helper()
	seen := map[string]int{}
	for i, batch := range plan.Batches {
		if batch.Index != i+1 {
			t.Fatalf("batch %d has index %d, want %d", i, batch.Index, i+1)
		}
		if len(batch.Clusters) > batchSize {
			t.Fatalf("batch %d holds %d clusters, batchSize is %d", batch.Index, len(batch.Clusters), batchSize)
		}
		if len(batch.Clusters) == 0 {
			t.Fatalf("batch %d is empty", batch.Index)
		}
		for j := 1; j < len(batch.Clusters); j++ {
			if batch.Clusters[j-1] >= batch.Clusters[j] {
				t.Fatalf("batch %d clusters are not ascending: %v", batch.Index, batch.Clusters)
			}
		}
		usedDomains := map[string]string{}
		for _, id := range batch.Clusters {
			domain, known := domains[id]
			if !known {
				t.Fatalf("batch %d contains unexpected cluster %q", batch.Index, id)
			}
			if prev, conflict := usedDomains[domain]; conflict {
				t.Fatalf("batch %d puts %q and %q in the same fault domain %q", batch.Index, prev, id, domain)
			}
			usedDomains[domain] = id
			seen[id]++
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("cluster %q appears in %d batches, want exactly 1", id, n)
		}
	}
	if len(seen) != len(domains) {
		t.Fatalf("planned %d distinct clusters, want %d", len(seen), len(domains))
	}
}

func TestPlanCLI_SpreadByFullPlan(t *testing.T) {
	code, stdout, stderr := runPlanCLI(t, spreadPlanDoc)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr on success, got %q", stderr)
	}

	plan := parseCLIPlan(t, stdout)

	// Application identity mirrors the input exactly.
	if plan.App.Name != "payments" || plan.App.Revision != "v1.2.3" || plan.App.Image != "reg/payments:v1.2.3" {
		t.Fatalf("app info mismatch: %+v", plan.App)
	}

	domains := map[string]string{
		"a": "east", "b": "east", "e": "east",
		"c": "west", "d": "west",
	}
	assertPlanShape(t, plan, 2, domains)

	// b shares a domain with a and is deferred, but c (another domain) still
	// joins batch 1; the same pattern repeats until e is scheduled alone.
	wantBatches := [][]string{{"a", "c"}, {"b", "d"}, {"e"}}
	if len(plan.Batches) != len(wantBatches) {
		t.Fatalf("got %d batches, want %d: %+v", len(plan.Batches), len(wantBatches), plan.Batches)
	}
	for i, want := range wantBatches {
		if strings.Join(plan.Batches[i].Clusters, ",") != strings.Join(want, ",") {
			t.Fatalf("batch %d = %v, want %v", i+1, plan.Batches[i].Clusters, want)
		}
	}

	// Filtered clusters are listed by ascending ID with their existing reasons.
	wantExcluded := []struct {
		id, reason string
	}{
		{"f", ReasonDisabled},
		{"g", ReasonIncludeNotMatched},
		{"h", ReasonExcludeMatched},
	}
	if len(plan.Excluded) != len(wantExcluded) {
		t.Fatalf("got %d excluded clusters, want %d: %+v", len(plan.Excluded), len(wantExcluded), plan.Excluded)
	}
	for i, want := range wantExcluded {
		if plan.Excluded[i].ID != want.id || plan.Excluded[i].Reason != want.reason {
			t.Fatalf("excluded[%d] = {%s %s}, want {%s %s}", i, plan.Excluded[i].ID, plan.Excluded[i].Reason, want.id, want.reason)
		}
	}
}

func TestPlanCLI_SpreadByIndependentOfFileOrder(t *testing.T) {
	code1, stdout1, stderr1 := runPlanCLI(t, spreadPlanDoc)
	if code1 != 0 {
		t.Fatalf("ordered doc: expected exit 0, got %d; stderr=%q", code1, stderr1)
	}
	code2, stdout2, stderr2 := runPlanCLI(t, spreadPlanDocShuffled)
	if code2 != 0 {
		t.Fatalf("shuffled doc: expected exit 0, got %d; stderr=%q", code2, stderr2)
	}
	if stdout1 != stdout2 {
		t.Fatalf("plan output depends on file ordering:\n ordered: %s\n shuffled: %s", stdout1, stdout2)
	}
}

// Filtering (disabled / include / exclude) happens before the fault-domain
// tag check: candidates removed by those rules lack the spreadBy tag here, yet
// the plan for the tagged survivors must still succeed and the filtered ones
// must appear in the excluded list with their ordinary reasons.
func TestPlanCLI_SpreadByFilteredClustersNeedNoTag(t *testing.T) {
	doc := `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3,
		"spreadBy": "zone",
		"clusters": [
			{"id": "s1", "tags": {"zone": "east"}},
			{"id": "d1", "disabled": true},
			{"id": "i1", "tags": {"env": "dev"}},
			{"id": "x1", "tags": {"env": "temp"}}
		],
		"include": [{"env": "prod"}, {"zone": "east"}],
		"exclude": [{"env": "temp"}]
	}`
	code, stdout, stderr := runPlanCLI(t, doc)
	if code != 0 {
		t.Fatalf("filtered clusters must not need the spread tag; exit=%d stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	plan := parseCLIPlan(t, stdout)
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "s1" {
		t.Fatalf("expected only s1 planned, got %+v", plan.Batches)
	}
	wantExcluded := []struct {
		id, reason string
	}{
		{"d1", ReasonDisabled},
		{"i1", ReasonIncludeNotMatched},
		{"x1", ReasonExcludeMatched},
	}
	if len(plan.Excluded) != len(wantExcluded) {
		t.Fatalf("got %d excluded, want %d: %+v", len(plan.Excluded), len(wantExcluded), plan.Excluded)
	}
	for i, want := range wantExcluded {
		if plan.Excluded[i].ID != want.id || plan.Excluded[i].Reason != want.reason {
			t.Fatalf("excluded[%d] = {%s %s}, want {%s %s}", i, plan.Excluded[i].ID, plan.Excluded[i].Reason, want.id, want.reason)
		}
	}
}

// Any selected cluster missing the spreadBy tag fails the whole plan: non-zero
// exit, empty stdout (no partial batches), and stderr names the smallest
// offending selected cluster plus the tag. Disabled clusters missing the tag
// (here a0, whose ID is even smaller) are not selected and must not be named.
func TestPlanCLI_SpreadByMissingTagFailsCleanly(t *testing.T) {
	doc := `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 2,
		"spreadBy": "zone",
		"clusters": [
			{"id": "a0", "disabled": true},
			{"id": "a"},
			{"id": "b", "tags": {"zone": "east"}},
			{"id": "c", "tags": {"region": "us"}}
		]
	}`
	code, stdout, stderr := runPlanCLI(t, doc)
	if code == 0 {
		t.Fatalf("expected non-zero exit for missing spread tag; stdout=%q", stdout)
	}
	if stdout != "" {
		t.Fatalf("failure must not print partial batches on stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, `集群 "a"`) {
		t.Fatalf("stderr should name the smallest selected cluster missing the tag, got %q", stderr)
	}
	if !strings.Contains(stderr, `缺少故障域标签 "zone"`) {
		t.Fatalf("stderr should name the missing fault-domain tag %q, got %q", "zone", stderr)
	}
	if strings.Contains(stderr, `集群 "a0"`) || strings.Contains(stderr, `集群 "c"`) {
		t.Fatalf("stderr must name only the smallest offending selected cluster, got %q", stderr)
	}
}

// A tag present with an empty-string value is a legal fault domain: clusters
// sharing the empty value avoid each other exactly like any other domain, and
// they are not treated as missing the tag.
func TestPlanCLI_SpreadByEmptyTagValueIsDomain(t *testing.T) {
	doc := `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3,
		"spreadBy": "zone",
		"clusters": [
			{"id": "a", "tags": {"zone": ""}},
			{"id": "b", "tags": {"zone": ""}},
			{"id": "c", "tags": {"zone": "east"}}
		]
	}`
	code, stdout, stderr := runPlanCLI(t, doc)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
	plan := parseCLIPlan(t, stdout)
	domains := map[string]string{"a": "", "b": "", "c": "east"}
	assertPlanShape(t, plan, 3, domains)
	wantBatches := [][]string{{"a", "c"}, {"b"}}
	if len(plan.Batches) != len(wantBatches) {
		t.Fatalf("got %d batches, want %d: %+v", len(plan.Batches), len(wantBatches), plan.Batches)
	}
	for i, want := range wantBatches {
		if strings.Join(plan.Batches[i].Clusters, ",") != strings.Join(want, ",") {
			t.Fatalf("batch %d = %v, want %v", i+1, plan.Batches[i].Clusters, want)
		}
	}

	// An actually absent tag is still a failure, distinct from the empty value.
	missing := `{
		"app": "app", "revision": "r1", "image": "img",
		"batchSize": 3,
		"spreadBy": "zone",
		"clusters": [
			{"id": "d", "tags": {"zone": ""}},
			{"id": "e"}
		]
	}`
	code, stdout, stderr = runPlanCLI(t, missing)
	if code == 0 {
		t.Fatalf("cluster with no tag at all must fail; stdout=%q", stdout)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, `集群 "e"`) || !strings.Contains(stderr, `缺少故障域标签 "zone"`) {
		t.Fatalf("stderr should name e and the missing tag, got %q", stderr)
	}
}

// Without spreadBy, or with spreadBy set to an empty string, the legacy
// behaviour is preserved: ascending IDs packed to capacity, same-domain
// clusters allowed in one batch. Both forms must emit the identical plan.
func TestPlanCLI_WithoutSpreadByKeepsLegacyBatching(t *testing.T) {
	docs := map[string]string{
		"absent": `{
			"app": "legacy", "revision": "r9", "image": "img:r9",
			"batchSize": 2,
			"clusters": [
				{"id": "c2", "tags": {"zone": "e"}},
				{"id": "c1", "tags": {"zone": "e"}},
				{"id": "c3"}
			]
		}`,
		"empty": `{
			"app": "legacy", "revision": "r9", "image": "img:r9",
			"batchSize": 2,
			"spreadBy": "",
			"clusters": [
				{"id": "c2", "tags": {"zone": "e"}},
				{"id": "c1", "tags": {"zone": "e"}},
				{"id": "c3"}
			]
		}`,
	}
	var outputs []string
	for name, doc := range docs {
		code, stdout, stderr := runPlanCLI(t, doc)
		if code != 0 {
			t.Fatalf("%s: expected exit 0, got %d; stderr=%q", name, code, stderr)
		}
		if stderr != "" {
			t.Fatalf("%s: expected empty stderr, got %q", name, stderr)
		}
		plan := parseCLIPlan(t, stdout)
		if plan.App.Name != "legacy" || plan.App.Revision != "r9" || plan.App.Image != "img:r9" {
			t.Fatalf("%s: app info mismatch: %+v", name, plan.App)
		}
		wantBatches := [][]string{{"c1", "c2"}, {"c3"}}
		if len(plan.Batches) != len(wantBatches) {
			t.Fatalf("%s: got %d batches, want %d: %+v", name, len(plan.Batches), len(wantBatches), plan.Batches)
		}
		for i, want := range wantBatches {
			if strings.Join(plan.Batches[i].Clusters, ",") != strings.Join(want, ",") {
				t.Fatalf("%s: batch %d = %v, want %v (same-domain clusters may share a batch)", name, i+1, plan.Batches[i].Clusters, want)
			}
		}
		outputs = append(outputs, stdout)
	}
	if outputs[0] != outputs[1] {
		t.Fatalf("absent and empty-string spreadBy must give identical output:\n absent: %s\n empty:  %s", outputs[0], outputs[1])
	}
}
