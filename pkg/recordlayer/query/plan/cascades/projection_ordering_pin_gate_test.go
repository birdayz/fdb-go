package cascades

// A projection's own ordering claim CANNOT elide a sort, and this pins why.
//
// This is a NEGATIVE result, recorded because a decision was made on it. Porting
// Java's OrderingProperty.visitMapPlan pull-up to the projection provider
// (plans/ordering.go) was expected to restore a lost elision -- corpus entry
// `aggregate_order_by_java#17`, `SELECT score + 0 AS id, id AS y FROM (SELECT
// score, id FROM scores) d ORDER BY 2 LIMIT 3`, which gained an InMemorySort when
// the providers started carrying ordinals. It did not, and the plan-shape golden
// moved ZERO existing lines. Measured instead:
//
//	across the explaindiff corpus, ImplementSortRule reaches 69 projection
//	expressions whose ordering property SATISFIES the request and whose
//	pinOrderedSpine then returns nil, so no plan is yielded and the sort stays.
//	Exactly one of those 69 is #17 (`prov=[ID#1] req=[ID#1]`), and after the
//	pull-up the two sides are IDENTICAL renderings -- satisfaction is not the
//	blocker.
//
// The blocker is pinOrderedSpine's DELEGATOR arm. A projection is an
// orderingDelegator, so the pin resolves through its child group and asks the
// CHILD to satisfy the PARENT's request -- a request stated in the projection's
// output layout, handed to a scan that can only answer in its own. The pin fails,
// the yield is skipped, and the projection's correctly-pulled-up claim never gets
// to matter.
//
// Fixing that means pushing the request DOWN through the translator before asking
// the child (RichOrdering.PushDownThroughValue is the machinery) -- the
// translation at the quantifier boundary, whose own precondition is that the
// translator STATES the domain it baked against. That is a separate, reviewed
// piece of work, so this test exists to make its arrival visible instead of
// leaving the refutation in prose: both halves are asserted, and the pin half
// fails the day the translation lands, with instructions to re-bless #17.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestProjectionOrderingSatisfiesButCannotBePinned reproduces the #17 shape at
// the two decisions that disagree about it.
func TestProjectionOrderingSatisfiesButCannotBePinned(t *testing.T) {
	t.Parallel()

	// The scan under the sub-select: ordered by its primary key ID, which is
	// ordinal 0 of the row it flows.
	baseLayout := values.NewRecordType("SCORES", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SCORE", FieldType: values.NullableLong, Ordinal: 1},
	})
	baseDomain := values.OrdinalDomainOfType(baseLayout)
	if !baseDomain.IsKnown() {
		t.Fatalf("test setup: the base layout has no token")
	}
	scan := plans.NewRecordQueryScanPlan([]string{"SCORES"}, baseLayout, false).
		WithPrimaryKey([]values.Value{
			&values.FieldValue{Field: "ID", Typ: values.NotNullLong},
		})

	// The sub-select `SELECT score, id`: output layout (SCORE, ID), so the
	// ordered column moves from ordinal 0 to ordinal 1.
	baseField := func(name string, ordinal int) values.Value {
		return values.NewFieldValueWithResolvedOrdinalInDomain(
			name, ordinal, values.NullableLong, baseDomain)
	}
	proj := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{baseField("SCORE", 1), baseField("ID", 0)},
		[]string{"SCORE", "ID"},
		expressions.ForEachQuantifier(expressions.FinalOf(scan)))

	provided := computeWrapperRichOrdering(proj)
	if provided == nil || len(provided.GetKeys()) != 1 {
		t.Fatalf("projection provided ordering = %v keys, want 1 pulled up from "+
			"the scan", provided)
	}
	// The request as the translator hands it over: ordinal 1 of the SUB-SELECT's
	// output layout, which is what `ORDER BY 2` over that derived table means.
	requestedValue := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 1, values.UnknownType,
		values.OrdinalDomainOfColumnNames([]string{"SCORE", "ID"}))
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedValue,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)

	// Half one: the property agrees, and it agrees on the STRONGEST possible
	// footing -- the pull-up made the two renderings identical, so no bridge is
	// involved.
	if got := values.ExplainValue(provided.GetKeys()[0]); got != values.ExplainValue(requestedValue) {
		t.Fatalf("the pulled-up provided key renders %q while the request renders "+
			"%q. This test's premise is that satisfaction is NOT the blocker; if "+
			"they no longer coincide, the pull-up changed and the premise needs "+
			"re-measuring.", got, values.ExplainValue(requestedValue))
	}
	if !provided.Satisfies(requested) {
		t.Fatalf("the ordering property does not satisfy a request identical to "+
			"the projection's own pulled-up key (%q). That is the pull-up itself "+
			"regressing, not the pin gate.", values.ExplainValue(requestedValue))
	}

	// Half two: the pin refuses, so ImplementSortRule yields nothing and the
	// sort stays. This is the assertion that fails when the translation lands.
	if pinned := pinOrderedSpine(proj, requested, nil); pinned != nil {
		t.Fatalf("pinOrderedSpine now succeeds on a projection whose child cannot " +
			"answer the request in its own layout.\n\n" +
			"Good news -- it means the request is being TRANSLATED through the " +
			"projection before the child is asked, which is what unblocks the 69 " +
			"corpus elisions this gate refuses. Verify that is the mechanism (and " +
			"not that the delegator arm was simply dropped, which would let " +
			"extraction relink the child group to an unordered sibling after the " +
			"sort is gone), then re-bless the plan-shape golden: " +
			"aggregate_order_by_java#17 should lose its InMemorySort, and this " +
			"test should be replaced by one asserting the translation.")
	}
}
