package cascades

import (
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// Abstract data access utilities
// ---------------------------------------------------------------------------
//
// This file ports the non-abstract helper methods from Java's
// AbstractDataAccessRule as package-level functions. Concrete rules
// (AggregateDataAccessRule, future WithPrimaryKeyDataAccessRule) call
// these directly -- the Go equivalent of extending the abstract class.
//
// Ports Java's:
//   - prepareMatchesAndCompensations
//   - maximumCoverageMatches
//   - createScansForMatches
//   - dataAccessForMatchPartition
//
// Compensation and ordering satisfaction are fully wired (swingshift-86).
// Remaining: Pareto filtering in MaximumCoverageMatches (no containment
// pruning yet — every match is kept, which is conservative/correct).

// IntersectorFunc is the function type for computing intersections of
// multiple data accesses. Concrete rules provide their own intersection
// logic through this callback, replacing Java's abstract method
// createIntersectionAndCompensation.
type IntersectorFunc func(
	accesses []Vectored[*SingleMatchedAccess],
	requestedOrderings []*RequestedOrdering,
) *IntersectionResult

// PrepareMatchesAndCompensations compensates and sorts partial matches
// by coverage (descending bound predicate count). For each
// PartialMatch:
//   - assigns a unique candidateTopAlias
//   - computes compensation via CompensateCompleteMatch
//   - computes satisfying orderings via SatisfiesAnyRequestedOrderings
//   - creates a forward-scan SingleMatchedAccess with empty translation
//
// Returns the accesses sorted by coverage (highest first).
//
// Ports Java's AbstractDataAccessRule.prepareMatchesAndCompensations.
func PrepareMatchesAndCompensations(
	partialMatches []PartialMatch,
	requestedOrderings []*RequestedOrdering,
	_ PlanContext,
) []*SingleMatchedAccess {
	if len(partialMatches) == 0 {
		return nil
	}

	result := make([]*SingleMatchedAccess, 0, len(partialMatches))
	for _, pm := range partialMatches {
		candidateTopAlias := values.UniqueCorrelationIdentifier()

		var comp Compensation
		if pmi, ok := pm.(*PartialMatchImpl); ok {
			comp = pmi.CompensateCompleteMatch(nil, candidateTopAlias)
		} else {
			comp = NoCompensation
		}

		satisfying, scanDir := SatisfiesAnyRequestedOrderings(pm, requestedOrderings)
		// Skip a zero-prefix match (empty bound parameter prefix → a full
		// index scan) UNLESS it provides a requested ordering. A full index
		// scan with neither selectivity nor an ordering benefit is strictly
		// dominated by the full table scan + residual filter the planner
		// already has, and producing one per index lets a full scan of an
		// unrelated index (e.g. IDX_AMOUNT for a WHERE on customer_id) win
		// over the correct point lookup. ImplementIndexScanRule skips these
		// the same way (len(prefix) == 0 → continue); the data-access path
		// must too. (A restricted scan, or a zero-prefix scan that satisfies
		// the ORDER BY, IS kept — the latter for ordered full-index scans.)
		if satisfying == nil && !hasRestrictedScan(pm) {
			continue
		}

		// Required-for-binding gate (Java AbstractDataAccessRule line 665):
		// skip a match that did not bind every sargable alias the candidate
		// requires — it can't produce a valid physical plan. For a vector
		// candidate the index-only distance alias is required, so a
		// partition-only match (no DistanceRank, e.g. a plain WHERE on the
		// partition column) is discarded here instead of producing a
		// nil-query-vector scan.
		if reqCand, ok := pm.GetMatchCandidate().(interface {
			GetSargableAliasesRequiredForBinding() []values.CorrelationIdentifier
		}); ok {
			if required := reqCand.GetSargableAliasesRequiredForBinding(); len(required) > 0 {
				if pmi, ok := pm.(*PartialMatchImpl); ok {
					bound := pmi.GetBoundSargableAliases()
					allBound := true
					for _, a := range required {
						if _, ok := bound[a]; !ok {
							allBound = false
							break
						}
					}
					if !allBound {
						continue
					}
				}
			}
		}

		reverseScan := scanDir != nil && *scanDir == ScanDirectionReverse

		access := NewSingleMatchedAccess(
			pm,
			comp,
			candidateTopAlias,
			reverseScan,
			EmptyTranslationMap(),
			satisfying,
		)
		result = append(result, access)
	}

	// Sort by bound predicate count descending (maximum coverage first).
	// Ports Java's sort by getBoundPlaceholders().size().
	sort.SliceStable(result, func(i, j int) bool {
		iCount := boundPredicateCount(result[i].GetPartialMatch())
		jCount := boundPredicateCount(result[j].GetPartialMatch())
		return iCount > jCount
	})

	return result
}

// MaximumCoverageMatches eliminates PartialMatches whose coverage is
// entirely contained in other matches from the same MatchCandidate,
// then wraps survivors in Vectored with ascending position indices.
//
// The Pareto filtering logic (findContainingAccess) prunes dominated
// matches: if match A from candidate C binds {x, y, z} and match B
// from the same candidate C binds {x, y}, then B is dominated by A
// (A covers everything B covers plus more) and B is pruned.
//
// Ports Java's AbstractDataAccessRule.maximumCoverageMatches.
func MaximumCoverageMatches(
	partialMatches []PartialMatch,
	requestedOrderings []*RequestedOrdering,
	ctx PlanContext,
) []Vectored[*SingleMatchedAccess] {
	accesses := PrepareMatchesAndCompensations(partialMatches, requestedOrderings, ctx)
	if len(accesses) == 0 {
		return nil
	}

	var result []Vectored[*SingleMatchedAccess]
	idx := 0
	for i := range accesses {
		if !findContainingAccess(accesses, accesses[i]) {
			result = append(result, NewVectored(accesses[i], idx))
			idx++
		}
	}
	return result
}

// findContainingAccess checks whether `probe` is dominated by another
// access from the same MatchCandidate in the list. A probe is
// dominated if another match from the same candidate has strictly more
// bound sargable aliases that include all of the probe's.
//
// Ports Java's AbstractDataAccessRule.findContainingAccess.
func findContainingAccess(accesses []*SingleMatchedAccess, probe *SingleMatchedAccess) bool {
	probeMatch := probe.partialMatch
	probePMI, ok := probeMatch.(*PartialMatchImpl)
	if !ok {
		return false
	}
	probeBoundAliases := probePMI.GetBoundSargableAliases()

	for _, access := range accesses {
		if access == probe {
			continue
		}
		// Same MatchCandidate?
		if access.partialMatch.GetMatchCandidate() != probeMatch.GetMatchCandidate() {
			continue
		}
		accessPMI, ok := access.partialMatch.(*PartialMatchImpl)
		if !ok {
			continue
		}
		accessBoundAliases := accessPMI.GetBoundSargableAliases()

		// If probe has more or equal bindings, it can't be contained by this one.
		if len(probeBoundAliases) >= len(accessBoundAliases) {
			continue
		}

		// Check if access's bindings contain all of probe's.
		if containsAll(accessBoundAliases, probeBoundAliases) {
			return true
		}
	}
	return false
}

// containsAll reports whether `super` contains all keys in `sub`.
func containsAll(super, sub map[values.CorrelationIdentifier]struct{}) bool {
	for k := range sub {
		if _, ok := super[k]; !ok {
			return false
		}
	}
	return true
}

// CreateScansForMatches converts each SingleMatchedAccess into a
// physical scan plan by calling MatchCandidate.ToScanPlan with the
// bound parameter prefix and reverse flag.
//
// Returns a map from PartialMatch to RecordQueryPlan. The map uses
// the PartialMatch identity (pointer equality) as key.
//
// Ports Java's AbstractDataAccessRule.createScansForMatches.
func CreateScansForMatches(
	accesses []Vectored[*SingleMatchedAccess],
	_ PlanContext,
) map[PartialMatch]plans.RecordQueryPlan {
	result := make(map[PartialMatch]plans.RecordQueryPlan, len(accesses))
	for _, v := range accesses {
		access := v.Value
		pm := access.GetPartialMatch()
		candidate := pm.GetMatchCandidate()

		// Compute the bound parameter prefix from the match info's
		// parameter binding map.
		matchInfo := pm.GetMatchInfo()
		regularInfo := matchInfo.GetRegularMatchInfo()
		bindings := regularInfo.GetParameterBindingMap()
		prefix := candidate.ComputeBoundParameterPrefixMap(bindings)

		plan := candidate.ToScanPlan(prefix, access.IsReverseScanOrder())
		result[pm] = plan
	}
	return result
}

// DataAccessForMatchPartition orchestrates data access planning from
// a collection of PartialMatches. This is the main entry point that
// concrete rules call.
//
// Steps:
//  1. Compute maximum-coverage matches (with Pareto filtering)
//  2. Create scan plans for each viable match
//  3. For single match: wrap as expression with compensation
//  4. For multiple matches: try intersections via the provided
//     intersector callback
//
// The intersector is a function parameter so concrete rules can
// provide their own intersection logic (replaces Java's abstract
// createIntersectionAndCompensation method).
//
// Ports Java's AbstractDataAccessRule.dataAccessForMatchPartition.
func DataAccessForMatchPartition(
	requestedOrderings []*RequestedOrdering,
	partialMatches []PartialMatch,
	ctx PlanContext,
	intersector IntersectorFunc,
) []expressions.RelationalExpression {
	if len(partialMatches) == 0 {
		return nil
	}

	// Step 1: maximum coverage matches.
	bestMatches := MaximumCoverageMatches(partialMatches, requestedOrderings, ctx)
	if len(bestMatches) == 0 {
		return nil
	}

	// Step 2: create scan plans.
	scanMap := CreateScansForMatches(bestMatches, ctx)

	// Step 3: for each match, apply compensation and collect expressions.
	var resultExprs []expressions.RelationalExpression
	for _, v := range bestMatches {
		access := v.Value
		pm := access.GetPartialMatch()
		plan, ok := scanMap[pm]
		if !ok {
			continue
		}

		comp := access.GetCompensation()
		if comp.IsImpossible() {
			continue
		}

		// Determine if the index is covering: no result compensation
		// needed means all output fields are available from the index.
		cand := pm.GetMatchCandidate()
		isCovering := !comp.IsFinalNeeded()
		var coveringCols []string
		if isCovering {
			coveringCols = cand.GetColumnNames()
		}
		// Propagate the candidate's unique flag + column order onto the
		// scan wrapper, exactly as OrderedIndexScanRule does. These drive
		// the cost model (a unique index with all columns equality-bound
		// has provable max-cardinality 1 → cheapest point lookup) and the
		// ordering property (sort elimination). Omitting them made the
		// data-access scan look non-unique/unordered, so a non-unique
		// index could beat the unique one and sorts weren't eliminated.
		unique := false
		if u, ok := cand.(interface{ IsUnique() bool }); ok {
			unique = u.IsUnique()
		}
		var expr expressions.RelationalExpression = wrapScanPlanWithCoverage(plan, isCovering, coveringCols, unique, cand.GetColumnNames())

		if comp.IsNeeded() {
			if fmc, ok := comp.(*ForMatchCompensation); ok {
				// Java AbstractDataAccessRule.applyCompensationForSingleDataAccessMaybe:
				//   compensation.applyAllNeededCompensations(memoizer, plan,
				//       realizedAlias -> TranslationMap.ofAliases(candidateTopAlias, realizedAlias))
				// The function receives the matched query-side ForEach alias (the
				// realized base-quantifier alias) and rebases compensated predicates
				// from the candidate's top alias onto it.
				candidateTopAlias := access.GetCandidateTopAlias()
				expr = fmc.ApplyAllNeeded(expr, func(realizedAlias values.CorrelationIdentifier) TranslationMap {
					return TranslationMapOfAliases(candidateTopAlias, realizedAlias)
				})
			}
		}
		resultExprs = append(resultExprs, expr)
	}

	// Step 4: if multiple matches and an intersector is provided, try
	// intersections.
	if len(bestMatches) > 1 && intersector != nil {
		intersectionResult := intersector(bestMatches, requestedOrderings)
		if intersectionResult != nil && intersectionResult.IsViable() {
			resultExprs = append(resultExprs, intersectionResult.GetExpressions()...)
		}
	}

	return resultExprs
}

// candidateScanProps returns the unique flag and index column order for a
// match's candidate, for propagation onto the scan wrapper.
func candidateScanProps(cand MatchCandidate) (unique bool, columnNames []string) {
	if cand == nil {
		return false, nil
	}
	if u, ok := cand.(interface{ IsUnique() bool }); ok {
		unique = u.IsUnique()
	}
	return unique, cand.GetColumnNames()
}

// wrapAccessScan wraps a per-access scan plan, propagating the access's
// candidate unique flag + column order (used by the intersection branches).
func wrapAccessScan(access *SingleMatchedAccess, plan plans.RecordQueryPlan) expressions.RelationalExpression {
	unique, columnNames := candidateScanProps(access.GetPartialMatch().GetMatchCandidate())
	return wrapScanPlanWithCoverage(plan, false, nil, unique, columnNames)
}

// wrapScanPlanWithCoverage wraps a scan plan as the properly-typed physical
// RelationalExpression.
//
// For a Fetch(IndexScan) (what a value/windowed candidate's ToScanPlan always
// returns) the Fetch wrapper is ALWAYS emitted here and isCovering/coveringColumns
// are intentionally NOT consulted: covering is decided downstream by
// MergeProjectionAndFetchRule, which compares the actual projection's columns
// against the index's covered columns (index columns + PK) and eliminates the
// fetch when the projection is covered. That is strictly more precise than the
// coarse isCovering signal here (`!comp.IsFinalNeeded()`), which does not see
// the projection — eliminating the fetch on isCovering alone was wrong (it
// dropped fetches that a non-covered projection still needs). TestFDB_CoveringIndexScan
// pins that the deferral works: a covered projection yields IndexScan(... COVERING)
// with no Fetch; a non-covered one keeps the Fetch. (Costing nuance: the
// intermediate Fetch wrapper is costed non-covering during winner selection,
// before MergeProjectionAndFetch runs — codex P2; it does not change the chosen
// plan today and is folded into the template-aware costing work, RFC-076 step 3b.)
//
// For a bare IndexScan (no Fetch — e.g. a primary scan) isCovering IS applied
// directly, since there is no fetch to defer to.
func wrapScanPlanWithCoverage(plan plans.RecordQueryPlan, isCovering bool, coveringColumns []string, unique bool, columnNames []string) expressions.RelationalExpression {
	if fetchPlan, ok := plan.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		if innerIdx, ok := fetchPlan.GetInner().(*plans.RecordQueryIndexPlan); ok {
			// Covering is decided downstream by MergeProjectionAndFetchRule (see
			// the doc above) — do not consult isCovering/coveringColumns here.
			_ = coveringColumns
			idxWrapper := &physicalIndexScanWrapper{plan: innerIdx, unique: unique, columnNames: columnNames}
			idxRef := expressions.InitialOf(idxWrapper)
			fetchQ := expressions.ForEachQuantifier(idxRef)
			return NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
		}
	}
	if idxPlan, ok := plan.(*plans.RecordQueryIndexPlan); ok {
		if isCovering {
			idxPlan = idxPlan.WithCovering(coveringColumns)
		}
		return &physicalIndexScanWrapper{plan: idxPlan, covering: isCovering, unique: unique, columnNames: columnNames}
	}
	if vecPlan, ok := plan.(*plans.RecordQueryVectorIndexPlan); ok {
		return &physicalVectorIndexScanWrapper{plan: vecPlan}
	}
	return &scanPlanExpression{plan: plan}
}

// ---------------------------------------------------------------------------
// scanPlanExpression — wraps a RecordQueryPlan as a RelationalExpression
// ---------------------------------------------------------------------------

// scanPlanExpression is a thin wrapper that adapts a RecordQueryPlan to
// the RelationalExpression interface. Used by DataAccessForMatchPartition
// to yield scan plans as expressions into the memo.
//
// This mirrors the role of Java's physicalPlanExpression wrappers but is
// minimal — the full wrapper hierarchy (physicalIndexScanWrapper etc.)
// exists in physical_wrapper.go and handles the real planner flow. This
// type is used only by the abstract data access utilities when they need
// to return expressions.
type scanPlanExpression struct {
	plan plans.RecordQueryPlan
}

func (s *scanPlanExpression) GetResultValue() values.Value {
	return values.NewNullValue(values.UnknownType)
}

func (s *scanPlanExpression) GetQuantifiers() []expressions.Quantifier {
	return nil
}

func (s *scanPlanExpression) CanCorrelate() bool  { return false }
func (s *scanPlanExpression) ChildrenAsSet() bool { return false }
func (s *scanPlanExpression) HashCodeWithoutChildren() uint64 {
	return s.plan.HashCodeWithoutChildren()
}

// GetCorrelatedToWithoutChildren reports the outer correlations the wrapped plan
// carries. A bare PK RecordQueryScanPlan SARGed with a join predicate (`pk =
// QOV(outer).fk`) is a CORRELATED probe — returning nil here (the prior behaviour) let
// join-leg detection / winner-stamping treat it as self-contained and materialize/stamp
// it without tracking the outer alias (codex P2 on 05c742100; a pre-existing gap in the
// RFC-150 data-access correlation wiring, which reached physicalScanWrapper/
// physicalIndexScanWrapper but not this plan-backed leaf). dataAccessExprCorrelations
// reports the full set (SARG comparands + residual preds + map values, params excluded),
// the same source the physical scan wrappers use.
func (s *scanPlanExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return dataAccessExprCorrelations(s.plan)
}

func (s *scanPlanExpression) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*scanPlanExpression)
	if !ok {
		return false
	}
	return s.plan.EqualsWithoutChildren(o.plan)
}

func (s *scanPlanExpression) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return s
}

// GetRecordQueryPlan returns the underlying plan.
func (s *scanPlanExpression) GetRecordQueryPlan() plans.RecordQueryPlan {
	return s.plan
}

// compile-time check
var _ expressions.RelationalExpression = (*scanPlanExpression)(nil)

// ---------------------------------------------------------------------------
// Ordering satisfaction
// ---------------------------------------------------------------------------

// boundPredicateCount returns the number of bound sargable aliases
// for a PartialMatch. For PartialMatchImpl, uses GetBoundSargableAliases;
// for test stubs, falls back to matched ordering parts count.
func boundPredicateCount(pm PartialMatch) int {
	if pmi, ok := pm.(*PartialMatchImpl); ok {
		return len(pmi.GetBoundSargableAliases())
	}
	return len(pm.GetMatchInfo().GetMatchedOrderingParts())
}

// hasRestrictedScan reports whether the PartialMatch produces a scan
// plan with a non-empty bound parameter prefix (i.e., the scan
// actually restricts the key range). Full index scans (empty prefix)
// provide no selectivity advantage and should not be accepted as
// unordered alternatives.
func hasRestrictedScan(pm PartialMatch) bool {
	// Interface-based (not *PartialMatchImpl) so it also works for test
	// doubles. The bound parameter prefix map = the regular match info's
	// parameter binding map, fed through the candidate's prefix computer.
	rmi := pm.GetRegularMatchInfo()
	if rmi == nil {
		return false
	}
	bindings := rmi.GetParameterBindingMap()
	prefix := pm.GetMatchCandidate().ComputeBoundParameterPrefixMap(bindings)
	return len(prefix) > 0
}

// matchBoundPrefixIsCorrelated reports whether the PartialMatch's bound
// parameter prefix is restricted by a value that references an outer
// quantifier — i.e., the scan it produces is a CORRELATED access (a join
// predicate such as customer_id = c.id), not a local constant-bound
// predicate (status <> 'cancelled').
//
// Primary-key index intersections combine multiple independently-evaluable
// index scans by their shared primary key. A correlated leg is NOT
// independently evaluable: it depends on the FlatMap/NLJ outer binding, and
// Java never folds a correlated join predicate into an index intersection —
// it resolves the correlation via the FlatMap/NLJ and applies any remaining
// local predicate as a residual filter. Folding a correlated leg into a PK
// intersection yields a plan whose per-leg correlated binding the
// intersection cursor cannot evaluate, returning 0 rows (RFC-069). Such
// matches must therefore be excluded from intersection candidacy.
func matchBoundPrefixIsCorrelated(pm PartialMatch) bool {
	pmi, ok := pm.(*PartialMatchImpl)
	if !ok {
		return false
	}
	for _, cr := range pmi.GetBoundParameterPrefixMap() {
		if cr == nil || cr.IsEmpty() {
			continue
		}
		if cr.IsEquality() {
			if comparisonRowCorrelated(cr.GetEqualityComparison()) {
				return true
			}
		}
		if cr.IsInequality() {
			for _, ineq := range cr.GetInequalityComparisons() {
				if comparisonRowCorrelated(ineq) {
					return true
				}
			}
		}
	}
	return false
}

// comparisonRowCorrelated reports whether a bound comparison's RHS operand
// depends on a per-row OUTER quantifier (a join correlation such as c.id),
// as opposed to only constants. Plain ConstantValue literals carry no
// correlation at all; a ConstantObjectValue is a reference to the query's
// constant pool — bound once per execution, not per outer row — and likewise
// does not make the scan row-dependent. Only a genuine row-bearing correlation
// (a QuantifiedObjectValue etc. that survives subtracting constant-pool aliases)
// disqualifies a leg from an independently-evaluable primary-key intersection.
// (Today the SQL layer lowers WHERE constants as ConstantValue, so the
// subtraction is belt-and-suspenders, but it keeps the guard correct if literal
// parameterization to ConstantObjectValue is added later — Graefe review.)
func comparisonRowCorrelated(c *predicates.Comparison) bool {
	if c == nil {
		return false
	}
	corr := c.GetCorrelatedTo()
	if len(corr) == 0 {
		return false
	}
	if c.Operand != nil {
		values.WalkValue(c.Operand, func(node values.Value) bool {
			if cov, ok := node.(*values.ConstantObjectValue); ok {
				delete(corr, cov.Alias)
			}
			return true
		})
	}
	return len(corr) > 0
}

// SatisfiesRequestedOrdering checks if a PartialMatch's matched
// ordering parts satisfy a RequestedOrdering. Returns the scan
// direction needed, or nil if the ordering is not satisfied.
//
// Ports Java's AbstractDataAccessRule.satisfiesRequestedOrdering.
func SatisfiesRequestedOrdering(pm PartialMatch, ro *RequestedOrdering) *ScanDirection {
	if ro.IsPreserve() {
		both := ScanDirectionBoth
		return &both
	}

	resolved := ScanDirectionBoth
	mi := pm.GetMatchInfo()
	orderingParts := mi.GetMatchedOrderingParts()

	equalityBound := make(map[string]struct{})
	for _, op := range orderingParts {
		if op.GetComparisonRange().IsEquality() {
			equalityBound[values.ExplainValue(op.GetValue())] = struct{}{}
		}
	}

	opIdx := 0
	for _, reqPart := range ro.GetParts() {
		reqValue := reqPart.Value
		reqKey := values.ExplainValue(reqValue)

		if _, eq := equalityBound[reqKey]; eq {
			continue
		}

		found := false
		for opIdx < len(orderingParts) {
			op := orderingParts[opIdx]
			opIdx++
			if op.GetComparisonRange().IsEquality() {
				continue
			}

			opKey := values.ExplainValue(op.GetValue())
			if reqKey == opKey {
				reqSort := reqPart.SortOrder
				if reqSort != RequestedSortOrderAny {
					matchedSort := op.GetMatchedSortOrder()
					// Java AbstractDataAccessRule.satisfiesRequestedOrdering
					// (:820): the matched and requested NULL placement must
					// agree before the direction pick — a counterflow request
					// is not satisfied by a natural matched order (and vice
					// versa). Dropping this gate would let a data-access match
					// wrongly report "satisfied" for a counterflow request.
					if matchedSort.IsCounterflowNulls() != reqSort.IsCounterflowNulls() {
						return nil
					}
					reqDesc := reqSort.IsAnyDescending()
					if matchedSort.IsAnyDescending() == reqDesc {
						if resolved == ScanDirectionBoth {
							resolved = ScanDirectionForward
						} else if resolved != ScanDirectionForward {
							return nil
						}
					} else {
						if resolved == ScanDirectionBoth {
							resolved = ScanDirectionReverse
						} else if resolved != ScanDirectionReverse {
							return nil
						}
					}
				}
				found = true
				break
			}
			return nil
		}
		if !found {
			return nil
		}
	}

	return &resolved
}

// SatisfiesAnyRequestedOrderings filters requestedOrderings to those
// satisfied by the partial match. Returns the satisfied orderings and
// the scan direction, or nil if none are satisfied.
//
// Ports Java's AbstractDataAccessRule.satisfiesAnyRequestedOrderings.
func SatisfiesAnyRequestedOrderings(
	pm PartialMatch,
	requestedOrderings []*RequestedOrdering,
) ([]*RequestedOrdering, *ScanDirection) {
	seenForward := false
	seenReverse := false
	var satisfying []*RequestedOrdering

	for _, ro := range requestedOrderings {
		dir := SatisfiesRequestedOrdering(pm, ro)
		if dir != nil {
			satisfying = append(satisfying, ro)
			switch *dir {
			case ScanDirectionForward:
				seenForward = true
			case ScanDirectionReverse:
				seenReverse = true
			case ScanDirectionBoth:
				seenForward = true
				seenReverse = true
			}
		}
	}

	if !seenForward && !seenReverse {
		return nil, nil
	}

	var resolved ScanDirection
	if seenForward && seenReverse {
		resolved = ScanDirectionBoth
	} else if seenForward {
		resolved = ScanDirectionForward
	} else {
		resolved = ScanDirectionReverse
	}
	return satisfying, &resolved
}
