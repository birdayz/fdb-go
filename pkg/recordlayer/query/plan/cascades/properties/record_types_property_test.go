package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

func TestEvaluateRecordTypes_Nil(t *testing.T) {
	t.Parallel()
	got := EvaluateRecordTypes(nil)
	if got != nil {
		t.Fatalf("EvaluateRecordTypes(nil) = %v, want nil", got)
	}
}

func TestEvaluateRecordTypes_FullScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Customer", "Order"}, nil)
	got := EvaluateRecordTypes(scan)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if _, ok := got["Customer"]; !ok {
		t.Fatal("missing Customer")
	}
	if _, ok := got["Order"]; !ok {
		t.Fatal("missing Order")
	}
}

func TestEvaluateRecordTypes_TypeFilter(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Customer", "Order"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Customer"}, inner)
	got := EvaluateRecordTypes(tf)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if _, ok := got["Customer"]; !ok {
		t.Fatal("missing Customer")
	}
}

func TestEvaluateRecordTypes_FilterPreservesChild(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Customer"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	filter := expressions.NewLogicalFilterExpression(nil, inner)
	got := EvaluateRecordTypes(filter)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if _, ok := got["Customer"]; !ok {
		t.Fatal("missing Customer")
	}
}

func TestEvaluateRecordTypes_UnionMultipleTypes(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)
	ref1 := expressions.InitialOf(scan1)
	ref2 := expressions.InitialOf(scan2)
	q1 := expressions.ForEachQuantifier(ref1)
	q2 := expressions.ForEachQuantifier(ref2)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{q1, q2})
	got := EvaluateRecordTypes(union)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if _, ok := got["A"]; !ok {
		t.Fatal("missing A")
	}
	if _, ok := got["B"]; !ok {
		t.Fatal("missing B")
	}
}
