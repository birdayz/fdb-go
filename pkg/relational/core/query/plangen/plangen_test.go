package plangen_test

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/plangen"
)

func TestConvert_Nil(t *testing.T) {
	t.Parallel()
	_, err := plangen.Convert(nil)
	if err == nil {
		t.Fatal("expected error on nil input")
	}
}

func TestConvert_Scan(t *testing.T) {
	t.Parallel()
	src := logical.NewScan("Order", "")
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	scan, ok := got.(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
	}
	if names := scan.GetRecordTypes(); len(names) != 1 || names[0] != "Order" {
		t.Fatalf("record types = %v, want [Order]", names)
	}
}

func TestConvert_FilterOverScan(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewFilterWithPredicate(logical.NewScan("Order", ""), pT, "TRUE")
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	f, ok := got.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression", got)
	}
	if got := f.GetPredicates(); len(got) != 1 {
		t.Fatalf("predicate count = %d, want 1", len(got))
	}
	// Inner should be the converted Scan.
	innerExpr := f.GetInner().GetRangesOver().Get()
	if _, ok := innerExpr.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner = %T, want *FullUnorderedScanExpression", innerExpr)
	}
}

func TestConvert_FilterTextOnly_Unsupported(t *testing.T) {
	t.Parallel()
	// Text-only filter (no QueryPredicate) is the legacy non-catalog path.
	src := logical.NewFilter(logical.NewScan("Order", ""), "x > 5")
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

func TestConvert_Union(t *testing.T) {
	t.Parallel()
	a := logical.NewScan("A", "")
	b := logical.NewScan("B", "")
	src := logical.NewUnion([]logical.LogicalOperator{a, b}, false)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	u, ok := got.(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalUnionExpression", got)
	}
	if len(u.GetQuantifiers()) != 2 {
		t.Fatalf("union has %d children, want 2", len(u.GetQuantifiers()))
	}
}

func TestConvert_Delete(t *testing.T) {
	t.Parallel()
	src := logical.NewDelete("Order", logical.NewScan("Order", ""))
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	d, ok := got.(*expressions.DeleteExpression)
	if !ok {
		t.Fatalf("got %T, want *DeleteExpression", got)
	}
	if d.GetTargetRecordType() != "Order" {
		t.Fatalf("target = %q, want Order", d.GetTargetRecordType())
	}
}

func TestConvert_Insert(t *testing.T) {
	t.Parallel()
	src := logical.NewInsert("Order", []string{"id"}, logical.NewScan("OrderSource", ""))
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ins, ok := got.(*expressions.InsertExpression)
	if !ok {
		t.Fatalf("got %T, want *InsertExpression", got)
	}
	if ins.GetTargetRecordType() != "Order" {
		t.Fatalf("target = %q, want Order", ins.GetTargetRecordType())
	}
}

func TestConvert_Insert_NoSource_Unsupported(t *testing.T) {
	t.Parallel()
	src := logical.NewInsert("Order", []string{"id"}, nil)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (no Source)", err)
	}
}

func TestConvert_Project_Unsupported(t *testing.T) {
	t.Parallel()
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"id", "name"},
		[]string{"", ""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (Project text→Value parsing not yet wired)", err)
	}
}

// TestConvert_NestedFilterOverFilter — proves recursion through
// the converter walks correctly.
func TestConvert_NestedFilterOverFilter(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	inner := logical.NewFilterWithPredicate(logical.NewScan("Order", ""), pT, "TRUE")
	outer := logical.NewFilterWithPredicate(inner, pF, "FALSE")
	got, err := plangen.Convert(outer)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	outerF, ok := got.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression", got)
	}
	innerExpr := outerF.GetInner().GetRangesOver().Get()
	innerF, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("inner = %T, want *LogicalFilterExpression", innerExpr)
	}
	scanExpr := innerF.GetInner().GetRangesOver().Get()
	if _, ok := scanExpr.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("scan = %T, want *FullUnorderedScanExpression", scanExpr)
	}
}
