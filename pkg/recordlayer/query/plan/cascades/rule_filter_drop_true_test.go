package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestFilterDropTruePredicatesRule_DropsOne(t *testing.T) {
	t.Parallel()
	scan := filterRuleScan("T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	f := filterRuleFilter(
		[]predicates.QueryPredicate{pT, pF, pT}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rule := NewFilterDropTruePredicatesRule()
	yielded := fireFilterRule(t, rule, ref)
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
	scan := filterRuleScan("T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	pU := predicates.NewConstantPredicate(predicates.TriUnknown)
	f := filterRuleFilter(
		[]predicates.QueryPredicate{pF, pU}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rule := NewFilterDropTruePredicatesRule()
	yielded := fireFilterRule(t, rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired despite no TriTrue predicate — yielded %d, want 0", len(yielded))
	}
}

func TestFilterDropTruePredicatesRule_DropsAll(t *testing.T) {
	t.Parallel()
	scan := filterRuleScan("T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := filterRuleFilter(
		[]predicates.QueryPredicate{pT, pT}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rule := NewFilterDropTruePredicatesRule()
	yielded := fireFilterRule(t, rule, ref)
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
	scan := filterRuleScan("T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := filterRuleFilter(
		[]predicates.QueryPredicate{pT, pT}, scanQ,
	)
	ref := expressions.InitialOf(f)
	rules := []ExpressionRule{
		NewFilterDropTruePredicatesRule(),
		NewNoOpFilterRule(),
	}
	if _, converged := exploreFilterRewriting(NewPlanner(rules, nil), ref); !converged {
		t.Fatal("did not converge")
	}
	// The Scan member below requires the full DropTrue → NoOp chain
	// (two dependent rule fires), so finding it pins the composition.
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
