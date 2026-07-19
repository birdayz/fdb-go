package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// IsPhysicalNestedLoopJoin reports whether the given RelationalExpression
// is a physicalNestedLoopJoinWrapper.
func IsPhysicalNestedLoopJoin(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalNestedLoopJoinWrapper)
	return ok
}

// physicalNestedLoopJoinWrapper adapts a
// *plans.RecordQueryNestedLoopJoinPlan to the RelationalExpression
// interface. Two inner Quantifiers (outer, inner).
type physicalNestedLoopJoinWrapper struct {
	plan       *plans.RecordQueryNestedLoopJoinPlan
	outerQuant expressions.Quantifier
	innerQuant expressions.Quantifier
}

func newPhysicalNestedLoopJoinWrapper(
	plan *plans.RecordQueryNestedLoopJoinPlan,
	outerQuant, innerQuant expressions.Quantifier,
) *physicalNestedLoopJoinWrapper {
	return &physicalNestedLoopJoinWrapper{
		plan:       plan,
		outerQuant: outerQuant,
		innerQuant: innerQuant,
	}
}

func (w *physicalNestedLoopJoinWrapper) GetPlan() *plans.RecordQueryNestedLoopJoinPlan {
	return w.plan
}

func (w *physicalNestedLoopJoinWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalNestedLoopJoinWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalNestedLoopJoinWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.outerQuant, w.innerQuant}
}

func (w *physicalNestedLoopJoinWrapper) CanCorrelate() bool  { return false }
func (w *physicalNestedLoopJoinWrapper) ChildrenAsSet() bool { return false }

func (w *physicalNestedLoopJoinWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
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

func (w *physicalNestedLoopJoinWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalNestedLoopJoinWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

func (w *physicalNestedLoopJoinWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physnljwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// HintCost: nested-loop join is O(outer × inner). The join predicate
// selectivity reduces the output cardinality.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalNestedLoopJoinWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalNestedLoopJoinWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

func (w *physicalNestedLoopJoinWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("physicalNestedLoopJoinWrapper.WithChildren: expected 2 children, got %d", len(qs))
	}
	return &physicalNestedLoopJoinWrapper{plan: w.plan, outerQuant: qs[0], innerQuant: qs[1]}, nil
}

func (w *physicalNestedLoopJoinWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalNestedLoopJoinWrapper)(nil)
	_ physicalPlanExpression           = (*physicalNestedLoopJoinWrapper)(nil)
)
