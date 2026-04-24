package cascades

import "testing"

// Static interface assertions for every concrete predicate.
var (
	_ QueryPredicate = (*ConstantPredicate)(nil)
	_ QueryPredicate = (*AndPredicate)(nil)
	_ QueryPredicate = (*OrPredicate)(nil)
	_ QueryPredicate = (*NotPredicate)(nil)
	_ QueryPredicate = (*ValuePredicate)(nil)
)

func TestValuePredicate(t *testing.T) {
	t.Parallel()
	// Bare bool literal: TRUE → TriTrue.
	p := NewValuePredicate(NewBooleanValue(true))
	if got := p.Eval(nil); got != TriTrue {
		t.Fatalf("bool(true): got %v", got)
	}
	// FALSE → TriFalse.
	p = NewValuePredicate(NewBooleanValue(false))
	if got := p.Eval(nil); got != TriFalse {
		t.Fatalf("bool(false): got %v", got)
	}
	// NULL boolean literal → TriUnknown.
	p = NewValuePredicate(&BooleanValue{Value: nil})
	if got := p.Eval(nil); got != TriUnknown {
		t.Fatalf("NULL bool: got %v", got)
	}
	// Non-boolean Value → TriUnknown (safety net against analyzer gaps).
	p = NewValuePredicate(&ConstantValue{Value: int64(1), Typ: TypeInt})
	if got := p.Eval(nil); got != TriUnknown {
		t.Fatalf("int literal: got %v", got)
	}
	// Nil Value in the predicate itself.
	if got := (&ValuePredicate{}).Eval(nil); got != TriUnknown {
		t.Fatalf("nil-Value predicate: got %v", got)
	}
	// Explain renders the Value's name.
	p = NewValuePredicate(&FieldValue{Field: "is_active", Typ: TypeBool})
	if got := p.Explain(); got != "field" {
		t.Fatalf("Explain: got %q", got)
	}
}

func TestConstantPredicate(t *testing.T) {
	t.Parallel()
	if v := NewConstantPredicate(TriTrue).Eval(nil); v != TriTrue {
		t.Fatalf("TRUE const: got %v", v)
	}
	if v := NewConstantPredicate(TriFalse).Eval(nil); v != TriFalse {
		t.Fatalf("FALSE const: got %v", v)
	}
	if v := NewConstantPredicate(TriUnknown).Eval(nil); v != TriUnknown {
		t.Fatalf("UNKNOWN const: got %v", v)
	}
	if got := NewConstantPredicate(TriTrue).Explain(); got != "TRUE" {
		t.Fatalf("Explain TRUE: got %q", got)
	}
	if got := NewConstantPredicate(TriFalse).Explain(); got != "FALSE" {
		t.Fatalf("Explain FALSE: got %q", got)
	}
	if got := NewConstantPredicate(TriUnknown).Explain(); got != "UNKNOWN" {
		t.Fatalf("Explain UNKNOWN: got %q", got)
	}
}

// Kleene AND truth table.
func TestAnd_Kleene(t *testing.T) {
	t.Parallel()
	T := NewConstantPredicate(TriTrue)
	F := NewConstantPredicate(TriFalse)
	U := NewConstantPredicate(TriUnknown)

	cases := []struct {
		name string
		in   []QueryPredicate
		want TriBool
	}{
		{"empty → TRUE", nil, TriTrue},
		{"T", []QueryPredicate{T}, TriTrue},
		{"F", []QueryPredicate{F}, TriFalse},
		{"U", []QueryPredicate{U}, TriUnknown},
		{"T AND T", []QueryPredicate{T, T}, TriTrue},
		{"T AND F", []QueryPredicate{T, F}, TriFalse},
		{"T AND U", []QueryPredicate{T, U}, TriUnknown},
		{"F AND U", []QueryPredicate{F, U}, TriFalse}, // short-circuit
		{"U AND F", []QueryPredicate{U, F}, TriFalse},
		{"U AND U", []QueryPredicate{U, U}, TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NewAnd(tc.in...).Eval(nil)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Kleene OR truth table.
func TestOr_Kleene(t *testing.T) {
	t.Parallel()
	T := NewConstantPredicate(TriTrue)
	F := NewConstantPredicate(TriFalse)
	U := NewConstantPredicate(TriUnknown)

	cases := []struct {
		name string
		in   []QueryPredicate
		want TriBool
	}{
		{"empty → FALSE", nil, TriFalse},
		{"T", []QueryPredicate{T}, TriTrue},
		{"F", []QueryPredicate{F}, TriFalse},
		{"U", []QueryPredicate{U}, TriUnknown},
		{"F OR T", []QueryPredicate{F, T}, TriTrue}, // short-circuit
		{"F OR F", []QueryPredicate{F, F}, TriFalse},
		{"F OR U", []QueryPredicate{F, U}, TriUnknown},
		{"U OR T", []QueryPredicate{U, T}, TriTrue},
		{"U OR F", []QueryPredicate{U, F}, TriUnknown},
		{"U OR U", []QueryPredicate{U, U}, TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NewOr(tc.in...).Eval(nil)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Kleene NOT truth table: NOT TRUE = FALSE, NOT FALSE = TRUE,
// NOT UNKNOWN = UNKNOWN.
func TestNot_Kleene(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want TriBool
	}{
		{TriTrue, TriFalse},
		{TriFalse, TriTrue},
		{TriUnknown, TriUnknown},
	}
	for _, tc := range cases {
		got := NewNot(NewConstantPredicate(tc.in)).Eval(nil)
		if got != tc.want {
			t.Fatalf("NOT %v: got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Compose a realistic Kleene tree: NOT (T AND (F OR U)) → NOT (T
// AND U) → NOT U → UNKNOWN.
func TestPredicate_Composition(t *testing.T) {
	t.Parallel()
	T := NewConstantPredicate(TriTrue)
	F := NewConstantPredicate(TriFalse)
	U := NewConstantPredicate(TriUnknown)
	tree := NewNot(NewAnd(T, NewOr(F, U)))
	if got := tree.Eval(nil); got != TriUnknown {
		t.Fatalf("composition: got %v", got)
	}
	// Explain output is readable.
	want := "NOT (TRUE AND (FALSE OR UNKNOWN))"
	if got := tree.Explain(); got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}

// WalkPredicate pre-order traversal; skip-subtree on false.
func TestWalkPredicate(t *testing.T) {
	t.Parallel()
	// Tree: AND(NOT(TRUE), OR(FALSE, UNKNOWN))
	tree := NewAnd(
		NewNot(NewConstantPredicate(TriTrue)),
		NewOr(NewConstantPredicate(TriFalse), NewConstantPredicate(TriUnknown)),
	)
	// Visit all — expected count: AND + NOT + TRUE + OR + FALSE + UNKNOWN = 6.
	count := 0
	WalkPredicate(tree, func(QueryPredicate) bool {
		count++
		return true
	})
	if count != 6 {
		t.Fatalf("visit all: expected 6, got %d", count)
	}
	// Skip-subtree: return false from the OR node, its children should
	// be skipped — count 4 (AND, NOT, TRUE, OR).
	count = 0
	WalkPredicate(tree, func(p QueryPredicate) bool {
		count++
		_, isOr := p.(*OrPredicate)
		return !isOr
	})
	if count != 4 {
		t.Fatalf("skip OR subtree: expected 4, got %d", count)
	}
	// Nil-safe.
	WalkPredicate(nil, func(QueryPredicate) bool {
		t.Fatal("should not visit on nil")
		return true
	})
}

// AsConstant / PredicateSize helpers.
func TestAsConstant(t *testing.T) {
	t.Parallel()
	if v, ok := AsConstant(NewConstantPredicate(TriTrue)); !ok || v != TriTrue {
		t.Fatalf("const TRUE: got (%v, %v)", v, ok)
	}
	if _, ok := AsConstant(NewAnd(NewConstantPredicate(TriTrue))); ok {
		t.Fatal("AND: should not be a constant")
	}
	if _, ok := AsConstant(nil); ok {
		t.Fatal("nil: should not be a constant")
	}
}

func TestPredicateSize(t *testing.T) {
	t.Parallel()
	if n := PredicateSize(nil); n != 0 {
		t.Fatalf("nil: got %d", n)
	}
	if n := PredicateSize(NewConstantPredicate(TriTrue)); n != 1 {
		t.Fatalf("leaf: got %d, want 1", n)
	}
	// NOT over a leaf: 2 nodes.
	if n := PredicateSize(NewNot(NewConstantPredicate(TriTrue))); n != 2 {
		t.Fatalf("NOT leaf: got %d, want 2", n)
	}
	// AND(a, b, c) → 4 nodes (AND + 3 leaves)
	and := NewAnd(
		NewConstantPredicate(TriTrue),
		NewConstantPredicate(TriFalse),
		NewConstantPredicate(TriUnknown),
	)
	if n := PredicateSize(and); n != 4 {
		t.Fatalf("AND(a,b,c): got %d, want 4", n)
	}
	// NOT (AND(a, b)) → 4 nodes
	tree := NewNot(NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse)))
	if n := PredicateSize(tree); n != 4 {
		t.Fatalf("NOT(AND(a,b)): got %d, want 4", n)
	}
}

// PredicateEquals: structural-equality across predicate shapes.
func TestPredicateEquals(t *testing.T) {
	t.Parallel()
	// Constant equality
	if !PredicateEquals(NewConstantPredicate(TriTrue), NewConstantPredicate(TriTrue)) {
		t.Fatal("TRUE == TRUE should be true")
	}
	if PredicateEquals(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse)) {
		t.Fatal("TRUE != FALSE")
	}
	// Type mismatch: ConstantPredicate vs AndPredicate.
	if PredicateEquals(NewConstantPredicate(TriTrue),
		NewAnd(NewConstantPredicate(TriTrue))) {
		t.Fatal("const != and")
	}
	// AndPredicate structural
	a := NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse))
	b := NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse))
	if !PredicateEquals(a, b) {
		t.Fatal("structurally identical AND should be equal")
	}
	// AndPredicate different children.
	c := NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriTrue))
	if PredicateEquals(a, c) {
		t.Fatal("different AND children should not be equal")
	}
	// NotPredicate inner match
	n1 := NewNot(NewConstantPredicate(TriTrue))
	n2 := NewNot(NewConstantPredicate(TriTrue))
	if !PredicateEquals(n1, n2) {
		t.Fatal("NOT TRUE should equal NOT TRUE")
	}
	// ComparisonPredicate structural (same operand name + same op + same literal)
	c1 := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: int64(5)},
	)
	c2 := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: int64(5)},
	)
	if !PredicateEquals(c1, c2) {
		t.Fatal("same comparison should be equal")
	}
	// Different op.
	c3 := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonLessThan, Operand: int64(5)},
	)
	if PredicateEquals(c1, c3) {
		t.Fatal("different ops should not be equal")
	}
	// nil
	if !PredicateEquals(nil, nil) {
		t.Fatal("nil == nil should be true")
	}
	if PredicateEquals(nil, NewConstantPredicate(TriTrue)) {
		t.Fatal("nil != predicate")
	}
}

// Children walks: a structural visitor (future simplification
// rules) relies on Children() for recursion. Pin it here.
func TestChildren_Walk(t *testing.T) {
	t.Parallel()
	leaf := NewConstantPredicate(TriTrue)
	if len(leaf.Children()) != 0 {
		t.Fatalf("leaf: expected 0 children")
	}
	and := NewAnd(leaf, leaf, leaf)
	if got := and.Children(); len(got) != 3 {
		t.Fatalf("AND children: expected 3, got %d", len(got))
	}
	not := NewNot(leaf)
	if got := not.Children(); len(got) != 1 {
		t.Fatalf("NOT children: expected 1, got %d", len(got))
	}
}
