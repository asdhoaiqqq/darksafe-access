// This file implements the organization-scoped audit chain. Every
// successful publish and rollback, and every Decide that names a
// decision organization, is appended to a per-organization sequence of
// records linked by SHA-256 fingerprints. Records store detached
// copies, so requests, policies or query results mutated after the fact
// can never rewrite history. The chain can be paged, exported up to a
// frozen sequence range, and verified offline against an out-of-band
// (organization, up-to-sequence, fingerprint) basis.
package darksafe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// AuditCategory classifies an audit record.
type AuditCategory string

const (
	// AuditPolicyChange marks a publish or rollback that produced a new
	// policy version.
	AuditPolicyChange AuditCategory = "policy_change"
	// AuditDecision marks a Decide that named a decision organization,
	// including denials.
	AuditDecision AuditCategory = "decision"
)

// AuditRecord is one immutable entry in an organization's audit chain.
// Seq is the organization-local, 1-based consecutive number. Each record
// carries the fingerprint of the preceding record, which links the chain.
type AuditRecord struct {
	Org             string        `json:"org"`
	Seq             int           `json:"seq"`
	Category        AuditCategory `json:"category"`
	Version         int           `json:"version"`
	SourceVersion   int           `json:"source_version,omitempty"`
	Policies        []Policy      `json:"policies,omitempty"`
	Request         OrgRequest    `json:"request"`
	Decision        Decision      `json:"decision"`
	PrevFingerprint string        `json:"prev_fingerprint"`
	Fingerprint     string        `json:"fingerprint"`
}

// AuditQuery pages through an organization's audit chain in ascending
// sequence order. The first query leaves UpToSeq and Fingerprint at zero,
// which freezes the range at the current tail; later queries must echo
// that range so records appended in the meantime do not mix in.
type AuditQuery struct {
	PageSize    int           `json:"page_size"`
	UpToSeq     int           `json:"up_to_seq"`
	Fingerprint string        `json:"fingerprint"`
	AfterSeq    int           `json:"after_seq"`
	Category    AuditCategory `json:"category,omitempty"`
	Subject     string        `json:"subject,omitempty"`
}

// AuditPage is one page of an audit query. NextSeq is zero when the
// frozen range is exhausted.
type AuditPage struct {
	Org         string        `json:"org"`
	UpToSeq     int           `json:"up_to_seq"`
	Fingerprint string        `json:"fingerprint"`
	Records     []AuditRecord `json:"records"`
	NextSeq     int           `json:"next_seq"`
}

// AuditExport is a self-contained package for offline verification. The
// caller keeps Org, UpToSeq and Fingerprint out of band as the trusted
// basis; the records can then be verified without the original Store.
type AuditExport struct {
	Org         string        `json:"org"`
	UpToSeq     int           `json:"up_to_seq"`
	Fingerprint string        `json:"fingerprint"`
	Records     []AuditRecord `json:"records"`
}

// Sentinel errors so callers can distinguish failure causes with errors.Is.
var (
	// ErrAuditNotFound means the requested audit sequence does not exist.
	ErrAuditNotFound = errors.New("darksafe: audit record not found")
	// ErrAuditNotDecision means reverify was asked for a non-decision record.
	ErrAuditNotDecision = errors.New("darksafe: audit record is not a decision")
	// ErrInvalidAuditQuery means a query or export range is invalid.
	ErrInvalidAuditQuery = errors.New("darksafe: invalid audit query")
	// ErrAuditFingerprint means the echoed range fingerprint does not match
	// the chain at that sequence.
	ErrAuditFingerprint = errors.New("darksafe: audit fingerprint mismatch")
	// ErrAuditVerification means an export failed offline verification.
	ErrAuditVerification = errors.New("darksafe: audit verification failed")
)

// clonePolicies returns a detached copy of a policy slice.
func clonePolicies(ps []Policy) []Policy {
	if ps == nil {
		return nil
	}
	out := make([]Policy, len(ps))
	copy(out, ps)
	return out
}

// cloneRequest returns a detached copy of a request, including its
// subject roles. A nil roles slice stays nil and a non-nil one stays
// non-nil, so detached copies compare equal to the originals.
func cloneRequest(r OrgRequest) OrgRequest {
	if r.Subject.Roles != nil {
		cp := make([]string, len(r.Subject.Roles))
		copy(cp, r.Subject.Roles)
		r.Subject.Roles = cp
	}
	return r
}

// cloneDecision returns a detached copy of a decision, including its
// matched policy ids. A nil matched slice stays nil and a non-nil one
// stays non-nil, so detached copies compare equal to the originals.
func cloneDecision(d Decision) Decision {
	if d.Matched != nil {
		cp := make([]string, len(d.Matched))
		copy(cp, d.Matched)
		d.Matched = cp
	}
	return d
}

// cloneRecord returns a deep copy of a record.
func cloneRecord(r *AuditRecord) *AuditRecord {
	out := *r
	out.Policies = clonePolicies(r.Policies)
	out.Request = cloneRequest(r.Request)
	out.Decision = cloneDecision(r.Decision)
	return &out
}

// auditCanonical is the deterministic input to the fingerprint. Struct
// field order is fixed and no maps are used, so json.Marshal is stable
// across runs and processes.
type auditCanonical struct {
	Org             string
	Seq             int
	Category        string
	Version         int
	SourceVersion   int
	Policies        []Policy
	Request         OrgRequest
	Decision        Decision
	PrevFingerprint string
}

// computeFingerprint links a record to the preceding one: it hashes the
// record's canonical content together with the previous fingerprint.
func computeFingerprint(prev string, r *AuditRecord) string {
	in := auditCanonical{
		Org: r.Org, Seq: r.Seq, Category: string(r.Category),
		Version: r.Version, SourceVersion: r.SourceVersion,
		Policies: r.Policies, Request: r.Request, Decision: r.Decision,
		PrevFingerprint: prev,
	}
	b, err := json.Marshal(in)
	if err != nil {
		// All fields are JSON-safe types; this is unreachable.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// appendRecordLocked appends a record to the organization's chain,
// assigning the next sequence number and linking fingerprints. The
// caller must hold s.mu.
func (s *Store) appendRecordLocked(st *orgState, r *AuditRecord) {
	r.Seq = len(st.audit) + 1
	r.PrevFingerprint = ""
	if r.Seq > 1 {
		r.PrevFingerprint = st.audit[r.Seq-2].Fingerprint
	}
	r.Fingerprint = computeFingerprint(r.PrevFingerprint, r)
	st.audit = append(st.audit, r)
}

// auditTailLocked returns the current record count and the fingerprint
// of the last record ("" for an empty chain). The caller must hold s.mu.
func auditTailLocked(st *orgState) (int, string) {
	if st == nil || len(st.audit) == 0 {
		return 0, ""
	}
	return len(st.audit), st.audit[len(st.audit)-1].Fingerprint
}

// matches reports whether a record passes the query's filters. A subject
// filter implies decision records only, since policy changes carry no
// subject.
func (q AuditQuery) matches(r *AuditRecord) bool {
	if q.Subject != "" {
		if r.Category != AuditDecision || r.Request.Subject.ID != q.Subject {
			return false
		}
	}
	if q.Category != "" && r.Category != q.Category {
		return false
	}
	return true
}

// AuditQuery returns one page of the organization's audit chain, paged
// in ascending sequence order. The first query (UpToSeq zero) freezes
// the range at the current tail; later queries echo UpToSeq and
// Fingerprint from the first page and continue after AfterSeq, so
// concurrently appended records never mix in.
func (s *Store) AuditQuery(org string, q AuditQuery) (AuditPage, error) {
	if org == "" {
		return AuditPage{}, ErrMissingOrganization
	}
	if q.PageSize <= 0 {
		return AuditPage{}, fmt.Errorf("%w: page size must be positive, got %d", ErrInvalidAuditQuery, q.PageSize)
	}
	if q.UpToSeq < 0 {
		return AuditPage{}, fmt.Errorf("%w: upToSeq must not be negative, got %d", ErrInvalidAuditQuery, q.UpToSeq)
	}
	if q.AfterSeq < 0 {
		return AuditPage{}, fmt.Errorf("%w: afterSeq must not be negative, got %d", ErrInvalidAuditQuery, q.AfterSeq)
	}
	if q.Category != "" && q.Category != AuditPolicyChange && q.Category != AuditDecision {
		return AuditPage{}, fmt.Errorf("%w: unknown category %q", ErrInvalidAuditQuery, q.Category)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgs[org]
	tail, _ := auditTailLocked(st)

	upTo := q.UpToSeq
	if upTo == 0 {
		upTo = tail
	} else if upTo > tail {
		return AuditPage{}, fmt.Errorf("%w: upToSeq %d exceeds %d existing records", ErrInvalidAuditQuery, upTo, tail)
	}
	if q.AfterSeq > upTo {
		return AuditPage{}, fmt.Errorf("%w: afterSeq %d is beyond upToSeq %d", ErrInvalidAuditQuery, q.AfterSeq, upTo)
	}
	wantFP := ""
	if st != nil && upTo > 0 {
		wantFP = st.audit[upTo-1].Fingerprint
	}
	if q.UpToSeq != 0 && q.Fingerprint != wantFP {
		return AuditPage{}, fmt.Errorf("%w: fingerprint at seq %d does not match", ErrAuditFingerprint, upTo)
	}

	page := AuditPage{Org: org, UpToSeq: upTo, Fingerprint: wantFP}
	for seq := q.AfterSeq + 1; seq <= upTo; seq++ {
		r := st.audit[seq-1]
		if !q.matches(r) {
			continue
		}
		if len(page.Records) < q.PageSize {
			page.Records = append(page.Records, *cloneRecord(r))
			// NextSeq is the last returned sequence; the caller passes
			// it as AfterSeq to continue.
			page.NextSeq = seq
			continue
		}
		// The page is full and at least one more matching record exists.
		return page, nil
	}
	// The frozen range is exhausted.
	page.NextSeq = 0
	return page, nil
}

// AuditExport returns detached copies of all records with sequence
// numbers 1..upToSeq, together with the tail fingerprint, for offline
// verification. upToSeq may be zero, which exports an empty chain.
func (s *Store) AuditExport(org string, upToSeq int) (AuditExport, error) {
	if org == "" {
		return AuditExport{}, ErrMissingOrganization
	}
	if upToSeq < 0 {
		return AuditExport{}, fmt.Errorf("%w: upToSeq must not be negative, got %d", ErrInvalidAuditQuery, upToSeq)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgs[org]
	tail, _ := auditTailLocked(st)
	if upToSeq > tail {
		return AuditExport{}, fmt.Errorf("%w: upToSeq %d exceeds %d existing records", ErrInvalidAuditQuery, upToSeq, tail)
	}
	exp := AuditExport{Org: org, UpToSeq: upToSeq}
	if upToSeq > 0 {
		exp.Fingerprint = st.audit[upToSeq-1].Fingerprint
	}
	exp.Records = make([]AuditRecord, upToSeq)
	for i := 0; i < upToSeq; i++ {
		exp.Records[i] = *cloneRecord(st.audit[i])
	}
	return exp, nil
}

// policiesForVersion finds the policy-change record that created the
// given version and returns its full policy set. Version 0 has no
// policy set.
func policiesForVersion(records []AuditRecord, version int) ([]Policy, error) {
	if version == 0 {
		return nil, nil
	}
	for i := range records {
		r := &records[i]
		if r.Category == AuditPolicyChange && r.Version == version {
			return r.Policies, nil
		}
	}
	return nil, fmt.Errorf("no policy change record creates version %d", version)
}

// recomputeDecision restores the decision for a request under the given
// version's policies, mirroring Decide. Version 0 is the pre-publish
// denial.
func recomputeDecision(org string, req OrgRequest, version int, policies []Policy) Decision {
	if d, ok := checkRequest(org, req); !ok {
		return d
	}
	if version == 0 {
		return Decision{Allowed: false, Reason: "organization has no published version"}
	}
	return evaluate(req, policies, version)
}

// VerifyAuditExport verifies an export offline, without the original
// Store. It checks that the record count matches UpToSeq, that sequence
// numbers are exactly 1..N, that every record belongs to the export's
// organization, that every fingerprint links to its predecessor, and
// that every decision record is the correct outcome for its request
// under the policy version it names. The caller must separately trust
// Org, UpToSeq and the tail Fingerprint; any field mutation, deleted
// middle or tail record, swapped order, or spliced-in record fails.
func VerifyAuditExport(exp AuditExport) error {
	if exp.Org == "" {
		return ErrMissingOrganization
	}
	if exp.UpToSeq < 0 {
		return fmt.Errorf("%w: upToSeq must not be negative", ErrInvalidAuditQuery)
	}
	if len(exp.Records) != exp.UpToSeq {
		return fmt.Errorf("%w: declared %d records but found %d", ErrAuditVerification, exp.UpToSeq, len(exp.Records))
	}
	prev := ""
	for i := range exp.Records {
		r := &exp.Records[i]
		if r.Seq != i+1 {
			return fmt.Errorf("%w: record %d has sequence %d", ErrAuditVerification, i+1, r.Seq)
		}
		if r.Org != exp.Org {
			return fmt.Errorf("%w: record %d belongs to organization %q", ErrAuditVerification, r.Seq, r.Org)
		}
		if r.Category != AuditPolicyChange && r.Category != AuditDecision {
			return fmt.Errorf("%w: record %d has unknown category %q", ErrAuditVerification, r.Seq, r.Category)
		}
		fp := computeFingerprint(prev, r)
		if fp != r.Fingerprint {
			return fmt.Errorf("%w: fingerprint mismatch at record %d", ErrAuditVerification, r.Seq)
		}
		prev = fp
		if r.Category == AuditDecision {
			policies, err := policiesForVersion(exp.Records, r.Version)
			if err != nil {
				return fmt.Errorf("%w: record %d: %v", ErrAuditVerification, r.Seq, err)
			}
			want := recomputeDecision(exp.Org, r.Request, r.Version, policies)
			if !reflect.DeepEqual(want, r.Decision) {
				return fmt.Errorf("%w: record %d: decision does not match version %d", ErrAuditVerification, r.Seq, r.Version)
			}
		}
	}
	if prev != exp.Fingerprint {
		return fmt.Errorf("%w: tail fingerprint mismatch", ErrAuditVerification)
	}
	return nil
}

// ReverifyDecision restores the original result of a past Decide using
// the request and policy version stored in the decision record with the
// given organization-local sequence. Later publishes or rollbacks do
// not affect the conclusion. A non-decision record returns
// ErrAuditNotDecision; a sequence that does not exist returns
// ErrAuditNotFound. Reverify is read-only and creates no records.
func (s *Store) ReverifyDecision(org string, seq int) (Decision, error) {
	if org == "" {
		return Decision{}, ErrMissingOrganization
	}
	if seq < 1 {
		return Decision{}, fmt.Errorf("%w: sequence %d", ErrAuditNotFound, seq)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgs[org]
	if st == nil || seq > len(st.audit) {
		return Decision{}, fmt.Errorf("%w: organization %q has no audit record %d", ErrAuditNotFound, org, seq)
	}
	rec := st.audit[seq-1]
	if rec.Category != AuditDecision {
		return Decision{}, fmt.Errorf("%w: record %d is %q, not a decision", ErrAuditNotDecision, seq, rec.Category)
	}
	var policies []Policy
	if rec.Version >= 1 {
		src, ok := st.versions[rec.Version]
		if !ok {
			return Decision{}, fmt.Errorf("%w: version %d no longer exists", ErrAuditVerification, rec.Version)
		}
		policies = append([]Policy(nil), src...)
	}
	return recomputeDecision(org, rec.Request, rec.Version, policies), nil
}
