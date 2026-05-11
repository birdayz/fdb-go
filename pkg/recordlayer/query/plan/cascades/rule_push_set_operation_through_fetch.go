package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PushUnionThroughFetchRule handles the Union case.
type PushUnionThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushUnionThroughFetchRule() *PushUnionThroughFetchRule {
	return &PushUnionThroughFetchRule{
		matcher: NewExpressionMatcher[*physicalUnionWrapper]("phys_union_over_fetches"),
	}
}

func (r *PushUnionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushUnionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	unionW := matching.Get[*physicalUnionWrapper](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, unionW.innerQuants, func(newQuants []expressions.Quantifier) expressions.RelationalExpression {
		return NewPhysicalUnionWrapper(unionW.plan, newQuants)
	})
}

var _ ImplementationRule = (*PushUnionThroughFetchRule)(nil)

// PushIntersectionThroughFetchRule handles the Intersection case.
type PushIntersectionThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushIntersectionThroughFetchRule() *PushIntersectionThroughFetchRule {
	return &PushIntersectionThroughFetchRule{
		matcher: NewExpressionMatcher[*physicalIntersectionWrapper]("phys_intersection_over_fetches"),
	}
}

func (r *PushIntersectionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushIntersectionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	intW := matching.Get[*physicalIntersectionWrapper](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, intW.innerQuants, func(newQuants []expressions.Quantifier) expressions.RelationalExpression {
		return NewPhysicalIntersectionWrapper(intW.plan, newQuants)
	})
}

var _ ImplementationRule = (*PushIntersectionThroughFetchRule)(nil)

// PushUnorderedUnionThroughFetchRule handles the UnorderedUnion case.
// Java: PushSetOperationThroughFetchRule<RecordQueryUnorderedUnionPlan>.
type PushUnorderedUnionThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushUnorderedUnionThroughFetchRule() *PushUnorderedUnionThroughFetchRule {
	return &PushUnorderedUnionThroughFetchRule{
		matcher: NewExpressionMatcher[*physicalUnorderedUnionWrapper]("phys_unordered_union_over_fetches"),
	}
}

func (r *PushUnorderedUnionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushUnorderedUnionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	w := matching.Get[*physicalUnorderedUnionWrapper](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, w.innerQuants, func(newQuants []expressions.Quantifier) expressions.RelationalExpression {
		return NewPhysicalUnorderedUnionWrapper(w.plan, newQuants)
	})
}

var _ ImplementationRule = (*PushUnorderedUnionThroughFetchRule)(nil)

// PushInUnionThroughFetchRule handles the InUnion case.
// Java: PushSetOperationThroughFetchRule<RecordQueryInUnionOnValuesPlan>.
type PushInUnionThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushInUnionThroughFetchRule() *PushInUnionThroughFetchRule {
	return &PushInUnionThroughFetchRule{
		matcher: NewExpressionMatcher[*physicalInUnionWrapper]("phys_in_union_over_fetches"),
	}
}

func (r *PushInUnionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushInUnionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	w := matching.Get[*physicalInUnionWrapper](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, []expressions.Quantifier{w.innerQuant}, func(newQuants []expressions.Quantifier) expressions.RelationalExpression {
		if len(newQuants) != 1 {
			return nil
		}
		return NewPhysicalInUnionWrapper(w.plan, newQuants[0])
	})
}

var _ ImplementationRule = (*PushInUnionThroughFetchRule)(nil)

// pushSetOpThroughFetch is the shared logic for pushing a set operation
// through fetch wrappers. All children must be fetch wrappers with the
// same FetchIndexRecords mode. The buildSetOp callback constructs the
// new set operation wrapper with pushed-down quantifiers.
func pushSetOpThroughFetch(
	call *ImplementationRuleCall,
	quants []expressions.Quantifier,
	buildSetOp func([]expressions.Quantifier) expressions.RelationalExpression,
) {
	if len(quants) < 2 {
		return
	}

	// Collect fetch wrappers from all children.
	var fetchWrappers []*physicalFetchFromPartialRecordWrapper
	var fetchIndexRecords plans.FetchIndexRecords
	first := true

	for _, q := range quants {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		var fw *physicalFetchFromPartialRecordWrapper
		for _, m := range ref.AllMembers() {
			if f, ok := m.(*physicalFetchFromPartialRecordWrapper); ok {
				fw = f
				break
			}
		}
		if fw == nil {
			return // not all children are fetches
		}
		if first {
			fetchIndexRecords = fw.plan.GetFetchIndexRecords()
			first = false
		} else if fw.plan.GetFetchIndexRecords() != fetchIndexRecords {
			return // mismatched fetch modes
		}
		fetchWrappers = append(fetchWrappers, fw)
	}

	// All children are fetches with the same mode.
	// Build: SetOp(inner_a, inner_b, ...)
	newQuants := make([]expressions.Quantifier, 0, len(fetchWrappers))
	for _, fw := range fetchWrappers {
		fetchInnerRef := fw.innerQuant.GetRangesOver()
		if fetchInnerRef == nil {
			return
		}
		fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
		if fetchInnerExpr == nil {
			return
		}
		innerRef := call.MemoizeFinalExpressionsFromOther(
			fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr},
		)
		newQuants = append(newQuants, expressions.ForEachQuantifier(innerRef))
	}

	// Construct the pushed-down set operation.
	newSetOp := buildSetOp(newQuants)
	setOpRef := call.MemoizeFinalExpression(newSetOp)

	// Java combines TVFs via setOperationPlan.pushValueFunction. Go uses
	// the first fetch's TVF — valid when all fetches share the same
	// result type (same base record). The TVF translates field names
	// to index columns; for a union/intersection over the same table,
	// any covering TVF works because the result is the full record
	// fetched by PK regardless of which index provided the entry.
	translateFn := fetchWrappers[0].plan.GetTranslateValueFunction()
	resultType := fetchWrappers[0].plan.GetResultType()

	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		nil, translateFn, resultType, fetchIndexRecords,
	)
	newFetchQ := expressions.ForEachQuantifier(setOpRef)
	newFetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(newFetchPlan, newFetchQ)

	call.Yield(newFetchWrapper)
}
