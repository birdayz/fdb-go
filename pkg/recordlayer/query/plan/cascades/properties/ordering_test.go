package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestEstimateOrdering_Scan_NotKnown(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	o := EstimateOrdering(scan)
	if o.IsKnown {
		t.Fatalf("FullUnorderedScan ordering = known, want unknown")
	}
}

func TestEstimateOrdering_Sort_KnownByKeys(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(t, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(t, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	o := EstimateOrdering(sort)
	if !o.IsKnown {
		t.Fatal("Sort ordering = unknown, want known")
	}
	if len(o.Keys) != 1 {
		t.Fatalf("Sort.Keys len = %d, want 1", len(o.Keys))
	}
}

func TestEstimateOrdering_Filter_InheritsInner(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(t, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(t, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustLogicalFilterExpression(t,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(sort)))

	o := EstimateOrdering(filter)
	if !o.IsKnown {
		t.Fatal("Filter(Sort(...)) ordering = unknown, want known (Filter preserves order)")
	}
}

func TestEstimateOrdering_FilterOverScan_NotKnown(t *testing.T) {
	t.Parallel()
	// Filter over an unordered scan inherits unknown.
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustLogicalFilterExpression(t,
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	o := EstimateOrdering(filter)
	if o.IsKnown {
		t.Fatal("Filter(Scan) ordering = known, want unknown")
	}
}

func TestIsOrdered_Convenience(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	if IsOrdered(scan) {
		t.Fatal("IsOrdered(Scan) = true, want false")
	}

	keys := []expressions.SortKey{
		{Value: propertyField(t, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(t, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	if !IsOrdered(sort) {
		t.Fatal("IsOrdered(Sort) = false, want true")
	}
}

// TestEstimateOrdering_DMLInheritsInner pins that DML operations
// (Insert / Update / Delete) inherit ordering from their inner.
// Important because DML is row-pass-through — if the inner is a
// sorted scan, the DML output can be assumed sorted too.
func TestEstimateOrdering_InsertInheritsInner(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"Source"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(t, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(t, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	ins := mustInsertExpression(t,
		expressions.ForEachQuantifier(expressions.InitialOf(sort)),
		"Target",
		propertyTestFlowedType())

	o := EstimateOrdering(ins)
	if !o.IsKnown {
		t.Fatal("Insert(Sort(...)) ordering = unknown, want known (DML pass-through)")
	}
}

func TestEstimateOrdering_DeleteInheritsInner(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"Order"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(t, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(t, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	del := mustDeleteExpression(t,
		expressions.ForEachQuantifier(expressions.InitialOf(sort)),
		"Order")

	o := EstimateOrdering(del)
	if !o.IsKnown {
		t.Fatal("Delete(Sort(...)) ordering = unknown, want known (DML pass-through)")
	}
}

func TestEstimateOrdering_Union_NotKnown(t *testing.T) {
	t.Parallel()
	scanA := mustFullUnorderedScanExpression(t, []string{"A"}, propertyTestFlowedType())
	scanB := mustFullUnorderedScanExpression(t, []string{"B"}, propertyTestFlowedType())
	union := mustLogicalUnionExpression(t, []expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	})

	o := EstimateOrdering(union)
	if o.IsKnown {
		t.Fatal("Union ordering = known, want unknown (concat loses ordering)")
	}
}

// TestEstimateOrdering_DistinctOverSortPreserves pins that Distinct
// over Sort inherits the Sort's ordering — Distinct doesn't reorder
// rows, just drops duplicates.
func TestEstimateOrdering_DistinctOverSortPreserves(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(t, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(t, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	dist := mustLogicalDistinctExpression(t, expressions.ForEachQuantifier(expressions.InitialOf(sort)))
	o := EstimateOrdering(dist)
	if !o.IsKnown {
		t.Fatal("Distinct(Sort(...)) ordering = unknown, want known (Distinct preserves)")
	}
}

// TestEstimateOrdering_DistinctOverScanNotKnown verifies that
// Distinct over an unsorted scan still produces unknown.
func TestEstimateOrdering_DistinctOverScanNotKnown(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	dist := mustLogicalDistinctExpression(t, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	o := EstimateOrdering(dist)
	if o.IsKnown {
		t.Fatal("Distinct(Scan) ordering = known, want unknown (scan is unordered)")
	}
}

// TestEstimateOrdering_UniqueOverSortPreserves pins that Unique
// (PK-based dedup) preserves inner ordering — same rationale as
// Distinct.
func TestEstimateOrdering_UniqueOverSortPreserves(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	keys := []expressions.SortKey{
		{Value: propertyField(t, "id", values.NotNullLong)},
	}
	sort := mustLogicalSortExpression(t, keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	uq := mustLogicalUniqueExpression(t, expressions.ForEachQuantifier(expressions.InitialOf(sort)))
	o := EstimateOrdering(uq)
	if !o.IsKnown {
		t.Fatal("Unique(Sort(...)) ordering = unknown, want known (Unique preserves)")
	}
}
