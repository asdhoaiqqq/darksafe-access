package darksafe

import (
	"strings"
	"testing"
)

// 本文件回归保障“同一请求包含多个问题时”的错误选择规则，让用户能依据返回的
// 错误原因与位置逐步修正输入：
//
//   - 整行无法解析（不是完整 JSON 数组、数组后还有第二个 JSON 值、非法 UTF-8、
//     未配对代理项转义）一律是整行解析错误：不带 index、不带 conflict；即使数组
//     内较早的采样点已有未知字段或类型错误，也不能改报成采样点校验错误。
//   - 整行可以解析时，按数组先后位置报告第一个失败的采样点（index 从 1 开始）；
//     较晚采样点的其他问题不能覆盖已选定的位置与原因。
//   - 同一采样点内的字段问题按输入次序决定原因：先出现未知字段就报未知字段，
//     先出现类型错误就报类型错误；已知字段第二次出现一律报重复字段（即使第二次
//     的值类型也不对，直接书写与转义还原后同名都算重复，原因指出还原后的字段名）。
//   - 只有已出现的字段都合法时才报告缺少必填字段，同时缺多个时固定按
//     name、timestamp、value 的优先顺序，与其他字段排列无关。
//   - 字段校验失败绝不附带 conflict，也不会被解释成采样值冲突。
//   - 失败批次不写入任何数据（其前面的合法新增点也不保留），此前成功写入的数据
//     保持原值；合法批次与空批次仍按现有规则成功。

// assertSampleError 校验采样点级失败：index 指向 wantIndex（从 1 开始），
// 原因包含 want 子串，且绝不附带 conflict。
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
		t.Fatalf("sample validation error must not carry a conflict, got %+v", lerr.Conflict)
	}
}

// assertStoreOnly 校验存储中恰好只有基线序列 old（一个点，值为 9）：
// 失败批次的任何点都未提交，此前成功写入的数据保持原值。
func assertStoreOnly(t *testing.T, store *MetricStore) {
	t.Helper()
	res := mustOK(t, store, `[]`)
	if len(res.Series) != 1 || res.Series[0].Name != "old" {
		t.Fatalf("store = %+v, want only the baseline series old", res.Series)
	}
	pts := res.Series[0].Points
	if len(pts) != 1 || pts[0].Timestamp != 1 || pts[0].Value != 9 {
		t.Fatalf("baseline points = %+v, want the single point (1, 9)", pts)
	}
}

// 整行不是完整 JSON 数组、或数组后跟着第二个 JSON 值时，即使数组内较早的
// 采样点已有未知字段或类型错误，也必须返回整行解析错误（无 index、无 conflict）。
func TestWholeLineParseErrorBeatsSampleValidation(t *testing.T) {
	lines := []string{
		// 数组被截断：第一个采样点还带未知字段，仍不是采样点校验错误。
		`[{"name":"m","timestamp":1,"value":1,"zone":"x"}`,
		// 数组被截断在第二个采样点中间：第一个采样点类型错误也不改报。
		`[{"name":7,"timestamp":1,"value":1},{"name":"m","timestamp":2,"value":2}`,
		// 数组完整但后面跟着第二个 JSON 值：第一个采样点的类型错误不改报。
		`[{"name":7,"timestamp":1,"value":1}] []`,
		// 数组后跟着一个查询对象：第一个采样点的未知字段不改报。
		`[{"name":"m","timestamp":1,"value":1,"zone":"x"}] {"op":"query"}`,
		// 数组后跟着非 JSON 垃圾：第一个采样点的未知字段不改报。
		`[{"name":"m","timestamp":1,"value":1,"zone":"x"}] trailing`,
	}
	for _, line := range lines {
		store := NewMetricStore()
		mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)
		lerr := mustFail(t, store, line)
		if lerr.Index != 0 {
			t.Errorf("line %s: whole-line parse error must not carry index, got %d", line, lerr.Index)
		}
		if lerr.Conflict != nil {
			t.Errorf("line %s: whole-line parse error must not carry conflict, got %+v", line, lerr.Conflict)
		}
		if !strings.Contains(lerr.Error, "invalid JSON") {
			t.Errorf("line %s: error = %q, want invalid JSON reason", line, lerr.Error)
		}
		assertStoreOnly(t, store)
	}
}

// 数组内较早的采样点已有未知字段或类型错误，但较晚的采样点含非法 UTF-8 字节
// 或未配对代理项转义：整行文本校验先于采样点校验，必须返回整行解析错误，
// 且原因仍区分“UTF-8 不合法”与“代理项转义不合法”。
func TestCorruptTextBeatsEarlierSampleValidation(t *testing.T) {
	surrogateLines := []string{
		// 第一个采样点有未知字段，第二个采样点名称含孤立高代理项。
		`[{"name":"m","timestamp":1,"value":1,"zone":"x"},{"name":"bad\uD800","timestamp":2,"value":2}]`,
		// 第一个采样点 name 类型错误，第二个采样点标签值含孤立低代理项。
		`[{"name":7,"timestamp":1,"value":1},{"name":"m","timestamp":2,"value":2,"labels":{"k":"v\uDC00"}}]`,
	}
	for _, line := range surrogateLines {
		store := NewMetricStore()
		mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)
		_, lerr := store.IngestLine(line)
		assertWholeLineTextError(t, lerr, "surrogate")
		assertStoreOnly(t, store)
	}

	utf8Lines := [][]byte{
		// 第一个采样点有未知字段，第二个采样点名称夹非法字节 0xFF。
		[]byte(`[{"name":"m","timestamp":1,"value":1,"zone":"x"},{"name":"bad` + "\xff" + `","timestamp":2,"value":2}]`),
		// 第一个采样点 timestamp 类型错误，第二个采样点标签键含非法字节。
		[]byte(`[{"name":"m","timestamp":"x","value":1},{"name":"m","timestamp":2,"value":2,"labels":{"` + "\xfe" + `":"v"}}]`),
	}
	for _, raw := range utf8Lines {
		store := NewMetricStore()
		mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)
		_, lerr := store.IngestLine(string(raw))
		assertWholeLineTextError(t, lerr, "UTF-8")
		assertStoreOnly(t, store)
	}
}

// 整行可以解析时，按数组先后位置报告第一个失败的采样点（index 从 1 开始）；
// 较晚采样点的其他问题不能覆盖已选定的位置与原因；失败点之前的合法新增点
// 也不提交，此前成功写入的数据保持原值。
func TestFirstFailingSamplePositionWins(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 第一个点是合法新增，第二个点有未知字段，第三个点有类型错误：
	// 报告第二个点的未知字段，第三个点的问题不覆盖它。
	lerr := mustFail(t, store, `[
		{"name":"new","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":2,"zone":"x"},
		{"name":7,"timestamp":3,"value":3}
	]`)
	assertSampleError(t, lerr, 2, `unknown field "zone"`)

	// 第一个点就有类型错误，第二个点的未知字段不出现：报告第一个点。
	lerr = mustFail(t, store, `[
		{"name":7,"timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":2,"zone":"x"}
	]`)
	assertSampleError(t, lerr, 1, `field "name" must be a string`)

	// 两个失败批次都没有提交任何点：合法新增点 new 不在存储中，基线保持原值。
	assertStoreOnly(t, store)
}

// 同一采样点内先出现的字段问题决定返回原因：先出现未知字段就报未知字段，
// 先出现类型错误就报类型错误；交换两个错误字段的位置后原因随之改变，
// 但仍指向同一个采样点。
func TestFieldOrderDecidesErrorReason(t *testing.T) {
	// 类型错误在前、未知字段在后：报告 timestamp 的类型错误。
	store := NewMetricStore()
	lerr := mustFail(t, store, `[{"name":"m","timestamp":"x","zone":1,"value":1}]`)
	assertSampleError(t, lerr, 1, `field "timestamp" must be a JSON number`)

	// 交换位置：未知字段在前、类型错误在后，报告未知字段；仍指向同一个采样点。
	lerr = mustFail(t, store, `[{"name":"m","zone":1,"timestamp":"x","value":1}]`)
	assertSampleError(t, lerr, 1, `unknown field "zone"`)

	// 同样的次序规则作用于较后的采样点：位置由数组决定，原因由字段次序决定。
	lerr = mustFail(t, store, `[
		{"name":"ok","timestamp":1,"value":1},
		{"name":"m","timestamp":"x","zone":1,"value":1}
	]`)
	assertSampleError(t, lerr, 2, `field "timestamp" must be a JSON number`)
	lerr = mustFail(t, store, `[
		{"name":"ok","timestamp":1,"value":1},
		{"name":"m","zone":1,"timestamp":"x","value":1}
	]`)
	assertSampleError(t, lerr, 2, `unknown field "zone"`)

	// 所有失败批次都不写入数据。
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("failed batches must write nothing, series=%d", got)
	}
}

// 已知字段第二次出现时一律报告重复字段，即使第二次的值类型也不正确；
// 直接书写与 JSON 转义还原后相同的字段名仍算重复，原因指出还原后的字段名。
func TestDuplicateFieldBeatsSecondValueTypeError(t *testing.T) {
	cases := []struct {
		line  string
		field string
	}{
		// 第二个 name 的值是数字（类型也不对）：仍报重复字段。
		{`[{"name":"m","name":7,"timestamp":1,"value":1}]`, "name"},
		// 第二个 name 把首字母写成 n 的十六进制转义、值类型也不对：报还原后的 name。
		{`[{"name":"m","\u006eame":7,"timestamp":1,"value":1}]`, "name"},
		// 转义形式在前、直接书写在后，第二个值类型不对：仍报重复字段。
		{`[{"\u006eame":"m","name":7,"timestamp":1,"value":1}]`, "name"},
		// 第二个 timestamp 把首字母写成 t 的十六进制转义、值是字符串：报还原后的 timestamp。
		{`[{"timestamp":1,"\u0074imestamp":"x","name":"m","value":1}]`, "timestamp"},
		// 第二个 value 的值是字符串：报重复字段 value。
		{`[{"name":"m","timestamp":1,"value":1,"value":"x"}]`, "value"},
		// 第二个 labels 的值不是对象：报重复字段 labels。
		{`[{"name":"m","timestamp":1,"value":1,"labels":{},"labels":[]}]`, "labels"},
	}
	for _, tc := range cases {
		store := NewMetricStore()
		mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)
		lerr := mustFail(t, store, tc.line)
		assertSampleError(t, lerr, 1, `duplicate field "`+tc.field+`"`)
		assertStoreOnly(t, store)
	}
}

// 只有已出现的字段都合法时才报告缺少必填字段；同时缺少多个字段时优先顺序
// 固定为 name、timestamp、value，不受其他字段排列影响。
func TestMissingRequiredFieldPriority(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// 同时缺 name 与 timestamp：报 name，与字段排列无关。
		{`[{"value":1}]`, `missing required field "name"`},
		{`[{"value":1,"labels":{}}]`, `missing required field "name"`},
		// 同时缺 name 与 timestamp（两种排列）：都报 name。
		{`[{"value":1,"labels":{"h":"a"}}]`, `missing required field "name"`},
		{`[{"labels":{"h":"a"},"value":1}]`, `missing required field "name"`},
		// 缺 name 与 value：报 name。
		{`[{"timestamp":1}]`, `missing required field "name"`},
		// 缺 timestamp 与 value：报 timestamp。
		{`[{"name":"m"}]`, `missing required field "timestamp"`},
		{`[{"name":"m","labels":{}}]`, `missing required field "timestamp"`},
		// 只缺 value：报 value。
		{`[{"name":"m","timestamp":1}]`, `missing required field "value"`},
	}
	for _, tc := range cases {
		store := NewMetricStore()
		lerr := mustFail(t, store, tc.line)
		assertSampleError(t, lerr, 1, tc.want)
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("line %s: failed batch must write nothing, series=%d", tc.line, got)
		}
	}

	// 缺少 name 又带未知字段：先报未知字段；去掉未知字段后才报缺少 name。
	store := NewMetricStore()
	lerr := mustFail(t, store, `[{"timestamp":1,"value":1,"zone":"x"}]`)
	assertSampleError(t, lerr, 1, `unknown field "zone"`)
	lerr = mustFail(t, store, `[{"timestamp":1,"value":1}]`)
	assertSampleError(t, lerr, 1, `missing required field "name"`)

	// 缺少 name 且已出现的 timestamp 类型错误：先报类型错误，修正后才报缺 name。
	lerr = mustFail(t, store, `[{"timestamp":"x","value":1}]`)
	assertSampleError(t, lerr, 1, `field "timestamp" must be a JSON number`)
	lerr = mustFail(t, store, `[{"timestamp":1,"value":1}]`)
	assertSampleError(t, lerr, 1, `missing required field "name"`)
}

// 字段校验失败不能附带 conflict，也不能被解释成采样值冲突：
// 即使该采样点指向已存在值的序列与时间戳，字段问题仍只是字段问题。
func TestFieldValidationErrorNeverBecomesConflict(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":5}]`)

	// 与已存点同序列同时间戳，但带未知字段：报未知字段，无 conflict。
	lerr := mustFail(t, store, `[{"name":"m","timestamp":1,"value":2,"zone":"x"}]`)
	assertSampleError(t, lerr, 1, `unknown field "zone"`)

	// 与已存点同序列同时间戳，但 value 类型错误：报类型错误，无 conflict。
	lerr = mustFail(t, store, `[{"name":"m","timestamp":1,"value":"2"}]`)
	assertSampleError(t, lerr, 1, `field "value" must be a JSON number`)

	// 与已存点同序列同时间戳，但字段重复：报重复字段，无 conflict。
	lerr = mustFail(t, store, `[{"name":"m","timestamp":1,"value":2,"value":2}]`)
	assertSampleError(t, lerr, 1, `duplicate field "value"`)

	// 已存值保持 5；真正的采样值冲突（字段全部合法、值不同）才带 conflict。
	res := mustOK(t, store, `[]`)
	if pts := res.Series[0].Points; len(pts) != 1 || pts[0].Value != 5 {
		t.Fatalf("stored value changed after field-validation failures: %+v", pts)
	}
	lerr = mustFail(t, store, `[{"name":"m","timestamp":1,"value":2}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 2 {
		t.Fatalf("clean resubmission with a different value must be a value conflict, got %+v", lerr)
	}
}

// 多问题失败请求之后，合法批次与空批次仍按现有规则成功处理，
// 失败批次之前的合法新增点不会残留在存储中。
func TestValidAndEmptyBatchesStillSucceedAfterFailures(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 交织的失败批次：整行解析错误、采样点校验错误、字段重复。
	mustFail(t, store, `[{"name":"a","timestamp":1,"value":1}] []`)
	mustFail(t, store, `[{"name":"b","timestamp":1,"value":1},{"name":"b","timestamp":2,"zone":1,"value":2}]`)
	mustFail(t, store, `[{"name":"c","name":"c","timestamp":1,"value":1}]`)
	assertStoreOnly(t, store)

	// 合法批次照常写入并计入重复。
	res := mustOK(t, store, `[
		{"name":"new","timestamp":1,"value":1},
		{"name":"old","timestamp":1,"value":9.0}
	]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("valid batch after failures = added %d duplicates %d, want 1/1", res.Added, res.Duplicates)
	}

	// 空批次照常成功并列出当前全部序列。
	res = mustOK(t, store, `[]`)
	if res.Added != 0 || res.Duplicates != 0 || len(res.Series) != 2 {
		t.Fatalf("empty batch after failures = %+v, want 2 series", res)
	}
}

// ProcessLine 入口与 IngestLine 入口对多问题请求选择相同的错误：
// 整行问题报整行错误，采样点问题带从 1 开始的 index，结果结构保持兼容。
func TestProcessLineSelectsSameErrorAsIngestLine(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 整行问题：数组后还有第二个 JSON 值。
	_, lerr := store.ProcessLine(`[{"name":7,"timestamp":1,"value":1}] []`)
	if lerr == nil || lerr.Index != 0 || lerr.Conflict != nil ||
		!strings.Contains(lerr.Error, "invalid JSON") {
		t.Fatalf("ProcessLine whole-line error = %+v", lerr)
	}

	// 采样点问题：第一个失败采样点的第一个字段问题。
	_, lerr = store.ProcessLine(`[
		{"name":"new","timestamp":1,"value":1},
		{"name":"m","zone":1,"timestamp":"x","value":1}
	]`)
	assertSampleError(t, lerr, 2, `unknown field "zone"`)

	assertStoreOnly(t, store)
}
