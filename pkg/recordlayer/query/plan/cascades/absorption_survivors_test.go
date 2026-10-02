package cascades

import (
	"bytes"
	"os"
	"os/exec"
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

func TestPredicateClassesInlineOverflow(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		var classes predicateClasses
		if cached {
			classes.byPointer = make(map[uintptr]int)
		}
		operand := simplificationContractLeaves(t)[0].(*predicates.ComparisonPredicate).Operand
		makePredicate := func(i int) predicates.QueryPredicate {
			return predicates.NewComparisonPredicate(operand, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i)))
		}
		pool := make([]predicates.QueryPredicate, 34)
		for i := range pool {
			pool[i] = makePredicate(i)
			if got := classes.id(pool[i]); got != i {
				t.Fatalf("cached=%t: new class=%d, want %d", cached, got, i)
			}
		}
		for i := len(pool) - 1; i >= 0; i-- {
			for _, predicate := range []predicates.QueryPredicate{pool[i], makePredicate(i)} {
				if got := classes.id(predicate); got != i {
					t.Fatalf("cached=%t: repeated class=%d, want %d across inline/overflow boundary", cached, got, i)
				}
			}
		}
		if classes.size != len(pool) {
			t.Fatalf("cached=%t: classes=%d, want %d", cached, classes.size, len(pool))
		}
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

func TestAbsorptionRepeatedClausesAllocationBound(t *testing.T) {
	t.Parallel()
	runAbsorptionAllocationBenchmark(t, "BenchmarkAbsorptionRepeatedClauses")
}

func TestAbsorptionSmallClausesAllocationBound(t *testing.T) {
	t.Parallel()
	runAbsorptionAllocationBenchmark(t, "BenchmarkAbsorptionSmallClauses")
}

func runAbsorptionAllocationBenchmark(t *testing.T, benchmark string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Allocation accounting is process-wide, so isolate it from parallel tests.
	output, err := exec.CommandContext(t.Context(), executable,
		"-test.run=^$", "-test.bench=^"+benchmark+"$", "-test.benchtime=1x").CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(benchmark)) {
		t.Fatalf("allocation benchmark: %v\n%s", err, output)
	}
}

func BenchmarkAbsorptionRepeatedClauses(b *testing.B) {
	leaf := simplificationContractLeaves(b)[0]
	clauses := make([][]predicates.QueryPredicate, 1024)
	for i := range clauses {
		clauses[i] = []predicates.QueryPredicate{leaf}
	}
	var survivors []int
	allocations := testing.AllocsPerRun(5, func() {
		survivors = absorptionSurvivors(clauses)
	})
	if !slices.Equal(survivors, []int{len(clauses) - 1}) {
		b.Fatalf("survivors=%v, want the last identical clause", survivors)
	}
	// Repeated clauses share one equality class; workspace allocation must not
	// allocate a separate object for each clause.
	if allocations >= float64(len(clauses)/8) {
		b.Fatalf("%.0f allocations for %d identical clauses: per-clause workspace allocation returned", allocations, len(clauses))
	}
	b.ReportAllocs()
	for b.Loop() {
		absorptionSurvivors(clauses)
	}
}

func BenchmarkAbsorptionSmallClauses(b *testing.B) {
	pool := absorptionPool(b)
	clauses := make([][]predicates.QueryPredicate, 9)
	for i := range clauses {
		clauses[i] = pool[20+i : 21+i]
	}
	var survivors []int
	allocations := testing.AllocsPerRun(5, func() {
		survivors = absorptionSurvivors(clauses)
	})
	if !slices.Equal(survivors, []int{0, 1, 2, 3, 4, 5, 6, 7, 8}) {
		b.Fatalf("survivors=%v, want all nine distinct clauses in order", survivors)
	}
	if allocations > 1 {
		b.Fatalf("%.0f allocations for nine singleton clauses: only the returned survivor slice should escape", allocations)
	}
	b.ReportAllocs()
	for b.Loop() {
		absorptionSurvivors(clauses)
	}
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
