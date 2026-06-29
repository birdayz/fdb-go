package cascades

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// physicalVectorIndexScanWrapper adapts a RecordQueryVectorIndexPlan (a
// BY_DISTANCE K-NN scan) to a physical RelationalExpression for the memo. It
// mirrors physicalIndexScanWrapper but reports a small cardinality (the K-NN
// scan returns at most k rows), so the cost model prefers it over a
// partition-scan-plus-residual-distance-filter.
type physicalVectorIndexScanWrapper struct {
	plan *plans.RecordQueryVectorIndexPlan
}

func (w *physicalVectorIndexScanWrapper) GetPlan() *plans.RecordQueryVectorIndexPlan {
	return w.plan
}

func (w *physicalVectorIndexScanWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalVectorIndexScanWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalVectorIndexScanWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalVectorIndexScanWrapper) CanCorrelate() bool                       { return false }
func (w *physicalVectorIndexScanWrapper) ChildrenAsSet() bool                      { return false }

func (w *physicalVectorIndexScanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalVectorIndexScanWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalVectorIndexScanWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

func (w *physicalVectorIndexScanWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physvectorindexscanwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalVectorIndexScanWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalVectorIndexScanWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

func (w *physicalVectorIndexScanWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalVectorIndexScanWrapper)(nil)

// HintOrdering: an HNSW scan returns rows in distance order, which is not a
// column ordering the planner models — report unknown (empty) ordering.
func (w *physicalVectorIndexScanWrapper) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

func (w *physicalVectorIndexScanWrapper) HintRichOrdering() *RichOrdering {
	return EmptyOrdering()
}

// HintCost: a K-NN vector scan returns at most k rows (a small, bounded
// result), so its cardinality is k (defaulting to a small constant when k is
// not a plan-time literal). Far cheaper than scanning a partition and applying
// a residual distance filter.
func (w *physicalVectorIndexScanWrapper) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	card := vectorScanCardinality(w.plan)
	return properties.Cost{Cardinality: card * physicalWrapperCostMultiplier, CPU: 0}
}

// defaultVectorHorizon is the bounded re-ranked horizon an ordered-stream scan
// is costed at when ef_search is unspecified (mirrors the executor's
// defaultVectorEfSearch). It is deliberately >> any reasonable k so that, for a
// no-residual query, SinkLimitIntoVectorScanRule's folded self-limiting scan
// (cardinality k) out-costs the un-folded Limit-over-ordered-scan (cardinality
// horizon) and wins — restoring the legacy one-shot fast path.
const defaultVectorHorizon = 200.0

// vectorScanCardinality returns the plan-time top-K when it is a literal int,
// else a small default. An ORDERED-STREAM scan (RFC-156 Phase B) is NOT self-
// limited to k — it streams its fixed re-ranked horizon (the re-rank budget,
// decoupled from the probe width) — so it is costed at the horizon, not k. This
// makes a residual-bearing ordered scan correctly more expensive than a folded
// top-k scan, and lets the SinkLimit fold win whenever it is applicable.
func vectorScanCardinality(plan *plans.RecordQueryVectorIndexPlan) float64 {
	const defaultK = 10.0
	if plan == nil || plan.GetK() == nil {
		return defaultK
	}
	if plan.IsOrderedStream() {
		return defaultVectorHorizon
	}
	// Plan-time cost estimation: a non-constant or erroring K declines to the
	// default cardinality rather than failing planning.
	kv, err := plan.GetK().Evaluate(nil)
	if err != nil {
		return defaultK
	}
	switch n := kv.(type) {
	case int:
		if n > 0 {
			return float64(n)
		}
	case int32:
		if n > 0 {
			return float64(n)
		}
	case int64:
		if n > 0 {
			return float64(n)
		}
	}
	return defaultK
}

var _ physicalPlanExpression = (*physicalVectorIndexScanWrapper)(nil)
