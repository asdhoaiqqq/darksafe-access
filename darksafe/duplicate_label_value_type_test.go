package darksafe

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件回归“重复标签键被第二次值的类型错误掩盖”的问题。
//
// 同一 labels 对象内，某个非空标签键第一次出现且值为合法字符串后，再次出现这个
// 键就必须返回重复标签键错误，原因给出 JSON 转义还原后的键名；第二次对应任何
// 合法 JSON 值（数字、布尔、null、对象、数组，哪怕两个字符串值完全相同）都遵循
// 这条规则。只有键第一次出现时值就不是字符串，才报告那次值的类型错误。
// 判重范围仍限于同一个标签对象；整行残缺或编码损坏仍报整行解析错误。
// 写入失败时 index 指向含重复键的采样点且不带 conflict、整批新增点不提交；
// 查询失败时不带 index 与 conflict，也不返回查询结果。

// jsonHexKey 把键的首字符改写成 JSON 十六进制转义形式（host → 反斜杠 u0068ost）。
// 反斜杠用 rune(92) 构造，使本文件源码里不必直接出现反斜杠转义序列。
func jsonHexKey(s string) string {
	return string(rune(92)) + "u" + fmt.Sprintf("%04x", s[0]) + s[1:]
}

// assertLabelValueTypeError 校验“标签值必须是字符串”类失败：index 指向采样点，
// 原因包含对应标签键，且不能被误报成重复标签键。
func assertLabelValueTypeError(t *testing.T, lerr *LineError, wantIndex int, wantKey string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected label-value-type failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	want := fmt.Sprintf(`value of label %q must be a string`, wantKey)
	if !strings.Contains(lerr.Error, want) {
		t.Fatalf("error = %q, want substring %q", lerr.Error, want)
	}
	if strings.Contains(lerr.Error, "duplicate label key") {
		t.Fatalf("first-occurrence type error must not be reported as duplicate: %q", lerr.Error)
	}
	if lerr.Conflict != nil {
		t.Fatalf("label validation failure must not carry a conflict, got %+v", lerr.Conflict)
	}
}

// 写入路径：合法字符串值的标签键再次出现时，第二次值的任何类型都必须报重复键，
// 不能降级成“标签值必须是字符串”；直接书写与十六进制转义书写互换先后亦然。
func TestDuplicateLabelKeyBeatsSecondValueTypeOnWrite(t *testing.T) {
	eh := jsonHexKey("host") // 首字母转义书写的 host
	lines := []string{
		// 第二次值是数字、布尔、null。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":7}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":true}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":null}}]`,
		// 第二次值是对象或数组（含非空嵌套内容）。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":{}}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":{"z":1}}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":[]}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":["x"]}}]`,
		// 两个字符串值相同也仍然是重复键。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":"a"}}]`,
		// 直接书写在前、转义书写在后，第二次值为数字。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + eh + `":7}}]`,
		// 直接在前、转义在后，第二次值为 null。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + eh + `":null}}]`,
		// 转义书写在前（值合法）、直接书写在后，第二次值为对象。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + eh + `":"a","host":{}}}]`,
		// 转义在前、直接在后，第二次值为数组。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + eh + `":"a","host":[1]}}]`,
		// 转义在前、直接在后，第二次值为布尔。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + eh + `":"a","host":true}}]`,
	}
	for _, line := range lines {
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		assertDupLabelError(t, lerr, 1, "host")
		if strings.Contains(lerr.Error, "must be a string") {
			t.Fatalf("line %s: duplicate key masked by second value type: %q", line, lerr.Error)
		}
		// 任何重复键失败都不得写入数据。
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("line %s: failed batch mutated store, series=%d", line, got)
		}
	}

	// 另一个键名：直接书写后再以 z 的十六进制转义出现且值是数字，原因给出还原键名 zone。
	store := NewMetricStore()
	_, lerr := store.IngestLine(`[{"name":"m","timestamp":1,"value":1,"labels":{"zone":"x","` + jsonHexKey("zone") + `":9}}]`)
	assertDupLabelError(t, lerr, 1, "zone")
}

// 键第一次出现时值就不是字符串，仍报告那次出现的值类型错误；
// 同一标签对象内更早出现的其他键问题按输入次序决定原因。
func TestFirstOccurrenceLabelValueTypeAndEarlierErrors(t *testing.T) {
	eh := jsonHexKey("host")
	firstBad := []string{
		// 第一次值非字符串、第二次值是字符串或另一个非字符串：报第一次的类型错误。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":7,"host":"a"}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":7,"host":8}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":true}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":null,"host":"a"}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":{},"host":"a"}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":[]}}]`,
		// 转义形式第一次出现时值就非法（数字）：仍是那次的类型错误，不是重复。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + eh + `":7,"host":"a"}}]`,
		// 转义第一次出现、值为数组：仍报那次的值类型错误。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + eh + `":[]}}]`,
	}
	for _, line := range firstBad {
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		assertLabelValueTypeError(t, lerr, 1, "host")
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("line %s: failed batch mutated store, series=%d", line, got)
		}
	}

	store := NewMetricStore()
	// 同一采样点更早出现的字段错误（name 类型错误）先于 labels 内的重复键获选。
	_, lerr := store.IngestLine(`[{"name":7,"timestamp":1,"value":1,"labels":{"host":"a","host":7}}]`)
	assertSampleError(t, lerr, 1, `field "name" must be a string`)

	// 标签对象内另一个键 k 的值类型错误出现在 host 第二次出现之前：先报 k。
	_, lerr = store.IngestLine(`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","k":7,"host":9}}]`)
	assertLabelValueTypeError(t, lerr, 1, "k")

	// 交换次序后重复的 host 出现在前：报重复标签键 host。
	_, lerr = store.IngestLine(`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":9,"k":7}}]`)
	assertDupLabelError(t, lerr, 1, "host")

	// 空标签键出现在重复键之前：先报空键。
	_, lerr = store.IngestLine(`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","":"x","host":9}}]`)
	assertSampleError(t, lerr, 1, "label keys must be non-empty strings")

	// 所有失败都未写入数据。
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("failed batches must commit nothing, series=%d", got)
	}
}

// 写入批次的 index 指向含重复标签键的采样点（而非批次内更早的合法新增点），
// 不附带 conflict，整批新增点（包括更早的合法新时间戳点）一律不提交；
// 此前已成功写入的数据保持原来的点数与均值。
func TestDuplicateLabelKeyFailsWholeBatchAtIndex(t *testing.T) {
	store := NewMetricStore()
	// 基线：cpu/host=a 在 1000ms 有一个值为 2 的点。
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	// 第二个采样点的第二次标签值覆盖各类非字符串 JSON 类型，错误位置始终是 2。
	for _, second := range []string{
		"7", "true", "false", "null", "{}", "[]", `{"z":1}`, `["x"]`,
	} {
		line := `[
			{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
			{"name":"cpu","timestamp":3000,"value":5,"labels":{"host":"a","host":` + second + `}}
		]`
		lerr := mustFail(t, store, line)
		assertDupLabelError(t, lerr, 2, "host")
		if strings.Contains(lerr.Error, "must be a string") {
			t.Fatalf("second value %s masked the duplicate key: %q", second, lerr.Error)
		}
	}

	// 转义书写与直接书写互换：转义在前、直接在后且第二次值为数字，仍是 index 2。
	eh := jsonHexKey("host")
	lerr := mustFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":5,"labels":{"`+eh+`":"a","host":9}}
	]`)
	assertDupLabelError(t, lerr, 2, "host")

	// 整批回滚：2000ms 的新增点没有提交，基线仍是唯一的一个点（count=1、average=2）。
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 {
		t.Fatalf("snapshot = %+v, want only the baseline series", snap.Series)
	}
	pts := snap.Series[0].Points
	if len(pts) != 1 || pts[0].Timestamp != 1000 || pts[0].Value != 2 {
		t.Fatalf("points = %+v, want only (1000,2)", pts)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("query after rollback = %+v, want count=1 average=2", qr.Series)
	}
}

// 查询路径：标签键在合法字符串值之后再次出现时，第二次值的类型不得掩盖重复；
// 查询失败不带 index、不带 conflict、不返回结果，也不改变存储。
func TestDuplicateLabelKeyBeatsSecondValueTypeOnQuery(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a","zone":"z"}}]`)
	eh := jsonHexKey("host")

	bad := []string{
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a","host":7}}`,
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a","host":true}}`,
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a","host":null}}`,
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a","host":{}}}`,
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a","host":[]}}`,
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a","host":{"z":1}}}`,
		// 直接书写在前、转义书写在后，第二次值为数字。
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a","` + eh + `":7}}`,
		// 转义书写在前（值合法）、直接书写在后且值为数组。
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"` + eh + `":"a","host":[]}}`,
	}
	for _, q := range bad {
		res, lerr := store.QueryLine(q)
		if res != nil {
			t.Fatalf("query %s must return no result, got %+v", q, res)
		}
		if lerr == nil {
			t.Fatalf("query %s must fail on duplicate label key", q)
		}
		if lerr.Index != 0 || lerr.Conflict != nil {
			t.Fatalf("query %s: must carry neither index nor conflict, got %+v", q, lerr)
		}
		if !strings.Contains(lerr.Error, `duplicate label key "host"`) {
			t.Fatalf("query %s: error = %q, want duplicate label key host", q, lerr.Error)
		}
		if strings.Contains(lerr.Error, "must be a string") {
			t.Fatalf("query %s: duplicate masked by second value type: %q", q, lerr.Error)
		}
	}

	// 第一次出现时值就不是字符串：仍是那次值的类型错误，而非重复。
	lerr := mustQueryFail(t, store,
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":7,"host":"a"}}`)
	assertQueryError(t, lerr, `value of label "host" must be a string`)
	// 转义形式第一次出现时值就非法：同样报那次的类型错误。
	lerr = mustQueryFail(t, store,
		`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"`+eh+`":7,"host":"a"}}`)
	assertQueryError(t, lerr, `value of label "host" must be a string`)

	// 失败查询不改变存储，标签子集查询照常命中原来的一个点。
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("storage/read path must be unaffected, got %+v", qr.Series)
	}
}

// 整行 JSON 残缺或文本编码损坏时，即使标签对象内含重复键（第二次值还不是字符串），
// 也仍报整行解析错误，不被重复标签键错误替代：无 index、无 conflict，不提交任何点。
func TestDuplicateLabelKeyDoesNotReplaceWholeLineFailure(t *testing.T) {
	eh := jsonHexKey("host")
	cases := []struct {
		name string
		line string
		want string
	}{
		// 数组残缺未闭合：标签对象内第二次值是数字。
		{"truncated write array",
			`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":7}}`,
			"invalid JSON"},
		// 查询对象残缺未闭合：标签对象内第二次值是对象。
		{"truncated query object",
			`{"op":"query","name":"m","start":0,"end":1,"labels":{"host":"a","host":{}}`,
			"invalid JSON"},
		// 非法 UTF-8 字节出现在较后采样点；较早采样点的标签含重复键（第二次值为数字）。
		{"invalid UTF-8 after duplicate label",
			string([]byte(`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + eh + `":7}},{"name":"m` + "\xff" + `","timestamp":2,"value":2}]`)),
			"UTF-8"},
		// 未配对代理项转义出现在较后采样点；较早采样点的标签含重复键（第二次值为数组）。
		{"unpaired surrogate after duplicate label",
			`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + eh + `":[]}},{"name":"m\uD800","timestamp":2,"value":2}]`,
			"surrogate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			// 查询对象残缺用 QueryLine 校验，其余用写入入口；两类整行失败都无 index。
			var lerr *LineError
			if strings.Contains(tc.line, `"op":"query"`) {
				_, lerr = store.QueryLine(tc.line)
			} else {
				_, lerr = store.IngestLine(tc.line)
			}
			assertWholeLineTextError(t, lerr, tc.want)
			if strings.Contains(lerr.Error, "duplicate label key") {
				t.Fatalf("whole-line failure must not be replaced by duplicate key: %q", lerr.Error)
			}
			if got := len(mustOK(t, store, `[]`).Series); got != 0 {
				t.Fatalf("whole-line failure must commit nothing, series=%d", got)
			}
		})
	}
}

// 判重范围与既有合法行为不变：不同采样点使用同名标签、标签名与采样点字段同名
// 都合法；标签子集查询与同值采样去重继续保持。
func TestLabelDupScopingSubsetQueryAndDedupUnaffected(t *testing.T) {
	store := NewMetricStore()

	// 不同采样点各自带 host 标签：合法，两个新增点。
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1,"labels":{"host":"a"}},
		{"name":"m","timestamp":2,"value":2,"labels":{"host":"a"}}
	]`)
	if res.Added != 2 || res.Duplicates != 0 {
		t.Fatalf("same label key in separate samples must be legal, got %+v", res)
	}

	// 采样点字段名 name/value 与其标签键同名：分属不同对象，合法。
	res = mustOK(t, store, `[{"name":"m","timestamp":3,"value":3,"labels":{"name":"x","value":"y"}}]`)
	if res.Added != 1 {
		t.Fatalf("sample field sharing a name with a label key must be legal, got %+v", res)
	}

	// 同序列同时间戳同值重提：仍按重复采样点成功忽略，计入 duplicates。
	res = mustOK(t, store, `[{"name":"m","timestamp":1,"value":1.0,"labels":{"host":"a"}}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("equal re-submission must stay a duplicate, got %+v", res)
	}

	// 标签子集查询照常命中前两个点：count=2、average=1.5。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 1.5 {
		t.Fatalf("subset query must keep working, got %+v", qr.Series)
	}
}
