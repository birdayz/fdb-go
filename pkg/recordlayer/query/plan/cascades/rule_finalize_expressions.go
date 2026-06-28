package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// anyExpressionMatcher matches any RelationalExpression. Used by
// FinalizeExpressionsRule to promote all exploratory members to final.
type anyExpressionMatcher struct{}

func (m *anyExpressionMatcher) RootType() string { return "any" }

func (m *anyExpressionMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(expressions.RelationalExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}

// FinalizeExpressionsRule is a REWRITING-phase ImplementationRule that
// promotes any exploratory expression to a final expression. This is how
// the REWRITING phase marks its canonical output for OptimizeGroup to
// select the best, and for advancePlannerStage to promote as the
// PLANNING seed.
//
// Mirrors Java's FinalizeExpressionsRule.
type FinalizeExpressionsRule struct {
	matcher matching.BindingMatcher
}

func NewFinalizeExpressionsRule() *FinalizeExpressionsRule {
	return &FinalizeExpressionsRule{
		matcher: &anyExpressionMatcher{},
	}
}

func (r *FinalizeExpressionsRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *FinalizeExpressionsRule) OnMatch(call *ImplementationRuleCall) {
	expr := call.Bindings.Get(r.matcher)
	if re, ok := expr.(expressions.RelationalExpression); ok {
		call.Yield(re)
	}
}

var _ ImplementationRule = (*FinalizeExpressionsRule)(nil)
