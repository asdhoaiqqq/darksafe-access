package darksafe

import (
	"errors"
	"fmt"
	"strconv"
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
// Numbers carry no text, but their grammar is still judged as they are
// scanned: a malformed number (1e, 1E+, 0., 01) is a JSON format error at
// exactly that position, so the walk stops there and defers instead of
// skipping past it and letting a later string's text problem — or a later
// duplicate member — mask the earlier format error. Legal numbers beyond
// float64 range (1e400, a tiny 1e-400, a huge integer literal) are
// well-formed and pass; whether a number is acceptable for a given field
// is business validation's call, and the duplicate check confirms the
// document with json.Valid before trusting a completed walk.
//
// The walk is iterative: entered containers live on an explicit frame
// stack, never on the reading goroutine's call stack, and nesting is
// capped at maxJSONNestingDepth. A document that opens an 10001st level is
// refused at that opener, no matter whether the opener is in a known field
// or an ignored extra one. Location strings are assembled from the frames
// only when a verdict actually needs one, so merely accepting a document
// near the depth limit cannot spend quadratic memory on paths.

// errUndecided is the internal sentinel meaning "this document is not
// structurally recognizable here": the check stops and the encoding/json
// walk reports the format error instead. It is never returned to callers.
var errUndecided = errors.New("darksafe: unrecognized JSON structure")

// maxJSONNestingDepth is the only nesting depth a release configuration
// document may have. The outermost value counts as level 1 and every
// entered object or array adds one, whether the two kinds are mixed or
// not; siblings at the same level do not add up. Brackets that appear
// inside a string literal — written directly or decoded from an escape
// such as a backslash-u-007b spelling — are text, not structure, and
// are never counted.
// The limit matches the one encoding/json enforces, so a document at
// exactly 10000 levels still parses while the 10001st opener is refused
// everywhere.
const maxJSONNestingDepth = 10000

// errJSONDepthExceeded is the fixed, depth-independent verdict for a
// document whose structure opens more than maxJSONNestingDepth levels.
// It never carries a path: a deeper document must not produce a longer
// message. It is a real error (unlike errUndecided) and is returned to
// callers of the JSON parsing entry point.
var errJSONDepthExceeded = errors.New(
	"JSON 嵌套深度超限: 允许的最大嵌套深度为 10000 层（最外层对象或数组为第 1 层），文档已超过该上限")

// Frame states for the container on top of the explicit stack.
const (
	frameExpectKey   = iota // object: next token must be a member name
	frameExpectColon        // object: member name read, ':' next
	frameExpectValue        // object: ':' read, member value next
	frameObjectAfter        // object: member value read, ',' or '}' next
	frameArrayValue         // array: element at the frame's index next
	frameArrayAfter         // array: element read, ',' or ']' next
)

// walkFrame is one entered object or array on the walker's stack. loc is
// this container's own path segment relative to its parent (".name",
// "[\"name\"]" or "[i]"); the full location is joined from locs only when
// a verdict needs it, so the stack never holds quadratic-length paths.
type walkFrame struct {
	id    uint64 // unique per entered container over one walk
	kind  byte   // '{' or '['
	loc   string // segment from the parent container to this one
	state int
	index int    // real zero-based index of the next/current array element
	key   string // decoded name of the current object member
}

// jsonWalker reads one JSON value over the raw document bytes. It sees the
// text before any decoding, so corrupt bytes and escapes are still
// visible. Instead of recursive descent it keeps an explicit frame stack:
// a maliciously deep document can consume at most maxJSONNestingDepth
// frames and is refused at the next opener, never the reading goroutine's
// stack. The path strings follow the convention documented on
// duplicateMemberError: dot notation for identifier-style member names,
// brackets around a JSON string for every other name, and the real
// zero-based index for array items.
type jsonWalker struct {
	data []byte
	pos  int
	// onMember, when set, is called for every object member in document
	// order with the owning object's unique frame id and the decoded member
	// name. A non-nil result aborts the walk and is propagated to the
	// caller; framePathByID renders the owner's location if it needs one.
	onMember func(ownerID uint64, name string) error

	frames []walkFrame
	depth  int
	nextID uint64
	// stringPath renders the JSONPath-style location of the string literal
	// currently being scanned. It is evaluated only when string scanning
	// produces a text error, so legal strings never build the full path.
	stringPath func() string
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

// walk reads one complete JSON value from the start of the document,
// driven by an explicit frame stack rather than Go call frames. Scalars
// are consumed in place; an opener pushes a frame (after enforcing the
// nesting limit) and its matching closer pops it. The observable verdicts
// are exactly the recursive reader's: errUndecided for anything the
// encoding/json walk should judge, text errors from string scanning, the
// onMember abort, or errJSONDepthExceeded at the 10001st container level.
func (w *jsonWalker) walk() error {
	w.frames = w.frames[:0]
	w.depth = 0
	w.nextID = 0

	// readValue reads one value reached by segment loc from the current
	// top frame ("" for the document root). A scalar or an empty
	// container is consumed in place; a non-empty container pushes a
	// frame and the main loop continues. The walk is over exactly when
	// no frame remains afterwards (a root scalar or root empty
	// container); a value read inside a container always leaves its
	// parent frame on the stack.
	readValue := func(loc string) error {
		w.skipSpace()
		if w.pos >= len(w.data) {
			return errUndecided
		}
		switch c := w.data[w.pos]; {
		case c == '{' || c == '[':
			w.pos++ // consume opener
			w.depth++
			if w.depth > maxJSONNestingDepth {
				return errJSONDepthExceeded
			}
			closer := byte('}')
			state := frameExpectKey
			if c == '[' {
				closer = ']'
				state = frameArrayValue
			}
			w.skipSpace()
			if w.pos < len(w.data) && w.data[w.pos] == closer {
				// An empty container opens and closes the level without
				// pushing a frame; its parent (if any) moves straight to
				// the after-value state.
				w.pos++
				w.depth--
				return nil
			}
			w.nextID++
			w.frames = append(w.frames, walkFrame{
				id: w.nextID, kind: c, loc: loc, state: state,
			})
			return nil
		case c == '"':
			w.setValueStringPath(loc)
			_, err := w.jsonString(false)
			return err
		case c == 't':
			return w.literal("true")
		case c == 'f':
			return w.literal("false")
		case c == 'n':
			return w.literal("null")
		case c == '-' || ('0' <= c && c <= '9'):
			// Numbers carry no text, but a malformed number is a JSON format
			// error at exactly this position: judge the grammar here and stop
			// on it (deferring the report to the decoder) rather than skip
			// past it and blame a later string or duplicate.
			return w.number()
		default:
			return errUndecided
		}
	}

	for {
		if len(w.frames) == 0 {
			if err := readValue(""); err != nil {
				return err
			}
			if len(w.frames) == 0 {
				return nil
			}
			continue
		}
		parent := len(w.frames) - 1
		top := &w.frames[parent]
		switch top.state {
		case frameExpectKey:
			w.skipSpace()
			if w.pos >= len(w.data) || w.data[w.pos] != '"' {
				return errUndecided
			}
			owner := parent
			w.stringPath = func() string { return w.framePath(owner) }
			key, err := w.jsonString(true)
			if err != nil {
				return err
			}
			if w.onMember != nil {
				if err := w.onMember(top.id, key); err != nil {
					return err
				}
			}
			top.key = key
			top.state = frameExpectColon
		case frameExpectColon:
			w.skipSpace()
			if w.pos >= len(w.data) || w.data[w.pos] != ':' {
				return errUndecided
			}
			w.pos++
			top.state = frameExpectValue
		case frameExpectValue:
			if err := readValue(memberSegment(top.key)); err != nil {
				return err
			}
			// The after-value state is set even when a non-empty child
			// container pushed a frame: when that child pops, the parent
			// resumes here.
			w.frames[parent].state = frameObjectAfter
		case frameObjectAfter:
			w.skipSpace()
			if w.pos >= len(w.data) {
				return errUndecided
			}
			switch w.data[w.pos] {
			case ',':
				w.pos++
				top.state = frameExpectKey
			case '}':
				w.pos++
				w.depth--
				w.frames = w.frames[:parent]
				if len(w.frames) == 0 {
					return nil
				}
			default:
				return errUndecided
			}
		case frameArrayValue:
			if err := readValue(indexSegment(top.index)); err != nil {
				return err
			}
			w.frames[parent].state = frameArrayAfter
		case frameArrayAfter:
			w.skipSpace()
			if w.pos >= len(w.data) {
				return errUndecided
			}
			switch w.data[w.pos] {
			case ',':
				w.pos++
				top.index++
				top.state = frameArrayValue
			case ']':
				w.pos++
				w.depth--
				w.frames = w.frames[:parent]
				if len(w.frames) == 0 {
					return nil
				}
			default:
				return errUndecided
			}
		}
	}
}

// setValueStringPath prepares the lazy location of a string value reached
// by loc from the current top frame: the root value is at "$", an object
// member value at the owner's path plus its member segment, and an array
// element at the owner's path plus its index segment.
func (w *jsonWalker) setValueStringPath(loc string) {
	if len(w.frames) == 0 {
		w.stringPath = func() string { return "$" }
		return
	}
	owner := len(w.frames) - 1
	w.stringPath = func() string { return w.framePath(owner) + loc }
}

// framePath joins the location of the frame at stack index fi from its
// segments: "$" followed by every entered container's own loc. It is
// called only to fill in an error location, so an accepted deep document
// never pays for these strings.
func (w *jsonWalker) framePath(fi int) string {
	var b strings.Builder
	b.WriteByte('$')
	for i := 0; i <= fi; i++ {
		b.WriteString(w.frames[i].loc)
	}
	return b.String()
}

// framePathByID renders the current location of an entered container by
// its unique frame id, for a callback that needs to report its owner.
func (w *jsonWalker) framePathByID(id uint64) string {
	for i := range w.frames {
		if w.frames[i].id == id {
			return w.framePath(i)
		}
	}
	return "$"
}

func (w *jsonWalker) literal(word string) error {
	if len(w.data)-w.pos < len(word) || string(w.data[w.pos:w.pos+len(word)]) != word {
		return errUndecided
	}
	w.pos += len(word)
	return nil
}

// number scans one JSON number literal, judging its grammar exactly as the
// decoder would: an optional minus, an integer part with no leading zeros,
// an optional fraction with at least one digit, and an optional exponent
// with at least one digit. A malformed number — a missing exponent digit
// (1e, 1E+), a missing fraction (0.), an illegal leading zero (01) — is a
// JSON format error, so the scan stops with errUndecided and leaves the
// report to the encoding/json walk, at the number's own position in the
// document rather than after some later string. Legal numbers beyond
// float64 range (1e400, 1e-400, huge integer literals) are well-formed
// here; whether one is acceptable for a given field is decided later by
// business validation.
func (w *jsonWalker) number() error {
	if w.pos < len(w.data) && w.data[w.pos] == '-' {
		w.pos++
	}
	if w.pos >= len(w.data) {
		return errUndecided
	}
	switch c := w.data[w.pos]; {
	case c == '0':
		w.pos++
		if w.pos < len(w.data) && isJSONDigit(w.data[w.pos]) {
			return errUndecided // illegal leading zero
		}
	case '1' <= c && c <= '9':
		for w.pos < len(w.data) && isJSONDigit(w.data[w.pos]) {
			w.pos++
		}
	default:
		return errUndecided
	}
	if w.pos < len(w.data) && w.data[w.pos] == '.' {
		w.pos++
		if w.pos >= len(w.data) || !isJSONDigit(w.data[w.pos]) {
			return errUndecided // the fraction needs at least one digit
		}
		for w.pos < len(w.data) && isJSONDigit(w.data[w.pos]) {
			w.pos++
		}
	}
	if w.pos < len(w.data) && (w.data[w.pos] == 'e' || w.data[w.pos] == 'E') {
		w.pos++
		if w.pos < len(w.data) && (w.data[w.pos] == '+' || w.data[w.pos] == '-') {
			w.pos++
		}
		if w.pos >= len(w.data) || !isJSONDigit(w.data[w.pos]) {
			return errUndecided // the exponent needs at least one digit
		}
		for w.pos < len(w.data) && isJSONDigit(w.data[w.pos]) {
			w.pos++
		}
	}
	return nil
}

func isJSONDigit(c byte) bool { return '0' <= c && c <= '9' }

// jsonString reads one string literal whose opening quote is at w.pos,
// validating every byte and escape, and returns the decoded contents. The
// decoded form is needed for member names so that object paths are built —
// and duplicates compared — from the same decoded names the business
// validation sees. w.stringPath supplies the location lazily, evaluated
// only when a text error needs it.
func (w *jsonWalker) jsonString(isKey bool) (string, error) {
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
			if err := w.escape(&sb, isKey); err != nil {
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
				return "", &invalidUTF8Error{path: w.stringPath(), isKey: isKey}
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
func (w *jsonWalker) escape(sb *strings.Builder, isKey bool) error {
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
		r, err := w.unicodeEscape(isKey)
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
func (w *jsonWalker) unicodeEscape(isKey bool) (rune, error) {
	r, ok := w.hex4()
	if !ok {
		return 0, errUndecided // malformed hex: the decoder reports it
	}
	if !utf16.IsSurrogate(r) {
		return r, nil
	}
	unpaired := &unpairedSurrogateError{
		escape: fmt.Sprintf(`\u%04X`, uint16(r)),
		path:   w.stringPath(),
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

// memberSegment renders how one object member extends its owner's location:
// dot notation for a simple identifier name, otherwise brackets around a
// JSON-encoded string.
func memberSegment(key string) string {
	if isSimpleMemberName(key) {
		return "." + key
	}
	return "[" + jsonEncodePathString(key) + "]"
}

// indexSegment renders how one array item extends its owner's location,
// using the real zero-based index ("[0]").
func indexSegment(index int) string {
	return "[" + strconv.Itoa(index) + "]"
}

// joinIndexPath extends an array location with one item's real zero-based
// index ("$.clusters[0]").
func joinIndexPath(path string, index int) string {
	return path + indexSegment(index)
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
	return path + memberSegment(key)
}

// jsonEncodePathString renders key as a legal JSON string (including the
// surrounding quotes); decoding the result restores key exactly, including
// empty names and names containing quotes, backslashes, newlines or other
// control characters.
//
// Every code point with a control effect is written as an escape, never as a
// raw character: the JSON short escapes keep their established spellings
// (", \, \b, \f, \n, \r, \t), U+2028/U+2029 stay their \u2028/\u2029 spellings, and —
// unlike encoding/json, which leaves them in place — DEL (U+007F) and every
// C1 control character (U+0080–U+009F) are written as \uXXXX as well, so a
// location printed to a terminal can neither act on the reader nor hide a
// name fragment. The \uXXXX spelling is a real JSON escape: it decodes back
// to the control character and is distinct from the ordinary text of a
// backslash followed by "u007f". No \xNN form is ever produced, since JSON
// cannot decode one. The key is a Go string built from decoded member names,
// so it is always valid UTF-8; rune-range iteration therefore visits exactly
// its code points.
func jsonEncodePathString(key string) string {
	var b strings.Builder
	b.Grow(len(key) + 2)
	b.WriteByte('"')
	for _, r := range key {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r == '<' || r == '>' || r == '&':
				// Preserve encoding/json's HTML-safe spelling so the segment
				// stays identical to the old rendering.
				fallthrough
			case r < 0x20, r == 0x7f, 0x80 <= r && r <= 0x9f,
				r == 0x2028, r == 0x2029:
				// Every code point with a control effect is written as a real
				// \uXXXX JSON escape — including DEL and the C1 controls,
				// which encoding/json leaves raw — so the location can neither
				// act on the terminal nor hide part of a member name. The
				// spelling decodes back to the character itself and is
				// distinct from ordinary text containing "\u007f"; no
				// JSON-illegal \xNN form is ever emitted.
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
