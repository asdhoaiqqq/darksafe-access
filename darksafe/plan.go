package darksafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Reason constants for clusters that cannot enter a release plan.
const (
	ReasonDisabled          = "集群已停用"
	ReasonExcludeMatched    = "命中排除条件"
	ReasonIncludeNotMatched = "未命中包含条件"
)

// LabelCondition is a set of exact key-value matches; all must match.
type LabelCondition map[string]string

// Cluster is a candidate target for a release.
type Cluster struct {
	ID       string
	Disabled bool
	Tags     map[string]string
}

// ReleasePlanInput describes an application release to plan offline.
type ReleasePlanInput struct {
	App       string
	Revision  string
	Image     string
	BatchSize int
	Clusters  []Cluster
	Include   []LabelCondition
	Exclude   []LabelCondition
}

// AppInfo identifies the application being released.
type AppInfo struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Image    string `json:"image"`
}

// Batch is a sequentially numbered group of cluster IDs.
type Batch struct {
	Index    int      `json:"index"`
	Clusters []string `json:"clusters"`
}

// ExcludedCluster explains why a cluster is not in the plan.
type ExcludedCluster struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// ReleasePlan is the deterministic offline release plan.
type ReleasePlan struct {
	App      AppInfo           `json:"app"`
	Batches  []Batch           `json:"batches"`
	Excluded []ExcludedCluster `json:"excluded"`
}

// ParseReleaseInput reads a release plan document and validates its fields.
// A document that defines the same member name twice in any JSON object is
// rejected before any field validation, so an ambiguous value can never be
// silently resolved by taking the last occurrence.
func ParseReleaseInput(data []byte) (ReleasePlanInput, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return ReleasePlanInput{}, fmt.Errorf("JSON 格式错误: %w", err)
	}
	if dec.More() {
		return ReleasePlanInput{}, errors.New("JSON 格式错误: 文档包含多余内容")
	}
	if err := checkDuplicateMembers(data); err != nil {
		return ReleasePlanInput{}, err
	}
	return buildReleaseInput(doc)
}

// checkDuplicateMembers scans a syntactically valid JSON document token by
// token and rejects any object that defines the same member name twice,
// regardless of the values involved. The check covers every object in the
// document: the top level, each cluster, tags, include/exclude conditions,
// and unknown fields with their nested objects. Names are compared after
// JSON string decoding, so equivalent Unicode escapes count as the same
// name while case or whitespace differences remain distinct; no trimming or
// other normalization is applied. Separate objects in an array may reuse
// names freely. The reported duplicate is the member whose second
// occurrence appears first in the document, named by its decoded key and
// the position of the object containing it (array positions are zero-based).
func checkDuplicateMembers(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return walkJSONValue(dec, "")
}

// walkJSONValue consumes one JSON value from dec. path identifies the value
// itself: "" for the top level, joined with "." through object members and
// extended with "[i]" through array elements.
func walkJSONValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("JSON 格式错误: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return fmt.Errorf("JSON 格式错误: %w", err)
			}
			key := kt.(string)
			if _, dup := seen[key]; dup {
				where := path
				if where == "" {
					where = "顶层对象"
				}
				return fmt.Errorf("配置中存在重复的对象成员: %s 中的字段 %q 重复出现", where, key)
			}
			seen[key] = struct{}{}
			child := key
			if path != "" {
				child = path + "." + key
			}
			if err := walkJSONValue(dec, child); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		return nil
	case '[':
		for i := 0; dec.More(); i++ {
			if err := walkJSONValue(dec, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		return nil
	}
	return nil
}

func buildReleaseInput(doc map[string]any) (ReleasePlanInput, error) {
	app, err := requiredString(doc, "app")
	if err != nil {
		return ReleasePlanInput{}, err
	}
	revision, err := requiredString(doc, "revision")
	if err != nil {
		return ReleasePlanInput{}, err
	}
	image, err := requiredString(doc, "image")
	if err != nil {
		return ReleasePlanInput{}, err
	}
	batchSize, err := positiveInt(doc, "batchSize")
	if err != nil {
		return ReleasePlanInput{}, err
	}
	clusters, err := parseClusters(doc["clusters"])
	if err != nil {
		return ReleasePlanInput{}, err
	}
	include, err := parseConditions(doc["include"], "include")
	if err != nil {
		return ReleasePlanInput{}, err
	}
	exclude, err := parseConditions(doc["exclude"], "exclude")
	if err != nil {
		return ReleasePlanInput{}, err
	}

	return ReleasePlanInput{
		App:       app,
		Revision:  revision,
		Image:     image,
		BatchSize: batchSize,
		Clusters:  clusters,
		Include:   include,
		Exclude:   exclude,
	}, nil
}

// ValidateReleaseInput checks that a release configuration is well-formed:
// app/revision/image are non-empty, batchSize is a positive integer, cluster
// IDs are unique across every candidate (including disabled and filtered-out
// clusters), and tag/condition keys are non-empty. Errors name the field and
// the position in its list; within a cluster the ID is checked before its
// tags, and tag keys are examined in ascending order so repeated calls report
// the same error. The input is never mutated. MakeReleasePlan calls this
// automatically; library callers may use it to validate without planning.
func ValidateReleaseInput(in ReleasePlanInput) error {
	if strings.TrimSpace(in.App) == "" {
		return errors.New(`字段 "app" 不能为空或只含空白`)
	}
	if strings.TrimSpace(in.Revision) == "" {
		return errors.New(`字段 "revision" 不能为空或只含空白`)
	}
	if strings.TrimSpace(in.Image) == "" {
		return errors.New(`字段 "image" 不能为空或只含空白`)
	}
	if in.BatchSize <= 0 {
		return errors.New(`字段 "batchSize" 必须是正整数`)
	}
	seen := make(map[string]struct{}, len(in.Clusters))
	for i, c := range in.Clusters {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("clusters[%d]: 字段 %q 不能为空或只含空白", i, "id")
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("clusters[%d]: 重复的集群标识 %q", i, c.ID)
		}
		seen[c.ID] = struct{}{}
		if err := validateTagKeys(i, c.Tags); err != nil {
			return err
		}
	}
	if err := validateConditionKeys("include", in.Include); err != nil {
		return err
	}
	if err := validateConditionKeys("exclude", in.Exclude); err != nil {
		return err
	}
	return nil
}

// validateTagKeys checks tag keys in ascending order for deterministic errors.
func validateTagKeys(clusterIndex int, tags map[string]string) error {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" {
			return fmt.Errorf("clusters[%d] 的标签键不能为空", clusterIndex)
		}
	}
	return nil
}

// validateConditionKeys checks that every condition is non-empty and has only
// non-empty keys; conditions are visited in original order and keys in
// ascending order for deterministic errors.
func validateConditionKeys(field string, conds []LabelCondition) error {
	for i, cond := range conds {
		if len(cond) == 0 {
			return fmt.Errorf("%s[%d] 不能为空条件对象", field, i)
		}
		keys := make([]string, 0, len(cond))
		for k := range cond {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "" {
				return fmt.Errorf("%s[%d] 的标签键不能为空", field, i)
			}
		}
	}
	return nil
}

func requiredString(doc map[string]any, field string) (string, error) {
	v, ok := doc[field]
	if !ok {
		return "", fmt.Errorf("缺少必需字段 %q", field)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("字段 %q 必须是字符串", field)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("字段 %q 不能为空或只含空白", field)
	}
	return s, nil
}

func positiveInt(doc map[string]any, field string) (int, error) {
	v, ok := doc[field]
	if !ok {
		return 0, fmt.Errorf("缺少必需字段 %q", field)
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("字段 %q 必须是正整数", field)
	}
	i, err := n.Int64()
	if err != nil || i <= 0 {
		return 0, fmt.Errorf("字段 %q 必须是正整数", field)
	}
	return int(i), nil
}

func parseClusters(v any) ([]Cluster, error) {
	if v == nil {
		return nil, errors.New("缺少必需字段 \"clusters\"")
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, errors.New("字段 \"clusters\" 必须是数组")
	}
	clusters := make([]Cluster, 0, len(raw))
	seen := make(map[string]bool)
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("clusters[%d] 必须是对象", i)
		}
		id, err := requiredString(obj, "id")
		if err != nil {
			return nil, fmt.Errorf("clusters[%d]: %w", i, err)
		}
		if seen[id] {
			return nil, fmt.Errorf("clusters[%d]: 重复的集群标识 %q", i, id)
		}
		seen[id] = true

		disabled := false
		if dv, present := obj["disabled"]; present {
			b, ok := dv.(bool)
			if !ok {
				return nil, fmt.Errorf("集群 %q 的 \"disabled\" 必须是布尔值", id)
			}
			disabled = b
		}

		tags := map[string]string{}
		if tv, present := obj["tags"]; present {
			tm, ok := tv.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("集群 %q 的 \"tags\" 必须是对象", id)
			}
			keys := make([]string, 0, len(tm))
			for k := range tm {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if k == "" {
					return nil, fmt.Errorf("集群 %q 的标签键不能为空", id)
				}
				sv, ok := tm[k].(string)
				if !ok {
					return nil, fmt.Errorf("集群 %q 的标签 %q 值必须是字符串", id, k)
				}
				tags[k] = sv
			}
		}

		clusters = append(clusters, Cluster{ID: id, Disabled: disabled, Tags: tags})
	}
	return clusters, nil
}

func parseConditions(v any, field string) ([]LabelCondition, error) {
	if v == nil {
		return []LabelCondition{}, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("字段 %q 必须是条件数组", field)
	}
	conds := make([]LabelCondition, 0, len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] 必须是对象", field, i)
		}
		if len(obj) == 0 {
			return nil, fmt.Errorf("%s[%d] 不能为空条件对象", field, i)
		}
		cond := LabelCondition{}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "" {
				return nil, fmt.Errorf("%s[%d] 的标签键不能为空", field, i)
			}
			sv, ok := obj[k].(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d] 的标签 %q 值必须是字符串", field, i, k)
			}
			cond[k] = sv
		}
		conds = append(conds, cond)
	}
	return conds, nil
}

// MakeReleasePlan selects available clusters, orders them by ID, and splits
// them into batches. It validates the input first and never mutates it.
func MakeReleasePlan(in ReleasePlanInput) (ReleasePlan, error) {
	if err := ValidateReleaseInput(in); err != nil {
		return ReleasePlan{}, err
	}
	if len(in.Clusters) == 0 {
		return ReleasePlan{}, errors.New("没有可发布的集群：未提供候选集群")
	}

	selected := []string{}
	excluded := []ExcludedCluster{}
	for _, c := range in.Clusters {
		switch {
		case c.Disabled:
			excluded = append(excluded, ExcludedCluster{ID: c.ID, Reason: ReasonDisabled})
		case matchesAny(c.Tags, in.Exclude):
			excluded = append(excluded, ExcludedCluster{ID: c.ID, Reason: ReasonExcludeMatched})
		case len(in.Include) > 0 && !matchesAny(c.Tags, in.Include):
			excluded = append(excluded, ExcludedCluster{ID: c.ID, Reason: ReasonIncludeNotMatched})
		default:
			selected = append(selected, c.ID)
		}
	}

	sort.Strings(selected)
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].ID < excluded[j].ID })

	if len(selected) == 0 {
		var b strings.Builder
		b.WriteString("没有符合规则的可用集群，各候选集群未入选原因：")
		for _, e := range excluded {
			b.WriteString("\n  ")
			b.WriteString(e.ID)
			b.WriteString("：")
			b.WriteString(e.Reason)
		}
		return ReleasePlan{}, errors.New(b.String())
	}

	batches := []Batch{}
	for i := 0; i < len(selected); i += in.BatchSize {
		end := i + in.BatchSize
		if end > len(selected) {
			end = len(selected)
		}
		batches = append(batches, Batch{Index: len(batches) + 1, Clusters: selected[i:end]})
	}

	return ReleasePlan{
		App:      AppInfo{Name: in.App, Revision: in.Revision, Image: in.Image},
		Batches:  batches,
		Excluded: excluded,
	}, nil
}

func matchesAny(tags map[string]string, conditions []LabelCondition) bool {
	for _, cond := range conditions {
		if matchesAll(tags, cond) {
			return true
		}
	}
	return false
}

func matchesAll(tags map[string]string, cond LabelCondition) bool {
	for k, v := range cond {
		tv, ok := tags[k]
		if !ok || tv != v {
			return false
		}
	}
	return true
}
