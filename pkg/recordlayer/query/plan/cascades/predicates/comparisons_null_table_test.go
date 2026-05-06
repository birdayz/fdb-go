package predicates

// Parameterised null-propagation table for binary Comparison.EvalAgainst.
// Mirrors the contract Java's
// fdb-record-layer-core/src/test/java/com/apple/foundationdb/record/
// query/plan/cascades/predicates/ConstantFoldingTest.java pins via
// equalTestArguments / lessThanTestArguments / etc.: every binary
// (i.e. non-unary, non-IS-NOT-NULL-ish) comparison operator returns
// UNKNOWN whenever either operand is NULL — except the null-safe
// pair IS [NOT] DISTINCT FROM, which evaluates NULL = NULL → TRUE
// and NULL = X → FALSE per SQL 92.
//
// We already have spot tests for ComparisonEquals (Eval_NullIsUnknown).
// This table is the per-operator regression sweep so a future
// EvalAgainst rewrite can't quietly break one operator while leaving
// the others correct.

import (
	"testing"
)

func TestComparison_NullPropagation_AllBinaryOps(t *testing.T) {
	t.Parallel()

	const nonNullL = int64(5)
	const nonNullR = int64(7)

	binaryOps := []struct {
		op   ComparisonType
		name string
	}{
		{ComparisonEquals, "="},
		{ComparisonNotEquals, "<>"},
		{ComparisonLessThan, "<"},
		{ComparisonLessThanOrEq, "<="},
		{ComparisonGreaterThan, ">"},
		{ComparisonGreaterThanEq, ">="},
	}

	for _, op := range binaryOps {
		op := op
		t.Run(op.name+"_NULL_left", func(t *testing.T) {
			t.Parallel()
			c := Comparison{Type: op.op}
			if got := c.EvalAgainst(nil, nonNullR); got != TriUnknown {
				t.Fatalf("%s NULL <op> %d: got %v, want UNKNOWN", op.name, nonNullR, got)
			}
		})
		t.Run(op.name+"_NULL_right", func(t *testing.T) {
			t.Parallel()
			c := Comparison{Type: op.op}
			if got := c.EvalAgainst(nonNullL, nil); got != TriUnknown {
				t.Fatalf("%d <op> NULL: got %v, want UNKNOWN", nonNullL, got)
			}
		})
		t.Run(op.name+"_both_NULL", func(t *testing.T) {
			t.Parallel()
			c := Comparison{Type: op.op}
			if got := c.EvalAgainst(nil, nil); got != TriUnknown {
				t.Fatalf("NULL <op> NULL: got %v, want UNKNOWN", got)
			}
		})
	}

	// IS DISTINCT FROM is the null-safe inequality:
	//   NULL DISTINCT FROM NULL → FALSE
	//   NULL DISTINCT FROM X    → TRUE
	//   X    DISTINCT FROM NULL → TRUE
	//   X    DISTINCT FROM X    → FALSE
	//   X    DISTINCT FROM Y    → TRUE
	t.Run("IS_DISTINCT_FROM_NULL_NULL", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsDistinctFrom}
		if got := c.EvalAgainst(nil, nil); got != TriFalse {
			t.Fatalf("got %v, want FALSE", got)
		}
	})
	t.Run("IS_DISTINCT_FROM_NULL_X", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsDistinctFrom}
		if got := c.EvalAgainst(nil, nonNullR); got != TriTrue {
			t.Fatalf("got %v, want TRUE", got)
		}
	})
	t.Run("IS_DISTINCT_FROM_X_NULL", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsDistinctFrom}
		if got := c.EvalAgainst(nonNullL, nil); got != TriTrue {
			t.Fatalf("got %v, want TRUE", got)
		}
	})
	t.Run("IS_DISTINCT_FROM_X_X", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsDistinctFrom}
		if got := c.EvalAgainst(nonNullL, nonNullL); got != TriFalse {
			t.Fatalf("got %v, want FALSE", got)
		}
	})
	t.Run("IS_DISTINCT_FROM_X_Y", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsDistinctFrom}
		if got := c.EvalAgainst(nonNullL, nonNullR); got != TriTrue {
			t.Fatalf("got %v, want TRUE", got)
		}
	})

	// IS NOT DISTINCT FROM is the null-safe equality (the dual of the
	// above). Same table with TRUE/FALSE flipped.
	t.Run("IS_NOT_DISTINCT_FROM_NULL_NULL", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonNotDistinctFrom}
		if got := c.EvalAgainst(nil, nil); got != TriTrue {
			t.Fatalf("got %v, want TRUE", got)
		}
	})
	t.Run("IS_NOT_DISTINCT_FROM_NULL_X", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonNotDistinctFrom}
		if got := c.EvalAgainst(nil, nonNullR); got != TriFalse {
			t.Fatalf("got %v, want FALSE", got)
		}
	})
	t.Run("IS_NOT_DISTINCT_FROM_X_NULL", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonNotDistinctFrom}
		if got := c.EvalAgainst(nonNullL, nil); got != TriFalse {
			t.Fatalf("got %v, want FALSE", got)
		}
	})
	t.Run("IS_NOT_DISTINCT_FROM_X_X", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonNotDistinctFrom}
		if got := c.EvalAgainst(nonNullL, nonNullL); got != TriTrue {
			t.Fatalf("got %v, want TRUE", got)
		}
	})
	t.Run("IS_NOT_DISTINCT_FROM_X_Y", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonNotDistinctFrom}
		if got := c.EvalAgainst(nonNullL, nonNullR); got != TriFalse {
			t.Fatalf("got %v, want FALSE", got)
		}
	})

	// LIKE / STARTS_WITH on NULL operands degrade to UNKNOWN — same
	// rule as the binary comparators since LIKE is a binary string
	// predicate.
	t.Run("LIKE_NULL_left", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonLike}
		if got := c.EvalAgainst(nil, "abc%"); got != TriUnknown {
			t.Fatalf("got %v, want UNKNOWN", got)
		}
	})
	t.Run("LIKE_NULL_right", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonLike}
		if got := c.EvalAgainst("abcdef", nil); got != TriUnknown {
			t.Fatalf("got %v, want UNKNOWN", got)
		}
	})
	t.Run("STARTS_WITH_NULL_left", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonStartsWith}
		if got := c.EvalAgainst(nil, "abc"); got != TriUnknown {
			t.Fatalf("got %v, want UNKNOWN", got)
		}
	})
	t.Run("STARTS_WITH_NULL_right", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonStartsWith}
		if got := c.EvalAgainst("abcdef", nil); got != TriUnknown {
			t.Fatalf("got %v, want UNKNOWN", got)
		}
	})
}

// TestComparison_NullPropagation_UnaryOps pins the unary-operator
// branch: IS [NOT] NULL resolve even when the operand is NULL (their
// whole job IS to test for NULL — they must NOT degrade to UNKNOWN).
func TestComparison_NullPropagation_UnaryOps(t *testing.T) {
	t.Parallel()

	t.Run("IS_NULL_on_NULL", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsNull}
		if got := c.Eval(nil); got != TriTrue {
			t.Fatalf("got %v, want TRUE", got)
		}
	})
	t.Run("IS_NULL_on_value", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsNull}
		if got := c.Eval(int64(5)); got != TriFalse {
			t.Fatalf("got %v, want FALSE", got)
		}
	})
	t.Run("IS_NOT_NULL_on_NULL", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsNotNull}
		if got := c.Eval(nil); got != TriFalse {
			t.Fatalf("got %v, want FALSE", got)
		}
	})
	t.Run("IS_NOT_NULL_on_value", func(t *testing.T) {
		t.Parallel()
		c := Comparison{Type: ComparisonIsNotNull}
		if got := c.Eval(int64(5)); got != TriTrue {
			t.Fatalf("got %v, want TRUE", got)
		}
	})
}
