// This file implements the organization-scoped audit chain. Every successful
// policy publish or rollback, and every Decide called with a non-empty
// decision organization, leaves an immutable record in that organization.
//
// Records carry an organization-local sequence starting at 1 and a SHA-256
// fingerprint that chains each record to the previous one. The genesis
// fingerprint binds the organization name, so records from another
// organization can never validate as part of this chain even when the
// sequence numbers and identifiers coincide.
package darksafe

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Audit record categories.
const (
	// AuditPolicyChange is a successful publish or rollback.
	AuditPolicyChange = "policy_change"
	// AuditDecision is one Decide evaluation.
	AuditDecision = "decision"
)

// Sentinel errors for the audit entry points. Callers distinguish them
// with errors.Is; every one leaves state unchanged.
var (
	// ErrInvalidPage means a page size or range argument was invalid.
	ErrInvalidPage = errors.New("darksafe: invalid audit page")
	// ErrInvalidRange means the upper bound is beyond what exists, the
	// range is inverted, or a supplied fingerprint does not match.
	ErrInvalidRange = errors.New("darksafe: invalid audit range")
	// ErrAuditNotFound means no record exists at the requested sequence.
	ErrAuditNotFound = errors.New("darksafe: audit record not found")
	// ErrAuditNotADecision means the record at the sequence is not a
	// decision record and therefore cannot be rechecked.
	ErrAuditNotADecision = errors.New("darksafe: audit record is not a decision")
)

// PolicyChange captures a successful publish or rollback.
type PolicyChange struct {
	// Version is the newly created policy version.
	Version int
	// Policies is the full policy set of the new version.
	Policies []Policy
	// SourceVersion is the version copied by a rollback, and 0 for an
	// ordinary publish.
	SourceVersion int
	// RolledBack reports whether the change was produced by Rollback.
	RolledBack bool
}

// DecisionRecord captures one Decide call: the full request and the
// decision actually returned.
type DecisionRecord struct {
	Request  OrgRequest
	Decision Decision
}

// AuditRecord is one immutable entry in an organization's audit chain.
type AuditRecord struct {
	Org      string
	Seq      int // organization-local, starting at 1 and gapless
	Kind     string
	Change   *PolicyChange   // present when Kind == AuditPolicyChange
	Decision *DecisionRecord // present when Kind == AuditDecision

	// PrevFingerprint is the fingerprint of the preceding record, or the
	// genesis fingerprint for the first record.
	PrevFingerprint string
	// Fingerprint binds this record's full content to its predecessor.
	Fingerprint string
}

// hashEnvelope is the canonical, organization-bound representation that
// fingerprints are computed over. It is never exposed, so callers cannot
// forge one by constructing an AuditRecord with copied fields.
type hashEnvelope struct {
	Org             string          `json:"org"`
	Seq             int             `json:"seq"`
	Kind            string          `json:"kind"`
	Change          *PolicyChange   `json:"change,omitempty"`
	Decision        *DecisionRecord `json:"decision,omitempty"`
	PrevFingerprint string          `json:"prev"`
}

// envelopeUsesRawBytes reports whether any string protected by the
// fingerprint carries bytes that are not valid UTF-8. Such envelopes are
// hashed through encodeCanonical instead of encoding/json, because JSON
// string encoding replaces invalid bytes with U+FFFD and would let two
// different raw contents share a fingerprint.
func envelopeUsesRawBytes(env *hashEnvelope) bool {
	if !utf8.ValidString(env.Org) || !utf8.ValidString(env.Kind) ||
		!utf8.ValidString(env.PrevFingerprint) {
		return true
	}
	if env.Change != nil {
		for _, p := range env.Change.Policies {
			if !utf8.ValidString(p.ID) || !utf8.ValidString(p.Subject) ||
				!utf8.ValidString(p.Action) || !utf8.ValidString(p.Scope) ||
				!utf8.ValidString(string(p.Effect)) {
				return true
			}
		}
	}
	if env.Decision != nil {
		req := env.Decision.Request
		d := env.Decision.Decision
		for _, s := range []string{
			req.SubjectOrg, req.ResourceOrg,
			req.Subject.ID, req.Subject.Kind, req.Resource.ID, req.Resource.Scope,
			req.Action, d.Reason,
		} {
			if !utf8.ValidString(s) {
				return true
			}
		}
		for _, s := range req.Subject.Roles {
			if !utf8.ValidString(s) {
				return true
			}
		}
		for _, s := range d.Matched {
			if !utf8.ValidString(s) {
				return true
			}
		}
	}
	return false
}

// rawEnvelopePrefix opens every raw-byte canonical encoding. It never
// coincides with the start of a JSON envelope (always '{'), so
// fingerprints from the two encodings live in collision-free spaces.
const rawEnvelopePrefix = "darksafe-audit-raw-v1\x00"

// fingerprintTag delimits one field or nested value within the canonical
// binary encoding. Distinct tags keep two payloads that merely share
// adjacent bytes apart (e.g. ["ab",""] vs ["a","b"]).
type fingerprintTag byte

const (
	tagEnvelope fingerprintTag = iota + 1
	tagSeq
	tagString
	tagStringList
	tagBool
	tagPolicy
	tagPolicyList
	tagChange
	tagSubject
	tagResource
	tagDecision
	tagDecisionRecord
)

// fingerprintEncoder builds the canonical byte representation of an
// envelope. Unlike encoding/json it writes string contents byte-for-byte
// with an explicit length prefix, so strings carrying invalid UTF-8 stay
// distinguishable: encoding/json renders both a raw 0xFF byte and a raw
// 0xFE byte as the U+FFFD replacement rune, collapsing genuinely
// different content onto identical fingerprints.
type fingerprintEncoder struct {
	buf []byte
}

func (e *fingerprintEncoder) tag(t fingerprintTag) {
	e.buf = append(e.buf, byte(t))
}

func (e *fingerprintEncoder) int64Tag(t fingerprintTag, v int64) {
	e.tag(t)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	e.buf = append(e.buf, b[:]...)
}

func (e *fingerprintEncoder) boolTag(t fingerprintTag, v bool) {
	e.tag(t)
	if v {
		e.buf = append(e.buf, 1)
	} else {
		e.buf = append(e.buf, 0)
	}
}

// rawString tags and writes a string from its exact memory bytes. No
// validation, replacement or normalization is ever performed: an invalid
// UTF-8 byte reaches the hash unchanged.
func (e *fingerprintEncoder) rawString(s string) {
	e.tag(tagString)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(s)))
	e.buf = append(e.buf, b[:]...)
	e.buf = append(e.buf, s...)
}

// stringList preserves nil-vs-empty explicitly: in the JSON envelope a
// nil slice is "null" and a non-nil empty slice is "[]", so the raw
// encoding must keep the two shapes apart as well.
func (e *fingerprintEncoder) stringList(items []string) {
	e.tag(tagStringList)
	if items == nil {
		e.buf = append(e.buf, 0)
		return
	}
	e.buf = append(e.buf, 1)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(items)))
	e.buf = append(e.buf, b[:]...)
	for _, s := range items {
		e.rawString(s)
	}
}

func (e *fingerprintEncoder) policy(p Policy) {
	e.tag(tagPolicy)
	e.rawString(p.ID)
	e.rawString(p.Subject)
	e.rawString(p.Action)
	e.rawString(p.Scope)
	e.rawString(string(p.Effect))
	e.boolTag(tagBool, p.Recursive)
}

func (e *fingerprintEncoder) policyList(policies []Policy) {
	e.tag(tagPolicyList)
	if policies == nil {
		e.buf = append(e.buf, 0)
		return
	}
	e.buf = append(e.buf, 1)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(policies)))
	e.buf = append(e.buf, b[:]...)
	for _, p := range policies {
		e.policy(p)
	}
}

func (e *fingerprintEncoder) change(c *PolicyChange) {
	e.tag(tagChange)
	// A nil pointer is encoded as tagChange alone; a present value's
	// fields follow immediately.
	if c == nil {
		return
	}
	e.int64Tag(tagSeq, int64(c.Version))
	e.policyList(c.Policies)
	e.int64Tag(tagSeq, int64(c.SourceVersion))
	e.boolTag(tagBool, c.RolledBack)
}

func (e *fingerprintEncoder) subject(su Subject) {
	e.tag(tagSubject)
	e.rawString(su.ID)
	e.rawString(su.Kind)
	e.stringList(su.Roles)
	e.boolTag(tagBool, su.Disabled)
}

func (e *fingerprintEncoder) resource(r Resource) {
	e.tag(tagResource)
	e.rawString(r.ID)
	e.rawString(r.Scope)
}

func (e *fingerprintEncoder) decision(d Decision) {
	e.tag(tagDecision)
	e.boolTag(tagBool, d.Allowed)
	e.rawString(d.Reason)
	e.stringList(d.Matched)
	e.int64Tag(tagSeq, int64(d.Version))
}

func (e *fingerprintEncoder) decisionRecord(d *DecisionRecord) {
	e.tag(tagDecisionRecord)
	if d == nil {
		return
	}
	// OrgRequest fields are written inline at the same nesting depth the
	// request occupies in the envelope; no field is skipped.
	e.rawString(d.Request.SubjectOrg)
	e.rawString(d.Request.ResourceOrg)
	e.subject(d.Request.Subject)
	e.resource(d.Request.Resource)
	e.rawString(d.Request.Action)
	e.decision(d.Decision)
}

// encodeCanonical returns the raw bytes fingerprinted for an envelope
// whose strings include invalid UTF-8. Every string the record carries
// reaches the output byte-for-byte, at whatever nesting depth or list
// position it occupies. The prefix separates this encoding space from
// JSON fingerprints.
func encodeCanonical(env *hashEnvelope) []byte {
	e := &fingerprintEncoder{buf: []byte(rawEnvelopePrefix)}
	e.tag(tagEnvelope)
	e.rawString(env.Org)
	e.int64Tag(tagSeq, int64(env.Seq))
	e.rawString(env.Kind)
	e.change(env.Change)
	e.decisionRecord(env.Decision)
	e.rawString(env.PrevFingerprint)
	return e.buf
}

// genesisFingerprint derives the chain root from the organization name,
// keeping equal identifiers in different organizations independent.
func genesisFingerprint(org string) string {
	sum := sha256.Sum256([]byte("darksafe-audit-genesis\x00" + org))
	return hex.EncodeToString(sum[:])
}

// computeFingerprint returns the fingerprint for an envelope. Records made
// exclusively of valid UTF-8 keep the historical JSON fingerprint, so old
// exports and checkpoints remain valid; records containing invalid bytes
// use the length-prefixed raw encoding that protects their exact bytes.
func computeFingerprint(env *hashEnvelope) string {
	if envelopeUsesRawBytes(env) {
		sum := sha256.Sum256(encodeCanonical(env))
		return hex.EncodeToString(sum[:])
	}
	b, err := json.Marshal(env)
	if err != nil {
		// All envelope fields are plain, JSON-encodable values; a failure
		// here indicates a programming error, not user input.
		panic(fmt.Errorf("darksafe: audit envelope must encode: %w", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fingerprintFor builds and fingerprints the envelope for a record.
func fingerprintFor(r *AuditRecord) string {
	return computeFingerprint(&hashEnvelope{
		Org:             r.Org,
		Seq:             r.Seq,
		Kind:            r.Kind,
		Change:          r.Change,
		Decision:        r.Decision,
		PrevFingerprint: r.PrevFingerprint,
	})
}

// clonePolicies returns a detached copy of a policy set.
func clonePolicies(in []Policy) []Policy {
	if in == nil {
		return nil
	}
	return append([]Policy(nil), in...)
}

// cloneChange returns a detached copy of a policy-change payload.
func cloneChange(c *PolicyChange) *PolicyChange {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Policies = clonePolicies(c.Policies)
	return &cp
}

// cloneDecision returns a detached copy of a decision payload.
func cloneDecision(d *DecisionRecord) *DecisionRecord {
	if d == nil {
		return nil
	}
	cp := *d
	if len(d.Decision.Matched) > 0 {
		cp.Decision.Matched = append([]string(nil), d.Decision.Matched...)
	}
	if len(d.Request.Subject.Roles) > 0 {
		cp.Request.Subject.Roles = append([]string(nil), d.Request.Subject.Roles...)
	}
	return &cp
}

// cloneRecord returns a fully detached copy of a record.
func cloneRecord(r *AuditRecord) *AuditRecord {
	cp := *r
	cp.Change = cloneChange(r.Change)
	cp.Decision = cloneDecision(r.Decision)
	return &cp
}

// appendAuditLocked validates, fingerprints and stores one record. It runs
// under the store mutex and is the single place records are created.
func (st *orgState) appendAuditLocked(org, kind string, change *PolicyChange, decision *DecisionRecord) *AuditRecord {
	seq := len(st.audit) + 1
	prev := genesisFingerprint(org)
	if len(st.audit) > 0 {
		prev = st.audit[len(st.audit)-1].Fingerprint
	}
	// Store detached copies; later caller mutation must not reach the chain.
	rec := &AuditRecord{
		Org:             org,
		Seq:             seq,
		Kind:            kind,
		Change:          cloneChange(change),
		Decision:        cloneDecision(decision),
		PrevFingerprint: prev,
	}
	rec.Fingerprint = fingerprintFor(rec)
	st.audit = append(st.audit, rec)
	return cloneRecord(rec)
}

// auditHeadLocked returns the highest sequence in the chain and the
// fingerprint that covers it: (0, genesis) for an organization with no
// records.
func (st *orgState) auditHeadLocked(org string) (int, string) {
	if len(st.audit) == 0 {
		return 0, genesisFingerprint(org)
	}
	last := st.audit[len(st.audit)-1]
	return last.Seq, last.Fingerprint
}

// Checkpoint pins an audit query to an exact point in a chain.
type Checkpoint struct {
	Org         string
	EndSeq      int
	Fingerprint string
}

// AuditPage is one stable slice of an organization's audit chain.
type AuditPage struct {
	Org     string
	Records []AuditRecord
	// BeginSeq/EndSeq delimit the whole pinned range, not just this page.
	BeginSeq int
	EndSeq   int
	// Next is the first sequence of the following page, 0 when exhausted.
	Next int
	// Checkpoint is the pinned upper bound shared by every page.
	Checkpoint Checkpoint
}

// recordMatches reports whether a record survives the query filters.
func recordMatches(r *AuditRecord, kind, subjectID string) bool {
	if kind != "" && r.Kind != kind {
		return false
	}
	if subjectID != "" {
		// A subject filter selects decision records of that subject only.
		if r.Kind != AuditDecision || r.Decision == nil ||
			r.Decision.Request.Subject.ID != subjectID {
			return false
		}
	}
	return true
}

// AuditQuery pins the first page of an audit query. startSeq is the first
// sequence considered (1 from the beginning); endSeq <= 0 means "everything
// currently stored". The returned checkpoint must be passed to AuditPage
// for the following pages, so records appended after the first query never
// mix into later pages.
func (s *Store) AuditQuery(org string, startSeq, pageSize int, kind, subjectID string) (*AuditPage, error) {
	if org == "" {
		return nil, ErrMissingOrganization
	}
	if kind != "" && kind != AuditPolicyChange && kind != AuditDecision {
		return nil, fmt.Errorf("%w: unknown category %q", ErrInvalidPage, kind)
	}
	if pageSize <= 0 {
		return nil, fmt.Errorf("%w: page size must be positive, got %d", ErrInvalidPage, pageSize)
	}
	if startSeq < 1 {
		return nil, fmt.Errorf("%w: start sequence must be >= 1, got %d", ErrInvalidPage, startSeq)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgLocked(org)
	end, fingerprint := st.auditHeadLocked(org)
	cp := Checkpoint{Org: org, EndSeq: end, Fingerprint: fingerprint}
	if end == 0 {
		// An organization with no records always yields the empty genesis
		// page, regardless of where the caller asked to begin.
		return &AuditPage{Org: org, BeginSeq: 1, EndSeq: 0, Checkpoint: cp}, nil
	}
	if startSeq > end {
		return nil, fmt.Errorf("%w: start sequence %d is beyond end sequence %d", ErrInvalidRange, startSeq, end)
	}
	return s.auditPageLocked(org, startSeq, end, fingerprint, pageSize, kind, subjectID), nil
}

// AuditPage returns one page of a previously pinned query. nextSeq is the
// cursor from the previous page; when it is 0 the range is exhausted. The
// checkpoint is re-verified against the chain on every call.
func (s *Store) AuditPage(cp Checkpoint, nextSeq, pageSize int, kind, subjectID string) (*AuditPage, error) {
	if cp.Org == "" {
		return nil, ErrMissingOrganization
	}
	if kind != "" && kind != AuditPolicyChange && kind != AuditDecision {
		return nil, fmt.Errorf("%w: unknown category %q", ErrInvalidPage, kind)
	}
	if pageSize <= 0 {
		return nil, fmt.Errorf("%w: page size must be positive, got %d", ErrInvalidPage, pageSize)
	}
	if nextSeq < 0 {
		return nil, fmt.Errorf("%w: next sequence must be >= 0, got %d", ErrInvalidPage, nextSeq)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.orgs[cp.Org]
	if !ok {
		// No organization state: the only valid checkpoint is an empty one.
		g := genesisFingerprint(cp.Org)
		if cp.EndSeq == 0 && cp.Fingerprint == g && nextSeq == 0 {
			return &AuditPage{Org: cp.Org, BeginSeq: 0, EndSeq: 0, Checkpoint: cp}, nil
		}
		return nil, fmt.Errorf("%w: checkpoint organization has no records", ErrInvalidRange)
	}
	head := len(st.audit)
	if cp.EndSeq < 0 || cp.EndSeq > head {
		return nil, fmt.Errorf("%w: end sequence %d beyond %d", ErrInvalidRange, cp.EndSeq, head)
	}
	wantFP := genesisFingerprint(cp.Org)
	if cp.EndSeq > 0 {
		if cp.EndSeq > len(st.audit) {
			return nil, fmt.Errorf("%w: end sequence %d beyond %d", ErrInvalidRange, cp.EndSeq, head)
		}
		wantFP = st.audit[cp.EndSeq-1].Fingerprint
	}
	if cp.Fingerprint != wantFP {
		return nil, fmt.Errorf("%w: checkpoint fingerprint does not match the chain at sequence %d", ErrInvalidRange, cp.EndSeq)
	}
	if nextSeq == 0 {
		return &AuditPage{Org: cp.Org, BeginSeq: 1, EndSeq: cp.EndSeq, Checkpoint: cp}, nil
	}
	if nextSeq > cp.EndSeq+1 {
		return nil, fmt.Errorf("%w: next sequence %d beyond pinned range ending at %d", ErrInvalidRange, nextSeq, cp.EndSeq)
	}
	if nextSeq == cp.EndSeq+1 {
		return &AuditPage{Org: cp.Org, BeginSeq: nextSeq, EndSeq: cp.EndSeq, Checkpoint: cp}, nil
	}
	return s.auditPageLocked(cp.Org, nextSeq, cp.EndSeq, wantFP, pageSize, kind, subjectID), nil
}

// auditPageLocked scans forward collecting up to pageSize matching records
// and computes the next-page cursor. It does not stop at pageSize raw
// records: filtering happens within the pinned [start,end] window.
func (s *Store) auditPageLocked(org string, startSeq, endSeq int, endFP string, pageSize int, kind, subjectID string) *AuditPage {
	st := s.orgs[org]
	out := make([]AuditRecord, 0, pageSize)
	i := startSeq
	for ; i <= endSeq && len(out) < pageSize; i++ {
		r := st.audit[i-1]
		if recordMatches(r, kind, subjectID) {
			out = append(out, *cloneRecord(r))
		}
	}
	next := 0
	if i <= endSeq {
		next = i
	}
	return &AuditPage{
		Org:        org,
		Records:    out,
		BeginSeq:   startSeq,
		EndSeq:     endSeq,
		Next:       next,
		Checkpoint: Checkpoint{Org: org, EndSeq: endSeq, Fingerprint: endFP},
	}
}

// AuditExport returns detached copies of the complete records through
// endSeq, suitable for offline verification. endSeq <= 0 means everything
// currently stored. An organization with no records yields an empty slice
// together with its sequence-0 checkpoint.
func (s *Store) AuditExport(org string, endSeq int) (records []AuditRecord, cp Checkpoint, err error) {
	if org == "" {
		return nil, Checkpoint{}, ErrMissingOrganization
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.orgLocked(org)
	head := len(st.audit)
	if endSeq <= 0 {
		endSeq = head
	}
	if endSeq > head {
		return nil, Checkpoint{}, fmt.Errorf("%w: end sequence %d beyond %d", ErrInvalidRange, endSeq, head)
	}
	if endSeq < 0 {
		return nil, Checkpoint{}, fmt.Errorf("%w: end sequence must be >= 0, got %d", ErrInvalidPage, endSeq)
	}
	// The checkpoint fingerprints the final exported record, not the
	// possibly later current head.
	fingerprint := genesisFingerprint(org)
	if endSeq > 0 {
		fingerprint = st.audit[endSeq-1].Fingerprint
	}
	out := make([]AuditRecord, 0, endSeq)
	for i := 0; i < endSeq; i++ {
		out = append(out, *cloneRecord(st.audit[i]))
	}
	return out, Checkpoint{Org: org, EndSeq: endSeq, Fingerprint: fingerprint}, nil
}

// VerifyAudit validates an exported record set entirely without a Store.
// The caller separately retains the organization, end sequence and
// fingerprint they expect. It rejects any modified field, missing middle or
// tail record, swapped order, and record spliced in from another
// organization. Empty or nil records validate exactly when the checkpoint
// is the organization's genesis (end sequence 0).
func VerifyAudit(org string, records []AuditRecord, cp Checkpoint) error {
	if org == "" {
		return ErrMissingOrganization
	}
	if cp.Org != "" && cp.Org != org {
		return fmt.Errorf("%w: checkpoint organization %q does not match %q", ErrInvalidRange, cp.Org, org)
	}
	genesis := genesisFingerprint(org)
	if len(records) == 0 {
		if cp.EndSeq != 0 || cp.Fingerprint != genesis {
			return fmt.Errorf("%w: empty records but checkpoint claims end sequence %d", ErrInvalidRange, cp.EndSeq)
		}
		return nil
	}
	if cp.EndSeq != len(records) {
		return fmt.Errorf("%w: have %d records, checkpoint ends at %d", ErrInvalidRange, len(records), cp.EndSeq)
	}
	prev := genesis
	for i, r := range records {
		wantSeq := i + 1
		if r.Org != org {
			return fmt.Errorf("%w: record %d belongs to organization %q", ErrInvalidRange, wantSeq, r.Org)
		}
		if r.Seq != wantSeq {
			return fmt.Errorf("%w: record %d has sequence %d", ErrInvalidRange, wantSeq, r.Seq)
		}
		if r.PrevFingerprint != prev {
			return fmt.Errorf("%w: record %d breaks the chain", ErrInvalidRange, wantSeq)
		}
		wantFP := fingerprintFor(&r)
		if r.Fingerprint != wantFP {
			return fmt.Errorf("%w: record %d fingerprint mismatch", ErrInvalidRange, wantSeq)
		}
		if r.Kind == AuditPolicyChange {
			if r.Change == nil || r.Decision != nil {
				return fmt.Errorf("%w: record %d payload does not match its category", ErrInvalidRange, wantSeq)
			}
		} else if r.Kind == AuditDecision {
			if r.Decision == nil || r.Change != nil {
				return fmt.Errorf("%w: record %d payload does not match its category", ErrInvalidRange, wantSeq)
			}
		} else {
			return fmt.Errorf("%w: record %d has unknown category %q", ErrInvalidRange, wantSeq, r.Kind)
		}
		prev = r.Fingerprint
	}
	last := records[len(records)-1]
	if last.Fingerprint != cp.Fingerprint {
		return fmt.Errorf("%w: checkpoint fingerprint does not match the final record", ErrInvalidRange)
	}
	return nil
}

// RecheckDecision re-evaluates the request stored in an organization's
// decision record against the policy version that decision actually used.
// Later publishes or rollbacks cannot alter the result. Non-decision
// records and missing sequences return distinguishable errors.
func (s *Store) RecheckDecision(org string, seq int) (Decision, error) {
	if org == "" {
		return Decision{}, ErrMissingOrganization
	}
	if seq < 1 {
		return Decision{}, fmt.Errorf("%w: sequence %d", ErrAuditNotFound, seq)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.orgs[org]
	if !ok || seq > len(st.audit) {
		return Decision{}, fmt.Errorf("%w: organization %q has no audit sequence %d", ErrAuditNotFound, org, seq)
	}
	rec := st.audit[seq-1]
	if rec.Kind != AuditDecision || rec.Decision == nil {
		return Decision{}, fmt.Errorf("%w: sequence %d is %s", ErrAuditNotADecision, seq, rec.Kind)
	}
	req := rec.Decision.Request
	version := rec.Decision.Decision.Version
	if version == 0 {
		// The original decision evaluated no published version: requests
		// rejected at the envelope, or an organization with no publish yet.
		if d, ok := checkRequest(org, req); !ok {
			return d, nil
		}
		return Decision{Allowed: false, Reason: "organization has no published version"}, nil
	}
	src, ok := st.versions[version]
	if !ok {
		// A decision's version is immutable and never deleted; its absence
		// indicates store tampering rather than normal operation.
		return Decision{}, fmt.Errorf("%w: decision used version %d, which no longer exists", ErrVersionNotFound, version)
	}
	return evaluate(req, append([]Policy(nil), src...), version), nil
}
