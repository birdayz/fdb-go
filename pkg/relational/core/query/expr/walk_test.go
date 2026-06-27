package expr_test

import (
	"errors"
	"testing"

	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
	"github.com/stretchr/testify/require"
)

// parseFirstWhereExpr walks a SELECT ... WHERE <expr> parse tree
// and returns the first WHERE expression.
func parseFirstWhereExpr(t *testing.T, sql string) antlrgen.IExpressionContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	where := simple.FromClause().WhereExpr()
	if where == nil {
		t.Fatal("no WHERE clause")
	}
	return where.Expression()
}

func TestWalkExpression_BareColumn(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	fv, ok := v.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected *FieldValue, got %T", v)
	}
	if fv.Field != "NAME" {
		t.Fatalf("Field: got %q", fv.Field)
	}
}

func TestWalkExpression_QualifiedColumn(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users u WHERE u.name")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	fv := v.(*values.FieldValue)
	if fv.Field != "NAME" {
		t.Fatalf("qualified column Field: got %q, want NAME", fv.Field)
	}
}

func TestWalkExpression_IntegerLiteral(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE 42")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cv, ok := v.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", v)
	}
	if cv.Value != int64(42) {
		t.Fatalf("Value: got %v, want 42", cv.Value)
	}
	if cv.Typ != values.TypeInt {
		t.Fatalf("Typ: got %v, want TypeInt", cv.Typ)
	}
}

func TestWalkExpression_StringLiteral(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE 'hello'")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cv := v.(*values.ConstantValue)
	if cv.Value != "hello" {
		t.Fatalf("Value: got %v, want 'hello'", cv.Value)
	}
	if cv.Typ != values.TypeString {
		t.Fatalf("Typ: got %v", cv.Typ)
	}
}

// NULL literal → NullValue.
func TestWalkExpression_NullLiteral(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE NULL")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, ok := v.(*values.NullValue); !ok {
		t.Fatalf("expected *NullValue, got %T", v)
	}
}

// Escaped single-quote within a string literal.
func TestWalkExpression_StringLiteral_EscapedQuote(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE 'it''s'")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cv := v.(*values.ConstantValue)
	if cv.Value != "it's" {
		t.Fatalf("Value: got %q, want \"it's\"", cv.Value)
	}
}

// Unsupported shape — compound predicate like `a = b` — falls back
// with a typed error so callers can route to the existing logical-
// builder path.
func TestWalkExpression_Unsupported_BinaryComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id = 1")

	_, err := r.WalkExpression(ctx)
	if err == nil {
		t.Fatal("expected unsupported-shape error for binary comparison")
	}
	var u *expr.UnsupportedExpressionShapeError
	if !errors.As(err, &u) {
		t.Fatalf("expected UnsupportedExpressionShapeError, got %T", err)
	}
}

// Missing column surfaces the analyzer's typed error.
func TestWalkExpression_MissingColumn(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE nonexistent")

	_, err := r.WalkExpression(ctx)
	if err == nil {
		t.Fatal("expected error for nonexistent column")
	}
	var cnf *semantic.ColumnNotFoundError
	if !errors.As(err, &cnf) {
		t.Fatalf("expected ColumnNotFoundError, got %T", err)
	}
}

// --- WalkPredicate --------------------------------------------------

func TestWalkPredicate_Comparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id = 1")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != predicates.ComparisonEquals {
		t.Fatalf("Type: got %v, want Equals", cp.Comparison.Type)
	}
	rhsLit, ok := values.EvaluateConstant(cp.Comparison.Operand)
	if !ok || rhsLit != int64(1) {
		t.Fatalf("Operand: got %v, want 1", cp.Comparison.Operand)
	}
	// Evaluate.
	got, errEv0 := pred.Eval(map[string]any{"ID": int64(1)})
	require.NoError(t, errEv0)
	if got != predicates.TriTrue {
		t.Fatalf("1 = 1: got %v", got)
	}
}

func TestWalkPredicate_ComparisonOperators(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	cases := []struct {
		sql  string
		want predicates.ComparisonType
	}{
		{"SELECT * FROM users WHERE id = 1", predicates.ComparisonEquals},
		{"SELECT * FROM users WHERE id > 1", predicates.ComparisonGreaterThan},
		{"SELECT * FROM users WHERE id < 1", predicates.ComparisonLessThan},
		{"SELECT * FROM users WHERE id >= 1", predicates.ComparisonGreaterThanEq},
		{"SELECT * FROM users WHERE id <= 1", predicates.ComparisonLessThanOrEq},
		{"SELECT * FROM users WHERE id <> 1", predicates.ComparisonNotEquals},
		{"SELECT * FROM users WHERE id != 1", predicates.ComparisonNotEquals},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, tc.sql)
			pred, err := r.WalkPredicate(ctx)
			if err != nil {
				t.Fatalf("walk %q: %v", tc.sql, err)
			}
			cp := pred.(*predicates.ComparisonPredicate)
			if cp.Comparison.Type != tc.want {
				t.Fatalf("Type: got %v, want %v", cp.Comparison.Type, tc.want)
			}
		})
	}
}

func TestWalkPredicate_BareBooleanColumn(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// A bare boolean column lifts to `active = TRUE` (RFC-146 / Java
	// Expression.Utils.toUnderlyingPredicate :399), the SAME
	// ComparisonPredicate `active = TRUE` produces.
	bare, err := r.WalkPredicate(parseFirstWhereExpr(t, "SELECT * FROM users WHERE active"))
	if err != nil {
		t.Fatalf("walk bare: %v", err)
	}
	cp, ok := bare.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", bare)
	}
	if cp.Comparison.Type != predicates.ComparisonEquals {
		t.Fatalf("Type: got %v, want Equals", cp.Comparison.Type)
	}
	if lit, ok := values.EvaluateConstant(cp.Comparison.Operand); !ok || lit != true {
		t.Fatalf("Operand: got %v, want true", cp.Comparison.Operand)
	}

	// Structural unification with the explicit comparison (Graefe's gate):
	// `active` and `active = TRUE` must produce structurally-equal predicates
	// with the same semantic hash, so they unify for index matching / plan
	// shape rather than just rendering the same EXPLAIN string.
	cmp, err := r.WalkPredicate(parseFirstWhereExpr(t, "SELECT * FROM users WHERE active = TRUE"))
	if err != nil {
		t.Fatalf("walk cmp: %v", err)
	}
	if !predicates.PredicateEquals(bare, cmp) {
		t.Fatalf("`active` and `active = TRUE` must unify:\n  bare: %v\n  cmp:  %v", bare, cmp)
	}
	if predicates.SemanticHashCode(bare) != predicates.SemanticHashCode(cmp) {
		t.Fatalf("semantic hashes differ: bare=%d cmp=%d",
			predicates.SemanticHashCode(bare), predicates.SemanticHashCode(cmp))
	}

	// Truthiness eval: active=true keeps the row.
	got, evErr := bare.Eval(map[string]any{"ACTIVE": true})
	if evErr != nil {
		t.Fatalf("eval: %v", evErr)
	}
	if got != predicates.TriTrue {
		t.Fatalf("active=true: got %v, want TriTrue", got)
	}
}

// TestWalkPredicate_BareNull pins `WHERE NULL` → ConstantPredicate(TriUnknown)
// (the row is dropped). The comparison-form lift bypasses the constant-fold
// rule the bare ValuePredicate used to ride, so NULL is folded explicitly at
// the lift site (RFC-146 §3 step 2); without that fold NULL would route to the
// type gate and wrongly 42804.
func TestWalkPredicate_BareNull(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	pred, err := r.WalkPredicate(parseFirstWhereExpr(t, "SELECT * FROM users WHERE NULL"))
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ConstantPredicate)
	if !ok {
		t.Fatalf("expected *ConstantPredicate, got %T", pred)
	}
	got, evErr := cp.Eval(map[string]any{})
	if evErr != nil {
		t.Fatalf("eval: %v", evErr)
	}
	if got != predicates.TriUnknown {
		t.Fatalf("WHERE NULL: got %v, want TriUnknown (drop the row)", got)
	}
}

func TestWalkPredicate_LogicalAnd(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id = 1 AND name = 'bob'")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("AND children: got %d, want 2", len(and.SubPredicates))
	}
}

func TestWalkPredicate_LogicalOr(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id = 1 OR id = 2")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	or, ok := pred.(*predicates.OrPredicate)
	if !ok {
		t.Fatalf("expected *OrPredicate, got %T", pred)
	}
	if len(or.SubPredicates) != 2 {
		t.Fatalf("OR children: got %d, want 2", len(or.SubPredicates))
	}
}

// XOR desugars into `(a OR b) AND NOT (a AND b)`. Verify:
//
//	(1) top-level is AND with two children (the OR and the NOT),
//	(2) Kleene-3VL evaluation matches `a XOR b` truth table.
func TestWalkPredicate_LogicalXor(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE active XOR admin")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate at root, got %T", pred)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("AND children: got %d, want 2", len(and.SubPredicates))
	}
	if _, ok := and.SubPredicates[0].(*predicates.OrPredicate); !ok {
		t.Fatalf("first child: got %T, want *OrPredicate", and.SubPredicates[0])
	}
	if _, ok := and.SubPredicates[1].(*predicates.NotPredicate); !ok {
		t.Fatalf("second child: got %T, want *NotPredicate", and.SubPredicates[1])
	}
	// Pin the Explain output so Simplify / Explain-format regressions
	// surface here. The desugared form is canonical for XOR — if a
	// future XorPredicate type lands, update this. Bare boolean operands
	// lift to `col = TRUE` (RFC-146), as Java's toUnderlyingPredicate does
	// in every predicate position.
	const wantExplain = "((ACTIVE = TRUE OR ADMIN = TRUE) AND NOT (ACTIVE = TRUE AND ADMIN = TRUE))"
	if got := pred.Explain(); got != wantExplain {
		t.Fatalf("Explain:\n  got:  %q\n  want: %q", got, wantExplain)
	}

	type row = map[string]any
	cases := []struct {
		active, admin any
		want          predicates.TriBool
	}{
		{true, true, predicates.TriFalse},
		{true, false, predicates.TriTrue},
		{false, true, predicates.TriTrue},
		{false, false, predicates.TriFalse},
		{nil, true, predicates.TriUnknown},
		{nil, false, predicates.TriUnknown},
		{true, nil, predicates.TriUnknown},
		{false, nil, predicates.TriUnknown},
		{nil, nil, predicates.TriUnknown},
	}
	for _, tc := range cases {
		got, errEv0 := pred.Eval(row{"ACTIVE": tc.active, "ADMIN": tc.admin})
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Errorf("XOR(%v, %v): got %v, want %v", tc.active, tc.admin, got, tc.want)
		}
	}
}

// AND chain flattens — `a AND b AND c` produces a single 3-child
// And rather than nested pairs.
func TestWalkPredicate_AndChainFlattens(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id = 1 AND name = 'bob' AND active")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 3 {
		t.Fatalf("flattened AND children: got %d, want 3", len(and.SubPredicates))
	}
}

// End-to-end: full expression walks through Simplify. `id = 1 AND
// TRUE` → `id = 1` after the AndConstantSimplify rule drops TRUE.
// Tests that the walker output is a first-class citizen of the
// simplifier.
func TestWalkPredicate_FeedsSimplifier(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id = 1 AND 5 = 5")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	simplified := cascades.Simplify(pred, cascades.DefaultSimplifyRules())
	if got, want := simplified.Explain(), "ID = 1"; got != want {
		t.Fatalf("simplified: got %q, want %q", got, want)
	}
}

func TestWalkPredicate_Not(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE NOT active")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, ok := pred.(*predicates.NotPredicate); !ok {
		t.Fatalf("expected *NotPredicate, got %T", pred)
	}
}

func TestWalkPredicate_ParenWrappedComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE (id = 1)")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("parens should unwrap: expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != predicates.ComparisonEquals {
		t.Fatal("Type mismatch")
	}
}

func TestWalkPredicate_NotParenComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE NOT (id = 1)")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Through the simplifier: NOT(id = 1) → id <> 1 via
	// NotComparisonRewriteRule.
	simplified := cascades.Simplify(pred, cascades.DefaultSimplifyRules())
	cp, ok := simplified.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate after simplify, got %T", simplified)
	}
	if cp.Comparison.Type != predicates.ComparisonNotEquals {
		t.Fatalf("expected <>, got %v", cp.Comparison.Type)
	}
}

func TestWalkPredicate_NotAndCombo(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	// `NOT cond1 AND cond2` is left-associative in the grammar, parsed
	// as `(NOT cond1) AND cond2`.
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE NOT active AND id = 1")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("AND children: got %d, want 2", len(and.SubPredicates))
	}
	// First child should be a NOT wrapping a ValuePredicate.
	if _, ok := and.SubPredicates[0].(*predicates.NotPredicate); !ok {
		t.Fatalf("first child: expected NotPredicate, got %T", and.SubPredicates[0])
	}
}

// `NOT (a AND b)` parses via the NotExpression rule whose child is
// the parenthesised LogicalExpression. WalkPredicate handles this
// directly — the parens do NOT surface as a RecordConstructor atom
// because NotExpression.Expression() returns the inner logical node.
// Pins the wiring so a grammar shuffle doesn't silently fall back
// to the unsupported-shape error path.
func TestWalkPredicate_NotParenAnd(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE NOT (id = 1 AND name = 'bob')")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	not, ok := pred.(*predicates.NotPredicate)
	if !ok {
		t.Fatalf("expected *NotPredicate, got %T", pred)
	}
	and, ok := not.Child.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("NOT.Child: expected *AndPredicate, got %T", not.Child)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("inner AND children: got %d, want 2", len(and.SubPredicates))
	}
	// Evaluate the truth table to pin semantics: NOT(TRUE AND TRUE) = FALSE,
	// NOT(TRUE AND FALSE) = TRUE, NOT(FALSE AND _) = TRUE.
	cases := []struct {
		row  map[string]any
		want predicates.TriBool
	}{
		{map[string]any{"ID": int64(1), "NAME": "bob"}, predicates.TriFalse},
		{map[string]any{"ID": int64(1), "NAME": "eve"}, predicates.TriTrue},
		{map[string]any{"ID": int64(2), "NAME": "bob"}, predicates.TriTrue},
	}
	for _, tc := range cases {
		got, errEv0 := pred.Eval(tc.row)
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Errorf("row %v: got %v, want %v", tc.row, got, tc.want)
		}
	}
}

func TestWalkPredicate_Between(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id BETWEEN 1 AND 10")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("BETWEEN → AND(>=, <=): expected AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("AND children: got %d, want 2", len(and.SubPredicates))
	}
	// Eval.
	for _, tc := range []struct {
		id      int64
		want    predicates.TriBool
		comment string
	}{
		{1, predicates.TriTrue, "lower bound inclusive"},
		{5, predicates.TriTrue, "middle"},
		{10, predicates.TriTrue, "upper bound inclusive"},
		{11, predicates.TriFalse, "above"},
		{0, predicates.TriFalse, "below"},
	} {
		got, errEv0 := pred.Eval(map[string]any{"ID": tc.id})
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Fatalf("id=%d (%s): got %v, want %v", tc.id, tc.comment, got, tc.want)
		}
	}
}

func TestWalkPredicate_NotBetween(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id NOT BETWEEN 1 AND 10")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// NOT wraps the AND.
	if _, ok := pred.(*predicates.NotPredicate); !ok {
		t.Fatalf("expected NotPredicate wrapping, got %T", pred)
	}
	// Eval: id=5 is in [1,10] so NOT BETWEEN is FALSE.
	got, errEv0 := pred.Eval(map[string]any{"ID": int64(5)})
	require.NoError(t, errEv0)
	if got != predicates.TriFalse {
		t.Fatalf("5 NOT BETWEEN 1 AND 10: got %v, want FALSE", got)
	}
	got, errEv1 := pred.Eval(map[string]any{"ID": int64(15)})
	require.NoError(t, errEv1)
	if got != predicates.TriTrue {
		t.Fatalf("15 NOT BETWEEN 1 AND 10: got %v, want TRUE", got)
	}
}

func TestWalkPredicate_In(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id IN (1, 2, 3)")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonIn {
		t.Fatalf("Type: got %v, want In", cp.Comparison.Type)
	}
	lit, ok := values.EvaluateConstant(cp.Comparison.Operand)
	if !ok {
		t.Fatalf("Operand not constant: %v", cp.Comparison.Operand)
	}
	list, ok := lit.([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("Operand: got %v", cp.Comparison.Operand)
	}
	// Evaluate.
	for id, want := range map[int64]predicates.TriBool{
		1: predicates.TriTrue,
		2: predicates.TriTrue,
		3: predicates.TriTrue,
		4: predicates.TriFalse,
	} {
		got, errEv0 := pred.Eval(map[string]any{"ID": id})
		require.NoError(t, errEv0)
		if got != want {
			t.Fatalf("id=%d: got %v, want %v", id, got, want)
		}
	}
}

func TestWalkPredicate_NotIn(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id NOT IN (1, 2)")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, ok := pred.(*predicates.NotPredicate); !ok {
		t.Fatalf("expected *NotPredicate, got %T", pred)
	}
}

func TestWalkPredicate_Like(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name LIKE 'hel%'")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonLike {
		t.Fatalf("Type: got %v, want Like", cp.Comparison.Type)
	}
	patLit, ok := values.EvaluateConstant(cp.Comparison.Operand)
	if !ok || patLit != "hel%" {
		t.Fatalf("pattern: got %v", cp.Comparison.Operand)
	}
}

func TestWalkPredicate_NotLike(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name NOT LIKE 'hel%'")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, ok := pred.(*predicates.NotPredicate); !ok {
		t.Fatalf("expected *NotPredicate, got %T", pred)
	}
}

// LIKE with ESCAPE is now supported — verify the escape rune
// reaches the ComparisonPredicate and the matcher honours it.
// `'a\%b' ESCAPE '\'` matches the literal 3-char string `a%b`.
func TestWalkPredicate_LikeEscape(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	// SQL: name LIKE 'a\%b' ESCAPE '\'  — Go raw string keeps the
	// backslash: ESCAPE='\\' is the literal char `\` after the
	// SQL string-literal unescape.
	ctx := parseFirstWhereExpr(t, `SELECT * FROM users WHERE name LIKE 'a\%b' ESCAPE '\'`)
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != predicates.ComparisonLike {
		t.Fatalf("Type: got %v, want Like", cp.Comparison.Type)
	}
	if cp.Comparison.Escape != '\\' {
		t.Fatalf("Escape: got %q, want %q", cp.Comparison.Escape, '\\')
	}

	// Eval truth table — `\%` is a literal `%`, so the pattern
	// matches `a%b` and rejects `axb` (the `\` did NOT mean wildcard).
	cases := []struct {
		s    string
		want predicates.TriBool
	}{
		{"a%b", predicates.TriTrue},
		{"axb", predicates.TriFalse}, // wildcard interpretation would say true; escape blocks it
		{"a%bb", predicates.TriFalse},
	}
	for _, tc := range cases {
		got, errEv0 := pred.Eval(map[string]any{"NAME": tc.s})
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Errorf("Eval(%q): got %v, want %v", tc.s, got, tc.want)
		}
	}
}

// NOT LIKE with ESCAPE — the round-6 reviewer noted this combo
// wasn't pinned. The walker resolves the inner LIKE+ESCAPE then
// wraps in a NotPredicate, so Eval should be the negation of the
// LIKE+ESCAPE truth table.
func TestWalkPredicate_NotLikeEscape(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, `SELECT * FROM users WHERE name NOT LIKE 'a\%b' ESCAPE '\'`)
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	not, ok := pred.(*predicates.NotPredicate)
	if !ok {
		t.Fatalf("expected *NotPredicate, got %T", pred)
	}
	cp, ok := not.Child.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected NOT to wrap ComparisonPredicate, got %T", not.Child)
	}
	if cp.Comparison.Type != predicates.ComparisonLike {
		t.Fatalf("inner Type: got %v, want Like", cp.Comparison.Type)
	}
	if cp.Comparison.Escape != '\\' {
		t.Fatalf("inner Escape: got %q, want %q", cp.Comparison.Escape, '\\')
	}
	// NOT LIKE Eval: the literal `a%b` matches the pattern (so LIKE=TRUE)
	// → NOT LIKE = FALSE; `axb` doesn't match (LIKE=FALSE) → NOT LIKE = TRUE.
	cases := []struct {
		s    string
		want predicates.TriBool
	}{
		{"a%b", predicates.TriFalse},
		{"axb", predicates.TriTrue}, // escape blocks wildcard, so axb is NOT a match for `a\%b`
		{"a%bb", predicates.TriTrue},
	}
	for _, tc := range cases {
		got, errEv0 := pred.Eval(map[string]any{"NAME": tc.s})
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Errorf("NOT LIKE Eval(%q): got %v, want %v", tc.s, got, tc.want)
		}
	}
}

// Multi-character escape is invalid. The walker must reject
// rather than silently consuming only the first char.
func TestWalkPredicate_LikeEscape_MultiChar_Rejected(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, `SELECT * FROM users WHERE name LIKE 'a%' ESCAPE 'XY'`)
	if _, err := r.WalkPredicate(ctx); err == nil {
		t.Fatal("expected error for multi-char ESCAPE")
	}
}

func TestWalkPredicate_IsNull(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name IS NULL")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonIsNull {
		t.Fatalf("Type: got %v, want IsNull", cp.Comparison.Type)
	}
}

// `x IS TRUE` desugars into `(x IS NOT NULL) AND (x = TRUE)` so
// NULL inputs collapse to FALSE (2VL) instead of UNKNOWN (3VL).
// Verify shape + truth table including the nested-NOT case where
// the naive `x = TRUE` form would have diverged.
func TestWalkPredicate_IsTrue(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE admin IS TRUE")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, ok := pred.(*predicates.AndPredicate); !ok {
		t.Fatalf("expected *AndPredicate (IS-NOT-NULL AND =literal), got %T", pred)
	}
	cases := []struct {
		in   any
		want predicates.TriBool
	}{
		{true, predicates.TriTrue},
		{false, predicates.TriFalse},
		{nil, predicates.TriFalse}, // 2VL: NULL IS TRUE → FALSE
	}
	for _, tc := range cases {
		got, errEv0 := pred.Eval(map[string]any{"ADMIN": tc.in})
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Errorf("(%v) IS TRUE: got %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestWalkPredicate_IsFalse(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE admin IS FALSE")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cases := []struct {
		in   any
		want predicates.TriBool
	}{
		{false, predicates.TriTrue},
		{true, predicates.TriFalse},
		{nil, predicates.TriFalse}, // 2VL: NULL IS FALSE → FALSE
	}
	for _, tc := range cases {
		got, errEv0 := pred.Eval(map[string]any{"ADMIN": tc.in})
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Errorf("(%v) IS FALSE: got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// NULL under NOT — the prior `x = TRUE` desugar flipped UNKNOWN to
// UNKNOWN here, diverging from SQL. The new `(IS NOT NULL) AND (=)`
// desugar produces FALSE, making `NOT (NULL IS TRUE)` → TRUE.
func TestWalkPredicate_IsTrue_NegatedNull(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE NOT (admin IS TRUE)")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	got, errEv0 := pred.Eval(map[string]any{"ADMIN": nil})
	require.NoError(t, errEv0)
	if got != predicates.TriTrue {
		t.Errorf("NOT (NULL IS TRUE): got %v, want TRUE", got)
	}
	got, errEv1 := pred.Eval(map[string]any{"ADMIN": true})
	require.NoError(t, errEv1)
	if got != predicates.TriFalse {
		t.Errorf("NOT (TRUE IS TRUE): got %v, want FALSE", got)
	}
	got, errEv2 := pred.Eval(map[string]any{"ADMIN": false})
	require.NoError(t, errEv2)
	if got != predicates.TriTrue {
		t.Errorf("NOT (FALSE IS TRUE): got %v, want TRUE", got)
	}
}

func TestWalkPredicate_IsNotNull(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name IS NOT NULL")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	if cp.Comparison.Type != predicates.ComparisonIsNotNull {
		t.Fatalf("Type: got %v, want IsNotNull", cp.Comparison.Type)
	}
}

// `a XOR a` is a tautology for non-NULL inputs (always FALSE).
// The walker desugars to `(a OR a) AND NOT (a AND a)` and the
// simplifier could fold it, but the walker itself produces the
// structural form — verify Eval returns FALSE for non-NULL and
// UNKNOWN for NULL.
func TestWalkPredicate_XOR_SelfIsFalse(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE active XOR active")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	got, errEv0 := pred.Eval(map[string]any{"ACTIVE": true})
	require.NoError(t, errEv0)
	if got != predicates.TriFalse {
		t.Errorf("true XOR true: got %v, want FALSE", got)
	}
	got, errEv1 := pred.Eval(map[string]any{"ACTIVE": false})
	require.NoError(t, errEv1)
	if got != predicates.TriFalse {
		t.Errorf("false XOR false: got %v, want FALSE", got)
	}
	got, errEv2 := pred.Eval(map[string]any{"ACTIVE": nil})
	require.NoError(t, errEv2)
	if got != predicates.TriUnknown {
		t.Errorf("NULL XOR NULL: got %v, want UNKNOWN", got)
	}
}

func TestWalkPredicate_NilContext(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	if _, err := r.WalkPredicate(nil); err == nil {
		t.Fatal("expected error for nil context")
	}
}

// Arithmetic atoms: `a + b`, `a * b`, etc.
func TestWalkExpression_Arithmetic(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	cases := []struct {
		sql string
		op  values.ArithmeticOp
	}{
		{"SELECT * FROM users WHERE id + 1", values.OpAdd},
		{"SELECT * FROM users WHERE id - 1", values.OpSub},
		{"SELECT * FROM users WHERE id * 2", values.OpMul},
		{"SELECT * FROM users WHERE id / 2", values.OpDiv},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, tc.sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			av, ok := v.(*values.ArithmeticValue)
			if !ok {
				t.Fatalf("expected *ArithmeticValue, got %T", v)
			}
			if av.Op != tc.op {
				t.Fatalf("Op: got %v, want %v", av.Op, tc.op)
			}
		})
	}
}

// Nested arithmetic: `(a + 1) * 2` — parens wrap the inner sum.
func TestWalkExpression_Arithmetic_Nested(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE (id + 1) * 2")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	outer, ok := v.(*values.ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", v)
	}
	if outer.Op != values.OpMul {
		t.Fatalf("outer Op: got %v, want OpMul", outer.Op)
	}
	inner, ok := outer.Left.(*values.ArithmeticValue)
	if !ok {
		t.Fatalf("inner: expected *ArithmeticValue, got %T", outer.Left)
	}
	if inner.Op != values.OpAdd {
		t.Fatalf("inner Op: got %v, want OpAdd", inner.Op)
	}
}

// Aggregate function calls: COUNT(*), COUNT(col), SUM, MIN, MAX, AVG.
func TestWalkExpression_AggregateFunctions(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	cases := []struct {
		sql  string
		want values.AggregateOp
	}{
		{"SELECT * FROM users WHERE COUNT(*)", values.AggCountStar},
		{"SELECT * FROM users WHERE COUNT(id)", values.AggCount},
		{"SELECT * FROM users WHERE SUM(id)", values.AggSum},
		{"SELECT * FROM users WHERE MIN(id)", values.AggMin},
		{"SELECT * FROM users WHERE MAX(id)", values.AggMax},
		{"SELECT * FROM users WHERE AVG(id)", values.AggAvg},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, tc.sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			av, ok := v.(*values.AggregateValue)
			if !ok {
				t.Fatalf("expected *AggregateValue, got %T", v)
			}
			if av.Op != tc.want {
				t.Fatalf("Op: got %v, want %v", av.Op, tc.want)
			}
		})
	}
}

// End-to-end: a full WHERE clause → walker → simplifier → evaluate
// with multiple constant folds + survivors.
func TestWalker_E2E_SimplifyRichTree(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// `WHERE (5 = 5 OR name IS NULL) AND id > 0 AND TRUE`
	// Simplifier should:
	//   - Fold `5 = 5` → TRUE → OR(TRUE, ...) → TRUE → drop from AND.
	//   - Keep `id > 0` (opaque).
	//   - Fold `TRUE` → drop from AND.
	// Final: `id > 0`.
	ctx := parseFirstWhereExpr(t,
		"SELECT * FROM users WHERE (5 = 5 OR name IS NULL) AND id > 0 AND TRUE")
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	simplified := cascades.Simplify(pred, cascades.DefaultSimplifyRules())
	if got, want := simplified.Explain(), "ID > 0"; got != want {
		t.Fatalf("simplified: got %q, want %q", got, want)
	}
}

// End-to-end integration: a compound WHERE walks through the
// resolver, produces a predicate tree, and evaluates correctly.
func TestWalker_E2E_Integration(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	// `WHERE id >= 1 AND id <= 10 AND name IS NOT NULL` — range +
	// NOT NULL. Opaque-field predicates, simplifier leaves shape.
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id >= 1 AND id <= 10 AND name IS NOT NULL")
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 3 {
		t.Fatalf("children: got %d, want 3", len(and.SubPredicates))
	}

	// Evaluate against some rows.
	row := map[string]any{"ID": int64(5), "NAME": "bob"}
	got, errEv0 := pred.Eval(row)
	require.NoError(t, errEv0)
	if got != predicates.TriTrue {
		t.Fatalf("id=5, name=bob: got %v, want TRUE", got)
	}
	row["NAME"] = nil
	got, errEv1 := pred.Eval(row)
	require.NoError(t, errEv1)
	if got != predicates.TriFalse {
		t.Fatalf("id=5, name=NULL: got %v, want FALSE", got)
	}
	row["NAME"] = "bob"
	row["ID"] = int64(11)
	got, errEv2 := pred.Eval(row)
	require.NoError(t, errEv2)
	if got != predicates.TriFalse {
		t.Fatalf("id=11, name=bob: got %v, want FALSE", got)
	}
}

func TestWalkExpression_NilContext(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	if _, err := r.WalkExpression(nil); err == nil {
		t.Fatal("expected error for nil context")
	}
}

// Float literal → ConstantValue{Typ: TypeFloat}. Walker handles
// `3.14`, `0.5`, scientific notation, and negative forms via
// DecimalConstant + NegativeDecimalConstant dispatch on
// REAL_LITERAL terminal.
func TestWalkExpression_FloatLiteral(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	cases := map[string]float64{
		"3.14":    3.14,
		"0.5":     0.5,
		"-2.5":    -2.5,
		"1e2":     100,
		"-1.5e10": -1.5e10,
	}
	for sql, want := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE "+sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk %q: %v", sql, err)
			}
			cv, ok := v.(*values.ConstantValue)
			if !ok {
				t.Fatalf("expected *ConstantValue, got %T", v)
			}
			if cv.Typ != values.TypeFloat {
				t.Fatalf("Typ: got %v, want TypeFloat", cv.Typ)
			}
			if cv.Value != want {
				t.Fatalf("Value: got %v, want %v", cv.Value, want)
			}
		})
	}
}

// DIV operator — `a DIV b` is MySQL's integer-truncated division.
// At the seed it shares OpDiv with `/` because ArithmeticValue.Evaluate
// is int64-only and Go's `/` on int64 already truncates toward zero.
// Once float arithmetic lands DIV will need a distinct op. The test
// pins the dispatch + evaluation parity; if a future change splits
// the op, this test breaks loudly rather than silently changing
// truncation semantics.
func TestWalkExpression_IntegerDivOperator(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	for _, sql := range []string{"id / 7", "id DIV 7"} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE "+sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			av, ok := v.(*values.ArithmeticValue)
			if !ok {
				t.Fatalf("expected *ArithmeticValue, got %T", v)
			}
			if av.Op != values.OpDiv {
				t.Fatalf("Op: got %v, want OpDiv", av.Op)
			}
			// 23 / 7 → 3 (truncated toward zero).
			got, errEv0 := av.Evaluate(map[string]any{"ID": int64(23)})
			require.NoError(t, errEv0)
			if got != int64(3) {
				t.Errorf("23/7: got %v, want 3", got)
			}
			// -23 / 7 → -3 (Go truncates toward zero).
			got, errEv1 := av.Evaluate(map[string]any{"ID": int64(-23)})
			require.NoError(t, errEv1)
			if got != int64(-3) {
				t.Errorf("-23/7: got %v, want -3", got)
			}
			// Divide by zero → ArithmeticDivisionByZeroError on the error
			// channel (matches Java's ArithmeticException; executor maps 22012).
			divZero := &values.ArithmeticValue{
				Op:    values.OpDiv,
				Left:  &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
				Right: &values.ConstantValue{Value: int64(0), Typ: values.TypeInt},
			}
			if v, err := divZero.Evaluate(nil); v != nil || err == nil {
				t.Errorf("5/0: got (%v, %v), want (nil, ArithmeticDivisionByZeroError)", v, err)
			} else {
				var divByZero *values.ArithmeticDivisionByZeroError
				if !errors.As(err, &divByZero) {
					t.Errorf("5/0: got %T, want *ArithmeticDivisionByZeroError", err)
				}
			}
		})
	}
}

// MOD operator — walker produces an ArithmeticValue with Op=OpMod
// for both `a % b` and `a MOD b` syntactic forms. Eval returns
// truncated-toward-zero modulo (Go's `%`); MOD by zero returns nil
// (NULL-at-Value-layer).
func TestWalkExpression_ModuloOperator(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	for _, sql := range []string{"id % 7", "id MOD 7"} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE "+sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			av, ok := v.(*values.ArithmeticValue)
			if !ok {
				t.Fatalf("expected *ArithmeticValue, got %T", v)
			}
			if av.Op != values.OpMod {
				t.Fatalf("Op: got %v, want OpMod", av.Op)
			}
			got, errEv0 := av.Evaluate(map[string]any{"ID": int64(23)})
			require.NoError(t, errEv0)
			if got != int64(2) {
				t.Errorf("23 %% 7: got %v, want 2", got)
			}
		})
	}
}

// CAST(col AS INTEGER): walker produces a CastValue over the column
// FieldValue. Target ValueType mirrors the primitive-type token. The
// CAST shape is wired via SpecificFunction → DataTypeFunctionCall.
func TestWalkExpression_CastInteger(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE CAST(name AS INTEGER)")
	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cv, ok := v.(*values.CastValue)
	if !ok {
		t.Fatalf("expected *CastValue, got %T", v)
	}
	if cv.Target != values.NullableInt {
		t.Fatalf("Target: got %v, want NullableInt", cv.Target)
	}
	fv, ok := cv.Child.(*values.FieldValue)
	if !ok || fv.Field != "NAME" {
		t.Fatalf("Child: got %v", cv.Child)
	}
}

// CAST to STRING + BOOLEAN also compose; BIGINT aliases INTEGER.
func TestWalkExpression_CastTargets(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	cases := map[string]values.Type{
		"CAST(name AS STRING)":  values.TypeString,
		"CAST(name AS BOOLEAN)": values.TypeBool,
		"CAST(name AS BIGINT)":  values.TypeInt,
	}
	for sql, want := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE "+sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			cv := v.(*values.CastValue)
			if cv.Target != want {
				t.Fatalf("%q: got %v, want %v", sql, cv.Target, want)
			}
		})
	}
}

// CAST to a type outside the seed ValueType — FLOAT / DOUBLE /
// BYTES / UUID / VECTOR — returns UnsupportedExpressionShapeError.
// Waits on the Phase 4.0 Type hierarchy port.
func TestWalkExpression_CastUnsupportedTarget(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	cases := []string{
		// FLOAT / DOUBLE moved to TestWalkExpression_CastFloat now
		// that TypeFloat exists in the seed enum. BYTES still
		// declines pending the full Type hierarchy port.
		"CAST(name AS BYTES)",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE "+sql)
			if _, err := r.WalkExpression(ctx); err == nil {
				t.Fatalf("expected UnsupportedExpressionShapeError for %q", sql)
			}
		})
	}
}

// CAST AS FLOAT / DOUBLE → CastValue{Target: TypeFloat}. Walker now
// supports float casts after TypeFloat landed.
func TestWalkExpression_CastFloat(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	for _, sql := range []string{"CAST(name AS FLOAT)", "CAST(name AS DOUBLE)"} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE "+sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			cv, ok := v.(*values.CastValue)
			if !ok {
				t.Fatalf("expected *CastValue, got %T", v)
			}
			if cv.Target != values.TypeFloat {
				t.Fatalf("Target: got %v, want TypeFloat", cv.Target)
			}
		})
	}
}

// CONVERT(expr, type) is the alternate surface for the same
// DataTypeFunctionCall production. Walker shares one path so the
// resulting CastValue is identical to CAST(expr AS type).
func TestWalkExpression_ConvertSyntax(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE CONVERT(name, INTEGER)")
	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cv, ok := v.(*values.CastValue)
	if !ok {
		t.Fatalf("expected *CastValue, got %T", v)
	}
	if cv.Target != values.NullableInt {
		t.Fatalf("Target: got %v, want NullableInt", cv.Target)
	}
}

// CAST inside a WHERE predicate — real-world shape. Walker resolves
// the CAST into a CastValue then composes with ComparisonPredicate.
func TestWalkPredicate_CastInComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE CAST(name AS INTEGER) = 42")
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if _, ok := cp.Operand.(*values.CastValue); !ok {
		t.Fatalf("expected Operand to be *CastValue, got %T", cp.Operand)
	}
}

// UPPER / LOWER / LENGTH / CHAR_LENGTH / OCTET_LENGTH dispatch via
// walkScalarFunction → ScalarFunctionValue. Verifies result type +
// arg composition for each seed function.
func TestWalkExpression_ScalarFunctions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sql  string
		fn   string
		typ  values.Type
		args int
	}{
		{"SELECT * FROM users WHERE UPPER(name)", "UPPER", values.TypeString, 1},
		{"SELECT * FROM users WHERE LOWER(name)", "LOWER", values.TypeString, 1},
		{"SELECT * FROM users WHERE LENGTH(name)", "LENGTH", values.TypeInt, 1},
		{"SELECT * FROM users WHERE CHAR_LENGTH(name)", "CHAR_LENGTH", values.TypeInt, 1},
		{"SELECT * FROM users WHERE CHARACTER_LENGTH(name)", "CHARACTER_LENGTH", values.TypeInt, 1},
		{"SELECT * FROM users WHERE OCTET_LENGTH(name)", "OCTET_LENGTH", values.TypeInt, 1},
	}
	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			a, s := buildScope(t)
			r := expr.New(a, s)
			ctx := parseFirstWhereExpr(t, tc.sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			sf, ok := v.(*values.ScalarFunctionValue)
			if !ok {
				t.Fatalf("expected *ScalarFunctionValue, got %T", v)
			}
			if sf.FuncName != tc.fn {
				t.Fatalf("FuncName: got %q, want %q", sf.FuncName, tc.fn)
			}
			if sf.Type().Code() != tc.typ.Code() {
				t.Fatalf("Type code: got %v, want %v", sf.Type().Code(), tc.typ.Code())
			}
			if got := len(sf.Args); got != tc.args {
				t.Fatalf("len(Args): got %d, want %d", got, tc.args)
			}
		})
	}
}

// Extended scalar-function set added in swingshift-50: ABS / FLOOR /
// CEIL / CEILING / ROUND, SQRT / POWER / POW, COALESCE / NULLIF,
// TRIM / LTRIM / RTRIM, CONCAT, SUBSTRING / SUBSTR, REPLACE. The
// walker now recognises these names and the catalog-aware builder
// produces a real ScalarFunctionValue carrying their args. Polymorphic
// returns (ABS / FLOOR / CEIL / CEILING / ROUND / COALESCE / NULLIF)
// surface as TypeUnknown until the Type hierarchy port lands real
// per-arg type inference.
func TestWalkExpression_ScalarFunctionsExtended(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sql  string
		fn   string
		typ  values.Type
		args int
	}{
		// RFC-082: polymorphic value-preserving functions now infer their
		// result type from the operand (id is LONG) instead of UNKNOWN.
		{"SELECT * FROM users WHERE ABS(id)", "ABS", values.NullableLong, 1},
		{"SELECT * FROM users WHERE FLOOR(id)", "FLOOR", values.NullableLong, 1},
		{"SELECT * FROM users WHERE CEIL(id)", "CEIL", values.NullableLong, 1},
		{"SELECT * FROM users WHERE CEILING(id)", "CEILING", values.NullableLong, 1},
		{"SELECT * FROM users WHERE ROUND(id)", "ROUND", values.NullableLong, 1},
		{"SELECT * FROM users WHERE ROUND(id, 2)", "ROUND", values.NullableLong, 2},
		{"SELECT * FROM users WHERE SQRT(id)", "SQRT", values.TypeFloat, 1},
		{"SELECT * FROM users WHERE POWER(id, 2)", "POWER", values.TypeFloat, 2},
		{"SELECT * FROM users WHERE POW(id, 2)", "POW", values.TypeFloat, 2},
		{"SELECT * FROM users WHERE COALESCE(name, 'default')", "COALESCE", values.TypeString, 2},
		{"SELECT * FROM users WHERE NULLIF(name, 'admin')", "NULLIF", values.TypeString, 2},
		{"SELECT * FROM users WHERE TRIM(name)", "TRIM", values.TypeString, 1},
		{"SELECT * FROM users WHERE LTRIM(name)", "LTRIM", values.TypeString, 1},
		{"SELECT * FROM users WHERE RTRIM(name)", "RTRIM", values.TypeString, 1},
		{"SELECT * FROM users WHERE CONCAT(name, '_v2')", "CONCAT", values.TypeString, 2},
		{"SELECT * FROM users WHERE SUBSTRING(name, 1, 3)", "SUBSTRING", values.TypeString, 3},
		{"SELECT * FROM users WHERE SUBSTR(name, 1)", "SUBSTR", values.TypeString, 2},
		{"SELECT * FROM users WHERE REPLACE(name, 'a', 'b')", "REPLACE", values.TypeString, 3},
		// swingshift-50 additions: walker must build ScalarFunctionValue
		// for the new fn names so the cascades fold path can fire.
		{"SELECT * FROM users WHERE LEN(name)", "LEN", values.TypeInt, 1},
		{"SELECT * FROM users WHERE CONCAT_WS('-', name, name)", "CONCAT_WS", values.TypeString, 3},
		// PI() is a zero-arg function — exercises the no-args branch
		// of the function-call walker (Java's `PI()` parse shape).
		{"SELECT * FROM users WHERE PI()", "PI", values.TypeFloat, 0},
	}
	for _, tc := range cases {
		t.Run(tc.fn+"_"+tc.sql, func(t *testing.T) {
			t.Parallel()
			a, s := buildScope(t)
			r := expr.New(a, s)
			ctx := parseFirstWhereExpr(t, tc.sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			sf, ok := v.(*values.ScalarFunctionValue)
			if !ok {
				t.Fatalf("expected *ScalarFunctionValue, got %T", v)
			}
			if sf.FuncName != tc.fn {
				t.Fatalf("FuncName: got %q, want %q", sf.FuncName, tc.fn)
			}
			if sf.Type().Code() != tc.typ.Code() {
				t.Fatalf("Type code: got %v, want %v", sf.Type().Code(), tc.typ.Code())
			}
			if got := len(sf.Args); got != tc.args {
				t.Fatalf("len(Args): got %d, want %d", got, tc.args)
			}
		})
	}
}

// Unknown scalar function declines so the logical-builder text
// fallback can carry it.
func TestWalkExpression_UnknownScalarFunctionDeclines(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE FROBNICATE(name)")
	_, err := r.WalkExpression(ctx)
	var ue *expr.UnsupportedExpressionShapeError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UnsupportedExpressionShapeError, got %v", err)
	}
}

// Lower-cased function names are normalized to upper-case (SQL
// identifier semantics: function names are case-insensitive).
func TestWalkExpression_ScalarFunctionLowerCase(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE upper(name)")
	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sf := v.(*values.ScalarFunctionValue)
	if sf.FuncName != "UPPER" {
		t.Fatalf("FuncName: got %q, want UPPER", sf.FuncName)
	}
}

// UPPER(name) = 'ALICE' — scalar function on the LHS of a
// comparison. Composes with ResolveComparison since LHS is a Value.
// Constant-fold declines because UPPER(field) isn't constant.
func TestWalkPredicate_ScalarFunctionInComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE UPPER(name) = 'ALICE'")
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if _, ok := cp.Operand.(*values.ScalarFunctionValue); !ok {
		t.Fatalf("expected LHS *ScalarFunctionValue, got %T", cp.Operand)
	}
	want := "UPPER(NAME) = 'ALICE'"
	if got := cp.Explain(); got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}

// `?` in a WHERE expression resolves to a positional ParameterValue.
// Verifies the PreparedStatementParameterAtomContext dispatch.
func TestWalkExpression_PositionalParameter(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE ?")
	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	pv, ok := v.(*values.ParameterValue)
	if !ok {
		t.Fatalf("expected *ParameterValue, got %T", v)
	}
	// Walker assigns 1-based ordinal in declaration order.
	if pv.Ordinal != 1 || pv.ParamName != "" {
		t.Fatalf("got Ordinal=%d ParamName=%q", pv.Ordinal, pv.ParamName)
	}
}

// `?foo` in a WHERE expression resolves to a named ParameterValue.
// (Grammar rule: NAMED_PARAMETER: [?$][A-Za-z][A-Za-z0-9_/]*.)
func TestWalkExpression_NamedParameter(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE ?foo")
	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	pv, ok := v.(*values.ParameterValue)
	if !ok {
		t.Fatalf("expected *ParameterValue, got %T", v)
	}
	if pv.ParamName != "foo" || pv.Ordinal != 0 {
		t.Fatalf("got Ordinal=%d ParamName=%q", pv.Ordinal, pv.ParamName)
	}
}

// `name = ?` composes through ResolveComparison: LHS FieldValue, RHS
// ParameterValue. Constant-fold declines because RHS is non-constant.
func TestWalkPredicate_ParameterizedComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name = ?")
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if _, ok := cp.Operand.(*values.FieldValue); !ok {
		t.Fatalf("expected LHS *FieldValue, got %T", cp.Operand)
	}
	pv, ok := cp.Comparison.Operand.(*values.ParameterValue)
	if !ok {
		t.Fatalf("expected RHS *ParameterValue, got %T", cp.Comparison.Operand)
	}
	if pv.Ordinal != 1 {
		t.Fatalf("Ordinal: got %d, want 1", pv.Ordinal)
	}
	// Plan-cache key seam: render the predicate as `NAME = ?1` so two
	// queries with different bind values share the same Explain.
	want := "NAME = ?1"
	if got := cp.Explain(); got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}

// Two `?` in the same statement get distinct 1-based ordinals so
// ExplainValue / plan-cache keying disambiguates `WHERE x=?` from
// `WHERE x=? AND y=?`. The Resolver's nextOrdinal counter is the
// seam — same Resolver across both walks → continues numbering.
func TestWalkPredicate_MultiplePositionalParameters(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE id = ? AND name = ?")
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("SubPredicates: got %d, want 2", len(and.SubPredicates))
	}
	got := []int{}
	for _, sp := range and.SubPredicates {
		cp := sp.(*predicates.ComparisonPredicate)
		pv := cp.Comparison.Operand.(*values.ParameterValue)
		got = append(got, pv.Ordinal)
	}
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("ordinals: got %v, want [1 2]", got)
	}
	want := "(ID = ?1 AND NAME = ?2)"
	if exp := and.Explain(); exp != want {
		t.Fatalf("Explain: got %q, want %q", exp, want)
	}
}

// `name = ?user` — same composition for a named parameter.
func TestWalkPredicate_NamedParameterizedComparison(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name = ?user")
	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp := pred.(*predicates.ComparisonPredicate)
	pv, ok := cp.Comparison.Operand.(*values.ParameterValue)
	if !ok {
		t.Fatalf("expected RHS *ParameterValue, got %T", cp.Comparison.Operand)
	}
	if pv.ParamName != "user" {
		t.Fatalf("ParamName: got %q, want 'user'", pv.ParamName)
	}
	if got, want := cp.Explain(), "NAME = ?user"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}
