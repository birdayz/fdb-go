// Package properties — CardinalityProperty file.
//
// Cardinality is the estimated number of rows a RelationalExpression
// produces. Today it's a field on `Cost` (computed alongside CPU in
// the same EstimateCost walk); CardinalityProperty exposes the same
// number through a dedicated accessor so callers that ONLY care
// about row count don't need to walk the CPU calculation too.
//
// Java's `properties/` package has CardinalitiesProperty as a
// separate class (with min/max bounds; we expose the point estimate
// only). Going forward, when CardinalityProperty diverges from Cost
// (e.g. Cost adds I/O distinct from row-count, or Cardinality gains
// min/max bounds), the two should split into distinct walks. For
// now they share the underlying computation.

package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// EstimateCardinality returns just the cardinality (row count
// estimate) of an expression — wraps the Cost-walk and projects out
// the CPU axis. Useful for cardinality-aware rules / matchers that
// don't need to drag the CPU calculation through.
//
// O(N) over the expression sub-tree (same complexity as
// EstimateCost). Sub-Reference recursion picks the first member,
// matching EstimateCost's policy.
func EstimateCardinality(e expressions.RelationalExpression) float64 {
	return EstimateCost(e).Cardinality
}

// EstimateCardinalityWith uses a custom StatisticsProvider for
// per-record-type cardinality (e.g. via Catalog lookups). Wraps
// EstimateCostWith.
func EstimateCardinalityWith(e expressions.RelationalExpression, stats StatisticsProvider) float64 {
	return EstimateCostWith(e, stats).Cardinality
}

// BestRefCardinality returns the cardinality of the cheapest
// member in a Reference — wraps BestRefCost.
func BestRefCardinality(ref *expressions.Reference) float64 {
	return BestRefCost(ref).Cardinality
}

// CardinalityLess is a Reference.GetBest-compatible comparator that
// orders members by cardinality alone (ignoring CPU). Useful when
// the planner wants to pick the smallest-output member regardless
// of compute cost — e.g. picking the join-build side for a hash
// join.
//
// Ties are broken by member identity (first-appearance wins) — same
// stability contract as CostLess.
func CardinalityLess(a, b expressions.RelationalExpression) bool {
	return EstimateCardinality(a) < EstimateCardinality(b)
}
