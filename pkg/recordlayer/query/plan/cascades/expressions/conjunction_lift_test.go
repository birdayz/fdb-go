package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestConstructors_LiftTopLevelConjunctions pins the invariant Java's
// SelectExpression constructor establishes (partitionPredicates →
// flattenPredicate(AndPredicate.class)) on both Go constructors: the
// predicate list IS the conjunction. Every top-level AND, however nested, is
// lifted; an AND under an OR is that OR's child and is left alone; a
// typed-nil AND is kept for the consumers that fail closed on it; the
// lift is idempotent; and a list that needs no lifting keeps its identity.
func TestConstructors_LiftTopLevelConjunctions(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	a := predicates.NewConstantPredicate(predicates.TriTrue)
	b := predicates.NewConstantPredicate(predicates.TriFalse)
	c := predicates.NewConstantPredicate(predicates.TriUnknown)
	d := predicates.NewConstantPredicate(predicates.TriTrue)
	orWithAnd := predicates.NewOr(predicates.NewAnd(a, b), c)
	var typedNil *predicates.AndPredicate

	for _, tc := range []struct {
		name string
		in   []predicates.QueryPredicate
		want []predicates.QueryPredicate
	}{
		{"nested ANDs lift recursively", []predicates.QueryPredicate{predicates.NewAnd(a, predicates.NewAnd(b, c)), d}, []predicates.QueryPredicate{a, b, c, d}},
		{"an AND under an OR is left alone", []predicates.QueryPredicate{orWithAnd, d}, []predicates.QueryPredicate{orWithAnd, d}},
		{"a typed-nil AND is kept", []predicates.QueryPredicate{a, typedNil}, []predicates.QueryPredicate{a, typedNil}},
		{"an empty AND lifts to nothing", []predicates.QueryPredicate{predicates.NewAnd(), a}, []predicates.QueryPredicate{a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := ForEachQuantifier(InitialOf(leaf))
			f := mustExpression(NewLogicalFilterExpression(tc.in, q))
			assertSamePredicates(t, "filter", f.GetPredicates(), tc.want)
			s := mustExpression(NewSelectExpression(values.NewBooleanValue(true), []Quantifier{q}, tc.in))
			assertSamePredicates(t, "select", s.GetPredicates(), tc.want)

			// Idempotent: rebuilding from the lifted list changes nothing.
			again := mustExpression(NewLogicalFilterExpression(f.GetPredicates(), q))
			assertSamePredicates(t, "filter rebuilt", again.GetPredicates(), tc.want)
		})
	}

	flat := []predicates.QueryPredicate{a, b}
	if got := predicates.FlattenConjunction(flat); &got[0] != &flat[0] {
		t.Fatal("a list with nothing to lift must be returned as-is, not copied")
	}
}

// TestConstructors_LiftedConjunctionSharesMemoIdentity pins that memo identity
// follows the lifted list: a filter (or select) built from [And(a, b)] and
// one built from [a, b] are the same member — equal without children and
// hashing alike — so a rule that yields either spelling lands in the same
// place and neither can be explored twice.
func TestConstructors_LiftedConjunctionSharesMemoIdentity(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	a := predicates.NewConstantPredicate(predicates.TriTrue)
	b := predicates.NewConstantPredicate(predicates.TriFalse)

	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	fromAnd := mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{predicates.NewAnd(a, b)}, q1))
	fromList := mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{a, b}, q2))
	if !fromAnd.EqualsWithoutChildren(fromList, EmptyAliasMap()) {
		t.Fatal("filter from [And(a, b)] and filter from [a, b] must be equal without children")
	}
	if fromAnd.HashCodeWithoutChildren() != fromList.HashCodeWithoutChildren() {
		t.Fatal("filter from [And(a, b)] and filter from [a, b] must hash alike")
	}

	rv := values.NewBooleanValue(true)
	selFromAnd := mustExpression(NewSelectExpression(rv, []Quantifier{q1}, []predicates.QueryPredicate{predicates.NewAnd(a, b)}))
	selFromList := mustExpression(NewSelectExpression(rv, []Quantifier{q1}, []predicates.QueryPredicate{a, b}))
	if !selFromAnd.EqualsWithoutChildren(selFromList, EmptyAliasMap()) {
		t.Fatal("select from [And(a, b)] and select from [a, b] must be equal without children")
	}
	if selFromAnd.HashCodeWithoutChildren() != selFromList.HashCodeWithoutChildren() {
		t.Fatal("select from [And(a, b)] and select from [a, b] must hash alike")
	}
}

func TestSelectPartitionsPredicatesByValue(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("row")
	value := mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullLong))
	sameValue := mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullLong))
	other := mustExpression(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("other"), values.NotNullLong))
	comparison := func(v values.Value, kind predicates.ComparisonType, literal int64) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(v, predicates.NewLiteralComparison(kind, literal))
	}
	lower := comparison(value, predicates.ComparisonGreaterThan, 1)
	upper := comparison(sameValue, predicates.ComparisonLessThan, 4)
	residual := comparison(value, predicates.ComparisonNotEquals, 2)
	atomic := predicates.WithAtomicity(predicates.NewAnd(lower, upper), true)
	or := predicates.NewOr(lower, residual)
	selectFor := func(ps ...predicates.QueryPredicate) *SelectExpression {
		return mustExpression(NewSelectExpression(value, nil, ps))
	}
	got := selectFor(lower, or, upper, residual, comparison(other, predicates.ComparisonEquals, 3), atomic)
	ps := got.GetPredicates()
	if len(ps) != 5 || ps[0] != or || ps[1] != atomic || ps[2] != residual {
		t.Fatalf("want opaque predicates first, then per-value residuals and ranges; got %v", ps)
	}
	rangePredicate, ok := ps[3].(*predicates.PredicateWithValueAndRanges)
	if !ok || len(rangePredicate.GetRanges()) != 1 || len(rangePredicate.GetComparisons()) != 2 {
		t.Fatalf("equal value handles must coalesce into one two-bound range: %T %v", ps[3], ps[3])
	}
	otherRange, ok := ps[4].(*predicates.PredicateWithValueAndRanges)
	if !ok || !values.SemanticEqualsUnderAliasMap(otherRange.GetValue(), other, nil) {
		t.Fatal("same-shaped values under different free aliases must not coalesce")
	}
	left := selectFor(lower, upper)
	right := selectFor(upper, lower, lower)
	if !MemoEqual(left, right) || left.HashCodeWithoutChildren() != right.HashCodeWithoutChildren() {
		t.Fatal("comparison permutation and repetition on one value must have one memo identity")
	}
	rebuilt := selectFor(ps...)
	if !MemoEqual(got, rebuilt) {
		t.Fatal("partitioning existing ranges must be idempotent")
	}
	contradictory := selectFor(lower, comparison(value, predicates.ComparisonLessThan, 0))
	pvr, ok := contradictory.GetPredicates()[0].(*predicates.PredicateWithValueAndRanges)
	if !ok || len(pvr.GetComparisons()) != 2 {
		t.Fatal("coalescing must retain both contradictory bounds, never weaken the conjunction")
	}
}

func assertSamePredicates(t *testing.T, what string, got, want []predicates.QueryPredicate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d predicates %v, want %d %v", what, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %v, want %v", what, i, got[i], want[i])
		}
	}
}
