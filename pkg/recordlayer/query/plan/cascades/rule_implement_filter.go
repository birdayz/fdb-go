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
// The yielded wrapper carries the FilterPlan + an inner Quantifier
// that ranges over a FRESH Reference holding a fresh copy of the
// physical inner. This keeps Reference identity local to the rule
// fire — Memo merging across rule outputs is a future concern.
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
	// Find a physical inner. The seed only knows about
	// physicalScanWrapper today; future wrappers extend the type
	// switch.
	var innerPlan plans.RecordQueryPlan
	for _, m := range innerRef.Members() {
		switch w := m.(type) {
		case *physicalScanWrapper:
			innerPlan = w.GetPlan()
		case *physicalFilterWrapper:
			innerPlan = w.GetPlan()
		case *physicalSortWrapper:
			innerPlan = w.GetPlan()
		case *physicalDistinctWrapper:
			innerPlan = w.GetPlan()
		case *physicalTypeFilterWrapper:
			innerPlan = w.GetPlan()
		}
		if innerPlan != nil {
			break
		}
	}
	if innerPlan == nil {
		return // inner not yet implemented; rule fires later
	}

	filterPlan := plans.NewRecordQueryFilterPlan(f.GetPredicates(), innerPlan)

	// The wrapper's inner Quantifier ranges over a fresh Reference
	// holding the wrapped inner physical plan. This stitches the
	// filter wrapper into the Reference DAG.
	innerWrap := wrapPhysicalPlan(innerPlan)
	if innerWrap == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
	call.Yield(NewPhysicalFilterWrapper(filterPlan, innerQ))
}

// wrapPhysicalPlan returns the RelationalExpression-adapter wrapper
// for `p`. Returns nil if `p`'s concrete type doesn't have a wrapper
// yet — the rule fires later when the corresponding wrapper lands.
func wrapPhysicalPlan(p plans.RecordQueryPlan) expressions.RelationalExpression {
	switch concrete := p.(type) {
	case *plans.RecordQueryScanPlan:
		return &physicalScanWrapper{plan: concrete}
	case *plans.RecordQueryFilterPlan:
		// Need to recursively wrap THIS filter's inner — recurse.
		innerWrap := wrapPhysicalPlan(concrete.GetInner())
		if innerWrap == nil {
			return nil
		}
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
		return NewPhysicalFilterWrapper(concrete, innerQ)
	case *plans.RecordQuerySortPlan:
		innerWrap := wrapPhysicalPlan(concrete.GetInner())
		if innerWrap == nil {
			return nil
		}
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
		return NewPhysicalSortWrapper(concrete, innerQ)
	case *plans.RecordQueryDistinctPlan:
		innerWrap := wrapPhysicalPlan(concrete.GetInner())
		if innerWrap == nil {
			return nil
		}
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
		return NewPhysicalDistinctWrapper(concrete, innerQ)
	case *plans.RecordQueryTypeFilterPlan:
		innerWrap := wrapPhysicalPlan(concrete.GetInner())
		if innerWrap == nil {
			return nil
		}
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
		return NewPhysicalTypeFilterWrapper(concrete, innerQ)
	}
	return nil
}

var _ ExpressionRule = (*ImplementFilterRule)(nil)
