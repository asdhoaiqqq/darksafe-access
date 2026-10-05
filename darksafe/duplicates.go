package darksafe

import (
	"errors"
	"fmt"
)

// This file holds the duplicate-member check: every JSON object in a release
// configuration — known fields, unknown extra fields and their nested
// objects, however deep — must have distinct member names. encoding/json
// would silently keep only the last value of a repeated name, so two
// different documents could decode to the same configuration; the check
// rejects such objects before any business validation runs.
//
// Names are compared after JSON decoding, so a name written literally and
// the same name written with \uXXXX escapes are one name, while case or
// spacing differences stay distinct. The seen-set is per object: the same
// name in two different objects — two clusters' tags, a parent object and
// its nested object — never collides.
//
// The check itself is one line of orchestration: the structural walk it runs
// on — objects, arrays, string decoding, location tracking — is the shared
// document walk in walk.go, also used by the strict text check. The walk
// delivers each decoded member name with its owning object's location; this
// file only supplies the per-object seen-set and the error. A document the
// walk cannot structurally follow is handed to the encoding/json walk, which
// reports the JSON format error with the established wording — or a duplicate
// that still precedes the structural problem.

// duplicateMemberError reports a JSON object whose member name appears more
// than once. Path is a JSONPath-style location that points at the object
// owning the duplicate (the duplicate member name itself is reported
// separately in field): "$" for the document root, "$.clusters[1]" for a
// cluster, "$.clusters[0].tags" for a tag object,
// "$.include[2]" / "$.exclude[1]" for a condition, and "$.foo.bar[0]" for
// objects nested inside unknown fields. A member name that is not a plain
// identifier is wrapped in brackets around a JSON string, so that characters
// in the name can never be read as hierarchy or an array index: a top-level
// member "meta.info" is at $["meta.info"] (distinct from the nested object
// $.meta.info), "zone[0]" stays one whole name at $["zone[0]"], and names
// that are empty or contain quotes, backslashes or control characters are
// rendered as decodable JSON strings such as $[""] or $["a\nb"].
type duplicateMemberError struct {
	field string
	path  string
}

func (e *duplicateMemberError) Error() string {
	return fmt.Sprintf("JSON 对象存在重复成员: 字段 %q 重复出现于 %s", e.field, e.path)
}

// memberTracker is the memberListener that applies the duplicate rule: each
// open object gets its own seen-set on a stack, and a name already present in
// the innermost set is a duplicate at that object's location.
type memberTracker struct {
	stack []map[string]struct{}
}

func (t *memberTracker) openObject(string) {
	t.stack = append(t.stack, make(map[string]struct{}))
}

func (t *memberTracker) member(path, name string) error {
	seen := t.stack[len(t.stack)-1]
	if _, dup := seen[name]; dup {
		return &duplicateMemberError{field: name, path: path}
	}
	seen[name] = struct{}{}
	return nil
}

func (t *memberTracker) closeObject() {
	t.stack = t.stack[:len(t.stack)-1]
}

// checkDuplicateMembers walks every JSON object in data and rejects objects
// with duplicate member names. Names are compared after JSON decoding, so
// "env" and "env" are the same name while "env" and "ENV", or
// " env" and "env", are different. The first duplicate encountered in file
// order is reported: the walk is depth-first in document order, so the
// second occurrence that appears earliest wins, regardless of value type.
func checkDuplicateMembers(data []byte) error {
	w := &docWalker{data: data, listener: &memberTracker{}}
	err := w.walkValue()
	if err == nil && !w.atEnd() {
		// Content after the top-level value is a JSON format error, worded
		// by the encoding/json walk.
		err = errUndecided
	}
	if errors.Is(err, errUndecided) {
		return walkDecoderFormat(data)
	}
	return err
}
