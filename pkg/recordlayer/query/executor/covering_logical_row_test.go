package executor

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestBuildCoveringLogicalRow pins covering-index row conformance: a covering
// scan's row is shaped by the record's LOGICAL RecordType (descriptor field
// order — Java's IndexKeyValueToPartialRecord partial record), NOT the index
// layout [covered..., pk...]. A FieldValue ordinal baked against the record
// type must read the same slot on the base-scan and covering paths; with the
// index-layout row, a baked read of a value column silently served the PK
// column (SUM(b) summing ids — the TestFDB_MultiColumnIndex regression).
func TestBuildCoveringLogicalRow(t *testing.T) {
	t.Parallel()

	// mci_t(id, a, b, c) with index (a, b): logical order [ID A B C],
	// index layout [A B ID].
	logical := positionalTypeFromNames([]string{"ID", "A", "B", "C"})
	posNames := []string{"A", "B", "ID"}

	ords := coveringLogicalOrdinals(posNames, logical)
	if ords == nil {
		t.Fatal("coveringLogicalOrdinals: want a full mapping, got nil")
	}
	if ords[0] != 1 || ords[1] != 2 || ords[2] != 0 {
		t.Fatalf("logical ordinals = %v, want [1 2 0]", ords)
	}

	// PK carries a record-type prefix; the user PK is the tail (pkOffset skip).
	row := buildCoveringLogicalRow(
		[]string{"a", "b"}, []string{"id"},
		tuple.Tuple{"x", int64(20)}, tuple.Tuple{int64(7), int64(5)},
		logical, ords)

	if row.Type != logical {
		t.Fatal("covering row must carry the LOGICAL record type")
	}
	if v, _ := row.Get(0); v != int64(5) {
		t.Fatalf("Get(0)=ID: got %v, want 5 (pk tail, not the type-prefix 7)", v)
	}
	if v, _ := row.Get(1); v != "x" {
		t.Fatalf("Get(1)=A: got %v, want x", v)
	}
	if v, _ := row.Get(2); v != int64(20) {
		t.Fatalf("Get(2)=B: got %v, want 20 — the index-layout row served ID here", v)
	}
	if v, _ := row.Get(3); v != nil {
		t.Fatalf("Get(3)=C (non-covered): got %v, want nil (Java's unset partial field)", v)
	}

	// The regression axis: a reference baked to B's LOGICAL ordinal (2)
	// evaluates to the b value, not the id.
	bRef := values.NewFieldValueWithResolvedOrdinal("B", 2, values.UnknownType)
	got, err := bRef.Evaluate(values.OrdinalRow(row))
	if err != nil {
		t.Fatalf("baked read: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("baked B read = %v, want 20", got)
	}

	// The row's TYPE names slots identically to the base-scan path, so a
	// plan-time bake against it lands on the same slot.
	if idx, ok := row.Type.FieldIndex("B"); !ok || idx != 2 {
		t.Fatalf("FieldIndex(B) = (%d, %v), want (2, true)", idx, ok)
	}
}

// TestCoveringLogicalOrdinals_Fallback pins the all-or-nothing mapping rule and
// its CORRECT-or-LOUD consequence: a nil result (any covering column without a
// top-level logical slot — a nested/expression index column — or a multi-type
// scan with no single logical shape) means the scan cannot present a LOGICAL row.
// With flat refs baked to their logical ordinal, an index-layout row would misread
// a top-level sibling projection (descriptor [ID,A,ADDR] + index row [A,ADDR.CITY,
// ID]: A#1 reads ADDR.CITY — silent wrong rows). executeIndexScan therefore refuses
// such a covering scan LOUD at construction rather than serve the misread-able row;
// coveringIndexCursor.OnNext is logical-only. This test pins the DETECTION (nil)
// that the loud guard keys on; the guard itself is `if logicalOrds == nil { return
// error }` at the construction site.
func TestCoveringLogicalOrdinals_Fallback(t *testing.T) {
	t.Parallel()
	logical := positionalTypeFromNames([]string{"ID", "A"})
	if got := coveringLogicalOrdinals([]string{"A", "ADDR.CITY", "ID"}, logical); got != nil {
		t.Fatalf("unmappable (nested) column must yield nil (→ loud refusal), got %v", got)
	}
	if got := coveringLogicalOrdinals([]string{"A"}, nil); got != nil {
		t.Fatalf("nil logical type (multi-type scan) must yield nil (→ loud refusal), got %v", got)
	}
	// The mappable case still yields a full mapping (the scan is served logical).
	if got := coveringLogicalOrdinals([]string{"A", "ID"}, logical); got == nil {
		t.Fatal("a fully-mappable covering scan must yield logical ordinals, got nil")
	}
}
