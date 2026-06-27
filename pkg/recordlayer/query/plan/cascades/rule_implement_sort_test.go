package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestImplementSortRule_GetRequestedOrderings(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
		{Value: &values.FieldValue{Field: "name", Typ: values.UnknownType}, Reverse: true},
	}
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	rule := NewImplementSortRule()
	orderings := rule.GetRequestedOrderings(sort)
	if len(orderings) != 1 {
		t.Fatalf("expected 1 ordering, got %d", len(orderings))
	}
	parts := orderings[0].GetParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatal("first key should be ascending")
	}
	if parts[1].SortOrder != RequestedSortOrderDescending {
		t.Fatal("second key should be descending")
	}
}

func TestImplementSortRule_PreserveOrdering(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	sort := expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	rule := NewImplementSortRule()
	orderings := rule.GetRequestedOrderings(sort)
	if len(orderings) != 1 {
		t.Fatalf("expected 1 ordering, got %d", len(orderings))
	}
	if !orderings[0].IsPreserve() {
		t.Fatal("empty sort keys should produce preserve ordering")
	}
}

func TestSortExpressionToRequestedOrdering(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
	}
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(expressions.InitialOf(scan)))

	req := sortExpressionToRequestedOrdering(sort)
	if req.Size() != 1 {
		t.Fatalf("expected 1 part, got %d", req.Size())
	}
	if req.GetParts()[0].SortOrder != RequestedSortOrderAscending {
		t.Fatal("expected ascending")
	}
}
