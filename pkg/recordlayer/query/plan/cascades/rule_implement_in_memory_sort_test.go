package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// sortAltOverPlanYielded reports whether any yielded in-memory sort carries the
// given plan as a FROZEN single-member edge — the alternative arm's signature.
//
// The primary arm ranges over the shared inner GROUP (many members) so that
// cost and extraction resolve the same winner; the alternative arm snapshots
// one specific selective member. Discriminating on the edge, not on the
// resolved plan, is what makes "the alternative was yielded" checkable: the
// group's winner may happen to be the same plan, and then a resolved-plan check
// would report the alternative present when only the primary was built.
func sortAltOverPlanYielded(yielded []expressions.RelationalExpression, want plans.RecordQueryPlan) bool {
	for _, y := range yielded {
		sortPlan, ok := y.(*plans.RecordQueryInMemorySortPlan)
		if !ok {
			continue
		}
		qs := sortPlan.GetQuantifiers()
		if len(qs) != 1 {
			continue
		}
		members := qs[0].GetRangesOver().AllMembers()
		if len(members) != 1 {
			continue
		}
		ph, isPhys := members[0].(physicalPlanExpression)
		if isPhys && ph.GetRecordQueryPlan() == want {
			return true
		}
	}
	return false
}

// sortOverScanAndFetch builds ORDER BY amount over a group holding a full table
// scan FIRST and the given fetch second, and fires the in-memory-sort rule.
//
// The scan-first ordering is load-bearing: the rule's primary arm already
// covers the group's first physical member, so the alternative arm deliberately
// skips it. A single-member group would therefore prove nothing about the
// alternative.
func sortOverScanAndFetch(t *testing.T, fetch *plans.RecordQueryFetchFromPartialRecordPlan) []expressions.RelationalExpression {
	t.Helper()

	scan := plans.NewRecordQueryScanPlan([]string{"Orders"}, values.UnknownType, false)
	innerRef := expressions.InitialOf(scan)
	innerRef.Insert(fetch)

	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "amount", Typ: values.UnknownType}}},
		expressions.ForEachQuantifier(innerRef),
	)
	return FireImplementationRule(NewImplementInMemorySortRule(), expressions.InitialOf(sortExpr))
}

// TestImplementInMemorySort_RestrictedCoveringFetchAlternativeIsYielded pins
// that the "sort a SARG'd Fetch's small output" alternative is built for a
// restricted fetch and not built for an unrestricted one.
//
// Sorting the output of a selective index lookup can be dramatically cheaper
// than sorting a full scan, which is the entire reason the alternative exists.
// Telling the two apart needs the scan's comparison ranges, and those sit under
// the covering wrapper the access path now always builds — a walker that stops
// at the wrapper reads EVERY fetch as unrestricted and the alternative stops
// being constructed for every query.
//
// The failure is invisible to any plan-shape assertion: an alternative that is
// never constructed is not a losing plan. It appears in no cost comparison and
// no plan dump, so every golden stays byte-identical while the choice set
// shrank. Hence this asserts the YIELD, not the final plan.
func TestImplementInMemorySort_RestrictedCoveringFetchAlternativeIsYielded(t *testing.T) {
	t.Parallel()

	t.Run("restricted_fetch_yields_the_alternative", func(t *testing.T) {
		t.Parallel()

		fetch := coveringInnerFetch("idx_orders_cid", []*predicates.ComparisonRange{coveringInnerGT(t, int64(5))})
		yielded := sortOverScanAndFetch(t, fetch)
		if len(yielded) == 0 {
			t.Fatal("the in-memory sort rule should fire over a group with a physical member")
		}
		if !sortAltOverPlanYielded(yielded, fetch) {
			t.Fatalf("the sort-the-SARG'd-fetch alternative was NOT yielded (%d alternatives) — "+
				"the restriction test cannot see past the covering wrapper, so every fetch reads as "+
				"unrestricted and only the sort-the-full-scan plan is ever built", len(yielded))
		}
	})

	t.Run("unrestricted_fetch_does_not_yield_the_alternative", func(t *testing.T) {
		t.Parallel()

		fetch := coveringInnerFetch("idx_orders_cid", nil)
		yielded := sortOverScanAndFetch(t, fetch)
		if len(yielded) == 0 {
			t.Fatal("the in-memory sort rule should still yield its primary arm")
		}
		if sortAltOverPlanYielded(yielded, fetch) {
			t.Fatal("an alternative was yielded for an UNRESTRICTED fetch: its output is the whole table, " +
				"so there is no smaller result to sort and the arm must not fire")
		}
	})
}

// TestIsRestrictedFetch_SeesPastTheCoveringWrapper drives the decision directly,
// independently of everything else the rule requires of a member.
func TestIsRestrictedFetch_SeesPastTheCoveringWrapper(t *testing.T) {
	t.Parallel()

	sargd := coveringInnerFetch("idx_a", []*predicates.ComparisonRange{coveringInnerGT(t, int64(5))})
	if !isRestrictedFetch(sargd) {
		t.Fatal("a fetch over a SARG'd covering scan reads as unrestricted: the scan ranges live under the covering wrapper")
	}
	if isRestrictedFetch(coveringInnerFetch("idx_a", nil)) {
		t.Fatal("a fetch over an unrestricted covering scan must not read as restricted")
	}
}
