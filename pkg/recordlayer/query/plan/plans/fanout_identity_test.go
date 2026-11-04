package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

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
