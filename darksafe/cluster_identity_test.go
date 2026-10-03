package darksafe

import (
	"fmt"
	"strings"
	"testing"
)

// This file pins down the cluster identity rule that ParseReleaseInput (JSON
// document) and ValidateReleaseInput (directly constructed Go config) share
// after their empty/duplicate checks were unified:
//
//   - an id that is empty or only whitespace is invalid;
//   - ids must be unique across every candidate, including disabled clusters
//     and clusters that filtering would drop;
//   - emptiness recognizes whitespace, but uniqueness and preservation compare
//     the id exactly as written — never trimmed, case-folded or merged;
//   - the id is checked before a candidate's other fields, while an earlier
//     candidate's field error still precedes a later candidate's bad id.
//
// Both paths must report the same cause and zero-based position.

func identityJSONDoc(clusters string) string {
	return identityJSONDocBatch(1, clusters)
}

func identityJSONDocBatch(batchSize int, clusters string) string {
	return fmt.Sprintf(`{"app":"a","revision":"r","image":"i","batchSize":%d,"clusters":[%s]}`,
		batchSize, clusters)
}

func identityStructInput(ids ...string) ReleasePlanInput {
	in := ReleasePlanInput{App: "a", Revision: "r", Image: "i", BatchSize: 1}
	for _, id := range ids {
		in.Clusters = append(in.Clusters, Cluster{ID: id})
	}
	return in
}

// Empty and whitespace-only ids are rejected through both paths with the same
// message, and such an id is reported as empty (not as a duplicate) even when
// an earlier candidate was also empty.
func TestClusterID_EmptyAndWhitespaceBothPaths(t *testing.T) {
	cases := []struct {
		name   string
		jsonID string // raw JSON id token
		goID   string // Go-side id value
	}{
		{"empty", `""`, ""},
		{"spaces", `"   "`, "   "},
		{"tab", "\"\\t\"", "\t"},
		{"mixed whitespace", `"  \t\n "`, "  \t\n "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, jerr := ParseReleaseInput([]byte(identityJSONDoc(`{"id":` + tc.jsonID + `}`)))
			if jerr == nil {
				t.Fatal("JSON path: expected empty-id error")
			}
			serr := ValidateReleaseInput(identityStructInput(tc.goID))
			if serr == nil {
				t.Fatal("struct path: expected empty-id error")
			}
			if jerr.Error() != serr.Error() {
				t.Fatalf("messages differ:\n JSON:   %v\n struct: %v", jerr, serr)
			}
			if !strings.Contains(jerr.Error(), `clusters[0]`) ||
				!strings.Contains(jerr.Error(), `"id"`) ||
				strings.Contains(jerr.Error(), "重复") {
				t.Fatalf("empty id must be reported as an empty-field error, got %q", jerr)
			}
		})

		// A second empty id after a first empty id is still an empty-id error at
		// the first position, never a duplicate — empty ids are not recorded.
		t.Run(tc.name+" twice reports first as empty", func(t *testing.T) {
			in := identityStructInput(tc.goID, tc.goID)
			err := ValidateReleaseInput(in)
			if err == nil || !strings.Contains(err.Error(), `clusters[0]`) ||
				strings.Contains(err.Error(), "重复") {
				t.Fatalf("first empty id must win as an empty-field error, got %v", err)
			}
		})
	}
}

// Distinct spellings are three different identities: case and a leading space
// are significant. Both paths accept them, preserve each string verbatim, and
// plan with all three — none is trimmed, merged or dropped.
func TestClusterID_ExactStringIdentityPreserved(t *testing.T) {
	ids := []string{"c-a", "C-A", " c-a"}
	raw := identityJSONDocBatch(3,
		`{"id":"c-a"},{"id":"C-A"},{"id":" c-a"}`)

	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatalf("JSON path must accept distinct spellings: %v", err)
	}
	if len(in.Clusters) != 3 {
		t.Fatalf("expected 3 clusters, got %d", len(in.Clusters))
	}
	for i, want := range ids {
		if in.Clusters[i].ID != want {
			t.Fatalf("cluster %d id = %q, want %q (must not be trimmed)", i, in.Clusters[i].ID, want)
		}
	}

	structIn := identityStructInput(ids...)
	structIn.BatchSize = 3
	if err := ValidateReleaseInput(structIn); err != nil {
		t.Fatalf("struct path must accept distinct spellings: %v", err)
	}

	for name, in := range map[string]ReleasePlanInput{"json": in, "struct": structIn} {
		plan, err := MakeReleasePlan(in)
		if err != nil {
			t.Fatalf("%s: plan failed: %v", name, err)
		}
		if len(plan.Batches) != 1 || len(plan.Batches[0].Clusters) != 3 {
			t.Fatalf("%s: expected one batch of all three ids, got %+v", name, plan.Batches)
		}
		got := strings.Join(plan.Batches[0].Clusters, "|")
		// Sorting keeps each string intact; " c-a" sorts before "C-A"/"c-a".
		want := strings.Join([]string{" c-a", "C-A", "c-a"}, "|")
		if got != want {
			t.Fatalf("%s: batch = %q, want %q", name, got, want)
		}
	}
}

// Disabled clusters, clusters matching an exclude condition, and clusters not
// matching an include condition still participate in identity checks, on both
// paths. The error names the later position and the conflicting id.
func TestClusterID_DuplicatesAmongFilteredOutCandidates(t *testing.T) {
	cases := []struct {
		name     string
		clusterA string
		clusterB string
	}{
		{
			name:     "disabled then active",
			clusterA: `{"id":"x","disabled":true}`,
			clusterB: `{"id":"x"}`,
		},
		{
			name:     "exclude-matched then active",
			clusterA: `{"id":"x","tags":{"env":"temp"}}`,
			clusterB: `{"id":"x"}`,
		},
		{
			name:     "include-not-matched then active",
			clusterA: `{"id":"x","tags":{"env":"dev"}}`,
			clusterB: `{"id":"x","tags":{"env":"prod"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+" json", func(t *testing.T) {
			doc := identityJSONDoc(tc.clusterA + "," + tc.clusterB)
			if strings.Contains(tc.name, "exclude") {
				doc = insertExclude(t, doc)
			}
			if strings.Contains(tc.name, "include") {
				doc = insertInclude(t, doc)
			}
			_, err := ParseReleaseInput([]byte(doc))
			assertDuplicateAtOne(t, err)
		})
	}

	// Struct path: every filtered-out flavor is still a duplicate error.
	filtered := []Cluster{
		{ID: "x", Disabled: true},
		{ID: "x", Tags: map[string]string{"env": "temp"}},
	}
	in := identityStructInput()
	in.Clusters = filtered
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), `clusters[1]`) || !strings.Contains(err.Error(), `"x"`) {
		t.Fatalf("disabled duplicate must be rejected, got %v", err)
	}

	in = identityStructInput()
	in.Clusters = []Cluster{
		{ID: "x", Tags: map[string]string{"env": "dev"}},
		{ID: "x", Tags: map[string]string{"env": "prod"}},
	}
	in.Include = []LabelCondition{{"env": "prod"}}
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), `clusters[1]`) {
		t.Fatalf("include-not-matched duplicate must be rejected, got %v", err)
	}
}

// Error ordering across candidates:
//   - candidate 0's empty tag key must beat candidate 1's duplicate id;
//   - on one candidate, a duplicate id must beat that candidate's empty tag key.
//
// Both paths agree.
func TestClusterID_ErrorOrdering(t *testing.T) {
	t.Run("earlier candidate tag error beats later duplicate", func(t *testing.T) {
		raw := identityJSONDoc(
			`{"id":"x","tags":{"":"v"}},{"id":"x"}`)
		_, jerr := ParseReleaseInput([]byte(raw))
		// JSON path formats a cluster tag error by id; either way it must be
		// candidate 0's tag-key problem, not candidate 1's duplicate id.
		if jerr == nil || !strings.Contains(jerr.Error(), `"x"`) ||
			!strings.Contains(jerr.Error(), "标签键") ||
			strings.Contains(jerr.Error(), "重复") {
			t.Fatalf("JSON: expected first candidate tag-key error, got %v", jerr)
		}

		in := identityStructInput("x", "x")
		in.Clusters[0].Tags = map[string]string{"": "v"}
		serr := ValidateReleaseInput(in)
		if serr == nil || !strings.Contains(serr.Error(), `clusters[0]`) ||
			!strings.Contains(serr.Error(), "标签键") {
			t.Fatalf("struct: expected clusters[0] tag-key error, got %v", serr)
		}
	})

	t.Run("duplicate beats same candidate tag error", func(t *testing.T) {
		raw := identityJSONDoc(
			`{"id":"x"},{"id":"x","tags":{"":"v"}}`)
		_, jerr := ParseReleaseInput([]byte(raw))
		if jerr == nil || !strings.Contains(jerr.Error(), `clusters[1]`) ||
			!strings.Contains(jerr.Error(), "重复") {
			t.Fatalf("JSON: expected clusters[1] duplicate-id error, got %v", jerr)
		}

		in := identityStructInput("x", "x")
		in.Clusters[1].Tags = map[string]string{"": "v"}
		serr := ValidateReleaseInput(in)
		if serr == nil || !strings.Contains(serr.Error(), `clusters[1]`) ||
			!strings.Contains(serr.Error(), "重复") {
			t.Fatalf("struct: expected clusters[1] duplicate-id error, got %v", serr)
		}
	})
}

// Duplicate errors report the later occurrence's zero-based position and the
// conflicting id, identically on both paths.
func TestClusterID_DuplicateMessageMatchesBothPaths(t *testing.T) {
	raw := identityJSONDoc(`{"id":"a"},{"id":"b"},{"id":"a"}`)
	_, jerr := ParseReleaseInput([]byte(raw))
	assertDuplicateAtTwo := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected duplicate error")
		}
		if !strings.Contains(err.Error(), `clusters[2]`) {
			t.Fatalf("error must point at the later position clusters[2], got %q", err)
		}
		if !strings.Contains(err.Error(), `"a"`) {
			t.Fatalf("error must name the conflicting id, got %q", err)
		}
	}
	assertDuplicateAtTwo(t, jerr)

	serr := ValidateReleaseInput(identityStructInput("a", "b", "a"))
	assertDuplicateAtTwo(t, serr)
	if jerr.Error() != serr.Error() {
		t.Fatalf("paths differ:\n JSON:   %v\n struct: %v", jerr, serr)
	}
}

func assertDuplicateAtOne(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	if !strings.Contains(err.Error(), `clusters[1]`) ||
		!strings.Contains(err.Error(), "重复") ||
		!strings.Contains(err.Error(), `"x"`) {
		t.Fatalf("expected clusters[1] duplicate of %q, got %v", `"x"`, err)
	}
}

// insertExclude/insertInclude add a condition right before the closing brace.
func insertExclude(t *testing.T, doc string) string {
	t.Helper()
	return strings.TrimSuffix(doc, "}") + `,"exclude":[{"env":"temp"}]}`
}

func insertInclude(t *testing.T, doc string) string {
	t.Helper()
	return strings.TrimSuffix(doc, "}") + `,"include":[{"env":"prod"}]}`
}
