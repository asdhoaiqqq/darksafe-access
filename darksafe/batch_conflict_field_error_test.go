package darksafe

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件回归保障“同一失败批次中数值冲突与采样点字段错误同时出现时”的错误
// 选择规则与失败后的数据状态，固定用户收到的唯一失败原因与存储事实：
//
//   - 按数组中采样点的先后位置报告第一个失败的采样点：某点与已存数据在同一
//     序列、同一时间戳上数值不同，就报告该点的数值冲突（conflict 中的指标名、
//     完整标签、时间戳及 existing/submitted 都对应该点），其后采样点的字段
//     类型错误不能替换该原因；交换两个点的位置后只报类型错误，不附带
//     conflict，也不同时列出后面的冲突。
//   - 同一采样点自身的字段校验（未知字段）先于数值冲突：该点即使与已存点
//     同序列、同时间戳且值不同，也只报字段错误；已知字段写在未知字段前后
//     结果相同；删去未知字段、其余内容不变后才是真正的数值冲突。
//   - 数组未闭合属于整行解析失败：即使数组前面的采样点已经具备冲突条件，
//     也只返回整行解析错误，不带 index 或 conflict。
//   - 任何失败批次整批不提交：批次内排在前面的合法新增点不留存，已存原值
//     不被覆盖；随后提交的合法批次正常新增，查询如实反映最终数据。
//
// 本文件只锁定既有行为，不引入新的错误选择规则。

// newCPUHostABaseline 先成功写入 cpu、host=a、时间戳 1000、值 2，
// 返回持有该基线数据的存储。
func newCPUHostABaseline(t *testing.T) *MetricStore {
	t.Helper()
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	return store
}

// assertConflictError 校验数值冲突失败：index 指向 wantIndex（从 1 开始），
// conflict 中的指标名、完整标签、时间戳与 existing/submitted 都对应该采样点。
func assertConflictError(t *testing.T, lerr *LineError, wantIndex int,
	wantName string, wantLabels map[string]string, wantTS int64,
	wantExisting, wantSubmitted float64) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected conflict failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	if lerr.Conflict == nil {
		t.Fatalf("error %q must carry a conflict", lerr.Error)
	}
	c := lerr.Conflict
	if c.Series.Name != wantName || !reflect.DeepEqual(c.Series.Labels, wantLabels) ||
		c.Timestamp != wantTS || c.Existing != wantExisting || c.Submitted != wantSubmitted {
		t.Fatalf("conflict = %+v, want series %s%v timestamp %d existing=%v submitted=%v",
			c, wantName, wantLabels, wantTS, wantExisting, wantSubmitted)
	}
}

// assertCPUHostAQuery 校验区间查询只看到 host=a 一个序列，且点数与均值符合预期。
func assertCPUHostAQuery(t *testing.T, store *MetricStore, wantCount int, wantAverage float64) {
	t.Helper()
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 {
		t.Fatalf("query series = %+v, want exactly host=a", qr.Series)
	}
	got := qr.Series[0]
	if got.Name != "cpu" || !reflect.DeepEqual(got.Labels, map[string]string{"host": "a"}) ||
		got.Count != wantCount || got.Average != wantAverage {
		t.Fatalf("host=a query = %+v, want count=%d average=%v", got, wantCount, wantAverage)
	}
}

// 批内第一个点合法新增（ts2000=4）、第二个点与已存点冲突（ts1000=9）、
// 第三个点 name 是数字：只报告第二个点的数值冲突，第三个点的类型错误不能
// 替换该原因；冲突细节完整对应第二个点。
func TestConflictReportedBeforeLaterSampleTypeError(t *testing.T) {
	store := newCPUHostABaseline(t)

	lerr := mustFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}},
		{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}}
	]`)
	assertConflictError(t, lerr, 2, "cpu", map[string]string{"host": "a"}, 1000, 2, 9)

	wantText := "conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9"
	if !strings.Contains(lerr.Error, wantText) {
		t.Fatalf("error text = %q, want substring %q", lerr.Error, wantText)
	}
	if strings.Contains(lerr.Error, "must be a string") {
		t.Fatalf("later sample's type error must not replace the conflict reason: %q", lerr.Error)
	}

	// 失败批次前面的新增点不留存，原值 2 不被覆盖。
	assertStoreHasOnly(t, store, "cpu", 1000, 2)
	assertCPUHostAQuery(t, store, 1, 2)
}

// 交换后两个点的位置（name 类型错误提前到第二个点、冲突点挪到第三个）：
// 只报告第二个点的 name 类型错误，不附带 conflict，也不同时列出后面的冲突。
func TestSwapPutsTypeErrorFirstAndDropsConflict(t *testing.T) {
	store := newCPUHostABaseline(t)

	lerr := mustFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}
	]`)
	assertSampleError(t, lerr, 2, `field "name" must be a string`)
	if strings.Contains(lerr.Error, "conflict") {
		t.Fatalf("field type error must not also list the later conflict: %q", lerr.Error)
	}

	assertStoreHasOnly(t, store, "cpu", 1000, 2)
	assertCPUHostAQuery(t, store, 1, 2)
}

// 同一采样点与已存点同序列、同时间戳且值不同，但携带未知字段 bogus：
// 返回未知字段错误而不是数值冲突；bogus 写在已知字段之后或之前都一样。
// 删去 bogus、其余内容不变后，才报告真正的数值冲突。
func TestUnknownFieldOnConflictingSampleBeatsConflict(t *testing.T) {
	store := newCPUHostABaseline(t)

	// name、timestamp、value（及 labels）都写在 bogus 前面：仍是未知字段错误。
	lerr := mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"},"bogus":1}]`)
	assertSampleError(t, lerr, 1, `unknown field "bogus"`)
	assertStoreHasOnly(t, store, "cpu", 1000, 2)

	// bogus 写在最前面，结果相同。
	lerr = mustFail(t, store, `[{"bogus":1,"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`)
	assertSampleError(t, lerr, 1, `unknown field "bogus"`)
	assertStoreHasOnly(t, store, "cpu", 1000, 2)

	// 删去未知字段、其余内容不变：这才是真正的数值冲突。
	lerr = mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`)
	assertConflictError(t, lerr, 1, "cpu", map[string]string{"host": "a"}, 1000, 2, 9)
	if !strings.Contains(lerr.Error,
		"conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9") {
		t.Fatalf("genuine conflict error text = %q", lerr.Error)
	}
	// 冲突批次同样整批拒绝：原值仍是 2。
	assertStoreHasOnly(t, store, "cpu", 1000, 2)
	assertCPUHostAQuery(t, store, 1, 2)
}

// 数组未闭合：即使前面的采样点已经具备冲突条件（第二个点对象完整、与已存点
// 在 ts1000 上值不同），也只返回整行解析错误，不带 index 或 conflict，
// 行内第一个合法新增点同样不留存。
func TestUnclosedArrayWithConflictConditionIsWholeLineParseError(t *testing.T) {
	store := newCPUHostABaseline(t)

	_, lerr := store.IngestLine(`[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}
	`)
	assertWholeLineTextError(t, lerr, "invalid JSON")

	assertStoreHasOnly(t, store, "cpu", 1000, 2)
	assertCPUHostAQuery(t, store, 1, 2)
}

// 连续经历冲突、后置类型错误、未知字段、未闭合数组四类失败批次后，已存数据
// 始终是最初的一个点（ts1000=2）；随后提交 ts2000=4 的合法批次正常新增，
// 查询得到两个点、均值 3。
func TestFailedBatchesPreserveFactThenValidBatchRecovers(t *testing.T) {
	store := newCPUHostABaseline(t)

	failedBatches := []string{
		// 冲突在后、类型错误更后：冲突失败。
		`[
			{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
			{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}},
			{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}}
		]`,
		// 交换后：name 类型错误失败。
		`[
			{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
			{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}},
			{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}
		]`,
		// 冲突条件点携带未知字段：字段错误失败。
		`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"},"bogus":1}]`,
		// 真正的数值冲突。
		`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,
		// 数组未闭合：整行解析失败。
		`[
			{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
			{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}
		`,
	}
	for i, line := range failedBatches {
		if _, lerr := store.IngestLine(line); lerr == nil {
			t.Fatalf("failed batch %d unexpectedly succeeded", i+1)
		}
		// 每个失败批次之后事实都不变：只有 host=a 的一个点，均值 2。
		assertCPUHostAQuery(t, store, 1, 2)
	}

	// 合法批次正常新增 ts2000=4。
	res := mustOK(t, store, `[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("recovering batch = %+v, want added=1 duplicates=0", res)
	}
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 2 {
		t.Fatalf("snapshot after recovery = %+v, want one series with two points", snap.Series)
	}
	assertCPUHostAQuery(t, store, 2, 3)
}
