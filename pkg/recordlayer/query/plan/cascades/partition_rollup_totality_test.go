package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Roll-up is the gate that makes a partition-level property read TOTAL. These
// tests drive it with a deliberately HETEROGENEOUS partition — members that
// disagree on the property being read — because that is the only arm where the
// guarantee is observable, and it is the arm no corpus query has ever reached.
//
// Without roll-up a rule reads whichever member created the partition, and the
// corpus hides that completely: the queries that reach these rules happen to
// produce single-member partitions, so member[0] IS the whole group and the
// wrong read returns the right answer. The bug only appears once a property
// change makes partitions heterogeneous — which is exactly how it appeared.

// twoMemberRefWithDisagreeingProperty builds a reference whose PlanPropertiesMap
// holds two physical members that DISAGREE on DistinctRecords: a scan (true) and
// a projection over it (false). They agree on StoredRecord, so under a partition
// key that ignores DistinctRecords they would share a partition.
func twoMemberRefWithDisagreeingProperty(t *testing.T) (*expressions.Reference, expressions.RelationalExpression, expressions.RelationalExpression) {
	t.Helper()
	rowType := values.NewRecordType("Order", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	scanValue, scanErr := plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false)
	scan := mustConstruct(t, scanValue, scanErr)
	projValue, projErr := plans.NewRecordQueryProjectionPlan(nil, scan)
	proj := mustConstruct(t, projValue, projErr)

	ref := expressions.FinalOfAtStage(scan, expressions.StageCanonical)
	ref.InsertFinal(proj)
	computeRefPlanProperties(ref)

	if !computeWrapperProperties(scan).GetBool(properties.PropDistinctRecords) {
		t.Fatalf("premise broken: a scan must report DistinctRecords")
	}
	if computeWrapperProperties(proj).GetBool(properties.PropDistinctRecords) {
		t.Fatalf("premise broken: a projection must not report DistinctRecords")
	}
	return ref, scan, proj
}

// TestRollUp_PartitionValueDescribesEveryMember is the gate itself. After
// rolling up to a property, every member of every resulting partition must
// carry the partition's value for it — that is what licenses a rule to read it
// off the partition.
func TestRollUp_PartitionValueDescribesEveryMember(t *testing.T) {
	t.Parallel()
	ref, _, _ := twoMemberRefWithDisagreeingProperty(t)

	raw := ToPlanPartitions(ref)
	if len(raw) == 0 {
		t.Fatalf("no partitions; the totality assertion below would hold vacuously")
	}
	totalMembers := 0
	for _, p := range raw {
		totalMembers += len(p.GetExpressions())
	}
	if totalMembers < 2 {
		t.Fatalf("only %d member(s) reached the partitioner, so no partition can "+
			"be heterogeneous and this file tests nothing", totalMembers)
	}

	for _, prop := range []*properties.ExpressionProperty{
		properties.PropDistinctRecords,
		properties.PropStoredRecord,
		properties.PropOrdering,
		properties.PropRichOrdering,
	} {
		rolled := RollUpPlanPartitions(raw, prop)
		if len(rolled) == 0 {
			t.Fatalf("%s: roll-up produced no partitions", prop)
		}
		seen := 0
		for _, p := range rolled {
			want := p.GetPartitionPropertyValue(prop)
			for _, e := range p.GetExpressions() {
				seen++
				got := p.GetExpressionPropertyValue(e, prop)
				if !partitionPropValueEqual(want, got) {
					t.Errorf("%s: partition value %v does not describe member %T "+
						"(%v). A rule reading this property off the partition "+
						"would be asserting one member's fact of the whole group — "+
						"the defect roll-up exists to prevent", prop, want, e, got)
				}
			}
		}
		if seen != totalMembers {
			t.Errorf("%s: roll-up carried %d members, input had %d — roll-up must "+
				"repartition members, never drop them", prop, seen, totalMembers)
		}
	}
}

// TestRollUp_SplitsMembersThatDisagree pins the DIRECTION. Roll-up must SPLIT
// disagreeing members into different partitions; merging them is what produces
// a partition whose value describes only some of its members. This is the arm
// that goes green for the wrong reason if the property equality is too
// permissive — a too-permissive equality merges and the totality check above
// then compares a value against itself.
func TestRollUp_SplitsMembersThatDisagree(t *testing.T) {
	t.Parallel()
	ref, scan, proj := twoMemberRefWithDisagreeingProperty(t)

	rolled := RollUpPlanPartitions(ToPlanPartitions(ref), properties.PropDistinctRecords)

	partitionOf := func(target expressions.RelationalExpression) *PlanPartition {
		for _, p := range rolled {
			for _, e := range p.GetExpressions() {
				if e == target {
					return p
				}
			}
		}
		return nil
	}
	scanPart, projPart := partitionOf(scan), partitionOf(proj)
	if scanPart == nil || projPart == nil {
		t.Fatalf("roll-up dropped a member (scan found=%v, projection found=%v)",
			scanPart != nil, projPart != nil)
	}
	if scanPart == projPart {
		t.Fatalf("roll-up on DistinctRecords put a member reporting true and a " +
			"member reporting false in ONE partition; the partition's value then " +
			"describes only one of them")
	}
	if !scanPart.IsDistinct() {
		t.Errorf("scan's partition reports DistinctRecords false, want true")
	}
	if projPart.IsDistinct() {
		t.Errorf("projection's partition reports DistinctRecords true, want false")
	}
}

// TestRichOrderingsEqual_SeparatesOrderingSets guards the arm of the property
// equality that OVER-MERGES when it is missing.
//
// Roll-up's whole guarantee rests on its equality: merge two members whose
// property values actually differ and the resulting partition reports one
// member's value for both — which is the read-off-a-member defect roll-up
// exists to remove, arriving through the equality instead of through the rule.
// The ordering SET is the part that is easy to leave out, because two orderings
// can carry identical key sequences and identical bindings and still admit
// different comparison keys: the set is the partial order that
// EnumerateSatisfyingComparisonKeyValues walks.
//
// Omitting it is not hypothetical — it was omitted on the first attempt and
// measured as six ordered InUnions collapsing into InMemorySort. Nothing else
// in the suite distinguishes two orderings by their set alone.
func TestRichOrderingsEqual_SeparatesOrderingSets(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("ordering_keys", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	})
	rootValue, rootErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("rollup_ordering"), rowType)
	root := mustConstruct(t, rootValue, rootErr)
	keyAValue, keyAErr := values.ResolveFieldOrdinals(root, []int{0})
	keyA := mustConstruct(t, keyAValue, keyAErr)
	keyBValue, keyBErr := values.ResolveFieldOrdinals(root, []int{1})
	keyB := mustConstruct(t, keyBValue, keyBErr)
	keys := []values.Value{keyA, keyB}
	bindings := map[values.Value][]properties.OrderingBinding{
		keyA: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		keyB: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
	}

	// Same keys, same bindings, same distinctness — the ONLY difference is the
	// dependency structure, i.e. whether B is ordered after A or the two are
	// unconstrained relative to each other.
	//
	// The antichain has to be built from an EMPTY-but-non-nil dependency map.
	// Passing nil does NOT mean "no dependencies": NewRichOrderingWithDeps
	// treats nil as "derive the natural order" and chains each sorted key after
	// the previous one (rich_ordering.go:68-85), which is exactly the A->B chain
	// the other fixture states explicitly — so a nil-vs-explicit pair produces
	// two EQUAL sets and compares nothing.
	chainDeps := combinatorics.NewSetMultimap[string]()
	chainDeps.Put(values.ExplainValue(keyB), values.ExplainValue(keyA))
	antichainDeps := combinatorics.NewSetMultimap[string]()

	chained := properties.NewRichOrderingWithDeps(bindings, keys, chainDeps, properties.DistinctOverAllKeys())
	unconstrained := properties.NewRichOrderingWithDeps(bindings, keys, antichainDeps, properties.DistinctOverAllKeys())

	// Vacuity guard, and it FAILS rather than skips: if the two fixtures stop
	// producing different ordering sets the test can no longer express the
	// distinction it exists for, and silently passing then reads as evidence
	// the guard is intact when nothing was compared.
	if chained.OrderingSet().Equal(unconstrained.OrderingSet()) {
		t.Fatalf("the two fixtures produced EQUAL ordering sets, so the merge " +
			"assertion below would hold vacuously; rebuild the fixtures so they " +
			"differ in their ordering set alone")
	}
	if richOrderingsEqual(chained, unconstrained) {
		t.Fatalf("richOrderingsEqual merged two orderings that differ ONLY in " +
			"their ordering set. Roll-up would then place both in one partition " +
			"and report one member's ordering for the other — the read-off-a-" +
			"member defect, reintroduced through the equality")
	}
	// The equality must still be reflexive, or roll-up would split every member
	// into its own partition and the totality check above would hold vacuously.
	if !richOrderingsEqual(chained, chained) || !richOrderingsEqual(unconstrained, unconstrained) {
		t.Fatalf("richOrderingsEqual is not reflexive; roll-up would never merge " +
			"anything and every partition would be a singleton")
	}
}

// TestRollUp_GroupsByMemberValueNotSourcePartitionValue is the regression for
// the mechanism. RollUpPlanPartitions must group by each EXPRESSION's own
// property value; grouping by the source partition's value propagates whichever
// member created that partition to the whole group, which is the very defect
// being fixed one layer up.
func TestRollUp_GroupsByMemberValueNotSourcePartitionValue(t *testing.T) {
	t.Parallel()
	ref, _, _ := twoMemberRefWithDisagreeingProperty(t)

	raw := ToPlanPartitions(ref)
	// Force the heterogeneous input the mechanism has to survive: one partition
	// holding both members, carrying ONE member's value for the property. This
	// is the shape toPartitionsFromMap produces for any property outside its
	// three-part key, so it is not a contrived input.
	merged := &PlanPartition{
		partitionProps: properties.PropertyMap{},
		exprProps:      map[expressions.RelationalExpression]properties.PropertyMap{},
	}
	n := 0
	for _, p := range raw {
		for _, e := range p.GetExpressions() {
			if n == 0 {
				merged.partitionProps = p.exprProps[e]
			}
			merged.addExpression(e, p.exprProps[e])
			n++
		}
	}
	if n < 2 {
		t.Fatalf("forced partition holds %d member(s); it must hold both for the "+
			"grouping question to be asked at all", n)
	}

	rolled := RollUpPlanPartitions([]*PlanPartition{merged}, properties.PropDistinctRecords)
	if len(rolled) != 2 {
		t.Fatalf("roll-up of a single heterogeneous partition produced %d "+
			"partition(s), want 2 — it grouped by the SOURCE partition's value "+
			"(one member's) instead of by each member's own", len(rolled))
	}
}
