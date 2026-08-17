package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PushUnionThroughFetchRule handles Go's extra concat Union case. It has no
// direct Java counterpart: Java's ordered RecordQueryUnionOnValuesPlan maps
// to PushMergeSortUnionThroughFetchRule below.
type PushUnionThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushUnionThroughFetchRule() *PushUnionThroughFetchRule {
	return &PushUnionThroughFetchRule{
		matcher: NewExpressionMatcher[*plans.RecordQueryUnionPlan]("phys_union_over_fetches"),
	}
}

func (r *PushUnionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushUnionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	unionW := matching.Get[*plans.RecordQueryUnionPlan](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, setOpPush{
		quants:     unionW.GetQuantifiers(),
		resultType: unionW.GetResultType(),
		rebuildPlan: func(inners []plans.RecordQueryPlan) (plans.RecordQueryPlan, error) {
			return plans.NewRecordQueryUnionPlan(inners)
		},
		buildWrapper: func(_ plans.RecordQueryPlan, qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
			// Collapsed: the union is its own cascades expression over the live
			// pushed-down quantifiers (RFC-184 W2); the snapshot plan is unused.
			return plans.NewRecordQueryUnionPlanFromQuantifiers(qs)
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
		matcher: NewExpressionMatcher[*plans.RecordQueryIntersectionPlan]("phys_intersection_over_fetches"),
	}
}

func (r *PushIntersectionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushIntersectionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	intW := matching.Get[*plans.RecordQueryIntersectionPlan](call.Bindings, r.matcher)
	intersectionLayout, err := intW.ProvidedOutputLayout()
	if err != nil {
		call.Fail(fmt.Errorf("push intersection through fetch: output layout: %w", err))
		return
	}
	compKeys := intW.GetComparisonKeyValues()
	compParts := intW.GetComparisonKeyOrderingParts()
	reverse := intW.IsReverse()
	comparisonSource, sourceOK := intersectionComparisonKeySource(compParts)
	keysUseOutputCarrier := sourceOK && comparisonSource == intersectionLayout.Carrier()
	rebuildIntersection := func(
		qs []expressions.Quantifier,
	) (*plans.RecordQueryIntersectionPlan, error) {
		if keysUseOutputCarrier {
			return plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrderingAndSource(
				qs, compParts, reverse, intersectionLayout.Carrier())
		}
		return plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrdering(qs, compParts, reverse)
	}
	var requiredValuesCarrier values.QuantifiedObjectValue
	if keysUseOutputCarrier {
		requiredValuesCarrier = intersectionLayout.Carrier()
	}
	pushSetOpThroughFetch(call, setOpPush{
		quants:                intW.GetQuantifiers(),
		resultType:            intW.GetResultType(),
		requiredValuesCarrier: requiredValuesCarrier,
		// The merge evaluates the comparison keys against child rows, so
		// the pushed children (partial records) must be able to answer
		// them — Java's getRequiredValues/tryPushValues gate.
		requiredValues: compKeys,
		rebuildPlan: func(inners []plans.RecordQueryPlan) (plans.RecordQueryPlan, error) {
			// Java's withChildrenReferences mirrors every attribute except
			// the children — semantic comparison parts and direction carry
			// over, and the physical keys are deterministically re-derived.
			return rebuildIntersection(plans.QuantifiersOverPlans(inners))
		},
		buildWrapper: func(_ plans.RecordQueryPlan, qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
			return rebuildIntersection(qs)
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
		matcher: NewExpressionMatcher[*plans.RecordQueryUnorderedUnionPlan]("phys_unordered_union_over_fetches"),
	}
}

func (r *PushUnorderedUnionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushUnorderedUnionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	w := matching.Get[*plans.RecordQueryUnorderedUnionPlan](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, setOpPush{
		quants:     w.GetQuantifiers(),
		resultType: w.GetResultType(),
		rebuildPlan: func(inners []plans.RecordQueryPlan) (plans.RecordQueryPlan, error) {
			return plans.NewRecordQueryUnorderedUnionPlan(inners)
		},
		buildWrapper: func(_ plans.RecordQueryPlan, qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
			// Collapsed: the union is its own cascades expression over the live
			// pushed-down quantifiers (RFC-184 W2); the snapshot plan is unused.
			return plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers(qs)
		},
	})
}

var _ ImplementationRule = (*PushUnorderedUnionThroughFetchRule)(nil)

// PushMergeSortUnionThroughFetchRule handles the ordered merge-sort
// union — Go's analogue of Java's ordered union.
// Java: PushSetOperationThroughFetchRule<RecordQueryUnionOnValuesPlan>.
type PushMergeSortUnionThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushMergeSortUnionThroughFetchRule() *PushMergeSortUnionThroughFetchRule {
	return &PushMergeSortUnionThroughFetchRule{
		matcher: NewExpressionMatcher[*plans.RecordQueryMergeSortUnionPlan]("phys_merge_sort_union_over_fetches"),
	}
}

func (r *PushMergeSortUnionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushMergeSortUnionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	old := matching.Get[*plans.RecordQueryMergeSortUnionPlan](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, setOpPush{
		quants: old.GetQuantifiers(),
		// The ordered merge (and dedup when removeDuplicates) evaluates
		// the comparison keys against child rows — pushable only when
		// the partial records can answer them. The fetch above is a
		// PK-preserving per-row map, so merging/deduping on translated
		// keys below it is value-identical to doing it above.
		requiredValues: old.GetComparisonKeys(),
		resultType:     old.GetResultType(),
		rebuildPlan: func(inners []plans.RecordQueryPlan) (plans.RecordQueryPlan, error) {
			return plans.NewRecordQueryMergeSortUnionPlan(
				inners, old.GetComparisonKeys(), old.IsReverse(), old.RemovesDuplicates(),
			)
		},
		buildWrapper: func(_ plans.RecordQueryPlan, qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
			return plans.NewRecordQueryMergeSortUnionPlanFromQuantifiers(
				qs, old.GetComparisonKeys(), old.IsReverse(), old.RemovesDuplicates(),
			)
		},
	})
}

var _ ImplementationRule = (*PushMergeSortUnionThroughFetchRule)(nil)

// PushInUnionThroughFetchRule handles the InUnion case.
// Java: PushSetOperationThroughFetchRule<RecordQueryInUnionOnValuesPlan>.
type PushInUnionThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushInUnionThroughFetchRule() *PushInUnionThroughFetchRule {
	return &PushInUnionThroughFetchRule{
		matcher: NewExpressionMatcher[*plans.RecordQueryInUnionPlan]("phys_in_union_over_fetches"),
	}
}

func (r *PushInUnionThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushInUnionThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	// The InUnion is its own cascades expression now (RFC-184 W2).
	old := matching.Get[*plans.RecordQueryInUnionPlan](call.Bindings, r.matcher)
	pushSetOpThroughFetch(call, setOpPush{
		quants: []expressions.Quantifier{old.GetInnerQuantifier()},
		// InUnion is DYNAMIC (Java RecordQueryInUnionPlan.isDynamic():
		// one leg executed many times side-by-side over the IN bindings)
		// — it fires with its single leg when that leg is fetch-backed,
		// and every leg must be pushable.
		dynamic:        true,
		requiredValues: old.GetComparisonKeys(),
		resultType:     old.GetResultType(),
		rebuildPlan: func(inners []plans.RecordQueryPlan) (plans.RecordQueryPlan, error) {
			if len(inners) != 1 {
				return nil, fmt.Errorf("InUnion rebuild: expected 1 inner, got %d", len(inners))
			}
			np, err := plans.NewRecordQueryInUnionPlanWithBindingAliasesAndMaxSize(
				inners[0], old.GetBindingAliases(), old.GetComparisonKeys(),
				old.IsReverse(), old.GetMaxSize(),
			)
			if err != nil {
				return nil, err
			}
			np = np.WithInSources(old.GetInSources())
			return np, nil
		},
		buildWrapper: func(_ plans.RecordQueryPlan, qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
			if len(qs) != 1 {
				return nil, fmt.Errorf("InUnion wrapper: expected 1 quantifier, got %d", len(qs))
			}
			// Collapsed: the InUnion is its own cascades expression over the live
			// pushed-down inner edge (RFC-184 W2); the snapshot plan is unused.
			np, err := plans.NewRecordQueryInUnionPlanFromQuantifierWithBindingAliases(
				qs[0], old.GetBindingAliases(), old.GetComparisonKeys(),
				old.IsReverse(), old.GetMaxSize(),
			)
			if err != nil {
				return nil, err
			}
			np = np.WithInSources(old.GetInSources())
			return np, nil
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
	quants                []expressions.Quantifier
	dynamic               bool
	requiredValues        []values.Value
	requiredValuesCarrier values.QuantifiedObjectValue
	// resultType is the ORIGINAL set-op plan's result type when it
	// carries one (Java caps the new fetch with
	// scalarOf(setOperationPlan.getResultType()) — the matched plan's
	// output, not a leg's). Unknown → the first pushable leg's fetch
	// type stands in (identical for homogeneous legs).
	resultType   values.Type
	rebuildPlan  func([]plans.RecordQueryPlan) (plans.RecordQueryPlan, error)
	buildWrapper func(plans.RecordQueryPlan, []expressions.Quantifier) (expressions.RelationalExpression, error)
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
		idx           int
		fw            *plans.RecordQueryFetchFromPartialRecordPlan
		innerExpr     expressions.RelationalExpression
		innerPlan     plans.RecordQueryPlan
		sourceQOV     values.QuantifiedObjectValue
		outputCarrier values.QuantifiedObjectValue
	}
	var legs []fetchLeg
	for i, q := range p.quants {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		var fw *plans.RecordQueryFetchFromPartialRecordPlan
		for _, m := range ref.AllMembers() {
			if f, ok := m.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
				fw = f
				break
			}
		}
		if fw == nil {
			continue // residual leg — stays above the pushed fetch
		}
		// Resolve the fetch's inner from its quantifier, not from the fetch
		// PLAN: the quantifier ranges over the child GROUP, so this sees the
		// alternatives the group holds rather than only the one expression the
		// wrapper happened to bake at build time.
		innerExpr := findPhysicalExpr(fw.GetInnerQuantifier().GetRangesOver())
		if innerExpr == nil {
			continue
		}
		ph, ok := innerExpr.(physicalPlanExpression)
		if !ok || ph.GetRecordQueryPlan() == nil {
			continue
		}
		var sourceQOV values.QuantifiedObjectValue
		var outputCarrier values.QuantifiedObjectValue
		if p.requiredValuesCarrier != nil {
			var err error
			sourceQOV, err = q.RequireFlowedObjectValue()
			if err != nil {
				call.Fail(fmt.Errorf("push intersection through fetch: leg %d source QOV: %w", i, err))
				return
			}
			layout, layoutErr := fw.ProvidedOutputLayout()
			if layoutErr != nil {
				call.Fail(fmt.Errorf("push intersection through fetch: leg %d output layout: %w", i, layoutErr))
				return
			}
			outputCarrier = layout.Carrier()
		}
		legs = append(legs, fetchLeg{
			idx: i, fw: fw, innerExpr: innerExpr,
			innerPlan: ph.GetRecordQueryPlan(), sourceQOV: sourceQOV,
			outputCarrier: outputCarrier,
		})
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

	// tryPushValues (RecordQuerySetPlan.java:128-158), single pass per
	// required value over the LIVE candidates: a leg whose translation
	// function cannot answer a value drops out (it survives as a
	// residual), but a translation that DISAGREES with a surviving leg's
	// is a broken derivation path — decline everything, exactly when Java
	// does. Splitting this into filter-then-agree would let a
	// disagreeing leg exit via a later failure before the disagreement
	// is seen. The Go translation functions match by covered-column
	// name, so the aliases are placeholders.
	sourceAlias := values.UniqueCorrelationIdentifier()
	targetAlias := values.UniqueCorrelationIdentifier()
	alive := make(map[int]bool, len(legs))
	for _, leg := range legs {
		alive[leg.idx] = true
	}
	requiredValuesCarriers := []values.QuantifiedObjectValue{p.requiredValuesCarrier}
	if p.requiredValuesCarrier != nil {
		for _, leg := range legs {
			requiredValuesCarriers = append(requiredValuesCarriers, leg.outputCarrier)
		}
	}
	for _, rv := range p.requiredValues {
		var prev values.Value
		for _, leg := range legs {
			if !alive[leg.idx] {
				continue
			}
			candidateValue := rv
			candidateSource := sourceAlias
			if p.requiredValuesCarrier != nil {
				var err error
				candidateValue, _, err = translateIntersectionOutputCarrierToLegEdge(
					rv, requiredValuesCarriers, leg.sourceQOV)
				if err != nil {
					call.Fail(fmt.Errorf("push intersection through fetch: comparison key for leg %d: %w", leg.idx, err))
					return
				}
				candidateSource = leg.sourceQOV.Correlation()
			}
			tv, ok := leg.fw.GetTranslateValueFunction()(candidateValue, candidateSource, targetAlias)
			if !ok {
				delete(alive, leg.idx)
				continue
			}
			if prev == nil {
				prev = tv
				continue
			}
			if !values.SemanticEqualsUnderAliasMap(prev, tv, values.EmptyAliasMap()) {
				return
			}
		}
	}
	pushable := legs[:0:0]
	for _, leg := range legs {
		if alive[leg.idx] {
			pushable = append(pushable, leg)
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
	fetchIndexRecords := pushable[0].fw.GetFetchIndexRecords()
	for _, leg := range pushable[1:] {
		if leg.fw.GetFetchIndexRecords() != fetchIndexRecords {
			return
		}
	}

	// Combined translation function: Java RecordQuerySetPlan
	// .pushValueFunction — a value translates through the merged fetch
	// iff EVERY leg translates it, and to semantically equal values.
	var mergedFetchOutputCarrier values.QuantifiedObjectValue
	combined := func(v values.Value, sa, ta values.CorrelationIdentifier) (values.Value, bool) {
		var prev values.Value
		for _, leg := range pushable {
			candidateValue := v
			candidateSource := sa
			if p.requiredValuesCarrier != nil {
				moved := false
				carriers := requiredValuesCarriers
				if mergedFetchOutputCarrier != nil {
					carriers = append(
						append([]values.QuantifiedObjectValue(nil), carriers...),
						mergedFetchOutputCarrier,
					)
				}
				var err error
				candidateValue, moved, err = translateIntersectionOutputCarrierToLegEdge(
					candidateValue, carriers, leg.sourceQOV)
				if err != nil {
					return nil, false
				}
				if moved {
					candidateSource = leg.sourceQOV.Correlation()
				}
			}
			tv, ok := leg.fw.GetTranslateValueFunction()(candidateValue, candidateSource, ta)
			if !ok {
				return nil, false
			}
			if prev == nil {
				prev = tv
				continue
			}
			if !values.SemanticEqualsUnderAliasMap(prev, tv, values.EmptyAliasMap()) {
				return nil, false
			}
		}
		return prev, true
	}

	// Rebuild the set-op PLAN over the pushed INNER plans — Java's
	// setOperationPlan.withChildrenReferences(newPushedInnerPlans). The
	// old code passed the stale plan (children still the fetches) into
	// the wrapper, so extraction executed Fetch(SetOp(Fetch(leg)…))
	// while the cost model priced the pushed shape — a cost-model lie
	// (the executor's fetch is a per-row cap, so the double fetch costs
	// wrong more than it re-reads today; the real I/O win arrives with
	// covering execution). Each child quantifier is a FinalOf singleton over the exact
	// expression whose plan is baked, so cost and ordering read what
	// will execute.
	innerPlans := make([]plans.RecordQueryPlan, len(pushable))
	newQuants := make([]expressions.Quantifier, len(pushable))
	for i, leg := range pushable {
		innerPlans[i] = leg.innerPlan
		newQuants[i] = expressions.ForEachQuantifier(expressions.FinalOf(leg.innerExpr))
	}
	newSetOpPlan, err := p.rebuildPlan(innerPlans)
	if err != nil {
		call.Fail(err)
		return
	}
	if newSetOpPlan == nil {
		return
	}
	setOpWrapper, err := p.buildWrapper(newSetOpPlan, newQuants)
	if err != nil {
		call.Fail(err)
		return
	}
	if setOpWrapper == nil {
		return
	}
	setOpRef := call.MemoizeFinalExpression(setOpWrapper)

	// The merged fetch's output is the original set-op's output — full
	// records (Java: scalarOf(setOperationPlan.getResultType())); when
	// the matched plan doesn't carry a type, any pushable leg's fetch
	// produces exactly that for homogeneous legs.
	// "Carries no type" is asked as a PREDICATE for UNIFORMITY with the sibling
	// site (rule_implement_nested_loop_join.go's typeUnstated), and because
	// pointer identity against a singleton is brittle in principle: it is a
	// question about an instance where the intent is a question about a property.
	//
	// NOT because the old `== values.UnknownType` was catching the wrong set
	// TODAY. An earlier revision of this comment claimed a nullable unknown was
	// "a different pointer"; that is FALSE and measured false —
	// values.UnknownType is itself declared nullable, and WithNullability returns
	// its argument unchanged when the nullability already matches, so
	// `WithNullability(UnknownType, true) == UnknownType` is true. The only value
	// this predicate catches that pointer identity misses is a NON-NULLABLE
	// unknown, which no production site currently produces. So this edit is a
	// no-op on today's inputs and is here for the shape, not for a bug.
	resultType := p.resultType
	if typeUnstated(resultType) {
		resultType = pushable[0].fw.GetResultType()
	}
	// The merged fetch is its own cascades expression carrying the live setOpRef
	// edge (RFC-184 W2).
	newFetchPlan, err := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		expressions.ForEachQuantifier(setOpRef), combined, resultType, fetchIndexRecords,
	)
	if err != nil {
		call.Fail(err)
		return
	}
	if p.requiredValuesCarrier != nil {
		layout, layoutErr := newFetchPlan.ProvidedOutputLayout()
		if layoutErr != nil {
			call.Fail(fmt.Errorf("push intersection through fetch: merged fetch output layout: %w", layoutErr))
			return
		}
		mergedFetchOutputCarrier = layout.Carrier()
	}

	if len(pushable) == len(p.quants) {
		call.Yield(newFetchPlan)
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
		expressions.ForEachQuantifier(call.MemoizeFinalExpression(newFetchPlan)),
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
	outerPlan, err := p.rebuildPlan(outerPlans)
	if err != nil {
		call.Fail(err)
		return
	}
	if outerPlan == nil {
		return
	}
	outer, err := p.buildWrapper(outerPlan, outerQuants)
	if err != nil {
		call.Fail(err)
		return
	}
	if outer != nil {
		call.Yield(outer)
	}
}

// translateIntersectionOutputCarrierToLegEdge crosses the one phase boundary
// introduced by an Intersection over fetched rows. Comparison keys can be
// rooted at the Intersection's exact pass-through carrier, while each fetch
// candidate deliberately translates only values rooted at that leg's declared
// edge. TranslatePhaseRoot admits only the pointer-exact carrier and preserves
// the resolved path and exact type; same-shaped current or named rows remain
// foreign and are left for the candidate's strict alias gate to decline.
func translateIntersectionOutputCarrierToLegEdge(
	value values.Value,
	intersectionOutputCarriers []values.QuantifiedObjectValue,
	legEdge values.QuantifiedObjectValue,
) (values.Value, bool, error) {
	translated := value
	var movedFrom values.QuantifiedObjectValue
	seen := make(map[values.QuantifiedObjectValue]struct{}, len(intersectionOutputCarriers))
	for _, carrier := range intersectionOutputCarriers {
		if carrier == nil {
			continue
		}
		if _, duplicate := seen[carrier]; duplicate {
			continue
		}
		seen[carrier] = struct{}{}
		candidate, err := values.TranslatePhaseRoot(translated, carrier, legEdge)
		if err != nil {
			return nil, false, err
		}
		if candidate == translated {
			continue
		}
		if movedFrom != nil {
			return nil, false, fmt.Errorf(
				"comparison key spans distinct intersection output carriers")
		}
		movedFrom = carrier
		translated = candidate
	}
	return translated, movedFrom != nil, nil
}
