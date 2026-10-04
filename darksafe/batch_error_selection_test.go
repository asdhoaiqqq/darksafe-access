package darksafe

import (
	"strings"
	"testing"
)

// 本文件回归保障“同一请求包含多个问题时”的错误选择规则，使用户能依据返回的
// 错误原因与位置逐步修正输入：
//
//   - 整行无法解析（残缺数组、数组后还有第二个 JSON 值、非法 UTF-8、未配对
//     代理项转义）时只报整行解析错误，不带 index 与 conflict；即使数组内较早的
//     采样点已有未知字段或错误类型，也不能降级为采样点校验错误。
//   - 整行可解析时按数组先后报告第一个失败的采样点（index 从 1 开始）；同一
//     采样点内的字段问题按输入次序定原因；已知字段第二次出现一律报重复字段；
//     只有已出现字段都合法时才按 name、timestamp、value 的顺序报缺少必填字段。
//   - 字段校验失败绝不附带 conflict；失败批次前面的合法新增点不提交，此前
//     成功写入的数据保持原值；较晚采样点的问题不覆盖已选定的错误。

// assertSampleError 校验采样点级失败：index 指向 wantIndex（从 1 开始），
// 原因包含 want 子串，且不附带 conflict。
func assertSampleError(t *testing.T, lerr *LineError, wantIndex int, want string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected sample-level failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	if !strings.Contains(lerr.Error, want) {
		t.Fatalf("error = %q, want substring %q", lerr.Error, want)
	}
	if lerr.Conflict != nil {
		t.Fatalf("sample validation failure must not carry a conflict, got %+v", lerr.Conflict)
	}
}

// assertStoreHasOnly 校验存储中恰好只有 name 指标的一个点（ts,value），
// 用于确认失败批次没有留下任何新增点、此前写入的数据保持原值。
func assertStoreHasOnly(t *testing.T, store *MetricStore, name string, ts int64, value float64) {
	t.Helper()
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || snap.Series[0].Name != name {
		t.Fatalf("snapshot = %+v, want exactly series %q", snap.Series, name)
	}
	pts := snap.Series[0].Points
	if len(pts) != 1 || pts[0].Timestamp != ts || pts[0].Value != value {
		t.Fatalf("points = %+v, want exactly (%d,%v)", pts, ts, value)
	}
}

// 整行文本损坏或结构不完整时，即使数组内较早的采样点已经有未知字段或错误类型，
// 也必须报整行解析错误（无 index、无 conflict），而不是采样点校验错误。
func TestWholeLineParseErrorBeatsEarlierSampleValidation(t *testing.T) {
	// 每行都在“较早采样点已带字段问题”的前提下叠加一种整行级损坏。
	lines := []struct {
		name string
		line string
		want string // 错误原因子串，区分不同整行失败类别
	}{
		// 数组本身残缺：第一个采样点 name 类型错误，但整行未闭合。
		{"truncated array", `[{"name":7,"timestamp":1,"value":1}`, "invalid JSON"},
		// 数组后跟着第二个 JSON 值：数组内第一个采样点同样带类型错误。
		{"second JSON value", `[{"name":7,"timestamp":1,"value":1}] [{"name":"m","timestamp":2,"value":2}]`, "invalid JSON"},
		// 数组后跟着非 JSON 垃圾：第一个采样点带未知字段。
		{"trailing garbage", `[{"name":"m","timestamp":1,"value":1,"bogus":1}] oops`, "invalid JSON"},
		// 非法 UTF-8 字节出现在较后采样点，较早采样点带未知字段。
		{"invalid UTF-8", string([]byte(`[{"name":"m","timestamp":1,"value":1,"bogus":1},{"name":"m` + "\xff" + `","timestamp":2,"value":2}]`)), "UTF-8"},
		// 非法 UTF-8 字节出现在较后采样点，较早采样点带类型错误。
		{"invalid UTF-8 after type error", string([]byte(`[{"name":"m","timestamp":"x","value":1},{"name":"m` + "\xff" + `","timestamp":2,"value":2}]`)), "UTF-8"},
		// 未配对代理项转义出现在较后采样点，较早采样点带未知字段。
		{"unpaired surrogate", `[{"name":"m","timestamp":1,"value":1,"bogus":1},{"name":"m\uD800","timestamp":2,"value":2}]`, "surrogate"},
		// 未配对代理项转义出现在较后采样点，较早采样点带类型错误。
		{"unpaired surrogate after type error", `[{"name":"m","timestamp":"x","value":1},{"name":"m\uD800","timestamp":2,"value":2}]`, "surrogate"},
		// 未配对代理项转义出现在第一个采样点，较后采样点带未知字段：
		// 同样只能是整行错误，而不是较后采样点的校验错误。
		{"unpaired surrogate first", `[{"name":"m\uD800","timestamp":1,"value":1},{"name":"m","timestamp":2,"value":2,"bogus":1}]`, "surrogate"},
	}
	for _, tc := range lines {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			// 先写入一条基线数据，整行失败后必须原样保留。
			mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

			_, lerr := store.IngestLine(tc.line)
			assertWholeLineTextError(t, lerr, tc.want)

			// 整行失败：不写入任何点（包括行内合法的新增点），基线数据不变。
			assertStoreHasOnly(t, store, "old", 1, 9)
		})
	}
}

// 整行可以解析时，按数组中的先后位置报告第一个失败的采样点（index 从 1 开始）；
// 较晚采样点的其他问题不能覆盖已经选定的错误位置和原因。
func TestFirstFailingSampleWinsByArrayPosition(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 三个采样点各自带不同问题：必须报告最早的 index 1 及其自身原因
	// （timestamp 类型错误），不能被 index 2 的未知字段或 index 3 的缺字段覆盖。
	lerr := mustFail(t, store, `[
		{"name":"a","timestamp":"x","value":1},
		{"name":"b","timestamp":2,"value":2,"bogus":1},
		{"timestamp":3,"value":3}
	]`)
	assertSampleError(t, lerr, 1, `"timestamp" must be a JSON number`)

	// 第一个采样点是合法新增点、后两个各自失败：报告 index 2 的未知字段，
	// 且 index 1 的合法新增点不得留在存储中。
	lerr = mustFail(t, store, `[
		{"name":"new","timestamp":1,"value":1},
		{"name":"b","timestamp":2,"value":2,"bogus":1},
		{"name":7,"timestamp":3,"value":3}
	]`)
	assertSampleError(t, lerr, 2, `unknown field "bogus"`)

	// 失败批次未留下任何新增点，基线数据保持原值。
	assertStoreHasOnly(t, store, "old", 1, 9)
}

// 同一采样点内，已出现的字段问题按输入中的次序决定返回原因：
// 先出现未知字段就报告未知字段，先出现字段类型错误就报告该字段的类型错误；
// 交换这两个错误字段的位置后原因随之改变，但仍指向同一个采样点。
func TestFieldErrorReasonFollowsInputOrder(t *testing.T) {
	cases := []struct {
		name   string
		sample string
		want   string
	}{
		// 未知字段在前、类型错误在后 → 报告未知字段。
		{"unknown field first", `{"name":"m","bogus":1,"timestamp":"x","value":1}`, `unknown field "bogus"`},
		// 类型错误在前、未知字段在后 → 报告该字段的类型错误。
		{"type error first", `{"name":"m","timestamp":"x","bogus":1,"value":1}`, `"timestamp" must be a JSON number`},
		// 未知字段在 labels 类型错误之前 → 报告未知字段。
		{"unknown before labels type", `{"name":"m","timestamp":1,"bogus":1,"labels":[],"value":1}`, `unknown field "bogus"`},
		// labels 类型错误在未知字段之前 → 报告 labels 的类型错误。
		{"labels type before unknown", `{"name":"m","timestamp":1,"labels":[],"bogus":1,"value":1}`, `"labels" must be an object`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			// 出错采样点放在 index 2：原因随字段次序变化，位置始终指向它。
			line := `[{"name":"ok","timestamp":1,"value":1},` + tc.sample + `]`
			lerr := mustFail(t, store, line)
			assertSampleError(t, lerr, 2, tc.want)
			// 前面的合法新增点不得提交。
			if got := len(mustOK(t, store, `[]`).Series); got != 0 {
				t.Fatalf("failed batch must commit nothing, series=%d", got)
			}
		})
	}
}

// 一个已知字段第二次出现时一律报告重复字段，即使第二次的值类型也不正确；
// 直接书写与 JSON 转义还原后相同的字段名仍算重复，原因指出还原后的字段名。
func TestDuplicateFieldBeatsSecondValueTypeError(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // 还原后的字段名
	}{
		// 第二次出现的 timestamp 是字符串（类型也不合法）：仍报重复字段。
		{"literal duplicate, bad second value",
			`[{"name":"m","timestamp":1,"timestamp":"x","value":1}]`, `"timestamp"`},
		// 直接书写 name 后，再把首字母 n 用十六进制转义写一次且值是数字：报重复字段 name。
		{"escaped duplicate, bad second value",
			`[{"name":"m","\u006eame":7,"timestamp":1,"value":1}]`, `"name"`},
		// 转义形式在前（值合法），直接书写在后（值类型错误）：仍报重复字段 name。
		{"escaped first, bad literal second",
			`[{"\u006eame":"m","name":7,"timestamp":1,"value":1}]`, `"name"`},
		// value 第二次出现是布尔值：报重复字段 value。
		{"duplicate value field, bad second value",
			`[{"name":"m","timestamp":1,"value":1,"value":true}]`, `"value"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			lerr := mustFail(t, store, tc.line)
			assertSampleError(t, lerr, 1, "duplicate field")
			assertSampleError(t, lerr, 1, tc.want)
			if got := len(mustOK(t, store, `[]`).Series); got != 0 {
				t.Fatalf("failed batch must commit nothing, series=%d", got)
			}
		})
	}
}

// 只有已出现的字段都合法时，才报告缺少必填字段：缺少 name 又带未知字段时先报
// 未知字段，去掉未知字段后才报缺少 name；已出现字段的类型错误、重复字段同理
// 优先于缺字段报告。
func TestMissingRequiredReportedOnlyAfterPresentFieldsValid(t *testing.T) {
	store := NewMetricStore()

	// 缺 name 且带未知字段：先报未知字段。
	lerr := mustFail(t, store, `[{"timestamp":1,"value":1,"bogus":1}]`)
	assertSampleError(t, lerr, 1, `unknown field "bogus"`)

	// 去掉未知字段后，同一输入才报告缺少 name。
	lerr = mustFail(t, store, `[{"timestamp":1,"value":1}]`)
	assertSampleError(t, lerr, 1, `missing required field "name"`)

	// 缺 name 且 timestamp 类型错误：先报类型错误。
	lerr = mustFail(t, store, `[{"timestamp":"x","value":1}]`)
	assertSampleError(t, lerr, 1, `"timestamp" must be a JSON number`)

	// 缺 name 且 value 重复出现：先报重复字段。
	lerr = mustFail(t, store, `[{"timestamp":1,"value":1,"value":2}]`)
	assertSampleError(t, lerr, 1, `duplicate field "value"`)

	// 全部失败均不得写入数据。
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("failed batches must commit nothing, series=%d", got)
	}
}

// 同时缺少多个必填字段时，优先顺序固定为 name、timestamp、value，
// 不受其他字段排列影响。
func TestMissingRequiredPriorityOrder(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// 三个必填字段全缺：报 name。
		{`[{"labels":{}}]`, `"name"`},
		// 缺 name 与 timestamp：报 name（value 在前不影响）。
		{`[{"value":1}]`, `"name"`},
		// 缺 name 与 timestamp，字段排列不同：仍报 name。
		{`[{"value":1,"labels":{"h":"a"}}]`, `"name"`},
		// 缺 timestamp 与 value：报 timestamp。
		{`[{"name":"m"}]`, `"timestamp"`},
		// 缺 timestamp 与 value，labels 穿插其间：仍报 timestamp。
		{`[{"name":"m","labels":{}}]`, `"timestamp"`},
		// 只缺 value：报 value。
		{`[{"name":"m","timestamp":1}]`, `"value"`},
		// 只缺 name，timestamp/value 倒序书写：仍报 name。
		{`[{"value":1,"timestamp":1}]`, `"name"`},
	}
	for _, tc := range cases {
		store := NewMetricStore()
		lerr := mustFail(t, store, tc.line)
		assertSampleError(t, lerr, 1, "missing required field")
		assertSampleError(t, lerr, 1, tc.want)
	}
}

// 字段校验失败不能附带 conflict，也不能被解释成采样值冲突：
// 采样点身份与已存数据相同、值也不同，但采样点本身带字段问题时，
// 报的是字段校验错误而不是冲突。
func TestValidationFailureNeverBecomesConflict(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":5}]`)

	// 同序列同时间戳、不同值，但带未知字段：报未知字段，不带 conflict。
	lerr := mustFail(t, store, `[{"name":"m","timestamp":1,"value":9,"bogus":1}]`)
	assertSampleError(t, lerr, 1, `unknown field "bogus"`)

	// 同序列同时间戳、值类型错误：报类型错误，不带 conflict。
	lerr = mustFail(t, store, `[{"name":"m","timestamp":1,"value":"bad"}]`)
	assertSampleError(t, lerr, 1, `"value" must be a JSON number`)

	// 同序列同时间戳、字段重复：报重复字段，不带 conflict。
	lerr = mustFail(t, store, `[{"name":"m","timestamp":1,"value":9,"value":9}]`)
	assertSampleError(t, lerr, 1, `duplicate field "value"`)

	// 已存数据保持原值 5，未被任何失败输入改动。
	assertStoreHasOnly(t, store, "m", 1, 5)

	// 对照：字段全部合法、仅值不同，才是真正的采样值冲突（带 conflict）。
	lerr = mustFail(t, store, `[{"name":"m","timestamp":1,"value":9}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 9 {
		t.Fatalf("genuine value conflict must still report conflict, got %+v", lerr)
	}
	assertStoreHasOnly(t, store, "m", 1, 5)
}

// 在各类失败之间，合法批次与空批次仍按现有规则成功处理：
// 失败不改变存储，后续合法写入正常新增、空批次正常返回快照。
func TestValidAndEmptyBatchesStillSucceedBetweenFailures(t *testing.T) {
	store := NewMetricStore()

	// 合法批次成功。
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":2}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("valid batch = %+v, want added=1", res)
	}

	// 整行解析失败与采样点校验失败交替出现。
	_, lerr := store.IngestLine(`[{"name":"m","timestamp":1,"value":2}] ]`)
	assertWholeLineTextError(t, lerr, "invalid JSON")
	assertSampleError(t, mustFail(t, store, `[{"name":"m","timestamp":2,"value":1,"bogus":1}]`), 1, `unknown field "bogus"`)

	// 空批次仍成功，快照只含最初写入的一个点。
	res = mustOK(t, store, `[]`)
	if res.Added != 0 || res.Duplicates != 0 || len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("empty batch after failures = %+v", res)
	}

	// 合法批次继续正常新增；与已存点同值同时间戳仍计 duplicates。
	res = mustOK(t, store, `[{"name":"m","timestamp":2,"value":4},{"name":"m","timestamp":1,"value":2.0}]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("valid batch after failures = %+v, want added=1 duplicates=1", res)
	}
	if pts := mustOK(t, store, `[]`).Series[0].Points; len(pts) != 2 {
		t.Fatalf("points = %+v, want the two committed points", pts)
	}
}
