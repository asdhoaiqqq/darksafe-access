package darksafe

import (
	"fmt"
	"math"
	"testing"
)

// 本文件回归保障数值去重以“实际存储的有限 float64 值”为准：同一序列、同一时间戳
// 再次提交时，比较的是 JSON 数字转换后的 float64，而不是数字的原始文本，也不是
// 十进制下的“数值接近”。重点覆盖两类边界：
//
//   - 不同十进制写法转换成同一个存储值：9007199254740993 不可精确表示，与
//     9007199254740992 存为同一个 float64（2^53）；5e-324 与
//     4.9406564584124654e-324 都舍入为最小次正规值。它们是重复，成功忽略。
//   - 相邻可表示值仍须区分：9007199254740994 是 2^53 的下一个可表示值，
//     1e-323 是最小次正规值的两倍，它们与已存值不同，必须冲突而不是被合并。
//
// 规则在同一数组内与跨成功批次一致；冲突中的 existing/submitted 与错误原因
// 都对应实际存储值，失败批次整批回滚，成功忽略的重复不影响查询的 count 与
// average，写入计数只统计本批。

const (
	lit2Pow53       = "9007199254740992"        // 2^53，可精确表示
	lit2Pow53Plus1  = "9007199254740993"        // 不可表示，舍入到 2^53
	lit2Pow53Plus2  = "9007199254740994"        // 2^53 的下一个可表示值
	litMinSubnormal = "5e-324"                  // 舍入为最小次正规值
	litMinSubExact  = "4.9406564584124654e-324" // 最小次正规值的精确十进制写法
	litTwoMinSub    = "1e-323"                  // 最小次正规值的两倍（相邻可表示值）
)

var (
	stored2Pow53      = math.Float64frombits(0x4340000000000000) // 9007199254740992
	stored2Pow53Plus2 = math.Float64frombits(0x4340000000000001) // 9007199254740994
	storedMinSub      = math.Float64frombits(0x0000000000000001) // 最小次正规值
	storedTwoMinSub   = math.Float64frombits(0x0000000000000002) // 其两倍
)

// sampleLine 构造只含一个采样点的写入批次行。
func sampleLine(name string, ts int64, valueLiteral string) string {
	return fmt.Sprintf(`[{"name":%q,"timestamp":%d,"value":%s}]`, name, ts, valueLiteral)
}

// conflictMsg 按输出格式拼装冲突原因，existing/submitted 以存储值格式化。
func conflictMsg(name string, ts int64, existing, submitted float64) string {
	return fmt.Sprintf("conflict: series %s{} at timestamp %d already has value %s, submitted %s",
		name, ts, formatFloat(existing), formatFloat(submitted))
}

// assertConflictBits 校验冲突结果：index、错误原因、conflict 中的时间戳与
// existing/submitted 都必须按位对应实际存储的 float64 值。
func assertConflictBits(t *testing.T, lerr *LineError, wantIndex int, wantName string,
	wantTS int64, wantExisting, wantSubmitted float64) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected conflict failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	wantMsg := conflictMsg(wantName, wantTS, wantExisting, wantSubmitted)
	if lerr.Error != wantMsg {
		t.Fatalf("error = %q, want %q", lerr.Error, wantMsg)
	}
	c := lerr.Conflict
	if c == nil {
		t.Fatalf("conflict failure must carry conflict detail, got %+v", lerr)
	}
	if c.Series.Name != wantName || len(c.Series.Labels) != 0 {
		t.Fatalf("conflict series = %+v, want %s with no labels", c.Series, wantName)
	}
	if c.Timestamp != wantTS {
		t.Fatalf("conflict timestamp = %d, want %d", c.Timestamp, wantTS)
	}
	if math.Float64bits(c.Existing) != math.Float64bits(wantExisting) ||
		math.Float64bits(c.Submitted) != math.Float64bits(wantSubmitted) {
		t.Fatalf("conflict values = existing %v (%016x) submitted %v (%016x), want %v (%016x) / %v (%016x)",
			c.Existing, math.Float64bits(c.Existing), c.Submitted, math.Float64bits(c.Submitted),
			wantExisting, math.Float64bits(wantExisting), wantSubmitted, math.Float64bits(wantSubmitted))
	}
}

// assertSinglePointBits 校验存储中恰好只有 name 序列的一个点，且值按位等于 want。
func assertSinglePointBits(t *testing.T, store *MetricStore, name string, ts int64, want float64) {
	t.Helper()
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || snap.Series[0].Name != name {
		t.Fatalf("snapshot = %+v, want exactly series %q", snap.Series, name)
	}
	pts := snap.Series[0].Points
	if len(pts) != 1 || pts[0].Timestamp != ts ||
		math.Float64bits(pts[0].Value) != math.Float64bits(want) {
		t.Fatalf("points = %+v, want exactly (%d, %v [%016x])",
			pts, ts, want, math.Float64bits(want))
	}
}

// assertQueryBits 校验查询结果：唯一序列、count 与按位精确的 average。
func assertQueryBits(t *testing.T, store *MetricStore, name string, wantCount int, wantAvg float64) {
	t.Helper()
	q := mustQuery(t, store, fmt.Sprintf(
		`{"op":"query","name":%q,"start":-100,"end":100}`, name))
	if len(q.Series) != 1 {
		t.Fatalf("query series = %+v, want exactly %q", q.Series, name)
	}
	s0 := q.Series[0]
	if s0.Count != wantCount || math.Float64bits(s0.Average) != math.Float64bits(wantAvg) {
		t.Fatalf("query = count %d average %v (%016x), want count %d average %v (%016x)",
			s0.Count, s0.Average, math.Float64bits(s0.Average),
			wantCount, wantAvg, math.Float64bits(wantAvg))
	}
}

// 先确认测试所用的字面量确实按预期舍入：等值组转换成同一个存储值，
// 相邻值转换成不同的存储值。这是后续所有断言成立的前提。
func TestValueLiteralsRoundToExpectedStoredValues(t *testing.T) {
	cases := []struct {
		literal string
		want    float64
	}{
		{lit2Pow53, stored2Pow53},
		{lit2Pow53Plus1, stored2Pow53},      // 与 2^53 同一个存储值
		{lit2Pow53Plus2, stored2Pow53Plus2}, // 相邻可表示值，必须不同
		{"-" + lit2Pow53, -stored2Pow53},
		{"-" + lit2Pow53Plus1, -stored2Pow53},
		{"-" + lit2Pow53Plus2, -stored2Pow53Plus2},
		{litMinSubnormal, storedMinSub},
		{litMinSubExact, storedMinSub},  // 与 5e-324 同一个存储值
		{litTwoMinSub, storedTwoMinSub}, // 相邻可表示值，必须不同
	}
	for _, tc := range cases {
		if got := parseJSONFloat(t, tc.literal); math.Float64bits(got) != math.Float64bits(tc.want) {
			t.Fatalf("stored value of %s = %v (%016x), want %v (%016x)",
				tc.literal, got, math.Float64bits(got), tc.want, math.Float64bits(tc.want))
		}
	}
	// 相邻可表示值之间必须真的不同（防止把“数值接近”当成相等）。
	if stored2Pow53 == stored2Pow53Plus2 || storedMinSub == storedTwoMinSub {
		t.Fatalf("test constants broken: adjacent representable values must differ")
	}
}

// 跨成功批次：先写入 9007199254740992，再提交 9007199254740993——两者存为同一个
// float64，后一次成功忽略（added 0、duplicates 1），快照仍只有原来的一个点，
// 查询 count 与 average 均不受重复影响。交换提交顺序（先写不可表示的写法，
// 再提交精确写法）结果相同：判等只看存储值，与哪种写法先到无关。
func TestValueDedupSameStoredValueAcrossBatches(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, then string
		stored      float64
	}{
		{"exact literal first", lit2Pow53, lit2Pow53Plus1, stored2Pow53},
		{"rounded literal first", lit2Pow53Plus1, lit2Pow53, stored2Pow53},
		{"negative exact first", "-" + lit2Pow53, "-" + lit2Pow53Plus1, -stored2Pow53},
		{"negative rounded first", "-" + lit2Pow53Plus1, "-" + lit2Pow53, -stored2Pow53},
		{"min subnormal decimal first", litMinSubnormal, litMinSubExact, storedMinSub},
		{"min subnormal exact first", litMinSubExact, litMinSubnormal, storedMinSub},
		{"negative min subnormal", "-" + litMinSubnormal, "-" + litMinSubExact, -storedMinSub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			res := mustOK(t, store, sampleLine("m", 1, tc.first))
			if res.Added != 1 || res.Duplicates != 0 {
				t.Fatalf("first write = added %d duplicates %d, want 1/0", res.Added, res.Duplicates)
			}

			res = mustOK(t, store, sampleLine("m", 1, tc.then))
			if res.Added != 0 || res.Duplicates != 1 {
				t.Fatalf("equivalent rewrite = added %d duplicates %d, want 0/1",
					res.Added, res.Duplicates)
			}
			// 成功快照仍展示全部已提交数据：只有原来的一个点，值为存储值。
			if len(res.Series) != 1 || len(res.Series[0].Points) != 1 ||
				math.Float64bits(res.Series[0].Points[0].Value) != math.Float64bits(tc.stored) {
				t.Fatalf("snapshot after duplicate = %+v, want single point %v (%016x)",
					res.Series, tc.stored, math.Float64bits(tc.stored))
			}

			// 被忽略的重复不增加 count、不改变 average。
			assertQueryBits(t, store, "m", 1, tc.stored)
		})
	}
}

// 跨成功批次：9007199254740994 是 2^53 的下一个可表示值，与已存值不同，
// 必须冲突而不是被当成“接近”合并；conflict 的 existing/submitted 与错误原因
// 都对应实际存储值。负数与次正规值（1e-323 是最小次正规值的两倍）遵循同一规则。
// 失败批次不覆盖原值，后续查询仍看到此前的值。
func TestValueConflictAdjacentStoredValueAcrossBatches(t *testing.T) {
	for _, tc := range []struct {
		name                string
		first, then         string
		existing, submitted float64
	}{
		{"positive above 2^53", lit2Pow53, lit2Pow53Plus2, stored2Pow53, stored2Pow53Plus2},
		{"negative below -2^53", "-" + lit2Pow53, "-" + lit2Pow53Plus2, -stored2Pow53, -stored2Pow53Plus2},
		{"min subnormal vs its double", litMinSubnormal, litTwoMinSub, storedMinSub, storedTwoMinSub},
		{"negative min subnormal vs its double", "-" + litMinSubnormal, "-" + litTwoMinSub, -storedMinSub, -storedTwoMinSub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			mustOK(t, store, sampleLine("m", 1, tc.first))

			lerr := mustFail(t, store, sampleLine("m", 1, tc.then))
			assertConflictBits(t, lerr, 1, "m", 1, tc.existing, tc.submitted)

			// 整批拒绝：原值保留，查询仍只有此前的一个点。
			assertSinglePointBits(t, store, "m", 1, tc.existing)
			assertQueryBits(t, store, "m", 1, tc.existing)
		})
	}
}

// 同一数组内：此前不存在的采样位置在本批先出现、随后以等值的另一种数字写法
// 出现时，只新增一个点，duplicates 按被忽略的提交次数计算。
func TestValueDedupSameStoredValueWithinBatch(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":9007199254740992},
		{"name":"m","timestamp":1,"value":9007199254740993},
		{"name":"m","timestamp":1,"value":9007199254740992.0},
		{"name":"t","timestamp":1,"value":5e-324},
		{"name":"t","timestamp":1,"value":4.9406564584124654e-324}
	]`)
	if res.Added != 2 || res.Duplicates != 3 {
		t.Fatalf("counts = added %d duplicates %d, want 2/3", res.Added, res.Duplicates)
	}
	if len(res.Series) != 2 {
		t.Fatalf("series = %+v, want m and t", res.Series)
	}
	// 每个位置只新增一个点，值为存储值。
	for i, want := range []float64{stored2Pow53, storedMinSub} {
		pts := res.Series[i].Points
		if len(pts) != 1 || math.Float64bits(pts[0].Value) != math.Float64bits(want) {
			t.Fatalf("series %d points = %+v, want single point %v (%016x)",
				i, pts, want, math.Float64bits(want))
		}
	}
	assertQueryBits(t, store, "m", 1, stored2Pow53)
	assertQueryBits(t, store, "t", 1, storedMinSub)
}

// 同一数组内：新位置在本批首次出现后，又以不同存储值提交——整批拒绝，
// conflict 的 existing 对应本批较早的值（不是已写入的数据），这两个点都不保留；
// 批内排在冲突点前面的合法新增点同样不提交。
func TestValueConflictWithinBatchRollsBackEverything(t *testing.T) {
	store := NewMetricStore()
	lerr := mustFail(t, store, `[
		{"name":"other","timestamp":1,"value":1},
		{"name":"m","timestamp":1,"value":9007199254740992},
		{"name":"m","timestamp":1,"value":9007199254740994}
	]`)
	// 错误指出冲突点在数组中的位置（从 1 开始）；existing 是本批较早出现的值。
	assertConflictBits(t, lerr, 3, "m", 1, stored2Pow53, stored2Pow53Plus2)

	// 两个冲突点都不保留，前面的合法新增点 other 也不提交：存储为空。
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 0 {
		t.Fatalf("failed batch must not commit anything, snapshot = %+v", snap.Series)
	}

	// 次正规值同一规则：5e-324 与 1e-323 是相邻可表示值，批内冲突同样整批拒绝。
	lerr = mustFail(t, store, `[
		{"name":"t","timestamp":1,"value":5e-324},
		{"name":"t","timestamp":1,"value":1e-323}
	]`)
	assertConflictBits(t, lerr, 2, "t", 1, storedMinSub, storedTwoMinSub)
	if snap = mustOK(t, store, `[]`); len(snap.Series) != 0 {
		t.Fatalf("subnormal conflict batch must not commit anything, snapshot = %+v", snap.Series)
	}
}

// 位置已经存在时：冲突保留此前的值，本批排在冲突点前面的合法新增点也不提交；
// 失败后查询仍看到原值，后续合法输入继续处理。
func TestValueConflictAgainstExistingKeepsStoredAndRollsBack(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, sampleLine("m", 1, lit2Pow53))

	// 批内第一个点是合法新增（新时间戳），第二个点在已存在的位置冲突：
	// 整批拒绝，新增点不提交，已存值不被覆盖。
	lerr := mustFail(t, store, `[
		{"name":"m","timestamp":2,"value":1},
		{"name":"m","timestamp":1,"value":9007199254740994}
	]`)
	assertConflictBits(t, lerr, 2, "m", 1, stored2Pow53, stored2Pow53Plus2)
	assertSinglePointBits(t, store, "m", 1, stored2Pow53)
	assertQueryBits(t, store, "m", 1, stored2Pow53)

	// 后续合法输入继续处理：改回等值写法（即使文本不同）成功忽略，
	// 新时间戳的点作为另一批正常新增。
	res := mustOK(t, store, sampleLine("m", 1, lit2Pow53Plus1))
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("equivalent resubmit = added %d duplicates %d, want 0/1", res.Added, res.Duplicates)
	}
	res = mustOK(t, store, sampleLine("m", 2, "1"))
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("recovery batch = added %d duplicates %d, want 1/0", res.Added, res.Duplicates)
	}
	// 查询两个点都在；精确平均 (2^53+1)/2 = 4503599627370496.5 恰为两相邻
	// 可表示值的中点，取偶为 4503599627370496（舍入规则本身由 query_rounding
	// 测试保障，这里只确认重复与失败没有改动参与计算的数据）。
	q := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if len(q.Series) != 1 || q.Series[0].Count != 2 ||
		q.Series[0].Average != 4503599627370496 {
		t.Fatalf("query after recovery = %+v, want count 2 average 4503599627370496", q.Series)
	}
}

// 成功忽略的重复既不增加查询的 count，也不改变 average；写入计数只统计本批；
// 成功快照仍展示全部已提交数据（含此前批次写入的其他点）。
func TestValueDuplicateDoesNotAffectQueryAndCountsArePerBatch(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":9007199254740992},
		{"name":"m","timestamp":2,"value":9007199254740994}
	]`)

	// 两个位置都以等值的另一种写法重提：added 0、duplicates 2。
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":9007199254740993},
		{"name":"m","timestamp":2,"value":9.007199254740994e15}
	]`)
	if res.Added != 0 || res.Duplicates != 2 {
		t.Fatalf("duplicate batch = added %d duplicates %d, want 0/2", res.Added, res.Duplicates)
	}
	// 快照仍展示全部已提交数据：两个点都在，值保持原存储值。
	pts := res.Series[0].Points
	if len(res.Series) != 1 || len(pts) != 2 ||
		math.Float64bits(pts[0].Value) != math.Float64bits(stored2Pow53) ||
		math.Float64bits(pts[1].Value) != math.Float64bits(stored2Pow53Plus2) {
		t.Fatalf("snapshot after duplicates = %+v, want the two original points", res.Series)
	}

	// 查询：count 仍为 2；精确平均是 9007199254740993（两存储值的中点），
	// 不可表示，两侧等距时取偶回到 2^53——被忽略的重复没有改变参与计算的数据。
	q := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	s0 := q.Series[0]
	if s0.Count != 2 || math.Float64bits(s0.Average) != math.Float64bits(stored2Pow53) {
		t.Fatalf("query after duplicates = count %d average %v (%016x), want count 2 average %v (%016x)",
			s0.Count, s0.Average, math.Float64bits(s0.Average),
			stored2Pow53, math.Float64bits(stored2Pow53))
	}

	// 下一批的计数不受上一批 duplicates 影响。
	res = mustOK(t, store, sampleLine("m", 3, "1"))
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("next batch = added %d duplicates %d, want 1/0 (per-batch counts)",
			res.Added, res.Duplicates)
	}
}
