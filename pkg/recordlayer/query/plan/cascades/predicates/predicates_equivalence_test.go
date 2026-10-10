// Portions derived from FoundationDB Record Layer (QueryPredicateTest.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package predicates

// Java's QueryPredicateTest exercises AND/OR identity as sets of children.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPredicateEquals_ExistentialValuePredicate pins RFC-189 C4 (finding 12d):
// PredicateEquals had no *ExistentialValuePredicate case, so two EXISTS always
// fell through to false and `EXISTS(q) AND EXISTS(q)` never collapsed via
// AndDedup (an optimization inconsistency — memo interning handles correctness).
// Two EXISTS over the same quantifier are now equal; over different quantifiers
// they stay distinct.
func TestPredicateEquals_ExistentialValuePredicate(t *testing.T) {
	t.Parallel()
	mkExists := func(a values.CorrelationIdentifier) *ExistentialValuePredicate {
		return &ExistentialValuePredicate{
			Value:      mustQOV(t, a),
			Comparison: Comparison{Type: ComparisonIsNotNull},
		}
	}
	q := values.NamedCorrelationIdentifier("q")
	r := values.NamedCorrelationIdentifier("r")

	if !PredicateEquals(mkExists(q), mkExists(q)) {
		t.Fatal("EXISTS over the same quantifier must be equal (was always false pre-fix)")
	}
	if PredicateEquals(mkExists(q), mkExists(r)) {
		t.Fatal("EXISTS over different quantifiers must NOT be equal")
	}
	// Not equal to an unrelated predicate type.
	if PredicateEquals(mkExists(q), mkValuePred(t, "a", "x")) {
		t.Fatal("EXISTS must not equal a ValuePredicate")
	}
}

func TestPredicateEquals_OrSet(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred(t, "a", "Hello")
	p2 := mkValuePred(t, "b", "World")
	p3 := mkValuePred(t, "c", "Castro")

	or123 := &OrPredicate{SubPredicates: []QueryPredicate{p1, p2, p3}}
	or321 := &OrPredicate{SubPredicates: []QueryPredicate{p3, p2, p1}}

	if !PredicateEquals(or123, or321) {
		t.Fatal("OR child order must not affect equality")
	}
	// Same order — equal.
	or123Same := &OrPredicate{SubPredicates: []QueryPredicate{p1, p2, p3}}
	if !PredicateEquals(or123, or123Same) {
		t.Fatal("same-order Or should be equal")
	}
}

func TestPredicateEquals_AndSet(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred(t, "a", "Hello")
	p2 := mkValuePred(t, "b", "World")
	p3 := mkValuePred(t, "c", "Castro")

	and123 := &AndPredicate{SubPredicates: []QueryPredicate{p1, p2, p3}}
	and321 := &AndPredicate{SubPredicates: []QueryPredicate{p3, p2, p1}}
	if !PredicateEquals(and123, and321) {
		t.Fatal("AND child order must not affect equality")
	}
}

func TestPredicateEquals_NestedAndOrSet(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred(t, "a", "Hello")
	p2 := mkValuePred(t, "b", "World")
	p3 := mkValuePred(t, "c", "Castro")

	left := &AndPredicate{SubPredicates: []QueryPredicate{
		p1,
		&OrPredicate{SubPredicates: []QueryPredicate{p2, p3}},
	}}
	right := &AndPredicate{SubPredicates: []QueryPredicate{
		&OrPredicate{SubPredicates: []QueryPredicate{p3, p2}},
		p1,
	}}
	if !PredicateEquals(left, right) {
		t.Fatal("nested AND/OR permutations must compare equal")
	}
}

func TestPredicateEquals_DuplicateChildren(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred(t, "a", "Hello")
	p2 := mkValuePred(t, "b", "World")
	withDup := &AndPredicate{SubPredicates: []QueryPredicate{p1, p1, p2}}
	noDup := &AndPredicate{SubPredicates: []QueryPredicate{p1, p2}}
	if !PredicateEquals(withDup, noDup) {
		t.Fatal("duplicate children must not affect set equality")
	}
}

// TestPredicateEquals_SingletonAndOr pins the boundary: a single-
// child AND or OR with matching child IS equal to itself, but is NOT
// equal to its child (the wrapper changes the structure).
func TestPredicateEquals_SingletonAndOr(t *testing.T) {
	t.Parallel()
	p := mkValuePred(t, "a", "Hello")
	andP := &AndPredicate{SubPredicates: []QueryPredicate{p}}
	orP := &OrPredicate{SubPredicates: []QueryPredicate{p}}
	andP2 := &AndPredicate{SubPredicates: []QueryPredicate{p}}
	if !PredicateEquals(andP, andP2) {
		t.Fatal("identically-shaped singleton AND should be equal")
	}
	if PredicateEquals(andP, orP) {
		t.Fatal("singleton AND and OR with same child should NOT be equal")
	}
	if PredicateEquals(andP, p) {
		t.Fatal("AND wrapper should NOT equal its naked child predicate")
	}
}

// TestPredicateEquals_NotPosition pins that NotPredicate equality
// is just child equality wrapped in NOT — positional doesn't apply
// (single child).
func TestPredicateEquals_NotPosition(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred(t, "a", "Hello")
	p2 := mkValuePred(t, "b", "World")
	notP1a := &NotPredicate{Child: p1}
	notP1b := &NotPredicate{Child: p1}
	notP2 := &NotPredicate{Child: p2}
	if !PredicateEquals(notP1a, notP1b) {
		t.Fatal("NOT(p1) should equal NOT(p1)")
	}
	if PredicateEquals(notP1a, notP2) {
		t.Fatal("NOT(p1) should NOT equal NOT(p2)")
	}
}

// mkValuePred constructs a ComparisonPredicate of the form
// `<field> = <strLit>` for the equivalence tests. Java's
// QueryPredicateTest uses ValuePredicate(FieldValue, SimpleComparison
// EQUALS lit); our shape is ComparisonPredicate(FieldValue,
// Comparison{Equals, lit}) which serves the same role.
func mkValuePred(t testing.TB, field, strLit string) QueryPredicate {
	return NewComparisonPredicate(predicateTestField(t, field, values.TypeString), Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(strLit)})
}
