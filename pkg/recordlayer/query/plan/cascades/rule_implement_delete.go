package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementDeleteRule implements a logical DeleteExpression as a
// physical RecordQueryDeletePlan, gated on the inner Reference
// having at least one physical-plan member.
//
//	Delete(target, inner-with-physical-member)
//	  →  DeletePlan(target, inner-physical)
//
// Same gating pattern as the other Implement rules. Java's
// ImplementDeleteRule consults StoredRecordProperty for partition
// dispatch; the seed always emits.
type ImplementDeleteRule struct {
	matcher matching.BindingMatcher
}

// NewImplementDeleteRule constructs the rule.
func NewImplementDeleteRule() *ImplementDeleteRule {
	return &ImplementDeleteRule{
		matcher: NewExpressionMatcher[*expressions.DeleteExpression]("delete"),
	}
}

// Matcher returns the pattern.
func (r *ImplementDeleteRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every DeleteExpression with a physical inner.
func (r *ImplementDeleteRule) OnMatch(call *ExpressionRuleCall) {
	del := matching.Get[*expressions.DeleteExpression](call.Bindings, r.matcher)
	innerRef := del.GetInner().GetRangesOver()
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

	delPlan := plans.NewRecordQueryDeletePlan(innerPlan, del.GetTargetRecordType())

	innerWrap := wrapPhysicalPlan(innerPlan)
	if innerWrap == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
	call.Yield(NewPhysicalDeleteWrapper(delPlan, innerQ))
}

var _ ExpressionRule = (*ImplementDeleteRule)(nil)
