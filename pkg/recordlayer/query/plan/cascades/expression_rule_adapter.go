package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// AsImplementationRule adapts an ExpressionRule to run as an
// ImplementationRule during the PLANNING phase. The wrapped rule's
// Yield() inserts into Members (via the ImplementationRuleCall)
// alongside exploration rules. MemoizeExpression uses
// the planner's Memo when available.
//
// This enables moving physical implementation rules (BatchA) from the
// EXPLORE phase to the PLANNING phase without rewriting each rule.
func AsImplementationRule(rule ExpressionRule) ImplementationRule {
	return &expressionRuleAdapter{rule: rule}
}

type expressionRuleAdapter struct {
	rule ExpressionRule
}

func (a *expressionRuleAdapter) Matcher() matching.BindingMatcher {
	return a.rule.Matcher()
}

func (a *expressionRuleAdapter) OnMatch(implCall *ImplementationRuleCall) {
	if implCall.constraintOnly {
		return
	}
	call := &ExpressionRuleCall{
		Bindings:    implCall.Bindings,
		Reference:   implCall.Reference,
		Context:     implCall.Context,
		Constraints: implCall.Constraints,
		memo:        implCall.memo,
		yieldFn: func(expr expressions.RelationalExpression) bool {
			implCall.Yield(expr)
			return true
		},
	}
	a.rule.OnMatch(call)
	if implCall.memo != nil {
		for _, y := range call.yieldedExps {
			implCall.memo.AddExpression(implCall.Reference, y)
		}
	}
}

var _ ImplementationRule = (*expressionRuleAdapter)(nil)
