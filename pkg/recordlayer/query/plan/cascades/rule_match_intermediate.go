package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// MatchIntermediateRule is the Cascades rule that matches non-leaf
// query expressions (those with quantifiers) against candidate
// expressions by composing child PartialMatches. For every query
// expression with at least one quantifier, the rule:
//
//  1. Collects the child References from the expression's quantifiers.
//  2. Finds which MatchCandidates have PartialMatches on those child
//     References (seeded by MatchLeafRule or earlier
//     MatchIntermediateRule firings).
//  3. For each such candidate, walks upward through the candidate's
//     Traversal to find parent expressions that reference the
//     candidate-side References from those PartialMatches.
//  4. Attempts a structural match between the query expression and
//     each candidate parent expression, verifying that every quantifier
//     pair is backed by a child PartialMatch.
//  5. On match, creates a new composite PartialMatch and stores it on
//     the query Reference.
//
// This rule propagates matches upward from leaves, enabling multi-level
// expression trees to be matched against candidate (index) expression
// trees. It prepares AdjustMatchRule and physical-implementation rules
// to produce index-scan plans.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.rules.MatchIntermediateRule.
// The seed uses ordered quantifier matching (query[i] <-> candidate[i])
// rather than Java's full graph-matching enumeration via
// RelationalExpression.match(). This handles the common case (same
// quantifier count, same order) and will be extended to the full
// combinatorial matcher as needed.
type MatchIntermediateRule struct {
	matcher *ExpressionMatcher[expressions.RelationalExpression]
}

// NewMatchIntermediateRule constructs a MatchIntermediateRule.
func NewMatchIntermediateRule() *MatchIntermediateRule {
	return &MatchIntermediateRule{
		matcher: NewExpressionMatcher[expressions.RelationalExpression]("match_intermediate"),
	}
}

// Matcher returns the binding matcher. Matches any
// RelationalExpression (the non-leaf check is inside OnMatch). Mirrors
// Java's MatchIntermediateRule which returns Optional.empty() from
// getRootOperator().
func (r *MatchIntermediateRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch implements the intermediate matching logic. It collects
// child References, finds candidates with child PartialMatches, walks
// upward through each candidate's Traversal, and attempts structural
// matching at each candidate parent expression.
func (r *MatchIntermediateRule) OnMatch(call *ExpressionRuleCall) {
	expr := call.Bindings.Get(r.matcher).(expressions.RelationalExpression)

	// Only match non-leaf expressions (those with quantifiers).
	quantifiers := expr.GetQuantifiers()
	if len(quantifiers) == 0 {
		return // leaf — handled by MatchLeafRule
	}

	ctx := call.Context
	if ctx == nil {
		return
	}

	// Collect child references from all quantifiers.
	rangesOverRefs := make([]*expressions.Reference, 0, len(quantifiers))
	for _, q := range quantifiers {
		if ref := q.GetRangesOver(); ref != nil {
			rangesOverRefs = append(rangesOverRefs, ref)
		}
	}
	if len(rangesOverRefs) == 0 {
		return
	}

	// Form union of all match candidates that have PartialMatches on
	// any of the child references. This mirrors Java's:
	//   childMatchCandidates.addAll(rangesOverGroup.getMatchCandidates())
	candidateSet := make(map[MatchCandidate]struct{})
	for _, childRef := range rangesOverRefs {
		for _, cand := range GetPartialMatchCandidatesTyped(childRef) {
			candidateSet[cand] = struct{}{}
		}
	}

	// For each candidate, find parent expressions in the candidate's
	// traversal that reference the candidate-side refs from the child
	// PartialMatches. This mirrors Java's
	// MatchCandidate.findReferencingExpressions.
	for candidate := range candidateSet {
		traversal := candidate.GetTraversal()
		if traversal == nil {
			continue
		}

		refToExpressionMap := findReferencingExpressionsForCandidate(
			rangesOverRefs, candidate, traversal,
		)

		// For each (candidateRef, candidateExpr) pair, attempt to
		// match the query expression against the candidate expression.
		for candidateRef, candidateExprs := range refToExpressionMap {
			for _, candidateExpr := range candidateExprs {
				matchIntermediateWithCandidate(
					call, expr, candidate, candidateRef, candidateExpr,
				)
			}
		}
	}
}

// findReferencingExpressionsForCandidate implements Java's
// MatchCandidate.findReferencingExpressions: for each query-side child
// reference, retrieves the PartialMatches for the given candidate, then
// for each PartialMatch walks upward from the candidate-side reference
// to find the parent (ref, expr) pairs in the traversal.
//
// Returns a map from candidate Reference to the expressions that own
// quantifiers ranging over it.
func findReferencingExpressionsForCandidate(
	queryChildRefs []*expressions.Reference,
	candidate MatchCandidate,
	traversal *Traversal,
) map[*expressions.Reference][]expressions.RelationalExpression {
	result := make(map[*expressions.Reference][]expressions.RelationalExpression)

	type pairKey struct {
		ref  *expressions.Reference
		expr expressions.RelationalExpression
	}
	seen := make(map[pairKey]bool)

	for _, queryChildRef := range queryChildRefs {
		childMatches := GetPartialMatchesForCandidate(queryChildRef, candidate)
		for _, pm := range childMatches {
			pmi, ok := pm.(*PartialMatchImpl)
			if !ok {
				continue
			}
			candidateChildRef := pmi.GetCandidateRef()
			for _, parent := range traversal.GetParentRefPairs(candidateChildRef) {
				key := pairKey{ref: parent.ref, expr: parent.expr}
				if seen[key] {
					continue
				}
				seen[key] = true
				result[parent.ref] = append(result[parent.ref], parent.expr)
			}
		}
	}

	return result
}

// matchIntermediateWithCandidate attempts to match a query expression
// against a candidate expression at the intermediate (non-leaf) level.
// Checks structural equality of the expressions and verifies that
// every quantifier pair is backed by a child PartialMatch.
//
// Seed implementation: ordered quantifier matching (queryQs[i] <->
// candidateQs[i]). Java's full implementation uses
// RelationalExpression.match() which enumerates all valid quantifier
// permutations; the seed handles the common case of same-order,
// same-count quantifiers.
func matchIntermediateWithCandidate(
	call *ExpressionRuleCall,
	queryExpr expressions.RelationalExpression,
	candidate MatchCandidate,
	candidateRef *expressions.Reference,
	candidateExpr expressions.RelationalExpression,
) {
	// Structural equality path: same expression type, same quantifier
	// count, structurally equal (ignoring children).
	if matchIntermediateStructural(call, queryExpr, candidate, candidateRef, candidateExpr) {
		return
	}

	// Subsumption path: LogicalFilterExpression subsumed by
	// SelectExpression. The query filters rows from a scan via
	// ComparisonPredicates; the candidate models the same scan via a
	// SelectExpression with Placeholder predicates. The query
	// predicates bind to the candidate's Placeholders, producing
	// parameter bindings (sargable ranges) that the index scan uses.
	//
	// This is the Go equivalent of Java's match-then-subsumedBy path
	// where SelectExpression.subsumedBy handles predicate-to-
	// Placeholder mapping. SelectMergeRule normalises
	// Select(Filter(scan)) into flat Select(scan, preds) during
	// EXPLORE, but this inline path remains for LogicalFilter nodes
	// that aren't nested under a SelectExpression.
	cs, candidateIsSelect := candidateExpr.(*expressions.SelectExpression)
	if !candidateIsSelect {
		return
	}
	switch qe := queryExpr.(type) {
	case *expressions.LogicalFilterExpression:
		matchSingleSourceAgainstSelect(call, qe, flattenConjuncts(qe.GetPredicates()), cs, candidate, candidateRef)
	case *expressions.SelectExpression:
		// A pass-through single-source SelectExpression (the absorbed inner
		// of a join, PartitionBinarySelectRule output) matches an index
		// candidate exactly like a LogicalFilter — its correlated join
		// predicate SARGs the index, producing a correlated index-scan probe
		// (the inner of an index-nested-loop join). Java handles this via the
		// general SelectExpression.subsumedBy; the Go port previously only
		// matched LogicalFilter queries, so a join inner never index-matched.
		if isPassThroughSingleSourceSelect(qe) {
			matchSingleSourceAgainstSelect(call, qe, flattenConjuncts(qe.GetPredicates()), cs, candidate, candidateRef)
		}
	}
}

// matchIntermediateStructural handles the original same-type
// structural equality matching. Returns true if a PartialMatch was
// created (or at least the structural check passed enough to
// suppress further subsumption attempts).
func matchIntermediateStructural(
	call *ExpressionRuleCall,
	queryExpr expressions.RelationalExpression,
	candidate MatchCandidate,
	candidateRef *expressions.Reference,
	candidateExpr expressions.RelationalExpression,
) bool {
	queryQs := queryExpr.GetQuantifiers()
	candidateQs := candidateExpr.GetQuantifiers()

	if len(queryQs) != len(candidateQs) {
		return false
	}

	// Structural equality check at this level (ignoring children).
	exprAliasMap := expressions.EmptyAliasMap()
	if !queryExpr.EqualsWithoutChildren(candidateExpr, exprAliasMap) {
		return false
	}

	// Build the alias map from quantifier bindings and verify that
	// each quantifier pair is backed by a child PartialMatch.
	aliasBuilder := NewAliasMapBuilder()

	for i, queryQ := range queryQs {
		queryChildRef := queryQ.GetRangesOver()
		candidateChildRef := candidateQs[i].GetRangesOver()

		// Find a PartialMatch on queryChildRef for this candidate
		// whose candidate-side ref matches candidateChildRef.
		found := false
		childMatches := GetPartialMatchesForCandidate(queryChildRef, candidate)
		for _, pm := range childMatches {
			pmi, ok := pm.(*PartialMatchImpl)
			if !ok {
				continue
			}
			if pmi.GetCandidateRef() == candidateChildRef {
				// Incorporate the child's alias mappings.
				aliasBuilder.PutAll(pmi.GetBoundAliasMap())
				found = true
				break
			}
		}
		if !found {
			return false
		}

		// Map the query quantifier's alias to the candidate
		// quantifier's alias.
		aliasBuilder.Put(queryQ.GetAlias(), candidateQs[i].GetAlias())
	}

	// All quantifiers matched — create a composite PartialMatch.
	boundAliasMap := aliasBuilder.Build()

	// MaxMatchMap between the query's and candidate's result values
	// (Java's structural subsumedBy path). Mandatory — see
	// buildMatchMaxMatchMap.
	mmm := buildMatchMaxMatchMap(
		queryExpr.GetResultValue(),
		candidateExpr.GetResultValue(),
		boundAliasMap,
	)
	mi := NewRegularMatchInfo(
		nil,                    // parameterBindingMap
		boundAliasMap,          // bindingAliasMap
		nil,                    // predicateMap
		nil,                    // matchedOrderingParts
		mmm,                    // maxMatchMap
		EmptyGroupByMappings(), // groupByMappings
		nil,                    // rollUpToGroupingValues
		nil,                    // additionalPlanConstraint
	)

	pm := NewPartialMatch(
		boundAliasMap,
		candidate,
		call.Reference,
		queryExpr,
		candidateRef,
		mi,
	)
	AddPartialMatchForCandidate(call.Reference, candidate, pm)
	return true
}

// matchFilterAgainstSelect handles the subsumption case where a
// query LogicalFilterExpression is matched against a candidate
// SelectExpression with Placeholder predicates. This is the core
// of index matching: query predicates (ComparisonPredicates) bind
// to candidate Placeholders, producing parameter bindings
// (ComparisonRanges) that the physical index scan uses.
//
// Algorithm:
//  1. Both expressions must have exactly one quantifier (single-
//     source filter/select). The query's inner quantifier ranges
//     over the scan; the candidate's ForEach quantifier ranges over
//     the candidate scan. A child PartialMatch must link them.
//  2. For each candidate Placeholder, find a query
//     ComparisonPredicate whose operand references the same column.
//     If found, merge the comparison into a ComparisonRange and
//     record the binding. If not found, leave the Placeholder
//     unbound (empty range — the index column is unconstrained).
//  3. Build a PredicateMap recording which query predicate maps to
//     which candidate predicate. Build parameter bindings from the
//     ComparisonRanges.
//  4. Create a PartialMatch with the parameter bindings and
//     predicate map.
//
// Mirrors the predicate-mapping logic inside Java's
// SelectExpression.subsumedBy, narrowed to the Filter-vs-Select
// case that Go encounters alongside SelectMergeRule normalisation.
// pendingSargable is a candidate placeholder binding collected during matching,
// finalized as either a sargable scan constraint or a residual filter once the
// scan prefix is known.
type pendingSargable struct {
	ph  *predicates.Placeholder
	cp  *predicates.ComparisonPredicate
	rng *predicates.ComparisonRange
}

func matchSingleSourceAgainstSelect(
	call *ExpressionRuleCall,
	queryExpr expressions.RelationalExpression,
	queryPreds []predicates.QueryPredicate,
	candidateSelect *expressions.SelectExpression,
	candidate MatchCandidate,
	candidateRef *expressions.Reference,
) {
	// Step 1: Match quantifiers. Both sides must have exactly one.
	queryQs := queryExpr.GetQuantifiers()
	candidateQs := candidateSelect.GetQuantifiers()
	if len(queryQs) != 1 || len(candidateQs) != 1 {
		return
	}

	// Verify a child PartialMatch exists linking the two scan
	// references.
	queryChildRef := queryQs[0].GetRangesOver()
	candidateChildRef := candidateQs[0].GetRangesOver()

	var childMatch *PartialMatchImpl
	for _, pm := range GetPartialMatchesForCandidate(queryChildRef, candidate) {
		if pmi, ok := pm.(*PartialMatchImpl); ok {
			if pmi.GetCandidateRef() == candidateChildRef {
				childMatch = pmi
				break
			}
		}
	}
	if childMatch == nil {
		return
	}

	// Step 2: Match predicates. Try to bind each candidate
	// Placeholder with a query ComparisonPredicate.
	candidatePreds := candidateSelect.GetPredicates()

	paramBindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	predicateMapBuilder := NewPredicateMapBuilder()
	boundCount := 0
	// Track which query predicates were bound to a placeholder (sargable).
	// The rest become residual filters (Java: a query predicate with no
	// candidate match maps to a tautology candidate with an ofPredicate
	// compensation that re-applies it as a filter — SelectExpression.subsumedBy
	// → QueryPredicate.findImpliedMappings).
	matchedQueryPreds := make(map[predicates.QueryPredicate]bool, len(queryPreds))

	// Candidate bindings collected during the placeholder loop, finalized only
	// after the scan prefix is computed (see the reconciliation after the loop).
	var pendingSargables []pendingSargable

	for _, candPred := range candidatePreds {
		ph, ok := candPred.(*predicates.Placeholder)
		if !ok {
			// Non-Placeholder candidate predicates (e.g. constant
			// tautologies) are ignored for the seed — they don't
			// constrain the match.
			continue
		}

		matched := false
		for _, queryPred := range queryPreds {
			cp, ok := queryPred.(*predicates.ComparisonPredicate)
			if !ok {
				continue
			}
			if matchedQueryPreds[queryPred] {
				continue // already bound to an earlier placeholder
			}

			// Only sargable comparison types can bind to an index scan
			// constraint: value-index range types (= and binary inequalities
			// + STARTS_WITH) OR a vector DISTANCE_RANK bound. IN, IS NULL,
			// NOT EQUALS, LIKE, etc. cannot be a contiguous FDB range —
			// ComparisonRange.Merge would mis-classify them as an inequality
			// range (a bogus [<>] scan returning the wrong rows). They must
			// stay residual (or, for IN, take the explode→InJoin path).
			if !isSargableComparisonForMatch(cp.Comparison.Type) {
				continue
			}

			// The candidate-column operand must be a column of the source
			// being matched (queryQs[0]), not an outer correlation. valuesMatchColumn
			// compares FieldValues by field NAME only (the bound alias map isn't
			// built until after this step), so a join predicate like
			// `Customer.id = Order.customer_id`, when matching the ORDER source,
			// has its OUTER operand (Customer.id) on this side and would spuriously
			// bind to Order's same-named PK column (id) — seeking Order.id =
			// Customer.id, which is the wrong column and returns 0 rows
			// (TestFDB_InnerJoin). The query-side correlation and matchedQ.alias
			// share the table-alias namespace (7.1), so reject an operand whose
			// correlations exclude the matched source. Flat FieldValues (no
			// correlation) are assumed to be over the matched source.
			opCorr := values.GetCorrelatedToOfValue(cp.Operand)
			if len(opCorr) > 0 {
				if _, ofSource := opCorr[queryQs[0].GetAlias()]; !ofSource {
					continue
				}
			}

			// Check if the ComparisonPredicate's operand references
			// the same column as the Placeholder's value. Comparison
			// is structural via ExplainValue (field name for
			// FieldValue, full expression tree for complex values).
			if !valuesMatchColumn(cp.Operand, ph.GetValue()) {
				continue
			}

			// Don't push a type-incompatible comparison (e.g. a BIGINT
			// column vs a string literal) into a scan range — it must
			// surface as a residual so the executor raises the type error,
			// not silently produce an empty range. Mirrors the rule's guard.
			if fv, ok := cp.Operand.(*values.FieldValue); ok {
				if !comparisonTypesCompatible(fv, &cp.Comparison) {
					continue
				}
			}

			// Merge the comparison into a ComparisonRange.
			mr := predicates.EmptyComparisonRange().Merge(&cp.Comparison)
			if !mr.Ok {
				continue
			}

			paramBindings[ph.GetParameterAlias()] = mr.Range
			matched = true
			matchedQueryPreds[queryPred] = true
			// Defer the sargable mapping until after the scan prefix is known
			// (see reconciliation below): a binding the candidate cannot consume
			// into its prefix must become a residual, not a dropped sargable.
			pendingSargables = append(pendingSargables, pendingSargable{ph: ph, cp: cp, rng: mr.Range})
			break
		}

		if !matched {
			// Unbound Placeholder — index column is unconstrained.
			paramBindings[ph.GetParameterAlias()] = predicates.EmptyComparisonRange()
		}
	}

	// Reconcile bindings against the actual scan prefix. A comparison can match a
	// placeholder (right column, sargable type) yet not be consumable as a scan
	// constraint: a vector PARTITION inequality (the prefix is equality-leading
	// only), or a column whose leading prefix column is unbound (a positional
	// prefix cannot fix column N while column N-1 ranges free). Java's prefix
	// extraction stops at the same boundary. Such a binding must be re-applied as
	// a RESIDUAL filter, never silently dropped — dropping it returns wrong rows
	// (TestFDB_VectorSearch_MultiPartition_InequalityResidual: `region > 'r1'`
	// excluded the wrong partition) or hides an unplannable index-only composite.
	// ComputeBoundParameterPrefixMap is the single source of truth for what the
	// scan can actually constrain; the distance (index-only) binding it always
	// retains stays sargable.
	prefix := candidate.ComputeBoundParameterPrefixMap(paramBindings)
	for _, pb := range pendingSargables {
		if _, inPrefix := prefix[pb.ph.GetParameterAlias()]; inPrefix {
			mapping := RegularMappingBuilder(pb.cp, pb.cp, pb.ph).
				SetSargable(pb.ph.GetParameterAlias(), pb.rng).
				Build()
			predicateMapBuilder.Put(pb.cp, mapping)
			boundCount++
		} else {
			// Not consumable into the scan prefix → reclassify as residual.
			delete(matchedQueryPreds, predicates.QueryPredicate(pb.cp))
			paramBindings[pb.ph.GetParameterAlias()] = predicates.EmptyComparisonRange()
		}
	}

	// Residual predicates: any query predicate not bound to a placeholder
	// must be re-applied as a filter over the index scan (Java's residual
	// PredicateMapping with PredicateCompensationFunction.ofPredicate). A
	// match is produced even if EVERY predicate is residual (Java
	// SelectExpression.subsumedBy always produces the match; the resulting
	// full-index-scan is dominated by the table scan via cost/Pareto
	// pruning). Without this, the residual would be silently dropped →
	// wrong rows.
	residualCount := 0
	for _, queryPred := range queryPreds {
		if matchedQueryPreds[queryPred] {
			continue
		}
		residualPred := queryPred
		mapping := RegularMappingBuilder(
			residualPred,
			residualPred,
			predicates.NewConstantPredicate(predicates.TriTrue),
		).SetPredicateCompensation(reapplyResidualCompensation(residualPred)).Build()
		predicateMapBuilder.Put(residualPred, mapping)
		residualCount++
	}

	// Step 3: Build alias map incorporating child aliases +
	// quantifier mapping.
	aliasBuilder := NewAliasMapBuilder()
	aliasBuilder.PutAll(childMatch.GetBoundAliasMap())
	aliasBuilder.Put(queryQs[0].GetAlias(), candidateQs[0].GetAlias())
	boundAliasMap := aliasBuilder.Build()

	// Build the predicate map. BuildMaybe returns nil on conflicts
	// (shouldn't happen in the single-source seed). A nil result
	// with bound predicates means we hit a mapping conflict — bail.
	var predMultiMap *PredicateMultiMap
	if boundCount > 0 || residualCount > 0 {
		predMap := predicateMapBuilder.BuildMaybe()
		if predMap == nil {
			return
		}
		predMultiMap = &predMap.PredicateMultiMap
	}

	// MaxMatchMap between the query's result value and the candidate
	// SelectExpression's result value (Java SelectExpression.subsumedBy).
	// Mandatory — see buildMatchMaxMatchMap.
	mmm := buildMatchMaxMatchMap(
		queryExpr.GetResultValue(),
		candidateSelect.GetResultValue(),
		boundAliasMap,
	)
	mi := NewRegularMatchInfo(
		paramBindings,          // parameterBindingMap
		boundAliasMap,          // bindingAliasMap
		predMultiMap,           // predicateMap
		nil,                    // matchedOrderingParts
		mmm,                    // maxMatchMap
		EmptyGroupByMappings(), // groupByMappings
		nil,                    // rollUpToGroupingValues
		nil,                    // additionalPlanConstraint
	)
	mi.SetChildPartialMatch(queryQs[0].GetAlias(), childMatch)

	pm := NewPartialMatch(
		boundAliasMap,
		candidate,
		call.Reference,
		queryExpr,
		candidateRef,
		mi,
	)
	AddPartialMatchForCandidate(call.Reference, candidate, pm)
}

// isPassThroughSingleSourceSelect reports whether sel is a single-ForEach-
// quantifier SelectExpression whose result value flows the quantifier's row
// unchanged (a QuantifiedObjectValue over the quantifier). Such a Select is
// the absorbed-predicate inner of a join (PartitionBinarySelectRule output:
// Select([join pred], Scan) with result = quantifier's flowed object) and is
// structurally equivalent to a LogicalFilter for index-candidate matching —
// the predicate can SARG an index without any result-value compensation. A
// Select with a projecting/computing result value is NOT pass-through and
// must not take this path (the index scan returns full rows, not the
// projection), so it is rejected here.
func isPassThroughSingleSourceSelect(sel *expressions.SelectExpression) bool {
	qs := sel.GetQuantifiers()
	if len(qs) != 1 || qs[0].Kind() != expressions.QuantifierForEach {
		return false
	}
	if len(sel.GetPredicates()) == 0 {
		return false
	}
	qov, ok := sel.GetResultValue().(*values.QuantifiedObjectValue)
	return ok && qov.Correlation == qs[0].GetAlias()
}

// valuesMatchColumn checks if two values reference the same column.
// Uses structural comparison via ValuesStructurallyEqual; the cross-alias
// FieldValue fallback compares field names case-INsensitively (EqualFold) —
// belt-and-suspenders against any casing drift, even though SQL identifier
// resolution normalises column names to a single canonical casing. A case-only
// collision on a DIFFERENT table cannot leak through here: the caller already
// requires the operand to be correlated to the matched source (the
// outer-correlation guard), so this only compares columns of the same source
// against that source's candidate placeholders. For complex values (arithmetic,
// casts, etc.) it recursively compares the value tree.
func valuesMatchColumn(queryValue, placeholderValue values.Value) bool {
	if queryValue == nil || placeholderValue == nil {
		return false
	}
	// Fast path: structural equality (same field name, same child structure).
	if values.ValuesStructurallyEqual(queryValue, placeholderValue) {
		return true
	}
	// Cross-alias match: compare field names ignoring child QOV aliases.
	// This handles the case where the query has a flat FieldValue
	// ("COL") and the candidate has a child-bearing FieldValue
	// (QOV(alias)."COL") — or both have children with different aliases.
	// Mirrors Java's semanticEquals with alias equivalence map.
	qFV, qOk := queryValue.(*values.FieldValue)
	pFV, pOk := placeholderValue.(*values.FieldValue)
	if qOk && pOk {
		return strings.EqualFold(qFV.Field, pFV.Field)
	}
	// CARDINALITY() index: the query's predicate LHS for
	// `WHERE CARDINALITY(arr) = N` / `IS [NOT] NULL` is a
	// CardinalityValue(FieldValue(arr)); the candidate's placeholder is the same
	// value over the index column (built by ColumnValue). The QOV aliases differ
	// at this point (the alias map is built only after binding), so — like the
	// FieldValue and distance-row-number cases — match alias-invariantly by the
	// wrapped field name.
	if qCard, ok := queryValue.(*values.CardinalityValue); ok {
		if pCard, ok := placeholderValue.(*values.CardinalityValue); ok {
			return valuesMatchColumn(qCard.Child, pCard.Child)
		}
		return false
	}
	// Vector K-NN: the query's DistanceRank predicate LHS is a metric-specific
	// DistanceRowNumberValue; the candidate's distance placeholder is the same
	// value over the index columns. Match alias-invariantly by metric class +
	// partition/argument field names (the alias map is built only after this
	// binding step, so compare by name like the FieldValue case above).
	return distanceRowNumberValuesMatch(queryValue, placeholderValue)
}

// distanceRowNumberValuesMatch reports whether a and b are the same
// distance-row-number metric class with matching partition + argument field
// names (ignoring QOV aliases).
func distanceRowNumberValuesMatch(a, b values.Value) bool {
	ma, wa, oka := distanceRowNumberWindowed(a)
	mb, wb, okb := distanceRowNumberWindowed(b)
	if !oka || !okb || ma != mb {
		return false
	}
	return fieldNamesMatch(wa.PartitioningValues, wb.PartitioningValues) &&
		fieldNamesMatch(wa.ArgumentValues, wb.ArgumentValues)
}

// distanceRowNumberWindowed returns a metric tag + the embedded WindowedValue
// for the distance-row-number value variants, or ok=false otherwise.
func distanceRowNumberWindowed(v values.Value) (string, *values.WindowedValue, bool) {
	switch t := v.(type) {
	case *values.EuclideanDistanceRowNumberValue:
		return "euclidean", &t.WindowedValue, true
	case *values.EuclideanSquareDistanceRowNumberValue:
		return "euclidean_square", &t.WindowedValue, true
	case *values.CosineDistanceRowNumberValue:
		return "cosine", &t.WindowedValue, true
	case *values.DotProductDistanceRowNumberValue:
		return "dot_product", &t.WindowedValue, true
	default:
		return "", nil, false
	}
}

// fieldNamesMatch reports whether two value lists are positionally equal as
// FieldValues compared by (case-insensitive) field name, ignoring QOV aliases.
func fieldNamesMatch(a, b []values.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		fa, oka := a[i].(*values.FieldValue)
		fb, okb := b[i].(*values.FieldValue)
		if !oka || !okb || !strings.EqualFold(fa.Field, fb.Field) {
			return false
		}
	}
	return true
}

// flattenConjuncts recursively expands AndPredicates into their
// constituent conjuncts. [AND(a, b), c] → [a, b, c]. Non-AND
// predicates pass through unchanged.
func flattenConjuncts(preds []predicates.QueryPredicate) []predicates.QueryPredicate {
	var result []predicates.QueryPredicate
	for _, p := range preds {
		if and, ok := p.(*predicates.AndPredicate); ok {
			result = append(result, flattenConjuncts(and.SubPredicates)...)
		} else {
			result = append(result, p)
		}
	}
	return result
}

var _ ExpressionRule = (*MatchIntermediateRule)(nil)
