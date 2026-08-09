package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func physicalTypeTestEquality(value any) *predicates.ComparisonRange {
	comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, value)
	return predicates.EmptyComparisonRange().Merge(&comparison).Range
}

func requirePhysicalTypeCodes(t *testing.T, got []values.Type, want ...values.TypeCode) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("type count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] == nil || got[i].Code() != want[i] {
			t.Fatalf("type[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPhysicalKeyComponentTypesPreservedByPlanCopies(t *testing.T) {
	t.Parallel()

	comparisons := []*predicates.ComparisonRange{
		physicalTypeTestEquality(float64(0)),
		physicalTypeTestEquality(float64(1)),
	}
	index := NewRecordQueryIndexPlan("idx", comparisons, []string{"T"}, values.UnknownType, false).
		WithKeyComponentTypes([]values.Type{values.NullableFloat, values.NullableDouble})
	requirePhysicalTypeCodes(t, index.GetKeyComponentTypes(), values.TypeCodeFloat, values.TypeCodeDouble)
	requirePhysicalTypeCodes(t, index.WithStrictlySorted().GetKeyComponentTypes(), values.TypeCodeFloat, values.TypeCodeDouble)
	requirePhysicalTypeCodes(t, index.WithScanComparisons(comparisons[:1]).GetKeyComponentTypes(), values.TypeCodeFloat, values.TypeCodeDouble)

	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons(comparisons).
		WithKeyComponentTypes([]values.Type{values.NullableFloat, values.NullableDouble})
	requirePhysicalTypeCodes(t, scan.WithPrimaryKey(nil).GetKeyComponentTypes(), values.TypeCodeFloat, values.TypeCodeDouble)

	vector := NewRecordQueryVectorIndexPlan(
		"vec", comparisons, nil, values.LiteralValue(int64(3)),
		predicates.ComparisonDistanceRankLessThanOrEq, nil, nil,
		[]string{"T"}, values.UnknownType,
	).WithPartitionKeyComponentTypes([]values.Type{values.NullableFloat, values.NullableDouble})
	requirePhysicalTypeCodes(t, vector.WithOrderedStream().GetPartitionKeyComponentTypes(), values.TypeCodeFloat, values.TypeCodeDouble)
}

func TestPhysicalKeyComponentTypesAreStructuralIdentity(t *testing.T) {
	t.Parallel()

	comparison := []*predicates.ComparisonRange{physicalTypeTestEquality(float64(0))}
	floatPlan := NewRecordQueryIndexPlan("idx", comparison, []string{"T"}, values.UnknownType, false).
		WithKeyComponentTypes([]values.Type{values.NullableFloat})
	doublePlan := NewRecordQueryIndexPlan("idx", comparison, []string{"T"}, values.UnknownType, false).
		WithKeyComponentTypes([]values.Type{values.NullableDouble})
	if floatPlan.EqualsPlanWithoutChildren(doublePlan) {
		t.Fatal("FLOAT and DOUBLE physical keys must not share structural identity")
	}
	if floatPlan.HashCodeWithoutChildren() == doublePlan.HashCodeWithoutChildren() {
		t.Fatal("FLOAT and DOUBLE physical keys unexpectedly have the same structural hash")
	}
}

func TestPhysicalKeyMetadataDoesNotAliasCallerSlices(t *testing.T) {
	t.Parallel()

	comparison := []*predicates.ComparisonRange{physicalTypeTestEquality(float64(0))}
	columns := []string{"A"}
	primaryKey := []string{"ID"}
	index := NewRecordQueryIndexPlan("idx", comparison, []string{"T"}, values.UnknownType, false).
		WithIndexMetadata(columns, primaryKey, false).
		WithKeyComponentTypes([]values.Type{values.NullableFloat}).
		WithPhysicalGroupingPrefixCount(1)
	columns[0] = "MUTATED"
	primaryKey[0] = "MUTATED"
	if got := index.GetColumnNames()[0]; got != "A" {
		t.Fatalf("index column aliased caller slice: %q", got)
	}
	if got := index.GetPKColumnNames()[0]; got != "ID" {
		t.Fatalf("primary-key column aliased caller slice: %q", got)
	}

	groups := []string{"A", "B"}
	aggregate := NewRecordQueryAggregateIndexPlan(index, "T", values.UnknownType, "MAX").
		WithGroupColumns(groups, "V")
	groups[0] = "MUTATED"
	if got := aggregate.GetGroupCols()[0]; got != "A" {
		t.Fatalf("aggregate group column aliased caller slice: %q", got)
	}
	if got := aggregate.GetPhysicalGroupingPrefixCount(); got != 1 {
		t.Fatalf("physical grouping prefix = %d, want 1", got)
	}
	requirePhysicalTypeCodes(t, aggregate.GetKeyComponentTypes(), values.TypeCodeFloat)
}
