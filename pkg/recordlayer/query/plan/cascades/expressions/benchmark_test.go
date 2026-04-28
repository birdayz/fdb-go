package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// BenchmarkSemanticEquals_LeafPair times the simplest case: two leaf
// Scan expressions, no quantifiers, no permutations. Pins the
// hot-path cost.
func BenchmarkSemanticEquals_LeafPair(b *testing.B) {
	a := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	c := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	for i := 0; i < b.N; i++ {
		_ = SemanticEquals(a, c, EmptyAliasMap())
	}
}

// BenchmarkSemanticEquals_FilterTree times a 2-level (Filter over
// Scan) shape — exercises positional pairing, predicate equality,
// child Reference walking.
func BenchmarkSemanticEquals_FilterTree(b *testing.B) {
	build := func() RelationalExpression {
		scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		q := ForEachQuantifier(InitialOf(scan))
		return NewLogicalFilterExpression(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			q,
		)
	}
	a, c := build(), build()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SemanticEquals(a, c, EmptyAliasMap())
	}
}

// BenchmarkSemanticEquals_UnionPermuted times the permutation-enumerating
// path for a 4-child commutative operator. 4! = 24 permutations
// per call; the benchmark pins the overhead is acceptable on the
// expected commutative-children fan-out.
func BenchmarkSemanticEquals_UnionPermuted(b *testing.B) {
	build := func(order []string) *LogicalUnionExpression {
		qs := make([]Quantifier, len(order))
		for i, name := range order {
			scan := NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
			qs[i] = ForEachQuantifier(InitialOf(scan))
		}
		return NewLogicalUnionExpression(qs)
	}
	a := build([]string{"A", "B", "C", "D"})
	c := build([]string{"D", "C", "B", "A"}) // worst-case permutation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SemanticEquals(a, c, EmptyAliasMap())
	}
}

// BenchmarkAliasMap_Compose times the bijection composition — runs
// in the Insert hot path under permutation-aware SemanticEquals.
func BenchmarkAliasMap_Compose(b *testing.B) {
	a := AliasMapOf(
		values.NamedCorrelationIdentifier("a"), values.NamedCorrelationIdentifier("b"),
		values.NamedCorrelationIdentifier("c"), values.NamedCorrelationIdentifier("d"),
	)
	c := AliasMapOf(
		values.NamedCorrelationIdentifier("e"), values.NamedCorrelationIdentifier("f"),
	)
	for i := 0; i < b.N; i++ {
		_ = a.Compose(c)
	}
}
