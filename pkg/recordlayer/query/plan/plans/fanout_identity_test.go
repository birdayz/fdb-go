package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFanOutOrderingStopsAtUnpinnedEquality(t *testing.T) {
	t.Parallel()
	row := values.NewRecordType("fanout_zero", false, []values.Field{
		{Name: "A", FieldType: values.NewArrayType(true, values.NullableDouble), Ordinal: 0},
		{Name: "B", FieldType: values.NullableDouble, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	for _, tc := range []struct {
		name       string
		typ        values.Type
		comparison *predicates.ComparisonRange
		pinned     bool
	}{
		{"double zero", values.NullableDouble, pkOrderingEq(t, float64(0)), false},
		{"float zero", values.NullableFloat, pkOrderingEq(t, float32(0)), false},
		{"unknown zero", values.UnknownType, pkOrderingEq(t, float64(0)), false},
		{"unknown dynamic", values.UnknownType, plannerDynamicEquality(t, values.UnknownType), false},
		{"nonzero control", values.NullableDouble, pkOrderingEq(t, float64(1)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
					return NewRecordQueryIndexPlan("AB", []*predicates.ComparisonRange{
						tc.comparison, pkOrderingEq(t, float64(0)),
					}, []string{"T"}, row, reverse)
				}).WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false).
					WithKeyComponentTypes([]values.Type{tc.typ, values.NullableDouble}).
					WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
					WithDistinctRecordsSignal(true).
					WithKeyColumnDuplicates([]bool{true, false})
				if plain := plan.HintOrdering(); plain.IsKnown || len(plain.Keys) != 0 {
					t.Fatalf("reverse=%t: zero-bound B cannot carry plain suffix ordering: %#v", reverse, plain)
				}
				rich := plan.HintRichOrdering()
				if !tc.pinned && len(rich.GetKeys()) != 0 {
					t.Fatalf("reverse=%t: unpinned fan-out must stop rich ordering: %v", reverse, rich.GetKeys())
				}
				for _, direction := range []properties.RequestedSortOrder{properties.RequestedSortOrderAscending, properties.RequestedSortOrderDescending} {
					requested := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{{
						Value: testFieldIn(t, row, "requested", "B"), SortOrder: direction,
					}}, properties.DistinctnessPreserveDistinctness, false)
					want := tc.pinned && (reverse == (direction == properties.RequestedSortOrderDescending))
					if got := rich.Satisfies(requested); got != want {
						t.Fatalf("reverse=%t: satisfies B %v = %t, want %t", reverse, direction, got, want)
					}
				}
				if got := len(plan.recordIdentityCoordinates()) > 0; got != tc.pinned {
					t.Fatalf("record identity = %t, want %t", got, tc.pinned)
				}
			}
		})
	}
}

// An equality on every exploded key coordinate pins one physical entry per
// record. A logical float equality can cover both signed-zero entries instead.
func TestFanOutPinnedIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		duplicates []bool
		types      []values.Type
		comps      []*predicates.ComparisonRange
		want       bool
	}{
		{"bound", []bool{true, true, false}, testPhysicalLongTypes(3), []*predicates.ComparisonRange{pkOrderingEq(t, int64(1)), pkOrderingEq(t, int64(2))}, true},
		{"unbound", []bool{true, true, false}, testPhysicalLongTypes(3), []*predicates.ComparisonRange{pkOrderingEq(t, int64(1))}, false},
		{"residual after range", []bool{false, true, false}, testPhysicalLongTypes(3), []*predicates.ComparisonRange{pkOrderingGT(t, int64(0)), pkOrderingEq(t, int64(7))}, false},
		{"residual after gap", []bool{false, true, false}, testPhysicalLongTypes(3), []*predicates.ComparisonRange{nil, pkOrderingEq(t, int64(7))}, false},
		{"unknown classification", nil, testPhysicalLongTypes(3), []*predicates.ComparisonRange{pkOrderingEq(t, int64(1)), pkOrderingEq(t, int64(2))}, false},
		{"float zero", []bool{true, false, false}, []values.Type{values.NotNullDouble, values.NotNullLong, values.NotNullLong}, []*predicates.ComparisonRange{pkOrderingEq(t, float64(0))}, false},
		{"float nonzero", []bool{true, false, false}, []values.Type{values.NotNullDouble, values.NotNullLong, values.NotNullLong}, []*predicates.ComparisonRange{pkOrderingEq(t, float64(1))}, true},
		{"unknown dynamic", []bool{true, false, false}, nil, []*predicates.ComparisonRange{plannerDynamicEquality(t, values.UnknownType)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
				return NewRecordQueryIndexPlan("IDX", tc.comps, []string{"T"}, indexOrderingLayout(), false)
			}).WithIndexMetadata([]string{"A", "B", "C"}, []string{"ID"}, false).
				WithKeyComponentTypes(tc.types).
				WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1)).
				WithDistinctRecordsSignal(true).
				WithKeyColumnDuplicates(tc.duplicates)
			if got := plan.fanOutPinned(); got != tc.want {
				t.Fatalf("fanOutPinned = %v, want %v", got, tc.want)
			}
			if got := len(plan.recordIdentityCoordinates()) > 0; got != tc.want {
				t.Fatalf("record identity present = %v, want %v", got, tc.want)
			}
		})
	}
}
