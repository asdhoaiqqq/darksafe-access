package darksafe

import (
	"testing"
)

// 本文件回归保障写入批次在“不同采样点分别出现数值冲突与字段错误”时的错误选择，
// 以及同一采样点字段问题与数值冲突并存时的优先级。规则保持现状：
//
//   - 整行 JSON 合法时按数组先后报告第一个失败采样点（index 从 1 开始）：
//     先出现的数值冲突带完整 conflict 细节，后面采样点的类型错误不能替换它；
//     交换位置后则报告先出现的字段类型错误，不带 conflict，也不并列后面的冲突。
//   - 同一采样点只要带字段问题（如未知字段 bogus，即使 name/timestamp/value
//     都写在它前面），就报字段错误而不是数值冲突；删去未知字段后才是真正的冲突。
//   - 数组未闭合属于整行解析失败：即使前面的采样点已具备冲突条件，也只报
//     整行解析错误，不带 index 与 conflict。
//
// 每个失败批次都是原子的：批内排在前面的合法新增点不提交，已存原值不被覆盖；
// 随后合法批次仍可正常新增，查询统计随之更新。

// seedHostA 写入基线点：指标 cpu、标签 host=a、时间戳 1000、值 2。
func seedHostA(t *testing.T) *MetricStore {
	t.Helper()
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	return store
}

// assertConflictDetail 校验数值冲突结果的完整细节：index、错误文案、
// 指标名、完整标签集合、时间戳以及 existing/submitted 都必须精确对应被报告的点。
func assertConflictDetail(t *testing.T, lerr *LineError, wantIndex int, wantMsg string,
	wantName string, wantLabels map[string]string, wantTS int64, wantExisting, wantSubmitted float64) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected conflict failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	if lerr.Error != wantMsg {
		t.Fatalf("error = %q, want %q", lerr.Error, wantMsg)
	}
	c := lerr.Conflict
	if c == nil {
		t.Fatalf("conflict failure must carry conflict detail, got %+v", lerr)
	}
	if c.Series.Name != wantName {
		t.Fatalf("conflict series name = %q, want %q", c.Series.Name, wantName)
	}
	if len(c.Series.Labels) != len(wantLabels) {
		t.Fatalf("conflict labels = %+v, want %+v", c.Series.Labels, wantLabels)
	}
	for k, v := range wantLabels {
		if got, ok := c.Series.Labels[k]; !ok || got != v {
			t.Fatalf("conflict labels = %+v, want %+v", c.Series.Labels, wantLabels)
		}
	}
	if c.Timestamp != wantTS || c.Existing != wantExisting || c.Submitted != wantSubmitted {
		t.Fatalf("conflict detail = ts %d existing %v submitted %v, want ts %d existing %v submitted %v",
			c.Timestamp, c.Existing, c.Submitted, wantTS, wantExisting, wantSubmitted)
	}
}

const conflictHostA1000Msg = `conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9`

// 整行合法、三个采样点：第一个是新时间戳上的合法新增，第二个与已存点冲突，
// 第三个 name 类型错误。必须只报告第二个点的数值冲突：index 为 2，
// conflict 中的指标名、完整标签、时间戳及 existing=2、submitted=9 全部对应它，
// 后面的类型错误不能替换该原因。
func TestConflictAtEarlierSampleBeatsLaterTypeError(t *testing.T) {
	store := seedHostA(t)

	lerr := mustFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}},
		{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}}
	]`)
	assertConflictDetail(t, lerr, 2, conflictHostA1000Msg,
		"cpu", map[string]string{"host": "a"}, 1000, 2, 9)

	// 整批原子回滚：第一个采样点（时间戳 2000 的新增）不得留下，
	// 时间戳 1000 上仍是原来的 2。
	assertStoreHasOnly(t, store, "cpu", 1000, 2)
}

// 交换冲突点与类型错误点的位置后：第二个采样点 name 为数字，必须报告该点的
// name 类型错误（index 仍为 2），不附带 conflict，也不同时列出后面第三个点
// 的数值冲突。
func TestFieldErrorAtEarlierSampleBeatsLaterConflict(t *testing.T) {
	store := seedHostA(t)

	lerr := mustFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}
	]`)
	assertSampleError(t, lerr, 2, `field "name" must be a string`)

	// 失败批次前面的合法新增点同样不提交，基线数据保持原值。
	assertStoreHasOnly(t, store, "cpu", 1000, 2)
}

// 同一个采样点与已存点同序列、同时间戳且值不同，但还携带未知字段 bogus：
// 无论 bogus 写在所有已知字段之后，还是 name/timestamp/value 都写在 bogus
// 前面（labels 在其后），都只返回未知字段错误而不是数值冲突；删去 bogus、
// 其余内容不变后，才报告真正的数值冲突。
func TestUnknownFieldAtConflictingSampleMasksConflict(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		// bogus 在 labels 之后：四个已知字段都先出现。
		{"bogus after all known fields",
			`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"},"bogus":1}]`},
		// name/timestamp/value 都写在 bogus 前面，labels 在 bogus 之后：结果相同。
		{"bogus between value and labels",
			`[{"name":"cpu","timestamp":1000,"value":9,"bogus":1,"labels":{"host":"a"}}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := seedHostA(t)

			lerr := mustFail(t, store, tc.line)
			assertSampleError(t, lerr, 1, `unknown field "bogus"`)
			// 字段错误不允许伪装成冲突，原值 2 必须保留。
			assertStoreHasOnly(t, store, "cpu", 1000, 2)

			// 对照：删去未知字段、其余内容不变，同一时间戳才是真正的数值冲突。
			lerr = mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`)
			assertConflictDetail(t, lerr, 1, conflictHostA1000Msg,
				"cpu", map[string]string{"host": "a"}, 1000, 2, 9)
			assertStoreHasOnly(t, store, "cpu", 1000, 2)
		})
	}
}

// 数组未闭合是整行解析失败：即使第一个采样点已具备与已存数据的冲突条件，
// 也只返回整行解析错误，不带 index 或 conflict；行内任何点都不提交。
func TestUnclosedArrayBeatsEarlierConflict(t *testing.T) {
	store := seedHostA(t)

	// 第一个点本身是新时间戳上的合法新增，第二个点（值 9）在同一序列的
	// 1000 上与已存值 2 冲突，但数组缺少闭合。
	_, lerr := store.IngestLine(`[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}
	`)
	assertWholeLineTextError(t, lerr, "invalid JSON")

	// 整行失败：新增点未留下，原值未覆盖。
	assertStoreHasOnly(t, store, "cpu", 1000, 2)
}

// 连续多个失败批次之后，存储状态仍只有基线点；随后提交时间戳 2000、值 4 的
// 合法批次应正常新增，区间查询得到两个点、count=2、average=3。
func TestStoreStateAfterMixedFailuresThenRecovery(t *testing.T) {
	store := seedHostA(t)

	mustFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}},
		{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}}
	]`)
	mustFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":7,"timestamp":3000,"value":1,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}
	]`)
	mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"},"bogus":1}]`)

	// 恢复前：覆盖两个时间戳的区间只有一个点、均值 2。
	q := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != 2 {
		t.Fatalf("query after failures = %+v, want one point average 2", q.Series)
	}

	// 合法批次正常新增。
	res := mustOK(t, store, `[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("recovery batch = %+v, want added=1 duplicates=0", res)
	}

	// 恢复后：两个点、均值 (2+4)/2 = 3。
	q = mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`)
	if len(q.Series) != 1 {
		t.Fatalf("query series = %+v, want exactly host=a", q.Series)
	}
	s0 := q.Series[0]
	if s0.Name != "cpu" || len(s0.Labels) != 1 || s0.Labels["host"] != "a" {
		t.Fatalf("query identity = %+v, want cpu{host=a}", s0)
	}
	if s0.Count != 2 || s0.Average != 3 {
		t.Fatalf("query after recovery = count %d average %v, want count 2 average 3",
			s0.Count, s0.Average)
	}
}
