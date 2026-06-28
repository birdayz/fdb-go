package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFilterDedupPredicatesRule_RemovesDuplicate(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT, pF, pT}, q,
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewFilterDedupPredicatesRule(), ref)
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
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT, pF}, q,
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewFilterDedupPredicatesRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on all-unique predicates, want 0", len(yielded))
	}
}

func TestFilterDedupPredicatesRule_CooperatesWithFilterMerge(t *testing.T) {
	t.Parallel()
	// Filter([P], Filter([P, Q], X)) — two filters share predicate P.
	// FilterMergeRule yields Filter([P, P, Q], X), then
	// FilterDedupPredicatesRule yields Filter([P, Q], X). Pin the
	// two-rule chain through FixpointApply.
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT, pF}, scanQ,
	)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	outerF := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT}, innerFQ,
	)
	ref := expressions.InitialOf(outerF)
	rules := []ExpressionRule{
		NewFilterMergeRule(),
		NewFilterDedupPredicatesRule(),
	}
	progress, converged := FixpointApply(rules, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d", progress)
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
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT, pT, pT}, q,
	)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewFilterDedupPredicatesRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
