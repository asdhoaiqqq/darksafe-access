package darksafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Point 是一条序列上某个时间戳的采样值。
type Point struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// SeriesView 是结果输出用的一条序列视图，标签按键升序呈现。
type SeriesView struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// BatchResult 是一批采样点写入成功后的结果。
type BatchResult struct {
	Status     string       `json:"status"`
	Added      int          `json:"added"`
	Duplicates int          `json:"duplicates"`
	Series     []SeriesView `json:"series"`
}

// SeriesRef 标识一条序列：指标名加完整标签集合。
// 缺少某个标签与该标签值为空字符串属于不同序列。
type SeriesRef struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// Conflict 描述同一序列同一时间戳已存在不同的值。
type Conflict struct {
	Series    SeriesRef `json:"series"`
	Timestamp int64     `json:"timestamp"`
	Existing  float64   `json:"existing"`
	Submitted float64   `json:"submitted"`
}

// LineError 是一行（一批）输入失败后的结构化结果。
// Line 由命令入口按从 1 开始的行号填入；Index 为采样点在批次内从 1 开始的位置，
// 整行 JSON 无法解析或不是数组时 Index 为零并省略。
type LineError struct {
	Status   string    `json:"status"`
	Line     int       `json:"line,omitempty"`
	Index    int       `json:"index,omitempty"`
	Error    string    `json:"error"`
	Conflict *Conflict `json:"conflict,omitempty"`
}

// queryOp 是唯一支持的查询操作名。
const queryOp = "query"

// QuerySeries 是区间均值查询命中的一条序列视图。
// Labels 为该序列的完整标签集合（无标签时为空对象），不与其他序列合并。
type QuerySeries struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Count   int               `json:"count"`
	Average float64           `json:"average"`
}

// QueryResult 是区间均值查询成功后的结果。
type QueryResult struct {
	Status string        `json:"status"`
	Op     string        `json:"op"`
	Series []QuerySeries `json:"series"`
}

// parsedQuery 是严格校验后的查询对象。
type parsedQuery struct {
	name   string
	start  int64
	end    int64
	labels map[string]string
}

// MetricStore 保存一次命令运行期间已写入的指标数据，仅存在于内存中，进程结束即丢弃。
type MetricStore struct {
	series map[seriesID]*storedSeries
}

// seriesID 是序列的规范身份，标签书写顺序不影响身份。
type seriesID struct {
	name string
	// sig 为排序后标签键值对的长度前缀拼接，避免不同写法产生碰撞。
	sig string
}

type storedSeries struct {
	ref    SeriesRef
	points map[int64]float64
}

// NewMetricStore 创建一个空的指标存储。
func NewMetricStore() *MetricStore {
	return &MetricStore{series: make(map[seriesID]*storedSeries)}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func makeSeriesID(name string, labels map[string]string) seriesID {
	var b strings.Builder
	for _, k := range sortedKeys(labels) {
		v := labels[k]
		fmt.Fprintf(&b, "%d:%s;%d:%s;", len(k), k, len(v), v)
	}
	return seriesID{name: name, sig: b.String()}
}

// labelPair 是标签键值对字典序比较的单元。
type labelPair struct {
	Key, Value string
}

func sortedPairs(labels map[string]string) []labelPair {
	keys := sortedKeys(labels)
	pairs := make([]labelPair, len(keys))
	for i, k := range keys {
		pairs[i] = labelPair{Key: k, Value: labels[k]}
	}
	return pairs
}

func compareLabelPairs(a, b []labelPair) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].Key != b[i].Key {
			return strings.Compare(a[i].Key, b[i].Key)
		}
		if a[i].Value != b[i].Value {
			return strings.Compare(a[i].Value, b[i].Value)
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

var (
	jsonIntegerRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	jsonNumberRE  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
)

// ProcessLine 解析并处理一行输入：数组行为写入批次，对象行为区间均值查询。
// 成功返回 *BatchResult 或 *QueryResult；失败返回 *LineError，此时此前各批数据保留、
// 本批不写入任何数据，查询本身不改变存储。
func (s *MetricStore) ProcessLine(line string) (any, *LineError) {
	dec := json.NewDecoder(strings.NewReader(line))
	var top json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return nil, &LineError{Status: "error", Error: "invalid JSON: " + err.Error()}
	}
	// 第一个 JSON 值之后只允许输入结束（Decoder 自动跳过尾随空白）。
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected content after the JSON value")
		}
		return nil, &LineError{Status: "error", Error: "invalid JSON: " + err.Error()}
	}
	trimmed := strings.TrimSpace(string(top))
	if len(trimmed) == 0 {
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a JSON array of samples or a JSON query object"}
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(top, &items); err != nil {
			return nil, &LineError{Status: "error", Error: "invalid JSON: " + err.Error()}
		}
		return s.ingestItems(items)
	case '{':
		q, err := parseQuery(top)
		if err != nil {
			return nil, &LineError{Status: "error", Error: err.Error()}
		}
		return s.executeQuery(q), nil
	default:
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a JSON array of samples or a JSON query object"}
	}
}

// IngestLine 解析并写入一行（一批）采样点。成功返回 BatchResult；
// 失败返回 *LineError，此时此前各批数据保留、本批不写入任何数据。
// 仅接受 JSON 数组；查询对象行请改用 ProcessLine。
func (s *MetricStore) IngestLine(line string) (*BatchResult, *LineError) {
	res, err := s.ProcessLine(line)
	if err != nil {
		return nil, err
	}
	br, ok := res.(*BatchResult)
	if !ok {
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a JSON array of samples"}
	}
	return br, nil
}

type parsedSample struct {
	name   string
	ts     int64
	value  float64
	labels map[string]string
}

// parseSample 对单个采样点做严格校验：仅允许四个已知字段、拒绝重复键，
// 标量必须是精确的 JSON 类型，timestamp 为 int64 整数，value 为有限 float64。
func parseSample(raw json.RawMessage) (parsedSample, error) {
	var p parsedSample
	p.labels = map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return p, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return p, fmt.Errorf("each sample must be a JSON object")
	}

	present := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return p, err
		}
		key := keyTok.(string)
		switch key {
		case "name", "timestamp", "value", "labels":
		default:
			return p, fmt.Errorf("unknown field %q", key)
		}
		if present[key] {
			return p, fmt.Errorf("duplicate field %q", key)
		}
		present[key] = true

		switch key {
		case "name":
			t, err := dec.Token()
			if err != nil {
				return p, err
			}
			s, ok := t.(string)
			if !ok {
				return p, fmt.Errorf(`field "name" must be a string`)
			}
			if s == "" {
				return p, fmt.Errorf(`field "name" must be a non-empty string`)
			}
			p.name = s
		case "timestamp":
			n, err := nextJSONNumber(dec, "timestamp")
			if err != nil {
				return p, err
			}
			v, err := parseJSONInt(n)
			if err != nil {
				return p, fmt.Errorf(`field "timestamp": %s`, err)
			}
			p.ts = v
		case "value":
			n, err := nextJSONNumber(dec, "value")
			if err != nil {
				return p, err
			}
			v, err := parseFiniteFloat(n)
			if err != nil {
				return p, fmt.Errorf(`field "value": %s`, err)
			}
			p.value = v
		case "labels":
			t, err := dec.Token()
			if err != nil {
				return p, err
			}
			d, ok := t.(json.Delim)
			if !ok || d != '{' {
				return p, fmt.Errorf(`field "labels" must be an object of string keys to string values`)
			}
			labels := map[string]string{}
			for dec.More() {
				labelKeyTok, err := dec.Token()
				if err != nil {
					return p, err
				}
				labelKey := labelKeyTok.(string)
				if labelKey == "" {
					return p, fmt.Errorf(`field "labels": label keys must be non-empty strings`)
				}
				valTok, err := dec.Token()
				if err != nil {
					return p, err
				}
				labelVal, ok := valTok.(string)
				if !ok {
					return p, fmt.Errorf(`field "labels": value of label %q must be a string`, labelKey)
				}
				if _, dup := labels[labelKey]; dup {
					return p, fmt.Errorf(`field "labels": duplicate label key %q`, labelKey)
				}
				labels[labelKey] = labelVal
			}
			if _, err := dec.Token(); err != nil { // 消耗 '}'
				return p, err
			}
			p.labels = labels
		}
	}
	if _, err := dec.Token(); err != nil { // 消耗 '}'
		return p, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return p, fmt.Errorf("unexpected content after the sample object")
		}
		return p, err
	}

	for _, field := range []string{"name", "timestamp", "value"} {
		if !present[field] {
			return p, fmt.Errorf("missing required field %q", field)
		}
	}
	return p, nil
}

func nextJSONNumber(dec *json.Decoder, field string) (json.Number, error) {
	t, err := dec.Token()
	if err != nil {
		return "", err
	}
	n, ok := t.(json.Number)
	if !ok {
		return "", fmt.Errorf(`field %q must be a JSON number`, field)
	}
	return n, nil
}

func parseJSONInt(n json.Number) (int64, error) {
	s := string(n)
	if !jsonIntegerRE.MatchString(s) {
		return 0, fmt.Errorf("must be a JSON integer in milliseconds")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("must be within int64 range")
	}
	return v, nil
}

func parseFiniteFloat(n json.Number) (float64, error) {
	s := string(n)
	if !jsonNumberRE.MatchString(s) {
		return 0, fmt.Errorf("must be a JSON number")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("must be a finite number representable as float64")
	}
	return v, nil
}

// parseQuery 严格解析区间查询对象：仅允许 op/name/start/end/labels 五个字段，
// 拒绝重复键与未知字段；op 必须为 "query"，name 非空，start/end 为 int64 整数毫秒，
// labels 为非空键到字符串值的对象。start 大于 end 也在此拒绝。
func parseQuery(raw json.RawMessage) (parsedQuery, error) {
	var q parsedQuery
	q.labels = map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return q, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return q, fmt.Errorf("query must be a JSON object")
	}

	present := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return q, err
		}
		key := keyTok.(string)
		switch key {
		case "op", "name", "start", "end", "labels":
		default:
			return q, fmt.Errorf("unknown field %q", key)
		}
		if present[key] {
			return q, fmt.Errorf("duplicate field %q", key)
		}
		present[key] = true

		switch key {
		case "op":
			t, err := dec.Token()
			if err != nil {
				return q, err
			}
			s, ok := t.(string)
			if !ok {
				return q, fmt.Errorf(`field "op" must be a string`)
			}
			if s != queryOp {
				return q, fmt.Errorf(`unknown op %q: only "query" is supported`, s)
			}
		case "name":
			t, err := dec.Token()
			if err != nil {
				return q, err
			}
			s, ok := t.(string)
			if !ok {
				return q, fmt.Errorf(`field "name" must be a string`)
			}
			if s == "" {
				return q, fmt.Errorf(`field "name" must be a non-empty string`)
			}
			q.name = s
		case "start", "end":
			n, err := nextJSONNumber(dec, key)
			if err != nil {
				return q, err
			}
			v, err := parseJSONInt(n)
			if err != nil {
				return q, fmt.Errorf(`field %q: %s`, key, err)
			}
			if key == "start" {
				q.start = v
			} else {
				q.end = v
			}
		case "labels":
			t, err := dec.Token()
			if err != nil {
				return q, err
			}
			d, ok := t.(json.Delim)
			if !ok || d != '{' {
				return q, fmt.Errorf(`field "labels" must be an object of string keys to string values`)
			}
			labels := map[string]string{}
			for dec.More() {
				labelKeyTok, err := dec.Token()
				if err != nil {
					return q, err
				}
				labelKey := labelKeyTok.(string)
				if labelKey == "" {
					return q, fmt.Errorf(`field "labels": label keys must be non-empty strings`)
				}
				valTok, err := dec.Token()
				if err != nil {
					return q, err
				}
				labelVal, ok := valTok.(string)
				if !ok {
					return q, fmt.Errorf(`field "labels": value of label %q must be a string`, labelKey)
				}
				if _, dup := labels[labelKey]; dup {
					return q, fmt.Errorf(`field "labels": duplicate label key %q`, labelKey)
				}
				labels[labelKey] = labelVal
			}
			if _, err := dec.Token(); err != nil { // 消耗 '}'
				return q, err
			}
			q.labels = labels
		}
	}
	if _, err := dec.Token(); err != nil { // 消耗 '}'
		return q, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return q, fmt.Errorf("unexpected content after the query object")
		}
		return q, err
	}

	for _, field := range []string{"op", "name", "start", "end"} {
		if !present[field] {
			return q, fmt.Errorf("missing required field %q", field)
		}
	}
	if q.start > q.end {
		return q, fmt.Errorf("invalid range: start %d is greater than end %d", q.start, q.end)
	}
	return q, nil
}

// ingestItems 完成全部采样点校验与冲突检测后再统一提交，保证整批原子性。
func (s *MetricStore) ingestItems(items []json.RawMessage) (*BatchResult, *LineError) {
	type pending struct {
		id  seriesID
		ref SeriesRef
		ts  int64
		val float64
	}

	added := 0
	// 本批已确定身份的点（含与已写入数据重复的点），用于批内去重与冲突检测。
	seen := make(map[seriesID]map[int64]float64)
	// 仅记录需要落盘的新增点；校验全部通过后才提交。
	commits := make([]pending, 0, len(items))

	failAt := func(index int, msg string) *LineError {
		return &LineError{Status: "error", Index: index, Error: msg}
	}
	conflictAt := func(index int, ref SeriesRef, ts int64, existing, submitted float64) *LineError {
		return &LineError{
			Status: "error",
			Index:  index,
			Error: fmt.Sprintf("conflict: series %s at timestamp %d already has value %s, submitted %s",
				ref, ts, formatFloat(existing), formatFloat(submitted)),
			Conflict: &Conflict{
				Series:    ref,
				Timestamp: ts,
				Existing:  existing,
				Submitted: submitted,
			},
		}
	}

	for i, item := range items {
		pos := i + 1
		p, err := parseSample(item)
		if err != nil {
			return nil, failAt(pos, err.Error())
		}

		id := makeSeriesID(p.name, p.labels)
		ref := SeriesRef{Name: p.name, Labels: p.labels}

		if byTS, ok := seen[id]; ok {
			if old, ok := byTS[p.ts]; ok {
				if old != p.value {
					return nil, conflictAt(pos, ref, p.ts, old, p.value)
				}
				// 同批内重复出现且数值相同：重复，成功忽略。
				continue
			}
		}
		if sr, ok := s.series[id]; ok {
			if old, ok := sr.points[p.ts]; ok {
				if old != p.value {
					return nil, conflictAt(pos, sr.ref, p.ts, old, p.value)
				}
				// 与此前批次已写入的值相同：重复，成功忽略。
				if seen[id] == nil {
					seen[id] = map[int64]float64{}
				}
				seen[id][p.ts] = p.value
				continue
			}
		}

		added++
		if seen[id] == nil {
			seen[id] = map[int64]float64{}
		}
		seen[id][p.ts] = p.value
		commits = append(commits, pending{id: id, ref: ref, ts: p.ts, val: p.value})
	}

	// 全部采样点合法且无冲突，原子提交本批新增点。
	for _, c := range commits {
		sr := s.series[c.id]
		if sr == nil {
			sr = &storedSeries{ref: c.ref, points: map[int64]float64{}}
			s.series[c.id] = sr
		}
		sr.points[c.ts] = c.val
	}

	return s.snapshot(added, len(items)-added), nil
}

// formatFloat 以 JSON 数值风格格式化 float64：1 与 1.0 均显示为 1。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// String 让冲突原因中的序列身份可读：name{k=v,...}，标签按键排序。
func (r SeriesRef) String() string {
	var b strings.Builder
	b.WriteString(r.Name)
	b.WriteByte('{')
	for i, p := range sortedPairs(r.Labels) {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s", p.Key, p.Value)
	}
	b.WriteByte('}')
	return b.String()
}

// snapshot 生成按规范排序的全部序列视图：先按指标名，再按标签键值对字典序，
// 序列内按时间戳升序。
func (s *MetricStore) snapshot(added, duplicates int) *BatchResult {
	views := make([]SeriesView, 0, len(s.series))
	for _, sr := range s.series {
		tsList := make([]int64, 0, len(sr.points))
		for ts := range sr.points {
			tsList = append(tsList, ts)
		}
		sort.Slice(tsList, func(i, j int) bool { return tsList[i] < tsList[j] })
		points := make([]Point, len(tsList))
		for i, ts := range tsList {
			points[i] = Point{Timestamp: ts, Value: sr.points[ts]}
		}
		labels := make(map[string]string, len(sr.ref.Labels))
		for k, v := range sr.ref.Labels {
			labels[k] = v
		}
		views = append(views, SeriesView{Name: sr.ref.Name, Labels: labels, Points: points})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return compareLabelPairs(sortedPairs(views[i].Labels), sortedPairs(views[j].Labels)) < 0
	})
	return &BatchResult{Status: "ok", Added: added, Duplicates: duplicates, Series: views}
}

// queryLabelsMatch 判断序列标签是否满足查询标签：查询给出的每个键都必须存在且值相等，
// 序列可以有额外标签。缺少标签与标签值为空字符串不等价，因此必须用 comma-ok 区分。
func queryLabelsMatch(seriesLabels, queryLabels map[string]string) bool {
	for k, v := range queryLabels {
		sv, ok := seriesLabels[k]
		if !ok || sv != v {
			return false
		}
	}
	return true
}

// executeQuery 返回指标名精确匹配、标签匹配且闭区间 [start,end] 内存在采样点的序列，
// 按名称与标签字典序（沿用现有序列排序）排列；不修改存储。
func (s *MetricStore) executeQuery(q parsedQuery) *QueryResult {
	out := &QueryResult{Status: "ok", Op: queryOp, Series: []QuerySeries{}}
	for _, sr := range s.series {
		if sr.ref.Name != q.name {
			continue
		}
		if !queryLabelsMatch(sr.ref.Labels, q.labels) {
			continue
		}
		hit := make([]int64, 0, len(sr.points))
		for ts := range sr.points {
			if ts >= q.start && ts <= q.end {
				hit = append(hit, ts)
			}
		}
		if len(hit) == 0 {
			continue
		}
		sort.Slice(hit, func(i, j int) bool { return hit[i] < hit[j] })
		vals := make([]float64, len(hit))
		for i, ts := range hit {
			vals[i] = sr.points[ts]
		}
		labels := make(map[string]string, len(sr.ref.Labels))
		for k, v := range sr.ref.Labels {
			labels[k] = v
		}
		out.Series = append(out.Series, QuerySeries{
			Name:    sr.ref.Name,
			Labels:  labels,
			Count:   len(vals),
			Average: meanOf(vals),
		})
	}
	sort.Slice(out.Series, func(i, j int) bool {
		if out.Series[i].Name != out.Series[j].Name {
			return out.Series[i].Name < out.Series[j].Name
		}
		return compareLabelPairs(sortedPairs(out.Series[i].Labels), sortedPairs(out.Series[j].Labels)) < 0
	})
	return out
}

// meanOf 计算算术平均值。优先使用 Welford 在线递推（对普通数值结果最精确）；
// 当异号大值导致中间差值溢出 float64 时回退到按最大绝对值缩放的两遍算法，
// 保证合法有限值的均值始终为有限数，且不随求和先后次序改变。
func meanOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	mean := values[0]
	for i := 1; i < len(values); i++ {
		x := values[i]
		delta := x - mean
		if math.IsInf(delta, 0) {
			return scaledMean(values)
		}
		mean += delta / float64(i+1)
		if math.IsInf(mean, 0) || math.IsNaN(mean) {
			return scaledMean(values)
		}
	}
	if math.IsInf(mean, 0) || math.IsNaN(mean) {
		return scaledMean(values)
	}
	return mean
}

// scaledMean 先将各值除以绝对值最大值再求和，最后乘回，避免异号大值直接相减时溢出。
// 输入均为有限值时：|v/maxAbs| ≤ 1，项数为 int 级别，和与均值都必然有限。
func scaledMean(values []float64) float64 {
	var maxAbs float64
	for _, v := range values {
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v / maxAbs
	}
	m := maxAbs * (sum / float64(len(values)))
	if math.IsInf(m, 0) || math.IsNaN(m) {
		// 理论不可达的保守兜底：合法有限值的均值绝不输出无穷或 NaN。
		return maxAbs
	}
	return m
}
