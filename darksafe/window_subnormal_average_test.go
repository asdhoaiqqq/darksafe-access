package darksafe

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

// 本文件回归保障固定时间窗口统计（query_windows）在极小数均值上的行为：
// 依据是窗口内已保存的有限 float64 值，先求精确算术平均，再按最近偶数规则
// 舍入为可表示结果——不能把参与计算的微小非零值提前当成零，也不能按十进制
// 小数位截断。5e-324 是最小的正 float64（2^-1074），它与 0 的精确平均
// 2^-1075 恰好落在 0 与 5e-324 正中，是本文件的核心中点。
//
// 重点保护的区别：
//   - 5e-324 与 0 的两点窗口：精确平均居中，取偶为正零，JSON 中 average 为 0；
//     换成 -5e-324 则舍入为负零，JSON 中保留 -0。正零与负零数值相等，但符号
//     必须忠实保留，让用户能区分“负的微小均值被舍入为零”与真正的抵消。
//   - 5e-324 与 -5e-324 同窗：精确平均本来就是零，输出正零——与负零情形区分。
//   - 中点两侧：两个 5e-324 加一个 0 均值为 5e-324；一个 5e-324 加两个 0
//     均值为正零；负数情形遵循对应的符号规则。
//   - 同值采样在不同时间戳分别计数；同一位置的等值重复按既有写入规则忽略，
//     不能改变窗口点数或把均值推到中点另一侧。
//   - 相邻窗口各自只反映自己的点，区间外采样不参与；均值为零的窗口仍是有
//     数据的成功结果（保留真实点数，不省略），真正无点的窗口继续不出现。
//
// 注意：Window 结构体的 == 比较无法区分 +0 与 -0（float64 比较中二者相等），
// 因此本文件的均值断言一律用 math.Float64bits 按位比较。

// smallestSubnormal 是最小正 float64（0x0000000000000001）的 JSON 字面量。
const smallestSubnormal = "5e-324"

// subnormalBits 是各期望均值在测试中用到的位模式。
const (
	posZeroBits          = uint64(0x0000000000000000)
	negZeroBits          = uint64(0x8000000000000000)
	posSmallestSubnormal = uint64(0x0000000000000001)
	negSmallestSubnormal = uint64(0x8000000000000001)
)

// ingestWindowSamples 在同一序列 m 上写入一批采样：values 按时间戳
// base, base+1, ... 依次落点，值为 JSON 数值字面量。
func ingestWindowSamples(t *testing.T, store *MetricStore, base int64, values ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":"m","timestamp":`)
		b.WriteString(strconv.FormatInt(base+int64(i), 10))
		b.WriteString(`,"value":` + v + `}`)
	}
	b.WriteByte(']')
	mustOK(t, store, b.String())
}

// assertWindowAverageBits 断言窗口点数与均值位模式，并校验查询结果 JSON 中
// average 字段的数值表示：序列化文本必须包含 wantAvgJSON（区分 0 与 -0、
// 保留 5e-324 而不是冲刷成 0），且 JSON 往返后均值位模式不变。
func assertWindowAverageBits(t *testing.T, store *MetricStore, start, end, step int64,
	wantCount int, wantBits uint64, wantAvgJSON string, note string) Window {
	t.Helper()
	res := mustQueryWindows(t, store, queryWindowsAll("m", start, end, step))
	if len(res.Series) != 1 || len(res.Series[0].Windows) != 1 {
		t.Fatalf("%s: result = %+v, want one series with one window", note, res)
	}
	w := res.Series[0].Windows[0]
	if w.Count != wantCount {
		t.Fatalf("%s: count = %d, want %d", note, w.Count, wantCount)
	}
	if got := math.Float64bits(w.Average); got != wantBits {
		t.Fatalf("%s: average = %v (%016x), want %v (%016x)",
			note, w.Average, got, math.Float64frombits(wantBits), wantBits)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("%s: marshal query_windows result: %v", note, err)
	}
	if !strings.Contains(string(raw), wantAvgJSON) {
		t.Fatalf("%s: query_windows JSON = %s, want it to contain %s", note, raw, wantAvgJSON)
	}
	var round QueryWindowsResult
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("%s: unmarshal query_windows result: %v", note, err)
	}
	if got := math.Float64bits(round.Series[0].Windows[0].Average); got != wantBits {
		t.Fatalf("%s: average JSON %s round-trips to %016x, want %016x",
			note, raw, got, wantBits)
	}
	return w
}

// TestQueryWindowsSubnormalTieKeepsZeroSign 覆盖零附近的两点中点：
// 同一窗口在两个不同时间戳分别保存 5e-324 与 0，点数为 2，精确平均 2^-1075
// 居中于 0 与 5e-324 之间，取偶舍入为正零，JSON 中 average 为 0；把非零采样
// 换成 -5e-324，点数仍为 2，均值是负零，JSON 中保留 -0。均值为零的窗口仍是
// 有数据的成功结果，窗口必须出现且保留真实点数。
func TestQueryWindowsSubnormalTieKeepsZeroSign(t *testing.T) {
	cases := []struct {
		name        string
		value       string // 与 0 同窗的极小值字面量
		wantBits    uint64
		wantAvgJSON string
	}{
		{"smallest positive rounds to positive zero", smallestSubnormal, posZeroBits, `"average":0`},
		{"smallest negative rounds to negative zero", "-" + smallestSubnormal, negZeroBits, `"average":-0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			ingestWindowSamples(t, store, 1, tc.value, "0")
			w := assertWindowAverageBits(t, store, 0, 9, 10, 2, tc.wantBits, tc.wantAvgJSON, tc.name)
			if w.Start != 0 || w.End != 9 {
				t.Fatalf("%s: window bounds = [%d,%d], want [0,9]", tc.name, w.Start, w.End)
			}
			// 正零与负零数值相等但符号必须保留：结果不能落到另一种零上。
			if got := math.Float64bits(w.Average); got == (posZeroBits|negZeroBits)^tc.wantBits {
				t.Fatalf("%s: average landed on the opposite-signed zero", tc.name)
			}
		})
	}
}

// TestQueryWindowsSubnormalCancellationIsPositiveZero 同一窗口保存大小相等、
// 符号相反的非零采样（5e-324 与 -5e-324）：精确平均本来就是零，输出正零，
// JSON 中 average 为 0——与负的微小均值舍入成的负零明确区分。
func TestQueryWindowsSubnormalCancellationIsPositiveZero(t *testing.T) {
	store := NewMetricStore()
	ingestWindowSamples(t, store, 1, smallestSubnormal, "-"+smallestSubnormal)
	w := assertWindowAverageBits(t, store, 0, 9, 10, 2, posZeroBits, `"average":0`,
		"exact cancellation yields positive zero")
	if math.Signbit(w.Average) {
		t.Fatalf("cancelled average must be +0, got -0")
	}
	// 交换写入顺序结果不变：精确求和与点的提交顺序无关。
	store2 := NewMetricStore()
	ingestWindowSamples(t, store2, 1, "-"+smallestSubnormal, smallestSubnormal)
	assertWindowAverageBits(t, store2, 0, 9, 10, 2, posZeroBits, `"average":0`,
		"exact cancellation is order-independent")
}

// TestQueryWindowsSubnormalMidpointBothSides 保护中点两侧的三点组合：
// 两个 5e-324 加一个 0 的精确平均（2·2^-1074/3）高于中点，均值为 5e-324；
// 一个 5e-324 加两个 0 的精确平均（2^-1074/3）低于中点，均值为正零；
// 负数情形遵循对应的符号规则。参与计算的微小非零值不得被提前当成零。
func TestQueryWindowsSubnormalMidpointBothSides(t *testing.T) {
	cases := []struct {
		name        string
		values      []string
		wantBits    uint64
		wantAvgJSON string
	}{
		{"two positive one zero stays above midpoint",
			[]string{smallestSubnormal, smallestSubnormal, "0"}, posSmallestSubnormal, `"average":5e-324`},
		{"one positive two zeros rounds to positive zero",
			[]string{smallestSubnormal, "0", "0"}, posZeroBits, `"average":0`},
		{"two negative one zero stays below midpoint",
			[]string{"-" + smallestSubnormal, "-" + smallestSubnormal, "0"}, negSmallestSubnormal, `"average":-5e-324`},
		{"one negative two zeros rounds to negative zero",
			[]string{"-" + smallestSubnormal, "0", "0"}, negZeroBits, `"average":-0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			ingestWindowSamples(t, store, 1, tc.values...)
			assertWindowAverageBits(t, store, 0, 9, 10, 3, tc.wantBits, tc.wantAvgJSON, tc.name)
		})
	}
}

// TestQueryWindowsSubnormalDuplicateDoesNotShiftMidpoint 同一位置的等值重复
// 按既有写入规则忽略：不能改变窗口点数，也不能把均值推到中点另一侧；
// 同值采样出现在不同时间戳时则分别计数，按点数参与精确平均。
func TestQueryWindowsSubnormalDuplicateDoesNotShiftMidpoint(t *testing.T) {
	store := NewMetricStore()
	ingestWindowSamples(t, store, 1, smallestSubnormal, "0")

	// 同一位置（ts=1）重复提交 5e-324：被忽略，duplicates 计 1。
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":5e-324}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("duplicate counts = added %d duplicates %d, want 0/1", res.Added, res.Duplicates)
	}
	// 若被忽略的重复参与计数，精确平均会变成 2·2^-1074/3（高于中点），
	// 均值会被推到 5e-324；点数仍为 2、均值仍为正零才正确。
	assertWindowAverageBits(t, store, 0, 9, 10, 2, posZeroBits, `"average":0`,
		"ignored duplicate must not shift the midpoint")

	// 同值出现在不同时间戳（ts=3）是新的采样点：点数变为 3，
	// 精确平均 2·2^-1074/3 高于中点，均值为 5e-324。
	ingestWindowSamples(t, store, 3, smallestSubnormal)
	assertWindowAverageBits(t, store, 0, 9, 10, 3, posSmallestSubnormal, `"average":5e-324`,
		"same value on a distinct timestamp counts separately")
}

// TestQueryWindowsSubnormalAdjacentWindowsIsolated 同一序列的相邻窗口各自只
// 反映自己的点：窗口之间不混入点数或数值，区间外的采样不参与计算；窗口按
// 起点升序展示，真正没有采样的窗口不出现。
func TestQueryWindowsSubnormalAdjacentWindowsIsolated(t *testing.T) {
	store := NewMetricStore()
	// 窗口 [0,9]：5e-324 与 0 → 正零；窗口 [10,19]：-5e-324 与 0 → 负零；
	// 窗口 [20,29]：5e-324 与 -5e-324 → 精确抵消，正零。
	// 窗口 [30,39] 无点；ts=45 的 1e300 在查询区间 [0,39] 之外。
	ingestWindowSamples(t, store, 1, smallestSubnormal, "0")
	ingestWindowSamples(t, store, 11, "-"+smallestSubnormal, "0")
	ingestWindowSamples(t, store, 21, smallestSubnormal, "-"+smallestSubnormal)
	ingestWindowSamples(t, store, 45, "1e300")

	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 39, 10))
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", res.Series)
	}
	wins := res.Series[0].Windows
	if len(wins) != 3 {
		t.Fatalf("windows = %+v, want 3 (empty window [30,39] and out-of-range point excluded)", wins)
	}
	want := []struct {
		start, end int64
		count      int
		bits       uint64
	}{
		{0, 9, 2, posZeroBits},
		{10, 19, 2, negZeroBits},
		{20, 29, 2, posZeroBits},
	}
	for i, wn := range want {
		w := wins[i]
		if w.Start != wn.start || w.End != wn.end {
			t.Fatalf("window[%d] bounds = [%d,%d], want [%d,%d] (ascending by start)",
				i, w.Start, w.End, wn.start, wn.end)
		}
		if w.Count != wn.count {
			t.Fatalf("window[%d] count = %d, want %d (no cross-window mixing)", i, w.Count, wn.count)
		}
		if got := math.Float64bits(w.Average); got != wn.bits {
			t.Fatalf("window[%d] average = %v (%016x), want %016x",
				i, w.Average, got, wn.bits)
		}
	}
	// 区间外的 1e300 不得参与任何窗口：三个窗口的均值都在零附近。
	for i, w := range wins {
		if math.Abs(w.Average) > math.SmallestNonzeroFloat64 {
			t.Fatalf("window[%d] average = %v, out-of-range point leaked into statistics", i, w.Average)
		}
	}

	// 扩大区间到 [0,49]：ts=45 的点进入自己的窗口 [40,49]，不影响既有窗口。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 0, 49, 10))
	wins = res.Series[0].Windows
	if len(wins) != 4 {
		t.Fatalf("extended windows = %+v, want 4", wins)
	}
	if wins[3].Start != 40 || wins[3].End != 49 || wins[3].Count != 1 || wins[3].Average != 1e300 {
		t.Fatalf("window[3] = %+v, want [40,49] count 1 average 1e300", wins[3])
	}
	for i, wn := range want {
		if got := math.Float64bits(wins[i].Average); got != wn.bits || wins[i].Count != wn.count {
			t.Fatalf("window[%d] changed after widening range: %+v", i, wins[i])
		}
	}
}

// TestQueryWindowsSubnormalZeroAverageWindowNotOmitted 窗口均值为零仍是有数据
// 的成功结果：窗口出现且保留真实点数；真正没有采样的窗口继续不出现。
func TestQueryWindowsSubnormalZeroAverageWindowNotOmitted(t *testing.T) {
	store := NewMetricStore()
	// 窗口 [0,9]：5e-324 与 0，均值舍入为正零；窗口 [10,19] 完全无点；
	// 窗口 [20,29]：5e-324 与 -5e-324，精确抵消为正零。
	ingestWindowSamples(t, store, 1, smallestSubnormal, "0")
	ingestWindowSamples(t, store, 21, smallestSubnormal, "-"+smallestSubnormal)

	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 29, 10))
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want the zero-average series to appear", res.Series)
	}
	wins := res.Series[0].Windows
	if len(wins) != 2 {
		t.Fatalf("windows = %+v, want the two zero-average windows and no empty [10,19]", wins)
	}
	for i, wantStart := range []int64{0, 20} {
		w := wins[i]
		if w.Start != wantStart || w.End != wantStart+9 {
			t.Fatalf("window[%d] bounds = [%d,%d], want [%d,%d]", i, w.Start, w.End, wantStart, wantStart+9)
		}
		if w.Count != 2 {
			t.Fatalf("window[%d] count = %d, want 2 (zero average must keep the true count)", i, w.Count)
		}
		if math.Float64bits(w.Average) != posZeroBits {
			t.Fatalf("window[%d] average = %v (%016x), want +0", i, w.Average, math.Float64bits(w.Average))
		}
	}
	// JSON 中两个窗口都以 "average":0 出现，而不是被省略。
	raw := marshalCompact(t, res)
	if strings.Count(raw, `"average":0`) != 2 {
		t.Fatalf("query_windows JSON = %s, want two zero-average windows present", raw)
	}
}

// TestQueryWindowsSubnormalIdentityAndReadOnly 极小数均值场景下结果仍携带完整
// 序列身份（指标名与完整标签集合），标签子集查询条件不变；查询只读：重复查询
// 结果一致，存储中的采样点与后续写入行为不受影响。
func TestQueryWindowsSubnormalIdentityAndReadOnly(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":5e-324,"labels":{"host":"a","zone":"x"}},
		{"name":"m","timestamp":2,"value":0,"labels":{"host":"a","zone":"x"}},
		{"name":"m","timestamp":3,"value":-5e-324,"labels":{"host":"a","zone":"x"}}
	]`)

	line := `{"op":"query_windows","name":"m","start":0,"end":9,"step":10,"labels":{"host":"a"}}`
	r1 := mustQueryWindows(t, store, line)
	if len(r1.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", r1.Series)
	}
	s0 := r1.Series[0]
	if s0.Name != "m" || !sameLabels(s0.Labels, map[string]string{"host": "a", "zone": "x"}) {
		t.Fatalf("full identity not preserved: %+v", s0)
	}
	if len(s0.Windows) != 1 || s0.Windows[0].Count != 3 {
		t.Fatalf("windows = %+v, want one window with count 3", s0.Windows)
	}
	// 5e-324 + 0 + (-5e-324) 精确抵消：均值为正零。
	if math.Float64bits(s0.Windows[0].Average) != posZeroBits {
		t.Fatalf("average = %v (%016x), want +0", s0.Windows[0].Average,
			math.Float64bits(s0.Windows[0].Average))
	}

	// 只读：重复查询结果一致，存储未被改变。
	r2 := mustQueryWindows(t, store, line)
	if marshalCompact(t, r1) != marshalCompact(t, r2) {
		t.Fatalf("repeated query_windows differs: %s vs %s", marshalCompact(t, r1), marshalCompact(t, r2))
	}
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 3 {
		t.Fatalf("query_windows mutated storage, snapshot = %+v", snap.Series)
	}
	// 存储的负零采样保持原样（明细与快照不被均值规则改写）。
	if math.Float64bits(snap.Series[0].Points[2].Value) != negSmallestSubnormal {
		t.Fatalf("stored sample changed: %+v", snap.Series[0].Points)
	}

	// 后续写入可见：在相邻窗口补一个点，既有窗口统计不变。
	mustOK(t, store, `[{"name":"m","timestamp":15,"value":5e-324,"labels":{"host":"a","zone":"x"}}]`)
	res := mustQueryWindows(t, store, queryWindowsLabels("m", 0, 19, 10, map[string]string{"zone": "x"}))
	if len(res.Series) != 1 || len(res.Series[0].Windows) != 2 {
		t.Fatalf("windows after follow-up write = %+v, want 2", res.Series)
	}
	if math.Float64bits(res.Series[0].Windows[0].Average) != posZeroBits ||
		res.Series[0].Windows[1].Count != 1 ||
		math.Float64bits(res.Series[0].Windows[1].Average) != posSmallestSubnormal {
		t.Fatalf("windows after follow-up write = %+v", res.Series[0].Windows)
	}
}
