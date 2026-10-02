package darksafe

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeResult(t *testing.T, r LineResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return m
}

func TestIngestBasic(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":0.5},{"name":"cpu","timestamp":2000,"value":0.7}]`, 1)
	if !r.OK {
		t.Fatalf("unexpected failure: %s", r.Error)
	}
	if r.New != 2 || r.Duplicate != 0 {
		t.Fatalf("counts = new %d dup %d, want 2/0", r.New, r.Duplicate)
	}
	if len(r.Series) != 1 {
		t.Fatalf("series count = %d, want 1", len(r.Series))
	}
	s := r.Series[0]
	if s.Name != "cpu" || len(s.Points) != 2 {
		t.Fatalf("series = %+v", s)
	}
	if s.Points[0].Timestamp != 1000 || s.Points[0].Value != 0.5 {
		t.Fatalf("point[0] = %+v", s.Points[0])
	}
	if s.Points[1].Timestamp != 2000 || s.Points[1].Value != 0.7 {
		t.Fatalf("point[1] = %+v", s.Points[1])
	}
}

func TestIngestEmptyArray(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[]`, 1)
	if !r.OK {
		t.Fatalf("unexpected failure: %s", r.Error)
	}
	if r.New != 0 || r.Duplicate != 0 {
		t.Fatalf("counts = new %d dup %d, want 0/0", r.New, r.Duplicate)
	}
	if r.Series == nil {
		t.Fatal("series should be present as empty list, not nil")
	}
	if len(r.Series) != 0 {
		t.Fatalf("series = %v, want empty", r.Series)
	}
}

func TestIngestDuplicateAcrossBatches(t *testing.T) {
	e := NewEngine()
	r1 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1}]`, 1)
	if !r1.OK || r1.New != 1 || r1.Duplicate != 0 {
		t.Fatalf("first batch = %+v", r1)
	}
	r2 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1}]`, 2)
	if !r2.OK {
		t.Fatalf("duplicate should succeed: %s", r2.Error)
	}
	if r2.New != 0 || r2.Duplicate != 1 {
		t.Fatalf("counts = new %d dup %d, want 0/1", r2.New, r2.Duplicate)
	}
	if len(r2.Series) != 1 || len(r2.Series[0].Points) != 1 {
		t.Fatalf("series should still have one point: %+v", r2.Series)
	}
}

func TestIngestDuplicateEquivalentValues(t *testing.T) {
	e := NewEngine()
	r1 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1}]`, 1)
	if !r1.OK {
		t.Fatalf("first batch: %s", r1.Error)
	}
	r2 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1.0}]`, 2)
	if !r2.OK || r2.New != 0 || r2.Duplicate != 1 {
		t.Fatalf("1 vs 1.0 should be duplicate, got ok=%v new=%d dup=%d err=%q", r2.OK, r2.New, r2.Duplicate, r2.Error)
	}
}

func TestIngestConflictAcrossBatches(t *testing.T) {
	e := NewEngine()
	e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":0.5}]`, 1)
	r := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":0.9}]`, 2)
	if r.OK {
		t.Fatal("conflicting batch should fail")
	}
	if r.Position != 1 {
		t.Fatalf("position = %d, want 1", r.Position)
	}
	if r.Metric != "cpu" || r.Timestamp != 1000 {
		t.Fatalf("conflict detail = %+v", r)
	}
	if r.Existing == nil || r.Submitted == nil || *r.Existing != 0.5 || *r.Submitted != 0.9 {
		t.Fatalf("existing/submitted = %v/%v, want 0.5/0.9", r.Existing, r.Submitted)
	}
	if !strings.Contains(r.Error, "line 2") && r.Line != 2 {
		t.Fatalf("line number missing: %+v", r)
	}
	// Engine state must be unchanged after the failed batch.
	r3 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":0.5}]`, 3)
	if !r3.OK || r3.Duplicate != 1 {
		t.Fatalf("old value should still be present, got ok=%v dup=%d err=%q", r3.OK, r3.Duplicate, r3.Error)
	}
}

func TestIngestConflictWithinBatch(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1},{"name":"cpu","timestamp":1000,"value":2}]`, 1)
	if r.OK {
		t.Fatal("conflict within batch should fail")
	}
	if r.Position != 2 {
		t.Fatalf("position = %d, want 2", r.Position)
	}
	if r.Existing == nil || *r.Existing != 1 || r.Submitted == nil || *r.Submitted != 2 {
		t.Fatalf("existing/submitted = %v/%v, want 1/2", r.Existing, r.Submitted)
	}
	// Nothing from the failed line may have been applied.
	r2 := e.IngestLine(`[]`, 2)
	if len(r2.Series) != 0 {
		t.Fatalf("series should be empty after failed batch, got %+v", r2.Series)
	}
}

func TestIngestSameBatchNewThenDuplicate(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1},{"name":"cpu","timestamp":1000,"value":1}]`, 1)
	if !r.OK || r.New != 1 || r.Duplicate != 1 {
		t.Fatalf("ok=%v new=%d dup=%d, want true/1/1", r.OK, r.New, r.Duplicate)
	}
}

func TestIngestLabelOrderEquivalence(t *testing.T) {
	e := NewEngine()
	r1 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":"1","b":"2"}}]`, 1)
	if !r1.OK {
		t.Fatalf("first batch: %s", r1.Error)
	}
	r2 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"b":"2","a":"1"}}]`, 2)
	if !r2.OK || r2.Duplicate != 1 {
		t.Fatalf("reordered labels should be duplicate, got ok=%v dup=%d err=%q", r2.OK, r2.Duplicate, r2.Error)
	}
}

func TestIngestOmittedVsEmptyLabels(t *testing.T) {
	e := NewEngine()
	r1 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1}]`, 1)
	if !r1.OK {
		t.Fatalf("first batch: %s", r1.Error)
	}
	r2 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1,"labels":{}}]`, 2)
	if !r2.OK || r2.Duplicate != 1 {
		t.Fatalf("empty labels should equal omitted, got ok=%v dup=%d err=%q", r2.OK, r2.Duplicate, r2.Error)
	}
}

func TestIngestMissingLabelVsEmptyValue(t *testing.T) {
	e := NewEngine()
	r1 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":""}}]`, 1)
	if !r1.OK || r1.New != 1 {
		t.Fatalf("empty label value should be a new series, got ok=%v new=%d err=%q", r1.OK, r1.New, r1.Error)
	}
	r2 := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1}]`, 2)
	if !r2.OK || r2.New != 1 {
		t.Fatalf("missing label should be a different series, got ok=%v new=%d err=%q", r2.OK, r2.New, r2.Error)
	}
	if len(r2.Series) != 2 {
		t.Fatalf("series count = %d, want 2", len(r2.Series))
	}
}

func TestIngestEmptyLabelValuePreserved(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":""}}]`, 1)
	if !r.OK {
		t.Fatalf("batch: %s", r.Error)
	}
	labels := r.Series[0].Labels
	if labels["a"] != "" {
		t.Fatalf("label value = %q, want empty string", labels["a"])
	}
}

func TestIngestOutOfOrderTimestamps(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[
		{"name":"cpu","timestamp":3000,"value":3},
		{"name":"cpu","timestamp":1000,"value":1},
		{"name":"cpu","timestamp":2000,"value":2}
	]`, 1)
	if !r.OK {
		t.Fatalf("batch: %s", r.Error)
	}
	ts := []int64{r.Series[0].Points[0].Timestamp, r.Series[0].Points[1].Timestamp, r.Series[0].Points[2].Timestamp}
	if ts[0] != 1000 || ts[1] != 2000 || ts[2] != 3000 {
		t.Fatalf("timestamps = %v, want [1000 2000 3000]", ts)
	}
}

func TestIngestSeriesSorting(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[
		{"name":"mem","timestamp":1,"value":0,"labels":{"b":"2"}},
		{"name":"cpu","timestamp":1,"value":0},
		{"name":"cpu","timestamp":1,"value":0,"labels":{"a":"1"}},
		{"name":"cpu","timestamp":1,"value":0,"labels":{"a":"1","b":"2"}},
		{"name":"cpu","timestamp":1,"value":0,"labels":{"b":"2"}}
	]`, 1)
	if !r.OK {
		t.Fatalf("batch: %s", r.Error)
	}
	got := make([]string, 0, len(r.Series))
	for _, s := range r.Series {
		got = append(got, seriesDisplay(s.Name, s.Labels))
	}
	want := []string{"cpu", "cpu{a=1}", "cpu{a=1,b=2}", "cpu{b=2}", "mem{b=2}"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestIngestAtomicityOnInvalidPoint(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[
		{"name":"cpu","timestamp":1000,"value":1},
		{"name":"","timestamp":2000,"value":2}
	]`, 1)
	if r.OK {
		t.Fatal("batch with invalid point should fail")
	}
	if r.Position != 2 {
		t.Fatalf("position = %d, want 2", r.Position)
	}
	r2 := e.IngestLine(`[]`, 2)
	if len(r2.Series) != 0 {
		t.Fatalf("no points should have been applied, got %+v", r2.Series)
	}
}

func TestIngestAtomicityOnConflict(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[
		{"name":"cpu","timestamp":1000,"value":1},
		{"name":"cpu","timestamp":1000,"value":2},
		{"name":"cpu","timestamp":3000,"value":3}
	]`, 1)
	if r.OK {
		t.Fatal("batch with conflict should fail")
	}
	r2 := e.IngestLine(`[]`, 2)
	if len(r2.Series) != 0 {
		t.Fatalf("no points should have been applied, got %+v", r2.Series)
	}
}

func TestIngestSnapshotIncludesEarlierBatches(t *testing.T) {
	e := NewEngine()
	e.IngestLine(`[{"name":"a","timestamp":1,"value":1}]`, 1)
	e.IngestLine(`[{"name":"b","timestamp":1,"value":1}]`, 2)
	r := e.IngestLine(`[]`, 3)
	if len(r.Series) != 2 {
		t.Fatalf("series count = %d, want 2", len(r.Series))
	}
	if r.Series[0].Name != "a" || r.Series[1].Name != "b" {
		t.Fatalf("order = %s, %s", r.Series[0].Name, r.Series[1].Name)
	}
}

func TestIngestInvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"not array object", `{}`},
		{"not array string", `"hello"`},
		{"not array number", `42`},
		{"null", `null`},
		{"invalid json", `[1,`},
		{"trailing data", `[] garbage`},
		{"element not object", `[1]`},
		{"element array", `[[]]`},
		{"missing name", `[{"timestamp":1,"value":1}]`},
		{"empty name", `[{"name":"","timestamp":1,"value":1}]`},
		{"name number", `[{"name":5,"timestamp":1,"value":1}]`},
		{"name null", `[{"name":null,"timestamp":1,"value":1}]`},
		{"missing timestamp", `[{"name":"cpu","value":1}]`},
		{"timestamp float", `[{"name":"cpu","timestamp":1.5,"value":1}]`},
		{"timestamp string", `[{"name":"cpu","timestamp":"1000","value":1}]`},
		{"timestamp out of range", `[{"name":"cpu","timestamp":9223372036854775808,"value":1}]`},
		{"timestamp negative out of range", `[{"name":"cpu","timestamp":-9223372036854775809,"value":1}]`},
		{"missing value", `[{"name":"cpu","timestamp":1}]`},
		{"value string", `[{"name":"cpu","timestamp":1,"value":"1"}]`},
		{"value null", `[{"name":"cpu","timestamp":1,"value":null}]`},
		{"value overflow", `[{"name":"cpu","timestamp":1,"value":1e400}]`},
		{"unknown field", `[{"name":"cpu","timestamp":1,"value":1,"extra":2}]`},
		{"labels not object", `[{"name":"cpu","timestamp":1,"value":1,"labels":5}]`},
		{"labels null", `[{"name":"cpu","timestamp":1,"value":1,"labels":null}]`},
		{"labels value number", `[{"name":"cpu","timestamp":1,"value":1,"labels":{"a":1}}]`},
		{"labels value null", `[{"name":"cpu","timestamp":1,"value":1,"labels":{"a":null}}]`},
		{"labels empty key", `[{"name":"cpu","timestamp":1,"value":1,"labels":{"":"x"}}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine()
			r := e.IngestLine(tc.line, 7)
			if r.OK {
				t.Fatalf("expected failure for %s, got success", tc.line)
			}
			if r.Line != 7 {
				t.Fatalf("line = %d, want 7", r.Line)
			}
			if r.Error == "" {
				t.Fatal("error message is empty")
			}
		})
	}
}

func TestIngestParseErrorHasNoPosition(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[1,`, 3)
	if r.OK {
		t.Fatal("expected parse failure")
	}
	if r.Position != 0 {
		t.Fatalf("position = %d, want 0 for parse error", r.Position)
	}
	if !strings.Contains(r.Error, "line 3") && r.Line != 3 {
		t.Fatalf("line number missing: %+v", r)
	}
}

func TestIngestPointErrorHasPosition(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[{"name":"cpu","timestamp":1,"value":1},{"name":"","timestamp":2,"value":2}]`, 5)
	if r.OK {
		t.Fatal("expected failure")
	}
	if r.Position != 2 {
		t.Fatalf("position = %d, want 2", r.Position)
	}
	if r.Line != 5 {
		t.Fatalf("line = %d, want 5", r.Line)
	}
}

func TestIngestInt64Boundaries(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[
		{"name":"a","timestamp":9223372036854775807,"value":1},
		{"name":"b","timestamp":-9223372036854775808,"value":1},
		{"name":"c","timestamp":0,"value":1}
	]`, 1)
	if !r.OK {
		t.Fatalf("boundary timestamps should be valid: %s", r.Error)
	}
	if r.New != 3 {
		t.Fatalf("new = %d, want 3", r.New)
	}
}

func TestIngestValueFormats(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[
		{"name":"a","timestamp":1,"value":-0},
		{"name":"b","timestamp":1,"value":1.5e3},
		{"name":"c","timestamp":1,"value":-3.14},
		{"name":"d","timestamp":1,"value":1e-100}
	]`, 1)
	if !r.OK {
		t.Fatalf("finite values should be valid: %s", r.Error)
	}
	if r.New != 4 {
		t.Fatalf("new = %d, want 4", r.New)
	}
}

func TestIngestResultJSONShape(t *testing.T) {
	e := NewEngine()
	r := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]`, 1)
	m := decodeResult(t, r)
	if m["line"].(float64) != 1 {
		t.Fatalf("line = %v", m["line"])
	}
	if m["ok"].(bool) != true {
		t.Fatalf("ok = %v", m["ok"])
	}
	if m["new"].(float64) != 1 || m["duplicate"].(float64) != 0 {
		t.Fatalf("counts = %v/%v", m["new"], m["duplicate"])
	}
	series := m["series"].([]any)
	s0 := series[0].(map[string]any)
	if s0["name"] != "cpu" {
		t.Fatalf("series name = %v", s0["name"])
	}
	labels := s0["labels"].(map[string]any)
	if labels["host"] != "a" {
		t.Fatalf("labels = %v", labels)
	}
	p0 := s0["points"].([]any)[0].(map[string]any)
	if p0["timestamp"].(float64) != 1000 || p0["value"] != 0.5 {
		t.Fatalf("point = %v", p0)
	}
}

func TestIngestConflictJSONShape(t *testing.T) {
	e := NewEngine()
	e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]`, 1)
	r := e.IngestLine(`[{"name":"cpu","timestamp":1000,"value":0.9,"labels":{"host":"a"}}]`, 2)
	m := decodeResult(t, r)
	if m["ok"].(bool) != false {
		t.Fatalf("ok = %v", m["ok"])
	}
	if m["position"].(float64) != 1 {
		t.Fatalf("position = %v", m["position"])
	}
	if m["metric"] != "cpu" {
		t.Fatalf("metric = %v", m["metric"])
	}
	if m["timestamp"].(float64) != 1000 {
		t.Fatalf("timestamp = %v", m["timestamp"])
	}
	if m["existing"].(float64) != 0.5 || m["submitted"].(float64) != 0.9 {
		t.Fatalf("existing/submitted = %v/%v", m["existing"], m["submitted"])
	}
	if _, ok := m["series"]; ok {
		t.Fatalf("failure result should not include series: %v", m["series"])
	}
}
