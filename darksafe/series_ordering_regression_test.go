package darksafe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 本文件为序列在成功结果中的排列顺序补充回归保障。metrics_test.go 只覆盖了
// 普通单标签（host=a/host=b）的排序；这里重点保护容易被错误实现误导的合法身份：
//
//   - 标签值是文本："10" 按字符串排在 "2" 之前，不能按数值排序；
//   - 标签按键排序后逐对比较：键 "aa" 排在 "b" 之前，字符多不代表靠后；
//   - 一套标签是另一套的完整前缀时较短者在前，无标签序列因此在同名序列最前；
//   - 名称、键、值中的逗号、等号、引号、反斜杠与合法中文，一律按 JSON 解析后的
//     原始字符串比较；冲突原因里为区分身份而添加的引号/转义文字不参与排序；
//   - 标签书写顺序、以及同一字符直接书写还是用合法 JSON \uXXXX 转义书写，
//     都不改变身份，也就不改变排列。
//
// 查询按标签子集与闭区间过滤后，保留条目继续携带各自完整标签、点数与均值，
// 且相对次序仍是全量排序的一个子序列；只有区间外点的序列不出现，全被过滤返回 []。
// 这里只锁定既有输出约定，不新增排序选项，也不改变写入/查询的返回结构。

// identitySpec 描述一条参与排序回归的序列：规范身份（指标名+完整标签集合）、
// 一个采样点与其数值。各序列时间戳互不相同，使点数与均值能一一对应到身份，
// 排序整理一旦串换统计值即可被查出。
type identitySpec struct {
	name   string
	labels map[string]string
	ts     int64
	value  float64
}

// orderingSpecs 是期望的规范顺序（已按排序规则手工排好，并经实际比较器核对）。
// 同名 cpu 的序列刻意覆盖：无标签、数值外观的值（"10"/"2"）、长短键（"aa"/"b"）、
// 标签集合前缀、空字符串值、含逗号/等号/引号/反斜杠的键值、中文键值；
// 指标名再覆盖逗号与中文，验证名称按原始字符串字典序排在标签比较之前。
func orderingSpecs() []identitySpec {
	return []identitySpec{
		{name: "cpu", labels: map[string]string{}, ts: 100, value: 1}, // 无标签：同名最前
		{name: "cpu", labels: map[string]string{"a": "10"}, ts: 101, value: 2},
		{name: "cpu", labels: map[string]string{"a": "10", "b": "x"}, ts: 102, value: 3},
		{name: "cpu", labels: map[string]string{"a": "2"}, ts: 103, value: 4}, // "10" < "2"
		{name: "cpu", labels: map[string]string{"a": "2", "b": ""}, ts: 104, value: 5},
		{name: "cpu", labels: map[string]string{"a": "2", "b": "1"}, ts: 105, value: 6},
		{name: "cpu", labels: map[string]string{"a": `2,b="x"`}, ts: 106, value: 7},
		{name: "cpu", labels: map[string]string{`a"b`: "1"}, ts: 107, value: 8},
		{name: "cpu", labels: map[string]string{`a\b`: "1"}, ts: 108, value: 9},
		{name: "cpu", labels: map[string]string{"aa": "1"}, ts: 109, value: 10}, // 键 "aa" < "b"
		{name: "cpu", labels: map[string]string{"aa": "1", "b": ""}, ts: 110, value: 11},
		{name: "cpu", labels: map[string]string{"aa": "1", "b": "1"}, ts: 111, value: 12},
		{name: "cpu", labels: map[string]string{"b": "1"}, ts: 112, value: 13},
		{name: "cpu", labels: map[string]string{"k": `"`}, ts: 113, value: 14},
		{name: "cpu", labels: map[string]string{"k": ","}, ts: 114, value: 15},
		{name: "cpu", labels: map[string]string{"k": "2"}, ts: 115, value: 16},
		{name: "cpu", labels: map[string]string{"k": "中"}, ts: 116, value: 17},
		{name: "cpu", labels: map[string]string{"k": "中", "z": "v"}, ts: 117, value: 18},
		{name: "cpu", labels: map[string]string{"zone": "out"}, ts: 1018, value: 19},
		{name: "cpu,total", labels: map[string]string{}, ts: 119, value: 20}, // 名称含逗号
		{name: "指标", labels: map[string]string{"k": "v"}, ts: 120, value: 21},
	}
}

// identitySig 以 JSON 文本表示一条序列的规范身份（map 由 encoding/json 按键排序），
// 用于与结果中实际出现的身份逐一比对。
func identitySig(name string, labels map[string]string) string {
	b, _ := json.Marshal(struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	}{name, labels})
	return string(b)
}

func snapshotIdentitySigs(res *BatchResult) []string {
	out := make([]string, len(res.Series))
	for i, s := range res.Series {
		out[i] = identitySig(s.Name, s.Labels)
	}
	return out
}

func queryIdentitySigs(res *QueryResult) []string {
	out := make([]string, len(res.Series))
	for i, s := range res.Series {
		out[i] = identitySig(s.Name, s.Labels)
	}
	return out
}

func wantSigs(specs []identitySpec) []string {
	out := make([]string, len(specs))
	for i, sp := range specs {
		out[i] = identitySig(sp.name, sp.labels)
	}
	return out
}

// orderingBatch 把一组序列各取一个点编为一个写入批次。
func orderingBatch(specs []identitySpec) string {
	parts := make([]string, len(specs))
	for i, sp := range specs {
		parts[i] = sample(sp.name, sp.ts, sp.value, sp.labels)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// queryAll 构造省略 labels 的全序列查询 JSON 文本（labels 省略而非 null）。
func queryAll(name string, start, end int64) string {
	return `{"op":"query","name":` + jsonString(name) +
		fmt.Sprintf(`,"start":%d,"end":%d}`, start, end)
}

// shuffledOrderingPerm 是 0..20 的一个固定乱序排列，既非正序也非反序。
var shuffledOrderingPerm = []int{12, 3, 17, 0, 8, 20, 5, 14, 1, 18, 9, 6, 15, 2, 11, 19, 4, 7, 16, 10, 13}

func permutedSpecs(specs []identitySpec, perm []int) []identitySpec {
	out := make([]identitySpec, len(perm))
	for i, p := range perm {
		out[i] = specs[p]
	}
	return out
}

// TestSnapshotOrderIndependentOfSubmissionOrder 是写入侧核心回归：同一组数据
// 无论正序、反序还是乱序提交（乱序还拆成三个批次），写入成功后全量快照中的
// 序列排列必须完全一致，且正是按“指标名原始字符串字典序 + 标签逐对字符串比较”
// 得到的规范顺序，与标签书写顺序、map 遍历或批次划分都无关。
func TestSnapshotOrderIndependentOfSubmissionOrder(t *testing.T) {
	specs := orderingSpecs()
	want := wantSigs(specs)

	backward := make([]identitySpec, len(specs))
	for i, sp := range specs {
		backward[len(specs)-1-i] = sp
	}
	shuffled := permutedSpecs(specs, shuffledOrderingPerm)

	// 三个存储以不同顺序/批次提交相同数据。
	forward := NewMetricStore()
	mustOK(t, forward, orderingBatch(specs))

	reversed := NewMetricStore()
	mustOK(t, reversed, orderingBatch(backward))

	chunked := NewMetricStore()
	for _, cut := range [][2]int{{0, 7}, {7, 14}, {14, 21}} {
		mustOK(t, chunked, orderingBatch(shuffled[cut[0]:cut[1]]))
	}

	// 每个存储再用一个空批取全量快照：快照来自当下存储，与本批 added 无关，
	// 三份身份排列必须逐一相同。
	snaps := []*BatchResult{
		mustOK(t, forward, `[]`),
		mustOK(t, reversed, `[]`),
		mustOK(t, chunked, `[]`),
	}
	for i, snap := range snaps {
		if got := snapshotIdentitySigs(snap); !reflect.DeepEqual(got, want) {
			t.Fatalf("store %d snapshot order = %v\nwant %v", i, got, want)
		}
		if len(snap.Series) != len(specs) {
			t.Fatalf("store %d series count = %d, want %d", i, len(snap.Series), len(specs))
		}
	}

	// 关键相邻关系单独点名，防止错误实现悄悄溜过：
	got := snapshotIdentitySigs(snaps[0])
	cpuCount := 0
	for _, s := range snaps[0].Series {
		if s.Name == "cpu" {
			cpuCount++
		}
	}
	if cpuCount != 19 {
		t.Fatalf("cpu series = %d, want 19", cpuCount)
	}
	// 无标签 cpu 是同名序列中的第一条；指标名 cpu,total 与 指标 在全部 cpu 之后，
	// 且 "cpu" < "cpu,total" < "指标"（按原始字符串，逗号与中文都不特殊处理）。
	if snaps[0].Series[0].Name != "cpu" || len(snaps[0].Series[0].Labels) != 0 {
		t.Fatalf("first series = %+v, want unlabeled cpu", snaps[0].Series[0])
	}
	if got[19] != identitySig("cpu,total", map[string]string{}) ||
		got[20] != identitySig("指标", map[string]string{"k": "v"}) {
		t.Fatalf("metric-name ordering = %q, %q", got[19], got[20])
	}

	// 每条序列快照里的点仍是各自的点：身份与采样值按时间戳升序对应，
	// 不允许为了排列序列而串换不同序列的数据。
	for i, sp := range specs {
		idx := findViewIndex(t, snaps[0], sp.name, sp.labels)
		pts := snaps[0].Series[idx].Points
		if len(pts) != 1 || pts[0] != (Point{Timestamp: sp.ts, Value: sp.value}) {
			t.Fatalf("series %s points = %+v, want (%d,%v)", got[i], pts, sp.ts, sp.value)
		}
	}
}

// TestQueryKeepsSnapshotOrderAfterFiltering 是查询侧核心回归：子集标签与闭区间
// 过滤只删除不满足条件的条目，保留下来的条目仍是全量排序中的子序列，各自带着
// 完整标签、点数与均值；多出来的标签（如 k=中 之外的 z=v）不被裁剪，
// 空字符串值只匹配真实存在该标签的序列；全部被过滤时返回空数组。
func TestQueryKeepsSnapshotOrderAfterFiltering(t *testing.T) {
	specs := orderingSpecs()
	store := NewMetricStore()
	mustOK(t, store, orderingBatch(permutedSpecs(specs, shuffledOrderingPerm)))

	// 追加若干第二点，使过滤后保留条目的 count/average 互不相同且非平凡：
	//	{a:2}            再写 ts=300,value=10 → 与 ts=103 的 4 得 count=2,avg=7
	//	{a:`2,b="x"`}    再写 ts=150,value=13 → 与 ts=106 的 7 得 count=2,avg=10
	//	{zone:out}       再写 ts=5000,value=42（区间过滤时用于排除）
	mustOK(t, store, "["+strings.Join([]string{
		sample("cpu", 300, 10, map[string]string{"a": "2"}),
		sample("cpu", 150, 13, map[string]string{"a": `2,b="x"`}),
		sample("cpu", 5000, 42, map[string]string{"zone": "out"}),
	}, ",")+"]")

	// 全量查询（[0,5000]）：19 条 cpu 的顺序与快照一致；点数/均值逐条核对，
	// 含逗号/引号的身份不与其他序列串换统计值。默认每条一个点、值为其写入值，
	// 仅三条追加过第二点的序列覆盖成 count=2 的实际统计。
	full := mustQuery(t, store, queryLabels("cpu", 0, 5000, map[string]string{}))
	if got, want := queryIdentitySigs(full), wantSigs(specs[:19]); !reflect.DeepEqual(got, want) {
		t.Fatalf("full query order = %v\nwant %v", got, want)
	}
	wantStats := map[string]struct {
		count int
		avg   float64
	}{}
	for _, sp := range specs[:19] {
		wantStats[identitySig(sp.name, sp.labels)] = struct {
			count int
			avg   float64
		}{1, sp.value}
	}
	for sig, st := range map[string]struct {
		count int
		avg   float64
	}{
		identitySig("cpu", map[string]string{"a": "2"}):       {2, 7},
		identitySig("cpu", map[string]string{"a": `2,b="x"`}): {2, 10},
		identitySig("cpu", map[string]string{"zone": "out"}):  {2, 30.5},
	} {
		wantStats[sig] = st
	}
	for _, s := range full.Series {
		sig := identitySig(s.Name, s.Labels)
		want, ok := wantStats[sig]
		if !ok {
			t.Fatalf("unexpected series in full query: %s", sig)
		}
		if s.Count != want.count || s.Average != want.avg {
			t.Fatalf("%s: count=%d average=%v, want %d/%v", sig, s.Count, s.Average, want.count, want.avg)
		}
		// 保留条目继续携带各自完整标签（map 与身份一致，未被子集条件裁剪）。
		if !reflect.DeepEqual(s.Labels, labelOf(specs, sig)) {
			t.Fatalf("%s labels = %v, want full label set", sig, s.Labels)
		}
	}

	// 子集 a=2 只命中真实值恰为 "2" 的三条（值文本为 `2,b="x"` 的序列不是 "2"，
	// 不得被匹配）。它们在全量结果中的相对位置（specs[3..5]）原样保留，
	// 双点序列在前、两个单点序列随后，count/avg 各自正确不串换。
	sub := mustQuery(t, store, queryLabels("cpu", 0, 2000, map[string]string{"a": "2"}))
	wantSub := []struct {
		sig   string
		count int
		avg   float64
	}{
		{identitySig("cpu", map[string]string{"a": "2"}), 2, 7},
		{identitySig("cpu", map[string]string{"a": "2", "b": ""}), 1, 5},
		{identitySig("cpu", map[string]string{"a": "2", "b": "1"}), 1, 6},
	}
	if len(sub.Series) != len(wantSub) {
		t.Fatalf("a=2 subset = %+v, want %d series", sub.Series, len(wantSub))
	}
	for i, w := range wantSub {
		got := sub.Series[i]
		if sig := identitySig(got.Name, got.Labels); sig != w.sig {
			t.Fatalf("subset series[%d] = %s, want %s", i, sig, w.sig)
		}
		if got.Count != w.count || got.Average != w.avg {
			t.Fatalf("%s stats = %d/%v, want %d/%v", w.sig, got.Count, got.Average, w.count, w.avg)
		}
	}
	// 含逗号/引号的值按完整值精确命中其唯一序列（不是 a=2 的子集结果），
	// 它带两个点、均值 10，统计值不与上面三条串换。
	delim := queryOne(t, store, queryLabels("cpu", 0, 2000, map[string]string{"a": `2,b="x"`}))
	if delim.Count != 2 || delim.Average != 10 {
		t.Fatalf("exact delimiter-value series = %+v, want count=2 average=10", delim)
	}

	// 空字符串值是真实标签：b="" 命中 {a:2,b:""} 与 {aa:1,b:""} 两条，
	// 顺序沿用全量排序（specs[4] 在 specs[10] 前），且不会命中缺 b 的序列。
	emptyVal := mustQuery(t, store, queryLabels("cpu", 0, 2000, map[string]string{"b": ""}))
	if got := queryIdentitySigs(emptyVal); !reflect.DeepEqual(got, []string{
		identitySig("cpu", map[string]string{"a": "2", "b": ""}),
		identitySig("cpu", map[string]string{"aa": "1", "b": ""}),
	}) {
		t.Fatalf("b=\"\" subset order = %v", got)
	}
	if emptyVal.Series[0].Average != 5 || emptyVal.Series[1].Average != 11 {
		t.Fatalf("b=\"\" averages = %v,%v, want 5 and 11", emptyVal.Series[0].Average, emptyVal.Series[1].Average)
	}

	// 中文值与“额外标签”序列：k=中 命中两条，带 z=v 的一条排后（先比键 k 的值，
	// 值相同才轮到下一对 z），且保留 z=v 这个子集之外的完整标签。
	cn := mustQuery(t, store, queryLabels("cpu", 0, 2000, map[string]string{"k": "中"}))
	if got := queryIdentitySigs(cn); !reflect.DeepEqual(got, []string{
		identitySig("cpu", map[string]string{"k": "中"}),
		identitySig("cpu", map[string]string{"k": "中", "z": "v"}),
	}) {
		t.Fatalf("k=中 subset order = %v", got)
	}
	if cn.Series[1].Labels["z"] != "v" || cn.Series[1].Count != 1 || cn.Series[1].Average != 18 {
		t.Fatalf("extra-label series stats/labels = %+v", cn.Series[1])
	}

	// 其他指标名精确匹配；名称中的逗号与中文按原名命中各自唯一序列。
	if got := queryOne(t, store, queryAll("cpu,total", 0, 5000)); got.Count != 1 || got.Average != 20 {
		t.Fatalf("cpu,total query = %+v", got)
	}
	if got := queryOne(t, store, queryLabels("指标", 0, 5000, map[string]string{"k": "v"})); got.Count != 1 || got.Average != 21 {
		t.Fatalf("chinese-named query = %+v", got)
	}

	// 只有区间外采样点的序列不出现：
	//	[2001,4999] 内 a=2 四条都没有点 → 空数组；
	//	k=中 两条的点都在 116/117，区间 [3001,4999] → 空数组；
	//	zone=out 的第二点恰在 5000：[5000,5000] 闭区间命中一条 count=1,avg=42。
	for _, line := range []string{
		queryLabels("cpu", 2001, 4999, map[string]string{"a": "2"}),
		queryLabels("cpu", 3001, 4999, map[string]string{"k": "中"}),
	} {
		if qr := mustQuery(t, store, line); len(qr.Series) != 0 {
			t.Fatalf("%s = %+v, want empty series array", line, qr.Series)
		}
	}
	atEnd := queryOne(t, store, queryLabels("cpu", 5000, 5000, map[string]string{"zone": "out"}))
	if atEnd.Count != 1 || atEnd.Average != 42 {
		t.Fatalf("closed-range endpoint query = %+v, want count=1 average=42", atEnd)
	}
}

// labelOf 在规范序列表中按身份签名找回完整标签集合。
func labelOf(specs []identitySpec, sig string) map[string]string {
	for _, sp := range specs {
		if identitySig(sp.name, sp.labels) == sig {
			return sp.labels
		}
	}
	return nil
}

// jsonStringEscaped 与 sample 的编码同义，但允许把指定字符写成合法的 \uXXXX
// 转义（逗号、等号、引号、反斜杠、中文等），解析后仍是原字符串；标签按按键
// 排序的逆序输出，制造与“直接书写+正序”完全不同的词法写法。
func jsonStringEscaped(s string, escape func(rune) bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case escape(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// escapedSample 构造一个采样点：名称与标签值里的指定字符以 \uXXXX 书写，
// 标签按键排序后逆序书写；解析后身份与直接书写完全一致。
func escapedSample(sp identitySpec, escape func(rune) bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"name":%s,"timestamp":%d,"value":%v`,
		jsonStringEscaped(sp.name, escape), sp.ts, sp.value)
	if sp.labels != nil {
		keys := sortedKeys(sp.labels)
		b.WriteString(`,"labels":{`)
		for i := len(keys) - 1; i >= 0; i-- {
			if i != len(keys)-1 {
				b.WriteByte(',')
			}
			k := keys[i]
			b.WriteString(jsonStringEscaped(k, escape))
			b.WriteByte(':')
			b.WriteString(jsonStringEscaped(sp.labels[k], escape))
		}
		b.WriteString(`}`)
	}
	b.WriteByte('}')
	return b.String()
}

// TestOrderingSameWhetherLiteralOrJSONEscaped：同一身份直接书写，或把逗号、等号、
// 引号、反斜杠、中文写成合法 JSON 字符转义并打乱标签书写顺序，得到的快照排列、
// 查询排列与每条统计值必须一致；冲突原因中为看清身份加的引号/转义文字不影响
// 身份与排序，结构化冲突仍指向解析后的真实标签。
func TestOrderingSameWhetherLiteralOrJSONEscaped(t *testing.T) {
	specs := orderingSpecs()
	escape := func(r rune) bool { return strings.ContainsRune(`,="\中`, r) }

	literal := NewMetricStore()
	mustOK(t, literal, orderingBatch(specs))
	mustOK(t, literal, "["+strings.Join([]string{
		sample("cpu", 300, 10, map[string]string{"a": "2"}),
		sample("cpu", 150, 13, map[string]string{"a": `2,b="x"`}),
		sample("cpu", 5000, 42, map[string]string{"zone": "out"}),
	}, ",")+"]")

	encoded := NewMetricStore()
	shuffled := permutedSpecs(specs, shuffledOrderingPerm)
	parts := make([]string, len(shuffled))
	for i, sp := range shuffled {
		parts[i] = escapedSample(sp, escape)
	}
	mustOK(t, encoded, "["+strings.Join(parts, ",")+"]")
	mustOK(t, encoded, "["+strings.Join([]string{
		escapedSample(identitySpec{name: "cpu", labels: map[string]string{"a": `2,b="x"`}, ts: 150, value: 13}, escape),
		escapedSample(identitySpec{name: "cpu", labels: map[string]string{"zone": "out"}, ts: 5000, value: 42}, escape),
		escapedSample(identitySpec{name: "cpu", labels: map[string]string{"a": "2"}, ts: 300, value: 10}, escape),
	}, ",")+"]")

	// 两份数据经不同写法提交，全量快照身份排列与逐序列采样点逐一相同。
	litSnap := mustOK(t, literal, `[]`)
	encSnap := mustOK(t, encoded, `[]`)
	if !reflect.DeepEqual(snapshotIdentitySigs(litSnap), snapshotIdentitySigs(encSnap)) {
		t.Fatalf("snapshot order differs: literal %v vs escaped %v",
			snapshotIdentitySigs(litSnap), snapshotIdentitySigs(encSnap))
	}
	if !reflect.DeepEqual(litSnap.Series, encSnap.Series) {
		t.Fatalf("snapshots differ beyond ordering: %+v vs %+v", litSnap.Series, encSnap.Series)
	}

	// 全量查询与子集+区间过滤查询（含只命中区间外点的空结果）在两种写法下结构相同。
	for _, q := range []string{
		queryAll("cpu", 0, 5000),
		queryLabels("cpu", 0, 2000, map[string]string{"a": "2"}),
		queryLabels("cpu", 0, 2000, map[string]string{"b": ""}),
		queryLabels("cpu", 0, 2000, map[string]string{"k": "中"}),
		queryLabels("cpu", 2001, 4999, map[string]string{"a": "2"}),
		queryLabels("指标", 0, 5000, map[string]string{"k": "v"}),
	} {
		lr := mustQuery(t, literal, q)
		er := mustQuery(t, encoded, q)
		if !reflect.DeepEqual(lr, er) {
			t.Fatalf("query %s differs between literal and escaped stores:\n%+v\n%+v", q, lr, er)
		}
	}

	// 以转义+逆序写法重提两个既有同点同值（一个含中文双标签、一个含空值标签）：
	// 仍是重复采样，不新增点，排列不变。
	dup := mustOK(t, encoded, "["+strings.Join([]string{
		escapedSample(identitySpec{name: "cpu", labels: map[string]string{"k": "中", "z": "v"}, ts: 117, value: 18}, escape),
		escapedSample(identitySpec{name: "cpu", labels: map[string]string{"aa": "1", "b": ""}, ts: 110, value: 11}, escape),
	}, ",")+"]")
	if dup.Added != 0 || dup.Duplicates != 2 {
		t.Fatalf("escaped resubmit = added %d duplicates %d, want 0/2", dup.Added, dup.Duplicates)
	}
	if got := snapshotIdentitySigs(dup); !reflect.DeepEqual(got, wantSigs(specs)) {
		t.Fatalf("order changed after escaped duplicates: %v", got)
	}

	// 以转义写法对引号值序列提交不同数值：整批冲突拒绝，结构化身份是解析后的
	// 真实标签 {k:"}（不是渲染文本 k="\""），原因文本中的引号只为可辨识，
	// 不改变排序依据的身份。
	lerr := mustFail(t, encoded,
		"["+escapedSample(identitySpec{name: "cpu", labels: map[string]string{"k": `"`}, ts: 113, value: 999}, escape)+"]")
	if lerr.Conflict == nil || lerr.Conflict.Existing != 14 || lerr.Conflict.Submitted != 999 {
		t.Fatalf("escaped conflict detail = %+v", lerr.Conflict)
	}
	if !reflect.DeepEqual(lerr.Conflict.Series.Labels, map[string]string{"k": `"`}) {
		t.Fatalf("escaped conflict labels = %v, want parsed {k:\"}", lerr.Conflict.Series.Labels)
	}
	if !strings.Contains(lerr.Error, `series cpu{k="\""} at timestamp 113`) {
		t.Fatalf("escaped conflict message = %q", lerr.Error)
	}
	// 冲突拒绝不改变事实与排列。
	if got := mustQuery(t, encoded, queryLabels("cpu", 113, 113, map[string]string{"k": `"`})); got.Series[0].Average != 14 {
		t.Fatalf("value changed after rejected conflict batch: %+v", got.Series)
	}
}
