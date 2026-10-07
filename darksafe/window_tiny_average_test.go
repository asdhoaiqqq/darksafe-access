package darksafe

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// 本文件为固定时间窗口统计（query_windows）补充“零附近极小数均值”的自动化
// 回归保障。依据是写入后实际保存的有限 float64 值：窗口均值先在有理数上精确
// 求和、精确除以点数，再按最近偶数（二进制有效数字末位为零）舍入为可表示的
// float64。参与计算的微小非零值绝不能在求平均前被当成零，也不能按十进制小数
// 位截断。
//
// 重点锁定正零与负零在数值相等下的区别：
//   - 一个窗口在两个不同时间戳分别保存最小正次正规值（5e-324，二进制 0x1）
//     与 0：精确平均是最小正次正规值的一半（2^-1075），离正零更近，结果舍入
//     为正零，JSON 中 average 写成 0；把非零采样换成 -5e-324，精确平均是
//     -2^-1075，结果舍入为负零，JSON 必须保留 -0。
//   - 窗口内是大小相等、符号相反的两个非零采样时，精确平均本来就是零，
//     输出正零（不是负零），与上面的“负的微小均值被舍入为零”明确区分。
//
// 同时锁定最小可表示非零值两侧的中点结果（最近偶数在零附近的表现）：
//   - 两个 5e-324 与一个 0：精确平均 (2/3)·2^-1074 离 2^-1074 更近，
//     均值必须是 5e-324 而不是零；
//   - 一个 5e-324 与两个 0：精确平均 (1/3)·2^-1074 离零更近，均值为正零；
//   - 负数遵循同一条符号规则，分别给出 -5e-324 与负零。
//
// 其余结构约定一并保护：同值采样出现在不同时间戳分别计数；同一位置的等值
// 重复按既有写入规则忽略，不改变窗口点数，也不能把均值推过中点；相邻窗口
// 各自只反映自己的点，窗口之间不混入点数或数值；区间外采样不参与；均值为零
// 的窗口仍是“有数据”的成功结果（保留真实 count），不能像空窗口那样省略，
// 而真正没有采样的窗口继续不出现；结果携带完整序列身份并按窗口起点升序。

// minSubnormalLiteral 是最小的可表示非零正 float64（次正规数 2^-1074，
// 二进制 0x0000000000000001）的 JSON 数字写法；其负数写法是 -5e-324。
const minSubnormalLiteral = "5e-324"

// minSubnormalAltLiteral 是另一个不同的十进制文本，转换后同样舍入为最小正
// 次正规值——用它在同一位置重提，验证等值重复按转换后的 float64 判定。
const minSubnormalAltLiteral = "4.9406564584124654e-324"

// tinySample 构造一条写入到指标 name、时间戳 ts 的采样点 JSON。
func tinySample(name string, ts int64, valueLiteral string, labels map[string]string) string {
	return fmt.Sprintf(`{"name":%s,"timestamp":%d,"value":%s%s}`,
		jsonString(name), ts, valueLiteral, tinyLabelsSuffix(labels))
}

func tinyLabelsSuffix(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	return `,"labels":` + jsonLabels(labels)
}

// ingestTiny 在同一批次内写入多条带 JSON 数字字面量的采样点。
func ingestTiny(t *testing.T, store *MetricStore, lines ...string) {
	t.Helper()
	mustOK(t, store, "["+strings.Join(lines, ",")+"]")
}

// tinyWindowResult 取出一条序列在唯一/指定窗口上的统计；调用方保证查询成功
// 且恰好命中该序列。
func tinyWindowResult(t *testing.T, res *QueryWindowsResult, seriesIdx, windowIdx int, note string) Window {
	t.Helper()
	if len(res.Series) <= seriesIdx {
		t.Fatalf("%s: got %d series, want index %d: %+v", note, len(res.Series), seriesIdx, res.Series)
	}
	wins := res.Series[seriesIdx].Windows
	if len(wins) <= windowIdx {
		t.Fatalf("%s: series %d got %d windows, want index %d: %+v",
			note, seriesIdx, len(wins), windowIdx, wins)
	}
	return wins[windowIdx]
}

// assertWindowCountAverage 按位断言一个窗口的 count 与 average（正零/负零
// 因此可区分），average 还必须是有限值。
func assertWindowCountAverage(t *testing.T, got Window, wantCount int, wantAvg float64, note string) {
	t.Helper()
	if got.Count != wantCount {
		t.Fatalf("%s: count = %d, want %d", note, got.Count, wantCount)
	}
	if math.IsInf(got.Average, 0) || math.IsNaN(got.Average) {
		t.Fatalf("%s: average must be finite, got %v", note, got.Average)
	}
	if math.Float64bits(got.Average) != math.Float64bits(wantAvg) {
		t.Fatalf("%s: average = %v (%016x), want %v (%016x)",
			note, got.Average, math.Float64bits(got.Average), wantAvg, math.Float64bits(wantAvg))
	}
}

// TestQueryWindowsTinyMeanSignedZero 保护零附近最关键的区别：正的微小均值
// 舍入为正零、负的微小均值舍入为负零、采样真正抵消时精确平均为零（正零）。
// 三种情形数值上都等于零，count 都为 2，但结果的符号位与 JSON 文本不同。
func TestQueryWindowsTinyMeanSignedZero(t *testing.T) {
	posZero := 0.0
	negZero := math.Copysign(0, -1)

	cases := []struct {
		name        string
		values      [2]string // 两个不同时间戳 0、1，同一窗口 [0,9]
		wantAvg     float64
		wantJSONAvg string // 窗口 JSON 中 average 字段的原始文本
	}{
		// (5e-324 + 0)/2 = 2^-1075，距正零 2^-1075、距 5e-324 同样是
		// 2^-1075——精确结果恰居正中，且零的有效数字末位为零，取偶落到正零。
		{"positive tiny mean rounds to positive zero",
			[2]string{minSubnormalLiteral, "0"}, posZero, "0"},
		// (-5e-324 + 0)/2 = -2^-1075，同理舍入为负零，符号必须保留。
		{"negative tiny mean rounds to negative zero",
			[2]string{"-" + minSubnormalLiteral, "0"}, negZero, "-0"},
		// 两个非零采样大小相等、符号相反：精确平均本来就是零（不是舍入来的），
		// 按约定输出正零。
		{"equal opposite nonzero samples cancel to positive zero",
			[2]string{minSubnormalLiteral, "-" + minSubnormalLiteral}, posZero, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			ingestTiny(t, store,
				tinySample("m", 0, tc.values[0], nil),
				tinySample("m", 1, tc.values[1], nil))

			res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
			if len(res.Series) != 1 {
				t.Fatalf("series = %+v, want exactly one", res.Series)
			}
			wins := res.Series[0].Windows
			if len(wins) != 1 {
				t.Fatalf("windows = %+v, want the single populated window (zero mean is still data)", wins)
			}
			assertWindowCountAverage(t, wins[0], 2, tc.wantAvg, tc.name)

			// 锁定用户取得的 JSON 数值表示：正零与负零在 JSON 中必须可区分。
			raw := marshalCompact(t, res)
			wantField := `"average":` + tc.wantJSONAvg
			if !strings.Contains(raw, wantField) {
				t.Fatalf("%s: result JSON %s must contain %s", tc.name, raw, wantField)
			}
			// JSON 往返后符号位不变：解析 -0 仍应是负零。
			var round QueryWindowsResult
			if err := json.Unmarshal([]byte(raw), &round); err != nil {
				t.Fatalf("%s: unmarshal result: %v", tc.name, err)
			}
			gotAvg := round.Series[0].Windows[0].Average
			if math.Signbit(gotAvg) != math.Signbit(tc.wantAvg) {
				t.Fatalf("%s: average sign after JSON round-trip = signbit %v, want %v (raw %s)",
					tc.name, math.Signbit(gotAvg), math.Signbit(tc.wantAvg), raw)
			}
		})
	}
}

// TestQueryWindowsTinyMeanMidpointSides 保护最小非零值与零之间的中点两侧：
// 三点组合让精确平均分别落在 (1/3)·2^-1074 与 (2/3)·2^-1074，结果必须是
// 正零/负零或 ±5e-324，不能把微小非零采样提前清零。
func TestQueryWindowsTinyMeanMidpointSides(t *testing.T) {
	minSub := math.Float64frombits(0x0000000000000001)
	posZero := 0.0
	negZero := math.Copysign(0, -1)

	cases := []struct {
		name    string
		values  []string // 时间戳 1..n，全部落在同一窗口 [0,9]
		wantAvg float64
		note    string
	}{
		// 两个 5e-324 + 一个 0：均值 (2/3)·2^-1074，比中点（1/2)·2^-1074
		// 更靠近 2^-1074，取 5e-324；朴素地先把微小值清零会错误地得到 0。
		{"two positive minima and a zero return min subnormal",
			[]string{minSubnormalLiteral, minSubnormalLiteral, "0"}, minSub,
			"(2/3)*2^-1074 rounds up to 2^-1074"},
		// 一个 5e-324 + 两个 0：均值 (1/3)·2^-1074，比中点更靠近零，取正零；
		// 朴素地“非零就算 5e-324”会错误地把均值抬到中点另一侧。
		{"one positive minimum and two zeros return positive zero",
			[]string{minSubnormalLiteral, "0", "0"}, posZero,
			"(1/3)*2^-1074 rounds down to +0"},
		// 负数对称：两个 -5e-324 + 一个 0 给出 -5e-324。
		{"two negative minima and a zero return negative min subnormal",
			[]string{"-" + minSubnormalLiteral, "-" + minSubnormalLiteral, "0"}, -minSub,
			"-(2/3)*2^-1074 rounds to -2^-1074"},
		// 一个 -5e-324 + 两个 0：负的微小均值舍入为负零。
		{"one negative minimum and two zeros return negative zero",
			[]string{"-" + minSubnormalLiteral, "0", "0"}, negZero,
			"-(1/3)*2^-1074 rounds to -0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			samples := make([]string, len(tc.values))
			for i, v := range tc.values {
				samples[i] = tinySample("m", int64(i+1), v, nil)
			}
			ingestTiny(t, store, samples...)

			res := mustQueryWindows(t, store, queryWindowsAll("m", 1, 10, 10))
			w := tinyWindowResult(t, res, 0, 0, tc.name)
			assertWindowCountAverage(t, w, len(tc.values), tc.wantAvg, tc.note)

			// 非零的 ±5e-324 必须以可往返的极小 JSON 数字出现，不能写成 0；
			// ±0 的区分已在另一个测试锁定，这里只核对数值文本不是被截断的十进制。
			raw := marshalCompact(t, res)
			if tc.wantAvg != 0 {
				wantField := `"average":` + formatFloat(tc.wantAvg)
				if !strings.Contains(raw, wantField) {
					t.Fatalf("%s: result JSON %s must preserve tiny nonzero average %s",
						tc.name, raw, wantField)
				}
			}
		})
	}
}

// TestQueryWindowsTinyMeanRepeatedTimestampsDedup 等值重复只在“同一序列同一
// 时间戳”上忽略：同值采样出现在不同时间戳分别计数；同一位置用不同十进制
// 文本提交同一个 float64 仍按既有规则计为 duplicate，点数不增加，均值也
// 因此留在中点原来的一侧。
func TestQueryWindowsTinyMeanRepeatedTimestampsDedup(t *testing.T) {
	minSub := math.Float64frombits(0x0000000000000001)
	store := NewMetricStore()

	// 初始窗口 [0,9]：一个 5e-324（时间戳 1）与一个 0（时间戳 2），
	// 精确平均在零与最小非零值正中，取偶为正零，count 为 2。
	ingestTiny(t, store,
		tinySample("m", 1, minSubnormalLiteral, nil),
		tinySample("m", 2, "0", nil))
	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	assertWindowCountAverage(t, res.Series[0].Windows[0], 2, 0, "tie before dedup submissions")

	// 不同时间戳再存一个 5e-324：它是独立的第三个点，均值越过中点变为
	// 5e-324，count 为 3。
	ingestTiny(t, store, tinySample("m", 3, minSubnormalLiteral, nil))
	res = mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	assertWindowCountAverage(t, res.Series[0].Windows[0], 3, minSub,
		"same value on a new timestamp counts separately and pushes mean across the midpoint")

	// 同一时间戳 3 用另一种十进制文本重提同一个 float64：等值重复被忽略，
	// 写回 added=0/duplicates=1，窗口点数与均值保持不变。
	dup := mustOK(t, store, "["+tinySample("m", 3, minSubnormalAltLiteral, nil)+"]")
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("alternate-spelling duplicate = added %d duplicates %d, want 0/1",
			dup.Added, dup.Duplicates)
	}
	res = mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	assertWindowCountAverage(t, res.Series[0].Windows[0], 3, minSub,
		"equal float64 on the same timestamp is ignored")

	// 同一位置用 -0 重提（与已存 0 数值相等），同样只是重复：不改变点数，
	// 也不把正零侧窗口变成负零。
	dup = mustOK(t, store, "["+tinySample("m", 2, "-0", nil)+"]")
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("signed-zero duplicate = added %d duplicates %d, want 0/1",
			dup.Added, dup.Duplicates)
	}
	res = mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	assertWindowCountAverage(t, res.Series[0].Windows[0], 3, minSub,
		"-0 resubmitted over stored 0 is an equal-value duplicate")

	// 同值点放在新时间戳上是真正的新增点，而同一位置的等值重复永远不计数：
	// 再在时间戳 4 放一个 0，count 变 4，均值 (2·5e-324)/4 = 2^-1075
	// 再次恰居正中，取偶回到正零——count 的变化只来自不同时间戳。
	ingestTiny(t, store, tinySample("m", 4, "0", nil))
	res = mustQueryWindows(t, store, queryWindowsAll("m", 0, 9, 10))
	assertWindowCountAverage(t, res.Series[0].Windows[0], 4, 0,
		"two minima and two zeros tie again back to positive zero")
}

// TestQueryWindowsTinyMeanAdjacentWindowIsolation 极小数采样放在同一序列的
// 相邻窗口时，每个窗口只反映自己的点：正负微小均值不得跨窗混入点数或符号；
// 区间之外的采样完全不参与。均值为零的窗口仍然列出（带真实 count），
// 夹在两个非零均值窗口之间的空窗口则继续省略。
func TestQueryWindowsTinyMeanAdjacentWindowIsolation(t *testing.T) {
	minSub := math.Float64frombits(0x0000000000000001)
	negZero := math.Copysign(0, -1)
	store := NewMetricStore()

	// step=10、区间 [0,39]：
	//   窗口 [0,9]：ts0=5e-324、ts1=0           → count 2，正零
	//   窗口 [10,19]：ts10=-5e-324、ts11=0      → count 2，负零
	//   窗口 [20,29]：无点                        → 整条省略
	//   窗口 [30,39]：ts30=5e-324、ts31=5e-324、ts32=0 → count 3，5e-324
	// 另在区间外 ts100 放一个 5e-324，任何窗口都不能带上它。
	ingestTiny(t, store,
		tinySample("m", 0, minSubnormalLiteral, nil),
		tinySample("m", 1, "0", nil),
		tinySample("m", 10, "-"+minSubnormalLiteral, nil),
		tinySample("m", 11, "0", nil),
		tinySample("m", 30, minSubnormalLiteral, nil),
		tinySample("m", 31, minSubnormalLiteral, nil),
		tinySample("m", 32, "0", nil),
		tinySample("m", 100, minSubnormalLiteral, nil))

	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 39, 10))
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v, want exactly one", res.Series)
	}
	wins := res.Series[0].Windows
	if len(wins) != 3 {
		t.Fatalf("windows = %+v, want 3 populated windows (empty [20,29] omitted)", wins)
	}
	// 窗口按起点升序，且每个窗口边界与统计各自独立。
	want := []Window{
		window(0, 9, 2, 0),
		window(10, 19, 2, negZero),
		window(30, 39, 3, minSub),
	}
	assertWindows(t, wins, want, "adjacent windows isolate tiny values; zero-mean windows kept")

	// JSON 中相邻两个零均值窗口的符号必须分别是 0 与 -0。
	raw := marshalCompact(t, res)
	firstZero := strings.Index(raw, `"average":0`)
	negZeroPos := strings.Index(raw, `"average":-0`)
	if firstZero < 0 || negZeroPos < 0 {
		t.Fatalf("JSON must show both positive and negative zero averages: %s", raw)
	}
	if firstZero > negZeroPos {
		t.Fatalf("positive-zero window must sort before negative-zero window: %s", raw)
	}

	// 只查前两个窗口时看不到第三个窗口的点，也看不到区间外 ts100 的点。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 0, 19, 10))
	assertWindows(t, res.Series[0].Windows, want[:2], "first two windows only")

	// 把区间收紧到一个窗口：其余窗口与区间外点都不参与。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 10, 19, 10))
	assertWindows(t, res.Series[0].Windows, []Window{window(10, 19, 2, negZero)},
		"single negative-zero window")

	// 一个不覆盖任何已有点的区间：成功但空 series，ts100 的点不能让这里非空。
	res = mustQueryWindows(t, store, queryWindowsAll("m", 40, 99, 10))
	if len(res.Series) != 0 {
		t.Fatalf("series = %+v, want empty for a range with no points", res.Series)
	}
}

// TestQueryWindowsTinyMeanZeroWindowIsNotEmptyWindow 均值为零与“窗口无数据”
// 是两回事：前者保留窗口与真实 count，后者整条不出现。同一查询里两种窗口
// 并存时必须能区分。
func TestQueryWindowsTinyMeanZeroWindowIsNotEmptyWindow(t *testing.T) {
	negZero := math.Copysign(0, -1)
	store := NewMetricStore()
	// 窗口 [0,9] 有两个点（-5e-324 与 0）→ 负零、count 2；
	// 窗口 [10,19] 无点 → 省略；
	// 窗口 [20,29] 有两个真正抵消的点（±5e-324）→ 正零、count 2。
	ingestTiny(t, store,
		tinySample("m", 0, "-"+minSubnormalLiteral, nil),
		tinySample("m", 1, "0", nil),
		tinySample("m", 20, minSubnormalLiteral, nil),
		tinySample("m", 21, "-"+minSubnormalLiteral, nil))

	res := mustQueryWindows(t, store, queryWindowsAll("m", 0, 29, 10))
	wins := res.Series[0].Windows
	if len(wins) != 2 {
		t.Fatalf("windows = %+v, want two zero-mean windows with the empty one omitted", wins)
	}
	assertWindows(t, wins, []Window{
		window(0, 9, 2, negZero),
		window(20, 29, 2, 0),
	}, "zero-mean windows carry real counts and stay ordered")
}

// TestQueryWindowsTinyMeanPreservesSeriesIdentity 极小数窗口统计仍携带完整
// 序列身份（指标名 + 完整标签集合，不缩减为查询条件），多序列按既有次序
// 排列，且各序列的微小窗口值互不污染；查询只读，不改变存储。
func TestQueryWindowsTinyMeanPreservesSeriesIdentity(t *testing.T) {
	minSub := math.Float64frombits(0x0000000000000001)
	negZero := math.Copysign(0, -1)
	store := NewMetricStore()
	ingestTiny(t, store,
		// host=a：一个窗口两点 → 正零。
		tinySample("sig", 0, minSubnormalLiteral, map[string]string{"host": "a", "zone": "x"}),
		tinySample("sig", 1, "0", map[string]string{"host": "a", "zone": "x"}),
		// host=b：一个窗口两点 → 负零。
		tinySample("sig", 2, "-"+minSubnormalLiteral, map[string]string{"host": "b"}),
		tinySample("sig", 3, "0", map[string]string{"host": "b"}),
		// 另一指标同名标签不参与。
		tinySample("other", 0, "9", map[string]string{"host": "a"}))

	// 子集查询 host=a：返回完整标签集合 {host:a,zone:x}，且只有 host=a 的正零窗。
	res := mustQueryWindows(t, store,
		queryWindowsLabels("sig", 0, 9, 10, map[string]string{"host": "a"}))
	if len(res.Series) != 1 {
		t.Fatalf("subset series = %+v, want exactly host=a", res.Series)
	}
	s0 := res.Series[0]
	if s0.Name != "sig" || !sameLabels(s0.Labels, map[string]string{"host": "a", "zone": "x"}) {
		t.Fatalf("full identity not preserved: %+v", s0)
	}
	assertWindows(t, s0.Windows, []Window{window(0, 9, 2, 0)}, "host=a positive-zero window")

	// 省略 labels 命中 sig 的两条序列，按 organizeSeries 的标签字典序排列
	// （host=a,zone=x 在 host=b 之前），各自的零均值符号独立。
	res = mustQueryWindows(t, store, queryWindowsAll("sig", 0, 9, 10))
	if len(res.Series) != 2 {
		t.Fatalf("series = %+v, want two sig series", res.Series)
	}
	if !sameLabels(res.Series[0].Labels, map[string]string{"host": "a", "zone": "x"}) ||
		!sameLabels(res.Series[1].Labels, map[string]string{"host": "b"}) {
		t.Fatalf("series order/identity = %+v / %+v", res.Series[0].Labels, res.Series[1].Labels)
	}
	assertWindows(t, res.Series[0].Windows, []Window{window(0, 9, 2, 0)}, "host=a")
	assertWindows(t, res.Series[1].Windows, []Window{window(0, 9, 2, negZero)}, "host=b")

	// 非零均值窗同样保留身份：两个 5e-324 + 一个 0 在 host=a 的第二窗口。
	ingestTiny(t, store,
		tinySample("sig", 10, minSubnormalLiteral, map[string]string{"host": "a", "zone": "x"}),
		tinySample("sig", 11, minSubnormalLiteral, map[string]string{"host": "a", "zone": "x"}),
		tinySample("sig", 12, "0", map[string]string{"host": "a", "zone": "x"}))
	res = mustQueryWindows(t, store,
		queryWindowsLabels("sig", 10, 19, 10, map[string]string{"zone": "x"}))
	if len(res.Series) != 1 {
		t.Fatalf("zone=x series = %+v, want one", res.Series)
	}
	assertWindows(t, res.Series[0].Windows, []Window{window(10, 19, 3, minSub)},
		"tiny nonzero window with full identity")

	// 只读：查询后写入快照仍是最初提交的点数，且重复查询结果一致。
	snap := mustOK(t, store, `[]`)
	points := 0
	for _, sv := range snap.Series {
		points += len(sv.Points)
	}
	if wantPoints := 2 + 2 + 1 + 3; points != wantPoints {
		t.Fatalf("query_windows must be read-only: snapshot has %d points, want %d (%+v)",
			points, wantPoints, snap.Series)
	}
}
