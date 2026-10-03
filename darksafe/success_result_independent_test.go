package darksafe

import (
	"reflect"
	"testing"
)

// 本文件回归保障成功结果的隔离性：写入批次返回的序列快照（BatchResult.Series）
// 与区间查询返回的结果（QueryResult.Series）都是当次操作的独立结果。
// 调用方整理指标名、标签、点、count/average 或结果条目顺序，都不得改动已写入的
// 序列身份与点的事实，也不得影响此前/此后取得的其他成功结果。
// 与 alias_repro_test.go 中的冲突结果隔离测试相对应。

// findViewIndex 在写入快照中找到指定指标名与完整标签集合的序列下标。
func findViewIndex(t *testing.T, res *BatchResult, name string, labels map[string]string) int {
	t.Helper()
	for i, v := range res.Series {
		if v.Name == name && reflect.DeepEqual(v.Labels, labels) {
			return i
		}
	}
	t.Fatalf("series %s %v not found in snapshot: %+v", name, labels, res.Series)
	return -1
}

// findQueryIndex 在查询结果中找到指定指标名与完整标签集合的条目下标。
func findQueryIndex(t *testing.T, res *QueryResult, name string, labels map[string]string) int {
	t.Helper()
	for i, v := range res.Series {
		if v.Name == name && reflect.DeepEqual(v.Labels, labels) {
			return i
		}
	}
	t.Fatalf("series %s %v not found in query result: %+v", name, labels, res.Series)
	return -1
}

// 写入快照是独立结果：改指标名、标签、点的时间戳/数值或追加点，
// 都不能改变存储中的序列身份与点的事实。
func TestSnapshotMutationDoesNotChangeStoredIdentity(t *testing.T) {
	s := NewMetricStore()
	// 同一存储中安排四条 cpu 序列：host=a 两个点（count=2、average=3），
	// 一条同名不同标签序列，一条无标签序列，一条空字符串标签值序列。
	snap := mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}}
	]`)
	if snap.Added != 5 || snap.Duplicates != 0 || len(snap.Series) != 4 {
		t.Fatalf("initial batch = %+v, want added=5 and 4 series", snap)
	}

	// 事实基线：完整区间查询 host=a 返回 count=2、average=3。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("baseline host=a query = %+v, want count=2 average=3", qr.Series)
	}

	// 调用方取得快照后整理 host=a 视图：改名、换标签、加标签、
	// 改两个点的时间戳与数值、追加一个伪造点。
	a := &snap.Series[findViewIndex(t, snap, "cpu", map[string]string{"host": "a"})]
	a.Name = "cpu-renamed"
	a.Labels["host"] = "c"
	a.Labels["zone"] = "z"
	a.Points[0].Timestamp = 99999
	a.Points[0].Value = 99
	a.Points[1].Timestamp = 88888
	a.Points[1].Value = 88
	a.Points = append(a.Points, Point{Timestamp: 1, Value: 1})

	// 同名不同标签的同伴视图也被调用方整理。
	b := &snap.Series[findViewIndex(t, snap, "cpu", map[string]string{"host": "b"})]
	b.Labels["host"] = "d"
	b.Points[0].Value = 55

	// 原名称 + 原标签查到的仍是原事实：1000->2、2000->4。
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 {
		t.Fatalf("host=a query = %+v, want one series", qr.Series)
	}
	got := qr.Series[0]
	if got.Name != "cpu" || len(got.Labels) != 1 || got.Labels["host"] != "a" ||
		got.Count != 2 || got.Average != 3 {
		t.Fatalf("host=a fact changed after snapshot mutation: %+v", got)
	}

	// 同伴序列不受 host=a 视图修改影响。
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"b"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 5 {
		t.Fatalf("host=b fact changed: %+v, want count=1 average=5", qr.Series)
	}

	// 按快照里改出的身份查询，不能凭空找到新序列。
	for _, line := range []string{
		`{"op":"query","name":"cpu","start":0,"end":100000,"labels":{"host":"c"}}`,
		`{"op":"query","name":"cpu","start":0,"end":100000,"labels":{"host":"d"}}`,
		`{"op":"query","name":"cpu","start":0,"end":100000,"labels":{"host":"a","zone":"z"}}`,
		`{"op":"query","name":"cpu-renamed","start":0,"end":100000,"labels":{"host":"c"}}`,
	} {
		qr = mustQuery(t, s, line)
		if len(qr.Series) != 0 {
			t.Fatalf("mutated identity %s must not match, got %+v", line, qr.Series)
		}
	}
	// 快照里改出的时间戳在存储中不存在。
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":80000,"end":100000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 0 {
		t.Fatalf("mutated timestamps must not be stored, got %+v", qr.Series)
	}

	// 再次提交原时间戳、原数值仍是重复（存储认的是真实值 2，不是快照里改出的 99）。
	dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 || len(dup.Series) != 4 {
		t.Fatalf("resubmit original point = %+v, want one duplicate and still 4 series", dup)
	}

	// 提交不同数值：冲突报告真实已存值 2，而不是调用方在快照里改出的 99；
	// 冲突失败仍是整批不写入，改出的时间戳依旧没有点。
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":99,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil {
		t.Fatalf("expected conflict, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "cpu" || c.Series.Labels["host"] != "a" || len(c.Series.Labels) != 1 ||
		c.Timestamp != 1000 || c.Existing != 2 || c.Submitted != 99 {
		t.Fatalf("conflict must report stored fact, got %+v", c)
	}
	// 同伴序列同理：快照里改成 55，但真实已存值仍是 5。
	lerr = mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":55,"labels":{"host":"b"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 55 {
		t.Fatalf("host=b conflict must report existing=5, got %+v", lerr.Conflict)
	}

	// 之后取得的新快照是新的独立结果，仍按存储事实呈现。
	snap2 := mustOK(t, s, `[]`)
	if len(snap2.Series) != 4 {
		t.Fatalf("fresh snapshot = %+v, want 4 series", snap2.Series)
	}
	a2 := snap2.Series[findViewIndex(t, snap2, "cpu", map[string]string{"host": "a"})]
	if a2.Name != "cpu" || len(a2.Labels) != 1 || a2.Labels["host"] != "a" || len(a2.Points) != 2 {
		t.Fatalf("fresh host=a view = %+v", a2)
	}
	if a2.Points[0] != (Point{Timestamp: 1000, Value: 2}) ||
		a2.Points[1] != (Point{Timestamp: 2000, Value: 4}) {
		t.Fatalf("fresh host=a points = %+v, want (1000,2),(2000,4)", a2.Points)
	}
	bv := snap2.Series[findViewIndex(t, snap2, "cpu", map[string]string{"host": "b"})]
	if bv.Labels["host"] != "b" || bv.Points[0].Value != 5 {
		t.Fatalf("fresh host=b view = %+v, want host=b value 5", bv)
	}
}

// 无标签序列与空字符串标签值序列：向返回的空标签集合加键、删除返回的空值标签、
// 修改返回点的时间戳或数值，都不能改变存储中的身份区别。
func TestSnapshotEmptyAndEmptyValueLabelsIndependent(t *testing.T) {
	s := NewMetricStore()
	snap := mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}
	]`)

	none := &snap.Series[findViewIndex(t, snap, "cpu", map[string]string{})]
	if none.Labels == nil {
		t.Fatal("snapshot labels for unlabeled series must be non-nil empty map")
	}
	emptyVal := &snap.Series[findViewIndex(t, snap, "cpu", map[string]string{"zone": ""})]

	// 调用方整理两个返回视图：空集合加键、空值标签删键、改点值与时间戳。
	none.Labels["host"] = "x"
	none.Points[0].Value = 66
	delete(emptyVal.Labels, "zone")
	emptyVal.Points[0].Timestamp = 5000

	// 无标签身份仍是无标签：全量结果中仍有一条标签为空、值为 6 的序列。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000}`)
	if len(qr.Series) != 3 {
		t.Fatalf("full cpu query = %+v, want 3 distinct series", qr.Series)
	}
	unlabeled := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{})]
	if unlabeled.Count != 1 || unlabeled.Average != 6 {
		t.Fatalf("unlabeled series fact changed: %+v", unlabeled)
	}
	// 空字符串标签值身份仍保留该键，点的时间戳仍是 1000。
	zoneEmpty := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{"zone": ""})]
	if zoneEmpty.Count != 1 || zoneEmpty.Average != 7 {
		t.Fatalf("empty-value series fact changed: %+v", zoneEmpty)
	}

	// 按原有标签规则筛选：加出来的 host=x 不匹配任何序列；
	// 空值标签查询仍只命中真实存在的空值序列。
	if qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"x"}}`); len(qr.Series) != 0 {
		t.Fatalf("added label must not match: %+v", qr.Series)
	}
	if qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"zone":""}}`); len(qr.Series) != 1 {
		t.Fatalf("empty-value label must still match its series: %+v", qr.Series)
	}
	// 快照里改出的时间戳不存在；原时间戳仍可查到。
	if qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":4000,"end":6000,"labels":{"zone":""}}`); len(qr.Series) != 0 {
		t.Fatalf("mutated timestamp must not be stored: %+v", qr.Series)
	}
	// 提交被改出的数值 66 冲突于真实值 6。
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":66}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 6 || len(lerr.Conflict.Series.Labels) != 0 {
		t.Fatalf("unlabeled conflict must report existing=6 with empty labels, got %+v", lerr.Conflict)
	}
}

// assertFullCPUResult 校验完整 cpu 查询恰好为四条身份与事实明确的序列，
// 顺序与 snapshot 排序一致：无标签、host=a、host=b、zone=""。
func assertFullCPUResult(t *testing.T, qr *QueryResult) {
	t.Helper()
	if qr.Status != "ok" || qr.Op != "query" {
		t.Fatalf("result envelope = %+v, want ok query", qr)
	}
	want := []struct {
		labels  map[string]string
		count   int
		average float64
	}{
		{map[string]string{}, 1, 6},
		{map[string]string{"host": "a"}, 2, 3},
		{map[string]string{"host": "b"}, 1, 5},
		{map[string]string{"zone": ""}, 1, 7},
	}
	if len(qr.Series) != len(want) {
		t.Fatalf("series = %+v, want %d entries", qr.Series, len(want))
	}
	for i, w := range want {
		got := qr.Series[i]
		if got.Name != "cpu" || !reflect.DeepEqual(got.Labels, w.labels) ||
			got.Count != w.count || got.Average != w.average {
			t.Fatalf("series[%d] = %+v, want labels %v count %d average %v",
				i, got, w.labels, w.count, w.average)
		}
	}
}

// 查询结果是独立结果：改返回条目的标签、count、average 或整理结果条目，
// 不污染另一份已取得的查询结果，也不影响存储与之后的写入快照。
func TestQueryResultMutationDoesNotChangeStorage(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}}
	]`)

	// 先保留一份成功结果，随后修改另一份。
	q0 := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000}`)
	assertFullCPUResult(t, q0)
	q1 := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000}`)

	// 调用方整理 q1 的各条目：改名、改标签、删标签、加标签，篡改 count/average。
	a := &q1.Series[findQueryIndex(t, q1, "cpu", map[string]string{"host": "a"})]
	a.Name = "cpu-renamed"
	a.Labels["host"] = "c"
	a.Labels["extra"] = "e"
	a.Count = 99
	a.Average = 123

	b := &q1.Series[findQueryIndex(t, q1, "cpu", map[string]string{"host": "b"})]
	delete(b.Labels, "host")
	b.Labels["host"] = "d"
	b.Count = 7
	b.Average = 77

	none := &q1.Series[findQueryIndex(t, q1, "cpu", map[string]string{})]
	if none.Labels == nil {
		t.Fatal("query labels for unlabeled series must be non-nil empty map")
	}
	none.Labels["host"] = "x"
	none.Count = 41
	none.Average = 42

	emptyVal := &q1.Series[findQueryIndex(t, q1, "cpu", map[string]string{"zone": ""})]
	delete(emptyVal.Labels, "zone")
	emptyVal.Count = 5
	emptyVal.Average = 9

	// 整理结果中的序列条目：反转顺序、截断并追加伪造条目、篡改信封字段。
	for i, j := 0, len(q1.Series)-1; i < j; i, j = i+1, j-1 {
		q1.Series[i], q1.Series[j] = q1.Series[j], q1.Series[i]
	}
	q1.Series = append(q1.Series[:1], QuerySeries{Name: "ghost", Labels: map[string]string{}, Count: 1})
	q1.Status = "mutated"
	q1.Op = "mutated"

	// 另一份此前取得的查询结果不被污染。
	assertFullCPUResult(t, q0)
	// 之后取得的查询结果仍按存储事实返回。
	q2 := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000}`)
	assertFullCPUResult(t, q2)

	// 之后的写入快照同样不被污染。
	snap := mustOK(t, s, `[]`)
	if len(snap.Series) != 4 {
		t.Fatalf("snapshot after query mutation = %+v, want 4 series", snap.Series)
	}
	for _, want := range []struct {
		labels map[string]string
		value  float64
	}{
		{map[string]string{}, 6},
		{map[string]string{"host": "a"}, 2},
		{map[string]string{"host": "b"}, 5},
		{map[string]string{"zone": ""}, 7},
	} {
		v := snap.Series[findViewIndex(t, snap, "cpu", want.labels)]
		if v.Points[0].Value != want.value {
			t.Fatalf("snapshot point for %v = %+v, want value %v", want.labels, v.Points, want.value)
		}
	}

	// 按改出的身份筛选查不到序列；原标签规则保持不变。
	for _, line := range []string{
		`{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"c"}}`,
		`{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"d"}}`,
		`{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"x"}}`,
		`{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a","extra":"e"}}`,
		`{"op":"query","name":"cpu-renamed","start":0,"end":10000}`,
		`{"op":"query","name":"ghost","start":0,"end":10000}`,
	} {
		if qr := mustQuery(t, s, line); len(qr.Series) != 0 {
			t.Fatalf("mutated identity %s must not match, got %+v", line, qr.Series)
		}
	}
	// 删除返回条目的空值标签不改变存储身份：空值筛选仍命中该序列。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"zone":""}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 7 {
		t.Fatalf("empty-value label selection = %+v, want the zone='' series", qr.Series)
	}
}

// 保留一份未修改的成功结果，再向存储写入新时间戳：新查询与新快照包含新增点，
// 先前保留的快照与查询结果仍表示各自取得时的数据，不跟着新写入变化。
func TestRetainedSuccessResultsStayFixedAfterNewWrite(t *testing.T) {
	s := NewMetricStore()
	snapBefore := mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":8,"labels":{"host":"b"}}
	]`)
	qBefore := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	qAllBefore := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000}`)
	if qBefore.Series[0].Count != 2 || qBefore.Series[0].Average != 3 {
		t.Fatalf("retained host=a query baseline = %+v, want count=2 average=3", qBefore.Series)
	}
	if len(qAllBefore.Series) != 2 {
		t.Fatalf("retained full query baseline = %+v, want 2 series", qAllBefore.Series)
	}

	// 向 host=a 写入新时间戳 3000->6：average 变为 (2+4+6)/3 = 4。
	write := mustOK(t, s, `[{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a"}}]`)
	if write.Added != 1 || write.Duplicates != 0 {
		t.Fatalf("new timestamp write = %+v, want added=1", write)
	}

	qAfter := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	if len(qAfter.Series) != 1 || qAfter.Series[0].Count != 3 || qAfter.Series[0].Average != 4 {
		t.Fatalf("new query after write = %+v, want count=3 average=4", qAfter.Series)
	}
	snapAfter := mustOK(t, s, `[]`)
	aAfter := snapAfter.Series[findViewIndex(t, snapAfter, "cpu", map[string]string{"host": "a"})]
	if len(aAfter.Points) != 3 || aAfter.Points[2] != (Point{Timestamp: 3000, Value: 6}) {
		t.Fatalf("new snapshot must include new point, got %+v", aAfter.Points)
	}

	// 先前保留的写入快照不跟着变化。
	aBefore := snapBefore.Series[findViewIndex(t, snapBefore, "cpu", map[string]string{"host": "a"})]
	if len(snapBefore.Series) != 2 || len(aBefore.Points) != 2 ||
		aBefore.Points[0] != (Point{Timestamp: 1000, Value: 2}) ||
		aBefore.Points[1] != (Point{Timestamp: 2000, Value: 4}) {
		t.Fatalf("retained snapshot changed after new write: %+v", snapBefore.Series)
	}
	bBefore := snapBefore.Series[findViewIndex(t, snapBefore, "cpu", map[string]string{"host": "b"})]
	if len(bBefore.Points) != 1 || bBefore.Points[0] != (Point{Timestamp: 1000, Value: 8}) {
		t.Fatalf("retained host=b snapshot changed: %+v", bBefore)
	}

	// 先前保留的查询结果也不跟着变化。
	if len(qBefore.Series) != 1 || qBefore.Series[0].Count != 2 || qBefore.Series[0].Average != 3 {
		t.Fatalf("retained host=a query changed: %+v", qBefore.Series)
	}
	if len(qAllBefore.Series) != 2 {
		t.Fatalf("retained full query gained the new point: %+v", qAllBefore.Series)
	}
	if qAllBefore.Series[0].Count != 2 || qAllBefore.Series[0].Average != 3 ||
		qAllBefore.Series[1].Count != 1 || qAllBefore.Series[1].Average != 8 {
		t.Fatalf("retained full query facts changed: %+v", qAllBefore.Series)
	}
}
