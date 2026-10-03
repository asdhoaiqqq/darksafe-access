package darksafe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dupPath parses data expecting a duplicate-member error and returns the
// reported object position (the text after "重复出现于 ") and field name.
func dupPath(t *testing.T, data string) (field, path string) {
	t.Helper()
	err := parseDupErr(t, data)
	msg := err.Error()
	const marker = "重复出现于 "
	idx := strings.LastIndex(msg, marker)
	if idx < 0 {
		t.Fatalf("error %q has no position marker", msg)
	}
	// Field is shown with %q right after "字段 ".
	fm := regexp.MustCompile(`字段 (.*?) 重复出现于`).FindStringSubmatch(msg)
	if fm == nil {
		t.Fatalf("error %q does not name the duplicate field", msg)
	}
	if err := json.Unmarshal([]byte(fm[1]), &field); err != nil {
		t.Fatalf("field %s is not a JSON-quoted string: %v", fm[1], err)
	}
	return field, msg[idx+len(marker):]
}

// decodePathSteps splits a position text into its member steps, decoding each
// bracket-quoted name back to its real member name. Dot steps are returned
// verbatim; array indices and bracket strings are all represented as strings.
// It round-trips the rule that a bracket step must decode to exactly the
// member name it stands for.
func decodePathSteps(t *testing.T, path string) []string {
	t.Helper()
	if !strings.HasPrefix(path, "$") {
		t.Fatalf("path %q must start at $", path)
	}
	var steps []string
	rest := path[1:]
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			steps = append(steps, rest[:end])
			rest = rest[end:]
		case strings.HasPrefix(rest, "["):
			if len(rest) > 1 && rest[1] == '"' {
				// A bracket-quoted member name may itself contain brackets, so the
				// closing bracket is the one after the JSON string's terminating
				// quote. Locate that quote by scanning the string escapes.
				i := 2
				for i < len(rest) {
					switch rest[i] {
					case '\\':
						i += 2
					case '"':
						goto foundQuote
					default:
						i++
					}
				}
				t.Fatalf("path %q has an unterminated bracket string", path)
			foundQuote:
				close := i + 1
				if close >= len(rest) || rest[close] != ']' {
					t.Fatalf("path %q bracket string is not closed at %d", path, close)
				}
				inside := rest[1:close]
				rest = rest[close+1:]
				var name string
				if err := json.Unmarshal([]byte(inside), &name); err != nil {
					t.Fatalf("bracket step %s is not a decodable JSON string: %v", inside, err)
				}
				steps = append(steps, "name:"+name)
			} else {
				close := strings.Index(rest, "]")
				if close < 0 {
					t.Fatalf("path %q has an unclosed bracket", path)
				}
				steps = append(steps, "index:"+rest[1:close])
				rest = rest[close+1:]
			}
		default:
			t.Fatalf("path %q has unexpected text %q", path, rest)
		}
	}
	return steps
}

// TestParse_DuplicatePathDistinguishesDotInNameFromHierarchy is the core
// regression: a top-level member literally named "meta.info" and a member
// "info" nested under "meta" must not share a position. The former is
// $["meta.info"]; the latter stays $.meta.info.
func TestParse_DuplicatePathDistinguishesDotInNameFromHierarchy(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	topLevel := `{` + base + `,"meta.info":{"x":1,"x":2}}`
	if field, path := dupPath(t, topLevel); field != "x" || path != `$["meta.info"]` {
		t.Fatalf("top-level dotted name: got field=%q path=%q", field, path)
	}
	nested := `{` + base + `,"meta":{"info":{"x":1,"x":2}}}`
	if field, path := dupPath(t, nested); field != "x" || path != `$.meta.info` {
		t.Fatalf("nested object: got field=%q path=%q", field, path)
	}
}

// TestParse_DuplicatePathBracketNotation covers member names whose characters
// would otherwise be misread as hierarchy or array indexing: dots, brackets,
// leading digits, spaces, and the empty name. Each reported path is checked
// exactly and then decoded back to the real member name.
func TestParse_DuplicatePathBracketNotation(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	cases := []struct {
		name      string // real (decoded) member name
		body      string // JSON body without outer braces
		wantPath  string
		wantSteps []string // decoded steps after "$"
	}{
		{
			name:      "",
			body:      base + `,"":{"x":1,"x":2}`,
			wantPath:  `$[""]`,
			wantSteps: []string{"name:"},
		},
		{
			name:      "meta.info",
			body:      base + `,"meta.info":{"x":1,"x":2}`,
			wantPath:  `$["meta.info"]`,
			wantSteps: []string{"name:meta.info"},
		},
		{
			name:      "zone[0]",
			body:      base + `,"zone[0]":{"x":1,"x":2}`,
			wantPath:  `$["zone[0]"]`,
			wantSteps: []string{"name:zone[0]"},
		},
		{
			name:      `a"b`,
			body:      base + `,"a\"b":{"x":1,"x":2}`,
			wantPath:  `$["a\"b"]`,
			wantSteps: []string{"name:a\"b"},
		},
		{
			name:      `a\b`,
			body:      base + `,"a\\b":{"x":1,"x":2}`,
			wantPath:  `$["a\\b"]`,
			wantSteps: []string{`name:a\b`},
		},
		{
			name:      "a\nb",
			body:      base + `,"a\nb":{"x":1,"x":2}`,
			wantPath:  `$["a\nb"]`,
			wantSteps: []string{"name:a\nb"},
		},
		{
			name:      "0",
			body:      base + `,"0":{"x":1,"x":2}`,
			wantPath:  `$["0"]`,
			wantSteps: []string{"name:0"},
		},
		{
			name: "k.k inside a nested object under a dotted name",
			body: base + `,"meta.info":{"nested":{"k.k":[{"z":1,"z":2}]}}`,
			wantPath: `$["meta.info"].nested["k.k"][0]`,
			wantSteps: []string{
				"name:meta.info", "nested", "name:k.k", "index:0",
			},
		},
		{
			name:      "zone[0] as a tags key stays one member name",
			body:      `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[{"id":"x","tags":{"zone[0]":1,"zone[0]":2}}]`,
			wantPath:  `$.clusters[0].tags`,
			wantSteps: []string{"clusters", "index:0", "tags"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field, path := dupPath(t, "{"+tc.body+"}")
			if field != "x" && field != "zone[0]" && field != "z" {
				t.Fatalf("unexpected duplicate field %q", field)
			}
			if path != tc.wantPath {
				t.Fatalf("path mismatch:\n got %q\nwant %q", path, tc.wantPath)
			}
			if got := decodePathSteps(t, path); fmt.Sprint(got) != fmt.Sprint(tc.wantSteps) {
				t.Fatalf("decoded steps mismatch:\n got %q\nwant %q", got, tc.wantSteps)
			}
		})
	}
}

// TestParse_DuplicatePathDotNotationUnchanged guards the positions that must
// remain exactly as before: plain identifiers keep dots and arrays keep
// zero-based indices.
func TestParse_DuplicatePathDotNotationUnchanged(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1`
	cases := []struct {
		body     string
		wantPath string
	}{
		{`{"app":"a","app":"b","revision":"r","image":"i","batchSize":1,"clusters":[]}`, `$`},
		{`{` + base + `,"clusters":[{"id":"x","disabled":true,"disabled":false}]}`, `$.clusters[0]`},
		{`{` + base + `,"clusters":[{"id":"x","tags":{"env":"p","env":"d"}}]}`, `$.clusters[0].tags`},
		{`{` + base + `,"clusters":[],"include":[{},{"env":"p","env":"d"}]}`, `$.include[1]`},
		{`{` + base + `,"clusters":[],"a":{"b":{"c":[{"z":1,"z":2}]}}}`, `$.a.b.c[0]`},
	}
	for i, tc := range cases {
		_, path := dupPath(t, tc.body)
		if path != tc.wantPath {
			t.Fatalf("case %d: got %q want %q", i, path, tc.wantPath)
		}
	}
}

// TestParse_DuplicatePathIndependentOfEscapeSpelling checks that the path is
// expressed from the JSON-decoded name: a dotted name written with an
// equivalent Unicode escape for its dot (or letters) yields the same bracket
// position as the direct spelling.
func TestParse_DuplicatePathIndependentOfEscapeSpelling(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	direct := `{` + base + `,"meta.info":{"x":1,"x":2}}`
	// The dot, then the leading "m", are written as JSON Unicode escapes that
	// decode to the very same member name "meta.info".
	escapedDot := `{` + base + `,"meta` + jsonUEsc('.') + `info":{"x":1,"x":2}}`
	escapedLetter := `{` + base + `,"` + jsonUEsc('m') + `eta.info":{"x":1,"x":2}}`
	_, p1 := dupPath(t, direct)
	_, p2 := dupPath(t, escapedDot)
	_, p3 := dupPath(t, escapedLetter)
	if p1 != p2 || p2 != p3 {
		t.Fatalf("equivalent spellings gave different paths: %q %q %q", p1, p2, p3)
	}
	if p1 != `$["meta.info"]` {
		t.Fatalf("unexpected path %q", p1)
	}

	// A duplicate pair itself written with decode-equivalent spellings of a
	// dotted name reports the decoded name and a bracket position.
	doc := `{` + base + `,"a.b":1,"a` + jsonUEsc('.') + `b":2}`
	field, path := dupPath(t, doc)
	if field != "a.b" || path != `$` {
		t.Fatalf("got field=%q path=%q, want a.b at $", field, path)
	}
}

// TestParse_SpecialNamesWithoutDuplicatesStillAccepted ensures unusual names
// are only a path-formatting concern: an unknown field with a dot, brackets,
// quote, backslash, newline or empty name is ignored as before rather than
// rejecting the configuration, and the plan is computed normally.
func TestParse_SpecialNamesWithoutDuplicatesStillAccepted(t *testing.T) {
	raw := `{
		"app": "payments", "revision": "v1", "image": "reg/payments:v1",
		"batchSize": 1,
		"meta.info": {"zone[0]": 1, "": 2, "q\"q": 3, "b\\b": 4, "nl": "a\nb"},
		"clusters": [{"id": "c1", "tags": {"a.b": "v"}}, {"id": "c2"}]
	}`
	in, err := ParseReleaseInput([]byte(raw))
	if err != nil {
		t.Fatalf("special-named unknown fields must be accepted: %v", err)
	}
	plan, err := MakeReleasePlan(in)
	if err != nil {
		t.Fatalf("plan must still build: %v", err)
	}
	if got := strings.Join(plan.Batches[0].Clusters, ","); got != "c1" || plan.Batches[1].Clusters[0] != "c2" {
		t.Fatalf("unexpected batches: %+v", plan.Batches)
	}
}

// TestPlanCLI_DottedNamePositionIsUnambiguous drives the CLI with both
// colliding spellings: stderr must give the corrected object position, stdout
// must stay empty and the exit status must be non-zero.
func TestPlanCLI_DottedNamePositionIsUnambiguous(t *testing.T) {
	base := `"app":"a","revision":"r","image":"i","batchSize":1,"clusters":[]`
	docs := map[string]string{
		`$["meta.info"]`: `{` + base + `,"meta.info":{"x":1,"x":2}}`,
		`$.meta.info`:    `{` + base + `,"meta":{"info":{"x":1,"x":2}}}`,
	}
	for wantPath, doc := range docs {
		path := filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runCLI(t, "plan", path)
		if code == 0 {
			t.Fatalf("%s: expected non-zero exit, stdout=%q stderr=%q", wantPath, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("%s: expected empty stdout, got %q", wantPath, stdout)
		}
		if !strings.Contains(stderr, `"x"`) || !strings.Contains(stderr, wantPath) {
			t.Fatalf("%s: stderr should name field and corrected position, got %q", wantPath, stderr)
		}
	}
}
