package main

import (
	"bytes"
	"strings"
	"testing"
)

// 端到端回归保障：毫秒时间戳在整个 int64 范围内必须被精确选择，且查询边界
// 失败保持原有结果约定。本测试通过一次 ingest 调用覆盖：
//
//   - 同一序列在 2^53=9007199254740992 和 2^53+1=9007199254740993 分别存值
//     2 和 8（点按非时间顺序写入）：单点查询各自 count=1、平均分别为 2 和 8，
//     含两端的闭区间才 count=2、平均 5；两个紧邻位置不能因时间戳精度丢失合并；
//   - 相同规则适用于负的大整数对 -2^53-1/-2^53；
//   - 合法时间戳 MinInt64、0、MaxInt64：端点相等只统计该点，全范围包含全部
//     三个点（平均 5），跨零窄区间 [-1,1] 只包含 0 点；
//   - 没有采样的区间返回成功与空 series 数组（不是 count=0 的序列、不是零均值）；
//   - start/end 超出 int64：错误指出该字段越界，无 index、无 conflict；
//     start>end：报告区间倒置并保留实际边界值；
//   - 失败查询不改变已写入数据，命令仍继续处理随后各行，最终返回非零退出码；
//   - 查询结果的指标名与完整标签集合（含子集条件之外的额外标签）对应被统计序列。
func TestRunIngestInt64TimestampPrecisionAndQueryFailures(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：非时间顺序写入两个紧邻大时间戳（先 2^53+1=8，后 2^53=2），
		// 标签带两个键，查询只按 host=a 子集匹配，结果必须保留完整标签。
		`[{"name":"cpu","timestamp":9007199254740993,"value":8,"labels":{"host":"a","zone":"x"}},` +
			`{"name":"cpu","timestamp":9007199254740992,"value":2,"labels":{"host":"a","zone":"x"}}]`,
		// 行 2：单点查 2^53 → count=1、平均 2。
		`{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740992,"labels":{"host":"a"}}`,
		``, // 行 3：空白行，只占行号
		// 行 4：单点查 2^53+1 → count=1、平均 8（不能把 2 算进来）。
		`{"op":"query","name":"cpu","start":9007199254740993,"end":9007199254740993,"labels":{"host":"a"}}`,
		// 行 5：闭区间覆盖两点 → count=2、平均 5。
		`{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740993,"labels":{"host":"a"}}`,
		// 行 6：负的大整数对，同样非时间顺序写入（-2^53=2 先、-2^53-1=8 后）。
		`[{"name":"neg","timestamp":-9007199254740992,"value":2},{"name":"neg","timestamp":-9007199254740993,"value":8}]`,
		// 行 7：负向紧邻区间 → count=2、平均 5。
		`{"op":"query","name":"neg","start":-9007199254740993,"end":-9007199254740992}`,
		// 行 8：int64 两端与零，非时间顺序写入，值 2/5/8（均值也是 5）。
		`[{"name":"ext","timestamp":0,"value":5},{"name":"ext","timestamp":9223372036854775807,"value":8},` +
			`{"name":"ext","timestamp":-9223372036854775808,"value":2}]`,
		// 行 9：全范围 [MinInt64,MaxInt64] 包含全部三个点。
		`{"op":"query","name":"ext","start":-9223372036854775808,"end":9223372036854775807}`,
		// 行 10：跨零窄区间只包含 0 点。
		`{"op":"query","name":"ext","start":-1,"end":1}`,
		// 行 11：区间内没有任何点 → 成功、空 series 数组。
		`{"op":"query","name":"ext","start":1,"end":9223372036854775806}`,
		// 行 12：start 超出 int64 上界 → 指出 start 越界。
		`{"op":"query","name":"cpu","start":9223372036854775808,"end":9223372036854775807,"labels":{"host":"a"}}`,
		// 行 13：end 超出 int64 下界 → 指出 end 越界。
		`{"op":"query","name":"ext","start":-9223372036854775808,"end":-9223372036854775809}`,
		// 行 14：两端合法但 start>end（极值倒置）→ 报告倒置并给出实际边界值。
		`{"op":"query","name":"ext","start":9223372036854775807,"end":-9223372036854775808}`,
		// 行 15：失败行之后继续处理；neg 在 [-1,1] 没有点 → 空 series。
		`{"op":"query","name":"neg","start":-1,"end":1}`,
		// 行 16：后续合法查询仍得到失败前的统计：两点、平均 5、完整标签。
		`{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740993,"labels":{"host":"a"}}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because query lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	// 行 3 空白不产生结果：共 15 条输出。
	if len(lines) != 15 {
		t.Fatalf("got %d output lines, want 15: %v", len(lines), lines)
	}

	// assertCPUPrecisionQuery 校验一条 cpu{host=a,zone=x} 查询结果。
	assertCPUPrecisionQuery := func(idx int, wantCount, wantAvg float64, note string) {
		t.Helper()
		m := decodeResultLine(t, lines[idx])
		if m["status"] != "ok" || m["op"] != "query" {
			t.Fatalf("%s: output %d = %v, want ok query", note, idx, m)
		}
		series := m["series"].([]interface{})
		if len(series) != 1 {
			t.Fatalf("%s: output %d series = %v, want exactly one", note, idx, series)
		}
		s0 := series[0].(map[string]interface{})
		if s0["name"] != "cpu" {
			t.Fatalf("%s: name = %v, want cpu", note, s0["name"])
		}
		// 完整标签必须对应被统计序列：子集只给了 host=a，结果仍带 zone=x。
		labels := s0["labels"].(map[string]interface{})
		if len(labels) != 2 || labels["host"] != "a" || labels["zone"] != "x" {
			t.Fatalf("%s: labels = %v, want full set host=a,zone=x", note, labels)
		}
		if s0["count"].(float64) != wantCount || s0["average"].(float64) != wantAvg {
			t.Fatalf("%s: count/avg = %v/%v, want %v/%v",
				note, s0["count"], s0["average"], wantCount, wantAvg)
		}
	}

	// 行 1：两个点都写入。
	if m := decodeResultLine(t, lines[0]); m["status"] != "ok" || m["added"].(float64) != 2 {
		t.Fatalf("line 1 = %v, want added=2", m)
	}
	// 行 2 / 行 4：两个紧邻位置各自独立寻址，不被精度丢失合并。
	assertCPUPrecisionQuery(1, 1, 2, "single point at 2^53")
	assertCPUPrecisionQuery(2, 1, 8, "single point at 2^53+1")
	// 行 5：含两端才是两个点、平均 5。
	assertCPUPrecisionQuery(3, 2, 5, "range covering 2^53 and 2^53+1")

	// 行 6：负向大整数对写入成功。
	if m := decodeResultLine(t, lines[4]); m["status"] != "ok" || m["added"].(float64) != 2 {
		t.Fatalf("line 6 = %v, want added=2", m)
	}
	// 行 7：负向紧邻区间 count=2、平均 5，无标签序列输出 {}。
	m := decodeResultLine(t, lines[5])
	negSeries := m["series"].([]interface{})
	if m["status"] != "ok" || len(negSeries) != 1 {
		t.Fatalf("line 7 = %v, want one neg series", m)
	}
	neg0 := negSeries[0].(map[string]interface{})
	if neg0["name"] != "neg" || len(neg0["labels"].(map[string]interface{})) != 0 ||
		neg0["count"].(float64) != 2 || neg0["average"].(float64) != 5 {
		t.Fatalf("negative adjacent query = %v, want neg {} count=2 average=5", neg0)
	}

	// 行 8：三个端点写入。
	if m := decodeResultLine(t, lines[6]); m["status"] != "ok" || m["added"].(float64) != 3 {
		t.Fatalf("line 8 = %v, want added=3", m)
	}
	// 行 9：全范围包含全部点。
	m = decodeResultLine(t, lines[7])
	if m["status"] != "ok" {
		t.Fatalf("line 9 = %v, want ok", m)
	}
	full := m["series"].([]interface{})[0].(map[string]interface{})
	if full["count"].(float64) != 3 || full["average"].(float64) != 5 {
		t.Fatalf("full int64 range = %v, want count=3 average=5", full)
	}
	// 行 10：[-1,1] 只包含 0 点。
	m = decodeResultLine(t, lines[8])
	narrow := m["series"].([]interface{})[0].(map[string]interface{})
	if narrow["count"].(float64) != 1 || narrow["average"].(float64) != 5 {
		t.Fatalf("cross-zero narrow range = %v, want count=1 average=5", narrow)
	}
	// 行 11：无点区间成功且为空数组（不能出现 count=0/avg=0 的序列）。
	if raw := strings.TrimSpace(lines[9]); raw != `{"status":"ok","op":"query","series":[]}` {
		t.Fatalf("empty-range line = %q, want empty-array success", raw)
	}

	// 行 12-14：三条失败查询，只有错误，无 index/conflict/统计结果。
	wantErr := []struct {
		outIdx  int
		line    int
		substrs []string
	}{
		{10, 12, []string{`field "start"`, "must be within int64 range"}},
		{11, 13, []string{`field "end"`, "must be within int64 range"}},
		{12, 14, []string{"invalid range", "9223372036854775807 > -9223372036854775808"}},
	}
	for _, w := range wantErr {
		m := decodeResultLine(t, lines[w.outIdx])
		if m["status"] != "error" || int(m["line"].(float64)) != w.line {
			t.Fatalf("output %d = %v, want error on input line %d", w.outIdx, m, w.line)
		}
		if _, has := m["index"]; has {
			t.Fatalf("line %d query failure must not carry index: %v", w.line, m)
		}
		if _, has := m["conflict"]; has {
			t.Fatalf("line %d query failure must not carry conflict: %v", w.line, m)
		}
		if _, has := m["series"]; has {
			t.Fatalf("line %d query failure must not carry stats: %v", w.line, m)
		}
		msg := m["error"].(string)
		for _, sub := range w.substrs {
			if !strings.Contains(msg, sub) {
				t.Fatalf("line %d error = %q, want substring %q", w.line, msg, sub)
			}
		}
	}

	// 行 15：失败行之后仍被处理，无点区间返回空数组。
	if raw := strings.TrimSpace(lines[13]); raw != `{"status":"ok","op":"query","series":[]}` {
		t.Fatalf("post-failure empty-range line = %q, want empty-array success", raw)
	}
	// 行 16：后续合法查询仍得到失败前的统计，已写入数据未被失败查询改变。
	assertCPUPrecisionQuery(14, 2, 5, "query after failed query lines")
}
