package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// IsPhysicalRecursiveDfsJoin reports whether the given RelationalExpression
// is a physicalRecursiveDfsJoinWrapper.
func IsPhysicalRecursiveDfsJoin(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalRecursiveDfsJoinWrapper)
	return ok
}

type physicalRecursiveDfsJoinWrapper struct {
	plan       *plans.RecordQueryRecursiveDfsJoinPlan
	rootQuant  expressions.Quantifier
	childQuant expressions.Quantifier
}

func newPhysicalRecursiveDfsJoinWrapper(
	plan *plans.RecordQueryRecursiveDfsJoinPlan,
	rootQuant, childQuant expressions.Quantifier,
) *physicalRecursiveDfsJoinWrapper {
	return &physicalRecursiveDfsJoinWrapper{
		plan:       plan,
		rootQuant:  rootQuant,
		childQuant: childQuant,
	}
}

func (w *physicalRecursiveDfsJoinWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalRecursiveDfsJoinWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalRecursiveDfsJoinWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.rootQuant, w.childQuant}
}

func (w *physicalRecursiveDfsJoinWrapper) CanCorrelate() bool  { return true }
func (w *physicalRecursiveDfsJoinWrapper) ChildrenAsSet() bool { return false }

func (w *physicalRecursiveDfsJoinWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalRecursiveDfsJoinWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalRecursiveDfsJoinWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalRecursiveDfsJoinWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physrecdfswrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// HintCost: recursive DFS is O(root + depth*child) but depth is
// unknown at plan time. Use root×child as a pessimistic upper bound.
func (w *physicalRecursiveDfsJoinWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	rootCard := child[0].Cardinality
	childCard := child[1].Cardinality
	if rootCard == 0 {
		rootCard = properties.LeafScanCardinality
	}
	if childCard == 0 {
		childCard = properties.LeafScanCardinality
	}
	outCard := rootCard * childCard * physicalWrapperCostMultiplier
	cpu := (child[0].CPU + rootCard*child[1].CPU) * physicalWrapperCostMultiplier
	return properties.Cost{
		Cardinality: outCard,
		CPU:         cpu,
	}
}

func (w *physicalRecursiveDfsJoinWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalRecursiveDfsJoinWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("physicalRecursiveDfsJoinWrapper.WithChildren: expected 2 children, got %d", len(qs))
	}
	return &physicalRecursiveDfsJoinWrapper{plan: w.plan, rootQuant: qs[0], childQuant: qs[1]}, nil
}

func (w *physicalRecursiveDfsJoinWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalRecursiveDfsJoinWrapper)(nil)
	_ physicalPlanExpression           = (*physicalRecursiveDfsJoinWrapper)(nil)
)
