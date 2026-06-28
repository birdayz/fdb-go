package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestEvaluateExpressionCount_Nil(t *testing.T) {
	t.Parallel()
	if got := EvaluateExpressionCount(nil, nil); got != 0 {
		t.Fatalf("EvaluateExpressionCount(nil) = %d, want 0", got)
	}
}

func TestEvaluateExpressionCount_SingleLeaf(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	if got := EvaluateExpressionCount(scan, nil); got != 1 {
		t.Fatalf("EvaluateExpressionCount(leaf) = %d, want 1", got)
	}
}

func TestEvaluateExpressionCount_WithFilter(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	filter := expressions.NewLogicalFilterExpression(nil, inner)

	// Count all: should be 2 (filter + scan).
	all := EvaluateExpressionCount(filter, nil)
	if all != 2 {
		t.Fatalf("count all = %d, want 2", all)
	}

	// Count only FullUnorderedScan.
	onlyScan := EvaluateExpressionCount(filter, func(e expressions.RelationalExpression) bool {
		_, ok := e.(*expressions.FullUnorderedScanExpression)
		return ok
	})
	if onlyScan != 1 {
		t.Fatalf("count scans = %d, want 1", onlyScan)
	}

	// Count only LogicalFilter.
	onlyFilter := EvaluateExpressionCount(filter, func(e expressions.RelationalExpression) bool {
		_, ok := e.(*expressions.LogicalFilterExpression)
		return ok
	})
	if onlyFilter != 1 {
		t.Fatalf("count filters = %d, want 1", onlyFilter)
	}
}

func TestEvaluateExpressionCount_DeepTree(t *testing.T) {
	t.Parallel()
	// scan -> filter -> projection -> sort = 4 nodes
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref1 := expressions.InitialOf(scan)
	inner1 := expressions.ForEachQuantifier(ref1)
	filter := expressions.NewLogicalFilterExpression(nil, inner1)
	ref2 := expressions.InitialOf(filter)
	inner2 := expressions.ForEachQuantifier(ref2)
	proj := expressions.NewLogicalProjectionExpression([]values.Value{&values.FieldValue{Field: "x"}}, inner2)
	ref3 := expressions.InitialOf(proj)
	inner3 := expressions.ForEachQuantifier(ref3)
	sort := expressions.NewLogicalSortExpression([]expressions.SortKey{{Value: &values.FieldValue{Field: "x"}}}, inner3)

	if got := EvaluateExpressionCount(sort, nil); got != 4 {
		t.Fatalf("count deep tree = %d, want 4", got)
	}
}
