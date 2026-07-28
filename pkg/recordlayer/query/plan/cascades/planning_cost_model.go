package cascades

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"log/slog"
	"reflect"
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

	residualA := countResidualPredicatesWithContext(a, ctx)
	residualB := countResidualPredicatesWithContext(b, ctx)
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

// indexProvableMaxCard is the logical-memo walk's plan-local cardinality
// policy: a stamped unique index with every key column equality-bound is at
// most one row. The concrete walk additionally resolves metadata from its
// PlanContext; keeping this plan-local form here preserves the established
// logical-fallback comparison when no concrete root exists.
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
	// A widening equality (a zero float) binds TWO keys, so this is not a
	// provable one-row bound. Shared with computeCardinalities and
	// isProvablePointProbe so the same plan cannot carry contradictory
	// cardinality claims -- the cost model previously kept ranking on the
	// false bound after the property was fixed.
	if numBound > 0 && allEquality && numBound == len(p.GetColumnNames()) &&
		!properties.AnyEqualityWidensBeyondOneKey(p.GetScanComparisons()) {
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
	if plan, ok := e.(plans.RecordQueryPlan); ok {
		countLogicalPlanNode(plan, counts, ctx)
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

// countLogicalPlanNode applies the logical memo-descent policy for a physical
// member selected beneath a logical root. Classification is shared with the
// concrete walk so a future type cannot silently fall through, while the
// contribution formulas remain deliberately separate: replacing them with the
// concrete policy would activate context metadata, point-probe folding, and
// multi-intersection folding on a path where those criteria historically did
// not apply, changing winners merely because diagnostics were enabled.
func countLogicalPlanNode(plan plans.RecordQueryPlan, counts *expressionCounts, ctx PlanContext) {
	classification, known := classifyConcretePlan(plan)
	if !known {
		warnUnclassifiedPlanType(
			ctx,
			costModelDiagnosticCounts,
			plan,
			"counted as operator-neutral in the logical memo walk; classify its cost-model counts",
		)
		return
	}

	switch classification.count {
	case concreteCountNeutral:
		// Deliberately contributes no logical structural count.
	case concreteCountScan:
		scan := plan.(*plans.RecordQueryScanPlan)
		counts.scanCount++
		if card, bounded := scanPlanProvableMaxCard(scan, ctx); bounded {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
		}
		counts.unmatchedFieldCount += unmatchedFieldsForScan(scan, ctx)
	case concreteCountIndex:
		index := plan.(*plans.RecordQueryIndexPlan)
		if index.IsCovering() {
			counts.coveringIndexCount++
		} else {
			counts.indexScanCount++
		}
		if card, bounded := indexProvableMaxCard(index); bounded {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
		}
		totalCols := len(index.GetColumnNames())
		boundCols := 0
		for _, comparison := range index.GetScanComparisons() {
			if !comparison.IsEmpty() {
				boundCols++
			}
		}
		counts.unmatchedFieldCount += totalCols - boundCols
	case concreteCountAggregateIndex:
		counts.coveringIndexCount++
		counts.unboundedDataAccess = true
	case concreteCountMultiIntersection:
		// Historical logical policy: contribute only through the aggregate
		// children. Concrete candidates fold those legs into one access.
	case concreteCountVectorIndex:
		counts.indexScanCount++
		counts.unboundedDataAccess = true
	case concreteCountTextIndex:
		// Historical logical policy: text access is count-neutral here.
		// Concrete candidates count it as an unbounded index access.
	case concreteCountTypeFilter:
		counts.typeFilterCount += len(plan.(*plans.RecordQueryTypeFilterPlan).GetRecordTypes())
	case concreteCountPredicatesFilter:
		counts.predicatesFilterCount++
	case concreteCountMap:
		counts.mapCount++
	case concreteCountInJoin:
		counts.inJoinCount++
	case concreteCountInUnion:
		counts.inUnionCount++
	case concreteCountFlatMap:
		counts.flatMapCount++
	case concreteCountNestedLoopJoin:
		join := plan.(*plans.RecordQueryNestedLoopJoinPlan)
		counts.nestedLoopJoinCount++
		counts.nljPredicateCount += len(join.GetPredicates())
	case concreteCountFetch:
		counts.fetchCount++
	case concreteCountInMemorySort:
		counts.inMemorySortCount++
	case concreteCountDefaultOnEmpty:
		counts.numDefaultOnEmpty++
	default:
		// A newly-added count kind without a logical policy is a classifier
		// bug, not an intentionally neutral type.
		warnUnclassifiedPlanType(
			ctx,
			costModelDiagnosticCounts,
			plan,
			"classified count kind has no logical memo-walk policy",
		)
	}
}

func countResidualPredicates(e expressions.RelationalExpression) int {
	return countResidualPredicatesWithContext(e, nil)
}

func countResidualPredicatesWithContext(e expressions.RelationalExpression, ctx PlanContext) int {
	if ph, ok := e.(physicalPlanExpression); ok {
		if plan := ph.GetRecordQueryPlan(); plan != nil {
			return concreteResidualPredicatesWithContext(plan, ctx)
		}
	}
	count := 0
	countResidualPredicatesRec(e, &count, ctx)
	return count
}

func countResidualPredicatesRec(e expressions.RelationalExpression, count *int, ctx PlanContext) {
	if e == nil {
		return
	}
	// Mirror concreteResidualPredicates: PredicatesFilter, legacy Filter, and a
	// materialized NLJ's join predicate are all residual conjuncts (#3). This
	// fallback only runs when the compared expression is not itself a physical
	// plan (the physical path takes concreteResidualPredicates), so the counters
	// must agree on what a residual is.
	if plan, ok := e.(plans.RecordQueryPlan); ok {
		classification, known := classifyConcretePlan(plan)
		if !known {
			warnUnclassifiedPlanType(
				ctx,
				costModelDiagnosticResidual,
				plan,
				"counted as having zero residual predicates in the logical memo walk; classify its predicate payload",
			)
		} else {
			*count += countClassifiedResidualPredicates(plan, classification, ctx)
		}
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			countResidualPredicatesRec(child, count, ctx)
		}
	}
}

func countClassifiedResidualPredicates(
	plan plans.RecordQueryPlan,
	classification concretePlanClassification,
	ctx PlanContext,
) int {
	switch classification.residual {
	case concreteResidualNeutral:
		return 0
	case concreteResidualPredicateCNF:
		carrier, ok := plan.(expressions.RelationalExpressionWithPredicates)
		if !ok {
			warnUnclassifiedPlanType(
				ctx,
				costModelDiagnosticResidual,
				plan,
				"classified as residual-bearing but does not expose RelationalExpressionWithPredicates",
			)
			return 0
		}
		total := 0
		for _, predicate := range carrier.GetPredicates() {
			total += int(cnfSize(predicate))
		}
		return total
	default:
		warnUnclassifiedPlanType(
			ctx,
			costModelDiagnosticResidual,
			plan,
			"classified residual kind has no predicate-count policy",
		)
		return 0
	}
}

// compareRecursiveCTE ranks both sides by recursiveCTERank, so — like
// compareInPlan/inPlanPenaltyRank and comparePrimaryScanVsIndexScan/
// primaryVsIndexRankOf — it is antisymmetric by construction and a total
// preorder on a small integer rank: comparing two per-plan ranks with intCompare
// can never produce an intransitive tie (CQ-23). The prior form derived its
// verdict from the PAIR (aDFS&&bLevel / aLevel&&bDFS), which let DFS tie
// Unclassified, Level tie Unclassified, and DFS still strictly beat Level — an
// indifference-transitivity violation (a~b, b~c, but not a~c).
func compareRecursiveCTE(a, b expressions.RelationalExpression) int {
	return intCompare(recursiveCTERank(a), recursiveCTERank(b))
}

// recursiveCTERank is compareRecursiveCTE's intrinsic per-plan rank: 0 for a
// DFS recursive join, 1 for anything recursiveCTEKind cannot classify
// (including every ordinary non-recursive-CTE plan — overwhelmingly the common
// case), 2 for a level-union recursive join. DFS is verified strictly cheaper
// than Level (recursiveCTEKind's doc comment / RecursiveLevelUnionPlan.HintCost
// double-charges the level union), so it sits below the unclassified default;
// Level is the one known-worse shape, so it sits above. An unclassified plan
// has no informed preference either way, which the middle rank encodes as
// "loses to a known-good DFS, beats a known-bad Level" rather than tying with
// both simultaneously.
func recursiveCTERank(e expressions.RelationalExpression) int {
	isDFS, isLevel := recursiveCTEKind(e)
	switch {
	case isDFS:
		return 0
	case isLevel:
		return 2
	default:
		return 1
	}
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

// compareInPlan ranks both sides by inPlanPenaltyRank, so it is antisymmetric
// by construction and is a total preorder on a rank in {0,1} — a legal rung of
// the lexicographic chain.
//
// DELIBERATE DIVERGENCE from Java (PlanningCostModel.compareInOperator /
// flipFlop), which evaluates the rung for the LEFT argument only and returns a
// PRESENT 0 for a SARGed IN-plan that flipFlop hands back without asking the
// reverse question. A present tie must not short-circuit the reverse question:
// the two orientations then disagree, `less` is false both ways, and the winner
// falls to a plan-hash coin flip — or, in a fold that compares without a
// tie-break, to member insertion order — discarding every later rung including
// the full cost model. Nothing here touches the wire; this is read-side plan
// choice only. Full derivation, Java file:line evidence and the pair table are
// in DIVERGENCES.md ("Criterion 6 — IN-plan SARG penalty").
func compareInPlan(a, b expressions.RelationalExpression, _, _ expressionCounts) int {
	return intCompare(inPlanPenaltyRank(a), inPlanPenaltyRank(b))
}

// inPlanPenaltyRank is criterion #6's rank: 1 for an IN-plan whose bindings
// never became search arguments, 0 for everything else — a non-IN plan and a
// SARGed IN-plan are equally unpenalised, and both fall through to the
// remaining rungs, which is what Java's Javadoc means by a SARGed in-plan
// causing "the remainder of the tie-breaking code to be used".
func inPlanPenaltyRank(e expressions.RelationalExpression) int {
	penalty, isInPlan := compareInOperator(e)
	if !isInPlan {
		return 0
	}
	return penalty
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

// collectSargedAliases returns the CorrelationIdentifiers that criterion #6
// counts as search arguments under e: the correlations of the SARGing
// comparisons the plan tree contributes, in Java's two-stage order — collect
// the Comparisons first (collectSargedComparisons, mirroring
// ComparisonsProperty), then derive aliases from the equality ones
// (PlanningCostModel.compareInOperator's ValueComparison/EQUALS filter).
func collectSargedAliases(e expressions.RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	return equalityComparisonAliases(collectSargedComparisons(e))
}

// collectSargedComparisons walks the physical plan tree and collects the
// Comparisons bounding its scans. It is Go's only port of
// ComparisonsProperty.ComparisonsVisitor, and it reproduces two of that
// visitor's behaviours:
//
//   - evaluateAtExpression unions the child results with the node's OWN
//     comparisons, which it reads off any RecordQueryPlanWithComparisons — every
//     plan carrying scan comparisons, an index scan and a PRIMARY scan alike.
//     ScanComparisonProvider is that interface here, so a binding that became a
//     search argument on a primary-key range scan counts exactly as one on an
//     index scan does.
//   - visitRecordQueryIntersectionPlan replaces that union with a retainAll
//     fold over the legs, so only comparisons carried by ALL legs survive, and
//     the intersection node contributes none of its own. Dispatched off the
//     IntersectionExpression marker, as Java dispatches off the visit override.
//
// The visitor's remaining behaviours are deliberately NOT reproduced, and the
// list is open rather than closed — it is what the Java class holds today:
//
//   - Unwrapping RecordQueryCoveringIndexPlan has no Go analogue at all:
//     covering is a flag on the index plan, not a wrapper around it.
//   - visitRecordQueryScoreForRankPlan REPLACES the child result with the
//     ranks' comparisons, and visitRecordQueryTextIndexPlan contributes the
//     text scan's grouping and text comparisons. Go's score-for-rank plan is a
//     pass-through union here and its text-index plan contributes nothing. Both
//     are unreachable rather than agreed: neither plan is constructed outside
//     tests, so no query can reach either arm.
//   - evaluateAtRef unions ALL members of a Reference; this descends
//     firstPhysicalChild only. That narrowing is the cost model's convention
//     for walking a concrete candidate (see concretePlanCounts) and predates
//     this property.
//
// The fold is over COMPARISON OBJECTS, not over the correlation aliases derived
// from them, and that granularity is load-bearing rather than incidental. The
// two differ exactly when an intersection's legs bind the same alias through
// DIFFERENT comparands — a record-typed IN splits `(a, b) IN ((1, 2), ...)`
// into `a = $x.0` and `b = $x.1`, so a leg on `a` and a leg on `b` share the
// alias $x while sharing no comparison. Comparison granularity is the one
// criterion #6 means: it asks whether "the rewritten equality" — one
// comparison, not one alias — became an index search argument, and under an
// intersection that equality only bounds the scan if every leg carries it.
// Alias granularity would answer yes for the record-typed IN above even though
// neither leg is bounded by the equality the other leg matched.
func collectSargedComparisons(e expressions.RelationalExpression) []*predicates.Comparison {
	if e == nil {
		return nil
	}
	if _, isIntersection := e.(properties.IntersectionExpression); isIntersection {
		return intersectChildComparisons(e)
	}
	var out []*predicates.Comparison
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			for _, c := range collectSargedComparisons(child) {
				out = addComparisonToSet(out, c)
			}
		}
	}
	if p, ok := e.(properties.ScanComparisonProvider); ok {
		for _, c := range comparisonsFromRanges(p.GetScanComparisons()) {
			out = addComparisonToSet(out, c)
		}
	}
	return out
}

func intersectChildComparisons(e expressions.RelationalExpression) []*predicates.Comparison {
	var childSets [][]*predicates.Comparison
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if child := firstPhysicalChild(ref); child != nil {
			childSets = append(childSets, collectSargedComparisons(child))
		}
	}
	if len(childSets) == 0 {
		return nil
	}
	result := childSets[0]
	for _, other := range childSets[1:] {
		var kept []*predicates.Comparison
		for _, c := range result {
			if containsComparison(other, c) {
				kept = append(kept, c)
			}
		}
		result = kept
	}
	return result
}

// comparisonsFromRanges is Java's RecordQueryPlanWithComparisons.getComparisons:
// every Comparison the ranges carry, equalities and inequalities alike. The
// EQUALS filter belongs at extraction, not here — filtering elementwise before
// or after the intersection gives the same set, and collecting everything keeps
// this the plan's comparison property rather than a criterion-#6 special case.
func comparisonsFromRanges(ranges []*predicates.ComparisonRange) []*predicates.Comparison {
	var out []*predicates.Comparison
	for _, cr := range ranges {
		for _, c := range cr.GetComparisons() {
			if c == nil {
				continue
			}
			out = addComparisonToSet(out, c)
		}
	}
	return out
}

// equalityComparisonAliases is compareInOperator's extraction step: keep the
// equality comparisons, flat-map their correlations.
func equalityComparisonAliases(comps []*predicates.Comparison) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, c := range comps {
		if c == nil || c.Type != predicates.ComparisonEquals {
			continue
		}
		for alias := range c.GetCorrelatedTo() {
			out[alias] = struct{}{}
		}
	}
	return out
}

// addComparisonToSet appends c unless the slice already holds a comparison
// equal to it, giving the slice the membership semantics of Java's
// ImmutableSet<Comparison> — Comparison equality there is semantic
// (Comparisons.ValueComparison.equals is semanticEquals under the empty alias
// map), which is what comparisonsEqual implements.
func addComparisonToSet(set []*predicates.Comparison, c *predicates.Comparison) []*predicates.Comparison {
	if containsComparison(set, c) {
		return set
	}
	return append(set, c)
}

func containsComparison(set []*predicates.Comparison, c *predicates.Comparison) bool {
	for _, e := range set {
		if comparisonsEqual(e, c) {
			return true
		}
	}
	return false
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
	// Since RFC-184 W2 the memo holds distinct plans directly (no
	// physicalDistinctWrapper), so both full-row and primary-key variants are
	// bare type checks.
	switch e.(type) {
	case *plans.RecordQueryDistinctPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
		return true
	default:
		return false
	}
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

// comparePrimaryScanVsIndexScan ranks both sides on primaryVsIndexRank, so it
// is antisymmetric AND transitive by construction — a legal rung of the
// lexicographic chain.
//
// DELIBERATE DIVERGENCE from Java (PlanningCostModel.comparePrimaryScanToIndexScan
// via flipFlop), whose applicability guard fires only for (lone primary scan)
// versus (singular index-scan-with-fetch). A pair-restricted criterion is not a
// total preorder, and the chain composes transitively only if every rung is one.
// Java's verdicts cannot merely be extended to the abstained pairs, either:
// restricted to the pairs it DOES adjudicate they already contain a cycle,
// because the type-filter sub-case decides on the SUBSET relation between the
// two sides' search-argument sets, and a subset relation is a partial order
// that no total preorder can reproduce. Go therefore ranks the sets by SIZE,
// which agrees with the subset test on every comparable pair and differs only
// on incomparable ones — exactly the configuration that made Java cyclic.
// Nothing here touches the wire; this is read-side plan choice only. Full
// derivation, Java file:line evidence and the four-plan cycle are in
// DIVERGENCES.md ("Criterion 7 — primary scan versus index scan with fetch").
func comparePrimaryScanVsIndexScan(a, b expressions.RelationalExpression, opsA, opsB expressionCounts, pref IndexScanPreference) int {
	return comparePrimaryVsIndexRank(
		primaryVsIndexRankOf(a, opsA, pref),
		primaryVsIndexRankOf(b, opsB, pref),
	)
}

// primaryVsIndexRank is criterion #7's per-plan rank. Lower is better.
//
// tier is the only component that is meaningful across the whole rank domain;
// sargs and shape are zero outside the CONTESTED tier so every other tier is a
// flat equivalence class that falls through to the rungs below, exactly as the
// pair-restricted form's abstention did.
type primaryVsIndexRank struct {
	tier int
	// sargs is the number of DISTINCT search-argument comparisons the plan's
	// data access carries. Compared DESCENDING: more search arguments wins.
	sargs int
	// shape is 0 for the primary scan and 1 for the index scan. It breaks a
	// contested-tier search-argument TIE in the primary scan's favour, which is
	// what Java does when the index buys no comparison the primary lacks.
	shape int
}

const (
	// The plan has no stake in the primary-versus-index trade-off, or it is the
	// configured preference's favoured shape unencumbered.
	primaryVsIndexTierUnencumbered = iota
	// The trade-off itself: a primary scan paying a type-filter discard against
	// an index scan paying a fetch. Resolved by search-argument count.
	primaryVsIndexTierContested
	// An index scan that pays a fetch AND a type filter — it bought neither of
	// the two things this criterion trades off.
	primaryVsIndexTierEncumberedIndex
	// A plan carrying an in-memory sort. Go's ImplementInMemorySortRule is a
	// read-side extension whose plans Java's ordinal rungs never see, and an
	// InMemorySort(Scan) is not a bare primary scan — it pays O(n log n) on top.
	// Ranking every sort-bearing plan last hands the decision to the sort-count
	// rung immediately below, which is the same verdict that rung would reach on
	// its own; the pair-restricted form applied this guard to the primary side
	// only, so an InMemorySort(IndexScan) could still win here and pre-empt it.
	primaryVsIndexTierSorted
)

func comparePrimaryVsIndexRank(a, b primaryVsIndexRank) int {
	if a.tier != b.tier {
		return intCompare(a.tier, b.tier)
	}
	if a.sargs != b.sargs {
		return intCompare(b.sargs, a.sargs) // more search arguments wins
	}
	return intCompare(a.shape, b.shape)
}

// primaryVsIndexRankOf places one plan on criterion #7's ladder.
//
// Under PREFER_SCAN (the Cascades default) the penalised shape is the index
// scan that must pay for a fetch; under the index-preferring configurations it
// is the lone primary scan, and Java's type-filter sub-case is moot there
// because both of its branches already return "prefer the index".
func primaryVsIndexRankOf(e expressions.RelationalExpression, ops expressionCounts, pref IndexScanPreference) primaryVsIndexRank {
	if ops.inMemorySortCount > 0 {
		return primaryVsIndexRank{tier: primaryVsIndexTierSorted}
	}
	isPrimaryScan := ops.scanCount == 1 && ops.indexScanCount == 0 && ops.coveringIndexCount == 0
	isIndexScanWithFetch := isSingularIndexScanWithFetch(ops)

	if pref != PreferScan {
		// Only the lone primary scan is penalised, unconditionally: there is no
		// trade-off left to weigh once the configuration has said "prefer the
		// index" for every branch.
		if isPrimaryScan {
			return primaryVsIndexRank{tier: primaryVsIndexTierContested}
		}
		return primaryVsIndexRank{tier: primaryVsIndexTierUnencumbered}
	}

	switch {
	case isPrimaryScan && ops.typeFilterCount > 0:
		return primaryVsIndexRank{
			tier:  primaryVsIndexTierContested,
			sargs: distinctSargCount(e),
			shape: 0,
		}
	case isIndexScanWithFetch && ops.typeFilterCount == 0:
		return primaryVsIndexRank{
			tier:  primaryVsIndexTierContested,
			sargs: distinctSargCount(e),
			shape: 1,
		}
	case isIndexScanWithFetch:
		return primaryVsIndexRank{tier: primaryVsIndexTierEncumberedIndex}
	default:
		// A lone primary scan that pays no type-filter discard, and every plan
		// with no stake in the trade-off (a covering index that needs no fetch,
		// a multi-access plan, …).
		return primaryVsIndexRank{tier: primaryVsIndexTierUnencumbered}
	}
}

// distinctSargCount is |ComparisonsProperty| for e's concrete plan tree — the
// cardinality of the SET Java's comparePrimaryScanToIndexScan takes differences
// of, so repeated comparisons on different columns count once, as they do in
// Java's Set<Comparisons.Comparison>.
func distinctSargCount(e expressions.RelationalExpression) int {
	all := scanSargComparisons(e)
	distinct := 0
	for i, c := range all {
		seen := false
		for _, earlier := range all[:i] {
			if sargComparisonEqual(c, earlier) {
				seen = true
				break
			}
		}
		if !seen {
			distinct++
		}
	}
	return distinct
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
// Unlike stablePlanHash, this LOGICAL-path hash normally folds
// HashCodeWithoutChildren — which carries minted correlation identifiers
// (q$N) — so REWRITING-phase ties between alias-only twins resolve by
// arrival order, not hash order. Schema-bearing projections opt into their
// historical schema-neutral tie-break hash: output names refine memo identity
// but do not change the work being costed or the pre-existing winner. The
// remaining alias sensitivity is tolerated deliberately: REWRITING's
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
	h := tieBreakNodeHash(e)
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
	// RecordQueryFlatMapPlan`). It used to need a pair-dependent metric switch here
	// (CPU-only for a shape mismatch, full cost otherwise) because the two shapes'
	// cardinality FORMULAS disagreed on the SAME logical join: nestedLoopJoinCost
	// used a cross-product proxy (outerCard*innerCard*sel) while flatMapCost used an
	// outer-only proxy (outerCard*sel) that ignored the inner side entirely. That
	// metric switch made compareJoinOrdering depend on which PAIR was being compared,
	// not on either plan alone — not a total preorder, and Reference.GetBest folds
	// pairwise, so the elected plan tracked member arrival order rather than cost
	// (CQ-24).
	//
	// The real defect was upstream: true join output cardinality is a property of
	// the LOGICAL group (both physical shapes implement the same join, so both must
	// agree on how many rows it produces), and each physical shape applies the join
	// predicate EXACTLY ONCE, just at a different point in its own subtree. A
	// FlatMap's inner already carries the predicate — pushed there by
	// RewriteOuterJoinRule as a below-null-extension PredicatesFilter, by the
	// data-access rule as an equality-bound SARG on the leaf scan/index, or as a
	// FirstOrDefault collapse for a scalar/EXISTS inner — so inner.Cardinality
	// already IS the filtered per-outer-row count; the NestedLoopJoin's inner is
	// raw and the join predicate is applied once, explicitly, by
	// NestedLoopJoinCost's own FilterSelectivity term. FlatMapCost now uses
	// outerCard*innerCard with no extra selectivity factor (see FlatMapCost's
	// doc comment), which makes the two formulas compute the SAME true
	// cardinality for the same logical join. With a single consistent
	// cardinality term, Cost.Less alone is a fair, shape-independent metric — no
	// pair-dependent branch needed.
	if costA.Less(costB) {
		return -1
	}
	if costB.Less(costA) {
		return 1
	}
	return 0
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
		// FK-chain cap (fk_chain_cardinality.go): a join whose inner leg
		// equality-binds the FULL primary key the outer chain is already
		// provably threaded through cannot output more rows than the inner
		// table itself has — sound regardless of drive direction or index
		// uniqueness (see that file's doc comment). When the cap actually
		// binds, fkChainCappedInnerCost derives a CPU-consistent inner Cost
		// from the SAME proven bound (see its doc comment for the
		// derivation) instead of only overwriting Cardinality — a hop cannot
		// be credited with producing at most `cap` rows while still being
		// charged CPU for scanning the larger, disproven row count.
		innerCost := child[1]
		if cap, ok := fkChainCardinalityCap(pl, stats); ok {
			if corrected, applied := fkChainCappedInnerCost(child[0], innerCost, cap); applied {
				innerCost = corrected
			}
		}
		return properties.FlatMapCost(child[0], innerCost)
	case *plans.RecordQueryNestedLoopJoinPlan:
		if len(child) < 2 {
			return properties.Cost{}
		}
		// Same unique-key equality-join detection plans/cost.go's HintCost
		// uses (plans.NestedLoopJoinUniqueKeyConjuncts) — this walk and the
		// memo's HintCost must feed properties.NestedLoopJoinCost the
		// identical proof, or compareJoinOrdering's total-preorder guarantee
		// (RFC-192) would rank the SAME concrete plan two different ways
		// depending on which cost path evaluated it.
		uniqueKeyConjuncts, _ := plans.NestedLoopJoinUniqueKeyConjuncts(pl)
		return properties.NestedLoopJoinCost(
			child[0], child[1], predicates.CountConjuncts(pl.GetPredicates()), uniqueKeyConjuncts)
	case *plans.RecordQueryPredicatesFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		// CountConjuncts, not len(): the same logical residual must apply the
		// same number of selectivity factors whether it is packaged as
		// [And(a, b)] or as [a, b] — matching the NestedLoopJoinPlan arm
		// above and RecordQueryPredicatesFilterPlan.HintCost (plans/cost.go).
		return properties.FilterCost(c0(), predicates.CountConjuncts(pl.GetPredicates()))
	case *plans.RecordQueryFilterPlan:
		if len(child) == 0 {
			return properties.Cost{}
		}
		return properties.FilterCost(c0(), predicates.CountConjuncts(pl.GetPredicates()))
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
	case *plans.RecordQueryDistinctPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
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
		warnUnclassifiedPlanType(
			ctx,
			costModelDiagnosticCost,
			p,
			"priced first-child-transparent; add HintCost or an explicit cost arm",
		)
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

type costModelDiagnosticWalk string

const (
	costModelDiagnosticCost     costModelDiagnosticWalk = "join_ordering_cost"
	costModelDiagnosticCounts   costModelDiagnosticWalk = "operator_counts"
	costModelDiagnosticResidual costModelDiagnosticWalk = "residual_predicates"
)

type costModelDiagnosticKey struct {
	walk     costModelDiagnosticWalk
	planType reflect.Type
}

type costModelDiagnostics struct {
	logger *slog.Logger
	warned sync.Map
}

type costModelDiagnosticContext struct {
	PlanContext
	diagnostics *costModelDiagnostics
}

func (c *costModelDiagnosticContext) costModelDiagnostics() *costModelDiagnostics {
	return c.diagnostics
}

type costModelDiagnosticsProvider interface {
	costModelDiagnostics() *costModelDiagnostics
}

// WithCostModelDiagnostics returns a PlanContext that routes cost-model
// classification warnings to logger. Warnings are deduplicated once per
// (walk, concrete plan type) within the returned context, so concurrent and
// repeated comparisons stay quiet without one diagnostic wrapper suppressing a
// separately constructed wrapper. Reusing a wrapper intentionally reuses its
// dedupe scope. A nil logger explicitly disables diagnostics, including any
// sink carried by an already-wrapped context. A nil context is treated as
// EmptyPlanContext. Apply this after any other PlanContext decorators so the
// diagnostic wrapper remains outermost; the private sink is intentionally not
// added to the public PlanContext interface.
func WithCostModelDiagnostics(ctx PlanContext, logger *slog.Logger) PlanContext {
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	var diagnostics *costModelDiagnostics
	if logger != nil {
		diagnostics = &costModelDiagnostics{logger: logger}
	}
	return &costModelDiagnosticContext{
		PlanContext: ctx,
		diagnostics: diagnostics,
	}
}

func costModelDiagnosticsFrom(ctx PlanContext) *costModelDiagnostics {
	provider, ok := ctx.(costModelDiagnosticsProvider)
	if !ok {
		return nil
	}
	return provider.costModelDiagnostics()
}

// costModelDiagnosticsOnlyContext strips metadata and configuration from ctx
// while retaining its diagnostic sink. Nil-statistics planner/rule comparators
// historically ran with no PlanContext; logging must not activate new winner
// criteria as a side effect.
func costModelDiagnosticsOnlyContext(ctx PlanContext) PlanContext {
	diagnostics := costModelDiagnosticsFrom(ctx)
	if diagnostics == nil {
		return nil
	}
	return &costModelDiagnosticContext{
		PlanContext: EmptyPlanContext(),
		diagnostics: diagnostics,
	}
}

func warnUnclassifiedPlanType(
	ctx PlanContext,
	walk costModelDiagnosticWalk,
	p plans.RecordQueryPlan,
	fallback string,
) {
	diagnostics := costModelDiagnosticsFrom(ctx)
	if diagnostics == nil || diagnostics.logger == nil || p == nil {
		return
	}
	if !diagnostics.logger.Enabled(context.Background(), slog.LevelWarn) {
		return
	}
	planType := reflect.TypeOf(p)
	name := planType.String()
	key := costModelDiagnosticKey{walk: walk, planType: planType}
	if _, loaded := diagnostics.warned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	diagnostics.logger.Warn(
		"cascades cost model found an unclassified plan type",
		"walk", string(walk),
		"plan_type", name,
		"fallback", fallback,
	)
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
//
// The 1-row shortcut's CPU is FetchCPU, not ScanCPU: a full-PK/full-unique-index
// equality bind returns EXACTLY one row via ONE isolated GetRange call (proven
// against the executor — key_value_cursor.go's initIterator, flat_map_cursor.go's
// unpipelined per-outer-row ExecutePlan — see FetchCPU's doc comment for the full
// trace). ScanCPU is the AMORTIZED per-row rate for a range read that streams back
// MANY rows in one call; a point probe streams back exactly one, so there is
// nothing to amortize over — it is the same isolated round trip a Fetch pays,
// priced at the same rate.
func scanLikeCost(comps []*predicates.ComparisonRange, recordTypes []string, stats properties.StatisticsProvider, fullBindUnique bool) properties.Cost {
	sel, numBound, allEquality := properties.BoundSelectivity(comps)
	// A widening equality (a zero float) binds TWO keys, so the probe is not
	// a single-row fetch. Shared with computeCardinalities,
	// isProvablePointProbe and indexProvableMaxCard -- the fourth and last
	// independent copy of this proof.
	if fullBindUnique && numBound > 0 && allEquality && numBound == len(comps) &&
		!properties.AnyEqualityWidensBeyondOneKey(comps) {
		return properties.Cost{Cardinality: 1, CPU: properties.FetchCPU}
	}
	total := 0.0
	if len(recordTypes) == 0 {
		total = stats.RecordTypeCardinality("")
	} else {
		for _, t := range recordTypes {
			total += stats.RecordTypeCardinality(t)
		}
	}
	card := total * sel
	return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU * physicalWrapperCostMultiplier}
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
		return // Node already accounted for the intentionally folded child access.
	}
	for _, c := range p.GetChildren() {
		walkConcretePlan(c, counts, ctx)
	}
}

// countConcreteNode adds plan node p's OWN operator contribution to counts (no
// recursion) and returns skipChildren=true when the caller must NOT descend into p's
// children — either a full-PK point probe, which already accounts for its scan, or a
// multi-intersection, whose aggregate legs are deliberately folded into one logical
// access. Split out of walkConcretePlan so a node's own contribution is expressible
// independently of the descent.
type concreteCountKind uint8

const (
	concreteCountNeutral concreteCountKind = iota
	concreteCountScan
	concreteCountIndex
	concreteCountAggregateIndex
	concreteCountMultiIntersection
	concreteCountVectorIndex
	concreteCountTextIndex
	concreteCountTypeFilter
	concreteCountPredicatesFilter
	concreteCountMap
	concreteCountInJoin
	concreteCountInUnion
	concreteCountFlatMap
	concreteCountNestedLoopJoin
	concreteCountFetch
	concreteCountInMemorySort
	concreteCountDefaultOnEmpty
)

type concreteResidualKind uint8

const (
	concreteResidualNeutral concreteResidualKind = iota
	concreteResidualPredicateCNF
)

type concretePlanClassification struct {
	count    concreteCountKind
	residual concreteResidualKind
}

// classifyConcretePlan is the single exhaustive cost-model taxonomy for all
// concrete plan types. A known zero contribution is explicit, while ok=false
// means a future plan type reached a walk without taking a position.
func classifyConcretePlan(p plans.RecordQueryPlan) (classification concretePlanClassification, ok bool) {
	switch p.(type) {
	case *plans.RecordQueryScanPlan:
		return concretePlanClassification{count: concreteCountScan}, true
	case *plans.RecordQueryIndexPlan:
		return concretePlanClassification{count: concreteCountIndex}, true
	case *plans.RecordQueryAggregateIndexPlan:
		return concretePlanClassification{count: concreteCountAggregateIndex}, true
	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		return concretePlanClassification{count: concreteCountMultiIntersection}, true
	case *plans.RecordQueryVectorIndexPlan:
		return concretePlanClassification{count: concreteCountVectorIndex}, true
	case *plans.RecordQueryTextIndexPlan:
		return concretePlanClassification{count: concreteCountTextIndex}, true
	case *plans.RecordQueryTypeFilterPlan:
		return concretePlanClassification{count: concreteCountTypeFilter}, true
	case *plans.RecordQueryPredicatesFilterPlan:
		return concretePlanClassification{
			count:    concreteCountPredicatesFilter,
			residual: concreteResidualPredicateCNF,
		}, true
	case *plans.RecordQueryMapPlan:
		return concretePlanClassification{count: concreteCountMap}, true
	case *plans.RecordQueryInJoinPlan:
		return concretePlanClassification{count: concreteCountInJoin}, true
	case *plans.RecordQueryInUnionPlan:
		return concretePlanClassification{count: concreteCountInUnion}, true
	case *plans.RecordQueryFlatMapPlan:
		return concretePlanClassification{count: concreteCountFlatMap}, true
	case *plans.RecordQueryNestedLoopJoinPlan:
		return concretePlanClassification{
			count:    concreteCountNestedLoopJoin,
			residual: concreteResidualPredicateCNF,
		}, true
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return concretePlanClassification{count: concreteCountFetch}, true
	case *plans.RecordQueryInMemorySortPlan:
		return concretePlanClassification{count: concreteCountInMemorySort}, true
	case *plans.RecordQueryDefaultOnEmptyPlan:
		return concretePlanClassification{count: concreteCountDefaultOnEmpty}, true
	case *plans.RecordQueryFilterPlan:
		return concretePlanClassification{residual: concreteResidualPredicateCNF}, true
	case *plans.RecordQueryComparatorPlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryExplodePlan,
		*plans.RecordQueryFirstOrDefaultPlan,
		*plans.RecordQueryInsertPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryLoadByKeysPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryRecursiveDfsJoinPlan,
		*plans.RecordQueryRecursiveLevelUnionPlan,
		*plans.RecordQueryScoreForRankPlan,
		*plans.RecordQuerySelectorPlan,
		*plans.RecordQueryStreamingAggregationPlan,
		*plans.RecordQueryTableFunctionPlan,
		*plans.RecordQueryTempTableInsertPlan,
		*plans.RecordQueryTempTableScanPlan,
		*plans.RecordQueryUnionPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan,
		*plans.RecordQueryUnorderedUnionPlan,
		*plans.RecordQueryUpdatePlan,
		*plans.RecordQueryValuesPlan:
		return concretePlanClassification{}, true
	default:
		return concretePlanClassification{}, false
	}
}

func countConcreteNode(p plans.RecordQueryPlan, counts *expressionCounts, ctx PlanContext) (skipChildren bool) {
	classification, known := classifyConcretePlan(p)
	if !known {
		warnUnclassifiedPlanType(
			ctx,
			costModelDiagnosticCounts,
			p,
			"counted as operator-neutral; classify its cost-model counts",
		)
		return false
	}
	return countClassifiedConcreteNode(p, classification, counts, ctx)
}

func countClassifiedConcreteNode(
	p plans.RecordQueryPlan,
	classification concretePlanClassification,
	counts *expressionCounts,
	ctx PlanContext,
) (skipChildren bool) {
	switch classification.count {
	case concreteCountNeutral:
		// Deliberately contributes no structural count.
	case concreteCountScan:
		pl := p.(*plans.RecordQueryScanPlan)
		counts.scanCount++
		if card, known := scanPlanProvableMaxCard(pl, ctx); known {
			if card > counts.maxDataAccessCardinality {
				counts.maxDataAccessCardinality = card
			}
		} else {
			counts.unboundedDataAccess = true
		}
		counts.unmatchedFieldCount += unmatchedFieldsForScan(pl, ctx)
	case concreteCountIndex:
		pl := p.(*plans.RecordQueryIndexPlan)
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
	case concreteCountAggregateIndex:
		counts.coveringIndexCount++
		// Aggregate access groups rows — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	case concreteCountMultiIntersection:
		// A multi-aggregate intersection's children are aggregate-index scans
		// baked into this plan. Count it as ONE logical grouped data access — read
		// the pre-aggregated groups, comparable to a single aggregate-index scan,
		// NOT N independent accesses. Counting it as N made criterion #3 (fewer
		// data accesses) prefer a single full Scan (count 1) over the intersection
		// (count N) even though the scan reads the whole table + sorts; counting
		// it as 1 ties the scan on count, then it wins on the scan-vs-covering-
		// index criterion exactly like the single-aggregate path. Skip the child
		// walk so the per-child scans aren't also counted.
		counts.coveringIndexCount++
		counts.unboundedDataAccess = true
		validateSkippedConcreteCountSubtrees(p.GetChildren(), ctx)
		return true
	case concreteCountVectorIndex:
		counts.indexScanCount++
		// Top-K vector scan — no provable ≤1 bound (Java: unknown).
		counts.unboundedDataAccess = true
	case concreteCountTextIndex:
		counts.indexScanCount++
		counts.unboundedDataAccess = true
	case concreteCountTypeFilter:
		pl := p.(*plans.RecordQueryTypeFilterPlan)
		counts.typeFilterCount += len(pl.GetRecordTypes())
	case concreteCountPredicatesFilter:
		pl := p.(*plans.RecordQueryPredicatesFilterPlan)
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
			validateSkippedConcreteCountSubtrees(p.GetChildren(), ctx)
			return true // already accounted for the scan; do not recurse (would mark unbounded)
		}
	case concreteCountMap:
		// Map only — NOT RecordQueryProjectionPlan. The map-count criterion (#14)
		// is a structural tiebreak; a near-ubiquitous top-of-query projection is
		// not a discriminating operator, and counting it makes #14 fire on almost
		// every plan pair. (concretePlanCost charges a projection via mapCost for
		// magnitude, a different purpose — the two walks need not count the same
		// nodes.) Counting projections here re-ranks ties broadly and selected a
		// latent-buggy CTE plan that mis-projects an aliased column to NULL —
		// caught by TestFDB_{CTEChainedColumnAliases,CascadesCTEColumnAliases}.
		counts.mapCount++
	case concreteCountInJoin:
		counts.inJoinCount++
	case concreteCountInUnion:
		counts.inUnionCount++
	case concreteCountFlatMap:
		counts.flatMapCount++
	case concreteCountNestedLoopJoin:
		pl := p.(*plans.RecordQueryNestedLoopJoinPlan)
		counts.nestedLoopJoinCount++
		counts.nljPredicateCount += len(pl.GetPredicates())
	case concreteCountFetch:
		counts.fetchCount++
	case concreteCountInMemorySort:
		counts.inMemorySortCount++
	case concreteCountDefaultOnEmpty:
		counts.numDefaultOnEmpty++
	default:
		warnUnclassifiedPlanType(
			ctx,
			costModelDiagnosticCounts,
			p,
			"classified count kind has no concrete count policy",
		)
	}
	return false
}

// validateSkippedConcreteCountSubtrees preserves the unclassified-type detector
// beneath operators whose children are intentionally folded into the parent's
// count. It is diagnostics-only and cannot mutate the comparison's counts.
func validateSkippedConcreteCountSubtrees(children []plans.RecordQueryPlan, ctx PlanContext) {
	diagnostics := costModelDiagnosticsFrom(ctx)
	if diagnostics == nil ||
		diagnostics.logger == nil ||
		!diagnostics.logger.Enabled(context.Background(), slog.LevelWarn) {
		return
	}
	var validate func(plans.RecordQueryPlan)
	validate = func(plan plans.RecordQueryPlan) {
		if plan == nil {
			return
		}
		if _, known := classifyConcretePlan(plan); !known {
			warnUnclassifiedPlanType(
				ctx,
				costModelDiagnosticCounts,
				plan,
				"hidden beneath a folded count subtree; classify its cost-model counts",
			)
		}
		for _, child := range plan.GetChildren() {
			validate(child)
		}
	}
	for _, child := range children {
		validate(child)
	}
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
	// A widening equality (a terminal zero float) binds TWO keys, so a fully
	// equality-bound PK is still not a one-row proof. Guarding the ONE shared
	// helper covers both scanProvableMaxCard and scanPlanProvableMaxCard.
	if properties.AnyEqualityWidensBeyondOneKey(pl.GetScanComparisons()) {
		return false, true
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
	// Same widening guard as computeCardinalities, isProvablePointProbe,
	// indexProvableMaxCard and scanLikeCost. This is the CONCRETE physical
	// path PlanningCostModelLess actually walks, so leaving it unguarded kept
	// maxDataAccessCardinality=1 for a zero probe even after the property was
	// fixed -- criterion #2 ranking on a bound the property had disowned.
	if numBound > 0 && allEquality && numBound == len(cols) &&
		!properties.AnyEqualityWidensBeyondOneKey(pl.GetScanComparisons()) {
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
	return concreteResidualPredicatesWithContext(p, nil)
}

func concreteResidualPredicatesWithContext(p plans.RecordQueryPlan, ctx PlanContext) int {
	total := 0
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		classification, known := classifyConcretePlan(n)
		if !known {
			warnUnclassifiedPlanType(
				ctx,
				costModelDiagnosticResidual,
				n,
				"counted as having zero residual predicates; classify its predicate payload",
			)
			return true
		}
		// A materialized NLJ evaluates its join predicate per (outer, inner)
		// pair. It is residual just like PredicatesFilter and legacy Filter;
		// the shared taxonomy above keeps the logical and concrete walks aligned.
		total += countClassifiedResidualPredicates(n, classification, ctx)
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
		switch p.(type) {
		case *plans.RecordQueryDistinctPlan,
			*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
			return true
		default:
			return false
		}
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
	case *plans.RecordQueryProjectionPlan:
		// Deliberately type-only. Projection Values and output names belong to
		// memo identity; the #17 cost tie-break historically treated two
		// projections over the same child as equal work. Folding the new
		// schema discriminator here would flip established plan shapes.
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
