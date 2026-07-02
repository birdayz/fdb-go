package cascades

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// Single source of truth for per-operator physical cost formulas (RFC-069).
//
// Each function takes the already-rolled-up child Cost(s) and returns this
// operator's Cost. BOTH the physical-wrapper HintCost methods (which cost via
// the memo cost framework) and concretePlanCost (which costs the extracted
// RecordQueryPlan tree for the join-ordering criterion) call these — so a
// per-operator cost formula has exactly ONE definition and the two paths can
// never drift (RFC-069). Leaf scan/index cost is NOT here: the
// wrapper's leaf cost is metadata-aware (unique/covering) for the memo cost
// framework, while the concrete join-ordering cost uses a metadata-independent
// selectivity leaf cost (scanLikeCost) — those are deliberately different
// inputs to the same recursion, documented at each site.

// flatMapCost: a correlated dependent join re-runs the inner once per outer row.
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
// (Graefe, RFC-041/042).
func flatMapCost(outer, inner properties.Cost) properties.Cost {
	outerCard := outer.Cardinality
	if outerCard == 0 {
		outerCard = properties.LeafScanCardinality
	}
	innerCPU := inner.CPU
	if innerCPU == 0 {
		innerCPU = properties.FilterCPU
	}
	return properties.Cost{
		Cardinality: outerCard * properties.FilterSelectivity * physicalWrapperCostMultiplier,
		CPU:         (outer.CPU + outerCard*(innerCPU+properties.IterationOverhead)) * physicalWrapperCostMultiplier,
	}
}

// nestedLoopJoinCost: MATERIALIZED nested-loop join, outer × inner with per-pair filter.
//
// The inner subtree is executed ONCE: the executor (executor.go executeNestedLoopJoin)
// materializes the inner into a buffer via CollectAllBounded, then iterates the buffered
// rows per outer row. So the inner's own work (inner.CPU) is paid ONCE — NOT outerCard
// times. The per-pair term outerCard*innerCard*FilterCPU models iterating the buffer and
// evaluating the join predicate for every (outer,inner) pair (the in-memory work that DOES
// scale with the product).
//
// This materialization is what distinguishes the NLJ from a correlated FlatMap: flatMapCost
// charges outerCard*innerCPU because the FlatMap RE-EXECUTES (re-scans from FDB) its inner
// once per outer row, whereas the materialized NLJ scans the inner once and re-iterates the
// buffer. Charging the NLJ outerCard*inner.CPU (as if it re-scanned) erased that distinction
// and let a re-scan FlatMap tie/beat the materialized NLJ for a NON-PROBE inner — the
// RFC-152 preserved-only LEFT-OUTER regression. A card-1 PROBE inner keeps the FlatMap
// cheapest regardless (its outerCard*~1 work beats materialize+iterate). Graefe ACK.
func nestedLoopJoinCost(outer, inner properties.Cost) properties.Cost {
	outerCard, innerCard := outer.Cardinality, inner.Cardinality
	if outerCard == 0 {
		outerCard = properties.LeafScanCardinality
	}
	if innerCard == 0 {
		innerCard = properties.LeafScanCardinality
	}
	return properties.Cost{
		Cardinality: outerCard * innerCard * properties.FilterSelectivity * physicalWrapperCostMultiplier,
		CPU:         (outer.CPU + inner.CPU + outerCard*innerCard*properties.FilterCPU) * physicalWrapperCostMultiplier,
	}
}

// filterCost: one selectivity factor per predicate (min one).
func filterCost(child properties.Cost, numPreds int) properties.Cost {
	if numPreds == 0 {
		numPreds = 1
	}
	in := child.Cardinality
	sel := 1.0
	for i := 0; i < numPreds; i++ {
		sel *= properties.FilterSelectivity
	}
	return properties.Cost{
		Cardinality: in * sel * physicalWrapperCostMultiplier,
		CPU:         (child.CPU + in*properties.FilterCPU*float64(numPreds)) * physicalWrapperCostMultiplier,
	}
}

func typeFilterCost(child properties.Cost) properties.Cost {
	in := child.Cardinality
	return properties.Cost{
		Cardinality: in * properties.TypeFilterSelectivity * physicalWrapperCostMultiplier,
		CPU:         (child.CPU + in*properties.TypeFilterCPU) * physicalWrapperCostMultiplier,
	}
}

func fetchCost(child properties.Cost) properties.Cost {
	in := child.Cardinality
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child.CPU + in*properties.FetchCPU) * physicalWrapperCostMultiplier,
	}
}

func mapCost(child properties.Cost) properties.Cost {
	in := child.Cardinality
	return properties.Cost{
		Cardinality: in * physicalWrapperCostMultiplier,
		CPU:         (child.CPU + in*properties.ProjectionCPU) * physicalWrapperCostMultiplier,
	}
}

func firstOrDefaultCost(child properties.Cost) properties.Cost {
	return properties.Cost{Cardinality: 1 * physicalWrapperCostMultiplier, CPU: child.CPU * physicalWrapperCostMultiplier}
}

// inMemorySortCost: materialize + O(n log n). Note: NO physical-wrapper discount —
// an in-memory sort must stay strictly more expensive than index-based elimination.
func inMemorySortCost(child properties.Cost) properties.Cost {
	n := child.Cardinality
	if n < 1 {
		n = 1
	}
	logN := math.Max(1, math.Log2(math.Max(2, n)))
	return properties.Cost{Cardinality: n, CPU: child.CPU + n*properties.SortCPU*logN}
}

func distinctCost(child properties.Cost) properties.Cost {
	in := child.Cardinality
	return properties.Cost{
		Cardinality: in * properties.DistinctSelectivity * physicalWrapperCostMultiplier,
		CPU:         (child.CPU + in*properties.DistinctCPU) * physicalWrapperCostMultiplier,
	}
}

// intersectionCost: output bounded by the smallest child; work ~ scanning every
// child + per-output comparison-key merge. Carries the physical-wrapper discount
// like the other join-tree operators so the wrapper HintCost and the concrete
// join-ordering cost agree exactly.
func intersectionCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	minCard, sumCard, sumCPU := child[0].Cardinality, 0.0, 0.0
	for _, c := range child {
		if c.Cardinality < minCard {
			minCard = c.Cardinality
		}
		sumCard += c.Cardinality
		sumCPU += c.CPU
	}
	return properties.Cost{
		Cardinality: minCard * physicalWrapperCostMultiplier,
		CPU:         (sumCPU + sumCard*properties.IntersectionCPU) * physicalWrapperCostMultiplier,
	}
}
