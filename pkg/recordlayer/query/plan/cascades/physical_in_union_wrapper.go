package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalInUnionWrapper struct {
	plan       *plans.RecordQueryInUnionPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalInUnionWrapper(
	plan *plans.RecordQueryInUnionPlan,
	innerQuant expressions.Quantifier,
) *physicalInUnionWrapper {
	return &physicalInUnionWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalInUnionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalInUnionWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

func (w *physicalInUnionWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalInUnionWrapper) CanCorrelate() bool  { return false }
func (w *physicalInUnionWrapper) ChildrenAsSet() bool { return false }

func (w *physicalInUnionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalInUnionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalInUnionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalInUnionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physinunionwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalInUnionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalInUnionWrapper.WithChildren: expected 1, got %d", len(qs))
	}
	return &physicalInUnionWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalInUnionWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	inDims := float64(len(w.plan.GetBindingNames()))
	if inDims < 1 {
		inDims = 10
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * inDims * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*inDims*properties.UnionCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalInUnionWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: true, Keys: w.plan.GetComparisonKeys()}
}

func (w *physicalInUnionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalInUnionWrapper)(nil)
	_ physicalPlanExpression           = (*physicalInUnionWrapper)(nil)
)
