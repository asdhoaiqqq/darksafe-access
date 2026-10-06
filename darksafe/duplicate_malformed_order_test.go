package darksafe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file is the regression net for what a caller sees when ONE document
// carries BOTH a duplicate member and a JSON format error. The two existing
// test groups cover each failure in isolation (duplicate_members_test.go and
// the malformed-JSON cases such as big_numbers_test.go); these tests pin
// their meeting order, so reworking the reading logic can never swap the
// problem the user can fix first for another error later in the document.
//
// The rule is file order over the raw structural walk:
//
//   - If the second occurrence of a member name inside one object has already
//     been read before the structure becomes unreadable — the duplicate's
//     value is missing, its object never closes, or trailing content follows
//     one complete value — the already-decided duplicate-member error wins,
//     naming the decoded member and the owning object's position.
//   - If unreadable JSON syntax comes first (a number such as 1e, an extra
//     comma in an array), the JSON format error wins; the walk must not skip
//     past the bad syntax to fish out a later duplicate.
//
// Both failures precede every business check (missing fields, wrong tag value
// types). Failures return the zero-value ReleasePlanInput, and legal
// documents — including legal numbers beyond float64 range in ignored extra
// fields — keep parsing and planning exactly as before.

// parseComboFail parses doc, requiring a non-nil error and the zero-value
// config: no partially read app info or candidate clusters may survive.
func parseComboFail(t *testing.T, doc string) error {
	t.Helper()
	in, err := ParseReleaseInput([]byte(doc))
	if err == nil {
		t.Fatalf("expected an error for document:\n%s", doc)
	}
	if !reflect.DeepEqual(in, ReleasePlanInput{}) {
		t.Fatalf("failure must return the zero config, got %+v\nfor document:\n%s", in, doc)
	}
	return err
}

// assertDuplicateError requires err to be the duplicate-member verdict with
// exactly the decoded member name and owning-object path the caller needs to
// locate the problem — not a JSON format error, not a business error.
func assertDuplicateError(t *testing.T, err error, field, path string) {
	t.Helper()
	dme, ok := err.(*duplicateMemberError)
	if !ok {
		t.Fatalf("expected *duplicateMemberError for field %q at %q, got %T: %v",
			field, path, err, err)
	}
	if dme.field != field {
		t.Fatalf("duplicate field = %q, want %q (path reported: %q)", dme.field, field, dme.path)
	}
	if dme.path != path {
		t.Fatalf("duplicate path = %q, want %q (field reported: %q)", dme.path, path, dme.field)
	}
}

// assertJSONFormatError requires err to be the JSON format error rather than
// a duplicate-member verdict: a duplicate appearing later in the file must
// not be reached across unreadable syntax.
func assertJSONFormatError(t *testing.T, err error) {
	t.Helper()
	var dme *duplicateMemberError
	if errors.As(err, &dme) {
		t.Fatalf("expected a JSON format error to win over the later duplicate, "+
			"got duplicate %q at %s", dme.field, dme.path)
	}
	if !strings.Contains(err.Error(), "JSON 格式错误") {
		t.Fatalf("expected a JSON 格式错误, got %v", err)
	}
}

// TestParse_DuplicateSeenBeforeMalformedJSONWins covers the first half of the
// meeting rule: a duplicate whose second occurrence is readable before the
// document turns malformed stays the reported error. The malformations are
// exactly the ones named in the contract — the duplicate's value is missing,
// its nested object never closes, or extra content follows the complete
// value — exercised on candidate tag objects and on objects nested under
// unknown extra fields (which do not participate in planning but are still
// part of the document).
func TestParse_DuplicateSeenBeforeMalformedJSONWins(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name      string
		doc       string
		wantField string
		wantPath  string
	}{
		// --- candidate cluster tag objects ---
		{
			name:      "tags duplicate then duplicate value missing at EOF",
			doc:       `{` + base + `,"clusters":[{"id":"x","tags":{"env":"prod","env":`,
			wantField: "env",
			wantPath:  `$.clusters[0].tags`,
		},
		{
			name:      "tags duplicate then nested objects left unclosed",
			doc:       `{` + base + `,"clusters":[{"id":"x","tags":{"env":"prod","env":"dev"}}`,
			wantField: "env",
			wantPath:  `$.clusters[0].tags`,
		},
		{
			name:      "tags duplicate in a complete document followed by junk",
			doc:       `{` + base + `,"clusters":[{"id":"x","tags":{"env":"prod","env":"dev"}}]}junk`,
			wantField: "env",
			wantPath:  `$.clusters[0].tags`,
		},

		// --- objects nested under unknown extra fields ---
		{
			name:      "extra object duplicate then value missing",
			doc:       `{` + base + `,"clusters":[],"meta":{"x":1,"x":`,
			wantField: "x",
			wantPath:  `$.meta`,
		},
		{
			name:      "extra object duplicate then object unclosed",
			doc:       `{` + base + `,"clusters":[],"meta":{"x":1,"x":2}`,
			wantField: "x",
			wantPath:  `$.meta`,
		},
		{
			name:      "extra object duplicate in complete document then junk",
			doc:       `{` + base + `,"clusters":[],"meta":{"x":1,"x":2}}junk`,
			wantField: "x",
			wantPath:  `$.meta`,
		},
		{
			name:      "extra object duplicate then trailing comma in that object",
			doc:       `{` + base + `,"clusters":[],"meta":{"x":1,"x":2,}}`,
			wantField: "x",
			wantPath:  `$.meta`,
		},
		{
			name:      "duplicate object inside array then junk before array closes",
			doc:       `{` + base + `,"clusters":[],"a":[{"x":1,"x":2}junk]}`,
			wantField: "x",
			wantPath:  `$.a[0]`,
		},
		{
			name:      "duplicate in second array element, real index kept, unclosed",
			doc:       `{` + base + `,"clusters":[],"items":[{"k":1},{"y":1,"y":2}`,
			wantField: "y",
			wantPath:  `$.items[1]`,
		},
		{
			name:      "duplicate in deeply nested unknown object, unclosed",
			doc:       `{` + base + `,"clusters":[],"a":{"b":{"c":[{"z":1,"z":2}`,
			wantField: "z",
			wantPath:  `$.a.b.c[0]`,
		},

		// --- decoded-name and path conventions under the same malformation ---
		{
			name: "tags duplicate via equivalent Unicode escape then value missing",
			doc: `{` + base + `,"clusters":[{"id":"x",` +
				`"tags":{"env":"prod","` + jsonUEsc('e') + `nv":}}]}`,
			wantField: "env",
			wantPath:  `$.clusters[0].tags`,
		},
		{
			name:      "extra object duplicate via equivalent Unicode escape, unclosed",
			doc:       `{` + base + `,"clusters":[],"meta":{"x":1,"` + jsonUEsc('x') + `":2}`,
			wantField: "x",
			wantPath:  `$.meta`,
		},
		{
			name:      "dotted single member name stays one bracketed segment with junk after",
			doc:       `{` + base + `,"clusters":[],"a.b":{"x":1,"x":2}}junk`,
			wantField: "x",
			wantPath:  `$["a.b"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseComboFail(t, tc.doc)
			assertDuplicateError(t, err, tc.wantField, tc.wantPath)
			// The dotted-name case must not be rendered as a two-level path.
			if tc.wantPath == `$["a.b"]` {
				if strings.Contains(err.Error(), "$.a.b") {
					t.Fatalf("single dotted member name split into two levels in %q", err)
				}
			}
		})
	}
}

// TestParse_MalformedJSONSeenBeforeDuplicateWins covers the second half:
// unreadable syntax appearing first is the reported error even though a
// duplicate member occurs later in the same document. The walk must not jump
// over the bad syntax to select the later duplicate.
func TestParse_MalformedJSONSeenBeforeDuplicateWins(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name string
		doc  string
	}{
		{
			name: "malformed number 1e in extra field before duplicate object",
			doc:  `{` + base + `,"clusters":[],"bad":1e,"meta":{"y":1,"y":2}}`,
		},
		{
			name: "malformed number in first cluster before duplicate tags in second",
			doc: `{` + base + `,"clusters":[` +
				`{"id":"a","w":1e},` +
				`{"id":"b","tags":{"env":1,"env":2}}]}`,
		},
		{
			name: "extra comma in array before duplicate object",
			doc:  `{` + base + `,"clusters":[],"a":[1,,3],"meta":{"y":1,"y":2}}`,
		},
		{
			name: "extra comma inside the very object that later repeats a name",
			doc:  `{` + base + `,"clusters":[],"meta":{"x":1,,"y":1,"y":2}}`,
		},
		{
			name: "bad array syntax before a duplicate in a later object",
			doc: `{` + base + `,"clusters":[],` +
				`"a":[1,,2],"other":{"env":1,"env":2}}`,
		},
		{
			name: "malformed number after a complete value, later top-level duplicate",
			doc:  `{` + base + `,"clusters":[],"bad":1e,"revision":"r2"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseComboFail(t, tc.doc)
			assertJSONFormatError(t, err)
		})
	}
}

// TestParse_DuplicateAndMalformedBeatBusinessErrors pins both structural
// failures ahead of business validation: a missing app field and a wrong tag
// value type must not be reported while an earlier-in-file duplicate or bad
// syntax is present.
func TestParse_DuplicateAndMalformedBeatBusinessErrors(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		name  string
		doc   string
		dup   bool   // true: duplicate verdict; false: JSON format verdict
		field string // dup-only expectations
		path  string
	}{
		{
			name:  "missing app plus duplicate in extra object plus missing value",
			doc:   `{"revision":"r","image":"i","batchSize":1,"clusters":[],"meta":{"x":1,"x":`,
			dup:   true,
			field: "x",
			path:  `$.meta`,
		},
		{
			name: "missing app plus malformed number before a later duplicate",
			doc: `{"revision":"r","image":"i","batchSize":1,"clusters":[],` +
				`"bad":1e,"meta":{"y":1,"y":2}}`,
			dup: false,
		},
		{
			name: "duplicate after a cluster whose tag value has the wrong type",
			doc: `{` + base + `,"clusters":[{"id":"x","tags":{"env":1}}],` +
				`"meta":{"k":1,"k":2}`,
			dup:   true,
			field: "k",
			path:  `$.meta`,
		},
		{
			name: "malformed number before a wrong-typed tag value",
			doc:  `{` + base + `,"clusters":[{"id":"x","w":1e,"tags":{"env":1}}]}`,
			dup:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseComboFail(t, tc.doc)
			if tc.dup {
				assertDuplicateError(t, err, tc.field, tc.path)
				return
			}
			assertJSONFormatError(t, err)
		})
	}
}

// TestParse_HugeLegalNumbersDoNotChangeMeetingOrder locks the interaction with
// legal JSON numbers beyond float64 range in ignored extra fields: 1e400 is
// well-formed JSON and must never be mistaken for a format error, so it does
// not move the boundary between the two structural verdicts — a duplicate on
// either side of it is still the duplicate, and only a genuinely malformed
// number (1e) produces the JSON format error.
func TestParse_HugeLegalNumbersDoNotChangeMeetingOrder(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`

	// Legal huge number first, duplicate later: the duplicate is reached and
	// reported; the huge number is not a format error.
	err := parseComboFail(t, `{`+base+`,"clusters":[],"limit":1e400,"meta":{"y":1,"y":2}}`)
	assertDuplicateError(t, err, "y", "$.meta")

	// Duplicate first, legal huge number after it: the earlier duplicate wins
	// instead of the document being reclassified as malformed.
	err = parseComboFail(t, `{`+base+`,"clusters":[{"id":"x","tags":{"env":1,"env":2}}],"limit":1e400}`)
	assertDuplicateError(t, err, "env", "$.clusters[0].tags")

	// Legal huge number, then a genuinely malformed number, then a duplicate:
	// the malformed number sets the boundary, so the format error wins.
	err = parseComboFail(t, `{`+base+`,"clusters":[],"limit":1e400,"bad":1e,"meta":{"y":1,"y":2}}`)
	assertJSONFormatError(t, err)

	// A legal document carrying huge numbers in extra fields still parses and
	// plans identically; nothing in these ordering checks disturbs acceptance.
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
		"exclude": [{"region": "us"}],
		"meta": {"limit": 1e400, "notes": [1e400, {"deep": 1e400}]}
	}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatalf("legal config with huge extra numbers must parse: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("legal config must still plan: %v", err)
	}
	if len(plan.Batches) != 1 || strings.Join(plan.Batches[0].Clusters, ",") != "c3" {
		t.Fatalf("unexpected batches: %+v", plan.Batches)
	}
	if len(plan.Excluded) != 3 {
		t.Fatalf("expected the same 3 not-selected reasons, got %+v", plan.Excluded)
	}
}
