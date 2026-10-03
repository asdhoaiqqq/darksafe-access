package darksafe

import (
	"encoding/json"
	"math"
	"testing"
)

// 本文件回归保障区间均值查询的舍入行为：平均值以成功写入后存储的 float64 为准，
// 精确算术平均舍入到最近的 float64，恰好在两个相邻可表示值正中时取二进制有效
// 数字末位为零者（最近偶数舍入）。该规则对正负数一致，不因符号改变取偶的含义；
// 极小值舍入为零时保留符号（+0 与 -0 在 JSON 中分别为 0 与 -0），而精确平均
// 本来就是零时结果为正零。每种情况下查询都必须成功并返回有限平均值。

// queryAverage 执行一次区间查询并要求恰好一条序列成功返回，返回其 count 与 average。
func queryAverage(t *testing.T, store *MetricStore, line string) (int, float64) {
	t.Helper()
	res := mustQuery(t, store, line)
	if len(res.Series) != 1 {
		t.Fatalf("query %s: series = %+v, want exactly one", line, res.Series)
	}
	avg := res.Series[0].Average
	if math.IsInf(avg, 0) || math.IsNaN(avg) {
		t.Fatalf("query %s: average must be finite, got %v", line, avg)
	}
	return res.Series[0].Count, avg
}

func TestQueryAverageMidpointRoundsToEven(t *testing.T) {
	// 1 与 1.0000000000000002 是相邻的 float64（有效数字末位分别为 0 和 1），
	// 精确平均 1+2^-53 恰在两者正中：向较小值舍入，取偶数末位的 1。
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":1.0000000000000002}
	]`)
	count, avg := queryAverage(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != 1 {
		t.Fatalf("tie between 1 and 1.0000000000000002: count=%d average=%v, want count=2 average=1", count, avg)
	}

	// 1.0000000000000002 与 1.0000000000000004 同样相邻（末位 1 和 0），
	// 精确平均 1+1.5·2^-52 恰在正中：向较大值舍入，取偶数末位的 1.0000000000000004。
	// 两种情况并存说明规则是最近偶数，而不是总向零或总向上。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":1.0000000000000002},
		{"name":"m","timestamp":2,"value":1.0000000000000004}
	]`)
	count, avg = queryAverage(t, store2, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != 1.0000000000000004 {
		t.Fatalf("tie between 1.0000000000000002 and 1.0000000000000004: count=%d average=%v, want count=2 average=1.0000000000000004", count, avg)
	}

	// 负数遵循同一规则：取偶针对二进制有效数字末位，与符号无关。
	store3 := NewMetricStore()
	mustOK(t, store3, `[
		{"name":"m","timestamp":1,"value":-1},
		{"name":"m","timestamp":2,"value":-1.0000000000000002}
	]`)
	count, avg = queryAverage(t, store3, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != -1 {
		t.Fatalf("negative tie: count=%d average=%v, want count=2 average=-1", count, avg)
	}

	store4 := NewMetricStore()
	mustOK(t, store4, `[
		{"name":"m","timestamp":1,"value":-1.0000000000000002},
		{"name":"m","timestamp":2,"value":-1.0000000000000004}
	]`)
	count, avg = queryAverage(t, store4, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != -1.0000000000000004 {
		t.Fatalf("negative tie: count=%d average=%v, want count=2 average=-1.0000000000000004", count, avg)
	}
}

func TestQueryAverageTinyValuesRoundToSignedZero(t *testing.T) {
	// 5e-324 是最小正 float64；它与 0 的精确平均 2^-1075 恰在 0 与该值正中，
	// 按最近偶数舍入为 0，且符号为正：JSON 输出 0。
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":5e-324},
		{"name":"m","timestamp":2,"value":0}
	]`)
	count, avg := queryAverage(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != 0 || math.Signbit(avg) {
		t.Fatalf("5e-324 and 0: count=%d average=%v (signbit=%v), want count=2 average=+0", count, avg, math.Signbit(avg))
	}
	if b, _ := json.Marshal(avg); string(b) != "0" {
		t.Fatalf("positive tie-to-zero JSON = %s, want 0", b)
	}

	// -5e-324 与 0 的精确平均 -2^-1075 舍入为负零：数值与 +0 相等，但符号必须保留，
	// JSON 输出 -0。
	store2 := NewMetricStore()
	mustOK(t, store2, `[
		{"name":"m","timestamp":1,"value":-5e-324},
		{"name":"m","timestamp":2,"value":0}
	]`)
	count, avg = queryAverage(t, store2, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != 0 || !math.Signbit(avg) {
		t.Fatalf("-5e-324 and 0: count=%d average=%v (signbit=%v), want count=2 average=-0", count, avg, math.Signbit(avg))
	}
	if b, _ := json.Marshal(avg); string(b) != "-0" {
		t.Fatalf("negative tie-to-zero JSON = %s, want -0", b)
	}

	// 数值完全抵消、精确平均本来就是零时，结果为正零而非负零。
	store3 := NewMetricStore()
	mustOK(t, store3, `[
		{"name":"m","timestamp":1,"value":5e-324},
		{"name":"m","timestamp":2,"value":-5e-324}
	]`)
	count, avg = queryAverage(t, store3, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != 0 || math.Signbit(avg) {
		t.Fatalf("exact cancellation: count=%d average=%v (signbit=%v), want count=2 average=+0", count, avg, math.Signbit(avg))
	}
	if b, _ := json.Marshal(avg); string(b) != "0" {
		t.Fatalf("exact zero JSON = %s, want 0", b)
	}
}

func TestQueryAverageRoundingUsesOnlyPointsInRange(t *testing.T) {
	// 区间外的点不参与平均：既不影响点数，也不改变舍入结果。
	store := NewMetricStore()
	mustOK(t, store, `[
		{"name":"m","timestamp":1,"value":1},
		{"name":"m","timestamp":2,"value":1.0000000000000002},
		{"name":"m","timestamp":3,"value":1.0000000000000004}
	]`)

	// 闭区间 [1,2] 只含前两点：中点向偶数舍入为 1，第三点不影响结果。
	count, avg := queryAverage(t, store, `{"op":"query","name":"m","start":1,"end":2}`)
	if count != 2 || avg != 1 {
		t.Fatalf("range [1,2]: count=%d average=%v, want count=2 average=1", count, avg)
	}

	// 闭区间 [2,3] 只含后两点：中点向偶数舍入为 1.0000000000000004。
	count, avg = queryAverage(t, store, `{"op":"query","name":"m","start":2,"end":3}`)
	if count != 2 || avg != 1.0000000000000004 {
		t.Fatalf("range [2,3]: count=%d average=%v, want count=2 average=1.0000000000000004", count, avg)
	}

	// 单点区间：非零有限值的平均仍是该点的值本身。
	count, avg = queryAverage(t, store, `{"op":"query","name":"m","start":2,"end":2}`)
	if count != 1 || avg != 1.0000000000000002 {
		t.Fatalf("single-point range: count=%d average=%v, want count=1 average=1.0000000000000002", count, avg)
	}
}
