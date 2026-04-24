package expr_test

import (
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

// Full-stack integration: parse SQL → BuildScopeFromFromClause →
// WalkPredicate → Simplify → evaluate. Exercises every new seam
// landed this shift (semantic.Catalog, semantic.Analyzer,
// semantic.Scope, expr.Resolver, expr walker, cascades.Simplify).
// No stubs, no synthetic scaffolding beyond the catalog definition.
func TestFullStack_Pipeline(t *testing.T) {
	t.Parallel()

	// 1. Build a catalog mirroring a realistic USERS table.
	users := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("USERS", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{Id: semantic.NewUnquoted("name"), Type: "STRING", Nullable: true},
			{Id: semantic.NewUnquoted("age"), Type: "INT", Nullable: true},
			{Id: semantic.NewUnquoted("active"), Type: "BOOL"},
		},
	}
	cat := semantic.NewInMemoryCatalog(users)
	analyzer := semantic.NewAnalyzer(cat, false)

	// 2. Parse a realistic SELECT.
	sql := "SELECT * FROM users WHERE id >= 18 AND (name IS NOT NULL OR active) AND 5 = 5"
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// 3. Extract the FROM clause and WHERE expression.
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	fromCtx := simple.FromClause()
	whereExpr := fromCtx.WhereExpr().Expression()

	// 4. Build a Scope from the FROM clause.
	scope, err := analyzer.BuildScopeFromFromClause(nil, fromCtx)
	if err != nil {
		t.Fatalf("BuildScopeFromFromClause: %v", err)
	}
	if len(scope.Sources()) != 1 {
		t.Fatalf("scope: expected 1 source, got %d", len(scope.Sources()))
	}

	// 5. Walk the WHERE expression via the Resolver.
	r := expr.New(analyzer, scope)
	pred, err := r.WalkPredicate(whereExpr)
	if err != nil {
		t.Fatalf("WalkPredicate: %v", err)
	}

	// 6. Run through the simplifier. `5 = 5` tautology should fold
	// out of the AND.
	simplified := cascades.Simplify(pred, cascades.DefaultSimplifyRules())

	// 7. Evaluate against sample rows.
	type row = map[string]any
	cases := []struct {
		name string
		row  row
		want cascades.TriBool
	}{
		{
			name: "adult with name",
			row:  row{"ID": int64(25), "NAME": "alice", "ACTIVE": true, "AGE": int64(30)},
			want: cascades.TriTrue,
		},
		{
			name: "adult without name but active",
			row:  row{"ID": int64(25), "NAME": nil, "ACTIVE": true, "AGE": int64(30)},
			want: cascades.TriTrue,
		},
		{
			name: "adult without name and inactive",
			row:  row{"ID": int64(25), "NAME": nil, "ACTIVE": false, "AGE": int64(30)},
			want: cascades.TriFalse,
		},
		{
			name: "minor",
			row:  row{"ID": int64(10), "NAME": "child", "ACTIVE": true, "AGE": int64(10)},
			want: cascades.TriFalse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := simplified.Eval(tc.row)
			if got != tc.want {
				t.Fatalf("got %v, want %v (simplified predicate: %s)",
					got, tc.want, simplified.Explain())
			}
		})
	}

	// 8. Bonus: `5 = 5` tautology should have been dropped by the
	// simplifier — the surviving AND should have at most 2 children
	// (id >= 18 AND (name IS NOT NULL OR active)).
	and, ok := simplified.(*cascades.AndPredicate)
	if !ok {
		t.Fatalf("expected AND at top, got %T", simplified)
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("after simplify: expected 2 children (tautology dropped), got %d: %s",
			len(and.SubPredicates), simplified.Explain())
	}

	// 9. Pin the Explain output so Simplify regressions or
	// Explain-formatting changes surface here.
	wantExplain := "(ID >= 18 AND (NAME IS NOT NULL OR ACTIVE))"
	if got := simplified.Explain(); got != wantExplain {
		t.Fatalf("Explain: got %q, want %q", got, wantExplain)
	}
}
