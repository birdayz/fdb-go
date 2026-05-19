package predicates

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

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
	p := NewValuePredicate(values.NewBooleanValue(true))
	if got := p.Eval(nil); got != TriTrue {
		t.Fatalf("bool(true): got %v", got)
	}
	// FALSE → TriFalse.
	p = NewValuePredicate(values.NewBooleanValue(false))
	if got := p.Eval(nil); got != TriFalse {
		t.Fatalf("bool(false): got %v", got)
	}
	// NULL boolean literal → TriUnknown.
	p = NewValuePredicate(&values.BooleanValue{Value: nil})
	if got := p.Eval(nil); got != TriUnknown {
		t.Fatalf("NULL bool: got %v", got)
	}
	// Non-boolean Value → TriUnknown (safety net against analyzer gaps).
	p = NewValuePredicate(&values.ConstantValue{Value: int64(1), Typ: values.TypeInt})
	if got := p.Eval(nil); got != TriUnknown {
		t.Fatalf("int literal: got %v", got)
	}
	// Nil Value in the predicate itself.
	if got := (&ValuePredicate{}).Eval(nil); got != TriUnknown {
		t.Fatalf("nil-Value predicate: got %v", got)
	}
	// Explain renders the Value's per-instance form via ExplainValue
	// — FieldValue produces its column name, not the kind string.
	p = NewValuePredicate(&values.FieldValue{Field: "is_active", Typ: values.TypeBool})
	if got := p.Explain(); got != "is_active" {
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

// NotPredicate.Explain wraps non-connective children in parens so
// the SQL-like output is unambiguous. AndPredicate / OrPredicate
// already wrap themselves; avoid double-parenthesizing.
func TestNotPredicate_ExplainParens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   *NotPredicate
		want string
	}{
		{
			name: "NOT(AndPredicate) — no double parens",
			in:   NewNot(NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse))),
			want: "NOT (TRUE AND FALSE)",
		},
		{
			name: "NOT(OrPredicate) — no double parens",
			in:   NewNot(NewOr(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse))),
			want: "NOT (TRUE OR FALSE)",
		},
		{
			name: "NOT(ComparisonPredicate) — wraps",
			in: NewNot(NewComparisonPredicate(
				&values.FieldValue{Field: "age", Typ: values.TypeInt},
				Comparison{Type: ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
			)),
			want: "NOT (age >= 18)",
		},
		{
			name: "NOT(ConstantPredicate) — wraps",
			in:   NewNot(NewConstantPredicate(TriTrue)),
			want: "NOT (TRUE)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.Explain(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
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

// TestAndPredicate_Explain_Empty / TestOrPredicate_Explain_Empty pin
// the identity-element rendering for empty connectives:
// And{} → "TRUE" (AND identity), Or{} → "FALSE" (OR identity).
// These shapes appear when the simplifier drops every child of a
// connective and the calling code rebuilds with an empty subpred
// list (vs returning a concrete ConstantPredicate).
func TestAndPredicate_Explain_Empty(t *testing.T) {
	t.Parallel()
	if got := (&AndPredicate{}).Explain(); got != "TRUE" {
		t.Fatalf("And{}: got %q, want TRUE", got)
	}
}

func TestOrPredicate_Explain_Empty(t *testing.T) {
	t.Parallel()
	if got := (&OrPredicate{}).Explain(); got != "FALSE" {
		t.Fatalf("Or{}: got %q, want FALSE", got)
	}
}

// TestValuePredicate_Children pins the leaf-shape contract:
// ValuePredicate is a leaf, so Children returns an empty (non-nil)
// slice. Walker code paths range over Children() and rely on the
// non-nil contract.
func TestValuePredicate_Children(t *testing.T) {
	t.Parallel()
	got := (&ValuePredicate{Value: &values.FieldValue{Field: "x", Typ: values.TypeBool}}).Children()
	if got == nil {
		t.Fatal("Children should be non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("Children should be empty for leaf, got %d entries", len(got))
	}
}

// TestValuePredicate_Explain_NilValue pins the defensive branch:
// ValuePredicate{Value: nil}.Explain returns "<nil-value>" rather
// than panicking. Plan-tree rendering must stay total — a malformed
// predicate has to render to *something* so the explain output isn't
// truncated mid-tree.
func TestValuePredicate_Explain_NilValue(t *testing.T) {
	t.Parallel()
	vp := &ValuePredicate{Value: nil}
	if got := vp.Explain(); got != "<nil-value>" {
		t.Fatalf("ValuePredicate{Value:nil}.Explain() = %q, want \"<nil-value>\"", got)
	}
}

// TestValueNamesEqual_NilSafety pins the both-nil-equal / one-nil-not-
// equal contract used by PredicateEquals across nil Value fields.
// Reaches valueNamesEqual via PredicateEquals on ValuePredicate.
func TestValueNamesEqual_NilSafety(t *testing.T) {
	t.Parallel()
	withNil := &ValuePredicate{Value: nil}
	alsoNil := &ValuePredicate{Value: nil}
	withVal := &ValuePredicate{Value: &values.FieldValue{Field: "x", Typ: values.TypeInt}}

	if !PredicateEquals(withNil, alsoNil) {
		t.Fatal("two ValuePredicate{Value:nil} should be equal")
	}
	if PredicateEquals(withNil, withVal) {
		t.Fatal("nil-Value vs non-nil-Value ValuePredicate should NOT be equal")
	}
	if PredicateEquals(withVal, withNil) {
		t.Fatal("symmetric: non-nil-Value vs nil-Value ValuePredicate should NOT be equal")
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
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(5))},
	)
	c2 := NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(5))},
	)
	if !PredicateEquals(c1, c2) {
		t.Fatal("same comparison should be equal")
	}
	// Different op.
	c3 := NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		Comparison{Type: ComparisonLessThan, Operand: values.LiteralValue(int64(5))},
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

// PredicateEquals must distinguish ComparisonPredicates on different
// fields. Before the ExplainValue-based valueNamesEqual fix, two
// FieldValues would compare equal by Name() alone (both return "field"
// regardless of Field string), and AndDedup would incorrectly drop
// predicates like `age = 5 AND rank = 5` as duplicates.
func TestPredicateEquals_DifferentFieldsAreNotEqual(t *testing.T) {
	t.Parallel()
	age := NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(5))},
	)
	rank := NewComparisonPredicate(
		&values.FieldValue{Field: "rank", Typ: values.TypeInt},
		Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(5))},
	)
	if PredicateEquals(age, rank) {
		t.Fatal("age=5 and rank=5 should NOT be equal — different fields")
	}
	age2 := NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(5))},
	)
	if !PredicateEquals(age, age2) {
		t.Fatal("two identical age=5 predicates should be equal")
	}
	age10 := NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(10))},
	)
	if PredicateEquals(age, age10) {
		t.Fatal("age=5 and age=10 should NOT be equal — different literals")
	}
}

// ValuePredicate on different fields must also differ.
func TestPredicateEquals_ValuePredicateDifferentFields(t *testing.T) {
	t.Parallel()
	a := NewValuePredicate(&values.FieldValue{Field: "is_active", Typ: values.TypeBool})
	b := NewValuePredicate(&values.FieldValue{Field: "is_pending", Typ: values.TypeBool})
	if PredicateEquals(a, b) {
		t.Fatal("ValuePredicate on different fields should NOT be equal")
	}
	c := NewValuePredicate(&values.FieldValue{Field: "is_active", Typ: values.TypeBool})
	if !PredicateEquals(a, c) {
		t.Fatal("ValuePredicate on same field should be equal")
	}
}

// PredicateEquals must handle IN's slice-valued Operand without
// panicking. Before the reflect.DeepEqual fix, comparing two IN
// ComparisonPredicates crashed with "comparing uncomparable type".
func TestPredicateEquals_ComparisonInOperand(t *testing.T) {
	t.Parallel()
	field := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	pIn1 := NewComparisonPredicate(field, Comparison{
		Type: ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2), int64(3)}),
	})
	pIn2 := NewComparisonPredicate(field, Comparison{
		Type: ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2), int64(3)}),
	})
	pIn3 := NewComparisonPredicate(field, Comparison{
		Type: ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2)}),
	})
	if !PredicateEquals(pIn1, pIn2) {
		t.Fatal("same IN lists should be equal")
	}
	if PredicateEquals(pIn1, pIn3) {
		t.Fatal("different IN lists should be unequal")
	}
}

// PredicateEquals on unary comparisons (IS NULL / IS NOT NULL)
// must ignore the Operand field — `IS NULL{Operand: nil}` and
// `IS NULL{Operand: LiteralValue(nil)}` are semantically identical
// (Eval skips Operand entirely on unary types) and must compare
// equal even though their structural Operand differs.
func TestPredicateEquals_UnaryIgnoresOperand(t *testing.T) {
	t.Parallel()
	field := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	nilOp := NewComparisonPredicate(field, Comparison{Type: ComparisonIsNull})
	nullValueOp := NewComparisonPredicate(field, Comparison{Type: ComparisonIsNull, Operand: values.LiteralValue(nil)})
	if !PredicateEquals(nilOp, nullValueOp) {
		t.Fatalf("unary IS NULL with nil vs NullValue Operand should compare equal; got Explain a=%q b=%q",
			nilOp.Explain(), nullValueOp.Explain())
	}

	// IS NOT NULL — same property.
	notNilOp := NewComparisonPredicate(field, Comparison{Type: ComparisonIsNotNull})
	notNullValueOp := NewComparisonPredicate(field, Comparison{Type: ComparisonIsNotNull, Operand: values.LiteralValue(int64(0))})
	if !PredicateEquals(notNilOp, notNullValueOp) {
		t.Fatalf("unary IS NOT NULL must ignore Operand for equality")
	}

	// Cross-Type: IS NULL vs IS NOT NULL still distinct.
	if PredicateEquals(nilOp, notNilOp) {
		t.Fatal("IS NULL vs IS NOT NULL should be unequal")
	}
}

// ---------------------------------------------------------------------------
// GetCorrelatedTo — interface method tests
// ---------------------------------------------------------------------------

func TestConstantPredicate_GetCorrelatedTo_Empty(t *testing.T) {
	t.Parallel()
	corr := NewConstantPredicate(TriTrue).GetCorrelatedTo()
	if len(corr) != 0 {
		t.Fatalf("ConstantPredicate.GetCorrelatedTo() len = %d, want 0", len(corr))
	}
}

func TestComparisonPredicate_GetCorrelatedTo_LHSAndRHS(t *testing.T) {
	t.Parallel()
	lhsAlias := values.NamedCorrelationIdentifier("q_lhs")
	rhsAlias := values.NamedCorrelationIdentifier("q_rhs")

	lhs := &values.FieldValue{Field: "col", Typ: values.TypeInt, Child: values.NewQuantifiedObjectValue(lhsAlias)}
	rhs := values.NewQuantifiedObjectValue(rhsAlias)

	pred := NewComparisonPredicate(lhs, Comparison{
		Type:    ComparisonEquals,
		Operand: rhs,
	})

	corr := pred.GetCorrelatedTo()
	if _, ok := corr[lhsAlias]; !ok {
		t.Fatal("missing LHS correlation")
	}
	if _, ok := corr[rhsAlias]; !ok {
		t.Fatal("missing RHS correlation")
	}
	if len(corr) != 2 {
		t.Fatalf("GetCorrelatedTo() len = %d, want 2", len(corr))
	}
}

func TestComparisonPredicate_GetCorrelatedTo_UnaryEmpty(t *testing.T) {
	t.Parallel()
	// IS NULL with a plain FieldValue (no QOV) — empty correlations.
	pred := NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		Comparison{Type: ComparisonIsNull},
	)
	corr := pred.GetCorrelatedTo()
	if len(corr) != 0 {
		t.Fatalf("unary IS NULL on non-correlated field: len = %d, want 0", len(corr))
	}
}

func TestAndPredicate_GetCorrelatedTo_Union(t *testing.T) {
	t.Parallel()
	alias1 := values.NamedCorrelationIdentifier("q1")
	alias2 := values.NamedCorrelationIdentifier("q2")

	pred := NewAnd(
		NewComparisonPredicate(
			values.NewQuantifiedObjectValue(alias1),
			NewLiteralComparison(ComparisonEquals, int64(5)),
		),
		NewComparisonPredicate(
			values.NewQuantifiedObjectValue(alias2),
			NewLiteralComparison(ComparisonEquals, int64(10)),
		),
	)

	corr := pred.GetCorrelatedTo()
	if _, ok := corr[alias1]; !ok {
		t.Fatal("missing alias1")
	}
	if _, ok := corr[alias2]; !ok {
		t.Fatal("missing alias2")
	}
	if len(corr) != 2 {
		t.Fatalf("GetCorrelatedTo() len = %d, want 2", len(corr))
	}
}

func TestAndPredicate_GetCorrelatedTo_Empty(t *testing.T) {
	t.Parallel()
	pred := NewAnd(
		NewConstantPredicate(TriTrue),
		NewConstantPredicate(TriFalse),
	)
	corr := pred.GetCorrelatedTo()
	if len(corr) != 0 {
		t.Fatalf("AND of constants: len = %d, want 0", len(corr))
	}
}

func TestOrPredicate_GetCorrelatedTo_Union(t *testing.T) {
	t.Parallel()
	alias1 := values.NamedCorrelationIdentifier("q1")
	alias2 := values.NamedCorrelationIdentifier("q2")

	pred := NewOr(
		NewComparisonPredicate(
			values.NewQuantifiedObjectValue(alias1),
			NewLiteralComparison(ComparisonEquals, int64(5)),
		),
		NewComparisonPredicate(
			values.NewQuantifiedObjectValue(alias2),
			NewLiteralComparison(ComparisonEquals, int64(10)),
		),
	)

	corr := pred.GetCorrelatedTo()
	if _, ok := corr[alias1]; !ok {
		t.Fatal("missing alias1")
	}
	if _, ok := corr[alias2]; !ok {
		t.Fatal("missing alias2")
	}
}

func TestNotPredicate_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q_sub")
	pred := NewNot(NewExistsPredicate(alias))
	corr := pred.GetCorrelatedTo()
	if _, ok := corr[alias]; !ok {
		t.Fatal("NOT(EXISTS) should contain the existential alias")
	}
	if len(corr) != 1 {
		t.Fatalf("GetCorrelatedTo() len = %d, want 1", len(corr))
	}
}

func TestNotPredicate_GetCorrelatedTo_NilChild(t *testing.T) {
	t.Parallel()
	pred := &NotPredicate{Child: nil}
	corr := pred.GetCorrelatedTo()
	if len(corr) != 0 {
		t.Fatalf("NOT(nil) should return empty set, got len %d", len(corr))
	}
}

func TestExistsPredicate_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("exists_q")
	pred := NewExistsPredicate(alias)
	corr := pred.GetCorrelatedTo()
	if _, ok := corr[alias]; !ok {
		t.Fatal("ExistsPredicate should contain its alias")
	}
	if len(corr) != 1 {
		t.Fatalf("GetCorrelatedTo() len = %d, want 1", len(corr))
	}
}

func TestValuePredicate_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q_val")
	pred := NewValuePredicate(values.NewQuantifiedObjectValue(alias))
	corr := pred.GetCorrelatedTo()
	if _, ok := corr[alias]; !ok {
		t.Fatal("ValuePredicate should contain the QOV correlation")
	}
}

func TestValuePredicate_GetCorrelatedTo_NilValue(t *testing.T) {
	t.Parallel()
	pred := &ValuePredicate{Value: nil}
	corr := pred.GetCorrelatedTo()
	if len(corr) != 0 {
		t.Fatalf("nil Value: len = %d, want 0", len(corr))
	}
}

func TestDatabaseObjectDependenciesPredicate_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	pred := NewDatabaseObjectDependenciesPredicate([]UsedIndex{{Name: "idx", LastModifiedVersion: 1}})
	corr := pred.GetCorrelatedTo()
	if len(corr) != 0 {
		t.Fatalf("DatabaseObjectDependenciesPredicate: len = %d, want 0", len(corr))
	}
}

func TestCompatibleTypeEvolutionPredicate_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	pred := NewCompatibleTypeEvolutionPredicate(map[string]*FieldAccessTrieNode{
		"MyRecord": {FieldName: "name", Ordinal: 1},
	})
	corr := pred.GetCorrelatedTo()
	if len(corr) != 0 {
		t.Fatalf("CompatibleTypeEvolutionPredicate: len = %d, want 0", len(corr))
	}
}

// TestGetCorrelatedTo_MatchesGetCorrelatedToOfPredicate verifies that the
// new interface method returns the same result as the standalone walk function
// for a compound tree.
func TestGetCorrelatedTo_MatchesGetCorrelatedToOfPredicate(t *testing.T) {
	t.Parallel()
	alias1 := values.NamedCorrelationIdentifier("q1")
	alias2 := values.NamedCorrelationIdentifier("q_exists")

	tree := NewAnd(
		NewComparisonPredicate(
			values.NewQuantifiedObjectValue(alias1),
			NewLiteralComparison(ComparisonEquals, int64(42)),
		),
		NewExistsPredicate(alias2),
		NewConstantPredicate(TriTrue),
	)

	fromMethod := tree.GetCorrelatedTo()
	fromWalk := GetCorrelatedToOfPredicate(tree)

	if len(fromMethod) != len(fromWalk) {
		t.Fatalf("method len = %d, walk len = %d", len(fromMethod), len(fromWalk))
	}
	for k := range fromWalk {
		if _, ok := fromMethod[k]; !ok {
			t.Fatalf("method missing alias %s present in walk", k.Name())
		}
	}
}

// PredicateEquals must consider Comparison.Escape — two LIKE
// predicates with the same LHS / pattern but different escape runes
// are distinct. Pin both halves: same-escape → equal,
// different-escape → unequal.
func TestPredicateEquals_ComparisonLikeEscape(t *testing.T) {
	t.Parallel()
	field := &values.FieldValue{Field: "name", Typ: values.TypeString}
	withBackslash := NewComparisonPredicate(field, Comparison{
		Type: ComparisonLike, Operand: values.LiteralValue("a%b"), Escape: '\\',
	})
	withBackslash2 := NewComparisonPredicate(field, Comparison{
		Type: ComparisonLike, Operand: values.LiteralValue("a%b"), Escape: '\\',
	})
	withBang := NewComparisonPredicate(field, Comparison{
		Type: ComparisonLike, Operand: values.LiteralValue("a%b"), Escape: '!',
	})
	noEscape := NewComparisonPredicate(field, Comparison{
		Type: ComparisonLike, Operand: values.LiteralValue("a%b"),
	})

	if !PredicateEquals(withBackslash, withBackslash2) {
		t.Fatal("same escape should compare equal")
	}
	if PredicateEquals(withBackslash, withBang) {
		t.Fatal("different escape rune should compare unequal")
	}
	if PredicateEquals(withBackslash, noEscape) {
		t.Fatal("escape vs no-escape should compare unequal")
	}
}
