// Go extension — no Java equivalent.
//
// Wraps RecordQueryInMemorySortPlan as a RelationalExpression for the
// Cascades Memo. Java has no physical sort operator; this is a Go-only
// fallback for ORDER BY without a supporting index.
package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

type physicalInMemorySortWrapper struct {
	plan       *plans.RecordQueryInMemorySortPlan
	innerQuant expressions.Quantifier
}

func newPhysicalInMemorySortWrapper(plan *plans.RecordQueryInMemorySortPlan, innerQuant expressions.Quantifier) *physicalInMemorySortWrapper {
	return &physicalInMemorySortWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalInMemorySortWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalInMemorySortWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalInMemorySortWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalInMemorySortWrapper) CanCorrelate() bool  { return false }
func (w *physicalInMemorySortWrapper) ChildrenAsSet() bool { return false }

func (w *physicalInMemorySortWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalInMemorySortWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalInMemorySortWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalInMemorySortWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physimsort|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalInMemorySortWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalInMemorySortWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryInMemorySortPlan(innerPlan, w.plan.GetSortKeys())
		return &physicalInMemorySortWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalInMemorySortWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

func (w *physicalInMemorySortWrapper) HintOrdering() properties.Ordering {
	if w.plan == nil {
		return properties.Ordering{}
	}
	keys := make([]values.Value, len(w.plan.GetSortKeys()))
	desc := make([]bool, len(w.plan.GetSortKeys()))
	for i, sk := range w.plan.GetSortKeys() {
		keys[i] = &values.FieldValue{Field: sk.Field, Typ: values.UnknownType}
		desc[i] = sk.Desc
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// HintCost: in-memory sort is expensive — materialize + O(n log n).
// Must be more expensive than index-based sort elimination so Cascades
// prefers indexes when available.
func (w *physicalInMemorySortWrapper) HintCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	n := child[0].Cardinality
	if n < 1 {
		n = 1
	}
	sortCPU := n * 0.1 // O(n log n) approximated; 10x more expensive than filter
	return properties.Cost{
		Cardinality: n,
		CPU:         child[0].CPU + sortCPU,
	}
}

func (w *physicalInMemorySortWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var (
	_ expressions.RelationalExpression = (*physicalInMemorySortWrapper)(nil)
	_ physicalPlanExpression           = (*physicalInMemorySortWrapper)(nil)
)
