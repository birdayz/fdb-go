package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// IsPhysicalStreamingAgg reports whether the given RelationalExpression
// is a physicalStreamingAggWrapper.
func IsPhysicalStreamingAgg(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalStreamingAggWrapper)
	return ok
}

// physicalStreamingAggWrapper adapts a
// *plans.RecordQueryStreamingAggregationPlan to the
// RelationalExpression interface. Single inner Quantifier — the same
// single-inner delegator shape the collapsed unary plans carry directly.
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
	// Flow a TYPED QOV whose RecordType is the aggregate's output schema
	// ([groupKeys, aggregates], the plan's single naming authority), so the resolver
	// BAKES downstream references to ordinals at plan time (Java's getFieldNameToOrdinalMap).
	// A downstream ref then reads the aggregateCursor's PositionalRow by Get(ordinal) — order,
	// not spelling — robust to redundant spellings of the same column.
	return values.NewQuantifiedObjectValueOfType(values.UniqueCorrelationIdentifier(), w.plan.OutputRecordType())
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
	return w.plan.EqualsPlanWithoutChildren(o.plan)
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
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryStreamingAggregationPlan(innerPlan, w.plan.GetGroupingKeys(), w.plan.GetAggregates())
		return &physicalStreamingAggWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalStreamingAggWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: streaming aggregation is cheap — one pass over sorted
// input, output cardinality reduced by DistinctSelectivity. Cheaper
// than hash because no hash table is built (O(1) memory per group).
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalStreamingAggWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalStreamingAggWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

func (w *physicalStreamingAggWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalStreamingAggWrapper)(nil)
	_ physicalPlanExpression           = (*physicalStreamingAggWrapper)(nil)
)
