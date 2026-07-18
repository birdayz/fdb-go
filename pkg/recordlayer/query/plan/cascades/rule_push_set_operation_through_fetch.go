package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PushUnionThroughFetchRule handles the Union case.
// Java: PushSetOperationThroughFetchRule<RecordQueryUnionOnValuesPlan>.
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
	pushSetOpThroughFetch(call, setOpPush{
		quants: unionW.innerQuants,
		rebuildPlan: func(inners []plans.RecordQueryPlan) plans.RecordQueryPlan {
			return plans.NewRecordQueryUnionPlan(inners)
		},
		buildWrapper: func(p plans.RecordQueryPlan, qs []expressions.Quantifier) expressions.RelationalExpression {
			return NewPhysicalUnionWrapper(p.(*plans.RecordQueryUnionPlan), qs)
		},
	})
}

var _ ImplementationRule = (*PushUnionThroughFetchRule)(nil)

// PushIntersectionThroughFetchRule handles the Intersection case.
// Java: PushSetOperationThroughFetchRule<RecordQueryIntersectionOnValuesPlan>.
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
	pushSetOpThroughFetch(call, setOpPush{
		quants: intW.innerQuants,
		// The merge evaluates the comparison keys against child rows, so
		// the pushed children (partial records) must be able to answer
		// them — Java's getRequiredValues/tryPushValues gate.
		requiredValues: intW.plan.GetComparisonKeyValues(),
		rebuildPlan: func(inners []plans.RecordQueryPlan) plans.RecordQueryPlan {
			// Java's withChildrenReferences mirrors every attribute except
			// the children — comparison keys carry over verbatim.
			return plans.NewRecordQueryIntersectionPlan(inners, intW.plan.GetComparisonKeyValues())
		},
		buildWrapper: func(p plans.RecordQueryPlan, qs []expressions.Quantifier) expressions.RelationalExpression {
			return NewPhysicalIntersectionWrapper(p.(*plans.RecordQueryIntersectionPlan), qs)
		},
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
	pushSetOpThroughFetch(call, setOpPush{
		quants: w.innerQuants,
		rebuildPlan: func(inners []plans.RecordQueryPlan) plans.RecordQueryPlan {
			return plans.NewRecordQueryUnorderedUnionPlan(inners)
		},
		buildWrapper: func(p plans.RecordQueryPlan, qs []expressions.Quantifier) expressions.RelationalExpression {
			return NewPhysicalUnorderedUnionWrapper(p.(*plans.RecordQueryUnorderedUnionPlan), qs)
		},
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
	old := w.plan
	pushSetOpThroughFetch(call, setOpPush{
		quants: []expressions.Quantifier{w.innerQuant},
		// InUnion is DYNAMIC (Java RecordQueryInUnionPlan.isDynamic():
		// one leg executed many times side-by-side over the IN bindings)
		// — it fires with its single leg when that leg is fetch-backed,
		// and every leg must be pushable.
		dynamic:        true,
		requiredValues: old.GetComparisonKeys(),
		rebuildPlan: func(inners []plans.RecordQueryPlan) plans.RecordQueryPlan {
			if len(inners) != 1 {
				return nil
			}
			np := plans.NewRecordQueryInUnionPlanWithMaxSize(
				inners[0], old.GetBindingNames(), old.GetComparisonKeys(),
				old.IsReverse(), old.GetMaxSize(),
			)
			np.SetInSources(old.GetInSources())
			return np
		},
		buildWrapper: func(p plans.RecordQueryPlan, qs []expressions.Quantifier) expressions.RelationalExpression {
			if len(qs) != 1 {
				return nil
			}
			return NewPhysicalInUnionWrapper(p.(*plans.RecordQueryInUnionPlan), qs[0])
		},
	})
}

var _ ImplementationRule = (*PushInUnionThroughFetchRule)(nil)

// setOpPush parameterizes pushSetOpThroughFetch per set-operation type.
// rebuildPlan is the Go analogue of Java's
// RecordQuerySetPlan.withChildrenReferences: a set-op plan of the same
// kind and attributes over new child PLANS. requiredValues are the
// values the merge evaluates against child rows (comparison keys);
// empty for concat-style unions.
type setOpPush struct {
	quants         []expressions.Quantifier
	dynamic        bool
	requiredValues []values.Value
	rebuildPlan    func([]plans.RecordQueryPlan) plans.RecordQueryPlan
	buildWrapper   func(plans.RecordQueryPlan, []expressions.Quantifier) expressions.RelationalExpression
}

// pushSetOpThroughFetch pushes a set operation below its children's
// fetches so the fetch runs once, above the merged (smaller) stream.
// Port of Java PushSetOperationThroughFetchRule.onMatch: legs over
// fetches are candidates; a leg is pushable when its translation
// function can answer every required value; the set-op PLAN is rebuilt
// over the pushed inner plans (never the stale fetch-children snapshot)
// and capped by ONE fetch carrying the combined translation function.
// Non-pushable legs stay above under a rebuilt outer set-op (Java's
// "Case 2").
func pushSetOpThroughFetch(call *ImplementationRuleCall, p setOpPush) {
	type fetchLeg struct {
		idx       int
		fw        *physicalFetchFromPartialRecordWrapper
		innerExpr expressions.RelationalExpression
		innerPlan plans.RecordQueryPlan
	}
	var legs []fetchLeg
	for i, q := range p.quants {
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
			continue // residual leg — stays above the pushed fetch
		}
		// Resolve the fetch's inner from its quantifier, not from the
		// fetch PLAN: push rules build nil-inner fetch shells whose real
		// child lives in the wrapper quantifier (relinked at extraction).
		innerExpr := findPhysicalExpr(fw.innerQuant.GetRangesOver())
		if innerExpr == nil {
			continue
		}
		ph, ok := innerExpr.(physicalPlanExpression)
		if !ok || ph.GetRecordQueryPlan() == nil {
			continue
		}
		legs = append(legs, fetchLeg{idx: i, fw: fw, innerExpr: innerExpr, innerPlan: ph.GetRecordQueryPlan()})
	}

	// Java's viability gates: a dynamic set op pushes all legs or none;
	// otherwise pulling one fetch above fewer than two legs is
	// meaningless.
	if p.dynamic {
		if len(legs) < len(p.quants) {
			return
		}
	} else if len(legs) <= 1 {
		return
	}

	// tryPushValues: keep only legs whose translation function answers
	// every required value, and require the answers to agree across legs
	// (Java declines everything on disagreement — a broken derivation
	// path). The Go translation functions match by covered-column name,
	// so the aliases are placeholders.
	sourceAlias := values.UniqueCorrelationIdentifier()
	targetAlias := values.UniqueCorrelationIdentifier()
	pushable := legs[:0:0]
	for _, leg := range legs {
		fn := leg.fw.plan.GetTranslateValueFunction()
		ok := fn != nil
		for _, rv := range p.requiredValues {
			if ok {
				_, ok = fn(rv, sourceAlias, targetAlias)
			}
		}
		if ok {
			pushable = append(pushable, leg)
		}
	}
	for _, rv := range p.requiredValues {
		var prev values.Value
		for _, leg := range pushable {
			tv, _ := leg.fw.plan.GetTranslateValueFunction()(rv, sourceAlias, targetAlias)
			if prev == nil {
				prev = tv
				continue
			}
			if !values.SemanticEqualsUnderAliasMap(prev, tv, values.AliasMap{}) {
				return
			}
		}
	}
	if p.dynamic {
		if len(pushable) < len(p.quants) {
			return
		}
	} else if len(pushable) <= 1 {
		return
	}

	// All pushable legs must share one fetch mode (Java declines on
	// mismatch rather than splitting further).
	fetchIndexRecords := pushable[0].fw.plan.GetFetchIndexRecords()
	for _, leg := range pushable[1:] {
		if leg.fw.plan.GetFetchIndexRecords() != fetchIndexRecords {
			return
		}
	}

	// Combined translation function: Java RecordQuerySetPlan
	// .pushValueFunction — a value translates through the merged fetch
	// iff EVERY leg translates it, and to semantically equal values.
	legFns := make([]plans.TranslateValueFunction, len(pushable))
	for i, leg := range pushable {
		legFns[i] = leg.fw.plan.GetTranslateValueFunction()
	}
	combined := func(v values.Value, sa, ta values.CorrelationIdentifier) (values.Value, bool) {
		var prev values.Value
		for _, fn := range legFns {
			tv, ok := fn(v, sa, ta)
			if !ok {
				return nil, false
			}
			if prev == nil {
				prev = tv
				continue
			}
			if !values.SemanticEqualsUnderAliasMap(prev, tv, values.AliasMap{}) {
				return nil, false
			}
		}
		return prev, true
	}

	// Rebuild the set-op PLAN over the pushed INNER plans — Java's
	// setOperationPlan.withChildrenReferences(newPushedInnerPlans). The
	// old code passed the stale plan (children still the fetches) into
	// the wrapper, so extraction executed Fetch(SetOp(Fetch(leg)…)):
	// double-fetching every row while the cost model priced the pushed
	// shape. Each child quantifier is a FinalOf singleton over the exact
	// expression whose plan is baked, so cost and ordering read what
	// will execute.
	innerPlans := make([]plans.RecordQueryPlan, len(pushable))
	newQuants := make([]expressions.Quantifier, len(pushable))
	for i, leg := range pushable {
		innerPlans[i] = leg.innerPlan
		newQuants[i] = expressions.ForEachQuantifier(expressions.FinalOf(leg.innerExpr))
	}
	newSetOpPlan := p.rebuildPlan(innerPlans)
	if newSetOpPlan == nil {
		return
	}
	setOpWrapper := p.buildWrapper(newSetOpPlan, newQuants)
	if setOpWrapper == nil {
		return
	}
	setOpRef := call.MemoizeFinalExpression(setOpWrapper)

	// The merged fetch's output is the original set-op's output — full
	// records (Java: scalarOf(setOperationPlan.getResultType())); any
	// pushable leg's fetch already produces exactly that.
	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		newSetOpPlan, combined, pushable[0].fw.plan.GetResultType(), fetchIndexRecords,
	)
	newFetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(
		newFetchPlan, expressions.ForEachQuantifier(setOpRef),
	)

	if len(pushable) == len(p.quants) {
		call.Yield(newFetchWrapper)
		return
	}

	// Case 2: the outer set-op survives over the merged fetch plus the
	// residual legs (Java yields
	// setOperationPlan.withChildrenReferences(newFetchPlan ++ residuals)).
	isPushed := make(map[int]bool, len(pushable))
	for _, leg := range pushable {
		isPushed[leg.idx] = true
	}
	outerPlans := []plans.RecordQueryPlan{newFetchPlan}
	outerQuants := []expressions.Quantifier{
		expressions.ForEachQuantifier(call.MemoizeFinalExpression(newFetchWrapper)),
	}
	for i, q := range p.quants {
		if isPushed[i] {
			continue
		}
		resExpr := findPhysicalExpr(q.GetRangesOver())
		if resExpr == nil {
			return
		}
		ph, ok := resExpr.(physicalPlanExpression)
		if !ok || ph.GetRecordQueryPlan() == nil {
			return
		}
		outerPlans = append(outerPlans, ph.GetRecordQueryPlan())
		outerQuants = append(outerQuants, expressions.ForEachQuantifier(expressions.FinalOf(resExpr)))
	}
	outerPlan := p.rebuildPlan(outerPlans)
	if outerPlan == nil {
		return
	}
	if outer := p.buildWrapper(outerPlan, outerQuants); outer != nil {
		call.Yield(outer)
	}
}
