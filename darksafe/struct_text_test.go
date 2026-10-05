package darksafe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// This file covers the strict-text rule for a release configuration built
// directly in Go memory (ValidateReleaseInput / MakeReleasePlan): the file
// entry already rejects invalid UTF-8, but a Go string handed to the library
// can still contain invalid bytes, and those must be rejected before any
// identity comparison, label match or batching decision — never rewritten
// to "�" or silently carried into a successful plan.

// badStructInput builds a valid base config and applies one mutation.
func badStructInput(mutate func(*ReleasePlanInput)) ReleasePlanInput {
	in := validInput()
	mutate(&in)
	return in
}

// asciiReplacementEscape is how encoding/json spells a rewritten invalid
// rune in output: the six ASCII characters backslash-u-f-f-f-d.
const asciiReplacementEscape = "\\ufffd"

// requireStructTextError validates in both ways: ValidateReleaseInput must
// return a clear invalid-UTF-8 error naming want, and MakeReleasePlan must
// return that error together with the zero plan. It returns the error.
func requireStructTextError(t *testing.T, in ReleasePlanInput, want ...string) error {
	t.Helper()
	verr := ValidateReleaseInput(in)
	if verr == nil {
		t.Fatalf("ValidateReleaseInput accepted invalid UTF-8: %+v", in)
	}
	msg := verr.Error()
	if !strings.Contains(msg, "UTF-8") {
		t.Fatalf("error %q should identify invalid UTF-8", msg)
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Fatalf("error %q does not locate %q", msg, w)
		}
	}

	plan, perr := MakeReleasePlan(in)
	if perr == nil {
		t.Fatalf("MakeReleasePlan accepted invalid UTF-8: %+v", in)
	}
	if perr.Error() != msg {
		t.Fatalf("MakeReleasePlan error %q != ValidateReleaseInput error %q", perr, msg)
	}
	if !reflect.DeepEqual(plan, ReleasePlan{}) {
		t.Fatalf("invalid UTF-8 must yield the zero plan, got %+v", plan)
	}
	return verr
}

// TestStruct_InvalidUTF8RejectedEverywhere feeds one invalid byte through
// every string the configuration exposes, including a disabled cluster, a
// cluster filtering would drop, a tag that never participates in matching,
// and a fault-domain tag name. Each position must fail before planning.
func TestStruct_InvalidUTF8RejectedEverywhere(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ReleasePlanInput)
		locate []string
	}{
		{"app", func(in *ReleasePlanInput) { in.App += badUTF8 }, []string{`"app"`}},
		{"revision", func(in *ReleasePlanInput) { in.Revision += badUTF8 }, []string{`"revision"`}},
		{"image", func(in *ReleasePlanInput) { in.Image += badUTF8 }, []string{`"image"`}},
		{"spreadBy", func(in *ReleasePlanInput) { in.SpreadBy = "zone" + badUTF8 }, []string{`"spreadBy"`}},
		{"cluster id", func(in *ReleasePlanInput) { in.Clusters[0].ID += badUTF8 }, []string{"clusters[0]", `"id"`}},
		{"second cluster id", func(in *ReleasePlanInput) { in.Clusters[2].ID = badUTF8 }, []string{"clusters[2]", `"id"`}},
		{"disabled cluster id", func(in *ReleasePlanInput) {
			in.Clusters[0].Disabled = true
			in.Clusters[0].ID += badUTF8
		}, []string{"clusters[0]", `"id"`}},
		{"filtered-out cluster id", func(in *ReleasePlanInput) {
			in.Clusters[0].Tags = map[string]string{"env": "dev"}
			in.Clusters[0].ID += badUTF8
			in.Include = []LabelCondition{{"env": "prod"}}
		}, []string{"clusters[0]", `"id"`}},
		{"tag value on disabled cluster", func(in *ReleasePlanInput) {
			in.Clusters[0].Disabled = true
			in.Clusters[0].Tags = map[string]string{"env": "pr" + badUTF8 + "d"}
		}, []string{"clusters[0]", `"env"`}},
		{"tag value never matched", func(in *ReleasePlanInput) {
			// A tag no condition references and that is not the spread key:
			// it still carries text and must still be checked.
			in.Clusters[1].Tags = map[string]string{"unused": "v" + badUTF8}
		}, []string{"clusters[1]", `"unused"`}},
		{"tag key", func(in *ReleasePlanInput) {
			in.Clusters[0].Tags = map[string]string{"zo" + badUTF8 + "ne": "east"}
		}, []string{"clusters[0]", "标签"}},
		{"include key", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{{"env" + badUTF8: "prod"}}
		}, []string{"include[0]", "条件"}},
		{"include value", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{{"env": "pr" + badUTF8 + "d"}}
		}, []string{"include[0]", `"env"`}},
		{"exclude key", func(in *ReleasePlanInput) {
			in.Exclude = []LabelCondition{{"reg" + badUTF8 + "ion": "us"}}
		}, []string{"exclude[0]", "条件"}},
		{"exclude value", func(in *ReleasePlanInput) {
			in.Exclude = []LabelCondition{{"region": badUTF8}}
		}, []string{"exclude[0]", `"region"`}},
		{"second condition position", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{
				{"env": "dev"},
				{"tier": "ed" + badUTF8 + "ge"},
			}
		}, []string{"include[1]", `"tier"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireStructTextError(t, badStructInput(tc.mutate), tc.locate...)
		})
	}
}

// TestStruct_DistinctInvalidIDsAreNotDeduped is the core hazard: two
// different corrupt byte sequences are distinct Go strings but would both
// become "�" when the plan is marshaled as JSON. They must be a text error
// at the first candidate, never a successful plan nor a "duplicate ID"
// error, even though both clusters would otherwise be selected.
func TestStruct_DistinctInvalidIDsAreNotDeduped(t *testing.T) {
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "i", BatchSize: 2,
		Clusters: []Cluster{
			{ID: "c-" + "\xff"},
			{ID: "c-" + "\xfe"},
		},
	}
	err := requireStructTextError(t, in, "clusters[0]", `"id"`)
	if strings.Contains(err.Error(), "重复") {
		t.Fatalf("corrupt identities must not be compared as duplicates first: %v", err)
	}

	// Marshal on its own shows why accepting them is unsafe: both distinct
	// identities collapse into the same JSON string (encoding/json spells the
	// rewritten rune as the ASCII escape �).
	j1, _ := json.Marshal(in.Clusters[0].ID)
	j2, _ := json.Marshal(in.Clusters[1].ID)
	if string(j1) != string(j2) || !strings.Contains(string(j1), asciiReplacementEscape) {
		t.Fatalf("test setup no longer demonstrates the JSON rewrite: %q vs %q", j1, j2)
	}
}

// TestStruct_TextErrorBeatsBusinessErrors ensures invalid UTF-8 is rejected
// before emptiness, uniqueness and empty-key rules: none of those may run
// against bytes that are about to be rewritten.
func TestStruct_TextErrorBeatsBusinessErrors(t *testing.T) {
	// Bad app plus empty revision / non-positive batch size: text wins.
	in := validInput()
	in.App += badUTF8
	in.Revision = ""
	in.BatchSize = 0
	requireStructTextError(t, in, `"app"`)

	// A valid duplicate pair followed by a corrupt candidate: the corrupt
	// text still beats the duplicate, because text is checked first.
	in = validInput()
	in.Clusters = []Cluster{
		{ID: "x"},
		{ID: "x"}, // duplicate, would normally fail at clusters[1]
		{ID: "c-" + badUTF8},
	}
	requireStructTextError(t, in, "clusters[2]", `"id"`)

	// A corrupt tag value beside an empty tag key in the same object: key
	// ordering makes "" the first text entry either way, but the point is
	// that an empty-key business error is never reported ahead of text.
	in = validInput()
	in.Clusters[0].Tags = map[string]string{
		"":    "v",
		"env": "v" + badUTF8,
	}
	err := requireStructTextError(t, in, "clusters[0]")
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("expected UTF-8 text error, got %v", err)
	}
}

// TestStruct_TextErrorsAreDeterministic checks that one fixed problem is
// reported no matter how the tag and condition maps are iterated.
func TestStruct_TextErrorsAreDeterministic(t *testing.T) {
	in := validInput()
	in.Clusters[0].Tags = map[string]string{
		"z": "v" + badUTF8,
		"a": "v" + badUTF8,
		"m": "v" + badUTF8,
	}
	in.Include = []LabelCondition{{
		"region": "us" + badUTF8,
		"env":    "prod" + badUTF8,
	}}
	first := ValidateReleaseInput(in).Error()
	if !strings.Contains(first, `"a"`) {
		t.Fatalf("first tag problem must be the alphabetically first key, got %q", first)
	}
	for range 20 {
		err := ValidateReleaseInput(in)
		if err == nil || err.Error() != first {
			t.Fatalf("non-deterministic error: %q vs %q", err, first)
		}
	}
	// Conditions are reached after tags; with tags fixed, condition entries
	// are likewise deterministic on the alphabetically first key.
	in.Clusters[0].Tags = nil
	cfirst := ValidateReleaseInput(in).Error()
	if !strings.Contains(cfirst, "include[0]") || !strings.Contains(cfirst, `"env"`) {
		t.Fatalf("expected include[0] key \"env\" value error, got %q", cfirst)
	}
	for range 20 {
		if err := ValidateReleaseInput(in); err == nil || err.Error() != cfirst {
			t.Fatalf("non-deterministic condition error: %q vs %q", err, cfirst)
		}
	}
}

// TestStruct_TextErrorOrderIsFixed checks the overall reporting order:
// app, revision, image, spreadBy, candidates (ID before tags, in position
// order), then include and exclude in list order.
func TestStruct_TextErrorOrderIsFixed(t *testing.T) {
	in := validInput()
	in.App = badUTF8
	in.Revision = badUTF8
	in.Image = badUTF8
	in.SpreadBy = badUTF8
	in.Clusters[0].ID = badUTF8
	in.Clusters[1].Tags = map[string]string{"k": "v" + badUTF8}
	in.Include = []LabelCondition{{"env": "p" + badUTF8}}
	in.Exclude = []LabelCondition{{"env": "p" + badUTF8}}

	requireStructTextError(t, in, `"app"`)
	in.App = "payments"
	requireStructTextError(t, in, `"revision"`)
	in.Revision = "r"
	requireStructTextError(t, in, `"image"`)
	in.Image = "i"
	requireStructTextError(t, in, `"spreadBy"`)
	in.SpreadBy = ""
	requireStructTextError(t, in, "clusters[0]", `"id"`)
	in.Clusters[0].ID = "c3"
	requireStructTextError(t, in, "clusters[1]", `"k"`)
	in.Clusters[1].Tags = nil
	requireStructTextError(t, in, "include[0]", `"env"`)
	in.Include = nil
	requireStructTextError(t, in, "exclude[0]", `"env"`)
}

// TestStruct_InvalidKeyNamesOwningObject distinguishes a bad value from a
// bad key: the value error carries the key; the key error points at the
// object (tags map or condition) that owns it.
func TestStruct_InvalidKeyNamesOwningObject(t *testing.T) {
	// Invalid tag key: owning object is clusters[3]'s tag map.
	in := validInput()
	in.Clusters[3].Tags = map[string]string{"env\xfe": "prod"}
	err := requireStructTextError(t, in, "clusters[3]", "标签")
	if strings.Contains(err.Error(), "对应的值") {
		t.Fatalf("an invalid key must not be reported as a value error: %v", err)
	}

	// Invalid include condition key: owning object is the condition.
	in = validInput()
	in.Include = []LabelCondition{{"a": "1"}, {"env\xfe": "prod"}}
	err = requireStructTextError(t, in, "include[1]", "条件")
	if strings.Contains(err.Error(), "对应的值") {
		t.Fatalf("an invalid condition key must not be reported as a value error: %v", err)
	}
}

// TestStruct_InvalidUTF8DoesNotMutateInput verifies the caller's
// configuration is never repaired or altered: the corrupt bytes survive
// validation, and nothing is deleted or rewritten.
func TestStruct_InvalidUTF8DoesNotMutateInput(t *testing.T) {
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "i", BatchSize: 1,
		Clusters: []Cluster{
			{ID: "good", Tags: map[string]string{"env": "pr" + badUTF8 + "d"}},
			{ID: "bad" + badUTF8},
		},
		Include: []LabelCondition{{"env": "p" + badUTF8}},
	}
	before := fmt.Sprintf("%+v", in)
	if err := ValidateReleaseInput(in); err == nil {
		t.Fatal("expected error")
	}
	if _, err := MakeReleasePlan(in); err == nil {
		t.Fatal("expected error")
	}
	after := fmt.Sprintf("%+v", in)
	if after != before {
		t.Fatalf("input mutated:\n before: %s\n after:  %s", before, after)
	}
	if got := in.Clusters[0].Tags["env"]; !strings.Contains(got, badUTF8) {
		t.Fatalf("corrupt tag value was repaired: %q", got)
	}
	// Compare bytes, not runes: ranging over an invalid string yields
	// RuneError, but the 3-byte UTF-8 encoding of a real U+FFFD (EF BF BD)
	// must not have replaced the lone corrupt byte.
	if strings.Contains(in.Clusters[0].Tags["env"], string(rune(0xFFFD))) {
		t.Fatalf("corrupt bytes must not be replaced with U+FFFD: %q", in.Clusters[0].Tags["env"])
	}
}

// TestStruct_LegalTextUsedExactly guards the compatibility guarantees:
// Chinese, supplementary-plane characters, a literal "�", and ordinary text
// containing a backslash followed by uD800 are all legal UTF-8 and flow
// through planning unchanged.
func TestStruct_LegalTextUsedExactly(t *testing.T) {
	in := ReleasePlanInput{
		App:       "支付服务",
		Revision:  "v1😀",
		Image:     `reg\path\uD800-img`,
		BatchSize: 2,
		SpreadBy:  "区域",
		Clusters: []Cluster{
			{ID: "集群-一😀", Tags: map[string]string{"区域": "东�区"}},
			{ID: "集群-二😀", Tags: map[string]string{"区域": "东�区"}},
		},
	}
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("legal UTF-8 must validate: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("legal UTF-8 must plan: %v", err)
	}
	if plan.App.Name != "支付服务" || plan.App.Revision != "v1😀" || plan.App.Image != `reg\path\uD800-img` {
		t.Fatalf("app info rewritten: %+v", plan.App)
	}
	if len(plan.Batches) != 2 || plan.Batches[0].Clusters[0] != "集群-一😀" {
		t.Fatalf("identities or fault domains rewritten: %+v", plan.Batches)
	}

	// A literal replacement character is an ordinary, exact tag value.
	lit := ReleasePlanInput{
		App: "a", Revision: "r", Image: "i", BatchSize: 1,
		Clusters: []Cluster{{ID: "x", Tags: map[string]string{"env": "�"}}},
		Include:  []LabelCondition{{"env": "�"}},
	}
	plan, err = MakeReleasePlan(lit)
	if err != nil {
		t.Fatalf("literal U+FFFD must match exactly: %v", err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "x" {
		t.Fatalf("literal U+FFFD matching failed: %+v", plan)
	}

	// The same literal value must NOT match a different value carrying a
	// genuinely invalid byte.
	lit.Clusters[0].Tags["env"] = badUTF8
	if err := ValidateReleaseInput(lit); err == nil {
		t.Fatal("an invalid byte must not be accepted as equivalent to U+FFFD")
	}
}

// TestStruct_EmptyTagValueStillMatches confirms the text check does not
// disturb the existing empty-value fault-domain semantics.
func TestStruct_EmptyTagValueStillMatches(t *testing.T) {
	in := ReleasePlanInput{
		App: "a", Revision: "r", Image: "i", BatchSize: 1,
		SpreadBy: "zone",
		Clusters: []Cluster{{ID: "x", Tags: map[string]string{"zone": ""}}},
		Include:  []LabelCondition{{"zone": ""}},
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 1 || plan.Batches[0].Clusters[0] != "x" {
		t.Fatalf("empty tag value should still match and name a domain: %+v", plan)
	}
}
