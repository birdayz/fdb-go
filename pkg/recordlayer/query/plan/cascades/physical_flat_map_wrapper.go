package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalFlatMapWrapper struct {
	plan       plans.RecordQueryPlan
	outerQuant expressions.Quantifier
	innerQuant expressions.Quantifier
}

func newPhysicalFlatMapWrapper(
	plan plans.RecordQueryPlan,
	outerQuant, innerQuant expressions.Quantifier,
) *physicalFlatMapWrapper {
	return &physicalFlatMapWrapper{
		plan:       plan,
		outerQuant: outerQuant,
		innerQuant: innerQuant,
	}
}

func (w *physicalFlatMapWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalFlatMapWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalFlatMapWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.outerQuant, w.innerQuant}
}

func (w *physicalFlatMapWrapper) CanCorrelate() bool  { return true }
func (w *physicalFlatMapWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns empty. Java's
// RecordQueryFlatMapPlan returns resultValue.getCorrelatedTo() here,
// which matters for correlated subqueries where the result value
// references outer correlations. For current Go usage (joins only),
// the result value's correlations are inner/outer aliases which are
// children — excluded by the "WithoutChildren" semantics. Revisit
// when correlated subqueries are ported.
func (w *physicalFlatMapWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalFlatMapWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalFlatMapWrapper)
	if !ok {
		return false
	}
	if w.plan == nil || o.plan == nil {
		return w.plan == nil && o.plan == nil
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalFlatMapWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physflatmap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalFlatMapWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	outerCard := child[0].Cardinality
	if outerCard == 0 {
		outerCard = properties.LeafScanCardinality
	}
	// FlatMap with correlated index probe: inner cost is O(logM) per outer row.
	innerCPU := child[1].CPU
	if innerCPU == 0 {
		innerCPU = properties.FilterCPU
	}
	outCard := outerCard * properties.FilterSelectivity * physicalWrapperCostMultiplier
	cpu := (child[0].CPU + outerCard*innerCPU) * physicalWrapperCostMultiplier
	return properties.Cost{
		Cardinality: outCard,
		CPU:         cpu,
	}
}

func (w *physicalFlatMapWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

func (w *physicalFlatMapWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("physicalFlatMapWrapper.WithChildren: expected 2 children, got %d", len(qs))
	}
	return &physicalFlatMapWrapper{plan: w.plan, outerQuant: qs[0], innerQuant: qs[1]}, nil
}

func (w *physicalFlatMapWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

// IsPhysicalFlatMap reports whether expr is a physicalFlatMapWrapper.
func IsPhysicalFlatMap(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalFlatMapWrapper)
	return ok
}

var (
	_ expressions.RelationalExpression = (*physicalFlatMapWrapper)(nil)
	_ physicalPlanExpression           = (*physicalFlatMapWrapper)(nil)
)
