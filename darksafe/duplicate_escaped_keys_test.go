package darksafe

import (
	"strings"
	"testing"
)

// 回归保障：同一对象内的重复字段/标签键，必须以 JSON 转义还原后的字符为准判定。
// 例如指标名字段直接书写一次、其首字母改用小写 n 的十六进制转义再书写一次，
// 还原后两个键都是 name，即重复指标名字段；labels 中 host 的首字母改用小写 h
// 的十六进制转义再写一次，还原后都是 host，即重复标签键。即使两个值完全相同也
// 必须失败，值不同时也不能静默取其中一个或变成采样值冲突。重复判断只在同一个
// 对象内进行：不同采样点各自的字段、采样点字段名与其标签键同名都是合法输入。

// assertDupFieldError 校验“重复字段”类失败：index 指向出错采样点（写入路径），
// 原因包含字段名，且绝不附带 conflict（不能被解释成采样值冲突）。
func assertDupFieldError(t *testing.T, lerr *LineError, wantIndex int, wantField string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected duplicate-field failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	if !strings.Contains(lerr.Error, "duplicate field") || !strings.Contains(lerr.Error, wantField) {
		t.Fatalf("error = %q, want duplicate field %q", lerr.Error, wantField)
	}
	if lerr.Conflict != nil {
		t.Fatalf("duplicate field must not carry a conflict, got %+v", lerr.Conflict)
	}
}

// assertDupLabelError 校验“重复标签键”类失败：原因包含还原后的标签键，无 conflict。
func assertDupLabelError(t *testing.T, lerr *LineError, wantIndex int, wantKey string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected duplicate-label-key failure, got success")
	}
	if lerr.Index != wantIndex {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, wantIndex, lerr.Error)
	}
	if !strings.Contains(lerr.Error, "duplicate label key") || !strings.Contains(lerr.Error, wantKey) {
		t.Fatalf("error = %q, want duplicate label key %q", lerr.Error, wantKey)
	}
	if lerr.Conflict != nil {
		t.Fatalf("duplicate label key must not carry a conflict, got %+v", lerr.Conflict)
	}
}

func TestDuplicateSampleFieldDetectedAfterJSONUnescaping(t *testing.T) {
	// 每个已知字段都直接书写一次、再用首字母的十六进制转义书写一次；
	// 字段名比较的是转义还原后的字符，所以这些用例都必须判重。
	sameValue := []string{
		`[{"name":"m","\u006eame":"m","timestamp":1,"value":1}]`,
		`[{"name":"m","timestamp":1,"\u0074imestamp":1,"value":1}]`,
		`[{"name":"m","timestamp":1,"value":1,"\u0076alue":1}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{},"\u006cabels":{}}]`,
	}
	diffValue := []string{
		`[{"name":"m","\u006eame":"other","timestamp":1,"value":1}]`,
		`[{"name":"m","timestamp":1,"\u0074imestamp":2,"value":1}]`,
		`[{"name":"m","timestamp":1,"value":1,"\u0076alue":2}]`,
	}
	wantField := []string{"name", "timestamp", "value", "labels", "name", "timestamp", "value"}
	for i, line := range append(sameValue, diffValue...) {
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		assertDupFieldError(t, lerr, 1, wantField[i])
		// 任何重复字段失败都不得写入数据。
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("line %s: failed batch mutated store, series=%d", line, got)
		}
	}

	// 转义形式在前、直接书写在后，同样必须识别为重复。
	store := NewMetricStore()
	_, lerr := store.IngestLine(`[{"\u006eame":"m","timestamp":1,"value":1,"name":"m"}]`)
	assertDupFieldError(t, lerr, 1, "name")
}

func TestDuplicateLabelKeyDetectedAfterJSONUnescaping(t *testing.T) {
	// 两个 host 键一个直接书写、一个把首字母写成十六进制转义，还原后是同一个键。
	cases := []string{
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","\u0068ost":"a"}}]`, // 两个标签值相同
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","\u0068ost":"b"}}]`, // 两个标签值不同
		`[{"name":"m","timestamp":1,"value":1,"labels":{"\u0068ost":"a","host":"a"}}]`, // 转义形式在前
	}
	for _, line := range cases {
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		assertDupLabelError(t, lerr, 1, "host")
		// 不能被解释成两个标签，也不能产生任何采样点。
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("line %s: failed batch mutated store, series=%d", line, got)
		}
	}

	// 转义还原后撞上另一个普通标签键也要报告该还原键（z 的十六进制转义）。
	store := NewMetricStore()
	_, lerr := store.IngestLine(`[{"name":"m","timestamp":1,"value":1,"labels":{"zone":"x","\u007aone":"y"}}]`)
	assertDupLabelError(t, lerr, 1, "zone")
}

func TestEscapedDuplicateAtLaterSampleFailsWholeBatch(t *testing.T) {
	store := NewMetricStore()
	// 先成功写入一条基线数据，后续失败必须保留它原来的数量与均值。
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":4}]`)

	// 本批前两个点是合法新增，第三个点字段重复：整批失败，index 从 1 开始指向 3。
	_, lerr := store.IngestLine(`[
		{"name":"new","timestamp":1,"value":1},
		{"name":"new","timestamp":2,"value":2},
		{"name":"new","\u006eame":"new","timestamp":3,"value":3}
	]`)
	assertDupFieldError(t, lerr, 3, "name")

	// 重复标签键出现在较后的采样点：index 2，整批失败。
	_, lerr = store.IngestLine(`[
		{"name":"net","timestamp":1,"value":1},
		{"name":"net","timestamp":2,"value":2,"labels":{"host":"a","\u0068ost":"b"}}
	]`)
	assertDupLabelError(t, lerr, 2, "host")

	// 本批新增点一律未提交，此前成功写入的基线仍可查询到原来的数量与均值。
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 1 || snap.Series[0].Name != "old" {
		t.Fatalf("rolled-back batches must add no series, snapshot=%+v", snap.Series)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"old","start":0,"end":100}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 4 {
		t.Fatalf("earlier data must remain count=1 average=4, got %+v", qr.Series)
	}
}

func TestEscapedFieldAppearingOnceIsAccepted(t *testing.T) {
	store := NewMetricStore()

	// 只出现一次的转义字段名与直接书写同名同义，可以照常写入。
	res := mustOK(t, store, `[{"\u006eame":"m","timestamp":1,"value":7}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("escaped-once fields should write, got %+v", res)
	}
	// 与直接书写的 name/timestamp/value 是同一个点 → 重复并计入 duplicates。
	res = mustOK(t, store, `[{"name":"m","timestamp":1,"value":7.0}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("escaped-once identity must equal literal fields, got %+v", res)
	}

	// 标签键只转义出现一次：与直接书写的标签身份相同。
	res = mustOK(t, store, `[{"name":"m","timestamp":2,"value":1,"labels":{"\u0068ost":"a"}}]`)
	if res.Added != 1 {
		t.Fatalf("escaped-once label key should write, got %+v", res)
	}
	res = mustOK(t, store, `[{"name":"m","timestamp":2,"value":1.0,"labels":{"host":"a"}}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("escaped-once label must equal literal label, got %+v", res)
	}

	// 查询对象里只出现一次的转义字段照常工作，并能命中上面写入的点。
	qr := mustQuery(t, store, `{"op":"query","\u006eame":"m","start":0,"end":2}`)
	if len(qr.Series) != 2 {
		t.Fatalf("escaped-once query fields must query normally, got %+v", qr.Series)
	}
	// 查询标签键只转义一次：子集匹配照常。
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"\u0068ost":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 1 {
		t.Fatalf("escaped-once query label must match, got %+v", qr.Series)
	}
}

func TestDuplicateDetectionIsScopedToOneObject(t *testing.T) {
	store := NewMetricStore()

	// 不同采样点各自带 name：合法（同序列不同时间戳）。
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":2}
	]`)
	if res.Added != 2 {
		t.Fatalf("fields in separate samples must not clash, got %+v", res)
	}

	// 采样点字段名 name 与它的标签键 name 同时存在：分属不同对象，合法。
	res = mustOK(t, store, `[{"name":"m","timestamp":3,"value":3,"labels":{"name":"x"}}]`)
	if res.Added != 1 {
		t.Fatalf("sample field name and a label key named name must coexist, got %+v", res)
	}

	// 不同采样点的标签对象各自带同名键：合法。
	res = mustOK(t, store, `[
		{"name":"q","timestamp":1,"value":1,"labels":{"host":"a"}},
		{"name":"q","timestamp":2,"value":2,"labels":{"host":"b"}}
	]`)
	if res.Added != 2 {
		t.Fatalf("label keys in separate samples must not clash, got %+v", res)
	}
}

func TestLegitimateDuplicateSampleStillCountedAsDuplicate(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"d","timestamp":1,"value":5,"labels":{"host":"a"}}]`)

	// 同一序列、同一时间戳、值相等：按既有重复采样点规则成功忽略并计入 duplicates，
	// 指标名与标签键分别用直接/转义两种写法表达同一身份也不例外。
	res := mustOK(t, store, `[{"\u006eame":"d","timestamp":1,"value":5.0,"labels":{"\u0068ost":"a"}}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("equal re-submission must be a duplicate, not a field failure, got %+v", res)
	}

	// 值不同仍是采样值冲突（带 conflict），与重复字段失败（不带 conflict）明确区分。
	lerr := mustFail(t, store, `[{"name":"d","timestamp":1,"value":6,"labels":{"host":"a"}}]`)
	if lerr.Index != 1 || lerr.Conflict == nil || lerr.Conflict.Existing != 5 || lerr.Conflict.Submitted != 6 {
		t.Fatalf("different value must be a value conflict, got %+v", lerr)
	}
}

func TestDuplicateFieldInQueryObjectRejectedAfterUnescaping(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":3}]`)

	// 五个顶层字段各再用首字母的十六进制转义书写一次，全部必须判重（同值也算）。
	bad := []string{
		`{"op":"query","\u006fp":"query","name":"m","start":0,"end":10}`,
		`{"op":"query","name":"m","\u006eame":"m","start":0,"end":10}`,
		`{"op":"query","name":"m","start":0,"end":10,"\u0065nd":10}`,
		`{"op":"query","name":"m","start":0,"\u0073tart":10,"end":20}`,
		`{"op":"query","name":"m","start":0,"end":10,"labels":{},"\u006cabels":{}}`,
	}
	for _, q := range bad {
		_, lerr := store.QueryLine(q)
		if lerr == nil {
			t.Fatalf("query %s must fail on escaped duplicate field", q)
		}
		if lerr.Index != 0 {
			t.Fatalf("query %s: query errors must not carry index, got %d", q, lerr.Index)
		}
		if lerr.Conflict != nil {
			t.Fatalf("query %s: query errors must not carry conflict, got %+v", q, lerr.Conflict)
		}
		if !strings.Contains(lerr.Error, "duplicate field") {
			t.Fatalf("query %s: error = %q, want duplicate field", q, lerr.Error)
		}
	}

	// 失败查询不改变存储，正常查询仍能得到原结果。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 3 {
		t.Fatalf("failed duplicate-field queries must not affect storage, got %+v", qr.Series)
	}
}

func TestDuplicateLabelKeyInQueryRejectedAfterUnescaping(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":3,"labels":{"host":"a"}}]`)

	for _, q := range []string{
		`{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a","\u0068ost":"a"}}`, // 值相同
		`{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a","\u0068ost":"b"}}`, // 值不同
	} {
		_, lerr := store.QueryLine(q)
		if lerr == nil {
			t.Fatalf("query %s must fail on escaped duplicate label key", q)
		}
		if lerr.Index != 0 || lerr.Conflict != nil {
			t.Fatalf("query %s: must carry neither index nor conflict, got %+v", q, lerr)
		}
		if !strings.Contains(lerr.Error, `duplicate label key "host"`) {
			t.Fatalf("query %s: error = %q, want duplicate label key host", q, lerr.Error)
		}
	}

	// 绝不能把重复标签条件当成两个标签去匹配：合法查询结果保持不变。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 3 {
		t.Fatalf("storage/read path must be unaffected, got %+v", qr.Series)
	}
}
