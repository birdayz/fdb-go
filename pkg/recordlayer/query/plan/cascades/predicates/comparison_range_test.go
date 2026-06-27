package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestEmptyComparisonRange_Universe(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	if !r.IsEmpty() {
		t.Fatal("EmptyComparisonRange should be empty")
	}
	if r.IsEquality() || r.IsInequality() {
		t.Fatalf("empty range mis-typed: equality=%v ineq=%v", r.IsEquality(), r.IsInequality())
	}
}

func TestComparisonRange_MergeEqualsIntoEmpty(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c := NewLiteralComparison(ComparisonEquals, int64(5))
	res := r.Merge(&c)
	if !res.Ok {
		t.Fatal("Empty + EQUALS should merge")
	}
	if !res.Range.IsEquality() {
		t.Fatal("merged range should be equality")
	}
}

func TestComparisonRange_MergeInequalityIntoEmpty(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c := NewLiteralComparison(ComparisonGreaterThan, int64(5))
	res := r.Merge(&c)
	if !res.Ok {
		t.Fatal("Empty + > should merge")
	}
	if !res.Range.IsInequality() {
		t.Fatal("merged range should be inequality")
	}
	if got := len(res.Range.GetInequalityComparisons()); got != 1 {
		t.Fatalf("inequality count = %d, want 1", got)
	}
}

func TestComparisonRange_MergeEqualsIntoEqualitySameValueIdempotent(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c1 := NewLiteralComparison(ComparisonEquals, int64(5))
	r1 := r.Merge(&c1).Range
	c2 := NewLiteralComparison(ComparisonEquals, int64(5))
	res := r1.Merge(&c2)
	if !res.Ok {
		t.Fatal("EQ(5) + EQ(5) should merge (idempotent)")
	}
	if !res.Range.IsEquality() {
		t.Fatal("merged range should be equality")
	}
}

func TestComparisonRange_MergeEqualsIntoEqualityDifferentValueFails(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c1 := NewLiteralComparison(ComparisonEquals, int64(5))
	r1 := r.Merge(&c1).Range
	c2 := NewLiteralComparison(ComparisonEquals, int64(10))
	res := r1.Merge(&c2)
	if res.Ok {
		t.Fatal("EQ(5) + EQ(10) should fail (unsatisfiable)")
	}
	if res.Residual == nil || res.Residual.Type != ComparisonEquals {
		t.Fatalf("residual = %v, want EQ(10)", res.Residual)
	}
}

func TestComparisonRange_MergeInequalityIntoEqualityFails(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c1 := NewLiteralComparison(ComparisonEquals, int64(5))
	r1 := r.Merge(&c1).Range
	c2 := NewLiteralComparison(ComparisonGreaterThan, int64(3))
	res := r1.Merge(&c2)
	if res.Ok {
		t.Fatal("EQ + > should fail (planner doesn't combine = and range)")
	}
}

func TestComparisonRange_MergeInequalityIntoInequalityExtends(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c1 := NewLiteralComparison(ComparisonGreaterThan, int64(5))
	r1 := r.Merge(&c1).Range
	c2 := NewLiteralComparison(ComparisonLessThan, int64(20))
	res := r1.Merge(&c2)
	if !res.Ok {
		t.Fatal("> + < should merge into inequality range")
	}
	if !res.Range.IsInequality() {
		t.Fatal("range should remain inequality")
	}
	if got := len(res.Range.GetInequalityComparisons()); got != 2 {
		t.Fatalf("inequality count = %d, want 2", got)
	}
}

func TestComparisonRange_MergeEqualsIntoInequalityFails(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c1 := NewLiteralComparison(ComparisonGreaterThan, int64(5))
	r1 := r.Merge(&c1).Range
	c2 := NewLiteralComparison(ComparisonEquals, int64(10))
	res := r1.Merge(&c2)
	if res.Ok {
		t.Fatal("Inequality + EQUALS should fail")
	}
}

func TestComparisonRange_GetEqualityComparisonPanicsOnWrongType(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	defer func() {
		if recover() == nil {
			t.Fatal("GetEqualityComparison on Empty should panic")
		}
	}()
	r.GetEqualityComparison()
}

func TestComparisonRange_NilMergeIsNoOp(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	res := r.Merge(nil)
	if !res.Ok {
		t.Fatal("Merge(nil) should be no-op")
	}
	if res.Range != r {
		t.Fatal("Merge(nil) should return same range")
	}
}

// Suppress unused import — values is referenced by the test
// expectations below.
var _ = values.LiteralValue
