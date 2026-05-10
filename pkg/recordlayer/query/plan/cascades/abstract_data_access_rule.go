package cascades

import (
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
		if satisfying == nil {
			satisfying = make([]*RequestedOrdering, len(requestedOrderings))
			copy(satisfying, requestedOrderings)
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
		isCovering := !comp.IsFinalNeeded()
		var expr expressions.RelationalExpression = wrapScanPlanWithCoverage(plan, isCovering)

		if comp.IsNeeded() {
			if fmc, ok := comp.(*ForMatchCompensation); ok {
				expr = fmc.ApplyAllNeeded(expr, EmptyTranslationMap())
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

// wrapScanPlan converts a RecordQueryPlan from the data access pipeline
// into the properly-typed RelationalExpression wrapper.
func wrapScanPlan(plan plans.RecordQueryPlan) expressions.RelationalExpression {
	return wrapScanPlanWithCoverage(plan, false)
}

// wrapScanPlanWithCoverage converts a RecordQueryPlan into the
// properly-typed RelationalExpression wrapper with coverage info.
//
// When isCovering=true AND the plan is a FetchFromPartialRecordPlan
// wrapping an IndexPlan, the fetch is eliminated and the index scan
// is marked as covering. This matches Java's path where
// CoveringIndexPlan (no Fetch needed) is produced when the index
// covers all output fields.
//
// When isCovering=false, the Fetch wrapper is preserved so push-through
// rules can optimize it (push filters below, eliminate via PushMap).
func wrapScanPlanWithCoverage(plan plans.RecordQueryPlan, isCovering bool) expressions.RelationalExpression {
	if fetchPlan, ok := plan.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		if innerIdx, ok := fetchPlan.GetInner().(*plans.RecordQueryIndexPlan); ok {
			if isCovering {
				// Index covers all needed columns — no fetch needed.
				return &physicalIndexScanWrapper{plan: innerIdx, covering: true}
			}
			// Non-covering: preserve the fetch wrapper.
			idxWrapper := &physicalIndexScanWrapper{plan: innerIdx}
			idxRef := expressions.NewFinalReference([]expressions.RelationalExpression{idxWrapper})
			fetchQ := expressions.ForEachQuantifier(idxRef)
			return NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, fetchQ)
		}
	}
	if idxPlan, ok := plan.(*plans.RecordQueryIndexPlan); ok {
		return &physicalIndexScanWrapper{plan: idxPlan, covering: isCovering}
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

func (s *scanPlanExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
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
					reqDesc := reqSort == RequestedSortOrderDescending
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
