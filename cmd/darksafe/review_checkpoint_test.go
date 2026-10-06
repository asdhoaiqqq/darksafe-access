// End-to-end coverage for `darksafe review --checkpoint FILE`: reading the
// separately retained checkpoint file instead of spelling --org/
// --end-seq/--fingerprint out. These tests drive the real dispatch and
// pin, together:
//
//   - file input produces the byte-identical text and JSON reports the
//     three flags do, for the same retained checkpoint;
//   - --checkpoint is mutually exclusive with all three flags purely by
//     presence (identical values and any order still exit 2);
//   - an unreadable or malformed checkpoint file exits 2 with empty
//     stdout, while a readable, syntactically valid file whose checkpoint
//     contradicts the archive exits 1 with no partial review and never
//     falls back to the archive's embedded checkpoint;
//   - the file is read only, the whole archive is still validated exactly
//     once against the file's checkpoint, and a sequence-0 checkpoint of
//     an organization with no records validates before its absent target
//     fails as the ordinary target-missing case (exit 1).
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// writeCheckpointFile writes cp in the exact plain-text form the offline
// review example retains independently of the archive.
func writeCheckpointFile(t *testing.T, cp darksafe.Checkpoint) string {
	t.Helper()
	content := fmt.Sprintf("org=%q\nend_seq=%d\nfingerprint=%s\n", cp.Org, cp.EndSeq, cp.Fingerprint)
	path := filepath.Join(t.TempDir(), "retained.checkpoint")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkpointReviewArgs builds the file-based review argument vector.
func checkpointReviewArgs(archive, checkpoint string, seq int, extra ...string) []string {
	args := []string{"review", "--archive", archive, "--seq", strconv.Itoa(seq),
		"--checkpoint", checkpoint}
	return append(args, extra...)
}

// TestReviewCheckpointFileParityWithFlags is the core equivalence: the
// same archive and retained checkpoint reviewed from the file and from
// the three flags must give byte-identical reports, in both formats,
// including when the organization name carries Chinese and surrounding
// spaces that the file must restore byte for byte.
func TestReviewCheckpointFileParityWithFlags(t *testing.T) {
	const org = " 阿克米 acme " // leading/trailing spaces and Chinese
	path, cp := fixtureChain(t, org)
	cpPath := writeCheckpointFile(t, cp)

	for name, extra := range map[string][]string{
		"text": nil,
		"json": {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			codeFile, outFile, errFile := runDispatch(t, checkpointReviewArgs(path, cpPath, 2, extra...)...)
			if codeFile != 0 || errFile != "" {
				t.Fatalf("file review: code=%d stderr=%q", codeFile, errFile)
			}
			codeFlags, outFlags, errFlags := runDispatch(t,
				append(reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint), extra...)...)
			if codeFlags != 0 || errFlags != "" {
				t.Fatalf("flag review: code=%d stderr=%q", codeFlags, errFlags)
			}
			if outFile != outFlags {
				t.Fatalf("file and flag reports differ:\n--- file:\n%s\n--- flags:\n%s", outFile, outFlags)
			}
		})
	}
}

// TestReviewCheckpointFileNonUTF8Org proves the quoted org round trips raw
// bytes through the file on the real command path: each of a lone 0xFF, a
// lone 0xFE and a genuine U+FFFD makes a distinct organization whose chain
// validates only against its own file. The report itself never prints the
// organization, so distinguishability is asserted the way the command
// experiences it — self file succeeds, every foreign file fails material
// validation (exit 1) — rather than by report text.
func TestReviewCheckpointFileNonUTF8Org(t *testing.T) {
	type material struct {
		archive string
		cpFile  string
	}
	mats := map[string]material{}
	for name, org := range map[string]string{
		"ff":   "org\xff factory",
		"fe":   "org\xfe factory",
		"uffd": "org" + string(rune(0xFFFD)) + " factory",
	} {
		path, cp := fixtureChain(t, org)
		cpFile := writeCheckpointFile(t, cp)
		if code, _, errOut := runDispatch(t, checkpointReviewArgs(path, cpFile, 2)...); code != 0 {
			t.Fatalf("%s: own checkpoint file must validate, code=%d stderr=%q", name, code, errOut)
		}
		mats[name] = material{archive: path, cpFile: cpFile}
	}

	// Every archive paired with a different member's retained checkpoint
	// file must fail material validation, empty stdout, exit 1. A normalizer
	// that collapsed the invalid bytes together would wrongly accept these.
	for _, owner := range []string{"ff", "fe", "uffd"} {
		for _, foreign := range []string{"ff", "fe", "uffd"} {
			if owner == foreign {
				continue
			}
			code, out, errOut := runDispatch(t,
				checkpointReviewArgs(mats[owner].archive, mats[foreign].cpFile, 2)...)
			if code != 1 || out != "" {
				t.Fatalf("archive %s against checkpoint %s: code=%d stdout=%q stderr=%q, want 1 with empty stdout",
					owner, foreign, code, out, errOut)
			}
			if !strings.Contains(errOut, "archive validation failed") {
				t.Fatalf("cross-org file mismatch must be a material failure: %q", errOut)
			}
		}
	}
}

// TestReviewCheckpointMutexExitsTwo covers the presence-based mutual
// exclusion: every one of --org/--end-seq/--fingerprint together with
// --checkpoint exits 2 even when the value equals the file's content, in
// either flag order, in both --flag value and --flag=value forms. Nothing
// is read or printed before the conflict is reported.
func TestReviewCheckpointMutexExitsTwo(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	cpPath := writeCheckpointFile(t, cp)
	base := checkpointReviewArgs(path, cpPath, 2)

	cases := map[string][]string{
		"org same value after":  append(append([]string{}, base...), "--org", "acme"),
		"org same value before": {"review", "--org", "acme", "--archive", path, "--seq", "2", "--checkpoint", cpPath},
		"org equals form":       append(append([]string{}, base...), "--org=acme"),
		"org even empty":        append(append([]string{}, base...), "--org="),
		"end seq same value":    append(append([]string{}, base...), "--end-seq", strconv.Itoa(cp.EndSeq)),
		"end seq equals form":   append(append([]string{}, base...), "--end-seq="+strconv.Itoa(cp.EndSeq)),
		"fingerprint same":      append(append([]string{}, base...), "--fingerprint", cp.Fingerprint),
		"all three":             append(append([]string{}, base...), "--org", "acme", "--end-seq", "1", "--fingerprint", "00"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", code, out, errOut)
			}
			if out != "" {
				t.Fatalf("conflict must print nothing to stdout, got %q", out)
			}
			if !strings.Contains(errOut, "--checkpoint cannot be combined") ||
				!strings.Contains(errOut, "--") {
				t.Fatalf("stderr must name the conflict, got %q", errOut)
			}
		})
	}

	// The parser rejects the conflict directly too, regardless of order.
	_, _, perr := parseReviewArgs([]string{
		"review", "--checkpoint", cpPath, "--org", "acme",
		"--archive", path, "--seq", "2",
	})
	if perr == nil || !strings.Contains(perr.Error(), "--checkpoint") {
		t.Fatalf("parse must reject the mixture, err=%v", perr)
	}
}

// TestReviewCheckpointWithoutFileKeepsFlagRequirements ensures calls not
// using the new flag keep their original behavior: all three fields are
// still required and the file flag is not involved.
func TestReviewCheckpointWithoutFileKeepsFlagRequirements(t *testing.T) {
	path, cp := fixtureChain(t, "acme")

	// Missing one of the trio without --checkpoint still exits 2, naming
	// the flag (not the file).
	drop := func(drop string) []string {
		full := reviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint)
		out := make([]string, 0, len(full)-2)
		for i := 0; i < len(full); i++ {
			if full[i] == drop {
				i++
				continue
			}
			out = append(out, full[i])
		}
		return out
	}
	for _, flag := range []string{"--org", "--end-seq", "--fingerprint"} {
		code, out, errOut := runDispatch(t, drop(flag)...)
		if code != 2 || out != "" || !strings.Contains(errOut, flag) {
			t.Fatalf("missing %s: code=%d out=%q err=%q", flag, code, out, errOut)
		}
	}

	// --checkpoint with an empty value is treated as "not given": the three
	// flags then remain required, rather than silently reading no file.
	code, out, errOut := runDispatch(t,
		"review", "--archive", path, "--seq", "2", "--checkpoint=")
	if code != 2 || out != "" || !strings.Contains(errOut, "--org") {
		t.Fatalf("empty --checkpoint= must leave the flags required: code=%d out=%q err=%q",
			code, out, errOut)
	}
}

// TestReviewCheckpointFileReadFailuresExitTwo covers unreadable files: a
// missing path and a directory both exit 2 with empty stdout and the file
// named on stderr, before the archive is validated.
func TestReviewCheckpointFileReadFailuresExitTwo(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	_ = cp
	cases := map[string]string{
		"missing":   filepath.Join(t.TempDir(), "no-such.checkpoint"),
		"directory": t.TempDir(),
	}
	for name, cpFile := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, checkpointReviewArgs(path, cpFile, 2)...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr=%q", code, errOut)
			}
			if out != "" {
				t.Fatalf("read failure must leave stdout empty, got %q", out)
			}
			if !strings.Contains(errOut, "cannot read checkpoint") || !strings.Contains(errOut, cpFile) {
				t.Fatalf("stderr must name the checkpoint file, got %q", errOut)
			}
		})
	}
}

// TestReviewCheckpointMalformedFileExitsTwo enumerates malformed file
// contents over the real command: each exits 2, stdout stays empty, stderr
// names the file and the offending line/item, and the archive is never
// blamed for a bad checkpoint file.
func TestReviewCheckpointMalformedFileExitsTwo(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	_ = cp
	cases := map[string][]byte{
		"missing field":         []byte("org=\"acme\"\nend_seq=2\n"),
		"duplicate field":       []byte("org=\"acme\"\nend_seq=2\nend_seq=2\nfingerprint=" + strings.Repeat("a", 64) + "\n"),
		"extra field":           []byte("org=\"acme\"\nend_seq=2\nfingerprint=" + strings.Repeat("a", 64) + "\nextra=nope\n"),
		"empty org":             []byte("org=\"\"\nend_seq=2\nfingerprint=" + strings.Repeat("a", 64) + "\n"),
		"bad org quoting":       []byte("org=acme\nend_seq=2\nfingerprint=" + strings.Repeat("a", 64) + "\n"),
		"bad org escape":        []byte("org=\"a\\xzz\"\nend_seq=2\nfingerprint=" + strings.Repeat("a", 64) + "\n"),
		"negative end seq":      []byte("org=\"acme\"\nend_seq=-2\nfingerprint=" + strings.Repeat("a", 64) + "\n"),
		"huge end seq":          []byte("org=\"acme\"\nend_seq=" + strings.Repeat("9", 80) + "\nfingerprint=" + strings.Repeat("a", 64) + "\n"),
		"bad fingerprint":       []byte("org=\"acme\"\nend_seq=2\nfingerprint=not-hex\n"),
		"blank line":            []byte("\norg=\"acme\"\nend_seq=2\nfingerprint=" + strings.Repeat("a", 64) + "\n"),
		"garbage":               []byte("this is not a checkpoint file at all\n"),
		"two trailing newlines": []byte("org=\"acme\"\nend_seq=2\nfingerprint=" + strings.Repeat("a", 64) + "\n\n"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			cpFile := filepath.Join(t.TempDir(), "bad.checkpoint")
			if err := os.WriteFile(cpFile, content, 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := runDispatch(t, checkpointReviewArgs(path, cpFile, 2)...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", code, out, errOut)
			}
			if out != "" {
				t.Fatalf("malformed file must deliver no partial review, got %q", out)
			}
			if !strings.Contains(errOut, "invalid checkpoint file") || !strings.Contains(errOut, cpFile) {
				t.Fatalf("stderr must identify the bad checkpoint file, got %q", errOut)
			}
			if strings.Contains(errOut, "archive validation") {
				t.Fatalf("a malformed checkpoint is a usage error, not archive damage: %q", errOut)
			}
		})
	}
}

// TestReviewCheckpointMismatchExitsOne covers readable, syntactically valid
// checkpoint files whose organization or checkpoint contradicts the
// archive. These are material validation failures (exit 1): stdout is
// empty, no partial review is printed, and the archive's embedded
// checkpoint is never substituted.
func TestReviewCheckpointMismatchExitsOne(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	dir := filepath.Dir(path)

	write := func(name string, cp darksafe.Checkpoint) string {
		p := filepath.Join(dir, name)
		content := fmt.Sprintf("org=%q\nend_seq=%d\nfingerprint=%s\n", cp.Org, cp.EndSeq, cp.Fingerprint)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := map[string]string{
		"wrong org":         write("wrong-org.checkpoint", darksafe.Checkpoint{Org: "globex", EndSeq: cp.EndSeq, Fingerprint: cp.Fingerprint}),
		"wrong fingerprint": write("wrong-fp.checkpoint", darksafe.Checkpoint{Org: cp.Org, EndSeq: cp.EndSeq, Fingerprint: strings.Repeat("0", 64)}),
		"wrong end seq":     write("wrong-end.checkpoint", darksafe.Checkpoint{Org: cp.Org, EndSeq: cp.EndSeq - 1, Fingerprint: cp.Fingerprint}),
	}
	for name, cpFile := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, checkpointReviewArgs(path, cpFile, 2)...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%q", code, errOut)
			}
			if out != "" {
				t.Fatalf("material mismatch must print no partial review, got %q", out)
			}
			if !strings.Contains(errOut, "archive validation failed") {
				t.Fatalf("stderr must report material validation failure, got %q", errOut)
			}
		})
	}
}

// TestReviewCheckpointGenesisZeroRecords verifies end_seq=0 still means a
// checkpoint of an organization with no records: the file parses, the
// empty archive validates against it (exit would be 0 for validation),
// and only the absent target fails, as the ordinary target-missing case
// (exit 1) rather than a file-format error.
func TestReviewCheckpointGenesisZeroRecords(t *testing.T) {
	const org = "brand new org"
	s := darksafe.NewStore()
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cp.EndSeq != 0 {
		t.Fatalf("new organization checkpoint must end at 0, got %d", cp.EndSeq)
	}
	archive, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	arcPath := filepath.Join(t.TempDir(), "empty.audit")
	if err := os.WriteFile(arcPath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	cpPath := writeCheckpointFile(t, cp)

	code, out, errOut := runDispatch(t, checkpointReviewArgs(arcPath, cpPath, 1)...)
	if code != 1 {
		t.Fatalf("absent target in an empty, valid chain must exit 1, got %d: %q", code, errOut)
	}
	if out != "" {
		t.Fatalf("no partial review allowed, got %q", out)
	}
	if strings.Contains(errOut, "checkpoint") && strings.Contains(errOut, "invalid checkpoint") {
		t.Fatalf("end_seq=0 is a valid checkpoint, got %q", errOut)
	}
}

// TestReviewCheckpointReadsOnly ensures neither the archive nor the
// checkpoint file is modified by a file-based review, text or JSON.
func TestReviewCheckpointReadsOnly(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	cpPath := writeCheckpointFile(t, cp)
	arcBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cpBefore, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		checkpointReviewArgs(path, cpPath, 2),
		checkpointReviewArgs(path, cpPath, 2, "--json"),
	} {
		if code, _, errOut := runDispatch(t, args...); code != 0 {
			t.Fatalf("review: %d %s", code, errOut)
		}
		arcAfter, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cpAfter, err := os.ReadFile(cpPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(arcBefore, arcAfter) {
			t.Fatal("review modified the archive file")
		}
		if !bytes.Equal(cpBefore, cpAfter) {
			t.Fatal("review modified the checkpoint file")
		}
	}
}

// TestReviewCheckpointValidatesChainOnce pins one complete chain
// validation per file-based invocation, in both formats.
func TestReviewCheckpointValidatesChainOnce(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	cpPath := writeCheckpointFile(t, cp)
	for name, extra := range map[string][]string{"text": nil, "json": {"--json"}} {
		t.Run(name, func(t *testing.T) {
			count, restore := countChainValidations(t)
			defer restore()
			code, out, errOut := runDispatch(t, checkpointReviewArgs(path, cpPath, 2, extra...)...)
			if code != 0 || out == "" {
				t.Fatalf("code=%d out=%q stderr=%q", code, out, errOut)
			}
			if got := count(); got != 1 {
				t.Fatalf("file-based review walked the chain %d times, want 1", got)
			}
		})
	}
}

// TestReviewCheckpointHelpDocumentsFlag checks the help explains the file
// input, the mutual exclusion and the two failure families.
func TestReviewCheckpointHelpDocumentsFlag(t *testing.T) {
	for _, args := range [][]string{
		{"review", "--help"},
		{"review", "--checkpoint", "x", "--help"},
	} {
		code, out, errOut := runDispatch(t, args...)
		if code != 0 || errOut != "" {
			t.Fatalf("help: code=%d stderr=%q", code, errOut)
		}
		for _, want := range []string{
			"--checkpoint FILE",
			"mutually exclusive",
			"--org", "--end-seq", "--fingerprint",
			"malformed checkpoint",
			"does not match the archive",
			"acme-factory.checkpoint",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("help missing %q:\n%s", want, out)
			}
		}
	}
}
