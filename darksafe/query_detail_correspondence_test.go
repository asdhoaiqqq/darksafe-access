package darksafe

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// 本文件回归保障两种只读查询在相同条件下描述的是同一组采样：用户在同一份
// 已成功写入的数据上发送 op 为 "query" 的查询，再仅把 op 改成 "query_points"
// （指标名、标签条件、起止时间完全相同），两次结果必须逐序列对应——同一组
// 采样，统计数字能与明细逐条对应：
//
//   - 两种结果的序列身份（指标名+完整标签集合）与排列次序完全一致，
//     记录中的标签是该序列的完整标签集合，不能缩减成查询条件；
//   - 每条统计记录的 count 等于对应明细中的采样点数量；
//   - average 是明细中实际存储的 float64 值的算术平均：测试侧独立用 big.Rat
//     精确求和、精确除以点数后舍入到最近可表示 float64（正中取偶），
//     不经过生产代码的 ratToFloat64NearestEven，从而能拦住“统计侧与复核侧
//     共用同一错误舍入实现”这类双侧同源错误；
//   - 同名指标不同标签的序列分别统计，不能混在一起计算；
//   - 只有闭区间 [start,end] 内的点参与某条记录的 count/average；
//   - 条件未命中任何区间内采样时两种查询都成功返回非 nil 空数组，
//     不把没有点的序列补成 count=0、average=0 的记录。
//
// 失败信息一律指出是哪个查询区间、哪条序列（以指标名+完整标签签名）的明细或
// 统计不符合上述关系，便于定位后续修改造成的查询偏差。

// queryPair 是除 op 外完全相同的一对查询文本：用户复核时只切换 op，
// 其余条件（指标名、标签条件、起止时间）保持不变。
type queryPair struct {
	note   string // 区间/场景的人类可读说明
	query  string // op 为 "query"
	points string // op 为 "query_points"
	start  int64  // 用于失败信息指明是哪个区间
	end    int64
}

// detailMean 用与生产实现无关的方式从明细点切片复算算术平均：每个值以其实际
// 存储的 float64 经 big.Rat.SetFloat64 精确入算，精确求和、精确除以点数，
// 再由 big.Rat.Float64 按 IEEE 754 最近偶规则舍入为 float64。它不调用生产
// 代码的手写舍入 ratToFloat64NearestEven，保证统计侧与复核侧不共用实现：
// 若 runQuery 的求和或舍入出错，从 query_points 明细独立复算的结果仍正确，
// 两侧差异即被查出。
func detailMean(t *testing.T, points []Point) float64 {
	t.Helper()
	if len(points) == 0 {
		t.Fatalf("internal: detailMean called with no points")
	}
	sum := new(big.Rat)
	r := new(big.Rat)
	for _, p := range points {
		sum.Add(sum, r.SetFloat64(p.Value))
	}
	sum.Quo(sum, big.NewRat(int64(len(points)), 1))
	// 有限 float64 值的精确平均必在 float64 有限范围内（凸组合），不会得到 Inf。
	f, _ := sum.Float64()
	return f
}

// assertQueryPairCorrespond 对同一区间执行两次查询（仅 op 不同），断言：
//   - 双方都成功，status/op 正确；
//   - 序列身份（name+完整标签）与排列次序逐一相同；
//   - 每条序列：统计 count == 明细点数，average == 从明细独立复算的精确平均
//     （最近偶 float64），两侧标签相同且为完整集合；
//   - 明细点时间戳严格升序、两两不同，且每个点都落在闭区间 [start,end] 内；
//   - 两侧都不出现没有点的空记录（不补 count=0/average=0 条目）；
//   - 空结果是成功的非 nil 空数组。
//
// 每个失败信息都带区间与序列身份签名，明确指出是哪条序列的明细或统计不符。
func assertQueryPairCorrespond(t *testing.T, store *MetricStore, p queryPair) {
	t.Helper()
	stats := mustQuery(t, store, p.query)
	detail := mustQueryPoints(t, store, p.points)
	if stats.Status != "ok" || stats.Op != "query" {
		t.Fatalf("[%d,%d] %s: stats envelope = %q/%q, want ok/query",
			p.start, p.end, p.note, stats.Status, stats.Op)
	}
	if detail.Status != "ok" || detail.Op != "query_points" {
		t.Fatalf("[%d,%d] %s: detail envelope = %q/%q, want ok/query_points",
			p.start, p.end, p.note, detail.Status, detail.Op)
	}

	// 序列数量、身份与排列次序逐一相同。
	if len(stats.Series) != len(detail.Series) {
		t.Fatalf("[%d,%d] %s: query returned %d series but query_points returned %d:\nstats:  %s\ndetail: %s",
			p.start, p.end, p.note, len(stats.Series), len(detail.Series),
			statsIdentitySigs(stats.Series), detailIdentitySigs(detail.Series))
	}
	for i := range stats.Series {
		sigSt := identitySig(stats.Series[i].Name, stats.Series[i].Labels)
		sigDe := identitySig(detail.Series[i].Name, detail.Series[i].Labels)
		if sigSt != sigDe {
			t.Fatalf("[%d,%d] %s: series[%d] identity/order mismatch:\nquery        #%d = %s\nquery_points #%d = %s",
				p.start, p.end, p.note, i, i, sigSt, i, sigDe)
		}
	}

	// 逐条核对 count/average 与明细的对应关系。
	for i := range stats.Series {
		stat := stats.Series[i]
		det := detail.Series[i]
		sig := identitySig(stat.Name, stat.Labels)

		// 两种结果携带的标签集合必须相同（且各场景已另行核对它是完整集合）。
		if !sameLabels(stat.Labels, det.Labels) {
			t.Fatalf("[%d,%d] %s: %s: labels differ between results: stats=%v detail=%v",
				p.start, p.end, p.note, sig, stat.Labels, det.Labels)
		}

		// count == 明细点数。
		if stat.Count != len(det.Points) {
			t.Fatalf("[%d,%d] %s: %s: count=%d but detail lists %d points: %+v",
				p.start, p.end, p.note, sig, stat.Count, len(det.Points), det.Points)
		}
		// 没有点的序列不能被任一侧补成零记录。
		if len(det.Points) == 0 {
			t.Fatalf("[%d,%d] %s: %s: zero-point entry must not be emitted by either query",
				p.start, p.end, p.note, sig)
		}

		// 明细按时间戳严格升序，且每个点都在闭区间内。
		for j, pt := range det.Points {
			if pt.Timestamp < p.start || pt.Timestamp > p.end {
				t.Fatalf("[%d,%d] %s: %s: detail point[%d] ts=%d is outside the closed range",
					p.start, p.end, p.note, sig, j, pt.Timestamp)
			}
			if j > 0 && det.Points[j-1].Timestamp >= pt.Timestamp {
				t.Fatalf("[%d,%d] %s: %s: detail not strictly ascending at %d: %+v",
					p.start, p.end, p.note, sig, j, det.Points)
			}
		}

		// average == 从明细独立复算的结果（精确平均 → 最近偶 float64）。
		wantAvg := detailMean(t, det.Points)
		if stat.Average != wantAvg {
			t.Fatalf("[%d,%d] %s: %s: average=%v but recomputed from the detail = %v (count=%d, detail=%+v)",
				p.start, p.end, p.note, sig, stat.Average, wantAvg, len(det.Points), det.Points)
		}
	}
}

// statsIdentitySigs / detailIdentitySigs 在数量不一致的失败信息中列出两侧
// 全部序列身份，指出究竟多/少了哪条序列。
func statsIdentitySigs(xs []QuerySeries) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = identitySig(x.Name, x.Labels)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func detailIdentitySigs(xs []QueryPointsSeries) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = identitySig(x.Name, x.Labels)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// pairsFor 用同一组条件构造仅 op 不同的一对查询文本。labels 为 nil 时省略
// labels 字段（匹配该指标全部序列）；非 nil 时显式带 labels（空 map 即 {}）。
func pairsFor(name string, start, end int64, labels map[string]string) queryPair {
	note := fmt.Sprintf("metric=%q range=[%d,%d]", name, start, end)
	if labels == nil {
		return queryPair{
			note:   note + " labels=<omit>",
			query:  queryAll(name, start, end),
			points: queryPointsAll(name, start, end),
			start:  start,
			end:    end,
		}
	}
	return queryPair{
		note:   note + " labels=" + jsonLabels(labels),
		query:  queryLabels(name, start, end, labels),
		points: queryPointsLabels(name, start, end, labels),
		start:  start,
		end:    end,
	}
}

// TestQueryAndPointsDescribeSameSamplesCancellation 是任务书指定的核心采样：
// 同一序列在 1000、2000、3000 分别存入 1e16、1、-1e16。包含三个点的区间内
// 明细按时间升序保留这三个存储值，统计 count=3、average=0.3333333333333333；
// 正负大数抵消不能吞掉中间的 1。该序列在区间外（500、4000）的采样，以及
// 同名但标签不同的另一条序列（host=b），都不改变这条记录的结果。
func TestQueryAndPointsDescribeSameSamplesCancellation(t *testing.T) {
	store := NewMetricStore()
	labelsA := map[string]string{"host": "a"}
	labelsB := map[string]string{"host": "b"}
	mustOK(t, store, "["+
		sample("cpu", 500, 77, labelsA)+","+ // 区间下方之外
		sample("cpu", 1000, 1e16, labelsA)+","+
		sample("cpu", 2000, 1, labelsA)+","+
		sample("cpu", 3000, -1e16, labelsA)+","+
		sample("cpu", 4000, 88, labelsA)+","+ // 区间上方之外
		// 同名但标签不同的另一条序列，值刻意取成若被混入就会改变 host=a 的均值。
		sample("cpu", 2000, 999999, labelsB)+"]")

	pair := pairsFor("cpu", 1000, 3000, labelsA)
	stats := mustQuery(t, store, pair.query)
	detail := mustQueryPoints(t, store, pair.points)

	// 明细侧：恰好一条 host=a 序列，三个点按时间升序，值是实际存储的 float64。
	if len(detail.Series) != 1 {
		t.Fatalf("[1000,3000] host=a: detail series = %s, want exactly 1 (host=b must be excluded)",
			detailIdentitySigs(detail.Series))
	}
	gotDetail := detail.Series[0]
	if !sameLabels(gotDetail.Labels, labelsA) {
		t.Fatalf("[1000,3000] %s: detail labels = %v, want full set %v",
			identitySig("cpu", labelsA), gotDetail.Labels, labelsA)
	}
	wantPoints := []Point{
		{Timestamp: 1000, Value: 1e16},
		{Timestamp: 2000, Value: 1},
		{Timestamp: 3000, Value: -1e16},
	}
	if !reflect.DeepEqual(gotDetail.Points, wantPoints) {
		t.Fatalf("[1000,3000] %s: detail points = %+v, want ascending %+v (out-of-range 500/4000 excluded)",
			identitySig("cpu", labelsA), gotDetail.Points, wantPoints)
	}

	// 统计侧：一条 host=a 记录，count=3，average 是 1/3 的最近 float64。
	if len(stats.Series) != 1 {
		t.Fatalf("[1000,3000] host=a: stats series = %s, want exactly 1", statsIdentitySigs(stats.Series))
	}
	s0 := stats.Series[0]
	if !sameLabels(s0.Labels, labelsA) {
		t.Fatalf("[1000,3000] %s: stats labels = %v, want full set %v (must not shrink to the query filter)",
			identitySig("cpu", labelsA), s0.Labels, labelsA)
	}
	if s0.Count != 3 {
		t.Fatalf("[1000,3000] %s: count = %d, want 3", identitySig("cpu", labelsA), s0.Count)
	}
	if s0.Average != 1.0/3.0 {
		t.Fatalf("[1000,3000] %s: average = %.17g, want %.17g (1/3 nearest float64); the middle 1 must survive cancellation",
			identitySig("cpu", labelsA), s0.Average, 1.0/3.0)
	}
	b, _ := json.Marshal(s0.Average)
	if string(b) != "0.3333333333333333" {
		t.Fatalf("[1000,3000] %s: average JSON = %s, want 0.3333333333333333",
			identitySig("cpu", labelsA), b)
	}

	// 通用对应关系校验（身份/次序、count==len(points)、独立复算平均、点在区间内且升序）。
	assertQueryPairCorrespond(t, store, pair)

	// 省略标签条件：host=a 与 host=b 同时返回，序列不能混算，排列次序两侧一致。
	both := pairsFor("cpu", 1000, 3000, nil)
	assertQueryPairCorrespond(t, store, both)
	statsAll := mustQuery(t, store, both.query)
	detailAll := mustQueryPoints(t, store, both.points)
	if len(statsAll.Series) != 2 || len(detailAll.Series) != 2 {
		t.Fatalf("[1000,3000] all-labels: series count stats=%d detail=%d, want 2",
			len(statsAll.Series), len(detailAll.Series))
	}
	// host=a 仍为 3 点、均值 1/3；host=b 单点 999999。
	qa := statsAll.Series[findQueryIndex(t, statsAll, "cpu", labelsA)]
	if qa.Count != 3 || qa.Average != 1.0/3.0 {
		t.Fatalf("host=a must stay separate from host=b: stats = %+v, want count=3 average=1/3", qa)
	}
	qb := statsAll.Series[findQueryIndex(t, statsAll, "cpu", labelsB)]
	if qb.Count != 1 || qb.Average != 999999 {
		t.Fatalf("host=b stats = %+v, want count=1 average=999999", qb)
	}
	da := detailAll.Series[findPointsIndex(t, detailAll, "cpu", labelsA)]
	if len(da.Points) != 3 {
		t.Fatalf("host=a detail must have exactly the 3 in-range points, got %+v", da.Points)
	}
	db := detailAll.Series[findPointsIndex(t, detailAll, "cpu", labelsB)]
	if !reflect.DeepEqual(db.Points, []Point{{Timestamp: 2000, Value: 999999}}) {
		t.Fatalf("host=b detail = %+v, want single point (2000,999999)", db.Points)
	}

	// 缩成只含两个端点的区间 [1000,2000]：host=a 统计基于两个大数，
	// 3000 上的 -1e16 不参与，均值为 0；明细也只有两个点。
	pair2 := pairsFor("cpu", 1000, 2000, nil)
	assertQueryPairCorrespond(t, store, pair2)
}

// TestQueryAndPointsSingleTimestampRange 区间端点语义：闭区间包含两个端点；
// start == end 且该位置有点时，明细只列那个点，统计 count=1、均值等于该点
// 的实际存储值。区间只命中序列部分点时，统计只基于命中的子集。
func TestQueryAndPointsSingleTimestampRange(t *testing.T) {
	store := NewMetricStore()
	labels := map[string]string{"host": "a"}
	mustOK(t, store, "["+
		sample("cpu", 1000, 2, labels)+","+
		sample("cpu", 2000, 4, labels)+","+
		sample("cpu", 3000, 9, labels)+"]")

	// start == end，逐位置核对明细只有该点、count==1、average==存储值本身。
	for _, want := range []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 2000, Value: 4},
		{Timestamp: 3000, Value: 9},
	} {
		pair := pairsFor("cpu", want.Timestamp, want.Timestamp, labels)
		detail := mustQueryPoints(t, store, pair.points)
		if len(detail.Series) != 1 || !reflect.DeepEqual(detail.Series[0].Points, []Point{want}) {
			t.Fatalf("[%d,%d] detail = %+v, want single point %+v",
				want.Timestamp, want.Timestamp, detail.Series, want)
		}
		stats := mustQuery(t, store, pair.query)
		if stats.Series[0].Count != 1 || stats.Series[0].Average != want.Value {
			t.Fatalf("[%d,%d] stats = %+v, want count=1 average=%v (the stored value itself)",
				want.Timestamp, want.Timestamp, stats.Series[0], want.Value)
		}
		assertQueryPairCorrespond(t, store, pair)
	}

	// 闭区间两个端点都包含，且区间只命中部分点时统计只基于命中的子集。
	assertQueryPairCorrespond(t, store, pairsFor("cpu", 1000, 2000, labels))
	assertQueryPairCorrespond(t, store, pairsFor("cpu", 1000, 3000, labels))
	assertQueryPairCorrespond(t, store, pairsFor("cpu", 1500, 2500, labels))
}

// TestQueryAndPointsSubsetMatchPreservesExtraLabels 按标签子集查询命中多条
// 序列时，两种结果的序列身份/次序一致，各条明细与统计分别对应；记录里保留
// 完整标签集合（包括查询条件之外的额外标签），不缩减成查询条件。
func TestQueryAndPointsSubsetMatchPreservesExtraLabels(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":10,"value":100,"labels":{"zone":"x"}},
		{"name":"m","timestamp":20,"value":200,"labels":{"zone":"x","host":"h"}},
		{"name":"m","timestamp":30,"value":300,"labels":{"zone":"x","host":"h2"}},
		{"name":"m","timestamp":40,"value":400,"labels":{"zone":"y"}},
		{"name":"m","timestamp":10,"value":500}
	]`)

	// 子集 zone=x 命中三条序列；查询条件只有一个标签，返回必须完整保留额外标签。
	pair := pairsFor("m", 0, 1000, map[string]string{"zone": "x"})
	assertQueryPairCorrespond(t, store, pair)
	stats := mustQuery(t, store, pair.query)
	detail := mustQueryPoints(t, store, pair.points)
	// 期望次序（host 键在前；"h" 是 "h2" 的前缀排前；单标签序列最后）。
	wantFullLabels := []map[string]string{
		{"host": "h", "zone": "x"},
		{"host": "h2", "zone": "x"},
		{"zone": "x"},
	}
	if len(stats.Series) != 3 || len(detail.Series) != 3 {
		t.Fatalf("zone=x subset stats=%d detail=%d, want 3 series",
			len(stats.Series), len(detail.Series))
	}
	for i, want := range wantFullLabels {
		if !sameLabels(stats.Series[i].Labels, want) {
			t.Fatalf("zone=x stats series[%d] labels = %v, want full set %v (must not shrink to the filter)",
				i, stats.Series[i].Labels, want)
		}
		if !sameLabels(detail.Series[i].Labels, want) {
			t.Fatalf("zone=x detail series[%d] labels = %v, want full set %v",
				i, detail.Series[i].Labels, want)
		}
	}
	// 各条明细与统计分别对应：host=h 是 (20,200)，host=h2 是 (30,300)，
	// 单标签 zone=x 是 (10,100)。
	if !reflect.DeepEqual(detail.Series[0].Points, []Point{{Timestamp: 20, Value: 200}}) {
		t.Fatalf("host=h detail = %+v, want (20,200)", detail.Series[0].Points)
	}
	if !reflect.DeepEqual(detail.Series[1].Points, []Point{{Timestamp: 30, Value: 300}}) {
		t.Fatalf("host=h2 detail = %+v, want (30,300)", detail.Series[1].Points)
	}
	if !reflect.DeepEqual(detail.Series[2].Points, []Point{{Timestamp: 10, Value: 100}}) {
		t.Fatalf("zone=x detail = %+v, want (10,100)", detail.Series[2].Points)
	}

	// 进一步限定到唯一带额外标签的序列：身份、count、average、额外标签都对。
	one := pairsFor("m", 0, 1000, map[string]string{"zone": "x", "host": "h"})
	assertQueryPairCorrespond(t, store, one)
	st := mustQuery(t, store, one.query)
	if !sameLabels(st.Series[0].Labels, map[string]string{"host": "h", "zone": "x"}) {
		t.Fatalf("fully-specified subset labels = %v, want full set", st.Series[0].Labels)
	}
	if st.Series[0].Count != 1 || st.Series[0].Average != 200 {
		t.Fatalf("fully-specified subset stats = %+v, want count=1 average=200", st.Series[0])
	}

	// 区间只命中部分序列：[15,25] 内只有 host=h 的 (20,200)；host=h2(ts=30)
	// 与单标签 zone=x(ts=10) 区间内无点，都不出现。
	partial := pairsFor("m", 15, 25, map[string]string{"zone": "x"})
	assertQueryPairCorrespond(t, store, partial)
	ps := mustQuery(t, store, partial.query)
	if len(ps.Series) != 1 || !sameLabels(ps.Series[0].Labels, map[string]string{"host": "h", "zone": "x"}) {
		t.Fatalf("[15,25] zone=x = %+v, want only the host=h series", ps.Series)
	}
}

// TestQueryAndPointsNoMatchBothEmpty 条件未命中任何区间内采样时，两种查询都
// 成功返回空 series 数组（非 nil，序列化为 []），不补零点数零均值记录——
// 包括指标不存在、标签条件无命中、标签命中但区间内无点、start==end 落在
// 没有点的位置；并覆盖省略 labels 与显式 {} 两种写法。
func TestQueryAndPointsNoMatchBothEmpty(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":5,"labels":{"host":"b"}}
	]`)

	cases := []queryPair{
		pairsFor("nope", 0, 5000, nil),                              // 指标不存在（省略 labels）
		pairsFor("nope", 0, 5000, map[string]string{}),              // 指标不存在（显式 {}）
		pairsFor("cpu", 0, 5000, map[string]string{"host": "zzz"}),  // 标签值不匹配
		pairsFor("cpu", 0, 5000, map[string]string{"zone": "x"}),    // 缺少该标签键
		pairsFor("cpu", 1001, 1999, nil),                            // 区间内没有点
		pairsFor("cpu", 2001, 9000, map[string]string{"host": "a"}), // 序列存在但区间在点之后
		pairsFor("cpu", 1000, 1000, map[string]string{"host": "b"}), // 标签命中但该时刻无点
		pairsFor("cpu", 1500, 1500, nil),                            // start==end 落在空位置
	}
	for _, p := range cases {
		t.Run(p.note, func(t *testing.T) {
			assertQueryPairCorrespond(t, store, p)
			stats := mustQuery(t, store, p.query)
			detail := mustQueryPoints(t, store, p.points)
			if stats.Series == nil || len(stats.Series) != 0 {
				t.Fatalf("stats series = %+v, want non-nil empty list", stats.Series)
			}
			if detail.Series == nil || len(detail.Series) != 0 {
				t.Fatalf("detail series = %+v, want non-nil empty list", detail.Series)
			}
			if raw := marshalCompact(t, stats); raw != `{"status":"ok","op":"query","series":[]}` {
				t.Fatalf("stats JSON = %s, want empty series []", raw)
			}
			if raw := marshalCompact(t, detail); raw != `{"status":"ok","op":"query_points","series":[]}` {
				t.Fatalf("detail JSON = %s, want empty series []", raw)
			}
		})
	}
}

// TestQueryAndPointsMultiSeriesRangesTable 用表格驱动覆盖多条同名不同标签
// 序列在多种区间下的对应关系。每条区间两种查询逐序列对应，统计都能从该区间
// 的明细独立复算；不同标签序列不串算、区间外点不参与。各序列在不同区间的
// count/average 刻意互不相同且非平凡，能区分“区间过滤错”“序列混算错”
// “次序串换错”三类回归。
func TestQueryAndPointsMultiSeriesRangesTable(t *testing.T) {
	store := NewMetricStore()
	// 三条同名不同标签序列（其中一条带额外标签）+ 一个不同名指标，
	// 时间戳部分重叠，使区间过滤与序列混算造成的差异各不相同。
	mustOK(t, store, "["+
		sample("m", 10, 2, map[string]string{"host": "a"})+","+
		sample("m", 20, 4, map[string]string{"host": "a"})+","+
		sample("m", 30, 6, map[string]string{"host": "a"})+","+
		sample("m", 40, 8, map[string]string{"host": "a"})+","+
		sample("m", 10, 20, map[string]string{"host": "b"})+","+
		sample("m", 20, 40, map[string]string{"host": "b"})+","+
		sample("m", 30, 60, map[string]string{"host": "b"})+","+
		sample("m", 25, 7, map[string]string{"dc": "1", "host": "c"})+","+
		sample("other", 20, 999, nil)+"]")

	cases := []struct {
		name   string
		metric string
		start  int64
		end    int64
		labels map[string]string // nil = 省略 labels
	}{
		{"all labels full range", "m", 0, 100, nil},
		{"all labels [20,30]", "m", 20, 30, nil},
		{"all labels single ts 20", "m", 20, 20, nil},
		{"all labels single ts 25 only host=c", "m", 25, 25, nil},
		{"all labels empty interior", "m", 11, 19, nil},
		{"host=a full", "m", 0, 100, map[string]string{"host": "a"}},
		{"host=a [15,35]", "m", 15, 35, map[string]string{"host": "a"}},
		{"host=a [30,40] endpoints", "m", 30, 40, map[string]string{"host": "a"}},
		{"host=b [10,20]", "m", 10, 20, map[string]string{"host": "b"}},
		{"host=b [25,100] one point", "m", 25, 100, map[string]string{"host": "b"}},
		{"host=b empty interior", "m", 11, 19, map[string]string{"host": "b"}},
		{"host=c keeps extra label", "m", 0, 100, map[string]string{"host": "c"}},
		{"host=c with dc subset", "m", 0, 100, map[string]string{"dc": "1", "host": "c"}},
		{"host=c wrong extra is empty", "m", 0, 100, map[string]string{"dc": "2", "host": "c"}},
		{"other metric by exact name", "other", 0, 100, nil},
		{"unknown metric empty", "nope", 0, 100, map[string]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertQueryPairCorrespond(t, store, pairsFor(c.metric, c.start, c.end, c.labels))
		})
	}

	// 点名核验几条非平凡统计，确保独立复算之外再锁死具体数字：
	// [20,30] 省略 labels：host=a 三点 (4,6? 实际 20->4,30->6) 均值 5；
	// host=b 两点 (40,60) 均值 50；host=c 在 25 的值 7。
	mid := mustQuery(t, store, queryAll("m", 20, 30))
	a := mid.Series[findQueryIndex(t, mid, "m", map[string]string{"host": "a"})]
	if a.Count != 2 || a.Average != 5 {
		t.Fatalf("[20,30] host-a = %+v, want count=2 average=5", a)
	}
	bq := mid.Series[findQueryIndex(t, mid, "m", map[string]string{"host": "b"})]
	if bq.Count != 2 || bq.Average != 50 {
		t.Fatalf("[20,30] host-b = %+v, want count=2 average=50", bq)
	}
	cq := mid.Series[findQueryIndex(t, mid, "m", map[string]string{"dc": "1", "host": "c"})]
	if cq.Count != 1 || cq.Average != 7 {
		t.Fatalf("[20,30] host-c = %+v, want count=1 average=7", cq)
	}
}

// TestQueryAndPointsIndependentRecomputationSpecialMeans 补强独立复算在两类
// 非平凡均值上的对应关系：朴素 float64 求和会溢出/丢精度的情形，以及精确
// 平均恰在两个相邻可表示值正中需要取偶的情形。统计值必须仍与明细逐条对应。
func TestQueryAndPointsIndependentRecomputationSpecialMeans(t *testing.T) {
	// 三点 1e308：朴素求和得到 +Inf，精确平均仍是有限的 1e308。
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1e308},
		{"name":"m","timestamp":2,"value":1e308},
		{"name":"m","timestamp":3,"value":1e308}
	]`)
	pair := pairsFor("m", 0, 100, nil)
	assertQueryPairCorrespond(t, store, pair)
	if s := mustQuery(t, store, pair.query).Series[0]; s.Count != 3 || s.Average != 1e308 {
		t.Fatalf("overflow-range mean = %+v, want count=3 average=1e308", s)
	}

	// 1 与下一个可表示值 1.0000000000000002 的精确平均恰在正中：取偶为 1。
	// 独立复算（big.Rat.Float64 的最近偶）与生产侧必须一致。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":1.0000000000000002}
	]`)
	pair2 := pairsFor("m", 1, 2, nil)
	assertQueryPairCorrespond(t, store2, pair2)
	if s := mustQuery(t, store2, pair2.query).Series[0]; s.Count != 2 || s.Average != 1 {
		t.Fatalf("tie-to-even = %+v, want count=2 average=1", s)
	}

	// 区间外再放一个点：缩小区间后明细与统计都只用区间内两点，取偶仍为 1；
	// 区间外点既不增加 count 也不改变舍入。
	mustOK(t, store2, `[{"name":"m","timestamp":3,"value":7}]`)
	pair3 := pairsFor("m", 1, 2, nil)
	assertQueryPairCorrespond(t, store2, pair3)
	if s := mustQuery(t, store2, pair3.query).Series[0]; s.Count != 2 || s.Average != 1 {
		t.Fatalf("out-of-range point changed the tie result: %+v", s)
	}
}
