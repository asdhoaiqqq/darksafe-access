package darksafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// SpreadBy names a cluster tag whose values identify failure domains.
	// When non-empty, no batch contains two clusters with the same tag
	// value; the empty string disables spreading and leaves batching
	// unchanged. The tag key and values are matched exactly.
	SpreadBy string
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
// Every JSON object in the document — including unknown fields and their
// nested objects — must have distinct member names; a name appearing twice
// (even with the same value, or via equivalent Unicode escapes) is rejected
// before any business validation runs.
func ParseReleaseInput(data []byte) (ReleasePlanInput, error) {
	if err := checkDuplicateMembers(data); err != nil {
		return ReleasePlanInput{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return ReleasePlanInput{}, fmt.Errorf("JSON 格式错误: %w", err)
	}
	if dec.More() {
		return ReleasePlanInput{}, errors.New("JSON 格式错误: 文档包含多余内容")
	}
	return buildReleaseInput(doc)
}

// duplicateMemberError reports a JSON object whose member name appears more
// than once. Path is a JSONPath-style location: "$" for the document root,
// "$.clusters[1]" for a cluster, "$.clusters[0].tags" for a tag object,
// "$.include[2]" / "$.exclude[1]" for a condition, and "$.foo.bar[0]" for
// objects nested inside unknown fields.
type duplicateMemberError struct {
	field string
	path  string
}

func (e *duplicateMemberError) Error() string {
	return fmt.Sprintf("JSON 对象存在重复成员: 字段 %q 重复出现于 %s", e.field, e.path)
}

// checkDuplicateMembers walks every JSON object in data and rejects objects
// with duplicate member names. Names are compared after JSON decoding, so
// "env" and "env" are the same name while "env" and "ENV", or
// " env" and "env", are different. The first duplicate encountered in file
// order is reported: the walk is depth-first in document order, so the
// second occurrence that appears earliest wins, regardless of value type.
func checkDuplicateMembers(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(dec, "$"); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return errors.New("JSON 格式错误: 文档包含多余内容")
		}
		return fmt.Errorf("JSON 格式错误: %w", err)
	}
	return nil
}

// walkJSONValue reads one complete JSON value whose first token has not been
// consumed yet.
func walkJSONValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return errors.New("JSON 格式错误: 文档为空")
		}
		return fmt.Errorf("JSON 格式错误: %w", err)
	}
	return walkJSONValueToken(dec, path, tok)
}

// walkJSONValueToken reads the remainder of a value given its first token.
func walkJSONValueToken(dec *json.Decoder, path string, tok json.Token) error {
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			return walkJSONObject(dec, path)
		case '[':
			return walkJSONArray(dec, path)
		default:
			return fmt.Errorf("JSON 格式错误: 意外的分隔符 %q", d)
		}
	}
	return nil // scalar value (string, number, bool, null)
}

func walkJSONObject(dec *json.Decoder, path string) error {
	seen := make(map[string]struct{})
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("JSON 格式错误: 对象未闭合")
			}
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '}' {
				return nil
			}
			return fmt.Errorf("JSON 格式错误: 意外的分隔符 %q", d)
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("JSON 格式错误: 对象成员名必须是字符串")
		}
		if _, dup := seen[key]; dup {
			return &duplicateMemberError{field: key, path: path}
		}
		seen[key] = struct{}{}
		vtok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("JSON 格式错误: 缺少成员值")
			}
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		if err := walkJSONValueToken(dec, path+"."+key, vtok); err != nil {
			return err
		}
	}
}

func walkJSONArray(dec *json.Decoder, path string) error {
	for i := 0; ; i++ {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("JSON 格式错误: 数组未闭合")
			}
			return fmt.Errorf("JSON 格式错误: %w", err)
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return nil
		}
		if err := walkJSONValueToken(dec, fmt.Sprintf("%s[%d]", path, i), tok); err != nil {
			return err
		}
	}
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
	spreadBy, err := optionalString(doc, "spreadBy")
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
		SpreadBy:  spreadBy,
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
	if in.SpreadBy != "" && strings.TrimSpace(in.SpreadBy) == "" {
		return errors.New(`字段 "spreadBy" 不能只含空白`)
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

// optionalString reads an optional string field. A missing field yields the
// empty string (the disabled state); an explicitly empty string is also
// accepted as disabled, but a whitespace-only value is rejected because it
// is not a usable tag key.
func optionalString(doc map[string]any, field string) (string, error) {
	v, ok := doc[field]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("字段 %q 必须是字符串", field)
	}
	if s != "" && strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("字段 %q 不能只含空白", field)
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
// them into batches. When SpreadBy is set, batches also spread clusters across
// failure domains identified by that tag. It validates the input first and
// never mutates it.
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

	// Map selected IDs to their clusters for tag lookup.
	clusterByID := make(map[string]Cluster, len(in.Clusters))
	for _, c := range in.Clusters {
		clusterByID[c.ID] = c
	}

	if in.SpreadBy != "" {
		// Only selected clusters must carry the failure-domain tag; excluded
		// clusters are not checked and keep their original exclusion reason.
		// selected is sorted ascending, so the first missing ID is the
		// smallest one.
		for _, id := range selected {
			if _, ok := clusterByID[id].Tags[in.SpreadBy]; !ok {
				return ReleasePlan{}, fmt.Errorf("集群 %q 缺少 spreadBy 指定的标签 %q", id, in.SpreadBy)
			}
		}
	}

	var batches []Batch
	if in.SpreadBy != "" {
		batches = spreadBatches(selected, clusterByID, in.BatchSize, in.SpreadBy)
	} else {
		batches = plainBatches(selected, in.BatchSize)
	}

	return ReleasePlan{
		App:      AppInfo{Name: in.App, Revision: in.Revision, Image: in.Image},
		Batches:  batches,
		Excluded: excluded,
	}, nil
}

// plainBatches splits ascending IDs into fixed-size contiguous batches.
func plainBatches(selected []string, batchSize int) []Batch {
	batches := []Batch{}
	for i := 0; i < len(selected); i += batchSize {
		end := i + batchSize
		if end > len(selected) {
			end = len(selected)
		}
		batches = append(batches, Batch{Index: len(batches) + 1, Clusters: selected[i:end]})
	}
	return batches
}

// spreadBatches schedules ascending IDs into batches of at most batchSize,
// with no batch containing two clusters from the same failure domain (the
// exact tags[spreadBy] value; an empty string is a real domain distinct from
// a missing tag, and missing tags are rejected before this runs). Each batch
// is filled by scanning the remaining IDs in ascending order and taking the
// first cluster whose domain does not conflict with domains already used in
// this batch; a conflict only defers that cluster, so later IDs from other
// domains still fill the batch. A batch closes when it is full or no
// non-conflicting cluster remains, and deferred clusters carry into later
// batches until every ID appears exactly once.
func spreadBatches(selected []string, clusterByID map[string]Cluster, batchSize int, spreadBy string) []Batch {
	remaining := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		remaining[id] = struct{}{}
	}
	batches := []Batch{}
	for len(remaining) > 0 {
		batch := []string{}
		usedDomains := make(map[string]struct{})
		for _, id := range selected {
			if _, ok := remaining[id]; !ok {
				continue
			}
			if len(batch) >= batchSize {
				break
			}
			domain := clusterByID[id].Tags[spreadBy]
			if _, conflict := usedDomains[domain]; conflict {
				continue
			}
			usedDomains[domain] = struct{}{}
			batch = append(batch, id)
			delete(remaining, id)
		}
		batches = append(batches, Batch{Index: len(batches) + 1, Clusters: batch})
	}
	return batches
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
