package darksafe

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// This file holds the strict text check shared by every JSON string in a
// release configuration: member names and string values alike, in known
// fields and in unknown extra fields, however deeply nested.
//
// Go's encoding/json silently rewrites two kinds of corrupt text to the
// replacement character "�" (U+FFFD): invalid UTF-8 bytes inside a string,
// and \uXXXX escapes naming an unpaired surrogate (a high surrogate not
// immediately followed by a low-surrogate escape, or a low surrogate with no
// preceding high surrogate). Decoding through encoding/json alone would
// therefore turn two different original documents into the same cluster ID
// or tag value. This check rejects such documents before any decoded string
// is compared, so the rewrite can never feed identity comparison, label
// matching, or fault-domain batching.
//
// Legal text is untouched: valid UTF-8 (including a literally written "�",
// which is a legal character), \uXXXX escapes for BMP characters, and
// high+low surrogate pairs for non-BMP characters all decode to their
// intended strings. A backslash-escaped "D800" is ordinary text, not an
// escape sequence, and is accepted as such.
//
// The scanner only recognizes text problems. Anything structurally
// unexpected makes it stop (errUndecided) and defer to the encoding/json
// walks that follow, which report the JSON format error with the established
// wording — so malformed documents keep their existing error messages.

// invalidUTF8Error reports a JSON string or member name containing bytes
// that are not valid UTF-8. For a member name, path points at the object
// owning the name; for a value, at the field or array item holding it.
type invalidUTF8Error struct {
	path  string
	isKey bool
}

func (e *invalidUTF8Error) Error() string {
	if e.isKey {
		return fmt.Sprintf("JSON 成员名包含无效 UTF-8 字节: 所属对象位置 %s", e.path)
	}
	return fmt.Sprintf("JSON 字符串包含无效 UTF-8 字节: 位置 %s", e.path)
}

// unpairedSurrogateError reports a \uXXXX escape naming one half of a
// surrogate pair without its mate: a high surrogate not immediately followed
// by a low-surrogate escape, or a low surrogate appearing on its own.
// escape is the offending escape spelling (e.g. `D800`); path and isKey
// locate it exactly as for invalidUTF8Error.
type unpairedSurrogateError struct {
	escape string
	path   string
	isKey  bool
}

func (e *unpairedSurrogateError) Error() string {
	if e.isKey {
		return fmt.Sprintf("JSON 成员名包含未配对的 Unicode 转义 %s: 所属对象位置 %s", e.escape, e.path)
	}
	return fmt.Sprintf("JSON 字符串包含未配对的 Unicode 转义 %s: 位置 %s", e.escape, e.path)
}

// errUndecided is the internal sentinel meaning "this document is not
// structurally recognizable here": the check stops and the encoding/json
// walks report the format error instead. It is never returned to callers.
var errUndecided = errors.New("darksafe: unrecognized JSON structure")

// checkStrictText scans the raw document and rejects any string — member
// name or value, anywhere in the structure — containing invalid UTF-8 bytes
// or an unpaired-surrogate \uXXXX escape. It runs before duplicate-member
// detection and business validation, so no rewritten string can be compared
// or planned with. A document it cannot structurally follow is left to the
// later encoding/json walks, which report the format error.
func checkStrictText(data []byte) error {
	s := &strictTextScanner{data: data}
	if err := s.value("$"); err != nil && !errors.Is(err, errUndecided) {
		return err
	}
	return nil
}

// strictTextScanner is a recursive-descent reader over the raw document
// bytes. Unlike the token walks in plan.go it sees the text before any
// decoding, so corrupt bytes and escapes are still visible. Its path strings
// follow the same JSONPath-style convention as duplicateMemberError.
type strictTextScanner struct {
	data []byte
	pos  int
}

func (s *strictTextScanner) skipSpace() {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

// value reads one complete JSON value at the current position.
func (s *strictTextScanner) value(path string) error {
	s.skipSpace()
	if s.pos >= len(s.data) {
		return errUndecided
	}
	switch c := s.data[s.pos]; {
	case c == '{':
		return s.object(path)
	case c == '[':
		return s.array(path)
	case c == '"':
		_, err := s.jsonString(path, false)
		return err
	case c == 't':
		return s.literal("true")
	case c == 'f':
		return s.literal("false")
	case c == 'n':
		return s.literal("null")
	case c == '-' || ('0' <= c && c <= '9'):
		// Numbers carry no text; skip their characters loosely and let the
		// decoder judge whether the number itself is well-formed.
		s.number()
		return nil
	default:
		return errUndecided
	}
}

func (s *strictTextScanner) object(path string) error {
	s.pos++ // consume '{'
	s.skipSpace()
	if s.pos < len(s.data) && s.data[s.pos] == '}' {
		s.pos++
		return nil
	}
	for {
		s.skipSpace()
		if s.pos >= len(s.data) || s.data[s.pos] != '"' {
			return errUndecided
		}
		key, err := s.jsonString(path, true)
		if err != nil {
			return err
		}
		s.skipSpace()
		if s.pos >= len(s.data) || s.data[s.pos] != ':' {
			return errUndecided
		}
		s.pos++
		if err := s.value(joinMemberPath(path, key)); err != nil {
			return err
		}
		s.skipSpace()
		if s.pos >= len(s.data) {
			return errUndecided
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
		case '}':
			s.pos++
			return nil
		default:
			return errUndecided
		}
	}
}

func (s *strictTextScanner) array(path string) error {
	s.pos++ // consume '['
	s.skipSpace()
	if s.pos < len(s.data) && s.data[s.pos] == ']' {
		s.pos++
		return nil
	}
	for i := 0; ; i++ {
		if err := s.value(fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
		s.skipSpace()
		if s.pos >= len(s.data) {
			return errUndecided
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
		case ']':
			s.pos++
			return nil
		default:
			return errUndecided
		}
	}
}

func (s *strictTextScanner) literal(word string) error {
	if len(s.data)-s.pos < len(word) || string(s.data[s.pos:s.pos+len(word)]) != word {
		return errUndecided
	}
	s.pos += len(word)
	return nil
}

func (s *strictTextScanner) number() {
	for s.pos < len(s.data) {
		switch c := s.data[s.pos]; {
		case c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' || ('0' <= c && c <= '9'):
			s.pos++
		default:
			return
		}
	}
}

// jsonString reads one string literal whose opening quote is at s.pos,
// validating every byte and escape, and returns the decoded contents. The
// decoded form is needed for member names so that object paths are built
// from the same decoded names the duplicate check compares.
func (s *strictTextScanner) jsonString(path string, isKey bool) (string, error) {
	s.pos++ // consume the opening quote
	rawStart := s.pos
	var sb strings.Builder
	decoded := false // whether sb holds the decoded prefix
	flush := func(upto int) {
		if !decoded {
			sb.Write(s.data[rawStart:upto])
			decoded = true
		}
	}
	for {
		if s.pos >= len(s.data) {
			return "", errUndecided // unterminated: the decoder reports it
		}
		c := s.data[s.pos]
		switch {
		case c == '"':
			s.pos++
			if !decoded {
				return string(s.data[rawStart : s.pos-1]), nil
			}
			return sb.String(), nil
		case c == '\\':
			flush(s.pos)
			s.pos++
			if err := s.escape(&sb, path, isKey); err != nil {
				return "", err
			}
		case c < 0x20:
			// A raw control character is a JSON format error; leave the
			// report to the decoder.
			return "", errUndecided
		case c < utf8.RuneSelf:
			if decoded {
				sb.WriteByte(c)
			}
			s.pos++
		default:
			r, size := utf8.DecodeRune(s.data[s.pos:])
			// RuneError with size 1 means the bytes are not valid UTF-8; a
			// literally written "�" decodes with size 3 and is legal text.
			if r == utf8.RuneError && size == 1 {
				return "", &invalidUTF8Error{path: path, isKey: isKey}
			}
			if decoded {
				sb.Write(s.data[s.pos : s.pos+size])
			}
			s.pos += size
		}
	}
}

// escape consumes one escape sequence (the backslash is already consumed)
// and writes the decoded character to sb.
func (s *strictTextScanner) escape(sb *strings.Builder, path string, isKey bool) error {
	if s.pos >= len(s.data) {
		return errUndecided
	}
	e := s.data[s.pos]
	s.pos++
	switch e {
	case '"', '\\', '/':
		sb.WriteByte(e)
	case 'b':
		sb.WriteByte('\b')
	case 'f':
		sb.WriteByte('\f')
	case 'n':
		sb.WriteByte('\n')
	case 'r':
		sb.WriteByte('\r')
	case 't':
		sb.WriteByte('\t')
	case 'u':
		r, err := s.unicodeEscape(path, isKey)
		if err != nil {
			return err
		}
		sb.WriteRune(r)
	default:
		// An unknown escape is a JSON format error, reported by the decoder.
		return errUndecided
	}
	return nil
}

// unicodeEscape reads the four hex digits of a \uXXXX escape (the "\u" is
// already consumed) and, for a high surrogate, the low-surrogate escape that
// must immediately follow. It returns the decoded rune: the BMP character
// itself, or the non-BMP character a surrogate pair stands for.
func (s *strictTextScanner) unicodeEscape(path string, isKey bool) (rune, error) {
	r, ok := s.hex4()
	if !ok {
		return 0, errUndecided // malformed hex: the decoder reports it
	}
	if !utf16.IsSurrogate(r) {
		return r, nil
	}
	unpaired := &unpairedSurrogateError{
		escape: fmt.Sprintf(`\u%04X`, uint16(r)),
		path:   path,
		isKey:  isKey,
	}
	if r >= 0xDC00 {
		// A low surrogate with no preceding high surrogate.
		return 0, unpaired
	}
	// A high surrogate must be immediately followed by a `\uDC00`–`\uDFFF`
	// escape; anything else — end of string, a plain character, another
	// escape kind, or a non-low-surrogate \uXXXX — leaves it unpaired.
	if s.pos+1 < len(s.data) && s.data[s.pos] == '\\' && s.data[s.pos+1] == 'u' {
		s.pos += 2
		lo, ok := s.hex4()
		if !ok {
			return 0, errUndecided // malformed hex: the decoder reports it
		}
		if lo >= 0xDC00 && lo <= 0xDFFF {
			return utf16.DecodeRune(r, lo), nil
		}
	}
	return 0, unpaired
}

// hex4 reads exactly four hexadecimal digits. It reports false — consuming
// nothing — when they are absent or not hex, leaving the format error to the
// decoder.
func (s *strictTextScanner) hex4() (rune, bool) {
	if s.pos+4 > len(s.data) {
		return 0, false
	}
	var v rune
	for i := 0; i < 4; i++ {
		c := s.data[s.pos+i]
		var d byte
		switch {
		case '0' <= c && c <= '9':
			d = c - '0'
		case 'a' <= c && c <= 'f':
			d = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v*16 + rune(d)
	}
	s.pos += 4
	return v, true
}
