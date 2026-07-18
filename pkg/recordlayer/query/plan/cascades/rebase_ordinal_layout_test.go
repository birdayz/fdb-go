package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// legRC builds the positional RecordConstructorValue concat a FlatMap
// outer carries: each slot reads QOV(leg).col, names bare (duplicates
// across legs allowed — the layout is positional).
func legRC(entries ...[2]string) *values.RecordConstructorValue {
	fields := make([]values.RecordConstructorField, len(entries))
	for i, e := range entries {
		fields[i] = values.RecordConstructorField{
			Name: e[1],
			Value: values.NewFieldValue(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(e[0])),
				e[1], values.UnknownType,
			),
		}
	}
	return values.NewRecordConstructorValue(fields...)
}

// TestBuriedLegOrdinalLayout pins the (leg, column) → global-ordinal
// derivation (WS-N slice 4) for both derivable outer shapes.
func TestBuriedLegOrdinalLayout(t *testing.T) {
	t.Parallel()

	// FlatMap outer with an RC concat: A(id, flag) ++ B(id, a_id).
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	fm := plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		legRC([2]string{"A", "ID"}, [2]string{"A", "FLAG"}, [2]string{"B", "ID"}, [2]string{"B", "A_ID"}),
		false,
	)
	layout := buriedLegOrdinalLayout(fm)
	if layout == nil {
		t.Fatal("RC-concat FlatMap outer must derive a layout")
	}
	want := map[string]int{"A.ID": 0, "A.FLAG": 1, "B.ID": 2, "B.A_ID": 3}
	for k, w := range want {
		if got, ok := layout[k]; !ok || got != w {
			t.Errorf("layout[%s] = %d (ok=%v), want %d", k, got, ok, w)
		}
	}

	// Scan-chain outer: planBuriedLegConcat windows (single leg keyed by
	// its alias — the caller's alias argument is empty here, matching the
	// call site, so the single-scan case keys by "" and derives nothing
	// usable; a multi-leg NLJ chain windows per join alias).
	rt := &values.RecordType{Fields: []values.Field{
		{Name: "X", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "Y", FieldType: values.UnknownType, Ordinal: 1},
	}}
	typedScan := plans.NewRecordQueryScanPlan([]string{"T"}, rt, false)
	nlj := plans.NewRecordQueryNestedLoopJoinPlan(
		typedScan, typedScan, nil, plans.JoinInner, "L", "R",
		values.NewRecordConstructorValue(),
	)
	layout2 := buriedLegOrdinalLayout(nlj)
	if layout2 == nil {
		t.Fatal("ordinal-safe NLJ chain must derive a layout")
	}
	want2 := map[string]int{"L.X": 0, "L.Y": 1, "R.X": 2, "R.Y": 3}
	for k, w := range want2 {
		if got, ok := layout2[k]; !ok || got != w {
			t.Errorf("nlj layout[%s] = %d (ok=%v), want %d", k, got, ok, w)
		}
	}

	// Underivable: a FlatMap whose result value is not an RC concat.
	fmLazy := plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")), "ID", values.UnknownType),
		false,
	)
	if buriedLegOrdinalLayout(fmLazy) != nil {
		t.Fatal("non-RC FlatMap outer must not claim a layout")
	}
}

// TestRebaseOuterLegValue_OrdinalFirst pins the slice-4 rebase arms: a
// leg-matching lazy reference bakes to the merged row's global ordinal
// when the layout answers, keeps the lazy qualified mint on a layout
// miss, and keeps it with no layout at all (the pre-slice-4 behavior —
// the merged row's name-keyed read).
func TestRebaseOuterLegValue_OrdinalFirst(t *testing.T) {
	t.Parallel()
	mergedCorr := values.NamedCorrelationIdentifier("$m")
	legRef := values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")),
		"A_ID", values.UnknownType,
	)

	// Layout answers → born baked at the global ordinal.
	baked := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, map[string]int{"A.A_ID": 3})
	fv, ok := baked.(*values.FieldValue)
	if !ok {
		t.Fatalf("got %T", baked)
	}
	if fv.Resolved == nil || fv.Resolved.Root().Ordinal != 3 {
		t.Fatalf("want baked ordinal 3, got resolved=%v", fv.Resolved)
	}
	if fv.Field != "A.A_ID" {
		t.Fatalf("display field: got %q, want A.A_ID", fv.Field)
	}
	if qov, isQ := fv.Child.(*values.QuantifiedObjectValue); !isQ || qov.Correlation != mergedCorr {
		t.Fatalf("child must be QOV($m), got %T", fv.Child)
	}

	// Layout miss → lazy qualified mint.
	lazy := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, map[string]int{"B.OTHER": 0})
	lfv := lazy.(*values.FieldValue)
	if lfv.Resolved != nil {
		t.Fatalf("layout miss must stay lazy, got resolved=%v", lfv.Resolved)
	}
	if lfv.Field != "A.A_ID" {
		t.Fatalf("lazy field: got %q", lfv.Field)
	}

	// No layout → lazy qualified mint (pre-slice-4 behavior).
	lazy2 := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, nil)
	if lazy2.(*values.FieldValue).Resolved != nil {
		t.Fatal("nil layout must stay lazy")
	}

	// Non-matching leg → untouched.
	same := rebaseOuterLegValue(legRef, []string{"Z"}, mergedCorr, map[string]int{"A.A_ID": 3})
	if same != legRef {
		t.Fatal("non-matching leg must return the value unchanged")
	}
}
