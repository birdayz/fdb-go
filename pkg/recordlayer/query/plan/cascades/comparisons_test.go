package cascades

import "testing"

var _ QueryPredicate = (*ComparisonPredicate)(nil)

func TestComparisonType_Symbol(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   ComparisonType
		want string
	}{
		{ComparisonEquals, "="},
		{ComparisonNotEquals, "<>"},
		{ComparisonLessThan, "<"},
		{ComparisonLessThanOrEq, "<="},
		{ComparisonGreaterThan, ">"},
		{ComparisonGreaterThanEq, ">="},
		{ComparisonType(999), "?"},
	}
	for _, tc := range cases {
		if got := tc.in.Symbol(); got != tc.want {
			t.Fatalf("%d: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestComparison_Eval_Integers(t *testing.T) {
	t.Parallel()
	c := func(ct ComparisonType, rhs any) Comparison { return Comparison{Type: ct, Operand: rhs} }

	cases := []struct {
		name string
		cmp  Comparison
		left any
		want TriBool
	}{
		// Equality
		{"1 = 1", c(ComparisonEquals, int64(1)), int64(1), TriTrue},
		{"1 = 2", c(ComparisonEquals, int64(2)), int64(1), TriFalse},
		// Inequality
		{"1 <> 2", c(ComparisonNotEquals, int64(2)), int64(1), TriTrue},
		{"1 <> 1", c(ComparisonNotEquals, int64(1)), int64(1), TriFalse},
		// Strict lt/gt
		{"1 < 2", c(ComparisonLessThan, int64(2)), int64(1), TriTrue},
		{"2 < 1", c(ComparisonLessThan, int64(1)), int64(2), TriFalse},
		{"1 < 1", c(ComparisonLessThan, int64(1)), int64(1), TriFalse},
		{"2 > 1", c(ComparisonGreaterThan, int64(1)), int64(2), TriTrue},
		{"1 > 2", c(ComparisonGreaterThan, int64(2)), int64(1), TriFalse},
		// Inclusive lt/gt
		{"1 <= 1", c(ComparisonLessThanOrEq, int64(1)), int64(1), TriTrue},
		{"1 <= 2", c(ComparisonLessThanOrEq, int64(2)), int64(1), TriTrue},
		{"2 <= 1", c(ComparisonLessThanOrEq, int64(1)), int64(2), TriFalse},
		{"1 >= 1", c(ComparisonGreaterThanEq, int64(1)), int64(1), TriTrue},
		{"2 >= 1", c(ComparisonGreaterThanEq, int64(1)), int64(2), TriTrue},
		{"1 >= 2", c(ComparisonGreaterThanEq, int64(2)), int64(1), TriFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.cmp.Eval(tc.left)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComparison_Eval_NullIsUnknown(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonEquals, Operand: int64(5)}
	if got := c.Eval(nil); got != TriUnknown {
		t.Fatalf("left=NULL: got %v", got)
	}
	c2 := Comparison{Type: ComparisonEquals, Operand: nil}
	if got := c2.Eval(int64(5)); got != TriUnknown {
		t.Fatalf("right=NULL: got %v", got)
	}
}

func TestComparison_Eval_TypeMismatchIsUnknown(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonEquals, Operand: int64(5)}
	// String vs int: types don't match, cmpAny returns (0, false),
	// Eval degrades to UNKNOWN per SQL 3VL.
	if got := c.Eval("5"); got != TriUnknown {
		t.Fatalf("type mismatch: got %v", got)
	}
}

func TestComparison_Eval_Strings(t *testing.T) {
	t.Parallel()
	c := Comparison{Type: ComparisonLessThan, Operand: "b"}
	if got := c.Eval("a"); got != TriTrue {
		t.Fatalf("a < b: got %v", got)
	}
	if got := c.Eval("c"); got != TriFalse {
		t.Fatalf("c < b: got %v", got)
	}
}

func TestComparisonPredicate_EndToEnd(t *testing.T) {
	t.Parallel()
	// Predicate: field `age >= 18` against a row represented as a
	// map. FieldValue.Evaluate resolves the column; Value.Evaluate
	// now drives the predicate — no more closure seam.
	operand := &FieldValue{Field: "age", Typ: TypeInt}
	cmp := Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)}
	pred := NewComparisonPredicate(operand, cmp)

	row := map[string]any{"age": int64(21)}
	if got := pred.Eval(row); got != TriTrue {
		t.Fatalf("age=21 >= 18: got %v", got)
	}
	row["age"] = int64(15)
	if got := pred.Eval(row); got != TriFalse {
		t.Fatalf("age=15 >= 18: got %v", got)
	}
	row["age"] = nil
	if got := pred.Eval(row); got != TriUnknown {
		t.Fatalf("age=NULL >= 18: got %v", got)
	}

	if got := pred.Explain(); got != "age >= 18" {
		t.Fatalf("Explain: got %q", got)
	}
}

func TestComparisonPredicate_NilOperand(t *testing.T) {
	t.Parallel()
	pred := &ComparisonPredicate{
		// No Operand set — Eval degrades to UNKNOWN.
		Comparison: Comparison{Type: ComparisonEquals, Operand: int64(1)},
	}
	if got := pred.Eval(nil); got != TriUnknown {
		t.Fatalf("nil Operand: got %v", got)
	}
}

func TestComparisonPredicate_ComposesWithKleeneConnectives(t *testing.T) {
	t.Parallel()
	row := map[string]any{"age": int64(21), "rank": int64(3)}

	// (age >= 18) AND (rank < 5)
	tree := NewAnd(
		NewComparisonPredicate(&FieldValue{Field: "age", Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)}),
		NewComparisonPredicate(&FieldValue{Field: "rank", Typ: TypeInt},
			Comparison{Type: ComparisonLessThan, Operand: int64(5)}),
	)
	if got := tree.Eval(row); got != TriTrue {
		t.Fatalf("AND: got %v", got)
	}
	row["rank"] = int64(7)
	if got := tree.Eval(row); got != TriFalse {
		t.Fatalf("AND with rank=7: got %v", got)
	}
}

// ComparisonPredicate's operand can be an ArithmeticValue —
// exercises Value.Evaluate recursion.
func TestComparisonPredicate_ArithmeticOperand(t *testing.T) {
	t.Parallel()
	// (a + b) > 10
	sum := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "a", Typ: TypeInt},
		Right: &FieldValue{Field: "b", Typ: TypeInt},
	}
	pred := NewComparisonPredicate(sum,
		Comparison{Type: ComparisonGreaterThan, Operand: int64(10)})

	if got := pred.Eval(map[string]any{"a": int64(5), "b": int64(7)}); got != TriTrue {
		t.Fatalf("5+7=12 > 10: got %v", got)
	}
	if got := pred.Eval(map[string]any{"a": int64(3), "b": int64(4)}); got != TriFalse {
		t.Fatalf("3+4=7 > 10: got %v", got)
	}
	// NULL propagation: a=NULL -> a+b=NULL -> UNKNOWN.
	if got := pred.Eval(map[string]any{"a": nil, "b": int64(1)}); got != TriUnknown {
		t.Fatalf("a=NULL: got %v", got)
	}
}
