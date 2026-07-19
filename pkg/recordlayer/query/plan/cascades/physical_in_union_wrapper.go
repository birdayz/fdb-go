package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	return w.plan.EqualsPlanWithoutChildren(o.plan)
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
	// Relink to the extracted inner. Without this the wrapper kept the
	// EAGER snapshot taken at yield time, so an IN-union over a compensated
	// pk-intersection extracted `InUnion(PredicatesFilter(<nil>))` — the
	// child wrapper relinked its own inner correctly, but this level still
	// pointed at the stale pre-relink plan. Same relink duty the fetch
	// (RFC-070) and limit wrappers carry; isLeafReplaceable gates the SWAP
	// of join-structured children, and an IN-union's inner is a plain
	// sub-plan, so the gate applies here as it does for the in-join.
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalInUnionWrapper{plan: w.plan.WithInner(innerPlan), innerQuant: qs[0]}, nil
	}
	return &physicalInUnionWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalInUnionWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalInUnionWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

func (w *physicalInUnionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalInUnionWrapper)(nil)
	_ physicalPlanExpression           = (*physicalInUnionWrapper)(nil)
)
