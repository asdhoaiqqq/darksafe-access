package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// fixtureChain builds: publish v1 with one allow policy (seq 1), an allowed
// decision (seq 2), publish v2 with no policies (seq 3), a default-deny
// decision (seq 4). It archives the full export and returns the file path
// and the separately retained checkpoint.
func fixtureChain(t *testing.T, org string) (string, darksafe.Checkpoint) {
	t.Helper()
	s := darksafe.NewStore()
	s.Publish(org, 0, []darksafe.Policy{
		{ID: "p1 ledger", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
	})
	s.Decide(org, darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
		Action:   "read",
	})
	if v, err := s.Publish(org, 1, nil); err != nil || v != 2 {
		t.Fatalf("publish v2: %d %v", v, err)
	}
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
	archive, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "audit.bin")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cp
}

// runDispatch invokes the full top-level dispatch and captures everything.
func runDispatch(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := dispatch(append([]string{"darksafe"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func reviewArgs(path, org string, seq, endSeq int, fp string) []string {
	return []string{
		"review",
		"--archive", path,
		"--org", org,
		"--seq", strconv.Itoa(seq),
		"--end-seq", strconv.Itoa(endSeq),
		"--fingerprint", fp,
	}
}

// TestReviewSuccessPrintsBothDecisions verifies the happy path: both the
// original and recomputed decisions are shown with allowance, reason,
// matched policies and version, plus an explicit consistency verdict.
func TestReviewSuccessPrintsBothDecisions(t *testing.T) {
	const org = "acme payments" // spaces in the organization name
	path, cp := fixtureChain(t, org)

	code, out, errOut := runDispatch(t, reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if errOut != "" {
		t.Fatalf("unexpected stderr: %q", errOut)
	}
	for _, want := range []string{
		"target sequence: 2",
		"original decision:",
		"recomputed decision:",
		"allowed: true",
		`reason: "matched allow policy"`,
		`matched policies: ["p1 ledger"]`,
		"policy version: 1",
		"consistent: yes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// TestReviewUsesRecordedVersionNotNewer proves a later publish cannot change
// the recomputed decision: seq 2 used v1 even though v2 (no policies) is the
// current version at export time; seq 4 used v2 and denies by default.
func TestReviewUsesRecordedVersionNotNewer(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)

	code, early, _ := runDispatch(t, reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...)
	if code != 0 {
		t.Fatalf("seq 2 exit = %d", code)
	}
	if !strings.Contains(early, "allowed: true") || !strings.Contains(early, "policy version: 1") {
		t.Fatalf("early decision must replay v1 allow:\n%s", early)
	}

	code, late, _ := runDispatch(t, reviewArgs(path, org, 4, cp.EndSeq, cp.Fingerprint)...)
	if code != 0 {
		t.Fatalf("seq 4 exit = %d", code)
	}
	if !strings.Contains(late, "allowed: false") ||
		!strings.Contains(late, `reason: "no matching allow policy"`) ||
		!strings.Contains(late, "policy version: 2") {
		t.Fatalf("late decision must replay v2 default deny:\n%s", late)
	}
}

// TestReviewInconsistencyRendersCompletely covers the rendering of a
// legitimate-but-inconsistent result: both decisions are printed in full,
// the disagreement is explicit, and it is still a successful exit.
func TestReviewInconsistencyRendersCompletely(t *testing.T) {
	var stdout bytes.Buffer
	printReview(&stdout, darksafe.OfflineDecisionReview{
		Seq: 7,
		Original: darksafe.Decision{
			Allowed: true, Reason: "matched allow policy",
			Matched: []string{"p1"}, Version: 3,
		},
		Recomputed: darksafe.Decision{
			Allowed: false, Reason: "matched deny policy",
			Matched: []string{"p1", "p2"}, Version: 3,
		},
		Consistent: false,
	})
	out := stdout.String()
	for _, want := range []string{
		"target sequence: 7",
		"original decision:",
		"recomputed decision:",
		"consistent: no",
		`reason: "matched allow policy"`,
		`reason: "matched deny policy"`,
		`matched policies: ["p1", "p2"]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inconsistent output missing %q:\n%s", want, out)
		}
	}
}

// TestReviewUsageFailuresExitTwo covers every exit-2 condition: missing
// required inputs, unparsable integers, non-positive target, negative end
// sequence, unknown/unexpected arguments, and an unreadable archive.
func TestReviewUsageFailuresExitTwo(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	base := reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)

	dropFlag := func(name string) []string {
		out := make([]string, 0, len(base)-2)
		for i := 0; i < len(base); i++ {
			if base[i] == name {
				i++ // also drop its value
				continue
			}
			out = append(out, base[i])
		}
		return out
	}

	// --flag=value with a non-integer value must fail exactly like the
	// separated form.
	eqBad := reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)
	for i := range eqBad {
		if eqBad[i] == "--seq" {
			eqBad[i] = "--seq=nope"
			eqBad = append(eqBad[:i+1], eqBad[i+2:]...)
			break
		}
	}

	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", []string{"review"}},
		{"missing archive", dropFlag("--archive")},
		{"missing org", dropFlag("--org")},
		{"missing seq", dropFlag("--seq")},
		{"missing end-seq", dropFlag("--end-seq")},
		{"missing fingerprint", dropFlag("--fingerprint")},
		{"non-integer seq", replaceArg(base, "--seq", "abc")},
		{"float seq", replaceArg(base, "--seq", "1.5")},
		{"non-integer end-seq", replaceArg(base, "--end-seq", "xx")},
		{"zero seq", replaceArg(base, "--seq", "0")},
		{"negative seq", replaceArg(base, "--seq", "-4")},
		{"negative end-seq", replaceArg(base, "--end-seq", "-1")},
		{"equals form bad int", eqBad},
		{"unknown flag", append(append([]string{}, base...), "--bogus")},
		{"positional argument", append(append([]string{}, base...), "leftover")},
		{"missing file", replaceArg(base, "--archive", filepath.Join(t.TempDir(), "nope.bin"))},
		{"archive is directory", replaceArg(base, "--archive", t.TempDir())},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", code, out, errOut)
			}
			if errOut == "" {
				t.Fatal("expected a specific reason on stderr")
			}
			if out != "" {
				t.Fatalf("usage failure must print nothing to stdout, got %q", out)
			}
		})
	}
}

func replaceArg(base []string, flag, value string) []string {
	out := append([]string{}, base...)
	for i := range out {
		if out[i] == flag {
			out[i+1] = value
			return out
		}
	}
	return out
}

// TestReviewMaterialAndTargetFailuresExitOne covers the exit-1 family:
// unrecognized bytes, checkpoint mismatch (the archive's own checkpoint
// cannot substitute), a target past the export, and a non-decision target.
// None of them may print partial decision content.
func TestReviewMaterialAndTargetFailuresExitOne(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	dir := filepath.Dir(path)

	garbage := filepath.Join(dir, "garbage.bin")
	if err := os.WriteFile(garbage, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Flip a byte in a valid file: checksum/framing fails and no records are
	// delivered.
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), good...)
	corrupt[len(corrupt)-40] ^= 0x01
	corruptPath := filepath.Join(dir, "corrupt.bin")
	if err := os.WriteFile(corruptPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"unrecognized archive", reviewArgs(garbage, org, 2, cp.EndSeq, cp.Fingerprint)},
		{"corrupted bytes", reviewArgs(corruptPath, org, 2, cp.EndSeq, cp.Fingerprint)},
		{"wrong fingerprint", reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint[:len(cp.Fingerprint)-1]+"0")},
		{"foreign organization", reviewArgs(path, "globex", 2, cp.EndSeq, cp.Fingerprint)},
		{"wrong end sequence", reviewArgs(path, org, 2, cp.EndSeq-1, cp.Fingerprint)},
		{"target past export", reviewArgs(path, org, 99, cp.EndSeq, cp.Fingerprint)},
		{"target is policy change", reviewArgs(path, org, 1, cp.EndSeq, cp.Fingerprint)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runDispatch(t, tc.args...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%q", code, errOut)
			}
			if errOut == "" {
				t.Fatal("expected a distinguishable failure reason on stderr")
			}
			if out != "" {
				t.Fatalf("failure must not print a partial decision, stdout=%q", out)
			}
		})
	}
}

// archiveMagicV1 mirrors the darksafe package's stable framing prefix; the
// format is explicitly versioned (…-v1), so the CLI test relies only on its
// published layout.
const archiveMagicV1 = "darksafe-audit-archive-v1\n"

// reframeArchive parses one archive frame, lets mutate alter the payload in
// place, and rebuilds a frame with a consistent declared length and
// checksum, so a test can tamper content without tripping framing/checksum
// checks and reach the library's whole-chain validation.
func reframeArchive(t *testing.T, data []byte, mutate func(payload []byte)) []byte {
	t.Helper()
	magicLen := len(archiveMagicV1)
	n := int(binary.BigEndian.Uint32(data[magicLen : magicLen+4]))
	payload := append([]byte(nil), data[magicLen+4:magicLen+4+n]...)
	mutate(payload)
	out := append([]byte(nil), archiveMagicV1...)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)))
	out = append(out, lb[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	return append(out, sum[:]...)
}

// TestReviewValidatesMaterialAfterTarget builds a chain whose early decision
// (seq 2) is the review target but whose LATER record (seq 4, a uniquely
// named subject) is tampered with a valid checksum. Decode parses it and the
// whole-chain check must reject it: even an early target validates every
// record, and no partial decision is printed.
func TestReviewValidatesMaterialAfterTarget(t *testing.T) {
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
	// The later decision uses a unique subject so its bytes are locatable and
	// cannot be confused with the early record under review.
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

	tampered := reframeArchive(t, good, func(payload []byte) {
		idx := bytes.Index(payload, []byte("late-subject"))
		if idx < 0 {
			t.Fatal("later record subject not located in payload")
		}
		payload[idx] ^= 0x01 // same length; only the later record's content changes
	})
	path := filepath.Join(t.TempDir(), "late-damage.bin")
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runDispatch(t, reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...)
	if code != 1 {
		t.Fatalf("later-record tamper exit = %d, want 1; %s", code, errOut)
	}
	if !strings.Contains(errOut, "invalid audit range") {
		t.Fatalf("tampering a later record must fail chain validation, got %q", errOut)
	}
	if out != "" {
		t.Fatalf("no partial output allowed, got %q", out)
	}
}

// TestReviewPreservesDistinctRawBytes ensures output quoting keeps a lone
// 0xFF, a lone 0xFE and a genuine U+FFFD in matched policy identifiers
// distinguishable rather than folding all invalid bytes onto one rune.
func TestReviewPreservesDistinctRawBytes(t *testing.T) {
	rawIDArchive := func(t *testing.T, id string) (string, darksafe.Checkpoint) {
		t.Helper()
		const org = "raw org"
		s := darksafe.NewStore()
		s.Publish(org, 0, []darksafe.Policy{
			{ID: id, Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
		})
		s.Decide(org, darksafe.OrgRequest{
			SubjectOrg: org, ResourceOrg: org,
			Subject:  darksafe.Subject{ID: "u1"},
			Resource: darksafe.Resource{ID: "r", Scope: "org/a"},
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
		p := filepath.Join(t.TempDir(), "raw.bin")
		if err := os.WriteFile(p, arc, 0o600); err != nil {
			t.Fatal(err)
		}
		return p, cp
	}

	outputs := map[string]string{}
	for name, id := range map[string]string{
		"ff":   "p\xff",
		"fe":   "p\xfe",
		"uffd": "p�",
	} {
		path, cp := rawIDArchive(t, id)
		code, out, errOut := runDispatch(t, reviewArgs(path, "raw org", 2, cp.EndSeq, cp.Fingerprint)...)
		if code != 0 {
			t.Fatalf("%s review failed: %s", name, errOut)
		}
		outputs[name] = out
	}
	if outputs["ff"] == outputs["fe"] || outputs["ff"] == outputs["uffd"] || outputs["fe"] == outputs["uffd"] {
		t.Fatalf("distinct raw bytes collapsed:\nff:  %q\nfe:  %q\nuffd:%q",
			outputs["ff"], outputs["fe"], outputs["uffd"])
	}
	if !strings.Contains(outputs["ff"], `"p\xff"`) {
		t.Fatalf("0xFF must render as quoted \\xff:\n%s", outputs["ff"])
	}
	if !strings.Contains(outputs["fe"], `"p\xfe"`) {
		t.Fatalf("0xFE must render as quoted \\xfe:\n%s", outputs["fe"])
	}
	if !strings.Contains(outputs["uffd"], `p�`) {
		t.Fatalf("a real U+FFFD must render as itself, not an escape:\n%s", outputs["uffd"])
	}
}

// syntheticEnvelope mirrors the library's internal fingerprint envelope for
// the all-valid-UTF-8 case: record fingerprints are the SHA-256 of this JSON
// shape (see audit.go's hashEnvelope), and the genesis root is
// SHA-256("darksafe-audit-genesis\x00"+org). Field order and tags are
// load-bearing because the hash is order-sensitive; EncodeAuditArchive below
// independently re-verifies every fingerprint, so any drift fails the test
// loudly rather than silently testing the wrong thing.
type syntheticEnvelope struct {
	Org             string                   `json:"org"`
	Seq             int                      `json:"seq"`
	Kind            string                   `json:"kind"`
	Change          *darksafe.PolicyChange   `json:"change,omitempty"`
	Decision        *darksafe.DecisionRecord `json:"decision,omitempty"`
	PrevFingerprint string                   `json:"prev"`
}

func genesisFingerprintHex(org string) string {
	sum := sha256.Sum256([]byte("darksafe-audit-genesis\x00" + org))
	return hex.EncodeToString(sum[:])
}

func envelopeFingerprint(env syntheticEnvelope) string {
	b, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestReviewInconsistentButValidExitsZero proves the central exit-code rule:
// material that chains correctly but whose recorded decision contradicts the
// recorded policy is NOT archive damage. The command prints both decisions in
// full, marks the disagreement, and still exits 0.
func TestReviewInconsistentButValidExitsZero(t *testing.T) {
	const org = "acme"
	req := darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1"},
		Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
		Action:   "read",
	}
	rec1 := darksafe.AuditRecord{
		Org: org, Seq: 1, Kind: darksafe.AuditPolicyChange,
		Change: &darksafe.PolicyChange{
			Version: 1,
			Policies: []darksafe.Policy{
				{ID: "p1", Subject: "u1", Action: "read", Scope: "org/a", Effect: darksafe.EffectAllow},
			},
		},
		PrevFingerprint: genesisFingerprintHex(org),
	}
	rec1.Fingerprint = envelopeFingerprint(syntheticEnvelope{
		Org: rec1.Org, Seq: rec1.Seq, Kind: rec1.Kind,
		Change: rec1.Change, PrevFingerprint: rec1.PrevFingerprint,
	})
	// The recorded decision claims a deny the v1 policy cannot produce.
	rec2 := darksafe.AuditRecord{
		Org: org, Seq: 2, Kind: darksafe.AuditDecision,
		Decision: &darksafe.DecisionRecord{
			Request: req,
			Decision: darksafe.Decision{
				Allowed: false, Reason: "matched deny policy", Matched: []string{"p1"}, Version: 1,
			},
		},
		PrevFingerprint: rec1.Fingerprint,
	}
	rec2.Fingerprint = envelopeFingerprint(syntheticEnvelope{
		Org: rec2.Org, Seq: rec2.Seq, Kind: rec2.Kind,
		Decision: rec2.Decision, PrevFingerprint: rec2.PrevFingerprint,
	})
	records := []darksafe.AuditRecord{rec1, rec2}
	cp := darksafe.Checkpoint{Org: org, EndSeq: 2, Fingerprint: rec2.Fingerprint}

	arc, err := darksafe.EncodeAuditArchive(org, records, cp)
	if err != nil {
		t.Fatalf("synthetic chain must encode/verify: %v", err)
	}
	path := filepath.Join(t.TempDir(), "inconsistent.bin")
	if err := os.WriteFile(path, arc, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runDispatch(t, reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...)
	if code != 0 {
		t.Fatalf("a legitimate-but-inconsistent review must exit 0, got %d: %s", code, errOut)
	}
	for _, want := range []string{
		"target sequence: 2",
		"original decision:",
		"recomputed decision:",
		"consistent: no",
		// Original preserved verbatim.
		`reason: "matched deny policy"`,
		// Recomputed from the recorded request against v1: allow.
		`reason: "matched allow policy"`,
		"allowed: false",
		"allowed: true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inconsistent review output missing %q:\n%s", want, out)
		}
	}
}

// TestReviewMissingHistoricalVersionExitsOne builds a chain that is
// internally consistent but whose decision names version 5 although no
// policy change record in the material ever published it. Decode succeeds,
// targeting succeeds, but the historical version cannot be obtained: a
// distinguishable failure, exit 1, never a silent substitution with a newer
// version.
func TestReviewMissingHistoricalVersionExitsOne(t *testing.T) {
	const org = "acme"
	rec := darksafe.AuditRecord{
		Org: org, Seq: 1, Kind: darksafe.AuditDecision,
		Decision: &darksafe.DecisionRecord{
			Request: darksafe.OrgRequest{
				SubjectOrg: org, ResourceOrg: org,
				Subject:  darksafe.Subject{ID: "u1"},
				Resource: darksafe.Resource{ID: "r1", Scope: "org/a"},
				Action:   "read",
			},
			Decision: darksafe.Decision{
				Allowed: false, Reason: "no matching allow policy", Matched: nil, Version: 5,
			},
		},
		PrevFingerprint: genesisFingerprintHex(org),
	}
	rec.Fingerprint = envelopeFingerprint(syntheticEnvelope{
		Org: rec.Org, Seq: rec.Seq, Kind: rec.Kind,
		Decision: rec.Decision, PrevFingerprint: rec.PrevFingerprint,
	})
	records := []darksafe.AuditRecord{rec}
	cp := darksafe.Checkpoint{Org: org, EndSeq: 1, Fingerprint: rec.Fingerprint}
	arc, err := darksafe.EncodeAuditArchive(org, records, cp)
	if err != nil {
		t.Fatalf("chain must encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "missing-version.bin")
	if err := os.WriteFile(path, arc, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runDispatch(t, reviewArgs(path, org, 1, cp.EndSeq, cp.Fingerprint)...)
	if code != 1 {
		t.Fatalf("unavailable historical version must exit 1, got %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "version not found") {
		t.Fatalf("failure must name the missing version distinctly, got %q", errOut)
	}
	if out != "" {
		t.Fatalf("failure must print no partial decision, got %q", out)
	}
}

func TestReviewHelp(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		code, out, errOut := runDispatch(t, "review", flag)
		if code != 0 || errOut != "" {
			t.Fatalf("review %s: code=%d stderr=%q", flag, code, errOut)
		}
		for _, want := range []string{
			"--archive", "--org", "--seq", "--end-seq", "--fingerprint",
			"informational only", "Example:", "darksafe review",
			"Exit codes",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("review help missing %q:\n%s", want, out)
			}
		}
	}
}

// TestLegencyCommandsSurvive locks in the original CLI surface.
func TestLegacyCommandsSurvive(t *testing.T) {
	if code, out, _ := runDispatch(t); code != 0 || !strings.Contains(out, "summary:") {
		t.Fatalf("no-arg demo changed: code=%d out=%q", code, out)
	}
	if code, out, _ := runDispatch(t, "demo"); code != 0 || !strings.Contains(out, "summary:") {
		t.Fatalf("demo changed: code=%d out=%q", code, out)
	}
	if code, out, _ := runDispatch(t, "version"); code != 0 || strings.TrimSpace(out) != "darksafe 0.1.0" {
		t.Fatalf("version changed: code=%d out=%q", code, out)
	}
	if code, out, _ := runDispatch(t, "help"); code != 0 || !strings.Contains(out, "usage:") {
		t.Fatalf("help changed: code=%d out=%q", code, out)
	}
	if code, _, errOut := runDispatch(t, "bogus"); code != 2 || errOut == "" {
		t.Fatalf("unknown command exit = %d stderr=%q", code, errOut)
	}
}

// failingWriter rejects every write with a fixed error and records how many
// times it was called.
type failingWriter struct {
	err   error
	calls int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, w.err
}

// truncatingWriter accepts only the first limit bytes across all writes,
// reporting a short count WITHOUT an error — the case a plain err check
// cannot see. It records everything it accepted.
type truncatingWriter struct {
	limit    int
	accepted bytes.Buffer
	calls    int
}

func (w *truncatingWriter) Write(p []byte) (int, error) {
	w.calls++
	n := min(len(p), w.limit-w.accepted.Len())
	if n < 0 {
		n = 0
	}
	w.accepted.Write(p[:n])
	return n, nil
}

// TestReviewReportWriteFailureExitsOne covers both output modes against a
// receiver that refuses the report outright: exit 1, the concrete write
// error preserved on stderr, and nothing delivered to stdout.
func TestReviewReportWriteFailureExitsOne(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	writeErr := errors.New("disk full")

	for _, mode := range []struct {
		name string
		args []string
	}{
		{"text", reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)},
		{"json", jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)},
	} {
		t.Run(mode.name, func(t *testing.T) {
			out := &failingWriter{err: writeErr}
			var stderr bytes.Buffer
			code := runReview(mode.args, out, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "report output failed") {
				t.Fatalf("stderr must name the report output failure, got %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), writeErr.Error()) {
				t.Fatalf("stderr must keep the concrete write error, got %q", stderr.String())
			}
			if out.calls != 1 {
				t.Fatalf("report must be attempted exactly once, got %d writes", out.calls)
			}
		})
	}
}

// TestReviewReportShortWriteExitsOne covers a receiver that silently accepts
// only a prefix of the report: no error is returned, yet the missing bytes
// must still fail the review with exit 1. The delivered prefix is left in
// place and nothing further is written.
func TestReviewReportShortWriteExitsOne(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)

	for _, mode := range []struct {
		name string
		args []string
	}{
		{"text", reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)},
		{"json", jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)},
	} {
		t.Run(mode.name, func(t *testing.T) {
			out := &truncatingWriter{limit: 10}
			var stderr bytes.Buffer
			code := runReview(mode.args, out, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "report output failed") ||
				!strings.Contains(stderr.String(), "incomplete write") {
				t.Fatalf("stderr must report an incomplete report, got %q", stderr.String())
			}
			if out.accepted.Len() != 10 {
				t.Fatalf("the accepted prefix stays as delivered, got %d bytes", out.accepted.Len())
			}
			if out.calls != 1 {
				t.Fatalf("no retry or second format after a short write, got %d writes", out.calls)
			}
		})
	}
}

// TestReviewIsReadOnlyOnFile ensures a successful review leaves the archive
// bytes untouched.
func TestReviewIsReadOnlyOnFile(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runDispatch(t, reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint)...); code != 0 {
		t.Fatalf("review: %d %s", code, errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("review modified the archive file")
	}
}
