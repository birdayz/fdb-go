package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func filterOverTypeFilter(t testing.TB) *expressions.LogicalFilterExpression {
	t.Helper()
	scan := mustPushFilterScan(t, []string{"Order", "Customer"})
	scanQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, scan))
	typeFilter := mustPushFilterTypeFilter(t, []string{"Order"}, scanQ)
	typeFilterQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, typeFilter))
	predicate := pushFilterComparison(t, mustPushFilterFlowed(t, typeFilterQ), "ID",
		predicates.ComparisonGreaterThan, int64(0))
	return mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{predicate}, typeFilterQ)
}

func TestPushFilterThroughTypeFilterRule_Fires(t *testing.T) {
	t.Parallel()
	source := filterOverTypeFilter(t)
	yielded := mustFireExpressionRule(t, NewPushFilterThroughTypeFilterRule(),
		mustPushFilterInitial(t, source))
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newTypeFilter, ok := yielded[0].(*expressions.LogicalTypeFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalTypeFilterExpression", yielded[0])
	}
	if got := newTypeFilter.GetRecordTypes(); len(got) != 1 || got[0] != "Order" {
		t.Fatalf("rewritten record types = %v, want [Order]", got)
	}
	innerFilter, ok := newTypeFilter.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("type-filter inner = %T, want *LogicalFilterExpression", newTypeFilter.GetInner().GetRangesOver().Get())
	}
	got := innerFilter.GetPredicates()
	if len(got) != 1 {
		t.Fatalf("pushed predicates = %d, want 1", len(got))
	}
	requirePushFilterComparison(t, got[0], []string{"ID"},
		predicates.ComparisonGreaterThan, int64(0))
	requirePushFilterPredicateAlias(t, got[0], innerFilter.GetInner().GetAlias())
	if _, ok := innerFilter.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner = %T, want Scan", innerFilter.GetInner().GetRangesOver().Get())
	}
}

func TestPushFilterThroughTypeFilterRule_DeclinesOnNonTypeFilterInner(t *testing.T) {
	t.Parallel()
	scan := mustPushFilterScan(t, []string{"T"})
	quantifier := expressions.ForEachQuantifier(mustPushFilterInitial(t, scan))
	predicate := pushFilterComparison(t, mustPushFilterFlowed(t, quantifier), "ID",
		predicates.ComparisonGreaterThan, int64(0))
	source := mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{predicate}, quantifier)
	yielded := mustFireExpressionRule(t, NewPushFilterThroughTypeFilterRule(),
		mustPushFilterInitial(t, source))
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-TypeFilter inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughTypeFilterRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	source := filterOverTypeFilter(t)
	reference := mustPushFilterInitial(t, source)
	progress, converged := explorePushFilterRewriting(
		NewPlanner([]ExpressionRule{NewPushFilterThroughTypeFilterRule()}, nil), reference)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(reference.Members()))
	}
}
