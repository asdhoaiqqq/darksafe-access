package darksafe

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件回归保障冲突原因文本中的序列身份无歧义：名称、标签键和值中的逗号、
// 等号、花括号、引号、反斜杠、首尾空白与控制字符都是合法数据，渲染时必须与
// 身份表示自身的边界字符区分开，使用户只看 error 字符串就能分辨冲突属于哪条
// 真实序列。结构化 conflict 字段的语义由既有测试覆盖，这里锁定文本形态。

// errorTextAfterJSON 模拟命令输出被解析后的读取路径：把 error 字符串经 JSON
// 编码再解码，返回用户实际看到的文本。
func errorTextAfterJSON(t *testing.T, lerr *LineError) string {
	t.Helper()
	raw, err := json.Marshal(lerr)
	if err != nil {
		t.Fatalf("marshal LineError: %v", err)
	}
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal LineError: %v", err)
	}
	return decoded.Error
}

// TestConflictMessageDistinguishesLookAlikeSeries 是任务场景的核心回归：
// cpu {"a":"1,b=2"}（单标签，值里含逗号与等号）与 cpu {"a":"1","b":"2"}（两标签）
// 是两条不同序列；它们在同一时间戳各有已写入的值后分别提交新值，两条冲突原因
// 的身份部分必须不同，且各自指向真实序列。
func TestConflictMessageDistinguishesLookAlikeSeries(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":"1,b=2"}},
		{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":"1","b":"2"}}
	]`)

	single := mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"a":"1,b=2"}}]`)
	pair := mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"a":"1","b":"2"}}]`)

	singleText := errorTextAfterJSON(t, single)
	pairText := errorTextAfterJSON(t, pair)
	if singleText == pairText {
		t.Fatalf("conflict messages for distinct series must differ, both = %q", singleText)
	}
	if want := `conflict: series cpu{a="1,b=2"} at timestamp 1000 already has value 1, submitted 2`; singleText != want {
		t.Fatalf("single-label conflict = %q, want %q", singleText, want)
	}
	if want := `conflict: series cpu{a=1,b=2} at timestamp 1000 already has value 1, submitted 2`; pairText != want {
		t.Fatalf("two-label conflict = %q, want %q", pairText, want)
	}
	// 结构化 conflict 与文本指向同一条序列。
	if len(single.Conflict.Series.Labels) != 1 || single.Conflict.Series.Labels["a"] != "1,b=2" {
		t.Fatalf("single-label structured conflict = %+v", single.Conflict.Series)
	}
	if len(pair.Conflict.Series.Labels) != 2 {
		t.Fatalf("two-label structured conflict = %+v", pair.Conflict.Series)
	}
}

// TestConflictMessageEscapesBoundaryCharacters：名称、键、值中的逗号、等号、
// 花括号、引号与反斜杠都以可辨认的转义形式呈现，原字符不丢失、不与边界混淆；
// 首尾空格因加引号而可见；空字符串值与无标签仍是两种可区分形态。
func TestConflictMessageEscapesBoundaryCharacters(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1,"labels":{"k":" v "}},
		{"name":"m","timestamp":1,"value":1,"labels":{"k":"a,b"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"k":"a=b"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"k":"a{b}"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"k":"a\"b"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"k":"a\\b"}},
		{"name":"m","timestamp":1,"value":1,"labels":{"k":""}},
		{"name":"weird}name","timestamp":1,"value":1}
	]`)
	cases := []struct {
		line string
		want string
	}{
		{`[{"name":"m","timestamp":1,"value":2,"labels":{"k":" v "}}]`,
			`conflict: series m{k=" v "} at timestamp 1 already has value 1, submitted 2`},
		{`[{"name":"m","timestamp":1,"value":2,"labels":{"k":"a,b"}}]`,
			`conflict: series m{k="a,b"} at timestamp 1 already has value 1, submitted 2`},
		{`[{"name":"m","timestamp":1,"value":2,"labels":{"k":"a=b"}}]`,
			`conflict: series m{k="a=b"} at timestamp 1 already has value 1, submitted 2`},
		{`[{"name":"m","timestamp":1,"value":2,"labels":{"k":"a{b}"}}]`,
			`conflict: series m{k="a{b}"} at timestamp 1 already has value 1, submitted 2`},
		{`[{"name":"m","timestamp":1,"value":2,"labels":{"k":"a\"b"}}]`,
			`conflict: series m{k="a\"b"} at timestamp 1 already has value 1, submitted 2`},
		{`[{"name":"m","timestamp":1,"value":2,"labels":{"k":"a\\b"}}]`,
			`conflict: series m{k="a\\b"} at timestamp 1 already has value 1, submitted 2`},
		{`[{"name":"m","timestamp":1,"value":2,"labels":{"k":""}}]`,
			`conflict: series m{k=} at timestamp 1 already has value 1, submitted 2`},
		{`[{"name":"weird}name","timestamp":1,"value":2}]`,
			`conflict: series "weird}name"{} at timestamp 1 already has value 1, submitted 2`},
	}
	for _, tc := range cases {
		lerr := mustFail(t, store, tc.line)
		if got := errorTextAfterJSON(t, lerr); got != tc.want {
			t.Fatalf("conflict for %s = %q, want %q", tc.line, got, tc.want)
		}
	}
	// 无标签序列与空字符串值标签序列的渲染不同。
	mustOK(t, store, `[{"name":"n","timestamp":1,"value":1}]`)
	bare := mustFail(t, store, `[{"name":"n","timestamp":1,"value":2}]`)
	if got := errorTextAfterJSON(t, bare); !strings.Contains(got, "series n{}") {
		t.Fatalf("unlabeled conflict = %q, want n{}", got)
	}
}

// TestConflictMessageControlCharactersBecomeMarkers：名称或标签中的换行与
// 制表符在 error 字符串里呈现为 \n、\t 文字标记，一条冲突原因始终是一行文本，
// 不会因内容中的换行而呈现为多条记录。
func TestConflictMessageControlCharactersBecomeMarkers(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "[{\"name\":\"cpu\\nload\",\"timestamp\":1,\"value\":1,\"labels\":{\"ho\\tst\":\"a\\nb\"}}]")
	lerr := mustFail(t, store, "[{\"name\":\"cpu\\nload\",\"timestamp\":1,\"value\":2,\"labels\":{\"ho\\tst\":\"a\\nb\"}}]")
	text := errorTextAfterJSON(t, lerr)
	want := `conflict: series "cpu\nload"{"ho\tst"="a\nb"} at timestamp 1 already has value 1, submitted 2`
	if text != want {
		t.Fatalf("conflict = %q, want %q", text, want)
	}
	if strings.ContainsAny(text, "\n\t\r") {
		t.Fatalf("conflict text must not contain raw control characters: %q", text)
	}
}

// TestConflictMessageIdentityStableAcrossOrderAndEscapes：同一身份无论标签书写
// 顺序、或同一字符直接书写还是以合法 JSON 转义书写，冲突原因里的身份部分都相同，
// 标签仍按键排序展示。
func TestConflictMessageIdentityStableAcrossOrderAndEscapes(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"cpu","timestamp":1,"value":1,"labels":{"a":"1,b=2","b":"x"}}
	]`)
	want := `conflict: series cpu{a="1,b=2",b=x} at timestamp 1 already has value 1, submitted 2`
	// 直接书写。
	direct := mustFail(t, store, `[{"name":"cpu","timestamp":1,"value":2,"labels":{"a":"1,b=2","b":"x"}}]`)
	if got := errorTextAfterJSON(t, direct); got != want {
		t.Fatalf("direct conflict = %q, want %q", got, want)
	}
	// 标签顺序调换：身份与渲染均不变。
	reordered := mustFail(t, store, `[{"name":"cpu","timestamp":1,"value":2,"labels":{"b":"x","a":"1,b=2"}}]`)
	if got := errorTextAfterJSON(t, reordered); got != want {
		t.Fatalf("reordered conflict = %q, want %q", got, want)
	}
	// 值里的逗号改用合法 JSON 转义书写：解析后是同一字符，渲染不变。
	viaEscape := mustFail(t, store, `[{"name":"cpu","timestamp":1,"value":2,"labels":{"b":"x","a":"1`+uescape("002c")+`b=2"}}]`)
	if got := errorTextAfterJSON(t, viaEscape); got != want {
		t.Fatalf("escaped conflict = %q, want %q", got, want)
	}
}

// TestInBatchConflictMessageUsesEarlierPoint：同一批次内两个不同值撞上同一
// 采样点时，冲突原因的原值取该批较早出现的点，失败位置指向造成冲突的采样点，
// 身份渲染同样无歧义。
func TestInBatchConflictMessageUsesEarlierPoint(t *testing.T) {
	store := NewMetricStore()
	lerr := mustFail(t, store, `[
		{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":"1,b=2"}},
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"a":"1,b=2"}}
	]`)
	if lerr.Index != 2 {
		t.Fatalf("index = %d, want 2 (the conflicting sample)", lerr.Index)
	}
	want := `conflict: series cpu{a="1,b=2"} at timestamp 1000 already has value 1, submitted 2`
	if got := errorTextAfterJSON(t, lerr); got != want {
		t.Fatalf("in-batch conflict = %q, want %q", got, want)
	}
	if lerr.Conflict == nil || lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 2 {
		t.Fatalf("structured conflict = %+v", lerr.Conflict)
	}
	// 整批不提交：该序列不存在任何点。
	snap := mustOK(t, store, `[]`)
	if len(snap.Series) != 0 {
		t.Fatalf("rejected batch must commit nothing, got %+v", snap.Series)
	}
}

// TestConflictMessagePlainSeriesUnchanged：不含特殊字符的现有说明保留原样。
func TestConflictMessagePlainSeriesUnchanged(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`)
	lerr := mustFail(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	want := `conflict: series cpu{host=a} at timestamp 1000 already has value 1, submitted 2`
	if got := errorTextAfterJSON(t, lerr); got != want {
		t.Fatalf("plain conflict = %q, want %q", got, want)
	}
}
