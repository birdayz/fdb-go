package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// TestApplyAbsorption_TieBreakKeepsJavasSurvivor pins which of two IDENTICAL
// clauses absorption keeps.
//
// The tie-break can only ever fire on identical clauses — equal size plus
// `containsAll` means equal sets — so it never changes the CONTENT of the
// result. What it changes is the surviving clause's POSITION, and position is
// the order of children in the emitted AND/OR, which is the plan text.
//
// Java removes `ci` when `ci.size() == cj.size() && i < j`
// (BooleanPredicateNormalizer.java:461) — the EARLIER duplicate goes. Go
// removed the later one, so `[A, X, A]` came out `[A, X]` where Java gives
// `[X, A]`.
//
// The fixture uses THREE clauses with the duplicate pair straddling a third,
// because that is the smallest shape where the two rules disagree: with only
// `[A, A]` both rules keep one A and the sequence is `[A]` either way, and the
// test would pass against the un-fixed code.
func TestApplyAbsorption_TieBreakKeepsJavasSurvivor(t *testing.T) {
	t.Parallel()
	a, x, _ := absorptionTieBreakLeaves(t)

	clauses := [][]predicates.QueryPredicate{{a}, {x}, {a}}
	got := applyAbsorption(clauses)

	if len(got) != 2 {
		t.Fatalf("expected the duplicate to be absorbed leaving 2 clauses, got %d", len(got))
	}
	// Java's order: the surviving A sits where the LATER duplicate was, so X
	// comes first.
	if !predicates.PredicateEquals(got[0][0], x) {
		t.Fatalf("first surviving clause is %s, want %s — the tie-break dropped the "+
			"LATER duplicate (Go's old `i > j`) instead of the earlier one (Java's `i < j`), "+
			"so the emitted child order diverges from Java",
			got[0][0].Explain(), x.Explain())
	}
	if !predicates.PredicateEquals(got[1][0], a) {
		t.Fatalf("second surviving clause is %s, want %s", got[1][0].Explain(), a.Explain())
	}
}

// TestApplyAbsorption_ContentIsTheMinimalAntichain pins what the tie-break
// canNOT change: the set of surviving clauses.
//
// Absorption computes the minimal antichain under set inclusion, which is
// unique — so Java's in-place shrinking loop and Go's two-pass form must agree
// on CONTENT even though they disagreed on order. Without this, a later
// "simplification" of the loop could change which clauses survive and only the
// order assertion above would notice, in one shape.
func TestApplyAbsorption_ContentIsTheMinimalAntichain(t *testing.T) {
	t.Parallel()
	a, x, y := absorptionTieBreakLeaves(t)

	// {a} absorbs both {a,x} and {a,y}; {x,y} is incomparable and survives.
	clauses := [][]predicates.QueryPredicate{{a, x}, {a}, {a, y}, {x, y}}
	got := applyAbsorption(clauses)

	if len(got) != 2 {
		t.Fatalf("expected the 2-clause antichain {a} and {x,y}, got %d clauses", len(got))
	}
	sizes := map[int]int{}
	for _, c := range got {
		sizes[len(c)]++
	}
	if sizes[1] != 1 || sizes[2] != 1 {
		t.Fatalf("expected one 1-atom and one 2-atom clause, got sizes %v", sizes)
	}
	if !predicates.PredicateEquals(got[0][0], a) {
		t.Fatalf("the singleton clause should be {a}, got %s", got[0][0].Explain())
	}
}

func absorptionTieBreakLeaves(t *testing.T) (a, x, y predicates.QueryPredicate) {
	t.Helper()
	bld := &predicateBuilder{script: []byte{0}}
	col0 := bld.column(0)
	col1 := bld.column(1)
	return predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
		predicates.NewComparisonPredicate(col1,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2))),
		predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))
}
