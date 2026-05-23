package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalMergeSortUnionWrapper struct {
	plan        *plans.RecordQueryMergeSortUnionPlan
	innerQuants []expressions.Quantifier
}

func NewPhysicalMergeSortUnionWrapper(
	plan *plans.RecordQueryMergeSortUnionPlan,
	innerQuants []expressions.Quantifier,
) *physicalMergeSortUnionWrapper {
	copied := make([]expressions.Quantifier, len(innerQuants))
	copy(copied, innerQuants)
	return &physicalMergeSortUnionWrapper{plan: plan, innerQuants: copied}
}

func (w *physicalMergeSortUnionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalMergeSortUnionWrapper) GetResultValue() values.Value {
	if len(w.innerQuants) == 0 {
		return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	}
	return w.innerQuants[0].GetFlowedObjectValue()
}

func (w *physicalMergeSortUnionWrapper) GetQuantifiers() []expressions.Quantifier {
	return w.innerQuants
}

func (w *physicalMergeSortUnionWrapper) CanCorrelate() bool  { return false }
func (w *physicalMergeSortUnionWrapper) ChildrenAsSet() bool { return true }

func (w *physicalMergeSortUnionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalMergeSortUnionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalMergeSortUnionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalMergeSortUnionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physmergesortunionwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalMergeSortUnionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalMergeSortUnionWrapper{plan: w.plan, innerQuants: copied}, nil
}

func (w *physicalMergeSortUnionWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	sumCard := 0.0
	sumCPU := 0.0
	for _, c := range child {
		sumCard += c.Cardinality
		sumCPU += c.CPU
	}
	return properties.Cost{
		Cardinality: sumCard * physicalWrapperCostMultiplier,
		CPU:         (sumCPU + sumCard*properties.UnionCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalMergeSortUnionWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: true, Keys: w.plan.GetComparisonKeys()}
}

func (w *physicalMergeSortUnionWrapper) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(w.innerQuants) {
		panic(fmt.Sprintf("physicalMergeSortUnionWrapper.WithQuantifiers: expected %d, got %d", len(w.innerQuants), len(qs)))
	}
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalMergeSortUnionWrapper{plan: w.plan, innerQuants: copied}
}

var (
	_ expressions.RelationalExpression = (*physicalMergeSortUnionWrapper)(nil)
	_ physicalPlanExpression           = (*physicalMergeSortUnionWrapper)(nil)
)
