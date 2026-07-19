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
	return w.plan.EqualsPlanWithoutChildren(o.plan)
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
		// WithInner, not a constructor rebuild: the constructor drops
		// inValues/sourceKind unless every setter is replayed.
		return &physicalInJoinWrapper{plan: w.plan.WithInner(childPlan), innerQuant: qs[0]}, nil
	}
	return &physicalInJoinWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalInJoinWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalInJoinWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

func (w *physicalInJoinWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalInJoinWrapper)(nil)
	_ physicalPlanExpression           = (*physicalInJoinWrapper)(nil)
)
