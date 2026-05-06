package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// IsPhysicalStreamingAgg reports whether the given RelationalExpression
// is a physicalStreamingAggWrapper.
func IsPhysicalStreamingAgg(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalStreamingAggWrapper)
	return ok
}

// physicalStreamingAggWrapper adapts a
// *plans.RecordQueryStreamingAggregationPlan to the
// RelationalExpression interface. Single inner Quantifier — same
// shape as physicalDistinctWrapper.
type physicalStreamingAggWrapper struct {
	plan       *plans.RecordQueryStreamingAggregationPlan
	innerQuant expressions.Quantifier
}

func newPhysicalStreamingAggWrapper(plan *plans.RecordQueryStreamingAggregationPlan, innerQuant expressions.Quantifier) *physicalStreamingAggWrapper {
	return &physicalStreamingAggWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalStreamingAggWrapper) GetPlan() *plans.RecordQueryStreamingAggregationPlan {
	return w.plan
}
func (w *physicalStreamingAggWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalStreamingAggWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalStreamingAggWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalStreamingAggWrapper) CanCorrelate() bool  { return false }
func (w *physicalStreamingAggWrapper) ChildrenAsSet() bool { return false }

func (w *physicalStreamingAggWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalStreamingAggWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalStreamingAggWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalStreamingAggWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physstreamaggwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalStreamingAggWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalStreamingAggWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	return &physicalStreamingAggWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: streaming aggregation is cheap — one pass over sorted
// input, output cardinality reduced by DistinctSelectivity. Cheaper
// than hash because no hash table is built (O(1) memory per group).
func (w *physicalStreamingAggWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * properties.DistinctSelectivity * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.DistinctCPU*0.8) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalStreamingAggWrapper) HintOrdering() properties.Ordering {
	if w.plan == nil || len(w.plan.GetGroupingKeys()) == 0 {
		return properties.Ordering{IsKnown: false}
	}
	keys := make([]values.Value, len(w.plan.GetGroupingKeys()))
	copy(keys, w.plan.GetGroupingKeys())
	desc := make([]bool, len(keys))
	if idx, ok := w.plan.GetInner().(*plans.RecordQueryIndexPlan); ok && idx.IsReverse() {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

func (w *physicalStreamingAggWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalStreamingAggWrapper)(nil)
	_ physicalPlanExpression           = (*physicalStreamingAggWrapper)(nil)
)
