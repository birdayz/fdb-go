package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	return w.plan.EqualsPlanWithoutChildren(o.plan)
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
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalLimitWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering: LIMIT preserves inner ordering.
// OrderingSourceRef: this wrapper PRESERVES its input's order (see
// orderingDelegator in winner_lookup.go).
func (w *physicalLimitWrapper) OrderingSourceRef() *expressions.Reference {
	return w.innerQuant.GetRangesOver()
}

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
	// Always relink to the extracted inner — do NOT gate on isLeafReplaceable.
	// LIMIT is a transparent unary cap; like the fetch/projection/in-memory-sort
	// wrappers, WithChildren runs only at extraction where qs[0] resolves to the
	// fully-formed winner. Gating on isLeafReplaceable (which excludes Projection,
	// InJoin, etc.) left the eagerly-snapshotted nil-inner plan in place for a
	// top-level `LIMIT` over a `Projection` over an IN-join data access, so
	// `... WHERE c IN (...) LIMIT k` extracted `Limit(Project(Fetch(<nil>)))` /
	// `Limit(Project(InJoin(<nil>)))` → 0 rows or an execution error. Same bug
	// class the fetch wrapper fixed under RFC-070.
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil {
		// WithInner preserves the static OR runtime cap (limitValue) — rebuilding
		// via NewRecordQueryLimitPlan(GetLimit,...) would silently drop a runtime
		// parameterized cap and read the -1 sentinel as the literal limit.
		newPlan := w.plan.WithInner(innerPlan)
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
