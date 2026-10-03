package darksafe

import (
	"fmt"
	"sort"
)

// This file holds the label rules shared by the two ways a release
// configuration can be validated: parsing a JSON document (ParseReleaseInput)
// and validating a directly constructed Go config (ValidateReleaseInput).
//
// The rules themselves: label keys must be non-empty; label values are matched
// exactly as written — an empty string is a valid value and is never trimmed,
// dropped, or case-folded; a condition object must hold at least one entry.
// When one label set or condition has several problems, the first one in
// ascending key order is reported, so repeated calls give the same error.
// Each path keeps its own error wording (cluster identity vs. list position)
// by formatting errors at the call site.

// sortedLabelKeys returns the keys of a label set in ascending order, so both
// validation paths examine keys — and therefore report problems — in the same
// deterministic order.
func sortedLabelKeys[V any](labels map[string]V) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hasEmptyLabelKey reports whether the label set contains an empty key. An
// empty key sorts before any other key, so it is always the first problem
// found when keys are examined in ascending order.
func hasEmptyLabelKey[V any](labels map[string]V) bool {
	for _, k := range sortedLabelKeys(labels) {
		if k == "" {
			return true
		}
	}
	return false
}

// validateCondition enforces the shared rules for one include/exclude
// condition: it must contain at least one entry and no empty label keys. The
// error names the field ("include" or "exclude") and the condition's position,
// the wording both the JSON and the Go-config paths already use.
func validateCondition(field string, index int, cond LabelCondition) error {
	if len(cond) == 0 {
		return fmt.Errorf("%s[%d] 不能为空条件对象", field, index)
	}
	if hasEmptyLabelKey(cond) {
		return fmt.Errorf("%s[%d] 的标签键不能为空", field, index)
	}
	return nil
}

// labelsFromJSON validates a decoded JSON object against the shared label
// rules and returns it as a label set: keys must be non-empty and every value
// must be a string, preserved exactly as written. Keys are examined in
// ascending order and the first problem is reported through keyErr (empty key)
// or valueErr (non-string value), which format the error for the caller's
// context — cluster tags or an include/exclude condition.
func labelsFromJSON(obj map[string]any, keyErr, valueErr func(key string) error) (map[string]string, error) {
	labels := make(map[string]string, len(obj))
	for _, k := range sortedLabelKeys(obj) {
		if k == "" {
			return nil, keyErr(k)
		}
		sv, ok := obj[k].(string)
		if !ok {
			return nil, valueErr(k)
		}
		labels[k] = sv
	}
	return labels, nil
}
