package darksafe

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// 本文件为 query（区间点数与均值统计）与 query_points（区间原始采样明细）
// 这一对已有行为补充交叉复核的回归保障：用户在同一份已成功写入的数据上发送
// query，再仅把 op 改成 query_points，指标名、标签条件与起止时间完全相同；
// 两次结果必须描述同一组采样——序列身份与排列次序一致，统计数字能与明细逐条
// 对应（count 等于明细点数，average 是明细中实际存储 float64 值的精确算术平均
// 按最近偶数舍入后的 float64），区间外的点与同名异标签的序列都不能混入。
//
// 采样数据刻意选用能区分下列实现偏差的取值：
//   - 朴素 float64 求和把正负大数中间的小值“吞掉”（1e16、1、-1e16）；
//   - 把同名指标的不同标签序列混在一起统计；
//   - 让区间外的点参与某条记录的 count 与均值；
//   - 标签子集命中多条序列时丢掉额外标签或把明细/统计错配到另一条序列；
//   - 无点区间被补成 count=0、average=0 的记录。
//
// 所有断言都指出具体序列（指标名 + 完整标签集合）与具体查询区间，便于定位
// 后续修改造成的查询偏差。

// queryTwin 是除 op 外完全相同的一对查询：先统计（query），再把 op 改成
// query_points 取得同一区间的采样明细。
type queryTwin struct {
	name   string
	start  int64
	end    int64
	labels map[string]string // nil 表示省略 labels（匹配该指标全部序列）
}

// rangeText 以 [start,end] + 标签条件描述一次查询区间，用于失败信息。
func (q queryTwin) rangeText() string {
	if q.labels == nil {
		return fmt.Sprintf("[%d,%d] (labels omitted)", q.start, q.end)
	}
	return fmt.Sprintf("[%d,%d] labels=%s", q.start, q.end, jsonLabels(q.labels))
}

// statsLine / pointsLine 分别构造两次查询的 JSON 文本；两者除 op 外逐字一致。
func (q queryTwin) statsLine() string {
	return `{"op":"query","name":` + jsonString(q.name) +
		fmt.Sprintf(`,"start":%d,"end":%d`, q.start, q.end) + q.labelsSuffix() + `}`
}

func (q queryTwin) pointsLine() string {
	return `{"op":"query_points","name":` + jsonString(q.name) +
		fmt.Sprintf(`,"start":%d,"end":%d`, q.start, q.end) + q.labelsSuffix() + `}`
}

func (q queryTwin) labelsSuffix() string {
	if q.labels == nil {
		return ""
	}
	return `,"labels":` + jsonLabels(q.labels)
}

// seriesKey 是结果条目的序列身份（指标名 + 完整标签集合），
// 标签经排序后拼接，使不同书写次序的同一身份得到同一个键。
// display 不参与判等，只用于在失败信息中渲染 cpu{host=a} 这样的可读身份。
type seriesKey struct {
	name    string
	sig     string
	display string
}

func keyOf(name string, labels map[string]string) seriesKey {
	return seriesKey{
		name:    name,
		sig:     makeSeriesID(name, labels).sig,
		display: SeriesRef{Name: name, Labels: labels}.String(),
	}
}

func (k seriesKey) String() string {
	return k.display
}

// exactMeanOfPoints 按现有约定独立复算明细点的均值：把每个实际存储的 float64
// 精确纳入 big.Rat，精确求和后除以点数，再舍入到最近可表示 float64（正中取偶）。
// 测试不直接调用业务代码的求和路径，以便在统计实现出现偏差时仍以明细为准复核。
func exactMeanOfPoints(points []Point) float64 {
	sum := new(big.Rat)
	r := new(big.Rat)
	for _, p := range points {
		sum.Add(sum, r.SetFloat64(p.Value))
	}
	sum.Quo(sum, new(big.Rat).SetInt64(int64(len(points))))
	return ratToFloat64NearestEven(sum)
}

// assertEmptyTwin 断言两种查询在该区间都成功且都给出空的 series 数组：
// 未命中任何区间内采样时不能把没有点的序列补成零点数、零均值的记录。
// note 给出该空区间的语义（指标不存在 / 标签未命中 / 区间内无点）。
func assertEmptyTwin(t *testing.T, store *MetricStore, q queryTwin, note string) {
	t.Helper()
	stats := mustQuery(t, store, q.statsLine())
	if stats.Status != "ok" || stats.Op != "query" {
		t.Fatalf("%s: stats query %s envelope = %+v, want ok/query", note, q.rangeText(), stats)
	}
	if stats.Series == nil || len(stats.Series) != 0 {
		t.Fatalf("%s: stats query %s series = %+v, want non-nil empty array", note, q.rangeText(), stats.Series)
	}
	points := mustQueryPoints(t, store, q.pointsLine())
	if points.Status != "ok" || points.Op != "query_points" {
		t.Fatalf("%s: points query %s envelope = %+v, want ok/query_points", note, q.rangeText(), points)
	}
	if points.Series == nil || len(points.Series) != 0 {
		t.Fatalf("%s: points query %s series = %+v, want non-nil empty array", note, q.rangeText(), points.Series)
	}
	if raw := marshalCompact(t, points); raw != `{"status":"ok","op":"query_points","series":[]}` {
		t.Fatalf("%s: points query %s JSON = %s, want empty series array", note, q.rangeText(), raw)
	}
}

// assertStatsMatchPoints 是本文件的核心复核：对同一区间分别执行 query 与
// query_points，逐条序列核对两次结果描述的是同一组采样。
//
//   - 两次结果的序列身份（指标名 + 完整标签集合）集合相同、排列次序一致，
//     记录中的标签必须是该序列的完整标签集合，不能缩减成查询条件；
//   - 明细按时间戳严格升序，且每个点确实落在查询闭区间 [start,end] 内；
//   - 统计记录的 count 等于对应明细的采样点数量；
//   - average 既等于按明细实际存储 float64 值精确复算后最近偶数舍入的结果，
//     也等于这些值用 float64 直接解释时的期望值（调用方在 wantAvg 中给出，
//     如 1e16/1/-1e16 三点为 0.3333333333333333），双重比对防求和偏差；
//   - 同名异标签序列之间不混算：每条记录只由自己明细中的点复核。
//
// wantAvg 以序列身份为键给出业务期望均值；identity 为 true 时额外要求
// average 与单点值同一位模式（start==end 场景）。
func assertStatsMatchPoints(t *testing.T, store *MetricStore, q queryTwin, wantAvg map[seriesKey]float64) {
	t.Helper()
	stats := mustQuery(t, store, q.statsLine())
	points := mustQueryPoints(t, store, q.pointsLine())

	if stats.Status != "ok" || stats.Op != "query" {
		t.Fatalf("stats query %s envelope = %+v, want ok/query", q.rangeText(), stats)
	}
	if points.Status != "ok" || points.Op != "query_points" {
		t.Fatalf("points query %s envelope = %+v, want ok/query_points", q.rangeText(), points)
	}
	if len(stats.Series) != len(points.Series) {
		t.Fatalf("range %s: stats lists %d series %+v but points lists %d series %+v; the two queries must describe the same samples",
			q.rangeText(), len(stats.Series), stats.Series, len(points.Series), points.Series)
	}

	statsByKey := make(map[seriesKey]QuerySeries, len(stats.Series))
	pointsByKey := make(map[seriesKey]QueryPointsSeries, len(points.Series))
	for i := range stats.Series {
		s := stats.Series[i]
		statsByKey[keyOf(s.Name, s.Labels)] = s
	}
	for i := range points.Series {
		s := points.Series[i]
		pointsByKey[keyOf(s.Name, s.Labels)] = s
	}

	// 逐位置核对：身份与排列次序在两次结果间必须完全一致。
	for i := range stats.Series {
		qs := stats.Series[i]
		ps := points.Series[i]
		statKey := keyOf(qs.Name, qs.Labels)
		pointKey := keyOf(ps.Name, ps.Labels)
		if statKey != pointKey {
			t.Fatalf("range %s position %d: stats identity %s != points identity %s; series identity/ordering must match",
				q.rangeText(), i, statKey, pointKey)
		}
		if qs.Name != ps.Name {
			t.Fatalf("range %s position %d: name %q vs %q", q.rangeText(), i, qs.Name, ps.Name)
		}
		// 标签是该序列的完整标签集合，两次结果都不能缩减成查询条件。
		if !reflect.DeepEqual(qs.Labels, ps.Labels) {
			t.Fatalf("range %s series %s: stats labels %v != points labels %v",
				q.rangeText(), statKey, qs.Labels, ps.Labels)
		}
		if _, dup := statsByKey[statKey]; !dup {
			t.Fatalf("range %s: stats series %s missing from keyed view (impossible)", q.rangeText(), statKey)
		}
		if _, other := pointsByKey[statKey]; !other {
			t.Fatalf("range %s: stats series %s has no matching points record; distinct label series must not be merged",
				q.rangeText(), statKey)
		}
		// 子集查询时记录必须保留查询条件之外的额外标签。
		if q.labels != nil && len(qs.Labels) < len(q.labels) {
			t.Fatalf("range %s series %s: result labels %v are narrower than the queried subset %v",
				q.rangeText(), statKey, qs.Labels, q.labels)
		}
		for k, v := range q.labels {
			if got, ok := qs.Labels[k]; !ok || got != v {
				t.Fatalf("range %s series %s: result labels %v lost queried condition %s=%s",
					q.rangeText(), statKey, qs.Labels, k, v)
			}
		}

		// 明细核对：非空、严格时间戳升序，且全部落在闭区间 [start,end] 内。
		if len(ps.Points) == 0 {
			t.Fatalf("range %s series %s appears in results but its points detail is empty; "+
				"series without in-range points must be omitted", q.rangeText(), statKey)
		}
		for j, p := range ps.Points {
			if p.Timestamp < q.start || p.Timestamp > q.end {
				t.Fatalf("range %s series %s: detail point %d at timestamp %d with value %v lies outside the queried closed range",
					q.rangeText(), statKey, j, p.Timestamp, p.Value)
			}
			if j > 0 && p.Timestamp <= ps.Points[j-1].Timestamp {
				t.Fatalf("range %s series %s: detail points must be strictly timestamp-ascending, got %d after %d",
					q.rangeText(), statKey, p.Timestamp, ps.Points[j-1].Timestamp)
			}
			if math.IsInf(p.Value, 0) || math.IsNaN(p.Value) {
				t.Fatalf("range %s series %s: stored detail value at %d is non-finite %v",
					q.rangeText(), statKey, p.Timestamp, p.Value)
			}
		}

		// count 必须等于明细点数。
		if qs.Count != len(ps.Points) {
			t.Fatalf("range %s series %s: stats count = %d but detail lists %d points %+v",
				q.rangeText(), statKey, qs.Count, len(ps.Points), ps.Points)
		}

		// average 必须能由明细逐条复算：以明细实际存储的 float64 值精确求和、
		// 精确除以点数后按最近偶数舍入。
		wantFromPoints := exactMeanOfPoints(ps.Points)
		if math.IsNaN(qs.Average) || math.IsInf(qs.Average, 0) {
			t.Fatalf("range %s series %s: average must be finite, got %v", q.rangeText(), statKey, qs.Average)
		}
		if qs.Average != wantFromPoints {
			t.Fatalf("range %s series %s: average = %v but exact arithmetic mean of the %d detail values %v rounds to %v",
				q.rangeText(), statKey, qs.Average, len(ps.Points), ps.Points, wantFromPoints)
		}
		// 与调用方给出的业务期望再比对一次，使错误信息能直接指出预期统计数字
		//（如正负大数抵消后中间的 1 不能丢失）。
		if want, specified := wantAvg[statKey]; specified && qs.Average != want {
			t.Fatalf("range %s series %s: average = %v, want %v from detail %+v",
				q.rangeText(), statKey, qs.Average, want, ps.Points)
		}
	}

	// 反向核对：明细中每条序列也必须能在统计结果里找到，次序已在上面逐位置保证。
	for k := range pointsByKey {
		if _, ok := statsByKey[k]; !ok {
			t.Fatalf("range %s: points series %s has no matching stats record; the two queries must describe the same samples",
				q.rangeText(), k)
		}
	}
}

// TestQueryStatsMatchPointsCancellation 是任务书指定的核心采样：同一序列
// （cpu，host=a）在 1000、2000、3000 分别存入 1e16、1、-1e16。三点区间的明细
// 按时间升序保留这三个值；统计 count=3、average=0.3333333333333333——
// 精确总和为 1（正负大数抵消不能吞掉中间的 1），1/3 舍入到最近 float64。
// 区间外（0 与 4000）再各放一个点，同名异标签的另一条序列（host=b）也放三个点，
// 它们都不能改变 host=a 这条记录的结果。
func TestQueryStatsMatchPointsCancellation(t *testing.T) {
	store := NewMetricStore()
	hostA := map[string]string{"host": "a"}
	hostB := map[string]string{"host": "b"}
	mustOK(t, store, "["+
		sample("cpu", 0, 42, hostA)+","+ // 区间外（早于 1000）
		sample("cpu", 1000, 1e16, hostA)+","+
		sample("cpu", 2000, 1, hostA)+","+
		sample("cpu", 3000, -1e16, hostA)+","+
		sample("cpu", 4000, 77, hostA)+","+ // 区间外（晚于 3000）
		sample("cpu", 1000, 5, hostB)+","+
		sample("cpu", 2000, 7, hostB)+","+
		sample("cpu", 3000, 9, hostB)+"]") // 同名异标签，绝不能混入 host=a 的统计

	full := queryTwin{name: "cpu", start: 1000, end: 3000, labels: hostA}
	assertStatsMatchPoints(t, store, full, map[seriesKey]float64{
		keyOf("cpu", hostA): 1.0 / 3.0,
	})

	// 直接锁定统计数字与明细内容，使失败信息精确指出偏差所在。
	stats := mustQuery(t, store, full.statsLine())
	if len(stats.Series) != 1 {
		t.Fatalf("host=a stats = %+v, want exactly one record", stats.Series)
	}
	s0 := stats.Series[0]
	if s0.Count != 3 {
		t.Fatalf("range %s cpu{host=a} count = %d, want 3 (out-of-range points at 0/4000 must not count)",
			full.rangeText(), s0.Count)
	}
	if s0.Average != 1.0/3.0 {
		t.Fatalf("range %s cpu{host=a} average = %v, want %v", full.rangeText(), s0.Average, 1.0/3.0)
	}
	if raw := marshalCompact(t, s0.Average); raw != "0.3333333333333333" {
		t.Fatalf("cpu{host=a} average JSON = %s, want 0.3333333333333333", raw)
	}
	detail := mustQueryPoints(t, store, full.pointsLine())
	if !reflect.DeepEqual(detail.Series[0].Points, []Point{
		{Timestamp: 1000, Value: 1e16},
		{Timestamp: 2000, Value: 1},
		{Timestamp: 3000, Value: -1e16},
	}) {
		t.Fatalf("range %s cpu{host=a} detail = %+v, want ascending 1e16,1,-1e16",
			full.rangeText(), detail.Series[0].Points)
	}

	// 仅缩窄区间：端点 1000、3000 分别包含，区间外的点依旧不参与。
	left := queryTwin{name: "cpu", start: 1000, end: 1000, labels: hostA}
	assertStatsMatchPoints(t, store, left, map[seriesKey]float64{keyOf("cpu", hostA): 1e16})
	mid := queryTwin{name: "cpu", start: 2000, end: 2000, labels: hostA}
	assertStatsMatchPoints(t, store, mid, map[seriesKey]float64{keyOf("cpu", hostA): 1})
	right := queryTwin{name: "cpu", start: 3000, end: 3000, labels: hostA}
	assertStatsMatchPoints(t, store, right, map[seriesKey]float64{keyOf("cpu", hostA): -1e16})

	// 开区间外紧邻位置：start=end=4000 只包含 4000 自己；区间 (1000,3000)
	// 这样的窄缝只有中间一个点；0..999 与 3001..3999 都不含任何点。
	gap := queryTwin{name: "cpu", start: 1001, end: 2999, labels: hostA}
	assertStatsMatchPoints(t, store, gap, map[seriesKey]float64{keyOf("cpu", hostA): 1})
	at4000 := queryTwin{name: "cpu", start: 4000, end: 4000, labels: hostA}
	assertStatsMatchPoints(t, store, at4000, map[seriesKey]float64{keyOf("cpu", hostA): 77})
	assertEmptyTwin(t, store, queryTwin{name: "cpu", start: 1, end: 999, labels: hostA},
		"gap before the first in-range point")
	assertEmptyTwin(t, store, queryTwin{name: "cpu", start: 3001, end: 3999, labels: hostA},
		"gap between 3000 and the out-of-range 4000 point")

	// host=b 是同名异标签的独立序列：同样的三点区间给出它自己的明细与均值 (5+7+9)/3=7，
	// 不能与 host=a 的任何点混算。
	fullB := queryTwin{name: "cpu", start: 1000, end: 3000, labels: hostB}
	assertStatsMatchPoints(t, store, fullB, map[seriesKey]float64{
		keyOf("cpu", hostB): 7,
	})
	statsB := mustQuery(t, store, fullB.statsLine())
	if len(statsB.Series) != 1 || !sameLabels(statsB.Series[0].Labels, hostB) ||
		statsB.Series[0].Count != 3 || statsB.Series[0].Average != 7 {
		t.Fatalf("cpu{host=b} over %s = %+v, want count=3 average=7 computed from its own points",
			fullB.rangeText(), statsB.Series)
	}

	// 省略标签条件：两条序列都命中且各自独立复核，排列次序与两种查询保持一致。
	all := queryTwin{name: "cpu", start: 1000, end: 3000}
	assertStatsMatchPoints(t, store, all, map[seriesKey]float64{
		keyOf("cpu", hostA): 1.0 / 3.0,
		keyOf("cpu", hostB): 7,
	})
	both := mustQuery(t, store, all.statsLine())
	if len(both.Series) != 2 {
		t.Fatalf("omitted-labels stats = %+v, want both host=a and host=b", both.Series)
	}
	if !sameLabels(both.Series[0].Labels, hostA) || !sameLabels(both.Series[1].Labels, hostB) {
		t.Fatalf("omitted-labels order = %+v, want host=a then host=b", both.Series)
	}
}

// TestQueryStatsMatchPointsSubsetMultiSeries 标签子集查询命中多条序列时，
// 各条明细与统计仍分别对应：返回记录保留额外标签，count/均值只由本序列
// 区间内的点决定。数据刻意让不同序列在同一时间戳存不同值，并在区间外放点，
// 以便暴露序列混算与区间泄漏。
func TestQueryStatsMatchPointsSubsetMultiSeries(t *testing.T) {
	store := NewMetricStore()
	// 三条序列都带 zone=x（子集条件），其中两条还带额外标签；另一条 zone=y
	// 不应被命中；无标签序列也不应被命中。
	zx := map[string]string{"zone": "x"}
	hx := map[string]string{"host": "h", "zone": "x"}
	kx := map[string]string{"kind": "k", "zone": "x"}
	zy := map[string]string{"zone": "y"}
	mustOK(t, store, "["+
		// zone=x,host=h：区间内两点 10->2、30->4；区间外 50->100。
		sample("m", 10, 2, hx)+","+
		sample("m", 30, 4, hx)+","+
		sample("m", 50, 100, hx)+","+
		// zone=x（无额外标签）：区间内一点 20->8；区间外 0->200。
		sample("m", 0, 200, zx)+","+
		sample("m", 20, 8, zx)+","+
		// kind=k,zone=x：区间内三点 10->1e16、20->1、30->-1e16（抵消场景）。
		sample("m", 10, 1e16, kx)+","+
		sample("m", 20, 1, kx)+","+
		sample("m", 30, -1e16, kx)+","+
		// 不命中子集的序列。
		sample("m", 10, 9, zy)+","+
		sample("m", 20, 3, nil)+"]")

	q := queryTwin{name: "m", start: 10, end: 40, labels: map[string]string{"zone": "x"}}
	assertStatsMatchPoints(t, store, q, map[seriesKey]float64{
		keyOf("m", hx): 3,         // (2+4)/2
		keyOf("m", kx): 1.0 / 3.0, // (1e16+1-1e16)/3
		keyOf("m", zx): 8,         // 单点
	})

	stats := mustQuery(t, store, q.statsLine())
	if len(stats.Series) != 3 {
		t.Fatalf("subset stats over %s = %+v, want exactly the 3 zone=x series", q.rangeText(), stats.Series)
	}
	// 排列次序：按完整标签键值对字典序，host=h,zone=x 在 kind=k,zone=x 前，
	// 仅 zone=x 的最短集合最后。
	wantOrder := []map[string]string{hx, kx, zx}
	for i, wantLabels := range wantOrder {
		if !sameLabels(stats.Series[i].Labels, wantLabels) {
			t.Fatalf("subset stats position %d labels = %v, want full set %v (extra labels must be preserved)",
				i, stats.Series[i].Labels, wantLabels)
		}
	}
	wantCounts := map[string]int{labelSetText(hx): 2, labelSetText(kx): 3, labelSetText(zx): 1}
	for _, s := range stats.Series {
		key := labelSetText(s.Labels)
		if s.Count != wantCounts[key] {
			t.Fatalf("subset range %s series m%s count = %d, want %d", q.rangeText(), key, s.Count, wantCounts[key])
		}
	}

	// 明细与统计逐序列锁定。
	detail := mustQueryPoints(t, store, q.pointsLine())
	if len(detail.Series) != 3 {
		t.Fatalf("subset points = %+v, want 3 records", detail.Series)
	}
	wantPoints := map[string][]Point{
		labelSetText(hx): {{Timestamp: 10, Value: 2}, {Timestamp: 30, Value: 4}},
		labelSetText(kx): {
			{Timestamp: 10, Value: 1e16},
			{Timestamp: 20, Value: 1},
			{Timestamp: 30, Value: -1e16},
		},
		labelSetText(zx): {{Timestamp: 20, Value: 8}},
	}
	for _, s := range detail.Series {
		key := labelSetText(s.Labels)
		if !reflect.DeepEqual(s.Points, wantPoints[key]) {
			t.Fatalf("subset points series m%s = %+v, want %+v", key, s.Points, wantPoints[key])
		}
	}

	// 空标签对象与省略等价，也命中同样三条序列。
	qEmpty := queryTwin{name: "m", start: 10, end: 40, labels: map[string]string{}}
	assertStatsMatchPoints(t, store, qEmpty, map[seriesKey]float64{
		keyOf("m", hx):  3,
		keyOf("m", kx):  1.0 / 3.0,
		keyOf("m", zx):  8,
		keyOf("m", zy):  9, // (9)/1，区间内只有 10->9
		keyOf("m", nil): 3,
	})
}

// labelSetText 以稳定文本表示一组标签（JSON 按键排序），用作测试内 map 键。
func labelSetText(labels map[string]string) string {
	b, _ := json.Marshal(labels) // nil map 也序列化为 {}
	return string(b)
}

// TestQueryStatsMatchPointsSingleTimestamp 起止时间相等且该位置有点时：
// 明细只列出那个点（按时间升序的单元素列表），统计 count=1、average 等于
// 该点实际存储的 float64 值；该序列其他时间戳上的点与同名异标签序列同位置
// 的点都不能参与。无点的起止相等位置两种查询都成功返回空 series。
func TestQueryStatsMatchPointsSingleTimestamp(t *testing.T) {
	store := NewMetricStore()
	hostA := map[string]string{"host": "a"}
	hostB := map[string]string{"host": "b"}
	mustOK(t, store, "["+
		sample("cpu", 1000, 2.5, hostA)+","+
		sample("cpu", 2000, 4, hostA)+","+
		sample("cpu", 1000, 9, hostB)+"]")

	for _, tc := range []struct {
		ts   int64
		want float64
	}{
		{1000, 2.5},
		{2000, 4},
	} {
		q := queryTwin{name: "cpu", start: tc.ts, end: tc.ts, labels: hostA}
		assertStatsMatchPoints(t, store, q, map[seriesKey]float64{keyOf("cpu", hostA): tc.want})
		stats := mustQuery(t, store, q.statsLine())
		if len(stats.Series) != 1 || stats.Series[0].Count != 1 || stats.Series[0].Average != tc.want {
			t.Fatalf("single-timestamp range %s cpu{host=a} = %+v, want count=1 average=%v",
				q.rangeText(), stats.Series, tc.want)
		}
		detail := mustQueryPoints(t, store, q.pointsLine())
		if len(detail.Series) != 1 ||
			!reflect.DeepEqual(detail.Series[0].Points, []Point{{Timestamp: tc.ts, Value: tc.want}}) {
			t.Fatalf("single-timestamp range %s cpu{host=a} detail = %+v, want only (%d,%v)",
				q.rangeText(), detail.Series, tc.ts, tc.want)
		}
	}

	// 同位置但标签不同的序列互不干扰。
	qb := queryTwin{name: "cpu", start: 1000, end: 1000, labels: hostB}
	assertStatsMatchPoints(t, store, qb, map[seriesKey]float64{keyOf("cpu", hostB): 9})

	// 起止相等但该位置没有点：空 series，而不是 count=1 之外的零值记录。
	assertEmptyTwin(t, store,
		queryTwin{name: "cpu", start: 1500, end: 1500, labels: hostA},
		"start == end at an empty timestamp")
}

// TestQueryStatsMatchPointsEmptyRanges 条件未命中任何区间内采样时，两种查询
// 都成功返回空 series 数组：指标不存在、标签条件未命中、区间在所有点之外，
// 以及起止相等且无点。不能把没有点的序列补成 count=0、average=0 的记录。
func TestQueryStatsMatchPointsEmptyRanges(t *testing.T) {
	store := NewMetricStore()
	hostA := map[string]string{"host": "a"}
	mustOK(t, store, "["+
		sample("cpu", 1000, 1, hostA)+","+
		sample("cpu", 2000, 1, hostA)+","+
		sample("cpu", 1000, 5, map[string]string{"host": "b"})+"]")

	cases := []struct {
		note string
		q    queryTwin
	}{
		{"unknown metric", queryTwin{name: "nope", start: 0, end: 10000}},
		{"label value miss", queryTwin{name: "cpu", start: 0, end: 10000, labels: map[string]string{"host": "zzz"}}},
		{"missing label key", queryTwin{name: "cpu", start: 0, end: 10000, labels: map[string]string{"zone": "x"}}},
		{"range strictly before all points", queryTwin{name: "cpu", start: 0, end: 999, labels: hostA}},
		{"range strictly after all points", queryTwin{name: "cpu", start: 2001, end: 9999, labels: hostA}},
		{"gap between points", queryTwin{name: "cpu", start: 1001, end: 1999, labels: hostA}},
		{"empty timestamp start == end", queryTwin{name: "cpu", start: 1500, end: 1500, labels: hostA}},
		{"full int64 range over unknown metric", queryTwin{name: "ghost", start: tsMinInt64, end: tsMaxInt64}},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			assertEmptyTwin(t, store, tc.q, tc.note)
		})
	}

	// 空存储上的任意查询同样双双为空。
	empty := NewMetricStore()
	assertEmptyTwin(t, empty,
		queryTwin{name: "cpu", start: tsMinInt64, end: tsMaxInt64}, "fresh empty store")
}

// TestQueryStatsMatchPointsAcrossRanges 用同一存储扫描一组区间，每个区间都做
// 双查询交叉复核，确保区间边界（含两个端点）、多条同名序列与多种取值规模
// （1e308 溢出场景、0.1 这类十进制不可精表示值、居中取偶的相邻 float64）
// 下统计始终由且仅由区间内本序列的明细点决定。
func TestQueryStatsMatchPointsAcrossRanges(t *testing.T) {
	store := NewMetricStore()
	z1 := map[string]string{"z": "1"}
	z2 := map[string]string{"z": "2"}
	mustOK(t, store, "["+
		// 无标签序列：两个 1e308，区间取 1..2 时均值仍有限；区间外再放一个 1e308。
		`{"name":"m","timestamp":1,"value":1e308},`+
		`{"name":"m","timestamp":2,"value":1e308},`+
		`{"name":"m","timestamp":9,"value":1e308},`+
		// z=1：0.1 与 0.2，均值按存储值精确计算（不是朴素的 0.15 字面量）。
		sample("m", 1, 0.1, z1)+","+
		sample("m", 2, 0.2, z1)+","+
		sample("m", 3, 99, z1)+","+
		// z=2：居中取偶对（1 与 1.0000000000000002，精确平均正中 -> 1），
		// 再加一个区间外点，窄区间只取前两点时必须取偶为 1。
		sample("m", 1, 1, z2)+","+
		sample("m", 2, 1.0000000000000002, z2)+","+
		sample("m", 3, 1000, z2)+"]")

	// [1,2]：三条序列各自独立；无标签序列两个 1e308 的均值仍有限，
	// z=2 精确平均恰在相邻 float64 正中，取偶为 1。
	q := queryTwin{name: "m", start: 1, end: 2}
	assertStatsMatchPoints(t, store, q, map[seriesKey]float64{
		keyOf("m", nil): 1e308,
		keyOf("m", z2):  1,
	})
	// z=1 的均值以明细实际存储值独立锁定：朴素写法 0.1+0.2 本就不精确，
	// 精确平均按约定舍入为 0.15000000000000002，这里给出明确字面量防回归。
	stats := mustQuery(t, store, q.statsLine())
	detail := mustQueryPoints(t, store, q.pointsLine())
	z1Stats := stats.Series[findQueryIndex(t, stats, "m", z1)]
	z1Detail := detail.Series[findPointsIndex(t, detail, "m", z1)]
	if z1Stats.Count != 2 {
		t.Fatalf("m{z=1} count = %d, want 2", z1Stats.Count)
	}
	if want := exactMeanOfPoints(z1Detail.Points); z1Stats.Average != want {
		t.Fatalf("m{z=1} average = %v, want exact mean %v of stored detail %+v",
			z1Stats.Average, want, z1Detail.Points)
	}
	if b := marshalCompact(t, z1Stats.Average); b != "0.15000000000000002" {
		t.Fatalf("m{z=1} average JSON = %s, want 0.15000000000000002", b)
	}

	// 单点端点包含：[1,1]、[2,2] 各自只统计该位置；区间外的 ts=9/ts=3 点不参与。
	assertStatsMatchPoints(t, store, queryTwin{name: "m", start: 1, end: 1}, map[seriesKey]float64{
		keyOf("m", nil): 1e308,
		keyOf("m", z1):  0.1,
		keyOf("m", z2):  1,
	})
	assertStatsMatchPoints(t, store, queryTwin{name: "m", start: 2, end: 2}, map[seriesKey]float64{
		keyOf("m", nil): 1e308,
		keyOf("m", z1):  0.2,
		keyOf("m", z2):  1.0000000000000002,
	})

	// 只查 z=1 且区间扩展到 3：第三点 99 参与；z=1 窄区间 [2,3] 只有 0.2 与 99。
	// 期望值只由明细复算把关（区间泄漏或序列混算都会立刻体现为 count/均值不符）。
	assertStatsMatchPoints(t, store, queryTwin{name: "m", start: 1, end: 3, labels: z1}, nil)
	z1Three := mustQuery(t, store, queryTwin{name: "m", start: 1, end: 3, labels: z1}.statsLine())
	if z1Three.Series[0].Count != 3 || z1Three.Series[0].Average != 33.1 {
		t.Fatalf("m{z=1} [1,3] = %+v, want count=3 average=33.1", z1Three.Series)
	}
	assertStatsMatchPoints(t, store, queryTwin{name: "m", start: 2, end: 3, labels: z1}, nil)
	z1Two := mustQuery(t, store, queryTwin{name: "m", start: 2, end: 3, labels: z1}.statsLine())
	if z1Two.Series[0].Count != 2 || z1Two.Series[0].Average != 49.6 {
		t.Fatalf("m{z=1} [2,3] = %+v, want count=2 average=49.6", z1Two.Series)
	}

	// 无标签序列的 ts=9 区间外点只在它自己的区间里出现，不能进入 [1,2]；
	// [4,8] 对三条序列都没有点（z=1/z=2 的点在 3，无标签序列的点在 9）。
	assertEmptyTwin(t, store, queryTwin{name: "m", start: 4, end: 8},
		"gap between timestamp 3 points and the unlabeled outlier at 9")
	assertStatsMatchPoints(t, store, queryTwin{name: "m", start: 9, end: 9},
		map[seriesKey]float64{keyOf("m", nil): 1e308})
}

// TestQueryStatsMatchPointsResultShapes 交叉复核时锁定两种结果各自的字段形状：
// 统计记录带 count/average、明细记录带 points 且二者的 labels 都序列化为完整
// 标签集合；同一条件两次查询的序列条目数与次序一致。该检查防止后续修改把
// 两种结果格式合并或缩减标签。
func TestQueryStatsMatchPointsResultShapes(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("cpu", 1000, 2, map[string]string{"host": "a", "zone": "x"})+","+
		sample("cpu", 2000, 4, map[string]string{"host": "a", "zone": "x"})+","+
		sample("cpu", 1000, 8, map[string]string{"host": "b"})+"]")

	q := queryTwin{name: "cpu", start: 0, end: 3000}
	stats := mustQuery(t, store, q.statsLine())
	detail := mustQueryPoints(t, store, q.pointsLine())
	if len(stats.Series) != len(detail.Series) || len(stats.Series) != 2 {
		t.Fatalf("range %s: stats=%+v points=%+v, want the same 2 series in both",
			q.rangeText(), stats.Series, detail.Series)
	}
	for i := range stats.Series {
		if keyOf(stats.Series[i].Name, stats.Series[i].Labels) !=
			keyOf(detail.Series[i].Name, detail.Series[i].Labels) {
			t.Fatalf("range %s position %d identity differs: stats %+v vs points %+v",
				q.rangeText(), i, stats.Series[i], detail.Series[i])
		}
	}

	statsJSON := marshalCompact(t, stats)
	pointsJSON := marshalCompact(t, detail)
	// 统计结果不含 points，明细结果不含 count/average。
	var statsTop map[string]any
	if err := json.Unmarshal([]byte(statsJSON), &statsTop); err != nil {
		t.Fatal(err)
	}
	for _, raw := range statsTop["series"].([]any) {
		entry := raw.(map[string]any)
		if _, ok := entry["points"]; ok {
			t.Fatalf("stats entry must not carry points: %v", entry)
		}
		if _, ok := entry["count"]; !ok {
			t.Fatalf("stats entry must carry count: %v", entry)
		}
		if _, ok := entry["average"]; !ok {
			t.Fatalf("stats entry must carry average: %v", entry)
		}
	}
	var pointsTop map[string]any
	if err := json.Unmarshal([]byte(pointsJSON), &pointsTop); err != nil {
		t.Fatal(err)
	}
	for _, raw := range pointsTop["series"].([]any) {
		entry := raw.(map[string]any)
		if _, ok := entry["count"]; ok {
			t.Fatalf("points entry must not carry count: %v", entry)
		}
		if _, ok := entry["average"]; ok {
			t.Fatalf("points entry must not carry average: %v", entry)
		}
		if _, ok := entry["points"]; !ok {
			t.Fatalf("points entry must carry points: %v", entry)
		}
	}
	// 子集查询时完整标签集合（含未查询的 zone 键）必须出现在两种结果里。
	sub := queryTwin{name: "cpu", start: 0, end: 3000, labels: map[string]string{"host": "a"}}
	subStats := marshalCompact(t, mustQuery(t, store, sub.statsLine()))
	subPoints := marshalCompact(t, mustQueryPoints(t, store, sub.pointsLine()))
	for _, raw := range []string{subStats, subPoints} {
		if !strings.Contains(raw, `"zone":"x"`) {
			t.Fatalf("subset result must preserve the extra zone label: %s", raw)
		}
	}
}

// TestQueryStatsMatchPointsViaProcessLine 统一行入口 ProcessLine 上的双查询
// 同样必须对应：两次都返回各自的具体结果类型（不是带类型 nil），且统计与
// 明细逐条一致。
func TestQueryStatsMatchPointsViaProcessLine(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+
		sample("cpu", 1000, 1e16, map[string]string{"host": "a"})+","+
		sample("cpu", 2000, 1, map[string]string{"host": "a"})+","+
		sample("cpu", 3000, -1e16, map[string]string{"host": "a"})+"]")

	q := queryTwin{name: "cpu", start: 1000, end: 3000}
	r, lerr := store.ProcessLine(q.statsLine())
	if lerr != nil || r == nil {
		t.Fatalf("ProcessLine stats: r=%v lerr=%+v", r, lerr)
	}
	stats, ok := r.(*QueryResult)
	if !ok {
		t.Fatalf("ProcessLine stats type = %T, want *QueryResult", r)
	}
	r, lerr = store.ProcessLine(q.pointsLine())
	if lerr != nil || r == nil {
		t.Fatalf("ProcessLine points: r=%v lerr=%+v", r, lerr)
	}
	points, ok := r.(*QueryPointsResult)
	if !ok {
		t.Fatalf("ProcessLine points type = %T, want *QueryPointsResult", r)
	}
	if len(stats.Series) != 1 || len(points.Series) != 1 {
		t.Fatalf("ProcessLine twin = stats %+v points %+v, want one series each", stats.Series, points.Series)
	}
	qs := stats.Series[0]
	ps := points.Series[0]
	if !sameLabels(qs.Labels, ps.Labels) {
		t.Fatalf("ProcessLine twin labels differ: %v vs %v", qs.Labels, ps.Labels)
	}
	if qs.Count != len(ps.Points) || qs.Count != 3 {
		t.Fatalf("ProcessLine twin count = %d vs %d detail points, want 3", qs.Count, len(ps.Points))
	}
	if qs.Average != exactMeanOfPoints(ps.Points) || qs.Average != 1.0/3.0 {
		t.Fatalf("ProcessLine twin average = %v, want exact detail mean %v",
			qs.Average, exactMeanOfPoints(ps.Points))
	}
}
