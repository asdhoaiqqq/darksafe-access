package darksafe

import (
	"errors"
	"fmt"
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
// The check itself is one line of orchestration: the structural walk it runs
// on — objects, arrays, string decoding, location tracking — is the shared
// document walk in walk.go, also used by the duplicate-member check. The walk
// only recognizes text problems; anything structurally unexpected makes it
// stop (errUndecided) and defer to the encoding/json walk, which reports the
// JSON format error with the established wording — so malformed documents
// keep their existing error messages.

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

// checkStrictText scans the raw document and rejects any string — member
// name or value, anywhere in the structure — containing invalid UTF-8 bytes
// or an unpaired-surrogate \uXXXX escape. It runs before duplicate-member
// detection and business validation, so no rewritten string can be compared
// or planned with. A document the walk cannot structurally follow is left to
// the encoding/json walk, which reports the format error.
func checkStrictText(data []byte) error {
	w := &docWalker{data: data}
	if err := w.walkValue(); err != nil && !errors.Is(err, errUndecided) {
		return err
	}
	return nil
}
