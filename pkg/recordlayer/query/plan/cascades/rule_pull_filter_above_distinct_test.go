package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustPullFilterDistinctConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct pull-filter/distinct fixture: " + err.Error())
	}
	return value
}

func pullFilterDistinctScan() *expressions.FullUnorderedScanExpression {
	row := values.NewRecordType("PullFilterDistinctRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	return mustPullFilterDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, row))
}

func pullFilterDistinctShape(
	predicate predicates.QueryPredicate,
) (*expressions.LogicalDistinctExpression, *expressions.Reference) {
	scan := pullFilterDistinctScan()
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := mustPullFilterDistinctConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicate}, scanQ))
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	distinct := mustPullFilterDistinctConstruct(expressions.NewLogicalDistinctExpression(innerFQ))
	return distinct, expressions.InitialOf(distinct)
}

func firePullFilterDistinctRule(
	t testing.TB, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(NewPullFilterAboveDistinctRule(), ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func explorePullFilterDistinct(p *Planner, rootRef *expressions.Reference) (int, bool) {
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

func TestPullFilterAboveDistinctRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	_, ref := pullFilterDistinctShape(pT)
	yielded := firePullFilterDistinctRule(t, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newF, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalFilterExpression", yielded[0])
	}
	if got := newF.GetPredicates(); len(got) != 1 || got[0] != pT {
		t.Fatalf("filter predicates wrong: got %v", got)
	}
	innerD, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("filter inner = %T, want *LogicalDistinctExpression", newF.GetInner().GetRangesOver().Get())
	}
	if _, scanOK := innerD.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !scanOK {
		t.Fatalf("distinct inner = %T, want Scan", innerD.GetInner().GetRangesOver().Get())
	}
}

func TestPullFilterAboveDistinctRule_DeclinesOnNonFilterInner(t *testing.T) {
	t.Parallel()
	scan := pullFilterDistinctScan()
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := mustPullFilterDistinctConstruct(expressions.NewLogicalDistinctExpression(q))
	ref := expressions.InitialOf(src)
	yielded := firePullFilterDistinctRule(t, ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Filter inner, want 0", len(yielded))
	}
}

func TestPullFilterAboveDistinctRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	_, ref := pullFilterDistinctShape(pT)
	progress, converged := explorePullFilterDistinct(
		NewPlanner([]ExpressionRule{NewPullFilterAboveDistinctRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
