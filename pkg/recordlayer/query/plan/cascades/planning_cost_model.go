package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
	cmp := planningCostModelCompareWith(a, b, nil)
	return cmp < 0
}

// NewPlanningCostModelLess returns a stats-aware cost model comparator.
// The returned function uses real record counts (via stats) for
// cardinality estimation at scan/index wrappers and the scalar cost
// fallback (criterion #16). Pass nil for default (LeafScanCardinality).
func NewPlanningCostModelLess(stats properties.StatisticsProvider) func(a, b expressions.RelationalExpression) bool {
	return func(a, b expressions.RelationalExpression) bool {
		return planningCostModelCompareWith(a, b, stats) < 0
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

func planningCostModelCompareWith(a, b expressions.RelationalExpression, stats properties.StatisticsProvider) int {
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

	opsA := findExpressionsByType(a, stats)
	opsB := findExpressionsByType(b, stats)

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

	typeFilterDepthA := expressionDepth(a, isTypeFilterExpression)
	typeFilterDepthB := expressionDepth(b, isTypeFilterExpression)
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

	if cmp := compareJoinOrdering(a, b, stats); cmp != 0 {
		return cmp
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
	costA := properties.EstimateCostWith(a, stats)
	costB := properties.EstimateCostWith(b, stats)
	if costA.Less(costB) {
		return -1
	}
	if costB.Less(costA) {
		return 1
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
	maxDataAccessCardinality float64 // -1 means unknown (no data access found)
}

func findExpressionsByType(e expressions.RelationalExpression, stats properties.StatisticsProvider) expressionCounts {
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	counts := expressionCounts{maxDataAccessCardinality: -1}
	visited := make(map[*expressions.Reference]bool)
	walkExpressionTree(e, &counts, stats, visited)
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
		card := w.HintCost(nil, stats).Cardinality
		if card > counts.maxDataAccessCardinality {
			counts.maxDataAccessCardinality = card
		}
	case *physicalAggregateIndexWrapper:
		counts.coveringIndexCount++
		card := w.HintCost(nil, stats).Cardinality
		if card > counts.maxDataAccessCardinality {
			counts.maxDataAccessCardinality = card
		}
	case *physicalVectorIndexScanWrapper:
		counts.indexScanCount++
		card := w.HintCost(nil, stats).Cardinality
		if card > counts.maxDataAccessCardinality {
			counts.maxDataAccessCardinality = card
		}
	case *physicalIndexScanWrapper:
		if w.covering {
			counts.coveringIndexCount++
		} else {
			counts.indexScanCount++
		}
		cost := w.HintCost(nil, stats)
		card := cost.Cardinality
		if card > counts.maxDataAccessCardinality {
			counts.maxDataAccessCardinality = card
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

// compareFlatMapJoinOrdering implements Java's FlatMapJoinOrderingProperty
// criterion: when both plans are FlatMap, prefer the one with smaller outer
// cardinality (fewer inner loop executions).
// compareJoinOrdering ranks two join plans (FlatMap or NestedLoopJoin)
// by their RECURSIVE best-member TOTAL cost — Cascades' "combined cost with
// inputs" (§3.1): the whole join subtree, each sub-product costed at its WINNER
// (BestMemberCostWith). This replaces the prior top-outer-cardinality-only
// heuristic, which judged a multi-way plan by its top driver alone and let a
// plan with a great top driver but a pessimal inner join win — so the chosen
// order tracked FROM-clause position, not cost (RFC-041).
//
// Confined to join-rooted pairs (FlatMap/NLJ both model the join with an
// outer×inner cardinality term in HintCost): a join's total cost is the
// principled thing to compare here, and restricting to join roots keeps
// non-join winner selection on EstimateCostWith's established first-member
// behaviour (changing the global recursion flips unrelated cost ties — e.g. it
// made the unordered-union plan vanish in stage advancement). The best-member
// walk is memoised per call, so PR-A-merged shared sub-products are costed once.
func compareJoinOrdering(a, b expressions.RelationalExpression, stats properties.StatisticsProvider) int {
	if !isJoinWrapper(a) || !isJoinWrapper(b) {
		return 0
	}
	costA := properties.BestMemberCostWith(a, stats)
	costB := properties.BestMemberCostWith(b, stats)
	if costA.Less(costB) {
		return -1
	}
	if costB.Less(costA) {
		return 1
	}
	return 0
}

// isJoinWrapper reports whether e is a physical join operator whose cost
// reflects an outer×inner cardinality flow (the operators whose ORDER the
// cost model must choose among).
func isJoinWrapper(e expressions.RelationalExpression) bool {
	switch e.(type) {
	case *physicalFlatMapWrapper, *physicalNestedLoopJoinWrapper:
		return true
	default:
		return false
	}
}

func firstPhysicalChild(ref *expressions.Reference) expressions.RelationalExpression {
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	return nil
}
