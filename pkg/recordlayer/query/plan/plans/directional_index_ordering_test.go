package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func directionalIndexPlan(t *testing.T, direction values.OrderedBytesDirection, reverse bool) *RecordQueryIndexPlan {
	t.Helper()
	return mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, indexOrderingLayout(), reverse)
	}).
		WithKeyComponentTypes(testPhysicalLongTypes(2)).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1)).
		WithOrderingDirections([]values.OrderedBytesDirection{direction, values.OrderedBytesAscNullsFirst})
}

// TestRecordQueryIndexPlan_OrderFunctionColumnDirection: an order-function
// key column orders the scan by its field in the function's direction, and a
// reverse scan flips both direction and null placement; the natural columns
// keep the scan direction.
func TestRecordQueryIndexPlan_OrderFunctionColumnDirection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		direction values.OrderedBytesDirection
		reverse   bool
		want      properties.ProvidedSortOrder
	}{
		{values.OrderedBytesAscNullsFirst, false, properties.ProvidedSortOrderAscending},
		{values.OrderedBytesAscNullsFirst, true, properties.ProvidedSortOrderDescending},
		{values.OrderedBytesAscNullsLast, false, properties.ProvidedSortOrderAscendingNullsLast},
		{values.OrderedBytesAscNullsLast, true, properties.ProvidedSortOrderDescendingNullsFirst},
		{values.OrderedBytesDescNullsFirst, false, properties.ProvidedSortOrderDescendingNullsFirst},
		{values.OrderedBytesDescNullsFirst, true, properties.ProvidedSortOrderAscendingNullsLast},
		{values.OrderedBytesDescNullsLast, false, properties.ProvidedSortOrderDescending},
		{values.OrderedBytesDescNullsLast, true, properties.ProvidedSortOrderAscending},
	} {
		plan := directionalIndexPlan(t, tc.direction, tc.reverse)
		natural := properties.ProvidedSortOrderAscending
		if tc.reverse {
			natural = properties.ProvidedSortOrderDescending
		}
		wantDesc := tc.want == properties.ProvidedSortOrderDescending ||
			tc.want == properties.ProvidedSortOrderDescendingNullsFirst
		wantNullsFirst := tc.want == properties.ProvidedSortOrderAscending ||
			tc.want == properties.ProvidedSortOrderDescendingNullsFirst

		got := plan.HintOrdering()
		if !got.IsKnown || len(got.Keys) != 3 {
			t.Fatalf("%v reverse=%v: HintOrdering = %#v, want [A, B, ID]", tc.direction, tc.reverse, got)
		}
		if got.DescendingAt(0) != wantDesc || got.NullsFirstAt(0) != wantNullsFirst {
			t.Errorf("%v reverse=%v: A desc=%v nullsFirst=%v, want %v",
				tc.direction, tc.reverse, got.DescendingAt(0), got.NullsFirstAt(0), tc.want)
		}
		for i := 1; i < 3; i++ {
			if got.DescendingAt(i) != tc.reverse {
				t.Errorf("%v reverse=%v: natural key %d desc=%v, want the scan direction",
					tc.direction, tc.reverse, i, got.DescendingAt(i))
			}
		}

		rich := plan.HintRichOrdering()
		keys := rich.GetKeys()
		if len(keys) != 3 {
			t.Fatalf("%v reverse=%v: rich ordering has %d keys, want 3", tc.direction, tc.reverse, len(keys))
		}
		bm := rich.GetBindingMap()
		for i, want := range []properties.ProvidedSortOrder{tc.want, natural, natural} {
			if b := bm[keys[i]]; len(b) != 1 || b[0].GetSortOrder() != want {
				t.Errorf("%v reverse=%v: rich key %d = %v, want %v", tc.direction, tc.reverse, i, b, want)
			}
		}
	}
}

// TestRecordQueryMapPlan_HintRichOrderingNamesItsOutput: a map's ordering is
// its child's pulled up through the projection (Java's visitMapPlan), so a
// reordering projection orders by its own output columns, and a child key it
// does not project ends the ordering.
func TestRecordQueryMapPlan_HintRichOrderingNamesItsOutput(t *testing.T) {
	t.Parallel()
	scan := directionalIndexPlan(t, values.OrderedBytesDescNullsLast, false)
	innerQ := QuantifierOverPlan(scan)
	row := scan.GetFlowedType()
	cols := reanchoredOverPlan(t, scan,
		orderingColumnOfName(scan.GetResultValue(), row, "A"),
		orderingColumnOfName(scan.GetResultValue(), row, "B"),
		orderingColumnOfName(scan.GetResultValue(), row, "ID"))
	a, b, id := cols[0], cols[1], cols[2]

	reordered := projectionMapForTest(t, innerQ, []values.Value{id, b, a}, []string{"Z", "Y", "X"})
	rich := reordered.HintRichOrdering()
	keys := rich.GetKeys()
	if len(keys) != 3 {
		t.Fatalf("map ordering has %d keys, want [X, Y, Z]", len(keys))
	}
	bm := rich.GetBindingMap()
	for i, want := range []struct {
		ordinal int
		order   properties.ProvidedSortOrder
	}{
		{2, properties.ProvidedSortOrderDescending},
		{1, properties.ProvidedSortOrderAscending},
		{0, properties.ProvidedSortOrderAscending},
	} {
		fv, ok := values.AsFieldValue(keys[i])
		if !ok || fv.Path().Len() != 1 || fv.Path().Ordinals()[0] != want.ordinal {
			t.Fatalf("map ordering key %d = %s, want output ordinal %d", i, values.ExplainValue(keys[i]), want.ordinal)
		}
		if qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue()); !ok || qov.Correlation() != values.CurrentCorrelation() {
			t.Fatalf("map ordering key %d = %s, want a read of the map's own row", i, values.ExplainValue(keys[i]))
		}
		if bs := bm[keys[i]]; len(bs) != 1 || bs[0].GetSortOrder() != want.order {
			t.Fatalf("map ordering key %d = %v, want %v", i, bs, want.order)
		}
	}

	dropsLeading := projectionMapForTest(t, innerQ, []values.Value{b, id}, []string{"Y", "Z"})
	if keys := dropsLeading.HintRichOrdering().GetKeys(); len(keys) != 0 {
		t.Fatalf("a map without the leading key still claims %d ordering keys", len(keys))
	}
}
