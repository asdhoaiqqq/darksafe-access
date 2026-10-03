package darksafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
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

// QuerySeries 是查询结果中的一条序列：完整身份与区间内点的个数及算术平均值。
type QuerySeries struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Count   int               `json:"count"`
	Average float64           `json:"average"`
}

// QueryResult 是一次区间均值查询成功后的结果，只读，不改变存储。
type QueryResult struct {
	Status string        `json:"status"`
	Op     string        `json:"op"`
	Series []QuerySeries `json:"series"`
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

// cloneLabels 返回标签集合的独立副本；空标签仍复制为非 nil 的空集合，
// 调用方对副本的增删改不影响存储中的序列身份。
func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
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

// ProcessLine 处理一行非空输入：JSON 数组为写入批次，JSON 对象为查询/操作。
// 成功时返回 BatchResult 或 *QueryResult；失败返回 *LineError（Index 始终为零）。
func (s *MetricStore) ProcessLine(line string) (any, *LineError) {
	top, ok, lerr := decodeTopValue(line)
	if lerr != nil {
		return nil, lerr
	}
	switch ok {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(top, &items); err != nil {
			return nil, &LineError{Status: "error", Error: "invalid JSON: " + err.Error()}
		}
		return s.ingestItems(items)
	case '{':
		return s.queryFromObject(top)
	default:
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a JSON array of samples or a query object"}
	}
}

// IngestLine 解析并写入一行（一批）采样点。成功返回 BatchResult；
// 失败返回 *LineError，此时此前各批数据保留、本批不写入任何数据。
func (s *MetricStore) IngestLine(line string) (*BatchResult, *LineError) {
	top, ok, lerr := decodeTopValue(line)
	if lerr != nil {
		return nil, lerr
	}
	if ok != '[' {
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a JSON array of samples"}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(top, &items); err != nil {
		return nil, &LineError{Status: "error", Error: "invalid JSON: " + err.Error()}
	}
	return s.ingestItems(items)
}

// QueryLine 解析并执行一行查询对象。成功返回 QueryResult；查询只读，不改变存储。
func (s *MetricStore) QueryLine(line string) (*QueryResult, *LineError) {
	top, ok, lerr := decodeTopValue(line)
	if lerr != nil {
		return nil, lerr
	}
	if ok != '{' {
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a query object"}
	}
	return s.queryFromObject(top)
}

// decodeTopValue 解析单行中唯一的 JSON 值并返回其原始内容与首个非空白字节
// （'[' 或 '{'）。整个值无法解析、为空或存在尾随内容时返回 *LineError。
func decodeTopValue(line string) (json.RawMessage, byte, *LineError) {
	dec := json.NewDecoder(strings.NewReader(line))
	var top json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return nil, 0, &LineError{Status: "error", Error: "invalid JSON: " + err.Error()}
	}
	// 第一个 JSON 值之后只允许输入结束（Decoder 自动跳过尾随空白）。
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected content after the JSON value")
		}
		return nil, 0, &LineError{Status: "error", Error: "invalid JSON: " + err.Error()}
	}
	trimmed := strings.TrimSpace(string(top))
	if len(trimmed) == 0 || (trimmed[0] != '[' && trimmed[0] != '{') {
		return nil, 0, &LineError{Status: "error", Error: "invalid JSON: input must be a JSON array of samples or a query object"}
	}
	return top, trimmed[0], nil
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
			labels, err := parseLabels(dec, `field "labels" must be an object of string keys to string values`)
			if err != nil {
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

// parseLabels 从 dec 读取一个标签对象并做统一校验：必须是 JSON 对象
// （否则返回调用方给出的 notObjectMsg，写入与查询措辞不同），键为非空字符串、
// 值为字符串（空串合法，键值均不做清理），重复键一律拒绝。
// 返回的集合不含标签时也是非 nil 的空 map，与省略 labels 的语义一致。
func parseLabels(dec *json.Decoder, notObjectMsg string) (map[string]string, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := t.(json.Delim)
	if !ok || d != '{' {
		return nil, fmt.Errorf("%s", notObjectMsg)
	}
	labels := map[string]string{}
	for dec.More() {
		labelKeyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		labelKey := labelKeyTok.(string)
		if labelKey == "" {
			return nil, fmt.Errorf(`field "labels": label keys must be non-empty strings`)
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		labelVal, ok := valTok.(string)
		if !ok {
			return nil, fmt.Errorf(`field "labels": value of label %q must be a string`, labelKey)
		}
		if _, dup := labels[labelKey]; dup {
			return nil, fmt.Errorf(`field "labels": duplicate label key %q`, labelKey)
		}
		labels[labelKey] = labelVal
	}
	if _, err := dec.Token(); err != nil { // 消耗 '}'
		return nil, err
	}
	return labels, nil
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
				// 标签必须复制：冲突结果是本次失败的独立记录，
				// 调用方修改它不能改动已存序列的身份。
				Series:    SeriesRef{Name: ref.Name, Labels: cloneLabels(ref.Labels)},
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

type parsedQuery struct {
	name   string
	start  int64
	end    int64
	labels map[string]string
}

// queryFromObject 严格解析查询对象并执行只读区间均值查询。
func (s *MetricStore) queryFromObject(raw json.RawMessage) (*QueryResult, *LineError) {
	q, err := parseQuery(raw)
	if err != nil {
		return nil, &LineError{Status: "error", Error: err.Error()}
	}
	if q.start > q.end {
		return nil, &LineError{Status: "error", Error: fmt.Sprintf(
			`invalid range: "start" must not be greater than "end" (%d > %d)`, q.start, q.end)}
	}
	return s.runQuery(q), nil
}

// parseQuery 对查询对象做严格校验：仅允许 op/name/start/end/labels，
// 拒绝重复键与未知字段；标量必须是精确的 JSON 类型，start/end 为 int64 毫秒。
func parseQuery(raw json.RawMessage) (parsedQuery, error) {
	var q parsedQuery
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return q, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return q, fmt.Errorf("each query must be a JSON object")
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
			op, ok := t.(string)
			if !ok {
				return q, fmt.Errorf(`field "op" must be a string`)
			}
			if op != "query" {
				return q, fmt.Errorf(`unknown op %q (only "query" is supported)`, op)
			}
		case "name":
			t, err := dec.Token()
			if err != nil {
				return q, err
			}
			name, ok := t.(string)
			if !ok {
				return q, fmt.Errorf(`field "name" must be a string`)
			}
			if name == "" {
				return q, fmt.Errorf(`field "name" must be a non-empty string`)
			}
			q.name = name
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
			labels, err := parseLabels(dec, `field "labels" must be an object of non-empty string keys to string values`)
			if err != nil {
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
	return q, nil
}

// labelsMatch 实现子集匹配：want 中每个键都必须在 stored 中存在且值完全相等，
// stored 可以含额外标签；空字符串值只匹配真实存在的空值标签。
func labelsMatch(want, stored map[string]string) bool {
	for k, v := range want {
		got, ok := stored[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// runQuery 只读扫描已成功提交的数据：name 精确、标签子集、[start,end] 闭区间。
// 结果沿用 snapshot 的序列排序；无区间内点的序列不列出。
func (s *MetricStore) runQuery(q parsedQuery) *QueryResult {
	matched := make([]*storedSeries, 0)
	for _, sr := range s.series {
		if sr.ref.Name != q.name || !labelsMatch(q.labels, sr.ref.Labels) {
			continue
		}
		has := false
		for ts := range sr.points {
			if ts >= q.start && ts <= q.end {
				has = true
				break
			}
		}
		if has {
			matched = append(matched, sr)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].ref.Name != matched[j].ref.Name {
			return matched[i].ref.Name < matched[j].ref.Name
		}
		return compareLabelPairs(sortedPairs(matched[i].ref.Labels), sortedPairs(matched[j].ref.Labels)) < 0
	})

	out := make([]QuerySeries, 0, len(matched))
	for _, sr := range matched {
		tsList := make([]int64, 0, len(sr.points))
		for ts := range sr.points {
			if ts >= q.start && ts <= q.end {
				tsList = append(tsList, ts)
			}
		}
		sort.Slice(tsList, func(i, j int) bool { return tsList[i] < tsList[j] })
		values := make([]float64, len(tsList))
		for i, ts := range tsList {
			values[i] = sr.points[ts]
		}
		labels := cloneLabels(sr.ref.Labels)
		out = append(out, QuerySeries{
			Name:    sr.ref.Name,
			Labels:  labels,
			Count:   len(values),
			Average: finiteMean(values),
		})
	}
	return &QueryResult{Status: "ok", Op: "query", Series: out}
}

// finiteMean 返回已存储 float64 值的精确算术平均所对应的最近 float64，
// 恰好在两个相邻可表示值中间时按最近偶数舍入。
// 求和用有理数精确完成：与点的顺序、批次划分无关，正负大数抵消后的小余量
// 不会丢失，总和超出 float64 范围时结果仍然有限。精确平均为零时返回 +0；
// 只有一个点时结果就是该点的值。
func finiteMean(values []float64) float64 {
	sum := new(big.Rat)
	r := new(big.Rat)
	for _, v := range values {
		// 写入侧已保证 v 有限，SetFloat64 对有限值是精确的。
		sum.Add(sum, r.SetFloat64(v))
	}
	sum.Quo(sum, r.SetInt64(int64(len(values))))
	return ratToFloat64NearestEven(sum)
}

// ratToFloat64NearestEven 把有理数 x 舍入到最近的 float64，半数取偶。
// 调用方保证 |x| 不超过 MaxFloat64（有限值的均值必然满足），因此不会溢出。
func ratToFloat64NearestEven(x *big.Rat) float64 {
	if x.Sign() == 0 {
		return 0
	}
	num := new(big.Int).Set(x.Num())
	den := x.Denom()
	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}

	// e 满足 2^e <= num/den < 2^(e+1)。
	e := floorLog2Rat(num, den)
	var m *big.Int
	scale := 0
	if e >= -1022 {
		// 正规数：m 为 num/den 按 2^(e-52) 缩放后的 53 位舍入整数。
		scale = e - 52
		m = roundScaledRatio(num, den, scale)
		if m.BitLen() == 54 {
			// 舍入进位（如 1.111…1 进为 10.0），指数加一。
			m.Rsh(m, 1)
			scale++
		}
	} else {
		// 次正规数：有效位间距固定为 2^-1074；m 舍入为 0 时按 IEEE 得到带符号零。
		scale = -1074
		m = roundScaledRatio(num, den, scale)
	}
	// m < 2^53，可以精确放入 int64 与 float64。
	f := math.Ldexp(float64(m.Int64()), scale)
	if neg {
		return -f
	}
	return f
}

// floorLog2Rat 返回正有理数 num/den 的二进制阶：最大的 e 使 2^e <= num/den。
func floorLog2Rat(num, den *big.Int) int {
	k := num.BitLen() - den.BitLen()
	var t big.Int
	if k >= 0 {
		t.Lsh(den, uint(k))
		if num.Cmp(&t) >= 0 {
			return k
		}
		return k - 1
	}
	t.Lsh(num, uint(-k))
	if t.Cmp(den) >= 0 {
		return k
	}
	return k - 1
}

// roundScaledRatio 返回 round(num / (den · 2^scale))，半数取偶；num、den 均为正。
func roundScaledRatio(num, den *big.Int, scale int) *big.Int {
	n := new(big.Int).Set(num)
	d := new(big.Int).Set(den)
	if scale >= 0 {
		d.Lsh(d, uint(scale))
	} else {
		n.Lsh(n, uint(-scale))
	}
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	switch r2 := new(big.Int).Lsh(r, 1); r2.Cmp(d) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
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
		views = append(views, SeriesView{Name: sr.ref.Name, Labels: cloneLabels(sr.ref.Labels), Points: points})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return compareLabelPairs(sortedPairs(views[i].Labels), sortedPairs(views[j].Labels)) < 0
	})
	return &BatchResult{Status: "ok", Added: added, Duplicates: duplicates, Series: views}
}
