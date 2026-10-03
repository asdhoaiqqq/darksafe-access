// End-to-end CLI regression for rolling back to an empty historical policy
// version and then reviewing the resulting default denial offline with
// `darksafe review`. The archive is built from the real Store, written to a
// file, and re-read solely through the command using a separately retained
// checkpoint, exactly as an operator would after the service instance ended.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// emptyRollbackFixture builds: publish empty v1, publish allow v2, an allowed
// decision (seq 3), rollback to v1 producing v3 (seq 4), and the post-rollback
// default-denial decision (seq 5). It covers both empty input shapes.
func emptyRollbackFixture(t *testing.T, org string, emptyPolicies []darksafe.Policy) (string, darksafe.Checkpoint) {
	t.Helper()
	s := darksafe.NewStore()
	if v, err := s.Publish(org, 0, emptyPolicies); err != nil || v != 1 {
		t.Fatalf("publish empty v1: %d %v", v, err)
	}
	allow := []darksafe.Policy{
		{ID: "p-ledger-read", Subject: "u1", Action: "read",
			Scope: "org/acme/ledger", Effect: darksafe.EffectAllow},
	}
	if v, err := s.Publish(org, 1, allow); err != nil || v != 2 {
		t.Fatalf("publish allow v2: %d %v", v, err)
	}
	req := darksafe.OrgRequest{
		SubjectOrg: org, ResourceOrg: org,
		Subject:  darksafe.Subject{ID: "u1", Roles: []string{"owner"}},
		Resource: darksafe.Resource{ID: "ledger-1", Scope: "org/acme/ledger"},
		Action:   "read",
	}
	if d := s.Decide(org, req); !d.Allowed || d.Version != 2 {
		t.Fatalf("decision under v2 = %+v, want allowed at v2", d)
	}
	if v, err := s.Rollback(org, 2, 1); err != nil || v != 3 {
		t.Fatalf("rollback to empty v1: %d %v", v, err)
	}
	if d := s.Decide(org, req); d.Allowed || d.Version != 3 ||
		d.Reason != "no matching allow policy" || len(d.Matched) != 0 {
		t.Fatalf("decision under rolled-back v3 = %+v, want default denial at v3", d)
	}
	recs, cp, err := s.AuditExport(org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := darksafe.VerifyAudit(org, recs, cp); err != nil {
		t.Fatal(err)
	}
	archive, err := darksafe.EncodeAuditArchive(org, recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "empty-rollback.bin")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cp
}

// TestReviewAfterRollbackToEmptyVersion verifies the offline command re-derives
// the post-rollback default denial from the archived rollback record alone:
// the empty v3 content is treated as a real version, never as missing, and the
// allowing v2 is never substituted.
func TestReviewAfterRollbackToEmptyVersion(t *testing.T) {
	for shape, emptyPolicies := range map[string][]darksafe.Policy{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(shape, func(t *testing.T) {
			const org = "acme"
			path, cp := emptyRollbackFixture(t, org, emptyPolicies)

			// Seq 5 is the denial made under the rolled-back empty v3.
			code, out, errOut := runDispatch(t, reviewArgs(path, org, 5, cp.EndSeq, cp.Fingerprint)...)
			if code != 0 {
				t.Fatalf("review exit = %d, stderr = %q", code, errOut)
			}
			if errOut != "" {
				t.Fatalf("unexpected stderr: %q", errOut)
			}
			for _, want := range []string{
				"target sequence: 5",
				"allowed: false",
				`reason: "no matching allow policy"`,
				"matched policies: []",
				"policy version: 3",
				"consistent: yes",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q:\n%s", want, out)
				}
			}

			// The earlier allowed decision (seq 3) still replays at v2: the
			// offline reviewer never lets the later rollback rewrite history.
			code, early, errOut := runDispatch(t, reviewArgs(path, org, 3, cp.EndSeq, cp.Fingerprint)...)
			if code != 0 {
				t.Fatalf("review seq 3 exit = %d, stderr = %q", code, errOut)
			}
			for _, want := range []string{
				"allowed: true",
				`reason: "matched allow policy"`,
				`matched policies: ["p-ledger-read"]`,
				"policy version: 2",
				"consistent: yes",
			} {
				if !strings.Contains(early, want) {
					t.Fatalf("seq 3 output missing %q:\n%s", want, early)
				}
			}
		})
	}
}
