package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestOrderingRequestSurvivesSortThroughSelectToScan drives the WHOLE push chain
// — sort publishes, select forwards, scan receives — in one test, because the
// defect it pins lived in the JOIN between two rules that each looked correct in
// isolation. The sort published its request rooted at its own alias for the
// child; the select could only read one rooted at the reserved-current handle.
// Neither rule's own test could see the mismatch: each built its input in the
// convention its own code expected.
//
// What the mismatch cost was not a wrong ordering but a SILENTLY DROPPED one.
// The select declined every part it could not express, returned Preserve, and a
// Preserve request is satisfied by any access path at all — so every index in
// the schema became a viable zero-prefix full scan, and the memo enumerated
// three access paths where one was useful. On the stress suite's
// `WHERE cat IN (...) ORDER BY id` that tripled the planner's task count.
//
// The shape is the IN-list one: a SELECT over an explode leg plus the table leg
// whose result IS the table leg's row.
func TestOrderingRequestSurvivesSortThroughSelectToScan(t *testing.T) {
	t.Parallel()

	explodeQ := expressions.ForEachQuantifier(
		expressions.InitialOf(descendingInUnionExplode(t)))
	rowType := descendingInUnionRowType("TBL")
	scanRef := expressions.InitialOf(
		descendingInUnionFullScan(t, []string{"TBL"}, rowType))
	scanQ := expressions.ForEachQuantifier(scanRef)
	scanRow := descendingInUnionFlowedObject(t, scanQ)

	sel := descendingInUnionSelect(
		t, scanRow, []expressions.Quantifier{explodeQ, scanQ})
	selRef := expressions.InitialOf(sel)
	selQ := expressions.ForEachQuantifier(selRef)

	// ORDER BY <first column>, stated the way the SQL translator states it under
	// RFC-232: rooted at the sort's own quantifier over the select.
	sortKey := descendingInUnionField(
		t, descendingInUnionFlowedObject(t, selQ), 0)
	sortExprValue, sortErr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: sortKey, Reverse: false}}, selQ)
	sortExpr := mustConstruct(t, sortExprValue, sortErr)
	sortRef := expressions.InitialOf(sortExpr)

	constraints := NewConstraintMap()
	fireDescendingConstraintOnlyRule(
		t, NewPushRequestedOrderingThroughSortRule(), sortExpr, sortRef, constraints)

	atSelect := requireDescendingPushedOrdering(t, constraints, selRef)
	requireCurrentSpaceOrdering(t, "the SELECT", atSelect)

	fireDescendingConstraintOnlyRule(
		t, NewPushRequestedOrderingThroughSelectRule(), sel, selRef, constraints)

	atScan := requireDescendingPushedOrdering(t, constraints, scanRef)
	requireCurrentSpaceOrdering(t, "the SCAN", atScan)
}

// requireCurrentSpaceOrdering asserts the invariant every consumer of a pushed
// requested ordering relies on: it is CONCRETE (a Preserve here is the failure
// mode, not a weaker success) and every part names the receiving reference's own
// reserved-current row. Java states the same invariant by rebasing onto
// Quantifier.current() and keeping only the parts that land there
// (RequestedOrdering.pushDown).
func requireCurrentSpaceOrdering(
	t testing.TB,
	where string,
	ordering *properties.RequestedOrdering,
) {
	t.Helper()
	if ordering.IsPreserve() {
		t.Fatalf("%s was asked to preserve order, not to produce the requested one — "+
			"a Preserve request is satisfied by EVERY access path, so this is how an "+
			"unrelated index becomes a viable full scan", where)
	}
	parts := ordering.GetParts()
	if len(parts) != 1 {
		t.Fatalf("%s received %d ordering parts, want one", where, len(parts))
	}
	for i, part := range parts {
		correlations := values.GetCorrelatedToOfValue(part.Value)
		if len(correlations) == 0 {
			t.Fatalf("%s part %d (%s) names no row at all", where, i,
				values.ExplainValue(part.Value))
		}
		for correlation := range correlations {
			if correlation != values.CurrentCorrelation() {
				t.Fatalf("%s part %d (%s) is rooted at %s, want the reserved-current "+
					"handle — a reference has never heard of a parent's alias for it, "+
					"so a foreign root makes every downstream push-down decline",
					where, i, values.ExplainValue(part.Value), correlation)
			}
		}
	}
}

// TestRebasingAnOrderingKeepsItsExhaustiveFlag pins the half of the rebase that is
// NOT about correlation space.
//
// requestedOrderingAtInnerCurrent moves an ordering's parts between correlation
// spaces and decides nothing about what the request MEANS, so distinctness and the
// exhaustive flag must both cross unchanged. It hard-coded exhaustive=false, which
// is invisible from the sort rules — their own requests are never exhaustive — but
// the select push-down feeds its result through here and deliberately preserves
// the flag (it reads pushed.IsExhaustive()), and a union pushes EXHAUSTIVE requests
// into its first branch. So `union → select` lost the flag on the way down.
//
// Nothing comes out wrongly ordered: exhaustive drives only subsumption and
// constraint equality. What it costs is enumeration — the ordered-leg alternatives
// simply stop being generated — which is why no correctness test could see it.
func TestRebasingAnOrderingKeepsItsExhaustiveFlag(t *testing.T) {
	t.Parallel()

	rowType := descendingInUnionRowType("TBL")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(
		descendingInUnionFullScan(t, []string{"TBL"}, rowType)))
	key := descendingInUnionField(t, descendingInUnionFlowedObject(t, scanQ), 0)

	for _, exhaustive := range []bool{false, true} {
		request := properties.NewRequestedOrdering(
			[]properties.RequestedOrderingPart{{Value: key, SortOrder: properties.RequestedSortOrderAscending}},
			properties.DistinctnessPreserveDistinctness, exhaustive)
		if request.IsExhaustive() != exhaustive {
			t.Fatalf("the fixture request does not carry exhaustive=%v", exhaustive)
		}

		rebased, err := requestedOrderingAtInnerCurrent(request, scanQ)
		if err != nil {
			t.Fatalf("rebase (exhaustive=%v): %v", exhaustive, err)
		}
		if rebased.IsExhaustive() != exhaustive {
			t.Errorf("rebasing an exhaustive=%v request produced exhaustive=%v — the "+
				"rebase changes correlation space, not what the request means; dropping "+
				"the flag narrows enumeration silently under union -> select",
				exhaustive, rebased.IsExhaustive())
		}
		if rebased.GetDistinctness() != request.GetDistinctness() {
			t.Errorf("rebasing changed distinctness from %v to %v",
				request.GetDistinctness(), rebased.GetDistinctness())
		}
		// And it really did rebase: the pin above must not pass by returning the
		// input untouched.
		requireCurrentSpaceOrdering(t, "the rebased request", rebased)
	}
}
