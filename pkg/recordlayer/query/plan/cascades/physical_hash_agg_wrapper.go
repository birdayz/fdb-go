package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// IsPhysicalHashAgg reports whether the given RelationalExpression is
// a physicalHashAggWrapper.
func IsPhysicalHashAgg(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalHashAggWrapper)
	return ok
}

// physicalHashAggWrapper adapts a *plans.RecordQueryHashAggregationPlan
// to the RelationalExpression interface.
type physicalHashAggWrapper struct {
	plan       *plans.RecordQueryHashAggregationPlan
	innerQuant expressions.Quantifier
}

func newPhysicalHashAggWrapper(plan *plans.RecordQueryHashAggregationPlan, innerQuant expressions.Quantifier) *physicalHashAggWrapper {
	return &physicalHashAggWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalHashAggWrapper) GetPlan() *plans.RecordQueryHashAggregationPlan { return w.plan }
func (w *physicalHashAggWrapper) GetRecordQueryPlan() plans.RecordQueryPlan      { return w.plan }

func (w *physicalHashAggWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalHashAggWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalHashAggWrapper) CanCorrelate() bool  { return false }
func (w *physicalHashAggWrapper) ChildrenAsSet() bool { return false }

func (w *physicalHashAggWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalHashAggWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalHashAggWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalHashAggWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physhashaggwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalHashAggWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalHashAggWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	return &physicalHashAggWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalHashAggWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	outCard := in * properties.DistinctSelectivity * physicalWrapperCostMultiplier
	return properties.Cost{
		Cardinality: outCard,
		CPU:         (child[0].CPU + in*properties.DistinctCPU) * physicalWrapperCostMultiplier,
	}
}

// HintOrdering: hash aggregation does NOT provide any output ordering —
// groups come out in hash-bucket order.
func (w *physicalHashAggWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalHashAggWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalHashAggWrapper)(nil)
	_ physicalPlanExpression           = (*physicalHashAggWrapper)(nil)
)
