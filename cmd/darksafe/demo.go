package main

import (
	"fmt"
	"io"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// runDemo is the original no-argument demonstration: a handful of subjects
// are evaluated by the role-based core, independent of any Store.
func runDemo(w io.Writer) {
	subjects := []darksafe.Subject{
		{ID: "u-1001", Kind: "user", Roles: []string{"org/payments/ledger:read"}},
		{ID: "svc-batch", Kind: "service", Roles: []string{"owner"}},
		{ID: "u-1002", Kind: "user", Disabled: true},
	}
	resource := darksafe.Resource{ID: "ledger-main", Scope: "org/payments/ledger"}
	allowed := 0
	for _, subject := range subjects {
		decision := darksafe.Access(subject, resource, "read")
		if decision.Allowed {
			allowed++
		}
		fmt.Fprintf(w, "subject=%s allowed=%v reason=%s\n", subject.ID, decision.Allowed, decision.Reason)
	}
	fmt.Fprintf(w, "summary: %d of %d subjects allowed\n", allowed, len(subjects))
}
