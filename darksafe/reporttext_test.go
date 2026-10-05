package darksafe

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file covers the display-only rendering of a cluster ID inside the
// all-candidates-rejected failure report (clusterIDForReport in
// reporttext.go). A legal ID may carry newlines, carriage returns and other
// control characters; the rendering must keep one report line per
// candidate while staying decodable back to the exact original ID. The
// library/CLI end-to-end behavior built on this rendering is covered in
// rejection_report_id_test.go.

// TestClusterIDForReport_PlainIDsVerbatim pins the established format for
// ordinary IDs: no quotes, no escaping, regardless of Unicode plane — the
// two-space indent, the ID and the Chinese colon are added by the caller.
func TestClusterIDForReport_PlainIDsVerbatim(t *testing.T) {
	for _, id := range []string{
		"c-a",
		"C-A",
		" c-a ",
		"集群-华东-1",
		"😀-cluster",
	} {
		if got := clusterIDForReport(id); got != id {
			t.Fatalf("plain ID must pass through verbatim: got %q, want %q", got, id)
		}
	}
}

// TestClusterIDForReport_QuotedDecodable covers every trigger: a double
// quote, a backslash, every kind of control character (C0 and C1, including
// U+007F and the C1 line separator U+0085) and U+2028/U+2029. Each rendered
// form must be exactly one physical line containing no control effect and
// must decode as a JSON string back to the exact original ID.
func TestClusterIDForReport_QuotedDecodable(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"double quote", `a"b`},
		{"backslash n", `a\nb`},
		{"real newline", "a\nb"},
		{"carriage return", "a\rb"},
		{"crlf", "a\r\nb"},
		{"tab", "a\tb"},
		{"bell", "a\x07b"},
		{"nul", "a\x00b"},
		{"escape", "a\x1bb"},
		{"delete U+007F", "a\x7fb"},
		{"C1 control U+0085", "a\xc2\x85b"},
		{"C1 control U+009F", "a\xc2\x9fb"},
		{"line separator U+2028", "a b"},
		{"paragraph separator U+2029", "a b"},
		{"quote and newline", "a\"\nb"},
		{"backslash and real newline", "a\\\nb"},
		{"newline between c-a and c-b", "c-a\nc-b"},
		{"chinese with control", "集群\n一"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clusterIDForReport(tc.id)
			if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
				t.Fatalf("rendered ID must be wrapped in double quotes: %q", got)
			}
			// One rendered record must never carry a real control effect:
			// no newline, carriage return, tab or any other Cc rune, and no
			// U+2028/U+2029 — those characters may only appear escaped.
			inner := got
			for _, r := range inner {
				if r <= 0x1F || (r >= 0x7F && r <= 0x9F) || r == 0x2028 || r == 0x2029 {
					t.Fatalf("rendered ID contains unescaped control/separator rune %q: %q", r, got)
				}
			}
			if strings.ContainsAny(got, "\n\r\t") {
				t.Fatalf("rendered ID contains a real whitespace control effect: %q", got)
			}
			var decoded string
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatalf("rendered ID is not a decodable JSON string: %v (%q)", err, got)
			}
			if decoded != tc.id {
				t.Fatalf("rendered ID does not restore the original:\n got %q\nwant %q\nrendered %q", decoded, tc.id, got)
			}
		})
	}
}

// TestClusterIDForReport_DistinguishesLookalikes pins the examples from the
// report contract: a real newline and a backslash-n must render differently,
// and an ID's own quote must not merge visually with the wrapping quotes.
func TestClusterIDForReport_DistinguishesLookalikes(t *testing.T) {
	realNewline := clusterIDForReport("c-a\nc-b")
	backslashN := clusterIDForReport(`c-a\nc-b`)
	if realNewline == backslashN {
		t.Fatalf("real-newline ID and backslash-n ID rendered identically: %q", realNewline)
	}
	if want := `"c-a\nc-b"`; realNewline != want {
		t.Fatalf("real newline rendering = %q, want %q", realNewline, want)
	}
	if want := `"c-a\\nc-b"`; backslashN != want {
		t.Fatalf("backslash-n rendering = %q, want %q", backslashN, want)
	}

	quoted := clusterIDForReport(`a"b`)
	if want := `"a\"b"`; quoted != want {
		t.Fatalf("quoted ID rendering = %q, want %q", quoted, want)
	}
	var decoded string
	if err := json.Unmarshal([]byte(quoted), &decoded); err != nil || decoded != `a"b` {
		t.Fatalf("quoted ID must decode to the original, got %q err %v", decoded, err)
	}
}

// TestClusterIDForReport_DoesNotMutateOriginal confirms rendering is
// display-only: the string handed in is returned unchanged by the caller's
// sorting/comparison paths (rendering takes the value, not a pointer, but
// the test also documents no package-level state distinguishes calls).
func TestClusterIDForReport_DoesNotMutateOriginal(t *testing.T) {
	id := "c-a\nc-b"
	original := id
	_ = clusterIDForReport(id)
	if id != original {
		t.Fatalf("rendering must not alter the original ID: %q vs %q", id, original)
	}
	// Repeated rendering is stable.
	first := clusterIDForReport(id)
	second := clusterIDForReport(id)
	if first != second {
		t.Fatalf("rendering must be stable: %q vs %q", first, second)
	}
}
