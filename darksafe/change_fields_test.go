package darksafe

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestChangeFieldsCoverStruct guards the single policy-change field table
// against the exact drift this table exists to prevent: a PolicyChange field
// that no table entry reaches (it would silently miss the fingerprint, the
// UTF-8 decision, the archive and the detaching copy), a table entry that
// reads the same member as another, or an entry whose kind does not match
// its field's type. Each struct field is stamped with a unique sentinel,
// after which every table accessor must resolve to exactly one distinct
// field. The table's order must also match the canonical field order, which
// is the struct's declaration order.
func TestChangeFieldsCoverStruct(t *testing.T) {
	tp := reflect.TypeOf(PolicyChange{})
	var c PolicyChange
	cv := reflect.ValueOf(&c).Elem()

	// Stamp every struct field with a unique sentinel.
	intSentinel := make(map[string]int)
	boolFields := make(map[string]bool)
	listFields := make(map[string]string)
	var declOrder []string
	for i := 0; i < tp.NumField(); i++ {
		sf := tp.Field(i)
		declOrder = append(declOrder, sf.Name)
		switch sf.Type.Kind() {
		case reflect.Int:
			v := 1000 + i
			cv.Field(i).SetInt(int64(v))
			intSentinel[sf.Name] = v
		case reflect.Bool:
			cv.Field(i).SetBool(true)
			boolFields[sf.Name] = true
		case reflect.Slice:
			if sf.Type != reflect.TypeOf([]Policy(nil)) {
				t.Fatalf("PolicyChange.%s has unhandled slice type %v; extend changeFields", sf.Name, sf.Type)
			}
			v := fmt.Sprintf("\x00change-list-%d\x00", i)
			cv.Field(i).Set(reflect.ValueOf([]Policy{{ID: v}}))
			listFields[sf.Name] = v
		default:
			t.Fatalf("PolicyChange.%s has unhandled kind %v; extend changeFields", sf.Name, sf.Type.Kind())
		}
	}

	seen := make(map[string]bool)
	var orderedNames []string
	for i := range changeFields {
		f := &changeFields[i]
		var name string
		switch f.kind {
		case changeFieldInt:
			got := f.getInt(&c)
			for n, v := range intSentinel {
				if got == v {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("field table entry %d reads %d, not any stamped int field", i, got)
			}
		case changeFieldPolicyList:
			got := f.getPolicies(&c)
			if len(got) != 1 {
				t.Fatalf("field table entry %d reads %v, not any stamped policy list field", i, got)
			}
			for n, v := range listFields {
				if got[0].ID == v {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("field table entry %d reads %q, not any stamped policy list field", i, got[0].ID)
			}
		case changeFieldBool:
			if !f.getBool(&c) {
				t.Fatalf("field table entry %d bool accessor does not read a stamped bool field", i)
			}
			for n := range boolFields {
				probe := c // all bool fields stamped true
				reflect.ValueOf(&probe).Elem().FieldByName(n).SetBool(false)
				if !f.getBool(&probe) {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("field table entry %d bool accessor matches no struct field", i)
			}
		default:
			t.Fatalf("field table entry %d has unknown kind %d", i, f.kind)
		}
		if seen[name] {
			t.Fatalf("PolicyChange.%s is covered by more than one table entry", name)
		}
		seen[name] = true
		orderedNames = append(orderedNames, name)
	}

	for _, n := range declOrder {
		if !seen[n] {
			t.Fatalf("PolicyChange.%s is missing from changeFields", n)
		}
	}
	// The table order is the wire and fingerprint order; it must follow the
	// canonical declaration order field for field.
	if strings.Join(orderedNames, ",") != strings.Join(declOrder, ",") {
		t.Fatalf("changeFields order %v does not match canonical order %v", orderedNames, declOrder)
	}

	// Setters must each write exactly one distinct field and round-trip with
	// the matching getter, or archive decoding could populate the wrong
	// member. Every other field must remain zero.
	touched := make(map[string]bool)
	for i := range changeFields {
		f := &changeFields[i]
		var q PolicyChange
		switch f.kind {
		case changeFieldInt:
			f.setInt(&q, 4242+i)
			if f.getInt(&q) != 4242+i {
				t.Fatalf("entry %d int setter/getter do not round-trip", i)
			}
		case changeFieldPolicyList:
			f.setPolicies(&q, nil)
			if f.getPolicies(&q) != nil {
				t.Fatalf("entry %d list setter does not preserve nil", i)
			}
			marker := []Policy{{ID: fmt.Sprintf("\x01setter-%d\x01", i)}}
			f.setPolicies(&q, marker)
			if got := f.getPolicies(&q); len(got) != 1 || got[0].ID != marker[0].ID {
				t.Fatalf("entry %d list setter/getter do not round-trip", i)
			}
		case changeFieldBool:
			f.setBool(&q, true)
			if !f.getBool(&q) {
				t.Fatalf("entry %d bool setter/getter do not round-trip", i)
			}
		}
		var hit string
		qv := reflect.ValueOf(&q).Elem()
		for j := 0; j < tp.NumField(); j++ {
			if qv.Field(j).IsZero() {
				continue
			}
			if hit != "" {
				t.Fatalf("entry %d setter also wrote PolicyChange.%s", i, tp.Field(j).Name)
			}
			hit = tp.Field(j).Name
		}
		if hit == "" {
			t.Fatalf("entry %d setter wrote no struct field", i)
		}
		if touched[hit] {
			t.Fatalf("setter for PolicyChange.%s used by two entries", hit)
		}
		touched[hit] = true
	}
	for _, n := range declOrder {
		if !touched[n] {
			t.Fatalf("PolicyChange.%s has no working setter entry", n)
		}
	}

	if len(changeFields) != tp.NumField() {
		t.Fatalf("changeFields has %d entries, PolicyChange has %d fields", len(changeFields), tp.NumField())
	}
}
