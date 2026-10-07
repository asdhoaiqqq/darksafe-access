package darksafe

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 本文件回归保障 value 在 float64 有限范围上溢分界两侧的处理结果。
// 分界两侧的十进制输入只有最后一位之差，必须被清楚地区分为两类：
//
//   - 1.7976931348623157e308 与 1.7976931348623158e308 转换后都是最大有限
//     float64（math.MaxFloat64）：后者十进制原文更大，但按最近舍入落回最大
//     有限值，必须成功保存。同一序列同一时间戳先后提交这两种写法时，后一次
//     是按存储值比较的重复（added 为 0、duplicates 为 1，快照仍是原来的点），
//     放在同一批次也只新增一个点；不同时间戳则保留两个点，查询数量为 2、
//     平均值仍是最大有限值，输出既不出现无穷大，也不把数值改成 null。
//   - 1.7976931348623159e308（及其负数）转换后溢出为无穷大：必须作为 value
//     的字段校验失败拒绝，而不是截成最大有限值；即使该采样位置已存有有限值，
//     也不能报成数值冲突。错误指出 value 必须能转换为有限 float64、给出从 1
//     开始的批内位置，不携带 conflict。整批原子：失败点之前的合法新增点也不
//     提交，此前成功保存的边界值保持原样，后续合法写入继续处理。
//
// 负数在对应边界（-math.MaxFloat64）遵循完全相同的规则并保留负号。

// TestFloat64OverflowBoundarySpellingsDedupPositive 覆盖正数最大有限值两侧
// 仍可表示的两种十进制写法：跨批次与同批次提交都按同一个存储值去重，
// 保存值必须是 math.MaxFloat64，不能因原文更大而拒绝第二种写法。
func TestFloat64OverflowBoundarySpellingsDedupPositive(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("initial write counts = %d/%d, want 1/0", res.Added, res.Duplicates)
	}
	if got := res.Series[0].Points[0].Value; got != math.MaxFloat64 {
		t.Fatalf("stored value = %g, want MaxFloat64", got)
	}

	// 1.7976931348623158e308 的十进制数值更大，但转换后舍入为同一个最大有限值：
	// 同一位置重复提交必须成功忽略，而不是拒绝或报冲突。
	res = mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623158e308}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("larger-spelling resubmit counts = %d/%d, want 0/1", res.Added, res.Duplicates)
	}
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
		t.Fatalf("snapshot after duplicate = %+v, want still one point", res.Series)
	}
	if p := res.Series[0].Points[0]; p.Timestamp != 1000 || p.Value != math.MaxFloat64 {
		t.Fatalf("stored point = %+v, want ts=1000 value=MaxFloat64", p)
	}

	// 两种写法放在同一批次、同一位置：只新增一个点。
	store2 := NewMetricStore()
	res = mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":1.7976931348623157e308},
		{"name":"m","timestamp":1,"value":1.7976931348623158e308}
	]`)
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("two spellings within batch counts = %d/%d, want 1/1", res.Added, res.Duplicates)
	}
	if len(res.Series) != 1 || len(res.Series[0].Points) != 1 ||
		res.Series[0].Points[0].Value != math.MaxFloat64 {
		t.Fatalf("snapshot = %+v, want one point at MaxFloat64", res.Series)
	}
}

// TestFloat64OverflowBoundarySpellingsDedupNegative 覆盖负数边界：
// 两种写法都转换为 -math.MaxFloat64 并按同一存储值去重，负号必须保留。
func TestFloat64OverflowBoundarySpellingsDedupNegative(t *testing.T) {
	store := NewMetricStore()
	res := mustOK(t, store, `[{"name":"n","timestamp":1000,"value":-1.7976931348623157e308}]`)
	if res.Added != 1 {
		t.Fatalf("initial write counts = %d/%d, want 1/0", res.Added, res.Duplicates)
	}
	if got := res.Series[0].Points[0].Value; got != -math.MaxFloat64 {
		t.Fatalf("stored value = %g, want -MaxFloat64", got)
	}

	res = mustOK(t, store, `[{"name":"n","timestamp":1000,"value":-1.7976931348623158e308}]`)
	if res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("negative larger-spelling resubmit counts = %d/%d, want 0/1", res.Added, res.Duplicates)
	}
	if len(res.Series[0].Points) != 1 || res.Series[0].Points[0].Value != -math.MaxFloat64 {
		t.Fatalf("snapshot = %+v, want one point at -MaxFloat64", res.Series)
	}

	store2 := NewMetricStore()
	res = mustOK(t, store2, `[
		{"name":"n","timestamp":1,"value":-1.7976931348623157e308},
		{"name":"n","timestamp":1,"value":-1.7976931348623158e308}
	]`)
	if res.Added != 1 || res.Duplicates != 1 ||
		len(res.Series[0].Points) != 1 || res.Series[0].Points[0].Value != -math.MaxFloat64 {
		t.Fatalf("negative within-batch = %+v, want added=1 duplicates=1 one point at -MaxFloat64", res)
	}
}

// TestFloat64OverflowBoundaryDistinctTimestampsAverageFinite 覆盖两种写法落在
// 不同时间戳：保留两个点，query 数量为 2、平均值仍是最大有限值，query_points
// 原样给出两个最大有限值；结果序列化为 JSON 时既不能出现无穷大，也不能把值
// 输出成 null。负数侧同样验证。
func TestFloat64OverflowBoundaryDistinctTimestampsAverageFinite(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`)
	res := mustOK(t, store, `[{"name":"m","timestamp":2000,"value":1.7976931348623158e308}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("distinct-timestamp write counts = %d/%d, want 1/0", res.Added, res.Duplicates)
	}

	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":3000}`)
	if len(q.Series) != 1 {
		t.Fatalf("query series = %+v, want one series", q.Series)
	}
	if c := q.Series[0].Count; c != 2 {
		t.Fatalf("query count = %d, want 2", c)
	}
	if avg := q.Series[0].Average; avg != math.MaxFloat64 || math.IsInf(avg, 0) {
		t.Fatalf("query average = %g, want finite MaxFloat64", avg)
	}

	qp, lerr := store.QueryPointsLine(`{"op":"query_points","name":"m","start":0,"end":3000}`)
	if lerr != nil {
		t.Fatalf("query_points failed: %+v", lerr)
	}
	pts := qp.Series[0].Points
	if len(pts) != 2 || pts[0].Timestamp != 1000 || pts[1].Timestamp != 2000 {
		t.Fatalf("query_points points = %+v, want two timestamps 1000/2000", pts)
	}
	for _, p := range pts {
		if p.Value != math.MaxFloat64 || math.IsInf(p.Value, 0) {
			t.Fatalf("point = %+v, want finite MaxFloat64", p)
		}
	}
	// 结果必须能正常序列化为 JSON：无穷大会令编码失败或出现 "Inf" 字样，
	// 值被改成 null 也会在输出中直接可见。
	raw, err := json.Marshal(qp)
	if err != nil {
		t.Fatalf("query_points result must marshal as JSON, got error: %v", err)
	}
	if strings.Contains(string(raw), "Inf") || strings.Contains(string(raw), "null") {
		t.Fatalf("query_points JSON = %s, want finite numbers, no Inf or null", raw)
	}

	// 同批次、不同时间戳的两种写法同样保留两个点。
	store2 := NewMetricStore()
	res = mustOK(t, store2, `[
		{"name":"m","timestamp":1000,"value":1.7976931348623157e308},
		{"name":"m","timestamp":2000,"value":1.7976931348623158e308}
	]`)
	if res.Added != 2 || res.Duplicates != 0 || len(res.Series[0].Points) != 2 {
		t.Fatalf("within-batch distinct timestamps = %+v, want added=2 two points", res)
	}

	// 负数侧：两个点，平均值为 -MaxFloat64 且有限。
	neg := NewMetricStore()
	mustOK(t, neg, `[{"name":"n","timestamp":1000,"value":-1.7976931348623157e308}]`)
	mustOK(t, neg, `[{"name":"n","timestamp":2000,"value":-1.7976931348623158e308}]`)
	nq := mustQuery(t, neg, `{"op":"query","name":"n","start":0,"end":3000}`)
	if nq.Series[0].Count != 2 {
		t.Fatalf("negative query count = %d, want 2", nq.Series[0].Count)
	}
	if avg := nq.Series[0].Average; avg != -math.MaxFloat64 || math.IsInf(avg, 0) {
		t.Fatalf("negative average = %g, want finite -MaxFloat64", avg)
	}
}

// TestFloat64OverflowBoundaryOverflowIsFieldError 覆盖真正越过分界的输入：
// 1.7976931348623159e308 与其负数转换后都是无穷大，必须作为 value 字段校验
// 失败拒绝（批内位置从 1 开始、原因指出必须是可转换为有限 float64 的数字、
// 不带 conflict），而不是截成最大有限值。
func TestFloat64OverflowBoundaryOverflowIsFieldError(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"positive overflow", `[{"name":"m","timestamp":1,"value":1.7976931348623159e308}]`},
		{"negative overflow", `[{"name":"m","timestamp":1,"value":-1.7976931348623159e308}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			lerr := mustFail(t, store, tc.line)
			assertSampleError(t, lerr, 1, "must be a finite number representable as float64")
			if !strings.Contains(lerr.Error, `field "value"`) {
				t.Fatalf("error = %q, must name field %q", lerr.Error, "value")
			}
			// 拒绝后存储为空，绝不能以截断后的最大有限值落盘。
			snap := mustOK(t, store, `[]`)
			if len(snap.Series) != 0 {
				t.Fatalf("store after rejected overflow = %+v, want empty", snap.Series)
			}
		})
	}

	// 越界点排在批内第二位时，位置指向第二个采样点；第一位的合法新增点同样
	// 不提交（整批原子性在另一个用例中展开验证）。
	store := NewMetricStore()
	lerr := mustFail(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":1.7976931348623159e308}
	]`)
	assertSampleError(t, lerr, 2, "must be a finite number representable as float64")
	if snap := mustOK(t, store, `[]`); len(snap.Series) != 0 {
		t.Fatalf("rejected batch must leave no points, got %+v", snap.Series)
	}
}

// TestFloat64OverflowBoundaryOverflowAtExistingPositionNotConflict 覆盖采样位置
// 已存有有限值时提交越界输入：字段校验先于重复/冲突判定，必须仍然报 value
// 字段错误而不是数值冲突——无论已存值是否就是边界值。已存点保持原样。
func TestFloat64OverflowBoundaryOverflowAtExistingPositionNotConflict(t *testing.T) {
	// 已存最大有限值，同位置提交越界写法。
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`)
	lerr := mustFail(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623159e308}]`)
	assertSampleError(t, lerr, 1, "must be a finite number representable as float64")
	assertStoreHasOnly(t, store, "m", 1000, math.MaxFloat64)

	// 负数侧同样不能报冲突，已存 -MaxFloat64 保持原样。
	neg := NewMetricStore()
	mustOK(t, neg, `[{"name":"n","timestamp":1000,"value":-1.7976931348623157e308}]`)
	lerr = mustFail(t, neg, `[{"name":"n","timestamp":1000,"value":-1.7976931348623159e308}]`)
	assertSampleError(t, lerr, 1, "must be a finite number representable as float64")
	assertStoreHasOnly(t, neg, "n", 1000, -math.MaxFloat64)

	// 已存的是普通有限值时也一样：越界输入没有 submitted 值可比较，
	// 不能走冲突分支。
	plain := NewMetricStore()
	mustOK(t, plain, `[{"name":"m","timestamp":1000,"value":42}]`)
	lerr = mustFail(t, plain, `[{"name":"m","timestamp":1000,"value":1.7976931348623159e308}]`)
	assertSampleError(t, lerr, 1, "must be a finite number representable as float64")
	assertStoreHasOnly(t, plain, "m", 1000, 42)
}

// TestFloat64OverflowBoundaryRejectedBatchRollsBackAndRecovers 覆盖整批原子性
// 与失败后的继续处理：失败点之前的合法新增点随整批不提交，此前成功保存的
// 边界值与采样数量保持原样，查询仍可见；失败行之后的合法写入继续成功。
func TestFloat64OverflowBoundaryRejectedBatchRollsBackAndRecovers(t *testing.T) {
	store := NewMetricStore()
	mustOK(t, store, `[{"name":"m","timestamp":1000,"value":1.7976931348623157e308}]`)

	// ts=2000 是合法新增，ts=3000 越界：整批拒绝，ts=2000 不提交，
	// 错误位置指向批内第二个采样点。
	lerr := mustFail(t, store, `[
		{"name":"m","timestamp":2000,"value":1},
		{"name":"m","timestamp":3000,"value":1.7976931348623159e308}
	]`)
	assertSampleError(t, lerr, 2, "must be a finite number representable as float64")

	// 此前成功保存的边界值与采样数量保持原样：仍只有 ts=1000 一个最大有限值点。
	q := mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":4000}`)
	if len(q.Series) != 1 || q.Series[0].Count != 1 || q.Series[0].Average != math.MaxFloat64 {
		t.Fatalf("query after rejected batch = %+v, want only the original MaxFloat64 point", q.Series)
	}

	// 失败行之后的合法写入继续处理：ts=2000 现在可以正常新增。
	res := mustOK(t, store, `[{"name":"m","timestamp":2000,"value":1}]`)
	if res.Added != 1 || res.Duplicates != 0 {
		t.Fatalf("write after recovery counts = %d/%d, want 1/0", res.Added, res.Duplicates)
	}
	q = mustQuery(t, store, `{"op":"query","name":"m","start":0,"end":4000}`)
	if q.Series[0].Count != 2 || math.IsInf(q.Series[0].Average, 0) {
		t.Fatalf("query after recovery = %+v, want count=2 finite average", q.Series)
	}
}
