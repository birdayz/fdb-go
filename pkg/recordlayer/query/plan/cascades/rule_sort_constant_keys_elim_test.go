package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func sortRewriteRowType() *values.RecordType {
	names := []string{"a", "b", "id", "name", "k1", "k2", "k3"}
	fields := make([]values.Field, len(names))
	for i, name := range names {
		fields[i] = values.Field{Name: name, FieldType: values.NullableLong}
	}
	return values.NewRecordType("SortRewriteRow", false, fields)
}

func mustSortRewriteConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct sort-rewrite fixture: " + err.Error())
	}
	return value
}

func sortRewriteScan() *expressions.FullUnorderedScanExpression {
	return mustSortRewriteConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, sortRewriteRowType()))
}

func sortRewriteField(q expressions.Quantifier, ordinal int) values.Value {
	root := mustSortRewriteConstruct(q.RequireFlowedObjectValue())
	return mustSortRewriteConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func sortRewriteSort(
	keys []expressions.SortKey, q expressions.Quantifier,
) *expressions.LogicalSortExpression {
	return mustSortRewriteConstruct(expressions.NewLogicalSortExpression(keys, q))
}

func fireSortRewriteRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func exploreSortRewriting(p *Planner, rootRef *expressions.Reference) (int, bool) {
	if p.memo == nil {
		p.memo = NewMemo(rootRef)
	}
	if p.constraintMap == nil {
		p.constraintMap = NewConstraintMap()
	}
	if p.dataAccessConsumed == nil {
		p.dataAccessConsumed = make(map[*expressions.Reference]int)
	}
	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	for len(p.stack) > 0 {
		if p.tasksRun >= p.MaxTasks {
			return p.tasksRun, false
		}
		p.pop().Run(context.Background(), p)
		p.tasksRun++
	}
	return p.tasksRun, true
}

func sortLong(v int64) values.Value {
	return &values.ConstantValue{Value: v, Typ: values.NotNullLong}
}

func sortString(v string) values.Value {
	return &values.ConstantValue{Value: v, Typ: values.NotNullString}
}

func TestSortConstantKeysElimRule_AllConstant_Eliminates(t *testing.T) {
	t.Parallel()
	scan := sortRewriteScan()
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: sortLong(42)},
		{Value: sortString("x")},
	}
	src := sortRewriteSort(keys, q)
	ref := expressions.InitialOf(src)
	yielded := fireSortRewriteRule(t, NewSortConstantKeysElimRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("yielded %T, want *FullUnorderedScanExpression (sort eliminated)", yielded[0])
	}
}

func TestSortConstantKeysElimRule_OneNonConstantKey_NoFire(t *testing.T) {
	t.Parallel()
	scan := sortRewriteScan()
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: sortLong(42)},
		{Value: sortRewriteField(q, 2)}, // NOT constant
	}
	src := sortRewriteSort(keys, q)
	ref := expressions.InitialOf(src)
	yielded := fireSortRewriteRule(t, NewSortConstantKeysElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d when one key is non-constant, want 0", len(yielded))
	}
}

func TestSortConstantKeysElimRule_EmptyKeys_NoFire(t *testing.T) {
	t.Parallel()
	// Empty keys = Unsorted; UnsortedSortElim's territory. This rule
	// declines.
	scan := sortRewriteScan()
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := mustSortRewriteConstruct(expressions.UnsortedLogicalSortExpression(q))
	ref := expressions.InitialOf(src)
	yielded := fireSortRewriteRule(t, NewSortConstantKeysElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on empty keys, want 0", len(yielded))
	}
}

func TestSortConstantKeysElimRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	scan := sortRewriteScan()
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: sortLong(1)},
	}
	src := sortRewriteSort(keys, q)
	ref := expressions.InitialOf(src)
	progress, converged := exploreSortRewriting(NewPlanner([]ExpressionRule{NewSortConstantKeysElimRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
