package darksafe

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

// 本文件回归保障区间均值查询的舍入行为：平均值以成功写入后存储的 float64 数值
// 为依据，精确算术平均舍入到最近的 float64；精确平均恰在两个相邻可表示值正中时，
// 取二进制有效数字末位为零的那个值（最近偶数舍入），正数与负数遵循同一规则。
// 极小值与零的精确平均舍入为零时保留符号（+0 与 -0），而数值完全抵消、
// 精确平均本来就是零时结果为 +0（见 TestQueryAverageExactZeroIsPositiveZero）。

// TestQueryAverageMidpointRoundsToNearestEven 覆盖普通数值的中点舍入：
// 同一序列在两个不同时间戳写入一对相邻 float64，精确平均恰在两者正中，
// 分别保护向较小值与向较大值舍入的情形，且负数不改变取偶数的含义。
func TestQueryAverageMidpointRoundsToNearestEven(t *testing.T) {
	cases := []struct {
		name string
		a, b string  // 写入的 JSON 数值字面量
		want float64 // 期望的平均值
	}{
		// 精确平均 1.0000000000000001 在 1（末位偶）与 1.0000000000000002（末位奇）
		// 正中：向较小值舍入为 1。朴素 float64 求和 (1+1.0000000000000002)/2 会
		// 先舍入和再除，结果同样巧合为 1，但这里保障的是精确平均的取偶规则。
		{"positive tie rounds down to even", "1", "1.0000000000000002", 1},
		// 精确平均 1.0000000000000003 在 1.0000000000000002（末位奇）与
		// 1.0000000000000004（末位偶）正中：向较大值舍入。
		{"positive tie rounds up to even", "1.0000000000000002", "1.0000000000000004", 1.0000000000000004},
		// 负数遵循同一取偶规则：-1 的有效数字末位为零，向零方向（较大值）舍入。
		{"negative tie rounds to even toward zero", "-1", "-1.0000000000000002", -1},
		// -1.0000000000000004 末位为偶，向远离零方向（较小值）舍入。
		{"negative tie rounds to even away from zero", "-1.0000000000000002", "-1.0000000000000004", -1.0000000000000004},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			mustOK(t, store, `[
				{"name":"m","timestamp":1,"value":`+tc.a+`},
				{"name":"m","timestamp":2,"value":`+tc.b+`}
			]`)
			res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
			if len(res.Series) != 1 {
				t.Fatalf("series = %+v, want exactly one", res.Series)
			}
			s0 := res.Series[0]
			if s0.Count != 2 {
				t.Fatalf("count = %d, want 2", s0.Count)
			}
			if math.IsInf(s0.Average, 0) || math.IsNaN(s0.Average) {
				t.Fatalf("average must be finite, got %v", s0.Average)
			}
			if s0.Average != tc.want {
				t.Fatalf("average of %s and %s = %v, want %v (round half to even)",
					tc.a, tc.b, s0.Average, tc.want)
			}
		})
	}
}

// TestQueryAverageSubnormalTieKeepsZeroSign 覆盖极小值与零的中点舍入：
// 5e-324 是最小的正 float64，它与 0 的精确平均（2^-1075）在 0 与该值正中，
// 最近偶数舍入为 +0；-5e-324 对称地舍入为 -0。两者数值相等，但符号必须保留，
// JSON 输出分别为 0 与 -0。
func TestQueryAverageSubnormalTieKeepsZeroSign(t *testing.T) {
	cases := []struct {
		name        string
		value       string // 与 0 配对的极小值字面量
		wantNeg     bool   // 期望结果符号位
		wantAvgJSON string // 期望结果中 average 字段的 JSON 文本
	}{
		{"smallest positive rounds to positive zero", "5e-324", false, `"average":0`},
		{"smallest negative rounds to negative zero", "-5e-324", true, `"average":-0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			mustOK(t, store, `[
				{"name":"m","timestamp":1,"value":`+tc.value+`},
				{"name":"m","timestamp":2,"value":0}
			]`)
			res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
			if len(res.Series) != 1 {
				t.Fatalf("series = %+v, want exactly one", res.Series)
			}
			s0 := res.Series[0]
			if s0.Count != 2 {
				t.Fatalf("count = %d, want 2", s0.Count)
			}
			if s0.Average != 0 || math.Signbit(s0.Average) != tc.wantNeg {
				t.Fatalf("average of %s and 0 = %v (signbit=%v), want zero with signbit=%v",
					tc.value, s0.Average, math.Signbit(s0.Average), tc.wantNeg)
			}
			b, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), tc.wantAvgJSON) {
				t.Fatalf("query result JSON = %s, want it to contain %s", b, tc.wantAvgJSON)
			}
		})
	}
}

// TestQueryAverageTieAtFloat64Boundary 覆盖均值落在 float64 表示边界正中时的舍入：
// 2 与它下方紧邻的值（1.9999999999999998）的精确平均恰在两者正中，2 的有效数字
// 末位为零，最近偶数舍入为 2；最小正规正数（2.2250738585072014e-308，末位偶）与
// 它下方最大的次正规正数（2.225073858507201e-308，末位奇）的中点同样回到最小正规
// 正数，不得变成零或其他相邻值。负数遵循同一取偶规则：这里中点向远离零的一侧
// 取值（偶数侧），与 TestQueryAverageMidpointRoundsToNearestEven 中向零取偶的情形
// 合并说明规则只看有效数字末位，与符号方向无关。
// 两个输入都是普通的有限 JSON 数字，写入与查询均为正常成功结果。
func TestQueryAverageTieAtFloat64Boundary(t *testing.T) {
	cases := []struct {
		name string
		a, b string  // 写入的 JSON 数值字面量（一对相邻 float64）
		want float64 // 期望的平均值（中点取偶后的一侧）
	}{
		// 2 的有效数字末位为零：中点 1.9999999999999999 舍入为 2。
		{"tie below 2 rounds up to even 2", "1.9999999999999998", "2", 2},
		// -2 同样末位为偶：负数中点向远离零的一侧取偶，不是固定朝零。
		{"negative tie below -2 rounds to even -2", "-1.9999999999999998", "-2", -2},
		// 最小正规正数末位为偶，最大次正规值末位为奇：中点回到最小正规正数，
		// 不能因数值极小而冲刷为零。
		{"tie at smallest normal rounds to smallest normal", "2.225073858507201e-308", "2.2250738585072014e-308", 2.2250738585072014e-308},
		// 负数对称情形：中点取偶为负的最小正规数，不是 -0。
		{"negative tie at smallest normal keeps sign", "-2.225073858507201e-308", "-2.2250738585072014e-308", -2.2250738585072014e-308},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			mustOK(t, store, `[
				{"name":"m","timestamp":1,"value":`+tc.a+`},
				{"name":"m","timestamp":2,"value":`+tc.b+`}
			]`)
			res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
			if len(res.Series) != 1 {
				t.Fatalf("series = %+v, want exactly one", res.Series)
			}
			s0 := res.Series[0]
			if s0.Count != 2 {
				t.Fatalf("count = %d, want 2", s0.Count)
			}
			if math.IsInf(s0.Average, 0) || math.IsNaN(s0.Average) {
				t.Fatalf("average must be finite, got %v", s0.Average)
			}
			// 必须精确等于期望的相邻值：== 能区分相邻 float64，
			// 不允许用容差把另一侧相邻值（或零）也判为通过。
			if s0.Average != tc.want {
				t.Fatalf("average of %s and %s = %v, want %v (round half to even at boundary)",
					tc.a, tc.b, s0.Average, tc.want)
			}
			if tc.want != 0 && s0.Average == 0 {
				t.Fatalf("tiny average %v must not be flushed to zero", s0.Average)
			}
			// JSON 输出中的数值与查询返回值一致（同一 float64 的最短十进制表示）。
			b, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON := `"average":` + strconv.FormatFloat(tc.want, 'g', -1, 64)
			if !strings.Contains(string(b), wantJSON) {
				t.Fatalf("query result JSON = %s, want it to contain %s", b, wantJSON)
			}
		})
	}
}

// TestQueryAverageNearBoundaryRoundsToTrueNearest 让精确平均稍偏离边界中点：
// 同一数值在多个时间戳各算一个采样点，三个点 2、2、1.9999999999999998 的精确
// 平均（2 - 2^-52/3）比中点（2 - 2^-53）更靠近 2，应舍入为 2；两个
// 1.9999999999999998 加一个 2 的精确平均（2 - 2^-51/3）在中点另一侧，应舍入为
// 1.9999999999999998。只在中点碰巧正确的实现（例如先按 float64 求和：和
// 6 - 2^-51 自身是中点、按取偶舍回 6，再除以 3 得 2）会在后一情形露馅。
// 次正规边界同理：两个最小正规正数加一个最大次正规值在中点上方，应得最小正规
// 正数；反过来两个最大次正规值加一个最小正规正数在中点下方，应得最大次正规值。
func TestQueryAverageNearBoundaryRoundsToTrueNearest(t *testing.T) {
	cases := []struct {
		name   string
		values []string // 三个时间戳依次写入的 JSON 数值字面量
		want   float64
	}{
		{"just above midpoint below 2 rounds to 2",
			[]string{"2", "2", "1.9999999999999998"}, 2},
		{"just below midpoint below 2 rounds to lower neighbor",
			[]string{"1.9999999999999998", "1.9999999999999998", "2"}, 1.9999999999999998},
		{"just above midpoint at smallest normal rounds to smallest normal",
			[]string{"2.2250738585072014e-308", "2.2250738585072014e-308", "2.225073858507201e-308"}, 2.2250738585072014e-308},
		{"just below midpoint at smallest normal rounds to largest subnormal",
			[]string{"2.225073858507201e-308", "2.225073858507201e-308", "2.2250738585072014e-308"}, 2.225073858507201e-308},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			line := `[{"name":"m","timestamp":1,"value":` + tc.values[0] +
				`},{"name":"m","timestamp":2,"value":` + tc.values[1] +
				`},{"name":"m","timestamp":3,"value":` + tc.values[2] + `}]`
			mustOK(t, store, line)
			res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":3}`)
			if len(res.Series) != 1 {
				t.Fatalf("series = %+v, want exactly one", res.Series)
			}
			s0 := res.Series[0]
			// 相同数值出现在不同时间戳仍是多个采样点，按点数加权。
			if s0.Count != 3 {
				t.Fatalf("count = %d, want 3 (each timestamp is a sample)", s0.Count)
			}
			if s0.Average != tc.want {
				t.Fatalf("average of %v = %v, want %v (nearest float64 to the exact mean)",
					tc.values, s0.Average, tc.want)
			}
		})
	}
}

// TestQueryBoundaryRangeAndDedup 确认边界值在区间裁剪与去重下的行为：
// 查询区间只含边界一侧的单个点时，count 为 1 且 average 保持该点的存储值，
// 区间外另一侧的点不参与平均；同一时间戳重复提交相同数值沿用去重行为，
// 不增加采样点个数。
func TestQueryBoundaryRangeAndDedup(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":2},
		{"name":"m","timestamp":2,"value":1.9999999999999998},
		{"name":"m","timestamp":3,"value":2.2250738585072014e-308},
		{"name":"m","timestamp":4,"value":2.225073858507201e-308}
	]`)
	// 同一时间戳重复写入相同数值：重复，成功忽略，不增加采样点。
	dup := mustOK(t, store, `[{"name":"m","timestamp":2,"value":1.9999999999999998}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("duplicate resubmit = added %d duplicates %d, want 0/1", dup.Added, dup.Duplicates)
	}

	single := []struct {
		name       string
		start, end int64
		want       float64
	}{
		{"only 2 in range", 1, 1, 2},
		{"only value below 2 in range", 2, 2, 1.9999999999999998},
		{"only smallest normal in range", 3, 3, 2.2250738585072014e-308},
		{"only largest subnormal in range", 4, 4, 2.225073858507201e-308},
	}
	for _, tc := range single {
		t.Run(tc.name, func(t *testing.T) {
			q := `{"op":"query","name":"m","start":` + strconv.FormatInt(tc.start, 10) +
				`,"end":` + strconv.FormatInt(tc.end, 10) + `}`
			res := mustQuery(t, store, q)
			if len(res.Series) != 1 {
				t.Fatalf("series = %+v, want exactly one", res.Series)
			}
			s0 := res.Series[0]
			if s0.Count != 1 || s0.Average != tc.want {
				t.Fatalf("single-point range = count %d average %v, want count 1 average %v (out-of-range neighbor must not participate)",
					s0.Count, s0.Average, tc.want)
			}
		})
	}

	// 去重后两点区间仍按两个采样点计：中点取偶为 2，重复提交不增加权重。
	res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if len(res.Series) != 1 || res.Series[0].Count != 2 || res.Series[0].Average != 2 {
		t.Fatalf("range [1,2] after duplicate = %+v, want count 2 average 2", res.Series)
	}
	// 次正规一侧的单点区间：极小的存储值原样返回，不得被当成零。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":4,"end":4}`)
	if len(res.Series) != 1 || res.Series[0].Average != 2.225073858507201e-308 || res.Series[0].Average == 0 {
		t.Fatalf("subnormal single point = %+v, want stored largest subnormal, not zero", res.Series)
	}
}

// TestQueryRoundingOnlyCountsPointsInRange 确认参与平均的点须位于查询闭区间内：
// 区间外的点不影响点数与舍入结果；单点区间中非零有限值的平均仍是该点的值。
func TestQueryRoundingOnlyCountsPointsInRange(t *testing.T) {
	store := NewMetricStore()
	// 同一序列三个点；ts=3 的点在查询区间 [1,2] 之外。
	// 若它参与平均，count 会为 3 且均值远离中点舍入结果 1。
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":1.0000000000000002},
		{"name":"m","timestamp":3,"value":7}
	]`)

	res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", res.Series)
	}
	if res.Series[0].Count != 2 || res.Series[0].Average != 1 {
		t.Fatalf("range [1,2] = count %d average %v, want count 2 average 1 (out-of-range point must not participate)",
			res.Series[0].Count, res.Series[0].Average)
	}

	// 单点区间：非零有限值的平均就是该点存储的值本身。
	res = mustQuery(t, store, `{"op":"query","name":"m","start":2,"end":2}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 1.0000000000000002 {
		t.Fatalf("single-point range [2,2] = %+v, want count 1 average 1.0000000000000002", res.Series)
	}
	res = mustQuery(t, store, `{"op":"query","name":"m","start":3,"end":3}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 7 {
		t.Fatalf("single-point range [3,3] = %+v, want count 1 average 7", res.Series)
	}
}
