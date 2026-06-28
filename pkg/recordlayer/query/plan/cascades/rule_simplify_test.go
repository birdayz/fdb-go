package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

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
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "y", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	// Four children, two distinct: p and q.
	and := predicates.NewAnd(p, p, q, p)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	deduped, ok := got[0].(*predicates.AndPredicate)
	if !ok || len(deduped.SubPredicates) != 2 {
		t.Fatalf("expected AND with 2 children, got %v", got[0])
	}
}

// AND(p, p) → p (single-child collapse).
func TestAndDedup_AllSameCollapses(t *testing.T) {
	t.Parallel()
	rule := NewAndDedupRule()
	p := predicates.NewConstantPredicate(predicates.TriUnknown)
	and := predicates.NewAnd(p, p, p)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(p) {
		t.Fatalf("expected p, got %v", got[0])
	}
}

// No duplicates → rule declines.
func TestAndDedup_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewAndDedupRule()
	and := predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), predicates.NewConstantPredicate(predicates.TriFalse))
	if got := FireRule(rule, and); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// OrDedupRule mirror.
func TestOrDedup_RemovesDuplicates(t *testing.T) {
	t.Parallel()
	rule := NewOrDedupRule()
	p := predicates.NewConstantPredicate(predicates.TriUnknown)
	or := predicates.NewOr(p, p, p)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(p) {
		t.Fatalf("expected p, got %v", got[0])
	}
}

// TestOrDedup_PartialDedupKeepsRemaining pins the OrDedup default
// arm (len(deduped) > 1): NewOr(p1, p2, p1) dedups to NewOr(p1, p2),
// not just p1. Symmetric to AndDedup_RemovesDuplicates which already
// hit this branch on the AND side.
func TestOrDedup_PartialDedupKeepsRemaining(t *testing.T) {
	t.Parallel()
	rule := NewOrDedupRule()
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	or := predicates.NewOr(p1, p2, p1) // p1 appears twice
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	out, ok := got[0].(*predicates.OrPredicate)
	if !ok {
		t.Fatalf("expected *OrPredicate, got %T", got[0])
	}
	if len(out.SubPredicates) != 2 {
		t.Fatalf("expected 2 subpredicates after dedup, got %d", len(out.SubPredicates))
	}
	// First-occurrence order preserved.
	if out.SubPredicates[0] != predicates.QueryPredicate(p1) || out.SubPredicates[1] != predicates.QueryPredicate(p2) {
		t.Fatalf("dedup did not preserve first-occurrence order")
	}
}

// TestOrDedup_NoChange pins the no-op pointer-identity behaviour:
// when there are no duplicates, the rule does not yield (FireRule
// gets an empty slice). Caller's pointer-equality fixpoint loop
// breaks out without rebuilding the OR.
func TestOrDedup_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewOrDedupRule()
	or := predicates.NewOr(predicates.NewConstantPredicate(predicates.TriTrue), predicates.NewConstantPredicate(predicates.TriFalse))
	if got := FireRule(rule, or); len(got) != 0 {
		t.Fatalf("expected no yield (no dups), got %d", len(got))
	}
}

// AndFlattenRule collapses nested AndPredicates into a single flat
// list of operands.
func TestAndFlatten_NestedBecomesFlat(t *testing.T) {
	t.Parallel()
	rule := NewAndFlattenRule()
	// AND(AND(a, b), c)  →  AND(a, b, c)
	a := predicates.NewConstantPredicate(predicates.TriUnknown)
	b := predicates.NewConstantPredicate(predicates.TriUnknown)
	c := predicates.NewConstantPredicate(predicates.TriUnknown)
	nested := predicates.NewAnd(predicates.NewAnd(a, b), c)
	got := FireRule(rule, nested)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	flat, ok := got[0].(*predicates.AndPredicate)
	if !ok || len(flat.SubPredicates) != 3 {
		t.Fatalf("expected flat AND with 3 children, got %v", got[0])
	}
}

// Already-flat AND → rule declines (idempotent).
func TestAndFlatten_AlreadyFlat(t *testing.T) {
	t.Parallel()
	rule := NewAndFlattenRule()
	flat := predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriUnknown), predicates.NewConstantPredicate(predicates.TriUnknown))
	if got := FireRule(rule, flat); len(got) != 0 {
		t.Fatalf("expected 0 yields, got %d", len(got))
	}
}

// OrFlattenRule mirror.
func TestOrFlatten_NestedBecomesFlat(t *testing.T) {
	t.Parallel()
	rule := NewOrFlattenRule()
	a := predicates.NewConstantPredicate(predicates.TriUnknown)
	b := predicates.NewConstantPredicate(predicates.TriUnknown)
	c := predicates.NewConstantPredicate(predicates.TriUnknown)
	nested := predicates.NewOr(predicates.NewOr(a, b), c)
	got := FireRule(rule, nested)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	flat, ok := got[0].(*predicates.OrPredicate)
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
		op   predicates.ComparisonType
		rhs  any
		want predicates.TriBool
	}{
		{"5=5→TRUE", int64(5), predicates.ComparisonEquals, int64(5), predicates.TriTrue},
		{"5=3→FALSE", int64(5), predicates.ComparisonEquals, int64(3), predicates.TriFalse},
		{"5>3→TRUE", int64(5), predicates.ComparisonGreaterThan, int64(3), predicates.TriTrue},
		{"1<2→TRUE", int64(1), predicates.ComparisonLessThan, int64(2), predicates.TriTrue},
		{"NULL=1→UNKNOWN", nil, predicates.ComparisonEquals, int64(1), predicates.TriUnknown},
		// Round out the operator matrix so every ComparisonType this
		// package ships has a documented fold.
		{"5<>3→TRUE", int64(5), predicates.ComparisonNotEquals, int64(3), predicates.TriTrue},
		{"5<>5→FALSE", int64(5), predicates.ComparisonNotEquals, int64(5), predicates.TriFalse},
		{"5>=5→TRUE", int64(5), predicates.ComparisonGreaterThanEq, int64(5), predicates.TriTrue},
		{"5<=5→TRUE", int64(5), predicates.ComparisonLessThanOrEq, int64(5), predicates.TriTrue},
		{"5<=3→FALSE", int64(5), predicates.ComparisonLessThanOrEq, int64(3), predicates.TriFalse},
		{"1=NULL→UNKNOWN", int64(1), predicates.ComparisonEquals, nil, predicates.TriUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: tc.lhs, Typ: values.TypeInt},
				predicates.Comparison{Type: tc.op, Operand: values.LiteralValue(tc.rhs)},
			)
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*predicates.ConstantPredicate)
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
		operand values.Value
		op      predicates.ComparisonType
		want    predicates.TriBool
	}{
		{"NULL IS NULL → TRUE", values.NewNullValue(values.TypeInt), predicates.ComparisonIsNull, predicates.TriTrue},
		{"NULL IS NOT NULL → FALSE", values.NewNullValue(values.TypeInt), predicates.ComparisonIsNotNull, predicates.TriFalse},
		{"ConstantValue(5) IS NULL → FALSE", &values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, predicates.ComparisonIsNull, predicates.TriFalse},
		{"ConstantValue(5) IS NOT NULL → TRUE", &values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, predicates.ComparisonIsNotNull, predicates.TriTrue},
		{"ConstantValue(nil) IS NULL → TRUE", &values.ConstantValue{Value: nil, Typ: values.TypeInt}, predicates.ComparisonIsNull, predicates.TriTrue},
		{"BooleanValue(true) IS NOT NULL → TRUE", values.NewBooleanValue(true), predicates.ComparisonIsNotNull, predicates.TriTrue},
		{"BooleanValue(nil) IS NULL → TRUE", &values.BooleanValue{Value: nil}, predicates.ComparisonIsNull, predicates.TriTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := predicates.NewComparisonPredicate(tc.operand, predicates.Comparison{Type: tc.op})
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*predicates.ConstantPredicate)
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
		cmp  predicates.Comparison
		want predicates.TriBool
	}{
		{
			"'hello' STARTS_WITH 'hel'", "hello",
			predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue("hel")},
			predicates.TriTrue,
		},
		{
			"'world' STARTS_WITH 'hel'", "world",
			predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue("hel")},
			predicates.TriFalse,
		},
		{
			"5 IN (1,5,9)", int64(5),
			predicates.Comparison{Type: predicates.ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(5), int64(9)})},
			predicates.TriTrue,
		},
		{
			"5 IN (1,NULL)", int64(5),
			predicates.Comparison{Type: predicates.ComparisonIn, Operand: values.LiteralValue([]any{int64(1), nil})},
			predicates.TriUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: tc.lhs, Typ: values.TypeString},
				tc.cmp,
			)
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*predicates.ConstantPredicate)
			if !ok || cp.Value != tc.want {
				t.Fatalf("got %T %v, want ConstantPredicate(%v)", got[0], got[0], tc.want)
			}
		})
	}
}

// LIKE folds when LHS is a known-constant string.
func TestComparisonConstSimplify_Like(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	cases := []struct {
		name    string
		s       string
		pattern string
		want    predicates.TriBool
	}{
		{"'hello' LIKE 'h_llo'", "hello", "h_llo", predicates.TriTrue},
		{"'hello' LIKE 'w%d'", "hello", "w%d", predicates.TriFalse},
		{"'hello' LIKE '%ll%'", "hello", "%ll%", predicates.TriTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: tc.s, Typ: values.TypeString},
				predicates.Comparison{Type: predicates.ComparisonLike, Operand: values.LiteralValue(tc.pattern)},
			)
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*predicates.ConstantPredicate)
			if !ok || cp.Value != tc.want {
				t.Fatalf("got %T %v, want ConstantPredicate(%v)", got[0], got[0], tc.want)
			}
		})
	}
}

// IS [NOT] DISTINCT FROM folds too when LHS is a known constant.
// Because IS DISTINCT FROM never returns UNKNOWN, the fold collapses
// to a definitive TRUE/FALSE even on NULL inputs.
func TestComparisonConstSimplify_IsDistinctFrom(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	cases := []struct {
		name    string
		operand values.Value
		cmp     predicates.Comparison
		want    predicates.TriBool
	}{
		{
			"NULL IS DISTINCT FROM NULL", values.NewNullValue(values.TypeInt),
			predicates.Comparison{Type: predicates.ComparisonIsDistinctFrom, Operand: values.LiteralValue(nil)},
			predicates.TriFalse,
		},
		{
			"NULL IS NOT DISTINCT FROM NULL", values.NewNullValue(values.TypeInt),
			predicates.Comparison{Type: predicates.ComparisonNotDistinctFrom, Operand: values.LiteralValue(nil)},
			predicates.TriTrue,
		},
		{
			"5 IS NOT DISTINCT FROM 5", &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonNotDistinctFrom, Operand: values.LiteralValue(int64(5))},
			predicates.TriTrue,
		},
		{
			"5 IS DISTINCT FROM NULL", &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonIsDistinctFrom, Operand: values.LiteralValue(nil)},
			predicates.TriTrue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := predicates.NewComparisonPredicate(tc.operand, tc.cmp)
			got := FireRule(rule, pred)
			if len(got) != 1 {
				t.Fatalf("expected 1 yield, got %d", len(got))
			}
			cp, ok := got[0].(*predicates.ConstantPredicate)
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
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "middle_name", Typ: values.TypeString},
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected 0 yields (field operand), got %d", len(got))
	}
}

// Non-constant operand (FieldValue) — rule declines.
func TestComparisonConstSimplify_FieldOperandDeclines(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected 0 yields (field operand), got %d", len(got))
	}
}

func TestNotSimplify_ConstantFold(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	cases := []struct {
		in   predicates.TriBool
		want predicates.TriBool
	}{
		{predicates.TriTrue, predicates.TriFalse},
		{predicates.TriFalse, predicates.TriTrue},
		{predicates.TriUnknown, predicates.TriUnknown},
	}
	for _, tc := range cases {
		got := FireRule(rule, predicates.NewNot(predicates.NewConstantPredicate(tc.in)))
		if len(got) != 1 {
			t.Fatalf("%v: expected 1 replacement, got %d", tc.in, len(got))
		}
		cp, ok := got[0].(*predicates.ConstantPredicate)
		if !ok || cp.Value != tc.want {
			t.Fatalf("%v: got %v, want ConstantPredicate(%v)", tc.in, got[0], tc.want)
		}
	}
}

// NOT NOT x → x (double-negation elimination).
func TestNotSimplify_DoubleNegation(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	inner := predicates.NewConstantPredicate(predicates.TriUnknown)
	got := FireRule(rule, predicates.NewNot(predicates.NewNot(inner)))
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(inner) {
		t.Fatalf("double-negation: expected inner predicate, got %T", got[0])
	}
}

// NOT over a non-constant, non-NOT predicate — rule declines.
func TestNotSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	and := predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue))
	// NewNot(AndPredicate) — inner is neither ConstantPredicate nor
	// another NotPredicate, so NotConstantSimplifyRule declines.
	if got := FireRule(rule, predicates.NewNot(and)); len(got) != 0 {
		t.Fatalf("expected 0 yields, got %d", len(got))
	}
}

// AndPredicate with all-TRUE children → TRUE.
func TestAndSimplify_AllTrueToConstant(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// AndPredicate with a FALSE child → FALSE (short-circuit).
func TestAndSimplify_FalseShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriFalse {
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
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	and := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		leaf,
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	// Single non-constant child remains — rule yields it directly.
	if got[0] != predicates.QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %v", got[0], got[0])
	}
}

// No constant children → rule declines to yield (idempotent).
func TestAndSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	and := predicates.NewAnd(leaf, leaf)
	got := FireRule(rule, and)
	if len(got) != 0 {
		t.Fatalf("expected rule to decline (0 yields), got %d", len(got))
	}
}

// OrPredicate with a TRUE child → TRUE.
func TestOrSimplify_TrueShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriFalse),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// Drop FALSE children from an OR, leaving the non-trivial children.
// Symmetric to TestAndSimplify_DropTrueChildren.
func TestOrSimplify_DropFalseChildren(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	or := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriFalse),
		leaf,
		predicates.NewConstantPredicate(predicates.TriFalse),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %v", got[0], got[0])
	}
}

// No FALSE children → rule declines. Symmetric to
// TestAndSimplify_NoChange.
func TestOrSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	or := predicates.NewOr(leaf, leaf)
	got := FireRule(rule, or)
	if len(got) != 0 {
		t.Fatalf("expected rule to decline (0 yields), got %d", len(got))
	}
}

// OrPredicate with all-FALSE children → FALSE.
func TestOrSimplify_AllFalseToConstant(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriFalse),
		predicates.NewConstantPredicate(predicates.TriFalse),
	)
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriFalse {
		t.Fatalf("expected ConstantPredicate(FALSE), got %v", got[0])
	}
}

// Rules do not fire when the input isn't the matcher's type.
func TestAndSimplify_WrongType(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	// Feed an OrPredicate — AND rule's matcher should bail.
	or := predicates.NewOr(predicates.NewConstantPredicate(predicates.TriTrue))
	if got := FireRule(rule, or); len(got) != 0 {
		t.Fatalf("expected AND rule to not fire on OR, got %d yields", len(got))
	}
}

// AndAbsorbOrRule: p AND (p OR q) → drop the OR, leaving just `p`.
func TestAndAbsorbOr_DropsRedundantOrChild(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "rank", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	and := predicates.NewAnd(p, predicates.NewOr(p, q))
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(p) {
		t.Fatalf("expected p, got %T %v", got[0], got[0])
	}
}

// AndAbsorbOrRule leaves AND alone when no OR child shares an
// operand with a sibling.
// TestAndAbsorbOr_KeepsMultipleSurvivors pins the default arm:
// AND(p, OR(p, q), r) drops OR(p,q) leaving AND(p, r) — TWO
// surviving children, so the rule rebuilds an AndPredicate (case
// default in OnMatch's switch). The DropsRedundantOrChild test
// only exercised the case-1 arm.
func TestAndAbsorbOr_KeepsMultipleSurvivors(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "rank", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	r := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "score", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(50))},
	)
	and := predicates.NewAnd(p, predicates.NewOr(p, q), r) // p AND (p OR q) AND r
	got := FireRule(rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	out, ok := got[0].(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected AndPredicate, got %T", got[0])
	}
	if len(out.SubPredicates) != 2 {
		t.Fatalf("expected 2 surviving children (p, r), got %d", len(out.SubPredicates))
	}
	if out.SubPredicates[0] != predicates.QueryPredicate(p) || out.SubPredicates[1] != predicates.QueryPredicate(r) {
		t.Fatalf("survivors out of order: got [%T, %T]", out.SubPredicates[0], out.SubPredicates[1])
	}
}

func TestAndAbsorbOr_NoOpWhenNoSharedOperand(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "rank", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	r := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "score", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(50))},
	)
	and := predicates.NewAnd(p, predicates.NewOr(q, r))
	if got := FireRule(rule, and); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// OrAbsorbAndRule: p OR (p AND q) → drop the AND, leaving just `p`.
func TestOrAbsorbAnd_DropsRedundantAndChild(t *testing.T) {
	t.Parallel()
	rule := NewOrAbsorbAndRule()
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "rank", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	or := predicates.NewOr(p, predicates.NewAnd(p, q))
	got := FireRule(rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(p) {
		t.Fatalf("expected p, got %T %v", got[0], got[0])
	}
}

// End-to-end through Simplify: a classic absorption plus flatten +
// dedup cooperation.
func TestSimplify_Absorption_EndToEnd(t *testing.T) {
	t.Parallel()
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "rank", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	// AND(p, OR(p, q), TRUE) → AND(p, TRUE) → p.
	pred := predicates.NewAnd(
		p,
		predicates.NewOr(p, q),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != predicates.QueryPredicate(p) {
		t.Fatalf("expected p to survive, got %T %s", got, got.Explain())
	}
}

// NotComparisonRewriteRule: NOT(x = 5) → x <> 5.
func TestNotComparisonRewrite_NegatesEquals(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))},
	)
	got := FireRule(rule, predicates.NewNot(cp))
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	out, ok := got[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", got[0])
	}
	if out.Comparison.Type != predicates.ComparisonNotEquals {
		t.Fatalf("got %s, want <>", out.Comparison.Type.Symbol())
	}
	rhsLit, ok := values.EvaluateConstant(out.Comparison.Operand)
	if !ok || rhsLit != int64(5) {
		t.Fatalf("operand changed: got %v", out.Comparison.Operand)
	}
}

// NOT(x IS NULL) → x IS NOT NULL.
// NOT(IS NULL) is NOT invertible in Java — invertComparisonType
// rejects unary operators. The rule should decline.
func TestNotComparisonRewrite_IsNullDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "email", Typ: values.TypeString},
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	if got := FireRule(rule, predicates.NewNot(cp)); len(got) != 0 {
		t.Fatalf("expected rule to decline for IS NULL, got %d yields", len(got))
	}
}

// NOT(x IN (...)) declines — IN has no direct-negation type, the
// NOT must stay as a wrapper.
func TestNotComparisonRewrite_InDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2)})},
	)
	if got := FireRule(rule, predicates.NewNot(cp)); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// NOT(<non-comparison>) declines — rule is comparison-specific.
func TestNotComparisonRewrite_NonComparisonDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	inner := predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), predicates.NewConstantPredicate(predicates.TriFalse))
	if got := FireRule(rule, predicates.NewNot(inner)); len(got) != 0 {
		t.Fatalf("expected rule to decline on NOT(AND), got %d yields", len(got))
	}
}

// End-to-end: NOT(age = 18) fixes up through the simplifier to
// `age <> 18` and the outer NOT vanishes.
func TestSimplify_NotComparisonEndToEnd(t *testing.T) {
	t.Parallel()
	age := &values.FieldValue{Field: "age", Typ: values.TypeInt}
	got := Simplify(
		predicates.NewNot(predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(18))})),
		DefaultSimplifyRules(),
	)
	cp, ok := got.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T %s", got, got.Explain())
	}
	if cp.Comparison.Type != predicates.ComparisonLessThanOrEq {
		t.Fatalf("expected age <= 18, got %s", got.Explain())
	}
}
