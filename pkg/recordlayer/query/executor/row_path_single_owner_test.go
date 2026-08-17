package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The row path's cost is pinned STRUCTURALLY rather than by a measured
// allocation count: every test in this package is parallel, so
// testing.AllocsPerRun panics and testing.Benchmark reads process-wide MemStats
// that a concurrent test inflates. The property that PRODUCES the saving —
// a row minted with its plan's layout crosses the output boundary as itself —
// is deterministic, and it is what these tests assert.

func scanPlanWithLayout(t *testing.T, rowType *values.RecordType) (plans.RecordQueryPlan, values.OrdinalLayout) {
	t.Helper()
	plan, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	if err != nil {
		t.Fatalf("scan plan: %v", err)
	}
	layout, err := plan.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("provided layout: %v", err)
	}
	return plan, layout
}

// TestStampedRowCrossesTheOutputBoundaryAsItself pins the whole point of
// stamping a minted row with mintedRowLayout: the boundary takes its identity
// fast path and the row is not copied.
//
// The failure this guards is SILENT. A producer that stops stamping, or a
// mintedRowLayout that starts answering with a different handle than the
// boundary publishes, still yields correct rows — the boundary just copies every
// one of them again, and no correctness test can tell. On a 20k-row wide scan
// whose plan crosses two boundaries that was 4.4 allocations per row, the
// largest single item in the row path.
func TestStampedRowCrossesTheOutputBoundaryAsItself(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	plan, layout := scanPlanWithLayout(t, rowType)

	stamp := mintedRowLayout(plan)
	if stamp == nil {
		t.Fatal("mintedRowLayout returned nothing for a record plan, so no producer can stamp")
	}
	if stamp != layout {
		t.Fatal("mintedRowLayout and the output boundary disagree on the handle; every " +
			"minted row would be copied at the boundary to acquire the other one")
	}

	minted := &PositionalRow{Type: rowType, Slots: []any{int64(42)}, Layout: stamp}
	cursor, err := attachProvidedOutputLayout(plan, recordlayer.FromList([]QueryResult{{Positional: minted}}))
	if err != nil {
		t.Fatalf("attach output layout: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("next = (%v, %v), want row", result, err)
	}
	if emitted := result.GetValue().Positional; emitted != minted {
		t.Error("the boundary copied a row that already carried its layout — the " +
			"identity fast path did not fire")
	}
}

// TestBoundaryStillRejectsAStampedRowOutsideItsCarrier pins that stamping does
// not buy a row past the check. The carrier check runs BEFORE the identity fast
// path, so a producer cannot smuggle a mis-shaped row through by pre-setting the
// handle — which is the one way the optimisation above could have cost safety.
func TestBoundaryStillRejectsAStampedRowOutsideItsCarrier(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	plan, layout := scanPlanWithLayout(t, rowType)

	// The stamp is the plan's own handle; only the ROW disagrees with it.
	wrongType := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "EXTRA", FieldType: values.NotNullLong, Ordinal: 1},
	})
	smuggled := &PositionalRow{Type: wrongType, Slots: []any{int64(1), int64(2)}, Layout: layout}
	cursor, err := attachProvidedOutputLayout(plan, recordlayer.FromList([]QueryResult{{Positional: smuggled}}))
	if err != nil {
		t.Fatalf("attach output layout: %v", err)
	}
	if _, err := cursor.OnNext(context.Background()); err == nil {
		t.Error("a stamped row whose type is outside the layout carrier was published; the " +
			"carrier check must run before the identity fast path")
	}
}

// TestUnstampedRowIsCopiedWithIndependentSlots pins the arm the fast path does
// NOT cover, because that arm is why the fast path has to exist at all: a row
// arriving with a different layout may still be held by whoever produced it — a
// join re-emitting one outer row, a sort keeping its buffer — so the boundary
// owes it a copy whose slots do not alias.
func TestUnstampedRowIsCopiedWithIndependentSlots(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	plan, layout := scanPlanWithLayout(t, rowType)

	original := &PositionalRow{Type: rowType, Slots: []any{int64(42)}}
	cursor, err := attachProvidedOutputLayout(plan, recordlayer.FromList([]QueryResult{{Positional: original}}))
	if err != nil {
		t.Fatalf("attach output layout: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("next = (%v, %v), want row", result, err)
	}
	emitted := result.GetValue().Positional
	if emitted == original {
		t.Fatal("the boundary published the child's row in place")
	}
	if original.Layout != nil {
		t.Error("the boundary mutated the input row's layout")
	}
	if emitted.Layout != layout {
		t.Error("the copy did not retain the plan's layout handle")
	}
	emitted.Slots[0] = int64(7)
	if got, _ := original.Get(0); got != int64(42) {
		t.Errorf("the copy shares its slot array with the source row: source slot 0 = %v, want 42", got)
	}
}

// TestScanRowsAreStampedAtMintTime pins the scan producer's half: the row a scan
// builds carries the handle from birth, and a scan whose boundary publishes
// nothing leaves the row exactly as FromStoredRecord built it.
func TestScanRowsAreStampedAtMintTime(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	_, layout := scanPlanWithLayout(t, rowType)

	// storedRecordToQueryResult is the scan's row factory. Driving it with a nil
	// record isolates the stamping from proto materialization: a nil stored
	// record yields a nil row, and the stamp must not invent one.
	if got := storedRecordToQueryResult(layout); got == nil {
		t.Fatal("storedRecordToQueryResult returned no factory")
	}

	row := &PositionalRow{Type: rowType, Slots: []any{int64(9)}}
	stamped := stampRowLayout(QueryResult{Positional: row}, layout)
	if stamped.Positional.Layout != layout {
		t.Error("the scan's row factory did not stamp the plan's layout handle")
	}
	unstamped := stampRowLayout(QueryResult{Positional: &PositionalRow{Type: rowType, Slots: []any{int64(9)}}}, nil)
	if unstamped.Positional.Layout != nil {
		t.Error("a scan whose boundary publishes no layout must leave the row unstamped")
	}
}
