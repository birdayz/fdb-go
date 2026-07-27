package cascades

// This file is a STANDING cross-check between the two independent row-count
// derivations this planner carries:
//
//   - computeCardinalities / CardinalitiesProperty (plan_properties.go,
//     properties/cardinality.go) — a faithful port of Java's
//     CardinalitiesProperty. STRUCTURAL min/max bounds, or an explicit
//     unknown. Never statistics. Trustworthy by construction: e.g. a unique
//     index with every column equality-bound PROVES max=1, not "probably 1".
//   - The cost model (properties/cost_formulas.go, plans/cost.go's HintCost
//     methods, planning_cost_model.go's concretePlanCost) — a Go-only
//     extension with no Java counterpart, producing a POINT ESTIMATE from
//     flat selectivity constants (DistinctSelectivity, FilterSelectivity, …).
//
// A point estimate that exceeds a PROVEN max, or falls below a PROVEN min, is
// wrong by construction — no statistics needed to know it. Two independent
// derivations of "how many rows" can disagree with each other silently
// because nothing checks them against each other; this file is that check.
//
// Design: build real physical plan trees with the PRODUCTION constructors
// (plans.NewXxxPlan), which wire real Reference/Quantifier edges via
// plans.QuantifierOverPlan internally. For each shape, compute BOTH:
//   - the property's bounds, via the SAME priming sequence
//     computeRefPlanProperties runs in production (primeCardinalitiesProperty
//     below primes every child Reference bottom-up, then computeCardinalities
//     reads them exactly as computeWrapperProperties does);
//   - the cost model's point-estimate Cardinality, via the SAME recursion the
//     memo's winner selection uses (properties.EstimateCost).
//
// Assert the estimate falls in [provenMin, provenMax] whenever the property
// proves a bound. Where the property is unknown, nothing is asserted, but the
// gap is real: see the per-shape comments below for where the cost model
// flies blind.
//
// Five violations are DOCUMENTED and KNOWN, not fixed by this file (fixing
// the cost model is out of scope here — this file only proves the gap and
// pins it so nobody re-introduces a regression on top of it). Each is a
// SELF-CLEANING exclusion, the pattern cost_model_total_preorder_test.go
// established for CQ-23/CQ-24: the shape asserts the documented violation
// STILL REPRODUCES, in the documented direction. The moment someone fixes the
// underlying formula, that assertion flips to "no longer reproduces" and
// FAILS, forcing the entry's removal — the exclusion cannot outlive the bug.
import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ============================================================================
// Harness: prime the property bottom-up, then read both sides.
// ============================================================================

// primeCardinalitiesProperty computes and stores the Cardinalities property
// (via computeRefPlanProperties — the exact call the PLANNING phase makes
// after ImplementationRules fire on a group) for every Reference in plan's
// subtree, bottom-up. Each plan constructor mints a FRESH, finals-only
// Reference per child edge (plans.QuantifierOverPlan / QuantifiersOverPlans),
// so every edge in a freshly-built tree starts unprimed; recursing into
// children before priming the parent's edge is what lets
// cardinalitiesFromChildRef read a populated PlanPropertiesMap instead of an
// empty one. A no-op on an already-primed Reference (shared sub-trees are
// primed once).
func primeCardinalitiesProperty(plan plans.RecordQueryPlan) {
	if plan == nil {
		return
	}
	for _, q := range plan.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil || GetRefPlanPropertiesMap(ref) != nil {
			continue
		}
		members := ref.FinalMembers()
		if len(members) == 0 {
			members = ref.AllMembers()
		}
		for _, m := range members {
			if childPlan, ok := m.(plans.RecordQueryPlan); ok {
				primeCardinalitiesProperty(childPlan)
			}
		}
		computeRefPlanProperties(ref)
	}
}

// provenCardinalities returns the STRUCTURAL Cardinalities bound
// computeCardinalities proves for plan, after priming every child
// Reference's PlanPropertiesMap bottom-up (see primeCardinalitiesProperty).
// This is the oracle side of the check: a bound derived from plan SHAPE
// alone, never statistics.
func provenCardinalities(t *testing.T, plan plans.RecordQueryPlan) properties.Cardinalities {
	t.Helper()
	primeCardinalitiesProperty(plan)
	w, ok := plan.(physicalPlanExpression)
	if !ok {
		t.Fatalf("%T does not implement physicalPlanExpression -- every RecordQueryPlan should since RFC-184 W2", plan)
	}
	return computeCardinalities(w, plan)
}

// costCardinality returns the cost model's point-estimate Cardinality for
// plan, via properties.EstimateCost — the SAME recursion the memo's winner
// selection performs. No priming needed on this side: EstimateCost walks
// GetQuantifiers()/ref.Get() directly, and Reference.Get()'s no-exploratory-
// members fallback resolves a finals-only Reference (what QuantifierOverPlan
// mints) straight to its pinned final member.
func costCardinality(plan plans.RecordQueryPlan) float64 {
	return properties.EstimateCost(plan).Cardinality
}

// ============================================================================
// Shape table.
// ============================================================================

// cardinalityViolation classifies a shape's expected relationship between the
// proven bound and the cost estimate.
type cardinalityViolation int

const (
	// noKnownViolation: the standing invariant must hold — cost estimate
	// falls inside [provenMin, provenMax] whenever a bound is proven.
	noKnownViolation cardinalityViolation = iota
	// knownExceedsMax: a DOCUMENTED, NOT YET FIXED defect — the cost
	// estimate exceeds the proven max. Self-cleaning: see cardinalityCostViolations.
	knownExceedsMax
	// knownBelowMin: a DOCUMENTED, NOT YET FIXED defect — the cost estimate
	// falls below the proven min. Self-cleaning: see cardinalityCostViolations.
	knownBelowMin
)

type cardinalityCostShape struct {
	name      string
	build     func(t *testing.T) plans.RecordQueryPlan
	violation cardinalityViolation
	// reason documents, for an excluded shape, what the operator structurally
	// guarantees and why the cost formula misses it. Empty for non-excluded
	// shapes.
	reason string
}

func cardinalityCostShapes() []cardinalityCostShape {
	scan := func(t string) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{t}, values.UnknownType, false)
	}
	pkOf := func(field string) []values.Value {
		return []values.Value{&values.FieldValue{Field: field, Typ: values.NullableLong}}
	}
	// pointLookupScan is a full-PK-equality primary scan: computeCardinalities
	// proves AtMostOne (0,1), and HintCost's isProvablePointProbe branch
	// independently agrees (cardinality=1) -- the two derivations happen to
	// share this one recognizer, so this shape should never disagree.
	pointLookupScan := func(t *testing.T, table, field string) *plans.RecordQueryScanPlan {
		return scan(table).WithPrimaryKey(pkOf(field)).WithScanComparisons(
			[]*predicates.ComparisonRange{equalityRange(t, int64(1))})
	}
	uniquePointLookupIndex := func(t *testing.T, name string) *plans.RecordQueryIndexPlan {
		return plans.NewRecordQueryIndexPlan(name,
			[]*predicates.ComparisonRange{equalityRange(t, int64(1))},
			[]string{"T"}, values.UnknownType, false,
		).WithIndexMetadata([]string{"K"}, []string{"ID"}, true)
	}

	var shapes []cardinalityCostShape
	add := func(name string, build func(t *testing.T) plans.RecordQueryPlan) {
		shapes = append(shapes, cardinalityCostShape{name: name, build: build})
	}
	addExcluded := func(name string, violation cardinalityViolation, reason string, build func(t *testing.T) plans.RecordQueryPlan) {
		shapes = append(shapes, cardinalityCostShape{name: name, build: build, violation: violation, reason: reason})
	}

	// --- scans -----------------------------------------------------------
	add("scan/unbounded", func(t *testing.T) plans.RecordQueryPlan {
		return scan("SCAN_UNB")
	})
	add("scan/pointLookup", func(t *testing.T) plans.RecordQueryPlan {
		return pointLookupScan(t, "SCAN_PL", "ID")
	})

	// --- index scans -------------------------------------------------------
	add("indexScan/nonUnique", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryIndexPlan("IDX_NU", nil, []string{"T"}, values.UnknownType, false)
	})
	add("indexScan/uniquePointLookup", func(t *testing.T) plans.RecordQueryPlan {
		return uniquePointLookupIndex(t, "IDX_U_PL")
	})

	// --- fetch (1:1, transparent) -------------------------------------------
	add("fetch/overNonCoveringUnboundedIndex", func(t *testing.T) plans.RecordQueryPlan {
		idx := plans.NewRecordQueryIndexPlan("IDX_FETCH_UNB", nil, []string{"T"}, values.UnknownType, false)
		return plans.NewRecordQueryFetchFromPartialRecordPlan(idx, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	})
	add("fetch/overUniquePointLookupIndex", func(t *testing.T) plans.RecordQueryPlan {
		idx := uniquePointLookupIndex(t, "IDX_FETCH_PL")
		return plans.NewRecordQueryFetchFromPartialRecordPlan(idx, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	})

	// --- filter / predicates filter (varying predicate counts) -------------
	add("filter/onePred/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{rungPredicate("A")}, pointLookupScan(t, "FILT1", "ID"))
	})
	add("predicatesFilter/twoPreds/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryPredicatesFilterPlan(
			pointLookupScan(t, "FILT2", "ID"),
			[]predicates.QueryPredicate{rungPredicate("A"), rungPredicate("B")})
	})

	// --- type filter (transparent) ------------------------------------------
	add("typeFilter/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, pointLookupScan(t, "TF", "ID"))
	})

	// --- projection / map (cardinality-preserving) --------------------------
	add("projection/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryProjectionPlan(
			[]values.Value{&values.FieldValue{Field: "K", Typ: values.NullableLong}}, pointLookupScan(t, "PROJ", "ID"))
	})
	add("map/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryMapPlan(pointLookupScan(t, "MAP", "ID"), nil)
	})

	// --- FlatMap (correlated join): Times(outer, inner) --------------------
	add("flatMap/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryFlatMapPlan(
			pointLookupScan(t, "FM_O", "ID"), pointLookupScan(t, "FM_I", "ID"),
			values.NamedCorrelationIdentifier("fm_o"), values.NamedCorrelationIdentifier("fm_i"),
			nil, false)
	})
	add("flatMap/boundedOuterUnboundedInner_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		// The inner is unbounded, so the property's Times() propagates unknown
		// max -- nothing is asserted here, but the cost model still produces a
		// confident point estimate (outer*inner = 1e6). This is a documented
		// BLIND SPOT, not a violation: no proven bound exists to check against.
		return plans.NewRecordQueryFlatMapPlan(
			pointLookupScan(t, "FM_O2", "ID"), scan("FM_I2"),
			values.NamedCorrelationIdentifier("fm_o2"), values.NamedCorrelationIdentifier("fm_i2"),
			nil, false)
	})

	// --- NestedLoopJoin (materialized): Times(outer, inner) ----------------
	add("nestedLoopJoin/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryNestedLoopJoinPlan(
			pointLookupScan(t, "NLJ_O", "ID"), pointLookupScan(t, "NLJ_I", "ID"),
			[]predicates.QueryPredicate{rungPredicate("A")}, plans.JoinInner, "O", "I", nil)
	})

	// --- InJoin: child cardinalities scaled by in-list size ----------------
	add("inJoin/pointProbeInner/threeValues", func(t *testing.T) plans.RecordQueryPlan {
		p := plans.NewRecordQueryInJoinPlan(pointLookupScan(t, "INJ_PP", "ID"), "inj_binding", false, false)
		p.SetInValues([]any{int64(1), int64(2), int64(3)})
		return p
	})
	add("inJoin/unboundedInList_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		// No SetInValues call -> the in-list size is unknown at plan time, so
		// the property abstains (UnknownMaxCardinality) -- documented BLIND
		// SPOT: the cost model still charges a conservative default (10 rows).
		return plans.NewRecordQueryInJoinPlan(pointLookupScan(t, "INJ_UNK", "ID"), "inj_binding2", false, false)
	})

	// --- InUnion: child cardinalities scaled by literal-source fanout -------
	add("inUnion/knownFanoutThree", func(t *testing.T) plans.RecordQueryPlan {
		p := plans.NewRecordQueryInUnionPlan(
			pointLookupScan(t, "INU_PP", "ID"), []string{"inu_b"},
			[]values.Value{&values.FieldValue{Field: "K", Typ: values.NullableLong}}, false)
		p.SetInSources([][]any{{int64(1), int64(2), int64(3)}})
		return p
	})
	add("inUnion/emptySource", func(t *testing.T) plans.RecordQueryPlan {
		p := plans.NewRecordQueryInUnionPlan(
			pointLookupScan(t, "INU_EMPTY", "ID"), []string{"inu_b2"},
			[]values.Value{&values.FieldValue{Field: "K", Typ: values.NullableLong}}, false)
		p.SetInSources([][]any{{}})
		return p
	})

	// --- union variants: sum of children ------------------------------------
	add("union/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "UN_A", "ID"), pointLookupScan(t, "UN_B", "ID"),
		})
	})
	add("mergeSortUnion/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryMergeSortUnionPlan(
			[]plans.RecordQueryPlan{pointLookupScan(t, "MSU_A", "ID"), pointLookupScan(t, "MSU_B", "ID")},
			nil, false, true)
	})
	add("unorderedUnion/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "UU_A", "ID"), pointLookupScan(t, "UU_B", "ID"),
		})
	})

	// --- intersection: min-of-mins/maxes -------------------------------------
	add("intersection/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "INT_A", "ID"), pointLookupScan(t, "INT_B", "ID"),
		}, nil)
	})
	add("intersection/oneUnbounded_blindSpotOnMax", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "INT_C", "ID"), scan("INT_D"),
		}, nil)
	})

	// --- distinct (both variants): KNOWN VIOLATIONS below min --------------
	// Both use the SAME child: FirstOrDefault produces EXACTLY one row
	// (property ExactlyOne(), cost also exactly 1 -- FirstOrDefaultCost floors
	// unconditionally), so both distinct operators' PROPERTY says min=1 (an
	// exactly-one-row input can't become fewer rows), but their COST applies a
	// flat 0.7 selectivity multiplier with no floor at 1: 1*0.7=0.7 < 1.
	distinctChild := func(t *testing.T) *plans.RecordQueryFirstOrDefaultPlan {
		return plans.NewRecordQueryFirstOrDefaultPlan(scan("DISTINCT_SRC"), values.NewNullValue(values.UnknownType))
	}
	addExcluded("distinct/overExactlyOneChild", knownBelowMin,
		"RecordQueryDistinctPlan reports the child's bounds UNCHANGED (transparent) -- an exactly-one-row "+
			"child structurally proves min=1, but DistinctCost applies a flat DistinctSelectivity=0.7 with no "+
			"floor, so 1 row costs out to 0.7",
		func(t *testing.T) plans.RecordQueryPlan {
			return plans.NewRecordQueryDistinctPlan(distinctChild(t))
		})
	addExcluded("unorderedPrimaryKeyDistinct/overExactlyOneChild", knownBelowMin,
		"RecordQueryUnorderedPrimaryKeyDistinctPlan's OWN property explicitly floors min at 1 when the child's "+
			"min is known-nonzero (an exactly-one-row child dedups to exactly one), but it shares DistinctCost's "+
			"same unfloored 0.7 multiplier, so 1 row still costs out to 0.7",
		func(t *testing.T) plans.RecordQueryPlan {
			return plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(distinctChild(t))
		})
	add("distinct/overUnboundedChild_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryDistinctPlan(scan("DISTINCT_UNB"))
	})

	// --- sort (transparent) --------------------------------------------------
	add("inMemorySort/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryInMemorySortPlan(pointLookupScan(t, "SORT", "ID"), nil)
	})

	// --- limit, including LIMIT 0 --------------------------------------------
	add("limit/cappedBelowChild", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryLimitPlan(scan("LIMIT5"), 5, 0)
	})
	add("limit/zero", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryLimitPlan(scan("LIMIT0"), 0, 0)
	})
	add("limit/runtimeValue_transparentBothSides", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryLimitPlanWithValue(
			scan("LIMIT_RT"), &values.FieldValue{Field: "N", Typ: values.NullableLong}, 0)
	})

	// --- aggregate index (leaf, always unknown-max) --------------------------
	add("aggregateIndex/leaf_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		idx := plans.NewRecordQueryIndexPlan("AGG_IDX", nil, []string{"T"}, values.UnknownType, false)
		return plans.NewRecordQueryAggregateIndexPlan(idx, "T", values.UnknownType, "COUNT")
	})

	// --- streaming aggregation: grouped (unknown) vs ungrouped (KNOWN VIOLATION) ---
	add("streamingAggregation/grouped", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryStreamingAggregationPlan(
			scan("SAGG_GROUPED"), []values.Value{&values.FieldValue{Field: "K", Typ: values.NullableLong}}, nil)
	})
	addExcluded("streamingAggregation/ungrouped", knownExceedsMax,
		"an ungrouped RecordQueryStreamingAggregationPlan structurally emits EXACTLY ONE output row (property "+
			"proves max=1), but its HintCost formula charges in*DistinctSelectivity with no cap at 1 -- for a "+
			"1e6-row child that is ~700,000, wildly exceeding the proven max",
		func(t *testing.T) plans.RecordQueryPlan {
			return plans.NewRecordQueryStreamingAggregationPlan(scan("SAGG_UNGROUPED"), nil, nil)
		})

	// --- recursive DFS join (always unknown-max) ------------------------------
	add("recursiveDfsJoin/alwaysUnknownMax_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryRecursiveDfsJoinPlan(
			scan("DFS_ROOT"), scan("DFS_CHILD"), values.NamedCorrelationIdentifier("dfs_prior"), plans.DfsPreorder)
	})

	// --- recursive level union: KNOWN VIOLATION when the recursive leg's cost
	// estimate is below 1 ------------------------------------------------------
	add("recursiveLevelUnion/normalRecursion", func(t *testing.T) plans.RecordQueryPlan {
		// Seed and recursive legs both cost exactly 1 (Values-plan / point-
		// lookup style leaves) -- recursiveCost = 1*1 = 1, which meets the
		// property's proven min (seed.Min = 1). No violation: the defect below
		// needs the recursive leg's OWN cost estimate to drop under 1, which a
		// plain point-lookup leg does not do.
		seed := plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
		rec := plans.NewRecordQueryScanPlan([]string{"LU_REC"}, values.UnknownType, false).
			WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.NullableLong}}).
			WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(1))})
		return plans.NewRecordQueryRecursiveLevelUnionPlan(
			seed, rec, values.NamedCorrelationIdentifier("lu_scan"), values.NamedCorrelationIdentifier("lu_insert"))
	})
	addExcluded("recursiveLevelUnion/recursiveLegCollapsesTowardZero", knownBelowMin,
		"the property proves min=seed.Min (UNION ALL always emits at least the seed: a RecordQueryFirstOrDefaultPlan "+
			"seed structurally guarantees exactly one row), but recursiveCost computes seedCard*recCard with NO "+
			"additive seed term -- when the recursive leg's own cost estimate is exactly zero (a LIMIT 0 leg), the "+
			"product collapses to zero even though the seed alone guarantees at least one row",
		func(t *testing.T) plans.RecordQueryPlan {
			seed := plans.NewRecordQueryFirstOrDefaultPlan(scan("LU_SEED"), values.NewNullValue(values.UnknownType))
			rec := plans.NewRecordQueryLimitPlan(scan("LU_REC_ZERO"), 0, 0)
			return plans.NewRecordQueryRecursiveLevelUnionPlan(
				seed, rec, values.NamedCorrelationIdentifier("lu_scan2"), values.NamedCorrelationIdentifier("lu_insert2"))
		})

	// --- explode (leaf) --------------------------------------------------------
	add("explode/literalCollection", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{1, 2, 3, 4}, Typ: values.UnknownType})
	})
	add("explode/nonLiteralCollection_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryExplodePlan(&values.FieldValue{Field: "TAGS", Typ: values.UnknownType})
	})

	// --- values (leaf, exactly one row) -----------------------------------------
	add("values/leaf", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
	})

	// --- temp-table scan (leaf, always unknown-max) -----------------------------
	add("tempTableScan/leaf_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		return plans.NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt_alias"))
	})

	// --- DefaultOnEmpty: KNOWN VIOLATION when the child's cost is genuinely zero --
	add("defaultOnEmpty/overNonZeroChild", func(t *testing.T) plans.RecordQueryPlan {
		// Child already costs >=1 (a point lookup), so the un-floored HintCost
		// happens to already satisfy the property's Floor(1) -- NOT a
		// violation. Contrast with the excluded shape below, where the child's
		// cost estimate is a genuine zero.
		return plans.NewRecordQueryDefaultOnEmptyPlan(
			pointLookupScan(t, "DOE_OK", "ID"), values.NewNullValue(values.UnknownType))
	})
	addExcluded("defaultOnEmpty/overZeroCostChild", knownBelowMin,
		"the property applies child.Floor(1) (DefaultOnEmpty guarantees at least one row, real-or-default), but "+
			"HintCost returns the child's cost UNCHANGED with no floor of its own -- when the child's own cost "+
			"estimate is a genuine zero (a LIMIT 0 child), the DefaultOnEmpty cost stays zero. Contrast "+
			"RecordQueryFirstOrDefaultPlan.HintCost (FirstOrDefaultCost), which floors unconditionally for the "+
			"identical structural reason -- the same floor is simply missing here",
		func(t *testing.T) plans.RecordQueryPlan {
			return plans.NewRecordQueryDefaultOnEmptyPlan(
				plans.NewRecordQueryLimitPlan(scan("DOE_ZERO"), 0, 0), values.NewNullValue(values.UnknownType))
		})

	// --- typeFilter over an EXACTLY-ONE child: KNOWN VIOLATION below min ------
	// Found by the random-combo generator below (TypeFilter/Values(1) composes
	// two building blocks neither the fixed shapes above ever pairs directly):
	// RecordQueryValuesPlan proves ExactlyOne (min=1, max=1) -- it is a
	// literal row, deterministic, no "might not exist" the way a point-lookup
	// scan has -- but TypeFilterCost applies its flat TypeFilterSelectivity=0.5
	// with no floor, same shape as the Distinct/StreamingAggregation-ungrouped
	// exclusions above, just with TypeFilter as the unfloored operator. The
	// fixed typeFilter/overBoundedChild shape above cannot see this: its child
	// (pointLookupScan) proves AtMostOne (min=0), and 0.5 >= 0 never violates.
	addExcluded("typeFilter/overExactlyOneChild", knownBelowMin,
		"RecordQueryTypeFilterPlan reports the child's bounds UNCHANGED (transparent, matching Java's "+
			"CardinalitiesVisitor) -- an exactly-one-row child (RecordQueryValuesPlan) structurally proves min=1, "+
			"but TypeFilterCost applies a flat TypeFilterSelectivity=0.5 with no floor, so 1 row costs out to 0.5",
		func(t *testing.T) plans.RecordQueryPlan {
			return plans.NewRecordQueryTypeFilterPlan([]string{"T1"},
				plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))}))
		})

	// --- FirstOrDefault: always exactly one, and the cost model agrees --------
	add("firstOrDefault/overZeroCostChild_costFloorsCorrectly", func(t *testing.T) plans.RecordQueryPlan {
		// Direct contrast with defaultOnEmpty/overZeroCostChild above:
		// FirstOrDefaultCost floors unconditionally (Cardinality:1 regardless
		// of the child), so this NEVER violates even over a zero-cost child --
		// proof the floor is a simple, already-known-correct fix, not a hard
		// problem.
		return plans.NewRecordQueryFirstOrDefaultPlan(
			plans.NewRecordQueryLimitPlan(scan("FOD_ZERO"), 0, 0), values.NewNullValue(values.UnknownType))
	})

	return shapes
}

// ============================================================================
// The check itself, and the standing test.
// ============================================================================

// cardinalityCostViolations returns a human-readable violation message per
// broken expectation for sh's (prov, cost) pair, or nil when everything
// holds. Pure (no *testing.T) so the mutation-guard test below can exercise
// it directly without ever causing an actually-failing subtest to live
// permanently in the suite.
func cardinalityCostViolations(sh cardinalityCostShape, prov properties.Cardinalities, cost float64) []string {
	var out []string
	switch sh.violation {
	case noKnownViolation:
		if !prov.Min.IsUnknown() && cost < float64(prov.Min.Value()) {
			out = append(out, fmt.Sprintf(
				"%s: cost estimate %.4f is BELOW the proven min %d -- impossible by construction",
				sh.name, cost, prov.Min.Value()))
		}
		if !prov.Max.IsUnknown() && cost > float64(prov.Max.Value()) {
			out = append(out, fmt.Sprintf(
				"%s: cost estimate %.4f EXCEEDS the proven max %d -- impossible by construction",
				sh.name, cost, prov.Max.Value()))
		}
	case knownExceedsMax:
		if prov.Max.IsUnknown() {
			out = append(out, fmt.Sprintf(
				"%s: exclusion expects a proven max, but the property now reports unknown -- "+
					"shape changed, update or remove this exclusion entry", sh.name))
		} else if cost <= float64(prov.Max.Value()) {
			out = append(out, fmt.Sprintf(
				"%s: documented cost>provenMax violation NO LONGER REPRODUCES (cost=%.4f provenMax=%d) -- "+
					"the underlying formula was fixed, DELETE this exclusion entry (%s)",
				sh.name, cost, prov.Max.Value(), sh.reason))
		}
	case knownBelowMin:
		if prov.Min.IsUnknown() {
			out = append(out, fmt.Sprintf(
				"%s: exclusion expects a proven min, but the property now reports unknown -- "+
					"shape changed, update or remove this exclusion entry", sh.name))
		} else if cost >= float64(prov.Min.Value()) {
			out = append(out, fmt.Sprintf(
				"%s: documented cost<provenMin violation NO LONGER REPRODUCES (cost=%.4f provenMin=%d) -- "+
					"the underlying formula was fixed, DELETE this exclusion entry (%s)",
				sh.name, cost, prov.Min.Value(), sh.reason))
		}
	}
	return out
}

// assertCardinalityCostBound is cardinalityCostViolations wired to *testing.T.
func assertCardinalityCostBound(t *testing.T, sh cardinalityCostShape, prov properties.Cardinalities, cost float64) {
	t.Helper()
	for _, v := range cardinalityCostViolations(sh, prov, cost) {
		t.Error(v)
	}
	if sh.violation != noKnownViolation {
		t.Logf("%s: KNOWN violation still reproduces (cost=%.4f) -- %s", sh.name, cost, sh.reason)
	} else if prov.Min.IsUnknown() && prov.Max.IsUnknown() {
		t.Logf("%s: property proves NO bound at all (blind spot) -- cost estimate %.4f is unchecked", sh.name, cost)
	} else if prov.Max.IsUnknown() {
		t.Logf("%s: property proves min=%d but max is UNKNOWN (blind spot on the upper bound) -- "+
			"cost estimate %.4f is unchecked above", sh.name, prov.Min.Value(), cost)
	} else if prov.Min.IsUnknown() {
		t.Logf("%s: property proves max=%d but min is UNKNOWN (blind spot on the lower bound) -- "+
			"cost estimate %.4f is unchecked below", sh.name, prov.Max.Value(), cost)
	}
}

// TestCardinalityPropertyBoundsCostEstimate is the standing check: for every
// physical plan shape below, the cost model's Cardinality point estimate must
// fall inside the bound CardinalitiesProperty structurally proves, wherever a
// bound is proven at all. See the file doc comment for the full rationale and
// the exclusion-list self-cleaning mechanism.
func TestCardinalityPropertyBoundsCostEstimate(t *testing.T) {
	t.Parallel()
	for _, sh := range cardinalityCostShapes() {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			plan := sh.build(t)
			prov := provenCardinalities(t, plan)
			cost := costCardinality(plan)
			assertCardinalityCostBound(t, sh, prov, cost)
		})
	}
}

// TestCardinalityPropertyBoundsCostEstimate_MutationGuard proves the check
// above can actually FAIL, not just always report success. It takes a shape
// that legitimately passes (a plan-time LIMIT, whose proven max the cost
// model matches exactly), perturbs the proven max down by one in a LOCAL
// copy, and confirms cardinalityCostViolations flags it. A bound check that
// can never fail is worthless; this is the permanent, always-green
// demonstration that this one can -- without ever landing an intentionally
// failing subtest in the suite (see cardinalityCostViolations' doc comment).
func TestCardinalityPropertyBoundsCostEstimate_MutationGuard(t *testing.T) {
	t.Parallel()
	plan := plans.NewRecordQueryLimitPlan(
		plans.NewRecordQueryScanPlan([]string{"MUTGUARD"}, values.UnknownType, false), 5, 0)
	sh := cardinalityCostShape{name: "mutation-guard/limit5"}

	prov := provenCardinalities(t, plan)
	cost := costCardinality(plan)
	if v := cardinalityCostViolations(sh, prov, cost); len(v) != 0 {
		t.Fatalf("mutation guard shape must pass the REAL check first, got violations: %v", v)
	}
	if prov.Max.IsUnknown() {
		t.Fatal("mutation guard shape must have a known proven max to perturb")
	}

	perturbed := prov
	perturbed.Max = properties.OfCardinality(prov.Max.Value() - 1)
	v := cardinalityCostViolations(sh, perturbed, cost)
	if len(v) == 0 {
		t.Fatalf("MUTATION CHECK FAILED: perturbing provenMax down by 1 (from %d to %d) against cost=%v "+
			"produced NO violation -- the check does not actually fire",
			prov.Max.Value(), prov.Max.Value()-1, cost)
	}
	t.Logf("mutation check OK: cost=%v provenMax=%v (real, passes); perturbed provenMax=%v correctly flagged: %v",
		cost, prov.Max.Value(), prov.Max.Value()-1, v)
}

// ============================================================================
// Random combos: the fixed shape table above scaled arbitrarily wide.
// ============================================================================
//
// The standing invariant (cost estimate inside [provenMin, provenMax]
// whenever a bound is proven) needs NO reference oracle to check — both
// sides come from the plan itself, so a random COMPOSITION of the same
// well-behaved building blocks costs nothing extra to verify and can run at
// however many combos time allows. This is the self-checking-oracle
// equivalent of rowdiff/sargability's random generators, sized for a
// PR-gate-safe default and a nightly deep sweep via env, exactly like
// SARG_ORACLE_COMBOS/SARG_ORACLE_SEED:
//
//	CARDINALITY_COMBOS=200000 CARDINALITY_SEED=99 bazelisk test \
//	  //pkg/recordlayer/query/plan/cascades:cascades_test \
//	  --test_arg="--test.run=TestCardinalityPropertyBoundsCostEstimate_RandomCombos" \
//	  --test_arg="-test.v"
//
// The random generator draws ONLY from operators the fixed shape table
// documents as noKnownViolation (scan/indexScan/fetch/filter/typeFilter/
// projection/map/flatMap/nestedLoopJoin/union variants/intersection/sort) —
// it deliberately excludes every operator with a DOCUMENTED violation
// (Distinct, ungrouped StreamingAggregation, DefaultOnEmpty-over-zero-cost,
// the collapsing RecursiveLevelUnion shape, …), because those violations are
// conditional on a SPECIFIC child shape the fixed table constructs
// deliberately; composing them into arbitrary random trees would require
// re-deriving, per random shape, whether the violation propagates — turning
// a scale exercise into a source of false, un-investigatable failures. The
// well-behaved building blocks have no such precondition, so ANY composition
// of them is a valid noKnownViolation check.

func cardinalityComboCount(defaultN int) int {
	if v := os.Getenv("CARDINALITY_COMBOS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultN
}

func cardinalityComboSeed(defaultSeed uint64) uint64 {
	if v := os.Getenv("CARDINALITY_SEED"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultSeed
}

// randCardinalityPlan builds a random tree of noKnownViolation building
// blocks, up to depth deep. nextCorr mints a fresh, never-reused correlation
// identifier per FlatMap edge (kept out of the recursion's own state so the
// caller controls it, and so this function stays free of any shared
// mutable package state a concurrent caller would race on).
func randCardinalityPlan(t *testing.T, rng *rand.Rand, depth int, nextCorr func(prefix string) values.CorrelationIdentifier) plans.RecordQueryPlan {
	scan := func(name string) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{name}, values.UnknownType, false)
	}
	// Deliberately NO RecordQueryValuesPlan (ExactlyOne, min=1) in this leaf
	// pool: composed under a TypeFilter anywhere in the random tree, it
	// reproduces the ALREADY-KNOWN, ALREADY-PINNED
	// "typeFilter/overExactlyOneChild" violation above on every run — a
	// permanently red nightly for a gap that is already documented and
	// tracked, not a new finding. Every leaf here proves either UNKNOWN
	// bounds or AtMostOne (min=0), so no composition of transparent
	// operators over it can ever push a cost estimate below a proven floor
	// of 1 — keeping this generator a signal for genuinely NEW composition
	// gaps instead of a rediscovery engine for this one.
	leaf := func() plans.RecordQueryPlan {
		switch rng.IntN(3) {
		case 0:
			return scan("RC_SCAN")
		case 1:
			return scan("RC_PL").
				WithPrimaryKey([]values.Value{&values.FieldValue{Field: "ID", Typ: values.NullableLong}}).
				WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(1))})
		default:
			return plans.NewRecordQueryIndexPlan("RC_IDX", nil, []string{"RC_IDX_T"}, values.UnknownType, false)
		}
	}
	if depth <= 0 || rng.IntN(3) == 0 {
		return leaf()
	}
	child := func() plans.RecordQueryPlan { return randCardinalityPlan(t, rng, depth-1, nextCorr) }
	switch rng.IntN(11) {
	case 0:
		return plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{rungPredicate("A")}, child())
	case 1:
		return plans.NewRecordQueryPredicatesFilterPlan(child(),
			[]predicates.QueryPredicate{rungPredicate("A"), rungPredicate("B")})
	case 2:
		return plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, child())
	case 3:
		return plans.NewRecordQueryProjectionPlan(
			[]values.Value{&values.FieldValue{Field: "K", Typ: values.NullableLong}}, child())
	case 4:
		return plans.NewRecordQueryMapPlan(child(), nil)
	case 5:
		return plans.NewRecordQueryFlatMapPlan(child(), child(),
			nextCorr("rc_o"), nextCorr("rc_i"), nil, false)
	case 6:
		return plans.NewRecordQueryNestedLoopJoinPlan(child(), child(),
			[]predicates.QueryPredicate{rungPredicate("A")}, plans.JoinInner, "O", "I", nil)
	case 7:
		return plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{child(), child()})
	case 8:
		return plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{child(), child()})
	case 9:
		return plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{child(), child()}, nil)
	default:
		return plans.NewRecordQueryInMemorySortPlan(child(), nil)
	}
}

// TestCardinalityPropertyBoundsCostEstimate_RandomCombos scales the standing
// check (TestCardinalityPropertyBoundsCostEstimate) to however many random
// compositions the caller budgets, at a tiny PR-gate default and orders of
// magnitude more in the nightly deep sweep — see the file-section doc
// comment above for why this needs no reference oracle and stays free of
// false failures. t.Parallel() covers this test's scheduling relative to its
// SIBLINGS; the combo loop itself runs single-threaded in one goroutine (the
// correlation-identifier counter is not synchronized), which is what keeps
// it race-free without needing one.
func TestCardinalityPropertyBoundsCostEstimate_RandomCombos(t *testing.T) {
	t.Parallel()
	n := cardinalityComboCount(50)
	seed := cardinalityComboSeed(20260726)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	var corrCounter int
	nextCorr := func(prefix string) values.CorrelationIdentifier {
		corrCounter++
		return values.NamedCorrelationIdentifier(fmt.Sprintf("%s%d", prefix, corrCounter))
	}

	for i := 0; i < n; i++ {
		plan := randCardinalityPlan(t, rng, 4, nextCorr)
		prov := provenCardinalities(t, plan)
		cost := costCardinality(plan)
		sh := cardinalityCostShape{name: fmt.Sprintf("random/combo_%d", i)}
		for _, v := range cardinalityCostViolations(sh, prov, cost) {
			t.Errorf("combo %d (seed %d): %s\n  plan:\n%s", i, seed, v, plan.Explain())
		}
	}
	t.Logf("cardinality/cost random combos: %d checked (seed %d)", n, seed)
}
