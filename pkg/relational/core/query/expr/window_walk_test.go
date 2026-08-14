package expr_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
)

// parseFirstSelectExpr returns the first SELECT-list expression.
func parseFirstSelectExpr(t *testing.T, sql string) antlrgen.IExpressionContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	elem := simple.SelectElements().AllSelectElement()[0]
	see, ok := elem.(*antlrgen.SelectExpressionElementContext)
	if !ok {
		t.Fatalf("first select element is %T, want SelectExpressionElement", elem)
	}
	return see.Expression()
}

func TestWalk_DistanceFunction(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstSelectExpr(t, "SELECT euclidean_distance(id, id) FROM users")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	dv, ok := v.(*values.DistanceValue)
	if !ok {
		t.Fatalf("got %T, want *DistanceValue", v)
	}
	if dv.Operator != values.DistanceEuclidean {
		t.Errorf("operator = %v, want DistanceEuclidean", dv.Operator)
	}
}

func TestWalk_RowNumberOverDistance(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstSelectExpr(t,
		"SELECT ROW_NUMBER() OVER (PARTITION BY name ORDER BY euclidean_distance(id, id) ASC OPTIONS ef_search = 50) FROM users")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	rn, ok := v.(*values.RowNumberValue)
	if !ok {
		t.Fatalf("got %T, want *RowNumberValue", v)
	}
	if len(rn.PartitioningValues) != 1 {
		t.Fatalf("partitioning values = %d, want 1", len(rn.PartitioningValues))
	}
	if _, ok := values.AsFieldValue(rn.PartitioningValues[0]); !ok {
		t.Errorf("partition[0] = %T, want *FieldValue", rn.PartitioningValues[0])
	}
	if len(rn.ArgumentValues) != 1 {
		t.Fatalf("argument values = %d, want 1", len(rn.ArgumentValues))
	}
	if _, ok := rn.ArgumentValues[0].(*values.DistanceValue); !ok {
		t.Errorf("argument[0] = %T, want *DistanceValue", rn.ArgumentValues[0])
	}
	if rn.EfSearch == nil || *rn.EfSearch != 50 {
		t.Errorf("ef_search = %v, want 50", rn.EfSearch)
	}
}

// TestWalk_ArrayConstructorNumeric pins walkArrayConstructor's
// Java-aligned shape (LightArrayConstructorValue built by
// ExpressionVisitor.handleArray): a mixed int/double literal resolves
// to an ArrayConstructorValue whose element type is the MaximumType
// fold (here DOUBLE), with promotions injected so evaluation yields
// homogeneous []any elements. Negative literals
// (NegativeDecimalConstant) resolve via the typed parse tree, not
// GetText()+ParseFloat.
func TestWalk_ArrayConstructorNumeric(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstSelectExpr(t, "SELECT [1.5, -2.5, 3, 0] FROM users")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	av, ok := v.(*values.ArrayConstructorValue)
	if !ok {
		t.Fatalf("got %T, want *ArrayConstructorValue", v)
	}
	if av.ElementType.Code() != values.TypeCodeDouble {
		t.Fatalf("element type = %v, want DOUBLE (MaximumType of DOUBLE and INT)", av.ElementType.Code())
	}
	ev, err := av.Evaluate(nil)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	got, ok := ev.([]any)
	if !ok {
		t.Fatalf("evaluated to %T, want []any", ev)
	}
	want := []float64{1.5, -2.5, 3, 0}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		f, ok := got[i].(float64)
		if !ok || f != want[i] {
			t.Errorf("elem[%d] = %v (%T), want %v (float64) — the INT elements must be promoted to the DOUBLE element type", i, got[i], got[i], want[i])
		}
	}
}

// TestWalk_ArrayConstructorColumnElement pins that a non-constant
// array element (a column reference) walks cleanly — Java's
// LightArrayConstructorValue takes arbitrary child Values, so
// `[1.0, id, 3.0]` is a per-row array whose middle element is a
// FieldValue promoted to the folded element type.
func TestWalk_ArrayConstructorColumnElement(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstSelectExpr(t, "SELECT [1.0, id, 3.0] FROM users")
	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	av, ok := v.(*values.ArrayConstructorValue)
	if !ok {
		t.Fatalf("got %T, want *ArrayConstructorValue", v)
	}
	if len(av.Elements) != 3 {
		t.Fatalf("len(Elements) = %d, want 3", len(av.Elements))
	}
}

// TestWalk_ArrayConstructorIncompatibleElements pins Java's
// SemanticException INCOMPATIBLE_TYPE (surfaced as the relational
// 22000 CANNOT_CONVERT_TYPE class) for an element pair with no
// common maximum type, e.g. an int next to a string.
func TestWalk_ArrayConstructorIncompatibleElements(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstSelectExpr(t, "SELECT [1, 'a'] FROM users")
	_, err := r.WalkExpression(ctx)
	if err == nil {
		t.Fatal("expected CANNOT_CONVERT_TYPE for [1, 'a'], got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeCannotConvertType {
		t.Fatalf("got %v, want api.ErrCodeCannotConvertType", err)
	}
}

func TestWalk_RowNumberRejects(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	cases := []struct {
		name string
		sql  string
	}{
		{"DESC order", "SELECT ROW_NUMBER() OVER (ORDER BY euclidean_distance(id, id) DESC) FROM users"},
		{"RANK not supported", "SELECT RANK() OVER (ORDER BY euclidean_distance(id, id)) FROM users"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstSelectExpr(t, tc.sql)
			if _, err := r.WalkExpression(ctx); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}
