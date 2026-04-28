package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
