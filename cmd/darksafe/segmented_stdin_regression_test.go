package main

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 本文件在命令行端到端层面回归保障 ingest 的分段送达（packetization）
// 无关性：同一份标准输入，无论 Read 按什么大小返回数据，片段的结束位置
// 都不能被当成一行结束，每个非空白输入行只按原顺序产生一条完整的 JSON
// 结果；写入数量（added）、重复数量（duplicates）、查询统计（count/average）
// 以及最终退出状态都必须与整份一次到达时完全一致，空白行仍只占原始行号。
//
// 测试借助一个按指定边界把输入切成大小不一的片段逐段送达的 io.Reader，
// 在同一个输入脚本上遍历多种分段方式（含单字节与对齐 64 KiB 读缓冲的
// 边界），并直接构造原始字节，把多字节字符、JSON 转义以及代理项对的
// 字节精确放到片段边界上。

// segmentedReader 按绝对字节偏移 cuts 把 data 分成若干片段，逐段 Read：
// 每个片段在一次 Read 中完整返回，片段之间没有其他字节。例如
// cuts={3,7} 时依次返回 data[:3]、data[3:7]、data[7:]。cuts 必须落在
// (0, len(data)) 内，会排序去重；cuts 为空时整份数据一次返回。
type segmentedReader struct {
	data []byte
	pos  int
	cuts []int
	idx  int
}

func newSegmentedReader(data []byte, cuts ...int) *segmentedReader {
	kept := cuts[:0:0]
	for _, c := range cuts {
		if c > 0 && c < len(data) {
			kept = append(kept, c)
		}
	}
	sort.Ints(kept)
	out := kept[:0:0]
	for i, c := range kept {
		if i == 0 || c != out[len(out)-1] {
			out = append(out, c)
		}
	}
	return &segmentedReader{data: data, cuts: out}
}

func (r *segmentedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= len(r.data) {
		// 最后一段返回后，下一次调用才给出 EOF：
		// 即使最后一段恰好填满 p，也不会丢失或提前结束。
		return 0, io.EOF
	}
	for r.idx < len(r.cuts) && r.cuts[r.idx] <= r.pos {
		r.idx++
	}
	end := len(r.data)
	if r.idx < len(r.cuts) {
		end = r.cuts[r.idx]
		r.idx++
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	if r.pos < end {
		// p 小于当前段（本测试不会发生：p 为 64 KiB，段不超过 64 KiB）：
		// 保留边界在同一位置，下次继续返回本段剩余部分。
		r.idx--
	}
	return n, nil
}

// segmentEvery 把整份输入按固定大小逐段送达。
func segmentEvery(data []byte, size int) io.Reader {
	if size <= 0 {
		size = 1
	}
	var cuts []int
	for p := size; p < len(data); p += size {
		cuts = append(cuts, p)
	}
	return newSegmentedReader(data, cuts...)
}

// segments1Byte 逐字节送达（最强分段情形：多字节字符与转义的每个字节
// 都可能单独到达）。
func segments1Byte(data []byte) io.Reader {
	return segmentEvery(data, 1)
}

// runIngestForSegmentation 以给定 reader 执行一次 ingest，返回退出码与
// 逐条结果（已解析的 JSON）；输出必须始终是一条结果占一行。
func runIngestForSegmentation(t *testing.T, in io.Reader) (int, []map[string]interface{}) {
	t.Helper()
	var out bytes.Buffer
	code := runIngest(in, &out)
	text := out.String()
	if text != "" && !strings.HasSuffix(text, "\n") {
		t.Fatalf("every result must end with newline, got %q", text)
	}
	var results []map[string]interface{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		results = append(results, decodeResultLine(t, line))
	}
	return code, results
}

// baselineResults 以整份一次到达的方式跑出基准退出码与逐条结果。
func baselineResults(t *testing.T, data []byte) (int, []map[string]interface{}) {
	t.Helper()
	return runIngestForSegmentation(t, bytes.NewReader(data))
}

// assertMatchesBaseline 断言某种分段送达与整份一次到达产生完全相同的
// 退出码与逐条结果。
func assertMatchesBaseline(t *testing.T, label string, in io.Reader, baseCode int, baseRes []map[string]interface{}) {
	t.Helper()
	code, res := runIngestForSegmentation(t, in)
	if code != baseCode {
		t.Fatalf("%s: exit code = %d, want %d (same as whole-input delivery)",
			label, code, baseCode)
	}
	if len(res) != len(baseRes) {
		t.Fatalf("%s: %d result lines, want %d", label, len(res), len(baseRes))
	}
	for i := range baseRes {
		if !reflect.DeepEqual(res[i], baseRes[i]) {
			t.Fatalf("%s: result %d = %s\nwant %s",
				label, i, mustJSON(res[i]), mustJSON(baseRes[i]))
		}
	}
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(b)
}

// uescape 以 ASCII 文本返回一个 JSON Unicode 转义（如 uescape("D83D") -> `\uD83D`），
// 使含反斜杠的原始字节可以在源码里以纯 ASCII 构造、精确放到片段边界上。
func uescape(hex string) string {
	return `\u` + hex
}

// segmentationVariants 在同一份输入上生成一组有代表性的分段方式：
// 单字节、2/3/5/7/16/64/100 字节等大小不一的段，以及对齐和错开
// 64 KiB 读缓冲的段（不含整份基线，基线单独用 baselineResults 跑）。
func segmentationVariants(data []byte) []struct {
	label string
	in    io.Reader
} {
	type variant struct {
		label string
		in    io.Reader
	}
	mk := func(label string, in io.Reader) variant { return variant{label, in} }
	vs := []variant{
		mk("1-byte", segments1Byte(data)),
		mk("2-byte", segmentEvery(data, 2)),
		mk("3-byte", segmentEvery(data, 3)),
		mk("5-byte", segmentEvery(data, 5)),
		mk("7-byte", segmentEvery(data, 7)),
		mk("16-byte", segmentEvery(data, 16)),
		mk("64-byte", segmentEvery(data, 64)),
		mk("100-byte", segmentEvery(data, 100)),
		mk("64KiB-aligned", segmentEvery(data, 64*1024)),
		mk("64KiB-offset-by-1", io.MultiReader(
			bytes.NewReader(data[:1]),
			segmentEvery(data[1:], 64*1024),
		)),
	}
	out := make([]struct {
		label string
		in    io.Reader
	}, len(vs))
	for i, v := range vs {
		out[i] = struct {
			label string
			in    io.Reader
		}{v.label, v.in}
	}
	return out
}

// TestRunIngestSegmentationInvariantWritesAndQueries 是核心回归场景：
// 同一序列先后写入 ts=1000 值 2、ts=2000 值 4，再等值重复其中一点（计 1 次
// 重复），随后查询覆盖两个点的闭区间，应得到 count=2、average=3；中间穿插
// 空白行（只占行号）。该脚本在各种分段方式下输出与退出码必须逐条相同。
func TestRunIngestSegmentationInvariantWritesAndQueries(t *testing.T) {
	lines := []string{
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`, // 行 1
		``, // 行 2：空白行，无结果但占行号
		`{"op":"query","name":"cpu","start":1000,"end":1000,"labels":{"host":"a"}}`, // 行 3
		`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`,         // 行 4
		`   `, // 行 5：空白行
		`[{"name":"cpu","timestamp":1000,"value":2.0,"labels":{"host":"a"}}]`,       // 行 6：等值重复
		`{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`, // 行 7
		`{"op":"query","name":"cpu","start":1000,"end":2000}`,                       // 行 8：省略标签
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	baseCode, baseRes := baselineResults(t, data)
	if baseCode != 0 {
		t.Fatalf("baseline exit code = %d, want 0", baseCode)
	}
	if len(baseRes) != 6 {
		t.Fatalf("baseline got %d results, want 6 (3 writes + 3 queries)", len(baseRes))
	}
	// 行 1：added=1。
	if baseRes[0]["status"] != "ok" || baseRes[0]["added"].(float64) != 1 {
		t.Fatalf("baseline result 0 = %s", mustJSON(baseRes[0]))
	}
	// 行 3：单点查询 avg=2。
	q := baseRes[1]["series"].([]interface{})
	if len(q) != 1 || q[0].(map[string]interface{})["average"].(float64) != 2 {
		t.Fatalf("baseline ts=1000 query = %s", mustJSON(baseRes[1]))
	}
	// 行 4：added=1。
	if baseRes[2]["added"].(float64) != 1 {
		t.Fatalf("baseline result 2 = %s", mustJSON(baseRes[2]))
	}
	// 行 6：同序列同时间戳等值（2 == 2.0）写入算重复，added=0 duplicates=1。
	if baseRes[3]["added"].(float64) != 0 || baseRes[3]["duplicates"].(float64) != 1 {
		t.Fatalf("baseline duplicate line = %s, want added=0 duplicates=1", mustJSON(baseRes[3]))
	}
	// 行 7：闭区间覆盖两个点，count=2 average=(2+4)/2=3。
	q = baseRes[4]["series"].([]interface{})
	if len(q) != 1 {
		t.Fatalf("baseline range query series = %s", mustJSON(baseRes[4]))
	}
	s := q[0].(map[string]interface{})
	if s["count"].(float64) != 2 || s["average"].(float64) != 3 {
		t.Fatalf("baseline range query = %s, want count=2 average=3", mustJSON(s))
	}
	// 行 8：不带标签的同名查询命中同一条序列。
	q = baseRes[5]["series"].([]interface{})
	if len(q) != 1 || q[0].(map[string]interface{})["count"].(float64) != 2 {
		t.Fatalf("baseline unlabeled query = %s", mustJSON(baseRes[5]))
	}

	for _, v := range segmentationVariants(data) {
		v := v
		t.Run(v.label, func(t *testing.T) {
			assertMatchesBaseline(t, v.label, v.in, baseCode, baseRes)
		})
	}
}

// TestRunIngestSegmentationInvariantWithFailures 确认在含失败行（解析失败、
// 冲突、非零退出）的输入上，分段同样不改变任何结果与最终退出状态：失败行
// 的行号、index/conflict，以及后续成功行都与整份到达时一致。
func TestRunIngestSegmentationInvariantWithFailures(t *testing.T) {
	lines := []string{
		`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`, // 行 1 成功
		`not json`, // 行 2 整行解析失败，无 index
		``,         // 行 3 空白
		`[{"name":"cpu","timestamp":1000,"value":4,"labels":{"host":"a"}}]`, // 行 4 冲突
		`   `, // 行 5 空白
		`[]`,  // 行 6 空批成功，快照仍只有行 1
		`{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`, // 行 7
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	baseCode, baseRes := baselineResults(t, data)
	if baseCode == 0 {
		t.Fatalf("baseline exit code = 0, want non-zero")
	}
	for _, v := range segmentationVariants(data) {
		v := v
		t.Run(v.label, func(t *testing.T) {
			assertMatchesBaseline(t, v.label, v.in, baseCode, baseRes)
		})
	}
	if got := baseRes[1]; got["status"] != "error" || int(got["line"].(float64)) != 2 {
		t.Fatalf("baseline parse error = %s", mustJSON(got))
	}
	if _, has := baseRes[1]["index"]; has {
		t.Fatalf("parse error must not carry index: %s", mustJSON(baseRes[1]))
	}
	if got := baseRes[2]; got["status"] != "error" || int(got["line"].(float64)) != 4 ||
		int(got["index"].(float64)) != 1 {
		t.Fatalf("baseline conflict = %s", mustJSON(got))
	}
	if baseRes[4]["series"].([]interface{})[0].(map[string]interface{})["count"].(float64) != 1 {
		t.Fatalf("baseline query must see only the committed point: %s", mustJSON(baseRes[4]))
	}
}

// TestRunIngestMultibyteTextSurvivesFragmentation 覆盖中文（3 字节/字）与
// 补充平面字符（4 字节/字，如 U+1F600 😀）跨片段送达：一个 UTF-8 字符的字节
// 可以分开到达。完整输入合法时，输出必须保留还原后的真实名称与完整标签，
// 不得因分段出现字符替换（U+FFFD）、丢失或意外产生另一条序列。
func TestRunIngestMultibyteTextSurvivesFragmentation(t *testing.T) {
	name := "指标😀"
	labelKey := "机房😀"
	labelVal := "北京😀"
	writeLine := `[{"name":"` + name + `","timestamp":1,"value":1,"labels":{` +
		`"` + labelKey + `":"` + labelVal + `"}}]`
	queryLine := `{"op":"query","name":"` + name + `","start":0,"end":10,"labels":{` +
		`"` + labelKey + `":"` + labelVal + `"}}`
	data := []byte(writeLine + "\n" + queryLine + "\n")

	// 收集落在多字节字符内部的字节边界：续字节（10xxxxxx）所在偏移处切开，
	// 必然把某个 UTF-8 字符的字节分开。
	var internalCuts []int
	for i := 1; i < len(data); i++ {
		if data[i]&0xC0 == 0x80 {
			internalCuts = append(internalCuts, i)
		}
	}
	if len(internalCuts) == 0 {
		t.Fatal("test setup: expected multibyte characters to split")
	}

	checkRealIdentity := func(t *testing.T, res []map[string]interface{}) {
		t.Helper()
		if len(res) != 2 {
			t.Fatalf("%d results, want 2: %s", len(res), mustJSON(res))
		}
		arr := res[0]["series"].([]interface{})
		if len(arr) != 1 {
			t.Fatalf("series = %s, want exactly 1 (no accidental extra series)", mustJSON(arr))
		}
		s := arr[0].(map[string]interface{})
		if s["name"] != name {
			t.Fatalf("name = %q, want %q (no replacement/loss)", s["name"], name)
		}
		if !reflect.DeepEqual(s["labels"], map[string]interface{}{labelKey: labelVal}) {
			t.Fatalf("labels = %s, want the full real labels", mustJSON(s["labels"]))
		}
		q := res[1]["series"].([]interface{})
		if len(q) != 1 {
			t.Fatalf("query = %s, want exactly 1 match", mustJSON(res[1]))
		}
		q0 := q[0].(map[string]interface{})
		if q0["name"] != name {
			t.Fatalf("query name = %q, want %q", q0["name"], name)
		}
		if !reflect.DeepEqual(q0["labels"],
			map[string]interface{}{labelKey: labelVal}) {
			t.Fatalf("query labels = %s", mustJSON(q0))
		}
	}

	_, baseRes := baselineResults(t, data)
	checkRealIdentity(t, baseRes)

	// 每个多字节字符内部边界单独切开（前后各一整段）。
	for _, d := range internalCuts {
		d := d
		t.Run("split-inside-char@"+strconv.Itoa(d), func(t *testing.T) {
			_, res := runIngestForSegmentation(t, newSegmentedReader(data, d))
			checkRealIdentity(t, res)
		})
	}
	// 所有多字节字符同时在内部被切开，以及逐字节送达。
	t.Run("split-inside-every-multibyte-char", func(t *testing.T) {
		_, res := runIngestForSegmentation(t, newSegmentedReader(data, internalCuts...))
		checkRealIdentity(t, res)
	})
	t.Run("1-byte", func(t *testing.T) {
		_, res := runIngestForSegmentation(t, segments1Byte(data))
		checkRealIdentity(t, res)
	})
}

// TestRunIngestJSONEscapesSurviveFragmentation 覆盖 JSON 转义跨片段：
// 反斜杠与其后的字符可以分开送达（"\u" 之间断开、hex 之间断开），成对代理项
// 也可以在两个 \uXXXX 中间分开（甚至每个字节都分开）。直接书写与合法转义
// 书写的相同身份应仍能互相查询，同序列同时间戳的等值写入仍算重复；输出保留
// 还原后的真实名称与完整标签。
func TestRunIngestJSONEscapesSurviveFragmentation(t *testing.T) {
	// 用 ASCII 片段拼出 \uXXXX 转义（避免源码里直接写转义被还原）：
	// 指 U+6307、标 U+6807；😀 U+1F600 = D83D DE00；机 U+673A、房 U+623F；
	// 北 U+5317、京 U+4EAC。
	escName := uescape("6307") + uescape("6807") + uescape("D83D") + uescape("DE00")
	escKey := uescape("673A") + uescape("623F") + uescape("D83D") + uescape("DE00")
	escVal := uescape("5317") + uescape("4EAC") + uescape("D83D") + uescape("DE00")

	direct := `[{"name":"指标😀","timestamp":1000,"value":2,"labels":{"机房😀":"北京😀"}}]`
	write2000 := `[{"name":"指标😀","timestamp":2000,"value":4,"labels":{"机房😀":"北京😀"}}]`
	dupEscaped := `[{"name":"` + escName + `","timestamp":2000,"value":4,` +
		`"labels":{"` + escKey + `":"` + escVal + `"}}]`
	queryEscaped := `{"op":"query","name":"` + escName + `","start":0,"end":3000,` +
		`"labels":{"` + escKey + `":"` + escVal + `"}}`
	data := []byte(strings.Join([]string{direct, write2000, dupEscaped, queryEscaped}, "\n") + "\n")

	// 对每个 \uXXXX 转义，在反斜杠之后（\ 与 u 之间）、u 与 hex 之间、
	// 每个 hex 之间以及转义之后（成对代理项两部分的中间，即第一个 \uXXXX
	// 与第二个 \uXXXX 之间）都放置边界。\uXXXX 占 6 字节，k=1..6 覆盖
	// “反斜杠之后”到“两个代理项转义中间”的全部位置。
	var escapeCuts []int
	for i := 0; i < len(data); i++ {
		if data[i] == '\\' && i+1 < len(data) && data[i+1] == 'u' {
			for k := 1; k <= 6 && i+k < len(data); k++ {
				escapeCuts = append(escapeCuts, i+k)
			}
		}
	}
	sort.Ints(escapeCuts)
	if len(escapeCuts) == 0 {
		t.Fatal("test setup: expected \\uXXXX escapes to split")
	}

	realName := "指标😀"
	realKey := "机房😀"
	realVal := "北京😀"
	checkFlow := func(t *testing.T, res []map[string]interface{}) {
		t.Helper()
		if len(res) != 4 {
			t.Fatalf("%d results, want 4: %s", len(res), mustJSON(res))
		}
		if res[0]["added"].(float64) != 1 {
			t.Fatalf("write 1 = %s, want added=1", mustJSON(res[0]))
		}
		if res[1]["added"].(float64) != 1 {
			t.Fatalf("write 2 = %s, want added=1", mustJSON(res[1]))
		}
		// 转义书写与直接书写是同一序列同一时间戳的等值写入：算重复。
		if res[2]["added"].(float64) != 0 || res[2]["duplicates"].(float64) != 1 {
			t.Fatalf("escaped duplicate = %s, want added=0 duplicates=1", mustJSON(res[2]))
		}
		// 转义书写的查询命中直接书写的序列，输出还原后的真实名称与标签，
		// count=2、average=(2+4)/2=3。
		q := res[3]["series"].([]interface{})
		if len(q) != 1 {
			t.Fatalf("query = %s, want 1 match", mustJSON(res[3]))
		}
		s := q[0].(map[string]interface{})
		if s["name"] != realName ||
			!reflect.DeepEqual(s["labels"], map[string]interface{}{realKey: realVal}) ||
			s["count"].(float64) != 2 || s["average"].(float64) != 3 {
			t.Fatalf("query result = %s, want real identity count=2 average=3", mustJSON(s))
		}
	}

	_, baseRes := baselineResults(t, data)
	checkFlow(t, baseRes)

	for _, d := range escapeCuts {
		d := d
		t.Run("split-in-escape@"+strconv.Itoa(d), func(t *testing.T) {
			_, res := runIngestForSegmentation(t, newSegmentedReader(data, d))
			checkFlow(t, res)
		})
	}
	t.Run("split-at-every-escape-boundary", func(t *testing.T) {
		_, res := runIngestForSegmentation(t, newSegmentedReader(data, escapeCuts...))
		checkFlow(t, res)
	})
	t.Run("1-byte", func(t *testing.T) {
		_, res := runIngestForSegmentation(t, segments1Byte(data))
		checkFlow(t, res)
	})
}

// TestRunIngestLineLargerThanReadBufferStaysOneRequest 覆盖一条超过 64 KiB
// （读缓冲大小）、但远低于 64 MiB 上限的合法行：它跨越多个读取缓冲边界后
// 仍必须作为“一个”请求处理，只产出一条结果，而不是被缓冲边界拆成多条。
func TestRunIngestLineLargerThanReadBufferStaysOneRequest(t *testing.T) {
	// 单个采样点的标签值是一段长文本（含多字节字符），使行内容约 80 KiB，
	// 远大于 64 KiB 读缓冲、远小于 64 MiB 上限。
	longVal := strings.Repeat("x😀", 20*1024) // 20480 个重复单元，约 80 KiB
	bigLine := `[{"name":"big","timestamp":1,"value":1,"labels":{"pad":"` + longVal + `"}}]`
	if len(bigLine) <= 64*1024 {
		t.Fatalf("test setup: line only %d bytes, want > 64 KiB", len(bigLine))
	}
	if len(bigLine) >= maxIngestLineBytes {
		t.Fatalf("test setup: line %d bytes must stay well under 64 MiB", len(bigLine))
	}
	data := []byte(bigLine + "\n" + `{"op":"query","name":"big","start":0,"end":10}` + "\n")

	baseCode, baseRes := baselineResults(t, data)
	if baseCode != 0 {
		t.Fatalf("baseline exit code = %d, want 0", baseCode)
	}
	if len(baseRes) != 2 || baseRes[0]["added"].(float64) != 1 {
		t.Fatalf("baseline = %s, want one big write + one query", mustJSON(baseRes))
	}
	for _, v := range segmentationVariants(data) {
		v := v
		t.Run(v.label, func(t *testing.T) {
			code, res := runIngestForSegmentation(t, v.in)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if len(res) != 2 {
				t.Fatalf("%d results, want exactly 2 (one big write + one query, "+
					"buffer boundaries must not split the line): %s", len(res), mustJSON(res))
			}
			if res[0]["status"] != "ok" || res[0]["added"].(float64) != 1 {
				t.Fatalf("big write = %s, want one added point", mustJSON(res[0]))
			}
			arr := res[0]["series"].([]interface{})
			if len(arr) != 1 {
				t.Fatalf("series = %s, want 1", mustJSON(arr))
			}
			gotVal := arr[0].(map[string]interface{})["labels"].(map[string]interface{})["pad"].(string)
			if gotVal != longVal {
				t.Fatalf("long label value length = %d, want %d (no loss across buffers)",
					len(gotVal), len(longVal))
			}
			q := res[1]["series"].([]interface{})
			if len(q) != 1 || q[0].(map[string]interface{})["count"].(float64) != 1 {
				t.Fatalf("query = %s, want the one big-line point", mustJSON(res[1]))
			}
		})
	}
}

// oneByte 只输出单个字节，然后 EOF；用于把分隔符本身单独作为一个片段送达。
type oneByte struct {
	b byte
	n int
}

func (r *oneByte) Read(p []byte) (int, error) {
	if r.n > 0 || len(p) == 0 {
		if r.n > 0 {
			return 0, io.EOF
		}
		return 0, nil
	}
	r.n++
	p[0] = r.b
	return 1, nil
}

// TestRunIngestNewlineAndCRLFSeparatorsAcrossFragments 覆盖换行分隔符本身被
// 分段的情形：'\n' 作为单独片段送达，以及回车与换行分开送达。无论怎样分段，
// 结果条数、退出码与行号约定都必须与整份一次到达一致（沿用现有以 '\n'
// 分隔、'\r' 留在行尾、JSON 尾随空白被跳过的行为）。
func TestRunIngestNewlineAndCRLFSeparatorsAcrossFragments(t *testing.T) {
	good := `[{"name":"m","timestamp":1,"value":1}]`

	// 场景 A：'\n' 单独送达，包括空白行的换行也单独送达；
	// 行 3 "bad" 解析失败，错误行号必须仍指向真实行号 3。
	aWhole := []byte(good + "\n\n" + "bad" + "\n" + "[]" + "\n")
	aSplit := io.MultiReader(
		bytes.NewReader([]byte(good)), &oneByte{b: '\n'},
		&oneByte{b: '\n'}, // 空白行（行 2）的换行单独送达
		bytes.NewReader([]byte("bad")), &oneByte{b: '\n'},
		bytes.NewReader([]byte("[]")), &oneByte{b: '\n'},
	)
	t.Run("newline-as-its-own-fragment", func(t *testing.T) {
		baseCode, baseRes := baselineResults(t, aWhole)
		assertMatchesBaseline(t, "standalone newlines", aSplit, baseCode, baseRes)
		if len(baseRes) != 3 {
			t.Fatalf("%d results, want 3 (write ok, line-3 error, empty batch ok)", len(baseRes))
		}
		if baseRes[1]["status"] != "error" || int(baseRes[1]["line"].(float64)) != 3 {
			t.Fatalf("error = %s, want parse failure on line 3", mustJSON(baseRes[1]))
		}
	})

	// 场景 B：'\r' 与 '\n' 分开送达（CRLF 的两部分各占一个片段）。
	// 现状约定：以 '\n' 分隔，'\r' 留在行内容末尾；'\r' 是 JSON 空白，
	// "[]\r" 仍是合法空批，"\r" 单独一行是空白行（无结果但占行号）。
	bWhole := []byte("[]\r\n\r\n[]\n")
	bSplit := io.MultiReader(
		bytes.NewReader([]byte("[]\r")), &oneByte{b: '\n'},
		bytes.NewReader([]byte("\r")), &oneByte{b: '\n'},
		bytes.NewReader([]byte("[]")), &oneByte{b: '\n'},
	)
	t.Run("cr-and-lf-separate-fragments", func(t *testing.T) {
		baseCode, baseRes := baselineResults(t, bWhole)
		assertMatchesBaseline(t, "CRLF split", bSplit, baseCode, baseRes)
		if baseCode != 0 || len(baseRes) != 2 {
			t.Fatalf("baseline CRLF = code %d, %s; want code 0 and 2 results "+
				"([]\\r ok, \\r blank, [] ok)", baseCode, mustJSON(baseRes))
		}
	})
}

// TestRunIngestFinalLineWithoutNewlineProcessedOnce 输入正常结束时，最后一条
// 完整请求即使没有结尾换行，也应处理一次；分段送达下同样只处理一次、不重复。
func TestRunIngestFinalLineWithoutNewlineProcessedOnce(t *testing.T) {
	data := []byte(`[{"name":"m","timestamp":1,"value":1}]` + "\n" + `[]`) // 末行无换行
	baseCode, baseRes := baselineResults(t, data)
	for _, v := range segmentationVariants(data) {
		v := v
		t.Run(v.label, func(t *testing.T) {
			assertMatchesBaseline(t, v.label, v.in, baseCode, baseRes)
		})
	}
	if baseCode != 0 || len(baseRes) != 2 {
		t.Fatalf("baseline = code %d %s, want code 0 and exactly 2 results",
			baseCode, mustJSON(baseRes))
	}
	if baseRes[0]["added"].(float64) != 1 || baseRes[1]["status"] != "ok" {
		t.Fatalf("results = %s", mustJSON(baseRes))
	}
}

// TestRunIngestUnclosedFinalLineFailsWithoutCommit 若末行 JSON 尚未闭合（输入
// 结束），只报告该行解析失败，不带 index 或 conflict，也不返回成功写入结果；
// 分段不能使未闭合的前缀被当成一条完整请求。
func TestRunIngestUnclosedFinalLineFailsWithoutCommit(t *testing.T) {
	good := `[{"name":"m","timestamp":1,"value":1}]`
	data := []byte(good + "\n" + `[{"name":"m","timestamp":2,"value":2}`) // 缺 ] 且无换行
	baseCode, baseRes := baselineResults(t, data)
	for _, v := range segmentationVariants(data) {
		v := v
		t.Run(v.label, func(t *testing.T) {
			assertMatchesBaseline(t, v.label, v.in, baseCode, baseRes)
		})
	}
	if baseCode == 0 {
		t.Fatalf("baseline exit code = 0, want non-zero for unclosed final line")
	}
	if len(baseRes) != 2 {
		t.Fatalf("baseline %d results, want 2 (ok then one parse failure): %s",
			len(baseRes), mustJSON(baseRes))
	}
	errLine := baseRes[1]
	if errLine["status"] != "error" || int(errLine["line"].(float64)) != 2 {
		t.Fatalf("final error = %s, want error on input line 2", mustJSON(errLine))
	}
	if _, has := errLine["index"]; has {
		t.Fatalf("unclosed line error must not carry index: %s", mustJSON(errLine))
	}
	if _, has := errLine["conflict"]; has {
		t.Fatalf("unclosed line error must not carry conflict: %s", mustJSON(errLine))
	}
	if !strings.Contains(errLine["error"].(string), "unexpected EOF") {
		t.Fatalf("error = %q, want substring %q", errLine["error"], "unexpected EOF")
	}
}

// TestRunIngestCorruptTextAcrossFragmentsStillRejected 完整行里确实存在非法
// UTF-8 字节或孤立代理项时，分段不能使损坏文本被接受：仍应整行失败（无
// index/conflict），保留之前成功写入的数据，后续合法行继续处理，最终非零
// 退出。即使损坏字节或转义恰好在片段边界处分开送达，结论也不变。
func TestRunIngestCorruptTextAcrossFragmentsStillRejected(t *testing.T) {
	good := `[{"name":"keep","timestamp":1,"value":1}]`
	later := `[{"name":"later","timestamp":1,"value":1}]`
	query := `{"op":"query","name":"keep","start":0,"end":10}`

	// 损坏行 A：name 中夹非法字节 0xFF；以及多字节序列被截断（E4 B8 后缺尾字节）。
	badUTF8 := []byte(`[{"name":"cpu` + "\xff" + `","timestamp":1,"value":1}]`)
	badUTF8Trunc := []byte(`[{"name":"abc` + "\xe4\xb8" + `","timestamp":1,"value":1}]`)
	// 损坏行 B：孤立高代理项转义；孤立代理项的查询。
	badSurrogate := []byte(`[{"name":"m\uD800","timestamp":1,"value":1}]`)
	badSurrogateQuery := []byte(`{"op":"query","name":"m\uD800","start":0,"end":10}`)

	build := func(badLine []byte) []byte {
		var b bytes.Buffer
		b.WriteString(good)
		b.WriteByte('\n')
		b.Write(badLine)
		b.WriteByte('\n')
		b.WriteString(later)
		b.WriteByte('\n')
		b.WriteString(query)
		b.WriteByte('\n')
		return b.Bytes()
	}

	cases := []struct {
		name    string
		badLine []byte
		substr  string
	}{
		{"invalid-utf8-0xff", badUTF8, "UTF-8"},
		{"invalid-utf8-truncated-multibyte", badUTF8Trunc, "UTF-8"},
		{"lone-surrogate-write", badSurrogate, "surrogate"},
		{"lone-surrogate-query", badSurrogateQuery, "surrogate"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data := build(tc.badLine)
			baseCode, baseRes := baselineResults(t, data)

			// 在坏行的每个字节偏移处都放一条边界（包括坏行行首与转义各处）。
			prefix := len(good) + 1
			var cuts []int
			for off := 0; off <= len(tc.badLine); off++ {
				if d := prefix + off; d > 0 && d < len(data) {
					cuts = append(cuts, d)
				}
			}
			readers := []struct {
				label string
				in    io.Reader
			}{
				{"whole", bytes.NewReader(data)},
				{"cut-at-every-bad-line-byte", newSegmentedReader(data, cuts...)},
				{"1-byte", segments1Byte(data)},
			}
			// 对含 \u 转义的损坏行，再精确覆盖反斜杠之后到代理项结束的每个切开点。
			for i := 0; i < len(data); i++ {
				if data[i] == '\\' {
					for k := 1; k <= 6 && i+k < len(data); k++ {
						readers = append(readers, struct {
							label string
							in    io.Reader
						}{
							"split-in-escape@" + strconv.Itoa(i+k),
							newSegmentedReader(data, i+k),
						})
					}
				}
			}

			for _, rd := range readers {
				rd := rd
				t.Run(rd.label, func(t *testing.T) {
					assertMatchesBaseline(t, rd.label, rd.in, baseCode, baseRes)
				})
			}

			// 基准语义本身的断言（这些不随分段改变）。
			if baseCode == 0 {
				t.Fatalf("baseline exit code = 0, want non-zero")
			}
			if len(baseRes) != 4 {
				t.Fatalf("baseline %d results, want 4 (good ok, bad error, later ok, query): %s",
					len(baseRes), mustJSON(baseRes))
			}
			bad := baseRes[1]
			if bad["status"] != "error" || int(bad["line"].(float64)) != 2 {
				t.Fatalf("corrupt line = %s, want whole-line error on input line 2", mustJSON(bad))
			}
			if _, has := bad["index"]; has {
				t.Fatalf("corrupt error must not carry index: %s", mustJSON(bad))
			}
			if _, has := bad["conflict"]; has {
				t.Fatalf("corrupt error must not carry conflict: %s", mustJSON(bad))
			}
			if !strings.Contains(bad["error"].(string), tc.substr) {
				t.Fatalf("error = %q, want substring %q", bad["error"], tc.substr)
			}
			// 坏行之后的合法写入继续处理；查询只见此前保留下来的 good 一个点。
			if baseRes[2]["status"] != "ok" {
				t.Fatalf("later legal line must still be processed: %s", mustJSON(baseRes[2]))
			}
			q := baseRes[3]["series"].([]interface{})
			if len(q) != 1 {
				t.Fatalf("query = %s, must retain only the earlier good point", mustJSON(baseRes[3]))
			}
		})
	}
}
