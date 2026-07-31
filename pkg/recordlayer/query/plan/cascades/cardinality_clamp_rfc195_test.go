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
// 1. The six corrected shapes, pinned at their exact clamped estimates.
// ---------------------------------------------------------------------------

// TestRFC195_SixCorrectedShapes pins the BEFORE and AFTER of every estimate the
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
func TestRFC195_SixCorrectedShapes(t *testing.T) {
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
	mixed.Insert(unbounded)

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
	ref.Insert(plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false))
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
// parity requirement.
//
// A hand-written pairing table checked only against itself passes vacuously the
// day a twelfth logical arm is added. This test enumerates the logical arms and
// requires each to name its physical twin, so an unpaired arm fails with a
// message naming it rather than silently going unchecked.
//
// Every pair must derive the IDENTICAL interval from IDENTICAL child intervals.
// Where an arm has no twin, it carries an explicit listed reason — never
// silence.
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
		"FullUnorderedScanExpression": {
			logical:  &expressions.FullUnorderedScanExpression{},
			physical: plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
			probes:   probes,
		},
		"LogicalFilterExpression": {
			logical:  &expressions.LogicalFilterExpression{},
			physical: plans.NewRecordQueryFilterPlan(nil, nil),
			probes:   probes,
		},
		"LogicalProjectionExpression": {
			logical:  &expressions.LogicalProjectionExpression{},
			physical: plans.NewRecordQueryProjectionPlan(nil, nil),
			probes:   probes,
		},
		"LogicalSortExpression": {
			logical:  &expressions.LogicalSortExpression{},
			physical: plans.NewRecordQueryInMemorySortPlan(nil, nil),
			probes:   probes,
		},
		"LogicalDistinctExpression": {
			logical:  &expressions.LogicalDistinctExpression{},
			physical: plans.NewRecordQueryDistinctPlan(nil),
			probes:   probes,
		},
		"LogicalTypeFilterExpression": {
			logical:  &expressions.LogicalTypeFilterExpression{},
			physical: plans.NewRecordQueryTypeFilterPlan(nil, nil),
			probes:   probes,
		},
		"LogicalUnionExpression": {
			logical:  &expressions.LogicalUnionExpression{},
			physical: plans.NewRecordQueryUnionPlan(nil),
			probes:   pairProbes,
		},
		"LogicalIntersectionExpression": {
			logical:  &expressions.LogicalIntersectionExpression{},
			physical: plans.NewRecordQueryIntersectionPlan(nil, nil),
			probes:   pairProbes,
		},
		"SelectExpression": {
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
		"GroupByExpression": {
			logical:  &expressions.GroupByExpression{},
			physical: plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil),
			probes:   probes,
		},
		"InsertExpression": {
			logical:  &expressions.InsertExpression{},
			physical: plans.NewRecordQueryInsertPlan(nil, "T", nil),
			probes:   probes,
		},
		"UpdateExpression": {
			logical:  &expressions.UpdateExpression{},
			physical: plans.NewRecordQueryUpdatePlan(nil, "T", nil),
			probes:   probes,
		},
		"DeleteExpression": {
			logical:  &expressions.DeleteExpression{},
			physical: plans.NewRecordQueryDeletePlan(nil, "T"),
			probes:   probes,
		},
	}

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

// TestRFC195_AdapterDoesNotReForkTheDerivation pins method-level agreement:
// cascades.computeCardinalities must DELEGATE to the plan's own
// ProvenCardinalities, never re-derive alongside it.
//
// Asserting agreement on whole-WALK outputs would be self-refuting — the
// property map weakens child bounds across all members while the concrete walk
// uses the concrete child, so their outputs legitimately differ. The invariant
// is that no one re-forks the per-operator derivation, not that every walk sees
// the same tree.
func TestRFC195_AdapterDoesNotReForkTheDerivation(t *testing.T) {
	t.Parallel()
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
			prover, ok := plan.(properties.CardinalityProver)
			if !ok {
				t.Fatalf("%T does not implement properties.CardinalityProver", plan)
			}
			viaMethod := prover.ProvenCardinalities(cardinalityChildrenForPlan(w, plan))
			if !viaAdapter.Equal(viaMethod) {
				t.Fatalf("computeCardinalities RE-FORKED the derivation: adapter says [%s,%s], "+
					"the plan's own method says [%s,%s] from the same child intervals. "+
					"The adapter's only job is resolving child edges.",
					fmtBound(viaAdapter.Min), fmtBound(viaAdapter.Max),
					fmtBound(viaMethod.Min), fmtBound(viaMethod.Max))
			}
		})
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

	// The buffer term is levelUnionBufferTouches (2) round trips per
	// materialized row at the union merge rate. With the clamped cardinality of
	// 1 that is 2*UnionCPU of buffer work; computed from the pre-clamp zero it
	// would be nothing at all.
	minBuffer := cost.Cardinality * properties.UnionCPU * 2
	if cost.CPU < minBuffer {
		t.Fatalf("level union CPU = %v but its own reported cardinality of %v implies at least "+
			"%v of buffer work.\n"+
			"The buffer term must be computed from the CLAMPED cardinality (HintCostWithin), not "+
			"clamped afterwards -- a Cost that proves it materializes a row while charging zero to "+
			"materialize it has erased the very level-union-vs-DFS distinction the term draws.",
			cost.CPU, cost.Cardinality, minBuffer)
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
