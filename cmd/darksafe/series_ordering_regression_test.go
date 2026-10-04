package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 本文件在命令行端到端（stdin → runIngest → JSON 输出）层面回归保障序列排列：
// 同一组数据以不同顺序、不同批次、直接书写或用合法 JSON 字符转义书写后，
// 写入成功返回的全量快照给出完全相同的序列排列；查询按标签子集与闭区间过滤后，
// 保留条目仍按全量排序中的相对位置输出，并各自携带完整标签、点数与均值，
// 只有区间外点的序列不出现，全部被过滤时是空数组。排序只依据解析后的指标名与
// 完整标签集合，冲突原因里为看清身份加的引号/转义文字不参与排序。

// wireIdent 是输出 JSON 中一条序列的身份（指标名 + 完整标签集合）。
type wireIdent struct {
	name   string
	labels map[string]interface{}
}

// wireTemplates 用占位符描述 15 条序列，literal/escaped 两种替换得到解析后
// 完全相同的数据：#C# 逗号、#EQ# 等号、#Q# 引号、#BS# 反斜杠、#ZH# “中”。
var wireTemplates = []string{
	`{"name":"cpu","timestamp":101,"value":1,"labels":{"b":"1"}}`,
	`{"name":"cpu","timestamp":102,"value":2,"labels":{"aa":"1","b":""}}`,
	`{"name":"cpu","timestamp":103,"value":3,"labels":{"a":"2","b":"1"}}`,
	`{"name":"cpu","timestamp":104,"value":4,"labels":{"a":"2","b":""}}`,
	`{"name":"cpu","timestamp":105,"value":5,"labels":{"a":"2"}}`,
	`{"name":"cpu","timestamp":106,"value":6,"labels":{"a":"10","b":"x"}}`,
	`{"name":"cpu","timestamp":107,"value":7,"labels":{"a":"10"}}`,
	`{"name":"cpu","timestamp":108,"value":8}`,
	`{"name":"cpu","timestamp":109,"value":9,"labels":{"k":"#C#"}}`,
	`{"name":"cpu","timestamp":110,"value":10,"labels":{"k":"#ZH#"}}`,
	`{"name":"cpu","timestamp":111,"value":11,"labels":{"z":"v","k":"#ZH#"}}`,
	// 值文本上像 “a=2,b=另一个标签”，实际只是一个含逗号、等号与引号的值。
	`{"name":"cpu","timestamp":112,"value":12,"labels":{"a":"2#C#b#EQ##Q#x#Q#"}}`,
	`{"name":"cpu#C#total","timestamp":113,"value":13}`,
	`{"name":"#ZH#标","timestamp":114,"value":14,"labels":{"k":"v"}}`,
	// 键含反斜杠：与引号、逗号一样只是键里的普通字符。
	`{"name":"cpu","timestamp":115,"value":15,"labels":{"a#BS#b":"1"}}`,
}

// wireLiteral 把占位符替换为直接书写的字符（引号/反斜杠给出 JSON 源码转义形态）。
func wireLiteral(tmpl string) string {
	return strings.NewReplacer(
		"#C#", ",", "#EQ#", "=", "#Q#", `\"`, "#BS#", `\\`, "#ZH#", "中",
	).Replace(tmpl)
}

// wireEscaped 把占位符替换为合法的 JSON \uXXXX 转义书写。
func wireEscaped(tmpl string) string {
	return strings.NewReplacer(
		"#C#", jsonBackslashEscape("002c"),
		"#EQ#", jsonBackslashEscape("003d"),
		"#Q#", jsonBackslashEscape("0022"),
		"#BS#", jsonBackslashEscape("005c"),
		"#ZH#", jsonBackslashEscape("4e2d"),
	).Replace(tmpl)
}

// wireBatch 把若干模板按给定替换方式编成一个写入批次行。
func wireBatch(tmpls []string, replace func(string) string) string {
	parts := make([]string, len(tmpls))
	for i, t := range tmpls {
		parts[i] = replace(t)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// expectedWireOrder 是规范顺序（手工按规则排好，已与核心包比较器核对）。
func expectedWireOrder() []wireIdent {
	return []wireIdent{
		{"cpu", map[string]interface{}{}},
		{"cpu", map[string]interface{}{"a": "10"}},
		{"cpu", map[string]interface{}{"a": "10", "b": "x"}},
		{"cpu", map[string]interface{}{"a": "2"}},
		{"cpu", map[string]interface{}{"a": "2", "b": ""}},
		{"cpu", map[string]interface{}{"a": "2", "b": "1"}},
		{"cpu", map[string]interface{}{"a": `2,b="x"`}},
		{"cpu", map[string]interface{}{`a\b`: "1"}},
		{"cpu", map[string]interface{}{"aa": "1", "b": ""}},
		{"cpu", map[string]interface{}{"b": "1"}},
		{"cpu", map[string]interface{}{"k": ","}},
		{"cpu", map[string]interface{}{"k": "中"}},
		{"cpu", map[string]interface{}{"k": "中", "z": "v"}},
		{"cpu,total", map[string]interface{}{}},
		{"中标", map[string]interface{}{"k": "v"}},
	}
}

// runIngestDecoded 运行整段输入，返回退出码与逐行解码后的结果。
func runIngestDecoded(t *testing.T, input string) (int, []map[string]interface{}) {
	t.Helper()
	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	res := make([]map[string]interface{}, 0, len(lines))
	for _, line := range lines {
		res = append(res, decodeResultLine(t, line))
	}
	return code, res
}

// resultIdents 从写入快照或查询结果的 series 数组提取身份顺序。
func resultIdents(t *testing.T, arr []interface{}) []wireIdent {
	t.Helper()
	out := make([]wireIdent, len(arr))
	for i, item := range arr {
		s := item.(map[string]interface{})
		out[i] = wireIdent{
			name:   s["name"].(string),
			labels: s["labels"].(map[string]interface{}),
		}
	}
	return out
}

func identSig(id wireIdent) string {
	b, _ := json.Marshal(id.labels)
	return id.name + "|" + string(b)
}

func assertIdentOrder(t *testing.T, got []wireIdent, want []wireIdent, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d identities %v, want %d %v", context, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].name != want[i].name || !reflect.DeepEqual(got[i].labels, want[i].labels) {
			t.Fatalf("%s: position %d = %s, want %s\nfull got: %v",
				context, i, identSig(got[i]), identSig(want[i]), got)
		}
	}
}

// wireQuery 构造查询行；labels 为 nil 时省略 labels 字段。
func wireQuery(name string, start, end int, labels map[string]string) string {
	var b strings.Builder
	nb, _ := json.Marshal(name)
	b.WriteString(`{"op":"query","name":`)
	b.Write(nb)
	b.WriteString(`,"start":`)
	b.WriteString(jsonInt(start))
	b.WriteString(`,"end":`)
	b.WriteString(jsonInt(end))
	if labels != nil {
		lb, _ := json.Marshal(labels)
		b.WriteString(`,"labels":`)
		b.Write(lb)
	}
	b.WriteByte('}')
	return b.String()
}

func jsonInt(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestRunIngestSeriesOrderRegression 是端到端主回归：
//   - 程序 A 正序写入全部序列，程序 B 乱序分两批、且把逗号/等号/引号/反斜杠/中文
//     一律写成 \uXXXX 转义；两份最终全量快照的序列与点排列逐字节一致；
//   - 快照身份顺序正是字符串字典序规则（"10"<"2"、键 "aa"<"b"、前缀短者在前、
//     无标签最前、名称中的逗号与中文按原字符排序），与标签渲染引号无关；
//   - 查询过滤后保留全量排序的相对位置与各自完整标签/count/average；
//   - 程序 B 末尾保留重复采样计为重复、不同值冲突被拒绝（快照不因此改变）。
func TestRunIngestSeriesOrderRegression(t *testing.T) {
	want := expectedWireOrder()

	// 第二批补充点：{a:2} 在 300 再写 50（count=2,avg=27.5）；
	// {k:中} 在 5000 再写 99（用于闭区间端点与“区间外不出现”）。
	secondPoints := []string{
		`{"name":"cpu","timestamp":300,"value":50,"labels":{"a":"2"}}`,
		`{"name":"cpu","timestamp":5000,"value":99,"labels":{"k":"中"}}`,
	}

	// 程序 A：正序、直接书写，全部成功；中间穿插查询，最后空批取全量快照。
	aLines := []string{
		wireBatch(wireTemplates, wireLiteral),
		"[" + strings.Join(secondPoints, ",") + "]",
		wireQuery("cpu", 0, 1000, nil),
		wireQuery("cpu", 0, 1000, map[string]string{"a": "2"}),
		wireQuery("cpu", 0, 1000, map[string]string{"b": ""}),
		wireQuery("cpu", 0, 1000, map[string]string{"k": "中"}),
		wireQuery("cpu", 5001, 6000, map[string]string{"k": "中"}),
		wireQuery("cpu", 5000, 5000, map[string]string{"k": "中"}),
		wireQuery("cpu", 2000, 2999, map[string]string{"a": "2"}),
		wireQuery("cpu,total", 0, 1000, nil),
		wireQuery("中标", 0, 1000, map[string]string{"k": "v"}),
		`[]`,
	}
	codeA, resA := runIngestDecoded(t, strings.Join(aLines, "\n"))
	if codeA != 0 {
		t.Fatalf("program A exit code = %d, want 0", codeA)
	}

	// 全量 cpu 查询：13 条 cpu（最后两条别的名字不出现），相对顺序即规范序前 13 条。
	fullCPU := resA[2]["series"].([]interface{})
	assertIdentOrder(t, resultIdents(t, fullCPU), want[:13], "full cpu query")
	// {a:2} 带两个点（105->5、300->50），均值 27.5；{k:中} 的 5000 点在区间外，
	// 仍只有一个点；其余序列各一个点，统计值不随整理串换。
	stats := map[string]struct {
		count float64
		avg   float64
	}{}
	for _, id := range want[:13] {
		stats[identSig(id)] = struct {
			count float64
			avg   float64
		}{1, idValueByName(id)}
	}
	stats[identSig(wireIdent{"cpu", map[string]interface{}{"a": "2"}})] = struct {
		count float64
		avg   float64
	}{2, 27.5}
	for _, item := range fullCPU {
		s := item.(map[string]interface{})
		id := wireIdent{s["name"].(string), s["labels"].(map[string]interface{})}
		want := stats[identSig(id)]
		if s["count"].(float64) != want.count || s["average"].(float64) != want.avg {
			t.Fatalf("full cpu query %s = count %v average %v, want %v/%v",
				identSig(id), s["count"], s["average"], want.count, want.avg)
		}
	}

	// 子集 a=2：值恰为 "2" 的三条，顺序 {a:2}、{a:2,b:""}、{a:2,b:1}，
	// 第一条带两个点。值文本为 2,b="x" 的序列不被 a=2 命中。
	sub := resA[3]["series"].([]interface{})
	assertIdentOrder(t, resultIdents(t, sub),
		[]wireIdent{want[3], want[4], want[5]}, "a=2 subset")
	if sub[0].(map[string]interface{})["count"] != 2.0 ||
		sub[0].(map[string]interface{})["average"] != 27.5 ||
		sub[1].(map[string]interface{})["average"] != 4.0 ||
		sub[2].(map[string]interface{})["average"] != 3.0 {
		t.Fatalf("a=2 subset stats swapped: %v", sub)
	}

	// 空字符串值只匹配真实存在该标签的两条，顺序沿用全量排序。
	emptyVal := resA[4]["series"].([]interface{})
	assertIdentOrder(t, resultIdents(t, emptyVal),
		[]wireIdent{want[4], want[8]}, `b="" subset`)
	if emptyVal[0].(map[string]interface{})["average"] != 4.0 ||
		emptyVal[1].(map[string]interface{})["average"] != 2.0 {
		t.Fatalf(`b="" subset averages swapped: %v`, emptyVal)
	}

	// k=中：两条，带 z=v 的额外标签完整保留且排后；5000 的点不在 [0,1000]。
	cn := resA[5]["series"].([]interface{})
	assertIdentOrder(t, resultIdents(t, cn),
		[]wireIdent{want[11], want[12]}, "k=中 subset")
	cn1 := cn[1].(map[string]interface{})
	if cn1["labels"].(map[string]interface{})["z"] != "v" ||
		cn1["count"] != 1.0 || cn1["average"] != 11.0 {
		t.Fatalf("extra-label entry lost labels/stats: %v", cn1)
	}

	// 全部点在区间外：空数组（不是 count=0 的条目）。
	if arr := resA[6]["series"].([]interface{}); len(arr) != 0 {
		t.Fatalf("out-of-range query = %v, want empty array", arr)
	}
	// 闭区间端点 5000：恰好命中 5000->99 一条。
	endpoint := resA[7]["series"].([]interface{})
	if len(endpoint) != 1 || endpoint[0].(map[string]interface{})["count"] != 1.0 ||
		endpoint[0].(map[string]interface{})["average"] != 99.0 {
		t.Fatalf("closed-range endpoint query = %v, want one point average 99", endpoint)
	}
	// {a:2} 的两个点（105、300）都在 [2000,2999] 外：空数组。
	if arr := resA[8]["series"].([]interface{}); len(arr) != 0 {
		t.Fatalf("range with no points = %v, want empty array", arr)
	}
	// 名称精确：含逗号的名字与中文名字各自命中唯一序列。
	if s := resA[9]["series"].([]interface{}); len(s) != 1 ||
		s[0].(map[string]interface{})["average"] != 13.0 {
		t.Fatalf("cpu,total name query = %v", s)
	}
	if s := resA[10]["series"].([]interface{}); len(s) != 1 ||
		s[0].(map[string]interface{})["average"] != 14.0 {
		t.Fatalf("Chinese name query = %v", s)
	}

	// 程序 A 的最终全量快照：15 条，顺序为规范序。
	snapA := resA[11]["series"].([]interface{})
	assertIdentOrder(t, resultIdents(t, snapA), want, "program A final snapshot")

	// 程序 B：乱序分两批写入，占位符全部以 \uXXXX 转义书写；再写补充点；
	// 然后一条等值重复（应计 duplicates），一条不同值冲突（整批拒绝）；
	// 最后空批取快照。
	batch1Idx := []int{12, 2, 9, 0, 14, 4, 7, 11}
	batch2Idx := []int{6, 1, 13, 3, 8, 5, 10}
	bLines := []string{
		wireBatch(wireTemplatesByIndex(batch1Idx...), wireEscaped),
		wireBatch(wireTemplatesByIndex(batch2Idx...), wireEscaped),
		"[" + strings.Join([]string{
			wireEscaped(`{"name":"cpu","timestamp":300,"value":50,"labels":{"a":"2"}}`),
			wireEscaped(`{"name":"cpu","timestamp":5000,"value":99,"labels":{"k":"#ZH#"}}`),
		}, ",") + "]",
		// 等值重复：{k:中} 在 110 的值 10，转义+标签顺序无关，计 1 个 duplicate。
		`[{"name":"cpu","timestamp":110,"value":10,"labels":{"k":"` + jsonBackslashEscape("4e2d") + `"}}]`,
		// 不同值冲突：整批拒绝，existing=10，submitted=777。
		`[{"name":"cpu","timestamp":110,"value":777,"labels":{"k":"` + jsonBackslashEscape("4e2d") + `"}}]`,
		`[]`,
	}
	codeB, resB := runIngestDecoded(t, strings.Join(bLines, "\n"))
	if codeB == 0 {
		t.Fatalf("program B exit code = 0, want non-zero due to the conflict line")
	}
	if ok := resB[2]; ok["added"] != 2.0 {
		t.Fatalf("second-points batch = %v, want 2 added", ok)
	}
	dup := resB[3]
	if dup["added"] != 0.0 || dup["duplicates"] != 1.0 {
		t.Fatalf("escaped duplicate = %v, want 0 added 1 duplicate", dup)
	}
	conflict := resB[4]
	if conflict["status"] != "error" || conflict["index"] != 1.0 ||
		conflict["conflict"].(map[string]interface{})["existing"] != 10.0 ||
		conflict["conflict"].(map[string]interface{})["submitted"] != 777.0 {
		t.Fatalf("escaped conflict result = %v", conflict)
	}
	cSeries := conflict["conflict"].(map[string]interface{})["series"].(map[string]interface{})
	if cSeries["name"] != "cpu" ||
		!reflect.DeepEqual(cSeries["labels"], map[string]interface{}{"k": "中"}) {
		t.Fatalf("escaped conflict must name parsed identity, got %v", cSeries)
	}

	// 程序 B 最终快照与程序 A 逐字节一致（顺序、标签、点全相同）：
	// 转义写法、乱序提交、重复与冲突拒绝都不改变公开结果。
	snapB := resB[5]["series"].([]interface{})
	if !reflect.DeepEqual(snapA, snapB) {
		t.Fatalf("literal vs escaped/reordered snapshots differ:\nA: %v\nB: %v", snapA, snapB)
	}
}

// wireTemplatesByIndex 按给定下标取模板行。
func wireTemplatesByIndex(idx ...int) []string {
	out := make([]string, len(idx))
	for i, n := range idx {
		out[i] = wireTemplates[n]
	}
	return out
}

// idValueByName 返回某条规范身份唯一采样点（1000 以内那一个）的写入值，
// 用于全量查询的 count/average 基线；多点点位由用例单独覆盖。
func idValueByName(id wireIdent) float64 {
	values := map[string]float64{
		identSig(wireIdent{"cpu", map[string]interface{}{}}):                    8,
		identSig(wireIdent{"cpu", map[string]interface{}{"a": "10"}}):           7,
		identSig(wireIdent{"cpu", map[string]interface{}{"a": "10", "b": "x"}}): 6,
		identSig(wireIdent{"cpu", map[string]interface{}{"a": "2"}}):            5,
		identSig(wireIdent{"cpu", map[string]interface{}{"a": "2", "b": ""}}):   4,
		identSig(wireIdent{"cpu", map[string]interface{}{"a": "2", "b": "1"}}):  3,
		identSig(wireIdent{"cpu", map[string]interface{}{"a": `2,b="x"`}}):      12,
		identSig(wireIdent{"cpu", map[string]interface{}{`a\b`: "1"}}):          15,
		identSig(wireIdent{"cpu", map[string]interface{}{"aa": "1", "b": ""}}):  2,
		identSig(wireIdent{"cpu", map[string]interface{}{"b": "1"}}):            1,
		identSig(wireIdent{"cpu", map[string]interface{}{"k": ","}}):            9,
		identSig(wireIdent{"cpu", map[string]interface{}{"k": "中"}}):            10,
		identSig(wireIdent{"cpu", map[string]interface{}{"k": "中", "z": "v"}}):  11,
		identSig(wireIdent{"cpu,total", map[string]interface{}{}}):              13,
		identSig(wireIdent{"中标", map[string]interface{}{"k": "v"}}):             14,
	}
	return values[identSig(id)]
}
