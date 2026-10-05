package darksafe

import (
	"strings"
	"testing"
)

// 本文件回归保障统一入口 ProcessLine 的返回约定：
//   - 任何失败都返回真正的 nil 结果（接口值为 nil，不是装进 any 的带类型 nil 指针）
//     与一个说明原因的结构化 *LineError；调用方直接判断 result == nil 即可确认
//     没有成功结果，无须先识别结果属于写入还是查询。
//   - 空写入数组与无命中查询是成功而不是失败：结果非 nil、错误为 nil。
//   - 正常写入与有命中查询分别保持 *BatchResult 与 *QueryResult 的成功结果类型。
// 所有用例只经过 ProcessLine 这一个公开入口，在同一份内存数据上交替写入与查询。

// processFail 经过 ProcessLine 处理一行并断言失败约定：结果必须是真正的 nil
// （接口比较即可判定），同时返回结构化的 *LineError。
func processFail(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	res, lerr := store.ProcessLine(line)
	if lerr == nil {
		t.Fatalf("expected failure for %s, got result: %+v", line, res)
	}
	// 关键约定：失败时 any 结果必须是真 nil。若把带类型的 nil 指针
	// （如 (*BatchResult)(nil)）装进 any 返回，接口值非 nil，调用方
	// 用 result == nil 判定失败就会漏判，这里直接以接口比较守住。
	if res != nil {
		t.Fatalf("failed line %s must return a true nil result, got %T %+v", line, res, res)
	}
	if lerr.Status != "error" || lerr.Error == "" {
		t.Fatalf("line %s: structured error must carry status and reason, got %+v", line, lerr)
	}
	return lerr
}

// processOK 经过 ProcessLine 处理一行并断言成功约定：结果非 nil 且错误为 nil。
func processOK(t *testing.T, store *MetricStore, line string) any {
	t.Helper()
	res, lerr := store.ProcessLine(line)
	if lerr != nil {
		t.Fatalf("expected success for %s, got error: %+v", line, lerr)
	}
	if res == nil {
		t.Fatalf("successful line %s must return a non-nil result", line)
	}
	return res
}

// processOKBatch 断言一行写入数组成功并返回 *BatchResult。
func processOKBatch(t *testing.T, store *MetricStore, line string) *BatchResult {
	t.Helper()
	res := processOK(t, store, line)
	batch, ok := res.(*BatchResult)
	if !ok {
		t.Fatalf("write line %s result type = %T, want *BatchResult", line, res)
	}
	return batch
}

// processOKQuery 断言一行查询对象成功并返回 *QueryResult。
func processOKQuery(t *testing.T, store *MetricStore, line string) *QueryResult {
	t.Helper()
	res := processOK(t, store, line)
	query, ok := res.(*QueryResult)
	if !ok {
		t.Fatalf("query line %s result type = %T, want *QueryResult", line, res)
	}
	return query
}

// 批内后面的点与已存数据数值冲突时整批拒绝：错误指向批内位置并携带冲突详情，
// 批内前面的新增点不生效，已存数据保持不变。
func TestProcessLineConflictRejectsWholeBatch(t *testing.T) {
	store := NewMetricStore()
	processOKBatch(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)

	// 第一个点（2000->4）本身合法，第二个点与已存的 1000->2 冲突：整批拒绝。
	lerr := processFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}
	]`)
	if lerr.Index != 2 {
		t.Fatalf("conflict error index = %d, want 2 (error: %s)", lerr.Index, lerr.Error)
	}
	if lerr.Conflict == nil {
		t.Fatalf("conflict error must carry conflict detail, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "cpu" || len(c.Series.Labels) != 1 || c.Series.Labels["host"] != "a" {
		t.Fatalf("conflict series identity = %+v, want cpu{host=a}", c.Series)
	}
	if c.Timestamp != 1000 || c.Existing != 2 || c.Submitted != 9 {
		t.Fatalf("conflict detail = %+v, want ts=1000 existing=2 submitted=9", c)
	}

	// 查询覆盖两个时间戳的区间：批内前面的新增点 2000->4 未生效，
	// 仍只有已提交的 1000->2 一个点，均值为 2。
	qr := processOKQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 {
		t.Fatalf("query after rejected batch = %+v, want exactly one series", qr.Series)
	}
	s0 := qr.Series[0]
	if s0.Name != "cpu" || s0.Labels["host"] != "a" || s0.Count != 1 || s0.Average != 2 {
		t.Fatalf("series[0] = %+v, want cpu host=a count=1 average=2", s0)
	}
}

// 采样点字段校验失败同样整批回滚：错误携带批内位置但不带数值冲突详情，
// 批内前面的新增点不留下。
func TestProcessLineValidationFailureRollsBackBatch(t *testing.T) {
	store := NewMetricStore()
	processOKBatch(t, store, `[{"name":"old","timestamp":1,"value":9}]`)

	// 第一个点是合法新增，第二个点 timestamp 类型非法：整批拒绝。
	lerr := processFail(t, store, `[
		{"name":"new","timestamp":1,"value":1},
		{"name":"new","timestamp":"2","value":2}
	]`)
	if lerr.Index != 2 {
		t.Fatalf("validation error index = %d, want 2 (error: %s)", lerr.Index, lerr.Error)
	}
	if lerr.Conflict != nil {
		t.Fatalf("validation failure must not carry conflict detail, got %+v", lerr.Conflict)
	}

	// 批内前面的新增点未生效：空批次快照里只有此前提交的 old。
	snap := processOKBatch(t, store, `[]`)
	if len(snap.Series) != 1 || snap.Series[0].Name != "old" {
		t.Fatalf("rolled-back batch must not add series, snapshot = %+v", snap.Series)
	}
	if pts := snap.Series[0].Points; len(pts) != 1 || pts[0].Value != 9 {
		t.Fatalf("stored points = %+v, want only old value 9", pts)
	}
}

// 查询对象能解析但字段类型不合法或起点大于终点：返回对应原因，
// 不带写入批内位置，也不带冲突详情；失败查询不改变已存数据。
func TestProcessLineQueryValidationFailures(t *testing.T) {
	store := NewMetricStore()
	processOKBatch(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	bad := []string{
		`{"op":"query","name":"m","start":"0","end":1}`,           // start 类型非法
		`{"op":"query","name":"m","start":0.5,"end":1}`,           // start 非整数
		`{"op":"query","name":7,"start":0,"end":1}`,               // name 类型非法
		`{"op":"query","name":"m","start":2,"end":1}`,             // 起点大于终点
		`{"op":"query","name":"m","start":0,"end":1,"x":1}`,       // 未知字段
		`{"op":"ingest","name":"m","start":0,"end":1}`,            // 未知 op
		`{"op":"query","name":"m","start":0,"end":1,"labels":[]}`, // labels 非对象
	}
	for _, line := range bad {
		lerr := processFail(t, store, line)
		if lerr.Index != 0 {
			t.Errorf("line %s: query failure must not carry batch index, got %d", line, lerr.Index)
		}
		if lerr.Conflict != nil {
			t.Errorf("line %s: query failure must not carry conflict detail", line)
		}
	}

	// 失败查询不改变已存数据：随后的合法查询仍看到此前的采样点。
	qr := processOKQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 1 {
		t.Fatalf("failed queries must not mutate storage, got %+v", qr.Series)
	}
}

// 结构不完整的 JSON 与不属于写入数组或查询对象的顶层值都是整行失败：
// 返回真 nil 结果与整行错误（无批内位置、无冲突详情），而不是成功的空结果；
// 整行解析失败不改变已存数据。
func TestProcessLineMalformedAndNonContainerLines(t *testing.T) {
	store := NewMetricStore()
	processOKBatch(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	for _, line := range []string{
		`[`,                       // 残缺数组
		`{"op":"query"`,           // 残缺对象
		`[{"name":"m"}] trailing`, // 尾随内容
		`[] []`,                   // 两个 JSON 值
		`123`,                     // 数字
		`"hello"`,                 // 字符串
		`true`,                    // 布尔
		`null`,                    // null
	} {
		lerr := processFail(t, store, line)
		if lerr.Index != 0 {
			t.Errorf("line %q: whole-line failure must not carry index, got %d", line, lerr.Index)
		}
		if lerr.Conflict != nil {
			t.Errorf("line %q: whole-line failure must not carry conflict", line)
		}
		if !strings.Contains(lerr.Error, "invalid JSON") {
			t.Errorf("line %q: error = %q, want invalid JSON reason", line, lerr.Error)
		}
	}

	// 整行解析失败不改变已存数据：合法查询仍看到此前的采样点。
	qr := processOKQuery(t, store, `{"op":"query","name":"m","start":0,"end":100}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 1 {
		t.Fatalf("malformed lines must not mutate storage, got %+v", qr.Series)
	}
}

// 空数组是合法写入批次：返回成功的 *BatchResult，新增数与重复数都是 0，
// 并展示当前已提交的数据；结果不是 nil，错误为空。
func TestProcessLineEmptyBatchIsSuccess(t *testing.T) {
	store := NewMetricStore()

	res := processOK(t, store, `[]`)
	batch, ok := res.(*BatchResult)
	if !ok {
		t.Fatalf("empty array result type = %T, want *BatchResult", res)
	}
	if batch.Status != "ok" || batch.Added != 0 || batch.Duplicates != 0 || len(batch.Series) != 0 {
		t.Fatalf("initial empty batch = %+v, want ok 0/0 with no series", batch)
	}

	// 已有数据时，空批次仍成功并展示当前已提交的数据。
	processOKBatch(t, store, `[{"name":"m","timestamp":1,"value":1}]`)
	batch = processOKBatch(t, store, `[]`)
	if batch.Added != 0 || batch.Duplicates != 0 {
		t.Fatalf("empty batch counts = %d/%d, want 0/0", batch.Added, batch.Duplicates)
	}
	if len(batch.Series) != 1 || batch.Series[0].Name != "m" ||
		len(batch.Series[0].Points) != 1 || batch.Series[0].Points[0].Value != 1 {
		t.Fatalf("empty batch must show committed data, got %+v", batch.Series)
	}
}

// 合法查询没有命中时返回成功的 *QueryResult 与空序列列表：结果不是 nil，错误为空。
func TestProcessLineEmptyQueryResultIsSuccess(t *testing.T) {
	store := NewMetricStore()
	processOKBatch(t, store, `[{"name":"m","timestamp":1,"value":1}]`)

	res := processOK(t, store, `{"op":"query","name":"nonexistent","start":0,"end":100}`)
	qr, ok := res.(*QueryResult)
	if !ok {
		t.Fatalf("no-hit query result type = %T, want *QueryResult", res)
	}
	if qr.Status != "ok" || qr.Op != "query" {
		t.Fatalf("no-hit query envelope = %+v, want ok query", qr)
	}
	if qr.Series == nil || len(qr.Series) != 0 {
		t.Fatalf("no-hit query series = %+v, want non-nil empty list", qr.Series)
	}

	// 区间外同样是无命中成功。
	qr = processOKQuery(t, store, `{"op":"query","name":"m","start":500,"end":600}`)
	if qr.Series == nil || len(qr.Series) != 0 {
		t.Fatalf("out-of-range query series = %+v, want non-nil empty list", qr.Series)
	}
}

// 在同一份内存数据上交替写入与查询：正常写入保持 *BatchResult、
// 有命中查询保持 *QueryResult，失败行穿插其间不影响两侧的成功结果类型。
func TestProcessLineInterleavedSuccessTypes(t *testing.T) {
	store := NewMetricStore()

	batch := processOKBatch(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if batch.Status != "ok" || batch.Added != 1 || batch.Duplicates != 0 || len(batch.Series) != 1 {
		t.Fatalf("write batch = %+v, want ok added=1 with one series", batch)
	}

	qr := processOKQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":1000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("hit query = %+v, want count=1 average=2", qr.Series)
	}

	// 失败行穿插：写入冲突与查询校验失败各自返回真 nil 结果。
	processFail(t, store, `[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`)
	processFail(t, store, `{"op":"query","name":"cpu","start":5,"end":1}`)

	// 之后的写入与查询仍各自保持成功结果类型与已存数据。
	batch = processOKBatch(t, store, `[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`)
	if batch.Added != 1 || len(batch.Series) != 1 || len(batch.Series[0].Points) != 2 {
		t.Fatalf("write after failures = %+v, want added=1 with two points", batch)
	}
	qr = processOKQuery(t, store, `{"op":"query","name":"cpu","start":1000,"end":2000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("query after failures = %+v, want count=2 average=3", qr.Series)
	}
}
