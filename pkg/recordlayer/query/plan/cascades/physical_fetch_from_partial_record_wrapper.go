package cascades

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// physicalFetchFromPartialRecordWrapper adapts a
// *plans.RecordQueryFetchFromPartialRecordPlan to the
// RelationalExpression interface. Single-inner shape: one Quantifier
// ranging over the child (typically a covering index scan).
type physicalFetchFromPartialRecordWrapper struct {
	plan       *plans.RecordQueryFetchFromPartialRecordPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalFetchFromPartialRecordWrapper constructs the wrapper.
func NewPhysicalFetchFromPartialRecordWrapper(
	plan *plans.RecordQueryFetchFromPartialRecordPlan,
	innerQuant expressions.Quantifier,
) *physicalFetchFromPartialRecordWrapper {
	return &physicalFetchFromPartialRecordWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalFetchFromPartialRecordWrapper) GetPlan() *plans.RecordQueryFetchFromPartialRecordPlan {
	return w.plan
}

func (w *physicalFetchFromPartialRecordWrapper) GetRecordQueryPlan() plans.RecordQueryPlan {
	return w.plan
}

func (w *physicalFetchFromPartialRecordWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalFetchFromPartialRecordWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalFetchFromPartialRecordWrapper) CanCorrelate() bool  { return false }
func (w *physicalFetchFromPartialRecordWrapper) ChildrenAsSet() bool { return false }

func (w *physicalFetchFromPartialRecordWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalFetchFromPartialRecordWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalFetchFromPartialRecordWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsWithoutChildren(o.plan)
}

func (w *physicalFetchFromPartialRecordWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physfetchpartialwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalFetchFromPartialRecordWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalFetchFromPartialRecordWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
			innerPlan,
			w.plan.GetTranslateValueFunction(),
			w.plan.GetResultType(),
			w.plan.GetFetchIndexRecords(),
		)
		return NewPhysicalFetchFromPartialRecordWrapper(newPlan, qs[0]), nil
	}
	return NewPhysicalFetchFromPartialRecordWrapper(w.plan, qs[0]), nil
}

func (w *physicalFetchFromPartialRecordWrapper) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.FetchCPU) * physicalWrapperCostMultiplier,
	}
}

func (w *physicalFetchFromPartialRecordWrapper) HintOrdering() properties.Ordering {
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

func (w *physicalFetchFromPartialRecordWrapper) HintRichOrdering() *RichOrdering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return EmptyOrdering()
	}
	for _, m := range ref.AllMembers() {
		if rh, ok := m.(RichOrderingHinter); ok {
			return rh.HintRichOrdering()
		}
	}
	return EmptyOrdering()
}

func (w *physicalFetchFromPartialRecordWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

// IsPhysicalFetchFromPartialRecord reports whether the given
// RelationalExpression is a physicalFetchFromPartialRecordWrapper.
func IsPhysicalFetchFromPartialRecord(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalFetchFromPartialRecordWrapper)
	return ok
}

// GetPhysicalFetchFromPartialRecordPlan returns the underlying plan if
// expr is a physicalFetchFromPartialRecordWrapper, nil otherwise.
func GetPhysicalFetchFromPartialRecordPlan(expr expressions.RelationalExpression) *plans.RecordQueryFetchFromPartialRecordPlan {
	w, ok := expr.(*physicalFetchFromPartialRecordWrapper)
	if !ok {
		return nil
	}
	return w.plan
}

var (
	_ expressions.RelationalExpression = (*physicalFetchFromPartialRecordWrapper)(nil)
	_ physicalPlanExpression           = (*physicalFetchFromPartialRecordWrapper)(nil)
)
