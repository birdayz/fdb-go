package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	return w.plan.EqualsPlanWithoutChildren(o.plan)
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

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalMergeSortUnionWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalMergeSortUnionWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
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
