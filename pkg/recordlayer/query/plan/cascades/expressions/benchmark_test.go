package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Every benchmark in this file VALIDATES the work it times, for the reason
// spelled out at the top of ../benchmark_test.go: a regression that turns the
// timed operation into an instant rejection records a headline speedup. That
// bites hardest here — a `SemanticEquals` that regressed to always-false would
// early-out of every comparison below and look like a large win, and an Insert
// whose dedup stopped firing would change which branch is being measured.
//
// Inputs are loop-invariant, so each check sits ONCE before b.ResetTimer():
// the same operands yield the same answer on every iteration, so one check
// speaks for the whole loop and the timed region does only the measured work.
//
// The two HashCodeWithoutChildren benchmarks in cte_expressions_test.go are the
// documented exception: a hash has no success/failure signal to assert, so they
// pin determinism (two calls agree) and nothing stronger. Their correctness is
// owned by the memo dedup tests, not by a benchmark.

// BenchmarkSemanticEquals_LeafPair times the simplest case: two leaf
// Scan expressions, no quantifiers, no permutations. Pins the
// hot-path cost.
func BenchmarkSemanticEquals_LeafPair(b *testing.B) {
	a := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	c := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	if !SemanticEquals(a, c, EmptyAliasMap()) {
		b.Fatal("SemanticEquals said two identical leaf scans differ — the benchmark would time an early-out, not a comparison")
	}
	b.ResetTimer()
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
	if !SemanticEquals(a, c, EmptyAliasMap()) {
		b.Fatal("SemanticEquals said two identically built filter trees differ — the child walk is not being measured")
	}
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
	// The reversed operand DOES match commutatively — that is the whole point:
	// the permutation search runs to a successful pairing. A regression that
	// stops finding it would return false sooner and read as a speedup while
	// silently breaking memo dedup for commutative operators.
	if !SemanticEquals(a, c, EmptyAliasMap()) {
		b.Fatal("SemanticEquals failed to match the reversed union — the permutation search is not being measured")
	}
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
	// 2 bindings composed with 1 must yield 3. Compose early-returns the
	// receiver untouched when the operand is empty, which is the cheap
	// degenerate path — asserting the merged size proves the loop measures the
	// real merge and not that early-out.
	if got := a.Compose(c).Size(); got != 3 {
		b.Fatalf("Compose produced %d bindings, want 3 — the merge path is not being measured", got)
	}
	b.ResetTimer()
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
	// Dedup must REJECT the duplicate (Insert=false, membership unchanged). If
	// it stopped firing, this would silently become the append benchmark under
	// a name that says dedup — and the Reference would grow by b.N members.
	// Insert is idempotent here, so this pre-loop probe leaves the loop's
	// starting state exactly as it found it.
	if inserted := r.Insert(scan); inserted || len(r.Members()) != 1 {
		b.Fatalf("Insert=%v members=%d; want false and 1 — dedup did not fire", inserted, len(r.Members()))
	}
	b.ResetTimer()
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
	// The mirror of the dedup case: the distinct insert must ACCEPT and grow to
	// 2. A dedup that over-matched would reject here and turn the full append
	// path this benchmark exists to time into an early-out. The probe uses its
	// own Reference, exactly as each iteration does.
	if probe := InitialOf(scanA); !probe.Insert(scanB) || len(probe.Members()) != 2 {
		b.Fatalf("Insert rejected a distinct expression (members=%d, want 2) — the append path is not being measured", len(probe.Members()))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := InitialOf(scanA)
		_ = r.Insert(scanB) // distinct → grows
	}
}
