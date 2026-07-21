package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementUnionRule implements a logical LogicalUnionExpression as
// a physical RecordQueryUnionPlan, gated on EVERY child Reference
// having at least one physical-plan member.
//
//	Union(child0-with-physical, child1-with-physical, ...)
//	  →  UnionPlan(child0-physical, child1-physical, ...)
//
// Per-child gating: unlike single-inner Implement rules, Union
// requires ALL children to be physical-implemented before yielding
// — partial physical-implementation produces an invalid mixed-
// hierarchy plan tree.
//
// Java has multiple Union variants (key-expression vs values, dedup
// vs no-dedup); this rule always emits RecordQueryUnionPlan
// (UNION ALL, no dedup) — the dedup / ordered variants come from
// their own rules (ImplementDistinctUnionRule,
// ImplementUnorderedUnionRule, ImplementInUnionRule).
type ImplementUnionRule struct {
	matcher matching.BindingMatcher
}

// NewImplementUnionRule constructs the rule.
func NewImplementUnionRule() *ImplementUnionRule {
	return &ImplementUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUnionExpression]("logical_union"),
	}
}

// Matcher returns the pattern.
func (r *ImplementUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when EVERY child Quantifier ranges over a Reference
// with at least one physical-plan member.
func (r *ImplementUnionRule) OnMatch(call *ExpressionRuleCall) {
	u := matching.Get[*expressions.LogicalUnionExpression](call.Bindings, r.matcher)
	children := u.GetQuantifiers()
	if len(children) == 0 {
		return
	}

	winners := make([]expressions.RelationalExpression, 0, len(children))
	for _, q := range children {
		innerRef := q.GetRangesOver()
		if innerRef == nil {
			return
		}
		winner, _ := getWinnerForOrdering(innerRef, properties.PreserveOrdering(), call.CostModel())
		if winner == nil {
			return // any child not physical → skip the whole rule fire
		}
		if _, ok := winner.(physicalPlanExpression); !ok {
			return
		}
		winners = append(winners, winner)
	}

	// Each leg ranges over its OPTIMIZE winner as a singleton memo edge.
	childQs := make([]expressions.Quantifier, 0, len(winners))
	for _, winner := range winners {
		childQs = append(childQs, expressions.ForEachQuantifier(call.MemoizeExpression(winner)))
	}

	// The union is its own cascades expression carrying those live leg edges
	// (RFC-184 W2) — no wrapper storing a second snapshot of the legs.
	call.Yield(plans.NewRecordQueryUnionPlanFromQuantifiers(childQs))
}

var _ ExpressionRule = (*ImplementUnionRule)(nil)
