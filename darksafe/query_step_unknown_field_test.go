package darksafe

import (
	"testing"
)

// 本文件回归保障查询对象中 step 字段的判定：step 只是 query_windows 的窗口宽度，
// 对完整且文本合法、op 只出现一次并指定受支持操作的查询，step 是否允许由该
// 操作决定，与字段书写位置和 step 的取值无关：
//
//   - query 与 query_points 携带 step 一律是未知字段：无论 step 写在 op 前
//     还是后，值是正整数、零、负数、小数、字符串、布尔、null、对象还是数组，
//     原因都只是 unknown field "step"，不提示把它改成合法窗口宽度；
//     删掉 step 后同一查询正常执行。
//   - 错误原因仍按字段在原对象中的先后选择：step 之前的字段有类型错误时先报
//     前面的字段；step 是最早的问题字段时只报 step，即使 op 写在它们之后；
//     step 的未知字段错误先于缺少必填项与区间倒置。
//   - query_windows 继续接受写在 op 前后的合法 step，值不合法时保留窗口宽度
//     的既有原因；op 缺失、类型错误、未知操作或重复出现时沿用既有处理；
//     对象未闭合仍先报整行解析失败。

// TestQueryStepUnknownRegardlessOfPositionAndValue query 与 query_points 中
// step 是未知字段：写在 op 前后、取任何 JSON 值（含合法窗口宽度）都只报
// unknown field "step"；每次失败后删掉 step 的同一查询正常执行、数据不变。
func TestQueryStepUnknownRegardlessOfPositionAndValue(t *testing.T) {
	values := []string{`1000`, `0`, `-5`, `1.5`, `"1000"`, `true`, `null`, `{}`, `[]`}
	for _, value := range values {
		t.Run("value="+value, func(t *testing.T) {
			store := NewMetricStore()
			mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2}]`)

			lines := []string{
				// step 写在 op 之前。
				`{"step":` + value + `,"op":"query","name":"cpu","start":0,"end":2000}`,
				// step 写在 op 之后。
				`{"op":"query","name":"cpu","start":0,"end":2000,"step":` + value + `}`,
				// query_points 同样两种位置。
				`{"step":` + value + `,"op":"query_points","name":"cpu","start":0,"end":2000}`,
				`{"op":"query_points","name":"cpu","start":0,"end":2000,"step":` + value + `}`,
			}
			for _, line := range lines {
				res, lerr := store.ProcessLine(line)
				if lerr == nil {
					t.Fatalf("line %s: expected failure, got result %+v", line, res)
				}
				if lerr.Error != `unknown field "step"` {
					t.Fatalf("line %s: error = %q, want exactly %q", line, lerr.Error, `unknown field "step"`)
				}
				if lerr.Index != 0 || lerr.Conflict != nil {
					t.Fatalf("line %s: query failure must not carry index/conflict: %+v", line, lerr)
				}
			}

			// 删掉 step 后原查询正常执行，此前写入的数据不受失败查询影响。
			res := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000}`)
			if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 2 {
				t.Fatalf("query without step = %+v, want count=1 average=2", res.Series)
			}
		})
	}
}

// TestQueryStepErrorFollowsFieldOrder step 的未知字段判定参与既有的按书写顺序
// 选择：step 之前的字段有问题先报前面的字段，step 最早则只报 step，
// 与 op 写在什么位置无关；step 的未知字段错误先于缺必填项与区间倒置。
func TestQueryStepErrorFollowsFieldOrder(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		// name 类型错误写在 step 之前：先报 name，即使 op 在两者之后。
		{"name error before step",
			`{"name":7,"step":1000,"op":"query","start":0,"end":2000}`, `"name" must be a string`},
		// 同一对问题字段交换位置：先报 step，即使 op 在两者之后。
		{"step before name error",
			`{"step":1000,"name":7,"op":"query","start":0,"end":2000}`, `unknown field "step"`},
		// step 为零且写在最前：仍只报未知字段，不按窗口宽度值报错。
		{"zero step earliest",
			`{"step":0,"name":7,"op":"query","start":0,"end":2000}`, `unknown field "step"`},
		// op 写在最后：step 仍是最早的问题字段。
		{"op written last",
			`{"step":1,"name":"cpu","start":0,"end":2000,"op":"query"}`, `unknown field "step"`},
		// step 的未知字段错误先于缺少必填项（缺 start/end）。
		{"step beats missing required",
			`{"step":1,"op":"query","name":"cpu"}`, `unknown field "step"`},
		// step 的未知字段错误先于区间倒置。
		{"step beats inverted range",
			`{"step":1,"op":"query","name":"cpu","start":5,"end":1}`, `unknown field "step"`},
		// op 字段名以 \uXXXX 转义书写：还原后仍确定操作，step 是未知字段。
		{"escaped op key",
			`{"step":0,"\u006fp":"query","name":"cpu","start":0,"end":2000}`, `unknown field "step"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			lerr := mustQueryFail(t, store, tc.line)
			assertQueryError(t, lerr, tc.want)
		})
	}
}

// TestQueryStepWindowsAndOpFallback 窗口查询与 op 异常时的既有处理不变：
// query_windows 接受写在 op 前后的合法 step、值不合法时保留窗口宽度原因；
// op 缺失、类型错误、未知操作或重复出现时 step 仍按既有逐字段规则处理；
// 对象未闭合仍先报整行解析失败。
func TestQueryStepWindowsAndOpFallback(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2}]`)

	// query_windows：step 写在 op 前，合法值成功。
	res := mustQueryWindows(t, store, `{"step":1000,"op":"query_windows","name":"cpu","start":0,"end":2000}`)
	if len(res.Series) != 1 || len(res.Series[0].Windows) != 1 {
		t.Fatalf("query_windows with step before op = %+v, want one window", res.Series)
	}

	cases := []struct {
		name string
		line string
		want string
	}{
		// query_windows：step 写在 op 前但值不合法，保留窗口宽度原因。
		{"windows zero step before op",
			`{"step":0,"op":"query_windows","name":"cpu","start":0,"end":2000}`, `field "step": must be greater than zero`},
		{"windows string step before op",
			`{"step":"x","op":"query_windows","name":"cpu","start":0,"end":2000}`, `field "step" must be a JSON number`},
		// op 缺失：step 仍按窗口宽度校验，值不合法报值、合法则报缺 op。
		{"op missing, zero step",
			`{"step":0,"name":"cpu","start":0,"end":2000}`, `field "step": must be greater than zero`},
		{"op missing, valid step",
			`{"step":5,"name":"cpu","start":0,"end":2000}`, `missing required field "op"`},
		// op 类型错误：op 在前先报 op。
		{"op wrong type",
			`{"op":7,"step":0,"name":"cpu","start":0,"end":2000}`, `field "op" must be a string`},
		// 未知操作：op 在前先报未知操作。
		{"unknown op",
			`{"op":"ping","step":0,"name":"cpu","start":0,"end":2000}`, `unknown op "ping"`},
		// 未知操作且 step 在前：op 未确定，step 按窗口宽度校验。
		{"unknown op after step",
			`{"step":0,"op":"ping","name":"cpu","start":0,"end":2000}`, `field "step": must be greater than zero`},
		// op 重复出现：报重复字段。
		{"duplicate op",
			`{"op":"query","op":"query","name":"cpu","start":0,"end":2000,"step":1}`, `duplicate field "op"`},
		// 对象未闭合：整行解析失败先于 step 判定。
		{"truncated object",
			`{"step":0,"op":"query","name":"cpu","start":0`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lerr := mustQueryFail(t, store, tc.line)
			assertQueryError(t, lerr, tc.want)
		})
	}

	// 全部失败之后，已写入数据在合法查询中原样可见。
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("baseline data changed after failures: %+v", qr.Series)
	}
}
