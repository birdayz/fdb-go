package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type physicalTempTableInsertWrapper struct {
	plan       *plans.RecordQueryTempTableInsertPlan
	innerQuant expressions.Quantifier
}

func newPhysicalTempTableInsertWrapper(
	plan *plans.RecordQueryTempTableInsertPlan,
	innerQuant expressions.Quantifier,
) *physicalTempTableInsertWrapper {
	return &physicalTempTableInsertWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalTempTableInsertWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalTempTableInsertWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalTempTableInsertWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalTempTableInsertWrapper) CanCorrelate() bool  { return false }
func (w *physicalTempTableInsertWrapper) ChildrenAsSet() bool { return false }

func (w *physicalTempTableInsertWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalTempTableInsertWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalTempTableInsertWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

func (w *physicalTempTableInsertWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physttinsertwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalTempTableInsertWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalTempTableInsertWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

func (w *physicalTempTableInsertWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalTempTableInsertWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryTempTableInsertPlan(innerPlan, w.plan.GetTempTableAlias(), w.plan.IsOwning())
		return &physicalTempTableInsertWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalTempTableInsertWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalTempTableInsertWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalTempTableInsertWrapper)(nil)
	_ physicalPlanExpression           = (*physicalTempTableInsertWrapper)(nil)
)
