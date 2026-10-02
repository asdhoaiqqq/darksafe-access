// This file implements the canonical, byte-preserving representation that
// audit fingerprints are computed over.
//
// Fingerprints used to be taken over encoding/json's output. That loses
// information: json.Marshal replaces every invalid UTF-8 byte with the
// Unicode replacement character, so two records whose strings differ only
// in raw invalid bytes (e.g. a single 0xFF versus 0xFE) produced identical
// fingerprints and VerifyAudit accepted the tampered material.
//
// The encoder below is byte-identical to json.Marshal for every string that
// is valid UTF-8, so fingerprints of ordinary content are unchanged and
// existing exports and checkpoints remain valid. Strings containing invalid
// bytes are encoded with a per-byte escape (\xNN) that no legal JSON string
// can produce, so different raw content always fingerprints differently even
// though JSON would render both as the same replacement character.
package darksafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"
)

const auditHexDigits = "0123456789abcdef"

// auditHashEncoder writes the canonical representation of an audit envelope.
type auditHashEncoder struct {
	buf bytes.Buffer
}

func (e *auditHashEncoder) writeRaw(s string) {
	e.buf.WriteString(s)
}

func (e *auditHashEncoder) writeByte(b byte) {
	e.buf.WriteByte(b)
}

func (e *auditHashEncoder) writeString(s string) {
	e.buf.Write(encodeAuditString(s))
}

func (e *auditHashEncoder) writeInt(v int) {
	e.buf.WriteString(strconv.Itoa(v))
}

func (e *auditHashEncoder) writeBool(v bool) {
	if v {
		e.buf.WriteString("true")
	} else {
		e.buf.WriteString("false")
	}
}

// writeStringSlice writes nil as null and a non-nil slice as a JSON array.
func (e *auditHashEncoder) writeStringSlice(v []string) {
	if v == nil {
		e.buf.WriteString("null")
		return
	}
	e.buf.WriteByte('[')
	for i, s := range v {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		e.writeString(s)
	}
	e.buf.WriteByte(']')
}

// writePolicies writes nil as null and a non-nil slice as a JSON array.
func (e *auditHashEncoder) writePolicies(v []Policy) {
	if v == nil {
		e.buf.WriteString("null")
		return
	}
	e.buf.WriteByte('[')
	for i := range v {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		e.writePolicy(v[i])
	}
	e.buf.WriteByte(']')
}

func (e *auditHashEncoder) writePolicy(p Policy) {
	e.writeRaw(`{"ID":`)
	e.writeString(p.ID)
	e.writeRaw(`,"Subject":`)
	e.writeString(p.Subject)
	e.writeRaw(`,"Action":`)
	e.writeString(p.Action)
	e.writeRaw(`,"Scope":`)
	e.writeString(p.Scope)
	e.writeRaw(`,"Effect":`)
	e.writeString(string(p.Effect))
	e.writeRaw(`,"Recursive":`)
	e.writeBool(p.Recursive)
	e.writeByte('}')
}

func (e *auditHashEncoder) writeChange(c *PolicyChange) {
	e.writeRaw(`{"Version":`)
	e.writeInt(c.Version)
	e.writeRaw(`,"Policies":`)
	e.writePolicies(c.Policies)
	e.writeRaw(`,"SourceVersion":`)
	e.writeInt(c.SourceVersion)
	e.writeRaw(`,"RolledBack":`)
	e.writeBool(c.RolledBack)
	e.writeByte('}')
}

func (e *auditHashEncoder) writeSubject(s Subject) {
	e.writeRaw(`{"ID":`)
	e.writeString(s.ID)
	e.writeRaw(`,"Kind":`)
	e.writeString(s.Kind)
	e.writeRaw(`,"Roles":`)
	e.writeStringSlice(s.Roles)
	e.writeRaw(`,"Disabled":`)
	e.writeBool(s.Disabled)
	e.writeByte('}')
}

func (e *auditHashEncoder) writeResource(r Resource) {
	e.writeRaw(`{"ID":`)
	e.writeString(r.ID)
	e.writeRaw(`,"Scope":`)
	e.writeString(r.Scope)
	e.writeByte('}')
}

func (e *auditHashEncoder) writeRequest(r OrgRequest) {
	e.writeRaw(`{"SubjectOrg":`)
	e.writeString(r.SubjectOrg)
	e.writeRaw(`,"ResourceOrg":`)
	e.writeString(r.ResourceOrg)
	e.writeRaw(`,"Subject":`)
	e.writeSubject(r.Subject)
	e.writeRaw(`,"Resource":`)
	e.writeResource(r.Resource)
	e.writeRaw(`,"Action":`)
	e.writeString(r.Action)
	e.writeByte('}')
}

func (e *auditHashEncoder) writeDecision(d Decision) {
	e.writeRaw(`{"Allowed":`)
	e.writeBool(d.Allowed)
	e.writeRaw(`,"Reason":`)
	e.writeString(d.Reason)
	e.writeRaw(`,"Matched":`)
	e.writeStringSlice(d.Matched)
	e.writeRaw(`,"Version":`)
	e.writeInt(d.Version)
	e.writeByte('}')
}

func (e *auditHashEncoder) writeDecisionRecord(d *DecisionRecord) {
	e.writeRaw(`{"Request":`)
	e.writeRequest(d.Request)
	e.writeRaw(`,"Decision":`)
	e.writeDecision(d.Decision)
	e.writeByte('}')
}

// writeEnvelope writes the envelope in the exact struct field order and with
// the same omitempty behavior as encoding/json.
func (e *auditHashEncoder) writeEnvelope(env *hashEnvelope) {
	e.writeRaw(`{"org":`)
	e.writeString(env.Org)
	e.writeRaw(`,"seq":`)
	e.writeInt(env.Seq)
	e.writeRaw(`,"kind":`)
	e.writeString(env.Kind)
	if env.Change != nil {
		e.writeRaw(`,"change":`)
		e.writeChange(env.Change)
	}
	if env.Decision != nil {
		e.writeRaw(`,"decision":`)
		e.writeDecisionRecord(env.Decision)
	}
	e.writeRaw(`,"prev":`)
	e.writeString(env.PrevFingerprint)
	e.writeByte('}')
}

// encodeAuditString returns the canonical encoding of s for fingerprinting.
// Valid UTF-8 strings are encoded exactly as json.Marshal encodes them, so
// fingerprints of ordinary content are unchanged. Strings containing invalid
// bytes are encoded with a distinctive per-byte escape: \x is never a valid
// JSON escape, so the result cannot collide with the encoding of any legal
// string, and the real U+FFFD character passes through as raw UTF-8 rather
// than matching an invalid byte.
func encodeAuditString(s string) []byte {
	if utf8.ValidString(s) {
		b, err := json.Marshal(s)
		if err != nil {
			// A Go string always marshals; failure here is a programming
			// error, not user input.
			panic(fmt.Errorf("darksafe: audit string must encode: %w", err))
		}
		return b
	}
	var buf bytes.Buffer
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			// ASCII: escape exactly as encoding/json does.
			switch b {
			case '"', '\\':
				buf.WriteByte('\\')
				buf.WriteByte(b)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			case '\b':
				buf.WriteString(`\b`)
			case '\f':
				buf.WriteString(`\f`)
			default:
				if b < 0x20 || b == '<' || b == '>' || b == '&' {
					buf.WriteString(`\u00`)
					buf.WriteByte(auditHexDigits[b>>4])
					buf.WriteByte(auditHexDigits[b&0xF])
				} else {
					buf.WriteByte(b)
				}
			}
			i++
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			// Preserve the raw invalid byte distinctly.
			buf.WriteString(`\x`)
			buf.WriteByte(auditHexDigits[b>>4])
			buf.WriteByte(auditHexDigits[b&0xF])
			i++
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			buf.WriteString(`\u202`)
			buf.WriteByte(auditHexDigits[c&0xF])
			i += size
			continue
		}
		buf.WriteString(s[i : i+size])
		i += size
	}
	buf.WriteByte('"')
	return buf.Bytes()
}
