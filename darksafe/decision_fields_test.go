package darksafe

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// decisionLeaf is one settable leaf field inside a DecisionRecord, found by
// walking the nested request and decision structs.
type decisionLeaf struct {
	path string       // dotted from DecisionRecord, e.g. "DecisionRecord.Request.Subject.ID"
	kind reflect.Kind // String, Bool, Int, or Slice (of string)
	str  string       // sentinel for string leaves and the element of slice leaves
	num  int          // sentinel for int leaves
}

// stampDecisionLeaves writes a unique sentinel into every leaf field of d
// and returns the leaves in walk order. Any field kind the decision field
// table cannot express fails the test, so a new DecisionRecord member cannot
// appear without a deliberate decision about its audit treatment.
func stampDecisionLeaves(t *testing.T, d *DecisionRecord) []decisionLeaf {
	t.Helper()
	var leaves []decisionLeaf
	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		vt := v.Type()
		for i := 0; i < vt.NumField(); i++ {
			sf := vt.Field(i)
			fv := v.Field(i)
			p := path + "." + sf.Name
			switch sf.Type.Kind() {
			case reflect.Struct:
				walk(fv, p)
			case reflect.String:
				s := fmt.Sprintf("\x00%s\x00", p)
				fv.SetString(s)
				leaves = append(leaves, decisionLeaf{path: p, kind: reflect.String, str: s})
			case reflect.Bool:
				fv.SetBool(true)
				leaves = append(leaves, decisionLeaf{path: p, kind: reflect.Bool})
			case reflect.Int:
				n := len(leaves) + 1
				fv.SetInt(int64(n))
				leaves = append(leaves, decisionLeaf{path: p, kind: reflect.Int, num: n})
			case reflect.Slice:
				if sf.Type.Elem().Kind() != reflect.String {
					t.Fatalf("%s has unhandled element kind %v; extend decisionRecordFields", p, sf.Type.Elem().Kind())
				}
				s := fmt.Sprintf("\x00%s\x00", p)
				fv.Set(reflect.ValueOf([]string{s}))
				leaves = append(leaves, decisionLeaf{path: p, kind: reflect.Slice, str: s})
			default:
				t.Fatalf("%s has unhandled kind %v; extend decisionRecordFields", p, sf.Type.Kind())
			}
		}
	}
	walk(reflect.ValueOf(d).Elem(), "DecisionRecord")
	return leaves
}

// decisionLeafValues reads every leaf field of d, keyed by path.
func decisionLeafValues(t *testing.T, d *DecisionRecord) map[string]interface{} {
	t.Helper()
	values := make(map[string]interface{})
	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		vt := v.Type()
		for i := 0; i < vt.NumField(); i++ {
			sf := vt.Field(i)
			fv := v.Field(i)
			p := path + "." + sf.Name
			switch sf.Type.Kind() {
			case reflect.Struct:
				walk(fv, p)
			case reflect.String:
				values[p] = fv.String()
			case reflect.Bool:
				values[p] = fv.Bool()
			case reflect.Int:
				values[p] = int(fv.Int())
			case reflect.Slice:
				if fv.IsNil() {
					values[p] = []string(nil)
				} else {
					values[p] = append([]string{}, fv.Interface().([]string)...)
				}
			default:
				t.Fatalf("%s has unhandled kind %v", p, sf.Type.Kind())
			}
		}
	}
	walk(reflect.ValueOf(d).Elem(), "DecisionRecord")
	return values
}

// lookupDecisionSentinel finds the string or string-list leaf stamped with s.
func lookupDecisionSentinel(s string, leaves []decisionLeaf) string {
	for _, l := range leaves {
		if (l.kind == reflect.String || l.kind == reflect.Slice) && l.str == s {
			return l.path
		}
	}
	return ""
}

// lookupDecisionInt finds the int leaf stamped with n.
func lookupDecisionInt(n int, leaves []decisionLeaf) string {
	for _, l := range leaves {
		if l.kind == reflect.Int && l.num == n {
			return l.path
		}
	}
	return ""
}

// lookupDecisionBool identifies the unique bool leaf a getter reads: every
// bool leaf is stamped true, and exactly one of them makes the getter flip
// to false when cleared.
func lookupDecisionBool(t *testing.T, get func(*DecisionRecord) bool, stamped *DecisionRecord, leaves []decisionLeaf) string {
	t.Helper()
	if !get(stamped) {
		return ""
	}
	for _, l := range leaves {
		if l.kind != reflect.Bool {
			continue
		}
		probe := *stamped
		fv := reflect.ValueOf(&probe).Elem()
		for _, part := range strings.Split(l.path, ".")[1:] {
			fv = fv.FieldByName(part)
		}
		fv.SetBool(false)
		if !get(&probe) {
			return l.path
		}
	}
	return ""
}

// TestDecisionRecordFieldsCoverStruct guards the single decision field table
// against the exact drift this table exists to prevent: a DecisionRecord
// leaf that no table entry reaches (it would silently miss the fingerprint,
// the UTF-8 decision and the archive), or a table entry that reads or writes
// the same member as another. Each leaf is stamped with a unique sentinel,
// after which every table accessor must resolve to exactly one distinct
// leaf, and its kind must match the leaf's type.
func TestDecisionRecordFieldsCoverStruct(t *testing.T) {
	var stamped DecisionRecord
	leaves := stampDecisionLeaves(t, &stamped)

	seen := make(map[string]bool)
	for i := range decisionRecordFields {
		f := &decisionRecordFields[i]
		var path string
		switch f.kind {
		case decisionFieldString:
			path = lookupDecisionSentinel(f.getString(&stamped), leaves)
		case decisionFieldStringList:
			got := f.getList(&stamped)
			if len(got) != 1 {
				t.Fatalf("field table entry %d reads a list of %d elements, want the 1 stamped", i, len(got))
			}
			path = lookupDecisionSentinel(got[0], leaves)
		case decisionFieldBool:
			path = lookupDecisionBool(t, f.getBool, &stamped, leaves)
		case decisionFieldInt:
			path = lookupDecisionInt(f.getInt(&stamped), leaves)
		default:
			t.Fatalf("field table entry %d has unknown kind %d", i, f.kind)
		}
		if path == "" {
			t.Fatalf("field table entry %d does not read any distinct struct field", i)
		}
		if seen[path] {
			t.Fatalf("%s is covered by more than one table entry", path)
		}
		seen[path] = true
	}
	for _, l := range leaves {
		if !seen[l.path] {
			t.Fatalf("%s is missing from decisionRecordFields", l.path)
		}
	}

	// Setters must each write exactly one distinct leaf and round-trip with
	// the matching getter, or archive decoding could populate the wrong
	// member. Every other leaf must remain zero.
	touched := make(map[string]bool)
	for i := range decisionRecordFields {
		f := &decisionRecordFields[i]
		var q DecisionRecord
		marker := fmt.Sprintf("\x01setter-%d\x01", i)
		var want interface{}
		switch f.kind {
		case decisionFieldString:
			f.setString(&q, marker)
			if f.getString(&q) != marker {
				t.Fatalf("entry %d setter/getter do not round-trip", i)
			}
			want = marker
		case decisionFieldStringList:
			f.setList(&q, []string{marker})
			if got := f.getList(&q); len(got) != 1 || got[0] != marker {
				t.Fatalf("entry %d list setter/getter do not round-trip: %q", i, got)
			}
			want = []string{marker}
		case decisionFieldBool:
			f.setBool(&q, true)
			if !f.getBool(&q) {
				t.Fatalf("entry %d bool setter/getter do not round-trip", i)
			}
			want = true
		case decisionFieldInt:
			f.setInt(&q, i+1)
			if f.getInt(&q) != i+1 {
				t.Fatalf("entry %d int setter/getter do not round-trip", i)
			}
			want = i + 1
		}
		hit := ""
		for path, v := range decisionLeafValues(t, &q) {
			zero := false
			switch tv := v.(type) {
			case string:
				zero = tv == ""
			case bool:
				zero = !tv
			case int:
				zero = tv == 0
			case []string:
				zero = tv == nil
			}
			if zero {
				continue
			}
			if hit != "" {
				t.Fatalf("entry %d setter also wrote %s", i, path)
			}
			if !reflect.DeepEqual(v, want) {
				t.Fatalf("entry %d setter wrote %v into %s, want %v", i, v, path, want)
			}
			hit = path
		}
		if hit == "" {
			t.Fatalf("entry %d setter wrote no struct field", i)
		}
		if touched[hit] {
			t.Fatalf("setter for %s used by two entries", hit)
		}
		touched[hit] = true
	}
	for _, l := range leaves {
		if !touched[l.path] {
			t.Fatalf("%s has no working setter entry", l.path)
		}
	}

	if len(decisionRecordFields) != len(leaves) {
		t.Fatalf("decisionRecordFields has %d entries, DecisionRecord has %d leaves", len(decisionRecordFields), len(leaves))
	}
}

// TestDecisionFieldGroupTagsOpenGroups pins the fingerprint grouping tags to
// the fields that historically open the subject, resource and decision
// groups: the tag rides on exactly the first field of each group, in the
// table's canonical order, so the raw fingerprint bytes cannot drift.
func TestDecisionFieldGroupTagsOpenGroups(t *testing.T) {
	want := []struct {
		field string // path suffix the tag attaches to
		tag   fingerprintTag
	}{
		{"Request.Subject.ID", tagSubject},
		{"Request.Resource.ID", tagResource},
		{"Decision.Allowed", tagDecision},
	}
	var got []struct {
		field string
		tag   fingerprintTag
	}
	for i := range decisionRecordFields {
		f := &decisionRecordFields[i]
		if f.groupTag == 0 {
			continue
		}
		// Identify the field by writing a sentinel through its setter and
		// finding which leaf moved.
		var q DecisionRecord
		switch f.kind {
		case decisionFieldString:
			f.setString(&q, "\x02tagprobe\x02")
		case decisionFieldBool:
			f.setBool(&q, true)
		}
		for path, v := range decisionLeafValues(t, &q) {
			switch tv := v.(type) {
			case string:
				if tv != "" {
					got = append(got, struct {
						field string
						tag   fingerprintTag
					}{strings.TrimPrefix(path, "DecisionRecord."), f.groupTag})
				}
			case bool:
				if tv {
					got = append(got, struct {
						field string
						tag   fingerprintTag
					}{strings.TrimPrefix(path, "DecisionRecord."), f.groupTag})
				}
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grouping tags attach to %v, want %v", got, want)
	}
}
