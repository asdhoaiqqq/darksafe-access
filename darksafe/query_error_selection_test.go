package darksafe

import (
	"strings"
	"testing"
)

// 本文件回归保障“同一条查询请求包含多个问题时”的错误选择规则，使用户能依据
// 返回的错误原因逐步修正输入、再看到剩余的问题（与写入侧的
// batch_error_selection_test.go 对应，覆盖查询对象）：
//
//   - 整条输入必须先能解析为唯一的完整 JSON 值：查询对象未闭合、对象之后
//     还有第二个值或非空白内容时，即使前面的字段已有类型错误，也只报整行
//     解析失败，不能把前面的字段问题当作最终原因。
//   - 结构完整的查询对象按字段原始书写顺序定原因：未知字段与字段类型错误
//     同时存在时先出现者获选，交换位置后原因随之改变。
//   - 首次值合法的已知字段再次出现一律报重复字段，即使第二次的值类型错误；
//     直接书写与转义后还原成同名（如 name）同样算重复。
//   - 缺少必填字段只在已出现字段都通过校验后报告；多个必填字段同时缺失时
//     按 op、name、start、end 的顺序选择，不受其他合法字段排列影响。
//   - start > end 的区间错误在字段校验与必填检查全部通过后才报告：同一请求
//     既有倒置区间又有字段问题时先指出字段问题，修正后再指出区间问题。
//   - 所有查询失败只返回错误：不返回查询结果，不带 index 与 conflict；
//     失败查询不改变存储，此前写入的数据在后续合法查询中原样可见。

// assertQueryError 校验查询级失败：无 index、无 conflict，原因包含 want 子串，
// 使不同的失败类别（未知字段、类型错误、重复字段、缺字段、区间倒置）可区分。
func assertQueryError(t *testing.T, lerr *LineError, want string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected query failure, got success")
	}
	if lerr.Index != 0 {
		t.Fatalf("query failure must not carry index, got index=%d (error: %s)", lerr.Index, lerr.Error)
	}
	if lerr.Conflict != nil {
		t.Fatalf("query failure must not carry a conflict, got %+v", lerr.Conflict)
	}
	if !strings.Contains(lerr.Error, want) {
		t.Fatalf("error = %q, want substring %q", lerr.Error, want)
	}
}

// 查询对象未闭合、对象之后还有第二个 JSON 值或非空白内容时，即使前面的字段
// 已经带类型错误或未知字段，也必须报整行解析失败（无 index、无 conflict），
// 而不是把前面的字段问题当作最终原因。
func TestQueryWholeLineParseErrorBeatsEarlierFieldErrors(t *testing.T) {
	lines := []struct {
		name string
		line string
		want string
	}{
		// 对象未闭合：name 已有类型错误，但整行无法解析。
		{"truncated object", `{"op":"query","name":7,"start":0`, "invalid JSON"},
		// 对象未闭合：未知字段已出现，但整行无法解析。
		{"truncated after unknown field", `{"op":"query","bogus":1,"name":"m"`, "invalid JSON"},
		// 对象之后跟着第二个 JSON 值：对象内 name 已有类型错误。
		{"second JSON value", `{"op":"query","name":7,"start":0,"end":1} {"op":"query"}`, "invalid JSON"},
		// 对象之后跟着第二个 JSON 值：对象内有未知字段。
		{"second value after unknown field", `{"op":"query","bogus":1,"name":"m","start":0,"end":1} []`, "invalid JSON"},
		// 对象之后跟着非空白垃圾：对象内 start 已有类型错误。
		{"trailing garbage", `{"op":"query","name":"m","start":"x","end":1} oops`, "invalid JSON"},
	}
	for _, tc := range lines {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			// 先写入基线数据，整行失败后必须原样保留、仍可查询。
			mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

			lerr := mustQueryFail(t, store, tc.line)
			assertWholeLineTextError(t, lerr, tc.want)

			res := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`)
			if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 2 {
				t.Fatalf("baseline data changed after whole-line failure: %+v", res.Series)
			}
		})
	}
}

// 结构完整的查询对象中，已出现的字段按原始书写顺序决定先报告哪个问题：
// 未知字段与字段类型错误同时存在时先出现者获选；只交换这两个字段的位置，
// 错误原因随之改变。
func TestQueryFieldErrorReasonFollowsInputOrder(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		// 未知字段在前、name 类型错误在后 → 报告未知字段。
		{"unknown field first",
			`{"op":"query","bogus":1,"name":7,"start":0,"end":1}`, `unknown field "bogus"`},
		// name 类型错误在前、未知字段在后 → 报告 name 的类型错误。
		{"type error first",
			`{"op":"query","name":7,"bogus":1,"start":0,"end":1}`, `"name" must be a string`},
		// start 类型错误在前、未知字段在后 → 报告 start 的类型错误。
		{"start type error first",
			`{"op":"query","name":"m","start":"x","bogus":1,"end":1}`, `"start" must be a JSON number`},
		// 未知字段在前、start 类型错误在后 → 报告未知字段。
		{"unknown before start type error",
			`{"op":"query","name":"m","bogus":1,"start":"x","end":1}`, `unknown field "bogus"`},
		// labels 类型错误在前、未知字段在后 → 报告 labels 的类型错误。
		{"labels type error first",
			`{"op":"query","name":"m","start":0,"end":1,"labels":[],"bogus":1}`, `"labels" must be an object`},
		// 未知字段在 labels 类型错误之前 → 报告未知字段。
		{"unknown before labels type error",
			`{"op":"query","name":"m","start":0,"end":1,"bogus":1,"labels":[]}`, `unknown field "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			lerr := mustQueryFail(t, store, tc.line)
			assertQueryError(t, lerr, tc.want)
		})
	}
}

// 首次值合法的已知字段再次出现时一律报告重复字段，即使第二次的值类型也
// 不合法；字段名直接书写与用 \uXXXX 转义还原成同名同样算重复，原因指出
// 还原后的字段名。
func TestQueryDuplicateFieldBeatsSecondValueTypeError(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // 还原后的字段名
	}{
		// 第二次出现的 start 是字符串（类型也不合法）：仍报重复字段。
		{"literal duplicate, bad second value",
			`{"op":"query","name":"m","start":0,"start":"x","end":1}`, `"start"`},
		// 直接书写 name 后，再把首字母 n 用十六进制转义写一次且值是数字：报重复字段 name。
		{"escaped duplicate, bad second value",
			`{"op":"query","name":"m","\u006eame":7,"start":0,"end":1}`, `"name"`},
		// 转义形式在前（值合法），直接书写在后（值类型错误）：仍报重复字段 name。
		{"escaped first, bad literal second",
			`{"op":"query","\u006eame":"m","name":7,"start":0,"end":1}`, `"name"`},
		// op 第二次出现是数字：报重复字段 op。
		{"duplicate op, bad second value",
			`{"op":"query","op":7,"name":"m","start":0,"end":1}`, `"op"`},
		// end 以转义形式重复出现且值是布尔：报重复字段 end。
		{"escaped duplicate end, bad second value",
			`{"op":"query","name":"m","start":0,"end":1,"\u0065nd":true}`, `"end"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			lerr := mustQueryFail(t, store, tc.line)
			assertQueryError(t, lerr, "duplicate field")
			assertQueryError(t, lerr, tc.want)
		})
	}
}

// 缺少必填字段的错误只有在已出现字段都通过校验后才返回：缺 name 同时带
// 未知字段、类型错误或重复字段时，先报告后者；修正已出现字段的问题后，
// 同一输入才报告缺少的必填字段。
func TestQueryMissingRequiredReportedOnlyAfterPresentFieldsValid(t *testing.T) {
	store := NewMetricStore()

	// 缺 name 且带未知字段：先报未知字段。
	lerr := mustQueryFail(t, store, `{"op":"query","start":0,"end":1,"bogus":1}`)
	assertQueryError(t, lerr, `unknown field "bogus"`)

	// 去掉未知字段后，同一输入才报告缺少 name。
	lerr = mustQueryFail(t, store, `{"op":"query","start":0,"end":1}`)
	assertQueryError(t, lerr, `missing required field "name"`)

	// 缺 name 且 start 类型错误：先报类型错误。
	lerr = mustQueryFail(t, store, `{"op":"query","start":"x","end":1}`)
	assertQueryError(t, lerr, `"start" must be a JSON number`)

	// 缺 name 且 start 重复出现：先报重复字段。
	lerr = mustQueryFail(t, store, `{"op":"query","start":0,"start":1,"end":2}`)
	assertQueryError(t, lerr, `duplicate field "start"`)

	// 缺 op 且 name 类型错误：先报类型错误，修正后才报缺 op。
	lerr = mustQueryFail(t, store, `{"name":7,"start":0,"end":1}`)
	assertQueryError(t, lerr, `"name" must be a string`)
	lerr = mustQueryFail(t, store, `{"name":"m","start":0,"end":1}`)
	assertQueryError(t, lerr, `missing required field "op"`)
}

// 多个必填字段同时缺失时，依次按 op、name、start、end 选择第一个缺失项，
// 不受其他合法字段（labels）及其排列位置影响。
func TestQueryMissingRequiredPriorityOrder(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// 四个必填字段全缺：报 op。
		{`{}`, `"op"`},
		{`{"labels":{}}`, `"op"`},
		// 缺 op 与 name：报 op（start/end 排列不影响）。
		{`{"start":0,"end":1}`, `"op"`},
		{`{"end":1,"start":0,"labels":{"host":"a"}}`, `"op"`},
		// 缺 name 与 start：报 name。
		{`{"op":"query","end":1}`, `"name"`},
		{`{"op":"query","end":1,"labels":{}}`, `"name"`},
		// 缺 start 与 end：报 start。
		{`{"op":"query","name":"m"}`, `"start"`},
		{`{"op":"query","name":"m","labels":{"host":"a"}}`, `"start"`},
		// 只缺 start：报 start。
		{`{"op":"query","name":"m","end":1}`, `"start"`},
		// 只缺 end，labels 穿插其间：仍报 end。
		{`{"op":"query","labels":{},"name":"m","start":0}`, `"end"`},
	}
	for _, tc := range cases {
		store := NewMetricStore()
		lerr := mustQueryFail(t, store, tc.line)
		assertQueryError(t, lerr, "missing required field")
		assertQueryError(t, lerr, tc.want)
	}
}

// start > end 的区间错误在字段校验与必填检查全部通过后才返回：同一请求既有
// 倒置区间又有字段问题时先指出字段问题；修正字段问题后，同一请求才指出区间
// 问题，且区间错误同时给出 start 与 end 的实际取值。
func TestQueryRangeErrorReportedOnlyAfterFieldsValid(t *testing.T) {
	store := NewMetricStore()

	// 倒置区间 + 未知字段：先报未知字段。
	lerr := mustQueryFail(t, store, `{"op":"query","name":"m","start":5,"end":1,"extra":1}`)
	assertQueryError(t, lerr, `unknown field "extra"`)

	// 倒置区间 + 缺 end 字段不可能倒置，这里缺 name：先报缺 name。
	lerr = mustQueryFail(t, store, `{"op":"query","start":5,"end":1}`)
	assertQueryError(t, lerr, `missing required field "name"`)

	// 倒置区间 + start 类型错误：先报类型错误。
	lerr = mustQueryFail(t, store, `{"op":"query","name":"m","start":"5","end":1}`)
	assertQueryError(t, lerr, `"start" must be a JSON number`)

	// 倒置区间 + 重复字段：先报重复字段。
	lerr = mustQueryFail(t, store, `{"op":"query","name":"m","start":5,"end":1,"end":2}`)
	assertQueryError(t, lerr, `duplicate field "end"`)

	// 字段全部合法后，同一倒置区间才报告区间错误，并指出 start 与 end 的值。
	lerr = mustQueryFail(t, store, `{"op":"query","name":"m","start":5,"end":1}`)
	assertQueryError(t, lerr, "invalid range")
	assertQueryError(t, lerr, `"start"`)
	assertQueryError(t, lerr, `"end"`)
	assertQueryError(t, lerr, "5 > 1")
}

// 所有查询失败都只返回错误：不返回查询结果，不带 index 与 conflict；
// 先写入一条包含多个时间戳的序列，各类失败查询之后再用合法查询读取相同
// 范围，完整标签、点数和平均值应与失败前一致。
func TestQueryFailuresReturnNoResultAndPreserveData(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a","zone":"z1"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a","zone":"z1"}},
		{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a","zone":"z1"}}
	]`)

	queryLine := `{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`
	assertBaseline := func() {
		t.Helper()
		res := mustQuery(t, store, queryLine)
		if len(res.Series) != 1 {
			t.Fatalf("series = %+v, want exactly one", res.Series)
		}
		s0 := res.Series[0]
		if s0.Name != "cpu" || len(s0.Labels) != 2 || s0.Labels["host"] != "a" || s0.Labels["zone"] != "z1" {
			t.Fatalf("series identity = %+v, want cpu{host=a,zone=z1}", s0)
		}
		if s0.Count != 3 || s0.Average != 4 {
			t.Fatalf("series = %+v, want count=3 average=4", s0)
		}
	}
	assertBaseline()

	// 覆盖本文件各类失败：整行解析、未知字段、类型错误、重复字段、
	// 缺必填字段、区间倒置。每一个都只返回错误，不改变存储。
	failures := []struct {
		line string
		want string
	}{
		{`{"op":"query","name":7,"start":0`, "invalid JSON"},
		{`{"op":"query","name":"cpu","start":0,"end":4000} {"op":"query"}`, "invalid JSON"},
		{`{"op":"query","bogus":1,"name":"cpu","start":0,"end":4000}`, `unknown field "bogus"`},
		{`{"op":"query","name":"cpu","start":"x","end":4000}`, `"start" must be a JSON number`},
		{`{"op":"query","name":"cpu","start":0,"start":1,"end":4000}`, `duplicate field "start"`},
		{`{"op":"query","name":"cpu","start":0}`, `missing required field "end"`},
		{`{"op":"query","name":"cpu","start":4000,"end":0}`, "invalid range"},
	}
	for _, f := range failures {
		lerr := mustQueryFail(t, store, f.line)
		assertQueryError(t, lerr, f.want)
	}

	// 失败查询之后，相同范围的合法查询结果与失败前完全一致。
	assertBaseline()
}
