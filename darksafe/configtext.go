package darksafe

import (
	"fmt"
	"unicode/utf8"
)

// This file holds the strict-text rule for a release configuration handed in
// through the Go library: a ReleasePlanInput built directly in memory never
// passed through ParseReleaseInput, so the JSON walker in stricttext.go never
// saw its strings. A Go string can hold arbitrary bytes — including byte
// sequences that are not valid UTF-8 — and encoding/json turns those bytes
// into the replacement character "�" (U+FFFD) when the result is marshaled.
// Two different corrupt cluster identities would then be written as the same
// "�", and corrupt application text would be silently rewritten.
//
// validateConfigText closes that gap at the same boundary the file entry
// uses: before any business rule runs — before emptiness and duplicate
// identity checks, before label matching, and before batching. Every string
// the configuration carries is examined as written: app, revision, image and
// spreadBy; every candidate's ID and all of its tag keys and values, whether
// the candidate is disabled or would be filtered out and whether a tag takes
// part in matching; and every include/exclude condition's keys and values.
//
// Legal text passes through untouched: valid UTF-8 (Chinese, supplementary
// plane characters, and a literally written "�"), a backslash followed by
// "uD800" as ordinary characters, and empty tag values are all left exactly
// as the caller wrote them. What cannot enter through the JSON entry cannot
// enter through the library either: the WTF-8 bytes of a lone surrogate
// (ED A0 80, the bytes a Go string "\uD800" holds) are not valid UTF-8 and
// are rejected here just as the unpaired escape is rejected by the walker.
//
// The check only reads the configuration; it never repairs a string, drops a
// candidate, or mutates a map. When several strings are corrupt, exactly one
// deterministic problem is reported: fields are examined in the fixed order
// app, revision, image, spreadBy, candidates (ID before that candidate's
// tags), include conditions, exclude conditions, and the keys of one label
// set or condition are visited in ascending order — so the verdict never
// depends on map iteration order.

// validateConfigText rejects the first string holding invalid UTF-8 bytes,
// in the deterministic order documented above. The error names the field or
// the candidate/condition position (both zero-based); a bad label or
// condition value also names its key, while a bad key points at the label set
// or condition object that owns it rather than printing the corrupt key.
func validateConfigText(in ReleasePlanInput) error {
	for _, field := range []struct {
		name string
		text string
	}{
		{"app", in.App},
		{"revision", in.Revision},
		{"image", in.Image},
		{"spreadBy", in.SpreadBy},
	} {
		if !utf8.ValidString(field.text) {
			return fmt.Errorf("字段 %q 包含无效 UTF-8 字节", field.name)
		}
	}
	for i, c := range in.Clusters {
		if !utf8.ValidString(c.ID) {
			return fmt.Errorf("clusters[%d]: 字段 %q 包含无效 UTF-8 字节", i, "id")
		}
		if err := validateLabelText(fmt.Sprintf("clusters[%d]", i), c.Tags); err != nil {
			return err
		}
	}
	if err := validateConditionText("include", in.Include); err != nil {
		return err
	}
	if err := validateConditionText("exclude", in.Exclude); err != nil {
		return err
	}
	return nil
}

// validateLabelText examines one label set in ascending key order, reporting
// the first corrupt key or value. A corrupt key is located through the owning
// object (owner, e.g. "clusters[0]" or "include[2]"), since the key itself is
// not printable text; a corrupt value additionally names the valid key that
// holds it.
func validateLabelText(owner string, labels map[string]string) error {
	for _, key := range sortedLabelKeys(labels) {
		if !utf8.ValidString(key) {
			return fmt.Errorf("%s 的标签键包含无效 UTF-8 字节", owner)
		}
		if !utf8.ValidString(labels[key]) {
			return fmt.Errorf("%s 的标签 %q 值包含无效 UTF-8 字节", owner, key)
		}
	}
	return nil
}

// validateConditionText examines include/exclude conditions in their list
// order, applying the shared label-text rule to each condition object.
func validateConditionText(field string, conds []LabelCondition) error {
	for i, cond := range conds {
		if err := validateLabelText(fmt.Sprintf("%s[%d]", field, i), cond); err != nil {
			return err
		}
	}
	return nil
}
