// This file implements lossless archiving of one organization's audit
// export. AuditExport hands back detached in-memory records and the caller
// separately retains the checkpoint pinning them; an archive serializes the
// export so it can be saved to a file when the service instance ends and
// read back later, still paired with the caller's own checkpoint and still
// usable as the material for RecheckDecisionOffline.
//
// The format is a length-delimited binary encoding built by hand rather
// than encoding/json: JSON text cannot carry bytes that are not valid
// UTF-8 without silently replacing them with U+FFFD, which would collapse
// genuinely different content (a lone 0xFF, a lone 0xFE and a real U+FFFD)
// onto identical records and change their fingerprints. Every string is
// written from its exact bytes with a length prefix, and nil slices,
// non-nil empty slices and absent payloads are each marked explicitly.
//
// Integrity never comes from trust in the file. A decode reconstructs the
// records and then runs the same whole-material VerifyAudit chain check
// against the checkpoint the caller supplies independently; the checkpoint
// embedded in the archive is informational only and never authoritative.
package darksafe

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrInvalidArchive means archive bytes are not a recognizable, complete,
// untampered audit archive: a foreign/truncated/trailing-byte framing, a
// failed checksum, or content that does not decode. Content-level failures
// (missing or mismatched organization, missing, out-of-order or modified
// records, a mismatched checkpoint) keep using ErrMissingOrganization and
// ErrInvalidRange exactly as the other audit entry points do.
var ErrInvalidArchive = errors.New("darksafe: invalid audit archive")

// Archive framing:
//
//	magic   = archiveMagic
//	u32 BE  = byte length of the payload that follows
//	payload = archivePayloadLen bytes
//	32 bytes = SHA-256 checksum of payload
//
// A second archive concatenated to a first is trailing material, not a
// second frame: a read consumes exactly one frame and rejects any byte
// beyond it.
const archiveMagic = "darksafe-audit-archive-v1\n"

const (
	archiveTagBoolFalse byte = 0
	archiveTagBoolTrue  byte = 1
	archiveTagNil       byte = 0
	archiveTagPresent   byte = 1
)

// archiveWriter appends length-delimited fields to a byte buffer.
type archiveWriter struct {
	buf []byte
	err error
}

func (w *archiveWriter) raw(data []byte) {
	w.buf = append(w.buf, data...)
}

// u32 writes a non-negative int as a big-endian uint32.
func (w *archiveWriter) u32(v int) {
	if v < 0 || uint64(v) > 0xffffffff {
		w.err = fmt.Errorf("%w: length out of range: %d", ErrInvalidArchive, v)
		return
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	w.buf = append(w.buf, b[:]...)
}

func (w *archiveWriter) boolean(v bool) {
	if v {
		w.buf = append(w.buf, archiveTagBoolTrue)
	} else {
		w.buf = append(w.buf, archiveTagBoolFalse)
	}
}

// put appends one raw framing byte. It is the archive sink's side of the
// shared list shape: the list presence marker (archiveTagNil for nil,
// archiveTagPresent for a non-nil list) goes into the payload verbatim.
func (w *archiveWriter) put(b byte) {
	w.buf = append(w.buf, b)
}

// listCount writes a list element count through the archive's own
// range-checked u32, so a list length the uint32 format cannot represent
// still fails the whole archive with the existing ErrInvalidArchive rather
// than emitting truncated framing or returning partial bytes.
func (w *archiveWriter) listCount(n int) {
	w.u32(n)
}

// stringField writes a string from its exact memory bytes. Nothing is
// validated, replaced or normalized: a lone 0xFF reaches the output as
// itself and stays distinguishable from 0xFE and a genuine U+FFFD.
func (w *archiveWriter) stringField(s string) {
	w.u32(len(s))
	if w.err != nil {
		return
	}
	w.buf = append(w.buf, s...)
}

// policy writes one Policy by walking the single policy field table, so the
// archive's field order is the fingerprint's order by construction. Even the
// fingerprint-optional ResourceID is always written here (empty included),
// so every field round trips on its own; its placement is fixed by the
// table and therefore cannot move a fingerprint boundary.
func (w *archiveWriter) policy(p Policy) {
	for i := range policyFields {
		f := &policyFields[i]
		switch f.kind {
		case policyFieldString, policyFieldOptionalString:
			w.stringField(f.getString(p))
		case policyFieldBool:
			w.boolean(f.getBool(p))
		}
	}
}

// change writes the policy-change payload, or a single absence marker:
// VerifyAudit only ever accepts a change payload on a policy-change record
// and its absence otherwise, but the distinction is encoded explicitly so
// decode never has to reconstruct a shape. A present payload walks the
// single policy-change field table, so the archive's field order is the
// fingerprint's order by construction and every change field round trips.
func (w *archiveWriter) change(c *PolicyChange) {
	if c == nil {
		w.buf = append(w.buf, archiveTagNil)
		return
	}
	w.buf = append(w.buf, archiveTagPresent)
	for i := range changeFields {
		f := &changeFields[i]
		switch f.kind {
		case changeFieldInt:
			w.u32(f.getInt(c))
		case changeFieldPolicyList:
			writeListShape(w, f.getPolicies(c), w.policy)
		case changeFieldBool:
			w.boolean(f.getBool(c))
		}
	}
}

// decisionRecord writes the decision payload, or a single absence marker:
// VerifyAudit only ever accepts a decision payload on a decision record and
// its absence otherwise, but the distinction is encoded explicitly so decode
// never has to reconstruct a shape. A present payload walks the single
// decision field table, so the archive's field order is the fingerprint's
// order by construction and every request and decision field round trips.
func (w *archiveWriter) decisionRecord(d *DecisionRecord) {
	if d == nil {
		w.buf = append(w.buf, archiveTagNil)
		return
	}
	w.buf = append(w.buf, archiveTagPresent)
	for i := range decisionFields {
		f := &decisionFields[i]
		switch f.kind {
		case decisionFieldString:
			w.stringField(f.getString(d))
		case decisionFieldStringList:
			writeListShape(w, f.getList(d), w.stringField)
		case decisionFieldBool:
			w.boolean(f.getBool(d))
		case decisionFieldInt:
			w.u32(f.getInt(d))
		}
	}
}

// EncodeAuditArchive serializes one complete audit export into savable
// bytes. org is the organization name, records is its complete chain from
// sequence 1 (a valid prefix ending at some historical sequence is enough,
// provided cp pins exactly that sequence), and cp is the checkpoint the
// caller separately retains. An organization with no records archives too:
// records is empty and cp is its sequence-0 genesis checkpoint.
//
// The whole material is validated exactly as VerifyAudit would before any
// bytes are returned: a missing organization is ErrMissingOrganization, and
// a missing, out-of-order or foreign record, modified content or checkpoint
// mismatch is ErrInvalidRange. On failure no archive bytes are returned.
// The checkpoint is carried in the archive for reference, but never stands
// in for the separately retained evidence: DecodeAuditArchive always
// validates against the checkpoint handed to it. The call is read-only:
// neither the records nor any service state are mutated and no audit record
// is appended.
func EncodeAuditArchive(org string, records []AuditRecord, cp Checkpoint) ([]byte, error) {
	if org == "" {
		return nil, ErrMissingOrganization
	}
	if err := VerifyAudit(org, records, cp); err != nil {
		return nil, err
	}

	pw := &archiveWriter{}
	pw.stringField(org)
	pw.u32(cp.EndSeq)
	pw.stringField(cp.Fingerprint)
	pw.u32(len(records))
	for i := range records {
		rec := &records[i]
		// The outer record fields walk the single record field table, so
		// the archive's record layout is the fingerprint envelope's layout
		// — plus the record's own fingerprint, which the envelope excludes
		// — by construction, and no outer field can be skipped or
		// reordered here alone.
		for j := range recordFields {
			f := &recordFields[j]
			switch f.kind {
			case recordFieldString:
				pw.stringField(f.getString(rec))
			case recordFieldInt:
				pw.u32(f.getInt(rec))
			case recordFieldChange:
				pw.change(f.getChange(rec))
			case recordFieldDecision:
				pw.decisionRecord(f.getDecision(rec))
			}
		}
	}
	if pw.err != nil {
		return nil, pw.err
	}
	payload := pw.buf

	out := make([]byte, 0, len(archiveMagic)+4+len(payload)+sha256.Size)
	out = append(out, archiveMagic...)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	out = append(out, lenBuf[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	out = append(out, sum[:]...)
	return out, nil
}

// archiveReader walks one payload without copying string contents: string
// fields are backed by the payload slice itself, which is owned by the
// decoder and never returned, so callers cannot mutate the input through
// the result.
type archiveReader struct {
	data []byte
	pos  int
	err  error
}

func (r *archiveReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if n < 0 || r.pos > len(r.data)-n {
		r.err = fmt.Errorf("%w: truncated archive payload", ErrInvalidArchive)
		return false
	}
	return true
}

func (r *archiveReader) marker() byte {
	if !r.need(1) {
		return 0
	}
	b := r.data[r.pos]
	r.pos++
	return b
}

func (r *archiveReader) u32() int {
	if !r.need(4) {
		return 0
	}
	v := int(binary.BigEndian.Uint32(r.data[r.pos : r.pos+4]))
	r.pos += 4
	return v
}

// boundedCount reads a count and proves at least minBytesPerItem bytes
// remain per item, so a hostile payload cannot drive a huge allocation
// with a count its body could never satisfy.
func (r *archiveReader) boundedCount(minBytesPerItem int) int {
	n := r.u32()
	if r.err != nil {
		return 0
	}
	if n < 0 || uint64(n)*uint64(minBytesPerItem) > uint64(len(r.data)-r.pos) {
		r.err = fmt.Errorf("%w: implausible list count %d", ErrInvalidArchive, n)
		return 0
	}
	return n
}

func (r *archiveReader) boolean() bool {
	switch r.marker() {
	case archiveTagBoolTrue:
		return true
	case archiveTagBoolFalse:
		return false
	default:
		if r.err == nil {
			r.err = fmt.Errorf("%w: invalid boolean marker", ErrInvalidArchive)
		}
		return false
	}
}

func (r *archiveReader) stringField() string {
	n := r.u32()
	if r.err != nil {
		return ""
	}
	if !r.need(n) {
		return ""
	}
	// Conversion copies the bytes, so the result never aliases the input.
	s := string(r.data[r.pos : r.pos+n])
	r.pos += n
	return s
}

// readList reads one length-counted list framed by writeListShape on the
// archive sink, whatever its item type: both string lists and the policy
// list decode here. Every list
// kind gets the exact same framing checks — the nil/presence marker, the
// per-item byte floor before any allocation, and each item's own
// truncation/structure errors — so a hostile count can neither drive a
// disproportionate allocation nor deliver a partial list: once an item
// fails, the reader's sticky error fails the whole read.
// minBytesPerItem is the minimum an item could occupy (each string item
// carries one length word; a policy item carries six length words and a
// bool), and is used only to reject counts the remaining bytes could never
// satisfy.
func readList[T any](r *archiveReader, minBytesPerItem int, readItem func() T) []T {
	switch r.marker() {
	case archiveTagNil:
		return nil
	case archiveTagPresent:
	default:
		if r.err == nil {
			r.err = fmt.Errorf("%w: invalid list marker", ErrInvalidArchive)
		}
		return nil
	}
	n := r.boundedCount(minBytesPerItem)
	if r.err != nil {
		return nil
	}
	items := make([]T, n)
	for i := range items {
		items[i] = readItem()
	}
	return items
}

func (r *archiveReader) policy() Policy {
	// Read fields in the single table's (write and fingerprint) order. The
	// fingerprint-optional ResourceID is still a mandatory, fixed field on
	// the archive, so it is read for every policy including the empty one.
	var p Policy
	for i := range policyFields {
		f := &policyFields[i]
		switch f.kind {
		case policyFieldString, policyFieldOptionalString:
			f.setString(&p, r.stringField())
		case policyFieldBool:
			f.setBool(&p, r.boolean())
		}
	}
	return p
}

func (r *archiveReader) change() *PolicyChange {
	switch r.marker() {
	case archiveTagNil:
		return nil
	case archiveTagPresent:
	default:
		if r.err == nil {
			r.err = fmt.Errorf("%w: invalid change payload marker", ErrInvalidArchive)
		}
		return nil
	}
	// Read fields in the single table's (write and fingerprint) order, so a
	// field can never be restored into the wrong member or skipped.
	c := &PolicyChange{}
	for i := range changeFields {
		f := &changeFields[i]
		switch f.kind {
		case changeFieldInt:
			f.setInt(c, r.u32())
		case changeFieldPolicyList:
			// A policy writes six length words (five strings plus
			// ResourceID) and one bool byte at minimum.
			f.setPolicies(c, readList(r, 6*4+1, r.policy))
		case changeFieldBool:
			f.setBool(c, r.boolean())
		}
	}
	return c
}

func (r *archiveReader) decisionRecord() *DecisionRecord {
	switch r.marker() {
	case archiveTagNil:
		return nil
	case archiveTagPresent:
	default:
		if r.err == nil {
			r.err = fmt.Errorf("%w: invalid decision payload marker", ErrInvalidArchive)
		}
		return nil
	}
	// Read fields in the single table's (write and fingerprint) order, so a
	// field can never be restored into the wrong member or skipped.
	d := &DecisionRecord{}
	for i := range decisionFields {
		f := &decisionFields[i]
		switch f.kind {
		case decisionFieldString:
			f.setString(d, r.stringField())
		case decisionFieldStringList:
			// Each string item carries at least one length word.
			f.setList(d, readList(r, 4, r.stringField))
		case decisionFieldBool:
			f.setBool(d, r.boolean())
		case decisionFieldInt:
			f.setInt(d, r.u32())
		}
	}
	return d
}

// VerifiedAuditArchive is one archive whose complete content has already
// been decoded AND chain-validated against the checkpoint the caller
// independently retained. It exists so the offline review command performs
// the whole-material VerifyAudit check exactly once: the decode proves the
// chain, and the subsequent decision review reuses that verdict instead of
// re-walking the same records.
//
// The type carries no "valid" flag to toggle. A value is obtained only
// from DecodeVerifiedAuditArchive, which returns nothing on failure, so a
// value that exists has been validated for exactly its Org/Checkpoint
// against exactly its records. It is not a cache keyed by archive bytes:
// changing the material or swapping the checkpoint means decoding again,
// and a previously obtained value can never certify different bytes. The
// records it holds are kept unexported precisely so code outside this
// package cannot mutate already validated material and then reuse the old
// verdict — different bytes are only reachable through a fresh,
// re-validating decode. The records are detached from the archive bytes
// and the returned decisions are detached from them.
type VerifiedAuditArchive struct {
	// Org is the organization the archive validated as belonging to.
	Org string
	// Checkpoint is the independently retained checkpoint the records were
	// validated against (the same one passed to the decode).
	Checkpoint Checkpoint
	// records is the complete, chain-validated export; only the decoder can
	// populate it and only the review method reads it.
	records []AuditRecord
}

// RecheckDecisionOffline reviews one decision in an already validated
// archive without a second whole-chain validation. It is the
// decode-then-review continuation of DecodeVerifiedAuditArchive: because
// the receiver exists only as the result of a successful validation for
// its own org and checkpoint, the material is trusted for THIS call and
// only the target decision is replayed (see recheckValidated). The result
// is read-only and detached from the receiver exactly like the standalone
// RecheckDecisionOffline function, and every error distinction —
// ErrAuditNotFound, ErrAuditNotADecision and ErrVersionNotFound — is
// unchanged.
func (a *VerifiedAuditArchive) RecheckDecisionOffline(seq int) (OfflineDecisionReview, error) {
	return recheckValidated(a.Org, a.records, seq)
}

// DecodeVerifiedAuditArchive reads one archive produced by
// EncodeAuditArchive, validates its complete content, and on success
// returns a VerifiedAuditArchive whose records can be handed straight to
// its RecheckDecisionOffline method without re-validating. org is the
// organization the caller expects, and cp is the checkpoint the caller
// independently retained; the checkpoint embedded in the file is never
// treated as evidence, so even an archive carrying a plausible embedded
// checkpoint only validates against the one supplied here.
//
// A missing organization is ErrMissingOrganization. Unrecognized framing,
// truncation, a trailing second archive or any other extra byte, a bad
// checksum, and undecodable content are ErrInvalidArchive. Records that
// decode but are missing, out of order, foreign to the organization,
// modified (including a record after the one the caller intends to review)
// or mismatched with the supplied checkpoint fail the whole read with
// ErrInvalidRange. Failure returns no archive. A successful decode only
// proves the material matches the checkpoint: an original decision that
// disagrees with a recomputed one is reported later by offline review, not
// treated as archive damage. The result is detached from archive (mutating
// one never reaches the other), and the call is read-only.
func DecodeVerifiedAuditArchive(archive []byte, org string, cp Checkpoint) (*VerifiedAuditArchive, error) {
	if org == "" {
		return nil, ErrMissingOrganization
	}
	magicLen := len(archiveMagic)
	if len(archive) < magicLen+4+sha256.Size || string(archive[:magicLen]) != archiveMagic {
		return nil, fmt.Errorf("%w: unrecognized audit archive", ErrInvalidArchive)
	}
	pos := magicLen
	payloadLen := int(binary.BigEndian.Uint32(archive[pos : pos+4]))
	pos += 4
	checksumEnd := pos + payloadLen
	if payloadLen < 0 || checksumEnd+sha256.Size != len(archive) {
		// A short frame is truncation; bytes beyond the one frame (a
		// concatenated second archive, junk appended to a good file) are a
		// framing failure too.
		return nil, fmt.Errorf("%w: truncated or trailing audit archive bytes", ErrInvalidArchive)
	}
	payload := archive[pos:checksumEnd]
	sum := sha256.Sum256(payload)
	if !bytes.Equal(sum[:], archive[checksumEnd:checksumEnd+sha256.Size]) {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrInvalidArchive)
	}

	r := &archiveReader{data: payload}
	embeddedOrg := r.stringField()
	embeddedEndSeq := r.u32()
	embeddedFingerprint := r.stringField()
	// Every record writes two payload markers, a sequence word, two
	// fingerprint length words and org/kind length words: 22 bytes even
	// when every string is empty.
	n := r.boundedCount(22)
	if r.err != nil {
		return nil, r.err
	}
	records := make([]AuditRecord, 0, n)
	for i := 0; i < n; i++ {
		// Read the outer fields in the single record field table's (write
		// and fingerprint) order, so a field can never be restored into
		// the wrong member or skipped.
		var rec AuditRecord
		for j := range recordFields {
			f := &recordFields[j]
			switch f.kind {
			case recordFieldString:
				f.setString(&rec, r.stringField())
			case recordFieldInt:
				f.setInt(&rec, r.u32())
			case recordFieldChange:
				f.setChange(&rec, r.change())
			case recordFieldDecision:
				f.setDecision(&rec, r.decisionRecord())
			}
		}
		records = append(records, rec)
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.pos != len(payload) {
		return nil, fmt.Errorf("%w: %d unconsumed archive bytes", ErrInvalidArchive, len(payload)-r.pos)
	}

	// Content checks reuse the exact audit-range validation the live export
	// and offline review paths use, so an archive can never smuggle in a
	// shape those entry points would reject. This is the single
	// whole-material validation of the decode-then-review path: the
	// returned VerifiedAuditArchive carries the verdict forward and its
	// decision review never walks the chain a second time.
	if embeddedOrg != org {
		return nil, fmt.Errorf("%w: archive organization %q does not match %q", ErrInvalidRange, embeddedOrg, org)
	}
	// The embedded checkpoint is informational; still, contradicting its own
	// header means the payload was altered, and the authoritative check
	// below always uses the caller's independently retained checkpoint.
	if embeddedEndSeq != cp.EndSeq || embeddedFingerprint != cp.Fingerprint {
		return nil, fmt.Errorf("%w: embedded checkpoint does not match the retained checkpoint", ErrInvalidRange)
	}
	if err := VerifyAudit(org, records, cp); err != nil {
		return nil, err
	}
	return &VerifiedAuditArchive{Org: org, Checkpoint: cp, records: records}, nil
}

// DecodeAuditArchive reads one archive produced by EncodeAuditArchive and
// returns records that can be handed directly to the standalone
// RecheckDecisionOffline function. org is the organization the caller
// expects, and cp is the checkpoint the caller independently retained; the
// checkpoint embedded in the file is never treated as evidence, so even an
// archive carrying a plausible embedded checkpoint only validates against
// the one supplied here.
//
// A missing organization is ErrMissingOrganization. Unrecognized framing,
// truncation, a trailing second archive or any other extra byte, a bad
// checksum, and undecodable content are ErrInvalidArchive. Records that
// decode but are missing, out of order, foreign to the organization,
// modified (including a record after the one the caller intends to review)
// or mismatched with the supplied checkpoint fail the whole read with
// ErrInvalidRange. Failure returns no records. A successful read only
// proves the material matches the checkpoint: an original decision that
// disagrees with a recomputed one is reported later by offline review, not
// treated as archive damage. The result is detached from archive (mutating
// one never reaches the other), and the call is read-only.
//
// This is the raw-material form of the decoder: it hands back records the
// standalone RecheckDecisionOffline validates again itself, so a caller
// that obtained the records through any route still gets the full check on
// every call. Code that decodes an archive solely to review one of its
// decisions should instead use DecodeVerifiedAuditArchive and its
// RecheckDecisionOffline method, which validate the whole material exactly
// once rather than verifying the chain on both sides.
func DecodeAuditArchive(archive []byte, org string, cp Checkpoint) ([]AuditRecord, error) {
	validated, err := DecodeVerifiedAuditArchive(archive, org, cp)
	if err != nil {
		return nil, err
	}
	return validated.records, nil
}
