package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPullCommonFilterAboveUnionRule_CommonPredicate_Pulls(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	mkChild := func(name string) expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
		return expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	src := expressions.NewLogicalUnionExpression(
		[]expressions.Quantifier{mkChild("A"), mkChild("B"), mkChild("C")},
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullCommonFilterAboveUnionRule(), ref)
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
	newUnion, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("filter inner = %T, want *LogicalUnionExpression", newF.GetInner().GetRangesOver().Get())
	}
	if got := len(newUnion.GetQuantifiers()); got != 3 {
		t.Fatalf("union has %d children, want 3", got)
	}
}

func TestPullCommonFilterAboveUnionRule_DifferentPredicates_NoFire(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	mkChild := func(name string, p predicates.QueryPredicate) expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p}, scanQ)
		return expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	src := expressions.NewLogicalUnionExpression(
		[]expressions.Quantifier{mkChild("A", pT), mkChild("B", pF)},
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullCommonFilterAboveUnionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on different-predicate union children, want 0", len(yielded))
	}
}

func TestPullCommonFilterAboveUnionRule_NonFilterChild_NoFire(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	mkFilteredChild := func() expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
		return expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	mkBareScan := func() expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
		return expressions.ForEachQuantifier(expressions.InitialOf(scan))
	}
	src := expressions.NewLogicalUnionExpression(
		[]expressions.Quantifier{mkFilteredChild(), mkBareScan()},
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullCommonFilterAboveUnionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d when one child is non-Filter, want 0", len(yielded))
	}
}

func TestPullCommonFilterAboveUnionRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	mkChild := func(name string) expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
		return expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	src := expressions.NewLogicalUnionExpression(
		[]expressions.Quantifier{mkChild("A"), mkChild("B")},
	)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPullCommonFilterAboveUnionRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
