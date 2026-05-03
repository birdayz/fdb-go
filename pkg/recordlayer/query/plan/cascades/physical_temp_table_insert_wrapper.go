package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalTempTableInsertWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physttinsertwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalTempTableInsertWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) < 1 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality * physicalWrapperCostMultiplier,
		CPU:         child[0].CPU * physicalWrapperCostMultiplier,
	}
}

func (w *physicalTempTableInsertWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalTempTableInsertWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalTempTableInsertWrapper.WithChildren: expected 1 child, got %d", len(qs))
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
