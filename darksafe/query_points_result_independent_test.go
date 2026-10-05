package darksafe

import (
	"reflect"
	"testing"
)

// 本文件回归保障采样明细查询（op 为 query_points）成功结果的独立性：业务代码通过
// Go 入口取得 query_points 的结果后，可以改动其中的指标名、标签与采样明细供本次
// 展示使用；这些改动只能留在那一份返回结果中——不能改变已写入的数据，不能污染
// 此前或此后取得的其他结果（包括另一份明细结果与写入快照），也不能影响后续写入
// 的重复/冲突判定与均值查询。
//
// 与 success_result_independent_test.go 中写入快照（BatchResult.Series）与均值
// 查询结果（QueryResult.Series）的隔离测试相对应；两种 Go 入口都在保障范围内：
// 专用入口 QueryPointsLine 与统一行处理入口 ProcessLine。

// findPointsIndex 在明细结果中找到指定指标名与完整标签集合的条目下标。
func findPointsIndex(t *testing.T, res *QueryPointsResult, name string, labels map[string]string) int {
	t.Helper()
	for i, v := range res.Series {
		if v.Name == name && reflect.DeepEqual(v.Labels, labels) {
			return i
		}
	}
	t.Fatalf("series %s %v not found in query_points result: %+v", name, labels, res.Series)
	return -1
}

// assertQueryPointsFact 按原指标名、标签与区间再查一次明细，断言返回的点
// （时间戳与数值）与取得时的事实完全一致。
func assertQueryPointsFact(t *testing.T, s *MetricStore, line string, want []Point, note string) {
	t.Helper()
	res := mustQueryPoints(t, s, line)
	if len(res.Series) != 1 {
		t.Fatalf("%s: series = %+v, want exactly one", note, res.Series)
	}
	if !reflect.DeepEqual(res.Series[0].Points, want) {
		t.Fatalf("%s: points = %+v, want %+v", note, res.Series[0].Points, want)
	}
}

// TestQueryPointsMutationDoesNotChangeStorage 是核心隔离场景：同一指标多条标签
// 不同的序列，调用方拿到明细结果后改名、增删改标签、改点的时间戳与数值、
// 整理点列表与序列列表（交换、截短、追加），都只是在整理本地结果。存储事实、
// 另一份明细结果、后续写入的重复/冲突判定与均值查询都不受影响。
func TestQueryPointsMutationDoesNotChangeStorage(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}}
	]`)

	hostA := []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 2000, Value: 4},
		{Timestamp: 3000, Value: 6},
	}
	hostB := []Point{{Timestamp: 1000, Value: 5}}

	// 先保留一份未修改的明细结果（全量 cpu），随后篡改另一份。
	kept := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000}`)
	if len(kept.Series) != 4 {
		t.Fatalf("baseline query_points = %+v, want 4 series", kept.Series)
	}
	res := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000}`)

	// 调用方整理 host=a 明细：改名、改写标签、加标签，篡改三个点的时间戳与数值。
	a := &res.Series[findPointsIndex(t, res, "cpu", map[string]string{"host": "a"})]
	a.Name = "cpu-renamed"
	a.Labels["host"] = "c"
	a.Labels["zone"] = "z"
	a.Points[0].Timestamp = 99990
	a.Points[0].Value = 90
	a.Points[1].Timestamp = 88880
	a.Points[1].Value = 80
	a.Points[2].Value = 70

	// 调用方整理点列表：交换、截短与追加伪造点，都只是本地整理。
	a.Points[0], a.Points[1] = a.Points[1], a.Points[0]
	a.Points = append(a.Points[:1], Point{Timestamp: 1, Value: 1})

	// 同伴序列 host=b 也被改名、改标签、改点。
	b := &res.Series[findPointsIndex(t, res, "cpu", map[string]string{"host": "b"})]
	b.Name = "cpu-other"
	delete(b.Labels, "host")
	b.Labels["host"] = "d"
	b.Points[0].Timestamp = 77770
	b.Points[0].Value = 55

	// 整理序列列表：交换条目、截短列表、追加伪造序列。
	res.Series[0], res.Series[len(res.Series)-1] = res.Series[len(res.Series)-1], res.Series[0]
	res.Series = append(res.Series[:2], QueryPointsSeries{
		Name:   "ghost",
		Labels: map[string]string{"host": "g"},
		Points: []Point{{Timestamp: 4242, Value: 42}},
	})
	res.Status = "mutated"
	res.Op = "mutated"

	// 另一条序列（host=a）的改动不能影响同伴序列：host=b 的事实仍是 (1000,5)。
	assertQueryPointsFact(t, s,
		`{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"b"}}`,
		hostB, "host=b fact after mutating host=a result")
	// 按原指标名、原标签、原区间查询，仍得到原始采样明细（含被截掉的点）。
	assertQueryPointsFact(t, s,
		`{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`,
		hostA, "host=a fact after mutating its result")

	// 按仅在返回结果中改出的身份查询，不应凭空出现数据。
	for _, line := range []string{
		`{"op":"query_points","name":"cpu","start":0,"end":100000,"labels":{"host":"c"}}`,
		`{"op":"query_points","name":"cpu","start":0,"end":100000,"labels":{"host":"d"}}`,
		`{"op":"query_points","name":"cpu","start":0,"end":100000,"labels":{"host":"g"}}`,
		`{"op":"query_points","name":"cpu","start":0,"end":100000,"labels":{"host":"a","zone":"z"}}`,
		`{"op":"query_points","name":"cpu-renamed","start":0,"end":100000}`,
		`{"op":"query_points","name":"ghost","start":0,"end":100000}`,
	} {
		if qr := mustQueryPoints(t, s, line); len(qr.Series) != 0 {
			t.Fatalf("mutated identity %s must not match, got %+v", line, qr.Series)
		}
	}
	// 按改出的时间戳查询，不应凭空出现数据（含追加的时间戳 1、改出的 77770 等）。
	for _, line := range []string{
		`{"op":"query_points","name":"cpu","start":70000,"end":100000,"labels":{"host":"a"}}`,
		`{"op":"query_points","name":"cpu","start":70000,"end":100000,"labels":{"host":"b"}}`,
		`{"op":"query_points","name":"cpu","start":0,"end":100,"labels":{"host":"a"}}`,
	} {
		if qr := mustQueryPoints(t, s, line); len(qr.Series) != 0 {
			t.Fatalf("mutated timestamps %s must not be stored, got %+v", line, qr.Series)
		}
	}

	// 先前保留的另一份明细结果保留各自原有的标签、点与次序。
	wantKept := []struct {
		labels map[string]string
		points []Point
	}{
		{map[string]string{}, []Point{{Timestamp: 1000, Value: 6}}},
		{map[string]string{"host": "a"}, hostA},
		{map[string]string{"host": "b"}, hostB},
		{map[string]string{"zone": ""}, []Point{{Timestamp: 1000, Value: 7}}},
	}
	if kept.Status != "ok" || kept.Op != "query_points" || len(kept.Series) != len(wantKept) {
		t.Fatalf("retained query_points envelope/series changed: %+v", kept)
	}
	for i, w := range wantKept {
		got := kept.Series[i]
		if got.Name != "cpu" || !reflect.DeepEqual(got.Labels, w.labels) ||
			!reflect.DeepEqual(got.Points, w.points) {
			t.Fatalf("retained query_points series[%d] = %+v, want labels %v points %+v",
				i, got, w.labels, w.points)
		}
	}

	// 写入快照也保留原有标签、点与次序。
	snap := mustOK(t, s, `[]`)
	if len(snap.Series) != 4 {
		t.Fatalf("snapshot after query_points mutation = %+v, want 4 series", snap.Series)
	}
	snapA := snap.Series[findViewIndex(t, snap, "cpu", map[string]string{"host": "a"})]
	if !reflect.DeepEqual(snapA.Points, hostA) {
		t.Fatalf("snapshot host=a points = %+v, want %+v", snapA.Points, hostA)
	}
	snapB := snap.Series[findViewIndex(t, snap, "cpu", map[string]string{"host": "b"})]
	if !reflect.DeepEqual(snapB.Points, hostB) {
		t.Fatalf("snapshot host=b points = %+v, want %+v", snapB.Points, hostB)
	}

	// 原序列、原时间戳、原值再次提交仍计为重复（存储认的是真实值，不是改出的 90/55）。
	dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("resubmit original host=a point = %+v, want one duplicate", dup)
	}
	dup = mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("resubmit original host=b point = %+v, want one duplicate", dup)
	}

	// 对同一位置提交不同值仍拒绝整批；冲突中的已存在值来自真实存储，
	// 不受返回结果里改出的数值影响（host=a 改出的 90、host=b 改出的 55）。
	lerr := mustFail(t, s, `[
		{"name":"cpu","timestamp":3000,"value":99,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":123,"labels":{"host":"a"}}
	]`)
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("conflict must target the first conflicting point, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "cpu" || len(c.Series.Labels) != 1 || c.Series.Labels["host"] != "a" ||
		c.Timestamp != 3000 || c.Existing != 6 || c.Submitted != 99 {
		t.Fatalf("host=a conflict must report stored fact, got %+v", c)
	}
	lerr = mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":55,"labels":{"host":"b"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 55 {
		t.Fatalf("host=b conflict must report existing=5, got %+v", lerr.Conflict)
	}
	// 整批拒绝：本批没有任何点留下。
	if dup = mustOK(t, s, `[{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a"}}]`); dup.Duplicates != 1 {
		t.Fatalf("conflict batch must not write anything, got %+v", dup)
	}

	// 均值查询的点数与平均值仍是原始数据对应的结果，展示时追加/改出的点不算。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 3 || qr.Series[0].Average != 4 {
		t.Fatalf("host=a average after mutation = %+v, want count=3 average=4", qr.Series)
	}
}

// TestQueryPointsEmptyAndEmptyValueLabelsIndependent 保留两种容易混淆的标签
// 情况：无标签序列与某个标签确实存在但值为空字符串的序列。给返回的空标签集合
// 加键、删除返回的空值标签，都不能合并或改写这两条真实序列；原标签筛选仍成立。
func TestQueryPointsEmptyAndEmptyValueLabelsIndependent(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":2000,"value":8},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}
	]`)

	res := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000}`)
	none := &res.Series[findPointsIndex(t, res, "cpu", map[string]string{})]
	if none.Labels == nil {
		t.Fatal("query_points labels for unlabeled series must be non-nil empty map")
	}
	emptyVal := &res.Series[findPointsIndex(t, res, "cpu", map[string]string{"zone": ""})]

	// 调用方整理两个返回视图：空集合加键、空值标签删键、改点值与时间戳。
	none.Labels["host"] = "x"
	none.Points[0].Value = 66
	delete(emptyVal.Labels, "zone")
	emptyVal.Points[0].Timestamp = 5000
	emptyVal.Points[0].Value = 77

	// 三条真实序列不被合并或改写：全量明细仍是三条序列。
	again := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000}`)
	if len(again.Series) != 3 {
		t.Fatalf("full query_points = %+v, want 3 distinct series", again.Series)
	}
	gotNone := again.Series[findPointsIndex(t, again, "cpu", map[string]string{})]
	if !reflect.DeepEqual(gotNone.Points, []Point{{Timestamp: 1000, Value: 6}, {Timestamp: 2000, Value: 8}}) {
		t.Fatalf("unlabeled series points changed: %+v", gotNone.Points)
	}
	gotEmpty := again.Series[findPointsIndex(t, again, "cpu", map[string]string{"zone": ""})]
	if !reflect.DeepEqual(gotEmpty.Points, []Point{{Timestamp: 1000, Value: 7}}) {
		t.Fatalf("empty-value series points changed: %+v", gotEmpty.Points)
	}

	// 原标签筛选仍成立：加出来的 host=x 不命中；空值标签筛选仍只命中真实空值序列。
	if qr := mustQueryPoints(t, s,
		`{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"x"}}`); len(qr.Series) != 0 {
		t.Fatalf("added label must not match: %+v", qr.Series)
	}
	zoneEmpty := mustQueryPoints(t, s,
		`{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"zone":""}}`)
	if len(zoneEmpty.Series) != 1 ||
		!reflect.DeepEqual(zoneEmpty.Series[0].Labels, map[string]string{"zone": ""}) ||
		!reflect.DeepEqual(zoneEmpty.Series[0].Points, []Point{{Timestamp: 1000, Value: 7}}) {
		t.Fatalf("empty-value label selection = %+v, want the zone='' series", zoneEmpty.Series)
	}
	// 删除返回的空值标签后，空条件（{}）仍按全部序列返回，两条身份各自保留：
	// 无标签条目仍是空集合，zone="" 条目仍带该键，不会被合并成一条。
	allSeries := mustQueryPoints(t, s,
		`{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{}}`)
	if len(allSeries.Series) != 3 {
		t.Fatalf("empty-label selection = %+v, want all 3 series with distinct identities", allSeries.Series)
	}
	if got := allSeries.Series[findPointsIndex(t, allSeries, "cpu", map[string]string{})].Labels; len(got) != 0 {
		t.Fatalf("unlabeled identity must stay label-free, got %v", got)
	}
	zoneEntry := allSeries.Series[findPointsIndex(t, allSeries, "cpu", map[string]string{"zone": ""})]
	if v, ok := zoneEntry.Labels["zone"]; !ok || v != "" || len(zoneEntry.Labels) != 1 {
		t.Fatalf("zone='' identity must keep its single key, got %v", zoneEntry.Labels)
	}
	// 改出的时间戳 5000 不存在。
	if qr := mustQueryPoints(t, s,
		`{"op":"query_points","name":"cpu","start":4000,"end":6000,"labels":{"zone":""}}`); len(qr.Series) != 0 {
		t.Fatalf("mutated timestamp must not be stored: %+v", qr.Series)
	}

	// 均值查询同样不合并两条序列。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000}`)
	if len(qr.Series) != 3 {
		t.Fatalf("full average query = %+v, want 3 distinct series", qr.Series)
	}
	if q := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{})]; q.Count != 2 || q.Average != 7 {
		t.Fatalf("unlabeled average = %+v, want count=2 average=7", q)
	}
	if q := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{"zone": ""})]; q.Count != 1 || q.Average != 7 {
		t.Fatalf("empty-value average = %+v, want count=1 average=7", q)
	}

	// 后续写入仍按真实身份判定重复与冲突。
	if dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":6}]`); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("unlabeled resubmit must duplicate, got %+v", dup)
	}
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":77,"labels":{"zone":""}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 7 || lerr.Conflict.Submitted != 77 {
		t.Fatalf("empty-value conflict must report existing=7, got %+v", lerr.Conflict)
	}
}

// TestRetainedQueryPointsStayFixedAfterNewWrite 保留一份未修改的明细结果后再
// 成功写入新时间戳：新查询应看到新增点，旧结果仍表示取得时的内容（标签、点、
// 次序都不跟随新写入变化）；同一存储上更早保留的写入快照同样固定。
func TestRetainedQueryPointsStayFixedAfterNewWrite(t *testing.T) {
	s := NewMetricStore()
	snapBefore := mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":8,"labels":{"host":"b"}}
	]`)
	qpBefore := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000}`)
	hostABefore := []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}
	if len(qpBefore.Series) != 2 {
		t.Fatalf("retained query_points baseline = %+v, want 2 series", qpBefore.Series)
	}
	if got := qpBefore.Series[findPointsIndex(t, qpBefore, "cpu", map[string]string{"host": "a"})]; !reflect.DeepEqual(got.Points, hostABefore) {
		t.Fatalf("retained host=a points baseline = %+v, want %+v", got.Points, hostABefore)
	}

	// 成功写入新时间戳 3000->6。
	write := mustOK(t, s, `[{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a"}}]`)
	if write.Added != 1 || write.Duplicates != 0 {
		t.Fatalf("new timestamp write = %+v, want added=1", write)
	}

	// 新查询看到新增点，按时间戳升序排在最后。
	qpAfter := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	wantAfter := append(hostABefore, Point{Timestamp: 3000, Value: 6})
	if len(qpAfter.Series) != 1 || !reflect.DeepEqual(qpAfter.Series[0].Points, wantAfter) {
		t.Fatalf("new query_points after write = %+v, want %+v", qpAfter.Series, wantAfter)
	}

	// 先前保留的明细结果仍表示取得时的内容：host=a 只有两个点，host=b 不变。
	if len(qpBefore.Series) != 2 {
		t.Fatalf("retained query_points gained a series entry: %+v", qpBefore.Series)
	}
	gotA := qpBefore.Series[findPointsIndex(t, qpBefore, "cpu", map[string]string{"host": "a"})]
	if !reflect.DeepEqual(gotA.Points, hostABefore) {
		t.Fatalf("retained query_points host=a changed after new write: %+v", gotA.Points)
	}
	gotB := qpBefore.Series[findPointsIndex(t, qpBefore, "cpu", map[string]string{"host": "b"})]
	if !reflect.DeepEqual(gotB.Points, []Point{{Timestamp: 1000, Value: 8}}) {
		t.Fatalf("retained query_points host=b changed: %+v", gotB.Points)
	}
	if qpBefore.Status != "ok" || qpBefore.Op != "query_points" {
		t.Fatalf("retained envelope changed: %+v", qpBefore)
	}

	// 先前保留的写入快照同样不跟随变化。
	if len(snapBefore.Series) != 2 {
		t.Fatalf("retained snapshot gained a series: %+v", snapBefore.Series)
	}
	snapA := snapBefore.Series[findViewIndex(t, snapBefore, "cpu", map[string]string{"host": "a"})]
	if !reflect.DeepEqual(snapA.Points, hostABefore) {
		t.Fatalf("retained snapshot host=a changed after new write: %+v", snapA.Points)
	}

	// 新写入的点也不应影响更早取得的点被篡改后的隔离：再取一份明细并篡改，
	// 新写入的 3000 点在存储中仍是 6。
	mut := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	mut.Series[0].Points[2].Value = 600
	mut.Series[0].Points[2].Timestamp = 3001
	fresh := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":3000,"end":3000,"labels":{"host":"a"}}`)
	if !reflect.DeepEqual(fresh.Series[0].Points, []Point{{Timestamp: 3000, Value: 6}}) {
		t.Fatalf("new point fact changed by mutating a later result: %+v", fresh.Series)
	}
}

// TestQueryPointsResultIndependentViaProcessLine 统一行处理入口 ProcessLine
// 返回的明细结果同样遵守隔离规则：通过类型断言取得 *QueryPointsResult 后修改其
// 指标名、标签、点与列表结构，不影响存储、另一份结果与后续经统一入口的写入判定。
func TestQueryPointsResultIndependentViaProcessLine(t *testing.T) {
	s := NewMetricStore()
	processOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}
	]`)

	// 通过统一入口取得第一份明细并保留（未修改）。
	r := processOK(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000}`)
	kept, ok := r.(*QueryPointsResult)
	if !ok {
		t.Fatalf("ProcessLine result type = %T, want *QueryPointsResult", r)
	}
	if len(kept.Series) != 2 {
		t.Fatalf("baseline via ProcessLine = %+v, want 2 series", kept.Series)
	}

	// 通过统一入口取得第二份明细并由调用方大幅整理。
	r = processOK(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000}`)
	res := r.(*QueryPointsResult)
	a := &res.Series[findPointsIndex(t, res, "cpu", map[string]string{"host": "a"})]
	a.Name = "renamed"
	a.Labels["host"] = "c"
	a.Points[0].Timestamp = 90001
	a.Points[0].Value = 200
	a.Points = a.Points[:1]
	a.Points = append(a.Points, Point{Timestamp: 2, Value: 20})
	b := &res.Series[findPointsIndex(t, res, "cpu", map[string]string{"host": "b"})]
	delete(b.Labels, "host")
	res.Series = res.Series[:1]
	res.Status = "mutated"
	res.Op = "mutated"

	// 存储事实不变：再经统一入口查询，host=a 仍是原始两个点。
	r = processOK(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	fresh := r.(*QueryPointsResult)
	if !reflect.DeepEqual(fresh.Series[0].Points,
		[]Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}) {
		t.Fatalf("host=a fact via ProcessLine changed: %+v", fresh.Series)
	}
	// host=b 同伴不受 host=a 视图修改影响。
	r = processOK(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"b"}}`)
	if !reflect.DeepEqual(r.(*QueryPointsResult).Series[0].Points, []Point{{Timestamp: 1000, Value: 5}}) {
		t.Fatalf("host=b fact via ProcessLine changed: %+v", r)
	}
	// 改出的身份与时间戳查不到数据。
	for _, line := range []string{
		`{"op":"query_points","name":"cpu","start":0,"end":100000,"labels":{"host":"c"}}`,
		`{"op":"query_points","name":"renamed","start":0,"end":100000}`,
		`{"op":"query_points","name":"cpu","start":90000,"end":91000,"labels":{"host":"a"}}`,
	} {
		r = processOK(t, s, line)
		if len(r.(*QueryPointsResult).Series) != 0 {
			t.Fatalf("mutated identity/timestamp %s must not match: %+v", line, r)
		}
	}

	// 先前保留的明细结果不被污染。
	if kept.Status != "ok" || kept.Op != "query_points" || len(kept.Series) != 2 {
		t.Fatalf("retained ProcessLine result changed: %+v", kept)
	}
	keptA := kept.Series[findPointsIndex(t, kept, "cpu", map[string]string{"host": "a"})]
	if !reflect.DeepEqual(keptA.Points, []Point{{Timestamp: 1000, Value: 2}, {Timestamp: 2000, Value: 4}}) {
		t.Fatalf("retained ProcessLine host=a points changed: %+v", keptA.Points)
	}

	// 后续写入经统一入口仍按真实存储判定：原值重复、不同值冲突且 existing 为真实值。
	r = processOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	br := r.(*BatchResult)
	if br.Added != 0 || br.Duplicates != 1 {
		t.Fatalf("duplicate via ProcessLine = %+v, want one duplicate", br)
	}
	lerr := processFail(t, s, `[{"name":"cpu","timestamp":1000,"value":200,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 2 || lerr.Conflict.Submitted != 200 {
		t.Fatalf("conflict via ProcessLine must report existing=2, got %+v", lerr.Conflict)
	}

	// 均值查询经统一入口仍是原始数据的点数与平均值。
	r = processOK(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	qa := r.(*QueryResult)
	if len(qa.Series) != 1 || qa.Series[0].Count != 2 || qa.Series[0].Average != 3 {
		t.Fatalf("average via ProcessLine = %+v, want count=2 average=3", qa.Series)
	}
}
