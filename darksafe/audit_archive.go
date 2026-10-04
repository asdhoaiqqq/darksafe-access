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

// stringList preserves the slice shape explicitly: nil is one marker, a
// non-nil list is a marker, a count and the raw strings.
func (w *archiveWriter) stringList(items []string) {
	if items == nil {
		w.buf = append(w.buf, archiveTagNil)
		return
	}
	w.buf = append(w.buf, archiveTagPresent)
	w.u32(len(items))
	for _, s := range items {
		w.stringField(s)
	}
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

func (w *archiveWriter) policyList(policies []Policy) {
	if policies == nil {
		w.buf = append(w.buf, archiveTagNil)
		return
	}
	w.buf = append(w.buf, archiveTagPresent)
	w.u32(len(policies))
	for _, p := range policies {
		w.policy(p)
	}
}

// change writes the policy-change payload, or a single absence marker:
// VerifyAudit only ever accepts a change payload on a policy-change record
// and its absence otherwise, but the distinction is encoded explicitly so
// decode never has to reconstruct a shape. A present payload walks the
// single change field table, so the archive's field order is the
// fingerprint's order by construction and every field — new version, full
// policy list, source version and rollback marker — round trips.
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
			w.policyList(f.getPolicies(c))
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
			w.stringList(f.getList(d))
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
		r := &records[i]
		pw.stringField(r.Org)
		pw.u32(r.Seq)
		pw.stringField(r.Kind)
		pw.change(r.Change)
		pw.decisionRecord(r.Decision)
		pw.stringField(r.PrevFingerprint)
		pw.stringField(r.Fingerprint)
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

func (r *archiveReader) stringList() []string {
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
	n := r.boundedCount(4) // each item carries at least one length word
	if r.err != nil {
		return nil
	}
	items := make([]string, n)
	for i := range items {
		items[i] = r.stringField()
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

func (r *archiveReader) policyList() []Policy {
	switch r.marker() {
	case archiveTagNil:
		return nil
	case archiveTagPresent:
	default:
		if r.err == nil {
			r.err = fmt.Errorf("%w: invalid policy list marker", ErrInvalidArchive)
		}
		return nil
	}
	// A policy writes six length words (five strings plus ResourceID) and
	// one bool byte at minimum.
	n := r.boundedCount(6*4 + 1)
	if r.err != nil {
		return nil
	}
	policies := make([]Policy, n)
	for i := range policies {
		policies[i] = r.policy()
	}
	return policies
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
	c := &PolicyChange{}
	// Read fields in the single table's (write and fingerprint) order, so a
	// field can never be restored into the wrong member, skipped or retained
	// by one route and dropped by another.
	for i := range changeFields {
		f := &changeFields[i]
		switch f.kind {
		case changeFieldInt:
			f.setInt(c, r.u32())
		case changeFieldPolicyList:
			f.setPolicies(c, r.policyList())
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
			f.setList(d, r.stringList())
		case decisionFieldBool:
			f.setBool(d, r.boolean())
		case decisionFieldInt:
			f.setInt(d, r.u32())
		}
	}
	return d
}

// DecodeAuditArchive reads one archive produced by EncodeAuditArchive and
// returns records that can be handed directly to RecheckDecisionOffline.
// org is the organization the caller expects, and cp is the checkpoint the
// caller independently retained; the checkpoint embedded in the file is
// never treated as evidence, so even an archive carrying a plausible
// checkpoint only validates against the one supplied here.
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
func DecodeAuditArchive(archive []byte, org string, cp Checkpoint) ([]AuditRecord, error) {
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
		var rec AuditRecord
		rec.Org = r.stringField()
		rec.Seq = r.u32()
		rec.Kind = r.stringField()
		rec.Change = r.change()
		rec.Decision = r.decisionRecord()
		rec.PrevFingerprint = r.stringField()
		rec.Fingerprint = r.stringField()
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
	// shape those entry points would reject.
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
	return records, nil
}
