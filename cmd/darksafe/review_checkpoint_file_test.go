// Unit tests for the strict checkpoint-file parser behind
// `darksafe review --checkpoint`. These pin the accepted text shape (each
// of org/end_seq/fingerprint once in any order, one optional trailing
// newline, nothing else), the exact-byte recovery of the quoted
// organization, and the usage-level error for every malformed shape.
package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func TestParseCheckpointAcceptsCanonicalShapes(t *testing.T) {
	fp := "04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299"
	cases := []struct {
		name string
		data []byte
		want darksafe.Checkpoint
	}{
		{"canonical",
			[]byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s\n", "acme factory", fp)),
			darksafe.Checkpoint{Org: "acme factory", EndSeq: 2, Fingerprint: fp}},
		{"no trailing newline",
			[]byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s", "acme factory", fp)),
			darksafe.Checkpoint{Org: "acme factory", EndSeq: 2, Fingerprint: fp}},
		{"reordered",
			[]byte(fmt.Sprintf("fingerprint=%s\norg=%q\nend_seq=2\n", fp, "acme factory")),
			darksafe.Checkpoint{Org: "acme factory", EndSeq: 2, Fingerprint: fp}},
		{"end seq first",
			[]byte(fmt.Sprintf("end_seq=2\nfingerprint=%s\norg=%q\n", fp, "acme factory")),
			darksafe.Checkpoint{Org: "acme factory", EndSeq: 2, Fingerprint: fp}},
		{"genesis end seq zero",
			[]byte(fmt.Sprintf("org=%q\nend_seq=0\nfingerprint=%s\n", "new org", fp)),
			darksafe.Checkpoint{Org: "new org", EndSeq: 0, Fingerprint: fp}},
		{"org value contains equals",
			[]byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s\n", "a=b", fp)),
			darksafe.Checkpoint{Org: "a=b", EndSeq: 2, Fingerprint: fp}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCheckpointData(tc.data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parsed = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParseCheckpointRejectsMalformedContent enumerates every malformed
// shape the spec names. Each must fail at parse time (the command's exit-2
// family), never reach archive validation.
func TestParseCheckpointRejectsMalformedContent(t *testing.T) {
	const (
		org = "acme factory"
		fp  = "04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299"
	)
	good := func() []byte { return []byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s\n", org, fp)) }

	cases := map[string][]byte{
		"missing org":                 []byte("end_seq=2\nfingerprint=" + fp + "\n"),
		"missing end_seq":             []byte(fmt.Sprintf("org=%q\nfingerprint=%s\n", org, fp)),
		"missing fingerprint":         []byte(fmt.Sprintf("org=%q\nend_seq=2\n", org)),
		"empty file":                  {},
		"only a newline":              []byte("\n"),
		"blank line in middle":        []byte(fmt.Sprintf("org=%q\n\nend_seq=2\nfingerprint=%s\n", org, fp)),
		"two trailing newlines":       append(good(), '\n'),
		"duplicate org":               append(good(), []byte(fmt.Sprintf("org=%q\n", org))...),
		"duplicate end_seq":           append(good(), []byte("end_seq=2\n")...),
		"duplicate fingerprint":       append(good(), []byte("fingerprint="+fp+"\n")...),
		"duplicate same value inline": []byte(fmt.Sprintf("org=%q\norg=%q\nend_seq=2\nfingerprint=%s\n", org, org, fp)),
		"unknown field":               append(good(), []byte("extra=1\n")...),
		"unknown field empty value":   append(good(), []byte("bogus=\n")...),
		"junk line without equals":    append(good(), []byte("not a key value pair\n")...),
		"org unquoted":                []byte("org=acme\nend_seq=2\nfingerprint=" + fp + "\n"),
		"org single quoted":           []byte("org='acme'\nend_seq=2\nfingerprint=" + fp + "\n"),
		"org backtick raw string":     []byte("org=`acme`\nend_seq=2\nfingerprint=" + fp + "\n"),
		"org empty quoted":            []byte("org=\"\"\nend_seq=2\nfingerprint=" + fp + "\n"),
		"org unterminated quote":      []byte("org=\"acme\nend_seq=2\nfingerprint=" + fp + "\n"),
		"org bad hex escape":          []byte("org=\"a\\xzz\"\nend_seq=2\nfingerprint=" + fp + "\n"),
		"org illegal escape":          []byte("org=\"a\\z\"\nend_seq=2\nfingerprint=" + fp + "\n"),
		"org dangling backslash":      []byte("org=\"a\\\nend_seq=2\nfingerprint=" + fp + "\n"),
		"end seq negative":            []byte(fmt.Sprintf("org=%q\nend_seq=-1\nfingerprint=%s\n", org, fp)),
		"end seq explicit plus":       []byte(fmt.Sprintf("org=%q\nend_seq=+2\nfingerprint=%s\n", org, fp)),
		"end seq empty":               []byte(fmt.Sprintf("org=%q\nend_seq=\nfingerprint=%s\n", org, fp)),
		"end seq decimal point":       []byte(fmt.Sprintf("org=%q\nend_seq=2.0\nfingerprint=%s\n", org, fp)),
		"end seq hex spelling":        []byte(fmt.Sprintf("org=%q\nend_seq=0x2\nfingerprint=%s\n", org, fp)),
		"end seq with space":          []byte(fmt.Sprintf("org=%q\nend_seq= 2\nfingerprint=%s\n", org, fp)),
		"end seq too large":           []byte(fmt.Sprintf("org=%q\nend_seq=%s9\nfingerprint=%s\n", org, strconv.Itoa(math.MaxInt), fp)),
		"end seq huge": []byte(fmt.Sprintf("org=%q\nend_seq=%s\nfingerprint=%s\n",
			org, strings.Repeat("9", 100), fp)),
		"fingerprint too short":    []byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s\n", org, fp[:63])),
		"fingerprint too long":     []byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s\n", org, fp+"00")),
		"fingerprint non hex":      []byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s\n", org, fp[:63]+"g")),
		"fingerprint empty":        []byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=\n", org)),
		"trailing spaces after fp": []byte(fmt.Sprintf("org=%q\nend_seq=2\nfingerprint=%s   \n", org, fp)),
		"carriage return at eol":   []byte(fmt.Sprintf("org=%q\r\nend_seq=2\nfingerprint=%s\n", org, fp)),
		"space before key":         []byte(fmt.Sprintf(" org=%q\nend_seq=2\nfingerprint=%s\n", org, fp)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseCheckpointData(data)
			if err == nil {
				t.Fatalf("expected rejection, contents:\n%s", data)
			}
		})
	}
}

func TestParseCheckpointEndSeqBoundaries(t *testing.T) {
	fp := "04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299"
	ok := map[string]int{"0": 0, "7": 7, "007": 7, strconv.Itoa(math.MaxInt): math.MaxInt}
	for spelling, want := range ok {
		data := []byte(fmt.Sprintf("org=\"acme\"\nend_seq=%s\nfingerprint=%s\n", spelling, fp))
		cp, err := parseCheckpointData(data)
		if err != nil {
			t.Fatalf("end_seq=%s must parse: %v", spelling, err)
		}
		if cp.EndSeq != want {
			t.Fatalf("end_seq=%s parsed to %d, want %d", spelling, cp.EndSeq, want)
		}
	}
}

// TestParseCheckpointRecoversOrgBytes is the byte-faithfulness guarantee:
// Chinese, leading/trailing spaces, escaped control characters and escaped
// non-UTF-8 bytes all come back exactly, and a lone 0xFF, a lone 0xFE and a
// genuine U+FFFD stay three different strings rather than collapsing.
func TestParseCheckpointRecoversOrgBytes(t *testing.T) {
	fp := "04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299"
	orgs := map[string]string{
		"chinese with spaces": " 空格 组织 ",
		"tab newline null":    "a\tb\nc\x00d",
		"quote backslash":     `a"b\c`,
		"lone ff":             "org\xff",
		"lone fe":             "org\xfe",
		"genuine ufffd":       "org" + string(rune(0xFFFD)),
	}
	recovered := map[string]string{}
	for name, want := range orgs {
		data := []byte(fmt.Sprintf("org=%s\nend_seq=0\nfingerprint=%s\n", strconv.Quote(want), fp))
		cp, err := parseCheckpointData(data)
		if err != nil {
			t.Fatalf("%s: parse failed: %v\n%s", name, err, data)
		}
		if cp.Org != want {
			t.Fatalf("%s: org = %q, want %q", name, cp.Org, want)
		}
		recovered[name] = cp.Org
	}
	if recovered["lone ff"] == recovered["lone fe"] ||
		recovered["lone ff"] == recovered["genuine ufffd"] ||
		recovered["lone fe"] == recovered["genuine ufffd"] {
		t.Fatalf("distinct invalid bytes collapsed: %q %q %q",
			recovered["lone ff"], recovered["lone fe"], recovered["genuine ufffd"])
	}
}
