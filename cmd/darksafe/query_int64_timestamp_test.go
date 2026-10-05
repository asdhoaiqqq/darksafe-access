package main

import (
	"bytes"
	"strings"
	"testing"
)

// 端到端回归保障：毫秒时间戳在整个 int64 范围内的精确区间选择。
// 同一序列在 9007199254740992（2^53）与 9007199254740993（2^53+1，作为
// float64 会舍入回 2^53）分别保存 2 与 8 后：单点查询各得一个点，包含两者
// 的区间得到两个点、平均 5；start/end 越界与区间倒置按原有规则报错且不附带
// index/conflict/统计结果；失败查询之后各行仍被处理，已写入数据不变，
// 命令最终以非零退出码结束。
func TestRunIngestInt64TimestampQueryEndToEnd(t *testing.T) {
	input := strings.Join([]string{
		// 行 1：非时间顺序写入两个相邻毫秒位置（先 2^53+1 后 2^53）。
		`[{"name":"cpu","timestamp":9007199254740993,"value":8,"labels":{"host":"a"}},` +
			`{"name":"cpu","timestamp":9007199254740992,"value":2,"labels":{"host":"a"}}]`,
		// 行 2：只查 2^53+1 一个位置 → count 1、average 8。
		`{"op":"query","name":"cpu","start":9007199254740993,"end":9007199254740993,"labels":{"host":"a"}}`,
		// 行 3：只查 2^53 一个位置 → count 1、average 2。
		`{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740992,"labels":{"host":"a"}}`,
		// 行 4：end 超出 int64 范围 → 失败，原因指出 end 越界。
		`{"op":"query","name":"cpu","start":0,"end":9223372036854775808}`,
		// 行 5：start 低于 int64 最小值 → 失败，原因指出 start 越界。
		`{"op":"query","name":"cpu","start":-9223372036854775809,"end":0}`,
		// 行 6：两者合法但 start > end → 失败，保留实际边界值。
		`{"op":"query","name":"cpu","start":9007199254740993,"end":9007199254740992}`,
		// 行 7：失败查询之后，包含两个位置的区间仍得到 count 2、average 5。
		`{"op":"query","name":"cpu","start":9007199254740992,"end":9007199254740993,"labels":{"host":"a"}}`,
		// 行 8：没有采样的区间 → 成功且 series 为空数组。
		`{"op":"query","name":"cpu","start":0,"end":100}`,
	}, "\n")

	var out bytes.Buffer
	code := runIngest(strings.NewReader(input), &out)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero because query lines failed")
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("got %d output lines, want 8: %v", len(lines), lines)
	}

	// 行 1：两个相邻毫秒位置都写入成功（added=2，未被合并为冲突或重复），
	// 且输出原文保留 2^53+1 的精确时间戳文本。
	m := decodeResultLine(t, lines[0])
	if m["status"] != "ok" || m["added"].(float64) != 2 || m["duplicates"].(float64) != 0 {
		t.Fatalf("line 1 = %v, want two distinct points written", m)
	}
	if !strings.Contains(lines[0], `"timestamp":9007199254740993`) ||
		!strings.Contains(lines[0], `"timestamp":9007199254740992`) {
		t.Fatalf("line 1 output lost an adjacent-millisecond timestamp: %s", lines[0])
	}

	// 行 2、3、7：单点查询与全区间的统计。
	checkQuery := func(idx int, wantCount, wantAvg float64) {
		t.Helper()
		q := decodeResultLine(t, lines[idx])
		if q["status"] != "ok" || q["op"] != "query" {
			t.Fatalf("output %d = %v, want ok query", idx, q)
		}
		series := q["series"].([]interface{})
		if len(series) != 1 {
			t.Fatalf("output %d series = %v, want exactly one", idx, series)
		}
		s0 := series[0].(map[string]interface{})
		if s0["name"] != "cpu" || s0["count"].(float64) != wantCount || s0["average"].(float64) != wantAvg {
			t.Fatalf("output %d series[0] = %v, want cpu count=%v average=%v", idx, s0, wantCount, wantAvg)
		}
		labels := s0["labels"].(map[string]interface{})
		if len(labels) != 1 || labels["host"] != "a" {
			t.Fatalf("output %d labels = %v, want the full label set host=a", idx, labels)
		}
	}
	checkQuery(1, 1, 8)
	checkQuery(2, 1, 2)
	checkQuery(6, 2, 5)

	// 行 4、5、6：边界失败——保留原始行号，不带 index/conflict/统计结果，
	// 原因分别指出越界字段与倒置区间的实际边界。
	wantErr := []struct {
		outIdx int
		line   int
		substr []string
	}{
		{3, 4, []string{`"end"`, "int64 range"}},
		{4, 5, []string{`"start"`, "int64 range"}},
		{5, 6, []string{"invalid range", "9007199254740993 > 9007199254740992"}},
	}
	for _, w := range wantErr {
		e := decodeResultLine(t, lines[w.outIdx])
		if e["status"] != "error" || int(e["line"].(float64)) != w.line {
			t.Fatalf("output %d = %v, want error on input line %d", w.outIdx, e, w.line)
		}
		for _, absent := range []string{"index", "conflict", "series"} {
			if _, has := e[absent]; has {
				t.Fatalf("line %d query failure must not carry %s: %v", w.line, absent, e)
			}
		}
		msg, _ := e["error"].(string)
		for _, sub := range w.substr {
			if !strings.Contains(msg, sub) {
				t.Fatalf("line %d error = %q, want substring %q", w.line, msg, sub)
			}
		}
	}

	// 行 8：无数据区间返回成功的空 series 数组，不是 count=0 或 average=0 的序列。
	q := decodeResultLine(t, lines[7])
	if q["status"] != "ok" || q["op"] != "query" {
		t.Fatalf("output 7 = %v, want ok query", q)
	}
	if series := q["series"].([]interface{}); len(series) != 0 {
		t.Fatalf("output 7 series = %v, want empty (no count=0 entries)", series)
	}
	if !strings.Contains(lines[7], `"series":[]`) {
		t.Fatalf("output 7 = %s, want an explicit empty series array", lines[7])
	}
}
