package darksafe

import (
	"reflect"
	"testing"
)

// 本文件回归保障采样明细查询结果的独立性：调用方经 Go 入口（专用入口
// QueryPointsLine 与统一入口 ProcessLine）取得 query_points 的明细结果后，
// 可以就地整理其中的指标名、标签、采样点与序列条目供本次展示使用；这些改动
// 只能留在那一份返回结果中——不能改变已写入的数据，不能污染此前或此后取得的
// 其他明细结果、均值查询结果与写入快照，也不能影响后续写入的重复/冲突判定。
// 与 success_result_independent_test.go 中的快照/均值结果隔离测试相对应；
// 范围选择、排列次序与查询本身只读由 query_points_test.go 保障，这里不重复。

// findQueryPointsIndex 在明细查询结果中找到指定指标名与完整标签集合的序列下标。
func findQueryPointsIndex(t *testing.T, res *QueryPointsResult, name string, labels map[string]string) int {
	t.Helper()
	for i, s := range res.Series {
		if s.Name == name && reflect.DeepEqual(s.Labels, labels) {
			return i
		}
	}
	t.Fatalf("series %s %v not found in query_points result: %+v", name, labels, res.Series)
	return -1
}

// seedQueryPointsFixture 写入四条 cpu 序列共五个点：host=a 两个点（1000->2、
// 2000->4），host=b 一个点（1000->5），无标签一个点（1000->6），空字符串
// 标签值 zone="" 一个点（1000->7）。返回写入快照供各测试保留基线。
func seedQueryPointsFixture(t *testing.T, store *MetricStore) *BatchResult {
	t.Helper()
	snap := mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}}
	]`)
	if snap.Added != 5 || snap.Duplicates != 0 || len(snap.Series) != 4 {
		t.Fatalf("fixture batch = %+v, want added=5 and 4 series", snap)
	}
	return snap
}

// assertFullCPUPoints 校验完整 cpu 明细查询恰好为四条身份与点明确的序列，
// 顺序与 organizeSeries 排序一致：无标签、host=a、host=b、zone=""。
func assertFullCPUPoints(t *testing.T, res *QueryPointsResult) {
	t.Helper()
	if res.Status != "ok" || res.Op != "query_points" {
		t.Fatalf("result envelope = %+v, want ok query_points", res)
	}
	want := []struct {
		labels map[string]string
		points []Point
	}{
		{map[string]string{}, []Point{{Timestamp: 1000, Value: 6}}},
		{map[string]string{"host": "a"}, []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}},
		{map[string]string{"host": "b"}, []Point{{Timestamp: 1000, Value: 5}}},
		{map[string]string{"zone": ""}, []Point{{Timestamp: 1000, Value: 7}}},
	}
	if len(res.Series) != len(want) {
		t.Fatalf("series = %+v, want %d entries", res.Series, len(want))
	}
	for i, w := range want {
		got := res.Series[i]
		if got.Name != "cpu" || !reflect.DeepEqual(got.Labels, w.labels) {
			t.Fatalf("series[%d] identity = %+v, want cpu %v", i, got, w.labels)
		}
		if !reflect.DeepEqual(got.Points, w.points) {
			t.Fatalf("series[%d] points = %+v, want %+v", i, got.Points, w.points)
		}
	}
}

// 明细结果是独立结果：修改其中一条返回记录的名称，增加、删除或改写标签，
// 改动点的时间戳与数值，以及整理点列表与序列列表（交换、截短、追加），
// 都不能改变已存数据，也不能影响其他序列与其他结果。
func TestQueryPointsResultMutationDoesNotChangeStorage(t *testing.T) {
	s := NewMetricStore()
	snapBefore := seedQueryPointsFixture(t, s)

	// 先保留一份未修改的明细结果与一份写入快照，随后修改另一份明细结果。
	qp0 := mustQueryPoints(t, s, queryPointsAll("cpu", 0, 10000))
	assertFullCPUPoints(t, qp0)
	qp1 := mustQueryPoints(t, s, queryPointsAll("cpu", 0, 10000))

	// 调用方整理 host=a 记录：改名、改写标签、加标签、改两个点的时间戳与
	// 数值、追加一个伪造点，并交换两个点的位置。
	a := &qp1.Series[findQueryPointsIndex(t, qp1, "cpu", map[string]string{"host": "a"})]
	a.Name = "cpu-renamed"
	a.Labels["host"] = "c"
	a.Labels["zone"] = "z"
	a.Points[0].Timestamp = 99999
	a.Points[0].Value = 99
	a.Points[1].Timestamp = 88888
	a.Points[1].Value = 88
	a.Points = append(a.Points, Point{Timestamp: 1, Value: 1})
	a.Points[0], a.Points[1] = a.Points[1], a.Points[0]

	// 同名不同标签的同伴记录也被整理：删改标签、改点值、截短点列表。
	b := &qp1.Series[findQueryPointsIndex(t, qp1, "cpu", map[string]string{"host": "b"})]
	delete(b.Labels, "host")
	b.Labels["host"] = "d"
	b.Points[0].Value = 55
	b.Points = b.Points[:0]

	// 整理序列列表：交换条目、截短并追加伪造条目、篡改信封字段。
	for i, j := 0, len(qp1.Series)-1; i < j; i, j = i+1, j-1 {
		qp1.Series[i], qp1.Series[j] = qp1.Series[j], qp1.Series[i]
	}
	qp1.Series = append(qp1.Series[:1], QueryPointsSeries{
		Name:   "ghost",
		Labels: map[string]string{},
		Points: []Point{{Timestamp: 7, Value: 7}},
	})
	qp1.Status = "mutated"
	qp1.Op = "mutated"

	// 此前保留的另一份明细结果不被污染：标签、点与次序都保持取得时的内容。
	assertFullCPUPoints(t, qp0)
	// 此前保留的写入快照同样保持原有标签、点与次序。
	if len(snapBefore.Series) != 4 {
		t.Fatalf("retained snapshot = %+v, want 4 series", snapBefore.Series)
	}
	aSnap := snapBefore.Series[findViewIndex(t, snapBefore, "cpu", map[string]string{"host": "a"})]
	if aSnap.Name != "cpu" || len(aSnap.Labels) != 1 || aSnap.Labels["host"] != "a" ||
		!reflect.DeepEqual(aSnap.Points, []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}) {
		t.Fatalf("retained snapshot host=a view changed: %+v", aSnap)
	}

	// 再次按原来的指标名、标签和区间查询，仍得到原始采样明细。
	qp2 := mustQueryPoints(t, s, queryPointsAll("cpu", 0, 10000))
	assertFullCPUPoints(t, qp2)
	res := mustQueryPoints(t, s, queryPointsLabels("cpu", 0, 10000, map[string]string{"host": "a"}))
	if len(res.Series) != 1 {
		t.Fatalf("host=a query_points = %+v, want one series", res.Series)
	}
	assertPoints(t, res.Series[0], []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}},
		"host=a facts after result mutation")

	// 按仅在返回结果中改出的身份或时间戳查询，不应凭空出现数据。
	for _, line := range []string{
		queryPointsLabels("cpu", 0, 100000, map[string]string{"host": "c"}),
		queryPointsLabels("cpu", 0, 100000, map[string]string{"host": "d"}),
		queryPointsLabels("cpu", 0, 100000, map[string]string{"host": "a", "zone": "z"}),
		queryPointsLabels("cpu-renamed", 0, 100000, map[string]string{"host": "c"}),
		queryPointsAll("ghost", 0, 100000),
		queryPointsLabels("cpu", 80000, 100000, map[string]string{"host": "a"}),
	} {
		if res = mustQueryPoints(t, s, line); len(res.Series) != 0 {
			t.Fatalf("mutated identity/timestamp %s must not match, got %+v", line, res.Series)
		}
	}

	// 均值查询的点数与平均值保持原始数据对应的结果：展示时追加的点不算进去。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("host=a average query = %+v, want count=2 average=3", qr.Series)
	}
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"b"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 5 {
		t.Fatalf("host=b average query = %+v, want count=1 average=5", qr.Series)
	}

	// 后续写入仍认真实存储：原序列、原时间戳、原值再次提交计为重复。
	dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 || len(dup.Series) != 4 {
		t.Fatalf("resubmit original point = %+v, want one duplicate and still 4 series", dup)
	}

	// 对同一位置提交不同值仍拒绝整批：冲突中的已存在值来自真实存储（2），
	// 不受返回结果里改出的数值（99）影响；改出的时间戳依旧没有点。
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":99,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil {
		t.Fatalf("expected conflict, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "cpu" || len(c.Series.Labels) != 1 || c.Series.Labels["host"] != "a" ||
		c.Timestamp != 1000 || c.Existing != 2 || c.Submitted != 99 {
		t.Fatalf("conflict must report stored fact, got %+v", c)
	}
	// 同伴序列同理：返回结果里改成 55，真实已存值仍是 5。
	lerr = mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":55,"labels":{"host":"b"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 55 {
		t.Fatalf("host=b conflict must report existing=5, got %+v", lerr.Conflict)
	}
	if res = mustQueryPoints(t, s, queryPointsLabels("cpu", 80000, 100000, map[string]string{"host": "a"})); len(res.Series) != 0 {
		t.Fatalf("rejected batch must not store mutated timestamps, got %+v", res.Series)
	}

	// 之后取得的写入快照仍按存储事实呈现。
	snapAfter := mustOK(t, s, `[]`)
	if len(snapAfter.Series) != 4 {
		t.Fatalf("snapshot after mutations = %+v, want 4 series", snapAfter.Series)
	}
	aAfter := snapAfter.Series[findViewIndex(t, snapAfter, "cpu", map[string]string{"host": "a"})]
	if !reflect.DeepEqual(aAfter.Points, []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}) {
		t.Fatalf("fresh snapshot host=a points = %+v, want (1000,2),(2000,4)", aAfter.Points)
	}
	bAfter := snapAfter.Series[findViewIndex(t, snapAfter, "cpu", map[string]string{"host": "b"})]
	if bAfter.Labels["host"] != "b" || !reflect.DeepEqual(bAfter.Points, []Point{{Timestamp: 1000, Value: 5}}) {
		t.Fatalf("fresh snapshot host=b view = %+v, want host=b (1000,5)", bAfter)
	}
}

// 无标签序列与空字符串标签值序列是两条真实序列：给返回的空标签集合加键、
// 删除返回的空值标签，不能合并或改写它们；原来的标签筛选结果仍应成立。
func TestQueryPointsEmptyAndEmptyValueLabelsIndependent(t *testing.T) {
	s := NewMetricStore()
	seedQueryPointsFixture(t, s)

	qp := mustQueryPoints(t, s, queryPointsAll("cpu", 0, 10000))
	none := &qp.Series[findQueryPointsIndex(t, qp, "cpu", map[string]string{})]
	if none.Labels == nil {
		t.Fatal("query_points labels for unlabeled series must be non-nil empty map")
	}
	emptyVal := &qp.Series[findQueryPointsIndex(t, qp, "cpu", map[string]string{"zone": ""})]

	// 调用方整理两个返回记录：空集合加键、空值标签删键、改点的时间戳与数值。
	none.Labels["host"] = "x"
	none.Points[0].Value = 66
	delete(emptyVal.Labels, "zone")
	emptyVal.Points[0].Timestamp = 5000

	// 两条真实序列不合并、不改写：完整查询仍是四条身份明确的序列。
	assertFullCPUPoints(t, mustQueryPoints(t, s, queryPointsAll("cpu", 0, 10000)))

	// 原来的标签筛选结果仍成立：加出来的 host=x 不匹配任何序列；
	// 空值标签筛选仍只命中真实存在的空值序列。
	if res := mustQueryPoints(t, s, queryPointsLabels("cpu", 0, 10000, map[string]string{"host": "x"})); len(res.Series) != 0 {
		t.Fatalf("added label must not match: %+v", res.Series)
	}
	res := mustQueryPoints(t, s, queryPointsLabels("cpu", 0, 10000, map[string]string{"zone": ""}))
	if len(res.Series) != 1 {
		t.Fatalf("empty-value label must still match its series: %+v", res.Series)
	}
	assertPoints(t, res.Series[0], []Point{{Timestamp: 1000, Value: 7}}, "zone='' series facts")
	// 返回结果里改出的时间戳不存在；原时间戳仍可查到。
	if res = mustQueryPoints(t, s, queryPointsLabels("cpu", 4000, 6000, map[string]string{"zone": ""})); len(res.Series) != 0 {
		t.Fatalf("mutated timestamp must not be stored: %+v", res.Series)
	}

	// 提交被改出的数值 66 冲突于真实值 6，冲突身份仍是无标签序列。
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":66}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 6 || len(lerr.Conflict.Series.Labels) != 0 {
		t.Fatalf("unlabeled conflict must report existing=6 with empty labels, got %+v", lerr.Conflict)
	}
}

// 保留一份未修改的明细结果后成功写入新时间戳：新查询看到新增点，
// 旧结果仍表示取得时的内容，不跟着新写入变化。
func TestQueryPointsRetainedResultStaysFixedAfterNewWrite(t *testing.T) {
	s := NewMetricStore()
	snapBefore := mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":8,"labels":{"host":"b"}}
	]`)
	qpBefore := mustQueryPoints(t, s, queryPointsAll("cpu", 0, 10000))
	if len(qpBefore.Series) != 2 {
		t.Fatalf("retained query_points baseline = %+v, want 2 series", qpBefore.Series)
	}

	// 向 host=a 写入新时间戳 3000->6。
	write := mustOK(t, s, `[{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a"}}]`)
	if write.Added != 1 || write.Duplicates != 0 {
		t.Fatalf("new timestamp write = %+v, want added=1", write)
	}

	// 新查询看到新增点，按时间戳升序列出。
	res := mustQueryPoints(t, s, queryPointsLabels("cpu", 0, 10000, map[string]string{"host": "a"}))
	if len(res.Series) != 1 {
		t.Fatalf("query_points after write = %+v, want one series", res.Series)
	}
	assertPoints(t, res.Series[0], []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 2000, Value: 4},
		{Timestamp: 3000, Value: 6},
	}, "new query_points includes the new point")

	// 先前保留的明细结果仍表示取得时的内容：两条序列、host=a 两个点。
	if len(qpBefore.Series) != 2 {
		t.Fatalf("retained query_points changed after new write: %+v", qpBefore.Series)
	}
	aBefore := qpBefore.Series[findQueryPointsIndex(t, qpBefore, "cpu", map[string]string{"host": "a"})]
	assertPoints(t, aBefore, []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}},
		"retained query_points host=a stays at fetch time")
	bBefore := qpBefore.Series[findQueryPointsIndex(t, qpBefore, "cpu", map[string]string{"host": "b"})]
	assertPoints(t, bBefore, []Point{{Timestamp: 1000, Value: 8}},
		"retained query_points host=b stays at fetch time")

	// 先前保留的写入快照同样不跟着变化。
	aSnap := snapBefore.Series[findViewIndex(t, snapBefore, "cpu", map[string]string{"host": "a"})]
	if len(snapBefore.Series) != 2 ||
		!reflect.DeepEqual(aSnap.Points, []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}) {
		t.Fatalf("retained snapshot changed after new write: %+v", snapBefore.Series)
	}
}

// 统一行处理入口 ProcessLine 返回的明细结果同样遵守独立性规则：
// 修改经统一入口取得的记录不污染存储，也不污染经专用入口取得的结果。
func TestQueryPointsResultIndependentViaProcessLine(t *testing.T) {
	s := NewMetricStore()
	seedQueryPointsFixture(t, s)

	r, lerr := s.ProcessLine(`{"op":"query_points","name":"cpu","start":0,"end":10000}`)
	if lerr != nil || r == nil {
		t.Fatalf("ProcessLine query_points: r=%v lerr=%+v", r, lerr)
	}
	qp, ok := r.(*QueryPointsResult)
	if !ok {
		t.Fatalf("ProcessLine query_points result type = %T, want *QueryPointsResult", r)
	}

	// 调用方整理经统一入口取得的记录：改名、加标签、改点、追加序列条目。
	a := &qp.Series[findQueryPointsIndex(t, qp, "cpu", map[string]string{"host": "a"})]
	a.Name = "cpu-renamed"
	a.Labels["host"] = "c"
	a.Points[0].Value = 99
	a.Points = append(a.Points, Point{Timestamp: 424242, Value: 1})
	qp.Series = append(qp.Series, QueryPointsSeries{Name: "ghost", Labels: map[string]string{}})

	// 专用入口取得的明细结果仍按存储事实返回，不受统一入口结果的修改影响。
	assertFullCPUPoints(t, mustQueryPoints(t, s, queryPointsAll("cpu", 0, 10000)))

	// 统一入口的均值查询同样保持原始数据对应的结果。
	r, lerr = s.ProcessLine(`{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	if lerr != nil {
		t.Fatalf("ProcessLine query: %+v", lerr)
	}
	qr := r.(*QueryResult)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("ProcessLine average query = %+v, want count=2 average=3", qr.Series)
	}

	// 按改出的身份经统一入口查询，不应凭空出现数据。
	r, lerr = s.ProcessLine(`{"op":"query_points","name":"cpu-renamed","start":0,"end":500000}`)
	if lerr != nil {
		t.Fatalf("ProcessLine query_points: %+v", lerr)
	}
	if qp = r.(*QueryPointsResult); len(qp.Series) != 0 {
		t.Fatalf("mutated identity must not match via ProcessLine, got %+v", qp.Series)
	}

	// 后续写入仍认真实存储：原值重复、不同值冲突且已存在值来自真实存储。
	dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("resubmit original point = %+v, want one duplicate", dup)
	}
	lerr2 := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":99,"labels":{"host":"a"}}]`)
	if lerr2.Conflict == nil || lerr2.Conflict.Existing != 2 || lerr2.Conflict.Submitted != 99 {
		t.Fatalf("conflict must report stored existing=2, got %+v", lerr2.Conflict)
	}
}
