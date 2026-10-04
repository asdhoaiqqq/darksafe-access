package darksafe

import (
	"strings"
	"testing"
)

// 本文件回归保障“同一条查询请求包含多个问题时”的错误选择规则，使用户能依据返回的
// 错误原因逐步修正输入、再看到剩余的问题；与 batch_error_selection_test.go 的写入侧
// 规则相对应：
//
//   - 整条输入必须先能解析为唯一的完整 JSON 值：查询对象残缺、对象之后还有第二个
//     值或非空白内容时，即使前面的字段已经有类型错误或未知字段，也只报整行解析失败。
//   - 结构完整的查询对象按字段书写顺序报告第一个字段问题：未知字段与字段类型错误
//     同时存在时先出现者获选，交换位置后原因随之改变；首次值合法的已知字段再次出现
//     一律报重复字段，即使第二次的值类型错误，直接书写与转义还原同名同样如此。
//   - 缺少必填字段只有在已出现字段都通过校验后才报告；多个必填字段同时缺失时按
//     op、name、start、end 的顺序选择，不受其他合法字段排列影响。
//   - start > end 的区间错误在字段校验与必填检查全部通过后才报告：同一请求既有
//     倒置区间又有字段问题时先指出字段问题，修正后再指出区间问题。
//   - 所有查询失败只返回错误：不返回查询结果，不带 index 与 conflict；此前成功
//     写入的数据在失败之后仍可按原范围查询到相同的标签、点数与平均值。

// assertQueryError 校验查询级失败：错误原因包含全部 want 子串（用于区分不同的
// 失败类别并指出对应字段或区间），且不带 index 与 conflict。
func assertQueryError(t *testing.T, lerr *LineError, want ...string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected query failure, got success")
	}
	if lerr.Index != 0 {
		t.Fatalf("query errors must not carry index, got %d (error: %s)", lerr.Index, lerr.Error)
	}
	if lerr.Conflict != nil {
		t.Fatalf("query errors must not carry conflict, got %+v", lerr.Conflict)
	}
	for _, w := range want {
		if !strings.Contains(lerr.Error, w) {
			t.Fatalf("error = %q, want substring %q", lerr.Error, w)
		}
	}
}

// mustQueryFailNoResult 校验查询失败时只返回错误、不同时给出查询结果。
func mustQueryFailNoResult(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	res, lerr := store.QueryLine(line)
	if lerr == nil {
		t.Fatalf("expected query failure for %s, got result: %+v", line, res)
	}
	if res != nil {
		t.Fatalf("failed query must not return a result, got %+v", res)
	}
	return lerr
}

// 整条输入无法解析为唯一的完整 JSON 值时，即使查询对象前面的字段已经有类型错误或
// 未知字段，也必须报整行解析失败，不能把前面的字段问题当作最终原因。
func TestQueryWholeLineParseErrorBeatsFieldValidation(t *testing.T) {
	lines := []struct {
		name string
		line string
	}{
		// 对象未闭合：name 已有类型错误，但整行没有结束。
		{"truncated object", `{"op":"query","name":7,"start":0`},
		// 对象未闭合：未知字段已出现，但整行没有结束。
		{"truncated after unknown field", `{"op":"query","bogus":1,"name":"m"`},
		// 对象之后跟了第二个 JSON 值：前面的 name 已有类型错误。
		{"second JSON value", `{"op":"query","name":7,"start":0,"end":1} {"op":"query","name":"m","start":0,"end":1}`},
		// 对象之后跟了数组：前面的 start 已有类型错误。
		{"array after object", `{"op":"query","name":"m","start":"x","end":1} []`},
		// 对象之后跟了非空白垃圾：前面的未知字段已出现。
		{"trailing garbage", `{"op":"query","bogus":1,"name":"m","start":0,"end":1} oops`},
	}
	for _, tc := range lines {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			// 先写入基线数据，整行失败后必须原样可查。
			mustOK(t, store, `[{"name":"m","timestamp":1,"value":9}]`)

			lerr := mustQueryFailNoResult(t, store, tc.line)
			assertQueryError(t, lerr, "invalid JSON")

			// 整行失败不执行查询，此前写入的数据保持原样。
			qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
			if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 9 {
				t.Fatalf("baseline data changed after whole-line failure: %+v", qr.Series)
			}
		})
	}
}

// 结构完整的查询对象中，已出现的字段问题按书写顺序决定返回原因：
// 先出现未知字段就报告未知字段，先出现字段类型错误就报告该字段的类型错误；
// 只交换这两个字段的位置，错误原因随之改变。
func TestQueryFieldErrorReasonFollowsInputOrder(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		// 未知字段在前、start 类型错误在后 → 报告未知字段。
		{"unknown field first",
			`{"op":"query","name":"m","bogus":1,"start":"x","end":1}`, `unknown field "bogus"`},
		// start 类型错误在前、未知字段在后 → 报告 start 的类型错误。
		{"type error first",
			`{"op":"query","name":"m","start":"x","bogus":1,"end":1}`, `"start" must be a JSON number`},
		// 未知字段在 labels 类型错误之前 → 报告未知字段。
		{"unknown before labels type",
			`{"op":"query","name":"m","start":0,"end":1,"bogus":1,"labels":[]}`, `unknown field "bogus"`},
		// labels 类型错误在未知字段之前 → 报告 labels 的类型错误。
		{"labels type before unknown",
			`{"op":"query","name":"m","start":0,"end":1,"labels":[],"bogus":1}`, `"labels" must be an object`},
		// op 类型错误在未知字段之前 → 报告 op 的类型错误。
		{"op type before unknown",
			`{"op":7,"bogus":1,"name":"m","start":0,"end":1}`, `"op" must be a string`},
		// 未知字段在 op 类型错误之前 → 报告未知字段。
		{"unknown before op type",
			`{"bogus":1,"op":7,"name":"m","start":0,"end":1}`, `unknown field "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			lerr := mustQueryFailNoResult(t, store, tc.line)
			assertQueryError(t, lerr, tc.want)
		})
	}
}

// 首次值合法的已知字段再次出现时应报告重复字段，即使第二次的值类型错误；
// 字段名直接书写与转义后还原成同名也应如此，原因指出还原后的字段名。
func TestQueryDuplicateFieldBeatsSecondValueTypeError(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // 还原后的字段名
	}{
		// 第二次出现的 end 是字符串（类型也不合法）：仍报重复字段。
		{"literal duplicate, bad second value",
			`{"op":"query","name":"m","start":0,"end":1,"end":"x"}`, `"end"`},
		// 直接书写 end 后，再把首字母 e 用十六进制转义写一次且值类型错误：报重复字段 end。
		{"escaped duplicate, bad second value",
			`{"op":"query","name":"m","start":0,"end":1,"\u0065nd":"x"}`, `"end"`},
		// 转义形式在前（值合法），直接书写在后（值类型错误）：仍报重复字段 name。
		{"escaped first, bad literal second",
			`{"op":"query","\u006eame":"m","name":7,"start":0,"end":1}`, `"name"`},
		// 第二次出现的 op 是数字：报重复字段 op。
		{"duplicate op, bad second value",
			`{"op":"query","op":7,"name":"m","start":0,"end":1}`, `"op"`},
		// 第二次出现的 start 是布尔值：报重复字段 start。
		{"duplicate start, bad second value",
			`{"op":"query","name":"m","start":0,"start":true,"end":1}`, `"start"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			lerr := mustQueryFailNoResult(t, store, tc.line)
			assertQueryError(t, lerr, "duplicate field", tc.want)
		})
	}
}

// 缺少必填字段的错误只有在已出现字段都通过校验后才返回：缺 op 又带未知字段时先报
// 未知字段，去掉未知字段后才报缺少 op；已出现字段的类型错误、重复字段同理优先。
func TestQueryMissingRequiredReportedOnlyAfterPresentFieldsValid(t *testing.T) {
	store := NewMetricStore()

	// 缺 op 且带未知字段：先报未知字段。
	lerr := mustQueryFailNoResult(t, store, `{"name":"m","start":0,"end":1,"bogus":1}`)
	assertQueryError(t, lerr, `unknown field "bogus"`)

	// 去掉未知字段后，同一输入才报告缺少 op。
	lerr = mustQueryFailNoResult(t, store, `{"name":"m","start":0,"end":1}`)
	assertQueryError(t, lerr, `missing required field "op"`)

	// 缺 end 且 start 类型错误：先报类型错误。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","name":"m","start":"x"}`)
	assertQueryError(t, lerr, `"start" must be a JSON number`)

	// 修正类型错误后，才报告缺少 end。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","name":"m","start":0}`)
	assertQueryError(t, lerr, `missing required field "end"`)

	// 缺 name 且 end 重复出现：先报重复字段。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","start":0,"end":1,"end":2}`)
	assertQueryError(t, lerr, `duplicate field "end"`)
}

// 多个必填字段同时缺失时，依次按 op、name、start、end 选择第一个缺失项，
// 不受其他合法字段排列影响。
func TestQueryMissingRequiredPriorityOrder(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// 四个必填字段全缺：报 op。
		{`{}`, `"op"`},
		// 全缺且带合法 labels：仍报 op。
		{`{"labels":{}}`, `"op"`},
		// 缺 op，其余字段倒序书写：仍报 op。
		{`{"end":1,"start":0,"name":"m"}`, `"op"`},
		// 缺 name 与 op：报 op。
		{`{"start":0,"end":1}`, `"op"`},
		// 只缺 name：报 name。
		{`{"op":"query","start":0,"end":1}`, `"name"`},
		// 缺 name，labels 穿插其间：仍报 name。
		{`{"op":"query","end":1,"labels":{"h":"a"},"start":0}`, `"name"`},
		// 缺 start 与 end：报 start。
		{`{"op":"query","name":"m"}`, `"start"`},
		// 只缺 start：报 start。
		{`{"op":"query","name":"m","end":1}`, `"start"`},
		// 只缺 end：报 end。
		{`{"op":"query","name":"m","start":0}`, `"end"`},
	}
	for _, tc := range cases {
		store := NewMetricStore()
		lerr := mustQueryFailNoResult(t, store, tc.line)
		assertQueryError(t, lerr, "missing required field", tc.want)
	}
}

// 开始时间大于结束时间的错误要等字段校验和必填检查全部通过后才返回：
// 同一请求既有倒置区间又有字段问题时先指出字段问题，修正后再指出区间问题；
// 区间错误的原因必须指出对应的起止值。
func TestQueryRangeErrorReportedOnlyAfterFieldsValid(t *testing.T) {
	store := NewMetricStore()

	// 倒置区间 + 未知字段：先报未知字段。
	lerr := mustQueryFailNoResult(t, store, `{"op":"query","name":"m","start":5,"end":1,"bogus":1}`)
	assertQueryError(t, lerr, `unknown field "bogus"`)

	// 修正未知字段后，同一请求才报告倒置区间，并指出起止值。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","name":"m","start":5,"end":1}`)
	assertQueryError(t, lerr, "invalid range", `"start"`, `"end"`, "5", "1")

	// 倒置区间 + 字段类型错误：先报类型错误。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","name":"m","start":5,"end":"x"}`)
	assertQueryError(t, lerr, `"end" must be a JSON number`)

	// 倒置区间 + 重复字段：先报重复字段。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","name":"m","start":5,"end":1,"end":1}`)
	assertQueryError(t, lerr, `duplicate field "end"`)

	// 倒置区间 + 缺少必填字段：先报缺少的字段（区间两端齐全前不谈区间方向）。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","start":5,"end":1}`)
	assertQueryError(t, lerr, `missing required field "name"`)

	// 字段全部合法后，倒置区间才被报告；start == end 则是合法的单点区间。
	lerr = mustQueryFailNoResult(t, store, `{"op":"query","name":"m","start":-1,"end":-2}`)
	assertQueryError(t, lerr, "invalid range")
	mustQuery(t, store, `{"op":"query","name":"m","start":5,"end":5}`)
}

// 所有查询失败都只返回错误：不返回查询结果、不带 index 与 conflict；
// 先成功写入一条包含多个时间戳的序列，再遇到各类失败查询，之后用合法查询读取
// 相同范围，完整标签、点数和平均值应与失败前一致。
func TestQueryFailuresPreservePreviouslyWrittenData(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a","zone":"east"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a","zone":"east"}},
		{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a","zone":"east"}}
	]`)

	// 失败前的基线：完整标签、点数与平均值。
	baseline := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`)
	if len(baseline.Series) != 1 {
		t.Fatalf("baseline = %+v, want exactly one series", baseline.Series)
	}
	b0 := baseline.Series[0]
	if b0.Name != "cpu" || len(b0.Labels) != 2 || b0.Labels["host"] != "a" || b0.Labels["zone"] != "east" ||
		b0.Count != 3 || b0.Average != 4 {
		t.Fatalf("baseline series = %+v, want full labels, count=3, average=4", b0)
	}

	// 覆盖本文件保障的全部失败类别：整行解析、未知字段、类型错误、重复字段、
	// 缺少必填字段、倒置区间。
	failures := []struct {
		line string
		want string
	}{
		{`{"op":"query","name":"cpu","start":0,"end":4000} extra`, "invalid JSON"},
		{`{"op":"query","name":"cpu","start":0,"end":4000,"bogus":1}`, `unknown field "bogus"`},
		{`{"op":"query","name":"cpu","start":"x","end":4000}`, `"start" must be a JSON number`},
		{`{"op":"query","name":"cpu","start":0,"end":4000,"end":4000}`, `duplicate field "end"`},
		{`{"op":"query","name":"cpu","start":0}`, `missing required field "end"`},
		{`{"op":"query","name":"cpu","start":4000,"end":0}`, "invalid range"},
	}
	for _, f := range failures {
		lerr := mustQueryFailNoResult(t, store, f.line)
		assertQueryError(t, lerr, f.want)
	}

	// 失败之后用合法查询读取相同范围：完整标签、点数和平均值与失败前一致。
	after := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`)
	if len(after.Series) != 1 {
		t.Fatalf("query after failures = %+v, want exactly one series", after.Series)
	}
	a0 := after.Series[0]
	if a0.Name != b0.Name || len(a0.Labels) != len(b0.Labels) ||
		a0.Labels["host"] != b0.Labels["host"] || a0.Labels["zone"] != b0.Labels["zone"] ||
		a0.Count != b0.Count || a0.Average != b0.Average {
		t.Fatalf("query after failures = %+v, want identical to baseline %+v", a0, b0)
	}
}
