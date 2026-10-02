package darksafe

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Point is one metric sample: a named measurement at a millisecond timestamp.
type Point struct {
	Name      string
	Labels    map[string]string
	Timestamp int64
	Value     float64
}

// PointView is one timestamp/value pair in a series snapshot.
type PointView struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// SeriesView is a sorted snapshot of one series.
type SeriesView struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points []PointView       `json:"points"`
}

// LineResult is the JSON result of ingesting one input line.
type LineResult struct {
	Line      int               `json:"line"`
	OK        bool              `json:"ok"`
	New       int               `json:"new"`
	Duplicate int               `json:"duplicate"`
	Series    []SeriesView      `json:"series,omitempty"`
	Error     string            `json:"error,omitempty"`
	Position  int               `json:"position,omitempty"`
	Metric    string            `json:"metric,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Timestamp int64             `json:"timestamp,omitempty"`
	Existing  *float64          `json:"existing,omitempty"`
	Submitted *float64         `json:"submitted,omitempty"`
}

// Engine accumulates metric points in memory. Series and points are never
// mutated in place: a batch is first validated and checked against the
// current state, then applied as a whole.
type Engine struct {
	series map[string]*seriesState
}

type seriesState struct {
	name   string
	labels map[string]string // sorted copy, never nil-free
	points map[int64]float64
}

// NewEngine returns an empty ingestion engine.
func NewEngine() *Engine {
	return &Engine{series: make(map[string]*seriesState)}
}

// IngestLine parses one input line and applies it transactionally: every point
// in the line is added, or none is. The returned result describes what happened.
func (e *Engine) IngestLine(line string, lineNo int) LineResult {
	result := LineResult{Line: lineNo}

	rawPoints, err := parseLine(line)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	points := make([]Point, 0, len(rawPoints))
	for i, raw := range rawPoints {
		p, perr := parsePoint(raw)
		if perr != nil {
			result.Error = fmt.Sprintf("point %d: %v", i+1, perr)
			result.Position = i + 1
			return result
		}
		points = append(points, p)
	}

	// Scratch state for this batch. Nothing is merged into the engine until
	// every point has been checked.
	added := make(map[string]map[int64]float64)
	meta := make(map[string]seriesState)
	newCount, dupCount := 0, 0

	for i, p := range points {
		key := seriesKey(p.Name, p.Labels)

		if existing, ok := e.series[key]; ok {
			if v, exists := existing.points[p.Timestamp]; exists {
				if v == p.Value {
					dupCount++
					continue
				}
				return e.conflict(result, p, i+1, v)
			}
		}
		if scratch, ok := added[key]; ok {
			if v, exists := scratch[p.Timestamp]; exists {
				if v == p.Value {
					dupCount++
					continue
				}
				return e.conflict(result, p, i+1, v)
			}
		}

		if _, ok := added[key]; !ok {
			added[key] = make(map[int64]float64)
			meta[key] = seriesState{name: p.Name, labels: sortedLabels(p.Labels)}
		}
		added[key][p.Timestamp] = p.Value
		newCount++
	}

	for key, pts := range added {
		state, ok := e.series[key]
		if !ok {
			state = &seriesState{
				name:   meta[key].name,
				labels: meta[key].labels,
				points: make(map[int64]float64),
			}
			e.series[key] = state
		}
		for ts, v := range pts {
			state.points[ts] = v
		}
	}

	result.OK = true
	result.New = newCount
	result.Duplicate = dupCount
	result.Series = e.snapshot()
	return result
}

func (e *Engine) conflict(result LineResult, p Point, position int, existing float64) LineResult {
	submitted := p.Value
	labels := sortedLabels(p.Labels)
	result.OK = false
	result.Position = position
	result.Metric = p.Name
	result.Labels = labels
	result.Timestamp = p.Timestamp
	result.Existing = &existing
	result.Submitted = &submitted
	result.Error = fmt.Sprintf(
		"point %d: value conflict for series %s at timestamp %d: existing value %v, submitted value %v",
		position, seriesDisplay(p.Name, labels), p.Timestamp, existing, submitted,
	)
	return result
}

// parseLine decodes a single JSON line into raw point messages. A line that
// is not a JSON array (including valid JSON of another type) is rejected.
func parseLine(line string) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "null" {
		return nil, fmt.Errorf("expected a JSON array of points, got null")
	}

	dec := json.NewDecoder(strings.NewReader(line))
	var elems []json.RawMessage
	if err := dec.Decode(&elems); err != nil {
		if !json.Valid([]byte(line)) {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return nil, fmt.Errorf("expected a JSON array of points: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing data after JSON array")
	}
	return elems, nil
}

var knownFields = map[string]bool{
	"name":      true,
	"labels":    true,
	"timestamp": true,
	"value":     true,
}

// isJSONNumber reports whether raw is a bare JSON number token (a leading
// digit or minus). json.Number would otherwise also accept a quoted string.
func isJSONNumber(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return false
	}
	c := s[0]
	return c == '-' || (c >= '0' && c <= '9')
}

// parsePoint validates one point object strictly: required fields, known
// fields only, exact types, int64 timestamp, finite float64 value.
func parsePoint(raw json.RawMessage) (Point, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Point{}, fmt.Errorf("expected a JSON object")
	}
	for k := range fields {
		if !knownFields[k] {
			return Point{}, fmt.Errorf("unknown field %q", k)
		}
	}

	rn, ok := fields["name"]
	if !ok {
		return Point{}, fmt.Errorf("missing required field %q", "name")
	}
	if string(rn) == "null" {
		return Point{}, fmt.Errorf("field %q must be a string", "name")
	}
	var name string
	if err := json.Unmarshal(rn, &name); err != nil {
		return Point{}, fmt.Errorf("field %q must be a string", "name")
	}
	if name == "" {
		return Point{}, fmt.Errorf("field %q must be a non-empty string", "name")
	}

	var labels map[string]string
	if rl, ok := fields["labels"]; ok {
		if string(rl) == "null" {
			return Point{}, fmt.Errorf("field %q must be an object of string to string", "labels")
		}
		var rawLabels map[string]json.RawMessage
		if err := json.Unmarshal(rl, &rawLabels); err != nil {
			return Point{}, fmt.Errorf("field %q must be an object of string to string", "labels")
		}
		labels = make(map[string]string, len(rawLabels))
		for k, rv := range rawLabels {
			if k == "" {
				return Point{}, fmt.Errorf("field %q contains an empty label key", "labels")
			}
			if string(rv) == "null" {
				return Point{}, fmt.Errorf("label %q must be a string value", k)
			}
			var v string
			if err := json.Unmarshal(rv, &v); err != nil {
				return Point{}, fmt.Errorf("label %q must be a string value", k)
			}
			labels[k] = v
		}
	}

	rt, ok := fields["timestamp"]
	if !ok {
		return Point{}, fmt.Errorf("missing required field %q", "timestamp")
	}
	if !isJSONNumber(rt) {
		return Point{}, fmt.Errorf("field %q must be a JSON integer in int64 range", "timestamp")
	}
	var tsNum json.Number
	if err := json.Unmarshal(rt, &tsNum); err != nil {
		return Point{}, fmt.Errorf("field %q must be a JSON integer in int64 range", "timestamp")
	}
	ts, err := strconv.ParseInt(tsNum.String(), 10, 64)
	if err != nil {
		return Point{}, fmt.Errorf("field %q must be a JSON integer in int64 range", "timestamp")
	}

	rv, ok := fields["value"]
	if !ok {
		return Point{}, fmt.Errorf("missing required field %q", "value")
	}
	if !isJSONNumber(rv) {
		return Point{}, fmt.Errorf("field %q must be a JSON number", "value")
	}
	var vNum json.Number
	if err := json.Unmarshal(rv, &vNum); err != nil {
		return Point{}, fmt.Errorf("field %q must be a JSON number", "value")
	}
	v, err := strconv.ParseFloat(vNum.String(), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return Point{}, fmt.Errorf("field %q must be a finite float64 number", "value")
	}

	return Point{Name: name, Labels: labels, Timestamp: ts, Value: v}, nil
}

func (e *Engine) snapshot() []SeriesView {
	views := make([]SeriesView, 0, len(e.series))
	for _, s := range e.series {
		labels := make(map[string]string, len(s.labels))
		for k, v := range s.labels {
			labels[k] = v
		}
		points := make([]PointView, 0, len(s.points))
		for ts, v := range s.points {
			points = append(points, PointView{Timestamp: ts, Value: v})
		}
		sort.Slice(points, func(i, j int) bool { return points[i].Timestamp < points[j].Timestamp })
		views = append(views, SeriesView{Name: s.name, Labels: labels, Points: points})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return compareLabels(views[i].Labels, views[j].Labels) < 0
	})
	return views
}

// seriesKey builds the canonical identity of a series: name plus sorted
// key=value pairs. Label order and omitted-vs-empty labels do not affect it.
func seriesKey(name string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, k := range sortedKeys(labels) {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

func seriesDisplay(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := sortedKeys(labels)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedLabels(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// compareLabels lexicographically compares sorted label pairs by key then value.
func compareLabels(a, b map[string]string) int {
	ka, kb := sortedKeys(a), sortedKeys(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if c := strings.Compare(ka[i], kb[i]); c != 0 {
			return c
		}
		if c := strings.Compare(a[ka[i]], b[kb[i]]); c != 0 {
			return c
		}
	}
	return len(ka) - len(kb)
}
