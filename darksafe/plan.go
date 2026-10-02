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
			return nil, fmt.Errorf("重复的集群标识 %q", id)
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
			for k, val := range tm {
				if k == "" {
					return nil, fmt.Errorf("集群 %q 的标签键不能为空", id)
				}
				sv, ok := val.(string)
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
		for k, val := range obj {
			if k == "" {
				return nil, fmt.Errorf("%s[%d] 的标签键不能为空", field, i)
			}
			sv, ok := val.(string)
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
// them into batches. It never mutates the input.
func MakeReleasePlan(in ReleasePlanInput) (ReleasePlan, error) {
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
