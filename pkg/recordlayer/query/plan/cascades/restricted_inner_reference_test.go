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

// Property-equivalent children share a restricted partition, while opposite
// scan directions must remain separate alternatives.
func TestImplementFilterRuleEnumeratesDistinctRestrictedInners(t *testing.T) {
	t.Parallel()
	a, b := mkEnumIndexPlan("IDX_A"), mkEnumIndexPlan("IDX_B")
	c := mustRestrictedInnerConstruct(plans.NewRecordQueryIndexPlan("IDX_C", []*predicates.ComparisonRange{predicates.EmptyComparisonRange()}, []string{"T"}, restrictedInnerRowType(), true)).WithIndexMetadata([]string{"A"}, []string{"ID"}, false)
	innerRef := expressions.InitialOf(a)
	for _, member := range []expressions.RelationalExpression{a, b, c} {
		innerRef.InsertFinal(member)
	}
	innerQ := expressions.ForEachQuantifier(innerRef)
	pred := predicates.NewValuePredicate(restrictedInnerField(innerQ, 2))
	filter := mustRestrictedInnerConstruct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, innerQ))
	root := expressions.InitialOf(filter)
	yielded := mustFireExpressionRuleWithMemo(t, NewImplementFilterRule(), root, EmptyPlanContext(), NewMemo(root))
	if len(yielded) != 2 {
		t.Fatalf("got %d filters, want two ordering partitions", len(yielded))
	}
	seen := make(map[expressions.RelationalExpression]bool)
	for _, expression := range yielded {
		ref := expression.(*plans.RecordQueryPredicatesFilterPlan).GetInnerQuantifier().GetRangesOver()
		if ref.Canonical() == innerRef.Canonical() {
			t.Fatal("filter reused the unrestricted child reference")
		}
		members := physicalMembersForParentEnumeration(ref)
		if len(members) == 0 {
			t.Fatal("empty partition")
		}
		reverse := members[0].(*plans.RecordQueryIndexPlan).IsReverse()
		for _, member := range members {
			if member.(*plans.RecordQueryIndexPlan).IsReverse() != reverse {
				t.Fatal("partition mixes scan directions")
			}
			if seen[member] {
				t.Fatal("child appears in multiple partitions")
			}
			seen[member] = true
		}
	}
	if !seen[a] || !seen[b] || !seen[c] || len(seen) != 3 {
		t.Fatal("partitioning lost a competing access path")
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

func TestImplementFilterRuleGroupsPropertyEquivalentChildren(t *testing.T) {
	t.Parallel()
	a, b := mkEnumIndexPlan("GROUP_A"), mkEnumIndexPlan("GROUP_B")
	inner := expressions.InitialOf(a)
	inner.InsertFinal(a)
	inner.InsertFinal(b)
	computeRefPlanProperties(inner)
	q := expressions.ForEachQuantifier(inner)
	filter := mustRestrictedInnerConstruct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{predicates.NewValuePredicate(restrictedInnerField(q, 2))}, q))
	root := expressions.InitialOf(filter)
	yielded := mustFireExpressionRuleWithMemo(t, NewImplementFilterRule(), root, EmptyPlanContext(), NewMemo(root))
	if len(yielded) != 1 {
		t.Fatalf("equivalent child properties produced %d parent filters, want one partition", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryPredicatesFilterPlan)
	ref := plan.GetInnerQuantifier().GetRangesOver()
	if ref.Canonical() == inner.Canonical() {
		t.Fatal("partition must have its own restricted reference")
	}
	members := physicalMembersForParentEnumeration(ref)
	if len(members) != 2 || members[0] != a || members[1] != b {
		t.Fatal("partition lost a competing child plan")
	}
}

func TestCompensatedFiltersDeduplicateLocalAliases(t *testing.T) {
	t.Parallel()
	scan := mkEnumIndexPlan("ALIAS_DEDUP")
	child := expressions.PinnedFinalOf(scan)
	makeFilter := func(ordinal int) *plans.RecordQueryPredicatesFilterPlan {
		q := expressions.ForEachQuantifier(child)
		predicate := predicates.NewComparisonPredicate(restrictedInnerField(q, ordinal), predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})
		return mustRestrictedInnerConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(q, []predicates.QueryPredicate{predicate}, q.GetAlias()))
	}
	first, same, different := makeFilter(0), makeFilter(0), makeFilter(1)
	ref := expressions.FinalOf(first)
	if ref.InsertFinal(same) {
		t.Error("alpha-equivalent compensation minted another physical filter")
	}
	if !ref.InsertFinal(different) {
		t.Error("different filter predicate was discarded")
	}
	if len(ref.FinalMembers()) != 2 {
		t.Fatalf("final members=%d, want two predicate alternatives", len(ref.FinalMembers()))
	}
}

func TestUnboundFiltersDeduplicateLocalAliases(t *testing.T) {
	t.Parallel()
	child := expressions.PinnedFinalOf(mkEnumIndexPlan("UNBOUND_ALIAS_DEDUP"))
	makeFilter := func() *plans.RecordQueryPredicatesFilterPlan {
		q := expressions.ForEachQuantifier(child)
		predicate := predicates.NewComparisonPredicate(restrictedInnerField(q, 0), predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})
		return mustRestrictedInnerConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(q, []predicates.QueryPredicate{predicate}))
	}
	first, same := makeFilter(), makeFilter()
	if !first.InternsAliasAware() {
		t.Fatal("filter without an extra binding must permit renaming its local quantifier")
	}
	if expressions.FinalOf(first).InsertFinal(same) {
		t.Fatal("alpha-equivalent unbound filter minted another final")
	}
}

func TestMemoizedPhysicalFinalStartsAtPlannedStage(t *testing.T) {
	t.Parallel()
	plan := mkEnumIndexPlan("MEMOIZED_FINAL_STAGE")
	for _, ref := range []*expressions.Reference{
		(&ExpressionRuleCall{}).MemoizeFinalExpression(plan),
		(&ImplementationRuleCall{}).MemoizeFinalExpression(plan),
	} {
		if ref.Stage() != expressions.StagePlanned {
			t.Fatalf("physical final stage = %v, want planned", ref.Stage())
		}
		if !ref.NeedsExploration() {
			t.Fatal("new physical plan still needs physical-rule exploration")
		}
	}
}

func TestMemoizedFinalPartitionPreservesSourceStage(t *testing.T) {
	t.Parallel()
	for _, stage := range []expressions.PlannerStage{expressions.StageCanonical, expressions.StagePlanned} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			t.Parallel()
			a, b := mkEnumIndexPlan("PARTITION_A"), mkEnumIndexPlan("PARTITION_B")
			source := expressions.FinalOfAtStage(a, stage)
			source.InsertFinal(b)
			constraints := NewConstraintMap()
			Set(constraints, source, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{properties.PreserveOrdering()})
			source.ConstraintsMap().SetExplored()
			ref := (&ImplementationRuleCall{Constraints: constraints}).MemoizeFinalExpressionsFromOther(source, []expressions.RelationalExpression{a, b})
			if ref.Stage() != stage || ref.NeedsExploration() {
				t.Fatalf("restricted partition stage=%v needsExploration=%v, want stage=%v and completed exploration", ref.Stage(), ref.NeedsExploration(), stage)
			}
			if len(ref.FinalMembers()) != 2 || len(ref.Members()) != 0 {
				t.Fatal("restriction changed final membership")
			}
		})
	}
}

func TestRestrictedFinalReferencePreservesCompletedExploration(t *testing.T) {
	t.Parallel()
	a, b := mkEnumIndexPlan("EXPLORED_A"), mkEnumIndexPlan("EXPLORED_B")
	source := expressions.FinalOfAtStage(a, expressions.StagePlanned)
	source.InsertFinal(b)
	source.ConstraintsMap().SetExplored()
	ref := newRestrictedFinalReference("test", source, []expressions.RelationalExpression{a, b}, expressions.StagePlanned)
	if ref.NeedsExploration() {
		t.Fatal("restricted completed plans were scheduled for fresh exploration")
	}
	ref.ConstraintsMap().ReArm()
	if !ref.NeedsExploration() {
		t.Fatal("new constraint could not rearm restricted plans")
	}
}

func TestLogicalCompensationsDeduplicateLocalAliases(t *testing.T) {
	t.Parallel()
	child := expressions.PinnedFinalOf(mkEnumIndexPlan("LOGICAL_ALIAS_DEDUP"))
	makeFilter := func(outer values.CorrelationIdentifier) *expressions.LogicalFilterExpression {
		q := expressions.ForEachQuantifier(child)
		operand := values.Value(values.LiteralValue(int64(1)))
		if !outer.IsZero() {
			operand = mustRestrictedInnerConstruct(values.NewQuantifiedObjectValue(outer, values.NotNullLong))
		}
		predicate := predicates.NewComparisonPredicate(restrictedInnerField(q, 0), predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand})
		return mustRestrictedInnerConstruct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{predicate}, q))
	}
	first, same := makeFilter(values.CorrelationIdentifier{}), makeFilter(values.CorrelationIdentifier{})
	ref := expressions.InitialOf(first)
	if ref.Insert(same) {
		t.Error("alpha-equivalent logical compensation minted another member")
	}
	for range 2 {
		if !ref.Insert(makeFilter(values.UniqueCorrelationIdentifier())) {
			t.Error("different outer correlation was discarded")
		}
	}
	if len(ref.Members()) != 3 {
		t.Fatalf("members=%d, want one local and two outer-correlated filters", len(ref.Members()))
	}
}
