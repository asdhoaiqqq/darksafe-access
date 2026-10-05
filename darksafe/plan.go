package darksafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	// SpreadBy, when non-empty, names a cluster tag whose value identifies a
	// fault domain; each batch then contains at most one cluster per domain.
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
// Every string in the document — member names and values, in known fields
// and in unknown extra fields — must be legal text: invalid UTF-8 bytes and
// \uXXXX escapes naming an unpaired surrogate are rejected first, before any
// decoded string can be silently rewritten to "�" and compared. Every JSON
// object in the document — including unknown fields and their nested
// objects — must then have distinct member names; a name appearing twice
// (even with the same value, or via equivalent Unicode escapes) is rejected
// before any business validation runs.
func ParseReleaseInput(data []byte) (ReleasePlanInput, error) {
	if err := checkStrictText(data); err != nil {
		return ReleasePlanInput{}, err
	}
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

// checkDuplicateMembers walks every JSON object in data and rejects objects
// with duplicate member names. Names are compared after JSON decoding, so
// "env" and "env" are the same name while "env" and "ENV", or
// " env" and "env", are different. The first duplicate encountered in file
// order is reported: the walk is depth-first in document order, so the
// second occurrence that appears earliest wins, regardless of value type.
//
// The check is a policy over the shared structural walk (jsonwalk.go): the
// walker recognizes the nesting, decodes the member names and tracks each
// object's location; this check only keeps the seen-name set of each object.
// A document the walk cannot fully vouch for — an unrecognized structure,
// trailing content, or a loosely scanned number only the decoder judges —
// is handed to the encoding/json walk, which reports the established format
// error (and any duplicate preceding the malformation) with the existing
// wording.
func checkDuplicateMembers(data []byte) error {
	dup, recognized := scanDuplicateMembers(data)
	// json.Valid confirms the whole document — including the numbers the
	// walk skipped loosely and any trailing content — is one well-formed
	// JSON value, so a completed walk's verdict can be trusted directly.
	// Legal numbers beyond float64 range (e.g. 1e400) are valid JSON and
	// pass; whether a number is acceptable for a given field is decided
	// later by business validation.
	if recognized && json.Valid(data) {
		return dup
	}
	return checkJSONFormatAndDuplicates(data)
}

// scanDuplicateMembers runs the duplicate-member policy over the shared
// structural walk. It reports the first duplicate in file order — the walk
// is depth-first in document order, so the second occurrence that appears
// earliest wins — and whether the walk recognized the document's structure
// (no errUndecided and no text problem, both of which leave the verdict to
// the decoder walk). recognized does not by itself mean the document is
// well-formed JSON: the caller confirms that with json.Valid.
func scanDuplicateMembers(data []byte) (dup error, recognized bool) {
	// Each object's location in the document is unique, so the seen-name
	// sets are keyed by the owning object's path; the walk needs no
	// enter/exit events for them.
	seen := make(map[string]map[string]struct{})
	w := &jsonWalker{data: data, onMember: func(ownerPath, name string) error {
		set := seen[ownerPath]
		if set == nil {
			set = make(map[string]struct{})
			seen[ownerPath] = set
		}
		if _, ok := set[name]; ok {
			dup = &duplicateMemberError{field: name, path: ownerPath}
			return dup
		}
		set[name] = struct{}{}
		return nil
	}}
	if err := w.value("$"); err != nil && dup == nil {
		return nil, false
	}
	return dup, true
}

// checkJSONFormatAndDuplicates is the encoding/json walk that documents the
// raw structural walk cannot recognize are handed to. It reports the JSON
// format error with the established wording — and a duplicate member when
// its second occurrence precedes the malformation — exactly as the duplicate
// check always has for such documents.
func checkJSONFormatAndDuplicates(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers are only walked past, never interpreted: UseNumber keeps legal
	// JSON numbers beyond float64 range (e.g. 1e400) from being turned into an
	// unmarshal-overflow error here. Whether a number is acceptable for a given
	// field is decided later by business validation.
	dec.UseNumber()
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
// consumed yet. Together with walkJSONObject and walkJSONArray it is the
// decoding half of checkJSONFormatAndDuplicates: it only runs for documents
// the shared structural walk could not recognize, to report the format error
// (or an earlier duplicate) with the established wording.
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
		if err := walkJSONValueToken(dec, joinMemberPath(path, key), vtok); err != nil {
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
		if err := walkJSONValueToken(dec, joinIndexPath(path, i), tok); err != nil {
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
	spreadBy, err := optionalSpreadBy(doc)
	if err != nil {
		return ReleasePlanInput{}, err
	}
	clusters, err := parseClusters(doc["clusters"])
	if err != nil {
		return ReleasePlanInput{}, err
	}
	include, err := parseConditions(doc, "include")
	if err != nil {
		return ReleasePlanInput{}, err
	}
	exclude, err := parseConditions(doc, "exclude")
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

// optionalSpreadBy reads the optional "spreadBy" field: absent or an empty
// string disables fault-domain spreading; a non-string value or a string of
// only whitespace is rejected. The tag key is used exactly as written.
func optionalSpreadBy(doc map[string]any) (string, error) {
	v, ok := doc["spreadBy"]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("字段 %q 必须是字符串", "spreadBy")
	}
	if err := checkSpreadBy(s); err != nil {
		return "", err
	}
	return s, nil
}

// checkSpreadBy rejects a spreadBy value that is neither empty nor a usable
// tag key: a string of only whitespace can never match a valid tag key.
func checkSpreadBy(s string) error {
	if s != "" && strings.TrimSpace(s) == "" {
		return fmt.Errorf("字段 %q 不能只含空白", "spreadBy")
	}
	return nil
}

// ValidateReleaseInput checks that a release configuration is well-formed.
// Every string is first required to be legal UTF-8 — app, revision, image,
// spreadBy, every candidate's ID and tag keys/values (disabled and
// filtered-out candidates included), and every include/exclude condition's
// keys and values — because invalid bytes would later be rewritten to "�"
// while the plan is written as JSON and could merge two distinct identities
// or take part in label matching as an ordinary value. That text check runs
// before any business rule, in a fixed order (field order, then candidate
// and condition positions from zero, then keys in ascending order), so the
// one problem it reports never depends on map iteration. Business
// validation then runs as before: app/revision/image are non-empty,
// batchSize is a positive integer, spreadBy is empty or a usable tag key
// (not whitespace-only), cluster IDs are unique across every candidate
// (including disabled and filtered-out clusters), and tag/condition keys
// are non-empty. Errors name the field and the position in its list; within
// a cluster the ID is checked before its tags, and tag keys are examined in
// ascending order so repeated calls report the same error. The input is
// never mutated. MakeReleasePlan calls this automatically; library callers
// may use it to validate without planning.
func ValidateReleaseInput(in ReleasePlanInput) error {
	if err := validateStrictText(in); err != nil {
		return err
	}
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
	if err := checkSpreadBy(in.SpreadBy); err != nil {
		return err
	}
	ids := newClusterIDSet(len(in.Clusters))
	for i, c := range in.Clusters {
		if err := ids.check(i, c.ID); err != nil {
			return err
		}
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

// validateTagKeys applies the shared label-key rule to one cluster's tags,
// reporting the cluster's position in the candidate list.
func validateTagKeys(clusterIndex int, tags map[string]string) error {
	if hasEmptyLabelKey(tags) {
		return fmt.Errorf("clusters[%d] 的标签键不能为空", clusterIndex)
	}
	return nil
}

// validateConditionKeys applies the shared condition rules to every condition
// in original order, so the first invalid condition is reported.
func validateConditionKeys(field string, conds []LabelCondition) error {
	for i, cond := range conds {
		if err := validateCondition(field, i, cond); err != nil {
			return err
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

// positiveInt reads a required field as a positive integer. The literal must
// be an integral JSON number whose value is representable in full by the Go
// int of the running environment: a value beyond math.MaxInt is rejected here
// rather than narrowed by an int() conversion, which on a 32-bit environment
// could turn 4294967297 into 1 (or a larger value into zero or a negative
// number). No truncation, rounding, default substitution or clamping to the
// maximum ever happens — such a value is a configuration error regardless of
// what int it would collapse into.
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
	if err != nil || i <= 0 || i > math.MaxInt {
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
	ids := newClusterIDSet(len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("clusters[%d] 必须是对象", i)
		}
		id, err := clusterIDFromJSON(obj, i, ids)
		if err != nil {
			return nil, err
		}

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
			tags, err = labelsFromJSON(tm,
				func(string) error { return fmt.Errorf("集群 %q 的标签键不能为空", id) },
				func(k string) error { return fmt.Errorf("集群 %q 的标签 %q 值必须是字符串", id, k) },
			)
			if err != nil {
				return nil, err
			}
		}

		clusters = append(clusters, Cluster{ID: id, Disabled: disabled, Tags: tags})
	}
	return clusters, nil
}

// clusterIDFromJSON reads the "id" member of one candidate object and applies
// the shared cluster-identity rule. A missing member or a non-string value is
// a JSON decoding problem with its own reason; an empty or duplicate ID is
// reported by the shared rule exactly as for a directly constructed config.
func clusterIDFromJSON(obj map[string]any, index int, ids *clusterIDSet) (string, error) {
	v, ok := obj["id"]
	if !ok {
		return "", fmt.Errorf("clusters[%d]: 缺少必需字段 %q", index, "id")
	}
	id, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("clusters[%d]: 字段 %q 必须是字符串", index, "id")
	}
	if err := ids.check(index, id); err != nil {
		return "", err
	}
	return id, nil
}

// parseConditions reads the optional include/exclude field from the decoded
// document. An absent field means "no conditions" (the same as an explicit
// empty array); a field that is present must be an array of condition
// objects. An explicit JSON null is a field type error — it must not be read
// as "no conditions", which would silently drop the filter the author wrote.
func parseConditions(doc map[string]any, field string) ([]LabelCondition, error) {
	v, ok := doc[field]
	if !ok {
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
		cond, err := labelsFromJSON(obj,
			func(string) error { return fmt.Errorf("%s[%d] 的标签键不能为空", field, i) },
			func(k string) error { return fmt.Errorf("%s[%d] 的标签 %q 值必须是字符串", field, i, k) },
		)
		if err != nil {
			return nil, err
		}
		if err := validateCondition(field, i, cond); err != nil {
			return nil, err
		}
		conds = append(conds, cond)
	}
	return conds, nil
}

// MakeReleasePlan selects available clusters, orders them by ID, and splits
// them into batches. When SpreadBy names a tag, each batch holds at most one
// cluster per value of that tag (its fault domain) and every selected cluster
// must carry the tag. It validates the input first and never mutates it.
//
// When every candidate is filtered out, the returned error lists each
// candidate's rejection reason on its own line, ascending by the original
// ID. An ID containing a double quote, backslash, control character or
// U+2028/U+2029 is displayed as a quoted JSON string with those characters
// escaped (see rejectreport.go), so one candidate always occupies exactly
// one line and the original ID can be recovered from the display; this is
// only a display spelling — the ID itself is never altered or re-compared.
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
			b.WriteString(reportClusterID(e.ID))
			b.WriteString("：")
			b.WriteString(e.Reason)
		}
		return ReleasePlan{}, errors.New(b.String())
	}

	var batches []Batch
	if in.SpreadBy == "" {
		batches = chunkBatches(selected, in.BatchSize)
	} else {
		domains, err := faultDomains(in.Clusters, selected, in.SpreadBy)
		if err != nil {
			return ReleasePlan{}, err
		}
		batches = spreadBatches(selected, domains, in.BatchSize)
	}

	return ReleasePlan{
		App:      AppInfo{Name: in.App, Revision: in.Revision, Image: in.Image},
		Batches:  batches,
		Excluded: excluded,
	}, nil
}

// chunkBatches splits the ascending IDs into batches of at most batchSize.
// Each batch's Clusters gets its own backing array: a subslice of selected
// would leave spare capacity pointing at the next batch's IDs, so a caller
// appending to one batch's list would overwrite the following batch.
func chunkBatches(selected []string, batchSize int) []Batch {
	batches := []Batch{}
	for i := 0; i < len(selected); i += batchSize {
		end := i + batchSize
		if end > len(selected) {
			end = len(selected)
		}
		clusters := make([]string, end-i)
		copy(clusters, selected[i:end])
		batches = append(batches, Batch{Index: len(batches) + 1, Clusters: clusters})
	}
	return batches
}

// faultDomains maps every selected cluster ID to the exact value of its
// spreadBy tag. A selected cluster missing the tag fails the whole plan;
// selected is ascending, so the smallest such ID is reported. An empty
// string tag value is a valid fault domain, distinct from a missing tag.
func faultDomains(clusters []Cluster, selected []string, spreadBy string) (map[string]string, error) {
	tagsByID := make(map[string]map[string]string, len(clusters))
	for _, c := range clusters {
		tagsByID[c.ID] = c.Tags
	}
	domains := make(map[string]string, len(selected))
	for _, id := range selected {
		v, ok := tagsByID[id][spreadBy]
		if !ok {
			return nil, fmt.Errorf("集群 %q 缺少故障域标签 %q", id, spreadBy)
		}
		domains[id] = v
	}
	return domains, nil
}

// spreadBatches packs the ascending IDs into batches of at most batchSize
// with at most one cluster per fault domain in each batch. Each batch takes
// the smallest still-unscheduled IDs whose domains do not conflict with the
// batch; conflicting clusters are deferred to later batches, so a batch may
// be short only when no non-conflicting cluster remains. Every ID appears in
// exactly one batch and IDs stay ascending within a batch.
func spreadBatches(selected []string, domains map[string]string, batchSize int) []Batch {
	remaining := append([]string(nil), selected...)
	batches := []Batch{}
	for len(remaining) > 0 {
		// The map never holds more entries than there are remaining clusters,
		// so size it by them — never by batchSize, which is only an upper
		// bound and may be a huge legal value (e.g. 1e9 meaning "no limit").
		used := make(map[string]struct{}, min(batchSize, len(remaining)))
		batch := []string{}
		deferred := []string{}
		for _, id := range remaining {
			d := domains[id]
			if _, conflict := used[d]; len(batch) < batchSize && !conflict {
				used[d] = struct{}{}
				batch = append(batch, id)
			} else {
				deferred = append(deferred, id)
			}
		}
		batches = append(batches, Batch{Index: len(batches) + 1, Clusters: batch})
		remaining = deferred
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
