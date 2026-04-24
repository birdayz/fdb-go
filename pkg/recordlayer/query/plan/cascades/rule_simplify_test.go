package cascades

import "testing"

var (
	_ CascadesRule = (*AndConstantSimplifyRule)(nil)
	_ CascadesRule = (*OrConstantSimplifyRule)(nil)
	_ CascadesRule = (*NotConstantSimplifyRule)(nil)
	_ CascadesRule = (*ComparisonConstantSimplifyRule)(nil)
	_ CascadesRule = (*AndFlattenRule)(nil)
	_ CascadesRule = (*OrFlattenRule)(nil)
	_ CascadesRule = (*AndDedupRule)(nil)
	_ CascadesRule = (*OrDedupRule)(nil)
	_ CascadesRule = (*AndAbsorbOrRule)(nil)
	_ CascadesRule = (*OrAbsorbAndRule)(nil)
	_ CascadesRule = (*NotComparisonRewriteRule)(nil)
)

// AND(p, p, q, p) → AND(p, q).
func TestAndDedup_RemovesDuplicates(t *testing.T) {
	t.Parallel()
	rule := NewAndDedupRule()
	p := NewComparisonPredicate(
		&FieldValue{Field: "x", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: int64(1)},
	)
	q := NewComparisonPredicate(
		&FieldValue{Field: "y", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThan, Operand: int64(0)},
	)
	// Four children, two distinct: p and q.
	and := NewAnd(p, p, q, p)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	deduped, ok := got[0].(*AndPredicate)
	if !ok || len(deduped.SubPredicates) != 2 {
		t.Fatalf("expected AND with 2 children, got %v", got[0])
	}
}

// AND(p, p) → p (single-child collapse).
func TestAndDedup_AllSameCollapses(t *testing.T) {
	t.Parallel()
	rule := NewAndDedupRule()
	p := NewConstantPredicate(TriUnknown)
	and := NewAnd(p, p, p)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != QueryPredicate(p) {
		t.Fatalf("expected p, got %v", got[0])
	}
}

// No duplicates → rule declines.
func TestAndDedup_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewAndDedupRule()
	and := NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse))
	if got := FireRule(rule, and); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// OrDedupRule mirror.
func TestOrDedup_RemovesDuplicates(t *testing.T) {
	t.Parallel()
	rule := NewOrDedupRule()
	p := NewConstantPredicate(TriUnknown)
	or := NewOr(p, p, p)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != QueryPredicate(p) {
		t.Fatalf("expected p, got %v", got[0])
	}
}

// AndFlattenRule collapses nested AndPredicates into a single flat
// list of operands.
func TestAndFlatten_NestedBecomesFlat(t *testing.T) {
	t.Parallel()
	rule := NewAndFlattenRule()
	// AND(AND(a, b), c)  →  AND(a, b, c)
	a := NewConstantPredicate(TriUnknown)
	b := NewConstantPredicate(TriUnknown)
	c := NewConstantPredicate(TriUnknown)
	nested := NewAnd(NewAnd(a, b), c)
	got := FireRule(rule, nested)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	flat, ok := got[0].(*AndPredicate)
	if !ok || len(flat.SubPredicates) != 3 {
		t.Fatalf("expected flat AND with 3 children, got %v", got[0])
	}
}

// Already-flat AND → rule declines (idempotent).
func TestAndFlatten_AlreadyFlat(t *testing.T) {
	t.Parallel()
	rule := NewAndFlattenRule()
	flat := NewAnd(NewConstantPredicate(TriUnknown), NewConstantPredicate(TriUnknown))
	if got := FireRule(rule, flat); len(got) != 0 {
		t.Fatalf("expected 0 yields, got %d", len(got))
	}
}

// OrFlattenRule mirror.
func TestOrFlatten_NestedBecomesFlat(t *testing.T) {
	t.Parallel()
	rule := NewOrFlattenRule()
	a := NewConstantPredicate(TriUnknown)
	b := NewConstantPredicate(TriUnknown)
	c := NewConstantPredicate(TriUnknown)
	nested := NewOr(NewOr(a, b), c)
	got := FireRule(rule, nested)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	flat, ok := got[0].(*OrPredicate)
	if !ok || len(flat.SubPredicates) != 3 {
		t.Fatalf("expected flat OR with 3 children, got %v", got[0])
	}
}

// ComparisonConstantSimplify: both sides literal → constant
// predicate. Covers true/false/unknown outcomes.
func TestComparisonConstSimplify_Folds(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()

	cases := []struct {
		name string
		lhs  any
		op   ComparisonType
		rhs  any
		want TriBool
	}{
		{"5=5→TRUE", int64(5), ComparisonEquals, int64(5), TriTrue},
		{"5=3→FALSE", int64(5), ComparisonEquals, int64(3), TriFalse},
		{"5>3→TRUE", int64(5), ComparisonGreaterThan, int64(3), TriTrue},
		{"1<2→TRUE", int64(1), ComparisonLessThan, int64(2), TriTrue},
		{"NULL=1→UNKNOWN", nil, ComparisonEquals, int64(1), TriUnknown},
		// Round out the operator matrix so every ComparisonType this
		// package ships has a documented fold.
		{"5<>3→TRUE", int64(5), ComparisonNotEquals, int64(3), TriTrue},
		{"5<>5→FALSE", int64(5), ComparisonNotEquals, int64(5), TriFalse},
		{"5>=5→TRUE", int64(5), ComparisonGreaterThanEq, int64(5), TriTrue},
		{"5<=5→TRUE", int64(5), ComparisonLessThanOrEq, int64(5), TriTrue},
		{"5<=3→FALSE", int64(5), ComparisonLessThanOrEq, int64(3), TriFalse},
		{"1=NULL→UNKNOWN", int64(1), ComparisonEquals, nil, TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := NewComparisonPredicate(
				&ConstantValue{Value: tc.lhs, Typ: TypeInt},
				Comparison{Type: tc.op, Operand: tc.rhs},
			)
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*ConstantPredicate)
			if !ok {
				t.Fatalf("expected ConstantPredicate, got %T", got[0])
			}
			if cp.Value != tc.want {
				t.Fatalf("got %v, want %v", cp.Value, tc.want)
			}
		})
	}
}

// ComparisonConstSimplify folds unary IS NULL / IS NOT NULL against
// any known-constant operand (ConstantValue, NullValue, BooleanValue).
// Cross-check with the unary comparisons landed alongside this rule.
func TestComparisonConstSimplify_UnaryIsNull(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	cases := []struct {
		name    string
		operand Value
		op      ComparisonType
		want    TriBool
	}{
		{"NULL IS NULL → TRUE", NewNullValue(TypeInt), ComparisonIsNull, TriTrue},
		{"NULL IS NOT NULL → FALSE", NewNullValue(TypeInt), ComparisonIsNotNull, TriFalse},
		{"ConstantValue(5) IS NULL → FALSE", &ConstantValue{Value: int64(5), Typ: TypeInt}, ComparisonIsNull, TriFalse},
		{"ConstantValue(5) IS NOT NULL → TRUE", &ConstantValue{Value: int64(5), Typ: TypeInt}, ComparisonIsNotNull, TriTrue},
		{"ConstantValue(nil) IS NULL → TRUE", &ConstantValue{Value: nil, Typ: TypeInt}, ComparisonIsNull, TriTrue},
		{"BooleanValue(true) IS NOT NULL → TRUE", NewBooleanValue(true), ComparisonIsNotNull, TriTrue},
		{"BooleanValue(nil) IS NULL → TRUE", &BooleanValue{Value: nil}, ComparisonIsNull, TriTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := NewComparisonPredicate(tc.operand, Comparison{Type: tc.op})
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*ConstantPredicate)
			if !ok || cp.Value != tc.want {
				t.Fatalf("got %T %v, want ConstantPredicate(%v)", got[0], got[0], tc.want)
			}
		})
	}
}

// STARTS_WITH / IN fold through the same rule since their operand
// is still a ConstantValue — the Comparison's Eval method knows how
// to handle the special comparator types, so the rule needs no
// special-casing.
func TestComparisonConstSimplify_StartsWithAndIn(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	cases := []struct {
		name string
		lhs  any
		cmp  Comparison
		want TriBool
	}{
		{
			"'hello' STARTS_WITH 'hel'", "hello",
			Comparison{Type: ComparisonStartsWith, Operand: "hel"},
			TriTrue,
		},
		{
			"'world' STARTS_WITH 'hel'", "world",
			Comparison{Type: ComparisonStartsWith, Operand: "hel"},
			TriFalse,
		},
		{
			"5 IN (1,5,9)", int64(5),
			Comparison{Type: ComparisonIn, Operand: []any{int64(1), int64(5), int64(9)}},
			TriTrue,
		},
		{
			"5 IN (1,NULL)", int64(5),
			Comparison{Type: ComparisonIn, Operand: []any{int64(1), nil}},
			TriUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := NewComparisonPredicate(
				&ConstantValue{Value: tc.lhs, Typ: TypeString},
				tc.cmp,
			)
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*ConstantPredicate)
			if !ok || cp.Value != tc.want {
				t.Fatalf("got %T %v, want ConstantPredicate(%v)", got[0], got[0], tc.want)
			}
		})
	}
}

// FieldValue operand still declines — can't fold without a row.
func TestComparisonConstSimplify_FieldWithIsNullDeclines(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := NewComparisonPredicate(
		&FieldValue{Field: "middle_name", Typ: TypeString},
		Comparison{Type: ComparisonIsNull},
	)
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected 0 yields (field operand), got %d", len(got))
	}
}

// Non-constant operand (FieldValue) — rule declines.
func TestComparisonConstSimplify_FieldOperandDeclines(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected 0 yields (field operand), got %d", len(got))
	}
}

func TestNotSimplify_ConstantFold(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	cases := []struct {
		in   TriBool
		want TriBool
	}{
		{TriTrue, TriFalse},
		{TriFalse, TriTrue},
		{TriUnknown, TriUnknown},
	}
	for _, tc := range cases {
		got := FireRule(rule, NewNot(NewConstantPredicate(tc.in)))
		if len(got) != 1 {
			t.Fatalf("%v: expected 1 replacement, got %d", tc.in, len(got))
		}
		cp, ok := got[0].(*ConstantPredicate)
		if !ok || cp.Value != tc.want {
			t.Fatalf("%v: got %v, want ConstantPredicate(%v)", tc.in, got[0], tc.want)
		}
	}
}

// NOT NOT x → x (double-negation elimination).
func TestNotSimplify_DoubleNegation(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	inner := NewConstantPredicate(TriUnknown)
	got := FireRule(rule, NewNot(NewNot(inner)))
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	if got[0] != QueryPredicate(inner) {
		t.Fatalf("double-negation: expected inner predicate, got %T", got[0])
	}
}

// NOT over a non-constant, non-NOT predicate — rule declines.
func TestNotSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	and := NewAnd(NewConstantPredicate(TriTrue))
	// NewNot(AndPredicate) — inner is neither ConstantPredicate nor
	// another NotPredicate, so NotConstantSimplifyRule declines.
	if got := FireRule(rule, NewNot(and)); len(got) != 0 {
		t.Fatalf("expected 0 yields, got %d", len(got))
	}
}

// AndPredicate with all-TRUE children → TRUE.
func TestAndSimplify_AllTrueToConstant(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := NewAnd(
		NewConstantPredicate(TriTrue),
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// AndPredicate with a FALSE child → FALSE (short-circuit).
func TestAndSimplify_FalseShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := NewAnd(
		NewConstantPredicate(TriTrue),
		NewConstantPredicate(TriFalse),
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriFalse {
		t.Fatalf("expected ConstantPredicate(FALSE), got %v", got[0])
	}
}

// Drop TRUE children from an AND, leaving the non-trivial children.
func TestAndSimplify_DropTrueChildren(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	// UNKNOWN is technically a ConstantPredicate too, but the And
	// rule keeps it — only TRUE (identity-drop) and FALSE
	// (absorbing) trigger folds. UNKNOWN-leaf stands in here for
	// any predicate the rule treats as opaque.
	leaf := NewConstantPredicate(TriUnknown)
	and := NewAnd(
		NewConstantPredicate(TriTrue),
		leaf,
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	// Single non-constant child remains — rule yields it directly.
	if got[0] != QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %v", got[0], got[0])
	}
}

// No constant children → rule declines to yield (idempotent).
func TestAndSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	leaf := NewConstantPredicate(TriUnknown)
	and := NewAnd(leaf, leaf)
	got := FireRule(rule, and)
	if len(got) != 0 {
		t.Fatalf("expected rule to decline (0 yields), got %d", len(got))
	}
}

// OrPredicate with a TRUE child → TRUE.
func TestOrSimplify_TrueShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := NewOr(
		NewConstantPredicate(TriFalse),
		NewConstantPredicate(TriTrue),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// Drop FALSE children from an OR, leaving the non-trivial children.
// Symmetric to TestAndSimplify_DropTrueChildren.
func TestOrSimplify_DropFalseChildren(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	leaf := NewConstantPredicate(TriUnknown)
	or := NewOr(
		NewConstantPredicate(TriFalse),
		leaf,
		NewConstantPredicate(TriFalse),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	if got[0] != QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %v", got[0], got[0])
	}
}

// No FALSE children → rule declines. Symmetric to
// TestAndSimplify_NoChange.
func TestOrSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	leaf := NewConstantPredicate(TriUnknown)
	or := NewOr(leaf, leaf)
	got := FireRule(rule, or)
	if len(got) != 0 {
		t.Fatalf("expected rule to decline (0 yields), got %d", len(got))
	}
}

// OrPredicate with all-FALSE children → FALSE.
func TestOrSimplify_AllFalseToConstant(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := NewOr(
		NewConstantPredicate(TriFalse),
		NewConstantPredicate(TriFalse),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*ConstantPredicate)
	if !ok || cp.Value != TriFalse {
		t.Fatalf("expected ConstantPredicate(FALSE), got %v", got[0])
	}
}

// Rules do not fire when the input isn't the matcher's type.
func TestAndSimplify_WrongType(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	// Feed an OrPredicate — AND rule's matcher should bail.
	or := NewOr(NewConstantPredicate(TriTrue))
	if got := FireRule(rule, or); len(got) != 0 {
		t.Fatalf("expected AND rule to not fire on OR, got %d yields", len(got))
	}
}

// AndAbsorbOrRule: p AND (p OR q) → drop the OR, leaving just `p`.
func TestAndAbsorbOr_DropsRedundantOrChild(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	q := NewComparisonPredicate(
		&FieldValue{Field: "rank", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThan, Operand: int64(0)},
	)
	and := NewAnd(p, NewOr(p, q))
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != QueryPredicate(p) {
		t.Fatalf("expected p, got %T %v", got[0], got[0])
	}
}

// AndAbsorbOrRule leaves AND alone when no OR child shares an
// operand with a sibling.
func TestAndAbsorbOr_NoOpWhenNoSharedOperand(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	q := NewComparisonPredicate(
		&FieldValue{Field: "rank", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThan, Operand: int64(0)},
	)
	r := NewComparisonPredicate(
		&FieldValue{Field: "score", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThan, Operand: int64(50)},
	)
	and := NewAnd(p, NewOr(q, r))
	if got := FireRule(rule, and); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// OrAbsorbAndRule: p OR (p AND q) → drop the AND, leaving just `p`.
func TestOrAbsorbAnd_DropsRedundantAndChild(t *testing.T) {
	t.Parallel()
	rule := NewOrAbsorbAndRule()
	p := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	q := NewComparisonPredicate(
		&FieldValue{Field: "rank", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThan, Operand: int64(0)},
	)
	or := NewOr(p, NewAnd(p, q))
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != QueryPredicate(p) {
		t.Fatalf("expected p, got %T %v", got[0], got[0])
	}
}

// End-to-end through Simplify: a classic absorption plus flatten +
// dedup cooperation.
func TestSimplify_Absorption_EndToEnd(t *testing.T) {
	t.Parallel()
	p := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	q := NewComparisonPredicate(
		&FieldValue{Field: "rank", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThan, Operand: int64(0)},
	)
	// AND(p, OR(p, q), TRUE) → AND(p, TRUE) → p.
	pred := NewAnd(
		p,
		NewOr(p, q),
		NewConstantPredicate(TriTrue),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != QueryPredicate(p) {
		t.Fatalf("expected p to survive, got %T %s", got, got.Explain())
	}
}

// NotComparisonRewriteRule: NOT(x = 5) → x <> 5.
func TestNotComparisonRewrite_NegatesEquals(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: int64(5)},
	)
	got := FireRule(rule, NewNot(cp))
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	out, ok := got[0].(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", got[0])
	}
	if out.Comparison.Type != ComparisonNotEquals {
		t.Fatalf("got %s, want <>", out.Comparison.Type.Symbol())
	}
	if out.Comparison.Operand != int64(5) {
		t.Fatalf("operand changed: got %v", out.Comparison.Operand)
	}
}

// NOT(x IS NULL) → x IS NOT NULL.
func TestNotComparisonRewrite_NegatesIsNull(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := NewComparisonPredicate(
		&FieldValue{Field: "email", Typ: TypeString},
		Comparison{Type: ComparisonIsNull},
	)
	got := FireRule(rule, NewNot(cp))
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	out := got[0].(*ComparisonPredicate)
	if out.Comparison.Type != ComparisonIsNotNull {
		t.Fatalf("got %s, want IS NOT NULL", out.Comparison.Type.Symbol())
	}
}

// NOT(x IN (...)) declines — IN has no direct-negation type, the
// NOT must stay as a wrapper.
func TestNotComparisonRewrite_InDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonIn, Operand: []any{int64(1), int64(2)}},
	)
	if got := FireRule(rule, NewNot(cp)); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// NOT(<non-comparison>) declines — rule is comparison-specific.
func TestNotComparisonRewrite_NonComparisonDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	inner := NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse))
	if got := FireRule(rule, NewNot(inner)); len(got) != 0 {
		t.Fatalf("expected rule to decline on NOT(AND), got %d yields", len(got))
	}
}

// End-to-end: NOT(age = 18) fixes up through the simplifier to
// `age <> 18` and the outer NOT vanishes.
func TestSimplify_NotComparisonEndToEnd(t *testing.T) {
	t.Parallel()
	age := &FieldValue{Field: "age", Typ: TypeInt}
	got := Simplify(
		NewNot(NewComparisonPredicate(age, Comparison{Type: ComparisonGreaterThan, Operand: int64(18)})),
		DefaultSimplifyRules(),
	)
	cp, ok := got.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T %s", got, got.Explain())
	}
	if cp.Comparison.Type != ComparisonLessThanOrEq {
		t.Fatalf("expected age <= 18, got %s", got.Explain())
	}
}
