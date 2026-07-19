package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type physicalTempTableScanWrapper struct {
	plan *plans.RecordQueryTempTableScanPlan
}

func newPhysicalTempTableScanWrapper(plan *plans.RecordQueryTempTableScanPlan) *physicalTempTableScanWrapper {
	return &physicalTempTableScanWrapper{plan: plan}
}

func (w *physicalTempTableScanWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalTempTableScanWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalTempTableScanWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalTempTableScanWrapper) CanCorrelate() bool                       { return false }
func (w *physicalTempTableScanWrapper) ChildrenAsSet() bool                      { return false }

func (w *physicalTempTableScanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalTempTableScanWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalTempTableScanWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

func (w *physicalTempTableScanWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physttscanwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalTempTableScanWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalTempTableScanWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

func (w *physicalTempTableScanWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalTempTableScanWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

func (w *physicalTempTableScanWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalTempTableScanWrapper)(nil)
	_ physicalPlanExpression           = (*physicalTempTableScanWrapper)(nil)
)
