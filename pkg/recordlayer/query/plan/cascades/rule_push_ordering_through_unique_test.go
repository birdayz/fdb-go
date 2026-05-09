package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushOrderingThroughUnique_SortKeysPassThrough(t *testing.T) {
	t.Parallel()

	// Sort(col1 ASC) -> Unique(Scan)
	// Expect: Unique(Sort(col1 ASC, Scan))
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	unique := expressions.NewLogicalUniqueExpression(scanQ)
	uniqueQ := expressions.ForEachQuantifier(expressions.InitialOf(unique))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		uniqueQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUniqueRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newUnique, ok := yielded[0].(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("expected *LogicalUniqueExpression, got %T", yielded[0])
	}
	innerSort, ok := newUnique.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below Unique, got %T", newUnique.GetInner().GetRangesOver().Get())
	}
	sortKeys := innerSort.GetSortKeys()
	if len(sortKeys) != 1 {
		t.Fatalf("expected 1 sort key, got %d", len(sortKeys))
	}
	fv, ok := sortKeys[0].Value.(*values.FieldValue)
	if !ok || fv.Field != "col1" {
		t.Fatalf("expected sort key col1, got %v", sortKeys[0].Value)
	}
	if sortKeys[0].Reverse {
		t.Fatal("expected ASC sort key")
	}
}

func TestPushOrderingThroughUnique_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	unique := expressions.NewLogicalUniqueExpression(scanQ)
	uniqueQ := expressions.ForEachQuantifier(expressions.InitialOf(unique))
	sort := expressions.UnsortedLogicalSortExpression(uniqueQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUniqueRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughUnique_NonUniqueDoesNotFire(t *testing.T) {
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

	yielded := FireExpressionRule(NewPushOrderingThroughUniqueRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire when inner is not Unique, but yielded %d", len(yielded))
	}
}

func TestPushOrderingThroughUnique_DescPreserved(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	unique := expressions.NewLogicalUniqueExpression(scanQ)
	uniqueQ := expressions.ForEachQuantifier(expressions.InitialOf(unique))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: true},
		},
		uniqueQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUniqueRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	innerSort := yielded[0].(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !innerSort.GetSortKeys()[0].Reverse {
		t.Fatal("sort direction should be DESC (preserved from original)")
	}
}

func TestPushOrderingThroughUnique_FixpointTerminates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	unique := expressions.NewLogicalUniqueExpression(scanQ)
	uniqueQ := expressions.ForEachQuantifier(expressions.InitialOf(unique))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		uniqueQ,
	)
	ref := expressions.InitialOf(sort)

	progress, converged := FixpointApply([]ExpressionRule{NewPushOrderingThroughUniqueRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge -- progress=%d, members=%d", progress, len(ref.Members()))
	}
}
