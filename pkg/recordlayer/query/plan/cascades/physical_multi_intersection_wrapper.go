package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// physicalMultiIntersectionWrapper adapts a
// *plans.RecordQueryMultiIntersectionOnValuesPlan to the
// RelationalExpression interface. Multi-intersection merges N
// compatibly-ordered aggregate index scans, combining grouping
// columns (taken from any stream — they're identical) with per-stream
// aggregate pick-up values.
type physicalMultiIntersectionWrapper struct {
	plan        *plans.RecordQueryMultiIntersectionOnValuesPlan
	innerQuants []expressions.Quantifier
}

func NewPhysicalMultiIntersectionWrapper(
	plan *plans.RecordQueryMultiIntersectionOnValuesPlan,
	innerQuants []expressions.Quantifier,
) *physicalMultiIntersectionWrapper {
	copied := make([]expressions.Quantifier, len(innerQuants))
	copy(copied, innerQuants)
	return &physicalMultiIntersectionWrapper{plan: plan, innerQuants: copied}
}

func (w *physicalMultiIntersectionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalMultiIntersectionWrapper) GetResultValue() values.Value {
	if w.plan != nil && w.plan.GetResultValue() != nil {
		return w.plan.GetResultValue()
	}
	if len(w.innerQuants) == 0 {
		return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	}
	return w.innerQuants[0].GetFlowedObjectValue()
}

func (w *physicalMultiIntersectionWrapper) GetQuantifiers() []expressions.Quantifier {
	return w.innerQuants
}

// IsIntersection implements properties.IntersectionExpression.
func (w *physicalMultiIntersectionWrapper) IsIntersection() {}

func (w *physicalMultiIntersectionWrapper) CanCorrelate() bool  { return false }
func (w *physicalMultiIntersectionWrapper) ChildrenAsSet() bool { return false }

func (w *physicalMultiIntersectionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalMultiIntersectionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalMultiIntersectionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalMultiIntersectionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physmultiintersectionwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalMultiIntersectionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalMultiIntersectionWrapper{plan: w.plan, innerQuants: copied}, nil
}

// HintCost: multi-intersection cardinality is bounded by the smallest
// child (same as regular intersection). CPU sums children + per-output
// merge work. When children aren't available as quantifiers (leaf-style
// embedding), estimates cardinality from DistinctSelectivity since
// the output produces one row per distinct group.
func (w *physicalMultiIntersectionWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		groupCard := properties.LeafScanCardinality * properties.DistinctSelectivity
		nChildren := len(w.plan.GetChildren())
		if nChildren < 1 {
			nChildren = 1
		}
		return properties.Cost{
			Cardinality: groupCard,
			CPU:         groupCard * properties.IntersectionCPU * float64(nChildren),
		}
	}
	// Single source of truth (cost_formulas.go) — shared with concretePlanCost.
	return intersectionCost(child)
}

// HintOrdering: multi-intersection output is ordered by the comparison
// key (grouping columns).
func (w *physicalMultiIntersectionWrapper) HintOrdering() properties.Ordering {
	if w.plan == nil {
		return properties.Ordering{}
	}
	compKey := w.plan.GetComparisonKey()
	if len(compKey) == 0 {
		return properties.Ordering{}
	}
	return properties.Ordering{IsKnown: true, Keys: compKey}
}

func (w *physicalMultiIntersectionWrapper) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(w.innerQuants) {
		panic(fmt.Sprintf("physicalMultiIntersectionWrapper.WithQuantifiers: expected %d, got %d", len(w.innerQuants), len(qs)))
	}
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalMultiIntersectionWrapper{plan: w.plan, innerQuants: copied}
}

var (
	_ expressions.RelationalExpression = (*physicalMultiIntersectionWrapper)(nil)
	_ physicalPlanExpression           = (*physicalMultiIntersectionWrapper)(nil)
)
