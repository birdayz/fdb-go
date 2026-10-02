package cascades

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// referenceAbsorptionSurvivors is the pairwise PredicateEquals scan that the
// equality-class bitsets replace; the two must agree on every input.
func referenceAbsorptionSurvivors(deduped [][]predicates.QueryPredicate) []int {
	containsAll := func(haystack, needles []predicates.QueryPredicate) bool {
		for _, needle := range needles {
			if !slices.ContainsFunc(haystack, func(candidate predicates.QueryPredicate) bool {
				return predicates.PredicateEquals(needle, candidate)
			}) {
				return false
			}
		}
		return true
	}
	result := make([]int, 0, len(deduped))
	for i, ci := range deduped {
		absorbed := false
		for j, cj := range deduped {
			if i != j && (len(ci) > len(cj) || (len(ci) == len(cj) && i < j)) && containsAll(ci, cj) {
				absorbed = true
				break
			}
		}
		if !absorbed {
			result = append(result, i)
		}
	}
	return result
}

// absorptionPool has distinct objects that PredicateEquals identifies, atomic
// twins it separates, and more classes than one bitset word holds.
func absorptionPool(t testing.TB) []predicates.QueryPredicate {
	t.Helper()
	leaves := simplificationContractLeaves(t)
	var pool []predicates.QueryPredicate
	twins := simplificationContractLeaves(t)
	for i, leaf := range leaves {
		pool = append(pool, leaf, twins[i])
	}
	pool = append(pool,
		predicates.WithAtomicity(predicates.NewOr(leaves[0], leaves[1]), true),
		predicates.NewOr(leaves[0], leaves[1]),
		predicates.NewNot(leaves[2]),
		predicates.NewNot(simplificationContractLeaves(t)[2]),
	)
	for i := range 70 {
		pool = append(pool, predicates.NewComparisonPredicate(leaves[i%len(leaves)].(*predicates.ComparisonPredicate).Operand,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(100+i))))
	}
	return pool
}

func TestAbsorptionSurvivorsMatchPairwiseScan(t *testing.T) {
	t.Parallel()
	pool := absorptionPool(t)
	leaves := simplificationContractLeaves(t)
	twins := simplificationContractLeaves(t)
	for _, tc := range []struct {
		name    string
		clauses [][]predicates.QueryPredicate
		want    []int
	}{
		{"equal sets keep the last", [][]predicates.QueryPredicate{{leaves[0]}, {twins[0]}}, []int{1}},
		{"subset absorbs superset", [][]predicates.QueryPredicate{{leaves[0], leaves[1]}, {twins[1]}}, []int{1}},
		{"atomicity separates", [][]predicates.QueryPredicate{{pool[16]}, {pool[17]}}, []int{0, 1}},
		{"second bitset word", [][]predicates.QueryPredicate{pool[20:90], {pool[89], pool[20]}}, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := absorptionSurvivors(tc.clauses); !slices.Equal(got, tc.want) || !slices.Equal(got, referenceAbsorptionSurvivors(tc.clauses)) {
				t.Fatalf("survivors=%v, want %v (pairwise %v)", got, tc.want, referenceAbsorptionSurvivors(tc.clauses))
			}
		})
	}
}

func FuzzAbsorptionSurvivorsMatchPairwiseScan(f *testing.F) {
	f.Add([]byte{2, 0, 1, 1, 2, 3, 0})
	f.Add([]byte{3, 16, 17, 18, 2, 0, 19, 4, 20, 89, 21, 88})
	f.Fuzz(func(t *testing.T, data []byte) {
		t.Parallel()
		pool := absorptionPool(t)
		var clauses [][]predicates.QueryPredicate
		for i := 0; i < len(data) && len(clauses) < 24; {
			size := 1 + int(data[i])%6
			i++
			var clause []predicates.QueryPredicate
			for ; size > 0 && i < len(data); size, i = size-1, i+1 {
				clause = append(clause, pool[int(data[i])%len(pool)])
			}
			clauses = append(clauses, dedupPredicateSlice(clause))
		}
		if got, want := absorptionSurvivors(clauses), referenceAbsorptionSurvivors(clauses); !slices.Equal(got, want) {
			t.Fatalf("survivors=%v, pairwise scan=%v", got, want)
		}
	})
}

func BenchmarkPredicateUnionNineFactorDNF(b *testing.B) {
	leaves := simplificationContractLeaves(b)
	p, q, r, s, x, u, v := leaves[0], leaves[1], leaves[2], leaves[3], leaves[4], leaves[5], leaves[6]
	or := predicates.NewOr
	factors := []predicates.QueryPredicate{
		or(p, s, x), or(q, s, x), or(r, s, x),
		or(p, u), or(q, u), or(r, u),
		or(p, v), or(q, v), or(r, v),
	}
	b.ReportAllocs()
	for b.Loop() {
		terms, err := predicateUnionDNFTerms(factors)
		if err != nil || len(terms) != 3 {
			b.Fatalf("terms=%d err=%v", len(terms), err)
		}
	}
}
