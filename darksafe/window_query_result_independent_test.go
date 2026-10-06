package darksafe

import (
	"reflect"
	"testing"
)

// 本文件回归保障固定时间窗口均值查询（op 为 query_windows）成功结果的独立性：
// 业务代码通过 Go 入口取得 query_windows 的结果后，可以改动其中的指标名、标签、
// 窗口边界、点数、均值，增删窗口或调整窗口与序列条目的顺序供本次展示使用；
// 这些改动只能留在那一份返回结果中——
//
//   - 不能改变已写入的采样事实：按原名称与原标签再次查询，窗口边界、点数与
//     均值仍是真实保存数据对应的结果；改出来的名称、标签与窗口不会成为存储内容；
//   - 不能污染此前或此后取得的其他 query_windows 结果，同名不同标签的序列
//     之间也互不影响；
//   - 不能影响写入快照、query 区间统计与 query_points 采样明细；
//   - 不能影响后续写入的重复/冲突判定：对原位置提交不同值仍报告冲突，
//     冲突中的已存在值是真实保存的旧值，而不是结果里改出的窗口均值或其他数值。
//
// 无标签与某个标签值为空字符串仍是不同序列：向返回的空标签集合加键、删除返回
// 结果中的空值标签，都不能合并或改名真实序列。
//
// 两种 Go 入口都在保障范围内：专用入口 QueryWindowsLine 与统一逐行处理入口
// ProcessLine（后者以类型断言取得 *QueryWindowsResult）。与
// success_result_independent_test.go（写入快照与 query）和
// query_points_result_independent_test.go（query_points）的隔离测试相对应。

// findWindowsIndex 在窗口结果中找到指定指标名与完整标签集合的条目下标。
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

// assertWindowsFact 按给定查询再查一次窗口统计，断言结果恰好包含一条序列且其
// 窗口列表（边界、点数、均值、顺序）与预期完全一致。
func assertWindowsFact(t *testing.T, s *MetricStore, line string, want []Window, note string) {
	t.Helper()
	res := mustQueryWindows(t, s, line)
	if len(res.Series) != 1 {
		t.Fatalf("%s: series = %+v, want exactly one", note, res.Series)
	}
	assertWindows(t, res.Series[0].Windows, want, note)
}

// TestQueryWindowsMutationDoesNotChangeStorage 是核心隔离场景：同一指标多条标签
// 不同的序列，调用方拿到窗口结果后改名、增删改标签、篡改窗口边界/点数/均值、
// 增删窗口并调整窗口与序列条目的顺序，都只是在整理本地结果。存储事实、另一份
// 窗口结果、写入快照、query/query_points 与后续写入的重复/冲突判定都不受影响。
func TestQueryWindowsMutationDoesNotChangeStorage(t *testing.T) {
	s := NewMetricStore()
	// host=a：[1000,3000]、step 1000 下窗口 0 有两个点（均值 3）、
	// 中间窗口为空（省略）、收尾窗口一个点（值 8）。
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}},
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}}
	]`)

	hostAWindows := []Window{
		window(1000, 1999, 2, 3),
		window(3000, 3000, 1, 8),
	}
	hostBWindows := []Window{window(1000, 1999, 1, 5)}
	noneWindows := []Window{window(1000, 1999, 1, 6)}
	zoneWindows := []Window{window(1000, 1999, 1, 7)}

	// 先保留一份未修改的窗口结果（全量 cpu），随后篡改另一份。
	kept := mustQueryWindows(t, s, queryWindowsAll("cpu", 1000, 3000, 1000))
	if len(kept.Series) != 4 {
		t.Fatalf("baseline query_windows = %+v, want 4 series", kept.Series)
	}
	res := mustQueryWindows(t, s, queryWindowsAll("cpu", 1000, 3000, 1000))

	// 调用方整理 host=a 条目：改名、改写标签、加标签，篡改两个窗口的边界、
	// 点数与均值，再交换窗口、截短并追加伪造窗口。
	a := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "a"})]
	a.Name = "cpu-renamed"
	a.Labels["host"] = "c"
	a.Labels["zone"] = "z"
	a.Windows[0].Start = 90001
	a.Windows[0].End = 91000
	a.Windows[0].Count = 90
	a.Windows[0].Average = 900
	a.Windows[1].Start = 80001
	a.Windows[1].End = 81000
	a.Windows[1].Count = 80
	a.Windows[1].Average = 800
	a.Windows[0], a.Windows[1] = a.Windows[1], a.Windows[0]
	a.Windows = append(a.Windows[:1], Window{Start: 1, End: 2, Count: 1, Average: 1})

	// 同伴序列 host=b 也被改名、改标签、篡改窗口。
	b := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "b"})]
	b.Name = "cpu-other"
	delete(b.Labels, "host")
	b.Labels["host"] = "d"
	b.Windows[0].Start = 77770
	b.Windows[0].End = 77779
	b.Windows[0].Count = 55
	b.Windows[0].Average = 555

	// 无标签与空值标签条目的标签也被改动。
	none := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{})]
	none.Labels["host"] = "x"
	emptyVal := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"zone": ""})]
	delete(emptyVal.Labels, "zone")

	// 整理序列列表：交换条目、截短列表、追加伪造序列，并篡改信封字段。
	res.Series[0], res.Series[len(res.Series)-1] = res.Series[len(res.Series)-1], res.Series[0]
	res.Series = append(res.Series[:2], QueryWindowsSeries{
		Name:    "ghost",
		Labels:  map[string]string{"host": "g"},
		Windows: []Window{{Start: 4242, End: 4242, Count: 1, Average: 42}},
	})
	res.Status = "mutated"
	res.Op = "mutated"

	// host=a 条目的改动不能影响同伴序列：host=b 的事实仍是窗口 0 一点值 5。
	assertWindowsFact(t, s, queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "b"}),
		hostBWindows, "host=b fact after mutating host=a result")
	// 按原指标名、原标签、原区间查询，仍得到原始两个窗口（含被截掉的收尾窗口）。
	assertWindowsFact(t, s, queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "a"}),
		hostAWindows, "host=a fact after mutating its result")

	// 按仅在返回结果中改出的名称或标签查询，不应凭空出现数据或序列。
	for _, line := range []string{
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "c"}),
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "d"}),
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "g"}),
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "x"}),
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "a", "zone": "z"}),
		queryWindowsAll("cpu-renamed", 1000, 3000, 1000),
		queryWindowsAll("ghost", 1000, 3000, 1000),
	} {
		if qw := mustQueryWindows(t, s, line); len(qw.Series) != 0 {
			t.Fatalf("mutated identity %s must not match, got %+v", line, qw.Series)
		}
	}
	// 按改出的窗口边界查询，同样不应凭空出现窗口（窗口边界只由查询参数决定）。
	for _, line := range []string{
		queryWindowsLabels("cpu", 70000, 92000, 1000, map[string]string{"host": "a"}),
		queryWindowsLabels("cpu", 0, 100, 1000, map[string]string{"host": "a"}),
		queryWindowsAll("cpu-other", 70000, 80000, 10),
	} {
		if qw := mustQueryWindows(t, s, line); len(qw.Series) != 0 {
			t.Fatalf("mutated window bounds %s must not be stored, got %+v", line, qw.Series)
		}
	}

	// 先前保留的另一份窗口结果保留各自原有的标签、窗口与次序。
	wantKept := []struct {
		labels  map[string]string
		windows []Window
	}{
		{map[string]string{}, noneWindows},
		{map[string]string{"host": "a"}, hostAWindows},
		{map[string]string{"host": "b"}, hostBWindows},
		{map[string]string{"zone": ""}, zoneWindows},
	}
	if kept.Status != "ok" || kept.Op != "query_windows" || len(kept.Series) != len(wantKept) {
		t.Fatalf("retained query_windows envelope/series changed: %+v", kept)
	}
	for i, w := range wantKept {
		got := kept.Series[i]
		if got.Name != "cpu" || !reflect.DeepEqual(got.Labels, w.labels) {
			t.Fatalf("retained query_windows series[%d] identity = %+v, want cpu %v", i, got, w.labels)
		}
		assertWindows(t, got.Windows, w.windows, "retained query_windows series")
	}

	// 写入快照也保留原有标签与采样点，序列数仍是 4。
	snap := mustOK(t, s, `[]`)
	if len(snap.Series) != 4 {
		t.Fatalf("snapshot after query_windows mutation = %+v, want 4 series", snap.Series)
	}
	snapA := snap.Series[findViewIndex(t, snap, "cpu", map[string]string{"host": "a"})]
	if !reflect.DeepEqual(snapA.Points, []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 1500, Value: 4},
		{Timestamp: 3000, Value: 8},
	}) {
		t.Fatalf("snapshot host=a points = %+v", snapA.Points)
	}
	snapB := snap.Series[findViewIndex(t, snap, "cpu", map[string]string{"host": "b"})]
	if !reflect.DeepEqual(snapB.Points, []Point{{Timestamp: 1000, Value: 5}}) {
		t.Fatalf("snapshot host=b points = %+v, want (1000,5)", snapB.Points)
	}

	// 原序列、原时间戳、原值再次提交仍计为重复（存储认的是真实值，不是改出的均值）。
	dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("resubmit original host=a point = %+v, want one duplicate", dup)
	}
	dup = mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("resubmit original host=b point = %+v, want one duplicate", dup)
	}

	// 对同一位置提交不同值仍拒绝整批；冲突中的已存在值来自真实存储，不受返回
	// 结果里改出的窗口点数/均值影响（host=a 改出的 900、host=b 改出的 555）。
	lerr := mustFail(t, s, `[
		{"name":"cpu","timestamp":3000,"value":99,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":123,"labels":{"host":"a"}}
	]`)
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("conflict must target the first conflicting point, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "cpu" || len(c.Series.Labels) != 1 || c.Series.Labels["host"] != "a" ||
		c.Timestamp != 3000 || c.Existing != 8 || c.Submitted != 99 {
		t.Fatalf("host=a conflict must report stored fact, got %+v", c)
	}
	lerr = mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":555,"labels":{"host":"b"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 555 {
		t.Fatalf("host=b conflict must report existing=5, got %+v", lerr.Conflict)
	}
	// 整批拒绝：本批没有任何点留下，3000 上的旧值仍在。
	if dup = mustOK(t, s, `[{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}}]`); dup.Duplicates != 1 {
		t.Fatalf("conflict batch must not write anything, got %+v", dup)
	}

	// 区间统计与采样明细仍反映真实采样点，不跟随被改动的窗口结果变化。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 3 || qr.Series[0].Average != 14.0/3.0 {
		t.Fatalf("host=a range query after mutation = %+v, want count=3 average=14/3", qr.Series)
	}
	qp := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"b"}}`)
	if len(qp.Series) != 1 || !reflect.DeepEqual(qp.Series[0].Points, []Point{{Timestamp: 1000, Value: 5}}) {
		t.Fatalf("host=b detail query after mutation = %+v, want (1000,5)", qp.Series)
	}
}

// TestQueryWindowsEmptyAndEmptyValueLabelsIndependent 保留两种容易混淆的标签
// 身份：无标签序列与某个标签确实存在但值为空字符串的序列。给返回的空标签集合
// 加键、删除返回的空值标签、篡改返回窗口，都不能合并或改名这两条真实序列；
// 标签子集匹配与后续冲突判定仍按真实身份进行。
func TestQueryWindowsEmptyAndEmptyValueLabelsIndependent(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":6},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"zone":""}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}
	]`)

	res := mustQueryWindows(t, s, queryWindowsAll("cpu", 1000, 3000, 1000))
	none := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{})]
	if none.Labels == nil {
		t.Fatal("query_windows labels for unlabeled series must be non-nil empty map")
	}
	emptyVal := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"zone": ""})]

	// 调用方整理两个返回视图：空集合加键、空值标签删键、篡改窗口边界与统计。
	none.Labels["host"] = "x"
	none.Windows[0].Start = 6000
	none.Windows[0].End = 6999
	none.Windows[0].Count = 66
	none.Windows[0].Average = 66
	delete(emptyVal.Labels, "zone")
	emptyVal.Windows[0].Start = 5000
	emptyVal.Windows[0].End = 5999
	emptyVal.Windows[0].Count = 77
	emptyVal.Windows[0].Average = 77

	// 三条真实序列不被合并或改名：全量窗口查询仍是三条序列，统计各自不变。
	again := mustQueryWindows(t, s, queryWindowsAll("cpu", 1000, 3000, 1000))
	if len(again.Series) != 3 {
		t.Fatalf("full query_windows = %+v, want 3 distinct series", again.Series)
	}
	gotNone := again.Series[findWindowsIndex(t, again, "cpu", map[string]string{})]
	assertWindows(t, gotNone.Windows, []Window{window(1000, 1999, 1, 6)}, "unlabeled windows")
	gotEmpty := again.Series[findWindowsIndex(t, again, "cpu", map[string]string{"zone": ""})]
	assertWindows(t, gotEmpty.Windows, []Window{window(1000, 1999, 1, 7)}, "empty-value windows")

	// 原标签筛选仍成立：加出来的 host=x 不命中；空值标签筛选仍只命中真实空值序列。
	if qw := mustQueryWindows(t, s,
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "x"})); len(qw.Series) != 0 {
		t.Fatalf("added label must not match: %+v", qw.Series)
	}
	zoneEmpty := mustQueryWindows(t, s,
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"zone": ""}))
	if len(zoneEmpty.Series) != 1 ||
		!reflect.DeepEqual(zoneEmpty.Series[0].Labels, map[string]string{"zone": ""}) {
		t.Fatalf("empty-value label selection = %+v, want the zone='' series", zoneEmpty.Series)
	}
	assertWindows(t, zoneEmpty.Series[0].Windows, []Window{window(1000, 1999, 1, 7)},
		"empty-value selection windows")
	// 删除返回的空值标签后，空条件（{}）仍按全部序列返回，两条身份各自保留：
	// 无标签条目仍是空集合，zone="" 条目仍带该键，不会被合并成一条。
	allSeries := mustQueryWindows(t, s,
		queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{}))
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
	// 改出的窗口区间不存在数据。
	if qw := mustQueryWindows(t, s,
		queryWindowsLabels("cpu", 5000, 6999, 1000, map[string]string{"zone": ""})); len(qw.Series) != 0 {
		t.Fatalf("mutated window range must not be stored: %+v", qw.Series)
	}

	// query 区间统计同样不合并两条序列。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":10000}`)
	if len(qr.Series) != 3 {
		t.Fatalf("full average query = %+v, want 3 distinct series", qr.Series)
	}
	if q := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{})]; q.Count != 1 || q.Average != 6 {
		t.Fatalf("unlabeled average = %+v, want count=1 average=6", q)
	}
	if q := qr.Series[findQueryIndex(t, qr, "cpu", map[string]string{"zone": ""})]; q.Count != 1 || q.Average != 7 {
		t.Fatalf("empty-value average = %+v, want count=1 average=7", q)
	}

	// 后续写入仍按真实身份判定重复与冲突，冲突报告真实旧值而非改出的窗口均值。
	if dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":6}]`); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("unlabeled resubmit must duplicate, got %+v", dup)
	}
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":77,"labels":{"zone":""}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 7 || lerr.Conflict.Submitted != 77 {
		t.Fatalf("empty-value conflict must report existing=7, got %+v", lerr.Conflict)
	}
}

// TestRetainedQueryWindowsStayFixedAfterNewWrite 任务书场景：区间 [1000,3000]、
// 窗口宽度 1000，原先只有 1000 与 3000 上的采样（两个非空窗口，中间窗口为空
// 被省略）；保留这份结果后成功写入 1500 与 2500 上的新点，新取得的结果更新
// 第一个窗口的点数与均值并出现原先为空的中间窗口，旧结果保持原来的两个窗口
// 及统计值。新旧结果中的标签也各自独立。
func TestRetainedQueryWindowsStayFixedAfterNewWrite(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}}
	]`)

	oldLine := queryWindowsLabels("cpu", 1000, 3000, 1000, map[string]string{"host": "a"})
	old := mustQueryWindows(t, s, oldLine)
	oldWindows := []Window{
		window(1000, 1999, 1, 2),
		window(3000, 3000, 1, 8),
	}
	assertWindows(t, old.Series[0].Windows, oldWindows, "retained baseline windows")

	// 成功写入 1500->4 与 2500->6：第一窗口增加一点（均值变为 3），
	// 原先为空的中间窗口 [2000,2999] 出现一个点值 6。
	write := mustOK(t, s, `[
		{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2500,"value":6,"labels":{"host":"a"}}
	]`)
	if write.Added != 2 || write.Duplicates != 0 {
		t.Fatalf("new points write = %+v, want added=2", write)
	}

	fresh := mustQueryWindows(t, s, oldLine)
	newWindows := []Window{
		window(1000, 1999, 2, 3),
		window(2000, 2999, 1, 6),
		window(3000, 3000, 1, 8),
	}
	assertWindows(t, fresh.Series[0].Windows, newWindows, "new result reflects new points and the formerly empty window")

	// 之前保存的结果仍保持原来的两个窗口及统计值，不冒出中间窗口。
	if old.Status != "ok" || old.Op != "query_windows" || len(old.Series) != 1 {
		t.Fatalf("retained result envelope/series changed: %+v", old)
	}
	assertWindows(t, old.Series[0].Windows, oldWindows, "retained windows stay fixed after new write")

	// 新旧结果的标签集合各自独立：分别向两份结果加不同的标签，互不串扰，
	// 也不影响存储中的真实身份。
	old.Series[0].Labels["generation"] = "old"
	fresh.Series[0].Labels["generation"] = "new"
	if _, ok := old.Series[0].Labels["generation"]; !ok || old.Series[0].Labels["generation"] != "old" {
		t.Fatalf("old result labels changed unexpectedly: %v", old.Series[0].Labels)
	}
	if g := fresh.Series[0].Labels["generation"]; g != "new" {
		t.Fatalf("new result labels leaked into old result or vice versa: old=%v new=%v",
			old.Series[0].Labels, fresh.Series[0].Labels)
	}
	again := mustQueryWindows(t, s, oldLine)
	if !reflect.DeepEqual(again.Series[0].Labels, map[string]string{"host": "a"}) {
		t.Fatalf("stored labels must stay host=a, got %v", again.Series[0].Labels)
	}
	assertWindows(t, again.Series[0].Windows, newWindows, "fact after mutating both retained results")

	// 修改旧结果窗口的统计也不影响新结果与存储事实。
	old.Series[0].Windows[0].Count = 99
	old.Series[0].Windows[0].Average = 99
	stillFresh := mustQueryWindows(t, s, oldLine)
	assertWindows(t, stillFresh.Series[0].Windows, newWindows, "mutating old window stats does not reach storage")

	// 原始采样明细与区间统计查询反映真实的四个采样点。
	qp := mustQueryPoints(t, s, `{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	if !reflect.DeepEqual(qp.Series[0].Points, []Point{
		{Timestamp: 1000, Value: 2},
		{Timestamp: 1500, Value: 4},
		{Timestamp: 2500, Value: 6},
		{Timestamp: 3000, Value: 8},
	}) {
		t.Fatalf("detail query must list the four true points: %+v", qp.Series[0].Points)
	}
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`)
	if qr.Series[0].Count != 4 || qr.Series[0].Average != 5 {
		t.Fatalf("range query = %+v, want count=4 average=5", qr.Series)
	}

	// 旧结果里原位置的旧值仍是冲突判定依据，而不是旧结果中被改出的 99。
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":20,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 2 || lerr.Conflict.Submitted != 20 {
		t.Fatalf("conflict after retained-result mutation must report existing=2, got %+v", lerr.Conflict)
	}
}

// TestQueryWindowsResultIndependentViaProcessLine 统一逐行处理入口 ProcessLine
// 返回的窗口结果同样遵守隔离规则：通过类型断言取得 *QueryWindowsResult 后修改
// 其指标名、标签、窗口边界/点数/均值与列表结构，不影响存储、另一份结果与后续
// 经统一入口的写入判定；保存旧结果后继续写入时，新结果更新而旧结果固定。
func TestQueryWindowsResultIndependentViaProcessLine(t *testing.T) {
	s := NewMetricStore()
	processOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":8,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"b"}}
	]`)
	line := `{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000}`

	// 通过统一入口取得第一份窗口结果并保留（未修改）。
	r := processOK(t, s, line)
	kept, ok := r.(*QueryWindowsResult)
	if !ok {
		t.Fatalf("ProcessLine result type = %T, want *QueryWindowsResult", r)
	}
	if kept.Op != "query_windows" || len(kept.Series) != 2 {
		t.Fatalf("baseline via ProcessLine = %+v, want query_windows with 2 series", kept)
	}
	hostAOld := []Window{window(1000, 1999, 1, 2), window(3000, 3000, 1, 8)}
	assertWindows(t, kept.Series[findWindowsIndex(t, kept, "cpu", map[string]string{"host": "a"})].Windows,
		hostAOld, "retained ProcessLine host=a windows")

	// 通过统一入口取得第二份窗口结果并由调用方大幅整理。
	res := processOK(t, s, line).(*QueryWindowsResult)
	a := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "a"})]
	a.Name = "renamed"
	a.Labels["host"] = "c"
	a.Windows[0].Start = 90001
	a.Windows[0].Count = 200
	a.Windows[0].Average = 200
	a.Windows = a.Windows[:1]
	a.Windows = append(a.Windows, Window{Start: 2, End: 2, Count: 20, Average: 20})
	bs := &res.Series[findWindowsIndex(t, res, "cpu", map[string]string{"host": "b"})]
	delete(bs.Labels, "host")
	res.Series = res.Series[:1]
	res.Status = "mutated"
	res.Op = "mutated"

	// 存储事实不变：再经统一入口查询，host=a 仍是原始两个窗口，host=b 不变。
	fresh := processOK(t, s,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`).(*QueryWindowsResult)
	assertWindows(t, fresh.Series[0].Windows, hostAOld, "host=a fact via ProcessLine")
	hostBFresh := processOK(t, s,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"b"}}`).(*QueryWindowsResult)
	assertWindows(t, hostBFresh.Series[0].Windows, []Window{window(1000, 1999, 1, 5)},
		"host=b fact via ProcessLine")
	// 改出的身份与窗口边界查不到数据。
	for _, q := range []string{
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"c"}}`,
		`{"op":"query_windows","name":"renamed","start":1000,"end":3000,"step":1000}`,
		`{"op":"query_windows","name":"cpu","start":90000,"end":92000,"step":1000,"labels":{"host":"a"}}`,
	} {
		r = processOK(t, s, q)
		if len(r.(*QueryWindowsResult).Series) != 0 {
			t.Fatalf("mutated identity/bounds %s must not match: %+v", q, r)
		}
	}

	// 先前保留的窗口结果不被污染。
	if kept.Status != "ok" || kept.Op != "query_windows" || len(kept.Series) != 2 {
		t.Fatalf("retained ProcessLine result changed: %+v", kept)
	}
	assertWindows(t, kept.Series[findWindowsIndex(t, kept, "cpu", map[string]string{"host": "a"})].Windows,
		hostAOld, "retained ProcessLine host=a windows after mutation")

	// 后续写入经统一入口仍按真实存储判定：原值重复、不同值冲突且 existing 为
	// 真实旧值（不是结果里改出的窗口均值 200）。
	r = processOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	br := r.(*BatchResult)
	if br.Added != 0 || br.Duplicates != 1 {
		t.Fatalf("duplicate via ProcessLine = %+v, want one duplicate", br)
	}
	lerr := processFail(t, s, `[{"name":"cpu","timestamp":1000,"value":200,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 2 || lerr.Conflict.Submitted != 200 {
		t.Fatalf("conflict via ProcessLine must report existing=2, got %+v", lerr.Conflict)
	}

	// query 与 query_points 经统一入口仍是原始数据对应的结果。
	qa := processOK(t, s, `{"op":"query","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`).(*QueryResult)
	if len(qa.Series) != 1 || qa.Series[0].Count != 2 || qa.Series[0].Average != 5 {
		t.Fatalf("average via ProcessLine = %+v, want count=2 average=5", qa.Series)
	}
	qpd := processOK(t, s, `{"op":"query_points","name":"cpu","start":0,"end":10000,"labels":{"host":"a"}}`).(*QueryPointsResult)
	if !reflect.DeepEqual(qpd.Series[0].Points, []Point{
		{Timestamp: 1000, Value: 2}, {Timestamp: 3000, Value: 8},
	}) {
		t.Fatalf("detail via ProcessLine = %+v", qpd.Series[0].Points)
	}

	// 保存旧结果后继续写入：新窗口结果更新第一窗口并出现中间窗口，旧结果固定。
	processOK(t, s, `[
		{"name":"cpu","timestamp":1500,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2500,"value":6,"labels":{"host":"a"}}
	]`)
	grown := processOK(t, s,
		`{"op":"query_windows","name":"cpu","start":1000,"end":3000,"step":1000,"labels":{"host":"a"}}`).(*QueryWindowsResult)
	assertWindows(t, grown.Series[0].Windows, []Window{
		window(1000, 1999, 2, 3),
		window(2000, 2999, 1, 6),
		window(3000, 3000, 1, 8),
	}, "new ProcessLine result grows with committed points")
	assertWindows(t, kept.Series[findWindowsIndex(t, kept, "cpu", map[string]string{"host": "a"})].Windows,
		hostAOld, "retained ProcessLine result stays at its original two windows")
}
