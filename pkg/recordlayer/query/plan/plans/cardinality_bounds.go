package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// Per-operator PROVEN cardinality bounds, owned by the plans themselves
// (RFC-195).
//
// Each plan answers properties.CardinalityProver: given the intervals proved
// for its children, it returns the interval it proves for its own output. This
// is a faithful port of Java's CardinalitiesProperty.CardinalitiesVisitor,
// relocated from cascades.computeCardinalities — which switched exclusively on
// `plans.*` types while living at the TOP of the layering
// (expressions <- properties <- plans <- cascades), out of reach of the
// `properties` cost walk that most needs it.
//
// The derivation now sits beside the HintCost formulas it constrains, which is
// the whole point: the six impossible estimates RFC-195 corrects each
// accumulated because "this operator structurally guarantees a row" lived in a
// different file from "this operator costs in*0.7", and nothing made the two
// agree. cascades.computeCardinalities is now a thin adapter over these
// methods, keeping its signature for the property-map consumers.
//
// STRUCTURAL bounds only, never statistics: a unique index with every column
// equality-bound PROVES max=1, it does not "probably" return one row. An
// operator that can prove nothing returns UnknownCardinalities, which makes the
// clamp a deterministic no-op for it — the correct direction, since inventing a
// bound is how a false proof would silently cap a legitimate estimate.

// provenChild returns the single child interval, or unknown when the child edge
// is absent or ambiguous. An absent child is an absent proof, never a bound.
func provenChild(child []properties.Cardinalities) properties.Cardinalities {
	if len(child) != 1 {
		return properties.UnknownCardinalities()
	}
	return child[0]
}

// --- exactly one row --------------------------------------------------------

// ProvenCardinalities: a FirstOrDefault emits the child's first row or the
// default — exactly one either way, regardless of the child.
func (p *RecordQueryFirstOrDefaultPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	return properties.ExactlyOne()
}

// ProvenCardinalities: a literal row source is one deterministic row.
func (p *RecordQueryValuesPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	return properties.ExactlyOne()
}

// --- transparent: the child's bounds, unchanged -----------------------------

// ProvenCardinalities: record-type discrimination removes rows but the model
// conservatively passes the child's bounds through, matching Java's
// CardinalitiesVisitor.
func (p *RecordQueryTypeFilterPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: per-row projection is 1:1.
func (p *RecordQueryMapPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: projection is cardinality-preserving.
func (p *RecordQueryProjectionPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: a temp-table insert emits what it consumed.
func (p *RecordQueryTempTableInsertPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: a fetch is 1:1 — Java's CardinalitiesProperty passes it
// through, so a bounded index access under a Fetch keeps its proven max.
func (p *RecordQueryFetchFromPartialRecordPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: a sort reorders rows, it never adds or removes them.
func (p *RecordQueryInMemorySortPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: full-row duplicate elimination — the current model
// conservatively preserves the child's bounds rather than claiming a dedup
// ratio it cannot prove.
func (p *RecordQueryDistinctPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: one effect per consumed row.
func (p *RecordQueryInsertPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: one effect per consumed row.
func (p *RecordQueryDeletePlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// ProvenCardinalities: one effect per consumed row.
func (p *RecordQueryUpdatePlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child)
}

// --- floors and filters -----------------------------------------------------

// ProvenCardinalities: DefaultOnEmpty guarantees at least one row —
// real-or-default — so the child's bounds are floored at one.
func (p *RecordQueryDefaultOnEmptyPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return provenChild(child).Floor(1)
}

// ProvenCardinalities: a filter may eliminate every row, so the minimum drops
// to zero; the maximum is inherited.
func (p *RecordQueryFilterPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return properties.Cardinalities{
		Min: properties.OfCardinality(0),
		Max: provenChild(child).GetMaxCardinality(),
	}
}

// ProvenCardinalities: a filter may eliminate every row, so the minimum drops
// to zero; the maximum is inherited.
func (p *RecordQueryPredicatesFilterPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return properties.Cardinalities{
		Min: properties.OfCardinality(0),
		Max: provenChild(child).GetMaxCardinality(),
	}
}

// ProvenCardinalities: primary-key duplicate elimination leaves the maximum
// unchanged, and any known non-empty input produces at least one unique key.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	c := provenChild(child)
	minCard := c.GetMinCardinality()
	if !minCard.IsUnknown() && minCard.Value() > 0 {
		minCard = properties.OfCardinality(1)
	}
	return properties.Cardinalities{Min: minCard, Max: c.GetMaxCardinality()}
}

// --- data-access leaves -----------------------------------------------------

// ProvenCardinalities: a full-primary-key-equality primary scan is a point
// lookup (at most one row), exactly as Java's
// CardinalitiesProperty.visitRecordQueryScanPlan bounds a scan whose
// equality-bound values cover the whole primary key. A range, partial or prefix
// bind stays unknown.
//
// The proof runs through isProvablePointProbe — the SAME recognizer
// RecordQueryScanPlan.HintCost gates its point-lookup branch on — so one plan
// cannot carry a cost that says "one row" and a proof that says otherwise, or
// the reverse. That reverse is not hypothetical: the derivation this replaces
// consulted a helper whose widening guard sat AFTER its stamped-primary-key
// early return, so a scan with a stamped PK and a terminal zero-valued FLOAT
// equality was PROVEN at-most-one while the cost model correctly declined to
// treat it as a point probe. The executor widens a zero bound across -0.0 and
// +0.0 (IEEE-equal, distinct adjacent keys), so that proof was false, and under
// RFC-195's clamp a false max=1 would have CAPPED the honest estimate to it.
func (p *RecordQueryScanPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	if isProvablePointProbe(p) {
		return properties.AtMostOne()
	}
	return properties.UnknownMaxCardinality()
}

// ProvenCardinalities: a UNIQUE index with every one of its columns
// equality-bound pins a single entry — at most one row.
//
// A WIDENING equality does not pin a single key, so it cannot support an
// at-most-one PROOF. The executor widens a terminal zero-valued float bound
// across both signed zeros (-0.0 and +0.0 are IEEE-equal but pack to distinct
// adjacent keys), and a UNIQUE index legitimately holds BOTH — uniqueness is
// enforced on the raw packed prefix, so the two are different entries.
// Measured: a unique index on a DOUBLE column holding both zeros returns TWO
// rows for `WHERE v = 0`. That is a false proof, not a loose estimate, and
// DISTINCT elision, single-row shortcuts and the cost model all inherit it.
//
// The check uses the SHARED widening predicate deliberately: a correlated or
// Unknown-typed operand — the QOV a multi-element float IN list produces — is
// not constant, yet the executor can still widen a zero binding at runtime.
// The matcher's constant-only helper is not runtime-safe, and using it here
// left the false proof reachable through exactly those operands.
//
// This arm counts EVERY comparison (including empty trailing ranges) rather
// than only the non-empty bound prefix, which is strictly stricter than
// isProvablePointProbe's index case: a trailing unconstrained column declines
// the proof here. Kept as it was — a proof may safely be stricter than the cost
// model's point-probe recognizer, since a declined proof only forgoes a clamp,
// while a granted one caps an estimate.
func (p *RecordQueryIndexPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	if p == nil || !p.IsUnique() {
		return properties.UnknownMaxCardinality()
	}
	comps := p.GetScanComparisons()
	allEquality := true
	for _, cr := range comps {
		if !cr.IsEquality() {
			allEquality = false
			break
		}
	}
	if properties.AnyEqualityWidensBeyondOneKey(comps) {
		allEquality = false
	}
	if allEquality && len(comps) == len(p.GetColumnNames()) {
		return properties.AtMostOne()
	}
	return properties.UnknownMaxCardinality()
}

// ProvenCardinalities: a K-NN probe's row count depends on how many neighbours
// the index actually holds within the probed region, which is not a structural
// property of the plan.
func (p *RecordQueryVectorIndexPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	return properties.UnknownMaxCardinality()
}

// ProvenCardinalities: the group count an aggregate index materializes is data,
// not structure.
func (p *RecordQueryAggregateIndexPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	return properties.UnknownMaxCardinality()
}

// ProvenCardinalities: the exploded collection's length is a runtime value.
func (p *RecordQueryExplodePlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	return properties.UnknownMaxCardinality()
}

// ProvenCardinalities: a table function's row count is opaque at plan time.
func (p *RecordQueryTableFunctionPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	return properties.UnknownMaxCardinality()
}

// ProvenCardinalities: a temp-table's contents are produced at runtime.
func (p *RecordQueryTempTableScanPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	return properties.UnknownMaxCardinality()
}

// --- set operations ---------------------------------------------------------

// ProvenCardinalities: a union concatenates its legs — bounds sum.
func (p *RecordQueryUnionPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return properties.UnionCardinalities(child)
}

// ProvenCardinalities: a union concatenates its legs — bounds sum.
func (p *RecordQueryMergeSortUnionPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return properties.UnionCardinalities(child)
}

// ProvenCardinalities: a union concatenates its legs — bounds sum.
func (p *RecordQueryUnorderedUnionPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return properties.UnionCardinalities(child)
}

// ProvenCardinalities: an intersection is bounded by its smallest leg, and can
// be empty whenever two legs are independently bounded.
func (p *RecordQueryIntersectionPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return properties.IntersectCardinalities(child)
}

// ProvenCardinalities: an intersection is bounded by its smallest leg, and can
// be empty whenever two legs are independently bounded.
func (p *RecordQueryMultiIntersectionOnValuesPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return properties.IntersectCardinalities(child)
}

// --- limits -----------------------------------------------------------------

// ProvenCardinalities: a plan-time LIMIT caps the child's maximum.
//
// LIMIT 0 produces exactly zero rows — not "no cap". Folding it into the
// negative ("no limit") arm would mis-bound any parent over a LIMIT-0 subtree
// as the full child cardinality.
//
// A negative limit is no cap (an OFFSET-only stream). A RUNTIME-Value limit is
// also stored as limit=-1 and DELIBERATELY lands there: its real cap is only
// known at execution, so reading it as conservative no-cap is the sound choice
// — over-estimation never enables an unsound rewrite, matching the explicit
// HintCost conservative choice. Do not "fix" this into reducing to -1.
func (p *RecordQueryLimitPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	c := provenChild(child)
	if p == nil {
		return c
	}
	// The cap arithmetic lives in properties.LimitBound, shared with the
	// LogicalLimitExpression arm — the pair must prove the same interval, and a
	// second transcription here is exactly how a pair drifts.
	return properties.LimitBound(p.GetLimit(), c)
}

// --- IN operators -----------------------------------------------------------

// ProvenCardinalities: the inner runs once per IN value, so both bounds scale
// by the in-list length. An in-list whose size is not known at plan time proves
// nothing.
func (p *RecordQueryInJoinPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	if p == nil {
		return properties.UnknownCardinalities()
	}
	c := provenChild(child)
	inVals := p.GetInValues()
	if len(inVals) == 0 {
		return properties.UnknownMaxCardinality()
	}
	inSize := properties.OfCardinality(int64(len(inVals)))
	return properties.Cardinalities{
		Min: inSize.Times(c.GetMinCardinality()),
		Max: inSize.Times(c.GetMaxCardinality()),
	}
}

// ProvenCardinalities: the child runs once per Cartesian-product IN-binding
// combination, so both bounds scale by the literal fanout.
//
// A known-EMPTY dimension proves exactly zero even when another dimension is
// runtime-unknown: the executor returns an empty cursor. Java's generic unknown
// multiplication keeps that mixed maximum unknown; exact zero is stronger but
// sound, and it is a deliberate Go precision extension.
func (p *RecordQueryInUnionPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	if p == nil {
		return properties.UnknownCardinalities()
	}
	fanout, known := p.LiteralFanout()
	if !known {
		return properties.UnknownMaxCardinality()
	}
	if fanout == 0 {
		return properties.Cardinalities{
			Min: properties.OfCardinality(0),
			Max: properties.OfCardinality(0),
		}
	}
	c := provenChild(child)
	return properties.Cardinalities{
		Min: properties.MultiplyCardinality(fanout, c.GetMinCardinality()),
		Max: properties.MultiplyCardinality(fanout, c.GetMaxCardinality()),
	}
}

// --- aggregation ------------------------------------------------------------

// ProvenCardinalities: an UNGROUPED streaming aggregation applies its
// aggregates over the entire child result set and emits a single row — at most
// one, structurally. A grouped aggregation's group count is data, not
// structure.
//
// This max=1 is the bound the cost model contradicted by five orders of
// magnitude: the hint charges in*DistinctSelectivity with no cap, which for a
// 1e6-row child is ~700,000 rows for an operator that provably emits one, so
// every join ordering above it was computed against a number wrong by 700,000x.
func (p *RecordQueryStreamingAggregationPlan) ProvenCardinalities(_ []properties.Cardinalities) properties.Cardinalities {
	if p == nil {
		return properties.UnknownCardinalities()
	}
	if len(p.GetGroupingKeys()) == 0 {
		return properties.Cardinalities{
			Min: properties.OfCardinality(0),
			Max: properties.OfCardinality(1),
		}
	}
	return properties.UnknownMaxCardinality()
}

// --- joins ------------------------------------------------------------------

// ProvenCardinalities: outer x inner, exactly as Java's
// visitRecordQueryFlatMapPlan multiplies the two legs.
func (p *RecordQueryFlatMapPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return joinTimes(child)
}

// ProvenCardinalities: outer x inner, matching the FlatMap shape of the same
// logical join — the two physical realizations of one logical join must prove
// the same bound, or the clamp would price them differently for reasons that
// are not about cost.
func (p *RecordQueryNestedLoopJoinPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return joinTimes(child)
}

func joinTimes(child []properties.Cardinalities) properties.Cardinalities {
	if len(child) < 2 {
		return properties.UnknownCardinalities()
	}
	return child[0].Times(child[1])
}

// --- recursive operators ----------------------------------------------------

// ProvenCardinalities: the traversal emits every ROOT row (plus whatever
// descendants it finds), so the minimum is the root leg's; the depth is data,
// so the maximum is unknown. Identical to the level union's bound, because the
// two are physical realizations of ONE logical operator and cannot prove
// different row counts for the same recursion.
//
// JAVA DIVERGENCE, deliberate and in the sound direction. Java's PLAN-level
// visitRecordQueryRecursiveDfsJoinPlan returns unknownMaxCardinality (min 0),
// while Java's own LOGICAL visitRecursiveUnionExpression proves
// {initialState.min, unknown} for the same operator — so Java's two arms
// disagree, and the plan arm is the loose one. Go follows the logical arm.
//
// Verified sound against Go's executor rather than assumed: recursive_cursor.go
// pushes each root node with emitPending set (:247), so every root row is
// emitted; UNION DISTINCT deduplication can only collapse duplicates, and a set
// built from at least one row still holds at least one row.
//
// The floor holds for BOTH traversal strategies, which the emitPending flag
// alone does not show. PREORDER emits on the way down (:168, gated on
// c.preorder); POSTORDER never takes that branch, and its rows come out of the
// POP path instead (:203-210), which drains any node still flagged emitPending
// as the walk unwinds — the same flag, consumed at the other end. A
// continuation-restored node re-arms it precisely for that case
// (`emitPending = !c.preorder`, :240): in preorder it was already emitted
// before its children on the prior page and must not repeat, in postorder it
// still owes its emission. So no root row is dropped under DfsPostorder, and
// the bound is not a preorder-only argument.
//
// Leaving it at Java's looser answer is not cosmetic. Measured on identical
// children, a LIMIT-0 recursive leg gives FlatMap(scan, dfsJoin) Cardinality 0
// against FlatMap(scan, levelUnion) 1e6 — so RFC-195's headline zero-collapse
// survived on the DFS alternative, which is the one the cost model PREFERS (the
// level union carries a strictly larger buffer term by construction). The
// clamp cannot floor what the proof does not claim.
func (p *RecordQueryRecursiveDfsJoinPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return recursiveSeedBound(child)
}

// ProvenCardinalities: UNION ALL always emits at least the seed, so the minimum
// is the seed leg's; the recursion depth is unbounded at plan time.
//
// Matching Java's visitRecordQueryRecursiveLevelUnionPlan, which takes the
// initial state's minimum and an unknown maximum.
//
// This minimum is the bound recursiveCost contradicted: it computes
// seedCard*recCard with NO additive seed term, so a recursive leg whose own
// estimate is exactly zero (a LIMIT 0 leg) collapses the product to zero even
// though the seed alone guarantees a row — and a zero propagates
// multiplicatively through FlatMapCost and NestedLoopJoinCost, costing an
// entire join subtree at zero.
func (p *RecordQueryRecursiveLevelUnionPlan) ProvenCardinalities(child []properties.Cardinalities) properties.Cardinalities {
	return recursiveSeedBound(child)
}

// recursiveSeedBound is the ONE bound both recursive physical operators prove:
// at least the seed/root leg emits, and the recursion depth is unknown.
//
// Shared rather than duplicated because the two plans implement the SAME
// logical recursion — Java has a single visitRecursiveUnionExpression for
// exactly that reason. Two copies is how they came to disagree, with the DFS
// side proving nothing and the level union proving a floor.
func recursiveSeedBound(child []properties.Cardinalities) properties.Cardinalities {
	if len(child) == 0 {
		return properties.UnknownMaxCardinality()
	}
	return properties.Cardinalities{
		Min: child[0].GetMinCardinality(),
		Max: properties.UnknownCardinality(),
	}
}

// HintCostWithin: the level union's buffer term is charged per MATERIALIZED
// OUTPUT ROW, so it must be computed from the CLAMPED cardinality — this is the
// one formula in the planner whose CPU derives from its own output rather than
// from what it consumes.
//
// Clamping the returned Cost instead would leave the buffer term computed from
// the disproven number: a one-row seed with a zero-estimated recursive leg pays
// ZERO buffer work, and the clamp then asserts the output is at least one row.
// That erases the level-union-vs-DFS cost distinction the buffer term exists to
// draw — the DFS join buffers no level at all — in the very act of fixing the
// cardinality. The clamp therefore runs on the interval BEFORE the term is
// computed, which is what keeps the emitted Cost internally consistent: no
// component of it is a function of a cardinality the same Cost no longer
// carries.
// It is also the ONLY body: RecordQueryRecursiveLevelUnionPlan.HintCost
// delegates here with an unknown interval (which makes the clamp a no-op), so
// there is exactly one implementation of this formula rather than two that must
// be kept in step. Two bodies is how a safety test comes to exercise the copy
// nothing calls.
func (p *RecordQueryRecursiveLevelUnionPlan) HintCostWithin(
	child []properties.Cost,
	bounds properties.Cardinalities,
	_ properties.StatisticsProvider,
) properties.Cost {
	if len(child) < 2 {
		return properties.Cost{}
	}
	cost := recursiveCost(child)
	cost.Cardinality = properties.ClampCardinality(cost.Cardinality, bounds)
	// The materialized set is the operator's output cardinality; each row pays
	// levelUnionBufferTouches buffer operations at the union merge rate
	// (UnionCPU). Charged on CPU ONLY — the union emits the SAME rows as the DFS
	// join, so its OUTPUT cardinality (which rolls up into parents) must not
	// change.
	cost.CPU += cost.Cardinality * properties.UnionCPU * levelUnionBufferTouches
	return cost
}
