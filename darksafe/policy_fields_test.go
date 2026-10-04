package darksafe

import (
	"fmt"
	"reflect"
	"testing"
)

// TestPolicyFieldsCoverStruct guards the single policy field table against
// the exact drift this table exists to prevent: a Policy struct field that no
// table entry reaches (it would silently miss the fingerprint, the UTF-8
// decision and the archive), or a table entry that reads the same member as
// another. Each struct field is stamped with a unique sentinel, after which
// every table accessor must resolve to exactly one distinct field, and its
// kind must match the field's type.
func TestPolicyFieldsCoverStruct(t *testing.T) {
	tp := reflect.TypeOf(Policy{})
	var p Policy
	pv := reflect.ValueOf(&p).Elem()
	sentinels := make(map[string]string, tp.NumField())
	boolFields := make(map[string]bool)
	for i := 0; i < tp.NumField(); i++ {
		sf := tp.Field(i)
		switch sf.Type.Kind() {
		case reflect.String:
			v := fmt.Sprintf("\x00policy-field-%d\x00", i)
			pv.Field(i).SetString(v)
			sentinels[sf.Name] = v
		case reflect.Bool:
			pv.Field(i).SetBool(true)
			boolFields[sf.Name] = true
		default:
			t.Fatalf("Policy.%s has unhandled kind %v; extend policyFields", sf.Name, sf.Type.Kind())
		}
	}

	seenString := make(map[string]bool)
	seenBool := make(map[string]bool)
	for i := range policyFields {
		f := &policyFields[i]
		switch f.kind {
		case policyFieldString, policyFieldOptionalString:
			got := f.getString(p)
			name, ok := lookupSentinel(got, sentinels)
			if !ok {
				t.Fatalf("field table entry %d reads %q, not any distinct struct field", i, got)
			}
			if seenString[name] {
				t.Fatalf("Policy.%s is covered by more than one table entry", name)
			}
			seenString[name] = true
		case policyFieldBool:
			if !f.getBool(p) {
				t.Fatalf("field table entry %d bool accessor does not read a stamped bool field", i)
			}
			matched := lookupBool(f, p)
			if matched == "" {
				t.Fatalf("field table entry %d bool accessor matches no struct field", i)
			}
			if seenBool[matched] {
				t.Fatalf("Policy.%s bool is covered more than once", matched)
			}
			seenBool[matched] = true
		default:
			t.Fatalf("field table entry %d has unknown kind %d", i, f.kind)
		}
	}

	for name := range sentinels {
		if !seenString[name] {
			t.Fatalf("string Policy.%s is missing from policyFields", name)
		}
	}
	for name := range boolFields {
		if !seenBool[name] {
			t.Fatalf("bool Policy.%s is missing from policyFields", name)
		}
	}

	// Setters must each write exactly one distinct field and round-trip with
	// the matching getter, or archive decoding could populate the wrong
	// member. Every other field must remain zero.
	touchedString := make(map[string]bool)
	touchedBool := make(map[string]bool)
	for i := range policyFields {
		f := &policyFields[i]
		var q Policy
		switch f.kind {
		case policyFieldString, policyFieldOptionalString:
			marker := fmt.Sprintf("\x01setter-%d\x01", i)
			f.setString(&q, marker)
			if f.getString(q) != marker {
				t.Fatalf("entry %d setter/getter do not round-trip", i)
			}
			var hit string
			qv := reflect.ValueOf(&q).Elem()
			for j := 0; j < tp.NumField(); j++ {
				if tp.Field(j).Type.Kind() != reflect.String {
					continue
				}
				switch v := qv.Field(j).String(); v {
				case marker:
					hit = tp.Field(j).Name
				case "":
				default:
					t.Fatalf("entry %d setter also wrote Policy.%s=%q", i, tp.Field(j).Name, v)
				}
			}
			if hit == "" {
				t.Fatalf("entry %d setter wrote no struct field", i)
			}
			if touchedString[hit] {
				t.Fatalf("setter for Policy.%s used by two entries", hit)
			}
			touchedString[hit] = true
		case policyFieldBool:
			f.setBool(&q, true)
			if !f.getBool(q) {
				t.Fatalf("entry %d bool setter/getter do not round-trip", i)
			}
			var hit string
			for j := 0; j < tp.NumField(); j++ {
				if tp.Field(j).Type.Kind() != reflect.Bool {
					continue
				}
				if reflect.ValueOf(&q).Elem().Field(j).Bool() {
					hit = tp.Field(j).Name
				}
			}
			if hit == "" {
				t.Fatalf("entry %d bool setter wrote no struct field", i)
			}
			if touchedBool[hit] {
				t.Fatalf("bool setter for Policy.%s used twice", hit)
			}
			touchedBool[hit] = true
		}
	}
	for name := range sentinels {
		if !touchedString[name] {
			t.Fatalf("Policy.%s has no working string setter entry", name)
		}
	}
	for name := range boolFields {
		if !touchedBool[name] {
			t.Fatalf("Policy.%s has no working bool setter entry", name)
		}
	}

	if len(policyFields) != tp.NumField() {
		t.Fatalf("policyFields has %d entries, Policy has %d fields", len(policyFields), tp.NumField())
	}
}

func lookupSentinel(got string, sentinels map[string]string) (string, bool) {
	for name, v := range sentinels {
		if got == v {
			return name, true
		}
	}
	return "", false
}

// lookupBool identifies the unique bool struct field whose value the spec
// reads; Policy has a single bool today, and the stamped value is true.
func lookupBool(f *policyFieldSpec, p Policy) string {
	if !f.getBool(p) {
		return ""
	}
	tp := reflect.TypeOf(Policy{})
	for i := 0; i < tp.NumField(); i++ {
		if tp.Field(i).Type.Kind() != reflect.Bool {
			continue
		}
		probe := p // all bool fields stamped true
		reflect.ValueOf(&probe).Elem().Field(i).SetBool(false)
		if !f.getBool(probe) {
			return tp.Field(i).Name
		}
	}
	return ""
}
