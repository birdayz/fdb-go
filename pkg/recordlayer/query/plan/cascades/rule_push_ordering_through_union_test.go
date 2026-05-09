package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushOrderingThroughUnion_PushesIntoEachBranch(t *testing.T) {
	t.Parallel()

	// Sort(col1 ASC) -> Union(ScanA, ScanB)
	// Expect: Union(Sort(col1 ASC, ScanA), Sort(col1 ASC, ScanB))
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		unionQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUnionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newUnion, ok := yielded[0].(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("expected *LogicalUnionExpression, got %T", yielded[0])
	}
	children := newUnion.GetQuantifiers()
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}

	for i, child := range children {
		innerRef := child.GetRangesOver()
		if innerRef == nil {
			t.Fatalf("child %d has nil Reference", i)
		}
		innerSort, ok := innerRef.Get().(*expressions.LogicalSortExpression)
		if !ok {
			t.Fatalf("child %d: expected *LogicalSortExpression, got %T", i, innerRef.Get())
		}
		sortKeys := innerSort.GetSortKeys()
		if len(sortKeys) != 1 {
			t.Fatalf("child %d: expected 1 sort key, got %d", i, len(sortKeys))
		}
		fv, ok := sortKeys[0].Value.(*values.FieldValue)
		if !ok || fv.Field != "col1" {
			t.Fatalf("child %d: expected sort key col1, got %v", i, sortKeys[0].Value)
		}
		if sortKeys[0].Reverse {
			t.Fatalf("child %d: expected ASC sort key", i)
		}
	}
}

func TestPushOrderingThroughUnion_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	sort := expressions.UnsortedLogicalSortExpression(unionQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUnionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughUnion_NonUnionDoesNotFire(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, Reverse: false},
		},
		scanQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUnionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire when inner is not Union, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughUnion_ThreeBranches(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	scanC := expressions.NewFullUnorderedScanExpression([]string{"C"}, values.UnknownType)
	scanCQ := expressions.ForEachQuantifier(expressions.InitialOf(scanC))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ, scanCQ})
	unionQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
			{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, Reverse: true},
		},
		unionQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUnionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newUnion := yielded[0].(*expressions.LogicalUnionExpression)
	children := newUnion.GetQuantifiers()
	if len(children) != 3 {
		t.Fatalf("expected 3 children, got %d", len(children))
	}

	for i, child := range children {
		innerSort := child.GetRangesOver().Get().(*expressions.LogicalSortExpression)
		sortKeys := innerSort.GetSortKeys()
		if len(sortKeys) != 2 {
			t.Fatalf("child %d: expected 2 sort keys, got %d", i, len(sortKeys))
		}
	}
}

func TestPushOrderingThroughUnion_FixpointTerminates(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		unionQ,
	)
	ref := expressions.InitialOf(sort)

	progress, converged := FixpointApply([]ExpressionRule{NewPushOrderingThroughUnionRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge -- progress=%d, members=%d", progress, len(ref.Members()))
	}
}
