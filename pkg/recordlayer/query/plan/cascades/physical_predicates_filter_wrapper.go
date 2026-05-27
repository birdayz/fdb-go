package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalPredicatesFilterWrapper struct {
	plan       *plans.RecordQueryPredicatesFilterPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalPredicatesFilterWrapper(
	plan *plans.RecordQueryPredicatesFilterPlan,
	innerQuant expressions.Quantifier,
) *physicalPredicatesFilterWrapper {
	return &physicalPredicatesFilterWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalPredicatesFilterWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalPredicatesFilterWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

func (w *physicalPredicatesFilterWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalPredicatesFilterWrapper) CanCorrelate() bool  { return false }
func (w *physicalPredicatesFilterWrapper) ChildrenAsSet() bool { return false }

func (w *physicalPredicatesFilterWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	if w.plan != nil {
		for _, p := range w.plan.GetPredicates() {
			for k := range predicates.GetCorrelatedToOfPredicate(p) {
				out[k] = struct{}{}
			}
		}
	}
	return out
}

func (w *physicalPredicatesFilterWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalPredicatesFilterWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalPredicatesFilterWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physpredsfilterwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalPredicatesFilterWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalPredicatesFilterWrapper.WithChildren: expected 1, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		var newPlan *plans.RecordQueryPredicatesFilterPlan
		if alias := w.plan.GetInnerAlias(); alias.Name() != "" {
			newPlan = plans.NewRecordQueryPredicatesFilterPlanWithAlias(innerPlan, w.plan.GetPredicates(), alias)
		} else {
			newPlan = plans.NewRecordQueryPredicatesFilterPlan(innerPlan, w.plan.GetPredicates())
		}
		return &physicalPredicatesFilterWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalPredicatesFilterWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalPredicatesFilterWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 || w.plan == nil {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	numPreds := len(w.plan.GetPredicates())
	if numPreds == 0 {
		numPreds = 1
	}
	sel := properties.FilterSelectivity
	for i := 1; i < numPreds; i++ {
		sel *= properties.FilterSelectivity
	}
	return properties.Cost{
		Cardinality: in * sel * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.FilterCPU*float64(numPreds)) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalPredicatesFilterWrapper) HintOrdering() properties.Ordering {
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

func (w *physicalPredicatesFilterWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalPredicatesFilterWrapper)(nil)
	_ physicalPlanExpression           = (*physicalPredicatesFilterWrapper)(nil)
)
