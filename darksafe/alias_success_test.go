package darksafe

import (
	"reflect"
	"testing"
)

// findSeriesView 在写入快照中按指标名与标签键值对定位一条序列视图；
// 标签键必须真实存在（空字符串值与缺少该键是可区分的）。
func findSeriesView(t *testing.T, br *BatchResult, name, labelKey, labelVal string) *SeriesView {
	t.Helper()
	for i := range br.Series {
		v, ok := br.Series[i].Labels[labelKey]
		if br.Series[i].Name == name && ok && v == labelVal {
			return &br.Series[i]
		}
	}
	t.Fatalf("snapshot missing %s %s=%s: %+v", name, labelKey, labelVal, br.Series)
	return nil
}

// 成功写入快照是当次操作的独立结果：调用方修改快照中的指标名、标签、
// 点的时间戳或数值，不改变存储中的序列身份与已存点的事实。
func TestBatchSnapshotMutationDoesNotAffectStore(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1000,"value":7,"labels":{"host":"b"}}
	]`)
	br := mustOK(t, s, `[]`)

	// 调用方整理展示数据：改名、改标签值、加键、删键、改点的时间戳与数值、截断点列表。
	view := findSeriesView(t, br, "cpu", "host", "a")
	view.Name = "mem"
	view.Labels["host"] = "z"
	view.Labels["extra"] = "x"
	delete(view.Labels, "host")
	view.Points[0].Timestamp = 9999
	view.Points[0].Value = 100
	view.Points = view.Points[:1]

	// 按原名称与原标签查询仍得到写入时的事实：count=2、average=3。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("original identity query = %+v, want count=2 average=3", qr.Series)
	}
	// 按修改后的身份查询不能凭空找到新序列。
	qr = mustQuery(t, s, `{"op":"query","name":"mem","start":0,"end":3000,"labels":{"host":"z"}}`)
	if len(qr.Series) != 0 {
		t.Fatalf("mutated name identity must not match: %+v", qr.Series)
	}
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"z"}}`)
	if len(qr.Series) != 0 {
		t.Fatalf("mutated label identity must not match: %+v", qr.Series)
	}
	// 同名但标签不同的兄弟序列不受修改影响。
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"b"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 7 {
		t.Fatalf("sibling series host=b polluted: %+v", qr.Series)
	}
	// 被改出的 9999/100 并未写入存储。
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":9000,"end":10000}`)
	if len(qr.Series) != 0 {
		t.Fatalf("mutated point must not exist: %+v", qr.Series)
	}

	// 再次提交原时间戳和原数值仍是重复。
	dup := mustOK(t, s, `[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("original point must be a duplicate, got added=%d duplicates=%d", dup.Added, dup.Duplicates)
	}
	// 提交不同数值仍报告真实已存值 2，而不是调用方在快照里改出的 100。
	lerr := mustFail(t, s, `[{"name":"cpu","timestamp":1000,"value":5,"labels":{"host":"a"}}]`)
	if lerr.Conflict == nil || lerr.Conflict.Existing != 2 || lerr.Conflict.Submitted != 5 {
		t.Fatalf("conflict must report stored value 2, got %+v", lerr.Conflict)
	}
}

// 查询结果同样是独立快照：修改返回条目的标签、count、average 或整理序列条目，
// 不污染另一份已取得的查询结果，也不影响之后的写入快照。
func TestQueryResultMutationDoesNotAffectOthers(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":1500,"value":7,"labels":{"host":"b"}}
	]`)
	qr1 := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000}`)
	qr2 := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000}`)
	if len(qr1.Series) != 2 {
		t.Fatalf("want 2 series, got %+v", qr1.Series)
	}

	// 修改 qr1：标签、统计值、指标名，并整理（交换、截断）序列条目。
	qr1.Series[0].Name = "mem"
	qr1.Series[0].Labels["host"] = "z"
	qr1.Series[0].Labels["extra"] = "x"
	delete(qr1.Series[0].Labels, "host")
	qr1.Series[0].Count = 99
	qr1.Series[0].Average = 99
	qr1.Series[0], qr1.Series[1] = qr1.Series[1], qr1.Series[0]
	qr1.Series = qr1.Series[:1]

	// qr2 仍表示取得时的数据：host=a 为 count=2、average=3，host=b 为 count=1、average=7。
	if len(qr2.Series) != 2 {
		t.Fatalf("qr2 polluted: %+v", qr2.Series)
	}
	a2 := qr2.Series[0]
	if a2.Name != "cpu" || len(a2.Labels) != 1 || a2.Labels["host"] != "a" || a2.Count != 2 || a2.Average != 3 {
		t.Fatalf("qr2 host=a entry polluted: %+v", a2)
	}
	b2 := qr2.Series[1]
	if b2.Labels["host"] != "b" || b2.Count != 1 || b2.Average != 7 {
		t.Fatalf("qr2 host=b entry polluted: %+v", b2)
	}

	// 之后的写入快照不受 qr1 修改的影响。
	br := mustOK(t, s, `[]`)
	view := findSeriesView(t, br, "cpu", "host", "a")
	if len(view.Labels) != 1 || len(view.Points) != 2 ||
		view.Points[0].Timestamp != 1000 || view.Points[0].Value != 2 ||
		view.Points[1].Timestamp != 2000 || view.Points[1].Value != 4 {
		t.Fatalf("snapshot after query-result mutation = %+v", view)
	}

	// 重新查询与 qr2 完全一致。
	qr3 := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000}`)
	if !reflect.DeepEqual(qr2, qr3) {
		t.Fatalf("fresh query differs from kept result: %+v vs %+v", qr3, qr2)
	}
}

// 保留的成功结果表示各自取得时的数据：之后向同一序列写入新点，
// 新查询包含新增点，先前保留的快照与查询结果不跟着新写入变化。
func TestKeptResultsFrozenAcrossLaterWrites(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},
		{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}}
	]`)
	snap := mustOK(t, s, `[]`)
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`)

	mustOK(t, s, `[{"name":"cpu","timestamp":3000,"value":9,"labels":{"host":"a"}}]`)

	// 新查询反映新点：count=3、average=5。
	now := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}`)
	if len(now.Series) != 1 || now.Series[0].Count != 3 || now.Series[0].Average != 5 {
		t.Fatalf("query after new write = %+v, want count=3 average=5", now.Series)
	}
	// 保留的查询结果仍是取得时的 count=2、average=3。
	if len(qr.Series) != 1 || qr.Series[0].Count != 2 || qr.Series[0].Average != 3 {
		t.Fatalf("kept query result changed after later write: %+v", qr.Series)
	}
	// 保留的写入快照仍只有当初的两个点。
	if len(snap.Series) != 1 || len(snap.Series[0].Points) != 2 ||
		snap.Series[0].Points[0].Timestamp != 1000 || snap.Series[0].Points[1].Timestamp != 2000 {
		t.Fatalf("kept snapshot changed after later write: %+v", snap.Series)
	}
}

// 无标签序列与空字符串标签值序列：向返回的空标签集合添加键、删除返回的空值标签，
// 均不改变存储中的身份区别，后续筛选仍按原有标签规则返回数据。
func TestSnapshotLabelIdentityEdgeCases(t *testing.T) {
	s := NewMetricStore()
	mustOK(t, s, `[
		{"name":"cpu","timestamp":1000,"value":1},
		{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":""}},
		{"name":"cpu","timestamp":1000,"value":3,"labels":{"host":"a"}}
	]`)
	br := mustOK(t, s, `[]`)
	if len(br.Series) != 3 {
		t.Fatalf("snapshot = %+v, want 3 series", br.Series)
	}
	// 调用方整理返回内容：给空标签集合加键，删掉空值标签。
	for i := range br.Series {
		if len(br.Series[i].Labels) == 0 {
			br.Series[i].Labels["host"] = "a"
		}
		if v, ok := br.Series[i].Labels["host"]; ok && v == "" {
			delete(br.Series[i].Labels, "host")
		}
	}

	// 无标签序列没有被补上标签：host=a 仍只匹配原本的那一条。
	qr := mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 3 {
		t.Fatalf("host=a query = %+v, want only the original host=a series", qr.Series)
	}
	// 空值标签没有被删除：host="" 仍只匹配空值标签序列。
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":""}}`)
	if len(qr.Series) != 1 || qr.Series[0].Average != 2 {
		t.Fatalf(`host="" query = %+v, want only the empty-value series`, qr.Series)
	}
	// 不做标签约束时三条都在，且各自身份保持原样：
	// 一条无标签（average=1）、一条空值标签（average=2）、一条 host=a（average=3）。
	qr = mustQuery(t, s, `{"op":"query","name":"cpu","start":0,"end":2000}`)
	if len(qr.Series) != 3 {
		t.Fatalf("all series = %+v, want 3", qr.Series)
	}
	seen := make(map[float64]bool)
	for _, qs := range qr.Series {
		seen[qs.Average] = true
		switch qs.Average {
		case 1:
			if len(qs.Labels) != 0 {
				t.Fatalf("unlabeled series gained labels: %+v", qs.Labels)
			}
		case 2:
			v, ok := qs.Labels["host"]
			if !ok || v != "" || len(qs.Labels) != 1 {
				t.Fatalf("empty-value label lost: %+v", qs.Labels)
			}
		case 3:
			if qs.Labels["host"] != "a" || len(qs.Labels) != 1 {
				t.Fatalf("host=a series identity changed: %+v", qs.Labels)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("series identities changed: %+v", qr.Series)
	}

	// 后续写入快照中的身份同样保持原样。
	br = mustOK(t, s, `[]`)
	if len(br.Series) != 3 {
		t.Fatalf("later snapshot = %+v, want 3 series", br.Series)
	}
	if v := findSeriesView(t, br, "cpu", "host", "a"); len(v.Labels) != 1 {
		t.Fatalf("host=a snapshot entry changed: %+v", v.Labels)
	}
	emptyValue := findSeriesView(t, br, "cpu", "host", "")
	if len(emptyValue.Labels) != 1 {
		t.Fatalf("empty-value snapshot entry changed: %+v", emptyValue.Labels)
	}
}
