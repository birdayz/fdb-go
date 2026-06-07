package cascades

// Tests for ComparisonConstantSimplifyRule. Live in root cascades/
// because the rule itself does (rule_simplify.go); they reach into
// predicate + value subpackages via qualified imports. Originally
// authored alongside ComparisonPredicate in comparisons_test.go;
// extracted during the cascades package split (RFC-025) since rule
// tests can't sit in `package predicates` without an import cycle.

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Plan-time constant-fold must NOT fire when the RHS is non-constant
// — `col = field` cannot be decided at plan time. The simplifier
// rule gates on values.IsConstantValue(RHS) per the values.Value migration.
func TestComparisonConstantSimplify_NonConstantRHS_NoFold(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := predicates.NewComparisonPredicate(
		&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.FieldValue{Field: "col", Typ: values.TypeInt}},
	)
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected no yield (non-constant RHS), got %d", len(got))
	}
}

// Swallow-axis (RFC-087 Graefe gate): `WHERE 5 = 'abc'` is a both-constant
// comparison whose Eval raises a type-mismatch on the error channel. The
// rule must DECLINE to fold — yield nothing, leaving the predicate intact —
// rather than crashing or surfacing the error from the planner. At runtime
// the predicate then evaluates to its normal three-valued result (no rows),
// not a crash. Complements the propagate edges pinned in the values package.
func TestComparisonConstantSimplify_TypeMismatch_DeclinesToFold(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := predicates.NewComparisonPredicate(
		&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: "abc", Typ: values.TypeString}},
	)
	got := FireRule(rule, pred)
	if len(got) != 0 {
		t.Fatalf("WHERE 5 = 'abc': expected no yield (type-mismatch declines to fold), got %d: %v", len(got), got)
	}
}

// Deeply-nested constant trees fold end-to-end: a values.CastValue
// wrapping a values.ConstantValue, inside an values.ArithmeticValue, on the RHS
// of a constant-LHS comparison. Pins that constantLiteral's
// fall-through to values.EvaluateConstant correctly recurses through
// composites without bailing on intermediate non-leaf shapes.
func TestComparisonConstantSimplify_DeeplyNestedConstants_Folds(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	// `5 = CAST(5 AS INT) + 0`
	rhs := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  values.NewCastValue(&values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, values.TypeInt),
		Right: &values.ConstantValue{Value: int64(0), Typ: values.TypeInt},
	}
	pred := predicates.NewComparisonPredicate(
		&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: rhs},
	)
	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T", got[0])
	}
	if cp.Value != predicates.TriTrue {
		t.Fatalf("5 = CAST(5 AS INT) + 0: got %v, want TRUE", cp.Value)
	}
}

// Plan-time constant-fold fires when BOTH sides are constant. Pins
// the values.Value-wrapped RHS variant: `5 = 5` folds to TRUE regardless
// of whether the RHS is a raw literal or a values.ConstantValue.
func TestComparisonConstantSimplify_ConstantValueRHS_Folds(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := predicates.NewComparisonPredicate(
		&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5), Typ: values.TypeInt}},
	)
	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T", got[0])
	}
	if cp.Value != predicates.TriTrue {
		t.Fatalf("5=5 should be TRUE, got %v", cp.Value)
	}
}

// Composite-constant LHS now folds at plan time. Before this
// pass, constantLiteral only recognised *values.ConstantValue / *values.NullValue /
// *values.BooleanValue and missed `CAST(5 AS STRING)` (a values.CastValue with a
// constant child). Now constantLiteral falls through to
// values.EvaluateConstant so composite-constant LHS like CAST / arithmetic
// over literals folds correctly.
func TestComparisonConstantSimplify_CompositeConstantLHS_Folds(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	// CAST(5 AS INT) is a composite constant — child values.ConstantValue is
	// constant, so the whole values.CastValue evaluates without a row.
	lhs := values.NewCastValue(&values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, values.TypeInt)
	pred := predicates.NewComparisonPredicate(
		lhs,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5), Typ: values.TypeInt}},
	)
	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield (composite-constant fold), got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T", got[0])
	}
	if cp.Value != predicates.TriTrue {
		t.Fatalf("CAST(5 AS INT) = 5: got %v, want TRUE", cp.Value)
	}
}

// LIKE / STARTS_WITH / IN with both sides constant fold all the way
// to a ConstantPredicate end-to-end. Pins that the constant-fold
// rule covers each comparison family — easy to silently break by
// adding a new ComparisonType to the dispatch without wiring fold
// support, and these checks would catch it.
func TestSimplify_StringPredicates_FoldEndToEnd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pred *predicates.ComparisonPredicate
		want predicates.TriBool
	}{
		{
			name: "LIKE matches",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: "hello", Typ: values.TypeString},
				predicates.Comparison{Type: predicates.ComparisonLike, Operand: values.LiteralValue("hel%")},
			),
			want: predicates.TriTrue,
		},
		{
			name: "LIKE doesn't match",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: "foo", Typ: values.TypeString},
				predicates.Comparison{Type: predicates.ComparisonLike, Operand: values.LiteralValue("bar")},
			),
			want: predicates.TriFalse,
		},
		{
			name: "LIKE+ESCAPE",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: "a%b", Typ: values.TypeString},
				predicates.Comparison{Type: predicates.ComparisonLike, Operand: values.LiteralValue(`a\%b`), Escape: '\\'},
			),
			want: predicates.TriTrue,
		},
		{
			name: "STARTS_WITH matches",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: "hello", Typ: values.TypeString},
				predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue("hel")},
			),
			want: predicates.TriTrue,
		},
		{
			name: "IN matches",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
				predicates.Comparison{Type: predicates.ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(5), int64(9)})},
			),
			want: predicates.TriTrue,
		},
		{
			name: "IN doesn't match",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: int64(99), Typ: values.TypeInt},
				predicates.Comparison{Type: predicates.ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2)})},
			),
			want: predicates.TriFalse,
		},
		{
			name: "bytes equality matches",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: []byte{0x01, 0x02}, Typ: values.TypeUnknown},
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue([]byte{0x01, 0x02})},
			),
			want: predicates.TriTrue,
		},
		{
			name: "bytes equality differs",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: []byte{0x01}, Typ: values.TypeUnknown},
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue([]byte{0x02})},
			),
			want: predicates.TriFalse,
		},
	}
	rule := NewComparisonConstantSimplifyRule()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FireRule(rule, tc.pred)
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

// IS [NOT] DISTINCT FROM with both sides constant folds at plan
// time. Pin all four corners of the null-safety truth table:
//   - NULL IS DISTINCT FROM NULL → FALSE (NOT distinct, both NULL)
//   - 5 IS NOT DISTINCT FROM 5 → TRUE (equal)
//   - NULL IS DISTINCT FROM 5 → TRUE (one is NULL, one isn't)
//   - 5 IS NOT DISTINCT FROM NULL → FALSE
//
// IS [NOT] DISTINCT FROM is the SQL null-safe equality / inequality
// — always resolves to TRUE/FALSE, never UNKNOWN. Catches a future
// regression where the constant-fold rule narrows its dispatch
// table and stops handling these types.
func TestSimplify_IsDistinctFrom_FoldsEndToEnd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		lhs  values.Value
		op   predicates.ComparisonType
		rhs  values.Value
		want predicates.TriBool
	}{
		{
			"NULL IS DISTINCT FROM NULL",
			&values.NullValue{Typ: values.TypeUnknown}, predicates.ComparisonIsDistinctFrom, &values.NullValue{Typ: values.TypeUnknown},
			predicates.TriFalse,
		},
		{
			"5 IS NOT DISTINCT FROM 5",
			&values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, predicates.ComparisonNotDistinctFrom, &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
			predicates.TriTrue,
		},
		{
			"NULL IS DISTINCT FROM 5",
			&values.NullValue{Typ: values.TypeUnknown}, predicates.ComparisonIsDistinctFrom, &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
			predicates.TriTrue,
		},
		{
			"5 IS NOT DISTINCT FROM NULL",
			&values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, predicates.ComparisonNotDistinctFrom, &values.NullValue{Typ: values.TypeUnknown},
			predicates.TriFalse,
		},
	}
	rule := NewComparisonConstantSimplifyRule()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pred := predicates.NewComparisonPredicate(tc.lhs, predicates.Comparison{Type: tc.op, Operand: tc.rhs})
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

// CAST(5 AS FLOAT) > 3.14 — composite-constant LHS via values.CastValue
// over an int. values.CastValue.Evaluate now handles values.TypeFloat, so the
// constant-fold rule unwraps to float64(5) and compares against
// 3.14 → TRUE. Round-12 reviewer flagged the missing values.TypeFloat
// case in values.CastValue.Evaluate which silently produced UNKNOWN.
func TestComparisonConstantSimplify_CastFloat_Folds(t *testing.T) {
	t.Parallel()
	rule := NewComparisonConstantSimplifyRule()
	pred := predicates.NewComparisonPredicate(
		values.NewCastValue(&values.ConstantValue{Value: int64(5), Typ: values.TypeInt}, values.TypeFloat),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(float64(3.14))},
	)
	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T", got[0])
	}
	if cp.Value != predicates.TriTrue {
		t.Fatalf("CAST(5 AS FLOAT) > 3.14: got %v, want TRUE", cp.Value)
	}
}
