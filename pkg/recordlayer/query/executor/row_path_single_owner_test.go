package executor

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

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

// TestScanRowsAreStampedAtMintTime pins the scan producer's half END TO END,
// through the actual row factory rather than through stampRowLayout.
//
// The distinction is the whole test. An earlier version called stampRowLayout
// directly, which pins only that a setter sets: returning bare FromStoredRecord
// from storedRecordToQueryResult — the exact regression that costs a copy per row
// at every boundary — left it green. So this drives
// storedRecordToQueryResult(layout) over a real stored record, which is what
// executeScanWithRowLayout hands to the cursor, and then crosses the boundary with
// the row it produced.
//
// It also pins the nil arm, which is not a degenerate case but a load-bearing one:
// executeScanUnstamped exists so a TypeFilter fused over a primary scan takes its
// rows UNSTAMPED, because that stage deliberately bypasses the scan's output
// boundary and a foreign descriptor from an intermingled store would otherwise
// carry a layout that lies about it.
func TestScanRowsAreStampedAtMintTime(t *testing.T) {
	t.Parallel()

	stored := &recordlayer.FDBStoredRecord[proto.Message]{
		Record: &wrapperspb.StringValue{Value: "x"},
	}
	// The plan's row type is taken from the factory itself, so the layout the scan
	// publishes really is the one this row belongs to — the stamp is then the same
	// claim the boundary makes, one step earlier.
	shape := FromStoredRecord(stored)
	if shape.Positional == nil || shape.Positional.Type == nil {
		t.Fatal("the stored-record factory produced no typed row")
	}
	if shape.Positional.Layout != nil {
		t.Fatal("FromStoredRecord stamped a layout on its own; this test cannot then " +
			"distinguish the stamping factory from the plain one")
	}
	plan, layout := scanPlanWithLayout(t, shape.Positional.Type)

	stamping := storedRecordToQueryResult(layout)
	if stamping == nil {
		t.Fatal("storedRecordToQueryResult returned no factory")
	}
	minted := stamping(stored)
	if minted.Positional == nil {
		t.Fatal("the stamping factory produced no row")
	}
	if minted.Positional.Layout != layout {
		t.Fatalf("the scan's row factory did not stamp the plan's layout handle "+
			"(got %v); every scanned row is then copied at the output boundary to "+
			"acquire a handle it could have been born with", minted.Positional.Layout)
	}

	// End to end: the row the factory minted crosses the boundary as itself.
	cursor, err := attachProvidedOutputLayout(plan, recordlayer.FromList([]QueryResult{minted}))
	if err != nil {
		t.Fatalf("attach output layout: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("next = (%v, %v), want row", result, err)
	}
	if result.GetValue().Positional != minted.Positional {
		t.Error("a factory-stamped scan row was still copied at the output boundary")
	}

	// The nil arm: no layout published means the factory is FromStoredRecord and
	// the row stays exactly as it was built.
	plainRow := storedRecordToQueryResult(nil)(stored)
	if plainRow.Positional == nil {
		t.Fatal("the plain factory produced no row")
	}
	if plainRow.Positional.Layout != nil {
		t.Error("a scan whose boundary publishes no layout must leave the row " +
			"unstamped; executeScanUnstamped depends on it")
	}
}

// TestIdentityAttachIsPresenceEquivalent pins why the identity fast path may skip
// finishAttach.
//
// finishAttach clears LayoutPresence when the prior layout is not RawEqual to the
// one being attached — it is discarding another address space's matched/unmatched
// bits. The fast path returns the row untouched, so the two agree only while
// pointer equality implies RawEqual, i.e. while RawEqual is REFLEXIVE. Nothing
// else states that, and if it stopped holding the fast path would start preserving
// presence the copy path would have dropped: an unmatched null-supplying source
// would read as matched, which is a wrong answer for an outer join and not a
// crash.
func TestIdentityAttachIsPresenceEquivalent(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	_, layout := scanPlanWithLayout(t, rowType)

	if !layout.RawEqual(layout) {
		t.Fatal("RawEqual is not reflexive, so the identity fast path no longer " +
			"agrees with finishAttach about LayoutPresence")
	}

	// And the fast path is what runs for an already-carrying row: same pointer out,
	// so whatever presence the row held is still the presence of THIS layout.
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatalf("layout row: %v", err)
	}
	attached, err := row.AttachOrdinalLayout(layout, layout.Carrier().FlowedType())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if attached != row {
		t.Error("a row already carrying the layout was not returned as itself")
	}
}
