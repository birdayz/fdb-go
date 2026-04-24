package cascades

import "testing"

// Static interface assertions for every concrete predicate.
var (
	_ QueryPredicate = (*ConstantPredicate)(nil)
	_ QueryPredicate = (*AndPredicate)(nil)
	_ QueryPredicate = (*OrPredicate)(nil)
	_ QueryPredicate = (*NotPredicate)(nil)
)

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
