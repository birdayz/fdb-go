package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type physicalDefaultOnEmptyWrapper struct {
	plan       *plans.RecordQueryDefaultOnEmptyPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalDefaultOnEmptyWrapper(
	plan *plans.RecordQueryDefaultOnEmptyPlan,
	innerQuant expressions.Quantifier,
) *physicalDefaultOnEmptyWrapper {
	return &physicalDefaultOnEmptyWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalDefaultOnEmptyWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalDefaultOnEmptyWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

func (w *physicalDefaultOnEmptyWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalDefaultOnEmptyWrapper) CanCorrelate() bool  { return false }
func (w *physicalDefaultOnEmptyWrapper) ChildrenAsSet() bool { return false }

func (w *physicalDefaultOnEmptyWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalDefaultOnEmptyWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalDefaultOnEmptyWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalDefaultOnEmptyWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physdefaultonemptywrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalDefaultOnEmptyWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalDefaultOnEmptyWrapper.WithChildren: expected 1, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryDefaultOnEmptyPlan(innerPlan, w.plan.GetDefaultValue())
		return &physicalDefaultOnEmptyWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalDefaultOnEmptyWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalDefaultOnEmptyWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality * physicalWrapperCostMultiplier,
		CPU:         child[0].CPU * physicalWrapperCostMultiplier,
	}
}

func (w *physicalDefaultOnEmptyWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalDefaultOnEmptyWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalDefaultOnEmptyWrapper)(nil)
	_ physicalPlanExpression           = (*physicalDefaultOnEmptyWrapper)(nil)
)
