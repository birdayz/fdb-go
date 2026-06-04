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
	// Always relink to the extracted inner, including compound joins — do NOT
	// gate on isLeafReplaceable. A fetch is a transparent unary cap (like the
	// projection and in-memory sort); PushInJoinThroughFetchRule builds
	// `Fetch(InJoin(...))` with a nil-inner fetch plan (the InJoin lives in the
	// wrapper quantifier). WithChildren runs only at extraction, where qs[0]
	// resolves to the fully-formed winner; without relinking, `SELECT id+100
	// ... WHERE a IN (...)` (where the expression is not pushable, so the fetch
	// survives) extracts `Fetch(<nil>)` and returns 0 rows (RFC-070).
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil {
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
	// Single source of truth (cost_formulas.go) — shared with concretePlanCost.
	return fetchCost(child[0])
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

// isNilInnerFetch reports whether expr is a physicalFetchFromPartialRecordWrapper
// whose embedded plan has a nil inner.
//
// Push-through-fetch rules (PushInJoinThroughFetchRule, PushFilterThroughFetchRule,
// PushDistinctThroughFetchRule, PushSetOperationThroughFetchRule) create these
// "shell" wrappers as part of the Cascades pattern: the wrapper's quantifier
// tracks the real child reference, while plan.GetInner() is nil because the
// plan is a template that gets assembled during extraction via WithChildren.
//
// This nil-inner state is architecturally intentional — setting the inner at
// rule time would introduce stale plan references (the inner's own children
// haven't been extracted yet), leading to incorrect explain output and plan
// quality regressions. The Cascades framework resolves this during plan
// extraction when WithChildren populates the inner from the quantifier graph.
//
// However, nil-inner fetch shells must NOT be selected as standalone winners
// before extraction, because callers that inspect plan.GetInner() (e.g.
// PhysicalIndexScanName, Explain) would get nil. Every site that selects a
// "best" candidate from a set of physical expressions must call this guard.
func isNilInnerFetch(expr expressions.RelationalExpression) bool {
	fw, ok := expr.(*physicalFetchFromPartialRecordWrapper)
	if !ok {
		return false
	}
	return fw.plan != nil && fw.plan.GetInner() == nil
}

var (
	_ expressions.RelationalExpression = (*physicalFetchFromPartialRecordWrapper)(nil)
	_ physicalPlanExpression           = (*physicalFetchFromPartialRecordWrapper)(nil)
)
