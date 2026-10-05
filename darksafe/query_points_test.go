package darksafe

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 本文件保障采样明细查询（op 为 query_points）的行为：沿用查询条件取得区间内
// 原始采样点，只读，结果不附加统计字段。

func mustQueryPoints(t *testing.T, store *MetricStore, line string) *QueryPointsResult {
	t.Helper()
	res, err := store.QueryPointsLine(line)
	if err != nil {
		t.Fatalf("expected query_points success for %s, got error: %+v", line, err)
	}
	return res
}

func mustQueryPointsFail(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	res, err := store.QueryPointsLine(line)
	if err == nil {
		t.Fatalf("expected query_points failure for %s, got result: %+v", line, res)
	}
	return err
}

// 规格示例：cpu、host=a 在 1000、2000、3000 上分别存有 2、4、9，
// 查询 [1000,2000] 只列出前两个点，3000 上的点不出现。
func TestQueryPointsSpecExample(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":9,"labels":{"host":"a"}}
	]`)

	res := mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`)
	if res.Status != "ok" || res.Op != "query_points" {
		t.Fatalf("status/op = %q/%q, want ok/query_points", res.Status, res.Op)
	}
	if len(res.Series) != 1 {
		t.Fatalf("series len = %d, want 1", len(res.Series))
	}
	s := res.Series[0]
	if s.Name != "cpu" || s.Labels["host"] != "a" {
		t.Fatalf("series identity = %+v, want cpu host=a", s)
	}
	want := []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}
	if len(s.Points) != 2 || s.Points[0] != want[0] || s.Points[1] != want[1] {
		t.Fatalf("points = %+v, want %+v", s.Points, want)
	}
}

// 区间包含两个端点；start == end 时只返回该时间戳上的采样。
func TestQueryPointsInclusiveBoundsAndSingleTimestamp(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1000,"value":1},
		{"name":"m","timestamp":2000,"value":2},
		{"name":"m","timestamp":3000,"value":3}
	]`)

	res := mustQueryPoints(t, store, `{"op":"query_points","name":"m","start":2000,"end":2000}`)
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("start==end result = %+v, want exactly one point", res.Series)
	}
	if p := res.Series[0].Points[0]; p.Timestamp != 2000 || p.Value != 2 {
		t.Fatalf("point = %+v, want {2000 2}", p)
	}

	// 两个端点上的点都要出现。
	res = mustQueryPoints(t, store, `{"op":"query_points","name":"m","start":1000,"end":3000}`)
	if got := len(res.Series[0].Points); got != 3 {
		t.Fatalf("inclusive range point count = %d, want 3", got)
	}
}

// 指标不存在、标签条件未命中、区间内没有点：都成功返回空的非 nil series 数组。
func TestQueryPointsEmptyMatches(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	for _, tc := range []struct {
		name string
		line string
	}{
		{"unknown metric", `{"op":"query_points","name":"mem","start":0,"end":2000}`},
		{"label mismatch", `{"op":"query_points","name":"cpu","start":0,"end":2000,"labels":{"host":"b"}}`},
		{"no points in range", `{"op":"query_points","name":"cpu","start":1001,"end":2000}`},
		{"empty store", `{"op":"query_points","name":"cpu","start":0,"end":2000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store
			if tc.name == "empty store" {
				s = NewMetricStore()
			}
			res := mustQueryPoints(t, s, tc.line)
			if res.Series == nil || len(res.Series) != 0 {
				t.Fatalf("series = %+v, want non-nil empty array", res.Series)
			}
			if b, _ := json.Marshal(res); !strings.Contains(string(b), `"series":[]`) {
				t.Fatalf("JSON = %s, want empty series array", b)
			}
		})
	}
}

// 标签按子集匹配：省略或 {} 查全部序列；返回记录保留完整标签集合，
// 不缩减成查询条件；无标签序列的 labels 为 {}。
func TestQueryPointsLabelSubsetAndFullIdentity(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a","dc":"x"}},
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":3}
	]`)

	// 子集条件命中两条序列，结果保留各自的完整标签集合。
	// 次序沿用既有约定：标签键值对逐对比较，dc 在 host 前，
	// 因此 {dc:x,host:a} 排在 {host:a} 前。
	res := mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`)
	if len(res.Series) != 2 {
		t.Fatalf("subset match series len = %d, want 2", len(res.Series))
	}
	if got := res.Series[0].Labels; len(got) != 2 || got["host"] != "a" || got["dc"] != "x" {
		t.Fatalf("series[0] labels = %+v, want full {dc:x,host:a}", got)
	}
	if got := res.Series[1].Labels; len(got) != 1 || got["host"] != "a" {
		t.Fatalf("series[1] labels = %+v, want exactly {host:a}", got)
	}

	// 省略 labels 与传 {} 都查全部序列，含无标签序列（labels 为 {}）。
	for _, line := range []string{
		`{"op":"query_points","name":"cpu","start":0,"end":2000}`,
		`{"op":"query_points","name":"cpu","start":0,"end":2000,"labels":{}}`,
	} {
		res = mustQueryPoints(t, store, line)
		if len(res.Series) != 3 {
			t.Fatalf("%s: series len = %d, want 3", line, len(res.Series))
		}
		// 无标签序列排在最前（较短标签集合是完整前缀时排前），labels 为空非 nil。
		if res.Series[0].Labels == nil || len(res.Series[0].Labels) != 0 {
			t.Fatalf("%s: no-label series labels = %+v, want {}", line, res.Series[0].Labels)
		}
	}
}

// 序列排列次序与现有成功结果一致；每条序列的点按时间戳升序（写入故意乱序）。
func TestQueryPointsOrderingMatchesExistingConvention(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"mem","timestamp":1000,"value":9},
		{"name":"cpu","timestamp":3000,"value":3,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":2000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1500,"value":15}
	]`)

	res := mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":0,"end":4000}`)
	if len(res.Series) != 3 {
		t.Fatalf("series len = %d, want 3", len(res.Series))
	}
	// 与写入快照同一次序：无标签在前，再按标签键值对 host=a、host=b。
	if len(res.Series[0].Labels) != 0 || res.Series[1].Labels["host"] != "a" || res.Series[2].Labels["host"] != "b" {
		t.Fatalf("series order = %+v, want {}, host=a, host=b", res.Series)
	}
	// 与均值查询的序列次序一致。
	qres := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":4000}`)
	for i := range res.Series {
		if res.Series[i].Name != qres.Series[i].Name {
			t.Fatalf("series[%d] name = %q, query has %q", i, res.Series[i].Name, qres.Series[i].Name)
		}
	}
	// host=b 序列的点按时间戳升序。
	pts := res.Series[2].Points
	if len(pts) != 2 || pts[0].Timestamp != 1000 || pts[1].Timestamp != 3000 {
		t.Fatalf("host=b points = %+v, want ascending 1000,3000", pts)
	}
}

// value 展示实际存储的 float64 值；时间戳在范围判断与输出中保留 int64 精度。
func TestQueryPointsValueAndTimestampPrecision(t *testing.T) {
	store := NewMetricStore()
	const big = int64(1)<<53 + 1 // 不能被 float64 精确表示的 int64
	line := `[{"name":"m","timestamp":` + jsonNumber(big) + `,"value":0.1},` +
		`{"name":"m","timestamp":` + jsonNumber(big+1) + `,"value":1e16}]`
	mustOK(t, store, line)

	// 用精确的 int64 边界只框住第一个点；若时间戳经 float64 转换，
	// 两个相邻大整数会糊成同一个值，边界判断就会出错。
	res := mustQueryPoints(t, store,
		`{"op":"query_points","name":"m","start":`+jsonNumber(big)+`,"end":`+jsonNumber(big)+`}`)
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("result = %+v, want exactly the first point", res.Series)
	}
	p := res.Series[0].Points[0]
	if p.Timestamp != big {
		t.Fatalf("timestamp = %d, want %d (int64 precision lost)", p.Timestamp, big)
	}
	if p.Value != 0.1 {
		t.Fatalf("value = %v, want stored float64 0.1", p.Value)
	}

	// int64 极值边界合法且覆盖全部点。
	res = mustQueryPoints(t, store,
		`{"op":"query_points","name":"m","start":-9223372036854775808,"end":9223372036854775807}`)
	if len(res.Series) != 1 || len(res.Series[0].Points) != 2 {
		t.Fatalf("extreme range result = %+v, want both points", res.Series)
	}
	if got := res.Series[0].Points[1].Value; got != 1e16 {
		t.Fatalf("second value = %v, want 1e16", got)
	}
}

// jsonNumber 把 int64 写成 JSON 数字字面量，避免测试里手写大数出错。
func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// 只读：明细查询不改变采样值或写入计数，随后写入与统计查询看到的数据不变。
func TestQueryPointsIsReadOnly(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":0,"end":2000}`)
	mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":0,"end":2000}`)

	// 统计查询仍是一个点、均值为 2。
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("summary after query_points = %+v, want count 1 average 2", qr.Series)
	}
	// 等值重提仍是重复而不是新增：明细查询没有改动已存数据。
	br := mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if br.Added != 0 || br.Duplicates != 1 {
		t.Fatalf("re-submit after query_points = %d/%d, want 0/1", br.Added, br.Duplicates)
	}
}

// 输入校验与失败原因的选择沿用现有查询约定：同样的字段问题给出同样的原因，
// 失败结果不带 index 与 conflict，倒置区间指出实际边界。
func TestQueryPointsValidationFollowsQueryConventions(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"unknown field", `{"op":"query_points","bogus":1,"name":"cpu","start":0,"end":1}`, `unknown field "bogus"`},
		{"name wrong type", `{"op":"query_points","name":7,"start":0,"end":1}`, `field "name" must be a string`},
		{"start wrong type", `{"op":"query_points","name":"cpu","start":"0","end":1}`, `field "start" must be a JSON number`},
		{"missing name", `{"op":"query_points","start":0,"end":1}`, `missing required field "name"`},
		{"missing start", `{"op":"query_points","name":"cpu","end":1}`, `missing required field "start"`},
		{"missing end", `{"op":"query_points","name":"cpu","start":0}`, `missing required field "end"`},
		{"unknown op", `{"op":"ping","name":"cpu","start":0,"end":1}`, `unknown op "ping" (only "query" and "query_points" are supported)`},
		{"duplicate field", `{"op":"query_points","op":"query_points","name":"cpu","start":0,"end":1}`, `duplicate field "op"`},
		{"inverted range", `{"op":"query_points","name":"cpu","start":5000,"end":1000}`,
			`invalid range: "start" must not be greater than "end" (5000 > 1000)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lerr := mustQueryPointsFail(t, store, tc.line)
			if lerr.Error != tc.want {
				t.Fatalf("error = %q, want %q", lerr.Error, tc.want)
			}
			if lerr.Index != 0 || lerr.Conflict != nil {
				t.Fatalf("query failure must not carry index/conflict, got %+v", lerr)
			}
		})
	}

	// 失败查询不改变已存数据。
	res := mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":0,"end":2000}`)
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("data changed by failed queries: %+v", res.Series)
	}
}

// 统一入口 ProcessLine 把 query_points 对象分派为 *QueryPointsResult，
// 失败时仍返回真正的 nil 结果。
func TestProcessLineDispatchesQueryPoints(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":2}]`)

	r, lerr := store.ProcessLine(`{"op":"query_points","name":"m","start":0,"end":10}`)
	if lerr != nil {
		t.Fatalf("ProcessLine query_points error: %+v", lerr)
	}
	qpr, ok := r.(*QueryPointsResult)
	if !ok {
		t.Fatalf("ProcessLine query_points result type = %T, want *QueryPointsResult", r)
	}
	if qpr.Op != "query_points" || len(qpr.Series) != 1 || qpr.Series[0].Points[0].Value != 2 {
		t.Fatalf("result = %+v", qpr)
	}

	r, lerr = store.ProcessLine(`{"op":"query_points","name":"m","start":10,"end":0}`)
	if r != nil || lerr == nil {
		t.Fatalf("inverted range via ProcessLine = %v, %+v; want nil result and error", r, lerr)
	}
}

// 专用入口各归各位：QueryLine 只接受均值查询，QueryPointsLine 只接受明细查询。
func TestDedicatedQueryEntriesRejectTheOtherOp(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":2}]`)

	if _, lerr := store.QueryLine(`{"op":"query_points","name":"m","start":0,"end":10}`); lerr == nil {
		t.Fatal("QueryLine accepted query_points")
	}
	if _, lerr := store.QueryPointsLine(`{"op":"query","name":"m","start":0,"end":10}`); lerr == nil {
		t.Fatal("QueryPointsLine accepted query")
	}
}

// 结果中的标签是独立副本：修改结果不回写存储。
func TestQueryPointsResultLabelsAreIndependent(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":2,"labels":{"host":"a"}}]`)

	res := mustQueryPoints(t, store, `{"op":"query_points","name":"m","start":0,"end":10}`)
	res.Series[0].Labels["host"] = "mutated"
	res.Series[0].Points[0].Value = math.NaN()

	again := mustQueryPoints(t, store, `{"op":"query_points","name":"m","start":0,"end":10}`)
	if again.Series[0].Labels["host"] != "a" || again.Series[0].Points[0].Value != 2 {
		t.Fatalf("stored data changed through earlier result: %+v", again.Series[0])
	}
}
