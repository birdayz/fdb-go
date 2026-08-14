package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestParentConstructionEnumeratesEveryPhysicalChildMember pins the ENUMERATION
// arm directly, on a child group built to have N physical members.
//
// This is deliberately NOT an end-to-end plan-shape assertion. The defect it
// guards survived a full corpus because every existing test observed the
// CHOSEN plan, and a parent alternative that is never CONSTRUCTED is invisible
// to that: it is not a losing plan, it is a plan the memo never saw. Only a
// test that counts what the rule yields can tell "the alternative lost" from
// "the alternative was never built".
//
// Java's guarantee is that a rule binds every member of a child reference, so
// an implementation rule fires once per (parent, child-member) pair. Go used to
// build one parent per requested ORDERING — with a single preserve ordering,
// exactly one — so a child member that was not the local cost winner never
// became a parent alternative at all. That is how a sound transformation
// (MergeFetchIntoCoveringIndexRule) adding a cheaper member could DELETE a
// better plan from consideration.
func TestParentConstructionEnumeratesEveryPhysicalChildMember(t *testing.T) {
	t.Parallel()

	mkIndex := func(name string) *plans.RecordQueryIndexPlan {
		index, err := plans.NewRecordQueryIndexPlan(
			name, []*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
			[]string{"T"}, values.NewRecordType("T", false, []values.Field{
				{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
				{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
			}), false,
		)
		return mustConstruct(t, index, err).
			WithIndexMetadata([]string{"A"}, []string{"ID"}, false)
	}

	// A child group with THREE distinct physical members. They are distinct
	// plans, not copies: a group that deduped them would make the count below
	// pass for the wrong reason.
	a := mkIndex("IDX_A")
	bValue, bErr := plans.NewRecordQueryCoveringIndexPlan(mkIndex("IDX_B"))
	b := mustConstruct(t, bValue, bErr)
	c := mkIndex("IDX_C")

	childRef := expressions.InitialOf(a)
	childRef.InsertFinal(a)
	childRef.InsertFinal(b)
	childRef.InsertFinal(c)

	got := physicalMembersForParentEnumeration(childRef)
	if len(got) != 3 {
		var names []string
		for _, m := range got {
			names = append(names, fmt.Sprintf("%T", m))
		}
		t.Fatalf("physicalMembersForParentEnumeration returned %d members, want 3.\n"+
			"got: %v\n"+
			"A parent must be constructed over EVERY physical child member. "+
			"Returning fewer means some alternative is never built, which no "+
			"plan-shape test can detect — it does not lose, it never exists.",
			len(got), names)
	}

	// Identity, not just arity: the three returned members must be the three
	// inserted ones. An arity-only check passes if the function returns the same
	// member three times.
	seen := map[expressions.RelationalExpression]bool{}
	for _, m := range got {
		seen[m] = true
	}
	for _, want := range []expressions.RelationalExpression{a, b, c} {
		if !seen[want] {
			t.Fatalf("member %T is missing from the enumeration; the arity matched "+
				"but the SET did not", want)
		}
	}

	// The empty case must be empty, not a nil-bearing single entry: callers
	// range over the result and would otherwise build a parent over nil.
	filterValue, filterErr := expressions.NewLogicalFilterExpression(
		nil, expressions.ForEachQuantifier(expressions.InitialOf(a)))
	filter := mustConstruct(t, filterValue, filterErr)
	if got := physicalMembersForParentEnumeration(expressions.InitialOf(filter)); len(got) != 0 {
		t.Fatalf("a group with no physical members enumerated %d entries, want 0", len(got))
	}

	// A nil reference is a valid input at the call sites (a quantifier may range
	// over nothing mid-planning) and must not panic.
	if got := physicalMembersForParentEnumeration(nil); got != nil {
		t.Fatalf("nil reference enumerated %v, want nil", got)
	}
}
