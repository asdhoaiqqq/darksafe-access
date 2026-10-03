package darksafe

import (
	"encoding/json"
	"math"
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
