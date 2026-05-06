package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// FinalizeExpressionsRule implements any exploratory expression with
// itself over disentangled children. This is the catch-all rule that
// runs during PhasePlanning when no specific ImplementationRule
// handles an expression.
//
// The rule matches any exploratory expression, creates new References
// for each child quantifier containing only that child's final members
// (disentangling the shared DAG), then yields the original expression
// rebuilt with new quantifiers over the disentangled References.
//
// Ports Java's FinalizeExpressionsRule.
type FinalizeExpressionsRule struct {
	matcher matching.BindingMatcher
}

func NewFinalizeExpressionsRule() *FinalizeExpressionsRule {
	return &FinalizeExpressionsRule{
		matcher: &anyExploratoryMatcher{},
	}
}

func (r *FinalizeExpressionsRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *FinalizeExpressionsRule) OnMatch(call *ImplementationRuleCall) {
	expr := call.Bindings.Get(r.matcher).(expressions.RelationalExpression)

	quantifiers := expr.GetQuantifiers()
	if len(quantifiers) == 0 {
		call.YieldFinalExpression(expr)
		return
	}

	newQuantifiers := make([]expressions.Quantifier, len(quantifiers))
	for i, q := range quantifiers {
		childRef := q.GetRangesOver()
		if childRef == nil {
			newQuantifiers[i] = q
			continue
		}

		finals := childRef.FinalMembers()
		if len(finals) == 0 {
			return
		}
		newRef := expressions.NewFinalReference(finals)
		newQuantifiers[i] = expressions.RebuildQuantifier(q, newRef)
	}

	finalized := expr.WithQuantifiers(newQuantifiers)
	call.YieldFinalExpression(finalized)
}

var _ ImplementationRule = (*FinalizeExpressionsRule)(nil)

// anyExploratoryMatcher matches any RelationalExpression.
type anyExploratoryMatcher struct{}

func (m *anyExploratoryMatcher) RootType() string { return "any" }

func (m *anyExploratoryMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(expressions.RelationalExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
