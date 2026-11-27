package values

import "testing"

// TestExactPrimitivesAreShared pins the property that makes primitive snapshots
// free: an exact type is content-addressed and immutable, so two snapshots of
// the same primitive are the same OBJECT, not merely equal ones.
//
// The volume lives in the leaves. A record's snapshot recurses once per field,
// so a twelve-column row is one record node and eleven primitives; over the
// pure-planner sweep that was 243.8 million snapshot calls producing 170
// distinct canonical types and allocating 52.65GB, 22% of everything the
// planner allocated.
//
// Both halves are asserted, because either one alone can go silently wrong:
// the table must COVER every admitted code (a miss falls back to a fresh node,
// which is correct and slow), and it must not COLLAPSE distinct types (a
// nullable and a non-nullable LONG are different exact types and must stay two
// objects).
func TestExactPrimitivesAreShared(t *testing.T) {
	t.Parallel()

	if len(exactPrimitiveCodes) == 0 {
		t.Fatal("the admitted-primitive list is empty, so this test proves nothing")
	}
	for _, code := range exactPrimitiveCodes {
		for _, nullable := range []bool{false, true} {
			first, err := SnapshotExactType(NewPrimitiveType(code, nullable))
			if err != nil {
				t.Fatalf("snapshot %v nullable=%v: %v", code, nullable, err)
			}
			second, err := SnapshotExactType(NewPrimitiveType(code, nullable))
			if err != nil {
				t.Fatalf("snapshot %v nullable=%v (again): %v", code, nullable, err)
			}
			if first != second {
				t.Errorf("snapshotting %v nullable=%v twice produced two objects; "+
					"the shared table does not cover this code, so every record "+
					"field of this type allocates a fresh leaf", code, nullable)
			}
			other, err := SnapshotExactType(NewPrimitiveType(code, !nullable))
			if err != nil {
				t.Fatalf("snapshot %v nullable=%v: %v", code, !nullable, err)
			}
			if first == other {
				t.Errorf("%v nullable=%v and nullable=%v share one exact object; "+
					"nullability IS part of exact identity", code, nullable, !nullable)
			}
		}
	}
}

// TestExactRecordFieldsReuseSharedPrimitives is the same property observed
// where it pays off: through the recursion, not through the top-level entry
// point. A record snapshot that rebuilt its leaves would still pass the test
// above.
func TestExactRecordFieldsReuseSharedPrimitives(t *testing.T) {
	t.Parallel()

	row := &RecordType{
		RecordName: "R",
		Fields: []Field{
			{Name: "A", Ordinal: 0, FieldType: NewPrimitiveType(TypeCodeLong, false)},
			{Name: "B", Ordinal: 1, FieldType: NewPrimitiveType(TypeCodeString, true)},
		},
	}
	handle, err := SnapshotExactType(row)
	if err != nil {
		t.Fatalf("snapshot record: %v", err)
	}
	exact := handle.(*exactType)
	if len(exact.fields) != 2 {
		t.Fatalf("snapshot has %d fields, want 2", len(exact.fields))
	}
	wantA := sharedPrimitiveExactType(TypeCodeLong, false)
	wantB := sharedPrimitiveExactType(TypeCodeString, true)
	if exact.fields[0].typ != wantA || exact.fields[1].typ != wantB {
		t.Error("record fields did not reuse the shared primitive leaves; " +
			"the recursion is allocating one exact type per field per snapshot")
	}
}
