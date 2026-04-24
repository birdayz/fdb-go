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

func TestWalkExpression_NilContext(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	if _, err := r.WalkExpression(nil); err == nil {
		t.Fatal("expected error for nil context")
	}
}
