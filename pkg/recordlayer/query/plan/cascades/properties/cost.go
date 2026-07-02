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
//     the seed prefers correctness-first-perf-later; (b) exploration
//     rules only ADD members — Reference.Insert appends, the original
//     input expression stays at index 0 — so this costs the
//     unoptimised sub-tree consistently across siblings, which is what
//     "compare members at THIS Reference" wants. Caveat: index 0 is
//     stable under rule yields but NOT under RFC-037 cross-group memo
//     merges, which re-point the canonical member list — see
//     FuzzCostSanity for the consequence (first-member cost of an
//     unchanged parent can shift after a merge). The planner's winner
//     stamping + selector-driven extraction
//     (ExtractBestPlanFromSelector) is the production path that avoids
//     re-costing.
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

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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

	// RangeSelectivity is the fraction of rows a range predicate
	// (col > X, col < Y, BETWEEN) retains. 0.33 assumes roughly
	// one-third of the value domain is selected.
	RangeSelectivity = 0.33

	// EqualityBoundSelectivity is the fraction of rows an EQUALITY index-scan
	// bound (col = X) retains. A point match selects far fewer rows than a range,
	// so this MUST be < RangeSelectivity — otherwise the cost model treats an
	// equality probe as less selective than a range probe and picks the wrong
	// index (RFC-164 COST-SELECTIVITY). It is deliberately distinct from
	// FilterSelectivity (0.5, the generic residual-Filter heuristic): a residual
	// filter of unknown form may keep half the rows, but an indexed equality bound
	// is a point lookup. The invariant EqualityBoundSelectivity < RangeSelectivity
	// is pinned by TestCostSelectivity_EqualityBeatsRange.
	EqualityBoundSelectivity = 0.1

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
	SortCPU         = 0.15 // multiplied by N * log2(N) for Nlog(N) sort
	DistinctCPU     = 1.5
	TypeFilterCPU   = 0.05
	UnionCPU        = 0.1
	IntersectionCPU = 1.0
	SelectCPU       = 0.5
	WriteCPU        = 1.0
	FetchCPU        = 1.5  // per-row base-record fetch via PK lookup (random I/O)
	StreamingAggCPU = 1.2  // cheaper than DistinctCPU (no hash table, pre-sorted input)
	ScanCPU         = 0.05 // per-row sequential I/O cost for full table/index scans (kept < FilterCPU so a bare scan is never costed as a filtered scan)

	// IterationOverhead is the fixed per-outer-row cost a dependent (FlatMap)
	// join pays to RE-EXECUTE its inner once per outer row: open the inner
	// cursor, initialize the range read, and bind the correlation. A 200-row
	// driver pays 10x the per-iteration overhead of a 20-row driver, so this term
	// expresses the genuine "drive the nested loop from the SMALLER side"
	// preference among two full-scan drivers — the case where Java's
	// CardinalitiesProperty (Go's criterion #2) ABSTAINS because both roots are
	// unbounded/unknown. It is a Go-only STATS read-side extension (RFC-041/042),
	// NOT a Java port: it lives only inside the Go-only compareJoinOrdering cost
	// path (flatMapCost), never in criterion #2. Sized as a TIE-BREAKER — small
	// enough never to flip a clear cardinality/total-cost winner, large enough to
	// tip a genuinely-close join-order tie toward fewer inner re-executions
	// (Graefe). 0.1 == FilterCPU: one extra row-equivalent of setup per iteration.
	IterationOverhead = 0.1
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

// StatisticsProvider is the planner's hook for table-level cardinality
// statistics. Returns the estimated row count for a given record type
// name. Implementations:
//
//   - DefaultStatistics — every record type returns LeafScanCardinality.
//     Used when no real stats are available.
//   - Catalog-backed (future) — reads `COUNT` index counters from the
//     RecordMetaData for the queried record store.
//   - Test stubs — return programmable per-name values to drive
//     calibration tests.
//
// Implementations must be safe for concurrent calls (the planner may
// query the same provider from multiple goroutines).
type StatisticsProvider interface {
	// RecordTypeCardinality returns the estimated row count for
	// `recordTypeName`. Returns LeafScanCardinality if the record
	// type is unknown — implementations should NOT return zero or
	// negative values.
	RecordTypeCardinality(recordTypeName string) float64
}

// DefaultStatistics returns LeafScanCardinality for every record type.
// Use this as a baseline when no real statistics are available.
type DefaultStatistics struct{}

// RecordTypeCardinality always returns LeafScanCardinality.
func (DefaultStatistics) RecordTypeCardinality(_ string) float64 {
	return LeafScanCardinality
}

// HasRealStats reports whether the statistics provider has real
// per-type cardinalities (not just the default LeafScanCardinality).
func HasRealStats(stats StatisticsProvider) bool {
	if stats == nil {
		return false
	}
	_, isDefault := stats.(DefaultStatistics)
	return !isDefault
}

// FixedStatistics returns a fixed cardinality for every record type.
// Useful in tests that want a non-default scan cost.
type FixedStatistics struct {
	Cardinality float64
}

// RecordTypeCardinality returns the configured fixed value.
func (s FixedStatistics) RecordTypeCardinality(_ string) float64 {
	return s.Cardinality
}

// MapStatistics returns per-record-type cardinalities from a map,
// falling back to a default for unknown names.
//
// Fallback semantics: zero or negative means "use LeafScanCardinality
// (the package default)"; any positive value is used as-is. A
// caller who wants 0 as the actual fallback should use FixedStatistics{0}
// instead — MapStatistics treats 0 as "no fallback configured" to
// avoid silent zero-cardinality bugs (a zero scan cardinality would
// make every plan score zero and break cost-driven extraction).
type MapStatistics struct {
	// PerType maps record-type name → estimated cardinality.
	PerType map[string]float64
	// Fallback returned for names not present in PerType. ≤ 0
	// means "no fallback configured" — the package default
	// (LeafScanCardinality) is substituted.
	Fallback float64
}

// RecordTypeCardinality returns PerType[name] if present and > 0,
// otherwise Fallback when > 0, otherwise LeafScanCardinality.
// Zero counts are clamped to 1 to avoid zero-cost plans that break
// cost-driven plan selection.
func (s MapStatistics) RecordTypeCardinality(name string) float64 {
	if c, ok := s.PerType[name]; ok {
		if c > 0 {
			return c
		}
		return 1
	}
	if s.Fallback > 0 {
		return s.Fallback
	}
	return LeafScanCardinality
}

// EstimateCost walks the expression tree rooted at e and returns the
// aggregated Cost estimate using DefaultStatistics. Equivalent to
// EstimateCostWith(e, DefaultStatistics{}).
//
// Sub-Reference recursion picks the first member's cost (see package
// doc for rationale).
//
// Returns the zero Cost on nil input.
//
// Cost is rolled UP from children: a parent's CPU includes each
// child's CPU plus the parent's own work. Cardinality is the parent
// operator's own emit count (NOT a sum of children unless the operator
// produces a union of child rows).
func EstimateCost(e expressions.RelationalExpression) Cost {
	return EstimateCostWith(e, DefaultStatistics{})
}

// EstimateCostWith is EstimateCost driven by the provided
// StatisticsProvider. Scan operators query the provider for
// per-record-type cardinality; all other operators use the default
// per-operator constants.
//
// Sub-Reference recursion picks the FIRST member's cost (see package
// doc). This is the cost used by GetBest / winner extraction / stage
// advancement; keeping it first-member preserves the established
// winner-selection behaviour. The cost-optimal multi-way join decision
// uses the recursive best-member walk (BestMemberCostWith) instead —
// see RFC-041.
//
// Pass nil to use DefaultStatistics.
func EstimateCostWith(e expressions.RelationalExpression, stats StatisticsProvider) Cost {
	if stats == nil {
		stats = DefaultStatistics{}
	}
	return estimateCostMemoised(e, nil, stats)
}

// estimateCostMemoised is the workhorse: with `memo == nil` it
// behaves exactly like EstimateCost (no caching); with a non-nil
// memo it caches Reference-keyed costs to avoid re-walking shared
// sub-trees. BestRefCost passes a memo so multiple members sharing
// inner Reference children pay the recursion once.
//
// `stats` drives leaf-scan cardinality lookup; nil falls back to
// DefaultStatistics.
func estimateCostMemoised(e expressions.RelationalExpression, memo map[*expressions.Reference]Cost, stats StatisticsProvider) Cost {
	if e == nil {
		return Cost{}
	}
	if stats == nil {
		stats = DefaultStatistics{}
	}
	qs := e.GetQuantifiers()
	childCosts := make([]Cost, len(qs))
	for i, q := range qs {
		childCosts[i] = firstMemberCostMemoised(q.GetRangesOver(), memo, stats)
	}
	return localCost(e, childCosts, stats)
}

// firstMemberCost returns the cost of the first member of `ref`.
// Returns Cost{Cardinality: LeafScanCardinality} if `ref` is nil or
// empty (defensive — represents "unknown sub-tree"). See package doc
// for why we use first-member rather than best-member here.
//
// Exposed for use by callers that need the unmemoised path.
func firstMemberCost(ref *expressions.Reference) Cost {
	return firstMemberCostMemoised(ref, nil, DefaultStatistics{})
}

func firstMemberCostMemoised(ref *expressions.Reference, memo map[*expressions.Reference]Cost, stats StatisticsProvider) Cost {
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
	c := estimateCostMemoised(members[0], memo, stats)
	if memo != nil {
		memo[ref] = c
	}
	return c
}

// BestRefCost returns the cheapest member's cost in `ref` using
// DefaultStatistics. See BestRefCostWith for the stats-bound variant.
//
// Returns the zero Cost if `ref` is nil or empty.
func BestRefCost(ref *expressions.Reference) Cost {
	return BestRefCostWith(ref, DefaultStatistics{})
}

// BestRefCostWith returns the cheapest member's cost in `ref` under
// the given StatisticsProvider. The TOP Reference's members are ranked
// by best; their children recurse via first-member (see EstimateCostWith).
// Used by Reference.GetBest extraction.
//
// Memoisation: builds a per-call `map[*Reference]Cost` so child
// References shared across multiple members are walked only once.
//
// Pass nil for stats to use DefaultStatistics.
func BestRefCostWith(ref *expressions.Reference, stats StatisticsProvider) Cost {
	if ref == nil {
		return Cost{}
	}
	members := ref.Members()
	if len(members) == 0 {
		return Cost{}
	}
	if stats == nil {
		stats = DefaultStatistics{}
	}
	memo := make(map[*expressions.Reference]Cost)
	best := estimateCostMemoised(members[0], memo, stats)
	for _, m := range members[1:] {
		c := estimateCostMemoised(m, memo, stats)
		if c.Less(best) {
			best = c
		}
	}
	return best
}

// BestMemberCostWith costs expression e with FULLY RECURSIVE best-member
// sub-products: every child Reference (transitively) is costed at its
// cheapest member, not its first. This is Cascades' "combined cost with
// inputs" (§3.1) — a join's cost reflects each sub-product's WINNER, the
// proper-memoisation replacement the package doc flagged for B6. Used
// only by the multi-way join-order decision (RFC-041), kept separate
// from EstimateCostWith so winner extraction / stage advancement retain
// their established first-member behaviour.
//
// Per-call memoisation keeps a shared (e.g. union-find-merged)
// sub-product to one walk: O(N+K) not O(N*K). The visited recursion-
// stack guard is defensive — the child-Reference DAG is acyclic
// (memo_merge.go reachable/mergeable forbids cycle-creating merges).
//
// Pass nil to use DefaultStatistics.
func BestMemberCostWith(e expressions.RelationalExpression, stats StatisticsProvider) Cost {
	return newCostWalk(stats).exprCost(e)
}

// costWalk carries the per-call memo + recursion-stack guard for the
// best-member cost walk. The memo is per-call (never persisted), so it
// cannot outlive a union-find merge that mutates a survivor's member
// set — there is no cross-call staleness to invalidate.
type costWalk struct {
	memo    map[*expressions.Reference]Cost
	visited map[*expressions.Reference]bool
	stats   StatisticsProvider
}

func newCostWalk(stats StatisticsProvider) *costWalk {
	if stats == nil {
		stats = DefaultStatistics{}
	}
	return &costWalk{
		memo:    make(map[*expressions.Reference]Cost),
		visited: make(map[*expressions.Reference]bool),
		stats:   stats,
	}
}

// exprCost costs a specific expression, recursing into each child
// Reference at its best member (refCost). A shared sub-product reached
// through multiple parents within one walk is costed once (memo hit on
// refCost), so the walk is O(N+K), not O(N*K).
func (w *costWalk) exprCost(e expressions.RelationalExpression) Cost {
	if e == nil {
		return Cost{}
	}
	qs := e.GetQuantifiers()
	childCosts := make([]Cost, len(qs))
	for i, q := range qs {
		childCosts[i] = w.refCost(q.GetRangesOver())
	}
	return localCost(e, childCosts, w.stats)
}

// refCost returns the cost of ref's BEST member, memoised. Defensive
// cycle break returns the "unknown sub-tree" cost without memoising
// (a partial result must not be cached as final).
func (w *costWalk) refCost(ref *expressions.Reference) Cost {
	if ref == nil {
		return Cost{Cardinality: LeafScanCardinality}
	}
	if c, ok := w.memo[ref]; ok {
		return c
	}
	if w.visited[ref] {
		return Cost{Cardinality: LeafScanCardinality}
	}
	members := ref.Members()
	if len(members) == 0 {
		c := Cost{Cardinality: LeafScanCardinality}
		w.memo[ref] = c
		return c
	}
	w.visited[ref] = true
	best := w.exprCost(members[0])
	for _, m := range members[1:] {
		if c := w.exprCost(m); c.Less(best) {
			best = c
		}
	}
	delete(w.visited, ref)
	w.memo[ref] = best
	return best
}

// localCost computes the cost of expression e given precomputed child
// costs. Switch-on-type rather than virtual dispatch — keeps the cost
// formulas in one place where a calibration shift can adjust them all
// together. Adding a new RelationalExpression concrete type requires
// adding a switch arm here; the default branch falls back to a
// pessimistic estimate so unknown operators don't crash the planner.
//
// `stats` drives the leaf-scan cardinality. For unioned record types
// the cardinality is summed (Union semantics); single-type scans
// query directly. Empty record-type list (a "scan everything") falls
// back to LeafScanCardinality — caller's responsibility to attach
// metadata for that case.
func localCost(e expressions.RelationalExpression, child []Cost, stats StatisticsProvider) Cost {
	sumCPU := 0.0
	for _, c := range child {
		sumCPU += c.CPU
	}

	switch ex := e.(type) {

	case *expressions.FullUnorderedScanExpression:
		// A scan over multiple record types emits the SUM of their
		// per-type cardinalities. Empty list → LeafScanCardinality.
		// CPU = card·ScanCPU: reading N rows costs ~N (sequential I/O).
		// This is load-bearing for join ordering (RFC-041): a scan that
		// reported CPU=0 made the nested-loop join cost order-symmetric
		// (cost(A⋈B)==cost(B⋈A)), so the cost model could not pick the
		// drive-from-smaller order. With a per-row scan cost, NLJ's
		// "read outer once" term (outerCard·ScanCPU rolled up via the
		// outer child's CPU) makes driving from the smaller side cheaper.
		types := ex.GetRecordTypes()
		if len(types) == 0 {
			return Cost{Cardinality: LeafScanCardinality, CPU: LeafScanCardinality * ScanCPU}
		}
		total := 0.0
		for _, name := range types {
			total += stats.RecordTypeCardinality(name)
		}
		return Cost{Cardinality: total, CPU: total * ScanCPU}

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

	case *expressions.GroupByExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		// GroupBy output cardinality is estimated as DistinctSelectivity
		// of the input (same heuristic as Distinct — we don't know the
		// actual group count). CPU: scan all input rows + per-row
		// aggregate computation.
		return Cost{
			Cardinality: in * DistinctSelectivity,
			CPU:         sumCPU + in*DistinctCPU,
		}

	case *expressions.InsertExpression, *expressions.UpdateExpression, *expressions.DeleteExpression:
		if len(child) == 0 {
			return Cost{}
		}
		in := child[0].Cardinality
		// DML emits the same number of rows it consumed (one effect
		// per input row); CPU dominated by the per-row write.
		return Cost{Cardinality: in, CPU: sumCPU + in*WriteCPU}

	default:
		// Optional CostHinter override — opaque wrappers (e.g.
		// physical-plan adapters) can supply their own cost via
		// HintCost(child) without forcing properties.localCost to
		// know about every concrete type. Wrappers around physical
		// plans should hint a cost equivalent to or lower than the
		// corresponding logical operator so cost-driven extraction
		// prefers the physical plan.
		if hinter, ok := e.(CostHinter); ok {
			return hinter.HintCost(child, stats)
		}
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

// CostHinter is the optional interface a RelationalExpression
// implements to override the cost model's default-arm
// pessimistic estimate. Used by opaque wrappers (e.g. cascades-
// internal physical-plan adapters) to declare a more accurate cost
// — typically equal to or lower than the corresponding logical
// operator's cost so cost-driven extraction prefers the physical
// plan.
type CostHinter interface {
	// HintCost returns this expression's cost given its children's
	// rolled-up costs and the active statistics provider.
	// Implementations should aggregate sumCPU = sum(child.CPU) +
	// own-CPU and compute cardinality based on the operator's
	// selectivity / row-shape contract. Use stats for table-level
	// cardinality when available (scan/index wrappers); most
	// operators ignore stats and derive cardinality from children.
	HintCost(childCosts []Cost, stats StatisticsProvider) Cost
}

// CostLess returns a comparator function `less(a, b RelationalExpression) bool`
// driven by EstimateCost with DefaultStatistics. Suitable as the
// Reference.GetBest argument.
//
// Stable across calls (no internal state). The closure captures
// nothing — exposed as a function so future callers can swap in a
// different cost model without changing the GetBest signature.
func CostLess(a, b expressions.RelationalExpression) bool {
	return EstimateCost(a).Less(EstimateCost(b))
}

// CostLessWith returns a comparator function bound to the given
// StatisticsProvider. Use this when extracting plans against a
// known table-stats environment — Reference.GetBest receives a
// pure function, so the stats are baked into the closure.
//
// Pass nil for stats to use DefaultStatistics (equivalent to
// CostLess).
func CostLessWith(stats StatisticsProvider) func(a, b expressions.RelationalExpression) bool {
	if stats == nil {
		stats = DefaultStatistics{}
	}
	return func(a, b expressions.RelationalExpression) bool {
		return EstimateCostWith(a, stats).Less(EstimateCostWith(b, stats))
	}
}
