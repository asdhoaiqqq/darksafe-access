package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 本文件在命令行端到端层面回归保障写入快照与查询结果的序列输出顺序。
// 与 darksafe 包内的 series_output_order_test.go 相对应，这里验证真正上线的
// JSON 线文本：相同数据以不同顺序、直接书写或合法 \uXXXX 转义书写提交后，
// 成功写入返回的全量快照序列排列一致；查询按标签子集与闭区间过滤后，保留条目
// 维持其在全量结果中的相对位置，并各自携带完整标签、点数与均值；区间内无点的
// 序列不出现，全部被过滤时线上仍是空数组 []。

// wireIdentity 以“指标名 + 规范 JSON 标签集合”标识输出数组中的一条序列，
// 仅用于比较次序；标签 map 经 json.Marshal 按键排序，与线上标签书写顺序无关。
func wireIdentity(t *testing.T, item map[string]interface{}) string {
	t.Helper()
	name, ok := item["name"].(string)
	if !ok {
		t.Fatalf("result series item missing string name: %v", item)
	}
	labels, ok := item["labels"].(map[string]interface{})
	if !ok {
		t.Fatalf("result series item %q missing labels object: %v", name, item)
	}
	b, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("marshal labels of %q: %v", name, err)
	}
	return name + "|" + string(b)
}

// wireOrder 返回结果 JSON 中 series 数组的身份次序。
func wireOrder(t *testing.T, arr []interface{}) []string {
	t.Helper()
	out := make([]string, len(arr))
	for i, it := range arr {
		out[i] = wireIdentity(t, it.(map[string]interface{}))
	}
	return out
}

// runIngestLines 逐行喂给 runIngest，返回退出码、逐行原始输出与解码后的结果数组。
func runIngestLines(t *testing.T, lines []string) (int, []string, []map[string]interface{}) {
	t.Helper()
	var out bytes.Buffer
	code := runIngest(strings.NewReader(strings.Join(lines, "\n")), &out)
	raw := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(raw) != len(lines) {
		t.Fatalf("got %d output lines, want %d (one per non-empty input): %v", len(raw), len(lines), raw)
	}
	decoded := make([]map[string]interface{}, len(raw))
	for i, l := range raw {
		decoded[i] = decodeResultLine(t, l)
	}
	return code, raw, decoded
}

// canonicalWireOrder 是本场景 10 条序列的规范输出次序：
// 无标签 < 键 aa < 键 b < 键 k；同为 k 时值按字节 "" < "\"q\"" < "10" < "2" < 中文，
// 同值 "2" 时较短集合在前，再按 z 的值字符串序 "10" < "2"。
func canonicalWireOrder() []string {
	labels := []string{
		`{}`,
		`{"aa":"x"}`,
		`{"b":"x"}`,
		`{"k":""}`,
		`{"k":"\"q\""}`,
		`{"k":"10"}`,
		`{"k":"2"}`,
		`{"k":"2","z":"10"}`,
		`{"k":"2","z":"2"}`,
		`{"k":"中"}`,
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = "m|" + l
	}
	return out
}

// firstPointSamples 与 secondPointSamples 按规范次序给出时间戳 1（值 1..10）
// 与时间戳 20（值 21..30）两批采样点。
func firstPointSamples() []string {
	return []string{
		`{"name":"m","timestamp":1,"value":1}`,
		`{"name":"m","timestamp":1,"value":2,"labels":{"aa":"x"}}`,
		`{"name":"m","timestamp":1,"value":3,"labels":{"b":"x"}}`,
		`{"name":"m","timestamp":1,"value":4,"labels":{"k":""}}`,
		`{"name":"m","timestamp":1,"value":5,"labels":{"k":"\"q\""}}`,
		`{"name":"m","timestamp":1,"value":6,"labels":{"k":"10"}}`,
		`{"name":"m","timestamp":1,"value":7,"labels":{"k":"2"}}`,
		`{"name":"m","timestamp":1,"value":8,"labels":{"k":"2","z":"10"}}`,
		`{"name":"m","timestamp":1,"value":9,"labels":{"k":"2","z":"2"}}`,
		`{"name":"m","timestamp":1,"value":10,"labels":{"k":"中"}}`,
	}
}

func secondPointSamples() []string {
	return []string{
		`{"name":"m","timestamp":20,"value":21}`,
		`{"name":"m","timestamp":20,"value":22,"labels":{"aa":"x"}}`,
		`{"name":"m","timestamp":20,"value":23,"labels":{"b":"x"}}`,
		`{"name":"m","timestamp":20,"value":24,"labels":{"k":""}}`,
		`{"name":"m","timestamp":20,"value":25,"labels":{"k":"\"q\""}}`,
		`{"name":"m","timestamp":20,"value":26,"labels":{"k":"10"}}`,
		`{"name":"m","timestamp":20,"value":27,"labels":{"k":"2"}}`,
		`{"name":"m","timestamp":20,"value":28,"labels":{"k":"2","z":"10"}}`,
		`{"name":"m","timestamp":20,"value":29,"labels":{"k":"2","z":"2"}}`,
		`{"name":"m","timestamp":20,"value":30,"labels":{"k":"中"}}`,
	}
}

func asJSONArray(samples []string) string { return "[" + strings.Join(samples, ",") + "]" }

// assertSeriesStats 按输出次序核对每条序列的点数与均值，标签身份由调用方顺序隐含。
func assertSeriesStats(t *testing.T, arr []interface{}, wantCount int, wantAvg []float64) {
	t.Helper()
	if len(arr) != len(wantAvg) {
		t.Fatalf("series count = %d, want %d: %v", len(arr), len(wantAvg), arr)
	}
	for i, it := range arr {
		s := it.(map[string]interface{})
		if int(s["count"].(float64)) != wantCount {
			t.Fatalf("series[%d] %v count = %v, want %d", i, s["labels"], s["count"], wantCount)
		}
		if s["average"].(float64) != wantAvg[i] {
			t.Fatalf("series[%d] %v average = %v, want %v (stats must stay attached to their own identity)",
				i, s["labels"], s["average"], wantAvg[i])
		}
	}
}

// TestRunIngestSnapshotAndQuerySeriesOrder 端到端固定序列次序：快照次序与提交顺序
// 无关，查询过滤保持全量相对位置，统计不串换，区间外序列缺席、全过滤返回 []。
func TestRunIngestSnapshotAndQuerySeriesOrder(t *testing.T) {
	first := firstPointSamples()
	second := secondPointSamples()

	// 行 1：时间戳 1 的 10 条按规范次序一次写入；行 2：时间戳 20 的点反序写入，
	// 验证提交顺序不改变快照排列。其余为各类查询与一次“部分序列区间内有点”的写入。
	lines := []string{
		asJSONArray(first),
		asJSONArray(reverseWireStrings(second)),
		`{"op":"query","name":"m","start":0,"end":1000}`,                    // 全量：两时间戳都在区间内
		`{"op":"query","name":"m","start":0,"end":1000,"labels":{"k":"2"}}`, // 子集 3 条
		`{"op":"query","name":"m","start":1,"end":1}`,                       // 只取时间戳 1
		`{"op":"query","name":"m","start":31,"end":999}`,                    // 全部点在区间外
		asJSONArray([]string{ // 只给两条序列补时间戳 5000 的点
			`{"name":"m","timestamp":5000,"value":100,"labels":{"aa":"x"}}`,
			`{"name":"m","timestamp":5000,"value":200,"labels":{"k":"2","z":"2"}}`,
		}),
		`{"op":"query","name":"m","start":4000,"end":6000}`, // 仅这两条区间内有点
	}
	code, raw, res := runIngestLines(t, lines)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (all lines valid)", code)
	}
	wantOrder := canonicalWireOrder()

	// 行 1：成功写入返回的全量快照就是规范次序；无标签序列线上标签为 {}，
	// 两标签序列的标签在输出 JSON 中按键排列（k 在 z 前），与提交顺序无关。
	if res[0]["status"] != "ok" || res[0]["added"].(float64) != 10 {
		t.Fatalf("line 1 = %v, want ok with 10 added", res[0])
	}
	if got := wireOrder(t, res[0]["series"].([]interface{})); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("snapshot order = %v\nwant %v", got, wantOrder)
	}
	if !strings.Contains(raw[0], `"labels":{}`) {
		t.Fatalf("unlabeled series must serialize labels as {}: %s", raw[0])
	}
	if !strings.Contains(raw[0], `"labels":{"k":"2","z":"10"}`) {
		t.Fatalf("two-label series must be emitted with keys sorted (k before z): %s", raw[0])
	}

	// 行 3：全量查询 10 条，次序与快照一致；每条 2 个点，均值 (v1+v20)/2 = 11..20，
	// 相邻均值随身份固定，不被次序整理串换。
	full := res[2]["series"].([]interface{})
	if got := wireOrder(t, full); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("full query order = %v\nwant %v", got, wantOrder)
	}
	assertSeriesStats(t, full, 2, []float64{11, 12, 13, 14, 15, 16, 17, 18, 19, 20})

	// 行 4：子集 k="2" 命中三条，保持全量相对位置（排在其前后的序列被拿掉，
	// 保留条目不重排），均值分别是 17、18、19。
	k2 := res[3]["series"].([]interface{})
	if got := wireOrder(t, k2); !reflect.DeepEqual(got, []string{
		`m|{"k":"2"}`, `m|{"k":"2","z":"10"}`, `m|{"k":"2","z":"2"}`,
	}) {
		t.Fatalf("k=2 subset order = %v", got)
	}
	assertSeriesStats(t, k2, 2, []float64{17, 18, 19})

	// 行 5：[1,1] 只取时间戳 1 的点，仍是 10 条、同一次序，均值等于时间戳 1 的值 1..10。
	narrow := res[4]["series"].([]interface{})
	if got := wireOrder(t, narrow); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("narrow query order = %v\nwant %v", got, wantOrder)
	}
	assertSeriesStats(t, narrow, 1, []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})

	// 行 6：区间内没有任何点 → 线上严格为空数组，而不是 count=0 的条目。
	if res[5]["status"] != "ok" {
		t.Fatalf("out-of-range line = %v, want ok", res[5])
	}
	if raw[5] != `{"status":"ok","op":"query","series":[]}` {
		t.Fatalf("out-of-range wire = %q, want empty series array", raw[5])
	}

	// 行 8：只有两条序列在 [4000,6000] 内有点，其他 8 条整体缺席；两条保留全量
	// 相对位置（aa 在前，k,z 在后），各自携带完整标签、点数 1 与自己的均值。
	partial := res[7]["series"].([]interface{})
	if got := wireOrder(t, partial); !reflect.DeepEqual(got, []string{
		`m|{"aa":"x"}`, `m|{"k":"2","z":"2"}`,
	}) {
		t.Fatalf("partial in-range order = %v", got)
	}
	assertSeriesStats(t, partial, 1, []float64{100, 200})
}

// TestRunIngestOrderSameAcrossLiteralAndEscapedInput：相同数据第二次以整体反序、
// 标签乱序、引号与中文改用合法 \uXXXX 转义书写提交，线上快照与全量查询的身份
// 次序及统计与直接书写完全一致。
func TestRunIngestOrderSameAcrossLiteralAndEscapedInput(t *testing.T) {
	// 转义形态：值 "q" 的引号用 "，中文“中”用 中，两标签序列把 z 写在 k 前。
	escapedFirst := []string{
		`{"name":"m","timestamp":1,"value":10,"labels":{"k":"` + jsonBackslashEscape("4e2d") + `"}}`,
		`{"name":"m","timestamp":1,"value":9,"labels":{"z":"2","k":"2"}}`,
		`{"name":"m","timestamp":1,"value":8,"labels":{"z":"10","k":"2"}}`,
		`{"name":"m","timestamp":1,"value":7,"labels":{"k":"2"}}`,
		`{"name":"m","timestamp":1,"value":6,"labels":{"k":"10"}}`,
		`{"name":"m","timestamp":1,"value":5,"labels":{"k":"` + jsonBackslashEscape("0022") + `q` + jsonBackslashEscape("0022") + `"}}`,
		`{"name":"m","timestamp":1,"value":4,"labels":{"k":""}}`,
		`{"name":"m","timestamp":1,"value":3,"labels":{"b":"x"}}`,
		`{"name":"m","timestamp":1,"value":2,"labels":{"aa":"x"}}`,
		`{"name":"m","timestamp":1,"value":1}`,
	}
	lines := []string{
		asJSONArray(escapedFirst),
		asJSONArray(reverseWireStrings(secondPointSamples())),
		`{"op":"query","name":"m","start":0,"end":1000}`,
	}
	code, raw, res := runIngestLines(t, lines)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	wantOrder := canonicalWireOrder()

	// 转义只影响书写：解析后引号与中文都是真实字符，快照次序与直接书写一致。
	if res[0]["added"].(float64) != 10 {
		t.Fatalf("escaped write added = %v, want 10 distinct series", res[0]["added"])
	}
	if got := wireOrder(t, res[0]["series"].([]interface{})); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("escaped/reversed snapshot order = %v\nwant %v", got, wantOrder)
	}

	// 全量查询的身份次序与统计同样一致：均值 11..20 仍绑定各自身份。
	full := res[2]["series"].([]interface{})
	if got := wireOrder(t, full); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("escaped-run query order = %v\nwant %v", got, wantOrder)
	}
	assertSeriesStats(t, full, 2, []float64{11, 12, 13, 14, 15, 16, 17, 18, 19, 20})

	// 直接比较两次运行中“全量查询”那一行的原始 JSON（含序列次序、标签、点数与均值）：
	// 直接书写、正序提交的运行与转义/反序运行必须逐字节一致。
	if literalQueryJSON := literalAllQueryJSON(t); literalQueryJSON != raw[2] {
		t.Fatalf("literal vs escaped full-query output differs:\nliteral: %s\nescaped: %s",
			literalQueryJSON, raw[2])
	}
}

// literalAllQueryJSON 以“直接书写、正序提交”的方式运行同一批数据，返回全量查询那行 JSON。
func literalAllQueryJSON(t *testing.T) string {
	t.Helper()
	_, raw, _ := runIngestLines(t, []string{
		asJSONArray(firstPointSamples()),
		asJSONArray(secondPointSamples()),
		`{"op":"query","name":"m","start":0,"end":1000}`,
	})
	return raw[2]
}

// reverseWireStrings 返回反序切片，便于以相反提交顺序写同一批点。
func reverseWireStrings(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}
