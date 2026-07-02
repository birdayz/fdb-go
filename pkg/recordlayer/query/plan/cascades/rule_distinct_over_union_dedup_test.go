package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// distinctOverUnion builds Distinct(Union(<scans>)).
func distinctOverUnion(scanNames []string) *expressions.LogicalDistinctExpression {
	qs := make([]expressions.Quantifier, 0, len(scanNames))
	for _, name := range scanNames {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		qs = append(qs, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	}
	union := expressions.NewLogicalUnionExpression(qs)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(union))
	return expressions.NewLogicalDistinctExpression(innerQ)
}

func TestDistinctOverUnionDedupRule_RemovesEquivalentSibling(t *testing.T) {
	t.Parallel()
	d := distinctOverUnion([]string{"A", "B", "A"})
	ref := expressions.InitialOf(d)
	yielded := FireExpressionRule(NewDistinctOverUnionDedupRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newDistinct, ok := yielded[0].(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalDistinctExpression", yielded[0])
	}
	newUnion, ok := newDistinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("rewritten inner = %T, want *LogicalUnionExpression", newDistinct.GetInner().GetRangesOver().Get())
	}
	if got := len(newUnion.GetQuantifiers()); got != 2 {
		t.Fatalf("union has %d children after dedup, want 2", got)
	}
}

func TestDistinctOverUnionDedupRule_AllUnique_NoFire(t *testing.T) {
	t.Parallel()
	d := distinctOverUnion([]string{"A", "B", "C"})
	ref := expressions.InitialOf(d)
	yielded := FireExpressionRule(NewDistinctOverUnionDedupRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on all-unique union, want 0", len(yielded))
	}
}

func TestDistinctOverUnionDedupRule_DeclinesOnNonUnionInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	d := expressions.NewLogicalDistinctExpression(innerQ)
	ref := expressions.InitialOf(d)
	yielded := FireExpressionRule(NewDistinctOverUnionDedupRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Union inner, want 0", len(yielded))
	}
}

func TestDistinctOverUnionDedupRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	d := distinctOverUnion([]string{"A", "B", "A"})
	ref := expressions.InitialOf(d)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewDistinctOverUnionDedupRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
