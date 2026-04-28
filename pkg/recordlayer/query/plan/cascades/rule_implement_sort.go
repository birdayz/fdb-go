package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementSortRule implements a logical LogicalSortExpression as a
// physical RecordQuerySortPlan, gated on the inner Reference having
// at least one physical-plan member. Same shape as ImplementFilterRule.
//
//	Sort([k1, k2], inner-with-physical-member)
//	  →  SortPlan([k1, k2], inner-physical)
//
// Java's `ImplementSortRule` is more sophisticated: it consults
// the planner's RequestedOrdering property and decides whether to
// emit a sort plan at all (the inner might already satisfy the
// ordering — see `OrderingProperty`). The seed always emits the
// sort; ordering-elimination lives in B5 follow-on rules.
type ImplementSortRule struct {
	matcher matching.BindingMatcher
}

// NewImplementSortRule constructs the rule.
func NewImplementSortRule() *ImplementSortRule {
	return &ImplementSortRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("logical_sort"),
	}
}

// Matcher returns the pattern.
func (r *ImplementSortRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every LogicalSortExpression. Walks the inner
// Reference for a physical-plan member; if found, yields a
// SortPlan wrapper.
func (r *ImplementSortRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
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
		case *physicalUnionWrapper:
			innerPlan = w.GetPlan()
		case *physicalIntersectionWrapper:
			innerPlan = w.GetPlan()
		}
		if innerPlan != nil {
			break
		}
	}
	if innerPlan == nil {
		return
	}

	sortPlan := plans.NewRecordQuerySortPlan(s.GetSortKeys(), innerPlan)

	innerWrap := wrapPhysicalPlan(innerPlan)
	if innerWrap == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
	call.Yield(NewPhysicalSortWrapper(sortPlan, innerQ))
}

var _ ExpressionRule = (*ImplementSortRule)(nil)
