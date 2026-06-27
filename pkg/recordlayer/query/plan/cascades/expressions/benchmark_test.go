package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// BenchmarkReference_Insert_Dedup times the per-Insert dedup hot path
// when inserting a duplicate. Pins the EqualsWithoutChildren +
// sameChildReferences gate cost.
func BenchmarkReference_Insert_Dedup(b *testing.B) {
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	r := InitialOf(scan)
	for i := 0; i < b.N; i++ {
		// The inserted scan is structurally equal AND has no children,
		// so dedup hits the fast-path early-out.
		_ = r.Insert(scan)
	}
}

// BenchmarkReference_Insert_Distinct times the case where the inserted
// expression IS new — exercises the full Insert path including the
// append. Use a fresh Reference per iteration so we don't accumulate.
func BenchmarkReference_Insert_Distinct(b *testing.B) {
	scanA := NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := InitialOf(scanA)
		_ = r.Insert(scanB) // distinct → grows
	}
}
