package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestFilterDropTruePredicatesRule_DropsOne(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	f := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT, pF, pT}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rule := NewFilterDropTruePredicatesRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalFilterExpression)
	got := merged.GetPredicates()
	if len(got) != 1 {
		t.Fatalf("predicate count after drop=%d, want 1", len(got))
	}
	if cp, ok := got[0].(*predicates.ConstantPredicate); !ok || cp.Value != predicates.TriFalse {
		t.Fatalf("retained predicate is not TriFalse: %T %v", got[0], got[0])
	}
}

func TestFilterDropTruePredicatesRule_DeclinesNoTrue(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	pU := predicates.NewConstantPredicate(predicates.TriUnknown)
	f := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pF, pU}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rule := NewFilterDropTruePredicatesRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired despite no TriTrue predicate — yielded %d, want 0", len(yielded))
	}
}

func TestFilterDropTruePredicatesRule_DropsAll(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT, pT}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rule := NewFilterDropTruePredicatesRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalFilterExpression)
	if got := merged.GetPredicates(); len(got) != 0 {
		t.Fatalf("after dropping all TriTrue, predicates=%d, want 0 (NoOpFilterRule will eliminate this)", len(got))
	}
}

func TestFilterDropTruePredicatesRule_ComposesWithNoOpFilter(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pT, pT}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rules := []ExpressionRule{
		NewFilterDropTruePredicatesRule(),
		NewNoOpFilterRule(),
	}
	progress, converged := FixpointApply(rules, ref, 10)
	if !converged {
		t.Fatal("did not converge")
	}
	if progress < 2 {
		t.Fatalf("progress=%d, want at least 2 (DropTrue + NoOp)", progress)
	}
	// After both rules run, the Reference should contain a Scan member.
	foundScan := false
	for _, m := range ref.Members() {
		if _, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			foundScan = true
			break
		}
	}
	if !foundScan {
		t.Fatal("after FilterDropTrue + NoOpFilter, Reference has no Scan member")
	}
}
