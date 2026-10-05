package darksafe

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// This file holds the single structural reading of a release configuration
// document, shared by the two checks that run before business validation:
// the strict text check (stricttext.go) and the duplicate-member check
// (plan.go). Both checks need exactly the same structural rules — how
// objects, arrays and literals nest, how a string literal is read and
// decoded, and how the JSONPath-style location of each position is built —
// so those rules live here once, in jsonWalker. Each check is then only its
// own policy layered on the walk: the text check rejects corrupt strings,
// the duplicate check rejects repeated member names within one object.
//
// The walker reads the raw document bytes, before any decoding layer can
// rewrite corrupt text to the replacement character "�" (U+FFFD), and
// decodes every string itself. Member names are reported to the checks in
// the same decoded form that business validation later compares, so a name
// written literally and the same name written with \uXXXX escapes are one
// name everywhere.
//
// The walker only recognizes structure. Anything structurally unexpected
// makes it stop (errUndecided) and defer to the encoding/json walk in
// plan.go, which reports the JSON format error with the established
// wording — so malformed documents keep their existing error messages.
// Numbers are scanned loosely (their characters are skipped, not judged):
// whether a number is well-formed is the decoder's call, and the duplicate
// check confirms the document with json.Valid before trusting a completed
// walk.

// errUndecided is the internal sentinel meaning "this document is not
// structurally recognizable here": the check stops and the encoding/json
// walk reports the format error instead. It is never returned to callers.
var errUndecided = errors.New("darksafe: unrecognized JSON structure")

// jsonWalker is a recursive-descent reader over the raw document bytes. It
// sees the text before any decoding, so corrupt bytes and escapes are still
// visible, and it tracks the JSONPath-style location of every position as it
// descends. The path strings follow the convention documented on
// duplicateMemberError: dot notation for identifier-style member names,
// brackets around a JSON string for every other name, and the real
// zero-based index for array items.
type jsonWalker struct {
	data []byte
	pos  int
	// onMember, when set, is called for every object member in document
	// order with the owning object's location and the decoded member name.
	// A non-nil result aborts the walk and is propagated to the caller.
	onMember func(ownerPath, name string) error
}

func (w *jsonWalker) skipSpace() {
	for w.pos < len(w.data) {
		switch w.data[w.pos] {
		case ' ', '\t', '\n', '\r':
			w.pos++
		default:
			return
		}
	}
}

// value reads one complete JSON value at the current position.
func (w *jsonWalker) value(path string) error {
	w.skipSpace()
	if w.pos >= len(w.data) {
		return errUndecided
	}
	switch c := w.data[w.pos]; {
	case c == '{':
		return w.object(path)
	case c == '[':
		return w.array(path)
	case c == '"':
		_, err := w.jsonString(path, false)
		return err
	case c == 't':
		return w.literal("true")
	case c == 'f':
		return w.literal("false")
	case c == 'n':
		return w.literal("null")
	case c == '-' || ('0' <= c && c <= '9'):
		// Numbers carry no text; skip their characters loosely and let the
		// decoder judge whether the number itself is well-formed.
		w.number()
		return nil
	default:
		return errUndecided
	}
}

func (w *jsonWalker) object(path string) error {
	w.pos++ // consume '{'
	w.skipSpace()
	if w.pos < len(w.data) && w.data[w.pos] == '}' {
		w.pos++
		return nil
	}
	for {
		w.skipSpace()
		if w.pos >= len(w.data) || w.data[w.pos] != '"' {
			return errUndecided
		}
		key, err := w.jsonString(path, true)
		if err != nil {
			return err
		}
		if w.onMember != nil {
			if err := w.onMember(path, key); err != nil {
				return err
			}
		}
		w.skipSpace()
		if w.pos >= len(w.data) || w.data[w.pos] != ':' {
			return errUndecided
		}
		w.pos++
		if err := w.value(joinMemberPath(path, key)); err != nil {
			return err
		}
		w.skipSpace()
		if w.pos >= len(w.data) {
			return errUndecided
		}
		switch w.data[w.pos] {
		case ',':
			w.pos++
		case '}':
			w.pos++
			return nil
		default:
			return errUndecided
		}
	}
}

func (w *jsonWalker) array(path string) error {
	w.pos++ // consume '['
	w.skipSpace()
	if w.pos < len(w.data) && w.data[w.pos] == ']' {
		w.pos++
		return nil
	}
	for i := 0; ; i++ {
		if err := w.value(joinIndexPath(path, i)); err != nil {
			return err
		}
		w.skipSpace()
		if w.pos >= len(w.data) {
			return errUndecided
		}
		switch w.data[w.pos] {
		case ',':
			w.pos++
		case ']':
			w.pos++
			return nil
		default:
			return errUndecided
		}
	}
}

func (w *jsonWalker) literal(word string) error {
	if len(w.data)-w.pos < len(word) || string(w.data[w.pos:w.pos+len(word)]) != word {
		return errUndecided
	}
	w.pos += len(word)
	return nil
}

func (w *jsonWalker) number() {
	for w.pos < len(w.data) {
		switch c := w.data[w.pos]; {
		case c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' || ('0' <= c && c <= '9'):
			w.pos++
		default:
			return
		}
	}
}

// jsonString reads one string literal whose opening quote is at w.pos,
// validating every byte and escape, and returns the decoded contents. The
// decoded form is needed for member names so that object paths are built —
// and duplicates compared — from the same decoded names the business
// validation sees.
func (w *jsonWalker) jsonString(path string, isKey bool) (string, error) {
	w.pos++ // consume the opening quote
	rawStart := w.pos
	var sb strings.Builder
	decoded := false // whether sb holds the decoded prefix
	flush := func(upto int) {
		if !decoded {
			sb.Write(w.data[rawStart:upto])
			decoded = true
		}
	}
	for {
		if w.pos >= len(w.data) {
			return "", errUndecided // unterminated: the decoder reports it
		}
		c := w.data[w.pos]
		switch {
		case c == '"':
			w.pos++
			if !decoded {
				return string(w.data[rawStart : w.pos-1]), nil
			}
			return sb.String(), nil
		case c == '\\':
			flush(w.pos)
			w.pos++
			if err := w.escape(&sb, path, isKey); err != nil {
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
			w.pos++
		default:
			r, size := utf8.DecodeRune(w.data[w.pos:])
			// RuneError with size 1 means the bytes are not valid UTF-8; a
			// literally written "�" decodes with size 3 and is legal text.
			if r == utf8.RuneError && size == 1 {
				return "", &invalidUTF8Error{path: path, isKey: isKey}
			}
			if decoded {
				sb.Write(w.data[w.pos : w.pos+size])
			}
			w.pos += size
		}
	}
}

// escape consumes one escape sequence (the backslash is already consumed)
// and writes the decoded character to sb.
func (w *jsonWalker) escape(sb *strings.Builder, path string, isKey bool) error {
	if w.pos >= len(w.data) {
		return errUndecided
	}
	e := w.data[w.pos]
	w.pos++
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
		r, err := w.unicodeEscape(path, isKey)
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
func (w *jsonWalker) unicodeEscape(path string, isKey bool) (rune, error) {
	r, ok := w.hex4()
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
	if w.pos+1 < len(w.data) && w.data[w.pos] == '\\' && w.data[w.pos+1] == 'u' {
		w.pos += 2
		lo, ok := w.hex4()
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
func (w *jsonWalker) hex4() (rune, bool) {
	if w.pos+4 > len(w.data) {
		return 0, false
	}
	var v rune
	for i := 0; i < 4; i++ {
		c := w.data[w.pos+i]
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
	w.pos += 4
	return v, true
}

// joinIndexPath extends an array location with one item's real zero-based
// index ("$.clusters[0]").
func joinIndexPath(path string, index int) string {
	return fmt.Sprintf("%s[%d]", path, index)
}

// isSimpleMemberName reports whether key may be written with dot notation:
// it must start with an ASCII letter or underscore and contain only ASCII
// letters, digits, and underscores afterward.
func isSimpleMemberName(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z'):
		case i > 0 && ('0' <= r && r <= '9'):
		default:
			return false
		}
	}
	return true
}

// joinMemberPath extends an object location with one member name. Simple
// identifier names keep the existing dot spelling ("$.clusters[0].tags");
// every other name is rendered as brackets around a JSON-encoded string, so
// the text decodes back to exactly the real member name and none of its
// characters can be mistaken for a path separator or array index.
func joinMemberPath(path, key string) string {
	if isSimpleMemberName(key) {
		return path + "." + key
	}
	return path + "[" + jsonEncodePathString(key) + "]"
}

// jsonEncodePathString renders key as a legal JSON string (including the
// surrounding quotes); decoding the result restores key exactly, including
// empty names and names containing quotes, backslashes, newlines or other
// control characters.
func jsonEncodePathString(key string) string {
	bs, err := json.Marshal(key)
	if err != nil {
		// json.Marshal cannot fail for a Go string.
		panic(fmt.Sprintf("json.Marshal(%q): %v", key, err))
	}
	return string(bs)
}
