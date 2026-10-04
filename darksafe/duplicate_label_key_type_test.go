package darksafe

import (
	"strings"
	"testing"
)

// 回归保障：labels 中某个非空键第一次出现且值为合法字符串后，再次出现该键，
// 无论第二次的值是什么 JSON 类型（数字、布尔、null、对象、数组、字符串），
// 都必须报告重复标签键并指出转义还原后的键名——不能让第二次值的类型
// （“标签值必须是字符串”）掩盖键重复。这与采样点字段、查询字段的判重行为一致：
// parseStrictObject 也是先判重再读值。判重只在同一个标签对象内进行。

// jsonEscHost 是 JSON 文本里的字面 `host`（用 \x5c 构造反斜杠，
// 使键的首字母以十六进制转义书写），用于验证判重以转义还原后的文字为准。
var jsonEscHost = "\x5cu0068ost"

// assertLabelTypeError 校验“标签值类型”失败：原因必须指出值必须是字符串，
// 而不能误报成重复标签键。
func assertLabelTypeError(t *testing.T, lerr *LineError, wantIndex int, wantKey string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected label value type failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	if !strings.Contains(lerr.Error, "must be a string") || !strings.Contains(lerr.Error, wantKey) {
		t.Fatalf("error = %q, want value-of-label %q must be a string", lerr.Error, wantKey)
	}
	if strings.Contains(lerr.Error, "duplicate label key") {
		t.Fatalf("first-occurrence type error must not be reported as duplicate: %q", lerr.Error)
	}
	if lerr.Conflict != nil {
		t.Fatalf("label validation errors must not carry a conflict, got %+v", lerr.Conflict)
	}
}

func TestDuplicateLabelKeyBeatsSecondValueTypeOnIngest(t *testing.T) {
	// 第一次 host 是合法字符串；第二次 host 的值覆盖全部合法 JSON 值类型，
	// 包括两个字符串完全相同的情形，全部必须报重复标签键而不是类型错误。
	secondValues := []string{
		`1`,       // 数字
		`true`,    // 布尔
		`null`,    // null
		`{"x":1}`, // 对象
		`[1,2]`,   // 数组
		`"a"`,     // 与第一个值相同的字符串
		`"b"`,     // 不同字符串
		`""`,      // 空字符串（空值合法，但键仍重复）
		`3.5`,     // 小数
		`-2`,      // 负整数
	}
	for _, second := range secondValues {
		line := `[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","host":` + second + `}}]`
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		assertDupLabelError(t, lerr, 1, "host")
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("second value %s: failed batch mutated store, series=%d", second, got)
		}
	}
}

func TestDuplicateLabelKeyBeatsSecondValueTypeOnQuery(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":3,"labels":{"host":"a"}}]`)

	for _, second := range []string{`1`, `true`, `null`, `{"x":1}`, `[1]`, `"a"`, `"b"`} {
		q := `{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a","host":` + second + `}}`
		_, lerr := store.QueryLine(q)
		if lerr == nil {
			t.Fatalf("query with second value %s must fail on duplicate label key", second)
		}
		if lerr.Index != 0 || lerr.Conflict != nil {
			t.Fatalf("query error must carry neither index nor conflict, got %+v", lerr)
		}
		if !strings.Contains(lerr.Error, `duplicate label key "host"`) {
			t.Fatalf("query second value %s: error = %q, want duplicate label key host", second, lerr.Error)
		}
	}

	// 失败查询不返回结果也不影响存储。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 3 {
		t.Fatalf("storage must be unaffected by failed queries, got %+v", qr.Series)
	}
}

func TestDuplicateLabelKeyAfterUnescapingBeatsNonStringValue(t *testing.T) {
	// host 与首字母十六进制转义的同名键判定为重复；第二次的值即便不是字符串，
	// 仍报重复标签键，原因指出还原后的键名。交换直接书写与转义书写的先后不改变原因。
	cases := []string{
		// 直接书写在前（合法字符串值），转义同名键在后且值非字符串：报重复。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + jsonEscHost + `":1}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + jsonEscHost + `":true}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + jsonEscHost + `":null}}]`,
		// 转义同名键在前（合法字符串值），直接书写在后且值非字符串：仍报重复。
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + jsonEscHost + `":"a",` + `"host":1}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + jsonEscHost + `":"a",` + `"host":[]}}]`,
	}
	for _, line := range cases {
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		assertDupLabelError(t, lerr, 1, "host")
	}
}

func TestFirstOccurrenceNonStringValueStillTypeError(t *testing.T) {
	// 键第一次出现时值就不是字符串：仍报告那次值的类型错误，不提前判重。
	store := NewMetricStore()
	_, lerr := store.IngestLine(`[{"name":"m","timestamp":1,"value":1,"labels":{"host":1,"host":"a"}}]`)
	assertLabelTypeError(t, lerr, 1, "host")

	// 转义形式先出现且其值非字符串，同样是第一次出现的值类型错误。
	line := `[{"name":"m","timestamp":2,"value":1,"labels":{"` + jsonEscHost + `":true,"host":"a"}}]`
	_, lerr = store.IngestLine(line)
	assertLabelTypeError(t, lerr, 1, "host")

	// 只出现一次、值为对象：类型错误，且无任何数据写入。
	_, lerr = store.IngestLine(`[{"name":"m","timestamp":3,"value":1,"labels":{"host":{"a":1}}}]`)
	assertLabelTypeError(t, lerr, 1, "host")
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("type failures must not mutate store, series=%d", got)
	}
}

func TestEarlierLabelErrorKeepsInputOrderPrecedence(t *testing.T) {
	store := NewMetricStore()

	// 空键在重复键之前出现：空键错误先发生，仍报告空键，不被后面的重复替代。
	_, lerr := store.IngestLine(`[{"name":"m","timestamp":1,"value":1,"labels":{"":"a","host":"b","host":"c"}}]`)
	if lerr == nil || !strings.Contains(lerr.Error, "label keys must be non-empty strings") {
		t.Fatalf("want empty-key error (it occurs first), got %+v", lerr)
	}

	// 第一个 host 的值类型错误先于第二个 host 出现：报告第一次的值类型错误。
	_, lerr = store.IngestLine(`[{"name":"m","timestamp":2,"value":1,"labels":{"host":1,"host":2}}]`)
	assertLabelTypeError(t, lerr, 1, "host")
}

func TestDuplicateLabelKeyRollsBackWholeBatchKeepsBaseline(t *testing.T) {
	store := NewMetricStore()
	// 基线：cpu{host=a} 在 1000 有一个值为 2 的点。
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	// 本批：先放入同一序列时间戳 2000、值 4 的新增点，再放入标签键重复的点。
	_, lerr := store.IngestLine(`[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":3000,"value":5,"labels":{"host":"a","host":1}}
	]`)
	assertDupLabelError(t, lerr, 2, "host")

	// 整批新增点都不提交：查询仍是原来的一个点，count=1、average=2。
	qr := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("rolled-back batch must leave baseline alone, got %+v", qr.Series)
	}
}

func TestDuplicateLabelScopingAndLegitimateFlowsUnchanged(t *testing.T) {
	store := NewMetricStore()

	// 不同采样点使用同名标签：合法。
	res := mustOK(t, store, `[
		{"name":"q","timestamp":1,"value":1,"labels":{"host":"a"}},
		{"name":"q","timestamp":2,"value":2,"labels":{"host":"a"}}
	]`)
	if res.Added != 2 {
		t.Fatalf("same label key in separate samples must be legal, got %+v", res)
	}

	// 标签名与采样点字段同名：合法。
	res = mustOK(t, store, `[{"name":"m","timestamp":1,"value":1,"labels":{"name":"x","value":"y"}}]`)
	if res.Added != 1 {
		t.Fatalf("label keys sharing sample field names must be legal, got %+v", res)
	}

	// 标签子集查询正常。
	qr := mustQuery(t, store, `{"op":"query","name":"q","start":0,"end":10,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 {
		t.Fatalf("subset label query must still work, got %+v", qr.Series)
	}

	// 同值采样去重继续保持。
	res = mustOK(t, store, `[{"name":"q","timestamp":1,"value":1.0,"labels":{"host":"a"}}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("equal re-submission must stay a duplicate, got %+v", res)
	}
}
