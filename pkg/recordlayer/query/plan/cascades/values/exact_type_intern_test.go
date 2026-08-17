package values

import (
	"sync"
	"testing"
)

func internTestRecord(name string, nullable bool, fieldName string, fieldType Type) *RecordType {
	return &RecordType{
		RecordName: name,
		Nullable:   nullable,
		Fields:     []Field{{Name: fieldName, Ordinal: 0, FieldType: fieldType}},
	}
}

// TestCompositeExactTypesAreShared pins that a snapshot of an equal composite
// type returns the SAME object, for every composite kind. The lookup runs
// before the node is built, so a hit allocates nothing at all — which is the
// entire reason the interner exists: over the pure-planner sweep,
// snapshotExactType ran 243.8 million times and produced 170 distinct types.
func TestCompositeExactTypesAreShared(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		build func() Type
	}{
		{"record", func() Type {
			return internTestRecord("SHARED_R", false, "ID", NewPrimitiveType(TypeCodeLong, false))
		}},
		{"nested record", func() Type {
			return internTestRecord("SHARED_OUTER", false, "INNER",
				internTestRecord("SHARED_INNER", true, "X", NewPrimitiveType(TypeCodeString, true)))
		}},
		{"array", func() Type {
			return &ArrayType{ElementType: NewPrimitiveType(TypeCodeDouble, false)}
		}},
		{"enum", func() Type {
			return &EnumType{EnumName: "SHARED_E", Values: []EnumValue{{Name: "A", Number: 1}}}
		}},
		{"relation", func() Type {
			return &RelationType{InnerType: internTestRecord("SHARED_REL", false, "ID",
				NewPrimitiveType(TypeCodeLong, false))}
		}},
		{"any record", func() Type { return NewAnyRecordType(false) }},
	}
	for _, tc := range cases {
		first, err := SnapshotExactType(tc.build())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		second, err := SnapshotExactType(tc.build())
		if err != nil {
			t.Fatalf("%s (again): %v", tc.name, err)
		}
		if first != second {
			t.Errorf("%s: two snapshots of an equal type produced two objects; "+
				"the probe does not match, so every snapshot of this kind rebuilds "+
				"the node and its canonical encoding", tc.name)
		}
	}
}

// TestExactInterningKeepsRecordNamesApart is the other half, and the one that
// would go wrong silently. Canonical identity EXCLUDES the record name (Java's
// Type.Record equality does), so a interner keyed on canonical identity would
// collapse these two — and thaw() rebuilds RecordName from the node, so one of
// them would then report the other's name from Type().
func TestExactInterningKeepsRecordNamesApart(t *testing.T) {
	t.Parallel()

	long := NewPrimitiveType(TypeCodeLong, false)
	left, err := SnapshotExactType(internTestRecord("NAME_LEFT", false, "ID", long))
	if err != nil {
		t.Fatalf("left: %v", err)
	}
	right, err := SnapshotExactType(internTestRecord("NAME_RIGHT", false, "ID", long))
	if err != nil {
		t.Fatalf("right: %v", err)
	}
	if left == right {
		t.Fatal("two records differing only in name were interned as one object; " +
			"Type() would then report the wrong RecordName for one of them")
	}
	// They ARE canonically equal — that is why this needed its own guard.
	if !exactTypesEqual(left.(*exactType), right.(*exactType)) {
		t.Error("the two records are not canonically equal, so this test is not " +
			"exercising the name axis it was written for")
	}
	if got := left.Type().(*RecordType).RecordName; got != "NAME_LEFT" {
		t.Errorf("left thaws as %q, want NAME_LEFT", got)
	}
	if got := right.Type().(*RecordType).RecordName; got != "NAME_RIGHT" {
		t.Errorf("right thaws as %q, want NAME_RIGHT", got)
	}
}

// TestExactInterningConvergesUnderConcurrency drives the double-checked store.
// The planner snapshots from many goroutines, so the failure this guards is a
// torn table or two winners for one type — run it under -race.
func TestExactInterningConvergesUnderConcurrency(t *testing.T) {
	t.Parallel()

	const goroutines = 16
	results := make([]ExactTypeHandle, goroutines)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range results {
		done.Add(1)
		go func(slot int) {
			defer done.Done()
			start.Wait()
			handle, err := SnapshotExactType(internTestRecord(
				"CONCURRENT_R", false, "ID", NewPrimitiveType(TypeCodeLong, false)))
			if err != nil {
				t.Errorf("goroutine %d: %v", slot, err)
				return
			}
			results[slot] = handle
		}(i)
	}
	start.Done()
	done.Wait()
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Fatalf("goroutine %d got a different object than goroutine 0; "+
				"the double-checked store admitted two winners for one type", i)
		}
	}
}
