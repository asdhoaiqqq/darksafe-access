// Tests for `darksafe review --checkpoint FILE`: reading the independently
// saved checkpoint document instead of typing --org/--end-seq/--fingerprint.
// The coverage mirrors the contract: file input is exactly equivalent to
// the three flags (byte-identical text and --json reports, no extra file
// notice), the two input forms are structurally mutually exclusive, a
// missing/unreadable/malformed checkpoint file is an invocation failure
// (exit 2, empty stdout), while a well-formed file whose organization or
// checkpoint does not match the archive stays a material failure (exit 1)
// and never falls back to the archive's embedded checkpoint.
package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// checkpointReviewArgs builds file-mode review arguments.
func checkpointReviewArgs(archive, checkpoint string, seq int) []string {
	return []string{
		"review",
		"--archive", archive,
		"--seq", strconv.Itoa(seq),
		"--checkpoint", checkpoint,
	}
}

// writeCheckpointText writes content into a fresh file in the test's temp
// directory and returns its path.
func writeCheckpointText(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// canonicalCheckpointText reproduces exactly the document
// examples/offline_review writes: %q-quoted org, one field per line, one
// trailing newline.
func canonicalCheckpointText(cp darksafe.Checkpoint) string {
	return fmt.Sprintf("org=%q\nend_seq=%d\nfingerprint=%s\n", cp.Org, cp.EndSeq, cp.Fingerprint)
}

// fixtureChainAndCheckpoint writes both the archive and a canonical
// separately saved checkpoint file, returning both paths and the checkpoint.
func fixtureChainAndCheckpoint(t *testing.T, org string) (string, string, darksafe.Checkpoint) {
	t.Helper()
	archive, cp := fixtureChain(t, org)
	cpPath := writeCheckpointText(t, "retained.checkpoint", canonicalCheckpointText(cp))
	return archive, cpPath, cp
}

// TestReviewCheckpointEquivalentToFlags is the core equivalence guarantee:
// the same checkpoint content delivered via the file must produce the exact
// same text report, the exact same --json object, the same exit code and the
// same (empty) stderr as typing the three flags — with no file-reading
// notice.
func TestReviewCheckpointEquivalentToFlags(t *testing.T) {
	const org = "acme factory"
	archive, cpPath, cp := fixtureChainAndCheckpoint(t, org)

	flagArgs := reviewArgs(archive, org, 2, cp.EndSeq, cp.Fingerprint)
	fileArgs := checkpointReviewArgs(archive, cpPath, 2)

	for _, tc := range []struct {
		name string
		json bool
	}{
		{"text", false},
		{"json", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := append([]string{}, flagArgs...)
			ua := append([]string{}, fileArgs...)
			if tc.json {
				fa = append(fa, "--json")
				ua = append(ua, "--json")
			}
			codeF, outF, errF := runDispatch(t, fa...)
			codeU, outU, errU := runDispatch(t, ua...)
			if codeF != 0 || codeU != 0 {
				t.Fatalf("exits: flags=%d file=%d; stderr flags=%q file=%q",
					codeF, codeU, errF, errU)
			}
			if errF != "" || errU != "" {
				t.Fatalf("neither form may chat on stderr, got flags=%q file=%q", errF, errU)
			}
			if outF != outU {
				t.Fatalf("file-mode output differs from flag mode\nflags:\n%s\nfile:\n%s", outF, outU)
			}
			if tc.json && !strings.Contains(outU, `"target_sequence": 2`) {
				t.Fatalf("expected the JSON object, got:\n%s", outU)
			}
			if !tc.json && !strings.Contains(outU, "consistent: yes") {
				t.Fatalf("expected the text report, got:\n%s", outU)
			}
		})
	}
}

// TestReviewCheckpointFieldOrderAndNewline proves lines may appear in any
// order and the final newline is optional: every accepted shape yields the
// same successful report.
func TestReviewCheckpointFieldOrderAndNewline(t *testing.T) {
	const org = "acme factory"
	archive, _, cp := fixtureChainAndCheckpoint(t, org)

	lines := map[string]string{
		"org":         fmt.Sprintf("org=%q", cp.Org),
		"end_seq":     fmt.Sprintf("end_seq=%d", cp.EndSeq),
		"fingerprint": "fingerprint=" + cp.Fingerprint,
	}
	orders := [][]string{
		{"org", "end_seq", "fingerprint"},
		{"org", "fingerprint", "end_seq"},
		{"end_seq", "org", "fingerprint"},
		{"end_seq", "fingerprint", "org"},
		{"fingerprint", "org", "end_seq"},
		{"fingerprint", "end_seq", "org"},
	}
	var want string
	{
		_, want, _ = runDispatch(t, reviewArgs(archive, org, 2, cp.EndSeq, cp.Fingerprint)...)
	}
	for oi, order := range orders {
		for _, trailing := range []bool{false, true} {
			doc := lines[order[0]] + "\n" + lines[order[1]] + "\n" + lines[order[2]]
			if trailing {
				doc += "\n"
			}
			cpPath := writeCheckpointText(t,
				fmt.Sprintf("order-%d-%v.checkpoint", oi, trailing), doc)
			code, out, errOut := runDispatch(t, checkpointReviewArgs(archive, cpPath, 2)...)
			if code != 0 || errOut != "" {
				t.Fatalf("order=%v trailing=%v: code=%d stderr=%q", order, trailing, code, errOut)
			}
			if out != want {
				t.Fatalf("order=%v trailing=%v changed the report:\n%s", order, trailing, out)
			}
		}
	}
}

// TestReviewCheckpointEndSeqZero covers the no-record organization
// checkpoint: end_seq=0 is a normal value, not a missing field. Such a
// checkpoint validates a recordless archive (the review then reports the
// absent target distinctly), and against a populated archive it is a
// material mismatch, not a format error.
func TestReviewCheckpointEndSeqZero(t *testing.T) {
	const org = "empty org"
	s := darksafe.NewStore()
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 || cp.EndSeq != 0 {
		t.Fatalf("expected a recordless genesis checkpoint, got %d records end=%d", len(recs), cp.EndSeq)
	}

	// Parser level: "end_seq=0" decodes to 0 with no error.
	parsed, perr := parseCheckpointDocument("genesis.checkpoint",
		[]byte(canonicalCheckpointText(cp)))
	if perr != nil {
		t.Fatalf("end_seq=0 must parse: %v", perr)
	}
	if parsed.EndSeq != 0 || parsed.Org != org || parsed.Fingerprint != cp.Fingerprint {
		t.Fatalf("parsed checkpoint mismatch: %+v", parsed)
	}

	arc, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "empty.audit")
	if err := os.WriteFile(archivePath, arc, 0o600); err != nil {
		t.Fatal(err)
	}
	cpPath := writeCheckpointText(t, "genesis.checkpoint", canonicalCheckpointText(cp))

	// The zero checkpoint VALIDATES the recordless archive; the failure must
	// be the absent target (ErrAuditNotFound), a material/target exit-1 —
	// never "archive validation failed", and never a usage exit-2.
	code, out, errOut := runDispatch(t, checkpointReviewArgs(archivePath, cpPath, 1)...)
	if code != 1 {
		t.Fatalf("absent target on recordless archive: exit=%d stderr=%q", code, errOut)
	}
	if out != "" {
		t.Fatalf("no partial output, got %q", out)
	}
	if strings.Contains(errOut, "archive validation failed") {
		t.Fatalf("end_seq=0 checkpoint must validate a recordless archive, got %q", errOut)
	}
	if !strings.Contains(errOut, "audit record not found") && !strings.Contains(errOut, "not found") {
		t.Fatalf("expected a not-found target error, got %q", errOut)
	}
}

// TestReviewCheckpointMutualExclusion verifies the structural conflict:
// --checkpoint alongside ANY of --org/--end-seq/--fingerprint is a usage
// error no matter which comes first and no matter whether the repeated value
// is correct; a duplicated --checkpoint is refused too.
func TestReviewCheckpointMutualExclusion(t *testing.T) {
	const org = "acme"
	archive, cpPath, cp := fixtureChainAndCheckpoint(t, org)

	conflicting := func(extra ...string) [][]string {
		base := checkpointReviewArgs(archive, cpPath, 2)
		front := append([]string{"review"}, extra...)
		front = append(front, base[1:]...)
		back := append([]string{}, base...)
		back = append(back, extra...)
		return [][]string{front, back}
	}

	groups := map[string][]string{
		"org":         {"--org", org},
		"end-seq":     {"--end-seq", strconv.Itoa(cp.EndSeq)},
		"fingerprint": {"--fingerprint", cp.Fingerprint},
		"all three":   {"--org", org, "--end-seq", strconv.Itoa(cp.EndSeq), "--fingerprint", cp.Fingerprint},
	}
	for name, extra := range groups {
		for order, args := range conflicting(extra...) {
			t.Run(fmt.Sprintf("%s/order%d", name, order), func(t *testing.T) {
				code, out, errOut := runDispatch(t, args...)
				if code != 2 {
					t.Fatalf("exit=%d, want 2; stdout=%q stderr=%q", code, out, errOut)
				}
				if out != "" {
					t.Fatalf("conflict must print nothing to stdout, got %q", out)
				}
				if !strings.Contains(errOut, "mutually exclusive") {
					t.Fatalf("conflict error must say so, got %q", errOut)
				}
			})
		}
	}

	// A bad --end-seq value next to --checkpoint is the conflict first:
	// presence alone is illegal, so the malformed integer neither parses nor
	// overrides anything.
	for _, args := range conflicting("--end-seq", "nope") {
		code, _, errOut := runDispatch(t, args...)
		if code != 2 || !strings.Contains(errOut, "mutually exclusive") {
			t.Fatalf("malformed --end-seq with checkpoint: code=%d stderr=%q", code, errOut)
		}
	}

	// --checkpoint given twice (even with the same path) is a usage error.
	twice := append(checkpointReviewArgs(archive, cpPath, 2), "--checkpoint", cpPath)
	if code, out, errOut := runDispatch(t, twice...); code != 2 || out != "" ||
		!strings.Contains(errOut, "checkpoint") {
		t.Fatalf("duplicate --checkpoint: code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// In file mode --archive and --seq remain mandatory; the other three
	// must not be demanded.
	for _, args := range [][]string{
		{"review", "--seq", "2", "--checkpoint", cpPath},
		{"review", "--archive", archive, "--checkpoint", cpPath},
		{"review", "--archive", archive, "--seq", "2", "--checkpoint", ""},
	} {
		code, out, errOut := runDispatch(t, args...)
		if code != 2 || out != "" || errOut == "" {
			t.Fatalf("file-mode missing input: args=%v code=%d stdout=%q stderr=%q",
				args, code, out, errOut)
		}
	}
}

// TestReviewCheckpointFileFormFailuresExitTwo enumerates the exit-2 family:
// the file cannot be read, a field is missing/repeated, extra or empty
// content appears, the org quoting is invalid, the end sequence is invalid
// or unrepresentable, or the fingerprint has the wrong shape. Every case
// leaves stdout empty and names the file plus the offending item.
func TestReviewCheckpointFileFormFailuresExitTwo(t *testing.T) {
	const org = "acme"
	archive, cpPath, cp := fixtureChainAndCheckpoint(t, org)
	_ = cpPath
	good := canonicalCheckpointText(cp)

	documentCases := []struct {
		name    string
		content string
		want    string // stderr must contain this fragment
	}{
		{"empty file", "", "checkpoint"},
		{"only a newline", "\n", "checkpoint"},
		{"two trailing newlines", good + "\n", "line 4"},
		{"missing org", fmt.Sprintf("end_seq=%d\nfingerprint=%s\n", cp.EndSeq, cp.Fingerprint), "missing field org"},
		{"missing end_seq", fmt.Sprintf("org=%q\nfingerprint=%s\n", cp.Org, cp.Fingerprint), "missing field end_seq"},
		{"missing fingerprint", fmt.Sprintf("org=%q\nend_seq=%d\n", cp.Org, cp.EndSeq), "missing field fingerprint"},
		{"duplicate org", good + fmt.Sprintf("org=%q\n", cp.Org), "duplicate field org"},
		{"duplicate end_seq", fmt.Sprintf("org=%q\nend_seq=%d\nend_seq=%d\nfingerprint=%s\n",
			cp.Org, cp.EndSeq, cp.EndSeq, cp.Fingerprint), "duplicate field end_seq"},
		{"duplicate fingerprint", good + "fingerprint=" + cp.Fingerprint + "\n", "duplicate field fingerprint"},
		{"unknown field", good + "foo=bar\n", `unknown field "foo"`},
		{"blank middle line", fmt.Sprintf("org=%q\n\nend_seq=%d\nfingerprint=%s\n",
			cp.Org, cp.EndSeq, cp.Fingerprint), "line 2"},
		{"line without equals", fmt.Sprintf("org=%q\nendseq2\nfingerprint=%s\n",
			cp.Org, cp.Fingerprint), "line 2"},
		{"empty key", fmt.Sprintf("=x\nend_seq=%d\nfingerprint=%s\n",
			cp.EndSeq, cp.Fingerprint), "line 1"},
		{"empty org", "org=\"\"\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "org must not be empty"},
		{"unquoted org", "org=acme\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "double-quoted"},
		{"single quoted org", "org='acme'\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "double-quoted"},
		{"unterminated quote", "org=\"acme\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "double-quoted"},
		{"bad escape", "org=\"ac\\me\"\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "quoted org"},
		{"raw non-utf8 byte", "org=\"acme\xff\"\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "non-UTF-8"},
		{"raw control byte", "org=\"ac\tme\"\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "control character"},
		{"raw newline byte", "org=\"ac\nme\"\nend_seq=2\nfingerprint=" + cp.Fingerprint + "\n", "line"},
		{"end_seq empty", "org=\"acme\"\nend_seq=\nfingerprint=" + cp.Fingerprint + "\n", "end_seq"},
		{"end_seq negative", "org=\"acme\"\nend_seq=-2\nfingerprint=" + cp.Fingerprint + "\n", "end_seq"},
		{"end_seq plus sign", "org=\"acme\"\nend_seq=+2\nfingerprint=" + cp.Fingerprint + "\n", "end_seq"},
		{"end_seq decimal", "org=\"acme\"\nend_seq=2.0\nfingerprint=" + cp.Fingerprint + "\n", "end_seq"},
		{"end_seq hex", "org=\"acme\"\nend_seq=0x2\nfingerprint=" + cp.Fingerprint + "\n", "end_seq"},
		{"end_seq spaced", "org=\"acme\"\nend_seq= 2\nfingerprint=" + cp.Fingerprint + "\n", "end_seq"},
		{"end_seq overflow", "org=\"acme\"\nend_seq=999999999999999999999999\nfingerprint=" + cp.Fingerprint + "\n", "range"},
		{"fingerprint empty", "org=\"acme\"\nend_seq=2\nfingerprint=\n", "fingerprint"},
		{"fingerprint too short", "org=\"acme\"\nend_seq=2\nfingerprint=abcd\n", "fingerprint"},
		{"fingerprint 63", "org=\"acme\"\nend_seq=2\nfingerprint=" + cp.Fingerprint[:63] + "\n", "fingerprint"},
		{"fingerprint 65", "org=\"acme\"\nend_seq=2\nfingerprint=" + cp.Fingerprint + "0\n", "fingerprint"},
		{"fingerprint non-hex", "org=\"acme\"\nend_seq=2\nfingerprint=" + strings.Repeat("g", 64) + "\n", "fingerprint"},
	}

	for _, tc := range documentCases {
		t.Run(tc.name, func(t *testing.T) {
			cpPath := writeCheckpointText(t, "bad.checkpoint", tc.content)
			code, out, errOut := runDispatch(t, checkpointReviewArgs(archive, cpPath, 2)...)
			if code != 2 {
				t.Fatalf("exit=%d want 2; stdout=%q stderr=%q", code, out, errOut)
			}
			if out != "" {
				t.Fatalf("form failure must leave stdout empty, got %q", out)
			}
			if !strings.Contains(errOut, "bad.checkpoint") {
				t.Fatalf("stderr must name the offending file, got %q", errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Fatalf("stderr must mention %q, got %q", tc.want, errOut)
			}
		})
	}

	// Unreadable paths are the same exit-2 family, reported against the file.
	missing := filepath.Join(t.TempDir(), "never-existed.checkpoint")
	for _, tc := range []struct {
		name string
		path string
	}{
		{"missing file", missing},
		{"path is a directory", t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, checkpointReviewArgs(archive, tc.path, 2)...)
			if code != 2 || out != "" || !strings.Contains(errOut, "cannot read checkpoint") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

// TestReviewCheckpointMismatchIsMaterialFailureExitOne proves a
// syntactically valid checkpoint file whose content does not match the
// archive fails exactly like a wrong --fingerprint flag: exit 1, empty
// stdout, archive-validation error — never an exit-2 format error and never
// a silent switch to the archive's embedded checkpoint.
func TestReviewCheckpointMismatchIsMaterialFailureExitOne(t *testing.T) {
	const org = "acme"
	archive, _, cp := fixtureChainAndCheckpoint(t, org)

	cases := []struct {
		name string
		text string
	}{
		{"wrong org", canonicalCheckpointText(darksafe.Checkpoint{Org: "globex", EndSeq: cp.EndSeq, Fingerprint: cp.Fingerprint})},
		{"wrong end_seq", canonicalCheckpointText(darksafe.Checkpoint{Org: org, EndSeq: cp.EndSeq - 1, Fingerprint: cp.Fingerprint})},
		{"wrong fingerprint", canonicalCheckpointText(darksafe.Checkpoint{Org: org, EndSeq: cp.EndSeq, Fingerprint: strings.Repeat("0", 64)})},
		{"uppercase fingerprint", canonicalCheckpointText(darksafe.Checkpoint{Org: org, EndSeq: cp.EndSeq, Fingerprint: strings.ToUpper(cp.Fingerprint)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cpPath := writeCheckpointText(t, "mismatch.checkpoint", tc.text)
			code, out, errOut := runDispatch(t, checkpointReviewArgs(archive, cpPath, 2)...)
			if code != 1 {
				t.Fatalf("exit=%d want 1; stdout=%q stderr=%q", code, out, errOut)
			}
			if out != "" {
				t.Fatalf("material failure must print no partial review, got %q", out)
			}
			if !strings.Contains(errOut, "archive validation failed") {
				t.Fatalf("must be reported as archive validation failure, got %q", errOut)
			}
		})
	}
}

// TestReviewCheckpointValidatesWholeArchiveAndVersion keeps, for file mode,
// the existing guarantees: a tampered record AFTER the target fails the
// whole review, and the replay uses the decision's recorded version.
func TestReviewCheckpointValidatesWholeArchiveAndVersion(t *testing.T) {
	const org = "acme"
	s := darksafe.NewStore()
	s.Publish(org, 0, []darksafe.Policy{
		{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
	})
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
		Action:   "read",
	})
	s.Publish(org, 1, nil)
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "late-subject"},
		Resource: darksafe.Resource{ID: "r9", Scope: "org/a"},
		Action:   "read",
	})
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	cpPath := writeCheckpointText(t, "retained.checkpoint", canonicalCheckpointText(cp))

	// The untouched archive reviews the early decision against v1 via the
	// checkpoint file.
	goodPath := filepath.Join(t.TempDir(), "good.bin")
	if err := os.WriteFile(goodPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runDispatch(t, checkpointReviewArgs(goodPath, cpPath, 2)...); code != 0 {
		t.Fatalf("early review via file: %d %q", code, errOut)
	} else if !strings.Contains(out, "policy version: 1") {
		t.Fatalf("file-mode review must replay v1:\n%s", out)
	}

	// Tamper only a LATER record, reframed with a valid checksum.
	tampered := reframeArchive(t, good, func(payload []byte) {
		idx := bytes.Index(payload, []byte("late-subject"))
		if idx < 0 {
			t.Fatal("later record subject not located")
		}
		payload[idx] ^= 0x01
	})
	badPath := filepath.Join(t.TempDir(), "tampered.bin")
	if err := os.WriteFile(badPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runDispatch(t, checkpointReviewArgs(badPath, cpPath, 2)...)
	if code != 1 {
		t.Fatalf("later-record tamper exit=%d, want 1; %q", code, errOut)
	}
	if !strings.Contains(errOut, "invalid audit range") || out != "" {
		t.Fatalf("whole-archive validation expected, stdout=%q stderr=%q", out, errOut)
	}
}

// TestReviewCheckpointOrgByteFidelity is the losslessness guarantee for the
// quoted organization: spaces, Chinese, escaped control characters and
// escaped non-UTF-8 bytes all round-trip to the SAME bytes the archive was
// built from, and distinct illegal bytes never collapse onto one character.
func TestReviewCheckpointOrgByteFidelity(t *testing.T) {
	// 0xFF and 0xFE in the organization name, plus Chinese, an embedded tab
	// and leading/trailing spaces — every tricky byte at once.
	org := " 安\\acme\xff\xfe\t "
	s := darksafe.NewStore()
	s.Publish(org, 0, []darksafe.Policy{
		{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
	})
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
		Action:   "read",
	})
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	arc, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "raw.bin")
	if err := os.WriteFile(archivePath, arc, 0o600); err != nil {
		t.Fatal(err)
	}

	// The file is exactly what the example's fmt.Sprintf("org=%q", ...)
	// writes: printable UTF-8 (including Chinese) stays readable, while
	// control and non-UTF-8 bytes appear only as escapes — so the document's
	// structure is always plain printable text.
	text := canonicalCheckpointText(cp)
	if !utf8.ValidString(text) {
		t.Fatalf("checkpoint document must be valid UTF-8 with non-UTF-8 org bytes escaped:\n%q", text)
	}
	// Check the org line in isolation: the 0x0A bytes separating fields are
	// structural line delimiters, not content.
	orgLine := text[:strings.IndexByte(text, '\n')]
	for i, b := range []byte(orgLine) {
		if b < 0x20 || b == 0x7f {
			t.Fatalf("control byte must be escaped on the org line, got raw %#x at %d", b, i)
		}
	}
	if !bytes.Contains([]byte(text), []byte(`\xff`)) ||
		!bytes.Contains([]byte(text), []byte(`\xfe`)) ||
		!bytes.Contains([]byte(text), []byte(`\t`)) {
		t.Fatalf("control/non-UTF-8 org bytes must be escaped in the document:\n%s", text)
	}
	cpPath := writeCheckpointText(t, "raw.checkpoint", text)

	// Parser level: the organization's exact bytes come back, 0xFF and 0xFE
	// staying distinct from each other and from a genuine U+FFFD.
	parsed, perr := parseCheckpointDocument("raw.checkpoint", []byte(text))
	if perr != nil {
		t.Fatal(perr)
	}
	if parsed.Org != org {
		t.Fatalf("org bytes changed in round-trip:\nwant % x\n got % x", org, parsed.Org)
	}
	for _, tc := range []struct {
		quoted string
		want   byte
	}{
		{`"a\xff"`, 0xff},
		{`"a\xfe"`, 0xfe},
		{`"a\xff\xfe"`, 0xff},
	} {
		doc := "org=" + tc.quoted + "\nend_seq=0\nfingerprint=" + strings.Repeat("a", 64) + "\n"
		p, err := parseCheckpointDocument("x.checkpoint", []byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", tc.quoted, err)
		}
		if p.Org[1] != tc.want {
			t.Fatalf("%s decoded byte %#x, want %#x", tc.quoted, p.Org[1], tc.want)
		}
	}
	ff, _ := parseCheckpointDocument("a", []byte(`org="a\xff"`+"\nend_seq=0\nfingerprint="+strings.Repeat("a", 64)))
	fe, _ := parseCheckpointDocument("a", []byte(`org="a\xfe"`+"\nend_seq=0\nfingerprint="+strings.Repeat("a", 64)))
	uffd, _ := parseCheckpointDocument("a", []byte("org=\"a"+"\xef\xbf\xbd"+"\"\nend_seq=0\nfingerprint="+strings.Repeat("a", 64)))
	if ff.Org == fe.Org || ff.Org == uffd.Org || fe.Org == uffd.Org {
		t.Fatalf("distinct bytes collapsed: ff=% x fe=% x ufffd=% x", ff.Org, fe.Org, uffd.Org)
	}

	// Command level: file-mode review of the raw-byte organization succeeds
	// and is identical to passing the raw bytes as flags.
	code, outFile, errOut := runDispatch(t, checkpointReviewArgs(archivePath, cpPath, 2)...)
	if code != 0 {
		t.Fatalf("raw-byte org review via file: %d %q", code, errOut)
	}
	_, outFlags, errFlags := runDispatch(t, reviewArgs(archivePath, org, 2, cp.EndSeq, cp.Fingerprint)...)
	if errFlags != "" {
		t.Fatal(errFlags)
	}
	if outFile != outFlags {
		t.Fatalf("raw-byte org: file and flag output differ\nfile:\n%s\nflags:\n%s", outFile, outFlags)
	}
}

// TestReviewCheckpointIntBoundaries pins end_seq parsing at the int boundary
// on the target architecture and rejects anything beyond it.
func TestReviewCheckpointIntBoundaries(t *testing.T) {
	doc := func(n string) []byte {
		return []byte("org=\"acme\"\nend_seq=" + n + "\nfingerprint=" + strings.Repeat("a", 64) + "\n")
	}
	cp, err := parseCheckpointDocument("b.checkpoint", doc(strconv.Itoa(math.MaxInt64)))
	if err != nil {
		t.Fatalf("MaxInt64 must parse on a 64-bit int target: %v", err)
	}
	if int64(cp.EndSeq) != math.MaxInt64 {
		t.Fatalf("end_seq = %d", cp.EndSeq)
	}
	if _, err := parseCheckpointDocument("b.checkpoint", doc("9223372036854775808")); err == nil {
		t.Fatal("MaxInt64+1 must be rejected")
	}
}

// TestReviewCheckpointIsReadOnly ensures a review rewrites neither the
// archive nor the checkpoint file.
func TestReviewCheckpointIsReadOnly(t *testing.T) {
	const org = "acme"
	archive, cpPath, _ := fixtureChainAndCheckpoint(t, org)
	beforeArchive, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	beforeCP, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runDispatch(t, checkpointReviewArgs(archive, cpPath, 2)...); code != 0 {
		t.Fatalf("review: %s", errOut)
	}
	afterArchive, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	afterCP, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeArchive, afterArchive) {
		t.Fatal("review rewrote the archive")
	}
	if !bytes.Equal(beforeCP, afterCP) {
		t.Fatal("review rewrote the checkpoint file")
	}
}
