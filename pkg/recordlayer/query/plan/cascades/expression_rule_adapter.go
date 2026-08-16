package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// AsImplementationRule adapts an ExpressionRule to run as an
// ImplementationRule during the PLANNING phase. The wrapped rule's
// Yield() inserts into Members (via the ImplementationRuleCall)
// alongside exploration rules. MemoizeExpression uses
// the planner's Memo when available.
//
// This lets the physical-implementation ExpressionRules
// (BatchAExpressionRules) run inside the PLANNING phase's
// ImplementationRule driver without rewriting each rule.
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
	if implCall.constraintOnly || implCall.CancellationErr() != nil {
		return
	}
	call := &ExpressionRuleCall{
		Bindings:    implCall.Bindings,
		Reference:   implCall.Reference,
		Context:     implCall.Context,
		RunContext:  implCall.RunContext,
		Constraints: implCall.Constraints,
		memo:        implCall.memo,
	}
	a.rule.OnMatch(call)
	if err := call.Err(); err != nil {
		implCall.Fail(err)
		return
	}
	// Hand the inner call's staged child inserts to the OUTER call rather than
	// committing them here. This driver's preflight can still reject the batch
	// after the rule body returned, and an insert committed at the inner
	// boundary would survive that rejection — which is the exact leak staging
	// exists to close, reintroduced one level down.
	implCall.AdoptStagedInserts(call.stagedInserts)
	call.stagedInserts = nil
	for _, y := range call.Yielded() {
		implCall.Yield(y)
	}
	// The implementation driver publishes this topology effect together
	// with the staged final-member insertions, after full-batch validation.
	implCall.indexYieldedInMemo = true
}

var _ ImplementationRule = (*expressionRuleAdapter)(nil)
