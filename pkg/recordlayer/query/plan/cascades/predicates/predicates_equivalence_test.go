package predicates

// Tests inspired by Java's QueryPredicateTest.testOrEquivalence /
// testAndEquivalence / testAndOrEquivalence, adapted to pin the
// CURRENT behaviour of our PredicateEquals — positional, not
// multiset.
//
// JAVA-DIVERGENCE: Java's AndPredicate.and / OrPredicate.or treat
// children as a multiset for equality + hash. Our PredicateEquals
// uses positional comparison via predicateListsEqual (line 230 of
// predicates.go). The semantic gap is intentional pinning here so a
// future change to multiset semantics is a deliberate decision
// flagged by failing tests, not a silent regression.
//
// When the Cascades port lands the rules that depend on multiset
// AND/OR equality (e.g. AndAbsorbOrRule, OrAbsorbAndRule today only
// fire on canonically-ordered inputs), this test file is the trigger
// to update PredicateEquals + predicateListsEqual to set semantics.
// Java's PredicateEquals does this; ours should too eventually.

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestPredicateEquals_OrPositional pins the documented positional
// behaviour for OR. `Or(p1, p2, p3)` is NOT equal to `Or(p3, p2, p1)`
// under our PredicateEquals — Java's `OrPredicate.or(p1,p2,p3) ==
// OrPredicate.or(p3,p2,p1)` returns true because Java treats children
// as a multiset.
func TestPredicateEquals_OrPositional(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred("a", "Hello")
	p2 := mkValuePred("b", "World")
	p3 := mkValuePred("c", "Castro")

	or123 := &OrPredicate{SubPredicates: []QueryPredicate{p1, p2, p3}}
	or321 := &OrPredicate{SubPredicates: []QueryPredicate{p3, p2, p1}}

	// JAVA-DIVERGENCE: Java says these are equal.
	if PredicateEquals(or123, or321) {
		t.Fatal("CURRENT behaviour is positional — Or(p1,p2,p3) should NOT equal Or(p3,p2,p1) under our PredicateEquals. " +
			"Java treats them as multiset-equal. If this test starts failing, multiset semantics has been adopted; update Java-divergence comment.")
	}
	// Same order — equal.
	or123Same := &OrPredicate{SubPredicates: []QueryPredicate{p1, p2, p3}}
	if !PredicateEquals(or123, or123Same) {
		t.Fatal("same-order Or should be equal")
	}
}

// TestPredicateEquals_AndPositional pins the same gap for AND.
func TestPredicateEquals_AndPositional(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred("a", "Hello")
	p2 := mkValuePred("b", "World")
	p3 := mkValuePred("c", "Castro")

	and123 := &AndPredicate{SubPredicates: []QueryPredicate{p1, p2, p3}}
	and321 := &AndPredicate{SubPredicates: []QueryPredicate{p3, p2, p1}}
	if PredicateEquals(and123, and321) {
		t.Fatal("CURRENT behaviour is positional — And(p1,p2,p3) should NOT equal And(p3,p2,p1) under our PredicateEquals. " +
			"Java treats them as multiset-equal.")
	}
}

// TestPredicateEquals_NestedAndOrPositional pins the same gap for
// nested AndOr trees. `And(p1, Or(p2,p3))` is NOT equal to
// `And(Or(p3,p2), p1)` under our PredicateEquals. Java's would
// consider these equal because both AND order and OR order are
// multiset-irrelevant.
func TestPredicateEquals_NestedAndOrPositional(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred("a", "Hello")
	p2 := mkValuePred("b", "World")
	p3 := mkValuePred("c", "Castro")

	left := &AndPredicate{SubPredicates: []QueryPredicate{
		p1,
		&OrPredicate{SubPredicates: []QueryPredicate{p2, p3}},
	}}
	right := &AndPredicate{SubPredicates: []QueryPredicate{
		&OrPredicate{SubPredicates: []QueryPredicate{p3, p2}},
		p1,
	}}
	if PredicateEquals(left, right) {
		t.Fatal("CURRENT positional behaviour: nested AndOr trees with reordered children should NOT match. " +
			"Java's testAndOrEquivalence says they should — this is the documented Java-divergence.")
	}
}

// TestPredicateEquals_DuplicateChildren pins another corollary of
// positional comparison: `And(p1, p1, p2)` is NOT equal to
// `And(p2, p1)` under multiset-style dedup, BUT it's also not equal
// to `And(p1, p2)` under positional comparison (different lengths).
// Confirms our equality is strictly positional with NO dedup.
func TestPredicateEquals_DuplicateChildren(t *testing.T) {
	t.Parallel()
	p1 := mkValuePred("a", "Hello")
	p2 := mkValuePred("b", "World")
	withDup := &AndPredicate{SubPredicates: []QueryPredicate{p1, p1, p2}}
	noDup := &AndPredicate{SubPredicates: []QueryPredicate{p1, p2}}
	if PredicateEquals(withDup, noDup) {
		t.Fatal("predicates with different child counts should not be equal regardless of dedup")
	}
}

// TestPredicateEquals_SingletonAndOr pins the boundary: a single-
// child AND or OR with matching child IS equal to itself, but is NOT
// equal to its child (the wrapper changes the structure).
func TestPredicateEquals_SingletonAndOr(t *testing.T) {
	t.Parallel()
	p := mkValuePred("a", "Hello")
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
	p1 := mkValuePred("a", "Hello")
	p2 := mkValuePred("b", "World")
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
func mkValuePred(field, strLit string) QueryPredicate {
	return NewComparisonPredicate(
		&values.FieldValue{Field: field, Typ: values.TypeString},
		Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(strLit)},
	)
}
