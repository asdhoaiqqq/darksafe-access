package darksafe

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// 本文件补充区间均值在 float64 表示边界上的回归保障。平均值以成功写入后保存的
// float64 数值（而不是十进制输入文本对应的精确实数）为依据，精确算术平均舍入为
// 最近的可表示值；恰好位于两个相邻值中间时取二进制有效数字末位为零的那个
// （最近偶数），正负号遵循同一规则，不固定朝零或朝远离零方向。
//
// 已覆盖的边界：
//   - 2 与其下方紧邻值 1.9999999999999998：两者跨越 binade，下方间距为 2^-52，
//     精确平均 2-2^-53 正中二者，取偶回到 2。
//   - 最小正规正数 2.2250738585072014e-308 与其下方最大次正规正数
//     2.225073858507201e-308：精确平均在二者正中，取偶回到最小正规正数，
//     不得变成零或次正规值。
//
// 除正中情形外，还构造精确平均稍低、稍高于中点的三点组合，确保实现不是只在
// 中点碰巧正确；相同数值出现在不同时间戳仍是多个采样点，权重按点数计算。

const (
	below2Literal    = "1.9999999999999998"      // 2 下方紧邻的 float64（0x3fffffffffffffff）
	above2Literal    = "2.0000000000000004"      // 2 上方紧邻的 float64（0x4000000000000001）
	minNormalLiteral = "2.2250738585072014e-308" // 最小正规正数（0x0010000000000000）
	maxSubnLiteral   = "2.225073858507201e-308"  // 最大次正规正数（0x000fffffffffffff）
)

// parseJSONFloat 按写入侧相同的 JSON 数字解析方式把字面量转为 float64。
func parseJSONFloat(t *testing.T, literal string) float64 {
	t.Helper()
	var v float64
	if err := json.Unmarshal([]byte(literal), &v); err != nil {
		t.Fatalf("unmarshal literal %s: %v", literal, err)
	}
	return v
}

// ingestBoundaryValues 在同一序列 m 上按时间戳 1..n 依次写入给定 JSON 数值字面量。
func ingestBoundaryValues(t *testing.T, store *MetricStore, values []string) {
	t.Helper()
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"m","timestamp":%d,"value":%s}`, i+1, v)
	}
	b.WriteByte(']')
	mustOK(t, store, b.String())
}

// assertBoundaryAverage 校验一次成功查询 [1,end]：唯一序列、平均有限且按位等于
// 期望的相邻 float64，并保证结果 JSON 往返后数值不偏到另一侧。count 由调用方断言。
func assertBoundaryAverage(t *testing.T, store *MetricStore, end int, want float64, note string) {
	t.Helper()
	res := mustQuery(t, store, fmt.Sprintf(`{"op":"query","name":"m","start":1,"end":%d}`, end))
	if res.Status != "ok" || res.Op != "query" {
		t.Fatalf("%s: status/op = %q/%q, want ok/query", note, res.Status, res.Op)
	}
	if len(res.Series) != 1 {
		t.Fatalf("%s: series = %+v, want exactly one", note, res.Series)
	}
	s0 := res.Series[0]
	if s0.Name != "m" {
		t.Fatalf("%s: series name = %q, want m", note, s0.Name)
	}
	if math.IsInf(s0.Average, 0) || math.IsNaN(s0.Average) {
		t.Fatalf("%s: average must be finite, got %v", note, s0.Average)
	}
	if math.Float64bits(s0.Average) != math.Float64bits(want) {
		t.Fatalf("%s: average = %v (%016x), want %v (%016x)",
			note, s0.Average, math.Float64bits(s0.Average), want, math.Float64bits(want))
	}
	// JSON 序列化再解析必须回到同一个 float64，不能在文本表示层面损失相邻值差异，
	// 极小值也不能因差值很小而变成零。
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("%s: marshal query result: %v", note, err)
	}
	var round QueryResult
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("%s: unmarshal query result: %v", note, err)
	}
	if math.Float64bits(round.Series[0].Average) != math.Float64bits(want) {
		t.Fatalf("%s: average JSON %s round-trips to %v (%016x), want %v (%016x)",
			note, raw, round.Series[0].Average, math.Float64bits(round.Series[0].Average),
			want, math.Float64bits(want))
	}
}

// TestQueryAverageBoundaryTies 覆盖两类表示边界上的正中舍入，正负数各一组：
// 中点两侧等距时取有效数字末位为零的那个值，结果不得偏到另一个相邻值。
func TestQueryAverageBoundaryTies(t *testing.T) {
	below2 := math.Float64frombits(0x3fffffffffffffff) // 1.9999999999999998
	minNormal := math.Float64frombits(0x0010000000000000)
	maxSubn := math.Float64frombits(0x000fffffffffffff)

	// 十进制文本先按 JSON 数字解析为存储值，再参与精确平均；不能把文本当成精确实数。
	for _, c := range []struct {
		literal string
		want    float64
	}{
		{below2Literal, below2},
		{above2Literal, math.Float64frombits(0x4000000000000001)},
		{minNormalLiteral, minNormal},
		{maxSubnLiteral, maxSubn},
	} {
		if got := parseJSONFloat(t, c.literal); math.Float64bits(got) != math.Float64bits(c.want) {
			t.Fatalf("stored value of %s = %016x, want %016x",
				c.literal, math.Float64bits(got), math.Float64bits(c.want))
		}
	}

	cases := []struct {
		name string
		// values 为两个不同时间戳写入的相邻 float64 字面量。
		values []string
		want   float64
		// otherSide 是中点另一侧的相邻值，结果必须与之按位不同（防止弱比较漏过偏移）。
		otherSide float64
	}{
		// 精确平均 2-2^-53 在 1.9999999999999998 与 2 正中：取偶回到 2。
		{"positive power-of-two tie returns even 2",
			[]string{below2Literal, "2"}, 2.0, below2},
		// 负数同一规则：-2 末位为零，向零方向取值，而不是固定朝远离零方向。
		{"negative power-of-two tie returns even -2",
			[]string{"-2", "-" + below2Literal}, -2.0, -below2},
		// 最小正规正数与最大次正规正数的中点：取偶回到最小正规正数，不变成零。
		{"normal/subnormal tie returns smallest normal",
			[]string{maxSubnLiteral, minNormalLiteral}, minNormal, maxSubn},
		// 负数对称：-最小正规数末位为零，结果不是零也不是 -最大次正规数。
		{"negative normal/subnormal tie returns -smallest normal",
			[]string{"-" + minNormalLiteral, "-" + maxSubnLiteral}, -minNormal, -maxSubn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			ingestBoundaryValues(t, store, tc.values)
			res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
			s0 := res.Series[0]
			if s0.Count != 2 {
				t.Fatalf("count = %d, want 2 distinct timestamps", s0.Count)
			}
			assertBoundaryAverage(t, store, 2, tc.want, tc.name)
			if math.Float64bits(s0.Average) == math.Float64bits(tc.otherSide) {
				t.Fatalf("%s: average landed on the other neighbor %v (%016x)",
					tc.name, tc.otherSide, math.Float64bits(tc.otherSide))
			}
		})
	}
}

// TestQueryAverageBoundaryNearTies 让精确平均稍低、稍高于上述中点：
// 三点组合按点数计权，必须分别回到中点两侧真正最近的相邻值，
// 从而识别“只在中点碰巧正确”的实现（例如朴素 float64 求和会在 2 的一侧出错）。
func TestQueryAverageBoundaryNearTies(t *testing.T) {
	below2 := math.Float64frombits(0x3fffffffffffffff)
	above2 := math.Float64frombits(0x4000000000000001) // 2.0000000000000004
	minNormal := math.Float64frombits(0x0010000000000000)
	maxSubn := math.Float64frombits(0x000fffffffffffff)

	cases := []struct {
		name   string
		values []string
		want   float64
	}{
		// 精确平均 = 2 - 2^-52/3，稍低于中点：真正最近的是下方相邻值。
		// 朴素 float64 求和 (lo+lo+2)/3 会先把和舍入为 6，错误地得到 2。
		{"power-of-two slightly below tie returns lower neighbor",
			[]string{below2Literal, below2Literal, "2"}, below2},
		// 精确平均 = 2 - 2·2^-52/3，仍稍低于 2，但最近值已是 2。
		{"power-of-two slightly above tie returns 2",
			[]string{below2Literal, "2", "2"}, 2.0},
		// 对称负数：偏向零的一侧取 -1.9999999999999998，偏向更负的一侧取 -2，
		// 证明取偶不依赖符号方向。
		{"negative power-of-two near tie toward zero returns -below2",
			[]string{"-" + below2Literal, "-" + below2Literal, "-2"}, -below2},
		{"negative power-of-two near tie away from zero returns -2",
			[]string{"-2", "-2", "-" + below2Literal}, -2.0},

		// 次正规边界稍低于中点（两个最大次正规值 + 一个最小正规值）：
		// 平均落在最大次正规值一侧，差值不足一个 ULP 也不能被冲刷成零。
		{"subnormal side slightly below tie returns largest subnormal",
			[]string{maxSubnLiteral, maxSubnLiteral, minNormalLiteral}, maxSubn},
		// 稍高于中点：回到最小正规正数。
		{"subnormal side slightly above tie returns smallest normal",
			[]string{maxSubnLiteral, minNormalLiteral, minNormalLiteral}, minNormal},
		// 负数两侧同一规则。
		{"negative subnormal side near tie returns -largest subnormal",
			[]string{"-" + maxSubnLiteral, "-" + maxSubnLiteral, "-" + minNormalLiteral}, -maxSubn},
		{"negative subnormal side near tie returns -smallest normal",
			[]string{"-" + minNormalLiteral, "-" + minNormalLiteral, "-" + maxSubnLiteral}, -minNormal},

		// 2 上方紧邻值参与时，稍低/稍高于其与 2 的中点同样按点数权重落到正确一侧。
		{"upper neighbor slightly below its tie returns 2",
			[]string{"2", "2", above2Literal}, 2.0},
		{"upper neighbor slightly above its tie returns upper neighbor",
			[]string{"2", above2Literal, above2Literal}, above2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			ingestBoundaryValues(t, store, tc.values)
			res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":3}`)
			if res.Series[0].Count != 3 {
				t.Fatalf("count = %d, want 3: repeated values on distinct timestamps are separate points",
					res.Series[0].Count)
			}
			assertBoundaryAverage(t, store, 3, tc.want, tc.name)
			// 次正规期望值必须与零明确区分：按位比较已保证，这里给出专门的失败信息。
			if (math.Float64bits(tc.want) < 0x0010000000000000) && res.Series[0].Average == 0 {
				t.Fatalf("%s: tiny nonzero average collapsed to zero", tc.name)
			}
		})
	}
}

// TestQueryAverageBoundaryDuplicateTimestampDedup 确认同一时间戳的重复采样
// 沿用已有去重行为：即使边界值多次提交，count 仍按唯一时间戳计为 1，
// 平均就是该点的存储值。
func TestQueryAverageBoundaryDuplicateTimestampDedup(t *testing.T) {
	below2 := math.Float64frombits(0x3fffffffffffffff)
	store := NewMetricStore()
	// 同批内同时间戳同值：重复但成功。
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":`+below2Literal+`},
		{"name":"m","timestamp":1,"value":1.9999999999999998}
	]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("within-batch duplicate counts = added %d duplicates %d, want 1/1",
			res.Added, res.Duplicates)
	}
	// 跨批同时间戳同值：同样计为重复。
	res = mustOK(t, store, `[{"name":"m","timestamp":1,"value":1.9999999999999998}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("cross-batch duplicate counts = added %d duplicates %d, want 0/1",
			res.Added, res.Duplicates)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1}`)
	if qr.Series[0].Count != 1 || math.Float64bits(qr.Series[0].Average) != math.Float64bits(below2) {
		t.Fatalf("dedup boundary point = %+v, want count 1 average %016x",
			qr.Series, math.Float64bits(below2))
	}
}

// TestQueryAverageBoundarySinglePointInRange 区间只包含边界一侧的单个点时
// count 为 1、平均保持该点存储值；区间外另一侧的相邻点不得参与平均。
func TestQueryAverageBoundarySinglePointInRange(t *testing.T) {
	below2 := math.Float64frombits(0x3fffffffffffffff)
	minNormal := math.Float64frombits(0x0010000000000000)
	maxSubn := math.Float64frombits(0x000fffffffffffff)

	type pt struct {
		ts    int64
		value string
		want  float64
	}
	cases := []struct {
		name   string
		points []pt
	}{
		{
			"power-of-two neighbors",
			[]pt{{10, below2Literal, below2}, {20, "2", 2.0}},
		},
		{
			"normal/subnormal neighbors",
			[]pt{{10, maxSubnLiteral, maxSubn}, {20, minNormalLiteral, minNormal}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			var b strings.Builder
			b.WriteByte('[')
			for i, p := range tc.points {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"name":"m","timestamp":%d,"value":%s}`, p.ts, p.value)
			}
			b.WriteByte(']')
			mustOK(t, store, b.String())

			// 只包含第一个点：第二个点在区间右端之外，不能把平均拉向中点。
			res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":15}`)
			s0 := res.Series[0]
			if s0.Count != 1 || math.Float64bits(s0.Average) != math.Float64bits(tc.points[0].want) {
				t.Fatalf("range [1,15] = %+v, want count 1 average %016x",
					s0, math.Float64bits(tc.points[0].want))
			}
			// 只包含第二个点：第一个点在区间左端之外。
			res = mustQuery(t, store, `{"op":"query","name":"m","start":15,"end":30}`)
			s0 = res.Series[0]
			if s0.Count != 1 || math.Float64bits(s0.Average) != math.Float64bits(tc.points[1].want) {
				t.Fatalf("range [15,30] = %+v, want count 1 average %016x",
					s0, math.Float64bits(tc.points[1].want))
			}
			// 端点闭区间同时包含两个点时才触发中点舍入。
			res = mustQuery(t, store, fmt.Sprintf(
				`{"op":"query","name":"m","start":%d,"end":%d}`, tc.points[0].ts, tc.points[1].ts))
			if res.Series[0].Count != 2 {
				t.Fatalf("full range count = %d, want 2", res.Series[0].Count)
			}
		})
	}
}

// TestQueryAverageBoundaryPreservesLabelsAndShape 保障序列名称、完整标签集合与
// 成功响应结构在边界均值场景下保持现状。
func TestQueryAverageBoundaryPreservesLabelsAndShape(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":`+below2Literal+`,"labels":{"host":"a","zone":"x"}},
		{"name":"m","timestamp":2,"value":2,"labels":{"host":"a","zone":"x"}}
	]`)
	res := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":2,"labels":{"host":"a"}}`)
	if res.Status != "ok" || res.Op != "query" || len(res.Series) != 1 {
		t.Fatalf("response shape = %+v, want ok/query with one series", res)
	}
	s0 := res.Series[0]
	if s0.Name != "m" || len(s0.Labels) != 2 || s0.Labels["host"] != "a" || s0.Labels["zone"] != "x" {
		t.Fatalf("identity/labels not preserved: %+v", s0)
	}
	if s0.Count != 2 || s0.Average != 2 {
		t.Fatalf("labeled boundary average = count %d avg %v, want count 2 average 2", s0.Count, s0.Average)
	}
}
