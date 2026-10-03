package darksafe

import (
	"fmt"
	"strings"
)

// This file holds the cluster-identity rule shared by the two ways a release
// configuration can be validated: parsing a JSON document (ParseReleaseInput)
// and validating a directly constructed Go config (ValidateReleaseInput).
//
// The rule itself: a candidate's ID must not be empty or only whitespace, and
// IDs must be unique across every candidate — disabled clusters and clusters
// that include/exclude conditions would filter out still take part, because
// identity is checked before any batching decision. Emptiness is the only
// whitespace-aware test: a legal ID is compared and kept exactly as written,
// so "c-a", "C-A" and " c-a" are three different IDs that are never trimmed,
// merged, or rewritten. Both paths report the same error for the same
// violation: the candidate's position in the input array (from zero) plus the
// empty-ID or duplicate-ID reason.

// clusterIDSet tracks the IDs already seen while candidates are checked in
// input order, so a later occurrence of an ID can be reported at its own
// position.
type clusterIDSet struct {
	seen map[string]struct{}
}

func newClusterIDSet(capacity int) *clusterIDSet {
	return &clusterIDSet{seen: make(map[string]struct{}, capacity)}
}

// check applies the cluster-identity rule to one candidate's ID at position
// index: the ID must not be empty or whitespace-only, and must not repeat an
// earlier candidate's ID. A legal ID is recorded exactly as written.
func (s *clusterIDSet) check(index int, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("clusters[%d]: 字段 %q 不能为空或只含空白", index, "id")
	}
	if _, dup := s.seen[id]; dup {
		return fmt.Errorf("clusters[%d]: 重复的集群标识 %q", index, id)
	}
	s.seen[id] = struct{}{}
	return nil
}
