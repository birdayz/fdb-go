package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// ImplementationRule is a rule that runs during PhasePlanning.
// Unlike ExpressionRule (which yields exploratory expressions into
// Members), an ImplementationRule yields final expressions into
// FinalMembers via InsertFinal. It operates on expression partitions
// — subsets of a Reference's final members — and creates disentangled
// sub-DAGs via FinalMemoizer operations.
//
// Ports Java's ImplementationCascadesRule.
type ImplementationRule interface {
	Matcher() matching.BindingMatcher
	OnMatch(call *ImplementationRuleCall)
}

// ImplementationRuleCall provides the restricted API that
// ImplementationRules are allowed to use. It extends the base
// RuleCall with FinalMemoizer and FinalYield operations.
//
// Ports Java's ImplementationCascadesRuleCall.
type ImplementationRuleCall struct {
	Bindings  *matching.PlannerBindings
	Reference *expressions.Reference
	yielded   []expressions.RelationalExpression
}

// Yield records a final expression to be inserted into the
// Reference's final members after the rule completes.
func (c *ImplementationRuleCall) Yield(expr expressions.RelationalExpression) {
	c.yielded = append(c.yielded, expr)
}

// YieldFinalExpression is an alias for Yield — matches Java's
// FinalYields.yieldFinalExpression naming.
func (c *ImplementationRuleCall) YieldFinalExpression(expr expressions.RelationalExpression) {
	c.Yield(expr)
}

// MemoizeFinalExpressionsFromOther creates a new Reference containing
// only the specified expressions (which must already be members of
// `source`). The new Reference holds them as final members —
// disentangled from the shared DAG.
//
// Ports Java's FinalMemoizer.memoizeFinalExpressionsFromOther.
func (c *ImplementationRuleCall) MemoizeFinalExpressionsFromOther(
	source *expressions.Reference,
	exprs []expressions.RelationalExpression,
) *expressions.Reference {
	return expressions.NewFinalReference(exprs)
}

// MemoizeFinalExpression creates a new Reference with a single final
// expression member.
func (c *ImplementationRuleCall) MemoizeFinalExpression(
	expr expressions.RelationalExpression,
) *expressions.Reference {
	return expressions.NewFinalReference([]expressions.RelationalExpression{expr})
}

// FireImplementationRule runs an ImplementationRule against a Reference,
// matching each member and collecting yielded final expressions.
// Returns the yielded expressions (which were also inserted into
// ref.FinalMembers).
func FireImplementationRule(rule ImplementationRule, ref *expressions.Reference) []expressions.RelationalExpression {
	var allYielded []expressions.RelationalExpression
	for _, member := range ref.AllMembers() {
		bindings := rule.Matcher().BindMatches(matching.NewBindings(), member)
		for _, b := range bindings {
			call := &ImplementationRuleCall{
				Bindings:  b,
				Reference: ref,
			}
			rule.OnMatch(call)
			for _, y := range call.yielded {
				ref.InsertFinal(y)
			}
			allYielded = append(allYielded, call.yielded...)
		}
	}
	return allYielded
}
