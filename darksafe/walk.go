package darksafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// This file holds the single structural walk over a raw release-configuration
// document shared by the two checks that run before business decoding: the
// strict text check (stricttext.go) and the duplicate-member check
// (duplicates.go). Keeping the structure rules — what an object, an array or
// a scalar is, how member names decode, and how a location is spelled — in
// this one walk means the two checks never drift apart on nesting or path
// tracking; each check only supplies its own finding.
//
// docWalker is a recursive-descent reader over the raw document bytes. Unlike
// the token walks further below it sees the text before encoding/json can
// rewrite anything, so corrupt bytes and escapes are still visible, and it
// decodes every string literal itself — member names and values alike, in
// known fields and in unknown extra fields, however deeply nested — producing
// the same decoded form the decoder would. Every position is tracked as a
// JSONPath-style location.
//
// The walk only recognizes structure; it never judges business fields. Two
// kinds of findings interrupt it:
//
//   - a text problem (invalid UTF-8 bytes or an unpaired-surrogate \uXXXX
//     escape), reported with the walk's current location;
//   - errUndecided, when the document is not structurally recognizable. The
//     walk never words JSON format errors itself: a document it cannot
//     follow is handed to the encoding/json walk (walkDecoderFormat), which
//     reports the format error with the established wording.
//
// Location strings follow one convention for both checks: "$" for the
// document root, dot notation for identifier-style member names
// ("$.clusters[0].tags"), the real zero-based index for array items, and
// brackets around a JSON-encoded string for any other member name, so
// characters in a name can never be read as hierarchy or an array index.

// errUndecided is the internal sentinel meaning "this document is not
// structurally recognizable here": the walk stops and the encoding/json walk
// reports the format error instead. It is never returned to callers.
var errUndecided = errors.New("darksafe: unrecognized JSON structure")

// memberListener receives the object events of a document walk: an object
// opening, each of its decoded member names (with the object's location), and
// its closing. The strict text check needs no events — its findings come from
// string decoding itself — and walks with a nil listener; the duplicate-member
// check tracks names per object (duplicates.go).
type memberListener interface {
	openObject(path string)
	member(path, name string) error
	closeObject()
}

// docWalker is a recursive-descent reader over the raw document bytes,
// tracking the JSONPath-style location of every value it visits.
type docWalker struct {
	data     []byte
	pos      int
	listener memberListener
}

// walkValue walks the single JSON value at the start of the document.
func (w *docWalker) walkValue() error {
	return w.value("$")
}

// atEnd reports whether nothing but whitespace follows the walked value.
func (w *docWalker) atEnd() bool {
	w.skipSpace()
	return w.pos >= len(w.data)
}

func (w *docWalker) skipSpace() {
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
func (w *docWalker) value(path string) error {
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

func (w *docWalker) object(path string) error {
	w.pos++ // consume '{'
	if w.listener != nil {
		w.listener.openObject(path)
	}
	w.skipSpace()
	if w.pos < len(w.data) && w.data[w.pos] == '}' {
		w.pos++
		w.closeObject()
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
		if w.listener != nil {
			if err := w.listener.member(path, key); err != nil {
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
			w.closeObject()
			return nil
		default:
			return errUndecided
		}
	}
}

func (w *docWalker) closeObject() {
	if w.listener != nil {
		w.listener.closeObject()
	}
}

func (w *docWalker) array(path string) error {
	w.pos++ // consume '['
	w.skipSpace()
	if w.pos < len(w.data) && w.data[w.pos] == ']' {
		w.pos++
		return nil
	}
	for i := 0; ; i++ {
		if err := w.value(fmt.Sprintf("%s[%d]", path, i)); err != nil {
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

func (w *docWalker) literal(word string) error {
	if len(w.data)-w.pos < len(word) || string(w.data[w.pos:w.pos+len(word)]) != word {
		return errUndecided
	}
	w.pos += len(word)
	return nil
}

func (w *docWalker) number() {
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
// and duplicates compared — from the same decoded names the decoder produces.
func (w *docWalker) jsonString(path string, isKey bool) (string, error) {
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
func (w *docWalker) escape(sb *strings.Builder, path string, isKey bool) error {
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
func (w *docWalker) unicodeEscape(path string, isKey bool) (rune, error) {
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
func (w *docWalker) hex4() (rune, bool) {
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

// walkDecoderFormat walks data with encoding/json and reports the first
// structural problem with the established JSON format error wording. The raw
// docWalker defers to it for any document it cannot follow (errUndecided), so
// malformed documents keep the messages they have always produced — including
// a duplicate member that appears before the structural problem. Numbers are
// only walked past, never interpreted: UseNumber keeps legal JSON numbers
// beyond float64 range (e.g. 1e400) from being turned into an
// unmarshal-overflow error here. Whether a number is acceptable for a given
// field is decided later by business validation.
func walkDecoderFormat(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walkJSONValue(dec, "$"); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return errors.New("JSON 格式错误: 文档包含多余内容")
		}
		return fmt.Errorf("JSON 格式错误: %w", err)
	}
	return nil
}

// walkJSONValue reads one complete JSON value whose first token has not been
// consumed yet.
func walkJSONValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return errors.New("JSON 格式错误: 文档为空")
		}
		return fmt.Errorf("JSON 格式错误: %w", err)
	}
	return walkJSONValueToken(dec, path, tok)
}

// walkJSONValueToken reads the remainder of a value given its first token.
func walkJSONValueToken(dec *json.Decoder, path string, tok json.Token) error {
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			return walkJSONObject(dec, path)
		case '[':
			return walkJSONArray(dec, path)
		default:
			return fmt.Errorf("JSON 格式错误: 意外的分隔符 %q", d)
		}
	}
	return nil // scalar value (string, number, bool, null)
}

func walkJSONObject(dec *json.Decoder, path string) error {
	seen := make(map[string]struct{})
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("JSON 格式错误: 对象未闭合")
			}
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '}' {
				return nil
			}
			return fmt.Errorf("JSON 格式错误: 意外的分隔符 %q", d)
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("JSON 格式错误: 对象成员名必须是字符串")
		}
		if _, dup := seen[key]; dup {
			return &duplicateMemberError{field: key, path: path}
		}
		seen[key] = struct{}{}
		vtok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("JSON 格式错误: 缺少成员值")
			}
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		if err := walkJSONValueToken(dec, joinMemberPath(path, key), vtok); err != nil {
			return err
		}
	}
}

func walkJSONArray(dec *json.Decoder, path string) error {
	for i := 0; ; i++ {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("JSON 格式错误: 数组未闭合")
			}
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return nil
		}
		if err := walkJSONValueToken(dec, fmt.Sprintf("%s[%d]", path, i), tok); err != nil {
			return err
		}
	}
}
