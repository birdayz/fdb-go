package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// filterOverUnion builds Filter([P], Union(<scans>)).
func filterOverUnion(p predicates.QueryPredicate, scanNames []string) *expressions.LogicalFilterExpression {
	qs := make([]expressions.Quantifier, 0, len(scanNames))
	for _, name := range scanNames {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		qs = append(qs, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	}
	union := expressions.NewLogicalUnionExpression(qs)
	unionQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p}, unionQ)
}

func TestPushFilterThroughUnionRule_DistributesAcrossChildren(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := filterOverUnion(pT, []string{"A", "B", "C"})
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughUnionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newUnion, ok := yielded[0].(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalUnionExpression", yielded[0])
	}
	if got := len(newUnion.GetQuantifiers()); got != 3 {
		t.Fatalf("union has %d children after push, want 3", got)
	}
	for i, q := range newUnion.GetQuantifiers() {
		filter, ok := q.GetRangesOver().Get().(*expressions.LogicalFilterExpression)
		if !ok {
			t.Errorf("union child %d = %T, want *LogicalFilterExpression", i, q.GetRangesOver().Get())
			continue
		}
		if got := filter.GetPredicates(); len(got) != 1 || got[0] != pT {
			t.Errorf("child %d filter predicates wrong: got %v", i, got)
		}
	}
}

func TestPushFilterThroughUnionRule_DeclinesOnNonUnionInner(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughUnionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Union inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughUnionRule_DeclinesOnEmptyUnion(t *testing.T) {
	t.Parallel()
	// Empty Union (no children) — distribution is a no-op (still
	// empty Union of zero filtered operands). Decline.
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	emptyUnion := expressions.NewLogicalUnionExpression(nil)
	unionQ := expressions.ForEachQuantifier(expressions.InitialOf(emptyUnion))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, unionQ)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughUnionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on empty Union inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughUnionRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := filterOverUnion(pT, []string{"A", "B"})
	ref := expressions.InitialOf(src)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewPushFilterThroughUnionRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
