package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// comparisonPredicatePool spans the ComparisonPredicate identity axes: type,
// literal comparand, parameter binding, LIKE escape, text-search comparand
// fields, and unary operand representation. The final entry duplicates the
// first so the property test is never vacuously true.
func comparisonPredicatePool() []*ComparisonPredicate {
	x := values.NewFlatFieldValue("X", values.UnknownType)
	lit := func(n int64) values.Value { return &values.ConstantValue{Value: n, Typ: values.NotNullLong} }
	return []*ComparisonPredicate{
		{Operand: x, Comparison: Comparison{Type: ComparisonEquals, Operand: lit(5)}},
		{Operand: x, Comparison: Comparison{Type: ComparisonEquals, Operand: lit(7)}},
		{Operand: x, Comparison: Comparison{Type: ComparisonEquals, ParameterName: "a"}},
		{Operand: x, Comparison: Comparison{Type: ComparisonEquals, ParameterName: "b"}},
		{Operand: x, Comparison: Comparison{Type: ComparisonLike, Operand: lit(5), Escape: '\\'}},
		{Operand: x, Comparison: Comparison{Type: ComparisonLike, Operand: lit(5), Escape: '!'}},
		{Operand: x, Comparison: Comparison{Type: ComparisonTextContainsAll, Operand: lit(5), TextTokenizerName: "default"}},
		{Operand: x, Comparison: Comparison{Type: ComparisonTextContainsAll, Operand: lit(5), TextTokenizerName: "ngram"}},
		{Operand: x, Comparison: Comparison{Type: ComparisonTextContainsAll, Operand: lit(5), TextMaxDistance: 3}},
		{Operand: x, Comparison: Comparison{Type: ComparisonTextContainsAll, Operand: lit(5), TextStrictPrefix: true}},
		{Operand: x, Comparison: Comparison{Type: ComparisonIsNull}},
		{Operand: x, Comparison: Comparison{Type: ComparisonIsNull, Operand: &values.NullValue{}}},
		{Operand: x, Comparison: Comparison{Type: ComparisonEquals, Operand: lit(5)}},
	}
}

// TestComparisonPredicate_EqualImpliesSameHash pins the predicate-layer
// equal⟹same-hash invariant for BOTH equality layers against
// SemanticHashCode. Pre-fix violations, each RED here:
//   - PredicateEquals ignored ParameterName while the hash folded it:
//     `x = $a` ≡ `x = $b` under PredicateEquals but hashed apart.
//   - The hash folded the unary Comparison.Operand while both equality
//     layers deliberately ignore it: IsNull{nil} ≡ IsNull{Literal(nil)}
//     compared equal but hashed apart.
func TestComparisonPredicate_EqualImpliesSameHash(t *testing.T) {
	t.Parallel()
	pool := comparisonPredicatePool()
	for i, a := range pool {
		for j, b := range pool {
			ha, hb := SemanticHashCode(a), SemanticHashCode(b)
			if PredicateEquals(a, b) && ha != hb {
				t.Errorf("pool[%d] vs pool[%d]: PredicateEquals but hash apart", i, j)
			}
			if SemanticEqualsUnderAliasMap(a, b, nil) && ha != hb {
				t.Errorf("pool[%d] vs pool[%d]: SemanticEqualsUnderAliasMap but hash apart", i, j)
			}
		}
	}
	// Non-vacuousness: first and last entries are the same predicate rebuilt.
	first, dup := pool[0], pool[len(pool)-1]
	if !PredicateEquals(first, dup) || SemanticHashCode(first) != SemanticHashCode(dup) {
		t.Fatal("identical predicates rebuilt must compare equal and hash equal")
	}
}

// TestComparisonPredicate_Discriminators pins the individual identity
// dimensions the fix added.
func TestComparisonPredicate_Discriminators(t *testing.T) {
	t.Parallel()
	pool := comparisonPredicatePool()
	pa, pb := pool[2], pool[3] // x = $a vs x = $b
	if PredicateEquals(pa, pb) {
		t.Error("x = $a and x = $b read different runtime bindings — must NOT compare equal")
	}
	tokA, tokB := pool[6], pool[7] // tokenizer default vs ngram
	if PredicateEquals(tokA, tokB) {
		t.Error("TEXT comparisons differing only in tokenizer read different results — must NOT compare equal")
	}
	nilOp, litOp := pool[10], pool[11] // IsNull operand representations
	if !PredicateEquals(nilOp, litOp) {
		t.Error("unary comparisons ignore Comparison.Operand at Eval time — representations must compare equal")
	}
	if SemanticHashCode(nilOp) != SemanticHashCode(litOp) {
		t.Error("equal unary predicates must hash equal (operand hash must not fold)")
	}
}
