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

// FinalizeExpressionsRule disentangles exploratory children into final
// expression partitions, as Java's rewriting-phase finalizer does.
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
	expr := matching.Get[expressions.RelationalExpression](call.Bindings, r.matcher)
	if !isExploratoryMember(call.Reference, expr) {
		return
	}
	quantifiers := expr.GetQuantifiers()
	if len(quantifiers) == 0 {
		final, err := expr.WithQuantifiers(nil)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(final)
		return
	}
	partitions := make([][][]expressions.RelationalExpression, len(quantifiers))
	for i, q := range quantifiers {
		partitions[i] = rewritingExpressionPartitions(q.GetRangesOver())
		if len(partitions[i]) == 0 {
			return
		}
	}
	for _, combination := range CrossProduct(partitions) {
		rebuilt := make([]expressions.Quantifier, len(quantifiers))
		for i, q := range quantifiers {
			ref := call.MemoizeFinalExpressionsFromOther(q.GetRangesOver(), combination[i])
			rebuilt[i] = expressions.RebuildQuantifier(q, ref)
		}
		final, err := expr.WithQuantifiers(rebuilt)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(final)
	}
}

// SelectMergeable is Java's only rewriting partition key. Keep all final
// alternatives with the same value, in encounter order.
func rewritingExpressionPartitions(ref *expressions.Reference) [][]expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	var partitions [][]expressions.RelationalExpression
	indices := make(map[bool]int)
	for _, member := range ref.FinalMembers() {
		_, mergeable := member.(expressions.RelationalExpressionWithPredicates)
		index, found := indices[mergeable]
		if !found {
			index = len(partitions)
			indices[mergeable] = index
			partitions = append(partitions, nil)
		}
		partitions[index] = append(partitions[index], member)
	}
	return partitions
}

var _ ImplementationRule = (*FinalizeExpressionsRule)(nil)
