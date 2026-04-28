package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestNoOpFilterRule_FiresOnEmptyPredicates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	f := expressions.NewLogicalFilterExpression(nil, scanQ)
	ref := expressions.InitialOf(f)

	rule := NewNoOpFilterRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("yielded type=%T, want *FullUnorderedScanExpression", yielded[0])
	}
}

func TestNoOpFilterRule_FiresOnAllTrue(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT, pT, pT}, scanQ)
	ref := expressions.InitialOf(f)
	rule := NewNoOpFilterRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
}

func TestNoOpFilterRule_DeclinesOnFalse(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pF}, scanQ)
	ref := expressions.InitialOf(f)
	rule := NewNoOpFilterRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on FALSE filter — yielded %d, want 0 (FALSE filters select no rows)", len(yielded))
	}
}

func TestNoOpFilterRule_DeclinesOnUnknown(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pU := predicates.NewConstantPredicate(predicates.TriUnknown)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pU}, scanQ)
	ref := expressions.InitialOf(f)
	rule := NewNoOpFilterRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on UNKNOWN filter — yielded %d, want 0 (UNKNOWN treated as FALSE for SELECT)", len(yielded))
	}
}

func TestNoOpFilterRule_DeclinesOnNonConstant(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	cmp := predicates.NewComparisonPredicate(
		values.NewBooleanValue(true),
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{cmp}, scanQ)
	ref := expressions.InitialOf(f)
	rule := NewNoOpFilterRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a non-constant filter — yielded %d, want 0", len(yielded))
	}
}

func TestNoOpFilterRule_DeclinesOnMixedPredicates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT, pF}, scanQ)
	ref := expressions.InitialOf(f)
	rule := NewNoOpFilterRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on mixed-truth-value filter — yielded %d, want 0", len(yielded))
	}
}
