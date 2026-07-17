package cascades

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type indexProbeRule struct {
	m matching.BindingMatcher
}

func (r *indexProbeRule) Matcher() matching.BindingMatcher { return r.m }
func (r *indexProbeRule) OnMatch(call *ExpressionRuleCall) {}
func (r *indexProbeRule) String() string                   { return "indexProbeRule" }
func probeRule(m matching.BindingMatcher) *indexProbeRule  { return &indexProbeRule{m: m} }

func TestRuleIndex_BucketsByRootOperator(t *testing.T) {
	t.Parallel()

	sortRule := probeRule(NewExpressionMatcher[*expressions.LogicalSortExpression]("s"))
	filterRule := probeRule(NewExpressionMatcher[*expressions.LogicalFilterExpression]("f"))
	// Interface-typed matcher — the match-anything shape MatchLeafRule /
	// MatchIntermediateRule use. It MUST land in the always bucket: bucketing
	// it under the interface type would key it where no concrete expression
	// ever looks, silently disabling the matching infrastructure (this
	// exact failure took out every data-access plan when the index first
	// went in — sorts stopped eliding because MatchLeafRule never fired).
	anyRule := probeRule(NewExpressionMatcher[expressions.RelationalExpression]("any"))

	ix := newRuleIndex([]ExpressionRule{sortRule, filterRule, anyRule})

	if len(ix.always) != 1 {
		t.Fatalf("interface-rooted rule must be an always rule; always=%d", len(ix.always))
	}
	if got := len(ix.byRoot[reflect.TypeFor[*expressions.LogicalSortExpression]()]); got != 1 {
		t.Fatalf("sort bucket size = %d, want 1", got)
	}

	sortExpr := expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(
		expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))))

	got := ix.rulesFor(sortExpr)
	if len(got) != 2 || got[0] != ExpressionRule(sortRule) || got[1] != ExpressionRule(anyRule) {
		t.Fatalf("rulesFor(sort) = %d rules, want [sortRule, anyRule] (indexed first, then always, registration order)", len(got))
	}
	for _, r := range got {
		if r == ExpressionRule(filterRule) {
			t.Fatal("a filter-rooted rule must not be offered to a sort expression")
		}
	}

	// A type with no indexed rules gets exactly the always bucket.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	if got := ix.rulesFor(scan); len(got) != 1 || got[0] != ExpressionRule(anyRule) {
		t.Fatalf("rulesFor(scan) = %d rules, want just the always rule", len(got))
	}

	// The cache returns the identical slice on re-query.
	a := ix.rulesFor(sortExpr)
	b := ix.rulesFor(sortExpr)
	if &a[0] != &b[0] {
		t.Fatal("rulesFor must serve the cached list on re-query")
	}
}

// TestRuleIndex_ProductionRootsAreConcrete sweeps every production rule
// set: a matcher that declares a root operator must declare a CONCRETE
// type an expression can actually have — never an interface, which would
// index the rule where no lookup ever hits.
func TestRuleIndex_ProductionRootsAreConcrete(t *testing.T) {
	t.Parallel()

	checkMatcher := func(name string, m matching.BindingMatcher) {
		rm, ok := m.(matching.RootOperatorMatcher)
		if !ok {
			return // always rule — fine
		}
		root := rm.RootOperator()
		if root == nil {
			return // explicit always — fine
		}
		if root.Kind() == reflect.Interface {
			t.Errorf("%s: RootOperator returned interface %v — the rule would never be offered to any expression", name, root)
		}
	}
	for _, r := range DefaultExpressionRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	for _, r := range BatchAExpressionRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	for _, r := range PlanningExplorationRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	for _, r := range DefaultImplementationRules() {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
	// The REWRITING implementation rule set (FinalizeExpressionsRule et al)
	// is registered in NewPlanner, not a exported list — sweep it too.
	for _, r := range NewPlanner(nil, nil).rewritingImplRules {
		checkMatcher(reflect.TypeOf(r).String(), r.Matcher())
	}
}
