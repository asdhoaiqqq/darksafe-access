package darksafe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// 本文件回归保障写入快照与区间查询中的序列输出顺序。
//
// 顺序只由“解析后的真实身份”决定：先按指标名原始字符串字典序，同名序列先把
// 标签按键排序，再逐对比较键和值（先比键、再比值，前一对完全相同才比下一对）；
// 一套标签是另一套完整前缀时较短者在前，无标签序列因此在同名序列中最前。
// 这里专补容易被错误实现改写次序的合法身份：
//   - 看起来是数字的标签值/键（"10" 与 "2"）：必须按字符串比较，"10" 在 "2" 前，
//     不能按数值大小排；键 "aa" 与 "b" 同理，"aa" 在 "b" 前，字符多不排后。
//   - 仅文本长度不同的字符串：比较的是字节内容，长度与标签对数都不参与。
//   - 名称、键、值里的逗号、等号、引号、反斜杠与合法中文：排序只看 JSON 解析后的
//     原始字符串（按字节）；冲突说明为区分身份而加的引号与转义文字不参与排序。
//   - 同一身份直接书写或用合法 JSON \uXXXX 转义书写：解析后次序一致。
//   - 空字符串值是真实存在的标签，不得被删去后与无标签序列混在一起。
//
// metrics_test.go 已覆盖单标签普通字符的排序，这里使用多标签、前缀、数字外观、
// 引号/反斜杠与中文身份给出能区分上述规则的实际结果。

// orderedIdentity 是序列在输出中的期望位置：指标名、完整标签集合。
type orderedIdentity struct {
	name   string
	labels map[string]string
}

// snapshotOrder 提取写入快照中的序列身份次序。
func snapshotOrder(res *BatchResult) []orderedIdentity {
	out := make([]orderedIdentity, len(res.Series))
	for i, s := range res.Series {
		out[i] = orderedIdentity{name: s.Name, labels: s.Labels}
	}
	return out
}

// queryOrder 提取查询结果中的序列身份次序。
func queryOrder(res *QueryResult) []orderedIdentity {
	out := make([]orderedIdentity, len(res.Series))
	for i, s := range res.Series {
		out[i] = orderedIdentity{name: s.Name, labels: s.Labels}
	}
	return out
}

// assertSameOrder 校验两份身份次序逐项相同（名称与完整标签集合都一致）。
func assertSameOrder(t *testing.T, got, want []orderedIdentity, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: order length = %d, want %d\n got %v\nwant %v",
			context, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].name != want[i].name || !reflect.DeepEqual(got[i].labels, want[i].labels) {
			t.Fatalf("%s: position %d = %v, want %v\nfull got %v\nfull want %v",
				context, i, got[i], want[i], got, want)
		}
	}
}

// uHex 返回 JSON \uXXXX 转义文本（反斜杠以 rune 构造，避免源码转义混淆）。
func uHex(hex string) string { return string(rune(92)) + "u" + hex }

// orderSamples 是同时用于“顺序规则”与“提交顺序无关”两组回归的 16 条采样点，
// 全部在时间戳 1 写入。每条 value 与其在数组中的位置相关但身份互不相同。
// 指标名、键、值中的引号、反斜杠、逗号、等号与中文都只按解析后的原始字符串比较：
// 冲突说明对这些字符加的引号与转义文字不参与排序。
var orderSamples = []string{
	`{"name":"m","timestamp":1,"value":1}`,                                        // 0 无标签
	`{"name":"m","timestamp":1,"value":2,"labels":{"k":""}}`,                      // 1 空字符串值
	`{"name":"m","timestamp":1,"value":3,"labels":{"k":"10"}}`,                    // 2 数字外观值
	`{"name":"m","timestamp":1,"value":4,"labels":{"k":"2"}}`,                     // 3
	`{"name":"m","timestamp":1,"value":5,"labels":{"k":"2","z":"10"}}`,            // 4
	`{"name":"m","timestamp":1,"value":6,"labels":{"k":"2","z":"2"}}`,             // 5
	`{"name":"m","timestamp":1,"value":7,"labels":{"k":"2","z":"9"}}`,             // 6
	`{"name":"m","timestamp":1,"value":8,"labels":{"aa":"x"}}`,                    // 7 双键名
	`{"name":"m","timestamp":1,"value":9,"labels":{"b":"x"}}`,                     // 8
	`{"name":"m","timestamp":1,"value":10,"labels":{"k":"\"q\""}}`,                // 9 值含引号
	`{"name":"m","timestamp":1,"value":11,"labels":{"k":"\\s"}}`,                  // 10 值含反斜杠
	`{"name":"m","timestamp":1,"value":12,"labels":{"k":"中","z":"a,b=c\\d\"e中"}}`, // 11 中文与分隔字符
	`{"name":"mz","timestamp":1,"value":13}`,                                      // 12
	`{"name":"m中","timestamp":1,"value":14}`,                                      // 13
	`{"name":"指标","timestamp":1,"value":15,"labels":{"a":"10","b":"2"}}`,          // 14
	`{"name":"m2","timestamp":1,"value":16}`,                                      // 15
}

// canonicalOrder 是 orderSamples 的规范输出次序。
//
//	同名 m 内先比第一对的键：无标签在最前，随后键 "aa"(0x61) < "b"(0x62) < "k"(0x6B)；
//	键都是 k 时比值，原始字节："" < "\"q\""(0x22) < "10"(0x31) < "2"(0x32) <
//	"\\s"(0x5C) < "中"(0xE4)；同值 "2" 时较短集合在前，随后按 z 的值 "10"<"2"<"9"。
//	指标名按字节："m" 是前缀最先，之后 "m2"(…32) < "mz"(…7A) < "m中"(…E4)，
//	首字节 0x6D 的都在首字节 0xE6 的“指标”之前。
func canonicalOrder() []orderedIdentity {
	return []orderedIdentity{
		{"m", map[string]string{}},                            // 0
		{"m", map[string]string{"aa": "x"}},                   // 7
		{"m", map[string]string{"b": "x"}},                    // 8
		{"m", map[string]string{"k": ""}},                     // 1
		{"m", map[string]string{"k": `"q"`}},                  // 9
		{"m", map[string]string{"k": "10"}},                   // 2
		{"m", map[string]string{"k": "2"}},                    // 3
		{"m", map[string]string{"k": "2", "z": "10"}},         // 4
		{"m", map[string]string{"k": "2", "z": "2"}},          // 5
		{"m", map[string]string{"k": "2", "z": "9"}},          // 6
		{"m", map[string]string{"k": `\s`}},                   // 10
		{"m", map[string]string{"k": "中", "z": `a,b=c\d"e中`}}, // 11
		{"m2", map[string]string{}},                           // 15
		{"mz", map[string]string{}},                           // 12
		{"m中", map[string]string{}},                           // 13
		{"指标", map[string]string{"a": "10", "b": "2"}},        // 14
	}
}

func joinSamples(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func reverseStrings(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

// TestSnapshotOrderIndependentOfSubmissionOrder：相同数据以不同顺序（逐条多批、
// 单批整体反转、交错乱序多批）提交后，成功写入返回的全量快照给出完全相同的序列排列。
func TestSnapshotOrderIndependentOfSubmissionOrder(t *testing.T) {
	want := canonicalOrder()

	// 顺序 A：按输入数组次序逐条提交到不同批次。
	storeA := NewMetricStore()
	for _, s := range orderSamples {
		mustOK(t, storeA, "["+s+"]")
	}
	orderA := snapshotOrder(mustOK(t, storeA, `[]`))
	assertSameOrder(t, orderA, want, "one-sample-per-batch snapshot")

	// 顺序 B：单批、数组顺序整体反转。
	storeB := NewMetricStore()
	mustOK(t, storeB, "["+joinSamples(reverseStrings(orderSamples))+"]")
	assertSameOrder(t, snapshotOrder(mustOK(t, storeB, `[]`)), want, "reversed single-batch snapshot")

	// 顺序 C：交错乱序并拆成大小不一的多个批次。
	storeC := NewMetricStore()
	shuffled := []int{11, 2, 14, 0, 7, 15, 4, 9, 12, 1, 6, 13, 3, 8, 10, 5}
	committed := 0
	for _, batch := range [][]int{shuffled[:3], shuffled[3:9], shuffled[9:10], shuffled[10:]} {
		parts := make([]string, len(batch))
		for j, k := range batch {
			parts[j] = orderSamples[k]
		}
		res := mustOK(t, storeC, "["+joinSamples(parts)+"]")
		committed += len(batch)
		// 每次成功结果都是截至当时的全量快照，序列数随提交单调增加。
		if len(res.Series) != committed {
			t.Fatalf("after %d submissions snapshot has %d series, want %d", committed, len(res.Series), committed)
		}
	}
	assertSameOrder(t, snapshotOrder(mustOK(t, storeC, `[]`)), want, "shuffled multi-batch snapshot")

	// 三种提交路径的最终快照身份次序彼此一致。
	assertSameOrder(t, orderA, snapshotOrder(mustOK(t, storeB, `[]`)), "storeA vs storeB")
	assertSameOrder(t, orderA, snapshotOrder(mustOK(t, storeC, `[]`)), "storeA vs storeC")
}

// TestSnapshotOrderRawStringNotNumericOrDisplay：固定容易被错误实现改写的相邻关系：
// 数字外观按字符串、长度不参与、前缀较短在前、引号/反斜杠/中文按原始字节、
// 无标签与空值标签区分、指标名按字节。
func TestSnapshotOrderRawStringNotNumericOrDisplay(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+joinSamples(orderSamples)+"]")
	got := snapshotOrder(mustOK(t, store, `[]`))
	assertSameOrder(t, got, canonicalOrder(), "raw-string snapshot")

	indexOf := func(id orderedIdentity) int {
		for i, g := range got {
			if g.name == id.name && reflect.DeepEqual(g.labels, id.labels) {
				return i
			}
		}
		t.Fatalf("identity %v missing from snapshot", id)
		return -1
	}
	pairChecks := []struct {
		earlier, later orderedIdentity
		rule           string
	}{
		{orderedIdentity{"m", map[string]string{}}, orderedIdentity{"m", map[string]string{"aa": "x"}},
			"no-label series precedes every same-name labeled series"},
		{orderedIdentity{"m", map[string]string{"b": "x"}}, orderedIdentity{"m", map[string]string{"k": ""}},
			`first pair compares keys ("b" < "k") before any value, so b-series precedes even empty-value k-series`},
		{orderedIdentity{"m", map[string]string{"aa": "x"}}, orderedIdentity{"m", map[string]string{"b": "x"}},
			`keys compare as strings: "aa" before "b" (more characters do not sort later)`},
		{orderedIdentity{"m", map[string]string{"k": ""}}, orderedIdentity{"m", map[string]string{"k": `"q"`}},
			"empty value is a real label and precedes non-empty value"},
		{orderedIdentity{"m", map[string]string{"k": `"q"`}}, orderedIdentity{"m", map[string]string{"k": "10"}},
			`display quotes are raw byte 0x22 and sort before digit 0x31; quoting is not part of ordering`},
		{orderedIdentity{"m", map[string]string{"k": "10"}}, orderedIdentity{"m", map[string]string{"k": "2"}},
			`values compare as strings: "10" before "2" (not numeric 2 < 10)`},
		{orderedIdentity{"m", map[string]string{"k": "2"}}, orderedIdentity{"m", map[string]string{"k": `\s`}},
			`"2"(0x32) sorts before backslash value "\\s"(0x5C); length does not matter`},
		{orderedIdentity{"m", map[string]string{"k": "2"}}, orderedIdentity{"m", map[string]string{"k": "2", "z": "9"}},
			"a full-prefix label set sorts first regardless of pair count"},
		{orderedIdentity{"m", map[string]string{"k": "2", "z": "10"}}, orderedIdentity{"m", map[string]string{"k": "2", "z": "2"}},
			`later pairs decide only after the first pair matches: z "10" before z "2"`},
		{orderedIdentity{"m", map[string]string{"k": `\s`}}, orderedIdentity{"m", map[string]string{"k": "中", "z": `a,b=c\d"e中`}},
			"ASCII backslash (0x5C) sorts before Chinese first byte (0xE4)"},
		{orderedIdentity{"m2", map[string]string{}}, orderedIdentity{"mz", map[string]string{}},
			`names compare by bytes: second byte '2'(0x32) < 'z'(0x7A)`},
		{orderedIdentity{"mz", map[string]string{}}, orderedIdentity{"m中", map[string]string{}},
			`names compare by bytes: 'z'(0x7A) < Chinese byte (0xE4), so "mz" before "m中"`},
		{orderedIdentity{"m中", map[string]string{}}, orderedIdentity{"指标", map[string]string{"a": "10", "b": "2"}},
			`ASCII-leading name "m中"(0x6D) precedes Chinese-leading "指标"(0xE6)`},
	}
	for _, c := range pairChecks {
		if indexOf(c.earlier) >= indexOf(c.later) {
			t.Fatalf("order rule violated (%s): %v must precede %v", c.rule, c.earlier, c.later)
		}
	}
}

// TestSnapshotOrderSameWithJSONEscapes：特殊字符改用合法 JSON \uXXXX 转义书写、标签
// 顺序打乱后，解析回的身份与输出次序与直接书写完全一致；等值重提计为重复而非新增。
func TestSnapshotOrderSameWithJSONEscapes(t *testing.T) {
	// 转义+乱序提交 5 条含特殊字符的身份（orderSamples 下标 9、10、11、13、14）。
	escaped := []string{
		// 中文指标名“指标”用 指 标 书写，标签书写顺序为 b 在前 a 在后。
		`{"name":"` + uHex("6307") + uHex("6807") + `","timestamp":1,"value":15,"labels":{"b":"2","a":"10"}}`,
		// 值中的引号改用 " 书写：k="q"（转义位于字符串引号内部）。
		`{"name":"m","timestamp":1,"value":10,"labels":{"k":"` + uHex("0022") + `q` + uHex("0022") + `"}}`,
		// 值中的反斜杠改用 \ 书写：k=\s。
		`{"name":"m","timestamp":1,"value":11,"labels":{"k":"` + uHex("005c") + `s"}}`,
		// m中：中 用 中 书写。
		`{"name":"m` + uHex("4e2d") + `","timestamp":1,"value":14}`,
		// 混合值 a,b=c\d"e中 中的逗号、等号、反斜杠、引号、中文全部转义；
		// 标签 z 在前、k 在后（乱序），解析后仍是同一条序列。
		`{"name":"m","timestamp":1,"value":12,"labels":{` +
			`"z":"a` + uHex("002c") + `b` + uHex("003d") + `c` + uHex("005c") + `d` + uHex("0022") + `e` + uHex("4e2d") + `",` +
			`"k":"` + uHex("4e2d") + `"}}`,
	}
	escapedIdx := map[int]bool{9: true, 10: true, 11: true, 13: true, 14: true}

	store := NewMetricStore()
	plain := make([]string, 0, len(orderSamples)-len(escapedIdx))
	for i, s := range orderSamples {
		if !escapedIdx[i] {
			plain = append(plain, s)
		}
	}
	// 普通身份反序单批写入，转义身份再反序一批写入，最终次序仍等于规范次序。
	mustOK(t, store, "["+joinSamples(reverseStrings(plain))+"]")
	mustOK(t, store, "["+joinSamples(reverseStrings(escaped))+"]")
	assertSameOrder(t, snapshotOrder(mustOK(t, store, `[]`)), canonicalOrder(), "escaped-identity snapshot")

	// 直接书写与转义书写确实是同一身份：以与原值相等的数值（整数与其 1.0 形式相等）、
	// 直接书写标签、顺序打乱后再提交这 5 条，全部计为重复而不是新增，次序不变。
	literal := []string{
		`{"name":"指标","timestamp":1,"value":15.0,"labels":{"a":"10","b":"2"}}`,
		`{"name":"m","timestamp":1,"value":10.0,"labels":{"k":"\"q\""}}`,
		`{"name":"m","timestamp":1,"value":11.0,"labels":{"k":"\\s"}}`,
		`{"name":"m中","timestamp":1,"value":14.0}`,
		`{"name":"m","timestamp":1,"value":12.0,"labels":{"k":"中","z":"a,b=c\\d\"e中"}}`,
	}
	dup := mustOK(t, store, "["+joinSamples(literal)+"]")
	if dup.Added != 0 || dup.Duplicates != 5 {
		t.Fatalf("literal resubmit of escaped identities must be 5 duplicates, got %+v", dup)
	}
	assertSameOrder(t, snapshotOrder(dup), canonicalOrder(), "post-duplicate snapshot")
}

// canonicalSecondPoints 为 canonicalOrder 中每条序列补一个时间戳 20 的点，值各不
// 相同且与时间戳 1 的值拉开，使区间过滤与“统计是否串换”都可被观察到。输出次序与
// canonicalOrder 对齐（用 jsonLabels 编码标签，身份与标签书写顺序无关）。
func canonicalSecondPoints() []string {
	// canonical 位置 -> 该序列时间戳 20 的值。
	v20 := []float64{30, 800, 900, 40, 1000, 300, 400, 500, 600, 700, 1100, 1200, 1600, 1300, 1400, 1500}
	want := canonicalOrder()
	out := make([]string, len(want))
	for i, id := range want {
		labels := ""
		if len(id.labels) > 0 {
			labels = `,"labels":` + jsonLabels(id.labels)
		}
		out[i] = fmt.Sprintf(`{"name":%s,"timestamp":20,"value":%v%s}`,
			jsonString(id.name), v20[i], labels)
	}
	return out
}

// TestQueryPreservesSnapshotRelativeOrderAndStats：查询按指标名、标签子集与闭区间
// 过滤后，保留条目仍是全量排序中的同一相对位置，各自携带完整标签、点数与均值，
// 统计值不随次序整理串换；只有区间外点的序列不出现，全部被过滤时返回 []。
func TestQueryPreservesSnapshotRelativeOrderAndStats(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+joinSamples(orderSamples)+"]")
	mustOK(t, store, "["+joinSamples(canonicalSecondPoints())+"]")

	want := canonicalOrder()

	// 快照跨指标给出全部 16 条，名字段次序为 m 的 12 条、m2、mz、m中、指标。
	assertSameOrder(t, snapshotOrder(mustOK(t, store, `[]`)), want, "snapshot with second points")

	// m 的全部序列：闭区间 [0,1000] 覆盖两个时间戳，12 条全部出现且保持全量相对次序。
	mQuery := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":1000}`)
	assertSameOrder(t, queryOrder(mQuery), want[:12], "m full-range order")

	// 每条命中序列携带各自完整标签、点数 2 与独立均值；相邻统计值不得串换。
	wantAvg := []float64{15.5, 404, 454.5, 21, 505, 151.5, 202, 252.5, 303, 353.5, 555.5, 606}
	for i, s := range mQuery.Series {
		if s.Name != "m" || !reflect.DeepEqual(s.Labels, want[i].labels) {
			t.Fatalf("m series[%d] identity = %v, want %v", i, s, want[i])
		}
		if s.Count != 2 {
			t.Fatalf("m series %v count = %d, want 2", s.Labels, s.Count)
		}
		if s.Average != wantAvg[i] {
			t.Fatalf("m series %v average = %v, want %v (stats must not swap when ordering)",
				s.Labels, s.Average, wantAvg[i])
		}
	}

	// 窄区间 [1,1] 只取时间戳 1 的点：仍是同样 12 条、同样次序，均值等于时间戳 1 的值。
	narrow := mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1}`)
	assertSameOrder(t, queryOrder(narrow), want[:12], "m narrow-range order")
	firstVals := []float64{1, 8, 9, 2, 10, 3, 4, 5, 6, 7, 11, 12}
	for i, s := range narrow.Series {
		if s.Count != 1 || s.Average != firstVals[i] {
			t.Fatalf("narrow series %v = count %d avg %v, want count 1 avg %v",
				s.Labels, s.Count, s.Average, firstVals[i])
		}
	}

	// 子集 k="2" 命中四条（单标签 k=2 与其三条两标签扩展），保持全量相对次序：
	// {"k":"2"} < {"k":"2","z":"10"} < {"k":"2","z":"2"} < {"k":"2","z":"9"}，
	// 其中 z 的值 "10"<"2"<"9" 仍是字符串比较。
	k2 := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":1000,"labels":{"k":"2"}}`)
	assertSameOrder(t, queryOrder(k2),
		[]orderedIdentity{
			{"m", map[string]string{"k": "2"}},
			{"m", map[string]string{"k": "2", "z": "10"}},
			{"m", map[string]string{"k": "2", "z": "2"}},
			{"m", map[string]string{"k": "2", "z": "9"}},
		}, "k=2 subset order")
	if k2.Series[0].Average != 202 || k2.Series[1].Average != 252.5 ||
		k2.Series[2].Average != 303 || k2.Series[3].Average != 353.5 {
		t.Fatalf("k=2 subset averages = %v %v %v %v, want 202, 252.5, 303, 353.5",
			k2.Series[0].Average, k2.Series[1].Average, k2.Series[2].Average, k2.Series[3].Average)
	}

	// 标签值按字符串相等匹配，不做数值解释：k="10" 只命中一条，均值独立。
	k10 := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":1000,"labels":{"k":"10"}}`)
	if len(k10.Series) != 1 || !reflect.DeepEqual(k10.Series[0].Labels, map[string]string{"k": "10"}) ||
		k10.Series[0].Count != 2 || k10.Series[0].Average != 151.5 {
		t.Fatalf(`k="10" subset = %+v, want only the string-"10" series with avg 151.5`, k10.Series)
	}

	// [20,20] 只取第二个点：12 条次序不变，均值是各自时间戳 20 的值。
	ts20 := mustQuery(t, store, `{"op":"query","name":"m","start":20,"end":20}`)
	assertSameOrder(t, queryOrder(ts20), want[:12], "m timestamp-20 order")
	secondVals := []float64{30, 800, 900, 40, 1000, 300, 400, 500, 600, 700, 1100, 1200}
	for i, s := range ts20.Series {
		if s.Count != 1 || s.Average != secondVals[i] {
			t.Fatalf("ts20 series %v = count %d avg %v, want count 1 avg %v",
				s.Labels, s.Count, s.Average, secondVals[i])
		}
	}

	// 所有点都在区间外：序列不出现，返回空数组而不是 count=0 的条目。
	outRange := mustQuery(t, store, `{"op":"query","name":"m","start":21,"end":9999}`)
	if len(outRange.Series) != 0 {
		t.Fatalf("out-of-range query = %+v, want empty series array", outRange.Series)
	}
	b, _ := json.Marshal(outRange)
	if string(b) != `{"status":"ok","op":"query","series":[]}` {
		t.Fatalf("out-of-range wire shape = %s, want empty series array", b)
	}
	// 子集 + 区间外：即使标签匹配，区间内无点也不列出。
	outSubset := mustQuery(t, store, `{"op":"query","name":"m","start":21,"end":9999,"labels":{"k":"2"}}`)
	if len(outSubset.Series) != 0 {
		t.Fatalf("out-of-range subset = %+v, want empty", outSubset.Series)
	}
	// 不存在的标签：空数组。
	missingLabel := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":1000,"labels":{"nope":"x"}}`)
	if len(missingLabel.Series) != 0 {
		t.Fatalf("missing-label query = %+v, want empty", missingLabel.Series)
	}

	// 部分序列在区间内：只给 k="2" 三条补时间戳 5000 的点，[4000,6000] 中只有这三条
	// 出现，其他 m 序列因区间内无点整体缺席，三条保持全量相对次序与各自统计。
	mustOK(t, store, `[
		{"name":"m","timestamp":5000,"value":1,"labels":{"k":"2"}},
		{"name":"m","timestamp":5000,"value":2,"labels":{"k":"2","z":"10"}},
		{"name":"m","timestamp":5000,"value":3,"labels":{"k":"2","z":"2"}}
	]`)
	partial := mustQuery(t, store, `{"op":"query","name":"m","start":4000,"end":6000}`)
	assertSameOrder(t, queryOrder(partial),
		[]orderedIdentity{
			{"m", map[string]string{"k": "2"}},
			{"m", map[string]string{"k": "2", "z": "10"}},
			{"m", map[string]string{"k": "2", "z": "2"}},
		}, "partial in-range order")
	if partial.Series[0].Average != 1 || partial.Series[1].Average != 2 || partial.Series[2].Average != 3 {
		t.Fatalf("partial in-range averages = %v %v %v, want 1, 2, 3",
			partial.Series[0].Average, partial.Series[1].Average, partial.Series[2].Average)
	}

	// 跨指标相对次序无法用一条查询表达（name 必填），改为在全量快照上固定四个
	// 指标名严格递增：m2 < mz < m中 < 指标，且都排在 m 的序列之后。
	snap := snapshotOrder(mustOK(t, store, `[]`))
	lastM := -1
	nameAt := map[string]int{}
	for i, id := range snap {
		if id.name == "m" {
			lastM = i
		}
		if _, seen := nameAt[id.name]; !seen {
			nameAt[id.name] = i
		}
	}
	for _, pair := range [][2]string{{"m2", "mz"}, {"mz", "m中"}, {"m中", "指标"}} {
		if !(lastM < nameAt[pair[0]] && nameAt[pair[0]] < nameAt[pair[1]]) {
			t.Fatalf("metric name order violated: all m series at <=%d, %s at %d, %s at %d",
				lastM, pair[0], nameAt[pair[0]], pair[1], nameAt[pair[1]])
		}
	}
	// 每个指标名单独查询仍是该指标的序列，均值与标签不与其他指标串换。
	for _, c := range []struct {
		name string
		avg  float64
	}{
		{"m2", 808}, {"mz", 656.5}, {"m中", 707}, {"指标", 757.5},
	} {
		q := mustQuery(t, store, `{"op":"query","name":`+jsonString(c.name)+`,"start":0,"end":1000}`)
		if len(q.Series) != 1 {
			t.Fatalf("metric %q query = %+v, want 1 series", c.name, q.Series)
		}
		if q.Series[0].Name != c.name || q.Series[0].Average != c.avg {
			t.Fatalf("metric %q = %+v, want average %v", c.name, q.Series[0], c.avg)
		}
	}
}

// TestQueryOrderSameWithJSONEscapes：查询条件中的引号、反斜杠、中文直接书写或用
// 合法 \uXXXX 转义书写，命中相同序列并得到相同次序与统计；说明性引号不影响匹配。
func TestQueryOrderSameWithJSONEscapes(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, "["+joinSamples(orderSamples)+"]")

	// 全量 m 查询中三条特殊身份保持相对先后：引号值(0x22) < 反斜杠值(0x5C) < 中文(0xE4)。
	special := []orderedIdentity{
		{"m", map[string]string{"k": `"q"`}},
		{"m", map[string]string{"k": `\s`}},
		{"m", map[string]string{"k": "中", "z": `a,b=c\d"e中`}},
	}
	got := queryOrder(mustQuery(t, store, `{"op":"query","name":"m","start":1,"end":1}`))
	assertSameOrder(t, pickIdentities(got, special), special, "special-identity relative order")

	// 用全部转义书写的查询条件逐个子集匹配（转义位于值字符串引号内部），命中同一条。
	escapedQueries := []string{
		`{"op":"query","name":"m","start":1,"end":1,"labels":{"k":"` + uHex("0022") + `q` + uHex("0022") + `"}}`,
		`{"op":"query","name":"m","start":1,"end":1,"labels":{"k":"` + uHex("005c") + `s"}}`,
		`{"op":"query","name":"m","start":1,"end":1,"labels":{"k":"` + uHex("4e2d") + `"}}`,
	}
	for i, q := range escapedQueries {
		res := mustQuery(t, store, q)
		if len(res.Series) != 1 {
			t.Fatalf("escaped query %d = %+v, want exactly 1 series", i, res.Series)
		}
		s := res.Series[0]
		if !reflect.DeepEqual(s.Labels, special[i].labels) || s.Count != 1 || s.Average != float64(10+i) {
			t.Fatalf("escaped query %d = %+v, want %v with avg %d", i, s, special[i].labels, 10+i)
		}
	}

	// 中文名指标用 指 标 转义查询，与直接写“指标”命中同一条、同一份完整标签与统计。
	escName := mustQuery(t, store, `{"op":"query","name":"`+uHex("6307")+uHex("6807")+`","start":1,"end":1}`)
	if len(escName.Series) != 1 || escName.Series[0].Name != "指标" ||
		!reflect.DeepEqual(escName.Series[0].Labels, map[string]string{"a": "10", "b": "2"}) ||
		escName.Series[0].Average != 15 {
		t.Fatalf("escaped Chinese metric query = %+v, want 指标 a=10,b=2 avg 15", escName.Series)
	}
	litName := mustQuery(t, store, `{"op":"query","name":"指标","start":1,"end":1}`)
	if !reflect.DeepEqual(litName.Series, escName.Series) {
		t.Fatalf("literal and escaped Chinese metric queries differ: %+v vs %+v", litName.Series, escName.Series)
	}
}

// pickIdentities 按 full 中的出现次序挑出 targets 指定的身份（targets 的标签集合
// 必须都存在），用于验证过滤后相对位置不变。
func pickIdentities(full, targets []orderedIdentity) []orderedIdentity {
	wantSet := make(map[string]bool, len(targets))
	for _, tgt := range targets {
		wantSet[identityKey(tgt)] = true
	}
	out := make([]orderedIdentity, 0, len(targets))
	for _, id := range full {
		if wantSet[identityKey(id)] {
			out = append(out, id)
		}
	}
	return out
}

// identityKey 以稳定文本表示一条身份，仅用于测试内集合归类。
func identityKey(id orderedIdentity) string {
	b, _ := json.Marshal(id.labels)
	return fmt.Sprintf("%s|%s", id.name, b)
}
