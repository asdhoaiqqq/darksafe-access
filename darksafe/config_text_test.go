package darksafe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// badGoUTF8 and anotherBadUTF8 are two DIFFERENT corrupt strings: each holds
// a byte that is not valid UTF-8 anywhere. Go strings preserve the raw bytes,
// but encoding/json rewrites both to the same replacement character — the
// exact identity collapse the library check must prevent.
const (
	badGoUTF8      = "\xff"
	anotherBadUTF8 = "\xfe"
)

// validTextInput returns a well-formed in-memory config for library-path
// strict-text tests; it is separate from validInput so its tags/conditions
// give every position a legal baseline.
func validTextInput() ReleasePlanInput {
	return ReleasePlanInput{
		App:       "payments",
		Revision:  "v1.2.3",
		Image:     "reg/payments:v1.2.3",
		BatchSize: 2,
		SpreadBy:  "zone",
		Clusters: []Cluster{
			{ID: "c1", Tags: map[string]string{"zone": "east", "env": "prod"}},
			{ID: "c2", Tags: map[string]string{"zone": "west"}},
		},
		Include: []LabelCondition{{"env": "prod"}},
		Exclude: []LabelCondition{{"zone": "west"}},
	}
}

// TestValidate_InvalidUTF8RejectedEverywhere feeds one invalid byte through
// every string the Go-config path carries. Each error must say UTF-8 and
// locate the exact field or zero-based position; a bad value must also name
// its key, while a bad key points at the owning label/condition object.
func TestValidate_InvalidUTF8RejectedEverywhere(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*ReleasePlanInput)
		wantPart string // a fragment the error must contain
	}{
		{"app", func(in *ReleasePlanInput) { in.App = "a" + badGoUTF8 }, `字段 "app"`},
		{"revision", func(in *ReleasePlanInput) { in.Revision = badGoUTF8 }, `字段 "revision"`},
		{"image", func(in *ReleasePlanInput) { in.Image = "img:" + badGoUTF8 }, `字段 "image"`},
		{"spreadBy", func(in *ReleasePlanInput) { in.SpreadBy = "zo" + badGoUTF8 + "ne" }, `字段 "spreadBy"`},
		{"first cluster id", func(in *ReleasePlanInput) { in.Clusters[0].ID = "c" + badGoUTF8 }, `clusters[0]: 字段 "id"`},
		{"second cluster id", func(in *ReleasePlanInput) { in.Clusters[1].ID = badGoUTF8 }, `clusters[1]: 字段 "id"`},
		{"tag key", func(in *ReleasePlanInput) {
			in.Clusters[0].Tags = map[string]string{"env" + badGoUTF8: "prod"}
		}, `clusters[0] 的标签键`},
		{"tag value names key", func(in *ReleasePlanInput) {
			in.Clusters[0].Tags = map[string]string{"env": "pr" + badGoUTF8 + "d"}
		}, `clusters[0] 的标签 "env" 值`},
		{"second cluster tag value", func(in *ReleasePlanInput) {
			in.Clusters[1].Tags = map[string]string{"zone": "w" + anotherBadUTF8}
		}, `clusters[1] 的标签 "zone" 值`},
		{"include condition key", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{{"env" + badGoUTF8: "prod"}}
		}, `include[0] 的标签键`},
		{"include condition value names key", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{{"env": "pr" + badGoUTF8}}
		}, `include[0] 的标签 "env" 值`},
		{"second include condition", func(in *ReleasePlanInput) {
			in.Include = []LabelCondition{{"env": "prod"}, {"region": "u" + badGoUTF8 + "s"}}
		}, `include[1] 的标签 "region" 值`},
		{"exclude condition key", func(in *ReleasePlanInput) {
			in.Exclude = []LabelCondition{{"zone" + badGoUTF8: "west"}}
		}, `exclude[0] 的标签键`},
		{"exclude condition value names key", func(in *ReleasePlanInput) {
			in.Exclude = []LabelCondition{{"zone": "we" + badGoUTF8}}
		}, `exclude[0] 的标签 "zone" 值`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validTextInput()
			tc.mutate(&in)
			verr := ValidateReleaseInput(in)
			if verr == nil {
				t.Fatalf("ValidateReleaseInput must reject invalid UTF-8 in %s", tc.name)
			}
			msg := verr.Error()
			if !strings.Contains(msg, "UTF-8") {
				t.Fatalf("error %q must identify invalid UTF-8", msg)
			}
			if !strings.Contains(msg, tc.wantPart) {
				t.Fatalf("error %q must contain %q", msg, tc.wantPart)
			}
			// MakeReleasePlan must fail the same way and return no plan.
			plan, perr := MakeReleasePlan(in)
			if perr == nil {
				t.Fatalf("MakeReleasePlan must reject invalid UTF-8 in %s", tc.name)
			}
			if !reflect.DeepEqual(plan, ReleasePlan{}) {
				t.Fatalf("corrupt text must yield the zero plan, got %+v", plan)
			}
			if perr.Error() != msg {
				t.Fatalf("Validate and MakeReleasePlan errors differ:\n validate: %v\n plan:     %v", verr, perr)
			}
		})
	}
}

// TestValidate_InvalidUTF8ReachedRegardlessOfBusiness covers the requirement
// that disabled clusters, clusters rules would filter out, tags that never
// participate in matching, and conditions that are never evaluated still
// take part in the text check: identity and text are verified before any
// selection decision.
func TestValidate_InvalidUTF8ReachedRegardlessOfBusiness(t *testing.T) {
	// A disabled cluster still has its ID checked.
	in := validTextInput()
	in.Clusters = []Cluster{{ID: "dead" + badGoUTF8, Disabled: true}}
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), `clusters[0]: 字段 "id"`) {
		t.Fatalf("disabled cluster ID must be checked, got %v", err)
	}

	// A cluster the include rule would filter out still has its tags checked.
	in = validTextInput()
	in.Clusters = []Cluster{{ID: "x", Tags: map[string]string{"env": "dev" + badGoUTF8}}}
	in.Include = []LabelCondition{{"env": "prod"}}
	in.Exclude = nil
	in.SpreadBy = ""
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), `clusters[0] 的标签 "env" 值`) {
		t.Fatalf("filtered-out cluster tags must be checked, got %v", err)
	}

	// A tag key no condition references is still checked.
	in = validTextInput()
	in.Clusters[0].Tags = map[string]string{"unused" + badGoUTF8: "v"}
	in.Include = nil
	in.Exclude = nil
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), "clusters[0] 的标签键") {
		t.Fatalf("unmatched tag key must be checked, got %v", err)
	}

	// A condition is checked even though every cluster is disabled and the
	// condition would never be evaluated.
	in = validTextInput()
	in.Clusters = []Cluster{{ID: "x", Disabled: true}}
	in.Include = []LabelCondition{{"env": "p" + badGoUTF8}}
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), `include[0] 的标签 "env" 值`) {
		t.Fatalf("unevaluated condition must be checked, got %v", err)
	}
}

// TestValidate_CorruptIdentitiesNeverCollapse is the issue itself: two
// different corrupt strings marshal to the same "�", so they must be rejected
// as invalid UTF-8 — never reported as a duplicate of one rewritten identity.
func TestValidate_CorruptIdentitiesNeverCollapse(t *testing.T) {
	// Sanity: the two corrupt IDs really do collapse once marshaled to JSON.
	b1, _ := json.Marshal("c" + badGoUTF8)
	b2, _ := json.Marshal("c" + anotherBadUTF8)
	if string(b1) != string(b2) {
		t.Fatalf("test premise broken: marshaled corrupt IDs differ: %q vs %q", b1, b2)
	}

	in := validTextInput()
	in.SpreadBy = ""
	in.Include = nil
	in.Exclude = nil
	in.Clusters = []Cluster{
		{ID: "c" + badGoUTF8},
		{ID: "c" + anotherBadUTF8},
	}
	err := ValidateReleaseInput(in)
	if err == nil {
		t.Fatal("distinct corrupt IDs must be rejected")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("must be an invalid-UTF-8 error, not a duplicate: %v", err)
	}
	if strings.Contains(err.Error(), "重复") {
		t.Fatalf("corrupt bytes must not be compared as rewritten IDs: %v", err)
	}
	if !strings.Contains(err.Error(), "clusters[0]") {
		t.Fatalf("the first corrupt candidate (position 0) is reported, got %v", err)
	}

	// The same corrupt ID twice is a text error too, not a duplicate error.
	in.Clusters = []Cluster{{ID: "x" + badGoUTF8}, {ID: "x" + badGoUTF8}}
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), "UTF-8") ||
		strings.Contains(err.Error(), "重复") {
		t.Fatalf("repeated corrupt ID must be a text error, got %v", err)
	}

	// Corrupt text plus an unrelated business error: text wins, mirroring the
	// JSON entry where strict text runs before business validation.
	in = validTextInput()
	in.App = "" // also invalid
	in.Clusters[0].ID = "c" + badGoUTF8
	err = ValidateReleaseInput(in)
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("text error must beat the empty-app error, got %v", err)
	}
}

// TestValidate_TextErrorOrderingIsDeterministic fixes the one problem that is
// reported when many strings are corrupt: the field order is app, revision,
// image, spreadBy, candidates (ID before that candidate's tags), include,
// exclude, and the keys within one label set are ascending.
func TestValidate_TextErrorOrderingIsDeterministic(t *testing.T) {
	in := validTextInput()
	in.App = "a" + badGoUTF8
	in.Revision = "r" + badGoUTF8
	in.Image = "i" + badGoUTF8
	in.SpreadBy = "z" + badGoUTF8
	in.Clusters[0].ID = "c" + badGoUTF8
	in.Clusters[0].Tags = map[string]string{"a" + badGoUTF8: "v", "m": "v" + badGoUTF8}
	in.Clusters[1].ID = "d" + badGoUTF8
	in.Include = []LabelCondition{{"env": "p" + badGoUTF8}}
	in.Exclude = []LabelCondition{{"zone": "w" + badGoUTF8}}

	expectNext := func(want string) {
		t.Helper()
		err := ValidateReleaseInput(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
	}
	expectNext(`字段 "app"`)
	in.App = "payments"
	expectNext(`字段 "revision"`)
	in.Revision = "v1.2.3"
	expectNext(`字段 "image"`)
	in.Image = "reg/payments:v1.2.3"
	expectNext(`字段 "spreadBy"`)
	in.SpreadBy = "zone"
	expectNext(`clusters[0]: 字段 "id"`)
	in.Clusters[0].ID = "c1"
	// Within one candidate the ID precedes its tags; within one label set the
	// ascending key's problem wins ("a" before "m").
	expectNext(`clusters[0] 的标签键`)
	in.Clusters[0].Tags = map[string]string{"a": "v", "m": "v" + badGoUTF8}
	expectNext(`clusters[0] 的标签 "m" 值`)
	in.Clusters[0].Tags = map[string]string{"zone": "east", "env": "prod"}
	expectNext(`clusters[1]: 字段 "id"`)
	in.Clusters[1].ID = "c2"
	expectNext(`include[0]`)
	in.Include = []LabelCondition{{"env": "prod"}}
	expectNext(`exclude[0]`)
}

// TestValidate_TextErrorsDoNotDependOnMapOrder repeats validation over maps
// with several corrupt entries: the reported problem must be identical on
// every call even though map iteration order is randomized.
func TestValidate_TextErrorsDoNotDependOnMapOrder(t *testing.T) {
	in := validTextInput()
	in.Clusters[0].Tags = map[string]string{
		"z": "v" + badGoUTF8,
		"a": "v" + anotherBadUTF8,
		"m": "v" + badGoUTF8,
	}
	var first string
	for range 20 {
		err := ValidateReleaseInput(in)
		if err == nil {
			t.Fatal("expected error")
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("non-deterministic error: %q vs %q", err.Error(), first)
		}
	}
	if !strings.Contains(first, `clusters[0] 的标签 "a" 值`) {
		t.Fatalf("ascending key a must win, got %q", first)
	}

	// Same for a condition holding several corrupt keys and values.
	in = validTextInput()
	in.Exclude = []LabelCondition{
		{"z" + badGoUTF8: "w", "a" + anotherBadUTF8: "w", "m": "w" + badGoUTF8},
	}
	first = ""
	for range 20 {
		err := ValidateReleaseInput(in)
		if err == nil {
			t.Fatal("expected error")
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("non-deterministic condition error: %q vs %q", err.Error(), first)
		}
	}
	if !strings.Contains(first, "exclude[0] 的标签键") {
		t.Fatalf("ascending corrupt key a must win for the condition, got %q", first)
	}
}

// TestValidate_InvalidUTF8DoesNotMutateInput ensures rejection never repairs
// the caller's strings or maps: the corrupt byte stays exactly where it was
// and the whole configuration is byte-identical afterwards.
func TestValidate_InvalidUTF8DoesNotMutateInput(t *testing.T) {
	in := validTextInput()
	in.App = "a" + badGoUTF8
	in.Clusters[0].Tags = map[string]string{"env": "pr" + badGoUTF8 + "d"}
	before := fmt.Sprintf("%#v", in)
	_ = ValidateReleaseInput(in)
	_, _ = MakeReleasePlan(in)
	after := fmt.Sprintf("%#v", in)
	if after != before {
		t.Fatalf("input mutated:\n before: %s\n after:  %s", before, after)
	}
	if !strings.Contains(in.App, badGoUTF8) || in.Clusters[0].Tags["env"] != "pr"+badGoUTF8+"d" {
		t.Fatalf("corrupt bytes must be preserved in the caller's config")
	}
}

// TestValidate_LegalTextKeptExactly guards the compatibility side of the
// library path: valid UTF-8 — Chinese, a supplementary plane character, a
// literally written "�" — and ordinary text containing a backslash and
// "uD800" all remain valid, are used as written, and still plan. Empty tag
// values keep matching, and case/whitespace differences stay distinct.
func TestValidate_LegalTextKeptExactly(t *testing.T) {
	in := ReleasePlanInput{
		App:       "支付服务",
		Revision:  "版本一",
		Image:     "镜像/支付:版本一",
		BatchSize: 2,
		SpreadBy:  "区域",
		Clusters: []Cluster{
			{ID: "集群-😀-a", Tags: map[string]string{"区域": "华东😀", "note": "a�b"}},
			{ID: "集群-😀-b", Tags: map[string]string{"区域": "华北", "note": "a\\uD800b"}},
			{ID: "边缘-1", Tags: map[string]string{"区域": "", "env": ""}},
		},
		Include: []LabelCondition{
			{"note": "a\\uD800b"}, // ordinary backslash-uD800 text matches exactly
			{"env": ""},           // empty value is a real fault-domain value
		},
	}
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("legal text must validate: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("legal text must plan: %v", err)
	}
	if plan.App.Name != "支付服务" || plan.App.Revision != "版本一" || plan.App.Image != "镜像/支付:版本一" {
		t.Fatalf("app info rewritten: %+v", plan.App)
	}
	// The second cluster matches the "\uD800" ordinary-text condition; the
	// third matches the empty-value condition; the first cluster matches
	// neither include, so it is excluded and the plan keeps the other two.
	got := strings.Join(plan.Batches[0].Clusters, ",")
	if !strings.Contains(got, "集群-😀-b") || !strings.Contains(got, "边缘-1") {
		t.Fatalf("legal identities must survive into batches, got %+v", plan.Batches)
	}
	if len(plan.Excluded) != 1 || plan.Excluded[0].ID != "集群-😀-a" {
		t.Fatalf("first cluster should be the single include-not-matched one, got %+v", plan.Excluded)
	}

	// A literally written replacement character is legal everywhere.
	lit := validTextInput()
	lit.App = "a�b"
	lit.Clusters[0].Tags["env"] = "prod�"
	lit.Include = []LabelCondition{{"env": "prod�"}}
	if err := ValidateReleaseInput(lit); err != nil {
		t.Fatalf("literal U+FFFD must be legal: %v", err)
	}

	// Case and surrounding whitespace are not normalized.
	caseWS := ReleasePlanInput{
		App: "app", Revision: "r", Image: "i", BatchSize: 10,
		Clusters: []Cluster{{ID: "c-a"}, {ID: "C-A"}, {ID: " c-a "}},
	}
	if err := ValidateReleaseInput(caseWS); err != nil {
		t.Fatalf("case/whitespace-distinct IDs must be legal: %v", err)
	}
	plan, err = MakeReleasePlan(caseWS)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches[0].Clusters) != 3 {
		t.Fatalf("three distinct IDs expected, got %+v", plan.Batches)
	}
}

// wtf8High and wtf8Low are the WTF-8 encodings of a lone high surrogate
// (U+D800) and a lone low surrogate (U+DC00): three bytes each that are not
// valid UTF-8 as a sequence. They are what a caller could still build
// directly in memory, bypassing the JSON entry.
var (
	wtf8High = string([]byte{0xed, 0xa0, 0x80})
	wtf8Low  = string([]byte{0xed, 0xb0, 0x80})
)

// TestValidate_WTF8LoneSurrogateRejected covers the Go-side equivalent of the
// JSON unpaired-surrogate rule: the WTF-8 bytes of a lone surrogate are not
// valid UTF-8. It is rejected; the ordinary characters backslash-u-D-8-0-0
// are not.
func TestValidate_WTF8LoneSurrogateRejected(t *testing.T) {
	in := validTextInput()
	in.App = "a" + wtf8High + "b" // lone high surrogate encoded as WTF-8
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("WTF-8 lone surrogate must be invalid UTF-8, got %v", err)
	}
	in.App = "a" + wtf8Low + "b" // lone low surrogate
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("WTF-8 lone low surrogate must be invalid UTF-8, got %v", err)
	}
	// The same spelling as ordinary text stays legal.
	in.App = `a\uD800b`
	if err := ValidateReleaseInput(in); err != nil {
		t.Fatalf("backslash-uD800 as ordinary text must be legal: %v", err)
	}
}

// TestValidate_TextCheckLeavesAllExistingRulesIntact ensures that with all
// text legal, the pre-existing business errors (empty fields, duplicate IDs,
// empty tag keys, empty conditions) are still reported with their original
// wording — the text check precedes but never replaces them.
func TestValidate_TextCheckLeavesAllExistingRulesIntact(t *testing.T) {
	in := validTextInput()
	in.Clusters[1].ID = "c1" // duplicate of the first candidate
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), `clusters[1]: 重复的集群标识 "c1"`) {
		t.Fatalf("duplicate-ID rule must keep its wording, got %v", err)
	}

	in = validTextInput()
	in.Clusters[0].Tags = map[string]string{"": "v"}
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), "clusters[0] 的标签键不能为空") {
		t.Fatalf("empty-key rule must keep its wording, got %v", err)
	}

	in = validTextInput()
	in.Include = []LabelCondition{{}}
	if err := ValidateReleaseInput(in); err == nil ||
		!strings.Contains(err.Error(), "include[0] 不能为空条件对象") {
		t.Fatalf("empty-condition rule must keep its wording, got %v", err)
	}
}
