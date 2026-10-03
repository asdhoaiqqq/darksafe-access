package darksafe

import "testing"

// 修改返回的冲突结果不得改变已存序列的标签身份。
func TestConflictResultIsIndependent(t *testing.T) {
	s := NewMetricStore()
	if _, lerr := s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":"a"}}]`); lerr != nil {
		t.Fatal(lerr)
	}
	_, lerr := s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]`)
	if lerr == nil || lerr.Conflict == nil {
		t.Fatalf("expected conflict, got %+v", lerr)
	}
	// 调用方整理错误信息：改值、加键、删键。
	lerr.Conflict.Series.Labels["host"] = "b"
	lerr.Conflict.Series.Labels["extra"] = "x"
	delete(lerr.Conflict.Series.Labels, "host")
	lerr.Conflict.Series.Labels["host"] = "b"

	qr, lerr2 := s.QueryLine(`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`)
	if lerr2 != nil {
		t.Fatal(lerr2)
	}
	if len(qr.Series) != 1 || qr.Series[0].Count != 1 || qr.Series[0].Average != 1 {
		t.Fatalf("host=a query = %+v", qr.Series)
	}
	qr, _ = s.QueryLine(`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"b"}}`)
	if len(qr.Series) != 0 {
		t.Fatalf("host=b must not match: %+v", qr.Series)
	}
	// 后续快照与冲突仍反映真实存储。
	br, _ := s.IngestLine(`[{"name":"cpu","timestamp":1001,"value":3,"labels":{"host":"a"}}]`)
	if len(br.Series) != 1 || br.Series[0].Labels["host"] != "a" || len(br.Series[0].Points) != 2 {
		t.Fatalf("snapshot = %+v", br.Series[0])
	}
	_, lerr = s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]`)
	if lerr.Conflict.Series.Labels["host"] != "a" || len(lerr.Conflict.Series.Labels) != 1 {
		t.Fatalf("later conflict labels = %+v", lerr.Conflict.Series.Labels)
	}
}

// 无标签序列：向返回的空标签集合加键不得给已存序列补标签。
func TestConflictEmptyLabelsIndependent(t *testing.T) {
	s := NewMetricStore()
	s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1}]`)
	_, lerr := s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":2}]`)
	if lerr.Conflict.Series.Labels == nil {
		t.Fatal("conflict labels must be non-nil empty map")
	}
	lerr.Conflict.Series.Labels["host"] = "a"
	qr, _ := s.QueryLine(`{"op":"query","name":"cpu","start":0,"end":2000}`)
	if len(qr.Series) != 1 || len(qr.Series[0].Labels) != 0 {
		t.Fatalf("series labels changed: %+v", qr.Series[0])
	}
	qr, _ = s.QueryLine(`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}`)
	if len(qr.Series) != 0 {
		t.Fatalf("added label must not match: %+v", qr.Series)
	}
}

// 空字符串标签值在冲突结果与存储中都保留该键。
func TestConflictEmptyValueLabelPreserved(t *testing.T) {
	s := NewMetricStore()
	s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1,"labels":{"host":""}}]`)
	_, lerr := s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":""}}]`)
	h, ok := lerr.Conflict.Series.Labels["host"]
	if !ok || h != "" {
		t.Fatalf("empty-value label lost: %+v", lerr.Conflict.Series.Labels)
	}
	delete(lerr.Conflict.Series.Labels, "host")
	qr, _ := s.QueryLine(`{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":""}}`)
	if len(qr.Series) != 1 {
		t.Fatalf("empty-value label query = %+v", qr.Series)
	}
}

// 批内冲突：报告本批先出现的值，且本批新增点不生效。
func TestWithinBatchConflictUnaffected(t *testing.T) {
	s := NewMetricStore()
	_, lerr := s.IngestLine(`[{"name":"cpu","timestamp":1000,"value":1},{"name":"cpu","timestamp":1000,"value":2},{"name":"mem","timestamp":1,"value":5}]`)
	if lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 2 {
		t.Fatalf("conflict = %+v", lerr.Conflict)
	}
	lerr.Conflict.Series.Labels["host"] = "z"
	qr, _ := s.QueryLine(`{"op":"query","name":"cpu","start":0,"end":2000}`)
	if len(qr.Series) != 0 {
		t.Fatalf("failed batch must not commit: %+v", qr.Series)
	}
}
