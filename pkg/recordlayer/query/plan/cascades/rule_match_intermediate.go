package cascades

import (
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
// Like Java's RelationalExpression.match(), the structural path enumerates
// dependency-sound quantifier bijections and every compatible child-match
// branch. A strict shared visit budget may truncate the deterministic search as
// a safe optimization miss; it never falls back to positional pairing or
// relaxes a semantic gate.
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
	candidates := make([]MatchCandidate, 0)
	for _, childRef := range rangesOverRefs {
		for _, cand := range GetPartialMatchCandidatesTyped(childRef) {
			if _, seen := candidateSet[cand]; seen {
				continue
			}
			candidateSet[cand] = struct{}{}
			candidates = append(candidates, cand)
		}
	}

	// For each candidate, find parent expressions in the candidate's
	// traversal that reference the candidate-side refs from the child
	// PartialMatches. This mirrors Java's
	// MatchCandidate.findReferencingExpressions.
	for _, candidate := range candidates {
		traversal := candidate.GetTraversal()
		if traversal == nil {
			continue
		}

		referencingExpressions := findReferencingExpressionsForCandidate(
			rangesOverRefs, candidate, traversal,
		)

		// For each (candidateRef, candidateExpr) pair, attempt to
		// match the query expression against the candidate expression.
		for _, parent := range referencingExpressions {
			matchIntermediateWithCandidate(
				call, expr, candidate, parent.ref, parent.expr,
			)
		}
	}
}

// findReferencingExpressionsForCandidate implements Java's
// MatchCandidate.findReferencingExpressions: for each query-side child
// reference, retrieves the PartialMatches for the given candidate, then
// for each PartialMatch walks upward from the candidate-side reference
// to find the parent (ref, expr) pairs in the traversal.
//
// Returns pairs in first-discovery order. Preserving traversal order is
// load-bearing: the order becomes PartialMatch insertion order and eventually
// participates in deterministic equal-cost plan selection.
func findReferencingExpressionsForCandidate(
	queryChildRefs []*expressions.Reference,
	candidate MatchCandidate,
	traversal *Traversal,
) []refExprPair {
	var result []refExprPair

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
				result = append(result, parent)
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
// The exact structural path consumes complete dependency-sound bijections.
// The separate single-source Select path remains the bounded 190.4a
// candidate-existential subset; generic partial mappings are not admitted
// until Select-specific 190.4c semantics are present.
func matchIntermediateWithCandidate(
	call *ExpressionRuleCall,
	queryExpr expressions.RelationalExpression,
	candidate MatchCandidate,
	candidateRef *expressions.Reference,
	candidateExpr expressions.RelationalExpression,
) {
	// Structural and expression-specific subsumption are two routes for one
	// query/candidate-expression attempt, so they share one hard work/output
	// budget. Exhausting the structural route is a safe optimization miss; it
	// must not buy a second budget by falling through to the specialized path.
	budget := &matchIntermediateSearchBudget{}

	// Structural equality path: same expression type, same quantifier
	// count, structurally equal (ignoring children).
	if matchIntermediateStructural(
		call,
		queryExpr,
		candidate,
		candidateRef,
		candidateExpr,
		budget,
	) {
		return
	}
	if budget.shouldStop() {
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
		matchSingleSourceAgainstSelect(
			call,
			qe,
			flattenConjuncts(qe.GetPredicates()),
			cs,
			candidate,
			candidateRef,
			budget,
		)
	case *expressions.SelectExpression:
		// A pass-through single-source SelectExpression (the absorbed inner
		// of a join, PartitionBinarySelectRule output) matches an index
		// candidate exactly like a LogicalFilter — its correlated join
		// predicate SARGs the index, producing a correlated index-scan probe
		// (the inner of an index-nested-loop join). Java handles this via the
		// general SelectExpression.subsumedBy; the Go port previously only
		// matched LogicalFilter queries, so a join inner never index-matched.
		if isPassThroughSingleSourceSelect(qe) {
			matchSingleSourceAgainstSelect(
				call,
				qe,
				flattenConjuncts(qe.GetPredicates()),
				cs,
				candidate,
				candidateRef,
				budget,
			)
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
	budget *matchIntermediateSearchBudget,
) bool {
	queryQs := queryExpr.GetQuantifiers()
	candidateQs := candidateExpr.GetQuantifiers()

	if len(queryQs) != len(candidateQs) {
		return false
	}

	// 190.4b deliberately wires only the COMPLETE structural consumer. The
	// enumerator can expose dependency-sound proper subsets, but accepting
	// those requires Select-specific predicate/cardinality/compensation gates
	// that land separately in 190.4c.
	matched := false
	enumerateQuantifierMappings(
		queryQs,
		candidateQs,
		true,
		budget,
		func(mapping []quantifierMapping) bool {
			if tryIntermediateMapping(
				call,
				queryExpr,
				candidate,
				candidateRef,
				candidateExpr,
				queryQs,
				candidateQs,
				mapping,
				budget,
			) {
				matched = true
			}
			return !budget.shouldStop()
		},
	)
	return matched
}

// quantifierEdgesCompatible reports whether two quantifiers agree on the edge
// attributes that RelationalExpression.EqualsWithoutChildren excludes: kind,
// null-on-empty, and strict-single. Java pairs quantifiers via
// quantifier.semanticEquals (RelationalExpression.match's matchPredicate), which
// includes these; a match that ignored them could let e.g. a plain-ForEach leg
// substitute a NULL-ON-EMPTY leg and drop the null-extended row. Used on BOTH
// the structural bijection path and the single-source subsumption path so no
// match route can accept an edge-incompatible quantifier pairing.
func quantifierEdgesCompatible(a, b expressions.Quantifier) bool {
	return a.Kind() == b.Kind() &&
		a.IsNullOnEmpty() == b.IsNullOnEmpty() &&
		a.IsStrictSingle() == b.IsStrictSingle()
}

// tryIntermediateMapping expands every compatible child-PartialMatch branch
// for one complete alias skeleton. It returns true if the skeleton produced a
// semantically valid parent match, including an exact match already stored by
// an earlier rule firing.
func tryIntermediateMapping(
	call *ExpressionRuleCall,
	queryExpr expressions.RelationalExpression,
	candidate MatchCandidate,
	candidateRef *expressions.Reference,
	candidateExpr expressions.RelationalExpression,
	queryQs, candidateQs []expressions.Quantifier,
	mapping []quantifierMapping,
	budget *matchIntermediateSearchBudget,
) bool {
	topAliasBuilder := NewAliasMapBuilder()
	for _, pair := range mapping {
		queryQ := queryQs[pair.queryIndex]
		candidateQ := candidateQs[pair.candidateIndex]

		// Strict edge equality belongs to this exact structural consumer, not
		// the generic enumerator: Select subsumption can later authorize a
		// query Existential→candidate ForEach pair under additional gates.
		if !quantifierEdgesCompatible(queryQ, candidateQ) {
			return false
		}
		if !topAliasBuilder.PutChecked(
			queryQ.GetAlias(),
			candidateQ.GetAlias(),
		) {
			return false
		}
	}
	topAliasMap := topAliasBuilder.Build()

	selected := make([]quantifierPartialMatch, len(mapping))
	usedChildren := make(map[*PartialMatchImpl]struct{}, len(mapping))
	matched := false
	var expand func(int, *AliasMap) bool
	expand = func(depth int, composed *AliasMap) bool {
		if budget.shouldStop() {
			return false
		}
		if depth == len(mapping) {
			// Reaching a complete child product is a semantic match attempt,
			// whether node equality or metadata merge ultimately accepts it.
			// Charge before either potentially expensive operation so rejected
			// leaves cannot bypass the shared work cap.
			if !budget.chargeState() {
				return false
			}
			nodeAliasMap := expressions.EmptyAliasMap()
			for _, source := range composed.Sources() {
				var ok bool
				nodeAliasMap, ok = nodeAliasMap.With(
					source,
					composed.GetTarget(source),
				)
				if !ok {
					return true
				}
			}
			if !queryExpr.EqualsWithoutChildren(
				candidateExpr,
				nodeAliasMap,
			) {
				return true
			}

			mmm := buildMatchMaxMatchMap(
				queryExpr.GetResultValue(),
				candidateExpr.GetResultValue(),
				composed,
			)
			mi, ok := tryMergeRegularMatchInfo(
				composed,
				selected,
				nil,
				nil,
				mmm,
				EmptyGroupByMappings(),
				nil,
				nil,
			)
			if !ok {
				return true
			}
			pm := NewPartialMatch(
				composed,
				candidate,
				call.Reference,
				queryExpr,
				candidateRef,
				mi,
			)
			matched = true
			if budget.recordResult(pm) {
				AddPartialMatchForCandidate(
					call.Reference,
					candidate,
					pm,
				)
			}
			return !budget.shouldStop()
		}

		pair := mapping[depth]
		queryQ := queryQs[pair.queryIndex]
		candidateQ := candidateQs[pair.candidateIndex]
		return forEachPartialMatchForCandidate(
			queryQ.GetRangesOver(),
			candidate,
			func(child PartialMatch) bool {
				if !budget.chargeState() {
					return false
				}
				childImpl, ok := child.(*PartialMatchImpl)
				if !ok ||
					childImpl.GetCandidateRef().Canonical() !=
						candidateQ.GetRangesOver().Canonical() {
					return true
				}
				if _, duplicate := usedChildren[childImpl]; duplicate {
					return true
				}
				next, ok := mergeAliasMapsChecked(
					composed,
					childImpl.GetBoundAliasMap(),
				)
				if !ok {
					return true
				}
				usedChildren[childImpl] = struct{}{}
				selected[depth] = quantifierPartialMatch{
					quantifier:   queryQ,
					partialMatch: child,
				}
				if !expand(depth+1, next) {
					return false
				}
				delete(usedChildren, childImpl)
				selected[depth] = quantifierPartialMatch{}
				return true
			},
		)
	}
	expand(0, topAliasMap)
	return matched
}

func mergeAliasMapsChecked(left, right *AliasMap) (*AliasMap, bool) {
	builder := NewAliasMapBuilder()
	if left != nil && !builder.PutAllChecked(left) {
		return nil, false
	}
	if right != nil && !builder.PutAllChecked(right) {
		return nil, false
	}
	return builder.Build(), true
}

// matchSingleSourceAgainstSelect handles the subsumption case where a
// single-source query Filter or pass-through Select is matched against a
// candidate SelectExpression with Placeholder predicates. This is the core of
// index matching: query predicates (ComparisonPredicates) bind to candidate
// Placeholders, producing parameter bindings (ComparisonRanges) that the
// physical index scan uses.
//
// Algorithm:
//  1. The query must have exactly one ForEach quantifier. The candidate must
//     have exactly one ForEach quantifier, plus only guarded, semantically dead
//     existential quantifiers. A child PartialMatch must link the two ForEach
//     legs.
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
	budget *matchIntermediateSearchBudget,
) {
	// Step 1: Match the query's single ForEach quantifier to the candidate's
	// single ForEach quantifier. A candidate Select may additionally own
	// existential quantifiers, but only when they are provably dead: an
	// unmatched candidate ForEach can change cardinality, and a referenced
	// existential can filter or shape the candidate, so neither can be skipped.
	//
	// This is the bounded, compensation-safe subset of Java's non-exact
	// SelectExpression subsumption that this single-source path can represent.
	// The full Java matcher also enumerates query-side subsets and
	// Existential→ForEach pairings; those require multi-match identity and
	// general predicate-implication infrastructure beyond this routine.
	queryQs := queryExpr.GetQuantifiers()
	candidateQs := candidateSelect.GetQuantifiers()
	if len(queryQs) != 1 || queryQs[0].Kind() != expressions.QuantifierForEach || len(candidateQs) == 0 {
		return
	}
	switch candidateSelect.GetJoinType() {
	case expressions.JoinInner, expressions.JoinCross:
		// These join types do not encode directional/null-extension semantics.
	default:
		return
	}

	candidateForEachIndex := -1
	skippedCandidateAliases := make(map[values.CorrelationIdentifier]struct{})
	for i, candidateQ := range candidateQs {
		switch candidateQ.Kind() {
		case expressions.QuantifierForEach:
			if candidateForEachIndex >= 0 {
				return // every candidate ForEach must be matched
			}
			candidateForEachIndex = i
		case expressions.QuantifierExistential:
			skippedCandidateAliases[candidateQ.GetAlias()] = struct{}{}
		default:
			return
		}
	}
	if candidateForEachIndex < 0 {
		return
	}
	candidateQ := candidateQs[candidateForEachIndex]

	// A skipped existential is safe only when it is semantically inert. Check
	// every place at this node that could observe it, including tautological
	// predicates (the test is correlation-based, not predicate-class-based) and
	// dependencies of the selected ForEach leg.
	for skippedAlias := range skippedCandidateAliases {
		if _, referenced := values.GetCorrelatedToOfValue(candidateSelect.GetResultValue())[skippedAlias]; referenced {
			return
		}
		if _, dependency := candidateQ.GetCorrelatedTo()[skippedAlias]; dependency {
			return
		}
		for _, candidatePredicate := range candidateSelect.GetPredicates() {
			if _, referenced := predicates.GetCorrelatedToOfPredicate(candidatePredicate)[skippedAlias]; referenced {
				return
			}
		}
	}

	// The matched query/candidate quantifiers must have compatible edges, exactly
	// as the structural bijection path checks — this subsumption route bypasses
	// that path, so without this a pass-through Select over a NULL-ON-EMPTY leg
	// could subsume an index candidate over a plain ForEach leg and a later
	// substitution would drop the null-extended row.
	if !quantifierEdgesCompatible(queryQs[0], candidateQ) {
		return
	}

	// Select a child PartialMatch linking the two child references whose bound
	// aliases COMPOSE with the single quantifier mapping. A child reference can
	// hold several matches for the same candidate child (different bound maps); a
	// correlated child may bind the query alias to a leg incompatible with this
	// mapping, so try each and take the first that composes — never let
	// partial-match insertion ORDER decide whether the subsumption matches. The
	// composed alias map is reused below (Java only combines COMPATIBLE bound
	// maps).
	queryChildRef := queryQs[0].GetRangesOver()
	candidateChildRef := candidateQ.GetRangesOver()

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
			// A candidate predicate that filters rows cannot be ignored: the
			// candidate would then produce a subset of the query and no
			// compensation can restore the eliminated records. Java removes
			// only remaining tautologies at this point.
			if predicates.IsTautology(candPred) {
				continue
			}
			return
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

			// Index/PK matching is COMMUTATIVE: a join predicate `outer.fk =
			// inner.pk` constrains inner.pk exactly as `inner.pk = outer.fk`
			// does. The predicate is stored in SQL operand order, so for the
			// matched (inner) source the local column may sit on EITHER side. Try
			// the as-written orientation (column on the LHS), then the commuted
			// one (column on the RHS, operator flipped via ComparisonType.Commute).
			// tryFlatMapPlan hand-rolled this both-orientation probe for the inner
			// PK scan (matchJoinPKPredicate); the data-access path must do the same
			// so a join inner's correlated PK/index predicate SARGs into a bare
			// scan (Java's Value-based predicate matching is inherently
			// commutative). The CRUCIAL effect: the join predicate binds as a
			// sargable scan BOUND (residual-free, marked matched below so the
			// residual loop skips it), so the compensation is "noCompensationNeeded"
			// (Java PredicateWithValueAndRanges.java:423-432) and
			// DataAccessForMatchPartition returns a bare PHYSICAL probe — not a
			// LogicalFilter carrying an outer-correlated residual that
			// compensationSafeForYield must reject.
			rng := bindOrientedComparison(cp, ph, queryQs[0].GetAlias())
			if rng == nil {
				continue
			}

			paramBindings[ph.GetParameterAlias()] = rng
			matched = true
			matchedQueryPreds[queryPred] = true
			// Defer the sargable mapping until after the scan prefix is known
			// (see reconciliation below): a binding the candidate cannot consume
			// into its prefix must become a residual, not a dropped sargable.
			pendingSargables = append(pendingSargables, pendingSargable{ph: ph, cp: cp, rng: rng})
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
		).setKnownPredicateCompensation(
			reapplyResidualCompensation(residualPred),
			"residual",
		).Build()
		predicateMapBuilder.Put(residualPred, mapping)
		residualCount++
	}

	// Build the predicate map. BuildMaybe returns nil on conflicts
	// (not expected for a single-source expression). A nil result
	// with bound predicates means we hit a mapping conflict — bail.
	var predMultiMap *PredicateMultiMap
	if boundCount > 0 || residualCount > 0 {
		predMap := predicateMapBuilder.BuildMaybe()
		if predMap == nil {
			return
		}
		predMultiMap = &predMap.PredicateMultiMap
	}

	// Every compatible child branch participates. A child can carry parameter
	// or other metadata that conflicts with the local predicate bindings; that
	// rejects only that branch, never later alternatives.
	forEachPartialMatchForCandidate(
		queryChildRef,
		candidate,
		func(child PartialMatch) bool {
			if !budget.chargeState() {
				return false
			}
			childMatch, ok := child.(*PartialMatchImpl)
			if !ok ||
				childMatch.GetCandidateRef().Canonical() !=
					candidateChildRef.Canonical() {
				return true
			}
			aliasBuilder := NewAliasMapBuilder()
			if !aliasBuilder.PutAllChecked(childMatch.GetBoundAliasMap()) ||
				!aliasBuilder.PutChecked(
					queryQs[0].GetAlias(),
					candidateQ.GetAlias(),
				) {
				return true
			}
			boundAliasMap := aliasBuilder.Build()

			// This alias-compatible child is now a semantic match attempt.
			// Charge before value matching and metadata merge so a rejected
			// branch consumes the same bounded-work unit as a successful one.
			if !budget.chargeState() {
				return false
			}

			// MaxMatchMap between the query's result value and the candidate
			// SelectExpression's result value (Java SelectExpression.subsumedBy).
			// Mandatory — see buildMatchMaxMatchMap.
			mmm := buildMatchMaxMatchMap(
				queryExpr.GetResultValue(),
				candidateSelect.GetResultValue(),
				boundAliasMap,
			)
			mi, ok := tryMergeRegularMatchInfo(
				boundAliasMap,
				[]quantifierPartialMatch{{
					quantifier:   queryQs[0],
					partialMatch: childMatch,
				}},
				paramBindings,
				predMultiMap,
				mmm,
				EmptyGroupByMappings(),
				nil,
				nil,
			)
			if !ok {
				return true
			}

			pm := NewPartialMatch(
				boundAliasMap,
				candidate,
				call.Reference,
				queryExpr,
				candidateRef,
				mi,
			)
			if budget.recordResult(pm) {
				AddPartialMatchForCandidate(call.Reference, candidate, pm)
			}
			if budget.shouldStop() {
				return false
			}
			return true
		},
	)
}

// comparisonOrientation is one way to read a ComparisonPredicate as
// "column COMPARISON comparand": as-written (the LHS is the column) or commuted
// (the RHS is the column, operator flipped).
type comparisonOrientation struct {
	column     values.Value
	comparison predicates.Comparison
}

// comparisonOrientations returns the orientations to try when binding cp to a
// candidate placeholder. Index/PK matching is commutative, so the local column
// may be on either side of a join predicate. The as-written orientation is
// tried first (preserving behaviour for the common `column OP literal` shape);
// the commuted orientation is added only for a binary, commutable operator (the
// inner-leg join probe `outer.fk = inner.pk`, and the literal-on-the-left
// `5 = col`). Unary operators (IS [NOT] NULL) and non-commutable ones (IN,
// STARTS_WITH, LIKE) yield only the as-written orientation.
func comparisonOrientations(cp *predicates.ComparisonPredicate) []comparisonOrientation {
	out := []comparisonOrientation{{column: cp.Operand, comparison: cp.Comparison}}
	if cp.Comparison.Operand != nil {
		if flipped, ok := cp.Comparison.Type.Commute(); ok {
			commuted := cp.Comparison // copy preserves Escape and the other Comparison fields
			commuted.Type = flipped
			commuted.Operand = cp.Operand
			out = append(out, comparisonOrientation{column: cp.Comparison.Operand, comparison: commuted})
		}
	}
	return out
}

// bindOrientedComparison attempts to bind one of cp's operand orientations to
// the candidate placeholder ph as a sargable scan range over the matched source
// (sourceAlias). Returns the bound ComparisonRange, or nil if no orientation
// can SARG this placeholder. Each orientation must (1) be a sargable comparison
// type, (2) have its column operand be a column of the matched source — not an
// outer correlation, the field-name-collision guard — (3) have its COMPARAND be
// independently evaluable w.r.t. the matched source (an outer correlation or a
// constant, NOT a per-row column of the source — the self-comparison guard), (4)
// match the placeholder's column, and (5) be type-compatible. The comparand of the
// chosen orientation becomes the scan range's bound value (a correlated value for a
// join probe, a literal for an equality filter).
//
// Risk → 0-row / wrong-rows: the column must be the matched INNER source's column
// and the comparand a value evaluable WITHOUT a row of that source; when ambiguous
// (both sides are inner columns / a self-comparison `b = a`) or the operator is not
// commutable (IN / IS NULL / NOT EQUALS), no orientation SARGs and the predicate
// stays residual (correct rows). An outer column on the LHS fails guard (2); a
// source column on the comparand side fails guard (3) — which otherwise SARGs the
// circular range `a = <this row's b>` → 0 rows.
func bindOrientedComparison(
	cp *predicates.ComparisonPredicate,
	ph *predicates.Placeholder,
	sourceAlias values.CorrelationIdentifier,
) *predicates.ComparisonRange {
	for _, orient := range comparisonOrientations(cp) {
		if !isSargableComparisonForMatch(orient.comparison.Type) {
			continue
		}
		// The column operand must be a column of the matched source, not an
		// outer correlation. valuesMatchColumn compares the accessor PATH
		// alias-invariantly (the root correlation is excluded), so it cannot by
		// itself tell `Customer.id` from `Order.id` — both are path [ID]. This
		// guard is what pins the comparison to the matched source: without it a
		// join predicate like `Customer.id = Order.customer_id` matching the
		// ORDER source would bind Customer.id to Order's same-path PK column,
		// seeking Order.id = Customer.id — the wrong column, 0 rows
		// (TestFDB_InnerJoin). Reject a column operand whose correlations
		// exclude the matched source. A flat FieldValue (no correlation) is
		// assumed to be over the matched source.
		//
		// CRUCIAL: a multi-way join reads the OTHER side's
		// column through a merge RC (e.g. `(R⋈S).ID`) whose correlation set
		// names the merge legs — never the matched source — so the guard
		// rejects such a column and the predicate stays a residual filter
		// (a field-name collision, `.ID` vs the source PK `id`, would
		// otherwise bind the source's PK to the OTHER table's id — the wrong
		// column, 0 rows; TestFDB_MultiJoinWithFilter).
		colCorr := values.GetCorrelatedToOfValue(orient.column)
		if len(colCorr) > 0 {
			if _, ofSource := colCorr[sourceAlias]; !ofSource {
				continue
			}
		}
		// Comparand-side guard (design-ACK condition #1): the comparand bound into
		// the scan range must be INDEPENDENTLY EVALUABLE w.r.t. the matched source —
		// an outer correlation or a constant — NEVER a per-row column of the matched
		// source. A self-comparison `b = a` (both columns of the scanned row) would
		// otherwise bind the indexed column `a` and SARG the circular range
		// `a = <this row's b>` → 0 rows. This is the "self-cmp / both-inner → do NOT
		// SARG, leave residual" arm: rejecting here keeps the predicate a residual
		// filter (correct rows). A constant literal (`a = 5`) and a correlated join
		// probe (`inner.pk = outer.fk`) remain independently evaluable → still SARG.
		if !comparandIndependentOfSource(orient.comparison.Operand, sourceAlias) {
			continue
		}
		if !valuesMatchColumn(orient.column, ph.GetValue()) {
			continue
		}
		// Don't push a type-incompatible comparison (e.g. a BIGINT column vs a
		// string literal) into a scan range — it must surface as a residual so
		// the executor raises the type error, not silently produce an empty range.
		if fv, ok := orient.column.(*values.FieldValue); ok {
			if !comparisonTypesCompatible(fv, &orient.comparison) {
				continue
			}
		}
		comparison := orient.comparison
		mr := predicates.EmptyComparisonRange().Merge(&comparison)
		if !mr.Ok {
			continue
		}
		return mr.Range
	}
	return nil
}

// comparandIndependentOfSource reports whether comparand can be bound into a scan
// range over the matched source — i.e. it is evaluable WITHOUT a row of that source.
// It is independent iff:
//   - its correlation set is non-empty and EXCLUDES
//     sourceAlias — a pure OUTER correlation (the valid join-probe comparand,
//     `a = outer.fk`); or
//   - it references no column at all — a constant/literal (`a = 5`).
//
// It is NOT independent (→ leave the predicate residual) when it reads the matched
// source's own row: directly or via a merge RC (correlation includes sourceAlias),
// or as a FLAT FieldValue whose source correlation has been elided (a bare column
// of the scanned row, the `b = a` self-comparison case → circular range → 0 rows).
func comparandIndependentOfSource(comparand values.Value, sourceAlias values.CorrelationIdentifier) bool {
	if comparand == nil {
		// A unary comparison (IS [NOT] NULL) binds a null-range with no comparand —
		// nothing to evaluate against a source row, so no circular range is possible.
		return true
	}
	cc := values.GetCorrelatedToOfValue(comparand)
	if _, ofSource := cc[sourceAlias]; ofSource {
		return false
	}
	if len(cc) > 0 {
		return true // correlated only to OTHER alias(es) → outer, independently evaluable
	}
	// Empty correlation: independent only if it reads no column — a constant. A bare
	// FieldValue (a source column with its correlation elided) is NOT independent.
	readsColumn := false
	values.WalkValue(comparand, func(node values.Value) bool {
		if _, ok := node.(*values.FieldValue); ok {
			readsColumn = true
			return false
		}
		return true
	})
	return !readsColumn
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
	switch sel.GetJoinType() {
	case expressions.JoinInner, expressions.JoinCross:
		// A one-source Select has no join direction, but retaining this gate
		// prevents a future outer-join representation from entering the subset
		// path and losing null-extension semantics.
	default:
		return false
	}
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

// valuesMatchColumn reports whether a query column operand and a candidate
// placeholder value denote the same column. Column identity routes through the
// match-domain name-path comparison (values.ColumnNamePathsEqual): the full
// accessor path, not the leaf name, so a nested `addr.city` never binds a
// same-leaf-named top-level `city` index. The comparison is
// representation-agnostic — a resolver-baked query ref matches a lazy candidate
// over the same column (the candidate is name-based by construction; the query
// side may be baked) — and alias-invariant at the root, which is why the
// alias-map bridge the pre-name-path design needed is unnecessary. The caller's
// outer-correlation guard has already established both operands are over the
// matched source. CardinalityValue is a transparent wrapper handled by the same
// primitive; a complex non-column value (arithmetic, cast, …) that is not a
// distance key matches only by exact structural equality.
func valuesMatchColumn(queryValue, placeholderValue values.Value) bool {
	if queryValue == nil || placeholderValue == nil {
		return false
	}
	// Fast path / complex-expression path: exact structural equality (same
	// representation and aliases — arithmetic, casts, expression-index keys the
	// name-path primitive does not model).
	if values.ValuesStructurallyEqual(queryValue, placeholderValue) {
		return true
	}
	// Vector K-NN distance key: same metric class + same partition/argument
	// column paths.
	if distanceRowNumberValuesMatch(queryValue, placeholderValue) {
		return true
	}
	// Column identity across the mixed baked/lazy representation and any nested
	// accessor path — the wrong-column bind the leaf-name compare produced.
	return values.ColumnNamePathsEqual(queryValue, placeholderValue)
}

// distanceRowNumberValuesMatch reports whether a and b are the same
// distance-row-number metric class with matching partition + argument column
// accessor paths (alias-invariant at the root).
func distanceRowNumberValuesMatch(a, b values.Value) bool {
	ma, wa, oka := distanceRowNumberWindowed(a)
	mb, wb, okb := distanceRowNumberWindowed(b)
	if !oka || !okb || ma != mb {
		return false
	}
	return columnPathListsMatch(wa.PartitioningValues, wb.PartitioningValues) &&
		columnPathListsMatch(wa.ArgumentValues, wb.ArgumentValues)
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

// columnPathListsMatch reports whether two column-value lists are positionally
// equal by accessor name path — the path-aware replacement for a leaf-name list
// compare, so a nested partition/argument column is not conflated with a
// same-leaf-named top-level one.
func columnPathListsMatch(a, b []values.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !values.ColumnNamePathsEqual(a[i], b[i]) {
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
