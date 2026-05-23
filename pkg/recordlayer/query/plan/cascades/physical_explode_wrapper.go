package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalExplodeWrapper struct {
	plan *plans.RecordQueryExplodePlan
}

func newPhysicalExplodeWrapper(plan *plans.RecordQueryExplodePlan) *physicalExplodeWrapper {
	return &physicalExplodeWrapper{plan: plan}
}

func (w *physicalExplodeWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalExplodeWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalExplodeWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalExplodeWrapper) CanCorrelate() bool                       { return false }
func (w *physicalExplodeWrapper) ChildrenAsSet() bool                      { return false }

func (w *physicalExplodeWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if w.plan != nil && w.plan.GetCollectionValue() != nil {
		return values.GetCorrelatedToOfValue(w.plan.GetCollectionValue())
	}
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalExplodeWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalExplodeWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalExplodeWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physexplodewrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalExplodeWrapper) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	card := 10.0
	if w.plan != nil {
		if cv, ok := w.plan.GetCollectionValue().(*values.ConstantValue); ok {
			if sl, ok := cv.Value.([]any); ok {
				card = float64(len(sl))
				if card < 1 {
					card = 1
				}
			}
		}
	}
	return properties.Cost{
		Cardinality: card * physicalWrapperCostMultiplier,
		CPU:         0,
	}
}

func (w *physicalExplodeWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalExplodeWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalExplodeWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

func (w *physicalExplodeWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalExplodeWrapper)(nil)
	_ physicalPlanExpression           = (*physicalExplodeWrapper)(nil)
)
