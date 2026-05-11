package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// PlanningCostModelLess is the Java-aligned multi-criteria plan comparator.
// Mirrors Java's PlanningCostModel.compare() from fdb-record-layer-core.
//
// Returns true if a is strictly preferred over b. The comparison uses
// ordered tie-breaking criteria matching Java's priority:
//
//  1. Physical plan beats non-physical
//  2. Max cardinality of all data accesses
//  3. Fewer normalized residual predicates
//  4. Fewer data access operators (scan + index)
//  5. Recursive CTE tie-breaker (DFS > level-based)
//  6. IN-plan penalty (penalize if IN-values aren't SARGs)
//  7. Type filter count (fewer = better)
//  8. For index scans: fewer fetches
//  9. Distinct depth (deeper = better)
//  10. MAP/FILTER operation count (fewer = better)
//  11. Plan hash deterministic tie-break
//
// Criteria 7 (IndexScanPreference) and some sub-criteria of 8-9 are
// deferred to a follow-up shift — they require additional property
// evaluators not yet ported.
func PlanningCostModelLess(a, b expressions.RelationalExpression) bool {
	cmp := planningCostModelCompare(a, b)
	return cmp < 0
}

func planningCostModelCompare(a, b expressions.RelationalExpression) int {
	aIsPhysical := isPhysical(a)
	bIsPhysical := isPhysical(b)
	if aIsPhysical && !bIsPhysical {
		return -1
	}
	if !aIsPhysical && bIsPhysical {
		return 1
	}

	opsA := findExpressionsByType(a)
	opsB := findExpressionsByType(b)

	residualA := countResidualPredicates(a)
	residualB := countResidualPredicates(b)
	if residualA != residualB {
		return intCompare(residualA, residualB)
	}

	dataAccessA := opsA.scanCount + opsA.indexScanCount + opsA.coveringIndexCount
	dataAccessB := opsB.scanCount + opsB.indexScanCount + opsB.coveringIndexCount
	if dataAccessA != dataAccessB {
		return intCompare(dataAccessA, dataAccessB)
	}

	if cmp := compareRecursiveCTE(a, b); cmp != 0 {
		return cmp
	}

	if cmp := compareInPlan(a, b, opsA, opsB); cmp != 0 {
		return cmp
	}

	if cmp := comparePrimaryScanVsIndexScan(opsA, opsB); cmp != 0 {
		return cmp
	}

	if opsA.typeFilterCount != opsB.typeFilterCount {
		return intCompare(opsA.typeFilterCount, opsB.typeFilterCount)
	}

	if opsA.indexScanCount+opsA.coveringIndexCount > 0 &&
		opsB.indexScanCount+opsB.coveringIndexCount > 0 {
		fetchA := opsA.indexScanCount + opsA.fetchCount
		fetchB := opsB.indexScanCount + opsB.fetchCount
		if fetchA != fetchB {
			return intCompare(fetchA, fetchB)
		}
		fetchDepthA := expressionDepth(a, isFetchExpression)
		fetchDepthB := expressionDepth(b, isFetchExpression)
		if fetchDepthA >= 0 && fetchDepthB >= 0 && fetchDepthA != fetchDepthB {
			return intCompare(fetchDepthA, fetchDepthB)
		}
		if opsA.fetchCount != opsB.fetchCount {
			return intCompare(opsA.fetchCount, opsB.fetchCount)
		}
	}

	distinctDepthA := expressionDepth(a, isDistinctExpression)
	distinctDepthB := expressionDepth(b, isDistinctExpression)
	if distinctDepthA >= 0 && distinctDepthB >= 0 && distinctDepthA != distinctDepthB {
		return intCompare(distinctDepthB, distinctDepthA)
	}

	if opsA.unmatchedFieldCount != opsB.unmatchedFieldCount {
		return intCompare(opsA.unmatchedFieldCount, opsB.unmatchedFieldCount)
	}

	if opsA.inJoinCount != opsB.inJoinCount {
		return intCompare(opsB.inJoinCount, opsA.inJoinCount)
	}

	mapFilterA := opsA.mapCount + opsA.predicatesFilterCount
	mapFilterB := opsB.mapCount + opsB.predicatesFilterCount
	if mapFilterA != mapFilterB {
		return intCompare(mapFilterA, mapFilterB)
	}

	hashA := a.HashCodeWithoutChildren()
	hashB := b.HashCodeWithoutChildren()
	if hashA != hashB {
		if hashA < hashB {
			return -1
		}
		return 1
	}

	return 0
}

func isPhysical(e expressions.RelationalExpression) bool {
	_, ok := e.(physicalPlanExpression)
	return ok
}

type expressionCounts struct {
	scanCount             int
	indexScanCount        int
	coveringIndexCount    int
	fetchCount            int
	typeFilterCount       int
	inJoinCount           int
	inUnionCount          int
	mapCount              int
	predicatesFilterCount int
	unmatchedFieldCount   int
}

func findExpressionsByType(e expressions.RelationalExpression) expressionCounts {
	var counts expressionCounts
	walkExpressionTree(e, &counts)
	return counts
}

func walkExpressionTree(e expressions.RelationalExpression, counts *expressionCounts) {
	if e == nil {
		return
	}
	switch e.(type) {
	case *physicalScanWrapper:
		counts.scanCount++
	case *physicalIndexScanWrapper:
		w := e.(*physicalIndexScanWrapper)
		if w.covering {
			counts.coveringIndexCount++
		} else {
			counts.indexScanCount++
		}
		totalCols := len(w.columnNames)
		boundCols := 0
		if w.plan != nil {
			for _, cr := range w.plan.GetScanComparisons() {
				if !cr.IsEmpty() {
					boundCols++
				}
			}
		}
		counts.unmatchedFieldCount += totalCols - boundCols
	case *physicalTypeFilterWrapper:
		counts.typeFilterCount++
	case *physicalFilterWrapper:
		// regular filter, not counted as predicates filter
	case *physicalPredicatesFilterWrapper:
		counts.predicatesFilterCount++
	case *physicalMapWrapper:
		counts.mapCount++
	case *physicalInJoinWrapper:
		counts.inJoinCount++
	case *physicalInUnionWrapper:
		counts.inUnionCount++
	case *physicalFetchFromPartialRecordWrapper:
		counts.fetchCount++
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.AllMembers() {
			if _, ok := m.(physicalPlanExpression); ok {
				walkExpressionTree(m, counts)
				break
			}
		}
	}
}

func countResidualPredicates(e expressions.RelationalExpression) int {
	count := 0
	countResidualPredicatesRec(e, &count)
	return count
}

func countResidualPredicatesRec(e expressions.RelationalExpression, count *int) {
	if e == nil {
		return
	}
	if pf, ok := e.(*physicalPredicatesFilterWrapper); ok {
		for _, p := range pf.plan.GetPredicates() {
			*count += countConjuncts(p)
		}
	} else if ff, ok := e.(*physicalFilterWrapper); ok {
		*count += len(ff.plan.GetPredicates())
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.AllMembers() {
			if _, ok := m.(physicalPlanExpression); ok {
				countResidualPredicatesRec(m, count)
				break
			}
		}
	}
}

func countConjuncts(p predicates.QueryPredicate) int {
	if and, ok := p.(*predicates.AndPredicate); ok {
		total := 0
		for _, child := range and.Children() {
			total += countConjuncts(child)
		}
		return total
	}
	return 1
}

func compareRecursiveCTE(a, b expressions.RelationalExpression) int {
	_, aDFS := a.(*physicalRecursiveDfsJoinWrapper)
	_, bDFS := b.(*physicalRecursiveDfsJoinWrapper)
	_, aLevel := a.(*physicalRecursiveLevelUnionWrapper)
	_, bLevel := b.(*physicalRecursiveLevelUnionWrapper)

	if aDFS && bLevel {
		return -1
	}
	if aLevel && bDFS {
		return 1
	}
	return 0
}

func compareInPlan(_, _ expressions.RelationalExpression, _, _ expressionCounts) int {
	// Java's IN-plan penalty checks whether the IN-values are used as
	// SARGs (index search arguments) by inspecting scan comparison
	// correlation. Only penalizes when IN-values aren't SARGs. The full
	// SARG check requires ComparisonsProperty + correlation inspection.
	// Deferred to next shift — returning 0 (no preference) is safe.
	return 0
}

func expressionDepth(e expressions.RelationalExpression, match func(expressions.RelationalExpression) bool) int {
	return expressionDepthRec(e, match, 0)
}

func expressionDepthRec(e expressions.RelationalExpression, match func(expressions.RelationalExpression) bool, depth int) int {
	if e == nil {
		return -1
	}
	if match(e) {
		return depth
	}
	best := -1
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.AllMembers() {
			if _, ok := m.(physicalPlanExpression); ok {
				d := expressionDepthRec(m, match, depth+1)
				if d >= 0 && (best < 0 || d < best) {
					best = d
				}
				break
			}
		}
	}
	return best
}

func isDistinctExpression(e expressions.RelationalExpression) bool {
	_, ok := e.(*physicalDistinctWrapper)
	return ok
}

func isFetchExpression(e expressions.RelationalExpression) bool {
	_, ok := e.(*physicalFetchFromPartialRecordWrapper)
	if ok {
		return true
	}
	_, ok = e.(*physicalIndexScanWrapper)
	return ok
}

func comparePrimaryScanVsIndexScan(opsA, opsB expressionCounts) int {
	aIsPrimaryScan := opsA.scanCount == 1 && opsA.indexScanCount == 0 && opsA.coveringIndexCount == 0
	bIsPrimaryScan := opsB.scanCount == 1 && opsB.indexScanCount == 0 && opsB.coveringIndexCount == 0
	aIsIndexScan := opsA.scanCount == 0 && (opsA.indexScanCount+opsA.coveringIndexCount) == 1
	bIsIndexScan := opsB.scanCount == 0 && (opsB.indexScanCount+opsB.coveringIndexCount) == 1

	if aIsPrimaryScan && bIsIndexScan {
		return 1
	}
	if bIsPrimaryScan && aIsIndexScan {
		return -1
	}
	return 0
}

func intCompare(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
