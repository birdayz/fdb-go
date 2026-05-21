package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementFilterRule implements a logical LogicalFilterExpression
// as a physical RecordQueryFilterPlan, provided the inner
// Quantifier's Reference contains at least one physical-plan member.
//
//	Filter([P], inner-with-physical-member)
//	  →  FilterPlan([P], inner-physical)
//
// The "with-physical-member" guard ensures the rule fires only AFTER
// the inner has been implemented (i.e. PrimaryScanRule or another
// implement rule has yielded a physical wrapper into the inner's
// Reference). This avoids producing partial physical plans that
// reference still-logical inner trees.
//
// Java's task-stack planner orders OPTIMIZE bottom-up so children
// are always implemented first. Our seed FixpointApply / Planner
// don't enforce strict order; the guard substitutes for that.
//
// The yielded wrapper's inner Quantifier ranges over the SAME
// Reference as the logical filter's inner Quantifier — Memo sharing
// via MemoizeExpression ensures cross-Reference dedup.
type ImplementFilterRule struct {
	matcher matching.BindingMatcher
}

// NewImplementFilterRule constructs the rule.
func NewImplementFilterRule() *ImplementFilterRule {
	return &ImplementFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *ImplementFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every LogicalFilterExpression. Walks the inner
// Reference for any physical-plan member; if found, yields a
// FilterPlan wrapper over a fresh Reference holding that physical
// inner.
func (r *ImplementFilterRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerRef := f.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	filterPlan := plans.NewRecordQueryFilterPlan(f.GetPredicates(), innerPlan)

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(NewPhysicalFilterWrapper(filterPlan, innerQ))
}

var _ ExpressionRule = (*ImplementFilterRule)(nil)
