package darksafe

import (
	"strings"
	"testing"
)

// 本文件回归保障数值去重与冲突判定以实际存储的有限 float64 值为准：
// 同一序列、同一时间戳再次提交时，比较的是 JSON 数字转换后的 float64，
// 既不是原始文本，也不是“数值接近就算相等”。重点覆盖两类边界：
//
//   - 不同的十进制写法转换成同一个存储值（如 9007199254740992 与
//     9007199254740993、5e-324 与 4.9406564584124654e-324）：视为重复，
//     成功忽略并计入 duplicates，不新增点、不改变查询统计。
//   - 相邻但仍可区分的可表示值（如 9007199254740992 与 9007199254740994、
//     5e-324 与 1e-323）：必须视为冲突，整批拒绝，conflict 中的
//     existing/submitted 都是转换后的实际存储值。
//
// 规则在同一数组内与跨成功批次提交时一致；正数、负数与接近零的次正规值
// 遵循同一条规则。

// TestFloat64DedupEquivalentSpellingsCrossBatch 覆盖跨批次提交：
// 同一位置先写入 2^53，随后以转换后相等的另一种写法（9007199254740993、
// 9007199254740992.5、9.007199254740992e15）再提交，全部成功忽略；
// added 为 0、duplicates 按被忽略的提交次数计，快照仍只有原来的一个点，
// 查询的 count 与 average 均不受影响。
func TestFloat64DedupEquivalentSpellingsCrossBatch(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[{"name":"m","timestamp":1000,"value":9007199254740992}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("initial write counts = %d/%d, want 1/0", res.Added, res.Duplicates)
	}

	// 9007199254740993 超出 2^53 的精确表示范围，转换后与 9007199254740992 相同。
	res = mustOK(t, store, `[{"name":"m","timestamp":1000,"value":9007199254740993}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("9007199254740993 counts = %d/%d, want 0/1", res.Added, res.Duplicates)
	}
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("snapshot after duplicate = %+v, want still one point", res.Series)
	}
	if p := res.Series[0].Points[0]; p.Timestamp != 1000 || p.Value != 9007199254740992.0 {
		t.Fatalf("stored point = %+v, want ts=1000 value=9007199254740992", p)
	}

	// 9007199254740992.5 按最近偶数舍入到 2^53；9.007199254740992e15 是同一值的
	// 科学计数写法。两种写法各自提交一次，duplicates 只统计本批。
	res = mustOK(t, store, `[
		{"name":"m","timestamp":1000,"value":9007199254740992.5},
		{"name":"m","timestamp":1000,"value":9.007199254740992e15}
	]`)
	if res.Added != 0 || res.Duplicates != 2 {
		t.Fatalf("two equivalent spellings counts = %d/%d, want 0/2", res.Added, res.Duplicates)
	}

	// 被忽略的重复不增加 count、不改变 average。
	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2000}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != 9007199254740992.0 {
		t.Fatalf("query after duplicates = %+v, want count=1 average=9007199254740992", q.Series)
	}
}

// TestFloat64DedupAdjacentValuesConflict 覆盖相邻可表示值：
// 2^53 处相邻的可表示值间隔为 2，9007199254740994 必须与 9007199254740992 区分；
// 提交的原始文本本身不可表示（9007199254740993.5）时，conflict 中的 submitted
// 也是转换后的存储值 9007199254740994，而不是原始文本或“接近即等”的合并结果。
func TestFloat64DedupAdjacentValuesConflict(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1000,"value":9007199254740992}]`)

	lerr := mustFail(t, store, `[{"name":"m","timestamp":1000,"value":9007199254740994}]`)
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("expected conflict at index 1, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Timestamp != 1000 || c.Existing != 9007199254740992.0 || c.Submitted != 9007199254740994.0 {
		t.Fatalf("conflict detail = %+v, want existing=9007199254740992 submitted=9007199254740994", c)
	}
	if !strings.Contains(lerr.Error, "9.007199254740992e+15") ||
		!strings.Contains(lerr.Error, "9.007199254740994e+15") {
		t.Fatalf("error reason must name the stored values, got %q", lerr.Error)
	}

	// 原始文本 9007199254740993.5 不可表示，舍入后为 9007199254740994：
	// 与已存的 9007199254740992 冲突，submitted 是转换后的存储值。
	lerr = mustFail(t, store, `[{"name":"m","timestamp":1000,"value":9007199254740993.5}]`)
	if lerr.Conflict == nil || lerr.Conflict.Submitted != 9007199254740994.0 {
		t.Fatalf("submitted must be the stored float64 9007199254740994, got %+v", lerr.Conflict)
	}

	// 1 与比它大的下一个可表示值也必须区分，不能因为接近而合并。
	store2 := NewMetricStore()
	mustOK(t, store2, `[{"name":"m","timestamp":1,"value":1}]`)
	lerr = mustFail(t, store2, `[{"name":"m","timestamp":1,"value":1.0000000000000002}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 1.0000000000000002 {
		t.Fatalf("adjacent values near 1 must conflict, got %+v", lerr.Conflict)
	}

	// 冲突整批拒绝：此前写入的原值不变。
	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2000}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != 9007199254740992.0 {
		t.Fatalf("query after rejected conflicts = %+v, want original value intact", q.Series)
	}
}

// TestFloat64DedupNegativeValues 确认负数遵循同一规则：
// -9007199254740993 转换后与 -9007199254740992 相同（重复），
// -9007199254740994 是相邻可表示值（冲突）。
func TestFloat64DedupNegativeValues(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":-9007199254740992}]`)

	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":-9007199254740993}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("-9007199254740993 counts = %d/%d, want 0/1", res.Added, res.Duplicates)
	}

	lerr := mustFail(t, store, `[{"name":"m","timestamp":1,"value":-9007199254740994}]`)
	if lerr.Conflict == nil ||
		lerr.Conflict.Existing != -9007199254740992.0 || lerr.Conflict.Submitted != -9007199254740994.0 {
		t.Fatalf("negative conflict detail = %+v", lerr.Conflict)
	}

	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != -9007199254740992.0 {
		t.Fatalf("query = %+v, want count=1 average=-9007199254740992", q.Series)
	}
}

// TestFloat64DedupSubnormalValues 确认接近零的有限数值也按存储值判定：
// 5e-324 与 4.9406564584124654e-324 是最小正次正规值的两种写法（重复），
// 1e-323 转换为下一个次正规值（冲突），不能因为数值很小就合并。
func TestFloat64DedupSubnormalValues(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":5e-324}]`)

	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":4.9406564584124654e-324}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("4.9406564584124654e-324 counts = %d/%d, want 0/1", res.Added, res.Duplicates)
	}

	lerr := mustFail(t, store, `[{"name":"m","timestamp":1,"value":1e-323}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5e-324 || lerr.Conflict.Submitted != 1e-323 {
		t.Fatalf("subnormal conflict detail = %+v, want existing=5e-324 submitted=1e-323", lerr.Conflict)
	}

	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != 5e-324 {
		t.Fatalf("query = %+v, want count=1 average=5e-324", q.Series)
	}
}

// TestFloat64DedupEquivalentSpellingsWithinBatch 覆盖同一数组内：
// 此前不存在的位置在本批先出现、随后以等值的另一种写法再出现时，
// 只新增一个点，duplicates 按本批被忽略的提交次数计算。
func TestFloat64DedupEquivalentSpellingsWithinBatch(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":9007199254740992},
		{"name":"m","timestamp":1,"value":9007199254740993},
		{"name":"m","timestamp":1,"value":9.007199254740992e15}
	]`)
	if res.Added != 1 || res.Duplicates != 2 {
		t.Fatalf("counts = %d/%d, want 1/2", res.Added, res.Duplicates)
	}
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 ||
		res.Series[0].Points[0].Value != 9007199254740992.0 {
		t.Fatalf("snapshot = %+v, want a single stored point 9007199254740992", res.Series)
	}

	// 次正规值在同一数组内同样只新增一个点。
	store2 := NewMetricStore()
	res = mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":5e-324},
		{"name":"m","timestamp":1,"value":4.9406564584124654e-324}
	]`)
	if res.Added != 1 || res.Duplicates != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("subnormal within-batch = %+v, want added=1 duplicates=1 one point", res)
	}
}

// TestFloat64DedupWithinBatchConflictUsesEarlierBatchValue 覆盖同一数组内的冲突：
// 位置此前不存在，本批先出现一个值、随后出现转换后不同的值时整批拒绝，
// conflict 中的 existing 是本批较早的值，两个点都不保留。
func TestFloat64DedupWithinBatchConflictUsesEarlierBatchValue(t *testing.T) {
	store := NewMetricStore()
	lerr := mustFail(t, store, `[
		{"name":"m","timestamp":1,"value":9007199254740992},
		{"name":"m","timestamp":1,"value":9007199254740994}
	]`)
	if lerr.Index != 2 || lerr.Conflict == nil {
		t.Fatalf("expected conflict at index 2, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Existing != 9007199254740992.0 || c.Submitted != 9007199254740994.0 {
		t.Fatalf("existing must be the earlier in-batch value, got %+v", c)
	}
	// 两个点都不保留：存储仍为空。
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("rejected batch must leave no points, series count = %d", got)
	}

	// 次正规值版本：5e-324 与 1e-323 是相邻可表示值，本批冲突同样整批拒绝。
	lerr = mustFail(t, store, `[
		{"name":"m","timestamp":2,"value":5e-324},
		{"name":"m","timestamp":2,"value":1e-323}
	]`)
	if lerr.Index != 2 || lerr.Conflict == nil ||
		lerr.Conflict.Existing != 5e-324 || lerr.Conflict.Submitted != 1e-323 {
		t.Fatalf("subnormal within-batch conflict = %+v", lerr)
	}
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("rejected subnormal batch must leave no points, series count = %d", got)
	}
}

// TestFloat64DedupConflictSkipsEarlierAddsInBatch 覆盖位置已存在时的冲突：
// 冲突保留此前的值，本批排在冲突点前面的合法新增点也不提交；
// 失败后的查询仍看到原值，后续合法输入继续处理。
func TestFloat64DedupConflictSkipsEarlierAddsInBatch(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1000,"value":9007199254740992}]`)

	// 第一个点是合法新增（等值写法 9007199254740993 与已存值相同，是重复），
	// 第二个点才是相邻可表示值的冲突：整批拒绝，index 指向冲突点。
	lerr := mustFail(t, store, `[
		{"name":"m","timestamp":2000,"value":2},
		{"name":"m","timestamp":1000,"value":9007199254740993},
		{"name":"m","timestamp":1000,"value":9007199254740994}
	]`)
	if lerr.Index != 3 || lerr.Conflict == nil {
		t.Fatalf("expected conflict at index 3, got %+v", lerr)
	}
	if lerr.Conflict.Existing != 9007199254740992.0 || lerr.Conflict.Submitted != 9007199254740994.0 {
		t.Fatalf("conflict must keep the previously stored value, got %+v", lerr.Conflict)
	}

	// 排在冲突点前面的合法新增点（ts=2000）也没有提交。
	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":3000}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != 9007199254740992.0 {
		t.Fatalf("query after failed batch = %+v, want only the original point", q.Series)
	}

	// 后续合法输入继续处理：ts=2000 现在可以写入。
	res := mustOK(t, store, `[{"name":"m","timestamp":2000,"value":2}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("counts after recovery = %d/%d, want 1/0", res.Added, res.Duplicates)
	}
	// 成功快照展示全部已提交数据（含此前批次写入的旧点）。
	if len(res.Series) != 1 || len(res.Series[0].Points) != 2 {
		t.Fatalf("snapshot = %+v, want both committed points", res.Series)
	}
	q = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":3000}`)
	// (9007199254740992 + 2) / 2 = 4503599627370497，精确可表示。
	if len(q.Series) != 1 || q.Series[0].Count != 2 || q.Series[0].Average != 4503599627370497.0 {
		t.Fatalf("query after recovery = %+v, want count=2 average=4503599627370497", q.Series)
	}
}
