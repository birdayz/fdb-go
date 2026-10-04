package cascades

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestGroupByRowOverAggregateRow drives both arms of the aggregate scan's row
// publication: equal width renames slot for slot (types and order kept, so a
// case-only or reordered name is taken verbatim), and any width mismatch
// declines because the scan's slots do not carry the GroupBy's row.
func TestGroupByRowOverAggregateRow(t *testing.T) {
	t.Parallel()

	row := func(types ...values.Type) *values.RecordType {
		fields := make([]values.Field, len(types))
		for i, ft := range types {
			fields[i] = values.Field{Name: []string{"G", "SUM(V)", "COUNT(*)"}[i], FieldType: ft, Ordinal: i}
		}
		return values.NewRecordType("", false, fields)
	}
	names := func(r *values.RecordType) []string {
		out := make([]string, len(r.Fields))
		for i, f := range r.Fields {
			out[i] = f.Name
		}
		return out
	}

	for _, want := range [][]string{
		{"G", "SUM(V)"},
		{"G", "SUM(GB.V)"},
		{"SUM(V)", "G"},
		{"g", "SUM(V)"},
	} {
		leaf := row(values.NotNullLong, values.NullableLong)
		got, ok := groupByRowOverAggregateRow(leaf, want)
		if !ok {
			t.Fatalf("%v: declined an equal-width row", want)
		}
		if !slices.Equal(names(got), want) {
			t.Errorf("published names %v, want %v", names(got), want)
		}
		for i := range got.Fields {
			if got.Fields[i].Ordinal != i || !sameExactType(got.Fields[i].FieldType, leaf.Fields[i].FieldType) {
				t.Errorf("%v: field %d is %v#%d, want the scan's %v#%d",
					want, i, got.Fields[i].FieldType, got.Fields[i].Ordinal, leaf.Fields[i].FieldType, i)
			}
		}
		if names(leaf)[1] != "SUM(V)" {
			t.Fatalf("publication renamed the scan's own row: %v", names(leaf))
		}
	}

	for _, tc := range []struct {
		leaf *values.RecordType
		want []string
	}{
		{row(values.NotNullLong, values.NullableLong, values.NullableLong), []string{"G", "SUM(V)"}},
		{row(values.NotNullLong), []string{"G", "SUM(V)"}},
		{nil, []string{"G"}},
	} {
		if got, ok := groupByRowOverAggregateRow(tc.leaf, tc.want); ok {
			t.Errorf("published %v over a scan row of a different width", got)
		}
	}
}
