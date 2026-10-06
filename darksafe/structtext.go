package darksafe

import (
	"fmt"
	"unicode/utf8"
)

// This file extends the strict-text rule (see stricttext.go for the JSON
// entry) to a release configuration constructed directly in Go memory and
// handed to ValidateReleaseInput or MakeReleasePlan.
//
// A Go string can hold bytes that are not valid UTF-8. Without this check
// such bytes would survive into planning unchanged at first, but two
// distinct corrupt identities would be indistinguishable the moment the
// plan is written as JSON — encoding/json rewrites every invalid rune to
// the replacement character "�" (U+FFFD) — and corrupt tag keys or values
// would silently take part in label matching and fault-domain batching. The
// check therefore runs as the first stage of ValidateReleaseInput, before
// any business rule and hence before selection or batching: the config is
// rejected outright rather than producing a plan that looks successful.
//
// Legal text is used exactly as written — including Chinese, supplementary
// plane characters such as "😀", and a literally written "�" — and ordinary
// text that merely contains a backslash followed by "uD800" is not invalid
// UTF-8 (only the JSON entry deals with \uXXXX escapes at all).
//
// When several strings are invalid, exactly one deterministic problem is
// reported. Scalars are checked in their fixed field order; candidates and
// conditions at their real zero-based positions (disabled clusters and
// clusters that filtering would drop are checked just the same); within one
// tags object or condition the entries are examined in ascending key order,
// so map iteration order can never change the error. Cluster tags and
// include/exclude conditions are all checked by the one shared label-set
// rule (checkStrictLabelText); only the error wording differs per object. A
// value error names the key it belongs to; an invalid key names the tags
// object or condition object owning it.

// invalidConfigTextError reports a string in a directly constructed config
// that contains bytes which are not valid UTF-8. where is a field-and-
// position description in the same style as the business-validation errors:
// a top-level field for scalars, clusters[i] (plus the offending tag key or
// the owning tags object) for candidate data, and include[i]/exclude[i]
// (plus the condition key or owning condition object) for conditions.
type invalidConfigTextError struct {
	where string
}

func (e *invalidConfigTextError) Error() string {
	return fmt.Sprintf("内存配置包含无效 UTF-8 字节: %s", e.where)
}

// validateStrictText rejects invalid UTF-8 anywhere in a Go-constructed
// config, in a fixed order independent of map iteration: app, revision,
// image, spreadBy, then every candidate (ID before its tags, tags in
// ascending key order), then every include and exclude condition in list
// order (entries likewise in ascending key order). The input is only read.
func validateStrictText(in ReleasePlanInput) error {
	if !utf8.ValidString(in.App) {
		return &invalidConfigTextError{where: `字段 "app"`}
	}
	if !utf8.ValidString(in.Revision) {
		return &invalidConfigTextError{where: `字段 "revision"`}
	}
	if !utf8.ValidString(in.Image) {
		return &invalidConfigTextError{where: `字段 "image"`}
	}
	if !utf8.ValidString(in.SpreadBy) {
		return &invalidConfigTextError{where: `字段 "spreadBy"`}
	}
	for i, c := range in.Clusters {
		if !utf8.ValidString(c.ID) {
			return &invalidConfigTextError{where: fmt.Sprintf(`clusters[%d] 的字段 "id"`, i)}
		}
		if err := validateStrictTagText(i, c.Tags); err != nil {
			return err
		}
	}
	if err := validateStrictConditionText("include", in.Include); err != nil {
		return err
	}
	if err := validateStrictConditionText("exclude", in.Exclude); err != nil {
		return err
	}
	return nil
}

// checkStrictLabelText is the one UTF-8 rule every label set in a Go-built
// config is held to — a candidate's tags and each include/exclude condition
// alike, so the same text is legal or illegal everywhere. Entries are
// examined in ascending key order; for each entry the key is checked before
// its value. The first invalid string is reported through keyErr (invalid
// key) or valueErr (invalid value), which format the error for the object
// owning the set — the same division of shared rule and per-context wording
// as labelsFromJSON. The set is only read.
func checkStrictLabelText(labels map[string]string, keyErr, valueErr func(key string) error) error {
	for _, k := range sortedLabelKeys(labels) {
		if !utf8.ValidString(k) {
			return keyErr(k)
		}
		if !utf8.ValidString(labels[k]) {
			return valueErr(k)
		}
	}
	return nil
}

// validateStrictTagText checks one candidate's tags at its zero-based
// candidate position. An invalid value is reported with the (valid) key it
// belongs to; an invalid key is quoted byte-for-byte (so the corrupt bytes
// are shown as Go-escaped octets rather than rewritten to "�") and reported
// as belonging to that candidate's tags object.
func validateStrictTagText(clusterIndex int, tags map[string]string) error {
	return checkStrictLabelText(tags,
		func(k string) error {
			return &invalidConfigTextError{
				where: fmt.Sprintf("clusters[%d] 的标签映射包含非法键 %q", clusterIndex, k),
			}
		},
		func(k string) error {
			return &invalidConfigTextError{
				where: fmt.Sprintf("clusters[%d] 的标签键 %q 对应的值", clusterIndex, k),
			}
		},
	)
}

// validateStrictConditionText checks every condition of one field
// ("include" or "exclude") at its zero-based list position, applying the
// shared label-set text rule to each.
func validateStrictConditionText(field string, conds []LabelCondition) error {
	for i, cond := range conds {
		if err := checkStrictLabelText(cond,
			func(k string) error {
				return &invalidConfigTextError{
					where: fmt.Sprintf("%s[%d] 的条件对象包含非法键 %q", field, i, k),
				}
			},
			func(k string) error {
				return &invalidConfigTextError{
					where: fmt.Sprintf("%s[%d] 的条件键 %q 对应的值", field, i, k),
				}
			},
		); err != nil {
			return err
		}
	}
	return nil
}
