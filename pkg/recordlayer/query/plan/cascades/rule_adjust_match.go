package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ExpressionMatchAdjuster is an optional interface that a
// RelationalExpression can implement to support match adjustment.
// When the AdjustMatches pass encounters a candidate expression that
// implements this interface, it calls AdjustMatch to produce a refined
// MatchInfo wrapping the existing PartialMatch's MatchInfo.
//
// In Java, this is the default method
// RelationalExpression.adjustMatch(PartialMatch) which returns
// Optional.empty(). Concrete overrides exist on MatchableSortExpression
// and SelectExpression.
type ExpressionMatchAdjuster interface {
	// AdjustMatch attempts to refine the given PartialMatch by
	// absorbing this expression on the candidate side. Returns a new
	// MatchInfo on success, or nil if the expression cannot be
	// absorbed.
	AdjustMatch(pm PartialMatch) MatchInfo
}

// AdjustMatches walks all References reachable from rootRef and, for
// every existing PartialMatch, attempts to absorb candidate-side-only
// expressions one level up from the matched candidate ref. On success,
// a new PartialMatch with AdjustedMatchInfo is stored on the same
// query Reference but pointing to the parent candidate Reference.
//
// This is the Go equivalent of Java's AdjustMatchRule, which fires on
// PartialMatches via a TransformPartialMatch task. In Go,
// AdjustMatches is called explicitly after MatchLeafRule and
// MatchIntermediateRule have run, rather than being scheduled as a
// rule (see also AdjustPartialMatchesForRef at the data-access
// consumption boundary).
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.rules.AdjustMatchRule.
func AdjustMatches(rootRef *expressions.Reference) {
	visited := map[*expressions.Reference]bool{}
	adjustMatchesRecursive(rootRef, visited)
}

// adjustMatchesRecursive visits every Reference reachable from ref
// (depth-first, children before parents) and runs adjustPartialMatch
// on each existing PartialMatch.
func adjustMatchesRecursive(ref *expressions.Reference, visited map[*expressions.Reference]bool) {
	if visited[ref] {
		return
	}
	visited[ref] = true

	// Visit children first so that child-level matches are adjusted
	// before parent-level matches (bottom-up).
	for _, m := range ref.AllMembers() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				adjustMatchesRecursive(childRef, visited)
			}
		}
	}

	// Adjust partial matches on this ref. Loop until stable because
	// each adjustment may create new PMs that need further adjustment
	// (e.g., absorbing SelectExpression creates a PM that then absorbs
	// MatchableSortExpression). Mirrors Java's event-driven
	// AdjustMatchRule which re-fires on each new PartialMatch.
	AdjustPartialMatchesForRef(ref)
}

// AdjustPartialMatchesForRef runs the AdjustMatchRule absorption loop on
// a single Reference's partial matches until stable. Each round attempts
// to absorb a candidate-side parent expression (SelectExpression →
// MatchableSortExpression) onto each existing PartialMatch, producing
// adjusted matches that carry e.g. matched ordering parts.
//
// This is the per-ref unit of Java's event-driven AdjustMatchRule. It is
// invoked both by the bottom-up AdjustMatches walk (for REWRITING-phase
// matches) AND by pushDataAccessTasks just before it consumes a ref's
// matches — PLANNING-phase matches are seeded during exploration, after
// the phase-start AdjustMatches walk, so they must be adjusted at
// consumption time or their ordering parts (needed to satisfy a
// requested ordering and eliminate an in-memory sort) are never computed.
func AdjustPartialMatchesForRef(ref *expressions.Reference) {
	// Idempotence is handled per-match by AddPartialMatchForCandidate's content
	// dedup (a match is rejected when an existing match shares its query
	// expression + candidate ref), NOT by a coarse ref-level short-circuit.
	// `pushDataAccessTasks` fires repeatedly per ref during PLANNING and matches
	// arrive in waves (a second candidate can seed its matches AFTER an earlier
	// candidate's matches were already adjusted). A ref-level "any adjusted match
	// exists → skip the whole ref" guard would skip those later seeds entirely —
	// their matchedOrderingParts stay empty, so they can never satisfy an ORDER BY
	// and sort elimination silently degrades to a full scan + sort (@claude
	// finding 1). Instead the round loop runs every time: re-adjusting an
	// already-absorbed match produces a content-equivalent match that the dedup
	// rejects (adjustPartialMatch returns false → no progress → the loop
	// converges in one round), while a freshly-seeded match IS absorbed. This
	// relies on the content dedup actually firing — see the
	// TestAdjustPartialMatches_* regressions (no duplicate explosion across
	// repeated calls; late-seeded candidate waves still get adjusted).
	for round := 0; round < 8; round++ {
		progress := false
		for _, candAny := range ref.GetPartialMatchCandidates() {
			cand := candAny.(MatchCandidate)
			matches := GetPartialMatchesForCandidate(ref, cand)
			for _, pm := range matches {
				if adjustPartialMatch(ref, cand, pm) {
					progress = true
				}
			}
		}
		if !progress {
			break
		}
	}
}

// adjustPartialMatch implements the core of Java's AdjustMatchRule.
// For the given PartialMatch, it finds candidate expressions one level
// up from the matched candidate ref in the candidate's Traversal. For
// each such parent expression with exactly one quantifier ranging over
// the matched candidate ref, it attempts to absorb that expression via
// ExpressionMatchAdjuster.AdjustMatch. If the expression does not
// implement ExpressionMatchAdjuster, no adjustment occurs (matching
// Java's default Optional.empty() return).
//
// Mirrors Java's AdjustMatchRule.onMatch + matchWithCandidate.
func adjustPartialMatch(queryRef *expressions.Reference, candidate MatchCandidate, pm PartialMatch) bool {
	pmi, ok := pm.(*PartialMatchImpl)
	if !ok {
		return false
	}

	traversal := candidate.GetTraversal()
	if traversal == nil {
		return false
	}

	candidateRef := pmi.GetCandidateRef()
	parentPairs := traversal.GetParentRefPairs(candidateRef)

	added := false
	for _, parent := range parentPairs {
		parentRef := parent.ref
		parentExpr := parent.expr

		adjustedMI := matchWithCandidate(pmi, parentExpr)
		if adjustedMI == nil {
			continue
		}

		newPM := NewPartialMatch(
			pmi.GetBoundAliasMap(),
			candidate,
			queryRef,
			pmi.GetQueryExpression(),
			parentRef,
			adjustedMI,
		)
		if AddPartialMatchForCandidate(queryRef, candidate, newPM) {
			added = true
		}
	}
	return added
}

// matchWithCandidate checks whether a candidate expression can be
// absorbed into the given partial match. Mirrors Java's
// AdjustMatchRule.matchWithCandidate.
//
// Requirements for absorption:
//  1. The candidate expression must have exactly one quantifier.
//  2. That quantifier must range over the partial match's candidate ref.
//  3. The candidate expression must not introduce external correlations
//     beyond what its child provides (getCorrelatedTo equality check).
//  4. The expression must successfully adjust the match (via
//     ExpressionMatchAdjuster or the default AdjustedBuilder).
func matchWithCandidate(pm *PartialMatchImpl, candidateExpr expressions.RelationalExpression) MatchInfo {
	quantifiers := candidateExpr.GetQuantifiers()

	// Java: Verify.verify(!candidateExpression.getQuantifiers().isEmpty())
	if len(quantifiers) == 0 {
		return nil
	}

	// Java: if (candidateExpression.getQuantifiers().size() > 1) return Optional.empty()
	if len(quantifiers) > 1 {
		return nil
	}

	// Single quantifier — get its child reference.
	candidateQ := quantifiers[0]
	otherRangesOver := candidateQ.GetRangesOver()

	// Java: if (!candidateExpression.getCorrelatedTo().equals(otherRangesOver.getCorrelatedTo()))
	// Go checks the candidate expression's NODE-LOCAL correlations only
	// (see correlatedToEquals) — part of the Quantifier.GetCorrelatedTo
	// divergence registered in DIVERGENCES.md.
	if !correlatedToEquals(candidateExpr, otherRangesOver) {
		return nil
	}

	// Java: if (partialMatch.getCandidateRef() != otherRangesOver) return Optional.empty()
	// The quantifier must range over the same ref the partial match
	// points to.
	if pm.GetCandidateRef() != otherRangesOver {
		return nil
	}

	// Delegate to the expression's adjustMatch.
	// Java: return candidateExpression.adjustMatch(partialMatch)
	if adjuster, ok := candidateExpr.(ExpressionMatchAdjuster); ok {
		return adjuster.AdjustMatch(pm)
	}

	// MatchableSortExpression and SelectExpression live in the
	// expressions package which cannot import cascades (circular
	// dependency). Handle their adjustMatch logic here via type
	// assertion.
	if mse, ok := candidateExpr.(*expressions.MatchableSortExpression); ok {
		return adjustMatchForMatchableSort(mse, pm)
	}
	if sel, ok := candidateExpr.(*expressions.SelectExpression); ok {
		return adjustMatchForSelect(sel, pm)
	}

	// Default: Java's RelationalExpression.adjustMatch returns
	// Optional.empty(). Most expressions cannot be absorbed.
	return nil
}

// OrderingPartsComputer is an optional interface that MatchCandidate
// implementations can satisfy to provide ordering-part computation
// for MatchableSortExpression adjustment. It is optional rather than
// part of the MatchCandidate interface because only order-providing
// candidates implement it (ValueIndexScanMatchCandidate today; the
// caller below falls back for the rest).
//
// Ports the computeMatchedOrderingParts method from Java's
// MatchCandidate / ValueIndexLikeMatchCandidate.
type OrderingPartsComputer interface {
	// ComputeMatchedOrderingParts computes matched ordering parts from
	// this candidate's structure, the existing match info, the sort
	// parameter IDs, and the reverse flag. Returns a list of
	// MatchedOrderingParts describing the order of the outgoing data
	// stream.
	ComputeMatchedOrderingParts(
		matchInfo MatchInfo,
		sortParameterIDs []values.CorrelationIdentifier,
		isReverse bool,
	) []*MatchedOrderingPart
}

// adjustMatchForMatchableSort implements the adjustMatch logic for
// MatchableSortExpression. This lives in the cascades package (not on
// the expression type) because it needs PartialMatch, MatchInfo,
// AdjustedBuilder, and MatchCandidate — all cascades types that the
// expressions package cannot import.
//
// Ports Java's MatchableSortExpression.adjustMatch.
func adjustMatchForMatchableSort(mse *expressions.MatchableSortExpression, pm *PartialMatchImpl) MatchInfo {
	childMatchInfo := pm.GetMatchInfo()
	maxMatchMap := childMatchInfo.GetMaxMatchMap()
	if maxMatchMap == nil {
		return nil
	}

	innerAlias := mse.GetInner().GetAlias()
	rangedOver := map[values.CorrelationIdentifier]struct{}{innerAlias: {}}

	adjustedMaxMatchMap, ok := maxMatchMap.AdjustMaybe(
		innerAlias,
		mse.GetResultValue(),
		rangedOver,
	)
	if !ok {
		return nil
	}

	// Compute matched ordering parts. Java delegates to
	// matchCandidate.computeMatchedOrderingParts; if the candidate
	// implements OrderingPartsComputer, use it. Otherwise, fall back
	// to the child match info's existing ordering parts (no-op
	// ordering adjustment).
	var orderingParts []*MatchedOrderingPart
	if computer, ok := pm.GetMatchCandidate().(OrderingPartsComputer); ok {
		orderingParts = computer.ComputeMatchedOrderingParts(
			childMatchInfo,
			mse.GetSortParameterIDs(),
			mse.IsReverse(),
		)
	} else {
		orderingParts = childMatchInfo.GetMatchedOrderingParts()
	}

	candidateLowerRef := mse.GetInner().GetRangesOver()
	candidateLowerExpression, single := onlyReferenceMember(candidateLowerRef)
	if !single {
		return nil
	}
	groupByMappings, ok := AdjustGroupByMappings(
		childMatchInfo.GetGroupByMappings(),
		innerAlias,
		candidateLowerExpression,
	)
	if !ok {
		return nil
	}

	return NewAdjustedBuilder(childMatchInfo).
		SetMaxMatchMap(adjustedMaxMatchMap).
		SetMatchedOrderingParts(orderingParts).
		SetGroupByMappings(groupByMappings).
		Build()
}

// adjustMatchForSelect implements the adjustMatch logic for
// SelectExpression in the candidate traversal. Ports Java's
// SelectExpression.adjustMatch.
//
// The method is a near-pass-through: it adjusts the MaxMatchMap for the
// inner alias so that the match can walk through the Select to the
// MatchableSortExpression above. The predicate guard bails if any
// Select predicate is constrained (non-empty range on a Placeholder).
func adjustMatchForSelect(sel *expressions.SelectExpression, pm *PartialMatchImpl) MatchInfo {
	childMatchInfo := pm.GetMatchInfo()
	maxMatchMap := childMatchInfo.GetMaxMatchMap()
	if maxMatchMap == nil {
		return nil
	}

	preds := sel.GetPredicates()
	for _, pred := range preds {
		if ph, ok := pred.(*predicates.Placeholder); ok {
			if !ph.GetComparisonRange().IsEmpty() {
				return nil
			}
			continue
		}
		if _, ok := pred.(*predicates.ConstantPredicate); ok {
			continue
		}
		return nil
	}

	qs := sel.GetQuantifiers()
	if len(qs) != 1 {
		return nil
	}
	innerAlias := qs[0].GetAlias()
	rangedOver := map[values.CorrelationIdentifier]struct{}{innerAlias: {}}

	adjustedMaxMatchMap, ok := maxMatchMap.AdjustMaybe(
		innerAlias,
		sel.GetResultValue(),
		rangedOver,
	)
	if !ok {
		return nil
	}

	orderingParts := childMatchInfo.GetMatchedOrderingParts()
	candidateLowerRef := qs[0].GetRangesOver()
	candidateLowerExpression, single := onlyReferenceMember(candidateLowerRef)
	if !single {
		return nil
	}
	groupByMappings, ok := AdjustGroupByMappings(
		childMatchInfo.GetGroupByMappings(),
		innerAlias,
		candidateLowerExpression,
	)
	if !ok {
		return nil
	}

	return NewAdjustedBuilder(childMatchInfo).
		SetMaxMatchMap(adjustedMaxMatchMap).
		SetMatchedOrderingParts(orderingParts).
		SetGroupByMappings(groupByMappings).
		Build()
}

// correlatedToEquals is Go's stand-in for Java's check
//
//	!candidateExpression.getCorrelatedTo().equals(otherRangesOver.getCorrelatedTo())
//
// Since AdjustMatch fires on single-quantifier expressions where the
// quantifier IS the child, the expression's full getCorrelatedTo
// equals its node-local correlations union the child's — so requiring
// ZERO node-local correlations verifies the expression introduces no
// correlations beyond what the child already has. This is a stricter
// approximation of Java's set equality (part of the
// Quantifier.GetCorrelatedTo divergence, DIVERGENCES.md): switching to
// the full Reference.GetCorrelatedTo comparison on both sides changes
// what adjusts and needs its own review cycle.
func correlatedToEquals(expr expressions.RelationalExpression, _ *expressions.Reference) bool {
	nodeCorrs := expr.GetCorrelatedToWithoutChildren()
	return len(nodeCorrs) == 0
}
