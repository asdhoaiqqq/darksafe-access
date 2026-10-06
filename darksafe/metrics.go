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
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
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

// QueryPointsSeries 是采样明细查询结果中的一条序列：完整身份与区间内
// 按时间戳升序排列的采样点，不附带点数或平均值。
type QueryPointsSeries struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// QueryPointsResult 是一次区间采样明细查询（op 为 query_points）成功后的结果，
// 只读，不改变存储。
type QueryPointsResult struct {
	Status string              `json:"status"`
	Op     string              `json:"op"`
	Series []QueryPointsSeries `json:"series"`
}

// Window 是固定时间窗口统计中的一个窗口：[Start,End] 闭区间（含首尾毫秒）内
// 已成功保存的点的个数与算术平均值。空窗口不会出现——只有实际有点的窗口才列出。
type Window struct {
	Start   int64   `json:"start"`
	End     int64   `json:"end"`
	Count   int     `json:"count"`
	Average float64 `json:"average"`
}

// QueryWindowsSeries 是固定窗口均值查询结果中的一条序列：完整身份与按窗口起点
// 升序排列的窗口统计，空窗口不补零。
type QueryWindowsSeries struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Windows []Window          `json:"windows"`
}

// QueryWindowsResult 是一次固定时间窗口均值查询（op 为 query_windows）成功后的
// 结果，只读，不改变存储。
type QueryWindowsResult struct {
	Status string               `json:"status"`
	Op     string               `json:"op"`
	Series []QueryWindowsSeries `json:"series"`
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
// 成功时返回非 nil 的 *BatchResult、*QueryResult、*QueryPointsResult 或
// *QueryWindowsResult 且错误为 nil；任何失败都返回真正的 nil 结果与 *LineError，
// 调用方直接比较 result == nil 即可判定失败，无须先区分结果类型。空写入数组与
// 无命中的查询仍是成功，结果不为 nil。
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
		// 不能把 *BatchResult 直接装进 any 返回：失败时它是带类型的 nil 指针，
		// 接口值非 nil，调用方会把失败误判为有结果。
		batch, lerr := s.ingestItems(items)
		if lerr != nil {
			return nil, lerr
		}
		return batch, nil
	case '{':
		// 同上：失败时不能把带类型 nil 的结果指针装进 any。
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

// QueryLine 解析并执行一行 op 为 "query" 的区间均值查询对象。成功返回 QueryResult；
// 查询只读，不改变存储。op 为 "query_points" 的采样明细查询由 QueryPointsLine
// 执行，op 为 "query_windows" 的固定窗口均值查询由 QueryWindowsLine 执行。
func (s *MetricStore) QueryLine(line string) (*QueryResult, *LineError) {
	top, ok, lerr := decodeTopValue(line)
	if lerr != nil {
		return nil, lerr
	}
	if ok != '{' {
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a query object"}
	}
	res, lerr := s.queryFromObject(top)
	if lerr != nil {
		return nil, lerr
	}
	qr, isQuery := res.(*QueryResult)
	if !isQuery {
		if _, isPoints := res.(*QueryPointsResult); isPoints {
			return nil, &LineError{Status: "error", Error: `op "query_points" lists sample points; use QueryPointsLine`}
		}
		return nil, &LineError{Status: "error", Error: `op "query_windows" lists fixed-window averages; use QueryWindowsLine`}
	}
	return qr, nil
}

// QueryWindowsLine 解析并执行一行 op 为 "query_windows" 的固定时间窗口均值查询
// 对象。成功返回 QueryWindowsResult；查询只读，不改变存储。
func (s *MetricStore) QueryWindowsLine(line string) (*QueryWindowsResult, *LineError) {
	top, ok, lerr := decodeTopValue(line)
	if lerr != nil {
		return nil, lerr
	}
	if ok != '{' {
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a query object"}
	}
	res, lerr := s.queryFromObject(top)
	if lerr != nil {
		return nil, lerr
	}
	qw, isWindows := res.(*QueryWindowsResult)
	if !isWindows {
		if _, isPoints := res.(*QueryPointsResult); isPoints {
			return nil, &LineError{Status: "error", Error: `op "query_points" lists sample points; use QueryPointsLine`}
		}
		return nil, &LineError{Status: "error", Error: `op "query" computes range statistics; use QueryLine`}
	}
	return qw, nil
}

// QueryPointsLine 解析并执行一行 op 为 "query_points" 的区间采样明细查询对象。
// 成功返回 QueryPointsResult；查询只读，不改变存储。
func (s *MetricStore) QueryPointsLine(line string) (*QueryPointsResult, *LineError) {
	top, ok, lerr := decodeTopValue(line)
	if lerr != nil {
		return nil, lerr
	}
	if ok != '{' {
		return nil, &LineError{Status: "error", Error: "invalid JSON: input must be a query object"}
	}
	res, lerr := s.queryFromObject(top)
	if lerr != nil {
		return nil, lerr
	}
	qp, isPoints := res.(*QueryPointsResult)
	if !isPoints {
		if _, isWindows := res.(*QueryWindowsResult); isWindows {
			return nil, &LineError{Status: "error", Error: `op "query_windows" lists fixed-window averages; use QueryWindowsLine`}
		}
		return nil, &LineError{Status: "error", Error: `op "query" computes range statistics; use QueryLine`}
	}
	return qp, nil
}

// validateLineText 在 JSON 解析之前强制整行文本合法：原始字节必须是合法
// UTF-8，字符串内的 Unicode 转义必须表示有效字符。编码层/JSON 解析器会把
// 非法字节或未配对代理项静默替换成 U+FFFD，使损坏文本混入合法序列（被误判为
// 重复、冲突或错误命中查询），因此这里一律拒绝而不是修补后继续。
//
// 逐字节扫描、只在字符串内部识别转义：跳过词法空白与全部非字符串 token。
// 被转义的反斜杠（\\）后的 "uXXXX" 只是普通文本，不会被当成 Unicode 转义。
func validateLineText(line string) *LineError {
	if !utf8.ValidString(line) {
		return &LineError{Status: "error", Error: "invalid JSON: input is not valid UTF-8 text"}
	}
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(line) {
				break // 悬空反斜杠：交给 JSON 解析器报错
			}
			if line[i+1] != 'u' {
				i++ // 单字符转义（\\、\"、\n 等），跳过被转义字符
				continue
			}
			// i 指向 '\'，i+1 指向 'u'；转义占据 i..i+5。
			r, size := decodeJSONHexEscape(line, i+1)
			if size == 0 {
				return &LineError{Status: "error", Error: "invalid JSON: invalid Unicode escape sequence"}
			}
			if !utf16.IsSurrogate(r) {
				i += 5
				continue
			}
			if r >= 0xDC00 {
				// 单独出现的低代理项。
				return &LineError{Status: "error", Error: "invalid JSON: unpaired Unicode surrogate escape"}
			}
			// 高代理项后必须紧接另一个表示低代理项的 \uXXXX 转义。
			loPos := i + 6 // 第一个转义之后的字符位置
			if loPos+1 >= len(line) || line[loPos] != '\\' || line[loPos+1] != 'u' {
				return &LineError{Status: "error", Error: "invalid JSON: unpaired Unicode surrogate escape"}
			}
			lo, loSize := decodeJSONHexEscape(line, loPos+1)
			if loSize == 0 || lo < 0xDC00 || lo > 0xDFFF {
				return &LineError{Status: "error", Error: "invalid JSON: unpaired Unicode surrogate escape"}
			}
			i = loPos + loSize // 跳到第二转义的最后一个十六进制位，循环自增后越出
		}
	}
	return nil
}

// decodeJSONHexEscape 解析 line[pos:] 处的 "uXXXX"（pos 指向 'u'），
// 返回其码元与占用字节数（固定为 5）；不足四个十六进制数字时 size 为 0。
func decodeJSONHexEscape(line string, pos int) (rune, int) {
	if pos+4 >= len(line) {
		return 0, 0
	}
	var v rune
	for j := pos + 1; j <= pos+4; j++ {
		c := line[j]
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, 0
		}
		v = v<<4 | d
	}
	return v, 5
}

// decodeTopValue 解析单行中唯一的 JSON 值并返回其原始内容与首个非空白字节
// （'[' 或 '{'）。整个值无法解析、为空或存在尾随内容时返回 *LineError。
func decodeTopValue(line string) (json.RawMessage, byte, *LineError) {
	if lerr := validateLineText(line); lerr != nil {
		return nil, 0, lerr
	}
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

// fieldReader 从 dec 读取并校验当前字段的值，写入调用方持有的结果。
// 采样点与查询对象各自注册自己允许的字段及字段含义。
type fieldReader func(dec *json.Decoder) error

// parseStrictObject 是写入采样点与查询对象共用的严格对象校验骨架：
// 仅允许 fields 中列出的字段（未知字段先于判重被拒绝），同一对象内按转义还原
// 后的字段名判重（即使两个值相同也拒绝；判重不跨对象、不跨层级），按输入次序
// 逐字段调用对应的 fieldReader，对象结束后要求输入恰好耗尽，最后按 required
// 的顺序报告缺失的必填字段。notObjectMsg 与 trailingMsg 由调用方给出，
// 保留各自输入的措辞。
func parseStrictObject(raw json.RawMessage, notObjectMsg, trailingMsg string, required []string, fields map[string]fieldReader) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("%s", notObjectMsg)
	}

	present := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key := keyTok.(string)
		read, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		if present[key] {
			return fmt.Errorf("duplicate field %q", key)
		}
		present[key] = true
		if err := read(dec); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // 消耗 '}'
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s", trailingMsg)
		}
		return err
	}

	for _, field := range required {
		if !present[field] {
			return fmt.Errorf("missing required field %q", field)
		}
	}
	return nil
}

// readNameField 读取写入与查询共用的指标名字段：必须是非空字符串。
func readNameField(dec *json.Decoder, dst *string) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	s, ok := t.(string)
	if !ok {
		return fmt.Errorf(`field "name" must be a string`)
	}
	if s == "" {
		return fmt.Errorf(`field "name" must be a non-empty string`)
	}
	*dst = s
	return nil
}

// readInt64Field 读取 int64 毫秒整数字段（timestamp/start/end）：
// 必须是 JSON 整数且在 int64 范围内，数字字符串、布尔值、带小数点一律拒绝。
func readInt64Field(dec *json.Decoder, field string, dst *int64) error {
	n, err := nextJSONNumber(dec, field)
	if err != nil {
		return err
	}
	v, err := parseJSONInt(n)
	if err != nil {
		return fmt.Errorf("field %q: %s", field, err)
	}
	*dst = v
	return nil
}

// readFiniteFloatField 读取采样值字段：必须是可表示为有限 float64 的 JSON 数字。
func readFiniteFloatField(dec *json.Decoder, field string, dst *float64) error {
	n, err := nextJSONNumber(dec, field)
	if err != nil {
		return err
	}
	v, err := parseFiniteFloat(n)
	if err != nil {
		return fmt.Errorf("field %q: %s", field, err)
	}
	*dst = v
	return nil
}

// readLabelsField 读取标签对象字段并写入 dst；notObjectMsg 保留各输入侧措辞。
func readLabelsField(dec *json.Decoder, notObjectMsg string, dst *map[string]string) error {
	labels, err := parseLabels(dec, notObjectMsg)
	if err != nil {
		return err
	}
	*dst = labels
	return nil
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
	err := parseStrictObject(raw,
		"each sample must be a JSON object",
		"unexpected content after the sample object",
		[]string{"name", "timestamp", "value"},
		map[string]fieldReader{
			"name":      func(dec *json.Decoder) error { return readNameField(dec, &p.name) },
			"timestamp": func(dec *json.Decoder) error { return readInt64Field(dec, "timestamp", &p.ts) },
			"value":     func(dec *json.Decoder) error { return readFiniteFloatField(dec, "value", &p.value) },
			"labels": func(dec *json.Decoder) error {
				return readLabelsField(dec, `field "labels" must be an object of string keys to string values`, &p.labels)
			},
		})
	return p, err
}

// parseLabels 从 dec 读取一个标签对象并做统一校验：必须是 JSON 对象
// （否则返回调用方给出的 notObjectMsg，写入与查询措辞不同），键为非空字符串、
// 值为字符串（空串合法，键值均不做清理），重复键一律拒绝。
// 判重先于值的类型校验：某个键第一次出现时若值不是字符串，仍报告该值的类型
// 错误；而该键再次出现时，无论第二次的值是什么（数字、布尔、null、对象、数组），
// 都先报告重复标签键，不能让第二次值的类型掩盖键重复。
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
		// 先判重再读值：与采样点/查询字段的判重一致，重复键不被值的类型掩盖。
		if _, dup := labels[labelKey]; dup {
			return nil, fmt.Errorf(`field "labels": duplicate label key %q`, labelKey)
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		labelVal, ok := valTok.(string)
		if !ok {
			return nil, fmt.Errorf(`field "labels": value of label %q must be a string`, labelKey)
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

		// 同一采样位置无论值来自本批较早接受的点还是此前批次已写入的点，
		// 都走同一条判定：转换后的 float64 相等即重复、成功忽略，不同即冲突、
		// 整批拒绝。existing 是该位置当前生效的值，两种来源下语义一致。
		if existing, ok := s.positionValue(seen, id, p.ts); ok {
			if existing != p.value {
				return nil, conflictAt(pos, ref, p.ts, existing, p.value)
			}
			// 等值重复：成功忽略。seen 记录的是生效值 existing 而不是本次
			// 提交的 p.value：数值相等但表示不同（如已存 -0、本次提交 +0）时，
			// 本批后续在同位置的冲突必须如实报告最先接受的值，
			// 不能被这条被忽略的重复改写。
			if seen[id] == nil {
				seen[id] = map[int64]float64{}
			}
			seen[id][p.ts] = existing
			continue
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

// positionValue 解析一个采样位置（序列身份 + 时间戳）当前生效的值：
// 本批已接受的值优先（含与已写入数据等值而被忽略的重复所登记的生效值），
// 其次是此前批次已成功写入的值；该位置没有任何数据时 ok 为 false。
// 重复与冲突判定只看这里返回的值，不再区分两种来源。
func (s *MetricStore) positionValue(seen map[seriesID]map[int64]float64, id seriesID, ts int64) (value float64, ok bool) {
	if byTS, found := seen[id]; found {
		if v, hit := byTS[ts]; hit {
			return v, true
		}
	}
	if sr, found := s.series[id]; found {
		if v, hit := sr.points[ts]; hit {
			return v, true
		}
	}
	return 0, false
}

// formatFloat 以 JSON 数值风格格式化 float64：1 与 1.0 均显示为 1。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

type parsedQuery struct {
	op        string
	name      string
	start     int64
	end       int64
	labels    map[string]string
	step      int64
	stepGiven bool
	// basicGiven 记录 op/name/start/end 四个基础字段是否在对象中出现过：
	// 必填检查不能用零值判断（0 与空串可能就是合法输入）。
	basicGiven map[string]bool
}

// queryFromObject 严格解析查询对象并执行对应的只读操作：op 为 "query" 时
// 统计区间内点数与均值，为 "query_points" 时列出区间内采样明细，为
// "query_windows" 时按固定宽度时间窗口分别统计点数与均值。
// 成功返回非 nil 的 *QueryResult、*QueryPointsResult 或 *QueryWindowsResult；
// 失败返回 nil 与 *LineError。
func (s *MetricStore) queryFromObject(raw json.RawMessage) (any, *LineError) {
	q, err := parseQuery(raw)
	if err != nil {
		return nil, &LineError{Status: "error", Error: err.Error()}
	}
	if q.op == "query_windows" && !q.stepGiven {
		return nil, &LineError{Status: "error", Error: `missing required field "step"`}
	}
	if q.start > q.end {
		return nil, &LineError{Status: "error", Error: fmt.Sprintf(
			`invalid range: "start" must not be greater than "end" (%d > %d)`, q.start, q.end)}
	}
	switch q.op {
	case "query_points":
		return s.runQueryPoints(q), nil
	case "query_windows":
		return s.runQueryWindows(q), nil
	default:
		return s.runQuery(q), nil
	}
}

// scanQueryOp 在逐字段严格校验之前单独扫一遍查询对象，只确认 op 的出现情况：
// op 恰好出现一次且值是受支持的操作字符串时返回该操作与 true；其余情况
// （op 缺失、重复出现、值不是字符串或不是受支持的操作）返回 false，
// 由严格解析按既有规则逐项报告。调用方保证 raw 是完整合法的 JSON 值，
// 因此这里的解码不会失败；失败时按未确定处理，不影响后续的严格校验。
// 字段名按转义还原后比较，与 parseStrictObject 的判重口径一致。
func scanQueryOp(raw json.RawMessage) (op string, certain bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	count := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := keyTok.(string)
		var v any
		if err := dec.Decode(&v); err != nil {
			return "", false
		}
		if key == "op" {
			count++
			if s, ok := v.(string); ok {
				op = s
			} else {
				op = ""
			}
		}
	}
	if count != 1 {
		return "", false
	}
	switch op {
	case "query", "query_points", "query_windows":
		return op, true
	}
	return "", false
}

// parseQuery 对查询对象做严格校验：仅允许 op/name/start/end/labels，
// 以及仅 query_windows 使用的 step；拒绝重复键与未知字段；标量必须是精确的
// JSON 类型，start/end 为 int64 毫秒。op 只接受 "query"（区间点数与均值）、
// "query_points"（区间采样明细）与 "query_windows"（固定窗口均值），三种操作
// 共用同一套字段校验与区间规则。
//
// 错误选择沿用既有次序：结构完整的对象按字段书写顺序暴露问题，已出现字段全部
// 合法后才按 op、name、start、end 的顺序报告缺失的必填字段（step 是否必填要
// 等 op 确定，其缺失由 queryFromObject 在四项必填齐全后判定）。step 的合法性
// 依赖 op：op 恰好出现一次且指定受支持操作时（由 scanQueryOp 在逐字段校验前
// 确定），step 是否允许由该操作决定，与字段书写位置无关——query 与
// query_points 中 step 是未知字段，无论写在 op 前后、值是正整数、零、字符串
// 还是对象，都在 step 出现处按书写次序拒绝（不读其值，与 parseStrictObject
// 对其他未知字段的处理一致），且先于缺字段与区间检查暴露；query_windows 中
// step 的值在出现处即按 JSON 正整数校验：int64 范围、大于零，缺失、类型不符
// 或值不合法都指出 step。op 缺失、重复、类型错误或未知操作时不做预判，
// 沿用逐字段的既有处理（op 自身的问题由字段校验与必填检查报告）。
func parseQuery(raw json.RawMessage) (parsedQuery, error) {
	var q parsedQuery
	q.basicGiven = map[string]bool{}
	certainOp, opCertain := scanQueryOp(raw)
	err := parseStrictObject(raw,
		"each query must be a JSON object",
		"unexpected content after the query object",
		nil,
		map[string]fieldReader{
			"op": func(dec *json.Decoder) error {
				q.basicGiven["op"] = true
				t, err := dec.Token()
				if err != nil {
					return err
				}
				op, ok := t.(string)
				if !ok {
					return fmt.Errorf(`field "op" must be a string`)
				}
				if op != "query" && op != "query_points" && op != "query_windows" {
					return fmt.Errorf(`unknown op %q (only "query", "query_points" and "query_windows" are supported)`, op)
				}
				q.op = op
				return nil
			},
			"name": func(dec *json.Decoder) error {
				q.basicGiven["name"] = true
				return readNameField(dec, &q.name)
			},
			"start": func(dec *json.Decoder) error {
				q.basicGiven["start"] = true
				return readInt64Field(dec, "start", &q.start)
			},
			"end": func(dec *json.Decoder) error {
				q.basicGiven["end"] = true
				return readInt64Field(dec, "end", &q.end)
			},
			"labels": func(dec *json.Decoder) error {
				return readLabelsField(dec, `field "labels" must be an object of non-empty string keys to string values`, &q.labels)
			},
			"step": func(dec *json.Decoder) error {
				// op 已唯一确定且不是 query_windows：对该操作 step 是未知字段，
				// 无论写在 op 前后、值是什么，都在此处按书写次序拒绝，
				// 不读其值（与 parseStrictObject 的未知字段处理一致）。
				if opCertain && certainOp != "query_windows" {
					return fmt.Errorf(`unknown field "step"`)
				}
				// op 未经预判但已读到合法的 op 值（如 op 重复出现时第二个
				// op 之前的 step）：沿用同一判定。
				if q.op != "" && q.op != "query_windows" {
					return fmt.Errorf(`unknown field "step"`)
				}
				if err := readInt64Field(dec, "step", &q.step); err != nil {
					return err
				}
				if q.step <= 0 {
					return fmt.Errorf(`field "step": must be greater than zero`)
				}
				q.stepGiven = true
				return nil
			},
		})
	if err != nil {
		return q, err
	}
	// 必填项固定按 op、name、start、end 的顺序报告第一个缺项，与字段书写位置无关。
	for _, field := range []string{"op", "name", "start", "end"} {
		if !q.basicGiven[field] {
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

// matchSeries 筛出名称精确匹配且标签子集命中的已存序列，供三种查询操作各自整理；
// 只读，不改变存储。
func (s *MetricStore) matchSeries(q parsedQuery) []*storedSeries {
	matched := make([]*storedSeries, 0)
	for _, sr := range s.series {
		if sr.ref.Name != q.name || !labelsMatch(q.labels, sr.ref.Labels) {
			continue
		}
		matched = append(matched, sr)
	}
	return matched
}

// runQuery 只读统计已成功提交的数据：name 精确、标签子集、[start,end] 闭区间。
// 序列身份的整理（标签副本、排序键值对、排列次序）与写入快照共用同一套
// organizeSeries 规则，只是入选范围不同：这里先按名称与标签条件筛出命中序列
// 再整理，不为无关指标承担展示准备；筛后剩余序列的相对次序因此与写入全量
// 结果一致。查询只返回每条序列的点数与均值，不构造采样明细列表（明细由
// query_points 操作提供）——单次遍历点表只做计数与精确有理数求和（计数与
// 均值都与点的排列次序无关）。区间内没有点的序列不列出，不补零值条目。
func (s *MetricStore) runQuery(q parsedQuery) *QueryResult {
	organized := organizeSeries(s.matchSeries(q))

	out := make([]QuerySeries, 0, len(organized))
	for _, entry := range organized {
		// 一次遍历直接统计：区间内点的个数与精确总和。不构造值切片、
		// 不排序时间戳，避免为只返回 count/average 的查询承担采样明细展示。
		// 求和在有理数上精确完成：与写入顺序、批次划分无关，正负大数抵消后的
		// 小余量不会丢失，总和超出 float64 范围时均值仍然有限。
		count := 0
		sum := new(big.Rat)
		r := new(big.Rat)
		for ts, v := range entry.sr.points {
			if ts < q.start || ts > q.end {
				continue
			}
			count++
			// 写入侧已保证 v 有限，SetFloat64 对有限值是精确的。
			sum.Add(sum, r.SetFloat64(v))
		}
		if count == 0 {
			continue
		}
		sum.Quo(sum, r.SetInt64(int64(count)))
		out = append(out, QuerySeries{
			Name:   entry.ref.Name,
			Labels: entry.ref.Labels,
			Count:  count,
			// 精确平均舍入到最近可表示 float64（正中取偶）；精确为零时返回 +0。
			Average: ratToFloat64NearestEven(sum),
		})
	}
	return &QueryResult{Status: "ok", Op: "query", Series: out}
}

// runQueryPoints 只读列出已成功提交的采样明细：name 精确、标签子集、
// [start,end] 闭区间，与 runQuery 共用同一套序列筛选与 organizeSeries 整理
// 规则，因此序列排列次序与均值查询一致。每条命中序列保留完整指标名与完整
// 标签集合，points 按时间戳升序列出区间内的 timestamp 与 value，不附加
// count 或 average；区间内没有点的序列不列出，没有任何命中时 series 为
// 空数组。时间戳以 int64 比较与输出，保留整数精度；value 是实际存储的
// float64。查询只读，不改变采样值或写入计数。
func (s *MetricStore) runQueryPoints(q parsedQuery) *QueryPointsResult {
	organized := organizeSeries(s.matchSeries(q))

	out := make([]QueryPointsSeries, 0, len(organized))
	for _, entry := range organized {
		points := seriesPointsInRange(entry.sr, q.start, q.end)
		if len(points) == 0 {
			continue
		}
		out = append(out, QueryPointsSeries{
			Name:   entry.ref.Name,
			Labels: entry.ref.Labels,
			Points: points,
		})
	}
	return &QueryPointsResult{Status: "ok", Op: "query_points", Series: out}
}

// seriesPointsInRange 按 timestamp 升序返回一条已存序列在 [start,end] 闭区间
// 内的采样点，供采样明细查询展示。选取、升序排列与点构造与写入快照共用
// seriesPoints，区别只在传入的选取范围是闭区间而不是全部。
func seriesPointsInRange(sr *storedSeries, start, end int64) []Point {
	return seriesPoints(sr, func(ts int64) bool { return ts >= start && ts <= end })
}

// runQueryWindows 只读统计固定时间窗口内的点数与均值：窗口把 [start,end]
// 闭区间按 step 毫秒从 start 起连续划分，第 k 个窗口为
// [start+k*step, start+(k+1)*step-1]（含首尾毫秒），最后一个截到 end，
// 各序列共用同一组由 start 与 step 决定的边界——因此不为空窗口构造任何条目，
// 即使区间横跨整个 int64 也不预先物化窗口表。
//
// 名称精确、标签子集的序列筛选与 organizeSeries 整理、序列排列次序和另两种
// 查询完全一致。每条序列独立统计：先取区间内按时间戳升序的采样点，升序点流
// 中落在同一窗口的点必然连续，据此一次遍历分窗累计 count 与精确有理数总和；
// 只输出实际有点的窗口，按窗口起点升序，空窗口不补零，整个区间没有点的序列
// 不列出，无任何命中时 series 为空数组。每个点只归入 floor((ts-start)/step)
// 唯一窗口；start==end 时所有点归入唯一窗口。平均值沿用 query 的精确平均与
// 居中取偶舍入，依据实际存储的 float64 值计算，正负大数抵消或总和超出
// float64 范围时仍得有限结果。查询只读，不改变存储。
func (s *MetricStore) runQueryWindows(q parsedQuery) *QueryWindowsResult {
	organized := organizeSeries(s.matchSeries(q))

	// diff = end - start，可能达到 2^64-1（start 为 MinInt64、end 为 MaxInt64），
	// 只能以 uint64 承载；窗口下标与窗口内偏移都在这个无符号区间内运算。
	diff := uint64(q.end) - uint64(q.start)

	out := make([]QueryWindowsSeries, 0, len(organized))
	for _, entry := range organized {
		points := seriesPointsInRange(entry.sr, q.start, q.end)
		if len(points) == 0 {
			continue
		}
		windows := make([]Window, 0)
		var curIdx uint64
		count := 0
		sum := new(big.Rat)
		rv := new(big.Rat)
		flush := func() {
			windows = append(windows, buildWindow(q.start, q.end, q.step, diff, curIdx, count, sum))
		}
		for i, p := range points {
			// 无符号偏移除以 step 得窗口下标；升序点流下标单调不减。
			idx := (uint64(p.Timestamp) - uint64(q.start)) / uint64(q.step)
			if i > 0 && idx != curIdx {
				flush()
				count = 0
				sum = new(big.Rat)
			}
			curIdx = idx
			count++
			// 写入侧已保证 value 有限，SetFloat64 对有限值是精确的。
			sum.Add(sum, rv.SetFloat64(p.Value))
		}
		flush()
		out = append(out, QueryWindowsSeries{
			Name:    entry.ref.Name,
			Labels:  entry.ref.Labels,
			Windows: windows,
		})
	}
	return &QueryWindowsResult{Status: "ok", Op: "query_windows", Series: out}
}

// buildWindow 构造一个非空窗口的统计：窗口起点是 start + idx*step，终点是
// 起点加 step 减一；起点加 step 减一越过查询区间（含无符号加法溢出）时截到
// end。sum 在此被精确除以 count，再按与 query 相同的居中取偶规则舍入为
// float64（精确为零返回 +0）。调用方保证 0 <= idx*step <= diff。
func buildWindow(start, end, step int64, diff, idx uint64, count int, sum *big.Rat) Window {
	startOff := idx * uint64(step)
	wStart := int64AtOffset(start, startOff)
	wEnd := end
	// tail <= diff-startOff 的比较不做 startOff+tail 加法，避免无符号溢出
	// （step 接近 2^63 时朴素相加可能绕过 2^64）。
	if tail := uint64(step) - 1; tail <= diff-startOff {
		wEnd = int64AtOffset(start, startOff+tail)
	}
	sum.Quo(sum, new(big.Rat).SetInt64(int64(count)))
	return Window{
		Start:   wStart,
		End:     wEnd,
		Count:   count,
		Average: ratToFloat64NearestEven(sum),
	}
}

// int64AtOffset 返回 start + off（0 <= off，且调用方保证结果不超过 MaxInt64）。
// 直接做有符号相加在 start 接近 int64 下界、off 很大时会溢出，因此统一走
// 无符号运算还原：start 非负时无符号相加必落在非负有符号范围；start 为负时
// 以 -start 为界先回到非正区间、再跨入正区间，两个分支都不产生溢出。
func int64AtOffset(start int64, off uint64) int64 {
	if start >= 0 {
		return int64(uint64(start) + off)
	}
	gap := uint64(-start) // 0 < gap <= 2^63（start 为 MinInt64 时恰为 2^63）
	if off <= gap {
		// gap-off 为 2^63 时 int64 表示恰为 MinInt64，取负仍是 MinInt64，语义正确。
		return -int64(gap - off)
	}
	return int64(off - gap)
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

// String 让冲突原因中的序列身份可读且无歧义：name{k=v,...}，标签按键排序。
// 名称、标签键、标签值是不含分隔字符（逗号、等号、花括号、引号、反斜杠）、
// 不含首尾空白与不可打印字符的非空字符串时按原文呈现（cpu{host=a} 保持原样）；
// 否则以带引号的字符串字面量呈现，使内容字符与标签边界永远可以区分——
// 单标签值 "1,b=2" 渲染为 a="1,b=2"，与两标签 a=1,b=2 不再混淆；换行、
// 制表符等呈现为 \n、\t 这样的可辨认文字标记，一条冲突原因始终是一条记录；
// 空字符串值渲染为 ""，与无标签的 {} 明确区分。渲染只依赖解析后的真实身份，
// 与标签书写顺序、字符是否以 JSON 转义书写无关。
func (r SeriesRef) String() string {
	var b strings.Builder
	b.WriteString(identityText(r.Name))
	b.WriteByte('{')
	for i, p := range sortedPairs(r.Labels) {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(identityText(p.Key))
		b.WriteByte('=')
		b.WriteString(identityText(p.Value))
	}
	b.WriteByte('}')
	return b.String()
}

// identityText 把身份组成部分（指标名、标签键、标签值）渲染为无歧义文本：
// 不含分隔字符、首尾空白与不可打印字符的非空字符串原样返回；其余情况用
// strconv.Quote 加引号并转义，引号边界标出内容的起止（首尾空格、空串可见），
// 控制字符呈现为 \n、\t 等转义标记。分隔字符与引号、反斜杠一律被引号包裹，
// 因此渲染结果不会把内容字符误读为标签边界。
func identityText(s string) string {
	if s == "" || s != strings.TrimSpace(s) {
		return strconv.Quote(s)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || strings.ContainsRune(`,={}"\`, r) {
			return strconv.Quote(s)
		}
	}
	return s
}

// organizedSeries 是结果整理中的一条序列：携带已存序列指针、
// 供结果直接使用的独立标签副本（标签书写顺序不影响身份，
// 副本使调用方对结果的修改不能回写存储），以及该副本按键升序
// 整理好的键值对，供排序比较直接复用。写入快照与区间查询都经由
// organizeSeries 得到这样的条目，区别只在入选的序列范围。
type organizedSeries struct {
	sr    *storedSeries
	ref   SeriesRef
	pairs []labelPair
}

// seriesLess 是写入快照与区间查询共用的序列次序约定：先按指标名字符串
// 字典序，同名再按标签键升序后的键值对逐对比较（先比键再比值，前一对
// 相同才比下一对；较短集合是完整前缀时排前，无标签序列因此排在最前）。
// 标签值按字符串比较（"10" 在 "2" 之前）。排序只看解析后的真实身份，
// 与冲突原因里的展示文字、标签的输入次序无关。
func seriesLess(a, b organizedSeries) bool {
	if a.ref.Name != b.ref.Name {
		return a.ref.Name < b.ref.Name
	}
	return compareLabelPairs(a.pairs, b.pairs) < 0
}

// organizeSeries 是写入快照与三种查询成功结果共用的序列身份整理：对入选的每条已存序列
// 复制独立的标签副本、预整理按键升序的键值对，并按 seriesLess 的统一次序
// 排列。入选范围由调用方决定：写入快照传入全部已提交序列，三种区间查询只
// 传入名称与标签条件命中的序列；它们因此遵循同一套整理规则而各自保留数据
// 范围。每条序列的标签只在入选时整理一次：排序后的键值对随条目保留，sort
// 的反复比较直接复用同一份结果，不会因一条序列参与多次比较而反复整理同一
// 套标签。整理结果不复制采样点；写入快照的采样点选取与升序排列由
// allSeriesPoints 完成，均值查询只在点表上计数与求和，采样明细查询由
// seriesPointsInRange 按区间选取并升序排列，固定窗口查询在区间升序点流上
// 按窗口累计计数与精确求和。
func organizeSeries(selected []*storedSeries) []organizedSeries {
	out := make([]organizedSeries, 0, len(selected))
	for _, sr := range selected {
		// 标签必须复制：成功结果是当次操作的独立记录，
		// 调用方修改结果标签不能改动已存序列的身份。
		labels := cloneLabels(sr.ref.Labels)
		out = append(out, organizedSeries{
			sr:    sr,
			ref:   SeriesRef{Name: sr.ref.Name, Labels: labels},
			pairs: sortedPairs(labels),
		})
	}
	sort.Slice(out, func(i, j int) bool { return seriesLess(out[i], out[j]) })
	return out
}

// allSeriesPoints 按 timestamp 升序返回一条已存序列的全部采样点，供写入快照
// 展示。选取、升序排列与点构造与采样明细查询共用 seriesPoints，区别只在传入
// 的选取范围是全部而不是闭区间。
func allSeriesPoints(sr *storedSeries) []Point {
	return seriesPoints(sr, nil)
}

// seriesPoints 是写入快照与采样明细查询共用的采样点整理：从已存序列的点表中
// 选出 keep 命中的时间戳（keep 为 nil 表示全部选取），按 timestamp 升序排列，
// 每个时间戳配上点表中实际存储的 value（数值与时间戳一一对应，排序只动时间戳
// 列表、不重配对值；int64 比较保留整数精度，float64 原样取出，负零保持负零）。
// 返回的是新建切片与新建 Point，不持有点表的任何引用：调用方对结果的修改不
// 影响存储，也不影响此前或此后取得的其他成功结果；取得结果后再写入新点，该份
// 结果也不跟随存储变化。均值查询不构造这样的明细列表，只在点表上计数与求和。
func seriesPoints(sr *storedSeries, keep func(ts int64) bool) []Point {
	tsList := make([]int64, 0, len(sr.points))
	for ts := range sr.points {
		if keep != nil && !keep(ts) {
			continue
		}
		tsList = append(tsList, ts)
	}
	sort.Slice(tsList, func(i, j int) bool { return tsList[i] < tsList[j] })
	points := make([]Point, len(tsList))
	for i, ts := range tsList {
		points[i] = Point{Timestamp: ts, Value: sr.points[ts]}
	}
	return points
}

// snapshot 生成按规范排序的全部序列视图：入选范围是全部已提交序列，
// 身份整理与次序来自 organizeSeries，点排列来自 allSeriesPoints；
// 三种区间查询共用同一套身份整理，各自只对命中序列统计均值、列出明细或
// 按固定窗口统计。
func (s *MetricStore) snapshot(added, duplicates int) *BatchResult {
	all := make([]*storedSeries, 0, len(s.series))
	for _, sr := range s.series {
		all = append(all, sr)
	}
	organized := organizeSeries(all)
	views := make([]SeriesView, 0, len(organized))
	for _, os := range organized {
		views = append(views, SeriesView{
			Name:   os.ref.Name,
			Labels: os.ref.Labels,
			Points: allSeriesPoints(os.sr),
		})
	}
	return &BatchResult{Status: "ok", Added: added, Duplicates: duplicates, Series: views}
}
