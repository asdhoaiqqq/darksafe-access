package darksafe

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// 本文件只针对统一逐行入口 MetricStore.ProcessLine 的公开返回约定做回归保障：
// 写入数组与查询对象在同一份内存数据上交替进入同一个入口，调用方只看返回的
// result 是否为 nil 就能判定成功与否，无须先识别它是写入结果还是查询结果。
//
// 约定必须保持：
//
//   - 任何失败（数值冲突、采样点字段校验、查询字段校验、倒置区间、整行解析
//     失败、顶层值不是数组或对象）都返回真正的 nil 接口结果与非空的 *LineError；
//     不能把带类型的 nil 指针（如 (*BatchResult)(nil)）装进 any，否则调用方的
//     result == nil 会把失败误判为成功。
//   - 成功（含空写入批次与无命中查询）返回非 nil 的具体结果（*BatchResult 或
//     *QueryResult）且错误为 nil；空结果不是失败。
//   - 失败的结构化原因按类别携带位置：冲突带 index 与 conflict；采样点字段
//     校验带 index 但不带 conflict；查询失败与整行失败两者都不带。
//   - 失败不改变已存数据：冲突/字段校验拒绝整批（批内前面的合法新增点也不
//     提交），失败查询与整行解析失败后，合法查询仍能看到此前提交的采样点。
//
// 专用入口 IngestLine/QueryLine 与命令行行为由其他测试保障，这里不重复。

// callerResultFailed 模拟真实调用方对统一入口的使用方式：成功判定只依赖
// result == nil，不做任何写入/查询类型识别。若实现把带类型的 nil 指针装入
// 接口，这里会错误地报告“成功”，测试因此必须失败。
func callerResultFailed(r any) bool {
	return r == nil
}

// processOK 要求统一入口成功：结果必须是非 nil 接口（错误为 nil），返回结果
// 供调用方按具体成功类型（*BatchResult/*QueryResult）使用。
func processOK(t *testing.T, store *MetricStore, line string) any {
	t.Helper()
	r, lerr := store.ProcessLine(line)
	if callerResultFailed(r) || lerr != nil {
		t.Fatalf("ProcessLine(%s): expected non-nil success result and nil error, got r=%v lerr=%+v",
			line, r, lerr)
	}
	return r
}

// processFail 要求统一入口失败：结果必须是没有动态类型的真正 nil（result == nil
// 与反射视角同时成立，直接拦截“带类型 nil 装入接口”的回归），错误必须是非空的
// *LineError，带 status 与非空原因文本。返回错误供各测试校验类别细节。
func processFail(t *testing.T, store *MetricStore, line string) *LineError {
	t.Helper()
	r, lerr := store.ProcessLine(line)
	if !callerResultFailed(r) {
		t.Fatalf("ProcessLine(%s): failure must return a true-nil result so result == nil detects it, got %T %+v",
			line, r, r)
	}
	// 接口内不得残留任何动态类型：带类型的 nil 指针同样是回归。
	if rv := reflect.ValueOf(r); rv.IsValid() {
		t.Fatalf("ProcessLine(%s): nil result must carry no dynamic type, got %s", line, rv.Type())
	}
	if lerr == nil {
		t.Fatalf("ProcessLine(%s): failure must return a non-nil structured error, got nil", line)
	}
	if lerr.Status != "error" {
		t.Fatalf("ProcessLine(%s): error status = %q, want %q", line, lerr.Status, "error")
	}
	if strings.TrimSpace(lerr.Error) == "" {
		t.Fatalf("ProcessLine(%s): structured error must explain the reason, got empty text", line)
	}
	return lerr
}

// marshalLineError 把结构化错误序列化为 JSON 字典，便于回归各类错误携带/省略
// 哪些字段（index、conflict 必须按类别出现或缺席，而不只是 Go 结构体上为零值）。
func marshalLineError(t *testing.T, lerr *LineError) map[string]any {
	t.Helper()
	b, err := json.Marshal(lerr)
	if err != nil {
		t.Fatalf("marshal LineError: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal LineError %s: %v", b, err)
	}
	if m["status"] != "error" {
		t.Fatalf("error JSON = %s, want status error", b)
	}
	if reason, _ := m["error"].(string); reason == "" {
		t.Fatalf("error JSON = %s, want a non-empty reason", b)
	}
	return m
}

// assertNoBatchPositionOrConflict 校验查询失败与整行失败的错误形状：
// JSON 中既没有批内位置 index，也没有数值冲突 conflict；入口自身不填行号。
func assertNoBatchPositionOrConflict(t *testing.T, m map[string]any, line string) {
	t.Helper()
	if _, ok := m["index"]; ok {
		t.Errorf("ProcessLine(%s): error must not carry batch index, got %v", line, m["index"])
	}
	if _, ok := m["conflict"]; ok {
		t.Errorf("ProcessLine(%s): error must not carry conflict detail, got %v", line, m["conflict"])
	}
	if _, ok := m["line"]; ok {
		t.Errorf("ProcessLine(%s): library entry must not stamp a CLI line number, got %v", line, m["line"])
	}
}

// processSeedHostA 通过统一入口写入基线点：cpu、host=a、时间戳 1000、值 2。
func processSeedHostA(t *testing.T, store *MetricStore) {
	t.Helper()
	r := processOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	br, ok := r.(*BatchResult)
	if !ok {
		t.Fatalf("seed write result type = %T, want *BatchResult", r)
	}
	if br.Added != 1 || br.Duplicates != 0 || len(br.Series) != 1 {
		t.Fatalf("seed write = %+v, want added=1 and one series", br)
	}
}

// processHostAQuery 通过统一入口查询覆盖 [start,end] 的 cpu{host=a}，
// 返回查询结果（无命中时 Series 为空，由调用方自行判定）。
func processHostAQuery(t *testing.T, store *MetricStore, start, end int64) *QueryResult {
	t.Helper()
	line := `{"op":"query","name":"cpu","start":` + strconv.FormatInt(start, 10) +
		`,"end":` + strconv.FormatInt(end, 10) + `,"labels":{"host":"a"}}`
	r := processOK(t, store, line)
	qr, ok := r.(*QueryResult)
	if !ok {
		t.Fatalf("query result type = %T, want *QueryResult", r)
	}
	return qr
}

// 全部失败类别都必须满足统一入口的失败约定：真正 nil 结果 + 结构化原因，
// 位置与冲突细节按类别携带；每个用例都预置基线点，失败后它必须原样可见。
func TestProcessLineFailuresReturnTrueNilWithStructuredReason(t *testing.T) {
	// kind 决定该类别错误应携带的位置结构：
	//   conflict：带批内 index 与完整 conflict；
	//   sample：带批内 index，不带 conflict；
	//   query/whole：两者都不带。
	cases := []struct {
		name       string
		line       string
		kind       string
		wantIndex  int
		wantReason string // 原因子串
	}{
		// 数值冲突：批内第 2 个点与已存值冲突，错误指向它并带完整 conflict。
		{"conflict with stored value",
			`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
				`{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`,
			"conflict", 2, "conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9"},
		// 采样点字段校验失败：第 2 个点 timestamp 类型错误，带位置但不带冲突详情。
		{"sample field validation after valid new point",
			`[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},` +
				`{"name":"cpu","timestamp":"soon","value":3,"labels":{"host":"a"}}]`,
			"sample", 2, `field "timestamp" must be a JSON number`},
		// 可解析的查询对象但字段类型不合法：查询原因，不带批内位置或冲突。
		{"query field wrong type",
			`{"op":"query","name":"cpu","start":"0","end":3000,"labels":{"host":"a"}}`,
			"query", 0, `field "start" must be a JSON number`},
		// 查询起点大于终点：给出实际边界，不带批内位置或冲突。
		{"query inverted range",
			`{"op":"query","name":"cpu","start":5000,"end":1000,"labels":{"host":"a"}}`,
			"query", 0, `invalid range: "start" must not be greater than "end" (5000 > 1000)`},
		// 结构完整但缺必填字段的查询对象：仍是失败，不能当成空查询结果。
		{"query object missing required fields",
			`{}`,
			"query", 0, `missing required field "op"`},
		// 整行 JSON 结构不完整：整行失败，无位置无冲突。
		{"malformed text", `not json`, "whole", 0, "invalid JSON"},
		{"unclosed array", `[{"name":"cpu","timestamp":1,"value":2}`, "whole", 0, "invalid JSON"},
		{"unclosed object", `{"op":"query","name":"cpu"`, "whole", 0, "invalid JSON"},
		{"two top-level values", `[] []`, "whole", 0, "invalid JSON"},
		{"trailing non-json content", `[{"name":"cpu"}] oops`, "whole", 0, "invalid JSON"},
		// 顶层值是合法 JSON 但不属于写入数组或查询对象：整行失败，
		// 不能被当成成功的空结果。
		{"top-level null", `null`, "whole", 0, "invalid JSON"},
		{"top-level string", `"hello"`, "whole", 0, "invalid JSON"},
		{"top-level number", `123`, "whole", 0, "invalid JSON"},
		{"top-level boolean", `true`, "whole", 0, "invalid JSON"},
		// 损坏文本同样是整行失败。
		{"invalid UTF-8 bytes",
			string([]byte(`{"op":"query","name":"cpu","start":0,"end":` + "\xff" + `}`)),
			"whole", 0, "UTF-8"},
		{"unpaired surrogate escape",
			`[{"name":"m\uD800","timestamp":1,"value":1}]`,
			"whole", 0, "surrogate"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMetricStore()
			processSeedHostA(t, store)

			lerr := processFail(t, store, tc.line)
			if !strings.Contains(lerr.Error, tc.wantReason) {
				t.Fatalf("error reason = %q, want substring %q", lerr.Error, tc.wantReason)
			}
			m := marshalLineError(t, lerr)

			switch tc.kind {
			case "conflict":
				if lerr.Index != tc.wantIndex {
					t.Fatalf("index = %d, want %d", lerr.Index, tc.wantIndex)
				}
				if got, _ := m["index"].(float64); int(got) != tc.wantIndex {
					t.Fatalf("error JSON index = %v, want %d", m["index"], tc.wantIndex)
				}
				c := lerr.Conflict
				if c == nil {
					t.Fatalf("conflict failure must carry structured conflict detail")
				}
				// 保留序列身份、时间戳、原值与提交值。
				if c.Series.Name != "cpu" || len(c.Series.Labels) != 1 || c.Series.Labels["host"] != "a" {
					t.Fatalf("conflict series identity = %+v, want cpu{host=a}", c.Series)
				}
				if c.Timestamp != 1000 || c.Existing != 2 || c.Submitted != 9 {
					t.Fatalf("conflict detail = ts %d existing %v submitted %v, want 1000/2/9",
						c.Timestamp, c.Existing, c.Submitted)
				}
				cm, _ := m["conflict"].(map[string]any)
				if cm == nil || cm["timestamp"] != float64(1000) ||
					cm["existing"] != float64(2) || cm["submitted"] != float64(9) {
					t.Fatalf("conflict JSON = %v, want timestamp/existing/submitted 1000/2/9", m["conflict"])
				}
			case "sample":
				if lerr.Index != tc.wantIndex {
					t.Fatalf("index = %d, want %d", lerr.Index, tc.wantIndex)
				}
				if lerr.Conflict != nil {
					t.Fatalf("sample validation failure must not carry conflict, got %+v", lerr.Conflict)
				}
				if _, ok := m["conflict"]; ok {
					t.Fatalf("sample validation JSON must omit conflict, got %v", m["conflict"])
				}
			default: // query 与整行失败都不带批内位置与冲突详情。
				if lerr.Index != 0 || lerr.Conflict != nil {
					t.Fatalf("query/whole-line failure must carry neither index nor conflict, got %+v", lerr)
				}
				assertNoBatchPositionOrConflict(t, m, tc.line)
			}

			// 任何失败都不改变已存数据：覆盖两个时间戳的区间仍只有基线点，
			// 批内排在前面的合法新增点（时间戳 2000）也不得留下。
			qr := processHostAQuery(t, store, 0, 3000)
			if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
				t.Fatalf("stored data changed after failure: %+v, want one point average 2", qr.Series)
			}
			if qr = processHostAQuery(t, store, 1500, 2500); len(qr.Series) != 0 {
				t.Fatalf("rolled-back new point at timestamp 2000 must not be visible: %+v", qr.Series)
			}
		})
	}
}

// README 中的冲突场景经统一入口复现：先合法写入 1000->2，再提交 2000->4 的
// 新增点和 1000->9 的冲突点，整批拒绝；错误指向批内第 2 个点并保留完整冲突
// 细节，随后覆盖两个时间戳的查询仍只有 1 个点、均值 2。
func TestProcessLineConflictRejectsWholeBatch(t *testing.T) {
	store := NewMetricStore()
	processSeedHostA(t, store)

	lerr := processFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}
	]`)
	assertConflictDetail(t, lerr, 2, conflictHostA1000Msg,
		"cpu", map[string]string{"host": "a"}, 1000, 2, 9)

	// 查询走统一入口、与写入交替进行：仍是 1 个点、均值 2。
	qr := processHostAQuery(t, store, 0, 3000)
	if len(qr.Series) != 1 {
		t.Fatalf("query series = %+v, want exactly cpu{host=a}", qr.Series)
	}
	s0 := qr.Series[0]
	if s0.Count != 1 || s0.Average != 2 {
		t.Fatalf("query after rejected batch = count %d average %v, want count 1 average 2",
			s0.Count, s0.Average)
	}

	// 空写入批次的快照同样证明时间戳 2000 的新增点没有留下。
	r := processOK(t, store, `[]`)
	snap := r.(*BatchResult)
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 1 ||
		snap.Series[0].Points[0] != (Point{Timestamp: 1000, Value: 2}) {
		t.Fatalf("snapshot after rejected batch = %+v, want only (1000,2)", snap.Series)
	}
}

// 采样点字段校验失败时，同一批前面已经解析通过的新增点同样不提交；
// 错误继续携带批内出错位置，但不附带数值冲突详情。
func TestProcessLineFieldValidationRollsBackEarlierSamples(t *testing.T) {
	store := NewMetricStore()
	processSeedHostA(t, store)

	lerr := processFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":"soon","value":3,"labels":{"host":"a"}}
	]`)
	if lerr.Index != 2 {
		t.Fatalf("index = %d, want 2", lerr.Index)
	}
	if !strings.Contains(lerr.Error, `field "timestamp" must be a JSON number`) {
		t.Fatalf("error = %q, want timestamp type reason", lerr.Error)
	}
	if lerr.Conflict != nil {
		t.Fatalf("field validation failure must not carry conflict detail, got %+v", lerr.Conflict)
	}

	// 第 1 个点是合法新增，也必须随整批回滚。
	qr := processHostAQuery(t, store, 0, 3000)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("earlier valid sample must be rolled back too, got %+v", qr.Series)
	}
}

// 可解析但字段类型不合法或区间倒置的查询对象返回对应原因的结构化错误，
// 不带写入批内位置或冲突详情；失败查询不改变存储，随后合法查询照常命中。
func TestProcessLineQueryFailuresAreReasonOnlyAndReadOnly(t *testing.T) {
	store := NewMetricStore()
	processSeedHostA(t, store)

	baseline := func() {
		t.Helper()
		qr := processHostAQuery(t, store, 0, 3000)
		if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
			t.Fatalf("baseline data changed by failed query: %+v", qr.Series)
		}
	}
	baseline()

	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"name wrong type", `{"op":"query","name":7,"start":0,"end":3000}`, `field "name" must be a string`},
		{"start wrong type",
			`{"op":"query","name":"cpu","start":"0","end":3000,"labels":{"host":"a"}}`,
			`field "start" must be a JSON number`},
		{"labels wrong type", `{"op":"query","name":"cpu","start":0,"end":3000,"labels":[]}`,
			`field "labels" must be an object`},
		{"missing end", `{"op":"query","name":"cpu","start":0}`, `missing required field "end"`},
		{"unknown op", `{"op":"ping","name":"cpu","start":0,"end":3000}`, `unknown op "ping"`},
		{"inverted range",
			`{"op":"query","name":"cpu","start":5000,"end":1000,"labels":{"host":"a"}}`,
			"5000 > 1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lerr := processFail(t, store, tc.line)
			if !strings.Contains(lerr.Error, tc.want) {
				t.Fatalf("error = %q, want substring %q", lerr.Error, tc.want)
			}
			if lerr.Index != 0 || lerr.Conflict != nil {
				t.Fatalf("query failure must not carry index/conflict, got %+v", lerr)
			}
			m := marshalLineError(t, lerr)
			assertNoBatchPositionOrConflict(t, m, tc.line)
		})
	}

	// 所有失败查询之后，合法查询结果与失败前一致。
	baseline()
}

// 空写入批次与无命中查询是“没有新增/没有命中”，不是失败：结果必须非 nil、
// 错误必须为 nil；空数组返回 *BatchResult（added/duplicates 均为 0，并展示
// 当前已提交数据），无命中查询返回 *QueryResult 与空（非 nil）序列列表。
func TestProcessLineEmptySuccessesAreNonNullResults(t *testing.T) {
	store := NewMetricStore()

	// 全新存储上的空批次：成功，0/0，序列列表为空但结果非 nil。
	r := processOK(t, store, `[]`)
	br, ok := r.(*BatchResult)
	if !ok {
		t.Fatalf("empty array result type = %T, want *BatchResult", r)
	}
	if br.Added != 0 || br.Duplicates != 0 || len(br.Series) != 0 {
		t.Fatalf("empty batch on fresh store = %+v, want 0/0 and no series", br)
	}

	// 全新存储上的查询：成功的查询结果，空序列列表（序列化为 [] 而不是缺省）。
	r = processOK(t, store, `{"op":"query","name":"cpu","start":0,"end":3000}`)
	qr, ok := r.(*QueryResult)
	if !ok {
		t.Fatalf("no-hit query result type = %T, want *QueryResult", r)
	}
	if qr.Status != "ok" || qr.Op != "query" || qr.Series == nil || len(qr.Series) != 0 {
		t.Fatalf("no-hit query = %+v, want non-nil empty series list", qr)
	}
	if b, _ := json.Marshal(qr); string(b) != `{"status":"ok","op":"query","series":[]}` {
		t.Fatalf("no-hit query JSON = %s, want empty series array", b)
	}

	// 提交一个点后：空批次仍是 0/0 的成功结果，但展示当前已提交的 1 个点。
	processSeedHostA(t, store)
	r = processOK(t, store, `[]`)
	br = r.(*BatchResult)
	if br.Added != 0 || br.Duplicates != 0 {
		t.Fatalf("empty batch counts = %d/%d, want 0/0", br.Added, br.Duplicates)
	}
	if len(br.Series) != 1 || len(br.Series[0].Points) != 1 ||
		br.Series[0].Points[0] != (Point{Timestamp: 1000, Value: 2}) {
		t.Fatalf("empty batch snapshot = %+v, want the committed (1000,2) point", br.Series)
	}

	// 区间不命中是成功的空查询结果，区别于任何失败（失败时结果为 nil）。
	r = processOK(t, store, `{"op":"query","name":"cpu","start":1001,"end":1999,"labels":{"host":"a"}}`)
	qr = r.(*QueryResult)
	if qr.Series == nil || len(qr.Series) != 0 {
		t.Fatalf("miss query = %+v, want non-nil empty series list", qr)
	}
	r = processOK(t, store, `{"op":"query","name":"other","start":0,"end":3000}`)
	if qr = r.(*QueryResult); len(qr.Series) != 0 {
		t.Fatalf("unknown metric query = %+v, want empty series list", qr)
	}

	// 对照：同样“没有数据可写入/可统计”的失败输入（空对象、null）必须是 nil，
	// 不能被放松成成功的空结果。
	if r, lerr := store.ProcessLine(`{}`); r != nil || lerr == nil {
		t.Fatalf("empty object must fail with nil result, got r=%+v lerr=%+v", r, lerr)
	}
	if r, lerr := store.ProcessLine(`null`); r != nil || lerr == nil {
		t.Fatalf("top-level null must fail with nil result, got r=%+v lerr=%+v", r, lerr)
	}
}

// 写入与查询经统一入口在同一存储上交替进行：成功结果各自保留 *BatchResult 与
// *QueryResult 类型，失败穿插其间不改变状态，恢复后新增与统计照常推进。
func TestProcessLineInterleavedWritesAndQueries(t *testing.T) {
	store := NewMetricStore()

	// 正常写入：*BatchResult，added/duplicates 只统计本批。
	r := processOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	br := r.(*BatchResult)
	if br.Added != 1 || br.Duplicates != 0 || len(br.Series) != 1 {
		t.Fatalf("first write = %+v, want added=1 one series", br)
	}

	// 有命中查询：*QueryResult，1 个点、均值 2。
	qr := processHostAQuery(t, store, 0, 3000)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("first query = %+v, want count 1 average 2", qr.Series)
	}

	// 失败写入与失败查询穿插：结果都是 nil，存储不进不退。
	processFail(t, store, `[
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}
	]`)
	processFail(t, store, `{"op":"query","name":"cpu","start":5000,"end":1000,"labels":{"host":"a"}}`)
	processFail(t, store, `not json`)

	qr = processHostAQuery(t, store, 0, 3000)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 2 {
		t.Fatalf("state changed by interleaved failures: %+v", qr.Series)
	}

	// 恢复：时间戳 2000、值 4 正常新增，随后查询得到两个点、均值 3。
	r = processOK(t, store, `[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}]`)
	br = r.(*BatchResult)
	if br.Added != 1 || br.Duplicates != 0 || len(br.Series[0].Points) != 2 {
		t.Fatalf("recovery write = %+v, want added=1 and two points", br)
	}
	qr = processHostAQuery(t, store, 0, 3000)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("query after recovery = %+v, want count 2 average 3", qr.Series)
	}

	// 等值重提：仍是成功写入结果，新增 0、重复 1。
	r = processOK(t, store, `[{"name":"cpu","timestamp":1000,"value":2.0,"labels":{"host":"a"}}]`)
	br = r.(*BatchResult)
	if br.Added != 0 || br.Duplicates != 1 {
		t.Fatalf("equal resubmit = %+v, want added=0 duplicates=1", br)
	}

	// 空批次与无命中查询两类“空但成功”继续可用，且类型不与查询结果混淆。
	r = processOK(t, store, `[]`)
	if _, ok := r.(*BatchResult); !ok {
		t.Fatalf("empty batch result type = %T, want *BatchResult", r)
	}
	r = processOK(t, store, `{"op":"query","name":"cpu","start":2001,"end":2999,"labels":{"host":"a"}}`)
	if _, ok := r.(*QueryResult); !ok {
		t.Fatalf("miss query result type = %T, want *QueryResult", r)
	}
}
