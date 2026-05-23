package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// IsPhysicalRecursiveLevelUnion reports whether the given
// RelationalExpression is a physicalRecursiveLevelUnionWrapper.
func IsPhysicalRecursiveLevelUnion(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalRecursiveLevelUnionWrapper)
	return ok
}

type physicalRecursiveLevelUnionWrapper struct {
	plan           *plans.RecordQueryRecursiveLevelUnionPlan
	initialQuant   expressions.Quantifier
	recursiveQuant expressions.Quantifier
}

func newPhysicalRecursiveLevelUnionWrapper(
	plan *plans.RecordQueryRecursiveLevelUnionPlan,
	initialQuant, recursiveQuant expressions.Quantifier,
) *physicalRecursiveLevelUnionWrapper {
	return &physicalRecursiveLevelUnionWrapper{
		plan:           plan,
		initialQuant:   initialQuant,
		recursiveQuant: recursiveQuant,
	}
}

func (w *physicalRecursiveLevelUnionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan {
	return w.plan
}

func (w *physicalRecursiveLevelUnionWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalRecursiveLevelUnionWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.initialQuant, w.recursiveQuant}
}

func (w *physicalRecursiveLevelUnionWrapper) CanCorrelate() bool  { return true }
func (w *physicalRecursiveLevelUnionWrapper) ChildrenAsSet() bool { return false }

func (w *physicalRecursiveLevelUnionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalRecursiveLevelUnionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalRecursiveLevelUnionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalRecursiveLevelUnionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physreclevelwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// HintCost: level-order traversal is O(initial + levels*recursive)
// but levels are unknown at plan time. Use initial×recursive as a
// pessimistic bound, same as the DFS wrapper.
func (w *physicalRecursiveLevelUnionWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	initCard := child[0].Cardinality
	recCard := child[1].Cardinality
	if initCard == 0 {
		initCard = properties.LeafScanCardinality
	}
	if recCard == 0 {
		recCard = properties.LeafScanCardinality
	}
	outCard := initCard * recCard * physicalWrapperCostMultiplier
	cpu := (child[0].CPU + initCard*child[1].CPU) * physicalWrapperCostMultiplier
	return properties.Cost{
		Cardinality: outCard,
		CPU:         cpu,
	}
}

func (w *physicalRecursiveLevelUnionWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalRecursiveLevelUnionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("physicalRecursiveLevelUnionWrapper.WithChildren: expected 2 children, got %d", len(qs))
	}
	return &physicalRecursiveLevelUnionWrapper{plan: w.plan, initialQuant: qs[0], recursiveQuant: qs[1]}, nil
}

func (w *physicalRecursiveLevelUnionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalRecursiveLevelUnionWrapper)(nil)
	_ physicalPlanExpression           = (*physicalRecursiveLevelUnionWrapper)(nil)
)
