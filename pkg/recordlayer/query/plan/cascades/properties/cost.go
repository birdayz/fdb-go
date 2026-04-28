// Package properties is the seed of Cascades' per-RelationalExpression
// derived-property machinery — the "decision support" the planner uses
// to pick a single best plan from an equivalence class of equivalent
// expressions.
//
// Track B4 (RFC-022 §4.4) — cost model. Per RFC-024 the Go cost model
// is Go-native (NOT hash-identical to Java) — the goal is "pick a
// sensible cheaper plan among the rule-generated alternatives", not
// "match Java's plan-cache key bit-for-bit". Java's `properties/`
// package has ~25 classes; the seed here implements one — Cost — that
// covers cardinality + a per-operator CPU heuristic. Future shifts
// add IntervalsProperty / OrderingProperty / DistinctRecordsProperty /
// etc. as Batch A index rules need them.
//
// Design choices captured:
//
//   - Cost is computed on demand by walking the expression tree, NOT
//     attached to expressions. Java caches cost on the equivalence
//     class; the seed re-computes per call. Memoisation lands when a
//     bench shows it as a bottleneck.
//
//   - Sub-Reference recursion picks the FIRST member's cost, not the
//     cheapest. Two reasons: (a) recursion through the cheapest is
//     well-defined for a DAG but exponential without memoisation, and
//     the seed prefers correctness-first-perf-later; (b) FixpointApply
//     fires every rule on every Reference until convergence, so by the
//     time we cost anything every Reference's first member is the
//     original input — we cost the unoptimised sub-tree consistently
//     across siblings, which is what "compare members at THIS Reference"
//     wants. The full Memo + cost-driven extraction (B6) replaces this
//     with proper memoisation.
//
//   - Tunable constants are package-level. Re-tune as B6 + Batch A land.
//     Calibration target: a Filter under a Sort should beat a Sort
//     under a Filter (push-Filter-through-Sort = cheaper); a Distinct
//     directly over a Sort should beat a Distinct over an unsorted
//     scan that would need its own sort (DistinctOverSortElim picks
//     the no-sort path); Union of cheap children should beat
//     Intersection (the latter scans every child end-to-end).
package properties

import (
	"math"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// Tunable constants. Calibrated to give plausible orderings for the
// 31-rule seed; not measured against real workloads.
const (
	// LeafScanCardinality is the cardinality estimate for a
	// FullUnorderedScan. Without table statistics the planner can't
	// know the row count; the seed uses a fixed large constant so
	// per-plan comparison still picks the plan with fewer / cheaper
	// post-scan operators.
	LeafScanCardinality = 1e6

	// FilterSelectivity is the fraction of rows a Filter retains by
	// default. 0.5 is a common heuristic when no per-predicate
	// selectivity is known. ProjectionMerge / DistinctOverSortElim
	// compare against this to pick rule outputs.
	FilterSelectivity = 0.5

	// DistinctSelectivity is the fraction of rows Distinct retains
	// (i.e. the assumed unique rate). 0.7 means 30% of input rows are
	// duplicates of an earlier row.
	DistinctSelectivity = 0.7

	// TypeFilterSelectivity is the fraction of rows a TypeFilter
	// retains. 0.5 is a rough "two type universes" assumption.
	TypeFilterSelectivity = 0.5

	// Per-operator CPU constants. CPU is in row-equivalent units so
	// cardinality and CPU share a scale (Total = Cardinality + CPU).
	FilterCPU       = 0.1
	ProjectionCPU   = 0.05
	SortCPU         = 1.0 // multiplied by N * log2(N) for Nlog(N) sort
	DistinctCPU     = 1.5
	TypeFilterCPU   = 0.05
	UnionCPU        = 0.1
	IntersectionCPU = 1.0
	SelectCPU       = 0.5
	WriteCPU        = 1.0
)

// Cost is a Go-native heuristic cost: a cardinality estimate plus a
// CPU estimate. Lower is cheaper.
//
// Both axes are in "logical row" units so Total = Cardinality + CPU
// is well-formed. The heuristic does NOT model wall-clock time and
// does NOT match Java's cost model bit-for-bit — per RFC-024 we're
// free to pick a Go-native shape.
type Cost struct {
	// Cardinality is the estimated number of rows this expression
	// emits when executed.
	Cardinality float64

	// CPU is the estimated row-equivalent CPU work to produce those
	// rows, summed over the sub-tree (children's CPU is rolled up
	// into parents).
	CPU float64
}

// Total returns the comparator-friendly total cost as
// Cardinality + CPU.
func (c Cost) Total() float64 { return c.Cardinality + c.CPU }

// Less reports whether c is strictly cheaper than other under the
// current cost model. Tie-break on cardinality alone (smaller wins on
// equal Total) so a deterministic ordering exists for fuzz-pinned
// cost monotonicity.
func (c Cost) Less(other Cost) bool {
	ct := c.Total()
	ot := other.Total()
	if ct != ot {
		return ct < ot
	}
	return c.Cardinality < other.Cardinality
}

// EstimateCost walks the expression tree rooted at e and returns the
// aggregated Cost estimate. Sub-Reference recursion picks the first
// member's cost (see package doc for rationale).
//
// Returns the zero Cost on nil input.
//
// Cost is rolled UP from children: a parent's CPU includes each
// child's CPU plus the parent's own work. Cardinality is the parent
// operator's own emit count (NOT a sum of children unless the operator
// produces a union of child rows).
func EstimateCost(e expressions.RelationalExpression) Cost {
	return estimateCostMemoised(e, nil)
}

// estimateCostMemoised is the workhorse: with `memo == nil` it
// behaves exactly like EstimateCost (no caching); with a non-nil
// memo it caches Reference-keyed costs to avoid re-walking shared
// sub-trees. BestRefCost passes a memo so multiple members sharing
// inner Reference children pay the recursion once.
func estimateCostMemoised(e expressions.RelationalExpression, memo map[*expressions.Reference]Cost) Cost {
	if e == nil {
		return Cost{}
	}
	qs := e.GetQuantifiers()
	childCosts := make([]Cost, len(qs))
	for i, q := range qs {
		childCosts[i] = firstMemberCostMemoised(q.GetRangesOver(), memo)
	}
	return localCost(e, childCosts)
}

// firstMemberCost returns the cost of the first member of `ref`.
// Returns Cost{Cardinality: LeafScanCardinality} if `ref` is nil or
// empty (defensive — represents "unknown sub-tree"). See package doc
// for why we use first-member rather than best-member here.
//
// Exposed for use by callers that need the unmemoised path.
func firstMemberCost(ref *expressions.Reference) Cost {
	return firstMemberCostMemoised(ref, nil)
}

func firstMemberCostMemoised(ref *expressions.Reference, memo map[*expressions.Reference]Cost) Cost {
	if ref == nil {
		return Cost{Cardinality: LeafScanCardinality}
	}
	if memo != nil {
		if c, ok := memo[ref]; ok {
			return c
		}
	}
	members := ref.Members()
	if len(members) == 0 {
		c := Cost{Cardinality: LeafScanCardinality}
		if memo != nil {
			memo[ref] = c
		}
		return c
	}
	c := estimateCostMemoised(members[0], memo)
	if memo != nil {
		memo[ref] = c
	}
	return c
}

// BestRefCost returns the cheapest member's cost in `ref`. Used by
// Reference.GetBest extraction. Equivalent to walking every member
// and picking the minimum by Cost.Less.
//
// Memoisation: builds a per-call `map[*Reference]Cost` so child
// References shared across multiple members are walked only once.
// For a Reference with N members each over a shared K-deep tree,
// memoised cost is O(N+K) vs the un-memoised O(N*K).
//
// Returns the zero Cost if `ref` is nil or empty.
func BestRefCost(ref *expressions.Reference) Cost {
	if ref == nil {
		return Cost{}
	}
	members := ref.Members()
	if len(members) == 0 {
		return Cost{}
	}
	memo := make(map[*expressions.Reference]Cost)
	best := estimateCostMemoised(members[0], memo)
	for _, m := range members[1:] {
		c := estimateCostMemoised(m, memo)
		if c.Less(best) {
			best = c
		}
	}
	return best
}

// localCost computes the cost of expression e given precomputed child
// costs. Switch-on-type rather than virtual dispatch — keeps the cost
// formulas in one place where a calibration shift can adjust them all
// together. Adding a new RelationalExpression concrete type requires
// adding a switch arm here; the default branch falls back to a
// pessimistic estimate so unknown operators don't crash the planner.
func localCost(e expressions.RelationalExpression, child []Cost) Cost {
	sumCPU := 0.0
	for _, c := range child {
		sumCPU += c.CPU
	}

	switch ex := e.(type) {

	case *expressions.FullUnorderedScanExpression:
		_ = ex
		return Cost{Cardinality: LeafScanCardinality, CPU: 0}

	case *expressions.LogicalFilterExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		// One unit of selectivity per predicate (capped at the
		// per-Filter floor). Two ANDed predicates cut more than one;
		// the per-predicate exponent matches Java's heuristic.
		numPreds := len(ex.GetPredicates())
		if numPreds == 0 {
			numPreds = 1
		}
		sel := math.Pow(FilterSelectivity, float64(numPreds))
		return Cost{
			Cardinality: in * sel,
			CPU:         sumCPU + in*FilterCPU*float64(numPreds),
		}

	case *expressions.LogicalProjectionExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		return Cost{Cardinality: in, CPU: sumCPU + in*ProjectionCPU}

	case *expressions.LogicalSortExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		// N log N sort work plus child's own CPU. Empty-key Sort still
		// costs N (the operator visits every row), but with log2(1)=0
		// would otherwise drop to zero — clamp the log term to 1 so
		// the sort isn't free for tiny inputs.
		logN := math.Max(1, math.Log2(math.Max(2, in)))
		return Cost{Cardinality: in, CPU: sumCPU + in*SortCPU*logN}

	case *expressions.LogicalDistinctExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		return Cost{
			Cardinality: in * DistinctSelectivity,
			CPU:         sumCPU + in*DistinctCPU,
		}

	case *expressions.LogicalTypeFilterExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		return Cost{
			Cardinality: in * TypeFilterSelectivity,
			CPU:         sumCPU + in*TypeFilterCPU,
		}

	case *expressions.LogicalUnionExpression:
		// Union concatenates children — output cardinality is the sum;
		// CPU is each child's CPU plus a small per-output-row penalty
		// for the merge work.
		sumCard := 0.0
		for _, c := range child {
			sumCard += c.Cardinality
		}
		return Cost{Cardinality: sumCard, CPU: sumCPU + sumCard*UnionCPU}

	case *expressions.LogicalIntersectionExpression:
		if len(child) == 0 {
			return Cost{}
		}
		// Intersection output cardinality is bounded by the smallest
		// child (every output row must be present in every input).
		// Work is roughly proportional to scanning every child end-to-
		// end (sumCard) — even the pruning cases require touching
		// each input row at least once.
		minCard := child[0].Cardinality
		sumCard := child[0].Cardinality
		for _, c := range child[1:] {
			if c.Cardinality < minCard {
				minCard = c.Cardinality
			}
			sumCard += c.Cardinality
		}
		return Cost{Cardinality: minCard, CPU: sumCPU + sumCard*IntersectionCPU}

	case *expressions.SelectExpression:
		if len(child) == 0 {
			return Cost{}
		}
		// Cross-product of children (FROM-list semantic), then filter
		// selectivity per WHERE predicate. SELECT is the only seed
		// expression with > 1 child whose cardinality multiplies.
		product := 1.0
		for _, c := range child {
			product *= c.Cardinality
		}
		numPreds := len(ex.GetPredicates())
		sel := 1.0
		if numPreds > 0 {
			sel = math.Pow(FilterSelectivity, float64(numPreds))
		}
		out := product * sel
		return Cost{Cardinality: out, CPU: sumCPU + product*SelectCPU}

	case *expressions.InsertExpression, *expressions.UpdateExpression, *expressions.DeleteExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		// DML emits the same number of rows it consumed (one effect
		// per input row); CPU dominated by the per-row write.
		return Cost{Cardinality: in, CPU: sumCPU + in*WriteCPU}

	default:
		// Unknown operator — pessimistic. Drives the planner toward
		// known-cost shapes when alternatives exist. Safe because the
		// default doesn't underestimate any concrete operator.
		in := LeafScanCardinality
		if len(child) > 0 {
			in = child[0].Cardinality
		}
		return Cost{Cardinality: in, CPU: sumCPU + in}
	}
}

// CostLess returns a comparator function `less(a, b RelationalExpression) bool`
// driven by EstimateCost. Suitable as the Reference.GetBest argument.
//
// Stable across calls (no internal state). The closure captures
// nothing — exposed as a function so future callers can swap in a
// different cost model without changing the GetBest signature.
func CostLess(a, b expressions.RelationalExpression) bool {
	return EstimateCost(a).Less(EstimateCost(b))
}
