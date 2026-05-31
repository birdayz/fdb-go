package expr_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
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
	if _, ok := rn.PartitioningValues[0].(*values.FieldValue); !ok {
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

// TestWalk_ArrayConstructorNumeric pins walkArrayConstructor's switch from
// GetText()+ParseFloat to the resolver: negative literals (NegativeDecimalConstant)
// and integer literals (promoted to float64) must both resolve to a
// []float64 LiteralValue — the query-vector operand shape of a K-NN distance.
func TestWalk_ArrayConstructorNumeric(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstSelectExpr(t, "SELECT [1.5, -2.5, 3, 0] FROM users")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cv, ok := v.(*values.ConstantValue)
	if !ok {
		t.Fatalf("got %T, want *ConstantValue", v)
	}
	got, ok := cv.Value.([]float64)
	if !ok {
		t.Fatalf("constant value is %T, want []float64", cv.Value)
	}
	want := []float64{1.5, -2.5, 3, 0}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("elem[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestWalk_ArrayConstructorRejectsNonConstant pins that a non-constant array
// element (a column reference) is rejected cleanly, not silently mis-parsed.
func TestWalk_ArrayConstructorRejectsNonConstant(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstSelectExpr(t, "SELECT [1.0, id, 3.0] FROM users")
	if _, err := r.WalkExpression(ctx); err == nil {
		t.Fatal("expected error for non-constant array element, got nil")
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
