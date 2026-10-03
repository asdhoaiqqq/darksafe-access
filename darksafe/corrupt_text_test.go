package darksafe

import (
	"strings"
	"testing"
)

// 损坏文本必须作为整行解析失败：不带 index、不带 conflict，且不与用户显式
// 写入的 U+FFFD 序列发生身份合并。写入与查询两条入口行为一致。

// assertWholeLineTextError 校验损坏文本产生的整行错误：无 index、无 conflict、
// 错误原因包含 want 子串（用于区分 UTF-8 与代理项两类原因）。
func assertWholeLineTextError(t *testing.T, lerr *LineError, want string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected whole-line failure, got success")
	}
	if lerr.Index != 0 {
		t.Fatalf("corrupt text must fail the whole line without index, got index=%d", lerr.Index)
	}
	if lerr.Conflict != nil {
		t.Fatalf("corrupt text must not report a conflict, got %+v", lerr.Conflict)
	}
	if !strings.Contains(lerr.Error, want) {
		t.Fatalf("error = %q, want substring %q", lerr.Error, want)
	}
}

func TestCorruptSurrogateEscapesRejected(t *testing.T) {
	bad := []string{
		`[{"name":"m\uD800","timestamp":1,"value":1}]`,                    // 单独高代理项
		`[{"name":"m\uDFFF","timestamp":1,"value":1}]`,                    // 高代理项区间末端
		`[{"name":"m\uDC00","timestamp":1,"value":1}]`,                    // 单独低代理项
		`[{"name":"m\uDfff","timestamp":1,"value":1}]`,                    // 小写十六进制的低代理项
		`[{"name":"m\uD800A","timestamp":1,"value":1}]`,                   // 高代理项后不是转义而是普通字符
		`[{"name":"m\uD800\uD800","timestamp":1,"value":1}]`,              // 高代理项后紧跟另一个高代理项
		`[{"name":"m\uD800\\uDE00","timestamp":1,"value":1}]`,             // 高代理项后是被转义的反斜杠
		`[{"name":"m","timestamp":1,"value":1,"labels":{"k":"x\uD800"}}]`, // 标签值
		`[{"name":"m","timestamp":1,"value":1,"labels":{"k\uD800":"v"}}]`, // 标签键
	}
	for _, line := range bad {
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		assertWholeLineTextError(t, lerr, "surrogate")
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("line %s: failed line must write no points, series=%d", line, got)
		}
	}
}

func TestCorruptSurrogateInQueryRejected(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m�","timestamp":1,"value":7}]`)
	badQueries := []string{
		`{"op":"query","name":"m\uD800","start":0,"end":10}`,
		`{"op":"query","name":"m","start":0,"end":10,"labels":{"k":"v\uDC00"}}`,
		`{"op":"query","name":"m","start":0,"end":10,"labels":{"k\uD800":"v"}}`,
	}
	for _, q := range badQueries {
		_, lerr := store.QueryLine(q)
		assertWholeLineTextError(t, lerr, "surrogate")
	}
	// 损坏查询绝不能经替换后命中用户显式写入的 U+FFFD 序列（上面已尝试 m\uD800）。
	res := mustQuery(t, store, `{"op":"query","name":"m�","start":0,"end":10}`)
	if len(res.Series) != 1 || res.Series[0].Average != 7 {
		t.Fatalf("real U+FFFD series still queryable, got %+v", res.Series)
	}
}

func TestInvalidUTF8BytesRejected(t *testing.T) {
	cases := [][]byte{
		[]byte(`[{"name":"m` + "\xed\xa0\x80" + `","timestamp":1,"value":1}]`),
		[]byte(`[{"name":"m` + "\xff" + `","timestamp":1,"value":1}]`),
		[]byte(`[{"name":"m","timestamp":1,"value":1,"labels":{"k":"` + "\xff" + `"}}]`),
		[]byte(`[{"name":"m","timestamp":1,"value":1,"labels":{"` + "\xfe" + `":"v"}}]`),
	}
	for _, raw := range cases {
		store := NewMetricStore()
		_, lerr := store.IngestLine(string(raw))
		assertWholeLineTextError(t, lerr, "UTF-8")
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("raw %v: failed line must write nothing, series=%d", raw, got)
		}
	}

	// 查询同样拒绝非法字节，且不返回替换后的匹配。
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m�","timestamp":1,"value":3}]`)
	q := []byte(`{"op":"query","name":"m` + "\xff" + `","start":0,"end":10}`)
	_, lerr := store.QueryLine(string(q))
	assertWholeLineTextError(t, lerr, "UTF-8")
}

func TestCorruptLineDoesNotMergeWithReplacementCharSeries(t *testing.T) {
	// 先写入一个用户显式命名为 “m�” 的序列。
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m�","timestamp":1,"value":7}]`)

	// 同值：损坏输入若被替换成 U+FFFD 会被误判为重复而成功——现在必须整行失败。
	_, lerr := store.IngestLine(`[{"name":"m\uD800","timestamp":1,"value":7}]`)
	assertWholeLineTextError(t, lerr, "surrogate")

	// 不同值：旧行为会误报与 U+FFFD 序列冲突——现在只是整行文本错误，无 conflict。
	_, lerr = store.IngestLine(`[{"name":"m\uD800","timestamp":1,"value":9}]`)
	assertWholeLineTextError(t, lerr, "surrogate")

	// 原始非法字节同值也不得误判为重复。
	raw := []byte(`[{"name":"m` + "\xff" + `","timestamp":1,"value":7}]`)
	_, lerr = store.IngestLine(string(raw))
	assertWholeLineTextError(t, lerr, "UTF-8")

	// 显式 U+FFFD 数据原样保留，仍只有一条序列、一个点。
	res := mustOK(t, store, `[]`)
	if len(res.Series) != 1 || res.Series[0].Name != "m�" ||
		len(res.Series[0].Points) != 1 || res.Series[0].Points[0].Value != 7 {
		t.Fatalf("U+FFFD series changed after corrupt inputs: %+v", res.Series)
	}
}

func TestCorruptLineAtomicEvenAfterValidSamples(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 数组第一个点完全合法，第二个点名称含孤立代理项：整行不得提交任何点。
	_, lerr := store.IngestLine(`[{"name":"brandnew","timestamp":1,"value":1},{"name":"bad\uD800","timestamp":2,"value":2}]`)
	assertWholeLineTextError(t, lerr, "surrogate")

	// 非法字节出现在后面的点同样整行回滚。
	raw := []byte(`[{"name":"brandnew2","timestamp":1,"value":1},{"name":"bad` + "\xff" + `","timestamp":2,"value":2}]`)
	_, lerr = store.IngestLine(string(raw))
	assertWholeLineTextError(t, lerr, "UTF-8")

	res := mustOK(t, store, `[]`)
	if len(res.Series) != 1 || res.Series[0].Name != "old" {
		t.Fatalf("valid prefix points must not commit, snapshot=%+v", res.Series)
	}
}

func TestValidUnicodeTextBehaviorPreserved(t *testing.T) {
	store := NewMetricStore()

	// 中文、补充平面字符直接书写均合法。
	mustOK(t, store, `[{"name":"中文","timestamp":1,"value":1,"labels":{"主机":"北京"}}]`)
	mustOK(t, store, `[{"name":"😀","timestamp":1,"value":2}]`)

	// 合法成对代理项转义与直接书写的补充平面字符身份相同 → 同点重复。
	// 上一行已直接书写 😀（U+1F600）；这里改用合法成对转义 😀 书写同一字符。
	res := mustOK(t, store, `[{"name":"m\uD83D\uDE00","timestamp":1,"value":2.0}]`)
	// 名称带 m 前缀是另一个序列，仅用于确认合法成对转义能正常写入。
	if res.Added != 1 {
		t.Fatalf("valid matched surrogate pair should write as its own series, got added=%d", res.Added)
	}
	res = mustOK(t, store, `[{"name":"\uD83D\uDE00","timestamp":1,"value":2.0}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("literal supplementary char and legal escaped pair must be the same identity, got added=%d dup=%d",
			res.Added, res.Duplicates)
	}

	// 用户显式书写的 “�” 与 � 身份相同 → 同点重复。
	mustOK(t, store, `[{"name":"m�","timestamp":1,"value":5}]`)
	res = mustOK(t, store, `[{"name":"m\uFFFD","timestamp":1,"value":5.0}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("literal U+FFFD and \\uFFFD must be the same identity, got added=%d dup=%d",
			res.Added, res.Duplicates)
	}

	// 被转义的反斜杠后的 uD800 只是普通文本，合法，且名称就是字面的 m\uD800；
	// 它既不是代理项转义，也与真正含 U+FFFD 的序列不同。
	res = mustOK(t, store, `[{"name":"m\\uD800","timestamp":1,"value":1}]`)
	if res.Added != 1 {
		t.Fatalf("escaped-backslash text must be accepted as its own series, got %+v", res)
	}
	found := false
	for _, sv := range res.Series {
		if sv.Name == `m\uD800` {
			found = true
		}
	}
	if !found {
		t.Fatalf(`literal name "m\uD800" not found in snapshot: %+v`, res.Series)
	}

	// 合法对后接普通字符也正常。
	mustOK(t, store, `[{"name":"x😀y","timestamp":1,"value":1}]`)
}

func TestValidUnicodeLabelRulesPreserved(t *testing.T) {
	store := NewMetricStore()
	// Unicode 标签：顺序无关、空值保留、子集匹配。
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":1,"labels":{"区域":"华东","机房":""}}]`)
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":1.0,"labels":{"机房":"","区域":"华东"}}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("label order with unicode must not change identity, got %+v", res)
	}
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10,"labels":{"区域":"华东"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 1 {
		t.Fatalf("unicode subset label match failed: %+v", qr.Series)
	}
	// 空值标签只匹配真实存在的空值。
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10,"labels":{"机房":""}}`)
	if len(qr.Series) != 1 {
		t.Fatalf("empty unicode label value must match, got %+v", qr.Series)
	}
}

func TestProcessLineConsistentAcrossEntries(t *testing.T) {
	store := NewMetricStore()
	// ProcessLine 对数组与对象都应先做文本校验。
	_, lerr := store.ProcessLine(`[{"name":"m\uD800","timestamp":1,"value":1}]`)
	assertWholeLineTextError(t, lerr, "surrogate")
	_, lerr = store.ProcessLine(`{"op":"query","name":"m\uDC00","start":0,"end":1}`)
	assertWholeLineTextError(t, lerr, "surrogate")
	raw := []byte(`[{"name":"` + "\xff" + `","timestamp":1,"value":1}]`)
	_, lerr = store.ProcessLine(string(raw))
	assertWholeLineTextError(t, lerr, "UTF-8")
}
