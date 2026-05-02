package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
// vs no-dedup); the seed always emits RecordQueryUnionPlan
// (UNION ALL, no dedup). Rules that need different variants
// extend this pattern.
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

	innerPlans := make([]plans.RecordQueryPlan, 0, len(children))
	childRefs := make([]*expressions.Reference, 0, len(children))
	for _, q := range children {
		innerRef := q.GetRangesOver()
		if innerRef == nil {
			return
		}
		innerPlan := findPhysicalPlan(innerRef)
		if innerPlan == nil {
			return // any child not physical → skip the whole rule fire
		}
		innerPlans = append(innerPlans, innerPlan)
		childRefs = append(childRefs, innerRef)
	}

	unionPlan := plans.NewRecordQueryUnionPlan(innerPlans)

	// Reuse the existing physical wrapper expressions from each child
	// Reference rather than re-wrapping from scratch.
	childQs := make([]expressions.Quantifier, 0, len(childRefs))
	for _, ref := range childRefs {
		innerExpr := findPhysicalExpr(ref)
		if innerExpr == nil {
			return
		}
		childQs = append(childQs, expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr)))
	}

	// We need a wrapper for the union plan too. Since UnionPlan has
	// N children (not 1), we use a generic UnionWrapper.
	call.Yield(NewPhysicalUnionWrapper(unionPlan, childQs))
}

var _ ExpressionRule = (*ImplementUnionRule)(nil)
