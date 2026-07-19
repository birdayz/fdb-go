package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Per-operator cost, owned by the plans themselves (RFC-183 P5).
//
// Each plan answers properties.CostHinter: given its children's rolled-up
// costs and the active statistics provider, it returns its own Cost. This is
// the metadata-aware cost the memo's winner selection consumes — distinct
// from the metadata-INDEPENDENT scanLikeCost used by the concrete
// join-ordering recursion, which deliberately abstains from unique/covering
// knowledge so it stays consistent without a PlanContext (RFC-069). Do not
// collapse the two.
//
// The shared per-operator formulas live in properties (FilterCost, MapCost,
// …) so that this file and the concrete join-ordering cost can never drift.

// --- leaves -----------------------------------------------------------------

// HintCost: a full scan reads every record of the covered types, discounted by
// the bound-comparison selectivity. A fully-equality-bound scan over the
// primary key is a point lookup (one row).
func (p *RecordQueryScanPlan) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	if p == nil {
		card := stats.RecordTypeCardinality("")
		return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU}
	}
	comps := p.GetScanComparisons()
	// Equality-vs-range bound selectivity (RFC-164 COST-SELECTIVITY).
	sel, numBound, allEquality := properties.BoundSelectivity(comps)
	if numBound > 0 && allEquality && numBound == len(comps) {
		return properties.Cost{Cardinality: 1, CPU: properties.ScanCPU}
	}
	types := p.GetRecordTypes()
	total := 0.0
	if len(types) == 0 {
		total = stats.RecordTypeCardinality("")
	} else {
		for _, t := range types {
			total += stats.RecordTypeCardinality(t)
		}
	}
	card := total * sel * properties.PhysicalWrapperCostMultiplier
	return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU}
}

// HintCost: index scans are cheaper than full table scans because they read a
// subset of records. Apply a selectivity multiplier on top of the physical
// discount. Unique indexes with all columns equality-bound return
// cardinality=1 (point lookup).
//
// Fetch I/O cost (FetchCPU per row) is NOT included here — it belongs on the
// Fetch enforcer, which is eliminated for covering scans.
func (p *RecordQueryIndexPlan) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	base := indexBaseCardinality(p, stats) * properties.PhysicalWrapperCostMultiplier
	if p != nil {
		// Equality-vs-range bound selectivity (RFC-164 COST-SELECTIVITY).
		sel, numBound, allEquality := properties.BoundSelectivity(p.GetScanComparisons())
		if p.IsUnique() && allEquality && numBound == len(p.GetColumnNames()) {
			return properties.Cost{Cardinality: properties.PhysicalWrapperCostMultiplier, CPU: 0}
		}
		base *= sel
	}
	return properties.Cost{Cardinality: base, CPU: base * properties.ScanCPU}
}

func indexBaseCardinality(plan *RecordQueryIndexPlan, stats properties.StatisticsProvider) float64 {
	if plan != nil {
		if types := plan.GetRecordTypes(); len(types) > 0 {
			total := 0.0
			for _, t := range types {
				total += stats.RecordTypeCardinality(t)
			}
			return total
		}
	}
	return stats.RecordTypeCardinality("")
}

// HintCost: a K-NN probe returns its top-K (or, for an ordered stream, its
// fixed re-ranked horizon) regardless of table size.
func (p *RecordQueryVectorIndexPlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	card := vectorScanCardinality(p)
	return properties.Cost{
		Cardinality: card * properties.PhysicalWrapperCostMultiplier,
		CPU:         card * properties.ScanCPU * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: an aggregate index materializes one row per group.
func (p *RecordQueryAggregateIndexPlan) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	tableCard := properties.LeafScanCardinality
	if stats != nil {
		tableCard = stats.RecordTypeCardinality(p.GetRecordTypeName())
	}
	cardinality := tableCard * properties.DistinctSelectivity * properties.PhysicalWrapperCostMultiplier
	if cardinality < 1 {
		cardinality = 1
	}
	return properties.Cost{Cardinality: cardinality, CPU: cardinality * properties.ScanCPU}
}

// HintCost: a literal row source costs nothing to produce.
func (p *RecordQueryValuesPlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.Cost{Cardinality: 1, CPU: 0}
}

// HintCost: exploding a literal collection yields one row per element; a
// non-literal collection falls back to a small default.
func (p *RecordQueryExplodePlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	card := 10.0
	if p != nil {
		if cv, ok := p.GetCollectionValue().(*values.ConstantValue); ok {
			if sl, ok := cv.Value.([]any); ok {
				card = float64(len(sl))
				if card < 1 {
					card = 1
				}
			}
		}
	}
	return properties.Cost{Cardinality: card * properties.PhysicalWrapperCostMultiplier, CPU: 0}
}

// HintCost: a temp-table scan reads an in-memory buffer of unknown size.
func (p *RecordQueryTempTableScanPlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.Cost{
		Cardinality: properties.LeafScanCardinality * properties.PhysicalWrapperCostMultiplier,
		CPU:         0,
	}
}

// HintCost: a table function's row count is opaque at plan time.
func (p *RecordQueryTableFunctionPlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.Cost{
		Cardinality: properties.LeafScanCardinality * properties.PhysicalWrapperCostMultiplier,
		CPU:         0,
	}
}

// --- single-child, shared formulas ------------------------------------------

// HintCost: one selectivity factor per predicate.
func (p *RecordQueryFilterPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 || p == nil {
		return properties.Cost{}
	}
	return properties.FilterCost(child[0], len(p.GetPredicates()))
}

// HintCost: same shape as FilterCost, spelled out because a zero-predicate
// predicates-filter still pays one selectivity factor.
func (p *RecordQueryPredicatesFilterPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 || p == nil {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	numPreds := len(p.GetPredicates())
	if numPreds == 0 {
		numPreds = 1
	}
	sel := properties.FilterSelectivity
	for i := 1; i < numPreds; i++ {
		sel *= properties.FilterSelectivity
	}
	return properties.Cost{
		Cardinality: in * sel * properties.PhysicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.FilterCPU*float64(numPreds)) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: record-type discrimination over the child stream.
func (p *RecordQueryTypeFilterPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.TypeFilterCost(child[0])
}

// HintCost: primary-key fetch of the full record per index entry.
func (p *RecordQueryFetchFromPartialRecordPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.FetchCost(child[0])
}

// HintCost: at most one row out, the child's work paid in full.
func (p *RecordQueryFirstOrDefaultPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.FirstOrDefaultCost(child[0])
}

// HintCost: materialize + O(n log n), with NO physical discount so an
// in-memory sort stays strictly more expensive than index-based elimination.
func (p *RecordQueryInMemorySortPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.InMemorySortCost(child[0])
}

// HintCost: duplicate elimination over the child stream.
func (p *RecordQueryDistinctPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.DistinctCost(child[0])
}

// HintCost: per-row projection, cardinality-preserving.
func (p *RecordQueryMapPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.MapCost(child[0])
}

// HintCost: projection is cardinality-preserving with a per-row CPU charge.
func (p *RecordQueryProjectionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality * properties.PhysicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + child[0].Cardinality*properties.ProjectionCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: DefaultOnEmpty passes its child through unchanged.
func (p *RecordQueryDefaultOnEmptyPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality * properties.PhysicalWrapperCostMultiplier,
		CPU:         child[0].CPU * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: a temp-table insert emits what it consumed.
func (p *RecordQueryTempTableInsertPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 1 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality * properties.PhysicalWrapperCostMultiplier,
		CPU:         child[0].CPU * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: a plan-time LIMIT caps the child's cardinality. A runtime cap
// (GetLimitValue != nil) is unknown at plan time — leave the child cardinality
// unreduced (conservative) rather than reading the -1 sentinel as "no rows".
func (p *RecordQueryLimitPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	outCard := child[0].Cardinality
	if p != nil && p.GetLimitValue() == nil {
		if l := p.GetLimit(); l == 0 {
			outCard = 0 // LIMIT 0 → no rows (not "no cap").
		} else if l > 0 && float64(l) < outCard {
			outCard = float64(l)
		}
	}
	return properties.Cost{
		Cardinality: outCard * properties.PhysicalWrapperCostMultiplier,
		CPU:         child[0].CPU * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: grouped aggregation emits one row per group.
func (p *RecordQueryStreamingAggregationPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * properties.DistinctSelectivity * properties.PhysicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.StreamingAggCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// --- DML --------------------------------------------------------------------

func dmlCost(child []properties.Cost) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * properties.PhysicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*properties.WriteCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: one write per consumed row.
func (p *RecordQueryInsertPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return dmlCost(child)
}

// HintCost: one write per consumed row.
func (p *RecordQueryDeletePlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return dmlCost(child)
}

// HintCost: one write per consumed row.
func (p *RecordQueryUpdatePlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return dmlCost(child)
}

// --- n-ary set operators ----------------------------------------------------

func unionLikeCost(child []properties.Cost) properties.Cost {
	sumCard, sumCPU := 0.0, 0.0
	for _, c := range child {
		sumCard += c.Cardinality
		sumCPU += c.CPU
	}
	return properties.Cost{
		Cardinality: sumCard * properties.PhysicalWrapperCostMultiplier,
		CPU:         (sumCPU + sumCard*properties.UnionCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: every leg is scanned and merged.
func (p *RecordQueryUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return unionLikeCost(child)
}

// HintCost: every leg is scanned and merged.
func (p *RecordQueryUnorderedUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return unionLikeCost(child)
}

// HintCost: every leg is scanned and merged.
func (p *RecordQueryMergeSortUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return unionLikeCost(child)
}

// HintCost: output bounded by the smallest leg.
func (p *RecordQueryIntersectionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.IntersectionCost(child)
}

// HintCost: output bounded by the smallest leg. With no child costs yet
// (an un-costed memo probe) fall back to a group-cardinality estimate over
// the declared leg count.
func (p *RecordQueryMultiIntersectionOnValuesPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		groupCard := properties.LeafScanCardinality * properties.DistinctSelectivity
		nChildren := len(p.GetChildren())
		if nChildren < 1 {
			nChildren = 1
		}
		return properties.Cost{
			Cardinality: groupCard,
			CPU:         groupCard * properties.IntersectionCPU * float64(nChildren),
		}
	}
	return properties.IntersectionCost(child)
}

// --- joins and IN operators -------------------------------------------------

// HintCost: a correlated dependent join re-runs the inner once per outer row.
func (p *RecordQueryFlatMapPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	return properties.FlatMapCost(child[0], child[1])
}

// HintCost: a MATERIALIZED nested-loop join — the inner is executed once.
func (p *RecordQueryNestedLoopJoinPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	return properties.NestedLoopJoinCost(child[0], child[1])
}

// HintCost: an InJoin is a correlated index probe — for each IN value the
// inner does an equality point-lookup returning ~1 row. The child's standalone
// cardinality overstates this (it reports the index's selectivity against the
// full table), so the IN-list length is the output cardinality.
func (p *RecordQueryInJoinPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	inListLen := float64(len(p.GetInValues()))
	if inListLen < 1 {
		inListLen = 10 // parameterized IN — values not bound at plan time
	}
	return properties.Cost{
		Cardinality: inListLen * properties.PhysicalWrapperCostMultiplier,
		CPU:         inListLen * (properties.ScanCPU + properties.FetchCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: an InUnion runs its child once per IN-binding combination.
func (p *RecordQueryInUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	inDims := float64(len(p.GetBindingNames()))
	if inDims < 1 {
		inDims = 10
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * inDims * properties.PhysicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + in*inDims*properties.UnionCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// --- recursive operators ----------------------------------------------------

// recursiveCost: the recursive leg is re-executed once per row the seed leg
// produced, so the seed's cardinality multiplies both the output and the
// recursive leg's CPU.
func recursiveCost(child []properties.Cost) properties.Cost {
	seedCard, recCard := child[0].Cardinality, child[1].Cardinality
	if seedCard == 0 {
		seedCard = properties.LeafScanCardinality
	}
	if recCard == 0 {
		recCard = properties.LeafScanCardinality
	}
	return properties.Cost{
		Cardinality: seedCard * recCard * properties.PhysicalWrapperCostMultiplier,
		CPU:         (child[0].CPU + seedCard*child[1].CPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: depth-first recursive traversal from each root row.
func (p *RecordQueryRecursiveDfsJoinPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	return recursiveCost(child)
}

// HintCost: level-at-a-time recursive union from the initial level.
func (p *RecordQueryRecursiveLevelUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	return recursiveCost(child)
}

// defaultVectorHorizon is the bounded re-ranked horizon an ordered-stream scan
// streams, decoupled from the probe width.
const defaultVectorHorizon = 200.0

// vectorScanCardinality returns the plan-time top-K when it is a literal int,
// else a small default. An ORDERED-STREAM scan (RFC-156 Phase B) is NOT self-
// limited to k — it streams its fixed re-ranked horizon (the re-rank budget,
// decoupled from the probe width) — so it is costed at the horizon, not k. This
// makes a residual-bearing ordered scan correctly more expensive than a folded
// top-k scan, and lets the SinkLimit fold win whenever it is applicable.
func vectorScanCardinality(plan *RecordQueryVectorIndexPlan) float64 {
	const defaultK = 10.0
	if plan == nil || plan.GetK() == nil {
		return defaultK
	}
	if plan.IsOrderedStream() {
		return defaultVectorHorizon
	}
	// Plan-time cost estimation: a non-constant or erroring K declines to the
	// default cardinality rather than failing planning.
	kv, err := plan.GetK().Evaluate(nil)
	if err != nil {
		return defaultK
	}
	switch n := kv.(type) {
	case int:
		if n > 0 {
			return float64(n)
		}
	case int32:
		if n > 0 {
			return float64(n)
		}
	case int64:
		if n > 0 {
			return float64(n)
		}
	}
	return defaultK
}
