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

// TestRebaseRefusesRatherThanPushingAnUnrebasedRequest pins the error path, which
// used to be the one answer this function exists to prevent.
//
// It returned the request UNREBASED when the inner quantifier could not state a
// flowed object value, reasoning that a quantifier with no exact row phase has no
// alias to rebase away from. The parts are still rooted at the parent's alias for
// the child, so that published exactly the wrong-space constraint — on the path
// where the shape is already broken. Every way the derivation fails is a
// structural defect of the Reference (no Reference, no members, a nil member, a
// member with no result Value, a member result missing its relation wrapper), so
// the error is the answer and all four call sites turn it into call.Fail.
//
// The failure this guards is silent in the same way the exhaustive drop was: a
// parent-space part cannot be expressed over the child, so push-down declines and
// answers Preserve, and Preserve is satisfied by EVERY access path.
func TestRebaseRefusesRatherThanPushingAnUnrebasedRequest(t *testing.T) {
	t.Parallel()

	rowType := descendingInUnionRowType("TBL")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(
		descendingInUnionFullScan(t, []string{"TBL"}, rowType)))
	key := descendingInUnionField(t, descendingInUnionFlowedObject(t, scanQ), 0)
	request := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{Value: key, SortOrder: properties.RequestedSortOrderAscending}},
		properties.DistinctnessPreserveDistinctness, true)

	// A quantifier over no Reference is the cheapest structural defect to build,
	// and it is the first thing GetFlowedObjectType rejects.
	broken := expressions.ForEachQuantifier(nil)
	if _, err := broken.RequireFlowedObjectValue(); err == nil {
		t.Fatal("a quantifier over no Reference stated a flowed object value, so this " +
			"test no longer reaches the branch it was written for")
	}

	rebased, err := requestedOrderingAtInnerCurrent(request, broken)
	if err == nil {
		t.Fatalf("rebase answered %v instead of refusing; a request whose parts could "+
			"not be moved into the child's space must not be pushed to that child",
			rebased)
	}
	if rebased != nil {
		t.Errorf("rebase returned both an error and a request (%v); a caller that "+
			"checks the request first would push the unrebased parts", rebased)
	}
}
