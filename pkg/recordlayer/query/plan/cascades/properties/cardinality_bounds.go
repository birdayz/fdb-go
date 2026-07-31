package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// RFC-195: the cost model must not contradict what the property layer proves.
//
// Two independent row-count derivations live in this planner. CardinalitiesProperty
// (a port of Java's) PROVES a structural [min, max] interval per plan shape. The
// cost model ESTIMATES a point from flat selectivity constants. An estimate is
// free to be wrong — that is what an estimate is — but it is never free to be
// IMPOSSIBLE: below a proven minimum or above a proven maximum is wrong by
// construction, with no statistics needed to know it.
//
// This file holds the three pieces that make the estimate respect the proof:
//
//   - ClampCardinality, the one-line contract, applied at each cost walk's
//     combine step.
//   - CardinalityProver, the interface physical plans implement to state their
//     own proof beside their own cost formula (the CostHinter precedent
//     exactly: properties declares, plans implement).
//   - provenCardinalities, the dispatch — typed arms for LOGICAL expressions,
//     interface dispatch for physical plans. Logical expressions cannot
//     implement CardinalityProver: `expressions` sits BELOW `properties`, and
//     the method signature names Cardinalities, so implementing it would invert
//     the layering. That is the same switch-for-logical / interface-for-physical
//     duality localCost already has, applied to bounds — two arms, not two
//     homes, and the pair is pinned identical by a logical/physical parity test.
//
// The clamp is SYMMETRIC across the logical/physical pair by construction: it
// runs at localCost's dispatch, after the switch, for both kinds of subject
// alike. Clamping only the physical side would floor
// RecordQueryDistinctPlan's estimate to 1 while LogicalDistinctExpression kept
// 0.7 — inverting localCost's contract that a physical wrapper hints a cost
// equivalent to or LOWER than its logical operator, which is what makes
// cost-driven extraction prefer the physical plan.

// ClampCardinality returns estimate constrained to the interval bounds proves,
// clamping per bound and ONLY where that bound is known.
//
// Zero is preserved deliberately. The floor applies only when the proof says
// min >= 1, so a proven-zero or possibly-zero child keeps its zero and
// FlatMapCost's LIMIT-0 contract survives untouched: `SELECT * FROM t1, (SELECT
// * FROM t2 LIMIT 0)` must cost as the free empty join it is, not be floored
// into a phantom row that propagates multiplicatively through the join tree.
//
// An unknown bound constrains nothing — the property abstaining is not a claim
// that the estimate is wrong, so the estimate passes through untouched on that
// side. A NEGATIVE estimate is not a row count and is raised to zero even with
// no proven min: no formula in this package produces one, and letting it
// through would make a subtree's cost cancel a sibling's under a summing
// parent.
func ClampCardinality(estimate float64, bounds Cardinalities) float64 {
	if estimate < 0 {
		estimate = 0
	}
	if !bounds.Min.IsUnknown() {
		if lo := float64(bounds.Min.Value()); estimate < lo {
			estimate = lo
		}
	}
	if !bounds.Max.IsUnknown() {
		if hi := float64(bounds.Max.Value()); estimate > hi {
			estimate = hi
		}
	}
	return estimate
}

// ClampCost returns c with its Cardinality constrained to the proven interval.
// The CPU axis is untouched: every formula in this package derives CPU from the
// rows it CONSUMES (already-clamped child cardinalities) rather than from the
// rows it emits, so a clamp on the output leaves those terms consistent.
//
// The one operator whose CPU genuinely derives from its OWN OUTPUT cardinality
// implements BoundedCostHinter instead and clamps before computing that term —
// see that interface's doc comment for why applying the clamp afterwards would
// erase the very cost distinction the formula exists to draw.
func ClampCost(c Cost, bounds Cardinalities) Cost {
	c.Cardinality = ClampCardinality(c.Cardinality, bounds)
	return c
}

// CardinalityProver is the interface a physical plan implements to state the
// structural [min, max] interval it PROVES for its own output, given the
// intervals proved for its children.
//
// Defined here, implemented in the `plans` package beside each operator's cost
// formula — the exact CostHinter precedent. Keeping the proof next to the
// formula it constrains is the point: the six estimates RFC-195 corrects each
// accumulated because "this operator guarantees a row" lived in a different
// file from "this operator costs in*0.7", and nothing made the two agree.
//
// child carries one interval per child, in the operator's own child order.
// Implementations MUST tolerate a child slice of unexpected length (an
// unresolved or partially built edge) by returning UnknownCardinalities rather
// than indexing past it — an absent child is an absent proof, never a bound.
type CardinalityProver interface {
	ProvenCardinalities(child []Cardinalities) Cardinalities
}

// BoundedCostHinter is the optional interface an operator implements when a
// component of its Cost OTHER than Cardinality is a function of its own OUTPUT
// cardinality. Such a formula must consume the CLAMPED value, so it receives
// the proven interval and applies the clamp itself, before those terms are
// computed.
//
// Clamping such a Cost afterwards leaves the rest of it computed from the
// disproven number. The pinned reproducer is the recursive level union: its
// hint charges buffer CPU proportional to estimated output, so a one-row seed
// with a zero-estimated recursive leg pays ZERO buffer work, and an
// after-the-fact clamp would then assert the output is at least one row —
// erasing the level-union-vs-DFS cost distinction the buffer term exists to
// draw, in the name of fixing it.
//
// A Cost emitted through this path is already clamped, and ClampCost is
// idempotent, so a combine site may apply the shared clamp uniformly either
// way.
type BoundedCostHinter interface {
	// HintCostWithin returns this expression's cost given its children's
	// rolled-up costs, the interval its own ProvenCardinalities derived, and
	// the active statistics provider.
	HintCostWithin(childCosts []Cost, bounds Cardinalities, stats StatisticsProvider) Cost
}

// provenCardinalities derives the interval e proves for its own output from the
// intervals proved for its children.
//
// Physical plans answer through CardinalityProver. Logical expressions get the
// typed arms below, each pinned identical to its physical twin's method by the
// logical/physical parity test — that pin is what makes the clamp symmetric,
// and symmetry is what preserves the physical <= logical cost relation
// cost-driven extraction depends on.
//
// An expression this switch does not recognize proves nothing (unknown on both
// bounds), which makes the clamp a deterministic no-op for it. That is the
// correct failure direction: an unrecognized operator must not have bounds
// invented for it.
func provenCardinalities(e expressions.RelationalExpression, child []Cardinalities) Cardinalities {
	if e == nil {
		return UnknownCardinalities()
	}
	// Physical plans first: since RFC-184 W2 the memo holds plans directly, so
	// this is the common case and it must not be shadowed by a logical arm.
	if prover, ok := e.(CardinalityProver); ok {
		return prover.ProvenCardinalities(child)
	}
	return provenLogicalCardinalities(e, child)
}

// provenLogicalCardinalities holds the LOGICAL half of the derivation — the
// arms for expression types that sit below `properties` and therefore cannot
// implement CardinalityProver themselves.
//
// Every arm here mirrors a PHYSICAL twin's ProvenCardinalities exactly; the
// pairing is enumerated and enforced by the parity test, which fails on any arm
// with no table entry rather than passing vacuously the day a twelfth arm is
// added.
//
// Where an arm's answer differs from Java's CardinalitiesVisitor arm for the
// same logical type, it is because the arm is pinned to the GO PHYSICAL twin,
// which is the derivation that actually feeds the property map. Divergence from
// the twin is what inverts the extraction preference; divergence from Java's
// LOGICAL arm is invisible to the property map, which only ever visits plans.
func provenLogicalCardinalities(e expressions.RelationalExpression, child []Cardinalities) Cardinalities {
	only := func() Cardinalities {
		if len(child) != 1 {
			return UnknownCardinalities()
		}
		return child[0]
	}

	switch ex := e.(type) {

	// Twin: RecordQueryScanPlan with no full-primary-key equality bind. A
	// FullUnorderedScanExpression carries no comparisons at all, so it can
	// never take the bounded branch.
	case *expressions.FullUnorderedScanExpression:
		return UnknownMaxCardinality()

	// Twin: RecordQueryFilterPlan / RecordQueryPredicatesFilterPlan. A filter
	// may eliminate every row, so the minimum drops to zero while the maximum
	// is inherited.
	//
	// Java's visitLogicalFilterExpression returns the child interval UNCHANGED
	// and its visitRecordQueryPredicatesFilterPlan applies floor(0L), which is a
	// no-op for any non-negative bound — so Java claims a filter over an
	// exactly-one-row child still emits at least one row. Go's physical arm
	// already dropped the minimum to zero (the sound answer), and this arm
	// matches the Go twin so the pair stays symmetric.
	case *expressions.LogicalFilterExpression:
		return Cardinalities{Min: OfCardinality(0), Max: only().GetMaxCardinality()}

	// Twin: RecordQueryProjectionPlan — cardinality-preserving.
	case *expressions.LogicalProjectionExpression:
		return only()

	// Twin: RecordQueryInMemorySortPlan — a sort reorders, it does not filter.
	case *expressions.LogicalSortExpression:
		return only()

	// Twin: RecordQueryDistinctPlan — the current model conservatively
	// preserves the child's bounds rather than claiming a dedup ratio.
	case *expressions.LogicalDistinctExpression:
		return only()

	// Twin: RecordQueryTypeFilterPlan — transparent, matching Java's
	// CardinalitiesVisitor.
	case *expressions.LogicalTypeFilterExpression:
		return only()

	// Twin: RecordQueryUnionPlan / MergeSortUnion / UnorderedUnion.
	case *expressions.LogicalUnionExpression:
		return UnionCardinalities(child)

	// Twin: RecordQueryIntersectionPlan.
	case *expressions.LogicalIntersectionExpression:
		return IntersectCardinalities(child)

	// Twin: RecordQueryFlatMapPlan / RecordQueryNestedLoopJoinPlan — the
	// cross-product of the legs, exactly as Java's visitSelectExpression reduces
	// times() over its quantifiers.
	//
	// The predicates deliberately do NOT drop the minimum here, matching both
	// Java and the Go join arms. A one-quantifier SelectExpression is the shape
	// whose physical realization is a filter, whose arm DOES drop the minimum;
	// that asymmetry is safe in the one direction it can act — the physical
	// minimum is the weaker of the two, so the clamp can only floor the LOGICAL
	// side higher, which preserves physical <= logical rather than inverting it.
	case *expressions.SelectExpression:
		if len(child) == 0 {
			return UnknownCardinalities()
		}
		out := child[0]
		for _, c := range child[1:] {
			out = out.Times(c)
		}
		return out

	// Twin: RecordQueryStreamingAggregationPlan. An ungrouped aggregation emits
	// at most one row; a grouped one has an unknown group count.
	//
	// Java's visitGroupByExpression proves EXACTLY one for the ungrouped case.
	// Go's physical twin proves at-most-one, and this arm follows the twin: a
	// disagreement on the minimum is exactly what makes a clamp asymmetric.
	case *expressions.GroupByExpression:
		if len(ex.GetGroupingKeys()) == 0 {
			return Cardinalities{Min: OfCardinality(0), Max: OfCardinality(1)}
		}
		return UnknownMaxCardinality()

	// Twin: RecordQueryInsertPlan / RecordQueryDeletePlan / RecordQueryUpdatePlan
	// — one effect per consumed row.
	case *expressions.InsertExpression, *expressions.UpdateExpression, *expressions.DeleteExpression:
		return only()

	default:
		return UnknownCardinalities()
	}
}

// ---------------------------------------------------------------------------
// The bounds walk: order-independent composition over ALL members
// ---------------------------------------------------------------------------

// boundsWalk derives proven intervals over an expression tree, memoised per
// Reference.
//
// How child BOUNDS are obtained differs from how child COSTS are obtained, and
// the difference is load-bearing. The cost recursion resolves a child Reference
// to its FIRST member (firstMemberCostMemoised); for cost that arbitrariness is
// accepted and long-standing. For BOUNDS it would be TIGHTER than the property
// layer's own answer: cardinalitiesForRef deliberately WEAKENS across all
// members, so taking members[0]'s bounds instead would let a group whose first
// member is a unique probe (max=1) and whose second is a full scan clamp its
// parent to a 1-vs-1e6 cliff decided by MEMBER INSERTION ORDER — arrival-order
// dependence re-entering through the bounds channel, and invisible to a
// priming-invariance test because priming does not reorder members.
//
// So bounds compose by WeakenCardinalities over AllMembers(), recursively:
// order-independent by construction, and pinned by a member-order-permutation
// test that the first-member design would have failed.
//
// Weakening only LOOSENS as exploration inserts members, so a clamped cost can
// move toward the unclamped estimate while planning runs. That is accepted
// explicitly: it is deterministic (the task schedule is deterministic, so the
// same growth points produce the same comparisons on every run) and it is the
// same time-variance the property maps already have under priming. What must
// never happen is that mid-flight movement leaking into what a winner shipped
// with, which the growth face of the permutation test pins.
type boundsWalk struct {
	memo    map[*expressions.Reference]Cardinalities
	visited map[*expressions.Reference]bool
}

// newBoundsWalk allocates a bounds walk. The memo is allocated UNCONDITIONALLY,
// including on the path where EstimateCostWith deliberately passes a nil COST
// memo: that entry point runs twice per winner comparison, and all-members
// recursion without a memo would expand the memo DAG combinatorially on the
// planner's hottest path.
func newBoundsWalk() *boundsWalk {
	return &boundsWalk{
		memo:    make(map[*expressions.Reference]Cardinalities),
		visited: make(map[*expressions.Reference]bool),
	}
}

// exprBounds derives the interval e proves, recursing into each child Reference
// at its WEAKENED all-members interval.
func (w *boundsWalk) exprBounds(e expressions.RelationalExpression) Cardinalities {
	if e == nil {
		return UnknownCardinalities()
	}
	qs := e.GetQuantifiers()
	child := make([]Cardinalities, len(qs))
	for i, q := range qs {
		child[i] = w.refBounds(q.GetRangesOver())
	}
	return provenCardinalities(e, child)
}

// refBounds returns ref's interval weakened across ALL members, memoised.
//
// The weakening is the property layer's own answer (cardinalitiesForRef), reached
// through the existing AllMembers() accessor rather than an open-coded union, so
// a group with both bounded and unbounded members correctly relaxes to the
// least-constraining bound instead of being second-guessed.
//
// The recursion-stack guard is defensive — the child-Reference DAG is acyclic
// (memo_merge.go's reachable/mergeable forbid cycle-creating merges) — and a
// broken cycle yields unknown WITHOUT memoising, since a partial result must
// never be cached as final.
func (w *boundsWalk) refBounds(ref *expressions.Reference) Cardinalities {
	if ref == nil {
		return UnknownCardinalities()
	}
	if c, ok := w.memo[ref]; ok {
		return c
	}
	if w.visited[ref] {
		return UnknownCardinalities()
	}
	members := ref.AllMembers()
	if len(members) == 0 {
		c := UnknownCardinalities()
		w.memo[ref] = c
		return c
	}
	w.visited[ref] = true
	items := make([]Cardinalities, 0, len(members))
	for _, m := range members {
		items = append(items, w.exprBounds(m))
	}
	delete(w.visited, ref)
	out := WeakenCardinalities(items)
	w.memo[ref] = out
	return out
}

// ProvenCardinalitiesOf derives the interval e proves, composing child bounds
// order-independently over all members of each child Reference.
//
// Exposed for the walks that live outside this package (the concrete
// join-ordering cost and partition ranking) and for the tests that pin
// order-independence and logical/physical parity.
func ProvenCardinalitiesOf(e expressions.RelationalExpression) Cardinalities {
	return newBoundsWalk().exprBounds(e)
}

// ProvenCardinalitiesFrom derives the interval e proves from EXPLICIT child
// intervals, bypassing the walk entirely.
//
// This is the method-level agreement point: it is the same dispatch
// provenCardinalities performs, so a test can assert that a plan's own
// ProvenCardinalities method and a logical twin's arm produce the identical
// interval FROM THE SAME CHILD INTERVALS. Asserting agreement on whole-walk
// outputs instead would be self-refuting — the property map weakens child
// bounds across all members while the concrete walk uses the concrete child, so
// their outputs legitimately differ. The invariant is that no one re-forks the
// per-operator derivation, not that every walk sees the same tree.
func ProvenCardinalitiesFrom(e expressions.RelationalExpression, child []Cardinalities) Cardinalities {
	return provenCardinalities(e, child)
}
