package darksafe

import (
	"fmt"
	"strings"
	"testing"
)

// 回归保障：同一对象内的重复字段/标签键，即使其中一个用 JSON Unicode 转义
// 书写（例如 name 改写成首字符为 u006e、即字母 n 的转义键名），也必须按
// 转义还原后的字符判定为重复并拒绝。判定范围只限同一个对象；不同采样点
// 之间、指标名与标签键之间互不影响。

// jsonUnicodeSpelling 把 s 的每个 ASCII 字节改写成 JSON 的 uXXXX 转义形式；
// 反斜杠以字节 92 直接生成，避免源码层面的转义歧义。返回的字符串是合法的
// JSON 字符串内容，解析后与直接书写的 s 完全等价，例如对 "host" 的改写
// 以 u0068（字母 h）开头。
func jsonUnicodeSpelling(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteByte(92) // JSON 转义引导符 '\'
		fmt.Fprintf(&b, "u%04x", s[i])
	}
	return b.String()
}

// assertDupFieldError 校验“重复字段”类错误：定位到正确的采样点 index，
// 错误原因点名重复的字段或标签键，且绝不附带 conflict。
func assertDupFieldError(t *testing.T, lerr *LineError, index int, key string) {
	t.Helper()
	if lerr == nil {
		t.Fatalf("expected duplicate failure at index %d, got success", index)
	}
	if lerr.Index != index {
		t.Fatalf("index = %d, want %d (error: %s)", lerr.Index, index, lerr.Error)
	}
	if lerr.Conflict != nil {
		t.Fatalf("duplicate key failure must not carry a conflict, got %+v", lerr.Conflict)
	}
	if !strings.Contains(lerr.Error, key) {
		t.Fatalf("error = %q, want it to name the duplicate key %q", lerr.Error, key)
	}
}

func TestEscapedDuplicateSampleFieldRejected(t *testing.T) {
	// 采样点顶层字段：name 与转义书写的同名键同属一个对象即重复；
	// 两个值相同或不同都必须失败，不能静默采用其中一个。
	escName := jsonUnicodeSpelling("name")
	escTimestamp := jsonUnicodeSpelling("timestamp")
	escValue := jsonUnicodeSpelling("value")
	escLabels := jsonUnicodeSpelling("labels")
	cases := []struct {
		name string
		line string
	}{
		{"name escaped second, same value",
			`[{"name":"m","` + escName + `":"m","timestamp":1,"value":1}]`},
		{"name escaped first, same value",
			`[{"` + escName + `":"m","name":"m","timestamp":1,"value":1}]`},
		{"name duplicate with different values",
			`[{"name":"m","` + escName + `":"n","timestamp":1,"value":1}]`},
		{"timestamp escaped duplicate",
			`[{"name":"m","timestamp":1,"` + escTimestamp + `":1,"value":1}]`},
		{"value escaped duplicate",
			`[{"name":"m","timestamp":1,"value":1,"` + escValue + `":1}]`},
		{"labels escaped duplicate",
			`[{"name":"m","timestamp":1,"value":1,"labels":{},"` + escLabels + `":{}}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			_, lerr := store.IngestLine(tc.line)
			if lerr == nil {
				t.Fatalf("line %s must be rejected", tc.line)
			}
			assertDupFieldError(t, lerr, 1, "duplicate field")
			if got := len(mustOK(t, store, `[]`).Series); got != 0 {
				t.Fatalf("rejected batch must write nothing, series=%d", got)
			}
		})
	}

	// 大写十六进制位书写同一码点（u006E 仍是 n）同样判为重复。
	store := NewMetricStore()
	upperEsc := string([]byte{92}) + "u006Eame"
	_, lerr := store.IngestLine(`[{"name":"m","` + upperEsc + `":"m","timestamp":1,"value":1}]`)
	assertDupFieldError(t, lerr, 1, "name")
}

func TestEscapedDuplicateLabelKeyRejected(t *testing.T) {
	// labels 内 host 与转义书写的同名键属于同一标签键：
	// 值相同、值不同都是重复键，不能解释成两个标签，也不能变成采样值冲突。
	escHost := jsonUnicodeSpelling("host")
	cases := []string{
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + escHost + `":"a"}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"host":"a","` + escHost + `":"b"}}]`,
		`[{"name":"m","timestamp":1,"value":1,"labels":{"` + escHost + `":"a","host":"a"}}]`,
	}
	for _, line := range cases {
		store := NewMetricStore()
		_, lerr := store.IngestLine(line)
		if lerr == nil {
			t.Fatalf("line %s must be rejected", line)
		}
		assertDupFieldError(t, lerr, 1, "host")
		if !strings.Contains(lerr.Error, "duplicate label key") {
			t.Fatalf("error = %q, want a duplicate label key reason", lerr.Error)
		}
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("rejected batch must write nothing, series=%d", got)
		}
	}
}

func TestEscapedDuplicateInLaterSampleFailsWholeBatch(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 第一个点是本批合法新增；第二个点的 labels 出现转义重复键：
	// 整批失败，index 从 1 开始指向第 2 个点，无 conflict。
	escHost := jsonUnicodeSpelling("host")
	_, lerr := store.IngestLine(`[
		{"name":"new","timestamp":1,"value":1},
		{"name":"new","timestamp":2,"value":2,"labels":{"host":"a","` + escHost + `":"b"}}
	]`)
	assertDupFieldError(t, lerr, 2, "host")

	// 顶层字段重复出现在靠后的点同样定位准确，且整批回滚。
	escName := jsonUnicodeSpelling("name")
	_, lerr = store.IngestLine(`[
		{"name":"new","timestamp":1,"value":1},
		{"name":"new","` + escName + `":"other","timestamp":2,"value":2}
	]`)
	assertDupFieldError(t, lerr, 2, "name")

	// 本批前面的合法新增点未提交；此前成功写入的 old 点数量与均值不变。
	res := mustQuery(t, store, `{"op":"query","name":"old","start":0,"end":10}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 9 {
		t.Fatalf("earlier data must survive unchanged, got %+v", res.Series)
	}
	res = mustQuery(t, store, `{"op":"query","name":"new","start":0,"end":10}`)
	if len(res.Series) != 0 {
		t.Fatalf("valid prefix of failed batch must not commit, got %+v", res.Series)
	}
}

func TestEscapedDuplicateKeyInQueryRejected(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1,"value":5,"labels":{"host":"a"}}]`)

	escName := jsonUnicodeSpelling("name")
	escOp := jsonUnicodeSpelling("op")
	escStart := jsonUnicodeSpelling("start")
	escEnd := jsonUnicodeSpelling("end")
	escHost := jsonUnicodeSpelling("host")
	bad := []struct {
		name string
		line string
		key  string
	}{
		{"escaped top-level duplicate",
			`{"op":"query","name":"m","` + escName + `":"m","start":0,"end":10}`, "name"},
		{"escaped op duplicate",
			`{"op":"query","` + escOp + `":"query","name":"m","start":0,"end":10}`, "op"},
		{"escaped start duplicate",
			`{"op":"query","name":"m","start":0,"` + escStart + `":0,"end":10}`, "start"},
		{"escaped end duplicate",
			`{"op":"query","name":"m","start":0,"end":10,"` + escEnd + `":10}`, "end"},
		{"escaped duplicate label key, same value",
			`{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a","` + escHost + `":"a"}}`, "host"},
		{"escaped duplicate label key, diff value",
			`{"op":"query","name":"m","start":0,"end":10,"labels":{"host":"a","` + escHost + `":"b"}}`, "host"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			res, lerr := store.QueryLine(tc.line)
			if lerr == nil {
				t.Fatalf("query %s must fail, got %+v", tc.line, res)
			}
			if lerr.Index != 0 {
				t.Fatalf("query errors must not carry an index, got %d", lerr.Index)
			}
			if lerr.Conflict != nil {
				t.Fatalf("query errors must not carry a conflict, got %+v", lerr.Conflict)
			}
			if !strings.Contains(lerr.Error, tc.key) {
				t.Fatalf("error = %q, want it to name %q", lerr.Error, tc.key)
			}
		})
	}

	// 失败查询不返回成功结果，也不改变存储。
	res := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	if len(res.Series) != 1 || res.Series[0].Count != 1 || res.Series[0].Average != 5 {
		t.Fatalf("storage changed after failed queries: %+v", res.Series)
	}
}

func TestSingleEscapedFieldAcceptedAndEquivalent(t *testing.T) {
	// 只出现一次的转义字段名与直接书写同名字段含义完全相同。
	store := NewMetricStore()
	escName := jsonUnicodeSpelling("name")
	escTimestamp := jsonUnicodeSpelling("timestamp")
	escValue := jsonUnicodeSpelling("value")
	escHost := jsonUnicodeSpelling("host")
	res := mustOK(t, store, `[{"`+escName+`":"m","`+escTimestamp+`":1,"`+escValue+`":2,"labels":{"`+escHost+`":"a"}}]`)
	if res.Added != 1 {
		t.Fatalf("escaped-only write should add one point, got %+v", res)
	}

	// 直接书写同一点：身份相同、值相同，计为重复。
	res = mustOK(t, store, `[{"name":"m","timestamp":1,"value":2.0,"labels":{"host":"a"}}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("escaped and literal spellings must be the same identity, got %+v", res)
	}

	// 查询侧：转义书写的 name/labels 与直接书写同样命中。
	qr := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10,"labels":{"`+escHost+`":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("escaped query spelling must match the stored point, got %+v", qr.Series)
	}
}

func TestDuplicateKeyScopeIsSingleObject(t *testing.T) {
	store := NewMetricStore()

	// 不同采样点各自带 name：合法，两个点都写入。
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":2}
	]`)
	if res.Added != 2 {
		t.Fatalf("name in distinct sample objects is not a duplicate, got %+v", res)
	}

	// 采样点的指标名 name 与它的标签键 name 分属不同对象：合法。
	res = mustOK(t, store, `[{"name":"name","timestamp":1,"value":3,"labels":{"name":"x"}}]`)
	if res.Added != 1 {
		t.Fatalf("metric name and a label key share text but live in different objects, got %+v", res)
	}

	// 查询对象的指标名与其标签条件键同名：同样合法且能命中。
	qr := mustQuery(t, store, `{"op":"query","name":"name","start":0,"end":10,"labels":{"name":"x"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 3 {
		t.Fatalf("query name and label key share text but must stay independent, got %+v", qr.Series)
	}
}

func TestIdenticalSamplesStillCountAsDuplicates(t *testing.T) {
	// 两个合法采样点（各自只含单份转义书写）代表同序列、同时间戳、同值：
	// 按既有重复采样点规则成功忽略并计入 duplicates，与字段重复导致的失败相区别。
	store := NewMetricStore()
	escName := jsonUnicodeSpelling("name")
	escTimestamp := jsonUnicodeSpelling("timestamp")
	escHost := jsonUnicodeSpelling("host")
	res := mustOK(t, store, `[
		{"`+escName+`":"m","timestamp":1,"value":4,"labels":{"`+escHost+`":"a"}},
		{"name":"m","`+escTimestamp+`":1,"value":4.0,"labels":{"host":"a"}}
	]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("identical escaped-spelling samples must be a duplicate, got added=%d dup=%d",
			res.Added, res.Duplicates)
	}
}
