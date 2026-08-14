package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func filterRuleRowType() *values.RecordType {
	return values.NewRecordType("FilterRuleRow", false, []values.Field{
		{Name: "a", FieldType: values.NullableLong},
		{Name: "b", FieldType: values.NullableLong},
		{Name: "c", FieldType: values.NullableLong},
		{Name: "x", FieldType: values.NullableLong},
	})
}

func mustFilterConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct filter-rule fixture: " + err.Error())
	}
	return value
}

func filterRuleScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustFilterConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, filterRuleRowType()))
}

func filterRuleFilter(
	preds []predicates.QueryPredicate, inner expressions.Quantifier,
) *expressions.LogicalFilterExpression {
	return mustFilterConstruct(expressions.NewLogicalFilterExpression(preds, inner))
}

func fireFilterRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func exploreFilterRewriting(p *Planner, rootRef *expressions.Reference) (int, bool) {
	if rootRef == nil {
		return 0, true
	}
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

func TestFilterDedupPredicatesRule_RemovesDuplicate(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	scan := filterRuleScan("T")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := filterRuleFilter(
		[]predicates.QueryPredicate{pT, pF, pT}, q,
	)
	ref := expressions.InitialOf(src)
	yielded := fireFilterRule(t, NewFilterDedupPredicatesRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newF, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalFilterExpression", yielded[0])
	}
	got := newF.GetPredicates()
	if len(got) != 2 {
		t.Fatalf("deduped predicates len=%d, want 2", len(got))
	}
}

func TestFilterDedupPredicatesRule_AllUnique_NoFire(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	scan := filterRuleScan("T")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := filterRuleFilter(
		[]predicates.QueryPredicate{pT, pF}, q,
	)
	ref := expressions.InitialOf(src)
	yielded := fireFilterRule(t, NewFilterDedupPredicatesRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on all-unique predicates, want 0", len(yielded))
	}
}

func TestFilterDedupPredicatesRule_CooperatesWithFilterMerge(t *testing.T) {
	t.Parallel()
	// Filter([P], Filter([P, Q], X)) — two filters share predicate P.
	// FilterMergeRule yields Filter([P, P, Q], X), then
	// FilterDedupPredicatesRule yields Filter([P, Q], X). Pin the
	// two-rule chain through the unified exploration driver.
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	scan := filterRuleScan("T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := filterRuleFilter(
		[]predicates.QueryPredicate{pT, pF}, scanQ,
	)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	outerF := filterRuleFilter(
		[]predicates.QueryPredicate{pT}, innerFQ,
	)
	ref := expressions.InitialOf(outerF)
	rules := []ExpressionRule{
		NewFilterMergeRule(),
		NewFilterDedupPredicatesRule(),
	}
	progress, converged := exploreFilterRewriting(NewPlanner(rules, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d", progress)
	}
	// Look for a Filter([P, F], Scan) member — the deduped form.
	foundDeduped := false
	for _, m := range ref.Members() {
		f, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		ps := f.GetPredicates()
		if len(ps) != 2 {
			continue
		}
		if _, scanOK := f.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); scanOK {
			foundDeduped = true
			break
		}
	}
	if !foundDeduped {
		t.Fatalf("FilterMerge + FilterDedupPredicates didn't reach Filter([P, F], Scan); members=%d", len(ref.Members()))
	}
}

func TestFilterDedupPredicatesRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := filterRuleScan("T")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := filterRuleFilter(
		[]predicates.QueryPredicate{pT, pT, pT}, q,
	)
	ref := expressions.InitialOf(src)
	progress, converged := exploreFilterRewriting(NewPlanner([]ExpressionRule{NewFilterDedupPredicatesRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
