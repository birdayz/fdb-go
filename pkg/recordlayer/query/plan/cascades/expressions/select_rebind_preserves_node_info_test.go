package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// WithQuantifiers rewires child EDGES. Everything else on the node is
// node-information and must survive the rewire, because the memo drives this
// method on nodes it did not construct and has no way to restore what the copy
// drops.
//
// The swap marker is the case that was actually broken, and it is the one that
// matters most: BOTH of its readers are safety DECLINES. RemoveRangeOneRule
// refuses a swapped Select outright ("reconstructing it would have to restore
// the swap + its SQL column ordering, which the removal path does not model"),
// and ImplementNestedLoopJoinRule gates its correlated-scan FlatMap fast path on
// `!sel.IsQuantifiersSwapped()`. A copy that reports UNSWAPPED therefore does
// not lose an optimization — it admits a shape two rules explicitly refuse to
// handle, and the observable is a wrong SQL column ORDER, not a slower plan.
//
// Reachability, measured rather than assumed, so the pin's status is honest:
// a logging probe in this method over an uncached
// `go test ./pkg/relational/... ./pkg/recordlayer/query/...` (every package
// green, including the containerised sqldriver suite and all four conformance
// corpora) recorded ZERO calls. The only interface caller that could route a
// Select here is rule_decorrelate_values.go's pushValuesDefault, and the Select
// case is intercepted one switch arm above it. So the defect is LATENT, and
// this test is a function-boundary pin on the shape rather than a reproducer of
// shipped rows. What re-arms it is any new caller of WithQuantifiers reached by
// a SelectExpression — which is exactly what a memo-driven rewrite adds without
// anyone reading this file.
func TestSelectWithQuantifiers_PreservesEveryNonQuantifierField(t *testing.T) {
	t.Parallel()

	leaf := &leafScan{name: "T"}
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	rv := values.NewBooleanValue(true)
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)

	base := NewSelectExpressionWithJoinType(
		rv,
		[]Quantifier{q1, q2},
		[]predicates.QueryPredicate{pTrue},
		[]string{"A", "B"},
		JoinCross,
	)
	swapped := base.WithSwappedQuantifiers()
	if !swapped.IsQuantifiersSwapped() {
		t.Fatal("WithSwappedQuantifiers did not set the marker — the fixture is broken, not the subject")
	}

	// The rewire the memo performs: same edges, freshly bound quantifier objects.
	rebound := swapped.WithQuantifiers([]Quantifier{
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
	})
	got, ok := rebound.(*SelectExpression)
	if !ok {
		t.Fatalf("WithQuantifiers returned %T, want *SelectExpression", rebound)
	}

	if !got.IsQuantifiersSwapped() {
		t.Error("WithQuantifiers dropped the quantifier-swap marker. " +
			"A rebound swapped Select now reports itself UNSWAPPED, so " +
			"RemoveRangeOneRule will strip a RANGE leg it declines to model the " +
			"swap for, and ImplementNestedLoopJoinRule will take the correlated-scan " +
			"fast path its `!IsQuantifiersSwapped()` guard exists to refuse. " +
			"Both failures are silent and both come out as a wrong SQL column order. " +
			"Copy the struct; do not re-list the fields.")
	}
	if got.GetJoinType() != JoinCross {
		t.Errorf("WithQuantifiers dropped the join type: got %v, want JoinCross", got.GetJoinType())
	}
	if aliases := got.GetSourceAliases(); len(aliases) != 2 || aliases[0] != "B" || aliases[1] != "A" {
		t.Errorf("WithQuantifiers dropped the (swapped) source aliases: got %v, want [B A]", aliases)
	}
	if got.GetResultValue() != rv {
		t.Error("WithQuantifiers dropped the result value")
	}
	if len(got.GetPredicates()) != 1 || got.GetPredicates()[0] != pTrue {
		t.Errorf("WithQuantifiers dropped the predicate list: got %v", got.GetPredicates())
	}

	// The edges really were rewired — without this the assertions above would
	// pass just as happily on a method that ignored its argument.
	if got.GetQuantifiers()[0].GetAlias() == swapped.GetQuantifiers()[0].GetAlias() {
		t.Error("WithQuantifiers did not install the replacement quantifiers")
	}
}

// An unswapped Select must stay unswapped. Without this half, a copy that
// hard-coded the marker to true would satisfy the test above — an invariant
// that only holds in one direction is not an invariant.
func TestSelectWithQuantifiers_DoesNotInventASwap(t *testing.T) {
	t.Parallel()

	leaf := &leafScan{name: "T"}
	base := NewSelectExpressionWithAliases(
		values.NewBooleanValue(true),
		[]Quantifier{ForEachQuantifier(InitialOf(leaf)), ForEachQuantifier(InitialOf(leaf))},
		nil,
		[]string{"A", "B"},
	)
	if base.IsQuantifiersSwapped() {
		t.Fatal("a freshly built Select must not be marked swapped")
	}
	rebound := base.WithQuantifiers([]Quantifier{
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
	}).(*SelectExpression)
	if rebound.IsQuantifiersSwapped() {
		t.Error("WithQuantifiers invented a swap marker on an unswapped Select — " +
			"the two decline gates would now refuse shapes they should handle")
	}
}
