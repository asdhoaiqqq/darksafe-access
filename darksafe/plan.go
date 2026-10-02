package darksafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PlanSpec is the offline rollout-plan request.
type PlanSpec struct {
	Application string         `json:"application"`
	Revision    string         `json:"revision"`
	Image       string         `json:"image"`
	Clusters    []ClusterInput `json:"clusters"`
	BatchSize   int            `json:"batchSize"`
	Include     []LabelRule    `json:"include,omitempty"`
	Exclude     []LabelRule    `json:"exclude,omitempty"`
}

// ClusterInput describes one candidate cluster.
type ClusterInput struct {
	ID       string            `json:"id"`
	Disabled bool              `json:"disabled,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// LabelRule is a conjunction of label key/value requirements.
type LabelRule map[string]string

// PlanBatch is one numbered rollout wave.
type PlanBatch struct {
	Batch    int      `json:"batch"`
	Clusters []string `json:"clusters"`
}

// SkippedCluster explains why a candidate is not in the plan.
type SkippedCluster struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Plan is the deterministic offline rollout plan.
type Plan struct {
	Application string           `json:"application"`
	Revision    string           `json:"revision"`
	Image       string           `json:"image"`
	Batches     []PlanBatch      `json:"batches"`
	Skipped     []SkippedCluster `json:"skipped"`
}

// Reasons reported for clusters left out of a plan.
const (
	ReasonDisabled     = "集群已停用"
	ReasonExcluded     = "命中排除条件"
	ReasonNotIncluded  = "未命中包含条件"
	reasonNoCandidates = "未提供候选集群"
	reasonNoDeployable = "没有符合规则的可用集群"
)

// ParsePlanSpec decodes and strictly validates a plan request. JSON type
// mismatches and unknown fields are rejected so that silently ignored input
// can never produce a misleading plan.
func ParsePlanSpec(raw []byte) (PlanSpec, error) {
	var spec PlanSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return PlanSpec{}, fmt.Errorf("配置 JSON 无法解析: %w", err)
	}
	if dec.More() {
		return PlanSpec{}, fmt.Errorf("配置 JSON 无法解析: 顶层存在多个 JSON 值")
	}
	if err := spec.Validate(); err != nil {
		return PlanSpec{}, err
	}
	return spec, nil
}

// Validate checks every field constraint without touching any remote system.
func (s PlanSpec) Validate() error {
	if isBlank(s.Application) {
		return fmt.Errorf("应用名称不能为空或只含空白")
	}
	if isBlank(s.Revision) {
		return fmt.Errorf("修订号不能为空或只含空白")
	}
	if isBlank(s.Image) {
		return fmt.Errorf("容器镜像不能为空或只含空白")
	}
	if s.BatchSize <= 0 {
		return fmt.Errorf("批次容量必须是正整数, 当前为 %d", s.BatchSize)
	}
	if len(s.Clusters) == 0 {
		return fmt.Errorf(reasonNoCandidates)
	}
	seen := make(map[string]struct{}, len(s.Clusters))
	for i := range s.Clusters {
		id := s.Clusters[i].ID
		if isBlank(id) {
			return fmt.Errorf("第 %d 个集群标识不能为空或只含空白", i+1)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("集群标识重复: %q", id)
		}
		seen[id] = struct{}{}
		for key, value := range s.Clusters[i].Labels {
			if key == "" {
				return fmt.Errorf("集群 %q 存在空标签键", id)
			}
			// 标签值允许为空字符串；map[string]string 已保证为字符串。
			_ = value
		}
	}
	if err := validateRules("include", s.Include); err != nil {
		return err
	}
	if err := validateRules("exclude", s.Exclude); err != nil {
		return err
	}
	return nil
}

func validateRules(field string, rules []LabelRule) error {
	for i, rule := range rules {
		if len(rule) == 0 {
			return fmt.Errorf("%s 第 %d 个条件为空条件对象", field, i+1)
		}
		for key := range rule {
			if key == "" {
				return fmt.Errorf("%s 第 %d 个条件包含空标签键", field, i+1)
			}
		}
	}
	return nil
}

func isBlank(s string) bool {
	return strings.TrimSpace(s) == ""
}

// BuildPlan selects clusters, orders them deterministically and splits them
// into batches. It never connects to Kubernetes or checks the image.
func BuildPlan(spec PlanSpec) (Plan, error) {
	if err := spec.Validate(); err != nil {
		return Plan{}, err
	}

	selected := make([]string, 0)
	skipped := make([]SkippedCluster, 0)
	for _, cluster := range spec.Clusters {
		switch {
		case cluster.Disabled:
			skipped = append(skipped, SkippedCluster{ID: cluster.ID, Reason: ReasonDisabled})
		case matchAny(cluster.Labels, spec.Exclude):
			// 排除优先于包含。
			skipped = append(skipped, SkippedCluster{ID: cluster.ID, Reason: ReasonExcluded})
		case len(spec.Include) > 0 && !matchAny(cluster.Labels, spec.Include):
			skipped = append(skipped, SkippedCluster{ID: cluster.ID, Reason: ReasonNotIncluded})
		default:
			selected = append(selected, cluster.ID)
		}
	}

	if len(selected) == 0 {
		return Plan{}, noDeployableError(skipped)
	}

	sort.Strings(selected)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].ID < skipped[j].ID })

	batches := make([]PlanBatch, 0, (len(selected)+spec.BatchSize-1)/spec.BatchSize)
	for start, num := 0, 1; start < len(selected); start, num = start+spec.BatchSize, num+1 {
		end := start + spec.BatchSize
		if end > len(selected) {
			end = len(selected)
		}
		batches = append(batches, PlanBatch{
			Batch:    num,
			Clusters: append([]string(nil), selected[start:end]...),
		})
	}

	return Plan{
		Application: spec.Application,
		Revision:    spec.Revision,
		Image:       spec.Image,
		Batches:     batches,
		Skipped:     skipped,
	}, nil
}

// noDeployableError lists each candidate with its reason on stderr-oriented
// lines, sorted by cluster id so the failure is also deterministic.
func noDeployableError(skipped []SkippedCluster) error {
	sorted := append([]SkippedCluster(nil), skipped...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var b strings.Builder
	b.WriteString(reasonNoDeployable)
	for _, sk := range sorted {
		fmt.Fprintf(&b, "\n  - %s: %s", sk.ID, sk.Reason)
	}
	return fmt.Errorf("%s", b.String())
}

// matchAny reports whether labels satisfy at least one rule. Every key/value
// pair in a rule must match exactly; missing keys, wildcards and case folding
// are never applied.
func matchAny(labels map[string]string, rules []LabelRule) bool {
	for _, rule := range rules {
		if matchAll(labels, rule) {
			return true
		}
	}
	return false
}

func matchAll(labels map[string]string, rule LabelRule) bool {
	for key, want := range rule {
		got, ok := labels[key]
		if !ok || got != want {
			return false
		}
	}
	return true
}
