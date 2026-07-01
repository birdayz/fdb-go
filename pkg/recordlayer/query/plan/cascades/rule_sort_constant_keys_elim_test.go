package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSortConstantKeysElimRule_AllConstant_Eliminates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.ConstantValue{Value: int64(42), Typ: values.TypeUnknown}},
		{Value: &values.ConstantValue{Value: "x", Typ: values.TypeUnknown}},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewSortConstantKeysElimRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("yielded %T, want *FullUnorderedScanExpression (sort eliminated)", yielded[0])
	}
}

func TestSortConstantKeysElimRule_OneNonConstantKey_NoFire(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.ConstantValue{Value: int64(42), Typ: values.TypeUnknown}},
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}}, // NOT constant
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewSortConstantKeysElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d when one key is non-constant, want 0", len(yielded))
	}
}

func TestSortConstantKeysElimRule_EmptyKeys_NoFire(t *testing.T) {
	t.Parallel()
	// Empty keys = Unsorted; UnsortedSortElim's territory. This rule
	// declines.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.UnsortedLogicalSortExpression(q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewSortConstantKeysElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on empty keys, want 0", len(yielded))
	}
}

func TestSortConstantKeysElimRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.ConstantValue{Value: int64(1), Typ: values.TypeUnknown}},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewSortConstantKeysElimRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
