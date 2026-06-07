package predicates

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

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
	op := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
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
	cv, ok := got.Operand.(*values.ConstantValue)
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
	rhs := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	pred := &ComparisonPredicate{
		Operand: &values.FieldValue{Field: "NAME", Typ: values.TypeInt},
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
	cv, ok := got.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected RHS folded to *ConstantValue, got %T", got.Comparison.Operand)
	}
	if cv.Value.(int64) != 3 {
		t.Fatalf("expected RHS=3, got %v", cv.Value)
	}
	// LHS untouched: still a FieldValue.
	if _, isField := got.Operand.(*values.FieldValue); !isField {
		t.Fatalf("expected LHS to remain a FieldValue, got %T", got.Operand)
	}
}

// TestSimplifyPredicateValues_AndRecurses pins the connective recursion:
// `(name = 1+2) AND (id = 3*4)` folds both leaves.
func TestSimplifyPredicateValues_AndRecurses(t *testing.T) {
	t.Parallel()
	pred := &AndPredicate{SubPredicates: []QueryPredicate{
		&ComparisonPredicate{
			Operand: &values.FieldValue{Field: "NAME"},
			Comparison: Comparison{
				Type: ComparisonEquals,
				Operand: &values.ArithmeticValue{
					Op:    values.OpAdd,
					Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
					Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
				},
			},
		},
		&ComparisonPredicate{
			Operand: &values.FieldValue{Field: "ID"},
			Comparison: Comparison{
				Type: ComparisonEquals,
				Operand: &values.ArithmeticValue{
					Op:    values.OpMul,
					Left:  &values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
					Right: &values.ConstantValue{Value: int64(4), Typ: values.TypeInt},
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
		cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
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
		Operand: &values.FieldValue{Field: "ID"},
		Comparison: Comparison{
			Type: ComparisonEquals,
			Operand: &values.ArithmeticValue{
				Op:    values.OpAdd,
				Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
				Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
			},
		},
	}
	out := SimplifyPredicateValues(&NotPredicate{Child: inner})
	notPred, ok := out.(*NotPredicate)
	if !ok {
		t.Fatalf("expected *NotPredicate, got %T", out)
	}
	cp := notPred.Child.(*ComparisonPredicate)
	cv := cp.Comparison.Operand.(*values.ConstantValue)
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
		Operand:    &values.FieldValue{Field: "ID"},
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
		Value: &values.ScalarFunctionValue{
			FuncName: "UPPER",
			Args:     []values.Value{&values.ConstantValue{Value: "hi", Typ: values.TypeString}},
			Typ:      values.TypeString,
		},
	}
	out := SimplifyPredicateValues(pred)
	vp, ok := out.(*ValuePredicate)
	if !ok {
		t.Fatalf("expected *ValuePredicate, got %T", out)
	}
	cv, ok := vp.Value.(*values.ConstantValue)
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

// TestSimplifyPredicateValues_OrRecurses pins that the Or branch
// recurses through children — symmetric to AndRecurses. The And and
// Or branches are independent code paths; one being correct doesn't
// imply the other is.
func TestSimplifyPredicateValues_OrRecurses(t *testing.T) {
	t.Parallel()
	op := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	left := &ComparisonPredicate{
		Operand:    op,
		Comparison: NewLiteralComparison(ComparisonEquals, int64(5)),
	}
	right := &ValuePredicate{Value: &values.FieldValue{Field: "x", Typ: values.TypeBool}}

	or := &OrPredicate{SubPredicates: []QueryPredicate{left, right}}
	out := SimplifyPredicateValues(or)

	got, ok := out.(*OrPredicate)
	if !ok {
		t.Fatalf("expected *OrPredicate, got %T", out)
	}
	if got == or {
		t.Fatal("expected fresh OrPredicate after fold (Or branch returned receiver)")
	}
	cp, ok := got.SubPredicates[0].(*ComparisonPredicate)
	if !ok {
		t.Fatalf("first child should be *ComparisonPredicate, got %T", got.SubPredicates[0])
	}
	cv, ok := cp.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("first child operand should fold to constant, got %T", cp.Operand)
	}
	if cv.Value.(int64) != 3 {
		t.Fatalf("expected folded constant=3, got %v", cv.Value)
	}
	if got.SubPredicates[1] != right {
		t.Fatal("second child should be unchanged (no fold to do)")
	}
}

// TestSimplifyPredicateValues_OrPointerStableWhenNoFold pins the
// "nothing changed → return receiver" optimisation for the Or branch
// (pinned for And in PointerStableWhenNoFold).
func TestSimplifyPredicateValues_OrPointerStableWhenNoFold(t *testing.T) {
	t.Parallel()
	leaf1 := &ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "x", Typ: values.TypeInt},
		Comparison: NewLiteralComparison(ComparisonEquals, int64(1)),
	}
	leaf2 := &ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "y", Typ: values.TypeInt},
		Comparison: NewLiteralComparison(ComparisonGreaterThan, int64(0)),
	}
	or := &OrPredicate{SubPredicates: []QueryPredicate{leaf1, leaf2}}
	if got := SimplifyPredicateValues(or); got != or {
		t.Fatalf("expected receiver pointer when nothing folds, got fresh alloc")
	}
}

// TestSimplifyPredicateValues_ComparisonPreservesEscape pins the easy-
// to-drop bug: Escape rune must survive the rebuild that happens when
// a fold changes the operand. LIKE … ESCAPE '\\' must NOT lose the
// escape rune.
func TestSimplifyPredicateValues_ComparisonPreservesEscape(t *testing.T) {
	t.Parallel()
	op := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: "foo", Typ: values.TypeString},
		Right: &values.ConstantValue{Value: "bar", Typ: values.TypeString},
	}
	pred := &ComparisonPredicate{
		Operand: op,
		Comparison: Comparison{
			Type:    ComparisonLike,
			Operand: values.LiteralValue("foo\\%"),
			Escape:  '\\',
		},
	}
	out := SimplifyPredicateValues(pred)
	got, ok := out.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", out)
	}
	if got.Comparison.Escape != '\\' {
		t.Fatalf("Escape rune dropped during fold: got %q, want '\\\\'", got.Comparison.Escape)
	}
	if got.Comparison.Type != ComparisonLike {
		t.Fatalf("ComparisonType dropped: got %v, want ComparisonLike", got.Comparison.Type)
	}
}

// TestSimplifyPredicateValues_UnknownPredicateShape: foreign predicate
// types (anything outside the switch) pass through unchanged. Pins
// the "no panic on extension types" contract documented at the
// bottom of the implementation.
func TestSimplifyPredicateValues_UnknownPredicateShape(t *testing.T) {
	t.Parallel()
	// A test-local predicate type the simplifier has never seen.
	p := &fakePred{}
	if got := SimplifyPredicateValues(p); got != p {
		t.Fatalf("unknown predicate type should pass through unchanged, got %T", got)
	}
}

// TestSimplifyPredicateValues_DeeplyNested: AND(OR(NOT(p))) over a
// foldable comparison must thread through all three connectives.
// One stale pointer-equality short-circuit anywhere in the tree
// would surface here.
func TestSimplifyPredicateValues_DeeplyNested(t *testing.T) {
	t.Parallel()
	op := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
	}
	leaf := &ComparisonPredicate{
		Operand:    op,
		Comparison: NewLiteralComparison(ComparisonEquals, int64(5)),
	}
	tree := &AndPredicate{SubPredicates: []QueryPredicate{
		&OrPredicate{SubPredicates: []QueryPredicate{
			&NotPredicate{Child: leaf},
		}},
	}}
	out := SimplifyPredicateValues(tree).(*AndPredicate)
	or := out.SubPredicates[0].(*OrPredicate)
	not := or.SubPredicates[0].(*NotPredicate)
	cp := not.Child.(*ComparisonPredicate)
	if _, ok := cp.Operand.(*values.ConstantValue); !ok {
		t.Fatalf("deeply nested fold did not reach the leaf: operand is %T", cp.Operand)
	}
}

// fakePred is a test-only QueryPredicate the simplifier has no case
// for. Used by UnknownPredicateShape.
type fakePred struct{}

func (*fakePred) Children() []QueryPredicate                          { return nil }
func (*fakePred) Eval(any) TriBool                                    { return TriUnknown }
func (*fakePred) EvalErr(any) (TriBool, error)                        { return TriUnknown, nil }
func (*fakePred) Explain() string                                     { return "fakePred" }
func (*fakePred) GetCorrelatedTo() map[CorrelationIdentifier]struct{} { return nil }
