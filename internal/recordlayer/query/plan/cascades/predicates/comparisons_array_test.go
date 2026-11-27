package predicates

// ARRAY (composite) comparison dispatch — the Go analogue of Java's
// EQ/NEQ/IS_DISTINCT/NOT_DISTINCT _ARRAY_ARRAY physical operators
// (RelOpValue.java:1149-1152), whose evaluation is
// Comparisons.compareListEquals (Comparisons.java:301-310). Pins the
// two-valued property: once both OPERANDS are non-NULL the answer is
// TRUE/FALSE, and element NULLs do not escalate to UNKNOWN.

import "testing"

func TestComparison_EvalAgainst_ArrayEquality(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		typ   ComparisonType
		left  any
		right any
		want  TriBool
	}{
		{"equal single", ComparisonEquals, []any{int64(1)}, []any{int64(1)}, TriTrue},
		{"order sensitive", ComparisonEquals, []any{int64(1), int64(2)}, []any{int64(2), int64(1)}, TriFalse},
		{"size mismatch is FALSE not UNKNOWN", ComparisonEquals, []any{int64(1)}, []any{int64(1), int64(2)}, TriFalse},
		{"both-NULL elements are EQUAL", ComparisonEquals, []any{nil}, []any{nil}, TriTrue},
		{"one-NULL element is UNEQUAL, still two-valued", ComparisonEquals, []any{int64(1), nil}, []any{int64(1), int64(2)}, TriFalse},
		{"nested arrays recurse", ComparisonEquals, []any{[]any{int64(1), int64(2)}}, []any{[]any{int64(1), int64(2)}}, TriTrue},
		{
			"record elements compare by field", ComparisonEquals,
			[]any{map[string]any{"a": int64(1), "b": "x"}},
			[]any{map[string]any{"a": int64(1), "b": "x"}},
			TriTrue,
		},
		{
			"record field mismatch", ComparisonEquals,
			[]any{map[string]any{"a": int64(1)}},
			[]any{map[string]any{"a": int64(2)}},
			TriFalse,
		},
		// Numeric-width noise between carriers of the same element type
		// must not fabricate inequality (scalar leaves go through cmpAny).
		{"int32 vs int64 carriers equal", ComparisonEquals, []any{int32(1)}, []any{int64(1)}, TriTrue},
		{"NEQ negates", ComparisonNotEquals, []any{int64(1)}, []any{int64(2)}, TriTrue},
		{"NEQ equal arrays", ComparisonNotEquals, []any{int64(1)}, []any{int64(1)}, TriFalse},
		// NULL OPERAND (not element) stays UNKNOWN per SQL 3VL.
		{"NULL operand is UNKNOWN", ComparisonEquals, nil, []any{int64(1)}, TriUnknown},
		// Null-safe forms resolve on arrays.
		{"IS DISTINCT FROM equal arrays", ComparisonIsDistinctFrom, []any{int64(1)}, []any{int64(1)}, TriFalse},
		{"IS DISTINCT FROM differing arrays", ComparisonIsDistinctFrom, []any{int64(1)}, []any{int64(2)}, TriTrue},
		{"NOT DISTINCT FROM equal arrays", ComparisonNotDistinctFrom, []any{int64(1)}, []any{int64(1)}, TriTrue},
		{"NOT DISTINCT FROM vs NULL operand", ComparisonNotDistinctFrom, []any{int64(1)}, nil, TriFalse},
		// An ordering op that meets an array at runtime (the plan gate
		// rejects it 42804 before this) degrades to UNKNOWN, exactly as
		// the scalar type-mismatch path always did.
		{"ordering over arrays degrades UNKNOWN", ComparisonLessThan, []any{int64(1)}, []any{int64(2)}, TriUnknown},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Comparison{Type: tc.typ}.EvalAgainst(tc.left, tc.right)
			if err != nil {
				t.Fatalf("EvalAgainst: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EvalAgainst(%v, %v) = %v, want %v", tc.left, tc.right, got, tc.want)
			}
		})
	}
}
