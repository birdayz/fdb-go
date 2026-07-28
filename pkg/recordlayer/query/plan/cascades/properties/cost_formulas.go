package properties

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Single source of truth for per-operator physical cost formulas (RFC-069).
//
// Each function takes the already-rolled-up child Cost(s) and returns this
// operator's Cost. BOTH the physical plans' HintCost methods (which cost via
// the memo cost framework) and concretePlanCost (which costs the extracted
// RecordQueryPlan tree for the join-ordering criterion) call these — so a
// per-operator cost formula has exactly ONE definition and the two paths can
// never drift (RFC-069). Leaf scan/index cost is NOT here: the plan's leaf
// cost is metadata-aware (unique/covering) for the memo cost framework, while
// the concrete join-ordering cost uses a metadata-independent selectivity leaf
// cost (scanLikeCost) — those are deliberately different inputs to the same
// recursion, documented at each site.
//
// These live in `properties` rather than alongside the physical operators so
// that the plans themselves — which own their cost answer — can reach them
// without importing the planner package.

// PhysicalWrapperCostMultiplier is applied to each physical operator's
// inherited CPU so cost-driven extraction prefers physical plans over their
// logical counterparts. 0.9 = "physical is 10% cheaper than logical" — enough
// to flip ordering on equally-shaped alternatives.
//
// CPU-ONLY. It must NEVER scale Cardinality. Cardinality is a property of the
// LOGICAL group — every physical implementation of the same join/filter/etc.
// produces the same number of rows — so it cannot legitimately shrink just
// because a plan happens to wrap its child in one more physical node than an
// alternative does. CPU is different: each additional physical operator is a
// genuine additional step at execution time, so a per-node execution-overhead
// discount is a coherent (if approximate) model there.
//
// This WAS applied to Cardinality throughout this file and in plan/plans/cost.go
// (compounding once per physical node in a plan's depth), and it was NOT "small
// enough not to dominate the cost comparison with structurally-different
// alternatives" as this comment used to claim: measured directly, comparing a
// materialized NestedLoopJoin (one extra Fetch/Filter-free inner) against a
// re-scanning FlatMap whose inner carries one more wrapper node
// (PredicatesFilter/DefaultOnEmpty) made the FlatMap's Cardinality ~10% cheaper
// for IDENTICAL true selectivity — a swing bigger than the entire CPU term the
// two shapes otherwise differ by, so it silently decided compareJoinOrdering by
// tree shape instead of cost (the RFC-152 preserved-only regression). Confining
// the multiplier to CPU makes two physical realizations of the same logical
// join carry EXACTLY EQUAL Cardinality, so Cost.Less falls through to CPU —
// which is where the real discriminator (materialize-once vs re-scan-per-row)
// already lives.
const PhysicalWrapperCostMultiplier = 0.9

// FlatMapCost: a correlated dependent join re-runs the inner once per outer row.
//
// The CPU has two per-outer-row terms: outerCard*innerCPU (the inner's own work,
// re-paid each iteration) PLUS outerCard*IterationOverhead — a fixed per-iteration
// re-execution overhead (open the inner cursor, init the range read, bind the
// correlation). The overhead term is what makes driving the nested loop from the
// SMALLER side cheaper when criterion #2 abstains (two full-scan drivers, e.g. a
// 20-row vs a 200-row outer in a re-enumerated multi-way sub-join): the 200-row
// driver pays 10x the per-iteration setup. It is a Go-only read-side cost
// extension that lives ONLY here (the compareJoinOrdering concrete-cost path),
// sized as a tie-breaker so it never flips a clear cardinality/total-cost winner
// (RFC-041/042).
//
// Cardinality is outerCard*innerCard, with NO extra FilterSelectivity factor —
// the join predicate is NOT reapplied here because it is already accounted for
// inside inner.Cardinality. Every FlatMap inner reaches this formula having
// already applied the join predicate exactly once, in whichever form the inner
// subtree takes: an equality-bound SARG on the leaf scan/index
// (scanLikeCost/BoundSelectivity already discounts by the bind), a
// PredicatesFilter the implementation rule stacked on the inner for a residual
// join predicate (FilterCost already applies FilterSelectivity), or a
// FirstOrDefault collapse for a scalar/EXISTS inner (FirstOrDefaultCost floors
// cardinality at ~1, the correct one-row-per-outer count for that shape — not
// outerCard*sel, which under-counts a guaranteed-one-row-out wrapper). So
// inner.Cardinality already IS the true rows-yielded-per-outer-row count; using
// it directly gives the same total cardinality a NestedLoopJoinCost over the
// SAME logical join computes (outerCard*innerCardRaw*FilterSelectivity), because
// innerCard here equals innerCardRaw*FilterSelectivity — each shape just applies
// the predicate at a different place in its own subtree. Reapplying
// FilterSelectivity on top of an already-filtered innerCard would double-count
// the predicate and systematically undercost FlatMap relative to the
// materialized NestedLoopJoin over the identical join (see NestedLoopJoinCost).
//
// outerCard/innerCard are NOT zero-guarded. A zero Cardinality here is never
// "the child's cost wasn't computed" — every leaf/operator formula in this
// file that lacks a real cardinality either floors it at >=1 (FirstOrDefault,
// AggregateIndex, Explode) or returns before reaching FlatMapCost at all (the
// `len(child) < 2` guards in the callers). It IS the genuine output of an
// empty-relation operator — RecordQueryLimitPlan.HintCost's explicit
// cardinality-0 for LIMIT 0, or RecordQueryInUnionPlan.HintCost's cost-0 for a
// zero-length literal IN source (plan/plans/cost.go) — and must propagate
// unchanged: `SELECT * FROM t1, (SELECT * FROM t2 LIMIT 0)` joins to exactly
// zero rows, not outerCard*LeafScanCardinality. Substituting LeafScanCardinality
// for an exact zero silently turned a free empty join into the single most
// expensive shape on the plan, which can flip the winner away from it.
func FlatMapCost(outer, inner Cost) Cost {
	outerCard := outer.Cardinality
	innerCard := inner.Cardinality
	innerCPU := inner.CPU
	if innerCPU == 0 {
		innerCPU = FilterCPU
	}
	return Cost{
		Cardinality: outerCard * innerCard,
		CPU:         (outer.CPU + outerCard*(innerCPU+IterationOverhead)) * PhysicalWrapperCostMultiplier,
	}
}

// NestedLoopJoinCost: MATERIALIZED nested-loop join, outer × inner with per-pair filter.
//
// The inner subtree is executed ONCE: the executor (executor.go executeNestedLoopJoin)
// materializes the inner into a buffer via CollectAllBounded, then iterates the buffered
// rows per outer row. So the inner's own work (inner.CPU) is paid ONCE — NOT outerCard
// times. The per-pair term outerCard*innerCard*FilterCPU models iterating the buffer and
// evaluating the join predicate for every (outer,inner) pair (the in-memory work that DOES
// scale with the product).
//
// This materialization is what distinguishes the NLJ from a correlated FlatMap: FlatMapCost
// charges outerCard*innerCPU because the FlatMap RE-EXECUTES (re-scans from FDB) its inner
// once per outer row, whereas the materialized NLJ scans the inner once and re-iterates the
// buffer. Charging the NLJ outerCard*inner.CPU (as if it re-scanned) erased that distinction
// and let a re-scan FlatMap tie/beat the materialized NLJ for a NON-PROBE inner — the
// RFC-152 preserved-only LEFT-OUTER regression. A card-1 PROBE inner keeps the FlatMap
// cheapest regardless (its outerCard*~1 work beats materialize+iterate).
//
// numPreds is the join's predicate count, compounded exactly like FilterCost compounds
// numPreds selectivity factors for PredicatesFilter — one FilterSelectivity factor PER
// predicate, not one factor total. A 2-predicate FlatMap's inner is a PredicatesFilter
// that already compounds FilterSelectivity^2; before this parameter existed,
// NestedLoopJoinCost always applied exactly ONE factor regardless of predicate count, so
// a multi-predicate join's SAME logical cardinality was estimated two different ways by
// the two physical shapes — the identical group-property violation
// PhysicalWrapperCostMultiplier caused, one level removed: here it is the join's OWN
// selectivity term, not an external per-node discount, that failed to scale with an
// input (predicate count) both shapes must agree on.
//
// outer.Cardinality/inner.Cardinality are NOT zero-guarded, for the same reason
// FlatMapCost's are not (see its doc comment): an exact zero here is a genuine
// empty child (LIMIT 0, an empty literal IN-list), never "unknown", and must
// produce a genuine zero-row join rather than being inflated to
// outerCard*LeafScanCardinality.
//
// uniqueKeyConjuncts is how many of numPreds conjuncts the caller has PROVEN
// (plans/cost.go's nestedLoopJoinUniqueKeyConjuncts) to be a full equality
// bind on the inner leg's own UNIQUE key (primary key or a declared-UNIQUE
// index). For those conjuncts the flat per-predicate FilterSelectivity guess
// is provably wrong: a unique-key equality can match AT MOST ONE inner row
// per pair, so their TRUE combined selectivity is exactly 1/innerCard, not
// FilterSelectivity^uniqueKeyConjuncts. This is the exact invariant
// FlatMapCost's doc comment requires of this function — the two physical
// shapes of the SAME logical unique-key equality join must compute the SAME
// cardinality (outerCard), and only 1/innerCard achieves that here; the flat
// guess overestimates a 1000-row inner's join by ~500x (0.5 vs 1/1000). The
// remaining (numPreds-uniqueKeyConjuncts) conjuncts still take the flat
// FilterSelectivity fallback — the unique-key bind explains only ITS OWN
// conjuncts' selectivity, not any other residual predicate's.
//
// uniqueKeyConjuncts<=0, or an innerCard<=0 the ratio cannot divide by,
// reproduces today's unconditional flat-selectivity formula exactly — the
// correction is a gated ADDITION, never a replacement of the honest fallback
// when uniqueness cannot be proven (e.g. a non-unique secondary-index
// equality join, where the flat guess remains the right answer).
func NestedLoopJoinCost(outer, inner Cost, numPreds int, uniqueKeyConjuncts int) Cost {
	if numPreds < 1 {
		numPreds = 1
	}
	outerCard, innerCard := outer.Cardinality, inner.Cardinality
	sel := 1.0
	if uniqueKeyConjuncts > 0 && uniqueKeyConjuncts <= numPreds && innerCard > 0 {
		sel = 1.0 / innerCard
		for i := 0; i < numPreds-uniqueKeyConjuncts; i++ {
			sel *= FilterSelectivity
		}
	} else {
		for i := 0; i < numPreds; i++ {
			sel *= FilterSelectivity
		}
	}
	return Cost{
		Cardinality: outerCard * innerCard * sel,
		CPU:         (outer.CPU + inner.CPU + outerCard*innerCard*FilterCPU*float64(numPreds)) * PhysicalWrapperCostMultiplier,
	}
}

// FilterCost: one selectivity factor per predicate (min one).
func FilterCost(child Cost, numPreds int) Cost {
	if numPreds == 0 {
		numPreds = 1
	}
	in := child.Cardinality
	sel := 1.0
	for i := 0; i < numPreds; i++ {
		sel *= FilterSelectivity
	}
	return Cost{
		Cardinality: in * sel,
		CPU:         (child.CPU + in*FilterCPU*float64(numPreds)) * PhysicalWrapperCostMultiplier,
	}
}

// TypeFilterCost: record-type discrimination over the child stream.
func TypeFilterCost(child Cost) Cost {
	in := child.Cardinality
	return Cost{
		Cardinality: in * TypeFilterSelectivity,
		CPU:         (child.CPU + in*TypeFilterCPU) * PhysicalWrapperCostMultiplier,
	}
}

// FetchCost: primary-key fetch of the full record per index entry.
func FetchCost(child Cost) Cost {
	in := child.Cardinality
	return Cost{
		Cardinality: in,
		CPU:         (child.CPU + in*FetchCPU) * PhysicalWrapperCostMultiplier,
	}
}

// MapCost: per-row projection, cardinality-preserving.
func MapCost(child Cost) Cost {
	in := child.Cardinality
	return Cost{
		Cardinality: in,
		CPU:         (child.CPU + in*ProjectionCPU) * PhysicalWrapperCostMultiplier,
	}
}

// FirstOrDefaultCost: at most one row out, the child's work paid in full.
func FirstOrDefaultCost(child Cost) Cost {
	return Cost{Cardinality: 1, CPU: child.CPU * PhysicalWrapperCostMultiplier}
}

// InMemorySortCost: materialize + O(n log n). Note: NO physical-wrapper discount —
// an in-memory sort must stay strictly more expensive than index-based elimination.
//
// The returned Cardinality is the child's EXACT count, never clamped — the
// same zero-preservation reasoning as FlatMapCost/NestedLoopJoinCost applies
// (a LIMIT-0 child sorts zero rows, and must report zero, not a phantom one).
// Only the LOG argument is floored at >=1: math.Log2 is undefined below that,
// and the floor is purely a numerical-domain guard local to logN — it must
// never leak into the Cardinality the caller rolls up.
func InMemorySortCost(child Cost) Cost {
	n := child.Cardinality
	logInput := n
	if logInput < 1 {
		logInput = 1
	}
	logN := math.Max(1, math.Log2(math.Max(2, logInput)))
	return Cost{Cardinality: n, CPU: child.CPU + n*SortCPU*logN}
}

// DistinctCost: duplicate elimination over the child stream.
func DistinctCost(child Cost) Cost {
	in := child.Cardinality
	return Cost{
		Cardinality: in * DistinctSelectivity,
		CPU:         (child.CPU + in*DistinctCPU) * PhysicalWrapperCostMultiplier,
	}
}

// IntersectionCost: output bounded by the smallest child; work ~ scanning every
// child + per-output comparison-key merge. Carries the physical-wrapper CPU
// discount like the other join-tree operators so the plan HintCost and the
// concrete join-ordering cost agree exactly.
func IntersectionCost(child []Cost) Cost {
	if len(child) == 0 {
		return Cost{}
	}
	minCard, sumCard, sumCPU := child[0].Cardinality, 0.0, 0.0
	for _, c := range child {
		if c.Cardinality < minCard {
			minCard = c.Cardinality
		}
		sumCard += c.Cardinality
		sumCPU += c.CPU
	}
	return Cost{
		Cardinality: minCard,
		CPU:         (sumCPU + sumCard*IntersectionCPU) * PhysicalWrapperCostMultiplier,
	}
}

// BoundSelectivity returns the combined selectivity of a scan's bound
// comparison prefix, how many columns are bound, and whether every bound
// column is an equality. Callers layer their own unique / point-lookup
// short-circuit on top.
//
// Each equality bound multiplies in EqualityBoundSelectivity and each open
// range multiplies in RangeSelectivity. A point probe is more selective than a
// range, so EqualityBoundSelectivity < RangeSelectivity (RFC-164
// COST-SELECTIVITY); using the generic residual FilterSelectivity (0.5) for an
// equality bound inverted that and mis-picked the index. Empty/nil bounds are
// skipped.
//
// This is the SINGLE source for equality-vs-range bound costing, shared by
// RecordQueryScanPlan.HintCost, RecordQueryIndexPlan.HintCost, and
// scanLikeCost. It is centralised deliberately: the same loop duplicated
// across those sites is how the inverted equality cost survived (in dead
// code) past the original fix.
//
// LOW-NDV CAVEAT: statless, this assumes every equality is a high-cardinality
// point (NDV≈10). A low-NDV equality (e.g. `status = ?`, a boolean) actually
// retains far more than 10% — costing it cheaper than it is. Distinguishing
// that needs per-column NDV statistics (not yet available here); see
// scanLikeCost's fullBindUnique note.
func BoundSelectivity(comps []*predicates.ComparisonRange) (sel float64, numBound int, allEquality bool) {
	sel = 1.0
	allEquality = true
	for _, cr := range comps {
		if cr == nil || cr.IsEmpty() {
			continue
		}
		numBound++
		if cr.IsEquality() {
			sel *= EqualityBoundSelectivity
		} else {
			allEquality = false
			sel *= RangeSelectivity
		}
	}
	return sel, numBound, allEquality
}

// EqualityBoundsCoverKey reports whether the comparison prefix proves that
// every column of a key with keyColumnCount columns is equality-bound.
//
// Scan comparisons contain only the bound prefix, so "every comparison that
// happens to be present is an equality" does not prove full coverage of a
// composite key. Unknown key arity (keyColumnCount <= 0), nil/empty gaps, and
// ranges all fail closed. More comparisons than the key arity are tolerated
// only when every one is a non-empty equality; primary-scan construction should
// not normally produce that shape, but full key coverage still holds.
func EqualityBoundsCoverKey(comps []*predicates.ComparisonRange, keyColumnCount int) bool {
	if keyColumnCount <= 0 {
		return false
	}
	_, numBound, allEquality := BoundSelectivity(comps)
	return allEquality && numBound >= keyColumnCount && numBound == len(comps)
}

// AnyEqualityWidensBeyondOneKey reports whether any equality in comps binds
// MORE THAN ONE physical key, so the comparisons do NOT pin a single index
// entry even when every column is equality-bound.
//
// The only such case today is a zero-valued FLOAT/DOUBLE equality. IEEE says
// -0.0 == +0.0, while FDB tuple encoding preserves the sign bit, so the two are
// distinct adjacent keys and the executor widens a zero bound to span both.
//
// This is the SINGLE home for that exception. It previously lived nowhere and
// four independent point-probe proofs each concluded "unique + all equalities =
// exactly one row" on their own — computeCardinalities, isProvablePointProbe,
// indexProvableMaxCard and scanLikeCost — so fixing one left the same plan with
// contradictory cardinality and the cost model still ranking on the false bound.
//
// Conservative on purpose for a NON-CONSTANT float operand: its runtime value is
// unknown, and if it binds to zero the scan widens. Callers that care about
// SARGABILITY rather than a proof want the stricter constant-only test instead
// (see match_candidate_index), because being conservative there de-sargs a probe
// into a full leading-column scan rather than merely costing a sort.
func AnyEqualityWidensBeyondOneKey(comps []*predicates.ComparisonRange) bool {
	for _, cr := range comps {
		if cr == nil || !cr.IsEquality() {
			continue
		}
		cmp := cr.GetEqualityComparison()
		if cmp == nil || cmp.Operand == nil {
			continue
		}
		if !values.IsConstantValue(cmp.Operand) {
			// Unknown at plan time — treat a declared float as possibly zero.
			if t := cmp.Operand.Type(); t != nil &&
				(t.Code() == values.TypeCodeFloat || t.Code() == values.TypeCodeDouble) {
				return true
			}
			continue
		}
		v, ok := values.EvaluateConstant(cmp.Operand)
		if !ok || v == nil {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n == 0 {
				return true
			}
		case float32:
			if n == 0 {
				return true
			}
		}
	}
	return false
}
