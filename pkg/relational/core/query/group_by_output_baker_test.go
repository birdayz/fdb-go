package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestGroupByOutputBaker_RebindsInputBoundRef pins groupByOutputBaker's rebind
// rule: a SINGLE-accessor reference that names a GROUP BY output column binds
// to the OUTPUT ordinal even when it arrives ALREADY carrying an ordinal —
// the resolver's construction-time bind is against the reference's SOURCE row
// (the scan), which is dead on the aggregate's output row. Java performs the
// same rebind structurally when the reference is pulled up over the group-by.
// Skipping already-baked nodes here left the input ordinal live above the
// aggregate: `SELECT col1 + 10 FROM t1 GROUP BY col1 ORDER BY col1` read
// input slot 1 off the 1-column output row — a loud out-of-range miss, or a
// silent wrong slot on a wider output.
func TestGroupByOutputBaker_RebindsInputBoundRef(t *testing.T) {
	t.Parallel()

	keyOrds := map[string]int{"COL1": 0}
	aggOrds := map[string]int{"SUM(V)": 1}
	baker := groupByOutputBaker(keyOrds, aggOrds)

	// Input-bound ref (COL1 at scan ordinal 1) must rebind to output slot 0.
	inputBound := values.NewFieldValueWithResolvedOrdinal("COL1", 1, values.UnknownType)
	got := baker(inputBound)
	fv, ok := got.(*values.FieldValue)
	if !ok || fv.Resolved == nil {
		t.Fatalf("rebind: got %#v, want a baked FieldValue", got)
	}
	if fv.Resolved.Root().Ordinal != 0 {
		t.Fatalf("rebind: ordinal %d, want 0 (the OUTPUT slot)", fv.Resolved.Root().Ordinal)
	}

	// Idempotent: a ref already bound to the output row rebinds to itself.
	again := baker(got)
	if afv := again.(*values.FieldValue); afv.Resolved.Root().Ordinal != 0 {
		t.Fatalf("rebind not idempotent: ordinal %d", afv.Resolved.Root().Ordinal)
	}

	// A lazy ref still bakes (the pre-existing path).
	lazy := values.NewFlatFieldValue("SUM(V)", values.UnknownType)
	if b := baker(lazy).(*values.FieldValue); b.Resolved == nil || b.Resolved.Root().Ordinal != 1 {
		t.Fatalf("lazy agg ref: got %#v, want output ordinal 1", b)
	}

	// A multi-accessor baked path is a nested access, never a bare
	// output-column ref — untouched.
	multi := &values.FieldValue{
		Field: "COL1",
		Typ:   values.UnknownType,
		Resolved: &values.FieldPath{Accessors: []values.ResolvedAccessor{
			{Field: "REC", Ordinal: 0},
			{Field: "COL1", Ordinal: 1},
		}},
	}
	if got := baker(multi); got != values.Value(multi) {
		t.Fatalf("multi-accessor path must pass through untouched, got %#v", got)
	}

	// A name that is NOT an output column stays as-is (input-bound ordinal
	// kept — it references the pass-through row, not the aggregate output).
	other := values.NewFieldValueWithResolvedOrdinal("OTHER", 3, values.UnknownType)
	if got := baker(other); got != values.Value(other) {
		t.Fatalf("non-output ref must pass through untouched, got %#v", got)
	}
}
