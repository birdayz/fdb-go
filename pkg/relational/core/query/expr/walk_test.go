package expr_test

import (
	"errors"
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
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
	fv, ok := v.(*cascades.FieldValue)
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
	fv := v.(*cascades.FieldValue)
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
	cv, ok := v.(*cascades.ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", v)
	}
	if cv.Value != int64(42) {
		t.Fatalf("Value: got %v, want 42", cv.Value)
	}
	if cv.Typ != cascades.TypeInt {
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
	cv := v.(*cascades.ConstantValue)
	if cv.Value != "hello" {
		t.Fatalf("Value: got %v, want 'hello'", cv.Value)
	}
	if cv.Typ != cascades.TypeString {
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
	if _, ok := v.(*cascades.NullValue); !ok {
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
	cv := v.(*cascades.ConstantValue)
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
	cp, ok := pred.(*cascades.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != cascades.ComparisonEquals {
		t.Fatalf("Type: got %v, want Equals", cp.Comparison.Type)
	}
	if cp.Comparison.Operand != int64(1) {
		t.Fatalf("Operand: got %v, want 1", cp.Comparison.Operand)
	}
	// Evaluate.
	if got := pred.Eval(map[string]any{"ID": int64(1)}); got != cascades.TriTrue {
		t.Fatalf("1 = 1: got %v", got)
	}
}

func TestWalkPredicate_ComparisonOperators(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	cases := []struct {
		sql  string
		want cascades.ComparisonType
	}{
		{"SELECT * FROM users WHERE id = 1", cascades.ComparisonEquals},
		{"SELECT * FROM users WHERE id > 1", cascades.ComparisonGreaterThan},
		{"SELECT * FROM users WHERE id < 1", cascades.ComparisonLessThan},
		{"SELECT * FROM users WHERE id >= 1", cascades.ComparisonGreaterThanEq},
		{"SELECT * FROM users WHERE id <= 1", cascades.ComparisonLessThanOrEq},
		{"SELECT * FROM users WHERE id <> 1", cascades.ComparisonNotEquals},
		{"SELECT * FROM users WHERE id != 1", cascades.ComparisonNotEquals},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, tc.sql)
			pred, err := r.WalkPredicate(ctx)
			if err != nil {
				t.Fatalf("walk %q: %v", tc.sql, err)
			}
			cp := pred.(*cascades.ComparisonPredicate)
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
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE active")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Bare column predicate → ValuePredicate.
	if _, ok := pred.(*cascades.ValuePredicate); !ok {
		t.Fatalf("expected *ValuePredicate, got %T", pred)
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
	and, ok := pred.(*cascades.AndPredicate)
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
	or, ok := pred.(*cascades.OrPredicate)
	if !ok {
		t.Fatalf("expected *OrPredicate, got %T", pred)
	}
	if len(or.SubPredicates) != 2 {
		t.Fatalf("OR children: got %d, want 2", len(or.SubPredicates))
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
	and, ok := pred.(*cascades.AndPredicate)
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
	if _, ok := pred.(*cascades.NotPredicate); !ok {
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
	cp, ok := pred.(*cascades.ComparisonPredicate)
	if !ok {
		t.Fatalf("parens should unwrap: expected *ComparisonPredicate, got %T", pred)
	}
	if cp.Comparison.Type != cascades.ComparisonEquals {
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
	cp, ok := simplified.(*cascades.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate after simplify, got %T", simplified)
	}
	if cp.Comparison.Type != cascades.ComparisonNotEquals {
		t.Fatalf("expected <>, got %v", cp.Comparison.Type)
	}
}

func TestWalkPredicate_NotAndCombo(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	// NOT binds across an AND — `NOT (cond1 AND cond2)` requires
	// parens in the grammar, but `NOT cond1 AND cond2` is parsed
	// as `(NOT cond1) AND cond2`. Test the latter, simpler shape —
	// parenthesised expressions surface as RecordConstructor atoms
	// which need a separate walker dispatch (next-shift work).
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE NOT active AND id = 1")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	and, ok := pred.(*cascades.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("AND children: got %d, want 2", len(and.SubPredicates))
	}
	// First child should be a NOT wrapping a ValuePredicate.
	if _, ok := and.SubPredicates[0].(*cascades.NotPredicate); !ok {
		t.Fatalf("first child: expected NotPredicate, got %T", and.SubPredicates[0])
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
	and, ok := pred.(*cascades.AndPredicate)
	if !ok {
		t.Fatalf("BETWEEN → AND(>=, <=): expected AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("AND children: got %d, want 2", len(and.SubPredicates))
	}
	// Eval.
	for _, tc := range []struct {
		id      int64
		want    cascades.TriBool
		comment string
	}{
		{1, cascades.TriTrue, "lower bound inclusive"},
		{5, cascades.TriTrue, "middle"},
		{10, cascades.TriTrue, "upper bound inclusive"},
		{11, cascades.TriFalse, "above"},
		{0, cascades.TriFalse, "below"},
	} {
		got := pred.Eval(map[string]any{"ID": tc.id})
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
	if _, ok := pred.(*cascades.NotPredicate); !ok {
		t.Fatalf("expected NotPredicate wrapping, got %T", pred)
	}
	// Eval: id=5 is in [1,10] so NOT BETWEEN is FALSE.
	if got := pred.Eval(map[string]any{"ID": int64(5)}); got != cascades.TriFalse {
		t.Fatalf("5 NOT BETWEEN 1 AND 10: got %v, want FALSE", got)
	}
	if got := pred.Eval(map[string]any{"ID": int64(15)}); got != cascades.TriTrue {
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
	cp := pred.(*cascades.ComparisonPredicate)
	if cp.Comparison.Type != cascades.ComparisonIn {
		t.Fatalf("Type: got %v, want In", cp.Comparison.Type)
	}
	list, ok := cp.Comparison.Operand.([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("Operand: got %v", cp.Comparison.Operand)
	}
	// Evaluate.
	for id, want := range map[int64]cascades.TriBool{
		1: cascades.TriTrue,
		2: cascades.TriTrue,
		3: cascades.TriTrue,
		4: cascades.TriFalse,
	} {
		got := pred.Eval(map[string]any{"ID": id})
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
	if _, ok := pred.(*cascades.NotPredicate); !ok {
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
	cp := pred.(*cascades.ComparisonPredicate)
	if cp.Comparison.Type != cascades.ComparisonLike {
		t.Fatalf("Type: got %v, want Like", cp.Comparison.Type)
	}
	if cp.Comparison.Operand != "hel%" {
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
	if _, ok := pred.(*cascades.NotPredicate); !ok {
		t.Fatalf("expected *NotPredicate, got %T", pred)
	}
}

func TestWalkPredicate_Like_Escape_Unsupported(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE name LIKE 'a\\\\%b' ESCAPE '\\\\'")

	_, err := r.WalkPredicate(ctx)
	if err == nil {
		t.Fatal("expected UnsupportedExpressionShapeError for LIKE with ESCAPE")
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
	cp := pred.(*cascades.ComparisonPredicate)
	if cp.Comparison.Type != cascades.ComparisonIsNull {
		t.Fatalf("Type: got %v, want IsNull", cp.Comparison.Type)
	}
}

func TestWalkPredicate_IsTrue(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE active IS TRUE")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp := pred.(*cascades.ComparisonPredicate)
	if cp.Comparison.Type != cascades.ComparisonEquals {
		t.Fatal("Type mismatch")
	}
	if cp.Comparison.Operand != true {
		t.Fatalf("Operand: got %v, want true", cp.Comparison.Operand)
	}
	if got := cp.Eval(map[string]any{"ACTIVE": true}); got != cascades.TriTrue {
		t.Fatalf("true IS TRUE: got %v", got)
	}
	if got := cp.Eval(map[string]any{"ACTIVE": false}); got != cascades.TriFalse {
		t.Fatalf("false IS TRUE: got %v", got)
	}
}

func TestWalkPredicate_IsFalse(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE active IS FALSE")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp := pred.(*cascades.ComparisonPredicate)
	if cp.Comparison.Operand != false {
		t.Fatalf("Operand: got %v, want false", cp.Comparison.Operand)
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
	cp := pred.(*cascades.ComparisonPredicate)
	if cp.Comparison.Type != cascades.ComparisonIsNotNull {
		t.Fatalf("Type: got %v, want IsNotNull", cp.Comparison.Type)
	}
}

// XOR is grammatically valid but maps to no cascades primitive.
// Walker returns UnsupportedExpressionShapeError so callers can
// fall back to the existing logical-builder path.
func TestWalkPredicate_XOR_Unsupported(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE active XOR active")

	_, err := r.WalkPredicate(ctx)
	if err == nil {
		t.Fatal("expected UnsupportedExpressionShapeError for XOR")
	}
	var u *expr.UnsupportedExpressionShapeError
	if !errors.As(err, &u) {
		t.Fatalf("expected UnsupportedExpressionShapeError, got %T", err)
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
		op  cascades.ArithmeticOp
	}{
		{"SELECT * FROM users WHERE id + 1", cascades.OpAdd},
		{"SELECT * FROM users WHERE id - 1", cascades.OpSub},
		{"SELECT * FROM users WHERE id * 2", cascades.OpMul},
		{"SELECT * FROM users WHERE id / 2", cascades.OpDiv},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, tc.sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			av, ok := v.(*cascades.ArithmeticValue)
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
	outer, ok := v.(*cascades.ArithmeticValue)
	if !ok {
		t.Fatalf("expected *ArithmeticValue, got %T", v)
	}
	if outer.Op != cascades.OpMul {
		t.Fatalf("outer Op: got %v, want OpMul", outer.Op)
	}
	inner, ok := outer.Left.(*cascades.ArithmeticValue)
	if !ok {
		t.Fatalf("inner: expected *ArithmeticValue, got %T", outer.Left)
	}
	if inner.Op != cascades.OpAdd {
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
		want cascades.AggregateOp
	}{
		{"SELECT * FROM users WHERE COUNT(*)", cascades.AggCountStar},
		{"SELECT * FROM users WHERE COUNT(id)", cascades.AggCount},
		{"SELECT * FROM users WHERE SUM(id)", cascades.AggSum},
		{"SELECT * FROM users WHERE MIN(id)", cascades.AggMin},
		{"SELECT * FROM users WHERE MAX(id)", cascades.AggMax},
		{"SELECT * FROM users WHERE AVG(id)", cascades.AggAvg},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, tc.sql)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			av, ok := v.(*cascades.AggregateValue)
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
	and, ok := pred.(*cascades.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", pred)
	}
	if len(and.SubPredicates) != 3 {
		t.Fatalf("children: got %d, want 3", len(and.SubPredicates))
	}

	// Evaluate against some rows.
	row := map[string]any{"ID": int64(5), "NAME": "bob"}
	if got := pred.Eval(row); got != cascades.TriTrue {
		t.Fatalf("id=5, name=bob: got %v, want TRUE", got)
	}
	row["NAME"] = nil
	if got := pred.Eval(row); got != cascades.TriFalse {
		t.Fatalf("id=5, name=NULL: got %v, want FALSE", got)
	}
	row["NAME"] = "bob"
	row["ID"] = int64(11)
	if got := pred.Eval(row); got != cascades.TriFalse {
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
