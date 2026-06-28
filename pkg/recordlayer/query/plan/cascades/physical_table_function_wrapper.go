package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type physicalTableFunctionWrapper struct {
	plan *plans.RecordQueryTableFunctionPlan
}

func newPhysicalTableFunctionWrapper(plan *plans.RecordQueryTableFunctionPlan) *physicalTableFunctionWrapper {
	return &physicalTableFunctionWrapper{plan: plan}
}

func (w *physicalTableFunctionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalTableFunctionWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalTableFunctionWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalTableFunctionWrapper) CanCorrelate() bool                       { return false }
func (w *physicalTableFunctionWrapper) ChildrenAsSet() bool                      { return false }

func (w *physicalTableFunctionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if w.plan != nil && w.plan.GetStreamValue() != nil {
		return values.GetCorrelatedToOfValue(w.plan.GetStreamValue())
	}
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalTableFunctionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalTableFunctionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalTableFunctionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("phystblwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalTableFunctionWrapper) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.Cost{
		Cardinality: properties.LeafScanCardinality * physicalWrapperCostMultiplier,
		CPU:         0,
	}
}

func (w *physicalTableFunctionWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalTableFunctionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalTableFunctionWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

func (w *physicalTableFunctionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalTableFunctionWrapper)(nil)
	_ physicalPlanExpression           = (*physicalTableFunctionWrapper)(nil)
)
