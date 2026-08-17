package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementProjectionFinalRule is the PLANNING-phase counterpart of
// ImplementProjectionRule. It fires after the inner has been finalized
// (children are physical plans), producing a physical projection wrapper.
type ImplementProjectionFinalRule struct {
	matcher matching.BindingMatcher
}

func NewImplementProjectionFinalRule() *ImplementProjectionFinalRule {
	return &ImplementProjectionFinalRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection_final"),
	}
}

func (r *ImplementProjectionFinalRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementProjectionFinalRule) OnMatch(call *ImplementationRuleCall) {
	proj := call.Bindings.Get(r.matcher).(*expressions.LogicalProjectionExpression)
	qs := proj.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	for _, m := range innerRef.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		// The EXPLORE-phase implementation rule can already reuse an exact
		// positional Projection child. Repeat the same proof here because the
		// final implementation lane is an independent producer; otherwise it
		// reintroduces the redundant wrapper as a cost competitor and extraction
		// may select it even though the earlier lane yielded the child directly.
		if reusable, ok := exactPositionalProjectionReuse(proj, m); ok {
			call.YieldFinalExpression(reusable)
			continue
		}
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(m))
		logicalEdge, err := qs[0].RequireFlowedObjectValue()
		if err != nil {
			call.Fail(err)
			return
		}
		physicalEdge, err := innerQ.RequireFlowedObjectValue()
		if err != nil {
			call.Fail(err)
			return
		}
		rebasedValues, err := rebaseProjectionValuesForPhysicalEdge(
			proj.GetProjectedValues(), logicalEdge, physicalEdge)
		if err != nil {
			call.Fail(err)
			return
		}
		// The projection is its own cascades expression carrying the live innerQ
		// edge (RFC-184 W2).
		projection, err := plans.NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
			rebasedValues, proj.GetAliases(), proj.GetAliasMinted(), proj.GetOutputNames(), innerQ)
		if err != nil {
			call.Fail(err)
			return
		}
		projection, err = projection.WithAliasSources(proj.GetAliasSources())
		if err != nil {
			call.Fail(err)
			return
		}
		call.YieldFinalExpression(projection)
	}
}

var _ ImplementationRule = (*ImplementProjectionFinalRule)(nil)
