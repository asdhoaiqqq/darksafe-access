package darksafe

import (
	"strings"
	"testing"
)

// 本文件回归保障损坏文本处理：每行 JSON 必须是合法 UTF-8，字符串中的
// \uXXXX 转义必须表示有效字符（代理项必须成对出现）。encoding/json 会把
// 两类损坏都静默替换为 U+FFFD，导致损坏输入混入合法序列身份；因此这类输入
// 必须作为整行解析失败：不带 index、不带 conflict，写入不提交任何点、
// 查询不返回替换后匹配；此前数据保留，后续行继续处理，进程非零退出。

// assertWholeLineTextError 断言失败是整行文本编码错误：无 index、无 conflict，
// 且错误原因中包含 wantSubstr。
func assertWholeLineTextError(t *testing.T, lerr *LineError, wantSubstr string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected whole-line text error, got success")
	}
	if lerr.Index != 0 {
		t.Fatalf("text corruption must fail the whole line without index, got %+v", lerr)
	}
	if lerr.Conflict != nil {
		t.Fatalf("text corruption must fail without conflict, got %+v", lerr)
	}
	if !strings.Contains(lerr.Error, wantSubstr) {
		t.Fatalf("error = %q, want it to contain %q", lerr.Error, wantSubstr)
	}
}

func TestCorruptUTF8BytesRejectedInAllStringPositions(t *testing.T) {
	// Go 源文件本身必须是合法 UTF-8，损坏字节用拼接构造。
	cases := []struct {
		name string
		line string
	}{
		{"metric name", `[{"name":"a` + "\xff" + `b","timestamp":1,"value":1}]`},
		{"label key", `[{"name":"m","timestamp":1,"value":1,"labels":{"a` + "\xff" + `b":"v"}}]`},
		{"label value", `[{"name":"m","timestamp":1,"value":1,"labels":{"k":"a` + "\xff" + `b"}}]`},
		{"truncated multibyte", `[{"name":"a` + "\xe4" + `b","timestamp":1,"value":1}]`},
		{"overlong null", `[{"name":"a` + "\xc0\x80" + `b","timestamp":1,"value":1}]`},
		{"lone continuation byte", `[{"name":"a` + "\x80" + `b","timestamp":1,"value":1}]`},
		// 查询对象中的字符串同样适用。
		{"query name", `{"op":"query","name":"a` + "\xff" + `b","start":0,"end":1}`},
		{"query label key", `{"op":"query","name":"m","start":0,"end":1,"labels":{"a` + "\xff" + `b":"v"}}`},
		{"query label value", `{"op":"query","name":"m","start":0,"end":1,"labels":{"k":"a` + "\xff" + `b"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			_, lerr := store.ProcessLine(tc.line)
			assertWholeLineTextError(t, lerr, "valid UTF-8")
			if strings.Contains(lerr.Error, "surrogate") {
				t.Fatalf("illegal UTF-8 must not be reported as a surrogate problem: %q", lerr.Error)
			}
			// 写入入口与查询入口结果一致。
			if strings.HasPrefix(tc.line, "[") {
				_, lerr2 := store.IngestLine(tc.line)
				assertWholeLineTextError(t, lerr2, "valid UTF-8")
			} else {
				_, lerr2 := store.QueryLine(tc.line)
				assertWholeLineTextError(t, lerr2, "valid UTF-8")
			}
			if got := len(mustOK(t, store, `[]`).Series); got != 0 {
				t.Fatalf("corrupt line must not write any series, got %d", got)
			}
		})
	}
}

func TestUnpairedSurrogateEscapesRejected(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"lone high in name", `[{"name":"a\uD800b","timestamp":1,"value":1}]`},
		{"lone low in name", `[{"name":"a\uDC00b","timestamp":1,"value":1}]`},
		{"high then ascii escape", `[{"name":"a\uD800Ab","timestamp":1,"value":1}]`},
		{"high then uFFFD", `[{"name":"a\uD800�B","timestamp":1,"value":1}]`},
		{"two highs", `[{"name":"a\uD800\uD800b","timestamp":1,"value":1}]`},
		{"pair then lone low", `[{"name":"a😀\uDE00","timestamp":1,"value":1}]`},
		{"high then truncated escape", `[{"name":"a\uD83D\uDE0","timestamp":1,"value":1}]`},
		{"high at end of string", `[{"name":"a\uD83D","timestamp":1,"value":1}]`},
		{"escaped slash then real lone high", `[{"name":"a\\` + `\uD800b","timestamp":1,"value":1}]`},
		{"uppercase lone high", `[{"name":"A\uDBFF","timestamp":1,"value":1}]`},
		{"lone low in label key", `[{"name":"m","timestamp":1,"value":1,"labels":{"k\uDC00":"v"}}]`},
		{"lone high in label value", `[{"name":"m","timestamp":1,"value":1,"labels":{"k":"v\uD800"}}]`},
		{"query name", `{"op":"query","name":"a\uD800b","start":0,"end":1}`},
		{"query label key", `{"op":"query","name":"m","start":0,"end":1,"labels":{"k\uDC00":"v"}}`},
		{"query label value", `{"op":"query","name":"m","start":0,"end":1,"labels":{"k":"v\uD800"}}`},
		{"query op string", `{"op":"quer\uD800","name":"m","start":0,"end":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			_, lerr := store.ProcessLine(tc.line)
			assertWholeLineTextError(t, lerr, "surrogate")
			if strings.Contains(lerr.Error, "UTF-8") {
				t.Fatalf("surrogate escape must be distinguished from UTF-8 byte errors: %q", lerr.Error)
			}
			if strings.HasPrefix(tc.line, "[") {
				_, lerr2 := store.IngestLine(tc.line)
				assertWholeLineTextError(t, lerr2, "surrogate")
			} else {
				_, lerr2 := store.QueryLine(tc.line)
				assertWholeLineTextError(t, lerr2, "surrogate")
			}
			if got := len(mustOK(t, store, `[]`).Series); got != 0 {
				t.Fatalf("corrupt line must not write any series, got %d", got)
			}
		})
	}
}

func TestCorruptWriteDoesNotMergeWithReplacementCharacterSeries(t *testing.T) {
	store := NewMetricStore()
	// 先写入一个合法的、名字里带 U+FFFD 的指标。
	mustOK(t, store, `[{"name":"m�","timestamp":1,"value":1}]`)

	// 损坏的 \uD800 若被修补成 U+FFFD，同值提交会被误判为重复——必须整行失败。
	lerr := mustFail(t, store, `[{"name":"m\uD800","timestamp":1,"value":1}]`)
	assertWholeLineTextError(t, lerr, "surrogate")

	// 不同值同样不能被误判为冲突（不能带 conflict）。
	lerr = mustFail(t, store, `[{"name":"m\uD800","timestamp":1,"value":2}]`)
	assertWholeLineTextError(t, lerr, "surrogate")

	// 原始非法字节同理。
	lerr = mustFail(t, store, `[{"name":"m`+"\xff"+`","timestamp":1,"value":1}]`)
	assertWholeLineTextError(t, lerr, "valid UTF-8")

	// 存储中始终只有那条合法的替换字符序列。
	res := mustOK(t, store, `[]`)
	if len(res.Series) != 1 || res.Series[0].Name != "m�" || len(res.Series[0].Points) != 1 {
		t.Fatalf("store must retain only the legitimate U+FFFD series, got %+v", res.Series)
	}
}

func TestCorruptQueryNeverMatchesReplacementCharacterData(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m�","timestamp":5,"value":7,"labels":{"h":"a�b"}}]`)

	// 指标名损坏：不得返回修补后的匹配。
	_, lerr := store.QueryLine(`{"op":"query","name":"m\uD800","start":0,"end":10}`)
	assertWholeLineTextError(t, lerr, "surrogate")
	_, lerr = store.QueryLine(`{"op":"query","name":"m` + "\xff" + `","start":0,"end":10}`)
	assertWholeLineTextError(t, lerr, "valid UTF-8")

	// 标签值损坏：不得通过子集匹配命中。
	_, lerr = store.QueryLine(`{"op":"query","name":"m�","start":0,"end":10,"labels":{"h":"a\uD800b"}}`)
	assertWholeLineTextError(t, lerr, "surrogate")
	_, lerr = store.QueryLine(`{"op":"query","name":"m�","start":0,"end":10,"labels":{"h":"a` + "\xff" + `b"}}`)
	assertWholeLineTextError(t, lerr, "valid UTF-8")

	// 标签键损坏同理。
	_, lerr = store.QueryLine(`{"op":"query","name":"m�","start":0,"end":10,"labels":{"h\uD800":"x"}}`)
	assertWholeLineTextError(t, lerr, "surrogate")

	// 存储未受任何失败查询影响，合法查询照常命中。
	res := mustQuery(t, store, `{"op":"query","name":"m�","start":0,"end":10,"labels":{"h":"a�b"}}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 7 {
		t.Fatalf("legitimate query = %+v, want the stored point", res.Series)
	}
}

func TestCorruptLineDiscardsEarlierValidPointsInSameBatch(t *testing.T) {
	store := NewMetricStore()
	// 第一个点完全合法，标签键里的损坏出现在第二个点：整行都不得提交。
	_, lerr := store.IngestLine(`[
		{"name":"good","timestamp":1,"value":1,"labels":{"host":"a"}},
		{"name":"bad","timestamp":1,"value":2,"labels":{"host":"b\uD800"}}
	]`)
	assertWholeLineTextError(t, lerr, "surrogate")
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("no point from a corrupt line may commit, series=%d", got)
	}

	// 非法 UTF-8 字节出现在同批较后的点同样回滚整批。
	_, lerr = store.IngestLine(`[
		{"name":"good","timestamp":1,"value":1},
		{"name":"b` + "\xff" + `d","timestamp":1,"value":2}
	]`)
	assertWholeLineTextError(t, lerr, "valid UTF-8")
	if got := len(mustOK(t, store, `[]`).Series); got != 0 {
		t.Fatalf("no point from a UTF-8-corrupt line may commit, series=%d", got)
	}
}

func TestValidUnicodeTextStillAccepted(t *testing.T) {
	store := NewMetricStore()
	// 中文、补充平面字符（代理对转义书写与直写各一次，同点同值为重复）、
	// U+FFFD（� 转义与直写各一次，同点同值为重复）。
	first := mustOK(t, store, `[
		{"name":"中文指标","timestamp":1,"value":1,"labels":{"区":"华北"}},
		{"name":"a😀b","timestamp":1,"value":2},
		{"name":"a😀b","timestamp":1,"value":2},
		{"name":"r�","timestamp":1,"value":4},
		{"name":"r�","timestamp":1,"value":4}
	]`)
	if first.Added != 3 || first.Duplicates != 2 {
		t.Fatalf("literal and escaped forms share identity; got added=%d duplicates=%d",
			first.Added, first.Duplicates)
	}

	// 同一补充平面字符直写与代理对转义身份相同：换时间戳写入后仍是同一条序列。
	res := mustOK(t, store, `[{"name":"a😀b","timestamp":2,"value":6}]`)
	var emojiViews []SeriesView
	for _, v := range res.Series {
		if strings.Contains(v.Name, "😀") {
			emojiViews = append(emojiViews, v)
		}
	}
	if len(emojiViews) != 1 || len(emojiViews[0].Points) != 2 {
		t.Fatalf("literal and escaped supplementary char must be one series, got %+v", emojiViews)
	}

	// U+FFFD 直写与 � 转义身份相同：同点同值为重复，同点异值为冲突而非新序列。
	dup := mustOK(t, store, `[{"name":"r�","timestamp":1,"value":4.0}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("literal and escaped U+FFFD must be the same identity, got %+v", dup)
	}
	lerr := mustFail(t, store, `[{"name":"r�","timestamp":1,"value":9}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 4 || lerr.Conflict.Submitted != 9 {
		t.Fatalf("U+FFFD identity conflict = %+v", lerr)
	}
}

func TestEscapedBackslashBeforeUIsPlainText(t *testing.T) {
	store := NewMetricStore()
	// 转义反斜杠后的 uD800 只是名字里的普通文本。
	res := mustOK(t, store, `[{"name":"a\\uD800b","timestamp":1,"value":1}]`)
	if len(res.Series) != 1 || res.Series[0].Name != `a\uD800b` {
		t.Fatalf("escaped backslash + uD800 must be literal text, got %+v", res.Series)
	}
	// 该身份可被原样查询到。
	qr := mustQuery(t, store, `{"op":"query","name":"a\\uD800b","start":0,"end":2}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 1 {
		t.Fatalf("literal-text series query = %+v", qr.Series)
	}
	// 标签值中同理。
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":2,"labels":{"k":"x\\uDC00y"}}]`)
	qr = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"k":"x\\uDC00y"}}`)
	if len(qr.Series) != 1 {
		t.Fatalf("literal low-surrogate text in label value must match, got %+v", qr.Series)
	}
	// 三个反斜杠：前两个组成转义反斜杠，第三个开启真正的代理转义——必须拒绝。
	_, lerr := store.IngestLine(`[{"name":"a\\\uD800b","timestamp":1,"value":1}]`)
	assertWholeLineTextError(t, lerr, "surrogate")
}

func TestSurrogateScanIgnoresEscapesOutsideStrings(t *testing.T) {
	// \uD800 出现在字符串之外时不是合法 JSON，应由常规 JSON 解析拒绝，
	// 而不是被报成“字符串中的代理项转义”问题。
	store := NewMetricStore()
	_, lerr := store.ProcessLine(`[1, \uD800]`)
	if lerr == nil || lerr.Index != 0 {
		t.Fatalf("non-string \\u escape must be a whole-line parse error, got %+v", lerr)
	}
	if !strings.Contains(lerr.Error, "invalid JSON") {
		t.Fatalf("error = %q, want invalid JSON parse error", lerr.Error)
	}
}

func TestValidNonBMPValuesInLabelsAndQuery(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":8,"labels":{"host":"🐹-甲"}}]`)
	// 代理对转义书写同样的标签值：同序列重复。
	res := mustOK(t, store, `[{"name":"m","timestamp":1,"value":8,"labels":{"host":"🐹-甲"}}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("surrogate-pair label must equal literal label, got %+v", res)
	}
	// 用代理对转义查询，子集匹配命中。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2,"labels":{"host":"🐹-甲"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 8 {
		t.Fatalf("surrogate-pair label query = %+v", qr.Series)
	}
	// 含损坏转义的标签值不做任何匹配。
	_, lerr := store.QueryLine(`{"op":"query","name":"m","start":0,"end":2,"labels":{"host":"\uD83D-甲"}}`)
	assertWholeLineTextError(t, lerr, "surrogate")
}
