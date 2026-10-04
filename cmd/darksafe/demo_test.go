package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDemoOutputPinsRoleBasis fixes the demo entry point surface: it runs
// the role-based Access core without any policy store, so its deterministic
// output names role-grant reasons, the disabled subject is denied, and no
// policy vocabulary or policy version ever appears. If the demo started
// depending on published policies, or silently dropped the disabled
// boundary, this pin fails.
func TestDemoOutputPinsRoleBasis(t *testing.T) {
	var buf bytes.Buffer
	runDemo(&buf)
	got := buf.String()

	want := strings.Join([]string{
		`subject=u-1001 allowed=true reason=role grants read:org/payments/ledger`,
		`subject=svc-batch allowed=true reason=role grants read:org/payments/ledger`,
		`subject=u-1002 allowed=false reason=subject is disabled`,
		`summary: 2 of 3 subjects allowed`,
		"",
	}, "\n")
	if got != want {
		t.Fatalf("demo output drifted from the role-based contract:\nwant:\n%q\n got:\n%q", want, got)
	}

	// The demo has no policy store: policy-basis vocabulary must never show.
	for _, forbidden := range []string{
		"matched allow policy",
		"no published version",
		"no matching allow policy",
		"policy version",
		"version=",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("demo output must stay role-based, contains policy vocabulary %q:\n%s", forbidden, got)
		}
	}

	// The disabled principal is denied even though the other two carry
	// granting roles; this is the shared boundary at the demo surface.
	if !strings.Contains(got, "subject=u-1002 allowed=false reason=subject is disabled") {
		t.Fatalf("demo must show the disabled-subject denial:\n%s", got)
	}
}
