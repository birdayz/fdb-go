package plans

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
// primary key is a point lookup (one row), but only when the primary-key shape
// stamped on the plan proves that the comparison prefix covers the whole key.
//
// The point-lookup branch charges FetchCPU, not ScanCPU: a full-PK equality
// bind is ONE isolated GetRange round trip with nothing to amortize over (see
// properties.FetchCPU's doc comment for the executor trace) — the same
// physical shape as a Fetch, not a multi-row streaming scan.
func (p *RecordQueryScanPlan) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	if p == nil {
		card := stats.RecordTypeCardinality("")
		return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU}
	}
	comps := p.GetScanComparisons()
	// Equality-vs-range bound selectivity (RFC-164 COST-SELECTIVITY).
	sel, _, _ := properties.BoundSelectivity(comps)
	if isProvablePointProbe(p) {
		return properties.Cost{Cardinality: 1, CPU: properties.FetchCPU}
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
	card := total * sel
	return properties.Cost{Cardinality: card, CPU: card * properties.ScanCPU * properties.PhysicalWrapperCostMultiplier}
}

// HintCost: index scans are cheaper than full table scans because they read a
// subset of records. Apply a selectivity multiplier on top of the physical
// discount. Unique indexes with all columns equality-bound return
// cardinality=1 (point lookup).
//
// The point-lookup branch charges FetchCPU for the INDEX round trip itself —
// it is ONE isolated GetRange call with nothing to amortize over, the same
// physical shape as a Fetch (see properties.FetchCPU's doc comment). This is
// NOT the base-record fetch: that per-row cost still belongs on the separate
// Fetch enforcer (eliminated for covering scans), added on TOP of this when
// the index is non-covering — a covering unique-index point-probe (no Fetch
// node) correctly costs one round trip; a non-covering one correctly costs
// two.
func (p *RecordQueryIndexPlan) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	base := indexBaseCardinality(p, stats)
	if p != nil {
		// Equality-vs-range bound selectivity (RFC-164 COST-SELECTIVITY).
		sel, _, _ := properties.BoundSelectivity(p.GetScanComparisons())
		if isProvablePointProbe(p) {
			return properties.Cost{Cardinality: 1, CPU: properties.FetchCPU}
		}
		base *= sel
	}
	return properties.Cost{Cardinality: base, CPU: base * properties.ScanCPU * properties.PhysicalWrapperCostMultiplier}
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

// isProvablePointProbe reports whether plan is proven, by its OWN static
// shape, to return AT MOST ONE row per execution: a full-PK equality-bound
// RecordQueryScanPlan, or a unique index with every column equality-bound.
// This is the SAME condition RecordQueryScanPlan.HintCost and
// RecordQueryIndexPlan.HintCost gate their point-lookup branch on, factored
// out so RecordQueryInJoinPlan.HintCost can reuse it rather than
// re-deriving it: ImplementInJoinRule (rule_implement_in_join.go) matches
// ANY equality-bound inner, unique or not, so a non-unique secondary-index
// probe is a reachable InJoin inner and must NOT be treated as a point
// lookup. A Fetch is transparent 1:1 over its child
// (RecordQueryFetchFromPartialRecordPlan.HintCost), so it inherits the
// child's provable-point-probe status unchanged.
func isProvablePointProbe(plan RecordQueryPlan) bool {
	switch p := plan.(type) {
	case *RecordQueryScanPlan:
		if p == nil {
			return false
		}
		return properties.EqualityBoundsCoverKey(p.GetScanComparisons(), len(p.GetPrimaryKeyValues())) &&
			!properties.AnyEqualityWidensBeyondOneKey(p.GetScanComparisons())
	case *RecordQueryIndexPlan:
		if p == nil {
			return false
		}
		comps := p.GetScanComparisons()
		_, numBound, allEquality := properties.BoundSelectivity(comps)
		// A widening equality (a zero float) binds TWO keys, so the probe is
		// not a point probe even with every column equality-bound. Shared with
		// computeCardinalities and the cost model so one plan cannot carry
		// contradictory cardinality claims.
		return p.IsUnique() && allEquality && numBound == len(p.GetColumnNames()) &&
			!properties.AnyEqualityWidensBeyondOneKey(comps)
	case *RecordQueryFetchFromPartialRecordPlan:
		if p == nil {
			return false
		}
		return isProvablePointProbe(p.GetInner())
	default:
		return false
	}
}

// HintCost: a K-NN probe returns its top-K (or, for an ordered stream, its
// fixed re-ranked horizon) regardless of table size.
func (p *RecordQueryVectorIndexPlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	card := vectorScanCardinality(p)
	return properties.Cost{
		Cardinality: card,
		CPU:         card * properties.ScanCPU * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: an aggregate index materializes one row per group.
func (p *RecordQueryAggregateIndexPlan) HintCost(_ []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	tableCard := properties.LeafScanCardinality
	if stats != nil {
		tableCard = stats.RecordTypeCardinality(p.GetRecordTypeName())
	}
	cardinality := tableCard * properties.DistinctSelectivity
	if cardinality < 1 {
		cardinality = 1
	}
	return properties.Cost{Cardinality: cardinality, CPU: cardinality * properties.ScanCPU * properties.PhysicalWrapperCostMultiplier}
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
	return properties.Cost{Cardinality: card, CPU: 0}
}

// HintCost: a temp-table scan reads an in-memory buffer of unknown size.
func (p *RecordQueryTempTableScanPlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.Cost{
		Cardinality: properties.LeafScanCardinality,
		CPU:         0,
	}
}

// HintCost: a table function's row count is opaque at plan time.
func (p *RecordQueryTableFunctionPlan) HintCost(_ []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return properties.Cost{
		Cardinality: properties.LeafScanCardinality,
		CPU:         0,
	}
}

// --- single-child, shared formulas ------------------------------------------

// HintCost: one selectivity factor per predicate. Counted via CountConjuncts,
// NOT len(), so this agrees with RecordQueryPredicatesFilterPlan.HintCost and
// NestedLoopJoinCost on the SAME logical residual regardless of whether the
// constructing rule packaged N conditions as one AndPredicate or N top-level
// list entries — see predicates.CountConjuncts's doc comment for why a raw
// len() here reintroduces the shape-dependent cardinality mismatch this cost
// model exists to eliminate.
func (p *RecordQueryFilterPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 || p == nil {
		return properties.Cost{}
	}
	return properties.FilterCost(child[0], predicates.CountConjuncts(p.GetPredicates()))
}

// HintCost: same formula as RecordQueryFilterPlan.HintCost — one selectivity
// factor per CONJUNCT (predicates.CountConjuncts, not len()), so the same
// logical residual costs identically whether it reaches this plan as
// `[a, b]` or as `[And(a, b)]`. Previously duplicated FilterCost's body
// inline while counting via len(); now delegates so there is exactly one
// formula to keep in sync with NestedLoopJoinCost's numPreds.
func (p *RecordQueryPredicatesFilterPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 || p == nil {
		return properties.Cost{}
	}
	return properties.FilterCost(child[0], predicates.CountConjuncts(p.GetPredicates()))
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

// HintCost: primary-key duplicate elimination has the same hash-set work shape
// as unordered value duplicate elimination.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) HintCost(
	child []properties.Cost,
	_ properties.StatisticsProvider,
) properties.Cost {
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
		Cardinality: child[0].Cardinality,
		CPU:         (child[0].CPU + child[0].Cardinality*properties.ProjectionCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: DefaultOnEmpty passes its child through unchanged — literally,
// Cardinality AND CPU. It is a per-row null-extension shim over the SAME rows
// the child produces, not an alternative implementation competing in the
// memo, so it earns no physical-wrapper CPU discount of its own — unlike
// every other wrapper here, it adds no real execution step worth pricing.
func (p *RecordQueryDefaultOnEmptyPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	return child[0]
}

// HintCost: a temp-table insert emits what it consumed.
func (p *RecordQueryTempTableInsertPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 1 {
		return properties.Cost{}
	}
	return properties.Cost{
		Cardinality: child[0].Cardinality,
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
		Cardinality: outCard,
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
		Cardinality: in * properties.DistinctSelectivity,
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
		Cardinality: in,
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
		Cardinality: sumCard,
		CPU:         (sumCPU + sumCard*properties.UnionCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// HintCost: every leg is scanned and concatenated.
func (p *RecordQueryUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	return unionLikeCost(child)
}

// HintCost: every leg is scanned and concatenated.
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
// When p's own join predicates PROVABLY bind the inner leg's full UNIQUE key
// via equality (nestedLoopJoinUniqueKeyConjuncts), NestedLoopJoinCost applies
// the derived 1/innerCard selectivity for those conjuncts instead of the
// flat FilterSelectivity guess — see that function's and
// NestedLoopJoinCost's doc comments for why the flat guess is wrong there
// (a ~500x overestimate for a 1000-row inner) and disagrees with the SAME
// logical join's FlatMap-shape cost, which the total-preorder join-ordering
// comparison (RFC-192) requires to agree.
func (p *RecordQueryNestedLoopJoinPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	uniqueKeyConjuncts, _ := NestedLoopJoinUniqueKeyConjuncts(p)
	return properties.NestedLoopJoinCost(
		child[0], child[1], predicates.CountConjuncts(p.GetPredicates()), uniqueKeyConjuncts)
}

// --- unique-key equality-join detection for the materialized NLJ -----------

// NestedLoopJoinUniqueKeyConjuncts reports how many of p's own join-predicate
// conjuncts are consumed by a full equality bind on the inner leg's own
// UNIQUE key — the primary key of a bare scan, or the key columns of a
// declared-UNIQUE index scan. Exported so cascades/planning_cost_model.go's
// combineConcreteCost can reuse the identical detection for its own
// RecordQueryNestedLoopJoinPlan arm (both call sites must feed
// properties.NestedLoopJoinCost the SAME proof, or the concrete
// join-ordering comparison and the memo's HintCost would rank the identical
// plan two different ways).
//
// This is the join-predicate analogue of isProvablePointProbe, applied
// exactly where isProvablePointProbe itself cannot fire: a materialized
// RecordQueryNestedLoopJoinPlan's inner leg is executed RAW and
// un-parameterized (NestedLoopJoinCost's own doc comment — the inner's own
// ScanComparisons carry no trace of the join predicate at all), so the
// equality only becomes visible in p's OWN predicate list, evaluated
// pairwise by the join itself. A FlatMap's correlated inner instead pushes
// the SAME equality down as a bound scan comparison, which is exactly what
// isProvablePointProbe already recognizes — this function recognizes the
// materialized join's OWN residual-predicate form of the identical shape.
//
// Fails closed (0, false) on anything not specifically recognized — a
// non-unique index, a composite/nested (non-flat) PK component, a conjunct
// under OR/NOT, or a predicate binding only SOME of the key's columns.
// Under-detecting only forgoes the correction (today's flat
// FilterSelectivity fallback remains exactly as before); over-detecting
// would let a non-selective join masquerade as a point probe.
//
// Also fails closed on JoinFullOuter: uniqueness caps only the MATCHED
// (outer, inner) pairs at one row each — it says nothing about the inner rows
// FULL OUTER additionally preserves when they matched no outer row at all, so
// a 10-row outer against a 1000-row inner cannot be capped to 10 the way an
// INNER join's equivalent bind can. JoinInner and JoinLeftOuter both still
// take the correction: LEFT OUTER already preserves every outer row exactly
// once (matched-or-NULL-padded) when the inner key is unique — 0 or 1 inner
// matches per outer row — so its true cardinality is already outerCard, same
// as INNER. JoinCross carries no join-predicate conjuncts to bind a key with
// in the first place, so it is excluded by omission rather than needing its
// own reasoning. Go has no separate JoinRightOuter: RIGHT JOIN is normalized
// to LEFT OUTER with outer/inner swapped at plan-construction time
// (cascades_translator.go), so that LEFT OUTER reasoning already covers it.
func NestedLoopJoinUniqueKeyConjuncts(p *RecordQueryNestedLoopJoinPlan) (int, bool) {
	if p == nil {
		return 0, false
	}
	switch p.GetJoinType() {
	case JoinInner, JoinLeftOuter:
	default:
		return 0, false
	}
	keyCols, ok := innerLeafUniqueKeyColumns(p.GetInner())
	if !ok || len(keyCols) == 0 {
		return 0, false
	}
	innerAlias := values.NamedCorrelationIdentifier(p.GetInnerAlias())

	want := make(map[string]bool, len(keyCols))
	for _, c := range keyCols {
		want[c] = true
	}
	bound := make(map[string]bool, len(keyCols))
	consumed := 0
	for _, pred := range p.GetPredicates() {
		for _, conjunct := range flattenAndConjuncts(pred) {
			field, matched := matchedInnerKeyEquality(conjunct, innerAlias, want, bound)
			if !matched {
				continue
			}
			bound[field] = true
			consumed++
		}
	}
	if len(bound) != len(want) {
		return 0, false
	}
	return consumed, true
}

// flattenAndConjuncts recursively flattens AndPredicate nodes into their leaf
// conjuncts — the same recursion predicates.CountConjuncts's own
// countConjunctsOne applies (predicates.go), duplicated locally because that
// helper only returns a COUNT, not the conjuncts themselves, and this
// function needs to inspect each one individually.
func flattenAndConjuncts(p predicates.QueryPredicate) []predicates.QueryPredicate {
	and, ok := p.(*predicates.AndPredicate)
	if !ok {
		return []predicates.QueryPredicate{p}
	}
	var out []predicates.QueryPredicate
	for _, child := range and.SubPredicates {
		out = append(out, flattenAndConjuncts(child)...)
	}
	return out
}

// matchedInnerKeyEquality reports whether conjunct is an equality binding one
// of want's inner-key columns via innerAlias — checked on EITHER side of the
// comparison, since the join predicate's inner reference can equally appear
// as the LHS operand or the RHS comparand. Declines an already-bound column:
// a redundant repeat conjunct (`inner.pk = outer.a AND inner.pk = outer.a`)
// is not a second consumed conjunct.
//
// Also requires the OPPOSITE operand to carry NO correlation to innerAlias
// (valueCorrelatedTo — the same "is this value a function of alias X"
// membership idiom rule_implement_nested_loop_join.go's
// referenceIsCorrelatedTo and physical_wrapper.go already use elsewhere in
// this planner): `i.id = i.other` and `i.id = i.id` both read the inner's OWN
// key column on one side, but the OTHER side is ALSO a function of the same
// inner row, so the predicate is an intra-inner comparison the join
// re-evaluates for every (outer, inner) pair — it does not pin the inner to
// at most one row per outer row the way a real outer-parameterized bind does.
func matchedInnerKeyEquality(
	conjunct predicates.QueryPredicate,
	innerAlias values.CorrelationIdentifier,
	want, bound map[string]bool,
) (string, bool) {
	cmp, ok := conjunct.(*predicates.ComparisonPredicate)
	if !ok || cmp.Comparison.Type != predicates.ComparisonEquals {
		return "", false
	}
	lhs, rhs := cmp.Operand, cmp.Comparison.Operand
	if field, corr, ok := correlatedInnerField(lhs); ok && corr == innerAlias && want[field] && !bound[field] &&
		!valueCorrelatedTo(rhs, innerAlias) {
		return field, true
	}
	if rhs != nil {
		if field, corr, ok := correlatedInnerField(rhs); ok && corr == innerAlias && want[field] && !bound[field] &&
			!valueCorrelatedTo(lhs, innerAlias) {
			return field, true
		}
	}
	return "", false
}

// valueCorrelatedTo reports whether v's correlation set includes alias.
func valueCorrelatedTo(v values.Value, alias values.CorrelationIdentifier) bool {
	if v == nil {
		return false
	}
	_, ok := values.GetCorrelatedToOfValue(v)[alias]
	return ok
}

// correlatedInnerField returns the field name and root correlation of a
// comparand shaped like a BARE column reference off a source
// QuantifiedObjectValue — the SAME bare-column allowlist
// match_candidate_index.go's buildTranslateValueFunction applies before
// trusting a FieldValue's leaf name (RFC-179 F12): a chained accessor
// (FieldValue{ID, Child: FieldValue{ADDRESS, Child: QOV}}) or a fused
// baked multi-accessor path (Child=QOV directly, Resolved=[ADDRESS,ID],
// Field="ID") both put a DIFFERENT column's leaf name where a real top-level
// "ID" reference would be; treating either shape's bare Field as a flat
// top-level match would let a same-leaf-named nested column (i.address.id)
// impersonate the top-level primary-key column ID.
//
// Unlike buildTranslateValueFunction — which can fall through to a lazy
// re-resolution against the actual target row that fails loud if the guess
// was wrong — this function's result is consumed directly as a cost-model map
// key with no downstream re-check, so (unlike that function) it declines a
// fused multi-accessor path outright (Resolved != nil && !Single()) rather
// than falling through to a name-only match.
func correlatedInnerField(v values.Value) (field string, corr values.CorrelationIdentifier, ok bool) {
	fv, isField := v.(*values.FieldValue)
	if !isField {
		return "", values.CorrelationIdentifier{}, false
	}
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV {
		return "", values.CorrelationIdentifier{}, false
	}
	if fv.Resolved != nil {
		if _, single := fv.Resolved.Single(); !single {
			return "", values.CorrelationIdentifier{}, false
		}
	}
	return fv.Field, qov.Correlation, true
}

// innerLeafUniqueKeyColumns resolves plan down through the same
// identity-preserving wrappers isProvablePointProbe/the fk-chain pkThread
// already see through (a Fetch/TypeFilter/PredicatesFilter/Filter changes
// neither a record's primary key nor a named index's own key columns) and
// returns the bottommost scan/index leaf's own UNIQUE key column names: the
// full primary key for a bare table scan, or the index's own key columns
// when the index is declared UNIQUE. ok=false for anything else (a
// non-unique index, an unrecognized wrapper, a nil leaf, or a
// composite/nested PK component that is not a flat field reference) — fails
// closed.
func innerLeafUniqueKeyColumns(plan RecordQueryPlan) ([]string, bool) {
	switch p := plan.(type) {
	case *RecordQueryScanPlan:
		if p == nil {
			return nil, false
		}
		names := make([]string, 0, len(p.GetPrimaryKeyValues()))
		for _, v := range p.GetPrimaryKeyValues() {
			if _, isRecordType := v.(*values.RecordTypeValue); isRecordType {
				// A per-thread CONSTANT (the RecordTypeKey-prefixed
				// composite PK every table in a non-intermingled multi-type
				// schema has) — never a discriminating column, so it needs
				// no corresponding equality conjunct. Same reasoning as
				// fk_chain_cardinality.go's innerFullyBindsThread doc
				// comment.
				continue
			}
			fv, ok := v.(*values.FieldValue)
			if !ok || fv.Child != nil {
				return nil, false // composite/nested PK component — fail closed
			}
			names = append(names, fv.Field)
		}
		if len(names) == 0 {
			return nil, false
		}
		return names, true
	case *RecordQueryIndexPlan:
		if p == nil || !p.IsUnique() {
			return nil, false
		}
		// GetColumnNames() retains display/leaf names even for a function or
		// nested-key index whose names WithOrderingKeyNamesUnavailable has
		// marked unsafe for name-based matching (e.g. a unique
		// CARDINALITY(TAGS) or a nested ADDR.CITY key) — the same marker
		// HintOrdering (ordering.go) already gates on before trusting these
		// names for a different purpose. Consult it here too: a plain leaf
		// name off such an index is not proof of a bind on a flat top-level
		// key column.
		if !p.orderingKeyNamesKnown || !p.orderingKeyNamesSafe {
			return nil, false
		}
		cols := p.GetColumnNames()
		if len(cols) == 0 {
			return nil, false
		}
		out := make([]string, len(cols))
		copy(out, cols)
		return out, true
	case *RecordQueryFetchFromPartialRecordPlan:
		if p == nil {
			return nil, false
		}
		return innerLeafUniqueKeyColumns(p.GetInner())
	case *RecordQueryTypeFilterPlan:
		if p == nil {
			return nil, false
		}
		return innerLeafUniqueKeyColumns(p.GetInner())
	case *RecordQueryPredicatesFilterPlan:
		if p == nil {
			return nil, false
		}
		return innerLeafUniqueKeyColumns(p.GetInner())
	case *RecordQueryFilterPlan:
		if p == nil {
			return nil, false
		}
		return innerLeafUniqueKeyColumns(p.GetInner())
	default:
		return nil, false
	}
}

// HintCost: an InJoin re-runs its inner once per IN value. ImplementInJoinRule
// (rule_implement_in_join.go) matches ANY equality-bound inner — a unique
// index, a full primary-key bind, OR a non-unique secondary index — so
// "the inner does an equality point-lookup returning ~1 row" is only sound
// when isProvablePointProbe proves it. When it does, the IN-list length IS
// the output cardinality and each value pays TWO isolated, unamortized round
// trips — the index equality point-probe itself (ONE GetRange bound to that
// value, no batching across IN values) and the base-record fetch it resolves
// — the same physical shape properties.FetchCPU's doc comment establishes for
// any isolated point-probe/fetch, not the amortized multi-row ScanCPU rate.
//
// When it is NOT a provable point probe (e.g. `category IN ('a','b','c')`
// over a non-unique index), the child's own cardinality is the per-probe row
// count and must NOT be discarded — the same principle plan_properties.go's
// computeCardinalities uses for RecordQueryInJoinPlan
// (inSize.Times(child.GetMaxCardinality())) and RecordQueryInUnionPlan.
// HintCost already applies below. FlatMapCost is the exact right shape for
// this: the IN-list is a synthetic, free-to-produce "outer" of inListLen rows
// (RecordQueryValuesPlan.HintCost prices a literal row source at CPU 0)
// driving one execution of the SAME inner per row — precisely what a
// correlated dependent join costs.
func (p *RecordQueryInJoinPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	inListLen := float64(len(p.GetInValues()))
	if inListLen < 1 {
		inListLen = 10 // parameterized IN — values not bound at plan time
	}
	if isProvablePointProbe(p.GetInner()) {
		return properties.Cost{
			Cardinality: inListLen,
			CPU:         inListLen * (properties.FetchCPU + properties.FetchCPU) * properties.PhysicalWrapperCostMultiplier,
		}
	}
	return properties.FlatMapCost(properties.Cost{Cardinality: inListLen}, child[0])
}

// HintCost: an InUnion runs its child once per Cartesian-product
// IN-binding combination. Literal sources have an exact fanout; each source
// unavailable at planning time contributes the conservative default of 10
// values. A single exact combination executes the child directly, so it must
// not receive an artificial wrapper discount.
func (p *RecordQueryInUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) == 0 {
		return properties.Cost{}
	}
	fanout := estimatedInUnionFanout(p)
	if fanout == 0 {
		return properties.Cost{}
	}
	if fanout == 1 {
		return child[0]
	}
	in := child[0].Cardinality
	return properties.Cost{
		Cardinality: in * fanout,
		CPU:         (child[0].CPU*fanout + in*fanout*properties.UnionCPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

func estimatedInUnionFanout(p *RecordQueryInUnionPlan) float64 {
	if exact, known := p.LiteralFanout(); known {
		return float64(exact)
	}
	if p == nil {
		return 1
	}

	// Preserve every known dimension and substitute ten only for an unknown
	// one. Saturating at MaxInt64 keeps the heuristic finite even for a
	// pathological number of dimensions.
	fanout := int64(1)
	bindings := p.GetBindingNames()
	sources := p.GetInSources()
	if len(sources) != len(bindings) {
		// A present but malformed source/binding shape cannot be assigned a
		// meaningful execution count. Keep it maximally conservative; valid
		// plans always align the two slices.
		return float64(math.MaxInt64)
	}
	if len(bindings) == 0 {
		return 1
	}
	for i := range bindings {
		size := int64(10)
		if i < len(sources) && sources[i] != nil {
			size = int64(len(sources[i]))
		}
		if size == 0 {
			return 0
		}
		if fanout > math.MaxInt64/size {
			return float64(math.MaxInt64)
		}
		fanout *= size
	}
	return float64(fanout)
}

// --- recursive operators ----------------------------------------------------

// recursiveCost: the recursive leg is re-executed once per row the seed leg
// produced, so the seed's cardinality multiplies both the output and the
// recursive leg's CPU.
//
// seedCard/recCard are NOT zero-guarded — the same reasoning as
// properties.FlatMapCost's doc comment applies here: an exact zero is a
// genuine empty seed (e.g. a LIMIT 0 seed leg), which correctly recurses zero
// times and must yield a zero-row CTE, not
// LeafScanCardinality*recCard rows conjured from a seed that produced none.
func recursiveCost(child []properties.Cost) properties.Cost {
	seedCard, recCard := child[0].Cardinality, child[1].Cardinality
	return properties.Cost{
		Cardinality: seedCard * recCard,
		CPU:         (child[0].CPU + seedCard*child[1].CPU) * properties.PhysicalWrapperCostMultiplier,
	}
}

// levelUnionBufferTouches counts the temp-table round-trips the level-at-a-time
// recursive union pays PER materialized frontier row that the DFS join does not
// — the cost signature of the two operators' different residency, grounded in
// the executor:
//
//   - The union executor (executor/recursive_union_cursor.go) drives the
//     recursion through two ping-ponging temp tables. Each frontier row is
//     WRITTEN into the insert table (tempTableInsertCursor.OnNext → TempTable.Add)
//     and later READ back as the next level's driver (a TempTableScanPlan over the
//     table the buffers just flipped into) — two buffer touches per row. On top of
//     that the temp table's byte charge is MONOTONIC: Clear does NOT refund
//     (executor/evaluation_context.go, TempTable.charged), so a drained frontier's
//     residency is re-charged ("echoed") into the reused buffer rather than
//     released — the materialized set stays resident until statement teardown
//     (ReleaseCharges).
//   - The DFS cursor (executor/recursive_cursor.go) buffers NO level: it charges
//     one stack slot per open depth on push (chargeNode) and RELEASES it on pop
//     (releaseNodes), streaming a single root→leaf path. Peak residency is the
//     path depth, not the whole output.
//
// So the level union does a write + read-back (2 touches) on every one of the
// ~output-cardinality rows it materializes; the DFS join does neither. This makes
// the level union strictly costlier for equal children, so the planner prefers
// DFS on COST — a preference that survives wrapper-identity changes (RFC-184 W2),
// unlike the structural compareRecursiveCTE tie-break. Not an epsilon nudge: the
// term is a genuine per-row buffer cost at the union merge rate.
const levelUnionBufferTouches = 2

// HintCost: depth-first recursive traversal from each root row. The DFS cursor
// streams one root→leaf path with a charge-once-per-depth stack (see
// levelUnionBufferTouches), so it carries no level-buffer term.
func (p *RecordQueryRecursiveDfsJoinPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	return recursiveCost(child)
}

// HintCost: level-at-a-time recursive union from the initial level. It costs the
// same base recursion as the DFS join PLUS a level-buffer echo: every
// materialized frontier row is written to and read back from a temp table, and
// the drained buffer's charge is echoed (not refunded) into the next level (see
// levelUnionBufferTouches). The added CPU term makes the union strictly costlier
// than the DFS join for identical children, so the planner prefers DFS on cost.
func (p *RecordQueryRecursiveLevelUnionPlan) HintCost(child []properties.Cost, _ properties.StatisticsProvider) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	cost := recursiveCost(child)
	// The materialized set is the operator's output cardinality; each row pays
	// levelUnionBufferTouches buffer operations at the union merge rate (UnionCPU).
	// Charged on CPU ONLY — the union emits the SAME rows as the DFS join, so its
	// OUTPUT cardinality (which rolls up into parents) must not change.
	cost.CPU += cost.Cardinality * properties.UnionCPU * levelUnionBufferTouches
	return cost
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
