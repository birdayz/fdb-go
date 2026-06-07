package expr_test

import (
	"testing"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
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
		want predicates.TriBool
	}{
		{
			name: "adult with name",
			row:  row{"ID": int64(25), "NAME": "alice", "ACTIVE": true, "AGE": int64(30)},
			want: predicates.TriTrue,
		},
		{
			name: "adult without name but active",
			row:  row{"ID": int64(25), "NAME": nil, "ACTIVE": true, "AGE": int64(30)},
			want: predicates.TriTrue,
		},
		{
			name: "adult without name and inactive",
			row:  row{"ID": int64(25), "NAME": nil, "ACTIVE": false, "AGE": int64(30)},
			want: predicates.TriFalse,
		},
		{
			name: "minor",
			row:  row{"ID": int64(10), "NAME": "child", "ACTIVE": true, "AGE": int64(10)},
			want: predicates.TriFalse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mustEvalPred(simplified, tc.row)
			if got != tc.want {
				t.Fatalf("got %v, want %v (simplified predicate: %s)",
					got, tc.want, simplified.Explain())
			}
		})
	}

	// 8. Bonus: `5 = 5` tautology should have been dropped by the
	// simplifier — the surviving AND should have at most 2 children
	// (id >= 18 AND (name IS NOT NULL OR active)).
	and, ok := simplified.(*predicates.AndPredicate)
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

// TestFullStack_RichPredicates exercises the parse → walk → simplify
// → eval pipeline with predicate shapes that test_Pipeline doesn't
// touch: BETWEEN, IN-list, LIKE, NOT, XOR. Gives us end-to-end
// coverage for every walker branch the seed handles.
func TestFullStack_RichPredicates(t *testing.T) {
	t.Parallel()

	users := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("USERS", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{Id: semantic.NewUnquoted("name"), Type: "STRING", Nullable: true},
			{Id: semantic.NewUnquoted("role"), Type: "STRING"},
			{Id: semantic.NewUnquoted("active"), Type: "BOOL"},
			{Id: semantic.NewUnquoted("admin"), Type: "BOOL", Nullable: true},
		},
	}
	cat := semantic.NewInMemoryCatalog(users)
	analyzer := semantic.NewAnalyzer(cat, false)

	cases := []struct {
		name string
		sql  string
		row  map[string]any
		want predicates.TriBool
	}{
		{
			name: "BETWEEN hit",
			sql:  "SELECT * FROM users WHERE id BETWEEN 10 AND 20",
			row:  map[string]any{"ID": int64(15)},
			want: predicates.TriTrue,
		},
		{
			name: "BETWEEN upper exclusive? no — SQL BETWEEN is inclusive",
			sql:  "SELECT * FROM users WHERE id BETWEEN 10 AND 20",
			row:  map[string]any{"ID": int64(20)},
			want: predicates.TriTrue,
		},
		{
			name: "BETWEEN miss",
			sql:  "SELECT * FROM users WHERE id BETWEEN 10 AND 20",
			row:  map[string]any{"ID": int64(5)},
			want: predicates.TriFalse,
		},
		{
			name: "IN list hit",
			sql:  "SELECT * FROM users WHERE role IN ('admin', 'owner')",
			row:  map[string]any{"ROLE": "owner"},
			want: predicates.TriTrue,
		},
		{
			name: "IN list miss",
			sql:  "SELECT * FROM users WHERE role IN ('admin', 'owner')",
			row:  map[string]any{"ROLE": "viewer"},
			want: predicates.TriFalse,
		},
		{
			name: "LIKE prefix",
			sql:  "SELECT * FROM users WHERE name LIKE 'al%'",
			row:  map[string]any{"NAME": "alice"},
			want: predicates.TriTrue,
		},
		{
			name: "LIKE with single-char wildcard",
			sql:  "SELECT * FROM users WHERE name LIKE 'b_b'",
			row:  map[string]any{"NAME": "bob"},
			want: predicates.TriTrue,
		},
		{
			name: "NOT IN list",
			sql:  "SELECT * FROM users WHERE role NOT IN ('banned', 'guest')",
			row:  map[string]any{"ROLE": "admin"},
			want: predicates.TriTrue,
		},
		{
			name: "XOR true/false",
			sql:  "SELECT * FROM users WHERE active XOR admin",
			row:  map[string]any{"ACTIVE": true, "ADMIN": false},
			want: predicates.TriTrue,
		},
		{
			name: "XOR NULL propagates to UNKNOWN",
			sql:  "SELECT * FROM users WHERE active XOR admin",
			row:  map[string]any{"ACTIVE": true, "ADMIN": nil},
			want: predicates.TriUnknown,
		},
		{
			name: "BETWEEN combined with IS NOT NULL",
			sql:  "SELECT * FROM users WHERE id BETWEEN 1 AND 100 AND name IS NOT NULL",
			row:  map[string]any{"ID": int64(42), "NAME": "alice"},
			want: predicates.TriTrue,
		},
		{
			name: "NULL name breaks IS NOT NULL half",
			sql:  "SELECT * FROM users WHERE id BETWEEN 1 AND 100 AND name IS NOT NULL",
			row:  map[string]any{"ID": int64(42), "NAME": nil},
			want: predicates.TriFalse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := parser.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sql, err)
			}
			sel := root.Statements().AllStatement()[0].SelectStatement()
			body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
			simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
			fromCtx := simple.FromClause()
			scope, err := analyzer.BuildScopeFromFromClause(nil, fromCtx)
			if err != nil {
				t.Fatalf("BuildScope: %v", err)
			}
			r := expr.New(analyzer, scope)
			pred, err := r.WalkPredicate(fromCtx.WhereExpr().Expression())
			if err != nil {
				t.Fatalf("WalkPredicate: %v", err)
			}
			simplified := cascades.Simplify(pred, cascades.DefaultSimplifyRules())
			if got := mustEvalPred(simplified, tc.row); got != tc.want {
				t.Errorf("got %v, want %v (pred: %s)", got, tc.want, simplified.Explain())
			}
		})
	}
}
