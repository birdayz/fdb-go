package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type physicalUnorderedUnionWrapper struct {
	plan        *plans.RecordQueryUnorderedUnionPlan
	innerQuants []expressions.Quantifier
}

func NewPhysicalUnorderedUnionWrapper(
	plan *plans.RecordQueryUnorderedUnionPlan,
	innerQuants []expressions.Quantifier,
) *physicalUnorderedUnionWrapper {
	copied := make([]expressions.Quantifier, len(innerQuants))
	copy(copied, innerQuants)
	return &physicalUnorderedUnionWrapper{plan: plan, innerQuants: copied}
}

func (w *physicalUnorderedUnionWrapper) GetPlan() *plans.RecordQueryUnorderedUnionPlan {
	return w.plan
}

func (w *physicalUnorderedUnionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalUnorderedUnionWrapper) GetResultValue() values.Value {
	if len(w.innerQuants) == 0 {
		return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	}
	return w.innerQuants[0].GetFlowedObjectValue()
}

func (w *physicalUnorderedUnionWrapper) GetQuantifiers() []expressions.Quantifier {
	return w.innerQuants
}

func (w *physicalUnorderedUnionWrapper) CanCorrelate() bool  { return false }
func (w *physicalUnorderedUnionWrapper) ChildrenAsSet() bool { return true }

func (w *physicalUnorderedUnionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalUnorderedUnionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	_, ok := other.(*physicalUnorderedUnionWrapper)
	return ok
}

func (w *physicalUnorderedUnionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physunorderedunionwrap|"))
	return h.Sum64()
}

func (w *physicalUnorderedUnionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalUnorderedUnionWrapper{plan: w.plan, innerQuants: copied}, nil
}

func (w *physicalUnorderedUnionWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
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

func (w *physicalUnorderedUnionWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

func (w *physicalUnorderedUnionWrapper) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(w.innerQuants) {
		panic(fmt.Sprintf("physicalUnorderedUnionWrapper.WithQuantifiers: expected %d, got %d", len(w.innerQuants), len(qs)))
	}
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalUnorderedUnionWrapper{plan: w.plan, innerQuants: copied}
}

var (
	_ expressions.RelationalExpression = (*physicalUnorderedUnionWrapper)(nil)
	_ physicalPlanExpression           = (*physicalUnorderedUnionWrapper)(nil)
)
