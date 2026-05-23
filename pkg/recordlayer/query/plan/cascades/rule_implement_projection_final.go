package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

	projectedValues := proj.GetProjectedValues()

	for _, m := range innerRef.AllMembers() {
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			continue
		}

		innerPlan := ph.GetRecordQueryPlan()

		if idxW, ok := m.(*physicalIndexScanWrapper); ok && !idxW.covering {
			if projectionCoveredByIndex(projectedValues, idxW, call.Context) {
				innerPlan = idxW.plan.WithCovering(idxW.columnNames)
			}
		}

		projPlan := plans.NewRecordQueryProjectionPlanWithAliases(
			proj.GetProjectedValues(), proj.GetAliases(), innerPlan)
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(m))
		call.YieldFinalExpression(newPhysicalProjectionFinalWrapper(projPlan, innerQ))
	}
}

func newPhysicalProjectionFinalWrapper(plan *plans.RecordQueryProjectionPlan, innerQuant expressions.Quantifier) *physicalProjectionWrapper {
	return NewPhysicalProjectionWrapper(plan, innerQuant)
}

// projectionCoveredByIndex checks whether all projected values are
// available from the index entry (index columns + PK columns).
// Every FDB index entry contains the PK, so projections that only
// reference index columns and/or PK columns can skip the record fetch.
func projectionCoveredByIndex(projected []values.Value, idxW *physicalIndexScanWrapper, ctx PlanContext) bool {
	available := make(map[string]struct{}, len(idxW.columnNames)+4)
	for _, col := range idxW.columnNames {
		available[strings.ToUpper(col)] = struct{}{}
	}
	if rts := idxW.plan.GetRecordTypes(); len(rts) > 0 {
		for _, col := range ctx.GetPrimaryKeyColumns(rts[0]) {
			available[strings.ToUpper(col)] = struct{}{}
		}
	}
	if len(available) == 0 {
		return false
	}
	for _, v := range projected {
		fv, ok := v.(*values.FieldValue)
		if !ok {
			return false
		}
		if _, found := available[strings.ToUpper(fv.Field)]; !found {
			return false
		}
	}
	return true
}

var _ ImplementationRule = (*ImplementProjectionFinalRule)(nil)
