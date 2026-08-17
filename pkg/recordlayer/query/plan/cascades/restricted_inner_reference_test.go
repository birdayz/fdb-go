package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRestrictedInnerConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct restricted-inner fixture: " + err.Error())
	}
	return value
}

func restrictedInnerRowType() values.Type {
	return values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "active", FieldType: values.TypeBool, Ordinal: 2},
	})
}

func restrictedInnerField(q expressions.Quantifier, ordinal int) values.Value {
	root := mustRestrictedInnerConstruct(q.RequireFlowedObjectValue())
	return mustRestrictedInnerConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

// mkEnumIndexPlan builds a distinct physical index plan for the enumeration
// pins below. Distinctness matters: a group whose members dedup would make an
// arity assertion pass for the wrong reason.
func mkEnumIndexPlan(name string) *plans.RecordQueryIndexPlan {
	return mustRestrictedInnerConstruct(plans.NewRecordQueryIndexPlan(
		name, []*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
		[]string{"T"}, restrictedInnerRowType(), false,
	)).WithIndexMetadata([]string{"A"}, []string{"ID"}, false)
}

// singleMemberOf returns the sole physical member of ref, or an error string
// describing why it is not sole. A restricted reference minted for a parent
// alternative must hold EXACTLY the one member the parent was built over;
// holding the whole child group is the defect these pins exist for.
func singleMemberOf(ref *expressions.Reference) (expressions.RelationalExpression, string) {
	if ref == nil {
		return nil, "reference is nil"
	}
	ms := physicalMembersForParentEnumeration(ref)
	if len(ms) != 1 {
		var names []string
		for _, m := range ms {
			names = append(names, describeEnumMember(m))
		}
		return nil, fmt.Sprintf("holds %d physical members %v, want exactly 1", len(ms), names)
	}
	return ms[0], ""
}

func describeEnumMember(m expressions.RelationalExpression) string {
	if p, ok := m.(interface{ GetRecordQueryPlan() plans.RecordQueryPlan }); ok {
		if ip, ok := p.GetRecordQueryPlan().(*plans.RecordQueryIndexPlan); ok {
			return ip.GetIndexName()
		}
	}
	if ip, ok := m.(*plans.RecordQueryIndexPlan); ok {
		return ip.GetIndexName()
	}
	return fmt.Sprintf("%T", m)
}

// TestImplementFilterRuleEnumeratesDistinctRestrictedInners drives the RULE,
// not the helper it calls.
//
// physicalMembersForParentEnumeration returning N members is necessary but not
// sufficient: the rule must also give each yielded parent an inner reference
// RESTRICTED to that one member. Memoizing a member through the interning path
// hands back the group that ALREADY CONTAINS it — the child group itself — so
// every one of the N yields is the structurally identical Filter(childGroup)
// and they collapse to one on insert. The loop then enumerates nothing.
//
// That collapse is invisible to a helper-level test (the helper is correct) and
// invisible to any plan-shape assertion (an alternative that was never
// constructed does not lose, it does not exist). Only counting the DISTINCT
// inners the rule actually built can tell the two apart.
//
// Java's shape is FinalMemoizer.memoizeMemberPlansFromOther
// (CascadesRuleCall.java:518 → Reference.newReferenceFromFinalMembers:587),
// used by ImplementFilterRule.java:89. Its contract is explicit: "The reference
// that is returned is always newly created and never reused."
func TestImplementFilterRuleEnumeratesDistinctRestrictedInners(t *testing.T) {
	t.Parallel()

	a := mkEnumIndexPlan("IDX_A")
	b := mkEnumIndexPlan("IDX_B")
	c := mkEnumIndexPlan("IDX_C")

	innerRef := expressions.InitialOf(a)
	innerRef.InsertFinal(a)
	innerRef.InsertFinal(b)
	innerRef.InsertFinal(c)

	innerQ := expressions.ForEachQuantifier(innerRef)
	pred := predicates.NewValuePredicate(restrictedInnerField(innerQ, 2))
	filter := mustRestrictedInnerConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		innerQ,
	))
	topRef := expressions.InitialOf(filter)

	// A MEMO is mandatory here. Without one MemoizeExpression falls back to
	// expressions.InitialOf, which mints a fresh reference per member and
	// therefore hides the defect completely — a memo-less version of this test
	// passes with the interning bug fully present.
	memo := NewMemo(topRef)
	yielded := mustFireExpressionRuleWithMemo(t, NewImplementFilterRule(), topRef, EmptyPlanContext(), memo)

	byMember := map[expressions.RelationalExpression]bool{}
	seenRefs := map[*expressions.Reference]bool{}
	for _, y := range yielded {
		fp, ok := y.(*plans.RecordQueryPredicatesFilterPlan)
		if !ok {
			continue
		}
		ref := fp.GetInnerQuantifier().GetRangesOver()
		m, why := singleMemberOf(ref)
		if why != "" {
			t.Fatalf("a yielded Filter ranges over a reference that %s.\n"+
				"Each enumerated parent must range over a reference RESTRICTED to "+
				"the single child member it was built over. Handing back the whole "+
				"child group makes every yield structurally identical, so the N "+
				"alternatives collapse into one and the enumeration is a no-op.\n"+
				"Java: Reference.newReferenceFromFinalMembers — 'always newly "+
				"created and never reused'.", why)
		}
		seenRefs[ref.Canonical()] = true
		byMember[m] = true
	}

	if len(byMember) != 3 {
		var names []string
		for m := range byMember {
			names = append(names, describeEnumMember(m))
		}
		t.Fatalf("the rule built parents over %d DISTINCT child members %v, want 3.\n"+
			"yielded %d expressions in total. Fewer distinct inners than child "+
			"members means some alternative was never constructed.",
			len(byMember), names, len(yielded))
	}
	if len(seenRefs) != 3 {
		t.Fatalf("the 3 parents share %d distinct inner references, want 3 — "+
			"two parents ranging over the same reference are the same expression",
			len(seenRefs))
	}
}

// TestImplementProjectionRuleEnumeratesDistinctRestrictedInners pins the
// parent half of the covering cost ladder. The projection implementation rule
// must construct a separate parent over every retained physical child, rather
// than selecting one local child and making every other access path invisible
// to root-level costing.
func TestImplementProjectionRuleEnumeratesDistinctRestrictedInners(t *testing.T) {
	t.Parallel()

	a := mkEnumIndexPlan("IDX_PROJ_A")
	b := mkEnumIndexPlan("IDX_PROJ_B")
	c := mkEnumIndexPlan("IDX_PROJ_C")

	innerRef := expressions.InitialOf(a)
	innerRef.InsertFinal(a)
	innerRef.InsertFinal(b)
	innerRef.InsertFinal(c)
	innerQ := expressions.ForEachQuantifier(innerRef)
	logicalAlias := innerQ.GetAlias()
	projected := restrictedInnerField(innerQ, 0)
	projection := mustRestrictedInnerConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{projected}, innerQ))
	topRef := expressions.InitialOf(projection)

	memo := NewMemo(topRef)
	yielded := mustFireExpressionRuleWithMemo(
		t, NewImplementProjectionRule(), topRef, EmptyPlanContext(), memo)

	byMember := map[expressions.RelationalExpression]bool{}
	seenRefs := map[*expressions.Reference]bool{}
	for _, yieldedExpression := range yielded {
		projectionPlan, ok := yieldedExpression.(*plans.RecordQueryProjectionPlan)
		if !ok {
			continue
		}
		ref := projectionPlan.GetInnerQuantifier().GetRangesOver()
		if got := projectionPlan.GetInnerQuantifier().GetAlias(); got != logicalAlias {
			t.Fatalf("projection physical edge alias = %s, want logical child alias %s; "+
				"retained Values and fetch translation are expressed in that domain", got.Name(), logicalAlias.Name())
		}
		if got := ref.Stage(); got != expressions.StagePlanned {
			t.Fatalf("a restricted physical projection child has stage %v, want StagePlanned; "+
				"a canonical-stage visit promotes the chosen final out of the final lane, "+
				"allowing a later physical rewrite to replace rather than compete with it", got)
		}
		if !ref.IsPinnedFinal() {
			t.Fatal("a restricted physical projection child is not marked as a pinned final selection")
		}
		member, why := singleMemberOf(ref)
		if why != "" {
			t.Fatalf("a yielded Projection ranges over a reference that %s", why)
		}
		seenRefs[ref.Canonical()] = true
		byMember[member] = true
	}

	if len(byMember) != 3 {
		t.Fatalf("the rule built parents over %d distinct child members, want 3 (yielded %d expressions)",
			len(byMember), len(yielded))
	}
	if len(seenRefs) != 3 {
		t.Fatalf("the 3 projection parents share %d distinct inner references, want 3", len(seenRefs))
	}
}

// TestImplementDeleteRuleMixedGroupDoesNotBypassTheDedup drives the dimension
// both existing DML pins leave unprobed: a child group holding a DISTINCT
// member and a NON-DISTINCT member at the same time.
//
// With single-member groups the interning memoize is indistinguishable from a
// restriction — the group IS the member. Mix them and the two diverge in a way
// that is a correctness bug, not merely a missed alternative. The distinct
// candidate is licensed to skip the primary-key dedup, so the rule yields
// Delete(ref). If ref is the interned CHILD GROUP rather than a reference
// restricted to the distinct member, that undeduped DELETE ranges over a group
// that ALSO contains the non-distinct member — and plan extraction may resolve
// it to exactly that member. The mutation then sees the same stored record more
// than once with no dedup in the plan to drop the repeat. That is the Halloween
// dedup bypassed, and cost makes it likelier rather than less: the undeduped
// plan is the cheaper of the two.
//
// Java cannot reach this state. ImplementDeleteRule.java:78-82 memoizes
// innerPlanPartition.getPlans() — a partition, whose members share their
// DistinctRecords value by construction — and reads the dedup decision from
// that same partition. The property and the reference always describe the same
// set of plans.
func TestImplementDeleteRuleMixedGroupDoesNotBypassTheDedup(t *testing.T) {
	t.Parallel()

	// Both members are NON-LEAF, over a shared child group. That is not
	// incidental: the memo's interning has two lookup paths and only one of them
	// can reach a mixed group here. memoizeLeaf scans `leafRefs`, and a group is
	// registered as a leaf only when NO member has quantifiers — so a group
	// holding one leaf plan and one non-leaf plan is absent from leafRefs, the
	// leaf member's memoize misses, and it gets a fresh single-member reference
	// by accident. A fixture built that way exhibits the over-broad reference on
	// the DEDUPED side while the undeduped side escapes, which hides exactly the
	// correctness half of the defect. memoizeNonLeaf looks a group up through its
	// children, so with both members non-leaf both memoize to the same group and
	// both halves are exercised.
	childScan := mustRestrictedInnerConstruct(plans.NewRecordQueryScanPlan(
		[]string{"Order"}, restrictedInnerRowType(), false))
	childRef := expressions.InitialOf(childScan)
	computeRefPlanProperties(childRef)

	// stored + distinct: a filter delegates record-level distinctness to its
	// child, and the child is a primary scan.
	distinctPlan := mustRestrictedInnerConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		expressions.ForEachQuantifier(childRef),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}))
	// stored but NOT distinct: a projection can map two records onto one tuple.
	// Project every slot so it stays an exact co-member of the pass-through
	// filter while still carrying projection's non-distinct property.
	projectionQ := expressions.ForEachQuantifier(childRef)
	nonDistinctPlan := mustRestrictedInnerConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{
			restrictedInnerField(projectionQ, 0),
			restrictedInnerField(projectionQ, 1),
			restrictedInnerField(projectionQ, 2),
		}, nil, projectionQ))

	// EXPLORATORY members, which is how the planner actually populates a group:
	// implementation rules Yield into the reference (Reference.Insert), and the
	// memo's lookup index is keyed on exploratory members. A group built purely
	// from InsertFinal is invisible to that lookup, so MemoizeExpression falls
	// through to minting a fresh reference and the defect this test is about
	// cannot occur — a finals-only version of this setup passes with the bug
	// fully present.
	innerRef := expressions.InitialOf(distinctPlan)
	innerRef.Insert(nonDistinctPlan)
	computeRefPlanProperties(innerRef)

	// Both halves of the premise, so the assertions below cannot hold for the
	// wrong reason if a property derivation changes.
	if !computeWrapperProperties(distinctPlan).GetBool(properties.PropDistinctRecords) {
		t.Fatalf("premise broken: the scan member must report DistinctRecords, " +
			"else no candidate takes the dedup-skipping arm and the bypass this " +
			"test is about is unreachable")
	}
	if computeWrapperProperties(nonDistinctPlan).GetBool(properties.PropDistinctRecords) {
		t.Fatalf("premise broken: the projection member must NOT report " +
			"DistinctRecords, else the group is not mixed and this test degenerates " +
			"into the single-class case the existing pins already cover")
	}

	del := mustRestrictedInnerConstruct(expressions.NewDeleteExpression(
		expressions.ForEachQuantifier(innerRef), "Order"))
	topRef := expressions.InitialOf(del)
	memo := NewMemo(topRef)
	yielded := mustFireExpressionRuleWithMemo(t, NewImplementDeleteRule(), topRef, EmptyPlanContext(), memo)

	if len(yielded) != 2 {
		t.Fatalf("ImplementDeleteRule yielded %d plans over a 2-member mixed "+
			"group, want 2 — one per stored-record access path", len(yielded))
	}

	sawDeduped, sawUndeduped := false, false
	for _, y := range yielded {
		plan, ok := y.(*plans.RecordQueryDeletePlan)
		if !ok {
			t.Fatalf("yield = %T, want *plans.RecordQueryDeletePlan", y)
		}
		if d, isDedup := plan.GetInner().(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan); isDedup {
			sawDeduped = true
			ref := d.GetInnerQuantifier().GetRangesOver()
			// Errorf, not Fatalf: the UNDEDUPED arm below is the correctness
			// assertion, and stopping here would leave it unexercised on exactly
			// the run where it matters — a mutation reverting the restriction
			// breaks both arms, and only one of them would ever be seen.
			if _, why := singleMemberOf(ref); why != "" {
				t.Errorf("the DEDUPED delete's inner reference %s; it must hold "+
					"only the non-distinct access path the dedup was built for", why)
			}
			continue
		}
		sawUndeduped = true
		// The bite. An undeduped DELETE is sound only if the reference it ranges
		// over cannot resolve to a non-distinct plan.
		ref := plan.GetQuantifiers()[0].GetRangesOver()
		m, why := singleMemberOf(ref)
		if why != "" {
			t.Errorf("an UNDEDUPED delete ranges over a reference that %s.\n"+
				"The dedup was skipped because ONE candidate reported "+
				"DistinctRecords, but the reference still offers the others — "+
				"including a non-distinct one. Extraction may resolve this plan to "+
				"a member that presents a stored record more than once, with no "+
				"dedup in the plan to drop the repeat. The DistinctRecords reading "+
				"and the reference must describe the SAME set of plans "+
				"(ImplementDeleteRule.java:78-82).", why)
			continue
		}
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			t.Fatalf("undeduped delete's sole inner member %T is not physical", m)
		}
		if !computeWrapperProperties(ph).GetBool(properties.PropDistinctRecords) {
			t.Fatal("the undeduped delete's sole inner member does NOT report " +
				"DistinctRecords — the dedup was skipped over an access path that " +
				"needed it")
		}
	}
	if !sawDeduped {
		t.Fatal("no DEDUPED delete was yielded; the non-distinct member never " +
			"became a candidate, so the mixed-group premise collapsed")
	}
	if !sawUndeduped {
		t.Fatal("no UNDEDUPED delete was yielded; the distinct member never took " +
			"the dedup-skipping arm, so the bypass assertion above held vacuously")
	}
}
