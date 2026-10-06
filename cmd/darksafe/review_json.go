// The JSON renderer for `darksafe review --json`. It serializes the very
// same OfflineDecisionReview the text report prints: the same archive
// validation and historical recheck feed both, so choosing JSON can never
// change the consistency verdict.
//
// String representation rule (part of the command contract):
//
//   - A string that is valid UTF-8 is emitted as an ordinary JSON string.
//     encoding/json performs the mandatory escaping only: quotation marks,
//     backslashes and control characters become escape sequences, while
//     ordinary Chinese and a genuine U+FFFD stay readable UTF-8.
//   - A string that is NOT valid UTF-8 is emitted as the object
//     {"base64": "<RFC 4648 standard base64>"} carrying the standard
//     base64 encoding of the string's exact bytes. The two shapes are
//     disjoint (a JSON string can never be an object), so a receiver can
//     recover every string's original bytes, and a lone 0xFF, a lone 0xFE
//     and a real U+FFFD stay distinguishable instead of collapsing onto
//     one replacement character.
package main

import (
	"encoding/base64"
	"encoding/json"
	"unicode/utf8"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

// jsonReview is the top-level --json object. Field order mirrors the text
// report: target sequence first, then the original decision, the
// recomputed decision, and the consistency verdict. The target sequence is
// a separate field from either decision's policy_version.
type jsonReview struct {
	TargetSequence int          `json:"target_sequence"`
	Original       jsonDecision `json:"original"`
	Recomputed     jsonDecision `json:"recomputed"`
	Consistent     bool         `json:"consistent"`
}

// jsonDecision mirrors one text-report decision block.
type jsonDecision struct {
	Allowed         bool  `json:"allowed"`
	Reason          any   `json:"reason"`
	MatchedPolicies []any `json:"matched_policies"`
	PolicyVersion   int   `json:"policy_version"`
}

// renderReviewJSON builds the complete JSON object for one review. It is a
// pure function of the review result: runReview calls it only after every
// validation has succeeded and writes its output in one piece, so a failed
// review can never leave a partial object on stdout.
func renderReviewJSON(r darksafe.OfflineDecisionReview) ([]byte, error) {
	out := jsonReview{
		TargetSequence: r.Seq,
		Original:       toJSONDecision(r.Original),
		Recomputed:     toJSONDecision(r.Recomputed),
		Consistent:     r.Consistent,
	}
	// Indentation keeps the single object human-readable; it is still one
	// complete JSON object with no surrounding text.
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// toJSONDecision converts one decision. Matched policy identifiers keep
// their stored order, and a nil matched list renders the same [] as an
// empty list: the list is always allocated non-nil, matching the review's
// own nil-equals-empty consistency rule.
func toJSONDecision(d darksafe.Decision) jsonDecision {
	matched := make([]any, 0, len(d.Matched))
	for _, id := range d.Matched {
		matched = append(matched, jsonStringValue(id))
	}
	return jsonDecision{
		Allowed:         d.Allowed,
		Reason:          jsonStringValue(d.Reason),
		MatchedPolicies: matched,
		PolicyVersion:   d.Version,
	}
}

// jsonStringValue maps one raw Go string to its JSON value: the string
// itself when it is valid UTF-8, otherwise a {"base64": ...} object over
// its exact bytes.
func jsonStringValue(s string) any {
	if utf8.ValidString(s) {
		return s
	}
	return map[string]string{"base64": base64.StdEncoding.EncodeToString([]byte(s))}
}
