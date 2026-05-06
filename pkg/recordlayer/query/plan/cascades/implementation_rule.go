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
	Bindings       *matching.PlannerBindings
	Reference      *expressions.Reference
	Context        PlanContext
	Constraints    *ConstraintMap
	yielded        []expressions.RelationalExpression
	constraintOnly bool
}

// Yield records a final expression to be inserted into the
// Reference's final members after the rule completes.
func (c *ImplementationRuleCall) Yield(expr expressions.RelationalExpression) {
	if c.constraintOnly {
		return
	}
	c.yielded = append(c.yielded, expr)
}

// YieldFinalExpression is an alias for Yield — matches Java's
// FinalYields.yieldFinalExpression naming.
func (c *ImplementationRuleCall) YieldFinalExpression(expr expressions.RelationalExpression) {
	c.Yield(expr)
}

// GetRequestedOrderings returns the requested orderings for this
// Reference, if set by a parent rule. Returns nil if no ordering
// constraint is set.
func (c *ImplementationRuleCall) GetRequestedOrderings() []*RequestedOrdering {
	orderings, ok := Get(c.Constraints, c.Reference, RequestedOrderingConstraintKey)
	if !ok {
		return nil
	}
	return orderings
}

// PushConstraint pushes a constraint value to a child Reference.
func (c *ImplementationRuleCall) PushConstraint(
	childRef *expressions.Reference,
	orderings []*RequestedOrdering,
) {
	Set(c.Constraints, childRef, RequestedOrderingConstraintKey, orderings)
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
	ref := expressions.NewFinalReference(exprs)
	if source != nil {
		ref.SetPlanProperties(source.GetPlanProperties())
	}
	return ref
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
// FireImplementationRule runs an ImplementationRule against a Reference.
// The constraints parameter carries ordering constraints from parent rules.
func FireImplementationRule(rule ImplementationRule, ref *expressions.Reference, constraints ...*ConstraintMap) []expressions.RelationalExpression {
	var cm *ConstraintMap
	if len(constraints) > 0 {
		cm = constraints[0]
	}
	var allYielded []expressions.RelationalExpression
	for _, member := range ref.AllMembers() {
		bindings := rule.Matcher().BindMatches(matching.NewBindings(), member)
		for _, b := range bindings {
			call := &ImplementationRuleCall{
				Bindings:    b,
				Reference:   ref,
				Context:     EmptyPlanContext(),
				Constraints: cm,
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
