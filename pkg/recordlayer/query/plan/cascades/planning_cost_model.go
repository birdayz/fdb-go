package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PlanningCostModelLess is the Java-aligned multi-criteria plan comparator.
// Mirrors Java's PlanningCostModel.compare() from fdb-record-layer-core.
//
// Returns true if a is strictly preferred over b. The comparison uses
// ordered tie-breaking criteria matching Java's priority:
//
//  1. Physical plan beats non-physical
//  2. Max cardinality of all data accesses (lower wins)
//  3. Fewer normalized residual predicates
//  4. Fewer data access operators (scan + index + covering)
//  5. Recursive CTE tie-breaker (DFS > level-based)
//  6. IN-plan penalty (penalize if IN-values aren't SARGs)
//  7. Primary scan vs index-scan-with-fetch (prefer primary)
//  8. Type filter count (fewer = better)
//  9. Type filter depth (deeper = better)
//  10. Index fetch metrics (fewer non-covering + fetch = better)
//  11. Distinct depth (deeper = better)
//  12. Unmatched index field count (fewer = better)
//  13. IN-join source count (more = better)
//  14. MAP + PredicatesFilter count (fewer = better)
//  15. Streaming aggregation beats hash aggregation
//  16. Scalar cost fallback (EstimateCost)
//  17. Plan hash deterministic tie-break
func PlanningCostModelLess(a, b expressions.RelationalExpression) bool {
	cmp := planningCostModelCompareWith(a, b, nil, nil)
	return cmp < 0
}

// NewPlanningCostModelLess returns a stats-aware cost model comparator
// with no PlanContext (index metadata for cardinality/unmatched-field
// criteria is resolved conservatively). Prefer NewPlanningCostModelLessWithContext.
func NewPlanningCostModelLess(stats properties.StatisticsProvider) func(a, b expressions.RelationalExpression) bool {
	return NewPlanningCostModelLessWithContext(stats, nil)
}

// NewPlanningCostModelLessWithContext returns a stats-aware cost model
// comparator. The returned function uses real record counts (via stats)
// for cardinality estimation and resolves index/primary-key metadata via
// ctx so the criterion-#2 (provable max cardinality) and criterion-#12
// (unmatched index fields) properties are computed faithfully from the
// CONCRETE plan tree (RFC-069). Pass nil stats for default
// (LeafScanCardinality); pass nil ctx to resolve index metadata
// conservatively (treat indexes as non-unique).
func NewPlanningCostModelLessWithContext(stats properties.StatisticsProvider, ctx PlanContext) func(a, b expressions.RelationalExpression) bool {
	return func(a, b expressions.RelationalExpression) bool {
		return planningCostModelCompareWith(a, b, stats, ctx) < 0
	}
}

// RewritingCostModelLess is the Java-aligned cost model for the REWRITING
// phase. Mirrors Java's RewritingCostModel.compare():
//  1. Fewer SelectExpressions
//  2. Fewer TableFunctionExpressions
//  3. Fewer normalized residual predicate conjuncts (CNF full-size)
//  4. More predicates at deeper levels (push predicates down)
//  5. Semantic hash tiebreak
func RewritingCostModelLess(a, b expressions.RelationalExpression) bool {
	return rewritingCostModelCompare(a, b) < 0
}

func rewritingCostModelCompare(a, b expressions.RelationalExpression) int {
	selectsA := properties.EvaluateExpressionCount(a, isSelectExpression)
	selectsB := properties.EvaluateExpressionCount(b, isSelectExpression)
	if selectsA != selectsB {
		return intCompare(selectsA, selectsB)
	}

	tfA := properties.EvaluateExpressionCount(a, isTableFunctionExpression)
	tfB := properties.EvaluateExpressionCount(b, isTableFunctionExpression)
	if tfA != tfB {
		return intCompare(tfA, tfB)
	}

	conjA := countResidualPredicates(a)
	conjB := countResidualPredicates(b)
	if conjA != conjB {
		return intCompare(conjA, conjB)
	}

	infoA := predicateCountByLevel(a)
	infoB := predicateCountByLevel(b)
	if cmp := comparePredicateCountByLevel(infoB, infoA); cmp != 0 {
		return cmp
	}

	hashA := deepHashCode(a)
	hashB := deepHashCode(b)
	if hashA != hashB {
		if hashA < hashB {
			return -1
		}
		return 1
	}
	return 0
}

// predicateCountByLevel computes predicate counts at each tree depth.
// Level 0 = leaves, increasing towards root. Matches Java's
// PredicateCountByLevelProperty.
func predicateCountByLevel(e expressions.RelationalExpression) map[int]int {
	result := map[int]int{}
	predicateCountByLevelRec(e, result)
	return result
}

func predicateCountByLevelRec(e expressions.RelationalExpression, counts map[int]int) int {
	if e == nil {
		return -1
	}
	maxChildLevel := -1
	// AllMembers (not firstPhysicalChild) — this runs in the REWRITING phase
	// where all members are logical and firstPhysicalChild would return nil.
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.AllMembers() {
			childLevel := predicateCountByLevelRec(m, counts)
			if childLevel > maxChildLevel {
				maxChildLevel = childLevel
			}
		}
	}
	currentLevel := maxChildLevel + 1
	predCount := 0
	if wp, ok := e.(expressions.RelationalExpressionWithPredicates); ok {
		predCount = len(wp.GetPredicates())
	}
	counts[currentLevel] += predCount
	return currentLevel
}

func comparePredicateCountByLevel(a, b map[int]int) int {
	maxLevelA, maxLevelB := -1, -1
	for k := range a {
		if k > maxLevelA {
			maxLevelA = k
		}
	}
	for k := range b {
		if k > maxLevelB {
			maxLevelB = k
		}
	}
	maxLevel := maxLevelA
	if maxLevelB > maxLevel {
		maxLevel = maxLevelB
	}
	for level := 0; level <= maxLevel; level++ {
		ac := a[level]
		bc := b[level]
		if ac != bc {
			return intCompare(ac, bc)
		}
	}
	return intCompare(maxLevelA, maxLevelB)
}

func isSelectExpression(e expressions.RelationalExpression) bool {
	_, ok := e.(*expressions.SelectExpression)
	return ok
}

func isTableFunctionExpression(e expressions.RelationalExpression) bool {
	_, ok := e.(*expressions.TableFunctionExpression)
	return ok
}

func planningCostModelCompareWith(a, b expressions.RelationalExpression, stats properties.StatisticsProvider, ctx PlanContext) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1
	}
	if b == nil {
		return -1
	}

	aIsPhysical := isPhysical(a)
	bIsPhysical := isPhysical(b)
	if aIsPhysical && !bIsPhysical {
		return -1
	}
	if !aIsPhysical && bIsPhysical {
		return 1
	}

	opsA := findExpressionsByType(a, stats, ctx)
	opsB := findExpressionsByType(b, stats, ctx)

	// Criterion #2: max cardinality of all data accesses — lower wins.
	// Unknown (-1) loses to known.
	cardA := opsA.maxDataAccessCardinality
	cardB := opsB.maxDataAccessCardinality
	if cardA >= 0 || cardB >= 0 {
		if cardA < 0 {
			return 1 // a unknown, b known — b wins
		}
		if cardB < 0 {
			return -1 // a known, b unknown — a wins
		}
		if cardA != cardB {
			if cardA < cardB {
				return -1
			}
			return 1
		}
	}

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

	// Join-order decision (recursive concrete-plan cost). For two join plans with the
	// SAME data-access count, the principled discriminator is the join's TOTAL cost over
	// its concrete tree — it captures driving from the smaller side and index-probing vs
	// re-scanning the larger table. This is Go's substitute for Java's CardinalitiesProperty
	// (which discriminates join orders EARLY, at the cardinality level), so it runs here —
	// before the structural fetch/unmatched/map tie-breakers (#7–#14). A "fewer index scans"
	// fetch heuristic must NOT override a large total-cost difference between join orders
	// (the RFC-069 multiway regression: an index-probe order lost to a full-scan order on
	// fetch count despite being far cheaper). It self-gates to join-wrapper pairs; non-join
	// comparisons fall straight through (RFC-069).
	if cmp := compareJoinOrdering(a, b, stats, ctx); cmp != 0 {
		return cmp
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

	typeFilterDepthA := costExprDepth(a, matchTypeFilter)
	typeFilterDepthB := costExprDepth(b, matchTypeFilter)
	if typeFilterDepthA >= 0 && typeFilterDepthB >= 0 && typeFilterDepthA != typeFilterDepthB {
		return intCompare(typeFilterDepthB, typeFilterDepthA)
	}

	if opsA.indexScanCount+opsA.coveringIndexCount > 0 &&
		opsB.indexScanCount+opsB.coveringIndexCount > 0 {
		fetchA := opsA.indexScanCount + opsA.fetchCount
		fetchB := opsB.indexScanCount + opsB.fetchCount
		if fetchA != fetchB {
			return intCompare(fetchA, fetchB)
		}
		fetchDepthA := costExprDepth(a, matchFetch)
		fetchDepthB := costExprDepth(b, matchFetch)
		if fetchDepthA >= 0 && fetchDepthB >= 0 && fetchDepthA != fetchDepthB {
			return intCompare(fetchDepthA, fetchDepthB)
		}
		if opsA.fetchCount != opsB.fetchCount {
			return intCompare(opsA.fetchCount, opsB.fetchCount)
		}
	}

	distinctDepthA := costExprDepth(a, matchDistinct)
	distinctDepthB := costExprDepth(b, matchDistinct)
	if distinctDepthA >= 0 && distinctDepthB >= 0 && distinctDepthA != distinctDepthB {
		return intCompare(distinctDepthB, distinctDepthA)
	}

	if opsA.unmatchedFieldCount != opsB.unmatchedFieldCount &&
		opsA.inMemorySortCount == 0 && opsB.inMemorySortCount == 0 {
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

	if cmp := compareFlatMapVsNLJ(opsA, opsB); cmp != 0 {
		return cmp
	}

	if opsA.nljPredicateCount != opsB.nljPredicateCount {
		return intCompare(opsB.nljPredicateCount, opsA.nljPredicateCount)
	}

	// Fall back to the scalar cost model when all multi-criteria tie.
	// This avoids the hash tiebreak picking semantically broken plans
	// (see D-4 wiring investigation). The scalar model's per-operator
	// cost formulas discriminate between plans that look identical to
	// the ordinal criteria.
	// Reject a redundant in-memory sort BEFORE the scalar-cost fallback. When two
	// plans tie on every ordinal criterion above (same data access, residuals,
	// joins, fetches, …) and differ only in how many in-memory sorts they carry,
	// the one with fewer sorts does strictly less work — a sort over identical
	// data access is pure overhead. Java eliminates such sorts structurally
	// (RemoveSortRule); Go's ImplementInMemorySortRule yields the sort
	// unconditionally and would otherwise rely on the scalar cost to discard it.
	// But the scalar fallback (EstimateCostWith) descends the Memo by best-member,
	// which costs a wrapper's child group at its CHEAPEST member rather than the
	// child actually embedded — so an InMemorySort over a StreamingAgg can look as
	// cheap as the aggregate group's cheapest member (an aggregate-index scan),
	// and a redundant ORDER BY sort over an already-grouping-ordered aggregate
	// wins at scale (RFC-069 group_by_status). Discriminating on sort count here,
	// before that phantom-prone fallback, restores the sort-eliminated plan.
	if opsA.inMemorySortCount != opsB.inMemorySortCount {
		return intCompare(opsA.inMemorySortCount, opsB.inMemorySortCount)
	}

	costA := properties.EstimateCostWith(a, stats)
	costB := properties.EstimateCostWith(b, stats)
	if costA.Less(costB) {
		return -1
	}
	if costB.Less(costA) {
		return 1
	}

	hashA := costExprHash(a)
	hashB := costExprHash(b)
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
	scanCount                int
	indexScanCount           int
	coveringIndexCount       int
	fetchCount               int
	typeFilterCount          int
	inJoinCount              int
	inUnionCount             int
	flatMapCount             int
	nestedLoopJoinCount      int
	mapCount                 int
	predicatesFilterCount    int
	unmatchedFieldCount      int
	inMemorySortCount        int
	nljPredicateCount        int
	maxDataAccessCardinality float64 // -1 means unknown (no PROVABLY-bounded data access)
	// unboundedDataAccess is set when ANY data access lacks a PROVABLE max-cardinality bound
	// (a range/partial/full scan, a non-unique or partially-bound index, an aggregate/vector
	// access). Mirrors Java's CardinalitiesProperty, where such an access is unknown and the
	// max-of-maxes is therefore unknown — so criterion #2 abstains rather than ranking by a
	// FilterSelectivity ESTIMATE (RFC-069).
	unboundedDataAccess bool
}

// scanProvableMaxCard returns a primary scan's PROVABLE max cardinality and whether it is known.
// Java's CardinalitiesProperty bounds a scan at 1 ONLY when every primary-key column is
// equality-bound (a point lookup); a range, partial bind, or full scan is unknown.
func scanProvableMaxCard(w *physicalScanWrapper) (float64, bool) {
	if w.plan == nil {
		return 0, false
	}
	comps := w.plan.GetScanComparisons()
	if len(comps) == 0 {
		return 0, false
	}
	numBound := 0
	allEquality := true
	for _, cr := range comps {
		if !cr.IsEmpty() {
			numBound++
			if !cr.IsEquality() {
				allEquality = false
			}
		}
	}
	if numBound > 0 && allEquality && numBound == len(comps) {
		return 1, true
	}
	return 0, false
}

// indexProvableMaxCard returns an index scan's PROVABLE max cardinality and whether it is known:
// 1 ONLY when the index is UNIQUE and every index column is equality-bound; otherwise unknown.
func indexProvableMaxCard(w *physicalIndexScanWrapper) (float64, bool) {
	if w.plan == nil || !w.unique {
		return 0, false
	}
	numBound := 0
	allEquality := true
	for _, cr := range w.plan.GetScanComparisons() {
		if !cr.IsEmpty() {
			numBound++
			if !cr.IsEquality() {
				allEquality = false
			}
		}
	}
	if numBound > 0 && allEquality && numBound == len(w.columnNames) {
		return 1, true
	}
	return 0, false
}

// findExpressionsByType computes the operator counts / provable cardinality /
// unmatched-field-count that the cost-model criteria consume.
//
// For a PHYSICAL expression it walks the CONCRETE RecordQueryPlan tree the
// wrapper carries (GetRecordQueryPlan) — Java's PlanningCostModel evaluates all
// properties over the concrete candidate plan, and the wrapper's plan is fully
// formed at construction (built from already-extracted child plans). This avoids
// the "phantom child" divergence where descending the memo References via the
// cheapest member counts a plan the extracted tree never executes (RFC-069).
//
// For a LOGICAL expression (no concrete plan yet) it retains the memo-descent
// walk: there is no extracted plan to read, so the phantom concept does not
// apply, and the best-physical-child descent is the established behaviour for
// ranking not-yet-implemented alternatives.
func findExpressionsByType(e expressions.RelationalExpression, stats properties.StatisticsProvider, ctx PlanContext) expressionCounts {
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	if ph, ok := e.(physicalPlanExpression); ok {
		if plan := ph.GetRecordQueryPlan(); plan != nil {
			// Template-aware: resolves a nil-inner Fetch shell's buried data access via
			// the expression graph (RFC-076 step 3b); falls through to the exact
			// phantom-free concretePlanCounts for template-free plans.
			return exprConcreteCounts(e, stats, ctx)
		}
	}
	counts := expressionCounts{maxDataAccessCardinality: -1}
	visited := make(map[*expressions.Reference]bool)
	walkExpressionTree(e, &counts, stats, visited)
	// Java's max-of-max-cardinalities is unknown if ANY data access is unbounded.
	if counts.unboundedDataAccess {
		counts.maxDataAccessCardinality = -1
	}
	return counts
}

// bestPhysicalChild picks the cost-best physical member from ref.
// Uses scalar EstimateCost to rank, matching Java's evaluateAtRef
// which expects exactly one member per Reference at cost-model time.
func bestPhysicalChild(ref *expressions.Reference, stats properties.StatisticsProvider) expressions.RelationalExpression {
	best := ref.GetBest(properties.CostLessWith(stats))
	if best != nil {
		if _, ok := best.(physicalPlanExpression); ok {
			return best
		}
	}
	return firstPhysicalChild(ref)
}

func walkExpressionTree(e expressions.RelationalExpression, counts *expressionCounts, stats properties.StatisticsProvider, visited map[*expressions.Reference]bool) {
	if e == nil {
		return
	}
	switch w := e.(type) {
	case *physicalScanWrapper:
		counts.scanCount++
		if card, known := scanProvableMaxCard(w); known {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
		}
	case *physicalAggregateIndexWrapper:
		counts.coveringIndexCount++
		// Aggregate access groups rows — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	case *physicalVectorIndexScanWrapper:
		counts.indexScanCount++
		// Top-K vector scan — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	case *physicalIndexScanWrapper:
		if w.covering {
			counts.coveringIndexCount++
		} else {
			counts.indexScanCount++
		}
		if card, known := indexProvableMaxCard(w); known {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
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
		counts.typeFilterCount += len(w.plan.GetRecordTypes())
	case *physicalFilterWrapper:
		_ = w // regular filter, not counted as predicates filter
	case *physicalPredicatesFilterWrapper:
		counts.predicatesFilterCount++
	case *physicalMapWrapper:
		counts.mapCount++
	case *physicalInJoinWrapper:
		counts.inJoinCount++
	case *physicalInUnionWrapper:
		counts.inUnionCount++
	case *physicalFlatMapWrapper:
		counts.flatMapCount++
	case *physicalNestedLoopJoinWrapper:
		counts.nestedLoopJoinCount++
		counts.nljPredicateCount += len(w.plan.GetPredicates())
	case *physicalFetchFromPartialRecordWrapper:
		counts.fetchCount++
	case *physicalInMemorySortWrapper:
		counts.inMemorySortCount++
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if visited[ref] {
			continue
		}
		visited[ref] = true
		if child := bestPhysicalChild(ref, stats); child != nil {
			walkExpressionTree(child, counts, stats, visited)
		}
	}
}

func countResidualPredicates(e expressions.RelationalExpression) int {
	if ph, ok := e.(physicalPlanExpression); ok {
		if plan := ph.GetRecordQueryPlan(); plan != nil {
			return concreteResidualPredicates(plan)
		}
	}
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
			*count += int(cnfSize(p))
		}
	} else if ff, ok := e.(*physicalFilterWrapper); ok {
		for _, p := range ff.plan.GetPredicates() {
			*count += int(cnfSize(p))
		}
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			countResidualPredicatesRec(child, count)
		}
	}
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

// compareInPlan implements Java's flipFlop(compareInOperator(a,b), compareInOperator(b,a)).
// If variant A is applicable (even if result is 0), variant B is never evaluated.
func compareInPlan(a, b expressions.RelationalExpression, _, _ expressionCounts) int {
	if cmp, applicable := compareInOperator(a); applicable {
		return cmp
	}
	if cmp, applicable := compareInOperator(b); applicable {
		return -cmp
	}
	return 0
}

// compareInOperator returns (penalty, applicable). applicable=false means the
// expression is not an IN-plan. Matches Java's OptionalInt return:
// empty → (0, false), present(0) → (0, true), present(1) → (1, true).
func compareInOperator(expr expressions.RelationalExpression) (int, bool) {
	var bindingNames []string
	switch w := expr.(type) {
	case *physicalInJoinWrapper:
		if w.plan != nil {
			bindingNames = []string{w.plan.GetBindingName()}
		}
	case *physicalInUnionWrapper:
		if w.plan != nil {
			bindingNames = w.plan.GetBindingNames()
		}
	default:
		return 0, false
	}
	if len(bindingNames) == 0 {
		return 0, false
	}

	sargedAliases := collectSargedAliases(expr)

	for _, name := range bindingNames {
		alias := values.NamedCorrelationIdentifier(name)
		if _, found := sargedAliases[alias]; found {
			return 0, true
		}
	}
	return 1, true
}

// collectSargedAliases walks the physical plan tree and collects all
// CorrelationIdentifiers that appear in equality comparisons of index
// scans. For intersection plans, takes the set intersection of children's
// aliases (only aliases SARGed by ALL legs count). For all other nodes,
// takes the union. Matches Java's ComparisonsProperty semantics.
func collectSargedAliases(e expressions.RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	if e == nil {
		return nil
	}
	if w, ok := e.(*physicalIndexScanWrapper); ok && w.plan != nil {
		return equalityAliasesFromRanges(w.plan.GetScanComparisons())
	}
	_, isIntersection := e.(*physicalIntersectionWrapper)
	_, isMultiIntersection := e.(*physicalMultiIntersectionWrapper)
	if isIntersection || isMultiIntersection {
		return intersectChildAliases(e)
	}
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			for alias := range collectSargedAliases(child) {
				out[alias] = struct{}{}
			}
		}
	}
	return out
}

func intersectChildAliases(e expressions.RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	var childSets []map[values.CorrelationIdentifier]struct{}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			childSets = append(childSets, collectSargedAliases(child))
		}
	}
	if len(childSets) == 0 {
		return nil
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	for alias := range childSets[0] {
		inAll := true
		for _, s := range childSets[1:] {
			if _, found := s[alias]; !found {
				inAll = false
				break
			}
		}
		if inAll {
			result[alias] = struct{}{}
		}
	}
	return result
}

func equalityAliasesFromRanges(ranges []*predicates.ComparisonRange) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, cr := range ranges {
		if cr == nil || !cr.IsEquality() {
			continue
		}
		eq := cr.GetEqualityComparison()
		if eq == nil {
			continue
		}
		if eq.Type != predicates.ComparisonEquals {
			continue
		}
		for alias := range eq.GetCorrelatedTo() {
			out[alias] = struct{}{}
		}
	}
	return out
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
		if child := firstPhysicalChild(ref); child != nil {
			d := expressionDepthRec(child, match, depth+1)
			if d >= 0 && (best < 0 || d < best) {
				best = d
			}
		}
	}
	return best
}

func isTypeFilterExpression(e expressions.RelationalExpression) bool {
	_, ok := e.(*physicalTypeFilterWrapper)
	return ok
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

// comparePrimaryScanVsIndexScan mirrors Java's comparePrimaryScanToIndexScan.
// Only fires when one plan is a singular primary scan and the other is a
// singular index scan WITH a fetch (non-covering or covering+fetch).
// A covering index without fetch is strictly better and doesn't enter this path.
func comparePrimaryScanVsIndexScan(opsA, opsB expressionCounts) int {
	aIsPrimaryScan := opsA.scanCount == 1 && opsA.indexScanCount == 0 && opsA.coveringIndexCount == 0 && opsA.inMemorySortCount == 0
	bIsPrimaryScan := opsB.scanCount == 1 && opsB.indexScanCount == 0 && opsB.coveringIndexCount == 0 && opsB.inMemorySortCount == 0
	aIsIndexScanWithFetch := isSingularIndexScanWithFetch(opsA)
	bIsIndexScanWithFetch := isSingularIndexScanWithFetch(opsB)

	if aIsPrimaryScan && bIsIndexScanWithFetch {
		return -1
	}
	if bIsPrimaryScan && aIsIndexScanWithFetch {
		return 1
	}
	return 0
}

func compareFlatMapVsNLJ(opsA, opsB expressionCounts) int {
	aHasFlatMap := opsA.flatMapCount > 0
	bHasFlatMap := opsB.flatMapCount > 0
	aHasNLJ := opsA.nestedLoopJoinCount > 0
	bHasNLJ := opsB.nestedLoopJoinCount > 0
	if aHasFlatMap && bHasNLJ && !aHasNLJ && !bHasFlatMap {
		return -1
	}
	if bHasFlatMap && aHasNLJ && !bHasNLJ && !aHasFlatMap {
		return 1
	}
	return 0
}

// isSingularIndexScanWithFetch matches Java's check: a single index scan
// (non-covering or covering) that is accompanied by a fetch.
func isSingularIndexScanWithFetch(ops expressionCounts) bool {
	if ops.scanCount != 0 {
		return false
	}
	if ops.indexScanCount == 1 {
		return true
	}
	return ops.coveringIndexCount == 1 && ops.fetchCount >= 1
}

// deepHashCode computes a recursive hash of the expression tree,
// matching Java's planHash(CURRENT_FOR_CONTINUATION). Combines the
// node's own hash with children's hashes via FNV mixing.
func deepHashCode(e expressions.RelationalExpression) uint64 {
	if e == nil {
		return 0
	}
	h := e.HashCodeWithoutChildren()
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			childHash := deepHashCode(child)
			h ^= childHash*0x517cc1b727220a95 + 0x6c62272e07bb0142
		}
	}
	return h
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

// compareJoinOrdering ranks two join plans (FlatMap or NestedLoopJoin) by their
// RECURSIVE TOTAL cost — Cascades' "combined cost with inputs" (§3.1): the whole
// join subtree, costed over the CONCRETE plan tree the wrapper carries. This
// replaces the prior top-outer-cardinality-only heuristic, which judged a
// multi-way plan by its top driver alone and let a plan with a great top driver
// but a pessimal inner join win — so the chosen order tracked FROM-clause
// position, not cost (RFC-041).
//
// It costs the concrete RecordQueryPlan (GetRecordQueryPlan) rather than walking
// the memo References via best/first member: the latter descends a join's inner
// into the cheapest member of a SHARED logical group, which is NOT the plan the
// extracted join actually runs (a correlated re-scanning inner gets costed as a
// cheap standalone access). That "phantom inner" under-costs the bad join order
// and let it win — the RFC-069 join-order regression both for the 2-way
// selective-predicate case and the multi-way index-probe cases. The wrapper's
// plan embeds its concrete children at construction, so this is the exact tree
// that executes (RFC-069).
//
// Confined to pairs whose concrete plans CONTAIN a join (FlatMap/NLJ) — NOT just
// pairs that ARE a bare join wrapper. The join-order alternatives at the top of a
// query are `Project(join)` / `InMemorySort(join)` members of one Reference, not
// bare join wrappers; gating on the wrapper TYPE missed them, so the structural
// fetch/unmatched tie-breakers (which prefer fewer index scans) picked a full-scan
// order over a far-cheaper index-probe order. Gating on "contains a join" lets the
// total concrete cost decide those, as it must (RFC-069). Non-join pairs fall
// through to the established structural + scalar criteria.
func compareJoinOrdering(a, b expressions.RelationalExpression, stats properties.StatisticsProvider, ctx PlanContext) int {
	pa, oka := a.(physicalPlanExpression)
	pb, okb := b.(physicalPlanExpression)
	if !oka || !okb {
		return 0
	}
	planA, planB := pa.GetRecordQueryPlan(), pb.GetRecordQueryPlan()
	if planA == nil || planB == nil {
		return 0
	}
	if !planContainsJoin(planA) || !planContainsJoin(planB) {
		return 0
	}
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	// Template-aware: resolves a join inner that is a nil-inner Fetch shell to its real
	// (ref-resolved) inner so the join order is costed honestly (RFC-076 step 3b);
	// template-free plans fall through to the exact phantom-free concretePlanCost.
	costA := exprConcreteCost(a, stats, ctx)
	costB := exprConcreteCost(b, stats, ctx)

	// A materialized NLJ vs a correlated FlatMap (DIFFERENT root join shapes) is a
	// Go-ONLY comparison — Java keeps no materialized NLJ (RewriteOuterJoinRule
	// canonicalizes the OuterJoinExpression to FlatMaps and the cost model only ever
	// ranks FlatMap-vs-FlatMap; see Java PlanningCostModel.compare, which gates its
	// join-ordering criterion on `a instanceof RecordQueryFlatMapPlan && b instanceof
	// RecordQueryFlatMapPlan`). For this Go-only pair the two cardinality FORMULAS are
	// inconsistent — nestedLoopJoinCost uses a cross-product proxy
	// (outerCard*innerCard*sel), flatMapCost an outer-only proxy (outerCard*sel) — for
	// the SAME logical join (identical true output cardinality, a group property), so
	// the cardinality term is an UNFAIR discriminator. Rank by WORK (CPU): with the
	// materialization fix (nestedLoopJoinCost charges the inner scanned ONCE), CPU
	// orders materialized-NLJ < re-scan-FlatMap for a non-probe inner and FlatMap < NLJ
	// for a card-1 probe inner — the correct plan for each, cost-driven, no rule
	// heuristic (RFC-152, Graefe).
	//
	// For SAME-shape pairs (FlatMap-vs-FlatMap, NLJ-vs-NLJ) the cardinality term is a
	// CONSISTENT, fair discriminator and is LOAD-BEARING — for two FlatMaps it is the
	// Java small-side-driving heuristic (drive from the lower-cardinality outer; Java
	// PlanningCostModel "Return the one with lower cardinality on the outer plan"). So
	// keep the full recursive Total there (the RFC-069 behaviour). Ranking those by CPU
	// would discard the outer-cardinality asymmetry and pick the larger-outer driver
	// (the order-invariant index-join regression). The CPU branch fires ONLY for the
	// Go-only NLJ-vs-FlatMap shape mismatch.
	if joinShapesDiffer(planA, planB) {
		if costA.CPU != costB.CPU {
			if costA.CPU < costB.CPU {
				return -1
			}
			return 1
		}
	}
	if costA.Less(costB) {
		return -1
	}
	if costB.Less(costA) {
		return 1
	}
	return 0
}

// topmostJoinIsNLJ reports whether the topmost join operator (NLJ or FlatMap) in a
// concrete plan tree is a materialized RecordQueryNestedLoopJoinPlan (true) or a
// correlated RecordQueryFlatMapPlan (false). The second return is false when the
// plan contains no join at all.
func topmostJoinIsNLJ(p plans.RecordQueryPlan) (isNLJ bool, found bool) {
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		if found {
			return false
		}
		switch n.(type) {
		case *plans.RecordQueryNestedLoopJoinPlan:
			isNLJ, found = true, true
			return false
		case *plans.RecordQueryFlatMapPlan:
			isNLJ, found = false, true
			return false
		}
		return true
	})
	return isNLJ, found
}

// joinShapesDiffer reports whether two plans' topmost join operators are different
// shapes — one a materialized NestedLoopJoin, the other a correlated FlatMap. This
// is the Go-only materialized-vs-re-scan comparison (Java has only FlatMaps), the
// one case where the per-shape cardinality proxies are inconsistent and the cost
// model must rank by work rather than the (unfair) cardinality term. Returns false
// for same-shape pairs (both FlatMap, both NLJ) and when either lacks a join.
func joinShapesDiffer(planA, planB plans.RecordQueryPlan) bool {
	aNLJ, aFound := topmostJoinIsNLJ(planA)
	bNLJ, bFound := topmostJoinIsNLJ(planB)
	if !aFound || !bFound {
		return false
	}
	return aNLJ != bNLJ
}

// concretePlanCost computes the recursive Cost of a CONCRETE RecordQueryPlan tree,
// mirroring the per-operator physical-wrapper HintCost formulas but walking the
// plan's actual GetChildren() (phantom-free — see compareJoinOrdering). Cardinality
// and CPU roll up from children exactly as the wrapper cost does, so a join's total
// cost reflects each sub-product's real (embedded) plan, not a shared-group winner.
func concretePlanCost(p plans.RecordQueryPlan, stats properties.StatisticsProvider, ctx PlanContext) properties.Cost {
	if p == nil {
		return properties.Cost{}
	}
	kids := p.GetChildren()
	child := make([]properties.Cost, len(kids))
	for i, c := range kids {
		child[i] = concretePlanCost(c, stats, ctx)
	}
	return combineConcreteCost(p, child, stats, ctx)
}

// combineConcreteCost applies a plan node's per-operator cost formula to its
// already-rolled-up child costs. Split out of concretePlanCost so the
// template-aware cost (exprConcreteCost, RFC-076 step 3b) can supply child costs
// that were resolved through the expression's quantifier graph — a nil-inner
// Fetch template's real inner — instead of the empty/free children its plan tree
// shows. The per-operator formulas (cost_formulas.go) are the single source of
// truth shared with the physical-wrapper HintCost methods.
func combineConcreteCost(p plans.RecordQueryPlan, child []properties.Cost, stats properties.StatisticsProvider, ctx PlanContext) properties.Cost {
	c0 := func() properties.Cost {
		if len(child) > 0 {
			return child[0]
		}
		return properties.Cost{}
	}
	switch pl := p.(type) {
	case *plans.RecordQueryScanPlan:
		// Primary-key scan: a full-equality bind on the PK is provably unique → 1 row.
		return scanLikeCost(pl.GetScanComparisons(), pl.GetRecordTypes(), stats, true)
	case *plans.RecordQueryIndexPlan:
		// Secondary index: a full-equality bind is a single row only if the index is
		// UNIQUE. Resolve uniqueness from PlanContext when available (codex review);
		// a nil/empty ctx falls back to non-unique (conservative bucket estimate) so
		// a non-unique equality (`status = ?`) is never mispriced as a point probe.
		_, unique := indexMetadata(pl, ctx)
		return scanLikeCost(pl.GetScanComparisons(), pl.GetRecordTypes(), stats, unique)
	case *plans.RecordQueryFlatMapPlan:
		if len(child) < 2 {
			return properties.Cost{}
		}
		return flatMapCost(child[0], child[1])
	case *plans.RecordQueryNestedLoopJoinPlan:
		if len(child) < 2 {
			return properties.Cost{}
		}
		return nestedLoopJoinCost(child[0], child[1])
	case *plans.RecordQueryPredicatesFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return filterCost(c0(), len(pl.GetPredicates()))
	case *plans.RecordQueryFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return filterCost(c0(), len(pl.GetPredicates()))
	case *plans.RecordQueryTypeFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return typeFilterCost(c0())
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return fetchCost(c0())
	case *plans.RecordQueryMapPlan, *plans.RecordQueryProjectionPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return mapCost(c0())
	case *plans.RecordQueryFirstOrDefaultPlan:
		return firstOrDefaultCost(c0())
	case *plans.RecordQueryInMemorySortPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return inMemorySortCost(c0())
	case *plans.RecordQueryDistinctPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return distinctCost(c0())
	case *plans.RecordQueryIntersectionPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return intersectionCost(child)
	default:
		// Transparent / unknown: roll up the first child's cardinality + summed CPU.
		sumCPU := 0.0
		for _, c := range child {
			sumCPU += c.CPU
		}
		if len(child) > 0 {
			return properties.Cost{Cardinality: child[0].Cardinality, CPU: sumCPU}
		}
		return properties.Cost{Cardinality: properties.LeafScanCardinality, CPU: properties.LeafScanCardinality * properties.ScanCPU}
	}
}

// scanLikeCost is the metadata-independent leaf cost for the concrete join-ordering
// recursion (provably-unique full-equality bind → 1 row; else table cardinality ×
// per-comparison selectivity × the physical-wrapper discount). The scan/index
// WRAPPER HintCost is metadata-aware (unique/covering) for the memo cost framework;
// the join-ordering cost deliberately uses selectivity-only leaf cost so it is
// consistent without PlanContext (RFC-069).
//
// fullBindUnique gates the 1-row shortcut: a fully-equality-bound access yields a
// single row ONLY when the access is provably unique. A primary-key scan with every
// PK column bound is unique (pass true); a secondary INDEX scan may be non-unique
// (pass false) — `status = ?` binds the whole index key but selects a large bucket,
// and costing that as a point probe would let join ordering drive off a big bucket as
// if it were one row (codex review). Without PlanContext we cannot prove a secondary
// index unique, so we conservatively fall through to the selectivity estimate; the
// metadata-aware wrapper HintCost still recognises unique indexes for the memo cost.
func scanLikeCost(comps []*predicates.ComparisonRange, recordTypes []string, stats properties.StatisticsProvider, fullBindUnique bool) properties.Cost {
	numBound := 0
	allEquality := true
	sel := 1.0
	for _, cr := range comps {
		if cr == nil || cr.IsEmpty() {
			continue
		}
		numBound++
		if cr.IsEquality() {
			sel *= properties.FilterSelectivity
		} else {
			allEquality = false
			sel *= properties.RangeSelectivity
		}
	}
	if fullBindUnique && numBound > 0 && allEquality && numBound == len(comps) {
		return properties.Cost{Cardinality: 1, CPU: properties.ScanCPU}
	}
	total := 0.0
	if len(recordTypes) == 0 {
		total = stats.RecordTypeCardinality("")
	} else {
		for _, t := range recordTypes {
			total += stats.RecordTypeCardinality(t)
		}
	}
	card := total * sel * physicalWrapperCostMultiplier
	return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU}
}

// planContainsJoin reports whether the concrete plan tree contains a join
// operator (FlatMap or NestedLoopJoin) anywhere — including under a Project /
// InMemorySort / Fetch, which is how the top-of-query join-order alternatives
// appear in a single Reference. Used to gate the join-ordering cost criterion.
func planContainsJoin(p plans.RecordQueryPlan) bool {
	found := false
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		switch n.(type) {
		case *plans.RecordQueryFlatMapPlan, *plans.RecordQueryNestedLoopJoinPlan:
			found = true
			return false
		}
		return true
	})
	return found
}

// planNodeIsStub reports whether a plan node is an UNRESOLVED push-through template:
// a unary operator (Fetch, PredicatesFilter, Distinct, Map, TypeFilter, …) presented
// with a nil inner. The data-access path builds chains of these shells
// (Fetch(<nil>) → PredicatesFilter(<nil>) → … → scan); the real inner lives in the
// expression's quantifier graph and is filled in at extraction via WithChildren. Every
// such template implements GetInner() RecordQueryPlan; a genuine data-access LEAF
// (Scan, IndexPlan, AggregateIndexPlan, VectorIndexPlan, TextIndexPlan, …) does not, so
// the interface assertion precisely distinguishes a stub from a leaf.
func planNodeIsStub(p plans.RecordQueryPlan) bool {
	if g, ok := p.(interface{ GetInner() plans.RecordQueryPlan }); ok {
		return g.GetInner() == nil
	}
	return false
}

// planTreeHasStub reports whether a concrete plan tree contains any unresolved
// push-through template stub (planNodeIsStub). Such a stub's GetChildren() is empty, so
// the buried data access under it is invisible to the concrete walks (concretePlanCounts
// / concretePlanCost) — they cost/count it as a ~free leaf. When this returns true the
// cost model must resolve the stub's real inner through the expression's quantifier graph
// instead (exprConcreteCost / exprConcreteCounts, RFC-076 step 3b).
func planTreeHasStub(p plans.RecordQueryPlan) bool {
	found := false
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		if planNodeIsStub(n) {
			found = true
			return false
		}
		return true
	})
	return found
}

func firstPhysicalChild(ref *expressions.Reference) expressions.RelationalExpression {
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	return nil
}

// exprConcreteCost costs a physical expression's concrete plan tree, resolving any
// nil-inner Fetch template to its REAL inner via the expression's quantifier graph
// (RFC-076 step 3b). A template-free (fully-formed) plan is costed by the phantom-free
// concretePlanCost directly; only a template child is ref-resolved, so the join
// structure stays phantom-free (the RFC-069 invariant) while the buried data access is
// no longer costed as the free empty shell its plan tree shows. Without this, a join
// whose inner is a push-through Fetch template (`FlatMap(outer, Fetch(<nil>))`) costs
// its inner as ~free, so a full-scan-driven join order wins over a far cheaper
// selective-outer order (TestFDB_JoinSelPred_Repro once the ordering-constraint pass
// (3a) makes the ordered template variant reachable).
func exprConcreteCost(e expressions.RelationalExpression, stats properties.StatisticsProvider, ctx PlanContext) properties.Cost {
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	return exprConcreteCostRec(e, stats, ctx, map[*expressions.Reference]bool{})
}

func exprConcreteCostRec(e expressions.RelationalExpression, stats properties.StatisticsProvider, ctx PlanContext, visited map[*expressions.Reference]bool) properties.Cost {
	ph, ok := e.(physicalPlanExpression)
	if !ok {
		return properties.Cost{}
	}
	plan := ph.GetRecordQueryPlan()
	if plan == nil {
		return properties.Cost{}
	}
	// Fast path: no template below this node — exact, phantom-free concrete cost.
	if !planTreeHasStub(plan) {
		return concretePlanCost(plan, stats, ctx)
	}
	// A template (nil-inner Fetch) is somewhere below. Cost children, preferring the
	// concrete embedded child plan and ref-resolving ONLY the template children — so
	// the join structure is costed from its concrete tree (phantom-free) and only the
	// unresolved shell is descended into via the quantifier Reference.
	quants := e.GetQuantifiers()
	planKids := plan.GetChildren()
	childCosts := make([]properties.Cost, 0, len(quants))
	for i, q := range quants {
		// Concrete embedded child that is itself template-free → cost it directly.
		if i < len(planKids) && planKids[i] != nil && !planTreeHasStub(planKids[i]) {
			childCosts = append(childCosts, concretePlanCost(planKids[i], stats, ctx))
			continue
		}
		// Template child (or a nil-inner Fetch node, whose plan has 0 children but whose
		// quantifier holds the real inner Reference) → resolve through the Reference.
		if ref := q.GetRangesOver(); ref != nil && !visited[ref] {
			visited[ref] = true
			if child := bestPhysicalChild(ref, stats); child != nil {
				childCosts = append(childCosts, exprConcreteCostRec(child, stats, ctx, visited))
				continue
			}
		}
		// Last resort (ref unresolvable / already visited via a cycle break): cost the
		// concrete child as-is. For a genuine nil-inner Fetch (0 plan children, 1 quantifier)
		// this branch is only reached on a memo cycle — unreachable in a valid DAG, where a
		// nil-inner Fetch's inner ref resolves down to a physical scan that never points back
		// up — and contributes the (free) shell cost for that one child rather than looping.
		if i < len(planKids) {
			childCosts = append(childCosts, concretePlanCost(planKids[i], stats, ctx))
		}
	}
	return combineConcreteCost(plan, childCosts, stats, ctx)
}

// ===== Concrete-plan property walk (Java PlanningCostModel alignment, RFC-069) =====
//
// Java's PlanningCostModel evaluates every cost property (FindExpressionVisitor
// operator counts, CardinalitiesProperty, UnmatchedFieldsCountProperty) over the
// CONCRETE candidate plan tree. The functions below port that: they walk a
// RecordQueryPlan's GetChildren() recursively with a type switch, instead of
// descending the logical memo References (whose shared multi-member groups let the
// best/first-member descent land on a "phantom" child the extracted plan never
// runs). The wrapper carries the fully-formed plan, so this exactly mirrors Java
// comparing two concrete candidates.

// concretePlanCounts computes operator counts, provable max-cardinality and the
// unmatched-index-field count for a concrete plan tree. ctx resolves index column
// names / uniqueness (by index name) and primary-key column counts; pass nil to
// resolve conservatively (indexes treated as non-unique, PK size 0).
func concretePlanCounts(p plans.RecordQueryPlan, ctx PlanContext) expressionCounts {
	counts := expressionCounts{maxDataAccessCardinality: -1}
	walkConcretePlan(p, &counts, ctx)
	// Java's max-of-max-cardinalities is unknown if ANY data access is unbounded.
	if counts.unboundedDataAccess {
		counts.maxDataAccessCardinality = -1
	}
	return counts
}

// exprConcreteCounts computes the cost-model operator counts / provable max-cardinality
// for a physical expression, resolving any nil-inner Fetch template through the
// expression's quantifier graph (RFC-076 step 3b). A template-free plan is counted by
// the phantom-free concretePlanCounts directly; only a template child is ref-resolved,
// so criterion #2 (max-cardinality) and the data-access counts SEE the real buried index
// scan instead of the free empty Fetch shell, while every concrete subtree is still
// counted from its plan tree (phantom-free, the RFC-069 invariant).
func exprConcreteCounts(e expressions.RelationalExpression, stats properties.StatisticsProvider, ctx PlanContext) expressionCounts {
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	counts := expressionCounts{maxDataAccessCardinality: -1}
	exprConcreteCountsRec(e, &counts, stats, ctx, map[*expressions.Reference]bool{})
	// Java's max-of-max-cardinalities is unknown if ANY data access is unbounded.
	if counts.unboundedDataAccess {
		counts.maxDataAccessCardinality = -1
	}
	return counts
}

func exprConcreteCountsRec(e expressions.RelationalExpression, counts *expressionCounts, stats properties.StatisticsProvider, ctx PlanContext, visited map[*expressions.Reference]bool) {
	ph, ok := e.(physicalPlanExpression)
	if !ok {
		return
	}
	plan := ph.GetRecordQueryPlan()
	if plan == nil {
		return
	}
	// Fast path: template-free subtree — merge its exact concrete counts.
	if !planTreeHasStub(plan) {
		mergeCounts(counts, concretePlanCounts(plan, ctx))
		return
	}
	// Count THIS node only, then resolve children through the expression graph: a
	// template-free concrete child contributes its exact concrete counts; a template
	// child (or a nil-inner Fetch node) is descended via its quantifier Reference.
	countConcreteNode(plan, counts, ctx)
	quants := e.GetQuantifiers()
	planKids := plan.GetChildren()
	for i, q := range quants {
		if i < len(planKids) && planKids[i] != nil && !planTreeHasStub(planKids[i]) {
			mergeCounts(counts, concretePlanCounts(planKids[i], ctx))
			continue
		}
		if ref := q.GetRangesOver(); ref != nil && !visited[ref] {
			visited[ref] = true
			if child := bestPhysicalChild(ref, stats); child != nil {
				exprConcreteCountsRec(child, counts, stats, ctx, visited)
				continue
			}
		}
		if i < len(planKids) {
			mergeCounts(counts, concretePlanCounts(planKids[i], ctx))
		}
	}
}

// mergeCounts adds src's operator counts into dst, taking the max of provable
// max-cardinalities (-1 = unknown) and OR-ing the unbounded-access flag. The final
// "unbounded ⇒ unknown" reset is applied once by exprConcreteCounts at the top.
func mergeCounts(dst *expressionCounts, src expressionCounts) {
	dst.scanCount += src.scanCount
	dst.indexScanCount += src.indexScanCount
	dst.coveringIndexCount += src.coveringIndexCount
	dst.fetchCount += src.fetchCount
	dst.typeFilterCount += src.typeFilterCount
	dst.inJoinCount += src.inJoinCount
	dst.inUnionCount += src.inUnionCount
	dst.flatMapCount += src.flatMapCount
	dst.nestedLoopJoinCount += src.nestedLoopJoinCount
	dst.mapCount += src.mapCount
	dst.predicatesFilterCount += src.predicatesFilterCount
	dst.unmatchedFieldCount += src.unmatchedFieldCount
	dst.inMemorySortCount += src.inMemorySortCount
	dst.nljPredicateCount += src.nljPredicateCount
	if src.maxDataAccessCardinality > dst.maxDataAccessCardinality {
		dst.maxDataAccessCardinality = src.maxDataAccessCardinality
	}
	if src.unboundedDataAccess {
		dst.unboundedDataAccess = true
	}
}

func walkConcretePlan(p plans.RecordQueryPlan, counts *expressionCounts, ctx PlanContext) {
	if p == nil {
		return
	}
	if countConcreteNode(p, counts, ctx) {
		return // PK point-probe already accounted for its scan; do not recurse.
	}
	for _, c := range p.GetChildren() {
		walkConcretePlan(c, counts, ctx)
	}
}

// countConcreteNode adds plan node p's OWN operator contribution to counts (no
// recursion) and returns skipChildren=true when the caller must NOT descend into p's
// children — the full-PK point-probe case, which already accounts for its scan and
// would otherwise be re-counted as unbounded. Split out of walkConcretePlan so the
// template-aware exprConcreteCounts (RFC-076 step 3b) can count a node while resolving
// its template children through the expression graph instead of the plan's children.
func countConcreteNode(p plans.RecordQueryPlan, counts *expressionCounts, ctx PlanContext) (skipChildren bool) {
	switch pl := p.(type) {
	case *plans.RecordQueryScanPlan:
		counts.scanCount++
		if card, known := scanPlanProvableMaxCard(pl, ctx); known {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
		}
	case *plans.RecordQueryIndexPlan:
		cols, unique := indexMetadata(pl, ctx)
		if pl.IsCovering() {
			counts.coveringIndexCount++
		} else {
			counts.indexScanCount++
		}
		if card, known := indexPlanProvableMaxCard(pl, cols, unique); known {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
		}
		counts.unmatchedFieldCount += unmatchedFieldsForIndex(pl, cols)
	case *plans.RecordQueryAggregateIndexPlan:
		counts.coveringIndexCount++
		// Aggregate access groups rows — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		// A multi-aggregate intersection's children are aggregate-index scans
		// baked into this plan (the template-aware exprConcreteCounts resolves
		// children via the empty expression graph, not plan children, so without
		// this case they go uncounted and the node ranks as a no-data-access
		// node). Count the intersection as ONE logical grouped data access — read
		// the pre-aggregated groups, comparable to a single aggregate-index scan,
		// NOT N independent accesses. Counting it as N made criterion #3 (fewer
		// data accesses) prefer a single full Scan (count 1) over the intersection
		// (count N) even though the scan reads the whole table + sorts; counting
		// it as 1 ties the scan on count, then it wins on the scan-vs-covering-
		// index criterion exactly like the single-aggregate path. Skip the child
		// walk so the per-child scans aren't also counted.
		counts.coveringIndexCount++
		counts.unboundedDataAccess = true
		return true
	case *plans.RecordQueryVectorIndexPlan:
		counts.indexScanCount++
		// Top-K vector scan — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	case *plans.RecordQueryTextIndexPlan:
		counts.indexScanCount++
		counts.unboundedDataAccess = true
	case *plans.RecordQueryTypeFilterPlan:
		counts.typeFilterCount += len(pl.GetRecordTypes())
	case *plans.RecordQueryPredicatesFilterPlan:
		counts.predicatesFilterCount++
		// A PredicatesFilter whose equality conjuncts cover the FULL primary key of
		// the inner full Scan is a point probe — it accesses at most one record, so
		// its data access is provably bounded at 1 (Java's CardinalitiesProperty
		// bounds the equivalent SARG'd scan; Go represents an IN-join / PK-equality
		// probe as a residual filter over a full scan rather than a SARG'd scan, so
		// without this the access reads as unbounded). Count the scan but DO NOT let
		// it mark the access unbounded: bound it here and skip the child walk's scan
		// arm. Missing this made criterion #2 (provable max cardinality) tie an
		// IN-join PK probe with a semantically-broken Scan∩aggregate-index
		// intersection, and criterion #3 (fewer residuals) then picked the broken
		// intersection — a DELETE/SELECT WHERE pk IN (...) returning 0 rows.
		if scan, ok := pl.GetInner().(*plans.RecordQueryScanPlan); ok &&
			predicatesFilterIsFullPKPointProbe(pl, scan, ctx) {
			counts.scanCount++
			if 1 > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = 1
			}
			return true // already accounted for the scan; do not recurse (would mark unbounded)
		}
	case *plans.RecordQueryFilterPlan:
		// Legacy filter — not counted as a predicates filter (matches the wrapper walk).
	case *plans.RecordQueryMapPlan:
		// Map only — NOT RecordQueryProjectionPlan. The map-count criterion (#14)
		// is a structural tiebreak; a near-ubiquitous top-of-query projection is
		// not a discriminating operator, and counting it makes #14 fire on almost
		// every plan pair. (concretePlanCost charges a projection via mapCost for
		// magnitude, a different purpose — the two walks need not count the same
		// nodes.) Counting projections here re-ranks ties broadly and selected a
		// latent-buggy CTE plan that mis-projects an aliased column to NULL —
		// caught by TestFDB_{CTEChainedColumnAliases,CascadesCTEColumnAliases}.
		counts.mapCount++
	case *plans.RecordQueryInJoinPlan:
		counts.inJoinCount++
	case *plans.RecordQueryInUnionPlan:
		counts.inUnionCount++
	case *plans.RecordQueryFlatMapPlan:
		counts.flatMapCount++
	case *plans.RecordQueryNestedLoopJoinPlan:
		counts.nestedLoopJoinCount++
		counts.nljPredicateCount += len(pl.GetPredicates())
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		counts.fetchCount++
	case *plans.RecordQueryInMemorySortPlan:
		counts.inMemorySortCount++
	}
	return false
}

// predicatesFilterIsFullPKPointProbe reports whether a PredicatesFilter over a full
// Scan is a single-record point probe: every primary-key column of the scanned record
// type has an equality conjunct in the filter (the scan itself carries no SARG bounds).
// This is the residual-filter representation Go uses for an IN-join PK probe / a
// `pk = <value>` lookup; Java SARGs the same access into the scan, where
// CardinalitiesProperty bounds it at 1. Returns false when the PK is unresolvable (no
// ctx) or any PK column lacks an equality conjunct — i.e. it never OVER-bounds.
func predicatesFilterIsFullPKPointProbe(pl *plans.RecordQueryPredicatesFilterPlan, scan *plans.RecordQueryScanPlan, ctx PlanContext) bool {
	if ctx == nil {
		return false
	}
	// The underlying scan must be an unbounded full scan — if it already SARGs the
	// PK, scanPlanProvableMaxCard handles the bound and this path is irrelevant.
	for _, cr := range scan.GetScanComparisons() {
		if cr != nil && !cr.IsEmpty() {
			return false
		}
	}
	recTypes := scan.GetRecordTypes()
	if len(recTypes) != 1 {
		return false
	}
	pkCols := ctx.GetPrimaryKeyColumns(recTypes[0])
	if len(pkCols) == 0 {
		return false
	}
	// Collect the field names the filter constrains by equality.
	eqFields := make(map[string]struct{})
	for _, pred := range pl.GetPredicates() {
		cp, ok := pred.(*predicates.ComparisonPredicate)
		if !ok || cp.Comparison.Type != predicates.ComparisonEquals {
			continue
		}
		fv, ok := cp.Operand.(*values.FieldValue)
		if !ok {
			continue
		}
		_, col := fieldValueAliasAndCol(fv)
		eqFields[strings.ToUpper(col)] = struct{}{}
	}
	for _, pk := range pkCols {
		if _, ok := eqFields[strings.ToUpper(pk)]; !ok {
			return false
		}
	}
	return true
}

// indexMetadata resolves an index plan's key-column names and uniqueness from the
// PlanContext's match candidates (matched by index name). Returns (nil, false) when
// ctx is nil or the candidate is not found — the conservative default (non-unique).
func indexMetadata(pl *plans.RecordQueryIndexPlan, ctx PlanContext) ([]string, bool) {
	if ctx == nil {
		return nil, false
	}
	name := pl.GetIndexName()
	for _, cand := range ctx.GetMatchCandidates() {
		if cand.CandidateName() == name {
			return cand.GetColumnNames(), cand.IsUnique()
		}
	}
	return nil, false
}

// scanPlanProvableMaxCard returns a primary scan's PROVABLE max cardinality (1) and
// whether it is known. Java's CardinalitiesProperty bounds a scan at 1 ONLY when every
// primary-key column is equality-bound (a point lookup); a range, partial bind, or full
// scan is unknown. When the PK column count is resolvable via ctx, require the bound
// columns to cover the FULL primary key (a partial equality prefix of a composite PK is
// still a range, hence unbounded).
func scanPlanProvableMaxCard(pl *plans.RecordQueryScanPlan, ctx PlanContext) (float64, bool) {
	comps := pl.GetScanComparisons()
	if len(comps) == 0 {
		return 0, false
	}
	numBound := 0
	allEquality := true
	for _, cr := range comps {
		if !cr.IsEmpty() {
			numBound++
			if !cr.IsEquality() {
				allEquality = false
			}
		}
	}
	if numBound == 0 || !allEquality || numBound != len(comps) {
		return 0, false
	}
	if ctx != nil && len(pl.GetRecordTypes()) > 0 {
		if pkLen := len(ctx.GetPrimaryKeyColumns(pl.GetRecordTypes()[0])); pkLen > 0 && numBound < pkLen {
			return 0, false
		}
	}
	return 1, true
}

// indexPlanProvableMaxCard returns an index scan's PROVABLE max cardinality (1) and
// whether it is known: 1 ONLY when the index is UNIQUE and every index key column is
// equality-bound; otherwise unknown.
func indexPlanProvableMaxCard(pl *plans.RecordQueryIndexPlan, cols []string, unique bool) (float64, bool) {
	if !unique || len(cols) == 0 {
		return 0, false
	}
	numBound := 0
	allEquality := true
	for _, cr := range pl.GetScanComparisons() {
		if !cr.IsEmpty() {
			numBound++
			if !cr.IsEquality() {
				allEquality = false
			}
		}
	}
	if numBound > 0 && allEquality && numBound == len(cols) {
		return 1, true
	}
	return 0, false
}

// unmatchedFieldsForIndex computes the UnmatchedFieldsCount contribution of an index
// plan: columnSize - numComparisons, where columnSize is the index's KEY column count and
// numComparisons = equality-bound count + 1 for a trailing inequality.
//
// Rationale (do NOT misattribute to Java): Java's UnmatchedFieldsCountProperty uses
// matchCandidate.getSargableAliases().size(), which for a value index INCLUDES the trimmed
// primary-key suffix — Java's columnSize is index-key + PK. Counting key-columns-only is
// nonetheless correct in GO because Go's match candidate (plan_context_builder.go) never
// folds the PK into its sargable aliases — it stores pkColumnNames separately, so the Go
// candidate's sargable surface IS the key columns. Adding the PK suffix here would
// over-count and penalize a fully-bound index probe vs a full scan, mis-ranking criterion
// #12 toward a full-scan join driver (the RFC-069 multiway regression). The real divergence
// is that Go's candidate omits the PK suffix; we match Go's candidate model, not paper a PK
// term over it. The clamp keeps it non-negative if a scan binds a PK-suffix range beyond its
// key columns (Java's invariant columnSize >= numComparisons).
func unmatchedFieldsForIndex(pl *plans.RecordQueryIndexPlan, cols []string) int {
	equalitySize := 0
	hasInequality := false
	for _, cr := range pl.GetScanComparisons() {
		if cr.IsEmpty() {
			continue
		}
		if cr.IsEquality() {
			equalitySize++
		} else {
			hasInequality = true
		}
	}
	numComparisons := equalitySize
	if hasInequality {
		numComparisons++
	}
	// columnSize is the index's KEY column count ONLY (not + primary key). Adding the
	// trailing PK overcounts and penalizes a fully-bound index probe (it shows
	// "unmatched" PK columns it never needs), so criterion #12 then prefers a
	// full-scan join driver over a correlated index probe — the RFC-069 multiway
	// regression. Matches the prior wrapper behaviour (len(columnNames) - boundCols).
	// The clamp keeps it non-negative when an index scan binds a PK-suffix range
	// beyond its key columns (Java's invariant: columnSize >= numComparisons).
	columnSize := len(cols)
	if columnSize < numComparisons {
		columnSize = numComparisons
	}
	return columnSize - numComparisons
}

// concreteResidualPredicates sums the CNF size of every residual predicate
// (PredicatesFilter + legacy Filter) in a concrete plan tree (criterion #3).
func concreteResidualPredicates(p plans.RecordQueryPlan) int {
	total := 0
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		switch pf := n.(type) {
		case *plans.RecordQueryPredicatesFilterPlan:
			for _, pr := range pf.GetPredicates() {
				total += int(cnfSize(pr))
			}
		case *plans.RecordQueryFilterPlan:
			for _, pr := range pf.GetPredicates() {
				total += int(cnfSize(pr))
			}
		case *plans.RecordQueryNestedLoopJoinPlan:
			// A materialized NLJ evaluates its join predicate per (outer,inner)
			// pair — it is NOT satisfied by a SARG, so it is a residual conjunct,
			// exactly like a PredicatesFilter. Counting it is essential for the
			// residual criterion (#3) to prefer a correlated FlatMap (which SARGs
			// the join key into an index/PK probe, leaving fewer residuals) over a
			// materialized NLJ that re-evaluates the same predicate per pair — Go
			// has no Java counterpart for a join-predicate-bearing NLJ, so this
			// keeps #3 from spuriously preferring the materialized join (RFC-069).
			for _, pr := range pf.GetPredicates() {
				total += int(cnfSize(pr))
			}
		}
		return true
	})
	return total
}

// planMatchKind selects which operator a depth query targets.
type planMatchKind int

const (
	matchTypeFilter planMatchKind = iota
	matchFetch
	matchDistinct
)

func concretePlanMatches(p plans.RecordQueryPlan, kind planMatchKind) bool {
	switch kind {
	case matchTypeFilter:
		_, ok := p.(*plans.RecordQueryTypeFilterPlan)
		return ok
	case matchFetch:
		if _, ok := p.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
			return true
		}
		// Mirror the wrapper-walk isFetchExpression, which also treats an index
		// scan as a fetch source (a non-covering index scan fetches base records).
		_, ok := p.(*plans.RecordQueryIndexPlan)
		return ok
	case matchDistinct:
		_, ok := p.(*plans.RecordQueryDistinctPlan)
		return ok
	}
	return false
}

// concretePlanDepth returns the minimum depth (root = 0) at which a node matching
// kind appears in the concrete plan tree, or -1 if none. Mirrors Java's
// ExpressionDepthProperty over the concrete plan.
func concretePlanDepth(p plans.RecordQueryPlan, kind planMatchKind) int {
	if p == nil {
		return -1
	}
	if concretePlanMatches(p, kind) {
		return 0
	}
	best := -1
	for _, c := range p.GetChildren() {
		d := concretePlanDepth(c, kind)
		if d >= 0 && (best < 0 || d+1 < best) {
			best = d + 1
		}
	}
	return best
}

// concretePlanHash hashes a concrete plan tree deterministically (criterion's final
// tiebreak), matching deepHashCode's mixing but over the concrete children.
func concretePlanHash(p plans.RecordQueryPlan) uint64 {
	if p == nil {
		return 0
	}
	h := p.HashCodeWithoutChildren()
	for _, c := range p.GetChildren() {
		h ^= concretePlanHash(c)*0x517cc1b727220a95 + 0x6c62272e07bb0142
	}
	return h
}

// costExprDepth returns the depth of a target operator, walking the concrete plan
// tree for a physical expression and the logical memo otherwise.
func costExprDepth(e expressions.RelationalExpression, kind planMatchKind) int {
	if ph, ok := e.(physicalPlanExpression); ok {
		if plan := ph.GetRecordQueryPlan(); plan != nil {
			return concretePlanDepth(plan, kind)
		}
	}
	switch kind {
	case matchTypeFilter:
		return expressionDepth(e, isTypeFilterExpression)
	case matchFetch:
		return expressionDepth(e, isFetchExpression)
	case matchDistinct:
		return expressionDepth(e, isDistinctExpression)
	}
	return -1
}

// costExprHash returns the deterministic tiebreak hash, over the concrete plan for a
// physical expression and the logical memo otherwise.
func costExprHash(e expressions.RelationalExpression) uint64 {
	if ph, ok := e.(physicalPlanExpression); ok {
		if plan := ph.GetRecordQueryPlan(); plan != nil {
			return concretePlanHash(plan)
		}
	}
	return deepHashCode(e)
}
