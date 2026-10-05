package darksafe

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// 本文件为指标写入的零值去重补上回归保障。重复与冲突一律以转换后的有限
// float64 存储值判定，零值边界遵循同一条规则：
//
//   - 0、-0、0.0、-0.0 转换后是同一数值（仅正负号位不同），在同一序列、
//     同一时间戳上互为重复：成功忽略、计入本批 duplicates，不新增点。
//   - 1e-400 与 -1e-400 因幅度小于最小次正规值，舍入后分别成为正零与负零，
//     与上述零值相遇同样算重复；原始文本非零或符号不同都不能导致冲突。
//   - 被忽略的重复不能覆盖首次接受的存储值：快照里的零保留首次写入时的
//     正负号，先正后负、先负后正都遵循这一规则。
//   - 5e-324 是仍可区分的最小非零次正规值：与已存零值相遇必须冲突，
//     整批拒绝，conflict 给出冲突位置的序列、时间戳、零值与该极小值。
//
// 查询按已成功提交的点计数：被忽略的重复不增加 count；精确均值为零时
// average 输出正零（即使存储的是负零）；零值出现在不同时间戳就是不同采样点。

// zeroSpellings 是值为零的四种十进制写法；前两种为整数字面量风格，
// 后两种带小数点。
var zeroSpellings = []string{"0", "-0", "0.0", "-0.0"}

// sampleLineZero 构造一条只有一个采样点、值按原文字面量书写的写入行。
func sampleLineZero(ts int, spelling string) string {
	return `[{"name":"m","timestamp":` + strconv.Itoa(ts) + `,"value":` + spelling + `}]`
}

// TestZeroDedupSignedZeroSpellingsCrossBatch 覆盖跨批次提交：
// 对每一种零值写法先行写入后，再以其余写法逐个跨批提交，全部成功忽略，
// added 为 0、duplicates 按本批被忽略的提交次数计（每批一次即 1）；
// 快照始终只有首次写入的那个点，正负号保留首次写法；查询 count 不变、
// average 为正零。
func TestZeroDedupSignedZeroSpellingsCrossBatch(t *testing.T) {
	for _, first := range zeroSpellings {
		store := NewMetricStore()
		res := mustOK(t, store, sampleLineZero(1000, first))
		if res.Added != 1 || res.Duplicates != 0 {
			t.Fatalf("initial %s counts = %d/%d, want 1/0", first, res.Added, res.Duplicates)
		}
		wantNeg := math.Signbit(parseZeroForTest(t, first))

		// 逐个跨批提交其余写法：每次都是本批的一次重复。
		for _, other := range zeroSpellings {
			if other == first {
				continue
			}
			res = mustOK(t, store, sampleLineZero(1000, other))
			if res.Added != 0 || res.Duplicates != 1 {
				t.Fatalf("after initial %s, submitting %s counts = %d/%d, want 0/1",
					first, other, res.Added, res.Duplicates)
			}
			if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
				t.Fatalf("after initial %s, submitting %s snapshot = %+v, want still one point",
					first, other, res.Series)
			}
			p := res.Series[0].Points[0]
			if p.Timestamp != 1000 || p.Value != 0 || math.Signbit(p.Value) != wantNeg {
				t.Fatalf("after initial %s, submitting %s stored point = %+v, want ts=1000 zero with first-written sign (neg=%v)",
					first, other, p, wantNeg)
			}
		}

		// 被忽略的重复不增加 count；单点负零的精确均值也是正零。
		q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2000}`)
		if len(q.Series) != 1 || q.Series[0].Count != 1 ||
			q.Series[0].Average != 0 || math.Signbit(q.Series[0].Average) {
			t.Fatalf("after initial %s, query = %+v, want count=1 average=+0", first, q.Series)
		}
	}
}

// parseZeroForTest 按写入路径相同的规则把字面量解析成 float64，
// 仅用于得到首次写法的期望正负号。
func parseZeroForTest(t *testing.T, spelling string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(spelling, 64)
	if err != nil {
		t.Fatalf("test literal %s must parse: %v", spelling, err)
	}
	return v
}

// TestZeroDedupSignedZeroSpellingsWithinBatch 覆盖同一数组内：
// 此前不存在的位置在本批先以一种零值写法出现，随后再以其余写法以及
// 1e-400、-1e-400（舍入后为正零、负零）提交时，只新增一个点，其余全部
// 计为重复；存储点的正负号取本批首次出现的写法。先正后负与先负后正
// 两种顺序都覆盖。
func TestZeroDedupSignedZeroSpellingsWithinBatch(t *testing.T) {
	for _, first := range zeroSpellings {
		store := NewMetricStore()
		others := []string{"1e-400", "-1e-400"}
		for _, other := range zeroSpellings {
			if other != first {
				others = append(others, other)
			}
		}
		parts := []string{`{"name":"m","timestamp":1000,"value":` + first + `}`}
		for _, other := range others {
			parts = append(parts, `{"name":"m","timestamp":1000,"value":`+other+`}`)
		}
		line := `[` + strings.Join(parts, ",") + `]`

		res := mustOK(t, store, line)
		if res.Added != 1 || res.Duplicates != len(others) {
			t.Fatalf("first %s within-batch counts = %d/%d, want 1/%d",
				first, res.Added, res.Duplicates, len(others))
		}
		if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
			t.Fatalf("first %s snapshot = %+v, want exactly one point", first, res.Series)
		}
		p := res.Series[0].Points[0]
		wantNeg := strings.HasPrefix(first, "-")
		if p.Timestamp != 1000 || p.Value != 0 || math.Signbit(p.Value) != wantNeg {
			t.Fatalf("first %s stored point = %+v, want zero with first-written sign (neg=%v)",
				first, p, wantNeg)
		}
	}
}

// TestZeroDedupRoundedToZeroInputs 覆盖写入时舍入成零的输入：
// 1e-400 舍入为正零、-1e-400 舍入为负零，在同一位置与已存零值相遇时按
// 重复忽略；原始文本非零或符号不同都不构成冲突。首次接受的存储值不被
// 覆盖（正零不被后写的负零改号，反之亦然）。
func TestZeroDedupRoundedToZeroInputs(t *testing.T) {
	// 已存正零：舍入为正零与负零的两种非零文本提交都被忽略，存储保持正零。
	store := NewMetricStore()
	mustOK(t, store, sampleLineZero(1000, "0"))
	for _, spelling := range []string{"1e-400", "-1e-400"} {
		res := mustOK(t, store, sampleLineZero(1000, spelling))
		if res.Added != 0 || res.Duplicates != 1 {
			t.Fatalf("%s against stored +0 counts = %d/%d, want 0/1",
				spelling, res.Added, res.Duplicates)
		}
		p := res.Series[0].Points[0]
		if p.Value != 0 || math.Signbit(p.Value) {
			t.Fatalf("%s must not overwrite stored +0, got point %+v", spelling, p)
		}
	}

	// 相反顺序：先存由 -1e-400 舍入成的负零，再提交正零文本与舍入成正零的
	// 1e-400，同样全部忽略，存储保持负零。
	store2 := NewMetricStore()
	mustOK(t, store2, sampleLineZero(1000, "-1e-400"))
	for _, spelling := range []string{"0", "0.0", "1e-400"} {
		res := mustOK(t, store2, sampleLineZero(1000, spelling))
		if res.Added != 0 || res.Duplicates != 1 {
			t.Fatalf("%s against stored -0 counts = %d/%d, want 0/1",
				spelling, res.Added, res.Duplicates)
		}
		p := res.Series[0].Points[0]
		if p.Value != 0 || !math.Signbit(p.Value) {
			t.Fatalf("%s must not overwrite stored -0, got point %+v", spelling, p)
		}
	}

	// 同一批内、此前没有该位置：1e-400 先出现则存正零，-1e-400 与 -0 是重复；
	// 调换先后则存负零。
	store3 := NewMetricStore()
	res := mustOK(t, store3, `[
		{"name":"m","timestamp":1,"value":1e-400},
		{"name":"m","timestamp":1,"value":-1e-400},
		{"name":"m","timestamp":1,"value":-0}
	]`)
	if res.Added != 1 || res.Duplicates != 2 {
		t.Fatalf("rounded-to-zero within-batch counts = %d/%d, want 1/2", res.Added, res.Duplicates)
	}
	if p := res.Series[0].Points[0]; p.Value != 0 || math.Signbit(p.Value) {
		t.Fatalf("first 1e-400 must be stored as +0, got %+v", p)
	}

	store4 := NewMetricStore()
	res = mustOK(t, store4, `[
		{"name":"m","timestamp":1,"value":-1e-400},
		{"name":"m","timestamp":1,"value":1e-400}
	]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("reversed within-batch counts = %d/%d, want 1/1", res.Added, res.Duplicates)
	}
	if p := res.Series[0].Points[0]; p.Value != 0 || !math.Signbit(p.Value) {
		t.Fatalf("first -1e-400 must be stored as -0, got %+v", p)
	}
}

// TestZeroDedupSubnormalConflictsWithZero 确认 5e-324 仍是可区分的非零值：
// 已存零（正零或负零）后向同一位置提交它必须冲突，整批拒绝——本批前面的
// 合法新增点不提交，冲突信息给出序列、时间戳、判定冲突时的零值（保留已存
// 符号）与提交的极小值，原有的零也不能被替换。反向提交（先存 5e-324 再
// 提交零）同样冲突。
func TestZeroDedupSubnormalConflictsWithZero(t *testing.T) {
	for _, zero := range []struct {
		spelling string
		neg      bool
	}{{"0", false}, {"-0.0", true}} {
		store := NewMetricStore()
		mustOK(t, store, sampleLineZero(1000, zero.spelling))

		// 第一个点是合法新增（ts=2000），第二个点才是冲突：整批拒绝。
		lerr := mustFail(t, store, `[
			{"name":"m","timestamp":2000,"value":1},
			{"name":"m","timestamp":1000,"value":5e-324}
		]`)
		if lerr.Index != 2 || lerr.Conflict == nil {
			t.Fatalf("stored %s: expected conflict at index 2, got %+v", zero.spelling, lerr)
		}
		c := lerr.Conflict
		if c.Series.Name != "m" || len(c.Series.Labels) != 0 || c.Timestamp != 1000 {
			t.Fatalf("stored %s: conflict identity = %+v, want m{} ts=1000", zero.spelling, c)
		}
		if c.Existing != 0 || math.Signbit(c.Existing) != zero.neg {
			t.Fatalf("stored %s: existing = %v (neg=%v), want the stored zero with neg=%v",
				zero.spelling, c.Existing, math.Signbit(c.Existing), zero.neg)
		}
		if c.Submitted != 5e-324 {
			t.Fatalf("stored %s: submitted = %v, want 5e-324", zero.spelling, c.Submitted)
		}
		wantZeroText := "0"
		if zero.neg {
			wantZeroText = "-0"
		}
		if !strings.Contains(lerr.Error, "already has value "+wantZeroText) ||
			!strings.Contains(lerr.Error, "submitted 5e-324") {
			t.Fatalf("stored %s: error must name %s and 5e-324, got %q",
				zero.spelling, wantZeroText, lerr.Error)
		}

		// 整批回滚：ts=2000 的新增点不留下，ts=1000 的零点（含符号）不被替换。
		q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":3000}`)
		if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != 0 {
			t.Fatalf("stored %s: query after conflict = %+v, want only the original zero point",
				zero.spelling, q.Series)
		}
		res := mustOK(t, store, `[]`)
		p := res.Series[0].Points[0]
		if p.Value != 0 || math.Signbit(p.Value) != zero.neg {
			t.Fatalf("stored %s: original zero sign changed after conflict, got %+v",
				zero.spelling, p)
		}
	}

	// 反向：先存 5e-324，再提交零也必须冲突，existing 是极小值、submitted 是零。
	store := NewMetricStore()
	mustOK(t, store, sampleLineZero(1, "5e-324"))
	lerr := mustFail(t, store, sampleLineZero(1, "0"))
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("reverse direction expected conflict at index 1, got %+v", lerr)
	}
	if lerr.Conflict.Existing != 5e-324 || lerr.Conflict.Submitted != 0 ||
		math.Signbit(lerr.Conflict.Submitted) {
		t.Fatalf("reverse conflict = %+v, want existing=5e-324 submitted=+0", lerr.Conflict)
	}
}

// TestZeroDedupDuplicateDoesNotRewriteConflictExisting 回归保障：同一批内先遇到
// 等值重复、随后在同一位置发生冲突时，冲突报告的 existing 与文字原因必须如实
// 对应首次接受的已存值，不能被本批被忽略的重复采样改写。
//
//   - 已存正零时，本批先提交负零（重复、忽略）再提交 5e-324（冲突）：
//     existing 是正零，文字原因显示 "already has value 0" 而不是 -0。
//   - 已存负零时，本批先提交正零再提交非零值：existing 与文字原因都保留负零符号。
//   - 该位置此前未写入时，existing 取本批首次接受的值（含零值符号），
//     后续等值重复（无论重复几次、用哪种零写法）都不替换它。
//   - 发生冲突后整批（含前面的合法新增点）都不提交，已存原值保持原样。
func TestZeroDedupDuplicateDoesNotRewriteConflictExisting(t *testing.T) {
	// 已存正零：批内先重复 -0 再冲突 5e-324，existing 仍是正零。
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","labels":{"host":"a"},"timestamp":1000,"value":0}]`)
	lerr := mustFail(t, store, `[
		{"name":"cpu","labels":{"host":"a"},"timestamp":2000,"value":7},
		{"name":"cpu","labels":{"host":"a"},"timestamp":1000,"value":-0},
		{"name":"cpu","labels":{"host":"a"},"timestamp":1000,"value":5e-324}
	]`)
	assertConflictDetail(t, lerr, 3,
		`conflict: series cpu{host=a} at timestamp 1000 already has value 0, submitted 5e-324`,
		"cpu", map[string]string{"host": "a"}, 1000, 0, 5e-324)
	if math.Signbit(lerr.Conflict.Existing) {
		t.Fatalf("existing = %v, want +0 (stored sign), not the ignored duplicate -0",
			lerr.Conflict.Existing)
	}
	// 整批回滚：ts=2000 的新增点不留下，只剩原来的正零点。
	q := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":3000}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != 0 {
		t.Fatalf("query after conflict = %+v, want only the original zero point", q.Series)
	}

	// 已存负零：批内先重复 +0 再冲突，existing 与文字原因保留负号。
	store = NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","labels":{"host":"a"},"timestamp":1000,"value":-0.0}]`)
	lerr = mustFail(t, store, `[
		{"name":"cpu","labels":{"host":"a"},"timestamp":1000,"value":0},
		{"name":"cpu","labels":{"host":"a"},"timestamp":1000,"value":5e-324}
	]`)
	assertConflictDetail(t, lerr, 2,
		`conflict: series cpu{host=a} at timestamp 1000 already has value -0, submitted 5e-324`,
		"cpu", map[string]string{"host": "a"}, 1000, 0, 5e-324)
	if !math.Signbit(lerr.Conflict.Existing) {
		t.Fatalf("existing = %v, want -0 (stored sign)", lerr.Conflict.Existing)
	}

	// 位置此前未写入：本批首次接受 -0，随后各种零写法重复多次，
	// existing 仍取首次接受的 -0；冲突后该位置（连同整批）不留下任何点。
	store = NewMetricStore()
	lerr = mustFail(t, store, `[
		{"name":"m","timestamp":1000,"value":-0},
		{"name":"m","timestamp":1000,"value":0},
		{"name":"m","timestamp":1000,"value":0.0},
		{"name":"m","timestamp":1000,"value":1e-400},
		{"name":"m","timestamp":1000,"value":-1e-400},
		{"name":"m","timestamp":1000,"value":5e-324}
	]`)
	assertConflictDetail(t, lerr, 6,
		`conflict: series m{} at timestamp 1000 already has value -0, submitted 5e-324`,
		"m", map[string]string{}, 1000, 0, 5e-324)
	if !math.Signbit(lerr.Conflict.Existing) {
		t.Fatalf("existing = %v, want -0 (first accepted in batch)", lerr.Conflict.Existing)
	}
	q = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":3000}`)
	if len(q.Series) != 0 {
		t.Fatalf("query after conflict = %+v, want no committed points", q.Series)
	}
}

// TestZeroDedupDistinctTimestampsAndZeroAverages 覆盖写入后的区间查询：
// 同一位置被忽略的重复不增加 count；零值出现在不同时间戳时是不同采样点，
// 每个已成功提交的点都参与计数；精确均值为零（含正负零混合、+1/-1 抵消）
// 时 average 一律输出正零。
func TestZeroDedupDistinctTimestampsAndZeroAverages(t *testing.T) {
	store := NewMetricStore()
	// 三个不同时间戳各写一个零（写法、符号各不相同）：都是新增点。
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":0},
		{"name":"m","timestamp":2,"value":-0.0},
		{"name":"m","timestamp":3,"value":1e-400}
	]`)
	if res.Added != 3 || res.Duplicates != 0 || len(res.Series[0].Points) != 3 {
		t.Fatalf("zeros at distinct timestamps = %+v, want 3 added points", res)
	}
	// 同一位置的重复（含舍入成相反符号零的文本）被忽略，不增加 count。
	res = mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":-1e-400},
		{"name":"m","timestamp":2,"value":0.0}
	]`)
	if res.Added != 0 || res.Duplicates != 2 {
		t.Fatalf("duplicate zeros counts = %d/%d, want 0/2", res.Added, res.Duplicates)
	}

	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	if len(q.Series) != 1 || q.Series[0].Count != 3 {
		t.Fatalf("zero query = %+v, want count=3", q.Series)
	}
	if q.Series[0].Average != 0 || math.Signbit(q.Series[0].Average) {
		t.Fatalf("zero query average = %v, want +0", q.Series[0].Average)
	}

	// 非零数据精确抵消成零时，average 同样是正零而不是负零。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"n","timestamp":1,"value":1},
		{"name":"n","timestamp":2,"value":-1}
	]`)
	q2 := mustQuery(t, store2, `{"op":"query","name":"n","start":0,"end":10}`)
	if len(q2.Series) != 1 || q2.Series[0].Count != 2 ||
		q2.Series[0].Average != 0 || math.Signbit(q2.Series[0].Average) {
		t.Fatalf("canceling average = %+v, want count=2 average=+0", q2.Series)
	}
}
