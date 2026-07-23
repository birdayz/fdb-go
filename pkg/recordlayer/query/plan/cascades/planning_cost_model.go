package cascades

import (
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"os"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PlanningCostModelLess is the Java-aligned multi-criteria plan comparator,
// with the documented Go-specific criteria called out below.
//
// Returns true if a is strictly preferred over b. The comparison uses
// these ordered tie-breaking criteria. Parenthetical numbers are the stable
// Java-aligned labels used by focused tests and design docs; Go extensions do
// not renumber those labels:
//
//   - Physical plan beats non-physical (#1)
//   - Max cardinality of all data accesses, lower wins (#2)
//   - Fewer normalized residual predicates (#3)
//   - Fewer data access operators: scan + index + covering (#4)
//   - Recursive CTE tie-breaker: DFS beats level-based (#5)
//   - Join-order cost (#15, broadened and hoisted in Go)
//   - IN-plan penalty when IN-values are not SARGs (#6)
//   - Primary scan vs index-scan-with-fetch preference (#7)
//   - In-memory sort count, fewer wins (Go sort-elimination analogue)
//   - Type filter count, fewer wins (#8)
//   - Type filter depth, deeper wins (#9)
//   - Index fetch metrics, fewer non-covering scans/fetches win (#10)
//   - Distinct depth, deeper wins (#11)
//   - Unmatched primary-scan/index key field count, fewer wins (#12)
//   - IN-join source count, more wins (#13)
//   - MAP + PredicatesFilter count, fewer wins (#14)
//   - Nested-loop-join predicate count, more wins (Go extension)
//   - ON EMPTY NULL operation count, fewer wins (#16)
//   - Scalar cost fallback (Go statistics extension)
//   - Plan hash deterministic tie-break (#17)
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
// (unmatched primary-scan/index key fields) properties are computed faithfully from the
// CONCRETE plan tree (RFC-069). Pass nil stats for default
// (LeafScanCardinality); pass nil ctx to resolve index metadata
// conservatively (treat indexes as non-unique).
func NewPlanningCostModelLessWithContext(stats properties.StatisticsProvider, ctx PlanContext) func(a, b expressions.RelationalExpression) bool {
	return func(a, b expressions.RelationalExpression) bool {
		return planningCostModelCompareWith(a, b, stats, ctx) < 0
	}
}

// RewritingCostModelLess is the cost model for the REWRITING phase. It ports the
// tail of Java's RewritingCostModel.compare():
//  1. Fewer SelectExpressions
//  2. Fewer TableFunctionExpressions
//  3. Fewer normalized residual predicate conjuncts (CNF full-size)
//  4. More predicates at deeper levels (push predicates down)
//  5. Semantic hash tiebreak
//
// DELIBERATE OMISSION — Java's FIRST criterion, outerJoinCount (penalize any
// surviving OuterJoinExpression), is NOT ported, and must not be. In Java it is a
// CORRECTNESS GUARD, not a heuristic: Java's OuterJoinExpression is a logical-only
// node with NO physical operator and exactly one consumer (RewriteOuterJoinRule),
// so it MUST be rewritten before planning. Java's single-final-expression prune
// keeps one survivor per group; without outerJoinCount the un-rewritten
// OuterJoinExpression (0 selects) would beat the rewritten form (2 selects) on the
// selectCount tie-break, survive the prune, and leave the planning phase with an
// UNIMPLEMENTABLE node — the query would fail to plan. outerJoinCount forces the
// implementable rewritten form to survive.
//
// Go has no such correctness problem, because Go's outer join IS directly
// implementable: an outer-join SelectExpression is planned by
// ImplementNestedLoopJoinRule as a MATERIALIZED RecordQueryNestedLoopJoinPlan
// (RFC-152) — a read-side extension Java lacks (Java has only the correlated
// FlatMap re-scan). Go deliberately keeps the un-rewritten outer-join select as the
// REWRITING prune survivor (it wins on selectCount, 1<2) precisely so PLANNING can
// derive BOTH the materialized NLJ (scan the inner once) AND the correlated FlatMap
// (re-scan) and cost-choose. Porting outerJoinCount would force the rewritten form
// to win the prune, discard the outer-join select, and thereby SUPPRESS the
// materialized-NLJ alternative — a regression, not a fix (pinned by
// TestFDB_ArrayUnnestOrdinality, which asserts the materialized NLJ box, and by
// TestRewritingCostModel_KeepsUnrewrittenOuterJoin).
// RFC-186: every tier derives through DESIGNATED child finals (the virtual
// prune, designated_final.go) — a function of a deterministically-chosen
// candidate tree, never of memo-population history. The pre-RFC-186 tiers
// summed over ALL memo members (exploratory included), so two equivalent
// candidates scored differently by how many rewrites their child groups
// happened to accumulate — and tier 3's physical-only descent counted
// nothing at all on the all-logical REWRITING memo. This package-level
// function mints a fresh designation scope per call (identical semantics to
// the planner-owned scope, merely uncached); the planner's REWRITING cost
// model uses its own scope so OptimizeGroup winners and designations come
// from the SAME comparator (coherence, RFC-186 instrument).
func RewritingCostModelLess(a, b expressions.RelationalExpression) bool {
	return newDesignationScope().compare(a, b, nil) < 0
}

// comparePredicateCountByLevel ports Java's PredicateCountByLevelProperty.compare.
// Java iterates the FIRST map's SortedMap entries reading getOrDefault(level, 0)
// on the second, but Java's producer is DENSE — every level 0..highest has an
// entry (0 for a non-predicate node) — so iterating a's entries covers all
// levels. Go's producer (designationScope.predCountByLevel) is SPARSE, so the
// faithful and ANTISYMMETRIC form is a single ascending pass over the UNION of
// levels (0..max): absent==0==getOrDefault makes the per-level counts equal
// Java's, and the union stays antisymmetric on the sparse maps Go passes — a
// first-map-only pass would return the same sign in both orientations for e.g.
// {2:1} vs {1:1}, making the REWRITING survivor insertion-order dependent.
// The residual divergence from Java is the highest-level TIEBREAK: because Go's
// map is sparse, maxLevelA/maxLevelB are the highest PREDICATE levels, not
// Java's tree-depth getHighestLevel (dense). Only bites on a full per-level tie;
// closing it (dense producer) flips REWRITING survivors — booked as Finding
// 6-followup.
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
		if a[level] != b[level] { // absent == 0 == getOrDefault(level, 0)
			return intCompare(a[level], b[level])
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
	//
	// OUTER GUARD (Java PlanningCostModel: the whole-plan max-cardinality gate
	// around the data-access-cardinality criterion): only consult the
	// data-access maxima when the PROVEN WHOLE-PLAN max cardinality of at least
	// one side is known. When both whole-plan maxima are unknown but a data
	// access is provably bounded (e.g. an InUnion/Explode over point lookups,
	// where the bounded access sits under an unbounded multiplier), Java
	// abstains here rather than ranking on the data-access maximum — the
	// bounded access does not bound the plan's output. Go used to skip this
	// gate and rank anyway.
	if wholePlanMaxCardinalityKnown(a) || wholePlanMaxCardinalityKnown(b) {
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
	// Recursive-CTE DECOMPOSITION choice (DFS join vs level union) runs
	// BEFORE join-order costing: the two candidates are different
	// algorithms over different subtrees, not two orders of one join —
	// outside compareJoinOrdering's jurisdiction. Pre-RFC-186 §2D the
	// concrete walk priced both recursive roots transparently (equal) and
	// abstained here by accident; the HintCost dispatch made it
	// discriminate across the decompositions' non-comparable children and
	// steal the choice from this comparison (the RecursiveCTE plan-shape
	// pin caught the flip). Jurisdiction, not ordering luck, now encodes
	// the precedence.
	if cmp := compareRecursiveCTE(a, b); cmp != 0 {
		return cmp
	}

	if cmp := compareJoinOrdering(a, b, stats, ctx); cmp != 0 {
		return cmp
	}

	if cmp := compareInPlan(a, b, opsA, opsB); cmp != 0 {
		return cmp
	}

	if cmp := comparePrimaryScanVsIndexScan(a, b, opsA, opsB, indexScanPreferenceOf(ctx)); cmp != 0 {
		return cmp
	}

	// Reject a redundant in-memory sort HERE — before every sort-blind
	// structural rung below (typeFilterCount/Depth, fetch metrics,
	// distinctDepth, unmatchedFieldCount, inJoinCount, mapFilter). Go's
	// RecordQueryInMemorySortPlan is a read-side extension Java has no cost
	// rung for at all: Java eliminates a redundant sort STRUCTURALLY
	// (RemoveSortRule), before its PlanningCostModel ever runs, so a
	// "Sort(X)" candidate never competes against a sort-free "X" sibling in
	// Java's memo in the first place. Promoting this rung to fire as soon as
	// cardinality/residuals/data-access-count/join-ordering/IN-plan/
	// primary-vs-index all TIE is the cost-time analog of that structural
	// elimination: once two candidates have done provably the same amount of
	// "real" work, the one carrying an extra full materializing sort is
	// strictly worse, full stop — no structural rung below should ever get a
	// chance to prefer the sorted candidate on some unrelated axis. This is a
	// pure lexicographic REORDER (the rung's own comparison is unchanged,
	// only its position moves earlier), so it cannot introduce a
	// transitivity violation: an ordinal comparator built by lexicographic
	// composition of total-preorder criteria is transitive regardless of
	// criterion order.
	if opsA.inMemorySortCount != opsB.inMemorySortCount {
		return intCompare(opsA.inMemorySortCount, opsB.inMemorySortCount)
	}

	if opsA.typeFilterCount != opsB.typeFilterCount {
		return intCompare(opsA.typeFilterCount, opsB.typeFilterCount)
	}

	// Structural depth tiebreaks (typeFilterDepth/fetchDepth/distinctDepth) are
	// SORT-INVARIANT (RFC-190 190.2): concretePlanDepth treats an in-memory sort
	// as transparent, so a redundant InMemorySort no longer inflates a
	// descendant's measured depth relative to a sort-elided sibling — the two
	// plans' depths are measured from the SAME effective root regardless of
	// which one happens to carry a sort. That, plus the promoted
	// inMemorySortCount rung above (which now guarantees every rung from here
	// down only ever compares candidates with an EQUAL sort count), was the
	// fix for the non-transitive 3-cycle a per-rung sort gate used to paper
	// over (a gated rung decided a sort-free pair outright but was skipped
	// whenever one side carried a sort, handing the decision to a later
	// ungated rung — two different rungs governing "the same slot" for
	// different pairs of the same plans). These three depth rungs, the
	// fetch/unmatchedFieldCount rungs, and the map/filter node-count rung
	// below are therefore all UNGATED.
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
		// Depth rung — sort-invariant, ungated (see the type-filter-depth
		// comment above); the fetch-COUNT rungs are also ungated.
		fetchDepthA := costExprDepth(a, matchFetch)
		fetchDepthB := costExprDepth(b, matchFetch)
		if fetchDepthA >= 0 && fetchDepthB >= 0 && fetchDepthA != fetchDepthB {
			return intCompare(fetchDepthA, fetchDepthB)
		}
		if opsA.fetchCount != opsB.fetchCount {
			return intCompare(opsA.fetchCount, opsB.fetchCount)
		}
	}

	// Depth rung — sort-invariant, ungated (see the type-filter-depth comment
	// above).
	distinctDepthA := costExprDepth(a, matchDistinct)
	distinctDepthB := costExprDepth(b, matchDistinct)
	if distinctDepthA >= 0 && distinctDepthB >= 0 && distinctDepthA != distinctDepthB {
		return intCompare(distinctDepthB, distinctDepthA)
	}

	// A sort adds no unmatched index fields, so this count is already
	// sort-invariant across an elided/sorted pair — ungated.
	if opsA.unmatchedFieldCount != opsB.unmatchedFieldCount {
		return intCompare(opsA.unmatchedFieldCount, opsB.unmatchedFieldCount)
	}

	if opsA.inJoinCount != opsB.inJoinCount {
		return intCompare(opsB.inJoinCount, opsA.inJoinCount)
	}

	// Structural node-count tiebreak (RFC-190 190.2: ungated, matching Java's
	// unconditional countSimpleOps). Any concern about a sort-elided sibling
	// carrying a pushdown-SPLIT residual (one filter node → two) winning
	// against a sorted-but-unsplit plan is moot here: by this point
	// opsA.inMemorySortCount == opsB.inMemorySortCount is GUARANTEED (the
	// promoted inMemorySortCount rung above already returned if they
	// differed), so this rung only ever compares two candidates with the
	// SAME sort count — the split-vs-sort interaction the old gate worried
	// about cannot arise here anymore.
	mapFilterA := opsA.mapCount + opsA.predicatesFilterCount
	mapFilterB := opsB.mapCount + opsB.predicatesFilterCount
	if mapFilterA != mapFilterB {
		return intCompare(mapFilterA, mapFilterB)
	}

	if opsA.nljPredicateCount != opsB.nljPredicateCount {
		return intCompare(opsB.nljPredicateCount, opsA.nljPredicateCount)
	}

	// Fall back to the scalar cost model when all multi-criteria tie.
	// This avoids the hash tiebreak picking semantically broken plans
	// (see D-4 wiring investigation). The scalar model's per-operator
	// cost formulas discriminate between plans that look identical to
	// the ordinal criteria.
	//
	// The redundant-in-memory-sort rejection that used to sit HERE (just
	// before this scalar fallback) is now promoted — see the
	// inMemorySortCount rung right after comparePrimaryScanVsIndexScan,
	// above — so it runs before the sort-blind structural rungs instead of
	// after them (RFC-190 190.2).

	// Fewer ON EMPTY NULL operations wins (Java PlanningCostModel: the last
	// ordinal rung before the planHash tiebreak). This is a structural count
	// criterion, so it sits with the other ordinal rungs — before Go's
	// statistics scalar-cost extension and the final hash tiebreak.
	if opsA.numDefaultOnEmpty != opsB.numDefaultOnEmpty {
		return intCompare(opsA.numDefaultOnEmpty, opsB.numDefaultOnEmpty)
	}

	// Statistics-driven scalar cost — a Go EXTENSION in the tiebreak
	// slot. Java's PlanningCostModel is purely heuristic (ordinal rungs
	// ending in a planHash tiebreak; no statistics rung exists), so this
	// discriminator sits before the hash tiebreak rather than mirroring
	// a Java rung. NOT a prune-to-1 workaround: retiring it (WS-P stage
	// (d) probe) regressed genuine selectivity decisions (equality-index
	// preference, vector outer-limit folding).
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

// wholePlanMaxCardinalityKnown reports whether the PROVEN whole-plan max
// cardinality of e is known — Java's cardinalities().evaluate(e).getMaxCardinality()
// gate. Computed from e's children's plan properties (computeCardinalities);
// falls back to unknown (guard fails → abstain) when e is not a physical plan
// or its children's properties are unavailable, which is the conservative
// direction (matches Java abstaining when the whole-plan bound is unknown).
func wholePlanMaxCardinalityKnown(e expressions.RelationalExpression) bool {
	ph, ok := e.(physicalPlanExpression)
	if !ok {
		return false
	}
	plan := ph.GetRecordQueryPlan()
	if plan == nil {
		return false
	}
	return !computeCardinalities(ph, plan).GetMaxCardinality().IsUnknown()
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
	numDefaultOnEmpty        int
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
//
// Operates on the bare *plans.RecordQueryScanPlan, the physical scan expression
// the memo-descent cost walk sees (RFC-184 W2).
func scanProvableMaxCard(plan *plans.RecordQueryScanPlan) (float64, bool) {
	if plan == nil {
		return 0, false
	}
	fullBind, provable := pkFullyEqualityBound(plan, nil)
	if fullBind && provable {
		return 1, true
	}
	return 0, false
}

// indexProvableMaxCard returns an index scan's PROVABLE max cardinality and whether it is known:
// 1 ONLY when the index is UNIQUE and every index column is equality-bound; otherwise unknown.
func indexProvableMaxCard(p *plans.RecordQueryIndexPlan) (float64, bool) {
	if p == nil || !p.IsUnique() {
		return 0, false
	}
	numBound := 0
	allEquality := true
	for _, cr := range p.GetScanComparisons() {
		if !cr.IsEmpty() {
			numBound++
			if !cr.IsEquality() {
				allEquality = false
			}
		}
	}
	if numBound > 0 && allEquality && numBound == len(p.GetColumnNames()) {
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
			return concretePlanCounts(plan, ctx)
		}
	}
	counts := expressionCounts{maxDataAccessCardinality: -1}
	visited := make(map[*expressions.Reference]bool)
	walkExpressionTree(e, &counts, stats, ctx, visited)
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
	// The scalar cost comparator is NOT a total order (Cost ties are routine
	// under default stats) and GetBest resolves ties by member insertion
	// order — which shifts across plannings (memo merges, alias
	// renumbering), flipping the picked child and with it every parent
	// criteria comparison run-to-run. Wrap with the deterministic
	// plan-hash tie-break so the minimum is unique (Java
	// PlanningCostModel.compare's final planHash arm; Java additionally
	// avoids most ties via prune-to-1, which Go lacks mid-phase).
	best := ref.GetBest(lessWithHashTieBreak(properties.CostLessWith(stats)))
	if best != nil {
		if _, ok := best.(physicalPlanExpression); ok {
			return best
		}
	}
	return firstPhysicalChild(ref)
}

func walkExpressionTree(e expressions.RelationalExpression, counts *expressionCounts, stats properties.StatisticsProvider, ctx PlanContext, visited map[*expressions.Reference]bool) {
	if e == nil {
		return
	}
	switch w := e.(type) {
	// A bare primary scan is its own physical expression now (RFC-184 W2) — the
	// memo-descent cost walk over a LOGICAL parent counts the scan here (the
	// physical top path already routes bare scans via concretePlanCounts).
	case *plans.RecordQueryScanPlan:
		counts.scanCount++
		if card, known := scanPlanProvableMaxCard(w, ctx); known {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
		}
		counts.unmatchedFieldCount += unmatchedFieldsForScan(w, ctx)
	// The aggregate-index plan is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryAggregateIndexPlan:
		counts.coveringIndexCount++
		// Aggregate access groups rows — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	// The vector scan is its own Cascades expression now (RFC-184 W2).
	case *plans.RecordQueryVectorIndexPlan:
		counts.indexScanCount++
		// Top-K vector scan — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	// An index scan is its own physical expression now (RFC-184 W2) — counted
	// exactly like the physicalIndexScanWrapper that used to carry it, reading its
	// covering flag and column metadata from the plan.
	case *plans.RecordQueryIndexPlan:
		if w.IsCovering() {
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
		totalCols := len(w.GetColumnNames())
		boundCols := 0
		for _, cr := range w.GetScanComparisons() {
			if !cr.IsEmpty() {
				boundCols++
			}
		}
		counts.unmatchedFieldCount += totalCols - boundCols
	// A type filter is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryTypeFilterPlan:
		counts.typeFilterCount += len(w.GetRecordTypes())
	// The PredicatesFilter is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryPredicatesFilterPlan:
		counts.predicatesFilterCount++
	// A map/projection is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryMapPlan:
		counts.mapCount++
	// The InJoin is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryInJoinPlan:
		counts.inJoinCount++
	// The InUnion is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryInUnionPlan:
		counts.inUnionCount++
	// The FlatMap is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryFlatMapPlan:
		counts.flatMapCount++
	// The NestedLoopJoin is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryNestedLoopJoinPlan:
		counts.nestedLoopJoinCount++
		counts.nljPredicateCount += len(w.GetPredicates())
	// A fetch is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		counts.fetchCount++
	// The ON EMPTY NULL default is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryDefaultOnEmptyPlan:
		counts.numDefaultOnEmpty++
	// The InMemorySort is its own physical expression now (RFC-184 W2) — the
	// memo-descent cost walk over a LOGICAL parent counts the sort here (the
	// physical top path already routes bare sorts via concretePlanCounts).
	case *plans.RecordQueryInMemorySortPlan:
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
			walkExpressionTree(child, counts, stats, ctx, visited)
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
	// Mirror concreteResidualPredicates: PredicatesFilter, legacy Filter, and a
	// materialized NLJ's join predicate are all residual conjuncts (#3). This
	// fallback only runs when the compared expression is not itself a physical
	// plan (the physical path takes concreteResidualPredicates), so the counters
	// must agree on what a residual is.
	switch n := e.(type) {
	case *plans.RecordQueryPredicatesFilterPlan:
		for _, p := range n.GetPredicates() {
			*count += int(cnfSize(p))
		}
	case *plans.RecordQueryFilterPlan:
		for _, p := range n.GetPredicates() {
			*count += int(cnfSize(p))
		}
	case *plans.RecordQueryNestedLoopJoinPlan:
		for _, p := range n.GetPredicates() {
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
	aDFS, aLevel := recursiveCTEKind(a)
	bDFS, bLevel := recursiveCTEKind(b)

	if aDFS && bLevel {
		return -1
	}
	if aLevel && bDFS {
		return 1
	}
	return 0
}

// recursiveCTEKind classifies an expression by its CONCRETE recursive-CTE plan
// type (via GetRecordQueryPlan), NOT the Cascades wrapper Go type. The physical
// wrappers are an implementation detail slated for deletion (RFC-184 W2); keying
// off the embedded plan keeps this DFS-over-LevelUnion tie-breaker alive once the
// wrappers are gone (a wrapper type-assert would return 0 → the decision would
// drop to a hash tie-break and RFC-130 would regress to the double-charging level
// union). Belt-and-suspenders: the cost term
// (plans.RecordQueryRecursiveLevelUnionPlan.HintCost) already makes the DFS join
// strictly cheaper, so the scalar-cost fallback also prefers DFS — but a
// structural preference that survives identity changes is cheap insurance.
func recursiveCTEKind(e expressions.RelationalExpression) (isDFS, isLevel bool) {
	ph, ok := e.(physicalPlanExpression)
	if !ok {
		return false, false
	}
	// The two recursive-CTE alternatives materialize as Project(RecursiveDfsJoin)
	// vs Project(RecursiveLevelUnion) — the recursive operator is NESTED under a
	// transparent projection (and possibly a chain of single-child pass-throughs),
	// not the root plan. Descend through single-child nodes to the TOP recursive
	// operator so the DFS-over-LevelUnion structural precedence actually fires;
	// checking only the root plan (a Project) always returned (false,false), which
	// left the decision to the cardinality-proportional cost margin — the margin
	// that collapses once point-lookup recursion legs are costed correctly.
	for p := ph.GetRecordQueryPlan(); p != nil; {
		switch p.(type) {
		case *plans.RecordQueryRecursiveDfsJoinPlan:
			return true, false
		case *plans.RecordQueryRecursiveLevelUnionPlan:
			return false, true
		}
		ch := p.GetChildren()
		if len(ch) != 1 {
			return false, false
		}
		p = ch[0]
	}
	return false, false
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
	// The InJoin is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryInJoinPlan:
		bindingNames = []string{w.GetBindingName()}
	// The InUnion is its own physical expression now (RFC-184 W2).
	case *plans.RecordQueryInUnionPlan:
		bindingNames = w.GetBindingNames()
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
	if p, ok := e.(*plans.RecordQueryIndexPlan); ok {
		return equalityAliasesFromRanges(p.GetScanComparisons())
	}
	_, isIntersection := e.(*plans.RecordQueryIntersectionPlan)
	_, isMultiIntersection := e.(*plans.RecordQueryMultiIntersectionOnValuesPlan)
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
	// Sort-invariant depth, in parity with concretePlanDepth (RFC-190 190.2): an
	// InMemorySort is transparent — it contributes no depth level — so this
	// logical-fallback path measures depth the same way the physical path does
	// (and the same way Java's sort-node-free tree would). Reachable because the
	// recursion descends into physical children (firstPhysicalChild below), which
	// can be an InMemorySort.
	inc := 1
	if _, isSort := e.(*plans.RecordQueryInMemorySortPlan); isSort {
		inc = 0
	}
	best := -1
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			d := expressionDepthRec(child, match, depth+inc)
			if d >= 0 && (best < 0 || d < best) {
				best = d
			}
		}
	}
	return best
}

func isTypeFilterExpression(e expressions.RelationalExpression) bool {
	_, ok := e.(*plans.RecordQueryTypeFilterPlan)
	return ok
}

func isDistinctExpression(e expressions.RelationalExpression) bool {
	// Since RFC-184 W2 the memo holds *plans.RecordQueryDistinctPlan directly
	// (no physicalDistinctWrapper), so this is a bare type check.
	_, ok := e.(*plans.RecordQueryDistinctPlan)
	return ok
}

func isFetchExpression(e expressions.RelationalExpression) bool {
	_, ok := e.(*plans.RecordQueryFetchFromPartialRecordPlan)
	if ok {
		return true
	}
	// A bare index scan is its own physical expression now (RFC-184 W2).
	_, ok = e.(*plans.RecordQueryIndexPlan)
	return ok
}

// comparePrimaryScanVsIndexScan ports Java's comparePrimaryScanToIndexScan
// (invoked via flipFlop). Only fires when one plan is a singular primary scan
// and the other is a singular index scan WITH a fetch (non-covering or
// covering+fetch); a covering index without fetch is strictly better and
// doesn't enter this path. When it fires it runs the type-filter SARG sub-case
// (prefer the index when it SARGs strictly more, needing no type filter) and
// otherwise applies the PREFER_SCAN default — see primaryVsIndexVerdict.
func comparePrimaryScanVsIndexScan(a, b expressions.RelationalExpression, opsA, opsB expressionCounts, pref IndexScanPreference) int {
	aIsPrimaryScan := opsA.scanCount == 1 && opsA.indexScanCount == 0 && opsA.coveringIndexCount == 0 && opsA.inMemorySortCount == 0
	bIsPrimaryScan := opsB.scanCount == 1 && opsB.indexScanCount == 0 && opsB.coveringIndexCount == 0 && opsB.inMemorySortCount == 0
	aIsIndexScanWithFetch := isSingularIndexScanWithFetch(opsA)
	bIsIndexScanWithFetch := isSingularIndexScanWithFetch(opsB)

	if aIsPrimaryScan && bIsIndexScanWithFetch {
		return primaryVsIndexVerdict(a, b, opsA, opsB, pref)
	}
	if bIsPrimaryScan && aIsIndexScanWithFetch {
		// Roles swapped: b is the primary scan, a the index. The verdict is
		// computed in the (primary, index) orientation, then negated — Java's
		// flipFlop(compare(primary,index), compare(index,primary)) negation.
		return -primaryVsIndexVerdict(b, a, opsB, opsA, pref)
	}
	return 0
}

// indexScanPreferenceOf reads the IndexScanPreference from the planner config,
// defaulting to PreferScan (the Cascades default) when the context or config is
// absent.
func indexScanPreferenceOf(ctx PlanContext) IndexScanPreference {
	if ctx == nil {
		return PreferScan
	}
	return ctx.GetPlannerConfiguration().IndexScanPreference
}

// primaryVsIndexVerdict ports the body of Java's comparePrimaryScanToIndexScan
// in its native (primaryScan=first, indexScan=second) orientation. Returns +1
// when the INDEX scan is preferred (the primary loses), -1 when the primary
// scan is preferred. The caller negates when the roles are swapped (flipFlop).
//
// The SARG sub-case: if the primary side carries a type filter and the index
// side none, and the two scans SARG the same comparisons EXCEPT the index has
// extra comparisons the primary lacks (e.g. a record-type-key comparison the
// index gets for free but the primary must pay for with a high-discard type
// filter), prefer the index. Otherwise fall back to the IndexScanPreference
// config branch (Java: PREFER_SCAN → prefer the primary; PREFER_INDEX /
// PREFER_PRIMARY_KEY_INDEX → prefer the index).
func primaryVsIndexVerdict(primaryScan, indexScan expressions.RelationalExpression, opsPrimary, opsIndex expressionCounts, pref IndexScanPreference) int {
	if opsPrimary.typeFilterCount > 0 && opsIndex.typeFilterCount == 0 {
		primaryComparisons := scanSargComparisons(primaryScan)
		indexComparisons := scanSargComparisons(indexScan)
		// primary − index empty AND index − primary non-empty ⇒ the index
		// SARGs everything the primary does plus more, without a type filter.
		if sargSubset(primaryComparisons, indexComparisons) &&
			!sargSubset(indexComparisons, primaryComparisons) {
			return 1 // prefer the index scan
		}
	}
	// Config branch (Java comparePrimaryScanToIndexScan): PREFER_SCAN prefers the
	// primary scan (-1); the non-scan preferences prefer the index (+1). The
	// Cascades default is PREFER_SCAN, so with no config surface setting a
	// non-default this stays -1 (unchanged behavior).
	if pref == PreferScan {
		return -1
	}
	return 1
}

// scanSargComparisons collects the BARE SARG comparisons (compared by Comparison
// identity — type/comparand/escape/parameter — NOT by column, which is implicit
// in ScanComparisons position) from every scan/index plan in e's CONCRETE plan
// tree. Mirrors Java's ComparisonsProperty (a Set<Comparisons.Comparison>).
//
// It unwraps to the concrete RecordQueryPlan (GetRecordQueryPlan) and walks it
// via GetChildren — the production scanPlanExpression / TypeFilter(Scan) wrapper
// exposes NO quantifiers (GetQuantifiers()==nil, like the counts walk's
// findExpressionsByType unwrap), so an expression-level walk would miss the
// nested scan's SARGs and make the primary set spuriously empty, firing the
// sub-case whenever the index has any SARG (e.g. demoting a PK point lookup).
//
// Comparisons are compared by structural equality (sargComparisonEqual), NOT a
// rendered/hashed key: display text collides distinct constants (int64(1) vs
// float64(1)) and an alias-blind hash collides correlated composite-key SARGs
// (k1=outerA.id vs k2=outerB.id).
func scanSargComparisons(e expressions.RelationalExpression) []*predicates.Comparison {
	var out []*predicates.Comparison
	if ph, ok := e.(physicalPlanExpression); ok {
		if plan := ph.GetRecordQueryPlan(); plan != nil {
			collectPlanSargComparisons(plan, &out)
		}
	}
	return out
}

func collectPlanSargComparisons(p plans.RecordQueryPlan, out *[]*predicates.Comparison) {
	if p == nil {
		return
	}
	var ranges []*predicates.ComparisonRange
	switch w := p.(type) {
	case *plans.RecordQueryScanPlan:
		ranges = w.GetScanComparisons()
	case *plans.RecordQueryIndexPlan:
		ranges = w.GetScanComparisons()
	}
	for _, r := range ranges {
		switch {
		case r.IsEquality():
			if eq := r.GetEqualityComparison(); eq != nil {
				*out = append(*out, eq)
			}
		case r.IsInequality():
			*out = append(*out, r.GetInequalityComparisons()...)
		}
	}
	for _, c := range p.GetChildren() {
		collectPlanSargComparisons(c, out)
	}
}

// sargSubset reports whether every comparison in sub also appears in super
// (i.e. sub − super is empty), by full Comparison identity.
func sargSubset(sub, super []*predicates.Comparison) bool {
	for _, c := range sub {
		found := false
		for _, s := range super {
			if sargComparisonEqual(c, s) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// sargComparisonEqual is the FULL structural identity of a scan comparison, the
// analog of the polymorphic Java Comparisons.Comparison.equals — every identity
// field of the flat Go Comparison struct: type, comparand (alias-SENSITIVE
// ValuesStructurallyEqual, so k1=outerA.id and k2=outerB.id are distinct),
// escape, parameter name, the text-search variants (tokenizer/analyzer/max
// distance/strict prefix) and the vector distance-rank variants (query vector /
// EfSearch / IsReturningVectors). The comparand is IGNORED for unary comparisons
// (IS NULL / IS NOT NULL), where it is semantically absent. NO column/position
// component (implicit in ScanComparisons position, deliberately excluded).
func sargComparisonEqual(a, b *predicates.Comparison) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Type != b.Type ||
		a.Escape != b.Escape ||
		a.ParameterName != b.ParameterName ||
		a.TextTokenizerName != b.TextTokenizerName ||
		a.TextAnalyzerName != b.TextAnalyzerName ||
		a.TextMaxDistance != b.TextMaxDistance ||
		a.TextStrictPrefix != b.TextStrictPrefix {
		return false
	}
	if !intPtrEqual(a.EfSearch, b.EfSearch) || !boolPtrEqual(a.IsReturningVectors, b.IsReturningVectors) {
		return false
	}
	if !values.ValuesStructurallyEqual(a.QueryVector, b.QueryVector) {
		return false
	}
	if a.Type.IsUnary() {
		return true // comparand semantically absent for IS NULL / IS NOT NULL
	}
	return values.ValuesStructurallyEqual(a.Operand, b.Operand)
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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
//
// Unlike stablePlanHash, this LOGICAL-path hash still folds
// HashCodeWithoutChildren — which carries minted correlation identifiers
// (q$N) — so REWRITING-phase ties between alias-only twins resolve by
// arrival order, not hash order. Tolerated deliberately: REWRITING's
// criteria (select/table-function/conjunct counts, predicate depth) are
// structural and rarely tie across genuinely different rewrites, no
// nondeterminism has been observed there (the PLANNING-phase flip that
// motivated stablePlanHash came from cost-tied PHYSICAL candidates), and a
// logical alias-blind hash needs per-expression-type stable content that
// does not exist yet. If a REWRITING-phase EXPLAIN flip ever surfaces,
// this is the site to extend.
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
			// ORDER-SENSITIVE fold — see stablePlanHash: a commutative XOR
			// made swapped join operands hash equal and the tie-break blind.
			h = h*0x100000001b3 ^ (childHash*0x517cc1b727220a95 + 0x6c62272e07bb0142)
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
	costA := concretePlanCost(planA, stats, ctx)
	costB := concretePlanCost(planB, stats, ctx)

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
	// heuristic (RFC-152).
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
// already-rolled-up child costs. Split out of concretePlanCost so the fold over
// children is expressible independently of the recursion. The per-operator
// formulas (cost_formulas.go) are the single source of truth shared with the
// physical-wrapper HintCost methods.
func combineConcreteCost(p plans.RecordQueryPlan, child []properties.Cost, stats properties.StatisticsProvider, ctx PlanContext) properties.Cost {
	c0 := func() properties.Cost {
		if len(child) > 0 {
			return child[0]
		}
		return properties.Cost{}
	}
	switch pl := p.(type) {
	case *plans.RecordQueryScanPlan:
		// Primary-key scan: 1 row ONLY on a PROVABLE full-PK equality bind
		// (RFC-186 §2B, RFC-190.3). The plan's stamped PK arity is
		// authoritative; a single-record-type PlanContext is only a fallback
		// for older/hand-built plans. Unknown coverage fails closed instead of
		// treating an all-equality prefix as a point probe.
		fullBind, provable := pkFullyEqualityBound(pl, ctx)
		return scanLikeCost(pl.GetScanComparisons(), pl.GetRecordTypes(), stats, fullBind && provable)
	case *plans.RecordQueryIndexPlan:
		// Secondary index: a full-equality bind is a single row only if the index is
		// UNIQUE. Resolve uniqueness from PlanContext when available;
		// a nil/empty ctx falls back to non-unique (conservative bucket estimate) so
		// a non-unique equality (`status = ?`) is never mispriced as a point probe.
		_, unique := indexMetadata(pl, ctx)
		return scanLikeCost(pl.GetScanComparisons(), pl.GetRecordTypes(), stats, unique)
	case *plans.RecordQueryFlatMapPlan:
		if len(child) < 2 {
			return properties.Cost{}
		}
		return properties.FlatMapCost(child[0], child[1])
	case *plans.RecordQueryNestedLoopJoinPlan:
		if len(child) < 2 {
			return properties.Cost{}
		}
		return properties.NestedLoopJoinCost(child[0], child[1])
	case *plans.RecordQueryPredicatesFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.FilterCost(c0(), len(pl.GetPredicates()))
	case *plans.RecordQueryFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.FilterCost(c0(), len(pl.GetPredicates()))
	case *plans.RecordQueryTypeFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.TypeFilterCost(c0())
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.FetchCost(c0())
	case *plans.RecordQueryMapPlan, *plans.RecordQueryProjectionPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.MapCost(c0())
	case *plans.RecordQueryFirstOrDefaultPlan:
		return properties.FirstOrDefaultCost(c0())
	case *plans.RecordQueryInMemorySortPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.InMemorySortCost(c0())
	case *plans.RecordQueryDistinctPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.DistinctCost(c0())
	case *plans.RecordQueryIntersectionPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.IntersectionCost(child)
	default:
		// RFC-186 §2D: an operator without an explicit arm dispatches to its
		// own HintCost — the per-plan formulas in plan/plans/cost.go are the
		// single source of truth — so a limit caps its child's cardinality,
		// an aggregation collapses to its group estimate, a union sums its
		// children and pays merge CPU, and every NEW plan type is priced
		// automatically the day it gains a HintCost. The explicit arms above
		// remain only where the join-ordering recursion deliberately
		// diverges from HintCost: the RFC-069 selectivity-only scan/index
		// leaves and the join/filter operators the walk recurses through
		// with its own child costs. Before this dispatch, whole operator
		// classes (limits, aggregations, unions, IN operators, recursive
		// plans) fell to first-child transparency — a join under a union
		// exposed only the first branch's cardinality and paid no merge
		// cost, flipping join-order selection.
		if hc, ok := p.(interface {
			HintCost([]properties.Cost, properties.StatisticsProvider) properties.Cost
		}); ok {
			return hc.HintCost(child, stats)
		}
		// No HintCost either: first-child-transparent, but LOUDLY (once per
		// concrete type) — silent transparency is how the class went
		// unpriced. Every new plan type must take a position.
		warnUnpricedPlanType(p)
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

// warnedUnpricedPlanTypes tracks which plan types have already produced the
// §2D transparency warning, so a hot planning loop emits each diagnosis
// exactly once per process.
var warnedUnpricedPlanTypes sync.Map

func warnUnpricedPlanType(p plans.RecordQueryPlan) {
	name := fmt.Sprintf("%T", p)
	if _, loaded := warnedUnpricedPlanTypes.LoadOrStore(name, struct{}{}); loaded {
		return
	}
	fmt.Fprintf(os.Stderr,
		"cascades: join-ordering cost walk found plan type %s with neither an explicit arm nor a HintCost — priced first-child-transparent (add a HintCost; RFC-186 §2D)\n",
		name)
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
// if it were one row. Without PlanContext we cannot prove a secondary
// index unique, so we conservatively fall through to the selectivity estimate; the
// metadata-aware wrapper HintCost still recognises unique indexes for the memo cost.
func scanLikeCost(comps []*predicates.ComparisonRange, recordTypes []string, stats properties.StatisticsProvider, fullBindUnique bool) properties.Cost {
	sel, numBound, allEquality := properties.BoundSelectivity(comps)
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

func firstPhysicalChild(ref *expressions.Reference) expressions.RelationalExpression {
	for _, m := range ref.AllMembers() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	return nil
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

// mergeCounts adds src's operator counts into dst, taking the max of provable
// max-cardinalities (-1 = unknown) and OR-ing the unbounded-access flag. The final
// "unbounded ⇒ unknown" reset is applied once by concretePlanCounts at the top.
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
	dst.numDefaultOnEmpty += src.numDefaultOnEmpty
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
// would otherwise be re-counted as unbounded. Split out of walkConcretePlan so a
// node's own contribution is expressible independently of the descent.
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
		counts.unmatchedFieldCount += unmatchedFieldsForScan(pl, ctx)
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
		// baked into this plan, and it reports no children to the walk, so
		// without this case they go uncounted and the node ranks as a
		// no-data-access node. Count it as ONE logical grouped data access — read
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
	case *plans.RecordQueryDefaultOnEmptyPlan:
		counts.numDefaultOnEmpty++
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
	fullBind, provable := pkFullyEqualityBound(pl, ctx)
	if !fullBind || !provable {
		return 0, false
	}
	return 1, true
}

// pkFullyEqualityBound is RFC-186 §2B's ONE shared full-PK-equality
// predicate — two subtly different inline gates in one file is how the
// composite-PK point-probe bug was born. fullBind reports that equality
// comparisons cover the FULL primary key (a partial equality prefix of a
// composite PK is still a range). provable reports whether the PK arity is
// known, independently of whether the scan is fully bound. Plan-stamped
// metadata is authoritative; a single-record-type ctx is the fallback.
func pkFullyEqualityBound(pl *plans.RecordQueryScanPlan, ctx PlanContext) (fullBind, provable bool) {
	if pl == nil {
		return false, false
	}
	pkLen := len(pl.GetPrimaryKeyValues())
	if pkLen > 0 {
		return properties.EqualityBoundsCoverKey(pl.GetScanComparisons(), pkLen), true
	}
	recordTypes := pl.GetRecordTypes()
	if ctx == nil || len(recordTypes) != 1 {
		return false, false
	}
	pkLen = len(ctx.GetPrimaryKeyColumns(recordTypes[0]))
	if pkLen == 0 {
		return false, false
	}
	return properties.EqualityBoundsCoverKey(pl.GetScanComparisons(), pkLen), true
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

// unmatchedFieldsForScan computes the UnmatchedFieldsCount contribution of a
// PRIMARY scan: PK-column-count minus numComparisons. Ports Java's
// UnmatchedFieldsCountProperty, which counts a RecordQueryScanPlan via
// commonPrimaryKey.getColumnSize() exactly like it counts an index plan via
// the index's key-column count — Go's port had ONLY the index-plan branch
// (this function did not exist), so a bare full scan (e.g. one driving an
// explicit in-memory sort to satisfy an ORDER BY that no index provides for
// free) silently scored 0 "unmatched fields" regardless of how few of its PK
// columns were bound, while a covering index scan used PURELY for ordering
// (every comparison empty — the index is walked only for the free sort
// order it provides, not for any WHERE-clause SARG) scored columnSize > 0.
// That is an apples-to-oranges comparison (only one side of the pair ever
// accrues a nonzero count), and once criterion #12 stopped being gated
// behind the in-memory-sort check (RFC-190 190.2 Part A/B), it let this
// asymmetry spuriously prefer the full-scan-plus-sort candidate over a
// well-targeted covering index scan the index already sorts for free — a
// real regression the golden diff caught (bytes.yaml `ORDER BY b` and
// siblings), traced to this missing branch by reading
// UnmatchedFieldsCountProperty.java line-for-line.
//
// Returns 0 (conservative, matching indexMetadata's not-found convention)
// when ctx is nil, the scan carries no record type, or ctx has no PK
// metadata for the type — the same "unknown metadata ⇒ no contribution"
// fallback the rest of this file uses (pkFullyEqualityBound,
// scanPlanProvableMaxCard).
func unmatchedFieldsForScan(pl *plans.RecordQueryScanPlan, ctx PlanContext) int {
	if ctx == nil || len(pl.GetRecordTypes()) == 0 {
		return 0
	}
	pkCols := ctx.GetPrimaryKeyColumns(pl.GetRecordTypes()[0])
	if len(pkCols) == 0 {
		return 0
	}
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
	columnSize := len(pkCols)
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
//
// SORT-INVARIANT (RFC-190 190.2): an in-memory sort is TRANSPARENT for
// structural depth — it does not increment the count. Java has no in-memory
// sort node at all (RemoveSortRule eliminates it before planning), so Java's
// structural depths never see one; a redundant Go RecordQueryInMemorySortPlan
// sitting on top of an otherwise-identical plan must not inflate every
// descendant's depth by +1 relative to a sort-elided sibling — that inflation
// made a depth comparison between a sorted and a sort-free plan measure from
// DIFFERENT roots, which is what produced the non-transitive 3-cycle this
// function's gate used to paper over (see cost_transitivity_test.go).
func concretePlanDepth(p plans.RecordQueryPlan, kind planMatchKind) int {
	if p == nil {
		return -1
	}
	if concretePlanMatches(p, kind) {
		return 0
	}
	if _, ok := p.(*plans.RecordQueryInMemorySortPlan); ok {
		best := -1
		for _, c := range p.GetChildren() {
			d := concretePlanDepth(c, kind)
			if d >= 0 && (best < 0 || d < best) {
				best = d
			}
		}
		return best
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

// stablePlanNodeHash is the #17 tie-break's per-node hash: the node's TYPE
// plus its ALIAS-BLIND stable content. It deliberately does NOT reuse
// HashCodeWithoutChildren — that is the memo-interning identity hash, where
// correlation aliases are sometimes semantic (TempTable identity) and where
// predicate content is folded via Explain text that RENDERS minted aliases
// (q$N). Minted identifiers differ on every planning of the same query, so a
// tie-break that hashes them ranks two cost-tied candidates in a different
// order each planning — a nondeterministic-EXPLAIN hazard
// caught live by the pins (the existential step-1 NLJ's operand order
// flipped across runs). Java's planHash is alias-blind at every node
// (QuantifiedObjectValue.planHash folds BASE_HASH only); predicates and
// values fold through the alias-blind SemanticHashCode here for the same
// property. Content the switch does not know folds as the type tag alone —
// COARSER stays deterministic: a residual tie keeps the first-arrived
// winner, which is stable once hash values stop varying per planning (the
// safety argument is the fallback, NOT rendering equivalence — hash-equal
// plans may still EXPLAIN differently where a discriminator is unhashed).
func stablePlanNodeHash(p plans.RecordQueryPlan) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%T|", p)
	switch t := p.(type) {
	case *plans.RecordQueryScanPlan:
		for _, rt := range t.GetRecordTypes() {
			_, _ = io.WriteString(h, rt)
			_, _ = h.Write([]byte{0})
		}
		// Unconditional 0/1 framing byte (not write-only-when-set): the
		// range tags that follow also start at 1, so a conditional write
		// would make "reverse + ranges" prefix-ambiguous with "forward + an
		// equality range".
		_, _ = h.Write([]byte{boolByte(t.IsReverse())})
		stableHashComparisonRanges(h, t.GetScanComparisons())
	case *plans.RecordQueryIndexPlan:
		_, _ = io.WriteString(h, t.GetIndexName())
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte{boolByte(t.IsReverse())})
		stableHashComparisonRanges(h, t.GetScanComparisons())
	case *plans.RecordQueryPredicatesFilterPlan:
		for _, pr := range t.GetPredicates() {
			stableHashU64(h, predicates.SemanticHashCode(pr))
		}
	case *plans.RecordQueryFilterPlan:
		for _, pr := range t.GetPredicates() {
			stableHashU64(h, predicates.SemanticHashCode(pr))
		}
	case *plans.RecordQueryNestedLoopJoinPlan:
		stableHashU64(h, uint64(t.GetJoinType()))
		for _, pr := range t.GetPredicates() {
			stableHashU64(h, predicates.SemanticHashCode(pr))
		}
		if rv := t.GetResultValue(); rv != nil {
			stableHashU64(h, values.SemanticHashCode(rv))
		}
	case *plans.RecordQueryFlatMapPlan:
		if rv := t.GetResultValue(); rv != nil {
			stableHashU64(h, values.SemanticHashCode(rv))
		}
	case *plans.RecordQueryMapPlan:
		if rv := t.GetResultValue(); rv != nil {
			stableHashU64(h, values.SemanticHashCode(rv))
		}
	case *plans.RecordQueryInMemorySortPlan:
		for _, k := range t.GetSortKeys() {
			_, _ = io.WriteString(h, k.Field)
			if k.ValueExpr != nil {
				stableHashU64(h, values.SemanticHashCode(k.ValueExpr))
			}
			if k.Desc {
				_, _ = h.Write([]byte{1})
			}
			_, _ = h.Write([]byte{0})
		}
	}
	return h.Sum64()
}

// stablePlanHash is the tie-break hash over a concrete plan tree: the
// alias-blind node head folded with the ORDERED children (FNV-style: the
// accumulator multiplies before each child mixes in — a bare `h ^= f(child)`
// is commutative, so the two operand orders of a symmetric join hashed
// IDENTICALLY and #17 returned 0 for exactly the swapped-operand pairs it
// exists to discriminate).
func stablePlanHash(p plans.RecordQueryPlan) uint64 {
	if p == nil {
		return 0
	}
	h := stablePlanNodeHash(p)
	for _, c := range p.GetChildren() {
		h = h*0x100000001b3 ^ (stablePlanHash(c)*0x517cc1b727220a95 + 0x6c62272e07bb0142)
	}
	return h
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func stableHashU64(h hash.Hash64, v uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	_, _ = h.Write(buf[:])
}

// stableHashComparisonRanges folds scan/index comparison ranges alias-blind:
// range shape (equality/inequality) + each comparison's type and operand
// value folded via values.SemanticHashCode (never Explain text — it renders
// minted correlation names).
func stableHashComparisonRanges(h hash.Hash64, ranges []*predicates.ComparisonRange) {
	for _, cr := range ranges {
		switch {
		case cr == nil || cr.IsEmpty():
			_, _ = h.Write([]byte{0})
		case cr.IsEquality():
			_, _ = h.Write([]byte{1})
			stableHashComparison(h, cr.GetEqualityComparison())
		case cr.IsInequality():
			_, _ = h.Write([]byte{2})
			for _, c := range cr.GetInequalityComparisons() {
				stableHashComparison(h, c)
			}
		}
	}
}

func stableHashComparison(h hash.Hash64, c *predicates.Comparison) {
	if c == nil {
		_, _ = h.Write([]byte{0})
		return
	}
	stableHashU64(h, uint64(c.Type))
	if c.Operand != nil {
		stableHashU64(h, values.SemanticHashCode(c.Operand))
	}
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

// costExprHash returns the deterministic tiebreak hash (criterion #17), over the
// concrete plan for a physical expression and the logical memo otherwise. A
// physical expression's plan is fully linked at construction, so the plan tree
// alone carries the buried data-access identity the tie-break needs.
func costExprHash(e expressions.RelationalExpression) uint64 {
	if ph, ok := e.(physicalPlanExpression); ok {
		if plan := ph.GetRecordQueryPlan(); plan != nil {
			return stablePlanHash(plan)
		}
	}
	return deepHashCode(e)
}
