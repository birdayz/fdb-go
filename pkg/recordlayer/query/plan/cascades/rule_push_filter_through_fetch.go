package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
		matcher: NewExpressionMatcher[*plans.RecordQueryPredicatesFilterPlan]("phys_filter_over_fetch"),
	}
}

func (r *PushFilterThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushFilterThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	filterW := matching.Get[*plans.RecordQueryPredicatesFilterPlan](call.Bindings, r.matcher)

	innerRef := filterW.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch in the filter's inner.
	var fetchW *plans.RecordQueryFetchFromPartialRecordPlan
	for _, m := range innerRef.AllMembers() {
		if fw, ok := m.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
			fetchW = fw
			break
		}
	}
	if fetchW == nil {
		return
	}

	fetchPlan := fetchW
	queryPredicates := filterW.GetPredicates()

	oldInnerAlias := filterW.GetInnerQuantifier().GetAlias()
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
	fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}
	// Bake the child plan bottom-up: every plan this rule constructs below
	// carries its real child, so nothing downstream has to relink a hole.
	fetchInnerPlan := bakedInnerPlan(fetchInnerExpr)
	if fetchInnerPlan == nil {
		return
	}

	// Build: Filter(pushed, fetchInner) as its own cascades expression carrying a
	// DISENTANGLED FINAL edge over the BAKED concrete fetchInnerPlan — the exact
	// snapshot structure the wrapper interned (RFC-184 W2). Freezing the baked plan
	// (not the live fetchInnerExpr memo edge, whose children may still be holes)
	// keeps this pushed member's structure byte-stable, so a parent that captures
	// it stays reachable as the memo re-explores. innerQ's alias is reused so
	// GetResultValue matches the wrapper.
	innerQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	pushedInnerQ := expressions.NamedForEachQuantifier(innerQ.GetAlias(),
		call.MemoizeFinalExpression(fetchInnerPlan))
	pushedFilterPlan := plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(pushedInnerQ, pushed)

	// Memoize the pushed filter.
	pushedFilterRef := call.MemoizeFinalExpression(pushedFilterPlan)

	// Build: Fetch(Filter(pushed, fetchInner)) as its own cascades expression
	// carrying the live pushedFilterRef edge (RFC-184 W2).
	newFetchQ := expressions.ForEachQuantifier(pushedFilterRef)
	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		newFetchQ,
		fetchPlan.GetTranslateValueFunction(),
		fetchPlan.GetResultType(),
		fetchPlan.GetFetchIndexRecords(),
	)

	if len(residual) == 0 {
		// Case 2: all pushed.
		call.Yield(newFetchPlan)
	} else {
		// Case 3: some residual.
		fetchRef := call.MemoizeFinalExpression(newFetchPlan)
		newQOverFetch := expressions.ForEachQuantifier(fetchRef)

		// Rebase residual predicates to use the new fetch quantifier alias.
		rebasedResidual := rebasePredicates(residual, oldInnerAlias, newQOverFetch.GetAlias())

		// The residual filter carries the live newQOverFetch edge — a
		// DISENTANGLED single-member reference over the concrete newFetchPlan
		// (MemoizeFinalExpression), so planFromQuantifier resolves it directly
		// (RFC-184 W2).
		residualFilterPlan := plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(newQOverFetch, rebasedResidual)
		call.Yield(residualFilterPlan)
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

// tryTranslateValue attempts to translate a value through the fetch, returning
// nil if the value cannot be fully pushed.
//
// Ports Java's ScanWithFetchMatchCandidate.pushValueThroughFetch: the recursive
// mapMaybe translation is done by tryTranslateValueRec; this wrapper applies
// Java's FINAL filter — the translation succeeded only if the result is no
// longer correlated to the source alias
// (translatedValueOptional.filter(v -> !v.getCorrelatedTo().contains(sourceAlias)),
// ScanWithFetchMatchCandidate.java:75). A residual source correlation means some
// leaf could not be pushed through the fetch — e.g. a chained accessor whose
// intermediate steps have no covered column, or a translate function that
// rebased a leaf but left another child correlated. Emitting such a value would
// read a nested field off the flat partial record and fail plan validation
// downstream; declining here keeps the (un-pushed) filter above the fetch, which
// is a valid plan. Without this filter Go relied on nil-propagation from
// untranslatable leaves alone, which misses a leaf that "translates" (ok=true)
// but stays correlated.
func tryTranslateValue(
	fetchPlan *plans.RecordQueryFetchFromPartialRecordPlan,
	oldAlias, newAlias values.CorrelationIdentifier,
	v values.Value,
) values.Value {
	translated := tryTranslateValueRec(fetchPlan, oldAlias, newAlias, v)
	if translated == nil {
		return nil
	}
	if _, stillCorrelated := values.GetCorrelatedToOfValue(translated)[oldAlias]; stillCorrelated {
		return nil
	}
	return translated
}

// tryTranslateValueRec is the recursive mapMaybe core: it translates composite
// values (like ArithmeticValue) by translating their children first, then
// reconstructing the parent. Leaf values correlated to the source alias are
// translated via PushValue; uncorrelated values and constants pass through
// unchanged. The final "no residual source correlation" check lives in the
// tryTranslateValue wrapper, matching Java's structure.
func tryTranslateValueRec(
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
	// An ACCESSOR is a column read out of a row, and the ONLY authority on
	// whether a column survives the fetch is the fetch's covered-column set.
	// Every FieldValue therefore goes through PushValue — deliberately
	// WITHOUT consulting correlation first.
	//
	// Correlation cannot be the discriminator here. After ordinalization a
	// bare column is a FieldValue with Child == nil and an EMPTY correlation
	// set, so the "uncorrelated ⇒ nothing to translate ⇒ push unchanged"
	// shortcut below would carry it through the fetch untouched and assert
	// the column exists on the INDEX ENTRY. Nothing downstream catches that:
	// tryTranslateValue's final guard only looks for residual correlation to
	// oldAlias, which an ordinalized field never has. The predicate then
	// rides below the fetch reading a column the index does not cover and the
	// query returns WRONG ROWS — with INDEX idx_k ON t(k), `k = 5 AND (a > 1
	// OR b < 2)` pushed `(A > 1 OR B < 2)` onto a k-only index entry.
	//
	// Nor can "correlated to some OTHER alias ⇒ foreign row ⇒ pass through"
	// stand in: a column of the fetched row is correlated to the TABLE alias
	// while oldAlias is the filter's QUANTIFIER alias, so that test
	// misclassifies own-row columns as foreign and reopens the hole on join
	// legs. (The two alias namespaces are a known, separate defect.) Asking
	// the covered-column set is namespace-independent and answers the
	// question that actually matters.
	//
	// Java has no such hole: pushValueThroughFetch matches the WHOLE value
	// tree against the candidate's provided index Values via semanticEquals
	// (ScanWithFetchMatchCandidate.java:51-76), so an uncovered column simply
	// fails to match and correlation never enters into it.
	if _, isAccessor := v.(*values.FieldValue); isAccessor {
		translated, ok := fetchPlan.PushValue(v, oldAlias, newAlias)
		if !ok {
			return nil // the index does not cover this column — not pushable
		}
		if _, ofSource := values.GetCorrelatedToOfValue(v)[oldAlias]; !ofSource {
			// Covered, and already expressed in the target domain. Return it
			// UNCHANGED: PushValue answers two questions at once — "is this
			// column covered?" and "what does it look like past the fetch?" —
			// and only the first applies here. Its rewrite builds a fresh
			// FieldValue over the target QOV, discarding the resolved ordinal
			// and leaving the runtime to fail the row lookup with "ordinal
			// -1". Use it purely as the coverage oracle.
			return v
		}
		return translated
	}
	// Check if the value is correlated to the source alias.
	//
	// The pass-through is guarded by valueReadsAColumn for the same reason the
	// accessor branch above exists: a COMPOSITE over ordinalized fields — say
	// ArithmeticValue(FieldValue{A}, FieldValue{B}) for `a + b > 3` — has an
	// empty correlation set of its own, so an unguarded shortcut hands the
	// whole expression through the fetch without ever examining the columns it
	// reads. The accessor branch never sees them: the composite returns first.
	// Recursing instead routes every leaf through the covered-column check.
	correlated := values.GetCorrelatedToOfValue(v)
	if _, isCorrelated := correlated[oldAlias]; !isCorrelated && !valueReadsAColumn(v) {
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
		tc := tryTranslateValueRec(fetchPlan, oldAlias, newAlias, child)
		if tc == nil {
			return nil
		}
		translated[i] = tc
	}
	// Reconstruct with translated children.
	return values.WithChildren(v, translated)
}

// valueReadsAColumn reports whether evaluating v requires reading a column out
// of a row — i.e. whether its tree contains an accessor anywhere.
//
// This is the property the "uncorrelated ⇒ push through unchanged" shortcut in
// tryTranslateValueRec actually needs to test. Correlation is the wrong proxy
// for it: after ordinalization a bare column is a FieldValue with Child == nil
// and NO correlation, so a value can read half the record and still look
// perfectly uncorrelated.
func valueReadsAColumn(v values.Value) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(*values.FieldValue); ok {
		return true
	}
	for _, c := range v.Children() {
		if valueReadsAColumn(c) {
			return true
		}
	}
	return false
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
