package darksafe

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestRecordFieldsCoverStruct guards the single record field table against
// the exact drift this table exists to prevent: an outer AuditRecord field
// that no table entry reaches (it would silently miss the fingerprint, the
// UTF-8 decision and the archive), a table entry that reads the same member
// as another, or an entry whose kind does not match its field's type. Each
// struct field is stamped with a unique sentinel, after which every table
// accessor must resolve to exactly one distinct field. The table's order
// must also match the canonical field order, which is the struct's
// declaration order.
func TestRecordFieldsCoverStruct(t *testing.T) {
	tp := reflect.TypeOf(AuditRecord{})
	var r AuditRecord
	rv := reflect.ValueOf(&r).Elem()

	// Stamp every struct field with a unique sentinel.
	stringSentinel := make(map[string]string)
	intSentinel := make(map[string]int)
	changePtr := make(map[string]*PolicyChange)
	decisionPtr := make(map[string]*DecisionRecord)
	var declOrder []string
	for i := 0; i < tp.NumField(); i++ {
		sf := tp.Field(i)
		declOrder = append(declOrder, sf.Name)
		switch sf.Type.Kind() {
		case reflect.String:
			v := fmt.Sprintf("\x00record-string-%d\x00", i)
			cv := rv.Field(i)
			cv.SetString(v)
			stringSentinel[sf.Name] = v
		case reflect.Int:
			v := 1000 + i
			rv.Field(i).SetInt(int64(v))
			intSentinel[sf.Name] = v
		case reflect.Ptr:
			switch sf.Type {
			case reflect.TypeOf((*PolicyChange)(nil)):
				p := &PolicyChange{Version: 5000 + i}
				rv.Field(i).Set(reflect.ValueOf(p))
				changePtr[sf.Name] = p
			case reflect.TypeOf((*DecisionRecord)(nil)):
				p := &DecisionRecord{Decision: Decision{Version: 6000 + i}}
				rv.Field(i).Set(reflect.ValueOf(p))
				decisionPtr[sf.Name] = p
			default:
				t.Fatalf("AuditRecord.%s has unhandled pointer type %v; extend recordFields", sf.Name, sf.Type)
			}
		default:
			t.Fatalf("AuditRecord.%s has unhandled kind %v; extend recordFields", sf.Name, sf.Type.Kind())
		}
	}

	seen := make(map[string]bool)
	var orderedNames []string
	for i := range recordFields {
		f := &recordFields[i]
		var name string
		switch f.kind {
		case recordFieldString:
			got := f.getString(&r)
			for n, v := range stringSentinel {
				if got == v {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("field table entry %d reads %q, not any stamped string field", i, got)
			}
		case recordFieldInt:
			got := f.getInt(&r)
			for n, v := range intSentinel {
				if got == v {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("field table entry %d reads %d, not any stamped int field", i, got)
			}
		case recordFieldChange:
			got := f.getChange(&r)
			for n, p := range changePtr {
				if got == p {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("field table entry %d reads %v, not any stamped change field", i, got)
			}
		case recordFieldDecision:
			got := f.getDecision(&r)
			for n, p := range decisionPtr {
				if got == p {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("field table entry %d reads %v, not any stamped decision field", i, got)
			}
		default:
			t.Fatalf("field table entry %d has unknown kind %d", i, f.kind)
		}
		if seen[name] {
			t.Fatalf("AuditRecord.%s is covered by more than one table entry", name)
		}
		seen[name] = true
		orderedNames = append(orderedNames, name)
	}

	for _, n := range declOrder {
		if !seen[n] {
			t.Fatalf("AuditRecord.%s is missing from recordFields", n)
		}
	}
	// The table order is the wire and fingerprint order; it must follow the
	// canonical declaration order field for field.
	if strings.Join(orderedNames, ",") != strings.Join(declOrder, ",") {
		t.Fatalf("recordFields order %v does not match canonical order %v", orderedNames, declOrder)
	}

	// Setters must each write exactly one distinct field and round-trip with
	// the matching getter, or archive decoding could populate the wrong
	// member. Every other field must remain zero.
	touched := make(map[string]bool)
	for i := range recordFields {
		f := &recordFields[i]
		var q AuditRecord
		switch f.kind {
		case recordFieldString:
			marker := fmt.Sprintf("\x01setter-%d\x01", i)
			f.setString(&q, marker)
			if f.getString(&q) != marker {
				t.Fatalf("entry %d string setter/getter do not round-trip", i)
			}
		case recordFieldInt:
			f.setInt(&q, 4242+i)
			if f.getInt(&q) != 4242+i {
				t.Fatalf("entry %d int setter/getter do not round-trip", i)
			}
		case recordFieldChange:
			marker := &PolicyChange{Version: 7000 + i}
			f.setChange(&q, marker)
			if f.getChange(&q) != marker {
				t.Fatalf("entry %d change setter/getter do not round-trip", i)
			}
		case recordFieldDecision:
			marker := &DecisionRecord{Decision: Decision{Version: 8000 + i}}
			f.setDecision(&q, marker)
			if f.getDecision(&q) != marker {
				t.Fatalf("entry %d decision setter/getter do not round-trip", i)
			}
		}
		var hit string
		qv := reflect.ValueOf(&q).Elem()
		for j := 0; j < tp.NumField(); j++ {
			if qv.Field(j).IsZero() {
				continue
			}
			if hit != "" {
				t.Fatalf("entry %d setter also wrote AuditRecord.%s", i, tp.Field(j).Name)
			}
			hit = tp.Field(j).Name
		}
		if hit == "" {
			t.Fatalf("entry %d setter wrote no struct field", i)
		}
		if touched[hit] {
			t.Fatalf("setter for AuditRecord.%s used by two entries", hit)
		}
		touched[hit] = true
	}
	for _, n := range declOrder {
		if !touched[n] {
			t.Fatalf("AuditRecord.%s has no working setter entry", n)
		}
	}

	if len(recordFields) != tp.NumField() {
		t.Fatalf("recordFields has %d entries, AuditRecord has %d fields", len(recordFields), tp.NumField())
	}
}

// TestRecordFieldsEnvelopeMembership pins which outer fields the
// fingerprint covers: every field up to and including the previous
// fingerprint is an envelope member, and the record's own fingerprint is
// the one field that is not — a fingerprint can never be an input to
// itself. The archive writes every field regardless.
func TestRecordFieldsEnvelopeMembership(t *testing.T) {
	for i := range recordFields {
		f := &recordFields[i]
		isSelf := f.getString != nil && f.jsonKey == ""
		if i < len(recordFields)-1 && f.jsonKey == "" {
			t.Fatalf("entry %d is not the final entry yet carries no envelope key", i)
		}
		if i == len(recordFields)-1 && !isSelf {
			t.Fatalf("final entry must be the record's own fingerprint, outside the envelope")
		}
	}
	var r AuditRecord
	recordFields[len(recordFields)-1].setString(&r, "self")
	if r.Fingerprint != "self" {
		t.Fatalf("the non-envelope entry must be Fingerprint")
	}
}

// TestJSONEnvelopeMatchesLegacy proves the table-driven JSON envelope
// encoder produces byte-identical output to json.Marshal over the
// historical envelope struct, across tricky string contents and payload
// shapes: HTML-escaped characters, control characters, multi-byte runes,
// absent and present payloads, and nil versus empty lists. The record's
// own fingerprint must never appear in the output.
func TestJSONEnvelopeMatchesLegacy(t *testing.T) {
	strs := []string{
		"", "plain", "中文组织", "with space", "tab\tnl\nnul\x00",
		"<html>&amp;</html>", "quote\"back\\slash", "",
		string(rune(0xFFFD)), "①②③", "a\rb\fc\bd",
	}
	payloads := []struct {
		change   *PolicyChange
		decision *DecisionRecord
	}{
		{nil, nil},
		{&PolicyChange{}, nil},
		{nil, &DecisionRecord{}},
		{&PolicyChange{Version: 3, Policies: []Policy{}, SourceVersion: 2, RolledBack: true}, nil},
		{&PolicyChange{Version: 1, Policies: []Policy{{ID: "p<1>", Subject: "中", Action: "read", Scope: "o/*", Effect: "allow", Recursive: true, ResourceID: "r&1"}}}, nil},
		{nil, &DecisionRecord{
			Request:  OrgRequest{SubjectOrg: "中", ResourceOrg: "r", Subject: Subject{ID: "s\t", Kind: "user", Roles: []string{"a", ""}, Disabled: true}, Resource: Resource{ID: "res", Scope: "o/x"}, Action: "act"},
			Decision: Decision{Allowed: true, Reason: "ok\n", Matched: []string{}, Version: 7},
		}},
	}
	for _, s := range strs {
		for _, p := range payloads {
			rec := &AuditRecord{
				Org: s, Seq: 42, Kind: s, Change: p.change, Decision: p.decision,
				PrevFingerprint: s, Fingerprint: "must-not-appear",
			}
			want, err := json.Marshal(&hashEnvelope{
				Org: rec.Org, Seq: rec.Seq, Kind: rec.Kind,
				Change: rec.Change, Decision: rec.Decision,
				PrevFingerprint: rec.PrevFingerprint,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := encodeJSONEnvelope(rec); string(got) != string(want) {
				t.Errorf("mismatch for %q %+v:\n got %q\nwant %q", s, p, got, want)
			}
		}
	}
}
