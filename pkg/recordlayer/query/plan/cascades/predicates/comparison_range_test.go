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
	if !res.Complete() {
		t.Fatal("Empty + EQUALS should merge")
	}
	if !res.Range.IsEquality() {
		t.Fatal("merged range should be equality")
	}
}

func TestComparisonRange_MergeNotDistinctFromIntoEmptyIsEquality(t *testing.T) {
	t.Parallel()

	comparison := Comparison{
		Type:    ComparisonNotDistinctFrom,
		Operand: values.LiteralValue(nil),
	}
	result := EmptyComparisonRange().Merge(&comparison)
	if !result.Complete() || result.Range == nil || !result.Range.IsEquality() {
		t.Fatalf("IS NOT DISTINCT FROM merge = %+v, want equality range", result)
	}
	if got := result.Range.GetEqualityComparison(); got != &comparison {
		t.Fatalf("stored comparison = %p, want %p", got, &comparison)
	}
}

func TestComparisonRange_MergeInequalityIntoEmpty(t *testing.T) {
	t.Parallel()
	r := EmptyComparisonRange()
	c := NewLiteralComparison(ComparisonGreaterThan, int64(5))
	res := r.Merge(&c)
	if !res.Complete() {
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
	if !res.Complete() {
		t.Fatal("EQ(5) + EQ(5) should merge (idempotent)")
	}
	if !res.Range.IsEquality() {
		t.Fatal("merged range should be equality")
	}
}

// TestComparisonRange_MergeIsTotal pins Java's ComparisonRange.merge arm for
// arm (ComparisonRange.java:358-400): the merge never fails, the range is what
// Java's would be, and the residual LIST names exactly the comparisons Java
// would hand back — asserted by identity, not by count, so an arm that
// residualises the wrong comparison cannot pass.
func TestComparisonRange_MergeIsTotal(t *testing.T) {
	t.Parallel()

	eq5 := NewLiteralComparison(ComparisonEquals, int64(5))
	eq5Again := NewLiteralComparison(ComparisonEquals, int64(5))
	eq10 := NewLiteralComparison(ComparisonEquals, int64(10))
	gt3 := NewLiteralComparison(ComparisonGreaterThan, int64(3))
	gt3Again := NewLiteralComparison(ComparisonGreaterThan, int64(3))
	lt20 := NewLiteralComparison(ComparisonLessThan, int64(20))
	ne7 := NewLiteralComparison(ComparisonNotEquals, int64(7))
	in := Comparison{Type: ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2)})}

	equality := EmptyComparisonRange().Merge(&eq5).Range
	inequality := EmptyComparisonRange().Merge(&gt3).Range

	for _, tc := range []struct {
		name          string
		start         *ComparisonRange
		incoming      *Comparison
		wantType      ComparisonRangeType
		wantRange     []*Comparison // the comparisons the merged range carries, in order
		wantResiduals []*Comparison
	}{
		{"none_type_into_empty_is_residual", EmptyComparisonRange(), &ne7, ComparisonRangeEmpty, nil, []*Comparison{&ne7}},
		{"in_into_equality_is_residual", equality, &in, ComparisonRangeEquality, []*Comparison{&eq5}, []*Comparison{&in}},
		{"none_type_into_inequality_is_residual", inequality, &ne7, ComparisonRangeInequality, []*Comparison{&gt3}, []*Comparison{&ne7}},
		{"equality_into_empty", EmptyComparisonRange(), &eq5, ComparisonRangeEquality, []*Comparison{&eq5}, nil},
		{"inequality_into_empty", EmptyComparisonRange(), &gt3, ComparisonRangeInequality, []*Comparison{&gt3}, nil},
		{"inequality_into_equality_is_residual", equality, &gt3, ComparisonRangeEquality, []*Comparison{&eq5}, []*Comparison{&gt3}},
		{"same_equality_into_equality_dedups", equality, &eq5Again, ComparisonRangeEquality, []*Comparison{&eq5}, nil},
		{"other_equality_into_equality_is_residual", equality, &eq10, ComparisonRangeEquality, []*Comparison{&eq5}, []*Comparison{&eq10}},
		{"present_inequality_into_inequality_dedups", inequality, &gt3Again, ComparisonRangeInequality, []*Comparison{&gt3}, nil},
		{"new_inequality_into_inequality_appends", inequality, &lt20, ComparisonRangeInequality, []*Comparison{&gt3, &lt20}, nil},
		{"equality_into_inequality_wins_and_residualises_the_inequalities", inequality, &eq10, ComparisonRangeEquality, []*Comparison{&eq10}, []*Comparison{&gt3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := tc.start.Merge(tc.incoming)
			if res.Range == nil {
				t.Fatal("total merge returned a nil range")
			}
			if got := res.Range.GetRangeType(); got != tc.wantType {
				t.Fatalf("range type = %v, want %v", got, tc.wantType)
			}
			assertSameComparisons(t, "range", res.Range.GetComparisons(), tc.wantRange)
			assertSameComparisons(t, "residuals", res.Residuals, tc.wantResiduals)
			if res.Complete() != (len(tc.wantResiduals) == 0) {
				t.Fatalf("Complete() = %v with residuals %v", res.Complete(), res.Residuals)
			}
		})
	}

	// The start ranges are not mutated by any arm: a merge returns a new range
	// or the receiver, never an edited receiver.
	if got := equality.GetComparisons(); len(got) != 1 || got[0] != &eq5 {
		t.Fatalf("equality receiver mutated: %v", got)
	}
	if got := inequality.GetComparisons(); len(got) != 1 || got[0] != &gt3 {
		t.Fatalf("inequality receiver mutated: %v", got)
	}
}

// TestComparisonRange_MergeRangeAndMergeAll pins the two list-level overloads
// (Java's merge(ComparisonRange) and mergeAll): residuals accumulate across
// the walk, an equality arriving after inequalities takes the range and
// residualises them, and an equality receiver keeps every incoming
// inequality as a residual.
func TestComparisonRange_MergeRangeAndMergeAll(t *testing.T) {
	t.Parallel()

	eq5 := NewLiteralComparison(ComparisonEquals, int64(5))
	gt3 := NewLiteralComparison(ComparisonGreaterThan, int64(3))
	lt20 := NewLiteralComparison(ComparisonLessThan, int64(20))
	ne7 := NewLiteralComparison(ComparisonNotEquals, int64(7))

	twoBounds := MergeAll([]*Comparison{&gt3, &lt20})
	assertSameComparisons(t, "two bounds range", twoBounds.Range.GetComparisons(), []*Comparison{&gt3, &lt20})
	assertSameComparisons(t, "two bounds residuals", twoBounds.Residuals, nil)

	late := MergeAll([]*Comparison{&gt3, &ne7, &lt20, &eq5})
	if !late.Range.IsEquality() {
		t.Fatalf("late equality must take the range, got %v", late.Range.GetRangeType())
	}
	assertSameComparisons(t, "late equality range", late.Range.GetComparisons(), []*Comparison{&eq5})
	assertSameComparisons(t, "late equality residuals", late.Residuals, []*Comparison{&ne7, &gt3, &lt20})

	onto := EmptyComparisonRange().Merge(&eq5).Range.MergeRange(twoBounds.Range)
	assertSameComparisons(t, "equality receiver range", onto.Range.GetComparisons(), []*Comparison{&eq5})
	assertSameComparisons(t, "equality receiver residuals", onto.Residuals, []*Comparison{&gt3, &lt20})

	empty := EmptyComparisonRange().MergeRange(EmptyComparisonRange())
	if !empty.Range.IsEmpty() || !empty.Complete() {
		t.Fatalf("empty onto empty = %+v", empty)
	}
	if nilRange := twoBounds.Range.MergeRange(nil); nilRange.Range != twoBounds.Range || !nilRange.Complete() {
		t.Fatalf("nil operand must be a no-op, got %+v", nilRange)
	}
}

func assertSameComparisons(t *testing.T, what string, got, want []*Comparison) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d comparisons %v, want %d %v", what, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s[%d]: got %p (%v), want %p (%v)", what, i, got[i], got[i].Type, want[i], want[i].Type)
		}
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
	if !res.Complete() {
		t.Fatal("Merge(nil) should be no-op")
	}
	if res.Range != r {
		t.Fatal("Merge(nil) should return same range")
	}
}

// Suppress unused import — values is referenced by the test
// expectations below.
var _ = values.LiteralValue
