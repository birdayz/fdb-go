package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func memoRecordType(name string, nullable bool) *values.RecordType {
	return &values.RecordType{RecordName: name, Nullable: nullable, Fields: []values.Field{
		{Name: "A", FieldType: values.NewPrimitiveType(values.TypeCodeLong, false), Ordinal: 0},
		{Name: "B", FieldType: values.NewArrayType(true, values.NewPrimitiveType(values.TypeCodeString, false)), Ordinal: 1},
	}}
}

// recordTypesEqual is RecordType.Equals: memoising a pair that compared equal
// must not make an unequal pair equal, by value or by a reused object.
func TestRecordTypesEqualMatchesEquals(t *testing.T) {
	a, b := memoRecordType("R", true), memoRecordType("R", true)
	other := memoRecordType("R", false)
	for range 3 {
		if !recordTypesEqual(a, b) || !recordTypesEqual(b, a) || !recordTypesEqual(a, a) {
			t.Fatal("equal record types compared unequal")
		}
		if recordTypesEqual(a, other) || recordTypesEqual(other, b) {
			t.Fatal("unequal record types compared equal")
		}
	}
	if recordTypesEqual(a, nil) || recordTypesEqual(nil, a) {
		t.Fatal("a record type compared equal to nil")
	}
}

// A full memo starts over and keeps answering correctly.
func TestRecordTypesEqualMemoIsBounded(t *testing.T) {
	keep := make([]*values.RecordType, 0, 2*(recordTypeEqualMemoEntries+8))
	for range recordTypeEqualMemoEntries + 8 {
		a, b := memoRecordType("R", true), memoRecordType("R", true)
		keep = append(keep, a, b)
		if !recordTypesEqual(a, b) {
			t.Fatal("equal record types compared unequal")
		}
	}
	if n := recordTypeEqualMemo.size.Load(); n > recordTypeEqualMemoEntries {
		t.Fatalf("memo holds %d pairs, bound %d", n, recordTypeEqualMemoEntries)
	}
	if recordTypesEqual(keep[0], memoRecordType("R", false)) {
		t.Fatal("unequal record types compared equal after a reset")
	}
}
