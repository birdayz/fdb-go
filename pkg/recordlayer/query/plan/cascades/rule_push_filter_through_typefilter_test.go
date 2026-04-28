package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushFilterThroughTypeFilterRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, scanQ)
	tfQ := expressions.ForEachQuantifier(expressions.InitialOf(tf))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, tfQ)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughTypeFilterRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newTF, ok := yielded[0].(*expressions.LogicalTypeFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalTypeFilterExpression", yielded[0])
	}
	if got := newTF.GetRecordTypes(); len(got) != 1 || got[0] != "Order" {
		t.Fatalf("rewritten record types = %v, want [Order]", got)
	}
	innerFilter, ok := newTF.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("type-filter inner = %T, want *LogicalFilterExpression", newTF.GetInner().GetRangesOver().Get())
	}
	if _, ok := innerFilter.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner = %T, want Scan", innerFilter.GetInner().GetRangesOver().Get())
	}
}

func TestPushFilterThroughTypeFilterRule_DeclinesOnNonTypeFilterInner(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughTypeFilterRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-TypeFilter inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughTypeFilterRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, scanQ)
	tfQ := expressions.ForEachQuantifier(expressions.InitialOf(tf))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, tfQ)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPushFilterThroughTypeFilterRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
