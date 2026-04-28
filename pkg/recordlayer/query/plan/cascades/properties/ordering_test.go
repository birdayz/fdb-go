package properties_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestEstimateOrdering_Scan_NotKnown(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	o := properties.EstimateOrdering(scan)
	if o.IsKnown {
		t.Fatalf("FullUnorderedScan ordering = known, want unknown")
	}
}

func TestEstimateOrdering_Sort_KnownByKeys(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	o := properties.EstimateOrdering(sort)
	if !o.IsKnown {
		t.Fatal("Sort ordering = unknown, want known")
	}
	if len(o.Keys) != 1 {
		t.Fatalf("Sort.Keys len = %d, want 1", len(o.Keys))
	}
}

func TestEstimateOrdering_Filter_InheritsInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(sort)),
	)
	o := properties.EstimateOrdering(filter)
	if !o.IsKnown {
		t.Fatal("Filter(Sort(...)) ordering = unknown, want known (Filter preserves order)")
	}
}

func TestEstimateOrdering_FilterOverScan_NotKnown(t *testing.T) {
	t.Parallel()
	// Filter over an unordered scan inherits unknown.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	o := properties.EstimateOrdering(filter)
	if o.IsKnown {
		t.Fatal("Filter(Scan) ordering = known, want unknown")
	}
}

func TestIsOrdered_Convenience(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	if properties.IsOrdered(scan) {
		t.Fatal("IsOrdered(Scan) = true, want false")
	}

	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	if !properties.IsOrdered(sort) {
		t.Fatal("IsOrdered(Sort) = false, want true")
	}
}

// TestEstimateOrdering_DMLInheritsInner pins that DML operations
// (Insert / Update / Delete) inherit ordering from their inner.
// Important because DML is row-pass-through — if the inner is a
// sorted scan, the DML output can be assumed sorted too.
func TestEstimateOrdering_InsertInheritsInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Source"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	ins := expressions.NewInsertExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(sort)),
		"Target",
		values.UnknownType,
	)
	o := properties.EstimateOrdering(ins)
	if !o.IsKnown {
		t.Fatal("Insert(Sort(...)) ordering = unknown, want known (DML pass-through)")
	}
}

func TestEstimateOrdering_DeleteInheritsInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	del := expressions.NewDeleteExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(sort)),
		"Order",
	)
	o := properties.EstimateOrdering(del)
	if !o.IsKnown {
		t.Fatal("Delete(Sort(...)) ordering = unknown, want known (DML pass-through)")
	}
}

func TestEstimateOrdering_Union_NotKnown(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	})
	o := properties.EstimateOrdering(union)
	if o.IsKnown {
		t.Fatal("Union ordering = known, want unknown (concat loses ordering)")
	}
}

// TestEstimateOrdering_DistinctOverSortPreserves pins that Distinct
// over Sort inherits the Sort's ordering — Distinct doesn't reorder
// rows, just drops duplicates.
func TestEstimateOrdering_DistinctOverSortPreserves(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	dist := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(expressions.InitialOf(sort)))
	o := properties.EstimateOrdering(dist)
	if !o.IsKnown {
		t.Fatal("Distinct(Sort(...)) ordering = unknown, want known (Distinct preserves)")
	}
}

// TestEstimateOrdering_DistinctOverScanNotKnown verifies that
// Distinct over an unsorted scan still produces unknown.
func TestEstimateOrdering_DistinctOverScanNotKnown(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	dist := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	o := properties.EstimateOrdering(dist)
	if o.IsKnown {
		t.Fatal("Distinct(Scan) ordering = known, want unknown (scan is unordered)")
	}
}

// TestEstimateOrdering_UniqueOverSortPreserves pins that Unique
// (PK-based dedup) preserves inner ordering — same rationale as
// Distinct.
func TestEstimateOrdering_UniqueOverSortPreserves(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.NotNullLong}},
	}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	uq := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(expressions.InitialOf(sort)))
	o := properties.EstimateOrdering(uq)
	if !o.IsKnown {
		t.Fatal("Unique(Sort(...)) ordering = unknown, want known (Unique preserves)")
	}
}
