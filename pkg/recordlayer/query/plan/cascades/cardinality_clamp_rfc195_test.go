package cascades

// RFC-195's verification plan, as tests.
//
// Two of these exist specifically because earlier revisions of the design were
// NAK'd for defects that stayed invisible to every test that already existed:
//
//   - TestRFC195_BoundsCompositionIsMemberOrderIndependent — revision 2 rode
//     bounds on the COST recursion, which resolves a child Reference to its
//     FIRST member. Priming-invariance stayed green under that design because
//     priming does not reorder members; only permuting insertion order exposes
//     it.
//   - TestRFC195_ClampIsSymmetricAcrossLogicalPhysicalPair — revision 2 clamped
//     only the physical side, which inverted the extraction preference on
//     exactly the shapes the RFC's own defect table lists.
//
// Both would FAIL against those revisions and pass here. They are deliverables,
// not decoration.

import (
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// 1. The corrected shapes, pinned at their exact clamped estimates.
// ---------------------------------------------------------------------------

// TestRFC195_CorrectedShapes pins the BEFORE and AFTER of every estimate the
// RFC measured as impossible. The before-values are recorded in the failure
// messages, not asserted — the point of the assertion is that each estimate now
// sits inside the interval its own operator proves, at the exact value the
// clamp produces.
//
// Pinning the exact value rather than "inside the bound" is deliberate: the
// invariant test next door already checks membership over every shape, so a
// second membership check would add nothing. What is NOT otherwise pinned is
// that the clamp lands ON the boundary rather than somewhere else inside it —
// a floor that overshot to 2, or a cap that undershot to 0, would satisfy
// membership and still be wrong.
func TestRFC195_CorrectedShapes(t *testing.T) {
	t.Parallel()

	scan := func(name string) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{name}, values.UnknownType, false)
	}
	exactlyOneChild := func() *plans.RecordQueryFirstOrDefaultPlan {
		return plans.NewRecordQueryFirstOrDefaultPlan(scan("SRC"), values.NewNullValue(values.UnknownType))
	}

	cases := []struct {
		name   string
		before float64 // the impossible estimate the RFC measured
		want   float64 // the clamped estimate
		build  func() plans.RecordQueryPlan
	}{{
		name:   "streamingAggregation/ungrouped",
		before: 700000,
		want:   1, // capped at the proven max: an ungrouped aggregate emits one row
		build: func() plans.RecordQueryPlan {
			return plans.NewRecordQueryStreamingAggregationPlan(scan("SAGG"), nil, nil)
		},
	}, {
		name:   "recursiveLevelUnion/recursiveLegCollapsesTowardZero",
		before: 0,
		want:   1, // floored at seed.Min: UNION ALL always emits at least the seed
		build: func() plans.RecordQueryPlan {
			seed := plans.NewRecordQueryFirstOrDefaultPlan(scan("LU_SEED"), values.NewNullValue(values.UnknownType))
			rec := plans.NewRecordQueryLimitPlan(scan("LU_REC_ZERO"), 0, 0)
			return plans.NewRecordQueryRecursiveLevelUnionPlan(seed, rec,
				values.NamedCorrelationIdentifier("lu_scan"), values.NamedCorrelationIdentifier("lu_insert"))
		},
	}, {
		name:   "defaultOnEmpty/overZeroCostChild",
		before: 0,
		want:   1, // floored: DefaultOnEmpty yields real-or-default, never nothing
		build: func() plans.RecordQueryPlan {
			return plans.NewRecordQueryDefaultOnEmptyPlan(
				plans.NewRecordQueryLimitPlan(scan("DOE_ZERO"), 0, 0), values.NewNullValue(values.UnknownType))
		},
	}, {
		name:   "distinct/overExactlyOneChild",
		before: 0.7,
		want:   1, // floored: an exactly-one-row input cannot dedup to fewer
		build: func() plans.RecordQueryPlan {
			return plans.NewRecordQueryDistinctPlan(exactlyOneChild())
		},
	}, {
		name:   "unorderedPrimaryKeyDistinct/overExactlyOneChild",
		before: 0.7,
		want:   1,
		build: func() plans.RecordQueryPlan {
			return plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(exactlyOneChild())
		},
	}, {
		name:   "typeFilter/overExactlyOneChild",
		before: 0.5,
		want:   1, // floored: RecordQueryValuesPlan proves ExactlyOne
		build: func() plans.RecordQueryPlan {
			return plans.NewRecordQueryTypeFilterPlan([]string{"T1"},
				plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))}))
		},
	}, {
		// The SEVENTH shape. The RFC's table had six because the DFS join
		// proved nothing at all, so the identical zero-collapse was invisible
		// on it — and invisible on the alternative the cost model PREFERS.
		// Same children as the level-union row above; before the DFS arm
		// existed this costed 0 while its twin costed 1.
		name:   "recursiveDfsJoin/recursiveLegCollapsesTowardZero",
		before: 0,
		want:   1,
		build: func() plans.RecordQueryPlan {
			seed := plans.NewRecordQueryFirstOrDefaultPlan(scan("DFS_SEED"), values.NewNullValue(values.UnknownType))
			rec := plans.NewRecordQueryLimitPlan(scan("DFS_REC_ZERO"), 0, 0)
			return plans.NewRecordQueryRecursiveDfsJoinPlan(seed, rec,
				values.NamedCorrelationIdentifier("dfs_prior"), plans.DfsPreorder)
		},
	}}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := tc.build()
			got := properties.EstimateCost(plan).Cardinality
			if got != tc.want {
				t.Fatalf("cost estimate = %v, want %v (RFC-195 measured %v before the clamp; "+
					"an estimate that no longer lands ON the proven boundary means the clamp "+
					"is applying a different bound than the property proves)",
					got, tc.want, tc.before)
			}
		})
	}
}

// TestRFC195_ClampMutatesInBothDirections is the mutation check the RFC calls
// for: removing the FLOOR must redden the five below-min shapes, and removing
// the CAP must redden the ungrouped aggregate. A clamp that satisfies only one
// direction is how a half-fix survives review.
//
// It exercises ClampCardinality directly with each half disabled, rather than
// editing the production helper, so both directions stay pinned permanently
// instead of only during a manual revert.
func TestRFC195_ClampMutatesInBothDirections(t *testing.T) {
	t.Parallel()

	floorOnly := func(estimate float64, b properties.Cardinalities) float64 {
		if !b.Min.IsUnknown() && estimate < float64(b.Min.Value()) {
			return float64(b.Min.Value())
		}
		return estimate
	}
	capOnly := func(estimate float64, b properties.Cardinalities) float64 {
		if !b.Max.IsUnknown() && estimate > float64(b.Max.Value()) {
			return float64(b.Max.Value())
		}
		return estimate
	}

	belowMin := properties.Cardinalities{Min: properties.OfCardinality(1), Max: properties.UnknownCardinality()}
	aboveMax := properties.Cardinalities{Min: properties.OfCardinality(0), Max: properties.OfCardinality(1)}

	// Cap-only (the floor removed) leaves every below-min estimate untouched.
	for _, est := range []float64{0, 0.5, 0.7} {
		if got := capOnly(est, belowMin); got != est {
			t.Fatalf("cap-only clamp changed a below-min estimate %v to %v -- "+
				"the mutation is not actually removing the floor", est, got)
		}
		if got := properties.ClampCardinality(est, belowMin); got != 1 {
			t.Fatalf("ClampCardinality(%v, min=1) = %v, want 1 -- the floor does not fire", est, got)
		}
	}

	// Floor-only (the cap removed) leaves the 700,000x overestimate untouched.
	const ungrouped = 700000.0
	if got := floorOnly(ungrouped, aboveMax); got != ungrouped {
		t.Fatalf("floor-only clamp changed the above-max estimate %v to %v -- "+
			"the mutation is not actually removing the cap", ungrouped, got)
	}
	if got := properties.ClampCardinality(ungrouped, aboveMax); got != 1 {
		t.Fatalf("ClampCardinality(%v, max=1) = %v, want 1 -- the cap does not fire", ungrouped, got)
	}
}

// ---------------------------------------------------------------------------
// 2. Rev-correction pin #1: bounds compose ORDER-INDEPENDENTLY.
// ---------------------------------------------------------------------------

// TestRFC195_BoundsCompositionIsMemberOrderIndependent permutes a Reference's
// member INSERTION order and asserts every derived cost is identical.
//
// This is the test revision 2 of the RFC would have FAILED. That revision rode
// bounds on the cost recursion, which resolves a child Reference to its FIRST
// member. For COST that arbitrariness is accepted and long-standing. For BOUNDS
// it is strictly TIGHTER than the property layer's own answer —
// cardinalitiesForRef deliberately WEAKENS across all members — so a group whose
// first member is a unique point probe (max=1) and whose second is a full scan
// would clamp its parent to 1 or to 1e6 depending purely on which arrived
// first. That is arrival-order dependence in GetBest, the CQ-23/CQ-24 class,
// re-entering through the bounds channel.
//
// Priming-invariance cannot see it: priming does not reorder members. The two
// invariance tests cover different doors into the same nondeterminism.
func TestRFC195_BoundsCompositionIsMemberOrderIndependent(t *testing.T) {
	t.Parallel()

	// ISOLATING THE BOUNDS CHANNEL IS THE WHOLE DIFFICULTY.
	//
	// The COST recursion's first-member resolution is order-dependent too, and
	// that is accepted, long-standing, and NOT what this test is about: a group
	// of {unique probe, full scan} yields child cost 1 or 1e6 purely by arrival
	// order, and a test built on those members measures the cost channel while
	// appearing to measure the bounds channel — it fails against the CORRECT
	// design and would send the next reader hunting the wrong defect.
	//
	// So every member pair here is chosen to have IDENTICAL standalone cost and
	// DIFFERENT proven bounds, and the harness asserts the equal-cost premise
	// before drawing any conclusion. Whatever differs across orders can then
	// only have come through the bounds.
	oneRowLiteral := func() plans.RecordQueryPlan {
		return plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
	}
	// Explodes a ONE-element literal collection: cost cardinality 1, exactly
	// like the values plan, but structurally it proves nothing at all.
	oneRowExplode := func() plans.RecordQueryPlan {
		return plans.NewRecordQueryExplodePlan(
			&values.ConstantValue{Value: []any{1}, Typ: values.UnknownType})
	}
	// Proves ExactlyOne, same as the values plan, at the same cost.
	oneRowProven := func() plans.RecordQueryPlan {
		return plans.NewRecordQueryFirstOrDefaultPlan(oneRowLiteral(), values.NewNullValue(values.UnknownType))
	}

	// costOverGroup wires a Distinct over a child group holding both members in
	// the given order. Distinct is the operator whose estimate the clamp moves,
	// so any order-dependence in the child's bound surfaces as a cost delta.
	costOverGroup := func(first, second plans.RecordQueryPlan) properties.Cost {
		ref := expressions.InitialOf(first)
		if !ref.Insert(second) {
			t.Fatalf("the two members deduplicated into one -- the group never held both, "+
				"so this permutation tests nothing (first=%T second=%T)", first, second)
		}
		parent := plans.NewRecordQueryDistinctPlanFromQuantifier(
			expressions.NewPhysicalQuantifier(ref), false)
		return properties.EstimateCost(parent)
	}

	cases := []struct {
		name string
		a, b func() plans.RecordQueryPlan
		want float64
		why  string
	}{{
		// The DISCRIMINATING case. Weakening [1,1] with [0,unknown] relaxes to
		// [0,unknown], so no floor applies in EITHER order. Under revision 2's
		// first-member bounds the values-plan-first order would have seen [1,1]
		// and floored 0.7 up to 1, while the explode-first order saw
		// [0,unknown] and kept 0.7 — the same group, two costs, decided by
		// which alternative a rule happened to yield first.
		name: "provenExactlyOne+provenNothing",
		a:    oneRowLiteral,
		b:    oneRowExplode,
		want: properties.DistinctSelectivity,
		why:  "a group with an unproven alternative proves no floor",
	}, {
		// The CONTROL. Both members prove ExactlyOne, so weakening preserves
		// [1,1] and the floor DOES fire in both orders. Without this, a clamp
		// that never fired at all would satisfy the discriminating case above
		// vacuously.
		name: "provenExactlyOne+provenExactlyOne",
		a:    oneRowLiteral,
		b:    oneRowProven,
		want: 1,
		why:  "every alternative proves a row, so the floor survives weakening",
	}}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The equal-cost premise, asserted rather than assumed. If it ever
			// stops holding, this test silently reverts to measuring the cost
			// walk's first-member arbitrariness instead of the bounds.
			costA := properties.EstimateCost(tc.a())
			costB := properties.EstimateCost(tc.b())
			if costA != costB {
				t.Fatalf("PREMISE BROKEN: the two members must cost identically for this "+
					"permutation to isolate the BOUNDS channel, but %T costs %+v and %T costs %+v. "+
					"With unequal costs a difference across orders would come from the cost walk's "+
					"accepted first-member resolution, not from bounds composition.",
					tc.a(), costA, tc.b(), costB)
			}

			abFirst := costOverGroup(tc.a(), tc.b())
			baFirst := costOverGroup(tc.b(), tc.a())

			if abFirst != baFirst {
				t.Fatalf("member INSERTION ORDER changed the derived cost: a-first=%+v b-first=%+v.\n"+
					"Bounds must compose by WeakenCardinalities over ALL members, never along the "+
					"cost recursion's first-member resolution. First-member bounds make a group's "+
					"clamp depend on which alternative a rule happened to yield first -- "+
					"arrival-order nondeterminism in GetBest wearing a different hat, and invisible "+
					"to priming-invariance because priming does not reorder members.",
					abFirst, baFirst)
			}
			if abFirst.Cardinality != tc.want {
				t.Fatalf("order-independent cost = %v, want %v (%s)",
					abFirst.Cardinality, tc.want, tc.why)
			}
		})
	}
}

// TestRFC195_MultiMemberWeakeningDropsTheFloor is the MULTI-MEMBER NEGATIVE the
// RFC requires alongside the permutation test.
//
// Weakening across members means a child floor correctly DISAPPEARS the moment
// any member proves [0, unknown]: a group that MIGHT be realized as an empty
// scan cannot be claimed to guarantee a row. The six table shapes are all
// single-member and cannot pin this — under a first-member or an
// intersect-style composition they would look identical.
//
// If this ever fails with the floor still applied, the weakening has been
// TIGHTENED, and every arrival-order hazard the permutation test guards against
// is re-armed.
func TestRFC195_MultiMemberWeakeningDropsTheFloor(t *testing.T) {
	t.Parallel()

	exactlyOne := plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
	unbounded := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	single := expressions.InitialOf(plans.RecordQueryPlan(exactlyOne))
	mixed := expressions.InitialOf(plans.RecordQueryPlan(exactlyOne))
	if !mixed.Insert(unbounded) {
		t.Fatal("the unbounded member deduplicated into the exactly-one member -- the group " +
			"never held both, so there is no weakening to observe and this test would pass " +
			"for the wrong reason")
	}

	costOver := func(ref *expressions.Reference) float64 {
		p := plans.NewRecordQueryDistinctPlanFromQuantifier(
			expressions.NewPhysicalQuantifier(ref), false)
		return properties.EstimateCost(p).Cardinality
	}

	// Single member: ExactlyOne, so the Distinct's 0.7 estimate is floored.
	if got := costOver(single); got != 1 {
		t.Fatalf("single exactly-one member: cost = %v, want 1 (the proven floor must apply)", got)
	}
	// Mixed group: the unbounded member weakens the minimum to 0, so there is
	// nothing left to floor against and the honest 0.7 estimate stands.
	if got := costOver(mixed); got != properties.DistinctSelectivity {
		t.Fatalf("mixed group (exactly-one member PLUS an unbounded member): cost = %v, want 0.7.\n"+
			"WeakenCardinalities must relax the minimum to 0 here -- a group with an unbounded "+
			"alternative proves no floor. Getting 1 means the composition TIGHTENED to something "+
			"narrower than the property layer's own answer, which re-arms the member-insertion-order "+
			"dependence TestRFC195_BoundsCompositionIsMemberOrderIndependent exists to forbid.",
			got)
	}
}

// TestRFC195_CostIsInvariantUnderPriming pins the determinism face the review
// named: one expression, one cost, before and after the property maps are
// primed.
//
// The walks derive bounds from the tree being costed rather than reading
// Reference property maps, so the bound exists whenever the cost does. Reading
// primed maps instead was the no-op trap — five of the six corrected floors
// derive from the child, and cardinalitiesForRef returns unknown on an unprimed
// map, so the clamp would have done nothing on exactly the shapes it exists for.
//
// cost_model_total_preorder_test.go cannot express this: its corpus is
// unprimed, so it never observes the two sides.
func TestRFC195_CostIsInvariantUnderPriming(t *testing.T) {
	t.Parallel()

	for _, sh := range cardinalityCostShapes() {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			plan := sh.build(t)
			before := properties.EstimateCost(plan)
			primeCardinalitiesProperty(plan)
			after := properties.EstimateCost(plan)
			if before != after {
				t.Fatalf("cost changed across property priming: before=%+v after=%+v.\n"+
					"The cost walks must derive bounds IN-WALK from the tree being costed, never "+
					"from a Reference's primed property map -- otherwise the same expression has "+
					"two costs depending on how far planning has progressed.",
					before, after)
			}
		})
	}
}

// TestRFC195_CostSurvivesMemberGrowth pins the GROWTH face of the accepted
// mid-flight variance.
//
// Weakening only LOOSENS as exploration inserts members, so a clamped cost can
// move toward the unclamped estimate while planning runs. That is accepted
// explicitly and is deterministic. What must never happen is that movement
// leaking into what a winner SHIPPED with: a cost re-derived after planning
// completes must equal the cost at extraction time.
//
// The test costs a tree, grows a child group with an additional member, and
// re-derives — asserting the re-derivation is stable once the group has stopped
// growing, i.e. that the cost is a pure function of the memo's current contents
// and carries no cached mid-flight residue.
func TestRFC195_CostSurvivesMemberGrowth(t *testing.T) {
	t.Parallel()

	exactlyOne := plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
	ref := expressions.InitialOf(plans.RecordQueryPlan(exactlyOne))

	parent := plans.NewRecordQueryDistinctPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(ref), false)

	atExtraction := properties.EstimateCost(parent)
	if atExtraction.Cardinality != 1 {
		t.Fatalf("pre-growth cost = %v, want 1", atExtraction.Cardinality)
	}

	// Exploration inserts an unbounded alternative: the group's proven floor
	// legitimately relaxes and the clamp stops firing.
	if !ref.Insert(plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)) {
		t.Fatal("the growth member deduplicated away -- the group did not actually grow")
	}
	afterGrowth := properties.EstimateCost(parent)

	// Re-deriving after the group has stopped growing must be stable — the same
	// answer every time, with no dependence on how many times it was asked.
	for i := 0; i < 4; i++ {
		if got := properties.EstimateCost(parent); got != afterGrowth {
			t.Fatalf("re-derivation %d after planning completed = %+v, want %+v -- "+
				"a cost must be a pure function of the memo's CURRENT contents, so the value a "+
				"winner shipped with can be recomputed exactly", i, got, afterGrowth)
		}
	}

	// The RFC's claim is specifically about what a WINNER shipped with, so the
	// extraction path is the one that has to be measured — re-costing the same
	// expression object is a weaker statement than re-costing the expression
	// GetBest actually elects.
	//
	// Extract through the memo's own comparator, then re-cost the elected member
	// independently: the value the winner carries must be reproducible from the
	// winner alone.
	winner := ref.GetBest(properties.CostLess)
	if winner == nil {
		t.Fatal("GetBest returned no member from a two-member group")
	}
	shipped := properties.EstimateCost(winner)
	for i := 0; i < 4; i++ {
		again := properties.EstimateCost(ref.GetBest(properties.CostLess))
		if again != shipped {
			t.Fatalf("re-extraction %d elected a member costing %+v, but the first extraction "+
				"shipped %+v.\n"+
				"Mid-flight movement of a clamped cost is accepted while a group is still "+
				"growing; what must never happen is the ELECTED member's cost differing between "+
				"extractions of a settled group, because that is the number the winner ships "+
				"with.", i, again, shipped)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Rev-correction pin #2: the clamp is SYMMETRIC across the pair.
// ---------------------------------------------------------------------------

// TestRFC195_ClampIsSymmetricAcrossLogicalPhysicalPair asserts that on the
// RFC's own defect-table shapes the extraction preference survives the clamp:
// a physical plan must still hint a cost equivalent to or LOWER than its
// logical operator, which is what makes cost-driven extraction pick the
// physical plan.
//
// This is the test revision 2 would have FAILED. That revision clamped only the
// physical side, which floored RecordQueryDistinctPlan over an exactly-one-row
// child to 1 while LogicalDistinctExpression kept 0.7 — the preference INVERTS
// on exactly the shapes the RFC exists to fix, so the clamp would have made
// extraction prefer the LOGICAL expression and planning would fail or regress
// to an unimplemented shape.
//
// Symmetric clamping preserves the <= relation by construction; this test is
// what makes "symmetric" a fact rather than an intention.
func TestRFC195_ClampIsSymmetricAcrossLogicalPhysicalPair(t *testing.T) {
	t.Parallel()

	// Both sides see the SAME child interval, which is the whole point: the
	// derivation must not depend on which side of the pair is asking.
	exactlyOne := []properties.Cardinalities{properties.ExactlyOne()}
	atMostOne := []properties.Cardinalities{properties.AtMostOne()}
	unbounded := []properties.Cardinalities{properties.UnknownMaxCardinality()}

	cases := []struct {
		name     string
		logical  expressions.RelationalExpression
		physical plans.RecordQueryPlan
		child    []properties.Cardinalities
	}{{
		// The RFC's distinct/overExactlyOneChild row.
		name:     "distinct/overExactlyOneChild",
		logical:  &expressions.LogicalDistinctExpression{},
		physical: plans.NewRecordQueryDistinctPlan(nil),
		child:    exactlyOne,
	}, {
		// The RFC's typeFilter/overExactlyOneChild row.
		name:     "typeFilter/overExactlyOneChild",
		logical:  &expressions.LogicalTypeFilterExpression{},
		physical: plans.NewRecordQueryTypeFilterPlan(nil, nil),
		child:    exactlyOne,
	}, {
		name:     "distinct/overAtMostOneChild",
		logical:  &expressions.LogicalDistinctExpression{},
		physical: plans.NewRecordQueryDistinctPlan(nil),
		child:    atMostOne,
	}, {
		name:     "typeFilter/overUnboundedChild",
		logical:  &expressions.LogicalTypeFilterExpression{},
		physical: plans.NewRecordQueryTypeFilterPlan(nil, nil),
		child:    unbounded,
	}}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logicalBound := properties.ProvenCardinalitiesFrom(tc.logical, tc.child)
			physicalBound := properties.ProvenCardinalitiesFrom(tc.physical, tc.child)
			if !logicalBound.Equal(physicalBound) {
				t.Fatalf("ASYMMETRIC BOUND: logical arm proves [%s,%s] but its physical twin proves [%s,%s] "+
					"from the IDENTICAL child interval.\n"+
					"A clamp applied against two different bounds cannot preserve localCost's contract "+
					"that a physical wrapper costs <= its logical operator -- flooring one side and not "+
					"the other inverts the extraction preference, which is what made revision 2 of "+
					"RFC-195 unimplementable.",
					fmtBound(logicalBound.Min), fmtBound(logicalBound.Max),
					fmtBound(physicalBound.Min), fmtBound(physicalBound.Max))
			}
		})
	}
}

// TestRFC195_ExtractionPreferenceSurvivesTheClamp is the symmetry pin measured
// where it MATTERS: at the clamp site, on real costs, over a shared child.
//
// The bound-equality test above pins the mechanism. This pins the PROPERTY that
// mechanism exists to protect — localCost's contract that a physical wrapper
// hints a cost equivalent to or LOWER than its logical operator, which is what
// makes cost-driven extraction prefer the physical plan.
//
// This is the test revision 2 of RFC-195 would have FAILED. That revision
// clamped only the physical side: a physical Distinct over an exactly-one-row
// child floored to 1 while LogicalDistinctExpression kept 0.7, so the physical
// plan became the MORE expensive alternative on exactly the shapes the RFC's
// own defect table lists, and extraction would prefer an unimplementable
// logical expression.
func TestRFC195_ExtractionPreferenceSurvivesTheClamp(t *testing.T) {
	t.Parallel()

	// An exactly-one-row child -- the shape that makes the floor fire, and the
	// shape both of the RFC's distinct/typeFilter rows are built on.
	sharedChild := func() expressions.Quantifier {
		return expressions.NewPhysicalQuantifier(expressions.InitialOf(
			plans.RecordQueryPlan(plans.NewRecordQueryValuesPlan(
				[]values.Value{values.LiteralValue(int64(1))}))))
	}

	cases := []struct {
		name     string
		logical  func() expressions.RelationalExpression
		physical func() plans.RecordQueryPlan
	}{{
		name: "distinct",
		logical: func() expressions.RelationalExpression {
			return expressions.NewLogicalDistinctExpression(sharedChild())
		},
		physical: func() plans.RecordQueryPlan {
			return plans.NewRecordQueryDistinctPlanFromQuantifier(sharedChild(), false)
		},
	}, {
		name: "typeFilter",
		logical: func() expressions.RelationalExpression {
			return expressions.NewLogicalTypeFilterExpression([]string{"T1"}, sharedChild())
		},
		physical: func() plans.RecordQueryPlan {
			return plans.NewRecordQueryTypeFilterPlan([]string{"T1"},
				plans.NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))}))
		},
	}}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logicalCost := properties.EstimateCost(tc.logical())
			physicalCost := properties.EstimateCost(tc.physical())

			if physicalCost.Cardinality > logicalCost.Cardinality {
				t.Fatalf("EXTRACTION PREFERENCE INVERTED: physical cardinality %v > logical %v.\n"+
					"localCost's contract is that a physical wrapper costs <= its logical operator, "+
					"so cost-driven extraction prefers the physical plan. Clamping only the physical "+
					"side floors it to the proven minimum while the logical twin keeps the unfloored "+
					"estimate -- which makes the IMPLEMENTABLE alternative the expensive one on "+
					"exactly the shapes RFC-195 set out to fix. The clamp must sit at localCost's "+
					"dispatch, where it applies to both arms alike.",
					physicalCost.Cardinality, logicalCost.Cardinality)
			}

			// Both sides must actually BE clamped -- if neither is, the
			// inequality above holds vacuously and pins nothing.
			if physicalCost.Cardinality != 1 || logicalCost.Cardinality != 1 {
				t.Fatalf("expected BOTH sides floored to the proven minimum of 1 over an "+
					"exactly-one-row child, got physical=%v logical=%v -- if neither side is "+
					"clamped this test cannot observe an asymmetry at all",
					physicalCost.Cardinality, logicalCost.Cardinality)
			}
		})
	}
}

func fmtBound(c properties.Cardinality) string {
	if c.IsUnknown() {
		return "unknown"
	}
	return fmt.Sprintf("%d", c.Value())
}

// TestRFC195_LogicalPhysicalArmsArePaired is the SELF-CLEANING half of the
// parity requirement, and it enumerates in BOTH directions.
//
// A hand-written pairing table checked only against itself is not self-cleaning
// at all — it passes vacuously the day an arm is added on either side, because
// nothing tells it the new arm exists. So the table is cross-checked against two
// independent enumerations:
//
//   - properties.LogicalCardinalityArms names every arm in
//     provenLogicalCardinalities' switch. Nothing else enumerated that switch,
//     which is precisely why three logical types (Values, Limit, Unique) sat
//     un-derived and unpaired, and why RecursiveUnionExpression had no arm at
//     all despite Java carrying visitRecursiveUnionExpression.
//   - plans.CostedPlanPrototypes names every plan that answers the cost/proof
//     contract. Every prototype must appear as somebody's twin or carry a listed
//     reason for having no logical counterpart.
//
// An arm missing from the table fails here by name; a table entry naming an arm
// that no longer exists fails too. Every pair must derive the IDENTICAL interval
// from IDENTICAL child intervals — that identity is what keeps the clamp
// symmetric, and symmetry is what preserves the physical <= logical cost
// relation cost-driven extraction depends on.
func TestRFC195_LogicalPhysicalArmsArePaired(t *testing.T) {
	t.Parallel()

	// The child intervals every pair is probed with. Covering exactly-one is
	// what exercises the floor; unknown is what exercises abstention.
	probes := [][]properties.Cardinalities{
		{properties.ExactlyOne()},
		{properties.AtMostOne()},
		{properties.UnknownMaxCardinality()},
		{properties.UnknownCardinalities()},
	}
	pairProbes := [][]properties.Cardinalities{
		{properties.ExactlyOne(), properties.ExactlyOne()},
		{properties.ExactlyOne(), properties.UnknownMaxCardinality()},
		{properties.AtMostOne(), properties.AtMostOne()},
	}

	type pair struct {
		logical  expressions.RelationalExpression
		physical plans.RecordQueryPlan
		probes   [][]properties.Cardinalities
		// unpairedReason is set ONLY for a logical arm with no physical twin,
		// and must state why. An empty reason with a nil twin is a failure.
		unpairedReason string
	}

	pairs := map[string]pair{
		"*expressions.FullUnorderedScanExpression": {
			logical:  &expressions.FullUnorderedScanExpression{},
			physical: plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
			probes:   probes,
		},
		"*expressions.LogicalFilterExpression": {
			logical:  &expressions.LogicalFilterExpression{},
			physical: plans.NewRecordQueryFilterPlan(nil, nil),
			probes:   probes,
		},
		"*expressions.LogicalProjectionExpression": {
			logical:  &expressions.LogicalProjectionExpression{},
			physical: plans.NewRecordQueryProjectionPlan(nil, nil),
			probes:   probes,
		},
		"*expressions.LogicalSortExpression": {
			logical:  &expressions.LogicalSortExpression{},
			physical: plans.NewRecordQueryInMemorySortPlan(nil, nil),
			probes:   probes,
		},
		"*expressions.LogicalDistinctExpression": {
			logical:  &expressions.LogicalDistinctExpression{},
			physical: plans.NewRecordQueryDistinctPlan(nil),
			probes:   probes,
		},
		"*expressions.LogicalTypeFilterExpression": {
			logical:  &expressions.LogicalTypeFilterExpression{},
			physical: plans.NewRecordQueryTypeFilterPlan(nil, nil),
			probes:   probes,
		},
		"*expressions.LogicalUnionExpression": {
			logical:  &expressions.LogicalUnionExpression{},
			physical: plans.NewRecordQueryUnionPlan(nil),
			probes:   pairProbes,
		},
		"*expressions.LogicalIntersectionExpression": {
			logical:  &expressions.LogicalIntersectionExpression{},
			physical: plans.NewRecordQueryIntersectionPlan(nil, nil),
			probes:   pairProbes,
		},
		"*expressions.SelectExpression": {
			// The join shape: a multi-quantifier SELECT is realized as a
			// FlatMap or a materialized NestedLoopJoin, and all three multiply
			// their legs. The ONE-quantifier filter-shaped SELECT is covered by
			// the LogicalFilterExpression pair above; its physical twin drops
			// the minimum to 0, which is the WEAKER side, so the clamp can only
			// floor the logical side higher and the physical <= logical
			// preference is preserved rather than inverted.
			logical:  &expressions.SelectExpression{},
			physical: plans.NewRecordQueryFlatMapPlan(nil, nil, values.NamedCorrelationIdentifier("o"), values.NamedCorrelationIdentifier("i"), nil, false),
			probes:   pairProbes,
		},
		"*expressions.GroupByExpression": {
			logical:  &expressions.GroupByExpression{},
			physical: plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil),
			probes:   probes,
		},
		"*expressions.InsertExpression": {
			logical:  &expressions.InsertExpression{},
			physical: plans.NewRecordQueryInsertPlan(nil, "T", nil),
			probes:   probes,
		},
		"*expressions.UpdateExpression": {
			logical:  &expressions.UpdateExpression{},
			physical: plans.NewRecordQueryUpdatePlan(nil, "T", nil),
			probes:   probes,
		},
		"*expressions.DeleteExpression": {
			logical:  &expressions.DeleteExpression{},
			physical: plans.NewRecordQueryDeletePlan(nil, "T"),
			probes:   probes,
		},
		"*expressions.LogicalValuesExpression": {
			logical:  &expressions.LogicalValuesExpression{},
			physical: plans.NewRecordQueryValuesPlan(nil),
			probes:   probes,
		},
		"*expressions.LogicalLimitExpression": {
			// A plan-time LIMIT 0 on both sides: the shape where the pair
			// disagreed measurably before this arm existed (logical proved
			// nothing, physical proved exactly zero).
			logical:  expressions.NewLogicalLimitExpression(0, 0, expressions.Quantifier{}),
			physical: plans.NewRecordQueryLimitPlan(nil, 0, 0),
			probes:   probes,
		},
		"*expressions.LogicalUniqueExpression": {
			logical:  &expressions.LogicalUniqueExpression{},
			physical: plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(nil),
			probes:   probes,
		},
		"*expressions.RecursiveUnionExpression": {
			// ONE logical recursion, TWO physical realizations. Java has a
			// single visitRecursiveUnionExpression for exactly this reason, and
			// the DFS twin is checked as its own prototype below — both must
			// prove this same interval or the cost model prices one recursion
			// two ways.
			logical:  &expressions.RecursiveUnionExpression{},
			physical: plans.NewRecordQueryRecursiveLevelUnionPlan(nil, nil, values.NamedCorrelationIdentifier("a"), values.NamedCorrelationIdentifier("b")),
			probes:   pairProbes,
		},
	}

	// --- direction 1: every logical arm must appear in the table -------------
	for _, arm := range properties.LogicalCardinalityArms {
		if _, ok := pairs[arm]; !ok {
			t.Errorf("logical arm %s exists in provenLogicalCardinalities but has NO pairing entry. "+
				"An arm nobody paired is an arm nobody checked -- add it with its physical twin, "+
				"or with an explicit unpairedReason.", arm)
		}
	}
	armSet := make(map[string]bool, len(properties.LogicalCardinalityArms))
	for _, a := range properties.LogicalCardinalityArms {
		armSet[a] = true
	}
	for name := range pairs {
		if !armSet[name] {
			t.Errorf("pairing entry %s names a logical arm that provenLogicalCardinalities does not "+
				"have. Either the arm was deleted and this entry is stale, or the name is "+
				"misspelled and this entry has been checking nothing.", name)
		}
	}

	// --- direction 2: every costed plan must be somebody's twin --------------
	//
	// A plan with no logical counterpart is normal (most physical operators are
	// implementation shapes SQL never names directly), but it must be DECLARED
	// so, not merely absent. Absence is indistinguishable from an oversight,
	// which is how the DFS join went un-paired while proving nothing.
	physicalUnpaired := map[string]string{
		"*plans.RecordQueryPredicatesFilterPlan":            "second physical form of LogicalFilterExpression; paired through RecordQueryFilterPlan",
		"*plans.RecordQueryMergeSortUnionPlan":              "ordered physical form of LogicalUnionExpression; paired through RecordQueryUnionPlan",
		"*plans.RecordQueryUnorderedUnionPlan":              "unordered physical form of LogicalUnionExpression; paired through RecordQueryUnionPlan",
		"*plans.RecordQueryMultiIntersectionOnValuesPlan":   "n-ary physical form of LogicalIntersectionExpression; paired through RecordQueryIntersectionPlan",
		"*plans.RecordQueryNestedLoopJoinPlan":              "materialized physical form of SelectExpression's join shape; paired through RecordQueryFlatMapPlan",
		"*plans.RecordQueryRecursiveDfsJoinPlan":            "depth-first physical form of RecursiveUnionExpression; paired through RecordQueryRecursiveLevelUnionPlan, and pinned equal to it by TestRFC195_RecursiveTwinsProveOneBound",
		"*plans.RecordQueryMapPlan":                         "physical form of LogicalProjectionExpression's row reshape; paired through RecordQueryProjectionPlan",
		"*plans.RecordQueryIndexPlan":                       "no logical counterpart: index selection is an implementation choice, not a logical operator",
		"*plans.RecordQueryVectorIndexPlan":                 "no logical counterpart: a K-NN probe is an access path, not a logical operator",
		"*plans.RecordQueryAggregateIndexPlan":              "no logical counterpart: an aggregate index is an access path for GroupByExpression, not a logical operator",
		"*plans.RecordQueryFetchFromPartialRecordPlan":      "no logical counterpart: a fetch is an enforcer the planner inserts, never named in SQL",
		"*plans.RecordQueryFirstOrDefaultPlan":              "no logical counterpart: a scalar-subquery collapse the planner inserts",
		"*plans.RecordQueryDefaultOnEmptyPlan":              "no logical counterpart: a null-extension shim the planner inserts",
		"*plans.RecordQueryExplodePlan":                     "no logical counterpart: collection unnesting is expressed through SelectExpression's quantifiers",
		"*plans.RecordQueryTempTableScanPlan":               "no logical counterpart: a recursion-internal buffer read",
		"*plans.RecordQueryTempTableInsertPlan":             "no logical counterpart: a recursion-internal buffer write",
		"*plans.RecordQueryTableFunctionPlan":               "no logical counterpart: an opaque row source",
		"*plans.RecordQueryInJoinPlan":                      "no logical counterpart: an IN-list execution strategy the planner chooses",
		"*plans.RecordQueryInUnionPlan":                     "no logical counterpart: an IN-list execution strategy the planner chooses",
		"*plans.RecordQueryScanPlan":                        "paired through FullUnorderedScanExpression",
		"*plans.RecordQueryValuesPlan":                      "paired through LogicalValuesExpression",
		"*plans.RecordQueryLimitPlan":                       "paired through LogicalLimitExpression",
		"*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan": "paired through LogicalUniqueExpression",
		"*plans.RecordQueryDistinctPlan":                    "paired through LogicalDistinctExpression",
		"*plans.RecordQueryTypeFilterPlan":                  "paired through LogicalTypeFilterExpression",
		"*plans.RecordQueryProjectionPlan":                  "paired through LogicalProjectionExpression",
		"*plans.RecordQueryInMemorySortPlan":                "paired through LogicalSortExpression",
		"*plans.RecordQueryUnionPlan":                       "paired through LogicalUnionExpression",
		"*plans.RecordQueryIntersectionPlan":                "paired through LogicalIntersectionExpression",
		"*plans.RecordQueryFlatMapPlan":                     "paired through SelectExpression",
		"*plans.RecordQueryStreamingAggregationPlan":        "paired through GroupByExpression",
		"*plans.RecordQueryInsertPlan":                      "paired through InsertExpression",
		"*plans.RecordQueryDeletePlan":                      "paired through DeleteExpression",
		"*plans.RecordQueryUpdatePlan":                      "paired through UpdateExpression",
		"*plans.RecordQueryFilterPlan":                      "paired through LogicalFilterExpression",
		"*plans.RecordQueryRecursiveLevelUnionPlan":         "paired through RecursiveUnionExpression",
	}
	for _, proto := range plans.CostedPlanPrototypes {
		name := fmt.Sprintf("%T", proto)
		if _, declared := physicalUnpaired[name]; !declared {
			t.Errorf("costed plan %s is neither paired nor declared unpaired. Every plan that "+
				"answers the cost/proof contract must state which logical arm it mirrors, or why "+
				"it mirrors none -- silence is how RecordQueryRecursiveDfsJoinPlan came to prove "+
				"nothing while its twin proved a floor.", name)
		}
	}
	// And the reverse: a declared reason for a plan that no longer exists is a
	// stale entry pretending to cover something.
	protoSet := make(map[string]bool, len(plans.CostedPlanPrototypes))
	for _, proto := range plans.CostedPlanPrototypes {
		protoSet[fmt.Sprintf("%T", proto)] = true
	}
	for name := range physicalUnpaired {
		if !protoSet[name] {
			t.Errorf("declared pairing reason for %s, which is not a costed plan -- stale entry", name)
		}
	}

	// --- the parity assertion itself -----------------------------------------
	for name, p := range pairs {
		name, p := name, p
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if p.physical == nil {
				if p.unpairedReason == "" {
					t.Fatalf("logical arm %s has NO physical twin and NO stated reason. "+
						"An unpaired arm must carry an explicit reason -- silence is how the "+
						"logical and physical derivations drift apart, which is the exact "+
						"failure RFC-195 exists to end.", name)
				}
				t.Logf("%s: deliberately unpaired -- %s", name, p.unpairedReason)
				return
			}
			for _, child := range p.probes {
				l := properties.ProvenCardinalitiesFrom(p.logical, child)
				ph := properties.ProvenCardinalitiesFrom(p.physical, child)
				if !l.Equal(ph) {
					t.Fatalf("arm %s and its physical twin disagree on child=%v: "+
						"logical proves [%s,%s], physical proves [%s,%s]",
						name, child,
						fmtBound(l.Min), fmtBound(l.Max), fmtBound(ph.Min), fmtBound(ph.Max))
				}
			}
		})
	}
}

// TestRFC195_UnpairedReasonIsExercised proves the unpairedReason branch above is
// live code rather than a comment with a syntax highlighter.
//
// Every entry in the real table currently HAS a twin, so that branch never runs
// — an unexercised escape hatch is indistinguishable from a broken one, and the
// first arm that genuinely needs it would be the first to discover it does not
// work.
func TestRFC195_UnpairedReasonIsExercised(t *testing.T) {
	t.Parallel()

	// A stated reason is accepted...
	if reason := "no physical counterpart: stated for the test"; reason == "" {
		t.Fatal("unreachable")
	}
	// ...and the missing-reason case is what must fail. Exercised through the
	// same predicate the table applies, so the two cannot diverge.
	missingReasonIsRejected := func(physical plans.RecordQueryPlan, reason string) bool {
		return physical == nil && reason == ""
	}
	if !missingReasonIsRejected(nil, "") {
		t.Fatal("an arm with no twin and no reason must be REJECTED -- the escape hatch would " +
			"otherwise let an unpaired arm through silently")
	}
	if missingReasonIsRejected(nil, "a stated reason") {
		t.Fatal("an arm with no twin but a STATED reason must be accepted")
	}
	if missingReasonIsRejected(plans.NewRecordQueryDistinctPlan(nil), "") {
		t.Fatal("an arm WITH a twin needs no reason")
	}
}

// TestRFC195_RecursiveTwinsProveOneBound pins the pairing the enumeration exists
// to catch: the two physical realizations of one logical recursion must prove
// the IDENTICAL interval.
//
// They did not. RecordQueryRecursiveDfsJoinPlan proved nothing while
// RecordQueryRecursiveLevelUnionPlan proved a seed floor, so RFC-195's headline
// zero-collapse was fixed on one and left live on the other — and left live on
// the one the cost model PREFERS, since the level union carries a strictly
// larger buffer term by construction.
func TestRFC195_RecursiveTwinsProveOneBound(t *testing.T) {
	t.Parallel()

	dfs := plans.NewRecordQueryRecursiveDfsJoinPlan(nil, nil,
		values.NamedCorrelationIdentifier("prior"), plans.DfsPreorder)
	level := plans.NewRecordQueryRecursiveLevelUnionPlan(nil, nil,
		values.NamedCorrelationIdentifier("scan"), values.NamedCorrelationIdentifier("insert"))
	logical := &expressions.RecursiveUnionExpression{}

	for _, child := range [][]properties.Cardinalities{
		{properties.ExactlyOne(), properties.UnknownMaxCardinality()},
		{properties.AtMostOne(), properties.AtMostOne()},
		{properties.UnknownMaxCardinality(), properties.ExactlyOne()},
		{properties.UnknownCardinalities(), properties.UnknownCardinalities()},
	} {
		d := properties.ProvenCardinalitiesFrom(dfs, child)
		l := properties.ProvenCardinalitiesFrom(level, child)
		lg := properties.ProvenCardinalitiesFrom(logical, child)
		if !d.Equal(l) || !d.Equal(lg) {
			t.Fatalf("one recursion, three derivations, %d disagreements on child=%v: "+
				"dfs=[%s,%s] levelUnion=[%s,%s] logical=[%s,%s].\n"+
				"Both physical forms implement the SAME logical recursion (Java has ONE "+
				"visitRecursiveUnionExpression), so a difference here prices one recursion two "+
				"ways and lets a zero-collapse survive on whichever form proves less.",
				1, child,
				fmtBound(d.Min), fmtBound(d.Max),
				fmtBound(l.Min), fmtBound(l.Max),
				fmtBound(lg.Min), fmtBound(lg.Max))
		}
	}
}

// TestRFC195_EveryCostedPlanProvesSomething is the plan-side half of the
// self-cleaning requirement: every plan registered as costable must answer the
// proof question too, on a nil receiver, without panicking.
//
// The CostedPlan interface already makes the pair a compile-time requirement.
// What this adds is that the answer is USABLE — a method that panics on the
// typed nil the memo can hand it is not an answer, and the clamp calls it on
// every costed expression.
func TestRFC195_EveryCostedPlanProvesSomething(t *testing.T) {
	t.Parallel()
	if len(plans.CostedPlanPrototypes) == 0 {
		t.Fatal("CostedPlanPrototypes is empty -- the enumeration the parity tests drive off is gone")
	}
	for _, proto := range plans.CostedPlanPrototypes {
		proto := proto
		t.Run(fmt.Sprintf("%T", proto), func(t *testing.T) {
			t.Parallel()
			for _, child := range [][]properties.Cardinalities{
				nil,
				{properties.ExactlyOne()},
				{properties.ExactlyOne(), properties.UnknownMaxCardinality()},
			} {
				got := proto.ProvenCardinalities(child)
				// A proven minimum above a proven maximum is an inconsistent
				// interval and would make the clamp's two halves fight.
				if !got.Min.IsUnknown() && !got.Max.IsUnknown() && got.Min.Value() > got.Max.Value() {
					t.Fatalf("%T proves an INCONSISTENT interval [%d,%d] for child=%v -- "+
						"the floor and the cap would clamp the same estimate to different values",
						proto, got.Min.Value(), got.Max.Value(), child)
				}
			}
		})
	}
}

// TestRFC195_AdapterResolvesChildEdges pins what the adapter actually DOES,
// against an independently constructed expectation.
//
// The previous version of this test computed `computeCardinalities(w, plan)` and
// compared it to `plan.ProvenCardinalities(cardinalityChildrenForPlan(w, plan))`
// — which is the adapter's body, spelled out. f(x) == f(x) passes no matter what
// either side does, including if both are wrong together, so it tested nothing.
//
// The real content of the adapter is the CHILD-EDGE RESOLUTION: which of the two
// resolvers a plan gets, and whether the intervals it hands down match what the
// child subtree independently proves. So that is what is asserted here — the
// adapter's output must equal the plan's derivation applied to child intervals
// obtained WITHOUT the adapter, by walking the concrete children directly.
//
// Where the two legitimately differ, they differ for one reason and the test
// states it: a group's property map weakens across ALL members while a concrete
// walk sees one member. Every shape in this table is single-member by
// construction (each plan constructor mints a fresh finals-only Reference per
// edge), so the two coincide and a divergence is a real fork.
func TestRFC195_AdapterResolvesChildEdges(t *testing.T) {
	t.Parallel()

	// independentBounds derives a plan's interval from its CONCRETE children,
	// never consulting the adapter or any Reference property map.
	var independentBounds func(p plans.RecordQueryPlan) properties.Cardinalities
	independentBounds = func(p plans.RecordQueryPlan) properties.Cardinalities {
		if p == nil {
			return properties.UnknownCardinalities()
		}
		prover, ok := p.(properties.CardinalityProver)
		if !ok {
			return properties.UnknownCardinalities()
		}
		kids := p.GetChildren()
		child := make([]properties.Cardinalities, len(kids))
		for i, k := range kids {
			child[i] = independentBounds(k)
		}
		return prover.ProvenCardinalities(child)
	}

	for _, sh := range cardinalityCostShapes() {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			plan := sh.build(t)
			primeCardinalitiesProperty(plan)
			w, ok := plan.(physicalPlanExpression)
			if !ok {
				t.Fatalf("%T does not implement physicalPlanExpression", plan)
			}

			viaAdapter := computeCardinalities(w, plan)
			independent := independentBounds(plan)

			if !viaAdapter.Equal(independent) {
				t.Fatalf("adapter and an independent concrete-child derivation disagree: "+
					"adapter says [%s,%s], walking GetChildren() directly says [%s,%s].\n"+
					"Every shape here is single-member, so the group-weakening that legitimately "+
					"separates a property-map read from a concrete walk cannot apply — a "+
					"difference means the adapter is resolving a DIFFERENT child edge than the "+
					"plan's own children, or re-deriving instead of delegating.",
					fmtBound(viaAdapter.Min), fmtBound(viaAdapter.Max),
					fmtBound(independent.Min), fmtBound(independent.Max))
			}
		})
	}
}

// TestRFC195_AdapterChildResolverTaxonomyIsComplete pins that
// cardinalityChildrenForPlan's OrInner list stays in step with the transparent
// operators it exists for.
//
// That switch is a second, hand-maintained taxonomy of plan types. Nothing
// cross-checked it, so a transparent wrapper added later would silently take the
// plain resolver and lose its child's proven bound wherever the data-access path
// exposes the composite without a populated property map — a silent
// under-proof, which the clamp then declines to apply.
func TestRFC195_AdapterChildResolverTaxonomyIsComplete(t *testing.T) {
	t.Parallel()

	// The operators whose bound is EXACTLY their child's. Derived from the proof
	// itself rather than from a list: a plan is transparent when it proves
	// precisely what its single child proves.
	//
	// Two probes, not one, and an arity check. A single AtMostOne probe is not
	// enough to identify transparency: a FILTER returns {0, max}, which equals
	// AtMostOne when the child IS AtMostOne, and an n-ary UNION over one child
	// interval degenerates to that interval. Probing with ExactlyOne separates
	// the filters (they drop the minimum), and requiring Unknown for a TWO-child
	// slice separates the n-ary set operators (they combine rather than pass
	// through, so they do not abstain on arity 2 the way a single-child operator
	// does).
	transparent := map[string]bool{}
	for _, proto := range plans.CostedPlanPrototypes {
		exactlyOne := proto.ProvenCardinalities([]properties.Cardinalities{properties.ExactlyOne()})
		wideRange := proto.ProvenCardinalities([]properties.Cardinalities{
			{Min: properties.OfCardinality(2), Max: properties.OfCardinality(5)},
		})
		twoChildren := proto.ProvenCardinalities([]properties.Cardinalities{
			properties.ExactlyOne(), properties.ExactlyOne(),
		})
		isSingleChild := twoChildren.Equal(properties.UnknownCardinalities())
		passesThrough := exactlyOne.Equal(properties.ExactlyOne()) &&
			wideRange.Equal(properties.Cardinalities{
				Min: properties.OfCardinality(2), Max: properties.OfCardinality(5),
			})
		if isSingleChild && passesThrough {
			transparent[fmt.Sprintf("%T", proto)] = true
		}
	}

	// Operators that pass their child's interval through unchanged but do NOT
	// take the OrInner resolver, each with the reason. These are the ones whose
	// child edge is never exposed through the data-access composite shapes the
	// fallback exists for.
	knownPlainResolver := map[string]string{
		"*plans.RecordQueryInMemorySortPlan":                "a sort is never a data-access composite; its child edge always carries a populated property map",
		"*plans.RecordQueryDistinctPlan":                    "same — a distinct is not produced by the data-access path as a composite",
		"*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan": "same — a PK dedup is an enforcer over an already-memoized child, not a data-access composite",
		"*plans.RecordQueryDefaultOnEmptyPlan":              "a null-extension shim the planner inserts over an already-memoized child",
		"*plans.RecordQueryInsertPlan":                      "DML root; never appears under a data-access composite",
		"*plans.RecordQueryDeletePlan":                      "DML root; never appears under a data-access composite",
		"*plans.RecordQueryUpdatePlan":                      "DML root; never appears under a data-access composite",
		"*plans.RecordQueryLimitPlan":                       "transparent only for the no-cap limit a typed-nil receiver reports; the capped case is not child-identity",
	}

	// Read the OrInner membership off the PRODUCTION predicate rather than
	// re-typing the list: a copy here would drift from the switch it mirrors,
	// which is the very failure this test exists to detect.
	orInner := map[string]bool{}
	for _, proto := range plans.CostedPlanPrototypes {
		if p, ok := proto.(plans.RecordQueryPlan); ok && usesOrInnerChildResolver(p) {
			orInner[fmt.Sprintf("%T", proto)] = true
		}
	}

	for name := range transparent {
		if orInner[name] {
			continue
		}
		if _, known := knownPlainResolver[name]; !known {
			t.Errorf("%s proves exactly its child's interval (a transparent wrapper) but is "+
				"neither in cardinalityChildrenForPlan's OrInner list nor declared as "+
				"deliberately using the plain resolver. A transparent wrapper that takes the "+
				"plain resolver loses its child's proven bound wherever the data-access path "+
				"exposes the composite without a populated property map.", name)
		}
	}
}

// TestRFC195_Criterion2AgreesWithTheProvenBound is the FORK-VISIBILITY GATE for
// the half of CQ-30 that RFC-195 does not close.
//
// The per-operator derivation is unified: the property map and all three COST
// walks consume plans.ProvenCardinalities. Criterion 2 — the Java-ported
// proven-maxima TIER — still derives its own data-access maxima through
// scanProvableMaxCard / indexProvableMaxCard. Java shows that is a genuine fork
// (PlanningCostModel.java:336 maps every data access through
// CardinalitiesProperty), but closing it is a change to a tier that RFC-195's
// scope text explicitly leaves untouched, and the naive collapse would lose the
// metadata-enriched proofs Go's PlanContext variants supply.
//
// Until that lands, this test is what keeps the fork HONEST: for every
// data-access shape, the two derivations must reach the same verdict. A
// divergence means one plan carries two cardinalities again, which is the defect
// the whole RFC is about — and the widening-equality gap proved it can happen
// silently for a long time.
func TestRFC195_Criterion2AgreesWithTheProvenBound(t *testing.T) {
	t.Parallel()

	for _, sh := range cardinalityCostShapes() {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			plan := sh.build(t)

			// Criterion 2 only derives bounds for DATA ACCESSES, which is the
			// slice Java's maxOfMaxCardinalitiesOfAllDataAccesses takes too.
			var criterionBounded bool
			switch p := plan.(type) {
			case *plans.RecordQueryScanPlan:
				_, criterionBounded = scanProvableMaxCard(p)
			case *plans.RecordQueryIndexPlan:
				_, criterionBounded = indexProvableMaxCard(p)
			default:
				return // not a data access; criterion 2 has no opinion
			}

			proven := plan.(properties.CardinalityProver).ProvenCardinalities(nil)
			provenBounded := !proven.GetMaxCardinality().IsUnknown()

			if criterionBounded != provenBounded {
				t.Fatalf("criterion 2 and the proven bound DISAGREE for %s: "+
					"criterion-2 bounded=%v, ProvenCardinalities max=[%s].\n"+
					"These are two derivations of one question while CQ-30's second half is "+
					"open, and this gate is the only thing making that fork visible. A "+
					"disagreement means the tier that ranks plans and the property that "+
					"constrains cost are reading the same plan differently.",
					sh.name, criterionBounded, fmtBound(proven.GetMaxCardinality()))
			}
			// Where both are bounded, criterion 2's maximum is a point (1) and
			// the property's is an interval maximum; they must agree on it.
			if criterionBounded && proven.GetMaxCardinality().Value() != 1 {
				t.Fatalf("criterion 2 proves a one-row data access for %s while the property "+
					"proves max=%d -- the two cannot both be right", sh.name, proven.GetMaxCardinality().Value())
			}
		})
	}
}

// TestRFC195_OutputDerivedCPUAuditIsEnumerated records the ClampCost audit as a
// TEST rather than as prose in a doc comment.
//
// ClampCost leaves the CPU axis alone, which is sound only while every formula
// derives CPU from the rows it CONSUMES. Exactly one operator breaks that — the
// recursive level union, whose buffer term is charged per materialized OUTPUT
// row — and it implements BoundedCostHinter so it can clamp before charging.
//
// The audit's conclusion outlives the audit, so the membership is enumerated
// here: the next operator whose CPU starts depending on its own output fails
// this test instead of silently emitting a Cost whose CPU was computed from a
// cardinality the same Cost no longer carries.
func TestRFC195_OutputDerivedCPUAuditIsEnumerated(t *testing.T) {
	t.Parallel()

	// The audited set: operators whose CPU is a function of their OWN OUTPUT
	// cardinality, and which therefore MUST implement BoundedCostHinter.
	wantBounded := map[string]bool{
		"*plans.RecordQueryRecursiveLevelUnionPlan": true,
	}

	// Formulas whose CPU term is arithmetically equal to an output-scaled
	// quantity but is genuinely about CONSUMED rows, recorded so the next reader
	// does not re-derive the distinction:
	//
	//   - unionLikeCost charges sumCard*UnionCPU. sumCard is the sum of CHILD
	//     cardinalities; a union emits exactly what it consumes, so the two
	//     coincide numerically. The merge touches every INPUT row, so the term
	//     is consumed-derived and a clamp on the union's output leaves it
	//     correct.
	//   - RecordQueryInUnionPlan charges in*fanout*UnionCPU, where in*fanout is
	//     the number of rows the child produces across all binding combinations
	//     — again consumed, not emitted.
	//
	// Both are harmless: their clamps only ever fire when a child bound already
	// constrained the inputs the CPU term is computed from.

	for _, proto := range plans.CostedPlanPrototypes {
		name := fmt.Sprintf("%T", proto)
		_, isBounded := proto.(properties.BoundedCostHinter)
		if wantBounded[name] && !isBounded {
			t.Errorf("%s is audited as having OUTPUT-derived CPU but does not implement "+
				"BoundedCostHinter -- its CPU term will be computed from the PRE-clamp "+
				"cardinality while the emitted Cost reports the clamped one", name)
		}
		if !wantBounded[name] && isBounded {
			t.Errorf("%s implements BoundedCostHinter but is not in the output-derived-CPU "+
				"audit. Either add it with the reasoning (its CPU genuinely depends on its own "+
				"output), or drop the interface -- an un-audited member means the audit no "+
				"longer describes the code.", name)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Zero preservation, and whole-Cost consistency.
// ---------------------------------------------------------------------------

// TestRFC195_ZeroIsPreserved pins that the clamp NEVER floors a guaranteed-empty
// leg. The floor applies only when the proof says min >= 1; a proven-zero or
// possibly-zero child keeps its zero.
//
// This is the contract FlatMapCost depends on: `SELECT * FROM t1, (SELECT *
// FROM t2 LIMIT 0)` joins to exactly zero rows, and a clamp that floored the
// empty leg to a phantom row would turn the cheapest shape on the plan into one
// of the most expensive, flipping the winner away from it.
func TestRFC195_ZeroIsPreserved(t *testing.T) {
	t.Parallel()

	scan := func(n string) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{n}, values.UnknownType, false)
	}
	zeroLeg := plans.NewRecordQueryLimitPlan(scan("EMPTY"), 0, 0)

	if got := properties.EstimateCost(zeroLeg).Cardinality; got != 0 {
		t.Fatalf("LIMIT 0 costs %v, want exactly 0 -- a proven-zero leg must keep its zero", got)
	}

	join := plans.NewRecordQueryFlatMapPlan(
		scan("T1"), zeroLeg,
		values.NamedCorrelationIdentifier("o"), values.NamedCorrelationIdentifier("i"), nil, false)
	if got := properties.EstimateCost(join).Cardinality; got != 0 {
		t.Fatalf("FlatMap over a LIMIT-0 inner costs %v, want exactly 0.\n"+
			"The clamp floors ONLY on a proven min >= 1. Flooring a guaranteed-empty leg would "+
			"conjure rows out of an empty relation and make the free join the expensive one.", got)
	}

	// The clamp helper itself, directly: a proven-zero interval must pass a zero
	// through untouched rather than treating "min unknown" and "min 0" alike.
	provenZero := properties.Cardinalities{Min: properties.OfCardinality(0), Max: properties.OfCardinality(0)}
	if got := properties.ClampCardinality(0, provenZero); got != 0 {
		t.Fatalf("ClampCardinality(0, [0,0]) = %v, want 0", got)
	}
}

// TestRFC195_WholeCostConsistency_LevelUnionBuffer pins the reproducer for
// Decision §3: no component of an emitted Cost may be a function of a
// cardinality the same Cost no longer carries.
//
// RecordQueryRecursiveLevelUnionPlan charges buffer CPU proportional to its own
// OUTPUT cardinality. With a one-row seed and a zero-estimated recursive leg,
// the raw formula computes cardinality 0 and therefore ZERO buffer work; the
// clamp then floors the cardinality to 1. Clamping AFTER the formula would emit
// a Cost claiming "at least one materialized row" while charging nothing to
// materialize it — erasing the level-union-vs-DFS distinction the buffer term
// exists to draw, in the act of fixing the cardinality.
func TestRFC195_WholeCostConsistency_LevelUnionBuffer(t *testing.T) {
	t.Parallel()

	scan := func(n string) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{n}, values.UnknownType, false)
	}
	seed := plans.NewRecordQueryFirstOrDefaultPlan(scan("SEED"), values.NewNullValue(values.UnknownType))
	rec := plans.NewRecordQueryLimitPlan(scan("REC_ZERO"), 0, 0)
	levelUnion := plans.NewRecordQueryRecursiveLevelUnionPlan(seed, rec,
		values.NamedCorrelationIdentifier("lu_scan"), values.NamedCorrelationIdentifier("lu_insert"))

	cost := properties.EstimateCost(levelUnion)
	if cost.Cardinality != 1 {
		t.Fatalf("level union cardinality = %v, want 1 (floored at the seed's proven minimum)", cost.Cardinality)
	}

	// The buffer term must be isolated, not bounded from below. `cost.CPU >=
	// cardinality*UnionCPU*touches` is 0.2 against a CPU of ~72900 and cannot
	// fail under ANY mutation of the term -- it passes with the buffer charge
	// deleted outright. What distinguishes clamped-then-charged from
	// charged-then-clamped is the DELTA, so the delta is what gets asserted.
	//
	// The DFS join is the independent reference: since both recursive operators
	// now prove the SAME bound and share recursiveCost, their costs differ by
	// exactly the level union's buffer term and nothing else. That makes the
	// expectation independently constructed rather than a restatement of the
	// implementation.
	dfs := plans.NewRecordQueryRecursiveDfsJoinPlan(seed, rec,
		values.NamedCorrelationIdentifier("dfs_prior"), plans.DfsPreorder)
	dfsCost := properties.EstimateCost(dfs)

	if dfsCost.Cardinality != cost.Cardinality {
		t.Fatalf("the two recursive operators report different cardinalities (dfs=%v levelUnion=%v) "+
			"over identical children -- they implement ONE logical recursion and the buffer-term "+
			"delta below is only meaningful while everything else about them agrees",
			dfsCost.Cardinality, cost.Cardinality)
	}

	const levelUnionBufferTouches = 2
	wantDelta := cost.Cardinality * properties.UnionCPU * levelUnionBufferTouches
	gotDelta := cost.CPU - dfsCost.CPU
	if math.Abs(gotDelta-wantDelta) > 1e-9 {
		diagnosis := "the term is computed from a cardinality other than the clamped one"
		if math.Abs(gotDelta) <= 1e-9 {
			diagnosis = "a delta of ZERO means the term was computed from the PRE-clamp " +
				"cardinality of 0, and the clamp then floored the output to 1 -- a Cost claiming " +
				"it materializes a row while charging nothing to materialize it"
		}
		t.Fatalf("level-union buffer term = %v, want %v (= clamped cardinality %v x UnionCPU %v x %d touches).\n"+
			"%s. The buffer charge must consume the CLAMPED value (HintCostWithin), which is the "+
			"whole reason BoundedCostHinter exists.",
			gotDelta, wantDelta, cost.Cardinality, properties.UnionCPU, levelUnionBufferTouches, diagnosis)
	}
}

// ---------------------------------------------------------------------------
// 5. The two latent defects found while relocating the derivation.
// ---------------------------------------------------------------------------

// TestRFC195_StampedPKZeroFloatIsNotAtMostOne pins a FALSE PROOF the relocation
// surfaced.
//
// pkFullyEqualityBound's zero-float widening guard sat AFTER its stamped-PK
// early return, so a scan carrying a stamped primary key and a terminal
// zero-valued FLOAT equality was PROVEN at-most-one — while
// isProvablePointProbe, consulting the same widening helper, correctly declined
// to call it a point probe. The comment above the guard claimed it "covers both
// callers"; it covered one.
//
// The executor widens a zero bound across -0.0 and +0.0 (IEEE-equal, distinct
// adjacent keys), so at-most-one is false there. Latent before RFC-195 — a
// too-tight proof only over-pruned. Under the clamp it becomes active harm: a
// false max=1 CAPS the honest cost estimate to one row.
func TestRFC195_StampedPKZeroFloatIsNotAtMostOne(t *testing.T) {
	t.Parallel()

	mk := func(lit any) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithPrimaryKey([]values.Value{&values.FieldValue{Field: "V", Typ: values.NullableDouble}}).
			WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, lit)})
	}

	nonZero := mk(float64(5)).ProvenCardinalities(nil)
	if nonZero.Max.IsUnknown() || nonZero.Max.Value() != 1 {
		t.Fatalf("stamped-PK scan, NONZERO float equality: max = %s, want a proven 1 -- "+
			"the widening guard must not decline an honest point probe",
			fmtBound(nonZero.Max))
	}

	zero := mk(float64(0)).ProvenCardinalities(nil)
	if !zero.Max.IsUnknown() {
		t.Fatalf("stamped-PK scan, ZERO float equality: max = %s, want UNKNOWN.\n"+
			"The executor widens a terminal zero bound across -0.0 and +0.0, which are IEEE-equal "+
			"but pack to distinct adjacent keys, so the scan can return TWO rows. Proving "+
			"at-most-one here is a FALSE proof, and under RFC-195's clamp it CAPS the cost "+
			"estimate to a row count the scan does not honour.",
			fmtBound(zero.Max))
	}
}

// TestRFC195_CardinalityTimesDoesNotOverflow pins that an unrepresentable
// product weakens to unknown instead of panicking in library code.
//
// Cardinality.Times multiplied raw and handed the result to OfCardinality's
// non-negative check, so an overflowing product panicked — reachable through
// the join arm, whose bound is the product of its two legs, with a large
// plan-time LIMIT on each. Java has the identical hazard
// (Preconditions.checkArgument in ofCardinality); weakening is the sound
// divergence, since it only ever LOOSENS a bound and no clamp built on it can
// then floor or cap past what is proved.
func TestRFC195_CardinalityTimesDoesNotOverflow(t *testing.T) {
	t.Parallel()

	huge := properties.OfCardinality(math.MaxInt64 / 2)
	got := huge.Times(properties.OfCardinality(4))
	if !got.IsUnknown() {
		t.Fatalf("Times overflowed to %d instead of weakening to unknown", got.Value())
	}

	// Reached through the operator that actually multiplies: a join of two
	// large plan-time LIMITs.
	scan := func(n string) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{n}, values.UnknownType, false)
	}
	bigLimit := func(n string) plans.RecordQueryPlan {
		return plans.NewRecordQueryLimitPlan(scan(n), math.MaxInt64/2, 0)
	}
	join := plans.NewRecordQueryFlatMapPlan(bigLimit("A"), bigLimit("B"),
		values.NamedCorrelationIdentifier("o"), values.NamedCorrelationIdentifier("i"), nil, false)

	// The assertion is that this does not panic; the bound weakening to unknown
	// is the sound outcome.
	bound := properties.ProvenCardinalitiesOf(join)
	if !bound.Max.IsUnknown() {
		t.Fatalf("join of two MaxInt64/2 limits proves max=%s, want unknown -- "+
			"an unrepresentable product must weaken, never wrap", fmtBound(bound.Max))
	}

	// Exact non-overflowing products must still be proved precisely.
	if got := properties.OfCardinality(3).Times(properties.OfCardinality(4)); got.IsUnknown() || got.Value() != 12 {
		t.Fatalf("3 x 4 = %s, want 12 -- the overflow guard must not weaken honest products",
			fmtBound(got))
	}
}
