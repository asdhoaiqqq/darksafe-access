package darksafe

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 本文件回归保障 value 在 float64 有限范围上界两侧的十进制分界：
//
// 1.7976931348623157e308（MaxFloat64 的最短十进制写法）与
// 1.7976931348623158e308 虽然是两个不同的十进制数，但后者转换时
// 舍入到最大有限 float64——二者都必须成功保存为同一个有限存储值，
// 不能因为 ...158 的字面数值更大就按溢出拒绝；在同一序列、同一时间戳
// 先后提交时按重复采样忽略，放在同一批次也只新增一个点；换不同时间戳
// 则保留两个点，查询 count 为 2、average 仍为最大有限值。
//
// 而 1.7976931348623159e308（及其负数）转换后已经超出有限范围，
// 必须作为 value 的字段校验失败拒绝，而不是截成最大有限值；即使该
// 采样位置已存有有限边界值，也不能把越界输入报告成数值冲突。
//
// 正数、负数在对应边界遵循同一条规则，负数保留负号。

// assertFiniteJSON 序列化结果并保证输出既不出现无穷大（json 对 Inf 会
// 编码失败），也不把任何数值写成 null——边界值必须以普通 JSON 数字呈现。
func assertFiniteJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("result must marshal as finite JSON numbers, got error: %v (%+v)", err, v)
	}
	s := string(b)
	if strings.Contains(s, "Inf") {
		t.Fatalf("output must not contain infinity: %s", s)
	}
	if strings.Contains(s, "null") {
		t.Fatalf("output must not turn values into null: %s", s)
	}
	return b
}

// TestFloat64OverflowBoundaryAcceptedSpellingsDedupPositive 覆盖正数上界：
// ...157e308 与 ...158e308 都保存为 math.MaxFloat64；同位置先后提交是重复
// （跨批次与同批次一致，顺序互换也一致），不同时间戳则保留两个点。
func TestFloat64OverflowBoundaryAcceptedSpellingsDedupPositive(t *testing.T) {
	store := NewMetricStore()

	// ...157e308 直接就是最大有限 float64。
	res := mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("initial boundary write counts = %d/%d, want 1/0", res.Added, res.Duplicates)
	}
	if v := res.Series[0].Points[0].Value; v != math.MaxFloat64 {
		t.Fatalf("stored value = %v, want MaxFloat64 %v", v, math.MaxFloat64)
	}

	// ...158e308 字面上更大，但转换后舍入到同一个最大有限值：
	// 不能因为写法更大就拒绝，也不能算冲突。
	res = mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623158e308}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("larger spelling that rounds to MaxFloat64 counts = %d/%d, want 0/1",
			res.Added, res.Duplicates)
	}
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("snapshot after duplicate = %+v, want still one point", res.Series)
	}
	if v := res.Series[0].Points[0].Value; v != math.MaxFloat64 {
		t.Fatalf("stored value after duplicate = %v, want MaxFloat64", v)
	}

	// 同一批次两种写法也只新增一个点：先 ...157 后 ...158。
	store2 := NewMetricStore()
	res = mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":1.7976931348623157e308},
		{"name":"m","timestamp":1,"value":1.7976931348623158e308}
	]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("both spellings within one batch counts = %d/%d, want 1/1",
			res.Added, res.Duplicates)
	}
	if len(res.Series[0].Points) != 1 || res.Series[0].Points[0].Value != math.MaxFloat64 {
		t.Fatalf("within-batch snapshot = %+v, want one MaxFloat64 point", res.Series)
	}

	// 顺序互换：先提交舍入后的 ...158，再提交 ...157，同样只新增一个点且值为 MaxFloat64。
	store3 := NewMetricStore()
	res = mustOK(t, store3, `[
		{"name":"m","timestamp":1,"value":1.7976931348623158e308},
		{"name":"m","timestamp":1,"value":1.7976931348623157e308}
	]`)
	if res.Added != 1 || res.Duplicates != 1 || res.Series[0].Points[0].Value != math.MaxFloat64 {
		t.Fatalf("reversed within-batch spellings = %+v, want added=1 duplicates=1 MaxFloat64", res)
	}

	// 换到不同时间戳：两种写法各占一个点，查询 count 为 2、average 仍为最大有限值。
	store4 := NewMetricStore()
	mustOK(t, store4, `[
		{"name":"m","timestamp":1000,"value":1.7976931348623157e308},
		{"name":"m","timestamp":2000,"value":1.7976931348623158e308}
	]`)
	q := mustQuery(t, store4, `{"op":"query","name":"m","start":0,"end":3000}`)
	if len(q.Series) != 1 {
		t.Fatalf("query series = %+v, want one series", q.Series)
	}
	s0 := q.Series[0]
	if s0.Count != 2 || s0.Average != math.MaxFloat64 {
		t.Fatalf("query = %+v, want count=2 average=MaxFloat64", s0)
	}
	if math.IsInf(s0.Average, 0) {
		t.Fatalf("average must be finite, got %v", s0.Average)
	}

	// 明细查询同样列出两个有限的最大有限值。
	qp := mustQueryPoints(t, store4, `{"op":"query_points","name":"m","start":0,"end":3000}`)
	if len(qp.Series) != 1 || len(qp.Series[0].Points) != 2 {
		t.Fatalf("query_points = %+v, want two points", qp.Series)
	}
	for _, p := range qp.Series[0].Points {
		if p.Value != math.MaxFloat64 || math.IsInf(p.Value, 0) {
			t.Fatalf("point = %+v, want finite MaxFloat64", p)
		}
	}

	// 写入快照、区间均值、采样明细的 JSON 输出都不得出现无穷大或 null。
	assertFiniteJSON(t, res)
	assertFiniteJSON(t, q)
	assertFiniteJSON(t, qp)
}

// TestFloat64OverflowBoundaryAcceptedSpellingsDedupNegative 覆盖负数下界：
// -...157e308 与 -...158e308 都保存为 -MaxFloat64 且保留负号；同位置互为
// 重复，不同时间戳保留两个点，average 仍为负的最大有限值。
func TestFloat64OverflowBoundaryAcceptedSpellingsDedupNegative(t *testing.T) {
	store := NewMetricStore()

	res := mustOK(t, store, `[{"name":"m","timestamp":1000,"value":-1.7976931348623157e308}]`)
	if v := res.Series[0].Points[0].Value; v != -math.MaxFloat64 || !math.Signbit(v) {
		t.Fatalf("stored value = %v signbit=%v, want -MaxFloat64 with sign preserved",
			v, math.Signbit(v))
	}

	// 负方向 ...158（字面更负，即更小）同样舍入到 -MaxFloat64，算重复而不是冲突。
	res = mustOK(t, store, `[{"name":"m","timestamp":1000,"value":-1.7976931348623158e308}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("negative ...158 counts = %d/%d, want 0/1", res.Added, res.Duplicates)
	}
	if v := res.Series[0].Points[0].Value; v != -math.MaxFloat64 || !math.Signbit(v) {
		t.Fatalf("negative stored value after duplicate = %v, want -MaxFloat64", v)
	}

	// 同批次两种负数写法只新增一个点。
	store2 := NewMetricStore()
	res = mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":-1.7976931348623158e308},
		{"name":"m","timestamp":1,"value":-1.7976931348623157e308}
	]`)
	if res.Added != 1 || res.Duplicates != 1 || res.Series[0].Points[0].Value != -math.MaxFloat64 {
		t.Fatalf("negative within-batch = %+v, want one -MaxFloat64 point", res)
	}

	// 不同时间戳保留两个点；两个 -MaxFloat64 的精确平均仍是 -MaxFloat64，且为有限值。
	store3 := NewMetricStore()
	mustOK(t, store3, `[
		{"name":"m","timestamp":1000,"value":-1.7976931348623157e308},
		{"name":"m","timestamp":2000,"value":-1.7976931348623158e308}
	]`)
	q := mustQuery(t, store3, `{"op":"query","name":"m","start":0,"end":3000}`)
	s0 := q.Series[0]
	if s0.Count != 2 || s0.Average != -math.MaxFloat64 || !math.Signbit(s0.Average) {
		t.Fatalf("negative query = %+v, want count=2 average=-MaxFloat64 with sign", s0)
	}
	if math.IsInf(s0.Average, 0) {
		t.Fatalf("negative average must stay finite, got %v", s0.Average)
	}
	assertFiniteJSON(t, q)
}

// TestFloat64OverflowBoundaryBeyondRangeRejectedAsFieldError 覆盖真正越界的
// ...159e308（正、负）：必须作为 value 的字段校验失败拒绝，指出 value 必须
// 能转换为有限 float64，给批内从 1 开始的 index，不携带 conflict，也不产生
// 成功写入结果。
func TestFloat64OverflowBoundaryBeyondRangeRejectedAsFieldError(t *testing.T) {
	wantMsg := `field "value": must be a finite number representable as float64`
	for _, line := range []string{
		`[{"name":"m","timestamp":1000,"value":1.7976931348623159e308}]`,
		`[{"name":"m","timestamp":1000,"value":-1.7976931348623159e308}]`,
	} {
		store := NewMetricStore()
		lerr := mustFail(t, store, line)
		if lerr.Index != 1 {
			t.Fatalf("line %s: index = %d, want 1", line, lerr.Index)
		}
		if lerr.Error != wantMsg {
			t.Fatalf("line %s: error = %q, want %q", line, lerr.Error, wantMsg)
		}
		if lerr.Conflict != nil {
			t.Fatalf("line %s: overflow field error must not carry conflict, got %+v", line, lerr.Conflict)
		}
		// 失败不写入任何点。
		if got := len(mustOK(t, store, `[]`).Series); got != 0 {
			t.Fatalf("line %s: store mutated by rejected batch, series=%d", line, got)
		}
	}
}

// TestFloat64OverflowAtOccupiedPositionIsFieldErrorNotConflict 保护关键分界：
// 即使采样位置已经存有有限边界值，越界的 ...159e308 仍按字段校验失败处理，
// 绝不能因为“已存值与提交值不同”而被报告成数值冲突（冲突比较只发生在
// 字段校验通过、提交值已确认为有限 float64 之后）。
func TestFloat64OverflowAtOccupiedPositionIsFieldErrorNotConflict(t *testing.T) {
	wantMsg := `field "value": must be a finite number representable as float64`
	cases := []struct {
		name     string
		stored   string // 先成功写入该位置的有限值
		overflow string
	}{
		{"positive", `1.7976931348623157e308`, `1.7976931348623159e308`},
		{"negative", `-1.7976931348623157e308`, `-1.7976931348623159e308`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			mustOK(t, store, `[{"name":"m","timestamp":1000,"value":`+tc.stored+`}]`)

			lerr := mustFail(t, store, `[{"name":"m","timestamp":1000,"value":`+tc.overflow+`}]`)
			if lerr.Index != 1 {
				t.Fatalf("index = %d, want 1", lerr.Index)
			}
			if lerr.Error != wantMsg {
				t.Fatalf("error = %q, want %q", lerr.Error, wantMsg)
			}
			if lerr.Conflict != nil {
				t.Fatalf("overflow at occupied position must be a field error without conflict, got %+v",
					lerr.Conflict)
			}

			// 已存的有限边界值原样保留，数量不变。
			q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":2000}`)
			if len(q.Series) != 1 || q.Series[0].Count != 1 {
				t.Fatalf("query = %+v, want the single original point", q.Series)
			}
			want := math.MaxFloat64
			if tc.name == "negative" {
				want = -math.MaxFloat64
			}
			if got := q.Series[0].Average; got != want || math.IsInf(got, 0) {
				t.Fatalf("average = %v, want finite %v", got, want)
			}
		})
	}
}

// TestFloat64OverflowBatchAtomicityAndContinuedProcessing 保护整批原子性与
// 失败后的处理连续性：越界点之前的合法新增点随整批一起丢弃，先前成功保存的
// 边界值与采样数量保持原样，后续合法查询仍能看到；失败行之后的合法写入继续
// 处理。
func TestFloat64OverflowBatchAtomicityAndContinuedProcessing(t *testing.T) {
	store := NewMetricStore()
	// 先前成功保存的边界值。
	mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`)

	// 第 1 个点是合法新增（ts=2000），第 2 个点越界：整批不提交，index 为 2，
	// 不是冲突。
	lerr := mustFail(t, store, `[
		{"name":"m","timestamp":2000,"value":5},
		{"name":"m","timestamp":1000,"value":1.7976931348623159e308}
	]`)
	if lerr.Index != 2 {
		t.Fatalf("index = %d, want 2 (1-based position in batch)", lerr.Index)
	}
	if lerr.Conflict != nil {
		t.Fatalf("overflow rejection must not carry conflict, got %+v", lerr.Conflict)
	}
	if !strings.Contains(lerr.Error, "finite number representable as float64") {
		t.Fatalf("error must state value must convert to finite float64, got %q", lerr.Error)
	}

	// 失败点之前的合法新增点（ts=2000）没有提交；先前的边界值与数量保持原样。
	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":3000}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != math.MaxFloat64 {
		t.Fatalf("query after rolled-back batch = %+v, want only the original MaxFloat64 point", q.Series)
	}

	// 失败行之后的合法写入继续处理：ts=2000 现在可以正常写入。
	res := mustOK(t, store, `[{"name":"m","timestamp":2000,"value":5}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("write after failure counts = %d/%d, want 1/0", res.Added, res.Duplicates)
	}
	q = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":3000}`)
	if q.Series[0].Count != 2 {
		t.Fatalf("query = %+v, want 2 points after continued processing", q.Series)
	}
	assertFiniteJSON(t, q)
}
