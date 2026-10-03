package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// 本文件在命令行端到端层面回归保障序列身份：标签键、标签值与指标名中的
// 冒号、分号、等号、逗号和花括号都是用户数据，不能被当成键值或标签边界。
// 经 stdin 写入、快照与查询输出（JSON 真实文本）后，混淆形态的合法输入仍各自
// 归属独立序列；成功/失败结果格式与现有 ingest 输出保持兼容。

// findWireSeries 在结果 JSON（写入快照或查询）里按指标名与完整标签集合定位序列。
func findWireSeries(t *testing.T, arr []interface{}, name string, labels map[string]interface{}) map[string]interface{} {
	t.Helper()
	for _, item := range arr {
		s := item.(map[string]interface{})
		if s["name"] == name && reflect.DeepEqual(s["labels"], labels) {
			return s
		}
	}
	t.Fatalf("series %s %v not found in %v", name, labels, arr)
	return nil
}

// jsonBackslashEscape 以 rune 构造反斜杠，生成 \uXXXX 形态的 JSON 转义文本。
func jsonBackslashEscape(hex string) string {
	return string(rune(92)) + "u" + hex
}

func TestRunIngestDelimiterCharactersAreUserData(t *testing.T) {
	// 行 1：五条序列都在时间戳 1000 写入相同数值 1，但身份互不相同：
	//   cpu {"a":"1","b":"2"}        两个标签
	//   cpu {"a":"b=2"}              一个标签，值看起来像两个标签
	//   cpu {"a":"b=2,c=3"}          一个标签，值看起来像三个标签
	//   cpu {"a:b":"c;d"}            键与值都含分隔字符
	//   cpu:total{x=1}（无标签）      指标名本身含冒号、等号和花括号
	lines := []string{
		// 行 1：五条序列都在时间戳 1000 写入相同数值 1，但身份互不相同：
		//   cpu {"a":"1","b":"2"}   两个标签
		//   cpu {"a":"b=2"}         一个标签，值看起来像两个标签
		//   cpu {"a":"b=2,c=3"}     一个标签，值看起来像三个标签
		//   cpu {"a:b":"c;d"}       键与值都含分隔字符
		//   cpu:total{x=1}（无标签） 指标名本身含冒号、等号和花括号
		`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":"1","b":"2"}},` +
			`{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":"b=2"}},` +
			`{"name":"cpu","timestamp":1000,"value":1,"labels":{"a":"b=2,c=3"}},` +
			`{"name":"cpu","timestamp":1000,"value":1,"labels":{"a:b":"c;d"}},` +
			`{"name":"cpu:total{x=1}","timestamp":1000,"value":1}]`,
		// 行 2：全部身份再次提交同点同值。标签书写顺序打乱，键里的冒号与值里的
		// 分号改用 \uXXXX 转义书写，指标名中的冒号同样转义——仍全部是重复。
		`[{"name":"cpu","timestamp":1000,"value":1.0,"labels":{"b":"2","a":"1"}},` +
			`{"name":"cpu","timestamp":1000,"value":1.0,"labels":{"a":"b=2"}},` +
			`{"name":"cpu","timestamp":1000,"value":1.0,"labels":{"a":"b=2,c=3"}},` +
			`{"name":"cpu","timestamp":1000,"value":1.0,"labels":{"a#CO#b":"c#SC#d"}},` +
			`{"name":"cpu#CO#total{x=1}","timestamp":1000,"value":1.0}]`,
		// 行 3：第一个点是新增（时间戳 2000），第二个点与既有数据冲突：整批回滚。
		`[{"name":"cpu","timestamp":2000,"value":5,"labels":{"a":"b=2"}},` +
			`{"name":"cpu","timestamp":1000,"value":9,"labels":{"a":"1","b":"2"}}]`,
		// 行 4：省略标签，查 cpu 全部四条序列，各自一个点，互不混合。
		`{"op":"query","name":"cpu","start":0,"end":3000}`,
		// 行 5：值里的 "b=2" 是完整值，只命中单标签序列。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"a":"b=2"}}`,
		// 行 6：真实存在的标签 b 只命中两标签序列，不命中值里写着 b=2 的单标签序列。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"b":"2"}}`,
		// 行 7：值片段 c=3 不能被当成标签 c。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"c":"3"}}`,
		// 行 8：键与值中的冒号、分号原样匹配。
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"a:b":"c;d"}}`,
		// 行 9：指标名中的分隔字符是名字的一部分。
		`{"op":"query","name":"cpu:total{x=1}","start":0,"end":3000}`,
	}
	input := strings.NewReplacer("#CO#", jsonBackslashEscape("003a"), "#SC#", jsonBackslashEscape("003b")).
		Replace(strings.Join(lines, "\n"))

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because line 3 conflicted")
	}
	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(got) != 9 {
		t.Fatalf("got %d output lines, want 9 (2 ok writes, 1 error, 6 queries): %v", len(got), got)
	}

	// 行 1：五条序列分别新增，快照中的标签是解析后的真实文本。
	first := decodeResultLine(t, got[0])
	if first["status"] != "ok" || first["added"].(float64) != 5 {
		t.Fatalf("line 1 = %v, want ok with 5 added", first)
	}
	firstSeries := first["series"].([]interface{})
	if len(firstSeries) != 5 {
		t.Fatalf("line 1 series = %v, want 5 distinct identities", firstSeries)
	}
	one := findWireSeries(t, firstSeries, "cpu", map[string]interface{}{"a": "1", "b": "2"})
	if pts := one["points"].([]interface{}); len(pts) != 1 {
		t.Fatalf("two-label series points = %v", pts)
	}
	findWireSeries(t, firstSeries, "cpu", map[string]interface{}{"a": "b=2"})
	findWireSeries(t, firstSeries, "cpu", map[string]interface{}{"a": "b=2,c=3"})
	findWireSeries(t, firstSeries, "cpu", map[string]interface{}{"a:b": "c;d"})
	named := findWireSeries(t, firstSeries, "cpu:total{x=1}", map[string]interface{}{})
	if named["labels"] == nil {
		t.Fatal("unlabeled series must serialize labels as {}")
	}

	// 行 2：转义与乱序不改变身份，全部重复、不增加点，序列数仍为 5。
	second := decodeResultLine(t, got[1])
	if second["status"] != "ok" || second["added"].(float64) != 0 || second["duplicates"].(float64) != 5 {
		t.Fatalf("line 2 = %v, want 0 added and 5 duplicates", second)
	}
	if n := len(second["series"].([]interface{})); n != 5 {
		t.Fatalf("line 2 series count = %d, want 5", n)
	}
	// 转义书写解析回真实冒号/分号后仍只匹配原来那一条。
	findWireSeries(t, second["series"].([]interface{}), "cpu", map[string]interface{}{"a:b": "c;d"})

	// 行 3：整批失败，结构化冲突指出真实完整序列、时间戳与两个数值。
	bad := decodeResultLine(t, got[2])
	if bad["status"] != "error" || int(bad["line"].(float64)) != 3 || int(bad["index"].(float64)) != 2 {
		t.Fatalf("line 3 = %v, want error on input line 3 index 2", bad)
	}
	conflict := bad["conflict"].(map[string]interface{})
	if conflict["timestamp"].(float64) != 1000 ||
		conflict["existing"].(float64) != 1 || conflict["submitted"].(float64) != 9 {
		t.Fatalf("conflict detail = %v", conflict)
	}
	cSeries := conflict["series"].(map[string]interface{})
	if cSeries["name"] != "cpu" ||
		!reflect.DeepEqual(cSeries["labels"], map[string]interface{}{"a": "1", "b": "2"}) {
		t.Fatalf("conflict series = %v, want the real two-label identity", cSeries)
	}

	// 行 4：回滚后 cpu 仍是四条序列，各自一个点（时间戳 2000 的新增点未落盘）。
	full := decodeResultLine(t, got[3])
	fullArr := full["series"].([]interface{})
	if len(fullArr) != 4 {
		t.Fatalf("full cpu query = %v, want 4 series", fullArr)
	}
	for _, item := range fullArr {
		s := item.(map[string]interface{})
		if s["count"].(float64) != 1 || s["average"].(float64) != 1 {
			t.Fatalf("series %v must keep its own single point, got count=%v average=%v",
				s["labels"], s["count"], s["average"])
		}
	}

	// 行 5/6/7：子集匹配不被标签文本中的分隔符欺骗。
	q5 := decodeResultLine(t, got[4])["series"].([]interface{})
	if len(q5) != 1 || !reflect.DeepEqual(q5[0].(map[string]interface{})["labels"],
		map[string]interface{}{"a": "b=2"}) {
		t.Fatalf("a=b=2 subset = %v, want only the single-label series", q5)
	}
	q6 := decodeResultLine(t, got[5])["series"].([]interface{})
	if len(q6) != 1 || !reflect.DeepEqual(q6[0].(map[string]interface{})["labels"],
		map[string]interface{}{"a": "1", "b": "2"}) {
		t.Fatalf("b=2 subset = %v, want only the real two-label series", q6)
	}
	if q7 := decodeResultLine(t, got[6])["series"].([]interface{}); len(q7) != 0 {
		t.Fatalf("value fragment c=3 must not match as a label: %v", q7)
	}

	// 行 8：键与值中的 : ; 原样参与匹配。
	q8 := decodeResultLine(t, got[7])["series"].([]interface{})
	if len(q8) != 1 || !reflect.DeepEqual(q8[0].(map[string]interface{})["labels"],
		map[string]interface{}{"a:b": "c;d"}) {
		t.Fatalf("a:b=c;d subset = %v, want the delimiter-keyed series", q8)
	}

	// 行 9：指标名含分隔字符的序列按名字精确命中一条，且无标签。
	q9 := decodeResultLine(t, got[8])["series"].([]interface{})
	if len(q9) != 1 {
		t.Fatalf("delimiter-in-name query = %v, want 1 series", q9)
	}
	s9 := q9[0].(map[string]interface{})
	if s9["name"] != "cpu:total{x=1}" ||
		!reflect.DeepEqual(s9["labels"], map[string]interface{}{}) ||
		s9["count"].(float64) != 1 || s9["average"].(float64) != 1 {
		t.Fatalf("delimiter-in-name series = %v", s9)
	}
}
