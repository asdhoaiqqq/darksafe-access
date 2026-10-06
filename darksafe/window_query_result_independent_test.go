package darksafe

import (
	"reflect"
	"testing"
)

// 本文件回归保障固定窗口查询（op 为 query_windows）成功结果的独立性：业务代码
// 通过 Go 入口取得 query_windows 的结果后，可以改动其中的指标名、标签、窗口
// 边界、点数、均值，并增删窗口、调整窗口与序列条目的顺序供本次展示使用；这些
// 改动只能留在那一份返回结果中——不能改变已写入的数据，不能让改出的名称、标签
// 或窗口成为实际存储内容，不能污染此前或此后取得的其他结果，也不能影响后续写入
// 的重复/冲突判定、原始采样明细（query_points）与区间统计（query）。
//
// 与 success_result_independent_test.go（写入快照与区间均值）和
// query_points_result_independent_test.go（采样明细）相对应；两种 Go 入口都在
// 保障范围内：固定窗口专用入口 QueryWindowsLine 与统一行处理入口 ProcessLine。

// findWindowsIndex 在固定窗口结果中找到指定指标名与完整标签集合的条目下标。
func findWindowsIndex(t *testing.T, res *QueryWindowsResult, name string, labels map[string]string) int {
	t.Helper()
	for i, v := range res.Series {
		if v.Name == name && reflect.DeepEqual(v.Labels, labels) {
			return i
		}
	}
	t.Fatalf("series %s %v not found in query_windows result: %+v", name, labels, res.Series)
	return -1
}

// assertWindowsFact 按原指标名、标签、区间与窗口宽度再查一次固定窗口结果，
// 断言返回的窗口（边界、点数、均值、顺序）与取得时的事实完全一致。
func assertWindowsFact(t *testing.T, store *MetricStore, line string, want []Window, note string) {
	t.Helper()
	res := mustQueryWindows(t, store, line)
	if len(res.Series) != 1 {
		t.Fatalf("%s: series = %+v, want exactly one", note, res.Series)
	}
	assertWindows(t, res.Series[0].Windows, want, note)
}

// TestQueryWindowsMutationDoesNotChangeStorage 是核心隔离场景：同一指标多条
// 标签不同的序列，调用方拿到固定窗口结果后改名、增删改标签，篡改窗口边界/点数/
// 均值，增删窗口、调整窗口与序列条目的顺序，都只是在整理本地结果。存储事实、
// 另一份窗口结果、原始明细、区间统计与后续写入的重复/冲突判定都不受影响。
func TestQueryWindowsMutationDoesNotChangeStorage(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}}
	]`)

	// 区间 [1000,3000]、step 1000：窗口为 [1000,1999]、[2000,2999]、[3000,3000]。
	hostA := []Window{
		window(1000, 1999, 2, 3), // 1000->2、1500->4
		window(3000, 3000, 1, 8), // 中间窗口为空，不补零
	}
	hostB := []Window{window(1000, 1999, 1, 5)}

	// 先保留一份未修改的窗口结果（全量 cpu），随后篡改另一份。
	kept := mustQueryWindows(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	if len(kept.Series) != 4 {
		t.Fatalf("baseline query_windows = %+v, want 4 series", kept.Series)
	}
	res := mustQueryWindows(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)

	// 调用方整理 host=a 的窗口：改名、改写标签、加标签，篡改两个窗口的
	// 边界、点数与均值，再补一个本来为空的中间窗口、追加一个伪造窗口。
	a := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "a"})]
	a.Name = "cpu-renamed"
	a.Labels["host"] = "c"
	a.Labels["zone"] = "z"
	a.Windows[0].Start = 2000
	a.Windows[0].End = 2999
	a.Windows[0].Count = 99
	a.Windows[0].Average = 123
	a.Windows[1].Start = 7000
	a.Windows[1].End = 7999
	a.Windows[1].Count = 41
	a.Windows[1].Average = 42
	// 凭空补出存储中本为空的中间窗口，并追加范围之外的伪造窗口。
	a.Windows = append(a.Windows,
		Window{Start: 2000, End: 2999, Count: 7, Average: 77},
		Window{Start: 8000, End: 8999, Count: 1, Average: 1})

	// 同伴序列 host=b 也被改名、改标签、改窗口。
	b := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "b"})]
	b.Name = "cpu-other"
	delete(b.Labels, "host")
	b.Labels["host"] = "d"
	b.Windows[0].Start = 9000
	b.Windows[0].End = 9999
	b.Windows[0].Count = 55
	b.Windows[0].Average = 55

	// 整理序列列表：反转条目、截短列表、追加伪造序列，篡改信封字段。
	for i, j := 0, len(res.Series)-1; i < j; i, j = i+1, j-1 {
		res.Series[i], res.Series[j] = res.Series[j], res.Series[i]
	}
	res.Series = append(res.Series[:1], QueryWindowsSeries{
		Name:    "ghost",
		Labels:  map[string]string{"host": "g"},
		Windows: []Window{{Start: 4000, End: 4999, Count: 1, Average: 4}},
	})
	res.Status = "mutated"
	res.Op = "mutated"

	// 另一条序列（host=a）的改动不能影响同伴序列：host=b 的事实仍是窗口 1 一点值 5。
	assertWindowsFact(t, store,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"b"}}`,
		hostB, "host=b fact after mutating host=a result")
	// 按原指标名、原标签、原区间查询，仍得到原始两个窗口（含边界、点数、均值）。
	assertWindowsFact(t, store,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		hostA, "host=a fact after mutating its result")

	// 按仅在返回结果中改出的身份查询，不应凭空出现数据。
	for _, line := range []string{
		`{"op":"query_windows","name":"cpu","start":0,"end":100000,"step":1000,"labels":{"host":"c"}}`,
		`{"op":"query_windows","name":"cpu","start":0,"end":100000,"step":1000,"labels":{"host":"d"}}`,
		`{"op":"query_windows","name":"cpu","start":0,"end":100000,"step":1000,"labels":{"host":"g"}}`,
		`{"op":"query_windows","name":"cpu","start":0,"end":100000,"step":1000,"labels":{"host":"a","zone":"z"}}`,
		`{"op":"query_windows","name":"cpu-renamed","start":0,"end":100000,"step":1000}`,
		`{"op":"query_windows","name":"ghost","start":0,"end":100000,"step":1000}`,
	} {
		if qr := mustQueryWindows(t, store, line); len(qr.Series) != 0 {
			t.Fatalf("mutated identity %s must not match, got %+v", line, qr.Series)
		}
	}
	// 改出的窗口不能成为存储内容：按改出的边界查询，host=a 在这些区间内没有点。
	for _, line := range []string{
		`{"op":"query_windows","name":"cpu","start":7000,"end":9999,"step":1000,"labels":{"host":"a"}}`,
		`{"op":"query_windows","name":"cpu","start":7000,"end":9999,"step":1000,"labels":{"host":"b"}}`,
		`{"op":"query_windows","name":"cpu","start":8000,"end":8999,"step":1000,"labels":{"host":"a"}}`,
	} {
		if qr := mustQueryWindows(t, store, line); len(qr.Series) != 0 {
			t.Fatalf("mutated windows %s must not be stored, got %+v", line, qr.Series)
		}
	}

	// 先前保留的另一份窗口结果保留各自原有的标签、窗口与次序。
	wantKept := []struct {
		labels  map[string]string
		windows []Window
	}{
		{map[string]string{}, []Window{window(1000, 1999, 1, 6)}},
		{map[string]string{"host": "a"}, hostA},
		{map[string]string{"host": "b"}, hostB},
		{map[string]string{"zone": ""}, []Window{window(1000, 1999, 1, 7)}},
	}
	if kept.Status != "ok" || kept.Op != "query_windows" || len(kept.Series) != len(wantKept) {
		t.Fatalf("retained query_windows envelope/series changed: %+v", kept)
	}
	for i, w := range wantKept {
		got := kept.Series[i]
		if got.Name != "cpu" || !reflect.DeepEqual(got.Labels, w.labels) {
			t.Fatalf("retained query_windows series[%d] identity = %+v, want labels %v",
				i, got, w.labels)
		}
		assertWindows(t, got.Windows, w.windows, "retained query_windows")
	}

	// 原始采样明细与区间统计仍反映真实采样点，不跟随被改动的窗口结果。
	qp := mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	if !reflect.DeepEqual(qp.Series[0].Points, []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 1500, Value: 4},
		{Timestamp: 3000, Value: 8},
	}) {
		t.Fatalf("host=a raw points changed with window result: %+v", qp.Series[0].Points)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 3 || qr.Series[0].Average != 14.0/3.0 {
		t.Fatalf("host=a range stats = %+v, want count=3 average=14/3", qr.Series)
	}

	// 原序列、原时间戳、原值再次提交仍计为重复（存储认的是真实值，不是改出的均值）。
	dup := mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("resubmit original host=a point = %+v, want one duplicate", dup)
	}
	dup = mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("resubmit original host=b point = %+v, want one duplicate", dup)
	}

	// 对原位置提交不同值仍拒绝整批；冲突中的已存在值来自真实存储，不受返回
	// 结果里改出的均值/点数影响（host=a 第一窗口改出的 123/99、host=b 改出的 55）。
	lerr := mustFail(t, store, `[{"name":"cpu","timestamp":1500,"value":99,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil {
		t.Fatalf("expected conflict for host=a at 1500, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "cpu" || len(c.Series.Labels) != 1 || c.Series.Labels["host"] != "a" ||
		c.Timestamp != 1500 || c.Existing != 4 || c.Submitted != 99 {
		t.Fatalf("host=a conflict must report stored fact, got %+v", c)
	}
	lerr = mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":123,"labels":{"host":"b"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 123 {
		t.Fatalf("host=b conflict must report existing=5, got %+v", lerr.Conflict)
	}
	// 整批拒绝：本批没有任何点留下。
	if dup = mustOK(t, store, `[{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}}]`); dup.Duplicates != 1 {
		t.Fatalf("conflict batch must not write anything, got %+v", dup)
	}

	// 再次取得的窗口结果仍按存储事实返回：空中间窗口依旧省略，没有伪造窗口。
	assertWindowsFact(t, store,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`,
		hostA, "fresh host=a window result")
}

// TestQueryWindowsEmptyAndEmptyValueLabelsIndependent 保留两种容易混淆的标签
// 情况：无标签序列与某个标签确实存在但值为空字符串的序列。给返回的空标签集合
// 加键、删除返回的空值标签，都不能合并或改名这两条真实序列；原标签筛选、
// 空窗口省略与后续冲突判定仍成立。
func TestQueryWindowsEmptyAndEmptyValueLabelsIndependent(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}
	]`)

	res := mustQueryWindows(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	none := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{})]
	if none.Labels == nil {
		t.Fatal("query_windows labels for unlabeled series must be non-nil empty map")
	}
	emptyVal := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"zone": ""})]

	// 调用方整理两个返回视图：空集合加键、空值标签删键、篡改窗口边界与统计。
	none.Labels["host"] = "x"
	none.Windows[0].Start = 2000
	none.Windows[0].End = 2999
	none.Windows[0].Count = 66
	none.Windows[0].Average = 66
	delete(emptyVal.Labels, "zone")
	emptyVal.Windows[0].Start = 3000
	emptyVal.Windows[0].End = 3000
	emptyVal.Windows[0].Count = 77
	emptyVal.Windows[0].Average = 77

	// 三条真实序列不被合并或改名：全量窗口结果仍是三条序列，身份、窗口不变。
	again := mustQueryWindows(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	if len(again.Series) != 3 {
		t.Fatalf("full query_windows = %+v, want 3 distinct series", again.Series)
	}
	assertWindows(t, again.Series[findWindowsIndex(t, again, "cpu", map[string]string{})].Windows,
		[]Window{window(1000, 1999, 1, 6)}, "unlabeled windows")
	assertWindows(t, again.Series[findWindowsIndex(t, again, "cpu", map[string]string{"zone": ""})].Windows,
		[]Window{window(1000, 1999, 1, 7)}, "empty-value windows")

	// 原标签筛选仍成立：加出来的 host=x 不命中；空值标签筛选仍只命中真实空值序列。
	if qr := mustQueryWindows(t, store,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"x"}}`); len(qr.Series) != 0 {
		t.Fatalf("added label must not match: %+v", qr.Series)
	}
	zoneEmpty := mustQueryWindows(t, store,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"zone":""}}`)
	if len(zoneEmpty.Series) != 1 ||
		!reflect.DeepEqual(zoneEmpty.Series[0].Labels, map[string]string{"zone": ""}) {
		t.Fatalf("empty-value label selection = %+v, want the zone='' series", zoneEmpty.Series)
	}
	assertWindows(t, zoneEmpty.Series[0].Windows,
		[]Window{window(1000, 1999, 1, 7)}, "empty-value selection windows")
	// 删除返回的空值标签后，空条件（{}）仍按全部序列返回，两条身份各自保留：
	// 无标签条目仍是空集合，zone="" 条目仍带该键，不会被合并成一条。
	allSeries := mustQueryWindows(t, store,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{}}`)
	if len(allSeries.Series) != 3 {
		t.Fatalf("empty-label selection = %+v, want all 3 series with distinct identities", allSeries.Series)
	}
	if got := allSeries.Series[findWindowsIndex(t, allSeries, "cpu", map[string]string{})].Labels; len(got) != 0 {
		t.Fatalf("unlabeled identity must stay label-free, got %v", got)
	}
	zoneEntry := allSeries.Series[findWindowsIndex(t, allSeries, "cpu", map[string]string{"zone": ""})]
	if v, ok := zoneEntry.Labels["zone"]; !ok || v != "" || len(zoneEntry.Labels) != 1 {
		t.Fatalf("zone='' identity must keep its single key, got %v", zoneEntry.Labels)
	}
	// 改出的窗口边界不代表存储：把窗口移到 [3000,3000] 后，该区段仍没有点。
	if qr := mustQueryWindows(t, store,
		`{"op":"query_windows","name":"cpu","start":3000,"end":3000,"step":1000,"labels":{"zone":""}}`); len(qr.Series) != 0 {
		t.Fatalf("mutated window must not be stored: %+v", qr.Series)
	}

	// 区间统计与原始明细同样不合并两条序列。
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":3000}`)
	if len(qr.Series) != 3 {
		t.Fatalf("full average query = %+v, want 3 distinct series", qr.Series)
	}
	if q := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{})]; q.Count != 1 || q.Average != 6 {
		t.Fatalf("unlabeled average = %+v, want count=1 average=6", q)
	}
	if q := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{"zone": ""})]; q.Count != 1 || q.Average != 7 {
		t.Fatalf("empty-value average = %+v, want count=1 average=7", q)
	}

	// 后续写入仍按真实身份判定重复与冲突，冲突指出真实旧值而非展示结果改出的均值。
	if dup := mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":6}]`); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("unlabeled resubmit must duplicate, got %+v", dup)
	}
	lerr := mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":77,"labels":{"zone":""}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 7 || lerr.Conflict.Submitted != 77 {
		t.Fatalf("empty-value conflict must report existing=7, got %+v", lerr.Conflict)
	}
}

// TestRetainedQueryWindowsStayFixedAfterNewWrite 对应任务书的“保存旧结果后继续
// 写入”场景：区间 [1000,3000]、窗口宽度 1000，原先只有 1000 与 3000 上的采样；
// 保留这份结果后成功写入 1500 和 2500 上的新点。新取得的结果应更新第一个窗口的
// 点数与均值，并出现原先为空的中间窗口；之前保存的结果仍保持原来的两个窗口及
// 统计值。新旧结果中的标签各自独立，修改任意一份不会改变另一份。
func TestRetainedQueryWindowsStayFixedAfterNewWrite(t *testing.T) {
	store := NewMetricStore()
	// 原先只有 1000->2 与 3000->8 两个采样（同一条 host=a 序列）。
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":20,"labels":{"host":"b"}}
	]`)

	oldLine := `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`
	old := mustQueryWindows(t, store, oldLine)
	// 旧结果：第一个窗口一个点值 2，中间窗口为空被省略，收尾窗口一个点值 8。
	oldWindows := []Window{
		window(1000, 1999, 1, 2),
		window(3000, 3000, 1, 8),
	}
	assertWindows(t, old.Series[0].Windows, oldWindows, "old host=a windows before new writes")
	oldAll := mustQueryWindows(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	if len(oldAll.Series) != 2 {
		t.Fatalf("old full windows = %+v, want host=a and host=b", oldAll.Series)
	}

	// 成功写入 1500（第一窗口）与 2500（原为空的中间窗口）两个新点。
	write := mustOK(t, store, `[
		{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2500,"value":6,"labels":{"host":"a"}}
	]`)
	if write.Added != 2 || write.Duplicates != 0 {
		t.Fatalf("new points write = %+v, want added=2", write)
	}

	// 新取得的 host=a 结果：第一个窗口点数 2、均值 (2+4)/2=3，
	// 出现原先为空的中间窗口（2500->6），收尾窗口仍是一个点值 8。
	newRes := mustQueryWindows(t, store, oldLine)
	newWindows := []Window{
		window(1000, 1999, 2, 3),
		window(2000, 2999, 1, 6),
		window(3000, 3000, 1, 8),
	}
	assertWindows(t, newRes.Series[0].Windows, newWindows, "new host=a windows after 1500/2500 writes")

	// 全量新结果中 host=a 同样是三个窗口；同伴 host=b 仍是旧的一个窗口，
	// host=a 新点不串到其他同名不同标签序列。
	newAll := mustQueryWindows(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	if len(newAll.Series) != 2 {
		t.Fatalf("new full windows = %+v, want 2 series", newAll.Series)
	}
	assertWindows(t, newAll.Series[findWindowsIndex(t, newAll, "cpu", map[string]string{"host": "a"})].Windows,
		newWindows, "new host=a full windows")
	assertWindows(t, newAll.Series[findWindowsIndex(t, newAll, "cpu", map[string]string{"host": "b"})].Windows,
		[]Window{window(1000, 1999, 1, 20)}, "host=b untouched by host=a writes")

	// 之前保存的 host=a 结果仍保持原来的两个窗口及统计值。
	if old.Status != "ok" || old.Op != "query_windows" || len(old.Series) != 1 {
		t.Fatalf("retained envelope/series changed: %+v", old)
	}
	assertWindows(t, old.Series[0].Windows, oldWindows, "retained host=a windows stay fixed")
	// 更早保留的全量结果也不跟随：host=a 仍是两个窗口，没有冒出中间窗口。
	assertWindows(t, oldAll.Series[findWindowsIndex(t, oldAll, "cpu", map[string]string{"host": "a"})].Windows,
		oldWindows, "retained full host=a windows stay fixed")

	// 新旧结果中的标签各自独立：篡改旧结果标签不改变新结果，反之亦然。
	old.Series[0].Labels["host"] = "old"
	old.Series[0].Labels["snapshot"] = "old"
	newRes.Series[0].Labels["host"] = "new"
	delete(newRes.Series[0].Labels, "host")
	newRes.Series[0].Labels["snapshot"] = "new"
	freshA := mustQueryWindows(t, store, oldLine)
	if !reflect.DeepEqual(freshA.Series[0].Labels, map[string]string{"host": "a"}) {
		t.Fatalf("host=a identity changed after mutating retained/new labels: %v", freshA.Series[0].Labels)
	}
	assertWindows(t, freshA.Series[0].Windows, newWindows, "fresh host=a windows unaffected by label edits")
	// 两份结果彼此不串标签。
	if v := old.Series[0].Labels["snapshot"]; v != "old" {
		t.Fatalf("old result labels changed: %v", old.Series[0].Labels)
	}
	if v := newRes.Series[0].Labels["snapshot"]; v != "new" {
		t.Fatalf("new result labels changed: %v", newRes.Series[0].Labels)
	}
	if _, ok := old.Series[0].Labels["snapshot2"]; ok {
		t.Fatal("old result must not see new result label edits")
	}

	// 原始明细与区间统计反映的是写入后的真实点集，而不是旧窗口结果。
	qp := mustQueryPoints(t, store, `{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	if !reflect.DeepEqual(qp.Series[0].Points, []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 1500, Value: 4},
		{Timestamp: 2500, Value: 6},
		{Timestamp: 3000, Value: 8},
	}) {
		t.Fatalf("host=a raw points after writes = %+v", qp.Series[0].Points)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 4 || qr.Series[0].Average != 5 {
		t.Fatalf("host=a range stats after writes = %+v, want count=4 average=5", qr.Series)
	}

	// 对原位置提交不同值仍按存储真实旧值报冲突，与任何窗口均值无关。
	lerr := mustFail(t, store, `[{"name":"cpu","timestamp":2500,"value":60,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 6 || lerr.Conflict.Submitted != 60 {
		t.Fatalf("conflict after retained-window scenario must report existing=6, got %+v", lerr.Conflict)
	}
}

// TestQueryWindowsResultIndependentViaProcessLine 统一行处理入口 ProcessLine
// 返回的固定窗口结果同样遵守隔离规则：通过类型断言取得 *QueryWindowsResult 后
// 修改其指标名、标签、窗口（边界/点数/均值/增删/顺序）与序列列表结构，不影响
// 存储、另一份结果与后续经统一入口的写入判定。
func TestQueryWindowsResultIndependentViaProcessLine(t *testing.T) {
	store := NewMetricStore()
	processOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}
	]`)

	// 通过统一入口取得第一份窗口结果并保留（未修改）。
	r := processOK(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	kept, ok := r.(*QueryWindowsResult)
	if !ok {
		t.Fatalf("ProcessLine result type = %T, want *QueryWindowsResult", r)
	}
	if len(kept.Series) != 2 {
		t.Fatalf("baseline via ProcessLine = %+v, want 2 series", kept.Series)
	}

	// 通过统一入口取得第二份窗口结果并由调用方大幅整理。
	r = processOK(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`)
	res := r.(*QueryWindowsResult)
	a := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "a"})]
	a.Name = "renamed"
	a.Labels["host"] = "c"
	a.Windows[0].Start = 5000
	a.Windows[0].End = 5999
	a.Windows[0].Count = 200
	a.Windows[0].Average = 200
	a.Windows = a.Windows[:1]
	a.Windows = append(a.Windows, Window{Start: 6000, End: 6999, Count: 1, Average: 9})
	b := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "b"})]
	delete(b.Labels, "host")
	res.Series = res.Series[:1]
	res.Status = "mutated"
	res.Op = "mutated"

	// 存储事实不变：再经统一入口查询，host=a 仍是原始两个窗口，host=b 不受影响。
	r = processOK(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`)
	fresh := r.(*QueryWindowsResult)
	assertWindows(t, fresh.Series[0].Windows, []Window{
		window(1000, 1999, 2, 3),
	}, "host=a fact via ProcessLine")
	r = processOK(t, store, `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"b"}}`)
	assertWindows(t, r.(*QueryWindowsResult).Series[0].Windows,
		[]Window{window(1000, 1999, 1, 5)}, "host=b fact via ProcessLine")
	// 改出的身份与窗口查不到数据。
	for _, line := range []string{
		`{"op":"query_windows","name":"cpu","start":5000,"end":6999,"step":1000,"labels":{"host":"a"}}`,
		`{"op":"query_windows","name":"cpu","start":0,"end":100000,"step":1000,"labels":{"host":"c"}}`,
		`{"op":"query_windows","name":"renamed","start":0,"end":100000,"step":1000}`,
	} {
		r = processOK(t, store, line)
		if len(r.(*QueryWindowsResult).Series) != 0 {
			t.Fatalf("mutated identity/window %s must not match: %+v", line, r)
		}
	}

	// 先前保留的窗口结果不被污染。
	if kept.Status != "ok" || kept.Op != "query_windows" || len(kept.Series) != 2 {
		t.Fatalf("retained ProcessLine result changed: %+v", kept)
	}
	assertWindows(t, kept.Series[findWindowsIndex(t, kept, "cpu", map[string]string{"host": "a"})].Windows,
		[]Window{window(1000, 1999, 2, 3)}, "retained ProcessLine host=a windows")

	// 后续写入经统一入口仍按真实存储判定：原值重复、不同值冲突且 existing 为真实值。
	r = processOK(t, store, `[{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}}]`)
	br := r.(*BatchResult)
	if br.Added != 0 || br.Duplicates != 1 {
		t.Fatalf("duplicate via ProcessLine = %+v, want one duplicate", br)
	}
	lerr := processFail(t, store, `[{"name":"cpu","timestamp":1500,"value":200,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 4 || lerr.Conflict.Submitted != 200 {
		t.Fatalf("conflict via ProcessLine must report existing=4, got %+v", lerr.Conflict)
	}

	// 原始明细与区间统计经统一入口仍是原始数据对应的结果。
	r = processOK(t, store, `{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	qp := r.(*QueryPointsResult)
	if !reflect.DeepEqual(qp.Series[0].Points, []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 1500, Value: 4},
	}) {
		t.Fatalf("raw points via ProcessLine = %+v", qp.Series[0].Points)
	}
	r = processOK(t, store, `{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	qa := r.(*QueryResult)
	if len(qa.Series) != 1 || qa.Series[0].Count != 2 || qa.Series[0].Average != 3 {
		t.Fatalf("average via ProcessLine = %+v, want count=2 average=3", qa.Series)
	}
}
