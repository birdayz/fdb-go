package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PushFilterThroughFetchRule pushes predicates that can be evaluated on
// partial records (index entries) underneath a FetchFromPartialRecordPlan.
// This filters out non-matching records before the expensive fetch.
//
// A predicate is pushable iff all its leaf values can be translated from
// the full-record domain to the partial-record (index) domain via the
// fetch's TranslateValueFunction.
//
// Three cases:
//  1. No predicates pushable → return (no yield)
//  2. All predicates pushable → Fetch(Filter(pushed, inner))
//  3. Some pushable, some residual → Filter(residual, Fetch(Filter(pushed, inner)))
//
// Mirrors Java's `PushFilterThroughFetchRule`.
type PushFilterThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushFilterThroughFetchRule() *PushFilterThroughFetchRule {
	return &PushFilterThroughFetchRule{
		matcher: NewExpressionMatcher[*physicalPredicatesFilterWrapper]("phys_filter_over_fetch"),
	}
}

func (r *PushFilterThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushFilterThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	filterW := matching.Get[*physicalPredicatesFilterWrapper](call.Bindings, r.matcher)

	innerRef := filterW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch wrapper in the filter's inner.
	var fetchW *physicalFetchFromPartialRecordWrapper
	for _, m := range innerRef.AllMembers() {
		if fw, ok := m.(*physicalFetchFromPartialRecordWrapper); ok {
			fetchW = fw
			break
		}
	}
	if fetchW == nil {
		return
	}

	fetchPlan := fetchW.plan
	queryPredicates := filterW.plan.GetPredicates()

	oldInnerAlias := filterW.innerQuant.GetAlias()
	newInnerAlias := values.UniqueCorrelationIdentifier()

	var pushed []predicates.QueryPredicate
	var residual []predicates.QueryPredicate

	for _, qp := range queryPredicates {
		translated, ok := tryPushPredicate(fetchPlan, oldInnerAlias, newInnerAlias, qp)
		if ok {
			pushed = append(pushed, translated)
		} else {
			residual = append(residual, qp)
		}
	}

	// Case 1: nothing pushable.
	if len(pushed) == 0 {
		return
	}

	// Get the fetch's inner (covering index scan).
	fetchInnerRef := fetchW.innerQuant.GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	// Build: Filter(pushed, fetchInner)
	pushedFilterPlan := plans.NewRecordQueryPredicatesFilterPlan(nil, pushed)
	innerQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	pushedFilterWrapper := NewPhysicalPredicatesFilterWrapper(pushedFilterPlan, innerQ)

	// Memoize the pushed filter.
	pushedFilterRef := call.MemoizeFinalExpression(pushedFilterWrapper)

	// Build: Fetch(Filter(pushed, fetchInner))
	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		nil,
		fetchPlan.GetTranslateValueFunction(),
		fetchPlan.GetResultType(),
		fetchPlan.GetFetchIndexRecords(),
	)
	newFetchQ := expressions.ForEachQuantifier(pushedFilterRef)
	newFetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(newFetchPlan, newFetchQ)

	if len(residual) == 0 {
		// Case 2: all pushed.
		call.Yield(newFetchWrapper)
	} else {
		// Case 3: some residual.
		fetchRef := call.MemoizeFinalExpression(newFetchWrapper)
		newQOverFetch := expressions.ForEachQuantifier(fetchRef)

		// Rebase residual predicates to use the new fetch quantifier alias.
		rebasedResidual := rebasePredicates(residual, oldInnerAlias, newQOverFetch.GetAlias())

		residualFilterPlan := plans.NewRecordQueryPredicatesFilterPlan(nil, rebasedResidual)
		residualFilterWrapper := NewPhysicalPredicatesFilterWrapper(residualFilterPlan, newQOverFetch)
		call.Yield(residualFilterWrapper)
	}
}

// tryPushPredicate attempts to translate a predicate's value references
// through the fetch plan. Returns the translated predicate and true on
// success, or nil and false if any value reference cannot be translated.
func tryPushPredicate(
	fetchPlan *plans.RecordQueryFetchFromPartialRecordPlan,
	oldAlias, newAlias values.CorrelationIdentifier,
	pred predicates.QueryPredicate,
) (predicates.QueryPredicate, bool) {
	switch p := pred.(type) {
	case *predicates.ComparisonPredicate:
		return tryPushComparisonPredicate(fetchPlan, oldAlias, newAlias, p)
	case *predicates.ValuePredicate:
		translated := tryTranslateValue(fetchPlan, oldAlias, newAlias, p.Value)
		if translated == nil {
			return nil, false
		}
		return predicates.NewValuePredicate(translated), true
	case *predicates.ConstantPredicate:
		return p, true
	case *predicates.AndPredicate:
		return tryPushAndPredicate(fetchPlan, oldAlias, newAlias, p)
	case *predicates.OrPredicate:
		return tryPushOrPredicate(fetchPlan, oldAlias, newAlias, p)
	case *predicates.NotPredicate:
		child, ok := tryPushPredicate(fetchPlan, oldAlias, newAlias, p.Child)
		if !ok {
			return nil, false
		}
		return predicates.NewNot(child), true
	default:
		// Unknown predicate type — keep as residual (don't push). Safe
		// default prevents future predicate types with correlation-bearing
		// values from being pushed without translation.
		return nil, false
	}
}

func tryPushComparisonPredicate(
	fetchPlan *plans.RecordQueryFetchFromPartialRecordPlan,
	oldAlias, newAlias values.CorrelationIdentifier,
	p *predicates.ComparisonPredicate,
) (predicates.QueryPredicate, bool) {
	// Translate LHS operand.
	var newOperand values.Value
	if p.Operand != nil {
		newOperand = tryTranslateValue(fetchPlan, oldAlias, newAlias, p.Operand)
		if newOperand == nil {
			return nil, false
		}
	}

	// Translate RHS comparison operand (if present).
	newComp := p.Comparison
	if p.Comparison.Operand != nil {
		translated := tryTranslateValue(fetchPlan, oldAlias, newAlias, p.Comparison.Operand)
		if translated == nil {
			return nil, false
		}
		newComp.Operand = translated
	}

	return predicates.NewComparisonPredicate(newOperand, newComp), true
}

// tryTranslateValue attempts to translate a value through the fetch.
// Recursively processes composite values (like ArithmeticValue) by
// translating their children first, then reconstructing the parent.
// Leaf values correlated to the source alias are translated via
// PushValue. Uncorrelated values and constants pass through unchanged.
//
// Ports Java's mapMaybe-based recursive translation in
// ScanWithFetchMatchCandidate.pushValueThroughFetch.
func tryTranslateValue(
	fetchPlan *plans.RecordQueryFetchFromPartialRecordPlan,
	oldAlias, newAlias values.CorrelationIdentifier,
	v values.Value,
) values.Value {
	if v == nil {
		return nil
	}
	// Constants are never correlated — always pushable.
	if _, isConst := v.(*values.ConstantValue); isConst {
		return v
	}
	// Check if the value is correlated to the source alias.
	correlated := values.GetCorrelatedToOfValue(v)
	if _, isCorrelated := correlated[oldAlias]; !isCorrelated {
		return v
	}
	// Try direct translation first (leaf values like FieldValue, QOV).
	if translated, ok := fetchPlan.PushValue(v, oldAlias, newAlias); ok {
		return translated
	}
	// Direct translation failed — try recursive decomposition.
	// Translate children first, then reconstruct the parent with
	// translated children. This handles composite values like
	// ArithmeticValue(FieldValue, Constant).
	children := v.Children()
	if len(children) == 0 {
		return nil // leaf that can't be translated
	}
	translated := make([]values.Value, len(children))
	for i, child := range children {
		tc := tryTranslateValue(fetchPlan, oldAlias, newAlias, child)
		if tc == nil {
			return nil
		}
		translated[i] = tc
	}
	// Reconstruct with translated children.
	return values.WithChildren(v, translated)
}

func tryPushAndPredicate(
	fetchPlan *plans.RecordQueryFetchFromPartialRecordPlan,
	oldAlias, newAlias values.CorrelationIdentifier,
	p *predicates.AndPredicate,
) (predicates.QueryPredicate, bool) {
	translated := make([]predicates.QueryPredicate, 0, len(p.SubPredicates))
	for _, child := range p.SubPredicates {
		t, ok := tryPushPredicate(fetchPlan, oldAlias, newAlias, child)
		if !ok {
			return nil, false
		}
		translated = append(translated, t)
	}
	return predicates.NewAnd(translated...), true
}

func tryPushOrPredicate(
	fetchPlan *plans.RecordQueryFetchFromPartialRecordPlan,
	oldAlias, newAlias values.CorrelationIdentifier,
	p *predicates.OrPredicate,
) (predicates.QueryPredicate, bool) {
	translated := make([]predicates.QueryPredicate, 0, len(p.SubPredicates))
	for _, child := range p.SubPredicates {
		t, ok := tryPushPredicate(fetchPlan, oldAlias, newAlias, child)
		if !ok {
			return nil, false
		}
		translated = append(translated, t)
	}
	return predicates.NewOr(translated...), true
}

var _ ImplementationRule = (*PushFilterThroughFetchRule)(nil)
