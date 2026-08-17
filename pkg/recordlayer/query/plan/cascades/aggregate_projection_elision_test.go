package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aggregateLeafPublishesGroupByRow decides whether an aggregate-index match
// yields its leaf directly or wraps it in a renaming projection. The corpus only
// ever exercises the first answer (see the function's own census), so the second
// is untested by every SQL-level suite — which is exactly why the decision is
// driven here, from constructed inputs, in both directions.
//
// Getting the FALSE direction wrong is the expensive one: a leaf whose column
// names genuinely differ from the GroupBy's would then be published under the
// leaf's names, and the driver's column labels would silently be the physical
// ones rather than the ones the query asked for.
func TestAggregateLeafPublishesGroupByRowDecidesBothWays(t *testing.T) {
	t.Parallel()

	leafRow := func(names ...string) values.QuantifiedObjectValue {
		t.Helper()
		fields := make([]values.Field, len(names))
		for i, name := range names {
			fields[i] = values.Field{Name: name, FieldType: values.NotNullLong, Ordinal: i}
		}
		qov, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("leaf"),
			values.NewRecordType("agg", false, fields))
		if err != nil {
			t.Fatalf("leaf QOV over %v: %v", names, err)
		}
		return qov
	}

	for _, tc := range []struct {
		name    string
		leaf    []string
		want    []string
		publish bool
	}{
		{
			name:    "identical names in identical order publish directly",
			leaf:    []string{"CUSTOMER_ID", "SUM(AMOUNT)"},
			want:    []string{"CUSTOMER_ID", "SUM(AMOUNT)"},
			publish: true,
		},
		{
			name:    "a renamed aggregate needs the projection",
			leaf:    []string{"CUSTOMER_ID", "SUM(AMOUNT)"},
			want:    []string{"CUSTOMER_ID", "TOTAL"},
			publish: false,
		},
		{
			name:    "the same names in a different ORDER need the projection",
			leaf:    []string{"CUSTOMER_ID", "SUM(AMOUNT)"},
			want:    []string{"SUM(AMOUNT)", "CUSTOMER_ID"},
			publish: false,
		},
		{
			name:    "a case-only difference is still a difference",
			leaf:    []string{"CUSTOMER_ID", "SUM(AMOUNT)"},
			want:    []string{"customer_id", "SUM(AMOUNT)"},
			publish: false,
		},
		{
			name:    "a wider leaf needs the projection",
			leaf:    []string{"CUSTOMER_ID", "SUM(AMOUNT)", "COUNT(*)"},
			want:    []string{"CUSTOMER_ID", "SUM(AMOUNT)"},
			publish: false,
		},
		{
			name:    "a narrower leaf needs the projection",
			leaf:    []string{"CUSTOMER_ID"},
			want:    []string{"CUSTOMER_ID", "SUM(AMOUNT)"},
			publish: false,
		},
	} {
		got := aggregateLeafPublishesGroupByRow(leafRow(tc.leaf...), tc.want)
		if got != tc.publish {
			t.Errorf("%s: leaf %v against output %v = %v, want %v",
				tc.name, tc.leaf, tc.want, got, tc.publish)
		}
	}

	// A scalar leaf has no columns to compare, so it can never be the GroupBy's
	// row — the guard must decline rather than read fields off a non-record.
	scalar, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("scalar"), values.NotNullLong)
	if err != nil {
		t.Fatalf("scalar QOV: %v", err)
	}
	if aggregateLeafPublishesGroupByRow(scalar, []string{"SUM(AMOUNT)"}) {
		t.Error("a scalar leaf was treated as publishing a one-column GroupBy row")
	}
}
