package cascades

import "testing"

// TestSimplifyPredicateValues_NilSafe pins the nil short-circuit:
// `SimplifyPredicateValues(nil)` returns nil rather than panicking.
func TestSimplifyPredicateValues_NilSafe(t *testing.T) {
	t.Parallel()
	if SimplifyPredicateValues(nil) != nil {
		t.Fatal("expected nil")
	}
}

// TestSimplifyPredicateValues_ComparisonOperandFold pins folding of the
// LHS Value: `(1+2) = field` collapses the LHS arithmetic to constant 3.
func TestSimplifyPredicateValues_ComparisonOperandFold(t *testing.T) {
	t.Parallel()
	op := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
	}
	pred := &ComparisonPredicate{
		Operand:    op,
		Comparison: NewLiteralComparison(ComparisonEquals, int64(5)),
	}
	out := SimplifyPredicateValues(pred)
	got, ok := out.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", out)
	}
	cv, ok := got.Operand.(*ConstantValue)
	if !ok {
		t.Fatalf("expected operand folded to *ConstantValue, got %T", got.Operand)
	}
	if cv.Value.(int64) != 3 {
		t.Fatalf("expected operand=3, got %v", cv.Value)
	}
}

// TestSimplifyPredicateValues_ComparisonRHSFold pins folding of the RHS:
// `name = (1+2)` collapses to `name = 3`.
func TestSimplifyPredicateValues_ComparisonRHSFold(t *testing.T) {
	t.Parallel()
	rhs := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
	}
	pred := &ComparisonPredicate{
		Operand: &FieldValue{Field: "NAME", Typ: TypeInt},
		Comparison: Comparison{
			Type:    ComparisonEquals,
			Operand: rhs,
		},
	}
	out := SimplifyPredicateValues(pred)
	got, ok := out.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", out)
	}
	cv, ok := got.Comparison.Operand.(*ConstantValue)
	if !ok {
		t.Fatalf("expected RHS folded to *ConstantValue, got %T", got.Comparison.Operand)
	}
	if cv.Value.(int64) != 3 {
		t.Fatalf("expected RHS=3, got %v", cv.Value)
	}
	// LHS untouched: still a FieldValue.
	if _, isField := got.Operand.(*FieldValue); !isField {
		t.Fatalf("expected LHS to remain a FieldValue, got %T", got.Operand)
	}
}

// TestSimplifyPredicateValues_AndRecurses pins the connective recursion:
// `(name = 1+2) AND (id = 3*4)` folds both leaves.
func TestSimplifyPredicateValues_AndRecurses(t *testing.T) {
	t.Parallel()
	pred := &AndPredicate{SubPredicates: []QueryPredicate{
		&ComparisonPredicate{
			Operand: &FieldValue{Field: "NAME"},
			Comparison: Comparison{
				Type: ComparisonEquals,
				Operand: &ArithmeticValue{
					Op:    OpAdd,
					Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
					Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
				},
			},
		},
		&ComparisonPredicate{
			Operand: &FieldValue{Field: "ID"},
			Comparison: Comparison{
				Type: ComparisonEquals,
				Operand: &ArithmeticValue{
					Op:    OpMul,
					Left:  &ConstantValue{Value: int64(3), Typ: TypeInt},
					Right: &ConstantValue{Value: int64(4), Typ: TypeInt},
				},
			},
		},
	}}
	out := SimplifyPredicateValues(pred)
	and, ok := out.(*AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", out)
	}
	for i, sp := range and.SubPredicates {
		cp, ok := sp.(*ComparisonPredicate)
		if !ok {
			t.Fatalf("sub %d: expected *ComparisonPredicate, got %T", i, sp)
		}
		cv, ok := cp.Comparison.Operand.(*ConstantValue)
		if !ok {
			t.Fatalf("sub %d: RHS not folded, got %T", i, cp.Comparison.Operand)
		}
		want := []int64{3, 12}[i]
		if cv.Value.(int64) != want {
			t.Fatalf("sub %d: expected %d, got %v", i, want, cv.Value)
		}
	}
}

// TestSimplifyPredicateValues_NotRecurses pins recursion through NOT.
func TestSimplifyPredicateValues_NotRecurses(t *testing.T) {
	t.Parallel()
	inner := &ComparisonPredicate{
		Operand: &FieldValue{Field: "ID"},
		Comparison: Comparison{
			Type: ComparisonEquals,
			Operand: &ArithmeticValue{
				Op:    OpAdd,
				Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
				Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
			},
		},
	}
	out := SimplifyPredicateValues(&NotPredicate{Child: inner})
	notPred, ok := out.(*NotPredicate)
	if !ok {
		t.Fatalf("expected *NotPredicate, got %T", out)
	}
	cp := notPred.Child.(*ComparisonPredicate)
	cv := cp.Comparison.Operand.(*ConstantValue)
	if cv.Value.(int64) != 3 {
		t.Fatalf("expected 3, got %v", cv.Value)
	}
}

// TestSimplifyPredicateValues_PointerStableWhenNoFold pins that an
// already-simple predicate returns its input pointer verbatim — no
// reallocation when nothing folds. Lets callers cheaply detect "did
// anything happen?" via pointer comparison.
func TestSimplifyPredicateValues_PointerStableWhenNoFold(t *testing.T) {
	t.Parallel()
	pred := &ComparisonPredicate{
		Operand:    &FieldValue{Field: "ID"},
		Comparison: NewLiteralComparison(ComparisonEquals, int64(5)),
	}
	if SimplifyPredicateValues(pred) != pred {
		t.Fatal("expected same pointer when no fold")
	}
	and := &AndPredicate{SubPredicates: []QueryPredicate{pred, pred}}
	if SimplifyPredicateValues(and) != and {
		t.Fatal("expected same pointer when no fold")
	}
}

// TestSimplifyPredicateValues_ValuePredicateFolds pins the bare-Value
// predicate arm: `WHERE UPPER('a') = 'A'` lifted as a ValuePredicate
// would fold the inner UPPER call to "A" (synthetic, but pins the
// recursion path).
func TestSimplifyPredicateValues_ValuePredicateFolds(t *testing.T) {
	t.Parallel()
	pred := &ValuePredicate{
		Value: &ScalarFunctionValue{
			FuncName: "UPPER",
			Args:     []Value{&ConstantValue{Value: "hi", Typ: TypeString}},
			Typ:      TypeString,
		},
	}
	out := SimplifyPredicateValues(pred)
	vp, ok := out.(*ValuePredicate)
	if !ok {
		t.Fatalf("expected *ValuePredicate, got %T", out)
	}
	cv, ok := vp.Value.(*ConstantValue)
	if !ok {
		t.Fatalf("expected folded ConstantValue, got %T", vp.Value)
	}
	if cv.Value.(string) != "HI" {
		t.Fatalf("expected HI, got %v", cv.Value)
	}
}

// TestSimplifyPredicateValues_ConstantPredicateUnchanged pins that
// ConstantPredicate (no Value operands) passes through verbatim.
func TestSimplifyPredicateValues_ConstantPredicateUnchanged(t *testing.T) {
	t.Parallel()
	cp := &ConstantPredicate{Value: TriTrue}
	if SimplifyPredicateValues(cp) != cp {
		t.Fatal("expected same pointer for ConstantPredicate")
	}
}
