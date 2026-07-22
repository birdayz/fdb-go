package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func eqComp(op values.Value) *predicates.Comparison {
	return &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: op}
}

// TestSargComparisonEqual_StructuralIdentity pins RFC-188 finding 3: the SARG
// set compares by BARE Comparison identity (type + comparand + escape + param),
// the analog of Java Comparisons.Comparison.equals — NOT rendered text or an
// alias-blind hash. Distinct constants that render alike (int64(1) vs float64(1))
// and correlated composite-key operands with the same structure but different
// aliases must NOT be treated as equal.
func TestSargComparisonEqual_StructuralIdentity(t *testing.T) {
	t.Parallel()

	if !sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), eqComp(values.LiteralValue(int64(5)))) {
		t.Fatal("same type + comparand must be equal")
	}
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), eqComp(values.LiteralValue(int64(7)))) {
		t.Fatal("different comparand must NOT be equal")
	}
	// int64(1) vs float64(1): render identically ("1") but are different
	// comparands — must not collide.
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(1))), eqComp(values.LiteralValue(float64(1)))) {
		t.Fatal("int64(1) and float64(1) must NOT be equal (structural, not display text)")
	}
	// Different comparison type.
	gt5 := &predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))}
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), gt5) {
		t.Fatal("different comparison type must NOT be equal")
	}
	// Different Escape — a Comparison identity field that a type+comparand key omits.
	esc := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5)), Escape: '\\'}
	if sargComparisonEqual(eqComp(values.LiteralValue(int64(5))), esc) {
		t.Fatal("different Escape must NOT be equal")
	}
	// Correlated operands over DIFFERENT quantifier aliases must NOT be equal
	// (ValuesStructurallyEqual is alias-sensitive).
	a := eqComp(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTERA")))
	b := eqComp(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("OUTERB")))
	if sargComparisonEqual(a, b) {
		t.Fatal("correlated operands over different aliases must NOT be equal (alias-sensitive)")
	}

	// Unary comparisons (IS NULL) ignore the comparand — two IS NULLs are equal
	// regardless of any (semantically-absent) operand.
	null1 := &predicates.Comparison{Type: predicates.ComparisonIsNull, Operand: values.LiteralValue(int64(1))}
	null2 := &predicates.Comparison{Type: predicates.ComparisonIsNull, Operand: values.LiteralValue(int64(99))}
	if !sargComparisonEqual(null1, null2) {
		t.Fatal("unary IS NULL comparisons must be equal (comparand semantically ignored)")
	}

	// A text-search variant identity field (tokenizer) distinguishes otherwise-
	// identical comparisons.
	tokA := &predicates.Comparison{Type: predicates.ComparisonTextContainsAll, TextTokenizerName: "std"}
	tokB := &predicates.Comparison{Type: predicates.ComparisonTextContainsAll, TextTokenizerName: "ngram"}
	if sargComparisonEqual(tokA, tokB) {
		t.Fatal("different text tokenizer must NOT be equal (variant identity field)")
	}
}

// TestSargSubset pins the "index SARGs strictly more" predicate: prefer the
// index iff (primary − index) is empty AND (index − primary) is non-empty.
func TestSargSubset(t *testing.T) {
	t.Parallel()

	pk := eqComp(values.LiteralValue(int64(1)))    // shared
	rt := eqComp(values.LiteralValue(int64(99)))   // index-only
	extra := eqComp(values.LiteralValue(int64(2))) // primary-only

	primary := []*predicates.Comparison{pk}
	index := []*predicates.Comparison{pk, rt}

	// Index has everything the primary has (pk) plus more (rt) → strict superset.
	if !sargSubset(primary, index) {
		t.Fatal("primary ⊆ index should hold (index covers pk)")
	}
	if sargSubset(index, primary) {
		t.Fatal("index ⊄ primary should hold (rt is index-only)")
	}

	// Primary has an extra SARG the index lacks → not a superset.
	primaryExtra := []*predicates.Comparison{pk, extra}
	if sargSubset(primaryExtra, index) {
		t.Fatal("primaryExtra ⊄ index (extra is primary-only)")
	}

	// Identical sets ⊆ each other.
	if !sargSubset(index, index) || !sargSubset(primary, primary) {
		t.Fatal("a ⊆ a must hold")
	}
	// Empty is a subset of anything.
	if !sargSubset(nil, index) {
		t.Fatal("∅ ⊆ index must hold")
	}
}
