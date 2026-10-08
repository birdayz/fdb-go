package predicates

import (
	"fmt"
	"hash/fnv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPredicateSemanticHashMatchesFNV(t *testing.T) {
	t.Parallel()
	comparison := NewComparisonPredicate(mustQOV(t, values.NamedCorrelationIdentifier("q")), Comparison{
		Type: ComparisonTextContainsAll, Operand: values.LiteralValue("abc\x00\xff"),
		ParameterName: "parameter", TextTokenizerName: "tokenizer", TextAnalyzerName: "analyzer",
		TextMaxDistance: 2, TextStrictPrefix: true,
	})
	for _, predicate := range []QueryPredicate{
		nil, comparison, NewNot(comparison), rangeCorrelationPredicate(t),
		NewAnd(comparison, NewNot(comparison)), NewOr(comparison, comparison),
		NewValuePredicate(values.NewBooleanValue(true)),
	} {
		want := fnv.New64a()
		writeSemanticHash(want, predicate)
		if got := SemanticHashCode(predicate); got != want.Sum64() {
			t.Fatalf("%T hash = %x, stdlib FNV = %x", predicate, got, want.Sum64())
		}
	}
}

func TestComparisonHashEncoding(t *testing.T) {
	t.Parallel()
	for _, predicate := range comparisonPredicatePool(t) {
		for _, atomic := range []bool{false, true} {
			p := *predicate
			var root QueryPredicate = &p
			comparison := p.Comparison
			want := fnv.New64a()
			if atomic {
				root = WithAtomicity(NewNot(root), true)
				_, _ = fmt.Fprint(want, "atomic:not[;")
			}
			_, _ = fmt.Fprintf(want, "cp:%d:%d:%s:%d:%s:%d:%s:%d:%t:%x:",
				comparison.Type, len(comparison.ParameterName), comparison.ParameterName,
				len(comparison.TextTokenizerName), comparison.TextTokenizerName,
				len(comparison.TextAnalyzerName), comparison.TextAnalyzerName,
				comparison.TextMaxDistance, comparison.TextStrictPrefix, values.SemanticHashCode(comparison.QueryVector))
			if comparison.EfSearch == nil {
				_, _ = fmt.Fprint(want, "-:")
			} else {
				_, _ = fmt.Fprintf(want, "e%d:", *comparison.EfSearch)
			}
			if comparison.IsReturningVectors == nil {
				_, _ = fmt.Fprint(want, "-:")
			} else {
				_, _ = fmt.Fprintf(want, "r%t:", *comparison.IsReturningVectors)
			}
			_, _ = fmt.Fprintf(want, "%x/", values.SemanticHashCode(p.Operand))
			if comparison.Type.IsUnary() {
				_, _ = fmt.Fprint(want, "u")
			} else {
				_, _ = fmt.Fprintf(want, "%x", values.SemanticHashCode(comparison.Operand))
			}
			_, _ = fmt.Fprint(want, "[]")
			if atomic {
				_, _ = fmt.Fprint(want, "]")
			}
			if got := SemanticHashCode(root); got != want.Sum64() {
				t.Fatalf("comparison %v atomic=%t: hash %x, want %x", comparison.Type, atomic, got, want.Sum64())
			}
		}
	}
}

func FuzzPredicateSemanticHashMatchesFNV(f *testing.F) {
	f.Add([]byte{2, 7, 3, 6, 4, 8})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Fuzz(func(t *testing.T, data []byte) {
		t.Parallel()
		predicate, _ := buildFuzzPredicate(t, data, 0, 0)
		want := fnv.New64a()
		writeSemanticHash(want, predicate)
		if got := SemanticHashCode(predicate); got != want.Sum64() {
			t.Fatalf("hash = %x, stdlib FNV = %x", got, want.Sum64())
		}
	})
}

func BenchmarkPredicateSemanticHash(b *testing.B) {
	comparison := NewComparisonPredicate(mustQOV(b, values.NamedCorrelationIdentifier("q")), Comparison{
		Type: ComparisonEquals, Operand: values.LiteralValue(int64(7)),
	})
	ranged := rangeCorrelationPredicate(b)
	predicate := NewOr(NewAnd(comparison, NewNot(ranged)), NewAnd(comparison, ranged))
	want := SemanticHashCode(predicate)
	b.ReportAllocs()
	for b.Loop() {
		if got := SemanticHashCode(predicate); got != want {
			b.Fatalf("hash changed from %x to %x", want, got)
		}
	}
}
