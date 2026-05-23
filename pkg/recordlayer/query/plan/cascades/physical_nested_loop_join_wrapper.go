package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalNestedLoopJoinWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalNestedLoopJoinWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
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
func (w *physicalNestedLoopJoinWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	outerCard := child[0].Cardinality
	innerCard := child[1].Cardinality
	if outerCard == 0 {
		outerCard = properties.LeafScanCardinality
	}
	if innerCard == 0 {
		innerCard = properties.LeafScanCardinality
	}
	outCard := outerCard * innerCard * properties.FilterSelectivity * physicalWrapperCostMultiplier
	cpu := (child[0].CPU + outerCard*child[1].CPU + outerCard*innerCard*properties.FilterCPU) * physicalWrapperCostMultiplier
	return properties.Cost{
		Cardinality: outCard,
		CPU:         cpu,
	}
}

func (w *physicalNestedLoopJoinWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
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
