package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// IsPhysicalLimit reports whether the given RelationalExpression is a
// physicalLimitWrapper.
func IsPhysicalLimit(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalLimitWrapper)
	return ok
}

type physicalLimitWrapper struct {
	plan       *plans.RecordQueryLimitPlan
	innerQuant expressions.Quantifier
}

func newPhysicalLimitWrapper(plan *plans.RecordQueryLimitPlan, innerQuant expressions.Quantifier) *physicalLimitWrapper {
	return &physicalLimitWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalLimitWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalLimitWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalLimitWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalLimitWrapper) CanCorrelate() bool  { return false }
func (w *physicalLimitWrapper) ChildrenAsSet() bool { return false }

func (w *physicalLimitWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalLimitWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalLimitWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalLimitWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physlimitwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// HintCost: LIMIT reduces cardinality to min(child, limit).
func (w *physicalLimitWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	outCard := child[0].Cardinality
	if w.plan != nil && w.plan.GetLimit() > 0 {
		limitF := float64(w.plan.GetLimit())
		if limitF < outCard {
			outCard = limitF
		}
	}
	return properties.Cost{
		Cardinality: outCard * physicalWrapperCostMultiplier,
		CPU:         child[0].CPU * physicalWrapperCostMultiplier,
	}
}

// HintOrdering: LIMIT preserves inner ordering.
func (w *physicalLimitWrapper) HintOrdering() properties.Ordering {
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

func (w *physicalLimitWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalLimitWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryLimitPlan(innerPlan, w.plan.GetLimit(), w.plan.GetOffset())
		return &physicalLimitWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalLimitWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalLimitWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalLimitWrapper)(nil)
	_ physicalPlanExpression           = (*physicalLimitWrapper)(nil)
)
