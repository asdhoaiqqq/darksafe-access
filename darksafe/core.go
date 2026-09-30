// Package darksafe implements the zero-trust access decision core.
package darksafe

// Subject is an identity that can request access to a resource.
type Subject struct {
	ID       string
	Kind     string   // "user", "service", "external"
	Roles    []string
	Disabled bool
}

// Resource is a protected object with a scope path.
type Resource struct {
	ID    string
	Scope string // e.g. "org/payments/ledger"
}

// Decision explains one access evaluation.
type Decision struct {
	Allowed bool
	Reason  string
	Matched []string
}

// Access evaluates a request and always returns an explanation.
func Access(subject Subject, resource Resource, action string) Decision {
	if subject.Disabled {
		return Decision{Allowed: false, Reason: "subject is disabled"}
	}
	if subject.ID == "" || resource.ID == "" || action == "" {
		return Decision{Allowed: false, Reason: "incomplete request"}
	}
	needed := action + ":" + resource.Scope
	for _, role := range subject.Roles {
		if role == "owner" || role == resource.Scope+":"+action {
			return Decision{Allowed: true, Reason: "role grants " + needed, Matched: []string{role}}
		}
	}
	return Decision{Allowed: false, Reason: "no role grants " + needed}
}
