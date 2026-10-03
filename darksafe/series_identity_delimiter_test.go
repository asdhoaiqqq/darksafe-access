package darksafe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 本文件回归保障序列身份在“分隔字符”上的正确性。冒号、分号、等号、逗号和花括号
// 经常被用作标签文本表示（如 SeriesRef.String 的 name{k=v,...} 与身份签名内部拼接）
// 的边界，但它们出现在指标名、标签键或标签值里时只是用户数据：身份只由指标名与
// JSON 解析后的完整标签集合决定，标签书写顺序与同一字符的合法 JSON 转义写法都不
// 参与身份判断。普通标签的重复与冲突已在 metrics_test.go 覆盖，这里专补在文本表示
// 中容易互相混淆的合法输入。

// labelsOf 按指标名与完整标签集合在写入快照中定位序列，返回其标签与采样点。
func labelsOf(t *testing.T, res *BatchResult, name string, labels map[string]string) (map[string]string, []Point) {
	t.Helper()
	i := findViewIndex(t, res, name, labels)
	return res.Series[i].Labels, res.Series[i].Points
}

// queryOne 要求查询恰好命中一个序列并返回该条目。
func queryOne(t *testing.T, store *MetricStore, line string) QuerySeries {
	t.Helper()
	res := mustQuery(t, store, line)
	if len(res.Series) != 1 {
		t.Fatalf("query %s: got %d series %+v, want exactly 1", line, len(res.Series), res.Series)
	}
	return res.Series[0]
}

// sample 构造一条采样点 JSON 文本；labels 为 nil 时省略 labels 字段。
// 名称与标签经 encoding/json 转义，因此调用方可以直接给含分隔字符与空格的原始文本。
func sample(name string, ts int64, value any, labels map[string]string) string {
	var b strings.Builder
	n, _ := json.Marshal(name)
	fmt.Fprintf(&b, `{"name":%s,"timestamp":%d,"value":%v`, n, ts, value)
	if labels != nil {
		b.WriteString(`,"labels":`)
		b.WriteString(jsonLabels(labels))
	}
	b.WriteByte('}')
	return b.String()
}

// jsonLabels 把标签集合编码为 JSON 对象文本（Go 按键排序输出，身份与顺序无关）。
func jsonLabels(labels map[string]string) string {
	b, _ := json.Marshal(labels)
	return string(b)
}

// jsonString 把单个字符串编码为 JSON 字符串文本。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// uescape 返回 JSON 的 \uXXXX 转义文本（反斜杠以 rune 构造，避免与源码转义混淆）。
func uescape(hex string) string {
	return string(rune(92)) + "u" + hex
}

// withJSONEscapes 把模板中的占位符替换为对应字符的 JSON \uXXXX 转义书写，
// 用于以“转义写法”提交与直接书写完全相同的真实文本。
func withJSONEscapes(tmpl string) string {
	return strings.NewReplacer(
		"#CO#", uescape("003a"), // ':'
		"#SC#", uescape("003b"), // ';'
		"#LB#", uescape("007b"), // '{'
		"#RB#", uescape("007d"), // '}'
	).Replace(tmpl)
}

// queryLabels 构造按标签子集查询的 JSON 对象文本。
func queryLabels(name string, start, end int64, labels map[string]string) string {
	return `{"op":"query","name":` + jsonString(name) +
		fmt.Sprintf(`,"start":%d,"end":%d,"labels":`, start, end) + jsonLabels(labels) + `}`
}

// TestDelimitersInLabelValueStayDistinct 是核心回归：一个标签值看起来像另外两个
// 标签（"b=2"），另一个标签值看起来包含标签对分隔符（"b=2,c=3"）。冒号、分号、
// 等号、逗号和花括号在值里都是普通字符，不能被当成键值或标签之间的边界。
func TestDelimitersInLabelValueStayDistinct(t *testing.T) {
	store := NewMetricStore()
	// 三条序列在同一时间戳写入相同数值 1，但完整标签集合不同，必须分别新增。
	// single:  一个标签，其值文本上像 "b=2"。
	// pair:    两个标签，文本拼起来恰好也是 a=1,b=2。
	// spill:   一个标签，其值文本上像两个标签 "b=2,c=3"。
	res := mustOK(t, store, `[
		{"name":"m","timestamp":10,"value":1,"labels":{"a":"b=2"}},
		{"name":"m","timestamp":10,"value":1,"labels":{"a":"1","b":"2"}},
		{"name":"m","timestamp":10,"value":1,"labels":{"a":"b=2,c=3"}}
	]`)
	if res.Added != 3 || res.Duplicates != 0 || len(res.Series) != 3 {
		t.Fatalf("look-alike label text must create 3 series, got %+v", res)
	}

	// 成功结果保留各自完整的名称、标签与采样点（值里的等号/逗号原样保留）。
	single := map[string]string{"a": "b=2"}
	pair := map[string]string{"a": "1", "b": "2"}
	spill := map[string]string{"a": "b=2,c=3"}
	for _, want := range []struct {
		labels map[string]string
		point  Point
	}{
		{single, Point{Timestamp: 10, Value: 1}},
		{pair, Point{Timestamp: 10, Value: 1}},
		{spill, Point{Timestamp: 10, Value: 1}},
	} {
		gotLabels, gotPoints := labelsOf(t, res, "m", want.labels)
		if !reflect.DeepEqual(gotLabels, want.labels) {
			t.Fatalf("labels not preserved verbatim: got %v want %v", gotLabels, want.labels)
		}
		if len(gotPoints) != 1 || gotPoints[0] != want.point {
			t.Fatalf("points for %v = %+v, want %+v", want.labels, gotPoints, want.point)
		}
	}

	// 子集查询各匹配各的序列：标签文本里的 "b=2" 片段不能被当成另一个标签 b。
	if got := queryOne(t, store, `{"op":"query","name":"m","start":0,"end":100,"labels":{"a":"b=2"}}`); !reflect.DeepEqual(got.Labels, single) || got.Count != 1 || got.Average != 1 {
		t.Fatalf("a=b=2 query = %+v, want the single-label series", got)
	}
	if got := queryOne(t, store, `{"op":"query","name":"m","start":0,"end":100,"labels":{"b":"2"}}`); !reflect.DeepEqual(got.Labels, pair) || got.Count != 1 {
		t.Fatalf("b=2 subset must match only the real two-label series, got %+v", got)
	}
	if len(mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100,"labels":{"c":"3"}}`).Series) != 0 {
		t.Fatalf("value fragment c=3 must not be read as a label")
	}
	// 全量查询仍是三条独立序列，各自一个点，点数与均值互不混淆。
	all := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(all.Series) != 3 {
		t.Fatalf("full query = %+v, want 3 separate series", all.Series)
	}
	for _, s := range all.Series {
		if s.Count != 1 || s.Average != 1 {
			t.Fatalf("series %v must keep its own single point, got count=%d average=%v", s.Labels, s.Count, s.Average)
		}
	}

	// 三条序列写入彼此不同的数值也不冲突；各序列各取自己的均值。
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":5,"labels":{"a":"b=2"}}]`)
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":7,"labels":{"a":"1","b":"2"}}]`)
	mustOK(t, store, `[{"name":"m","timestamp":20,"value":9,"labels":{"a":"b=2,c=3"}}]`)
	if got := queryOne(t, store, `{"op":"query","name":"m","start":0,"end":100,"labels":{"a":"b=2"}}`); got.Count != 2 || got.Average != 3 {
		t.Fatalf("single series average = count %d avg %v, want 2 points avg 3", got.Count, got.Average)
	}
	if got := queryOne(t, store, `{"op":"query","name":"m","start":0,"end":100,"labels":{"a":"1","b":"2"}}`); got.Count != 2 || got.Average != 4 {
		t.Fatalf("pair series average = count %d avg %v, want 2 points avg 4", got.Count, got.Average)
	}
	if got := queryOne(t, store, `{"op":"query","name":"m","start":0,"end":100,"labels":{"a":"b=2,c=3"}}`); got.Count != 2 || got.Average != 5 {
		t.Fatalf("spill series average = count %d avg %v, want 2 points avg 5", got.Count, got.Average)
	}
}

// TestDelimitersAcrossKeysValuesAndNames 把全部分隔字符放进标签键与标签值，
// 并让指标名本身也含分隔字符；任何字符都不得充当边界导致身份合并。
func TestDelimitersAcrossKeysValuesAndNames(t *testing.T) {
	store := NewMetricStore()
	tricky := []string{":", ";", "=", ",", "{", "}", "{=", "=}", ",;", ":=;{}", "x{y=z,w}"}
	batch := make([]string, 0, 2*len(tricky)+2)
	// 同名、标签键含分隔字符：每个键一条独立序列。
	for i, k := range tricky {
		batch = append(batch, sample("cpu", 100, i+1, map[string]string{k: "plain"}))
	}
	// 标签值含分隔字符的一组。
	for i, v := range tricky {
		batch = append(batch, sample("cpu", 100, i+100, map[string]string{"k": v}))
	}
	// 指标名含分隔字符，与名为 cpu 的序列区分开。
	batch = append(batch,
		sample("cpu:rate{host=a}", 100, 7, nil),
		sample("cpu:rate{host=a", 100, 8, nil), // 少一个花括号也是另一个名字
	)
	res := mustOK(t, store, "["+strings.Join(batch, ",")+"]")
	if want := 2*len(tricky) + 2; res.Added != want || len(res.Series) != want {
		t.Fatalf("delimiter-heavy identities: added=%d series=%d, want %d", res.Added, len(res.Series), want)
	}

	// 键含分号/冒号的标签仍能精确子集查询。
	for _, k := range tricky {
		line := queryLabels("cpu", 0, 200, map[string]string{k: "plain"})
		got := queryOne(t, store, line)
		if v, ok := got.Labels[k]; !ok || v != "plain" || len(got.Labels) != 1 {
			t.Fatalf("delimiter key %q not matched verbatim: %+v", k, got)
		}
	}
	// 值含花括号/逗号等的标签同样原样匹配。
	got := queryOne(t, store, queryLabels("cpu", 0, 200, map[string]string{"k": "x{y=z,w}"}))
	if got.Labels["k"] != "x{y=z,w}" || got.Average != float64(100+len(tricky)-1) {
		t.Fatalf("delimiter value not preserved/matched: %+v", got)
	}
	// 指标名中的分隔字符是名字的一部分：两个花括号形态不同的名字各自独立。
	for _, c := range []struct {
		name string
		val  float64
	}{
		{"cpu:rate{host=a}", 7},
		{"cpu:rate{host=a", 8},
	} {
		line := `{"op":"query","name":` + jsonString(c.name) + `,"start":0,"end":200}`
		s := queryOne(t, store, line)
		if s.Name != c.name || s.Average != c.val || len(s.Labels) != 0 {
			t.Fatalf("delimiter-in-name query = %+v, want name %q value %v", s, c.name, c.val)
		}
	}
}

// TestSameConcatenatedTextDifferentKeySplit 针对长度前缀身份签名自身的边界：
// 若把键值直接拼接，冒号/分号会被当成边界，使不同的键值划分产生同一段文本。
// 这些输入在朴素拼接表示下两两碰撞，但必须是四条不同序列：
//
//	A {"a":"b:c"} 与 B {"a:b":"c"}：朴素拼接都是 a:b:c
//	C {"a":"b;x:y"} 与 D {"a":"b","x":"y"}（值看起来像两个标签）：朴素拼接都是 a:b;x:y
func TestSameConcatenatedTextDifferentKeySplit(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1,"labels":{"a":"b:c"}},
		{"name":"m","timestamp":1,"value":2,"labels":{"a:b":"c"}},
		{"name":"m","timestamp":1,"value":3,"labels":{"a":"b;x:y"}},
		{"name":"m","timestamp":1,"value":4,"labels":{"a":"b","x":"y"}}
	]`)
	if res.Added != 4 || len(res.Series) != 4 {
		t.Fatalf("same concatenated text, different key/value splits must stay distinct: %+v", res)
	}
	want := []map[string]string{
		{"a": "b:c"},
		{"a:b": "c"},
		{"a": "b;x:y"},
		{"a": "b", "x": "y"},
	}
	all := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":10}`)
	if len(all.Series) != 4 {
		t.Fatalf("query = %+v, want 4 series", all.Series)
	}
	for i, labels := range want {
		found := false
		for _, s := range all.Series {
			if reflect.DeepEqual(s.Labels, labels) {
				found = true
				if s.Count != 1 || s.Average != float64(i+1) {
					t.Fatalf("series %v must keep its own point, got count=%d average=%v",
						labels, s.Count, s.Average)
				}
			}
		}
		if !found {
			t.Fatalf("series %v missing from query result %+v (case %d)", labels, all.Series, i)
		}
	}
	// 子集匹配按真实键值对：冒号在键里与在值里是不同的标签。
	if got := queryOne(t, store, queryLabels("m", 0, 10, map[string]string{"a:b": "c"})); got.Average != 2 {
		t.Fatalf("key-with-colon label match = %+v, want value 2", got)
	}
	if got := queryOne(t, store, queryLabels("m", 0, 10, map[string]string{"a": "b:c"})); got.Average != 1 {
		t.Fatalf("value-with-colon label match = %+v, want value 1", got)
	}
	// "x=y" 只真实存在于两标签序列；单标签值里的 "x:y" 片段不产生标签 x。
	if got := queryOne(t, store, queryLabels("m", 0, 10, map[string]string{"x": "y"})); got.Average != 4 {
		t.Fatalf("x=y must match only the real two-label series, got %+v", got)
	}
}

// TestDelimiterIdentityDuplicateAcrossOrderAndEscapes：身份确实相同时，调换标签
// 书写顺序、用合法 JSON 转义书写同一个分隔字符，再次提交相同时间戳与数值都计为
// 重复，不新增点；成功结果里字符仍是解析后的真实文本（只有一个真实冒号）。
func TestDelimiterIdentityDuplicateAcrossOrderAndEscapes(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m:x","timestamp":5,"value":4,"labels":{"a:1":"x;y","b":"{z}"}}]`)
	// 标签顺序调换；名字里的冒号、键里的冒号、值里的分号与花括号改用 JSON
	// #XX# 占位符对应的 \uXXXX 转义书写，解析后的真实文本必须与首条完全一致。
	resubmit := withJSONEscapes(
		`[{"name":"m#CO#x","timestamp":5,"value":4.0,"labels":{"b":"#LB#z#RB#","a#CO#1":"x#SC#y"}}]`)
	res := mustOK(t, store, resubmit)
	if res.Added != 0 || res.Duplicates != 1 || len(res.Series) != 1 {
		t.Fatalf("escaped/reordered resubmit must be one duplicate, got %+v", res)
	}
	got := res.Series[0]
	wantLabels := map[string]string{"a:1": "x;y", "b": "{z}"}
	if got.Name != "m:x" || !reflect.DeepEqual(got.Labels, wantLabels) {
		t.Fatalf("identity must resolve to parsed text, got name=%q labels=%v", got.Name, got.Labels)
	}
	if pts := got.Points; len(pts) != 1 || pts[0] != (Point{Timestamp: 5, Value: 4}) {
		t.Fatalf("duplicate must add no point, got %+v", pts)
	}

	// 同一时间戳换成不同数值（同样以转义+乱序书写身份）：拒绝整批，冲突指向
	// JSON 解析后的真实完整序列、时间戳以及已有值与提交值。
	conflictLine := withJSONEscapes(
		`[{"name":"m#CO#x","timestamp":5,"value":40,"labels":{"b":"#LB#z#RB#","a#CO#1":"x#SC#y"}}]`)
	lerr := mustFail(t, store, conflictLine)
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("expected conflict, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "m:x" || !reflect.DeepEqual(c.Series.Labels, wantLabels) ||
		c.Timestamp != 5 || c.Existing != 4 || c.Submitted != 40 {
		t.Fatalf("conflict must name real identity/values, got %+v", c)
	}
	// 冲突原因文本里的序列按 name{k=v,...} 呈现；该表示对含分隔字符的数据有歧义
	// （单标签 a=b=2 与两标签 a=1,b=2 的渲染相同），因此冲突的权威归属以结构化
	// conflict.series 为准。这里锁定现有文本格式不被意外改动。
	if !strings.HasPrefix(lerr.Error, "conflict: series m:x{a:1=x;y,b={z}} at timestamp 5 already has value 4, submitted 40") {
		t.Fatalf("conflict message = %q", lerr.Error)
	}
	// 原采样点保持不变。
	snap := mustOK(t, store, `[]`)
	if pts := snap.Series[0].Points; len(pts) != 1 || pts[0].Value != 4 {
		t.Fatalf("rejected batch must leave original point intact, got %+v", pts)
	}
}

// TestLookAlikeIdentitiesDoNotConflict：两条文本渲染完全相同的不同序列，在同一
// 时间戳分别提交不同数值不得互相冲突；随后对其中一条再改值才冲突，且冲突报告的
// 是结构化的真实完整标签集合，而不是有歧义的渲染文本。
func TestLookAlikeIdentitiesDoNotConflict(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"cpu","timestamp":1,"value":1,"labels":{"a":"b=2"}}]`)
	// 单标签 a="b=2" 与两标签 a=1,b=2 的 String() 都是 cpu{a=b=2}，但身份不同。
	res := mustOK(t, store, `[{"name":"cpu","timestamp":1,"value":2,"labels":{"a":"1","b":"2"}}]`)
	if res.Added != 1 || res.Duplicates != 0 || len(res.Series) != 2 {
		t.Fatalf("look-alike identities with different values must both be added, got %+v", res)
	}
	// 对单标签序列提交不同值：冲突，结构化标签只有一个键且值原样为 "b=2"。
	lerr := mustFail(t, store, `[{"name":"cpu","timestamp":1,"value":9,"labels":{"a":"b=2"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 9 {
		t.Fatalf("single-label conflict values wrong: %+v", lerr.Conflict)
	}
	if labels := lerr.Conflict.Series.Labels; len(labels) != 1 || labels["a"] != "b=2" {
		t.Fatalf("conflict must name real single-label identity, got %v", labels)
	}
	// 对两标签序列提交不同值同样冲突，结构化标签是两个键。
	lerr = mustFail(t, store, `[{"name":"cpu","timestamp":1,"value":9,"labels":{"a":"1","b":"2"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 2 {
		t.Fatalf("two-label conflict values wrong: %+v", lerr.Conflict)
	}
	if labels := lerr.Conflict.Series.Labels; len(labels) != 2 || labels["a"] != "1" || labels["b"] != "2" {
		t.Fatalf("conflict must name real two-label identity, got %v", labels)
	}
	// 两条序列原有采样点都保持各自的值，全量查询仍返回两条独立序列。
	full := mustQuery(t, store, `{"op":"query","name":"cpu","start":0,"end":10}`)
	if len(full.Series) != 2 {
		t.Fatalf("full cpu query = %+v, want 2 series", full.Series)
	}
	for _, s := range full.Series {
		if s.Count != 1 {
			t.Fatalf("series %v must keep its own point, got count=%d", s.Labels, s.Count)
		}
	}
}

// TestWhitespaceInIdentityPreservedVerbatim：名称、标签键和值中的合法空格原样
// 保留，不能通过去空格把不同输入合并为同一序列。
func TestWhitespaceInIdentityPreservedVerbatim(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[
		{"name":"m ","timestamp":1,"value":1,"labels":{"k ":"v"}},
		{"name":"m","timestamp":1,"value":2,"labels":{"k":"v"}},
		{"name":"m","timestamp":1,"value":3,"labels":{"k":" v"}},
		{"name":"m","timestamp":1,"value":4,"labels":{"k":"v "}},
		{"name":"m","timestamp":1,"value":5,"labels":{" k":"v"}}
	]`)
	if res.Added != 5 || len(res.Series) != 5 {
		t.Fatalf("whitespace variants must be 5 distinct series, got added=%d series=%d", res.Added, len(res.Series))
	}
	cases := []struct {
		name   string
		labels map[string]string
		value  float64
	}{
		{"m ", map[string]string{"k ": "v"}, 1},
		{"m", map[string]string{"k": "v"}, 2},
		{"m", map[string]string{"k": " v"}, 3},
		{"m", map[string]string{"k": "v "}, 4},
		{"m", map[string]string{" k": "v"}, 5},
	}
	for i, c := range cases {
		var line string
		if c.name == "m" {
			line = queryLabels("m", 0, 10, c.labels)
		} else {
			line = `{"op":"query","name":` + jsonString(c.name) + `,"start":0,"end":10,"labels":` + jsonLabels(c.labels) + `}`
		}
		got := queryOne(t, store, line)
		if got.Name != c.name || !reflect.DeepEqual(got.Labels, c.labels) || got.Average != c.value {
			t.Fatalf("whitespace case %d: got %+v, want name=%q labels=%v value=%v", i, got, c.name, c.labels, c.value)
		}
	}
	// 省略标签与空标签对象同一身份；缺标签与标签存在且值为空串仍是不同身份，
	// 即使标签键周围带有合法空格。
	mustOK(t, store, `[{"name":"n","timestamp":1,"value":1}]`)
	if dup := mustOK(t, store, `[{"name":"n","timestamp":1,"value":1.0,"labels":{}}]`); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("omitted vs empty labels must be the same series, got %+v", dup)
	}
	if added := mustOK(t, store, `[{"name":"n","timestamp":1,"value":2,"labels":{" z ":""}}]`); added.Added != 1 {
		t.Fatalf("missing label differs from present empty value, added = %d", added.Added)
	}
	// 快照中 n 现在有两条独立序列：无标签与带空值标签（连同 m 的五条共七条）。
	nSeries := 0
	for _, v := range mustOK(t, store, `[]`).Series {
		if v.Name == "n" {
			nSeries++
		}
	}
	if nSeries != 2 {
		t.Fatalf("metric n must have 2 distinct series, got %d", nSeries)
	}
	got := queryOne(t, store, queryLabels("n", 0, 10, map[string]string{" z ": ""}))
	if len(got.Labels) != 1 || got.Labels[" z "] != "" || got.Average != 2 {
		t.Fatalf("spaced-key empty-value label not preserved: %+v", got)
	}
}

// TestDelimiterLabelSubsetMatching：子集匹配按真实键值对进行，值里的分隔符既不会
// 制造额外的匹配标签，也不会让需要的标签匹配失败；不同序列的点不混合。
func TestDelimiterLabelSubsetMatching(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":10,"labels":{"a":"x","b":"y=z"}},
		{"name":"m","timestamp":1,"value":20,"labels":{"a":"x","b":"q"}},
		{"name":"m","timestamp":1,"value":30,"labels":{"a":"x","b":"y=z","c":"d,e"}}
	]`)
	// a=x 子集命中三条，各自独立返回自己的点与均值。
	res := mustQuery(t, store, queryLabels("m", 0, 10, map[string]string{"a": "x"}))
	if len(res.Series) != 3 {
		t.Fatalf("a=x subset = %+v, want 3 series", res.Series)
	}
	// b="y=z" 只命中真实拥有该键值的两条（含带额外标签 c 的一条），不命中 b=q。
	res = mustQuery(t, store, queryLabels("m", 0, 10, map[string]string{"b": "y=z"}))
	if len(res.Series) != 2 {
		t.Fatalf("b=y=z subset = %+v, want 2 series", res.Series)
	}
	wantAvgs := map[int]bool{10: false, 30: false}
	for _, s := range res.Series {
		if s.Count != 1 {
			t.Fatalf("matched series %v must report its own single point, got count=%d", s.Labels, s.Count)
		}
		if _, ok := wantAvgs[int(s.Average)]; !ok {
			t.Fatalf("unexpected average %v from %v", s.Average, s.Labels)
		}
		wantAvgs[int(s.Average)] = true
	}
	for v, seen := range wantAvgs {
		if !seen {
			t.Fatalf("series with average %d was not matched separately", v)
		}
	}
	// 查询值里写逗号文本是完整值比较：只有值恰好为 "d,e" 的标签能匹配。
	if got := queryOne(t, store, queryLabels("m", 0, 10, map[string]string{"c": "d,e"})); got.Average != 30 {
		t.Fatalf("comma-in-value subset = %+v, want average 30", got)
	}
	if len(mustQuery(t, store, queryLabels("m", 0, 10, map[string]string{"d": "e"})).Series) != 0 {
		t.Fatalf("value fragment d,e must not be split into label d=e")
	}
}
