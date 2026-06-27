package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

var (
	hashFuzzQ0 = values.NamedCorrelationIdentifier("q0")
	hashFuzzQ1 = values.NamedCorrelationIdentifier("q1")
)

// genHashFuzzValue builds a small Value tree over {q0,q1} driven by fuzz bytes.
// Covers correlation-bearing leaves (QOV) AND compound/discriminator-bearing
// structural types (FieldValue, RecordConstructor) so the consistency fuzz
// exercises the hash beyond the simplest leaves (RFC-040 review — broaden the
// completeness guard).
func genHashFuzzValue(b []byte, i, depth int) (values.Value, int) {
	if depth >= 4 || i >= len(b) {
		return &values.ConstantValue{Value: int64(0), Typ: values.NullableLong}, i
	}
	op := b[i] % 4
	i++
	switch op {
	case 0:
		al := hashFuzzQ0
		if i < len(b) && b[i]%2 == 1 {
			al = hashFuzzQ1
		}
		i++
		return values.NewQuantifiedObjectValue(al), i
	case 1:
		child, ni := genHashFuzzValue(b, i, depth+1)
		return &values.FieldValue{Field: "f", Typ: values.UnknownType, Child: child}, ni
	case 2:
		// RecordConstructor over a (possibly alias-bearing) child — compound
		// type with a folded discriminator (field name) + recursion.
		child, ni := genHashFuzzValue(b, i, depth+1)
		return values.NewRecordConstructorValue(values.RecordConstructorField{Name: "c", Value: child}), ni
	default:
		return &values.ConstantValue{Value: int64(b[i-1]), Typ: values.NullableLong}, i
	}
}

// FuzzValueSemanticHashConsistency is the RFC-040 040.0 linchpin gate: for any
// generated Value, alias-renaming it (swap q0↔q1) must (a) keep it
// ValueSemanticEquals under the corresponding alias map AND (b) leave its
// ValueSemanticHashCode UNCHANGED. This is exactly the contract the memo dedup
// gate relies on (semanticEquals ⟹ equal hash); a hash that depended on alias
// names would break it.
func FuzzValueSemanticHashConsistency(f *testing.F) {
	f.Add([]byte{0, 1})
	f.Add([]byte{1, 0, 0, 1})
	f.Add([]byte{0, 1, 1, 0, 2, 5})
	f.Add(make([]byte, 12))

	swap := values.AliasMap{hashFuzzQ0: hashFuzzQ1, hashFuzzQ1: hashFuzzQ0}
	bld := NewAliasMapBuilder()
	bld.Put(hashFuzzQ0, hashFuzzQ1)
	bld.Put(hashFuzzQ1, hashFuzzQ0)
	equiv := NewAliasMapValueEquivalence(bld.Build())

	f.Fuzz(func(t *testing.T, b []byte) {
		v, _ := genHashFuzzValue(b, 0, 0)
		rebased := values.RebaseValue(v, swap)

		// (a) rename preserves semantic equality under the swap map.
		if !ValueSemanticEquals(v, rebased, equiv).IsTrue() {
			t.Fatalf("alias-renamed value not semantically equal under swap map: %v vs %v", v, rebased)
		}
		// (b) hash is alias-invariant: equal-under-equiv ⟹ equal hash.
		if values.SemanticHashCode(v) != values.SemanticHashCode(rebased) {
			t.Fatalf("HASH CONSISTENCY VIOLATED: semantically-equal values hash differently: %v", v)
		}
	})
}

// FuzzPredicateSemanticHashConsistency extends the linchpin gate to predicates:
// a ComparisonPredicate over a generated Value, alias-swapped, must keep its
// PredicateSemanticHashCode unchanged (alias-invariant), consistent with
// alias-aware predicate equality.
func FuzzPredicateSemanticHashConsistency(f *testing.F) {
	f.Add([]byte{0, 1})
	f.Add([]byte{1, 0, 0, 1})
	f.Add([]byte{0, 1, 1, 0, 2, 5})
	f.Add(make([]byte, 12))

	swap := values.AliasMap{hashFuzzQ0: hashFuzzQ1, hashFuzzQ1: hashFuzzQ0}

	mkCmp := func(operand values.Value) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			operand,
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(7)}},
		)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		v, _ := genHashFuzzValue(b, 0, 0)
		rebased := values.RebaseValue(v, swap)
		if predicates.SemanticHashCode(mkCmp(v)) != predicates.SemanticHashCode(mkCmp(rebased)) {
			t.Fatalf("PREDICATE HASH CONSISTENCY VIOLATED: alias-renamed operand changed predicate hash: %v", v)
		}
	})
}
