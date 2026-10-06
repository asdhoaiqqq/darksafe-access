package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// 本文件从命令行入口（一次 ingest 进程内）回归保障用户的复核流程：在同一份
// 已成功写入的数据上发送 op 为 "query" 的对象行，再仅把 op 改成
// "query_points"（指标名、标签条件、起止时间逐字保持相同）。相邻两行输出必须
// 描述同一组采样：
//
//   - 两行 series 的序列身份（指标名+完整标签集合）与排列次序逐一相同；
//   - 统计行每条记录的 count 等于明细行对应序列的采样点数量；
//   - 统计行 average 等于明细点（经 JSON 往返后的 float64 存储值）用精确
//     有理数平均、最近偶舍入独立复算的结果；
//   - 明细点按时间戳升序且全部落在闭区间内；
//   - 标签子集命中多条序列时各条分别对应，额外标签完整保留；
//   - 条件未命中时两行都成功输出空 series 数组（[]），不补零记录。
//
// 与 darksafe 包内的 Go 级对应测试互补：这里数据真正经过 stdin 逐行写入、
// JSON 编码输出与退出码路径，保证用户在命令行上做的复核自洽。

// cpSig 以规范 JSON（map 键排序）表示一条序列身份，用于把相邻两行的序列配对。
func cpSig(name string, labels map[string]interface{}) string {
	b, _ := json.Marshal(struct {
		Name   string                 `json:"name"`
		Labels map[string]interface{} `json:"labels"`
	}{name, labels})
	return string(b)
}

// cpSigs 按输出次序列出一行中全部序列的身份签名。
func cpSigs(series []interface{}) []string {
	out := make([]string, len(series))
	for i, e := range series {
		m := e.(map[string]interface{})
		out[i] = cpSig(m["name"].(string), m["labels"].(map[string]interface{}))
	}
	return out
}

// cpDetailMean 从明细行的采样点独立复算算术平均：每个 JSON 往返后的 float64
// 值经 big.Rat 精确求和、精确除以点数，再按最近偶舍入为 float64。
func cpDetailMean(t *testing.T, points []interface{}) float64 {
	t.Helper()
	if len(points) == 0 {
		t.Fatalf("internal: cpDetailMean with no points")
	}
	sum := new(big.Rat)
	r := new(big.Rat)
	for _, e := range points {
		v := e.(map[string]interface{})["value"].(float64)
		sum.Add(sum, r.SetFloat64(v))
	}
	sum.Quo(sum, big.NewRat(int64(len(points)), 1))
	f, _ := sum.Float64()
	return f
}

// assertOutputPairCorrespond 核对相邻的 query / query_points 两条成功输出行
// 逐序列对应。start/end 用于失败信息指出区间，并验证明细点落在闭区间内。
func assertOutputPairCorrespond(t *testing.T, statsLine, detailLine string, start, end int64) {
	t.Helper()
	sm := decodeResultLine(t, statsLine)
	dm := decodeResultLine(t, detailLine)
	if sm["status"] != "ok" || sm["op"] != "query" {
		t.Fatalf("[%d,%d] stats line = %v, want ok/query", start, end, sm)
	}
	if dm["status"] != "ok" || dm["op"] != "query_points" {
		t.Fatalf("[%d,%d] detail line = %v, want ok/query_points", start, end, dm)
	}
	stats := sm["series"].([]interface{})
	detail := dm["series"].([]interface{})
	if len(stats) != len(detail) {
		t.Fatalf("[%d,%d] series count: query=%d (%v), query_points=%d (%v)",
			start, end, len(stats), cpSigs(stats), len(detail), cpSigs(detail))
	}
	for i := range stats {
		se := stats[i].(map[string]interface{})
		de := detail[i].(map[string]interface{})
		sig := cpSig(se["name"].(string), se["labels"].(map[string]interface{}))
		if sig != cpSig(de["name"].(string), de["labels"].(map[string]interface{})) {
			t.Fatalf("[%d,%d] series[%d] identity/order mismatch: stats=%s detail=%s",
				start, end, i,
				cpSig(se["name"].(string), se["labels"].(map[string]interface{})),
				cpSig(de["name"].(string), de["labels"].(map[string]interface{})))
		}
		points := de["points"].([]interface{})
		count := int(se["count"].(float64))
		if count != len(points) {
			t.Fatalf("[%d,%d] %s: count=%d but detail has %d points",
				start, end, sig, count, len(points))
		}
		if count == 0 {
			t.Fatalf("[%d,%d] %s: a zero-point record must not appear in either line", start, end, sig)
		}
		// 明细时间戳严格升序，且全部在闭区间内。
		var prevTS float64
		for j, pe := range points {
			pm := pe.(map[string]interface{})
			ts := pm["timestamp"].(float64)
			if ts < float64(start) || ts > float64(end) {
				t.Fatalf("[%d,%d] %s: detail point[%d] ts=%v is outside the closed range",
					start, end, sig, j, ts)
			}
			if j > 0 && prevTS >= ts {
				t.Fatalf("[%d,%d] %s: detail points not strictly ascending at %d: %v",
					start, end, sig, j, points)
			}
			prevTS = ts
		}
		// average 与明细独立复算一致（最近偶）。
		wantAvg := cpDetailMean(t, points)
		if got := se["average"].(float64); got != wantAvg {
			t.Fatalf("[%d,%d] %s: average=%v but recomputed from detail = %v (points=%v)",
				start, end, sig, got, wantAvg, points)
		}
	}
}

// TestRunIngestQueryAndPointsCorrespondEndToEnd 是命令行端到端核心复核：
// 先写入任务书指定的采样（含区间外点与同名不同标签序列），随后成对发送
// 仅 op 不同的两种查询，逐对核对身份/次序/count/平均/明细。
func TestRunIngestQueryAndPointsCorrespondEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：一次写入全部数据。
		// host=a：区间外 500/4000，区间内 1000=1e16、2000=1、3000=-1e16。
		`[{"name":"cpu","timestamp":500,"value":77,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":1000,"value":10000000000000000,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":2000,"value":1,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":3000,"value":-10000000000000000,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":4000,"value":88,"labels":{"host":"a"}},` +
			// 同名不同标签的另一条序列，混入即改变 host=a 的均值。
			`{"name":"cpu","timestamp":2000,"value":999999,"labels":{"host":"b"}},` +
			// 另一个指标：子集命中两条，其中一条带查询条件之外的额外标签。
			`{"name":"m","timestamp":10,"value":100,"labels":{"zone":"x"}},` +
			`{"name":"m","timestamp":20,"value":200,"labels":{"zone":"x","host":"h"}},` +
			`{"name":"m","timestamp":30,"value":400,"labels":{"zone":"y"}}]`,
		// 行 2/3：host=a 的 [1000,3000]，仅 op 不同。
		`{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
		`{"op":"query_points","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}`,
		// 行 4/5：省略 labels 命中全部 cpu 序列（host=a 与 host=b）。
		`{"op":"query","name":"cpu","start":1000,"end":3000}`,
		`{"op":"query_points","name":"cpu","start":1000,"end":3000}`,
		// 行 6/7：标签子集 zone=x 命中两条 m，额外 host 标签必须保留。
		`{"op":"query","name":"m","start":0,"end":10000,"labels":{"zone":"x"}}`,
		`{"op":"query_points","name":"m","start":0,"end":10000,"labels":{"zone":"x"}}`,
		// 行 8/9：start==end 且该位置有点。
		`{"op":"query","name":"cpu","start":2000,"end":2000,"labels":{"host":"a"}}`,
		`{"op":"query_points","name":"cpu","start":2000,"end":2000,"labels":{"host":"a"}}`,
		// 行 10/11：区间内没有任何点，两行都成功返回空数组。
		`{"op":"query","name":"cpu","start":4001,"end":9000}`,
		`{"op":"query_points","name":"cpu","start":4001,"end":9000}`,
	}, "\n")

	var out strings.Builder
	if code := runIngest(strings.NewReader(input), &out); code != 0 {
		t.Fatalf("all lines succeed, exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 11 {
		t.Fatalf("got %d output lines, want 11: %v", len(lines), lines)
	}

	// 行 2/3：核心取消例子——逐对身份/count/平均对应。
	assertOutputPairCorrespond(t, lines[1], lines[2], 1000, 3000)
	sm := decodeResultLine(t, lines[1])
	dm := decodeResultLine(t, lines[2])
	sStats := sm["series"].([]interface{})
	sDetail := dm["series"].([]interface{})
	if len(sStats) != 1 || len(sDetail) != 1 {
		t.Fatalf("host=a [1000,3000]: lines must each have one cpu{host=a} entry, got %d/%d",
			len(sStats), len(sDetail))
	}
	hostAStat := sStats[0].(map[string]interface{})
	hostADetail := sDetail[0].(map[string]interface{})
	if hostAStat["count"].(float64) != 3 || hostAStat["average"].(float64) != 1.0/3.0 {
		t.Fatalf("host-a stats = %v, want count=3 average=1/3 (middle 1 must survive cancellation)",
			hostAStat)
	}
	if b, _ := json.Marshal(hostAStat["average"]); string(b) != "0.3333333333333333" {
		t.Fatalf("host-a average JSON = %s, want 0.3333333333333333", b)
	}
	wantPoints := []map[string]float64{
		{"timestamp": 1000, "value": 1e16},
		{"timestamp": 2000, "value": 1},
		{"timestamp": 3000, "value": -1e16},
	}
	gotPoints := hostADetail["points"].([]interface{})
	if len(gotPoints) != 3 {
		t.Fatalf("host-a detail points = %v, want 3 ascending points", gotPoints)
	}
	for i, w := range wantPoints {
		gp := gotPoints[i].(map[string]interface{})
		if gp["timestamp"].(float64) != w["timestamp"] || gp["value"].(float64) != w["value"] {
			t.Fatalf("host-a detail point[%d] = %v, want %v (out-of-range 500/4000 excluded)",
				i, gp, w)
		}
	}

	// 行 4/5：全部 cpu 序列分别统计，host=a 与 host=b 不混算。
	assertOutputPairCorrespond(t, lines[3], lines[4], 1000, 3000)
	allStats := decodeResultLine(t, lines[3])["series"].([]interface{})
	allDetail := decodeResultLine(t, lines[4])["series"].([]interface{})
	if len(allStats) != 2 || len(allDetail) != 2 {
		t.Fatalf("all cpu series: want 2 entries in both lines, got %d/%d",
			len(allStats), len(allDetail))
	}
	bySig := func(series []interface{}) map[string]map[string]interface{} {
		m := make(map[string]map[string]interface{})
		for _, e := range series {
			em := e.(map[string]interface{})
			m[cpSig(em["name"].(string), em["labels"].(map[string]interface{}))] = em
		}
		return m
	}(allStats)
	if e := bySig[cpSig("cpu", map[string]interface{}{"host": "a"})]; e["count"].(float64) != 3 ||
		e["average"].(float64) != 1.0/3.0 {
		t.Fatalf("host=a must stay separate: %v, want count=3 average=1/3", e)
	}
	if e := bySig[cpSig("cpu", map[string]interface{}{"host": "b"})]; e["count"].(float64) != 1 ||
		e["average"].(float64) != 999999 {
		t.Fatalf("host=b must be its own record: %v, want count=1 average=999999", e)
	}
	// 两行的序列次序也必须一致（host=a 在前、host=b 在后）。
	if !reflect.DeepEqual(cpSigs(allStats), cpSigs(allDetail)) {
		t.Fatalf("series order differs between lines: stats=%v detail=%v",
			cpSigs(allStats), cpSigs(allDetail))
	}

	// 行 6/7：子集 zone=x 命中两条 m，额外 host 标签完整保留，各条分别对应。
	assertOutputPairCorrespond(t, lines[5], lines[6], 0, 10000)
	mStats := decodeResultLine(t, lines[5])["series"].([]interface{})
	mDetail := decodeResultLine(t, lines[6])["series"].([]interface{})
	if len(mStats) != 2 || len(mDetail) != 2 {
		t.Fatalf("zone=x subset: want 2 m series in both lines, got %d/%d",
			len(mStats), len(mDetail))
	}
	// 期望次序：带 host=h 额外标签的在前（host < zone），单标签在后。
	wantSigs := []string{
		cpSig("m", map[string]interface{}{"host": "h", "zone": "x"}),
		cpSig("m", map[string]interface{}{"zone": "x"}),
	}
	if got := cpSigs(mStats); !reflect.DeepEqual(got, wantSigs) {
		t.Fatalf("zone=x stats order = %v, want %v", got, wantSigs)
	}
	if got := cpSigs(mDetail); !reflect.DeepEqual(got, wantSigs) {
		t.Fatalf("zone=x detail order = %v, want %v", got, wantSigs)
	}
	// 额外标签必须真实出现在记录里（不能缩减成查询条件 {"zone":"x"}）。
	extraLabels := mStats[0].(map[string]interface{})["labels"].(map[string]interface{})
	if _, hasHost := extraLabels["host"]; !hasHost || len(extraLabels) != 2 {
		t.Fatalf("extra label dropped from stats record: %v", extraLabels)
	}
	extraDetailLabels := mDetail[0].(map[string]interface{})["labels"].(map[string]interface{})
	if _, hasHost := extraDetailLabels["host"]; !hasHost || len(extraDetailLabels) != 2 {
		t.Fatalf("extra label dropped from detail record: %v", extraDetailLabels)
	}

	// 行 8/9：start==end 单点，count=1、平均等于该点存储值。
	assertOutputPairCorrespond(t, lines[7], lines[8], 2000, 2000)
	oneStat := decodeResultLine(t, lines[7])["series"].([]interface{})
	oneDetail := decodeResultLine(t, lines[8])["series"].([]interface{})
	if len(oneStat) != 1 || oneStat[0].(map[string]interface{})["count"].(float64) != 1 ||
		oneStat[0].(map[string]interface{})["average"].(float64) != 1 {
		t.Fatalf("[2000,2000] stats = %v, want count=1 average=1", oneStat)
	}
	if pts := oneDetail[0].(map[string]interface{})["points"].([]interface{}); len(pts) != 1 ||
		pts[0].(map[string]interface{})["timestamp"].(float64) != 2000 ||
		pts[0].(map[string]interface{})["value"].(float64) != 1 {
		t.Fatalf("[2000,2000] detail = %v, want single (2000,1)", pts)
	}

	// 行 10/11：无命中两行都成功返回空数组（[] 而非 null），不补零记录。
	for i, line := range []string{lines[9], lines[10]} {
		m := decodeResultLine(t, line)
		op := [...]string{"query", "query_points"}[i]
		if m["status"] != "ok" || m["op"] != op {
			t.Fatalf("empty %s line = %v, want ok/%s", op, m, op)
		}
		if series, ok := m["series"].([]interface{}); !ok || len(series) != 0 {
			t.Fatalf("empty %s series = %v, want empty array []", op, m["series"])
		}
		// 原始 JSON 必须是 []，不能是 null，也不能出现 count:0/average:0 记录。
		if !strings.Contains(line, `"series":[]`) {
			t.Fatalf("empty %s JSON = %s, want \"series\":[]", op, line)
		}
		if strings.Contains(line, `"count":0`) || strings.Contains(line, `"average":0`) {
			t.Fatalf("empty %s must not fabricate zero records: %s", op, line)
		}
	}
}

// TestRunIngestQueryAndPointsIdentityOrderStableAcrossPairs 多写几条同名序列，
// 验证在多个不同区间重复“query 后紧跟 query_points”时，两行身份次序始终一致，
// 且与写入快照的规范次序（无标签在前，其后按标签键值对字典序）相同；区间只
// 命中部分序列时，两行保留的必须是同一个子序列。
func TestRunIngestQueryAndPointsIdentityOrderStableAcrossPairs(t *testing.T) {
	var b strings.Builder
	b.WriteString(`[`)
	// host=a..d 与无标签共 5 条 cpu 序列，时间戳与值互不相同，使次序串换或
	// 跨序列混算都会改变某条记录的 count/average 而被抓住。
	samples := []struct {
		ts    int
		value int
		host  string
	}{
		{10, 1, "d"}, {20, 2, "c"}, {30, 3, "b"}, {40, 4, "a"}, {50, 5, ""},
	}
	for i, sp := range samples {
		if i > 0 {
			b.WriteByte(',')
		}
		if sp.host == "" {
			fmt.Fprintf(&b, `{"name":"cpu","timestamp":%d,"value":%d}`, sp.ts, sp.value)
		} else {
			fmt.Fprintf(&b, `{"name":"cpu","timestamp":%d,"value":%d,"labels":{"host":"%s"}}`,
				sp.ts, sp.value, sp.host)
		}
	}
	b.WriteString("]\n")

	// 规范次序：无标签在前，其后 host=a、b、c、d。
	sigUnlabeled := cpSig("cpu", map[string]interface{}{})
	sigA := cpSig("cpu", map[string]interface{}{"host": "a"})
	sigB := cpSig("cpu", map[string]interface{}{"host": "b"})
	sigC := cpSig("cpu", map[string]interface{}{"host": "c"})
	sigD := cpSig("cpu", map[string]interface{}{"host": "d"})

	ranges := []struct {
		start, end int
		want       []string
	}{
		{0, 100, []string{sigUnlabeled, sigA, sigB, sigC, sigD}},
		{15, 45, []string{sigA, sigB, sigC}}, // 命中 a(40)、b(30)、c(20)
		{20, 40, []string{sigA, sigB, sigC}},
		{30, 30, []string{sigB}}, // 只命中 b(30,3)
		{60, 90, []string{}},     // 区间内无点
	}
	for _, rr := range ranges {
		fmt.Fprintf(&b, `{"op":"query","name":"cpu","start":%d,"end":%d}`+"\n", rr.start, rr.end)
		fmt.Fprintf(&b, `{"op":"query_points","name":"cpu","start":%d,"end":%d}`+"\n", rr.start, rr.end)
	}

	var out strings.Builder
	if code := runIngest(strings.NewReader(b.String()), &out); code != 0 {
		t.Fatalf("all lines succeed, exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if want := 1 + 2*len(ranges); len(lines) != want {
		t.Fatalf("got %d output lines, want %d", len(lines), want)
	}
	for i, rr := range ranges {
		statsLine := lines[1+2*i]
		detailLine := lines[2+2*i]
		assertOutputPairCorrespond(t, statsLine, detailLine, int64(rr.start), int64(rr.end))
		ss := cpSigs(decodeResultLine(t, statsLine)["series"].([]interface{}))
		ds := cpSigs(decodeResultLine(t, detailLine)["series"].([]interface{}))
		// 两行身份次序彼此一致，且正是规范次序在该区间上的子序列。
		if !reflect.DeepEqual(ss, rr.want) {
			t.Fatalf("range [%d,%d] stats order = %v, want canonical %v", rr.start, rr.end, ss, rr.want)
		}
		if !reflect.DeepEqual(ds, rr.want) {
			t.Fatalf("range [%d,%d] detail order = %v, want canonical %v", rr.start, rr.end, ds, rr.want)
		}
	}
}
