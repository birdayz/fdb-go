package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalFirstOrDefaultWrapper struct {
	plan       *plans.RecordQueryFirstOrDefaultPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalFirstOrDefaultWrapper(
	plan *plans.RecordQueryFirstOrDefaultPlan,
	innerQuant expressions.Quantifier,
) *physicalFirstOrDefaultWrapper {
	return &physicalFirstOrDefaultWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalFirstOrDefaultWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalFirstOrDefaultWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

func (w *physicalFirstOrDefaultWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalFirstOrDefaultWrapper) CanCorrelate() bool  { return false }
func (w *physicalFirstOrDefaultWrapper) ChildrenAsSet() bool { return false }

func (w *physicalFirstOrDefaultWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalFirstOrDefaultWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalFirstOrDefaultWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalFirstOrDefaultWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physfirstordefaultwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalFirstOrDefaultWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalFirstOrDefaultWrapper.WithChildren: expected 1, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryFirstOrDefaultPlan(innerPlan, w.plan.GetDefaultValue())
		return &physicalFirstOrDefaultWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalFirstOrDefaultWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalFirstOrDefaultWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: 1 * physicalWrapperCostMultiplier,
		CPU:         child[0].CPU * physicalWrapperCostMultiplier,
	}
}

func (w *physicalFirstOrDefaultWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalFirstOrDefaultWrapper)(nil)
	_ physicalPlanExpression           = (*physicalFirstOrDefaultWrapper)(nil)
)
