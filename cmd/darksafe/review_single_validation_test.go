// End-to-end pin for the single-validation review flow. The review command
// reads the archive (which verifies the complete audit chain against the
// separately retained checkpoint) and then replays the target from that
// already-verified material. These tests drive the real dispatch and count
// complete chain validations through the library's testing observer: one
// invocation, in either report format, must validate the whole chain
// exactly once — including when validation or the target fails.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// countChainValidations installs the library observer and returns the
// running count plus a restore function.
func countChainValidations(t *testing.T) (count func() int, restore func()) {
	t.Helper()
	n := 0
	restoreObserver := darksafe.SetVerifyAuditObserverForTesting(func() { n++ })
	return func() int { return n }, restoreObserver
}

// TestReviewCommandValidatesChainOnce drives the full command for one
// archive and proves the chain is walked once per invocation, for both the
// text report and --json.
func TestReviewCommandValidatesChainOnce(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)

	for name, args := range map[string][]string{
		"text": reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint),
		"json": jsonReviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint),
	} {
		t.Run(name, func(t *testing.T) {
			count, restore := countChainValidations(t)
			defer restore()

			code, out, errOut := runDispatch(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, errOut)
			}
			if out == "" {
				t.Fatal("expected a report on stdout")
			}
			if got := count(); got != 1 {
				t.Fatalf("one review validated the complete chain %d times, want exactly 1", got)
			}

			// A second invocation over the same file validates once again:
			// the conclusion belongs to each invocation, never reused.
			code, _, errOut = runDispatch(t, args...)
			if code != 0 {
				t.Fatalf("second review exit = %d, stderr = %q", code, errOut)
			}
			if got := count(); got != 2 {
				t.Fatalf("two invocations validated the chain %d times total, want 2", got)
			}
		})
	}
}

// TestReviewCommandFailuresStillValidateOnce pins the count on the
// rejection paths: neither a material failure nor a target failure walks
// the chain more than once, and none delivers a partial report. A retained
// checkpoint that contradicts the archive header is rejected before the
// chain pass even starts (wantPass 0); record tampering and target errors
// are reached after exactly one pass (wantPass 1).
func TestReviewCommandFailuresStillValidateOnce(t *testing.T) {
	const org = "acme"
	path, cp := fixtureChain(t, org)

	// A chain whose early target (seq 2) uses u1 and whose later record
	// (seq 4) carries a unique subject, archived and then tampered in that
	// later record with a valid checksum: framing passes, so the single
	// whole-chain validation is what must reject it.
	tamperedPath, tamperedCP := lateTamperedArchive(t, org)

	cases := []struct {
		name     string
		args     []string
		wantPass int
	}{
		{"header/checkpoint mismatch", reviewArgs(path, org, 2, cp.EndSeq, cp.Fingerprint[:len(cp.Fingerprint)-1]+"0"), 0},
		{"record tampered after target", reviewArgs(tamperedPath, org, 2, tamperedCP.EndSeq, tamperedCP.Fingerprint), 1},
		{"target past export", reviewArgs(path, org, 99, cp.EndSeq, cp.Fingerprint), 1},
		{"target is policy change", reviewArgs(path, org, 1, cp.EndSeq, cp.Fingerprint), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, restore := countChainValidations(t)
			defer restore()

			code, out, errOut := runDispatch(t, tc.args...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%q", code, errOut)
			}
			if out != "" {
				t.Fatalf("failure must deliver no partial report, stdout=%q", out)
			}
			if got := count(); got != tc.wantPass {
				t.Fatalf("failed review walked the chain %d times, want %d", got, tc.wantPass)
			}
		})
	}
}

// lateTamperedArchive builds publish v1 (seq 1), an early u1 decision
// (seq 2, the review target), publish v2 (seq 3) and a later decision with
// a uniquely named subject (seq 4), archives it, tampers one byte of that
// later subject with a consistent frame checksum, and returns the file path
// and the (still separately retained) checkpoint.
func lateTamperedArchive(t *testing.T, org string) (string, darksafe.Checkpoint) {
	t.Helper()
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
	tampered := reframeArchive(t, good, func(payload []byte) {
		idx := bytesIndex(payload, []byte("late-subject"))
		payload[idx] ^= 0x01 // same length; only the later record changes
	})
	path := filepath.Join(t.TempDir(), "late-damage.bin")
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cp
}

// bytesIndex returns the first index of needle in haystack.
func bytesIndex(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}
