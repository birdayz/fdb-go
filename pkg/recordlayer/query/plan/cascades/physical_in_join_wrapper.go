package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type physicalInJoinWrapper struct {
	plan       *plans.RecordQueryInJoinPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalInJoinWrapper(
	plan *plans.RecordQueryInJoinPlan,
	innerQuant expressions.Quantifier,
) *physicalInJoinWrapper {
	return &physicalInJoinWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalInJoinWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalInJoinWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

func (w *physicalInJoinWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalInJoinWrapper) CanCorrelate() bool  { return false }
func (w *physicalInJoinWrapper) ChildrenAsSet() bool { return false }

func (w *physicalInJoinWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalInJoinWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalInJoinWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalInJoinWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physinjoinwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalInJoinWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalInJoinWrapper.WithChildren: expected 1, got %d", len(qs))
	}
	if childPlan := extractChildPlanFromQuantifier(qs[0]); childPlan != nil && isLeafReplaceable(childPlan) {
		rebuilt := plans.NewRecordQueryInJoinPlan(
			childPlan, w.plan.GetBindingName(), w.plan.IsSorted(), w.plan.IsReverse())
		rebuilt.SetInValues(w.plan.GetInValues())
		rebuilt.SetSourceKind(w.plan.GetSourceKind())
		return &physicalInJoinWrapper{plan: rebuilt, innerQuant: qs[0]}, nil
	}
	return &physicalInJoinWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalInJoinWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	inListLen := float64(len(w.plan.GetInValues()))
	if inListLen < 1 {
		inListLen = 10 // parameterized IN — values not bound at plan time
	}
	// InJoin is a correlated index probe: for each IN value, the inner
	// plan does an equality point-lookup returning ~1 row. The child's
	// standalone cardinality overstates this (it reports the index's
	// selectivity against the full table). Use inListLen as the output
	// cardinality (one row per IN value for well-distributed data).
	return properties.Cost{
		Cardinality: inListLen * physicalWrapperCostMultiplier,
		CPU:         inListLen * (properties.ScanCPU + properties.FetchCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalInJoinWrapper) HintOrdering() properties.Ordering {
	// InJoin iterates IN-values one at a time. Each batch preserves
	// the inner scan's ordering, but the GLOBAL result ordering depends
	// on the IN-source order, not the inner scan. Don't claim the
	// inner's ordering — it would cause sort elimination to remove a
	// necessary ORDER BY.
	return properties.Ordering{}
}

func (w *physicalInJoinWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalInJoinWrapper)(nil)
	_ physicalPlanExpression           = (*physicalInJoinWrapper)(nil)
)
