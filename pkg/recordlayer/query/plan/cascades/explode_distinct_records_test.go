package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// DistinctRecordsProperty.visitExplodePlan: an ordinality explode's rows each
// carry their own ordinal, so they are distinct whether one-based (SQL `AT`) or
// zero-based; a bare explode's elements may repeat.
func TestExplodePlan_DistinctRecordsIffWithOrdinality(t *testing.T) {
	t.Parallel()
	array := &values.ConstantValue{
		Value: []any{int64(5), int64(5)},
		Typ:   values.NewArrayType(false, values.NotNullLong),
	}
	tests := []struct {
		name                      string
		withOrdinality, zeroBased bool
		distinct                  bool
	}{
		{name: "bare", distinct: false},
		{name: "one-based", withOrdinality: true, distinct: true},
		{name: "zero-based", withOrdinality: true, zeroBased: true, distinct: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan, err := plans.NewRecordQueryExplodePlanWithOrdinalityBase(array, test.withOrdinality, test.zeroBased)
			if err != nil {
				t.Fatal(err)
			}
			ref := expressions.InitialOf(plan)
			computeRefPlanProperties(ref)
			props := GetRefPlanPropertiesMap(ref).GetProperties(plan)
			if props == nil {
				t.Fatal("no properties computed for the explode plan")
			}
			if got := props.GetBool(properties.PropDistinctRecords); got != test.distinct {
				t.Fatalf("distinctRecords = %t, want %t", got, test.distinct)
			}
		})
	}
}
