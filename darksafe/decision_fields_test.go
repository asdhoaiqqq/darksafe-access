package darksafe

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestDecisionFieldsCoverStruct guards the single decision field table
// against the exact drift this table exists to prevent: a DecisionRecord
// leaf field that no table entry reaches (it would silently miss the
// fingerprint, the UTF-8 decision, the archive and the detaching copy), a
// table entry that reads the same member as another, or an entry whose kind
// does not match its field's type. Each leaf field is stamped with a unique
// sentinel, after which every table accessor must resolve to exactly one
// distinct field. The table's order must also match the canonical field
// order, which is the struct's declaration order, and the fingerprint group
// markers must sit exactly on the fields that historically opened their
// group.
func TestDecisionFieldsCoverStruct(t *testing.T) {
	paths := decisionLeafPaths(t)

	// Stamp every leaf field with a unique sentinel.
	var d DecisionRecord
	stringSentinel := make(map[string]string)
	listSentinel := make(map[string]string)
	intSentinel := make(map[string]int)
	boolPaths := make(map[string]bool)
	for i, p := range paths {
		fv := decisionFieldByPath(&d, p)
		switch fv.Kind() {
		case reflect.String:
			v := fmt.Sprintf("\x00decision-field-%d\x00", i)
			fv.SetString(v)
			stringSentinel[p] = v
		case reflect.Bool:
			fv.SetBool(true)
			boolPaths[p] = true
		case reflect.Int:
			fv.SetInt(int64(1000 + i))
			intSentinel[p] = 1000 + i
		case reflect.Slice:
			v := fmt.Sprintf("\x00decision-list-%d\x00", i)
			fv.Set(reflect.ValueOf([]string{v}))
			listSentinel[p] = v
		}
	}

	// The fingerprint's group markers belong to exactly these fields; moving
	// one would change every decision fingerprint.
	wantGroupTag := map[string]fingerprintTag{
		"Request.Subject.ID":  tagSubject,
		"Request.Resource.ID": tagResource,
		"Decision.Allowed":    tagDecision,
	}

	seen := make(map[string]bool)
	var orderedPaths []string
	for i := range decisionFields {
		f := &decisionFields[i]
		var path string
		switch f.kind {
		case decisionFieldString:
			got := f.getString(&d)
			path = matchStringSentinel(got, stringSentinel)
			if path == "" {
				t.Fatalf("field table entry %d reads %q, not any distinct string field", i, got)
			}
		case decisionFieldStringList:
			got := f.getList(&d)
			if len(got) != 1 {
				t.Fatalf("field table entry %d reads %v, not any stamped list field", i, got)
			}
			path = matchStringSentinel(got[0], listSentinel)
			if path == "" {
				t.Fatalf("field table entry %d reads %q, not any distinct list field", i, got[0])
			}
		case decisionFieldBool:
			if !f.getBool(&d) {
				t.Fatalf("field table entry %d bool accessor does not read a stamped bool field", i)
			}
			path = matchBoolSentinel(f, &d, boolPaths)
			if path == "" {
				t.Fatalf("field table entry %d bool accessor matches no struct field", i)
			}
		case decisionFieldInt:
			got := f.getInt(&d)
			for p, v := range intSentinel {
				if got == v {
					path = p
				}
			}
			if path == "" {
				t.Fatalf("field table entry %d reads %d, not any stamped int field", i, got)
			}
		default:
			t.Fatalf("field table entry %d has unknown kind %d", i, f.kind)
		}
		if seen[path] {
			t.Fatalf("DecisionRecord.%s is covered by more than one table entry", path)
		}
		seen[path] = true
		orderedPaths = append(orderedPaths, path)
		if want, ok := wantGroupTag[path]; ok {
			if f.groupTag != want {
				t.Fatalf("entry %d (%s) has group tag %d, want %d", i, path, f.groupTag, want)
			}
		} else if f.groupTag != 0 {
			t.Fatalf("entry %d (%s) carries unexpected group tag %d", i, path, f.groupTag)
		}
	}

	for _, p := range paths {
		if !seen[p] {
			t.Fatalf("DecisionRecord.%s is missing from decisionFields", p)
		}
	}
	// The table order is the wire and fingerprint order; it must follow the
	// canonical declaration order field for field.
	if strings.Join(orderedPaths, ",") != strings.Join(paths, ",") {
		t.Fatalf("decisionFields order %v does not match canonical order %v", orderedPaths, paths)
	}

	// Setters must each write exactly one distinct field and round-trip with
	// the matching getter, or archive decoding could populate the wrong
	// member. Every other field must remain zero.
	touched := make(map[string]bool)
	for i := range decisionFields {
		f := &decisionFields[i]
		var q DecisionRecord
		switch f.kind {
		case decisionFieldString:
			marker := fmt.Sprintf("\x01setter-%d\x01", i)
			f.setString(&q, marker)
			if f.getString(&q) != marker {
				t.Fatalf("entry %d setter/getter do not round-trip", i)
			}
		case decisionFieldStringList:
			f.setList(&q, nil)
			if f.getList(&q) != nil {
				t.Fatalf("entry %d list setter does not preserve nil", i)
			}
			marker := []string{fmt.Sprintf("\x01setter-%d\x01", i)}
			f.setList(&q, marker)
			if got := f.getList(&q); len(got) != 1 || got[0] != marker[0] {
				t.Fatalf("entry %d list setter/getter do not round-trip", i)
			}
		case decisionFieldBool:
			f.setBool(&q, true)
			if !f.getBool(&q) {
				t.Fatalf("entry %d bool setter/getter do not round-trip", i)
			}
		case decisionFieldInt:
			f.setInt(&q, 4242+i)
			if f.getInt(&q) != 4242+i {
				t.Fatalf("entry %d int setter/getter do not round-trip", i)
			}
		}
		var hit string
		for _, p := range paths {
			if decisionFieldByPath(&q, p).IsZero() {
				continue
			}
			if hit != "" {
				t.Fatalf("entry %d setter also wrote DecisionRecord.%s", i, p)
			}
			hit = p
		}
		if hit == "" {
			t.Fatalf("entry %d setter wrote no struct field", i)
		}
		if touched[hit] {
			t.Fatalf("setter for DecisionRecord.%s used by two entries", hit)
		}
		touched[hit] = true
	}
	for _, p := range paths {
		if !touched[p] {
			t.Fatalf("DecisionRecord.%s has no working setter entry", p)
		}
	}

	if len(decisionFields) != len(paths) {
		t.Fatalf("decisionFields has %d entries, DecisionRecord has %d leaf fields", len(decisionFields), len(paths))
	}
}

// decisionLeafPaths lists every leaf field path of DecisionRecord in
// declaration order, discovered by reflection so a new struct field fails
// the test instead of silently escaping the table.
func decisionLeafPaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	var walk func(tp reflect.Type, prefix string)
	walk = func(tp reflect.Type, prefix string) {
		for i := 0; i < tp.NumField(); i++ {
			sf := tp.Field(i)
			p := prefix + sf.Name
			switch sf.Type.Kind() {
			case reflect.Struct:
				walk(sf.Type, p+".")
			case reflect.String, reflect.Bool, reflect.Int:
				paths = append(paths, p)
			case reflect.Slice:
				if sf.Type.Elem().Kind() != reflect.String {
					t.Fatalf("DecisionRecord.%s has unhandled slice type %v; extend decisionFields", p, sf.Type)
				}
				paths = append(paths, p)
			default:
				t.Fatalf("DecisionRecord.%s has unhandled kind %v; extend decisionFields", p, sf.Type.Kind())
			}
		}
	}
	walk(reflect.TypeOf(DecisionRecord{}), "")
	return paths
}

// decisionFieldByPath resolves a dot-separated leaf path to its settable
// value inside d.
func decisionFieldByPath(d *DecisionRecord, path string) reflect.Value {
	v := reflect.ValueOf(d).Elem()
	for _, part := range strings.Split(path, ".") {
		v = v.FieldByName(part)
	}
	return v
}

// matchStringSentinel returns the field path whose sentinel equals got.
func matchStringSentinel(got string, sentinels map[string]string) string {
	for p, v := range sentinels {
		if got == v {
			return p
		}
	}
	return ""
}

// matchBoolSentinel identifies the unique bool field whose value the spec
// reads; every bool field is stamped true, so clearing the read field (and
// only it) flips the getter to false.
func matchBoolSentinel(f *decisionFieldSpec, d *DecisionRecord, boolPaths map[string]bool) string {
	for p := range boolPaths {
		probe := *d
		decisionFieldByPath(&probe, p).SetBool(false)
		if !f.getBool(&probe) {
			return p
		}
	}
	return ""
}
