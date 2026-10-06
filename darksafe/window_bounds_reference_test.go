package darksafe

import (
	"math/big"
	"math/rand"
	"testing"
)

// 用 big.Int 参考实现核对窗口边界与窗口归属，覆盖接近 int64 上下界的区间。
func TestQueryWindowsReferenceDifferential(t *testing.T) {
	cases := []struct {
		start, end, step int64
		ts               []int64
	}{
		{tsMinInt64, tsMaxInt64, tsMaxInt64, []int64{tsMinInt64, -2, -1, 0, 1, tsMaxInt64}},
		{tsMinInt64, tsMaxInt64, 1, []int64{tsMinInt64, 0, tsMaxInt64}},
		{-5, 4, 3, []int64{-5, -4, -1, 0, 1, 4}},
		{1000, 3000, 1000, []int64{1000, 1999, 2000, 2999, 3000}},
		{0, tsMaxInt64, tsMaxInt64, []int64{0, tsMaxInt64 - 1, tsMaxInt64}},
		{tsMinInt64, tsMinInt64 + 10, 3, []int64{tsMinInt64, tsMinInt64 + 2, tsMinInt64 + 10}},
		{tsMaxInt64 - 5, tsMaxInt64, 3, []int64{tsMaxInt64 - 5, tsMaxInt64 - 3, tsMaxInt64}},
		{-10, 10, 7, []int64{-10, -4, -3, 3, 10}},
	}
	bStart := big.NewInt(0)
	bEnd := big.NewInt(0)
	bStep := big.NewInt(0)
	bOne := big.NewInt(1)
	for _, tc := range cases {
		store := NewMetricStore()
		line := "["
		for i, ts := range tc.ts {
			if i > 0 {
				line += ","
			}
			line += sample("m", ts, float64(i+1), nil)
		}
		line += "]"
		mustOK(t, store, line)
		res := mustQueryWindows(t, store, queryWindowsAll("m", tc.start, tc.end, tc.step))

		// 参考分组：idx = (ts-start)/step（大整数整除），逐组累计点。
		// idx 最大可达 2^64-1（step=1、跨整个 int64 区间），只能以 uint64 承载。
		bStart.SetInt64(tc.start)
		bEnd.SetInt64(tc.end)
		bStep.SetInt64(tc.step)
		groups := map[uint64][]int64{} // idx → timestamps
		var order []uint64
		for _, ts := range tc.ts {
			if ts < tc.start || ts > tc.end {
				continue
			}
			bTS := big.NewInt(ts)
			idx := new(big.Int).Sub(bTS, bStart)
			idx.Quo(idx, bStep)
			if !idx.IsUint64() {
				t.Fatalf("window idx overflows uint64: %v", idx)
			}
			k := idx.Uint64()
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], ts)
		}

		if len(res.Series) != 1 {
			t.Fatalf("range [%d,%d] step %d: series = %+v", tc.start, tc.end, tc.step, res.Series)
		}
		got := res.Series[0].Windows
		if len(got) != len(order) {
			t.Fatalf("range [%d,%d] step %d: got %d windows %+v, want %d",
				tc.start, tc.end, tc.step, len(got), got, len(order))
		}
		for i, k := range order {
			// wStart = start + k*step；wEnd = min(wStart+step-1, end)。
			ws := new(big.Int).Mul(new(big.Int).SetUint64(k), bStep)
			ws.Add(ws, bStart)
			we := new(big.Int).Add(ws, bStep)
			we.Sub(we, bOne)
			if we.Cmp(bEnd) > 0 {
				we.Set(bEnd)
			}
			if !ws.IsInt64() || !we.IsInt64() {
				t.Fatalf("window bounds overflow int64: [%v,%v]", ws, we)
			}
			if got[i].Start != ws.Int64() || got[i].End != we.Int64() {
				t.Fatalf("window %d bounds = [%d,%d], want [%d,%d]",
					k, got[i].Start, got[i].End, ws.Int64(), we.Int64())
			}
			if got[i].Count != len(groups[k]) {
				t.Fatalf("window %d count = %d, want %d", k, got[i].Count, len(groups[k]))
			}
			// 检查窗口不倒置、不越界。
			if got[i].Start > got[i].End || got[i].Start < tc.start || got[i].End > tc.end {
				t.Fatalf("invalid window %+v within [%d,%d]", got[i], tc.start, tc.end)
			}
			// 平均 = (i+1) 的简单平均核对。
			var sum float64
			for _, ts := range groups[k] {
				for j, x := range tc.ts {
					if x == ts {
						sum += float64(j + 1)
					}
				}
			}
			if got[i].Average != sum/float64(len(groups[k])) {
				t.Fatalf("window %d average = %v, want %v", k, got[i].Average, sum/float64(len(groups[k])))
			}
		}
	}
}

// TestQueryWindowsRandomizedTiling 随机生成区间、步长与时间戳（含负数与接近
// int64 边界的值），直接核对输出窗口的归属、边界、截断与升序，不依赖参考分组。
func TestQueryWindowsRandomizedTiling(t *testing.T) {
	rng := rand.New(rand.NewSource(20261006))
	for iter := 0; iter < 300; iter++ {
		var start, end, step int64
		if rng.Intn(5) == 0 {
			// 极端分支：起点贴近 int64 下界、小区间、步长取 int64 上界。
			start = tsMinInt64 + int64(rng.Intn(5))
			end = start + int64(rng.Intn(5))
			step = tsMaxInt64
		} else {
			// 普通分支：起点限制在 2^60 内，保证 start+宽度 不会溢出 int64。
			start = rng.Int63n(2 << 59)
			if rng.Intn(2) == 0 {
				start = -start
			}
			end = start + int64(rng.Intn(40)) // 小区间，宽度为 0..39
			step = int64(1 + rng.Intn(50))
		}
		// 每个时间戳只写一次、值统一为 1（避免同位置冲突；计数按去重后的点）。
		// ts 在 start-1..end+1 内抽取，顺带验证区间外的点既不入窗也不影响计数。
		width := int(end-start) + 3
		perm := rng.Perm(width)
		n := 1 + rng.Intn(width)
		inRange := map[int64]bool{}
		line := "["
		for i := 0; i < n; i++ {
			ts := start - 1 + int64(perm[i])
			if i > 0 {
				line += ","
			}
			line += sample("m", ts, 1, nil)
			if ts >= start && ts <= end {
				inRange[ts] = true
			}
		}
		line += "]"
		store := NewMetricStore()
		mustOK(t, store, line)
		res := mustQueryWindows(t, store, queryWindowsAll("m", start, end, step))
		if len(inRange) == 0 {
			if len(res.Series) != 0 {
				t.Fatalf("iter %d: expected empty series, got %+v", iter, res.Series)
			}
			continue
		}
		if len(res.Series) != 1 {
			t.Fatalf("iter %d: expected exactly one series, got %+v", iter, res.Series)
		}
		wins := res.Series[0].Windows
		// 期望分组：朴素 uint64 下标（区间宽度小于 2^63，下标不会接近 uint64 上界）。
		type gw struct {
			s, e int64
			c    int
		}
		var want []gw
		indexOf := func(s, e int64) int {
			for i := range want {
				if want[i].s == s && want[i].e == e {
					return i
				}
			}
			return -1
		}
		for ts := range inRange {
			off := uint64(ts) - uint64(start)
			k := off / uint64(step)
			ws := int64(uint64(start) + k*uint64(step))
			we := ws + step - 1
			if we > end || we < ws {
				we = end
			}
			if i := indexOf(ws, we); i >= 0 {
				want[i].c++
			} else {
				want = append(want, gw{ws, we, 1})
			}
		}
		// 期望按起点升序。
		for i := 1; i < len(want); i++ {
			for j := i; j > 0 && want[j].s < want[j-1].s; j-- {
				want[j], want[j-1] = want[j-1], want[j]
			}
		}
		if len(wins) != len(want) {
			t.Fatalf("iter %d [%d,%d] step %d: got %d windows, want %d: %+v vs %+v",
				iter, start, end, step, len(wins), len(want), wins, want)
		}
		for i := range want {
			if wins[i].Start != want[i].s || wins[i].End != want[i].e || wins[i].Count != want[i].c {
				t.Fatalf("iter %d window %d = [%d,%d] c=%d avg=%v, want [%d,%d] c=%d",
					iter, i, wins[i].Start, wins[i].End, wins[i].Count, wins[i].Average,
					want[i].s, want[i].e, want[i].c)
			}
			if i > 0 && wins[i].Start <= wins[i-1].End {
				t.Fatalf("iter %d: windows overlap/out of order: %+v", iter, wins)
			}
		}
	}
}
