package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushTypeFilterBelowFilterRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, innerFQ)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushTypeFilterBelowFilterRule(), ref)
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
	innerTF, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalTypeFilterExpression)
	if !ok {
		t.Fatalf("filter inner = %T, want *LogicalTypeFilterExpression", newF.GetInner().GetRangesOver().Get())
	}
	if got := innerTF.GetRecordTypes(); len(got) != 1 || got[0] != "Order" {
		t.Fatalf("type-filter record types = %v, want [Order]", got)
	}
}

func TestPushTypeFilterBelowFilterRule_DeclinesOnNonFilterInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushTypeFilterBelowFilterRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Filter inner, want 0", len(yielded))
	}
}

func TestPushTypeFilterBelowFilterRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, innerFQ)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPushTypeFilterBelowFilterRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
