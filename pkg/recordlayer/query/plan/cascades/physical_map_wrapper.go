package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalMapWrapper struct {
	plan       *plans.RecordQueryMapPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalMapWrapper(
	plan *plans.RecordQueryMapPlan,
	innerQuant expressions.Quantifier,
) *physicalMapWrapper {
	return &physicalMapWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalMapWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalMapWrapper) GetResultValue() values.Value {
	if w.plan != nil {
		return w.plan.GetResultValue()
	}
	return w.innerQuant.GetFlowedObjectValue()
}

func (w *physicalMapWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalMapWrapper) CanCorrelate() bool  { return false }
func (w *physicalMapWrapper) ChildrenAsSet() bool { return false }

func (w *physicalMapWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalMapWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalMapWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalMapWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physmapwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalMapWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalMapWrapper.WithChildren: expected 1, got %d", len(qs))
	}
	return &physicalMapWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalMapWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + child[0].Cardinality*0.01) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalMapWrapper) HintOrdering() properties.Ordering {
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

func (w *physicalMapWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalMapWrapper)(nil)
	_ physicalPlanExpression           = (*physicalMapWrapper)(nil)
)
