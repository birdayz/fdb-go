package semantic

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

func parseFromClause(t *testing.T, sql string) antlrgen.IFromClauseContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	return simple.FromClause()
}

func buildAnalyzerWithUsersAndOrders(t *testing.T) *Analyzer {
	t.Helper()
	return NewAnalyzer(buildTestCatalog(), false)
}

func TestBuildScopeFromFromClause_SingleTable(t *testing.T) {
	t.Parallel()
	a := buildAnalyzerWithUsersAndOrders(t)
	from := parseFromClause(t, "SELECT * FROM users")

	scope, err := a.BuildScopeFromFromClause(nil, from)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srcs := scope.Sources()
	if len(srcs) != 1 {
		t.Fatalf("expected 1 source, got %d", len(srcs))
	}
	if got, want := srcs[0].Alias.Name(), "USERS"; got != want {
		t.Fatalf("Alias: got %q, want %q", got, want)
	}
}

// Implicit alias: `FROM t u` (no AS). The grammar allows `AS?`
// before the alias, so the parser produces a non-nil alias even
// without AS. Previously the scope builder gated on `atom.AS() !=
// nil` and silently dropped implicit aliases.
func TestBuildScopeFromFromClause_ImplicitAlias(t *testing.T) {
	t.Parallel()
	a := buildAnalyzerWithUsersAndOrders(t)
	from := parseFromClause(t, "SELECT * FROM users u")

	scope, err := a.BuildScopeFromFromClause(nil, from)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srcs := scope.Sources()
	if len(srcs) != 1 {
		t.Fatalf("expected 1 source, got %d", len(srcs))
	}
	if got, want := srcs[0].Alias.Name(), "U"; got != want {
		t.Fatalf("implicit alias: got %q, want %q", got, want)
	}
}

func TestBuildScopeFromFromClause_Aliased(t *testing.T) {
	t.Parallel()
	a := buildAnalyzerWithUsersAndOrders(t)
	from := parseFromClause(t, "SELECT * FROM users AS u")

	scope, err := a.BuildScopeFromFromClause(nil, from)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srcs := scope.Sources()
	if len(srcs) != 1 {
		t.Fatalf("expected 1 source, got %d", len(srcs))
	}
	if got, want := srcs[0].Alias.Name(), "U"; got != want {
		t.Fatalf("Alias: got %q, want %q", got, want)
	}
}

func TestBuildScopeFromFromClause_CommaJoin(t *testing.T) {
	t.Parallel()
	a := buildAnalyzerWithUsersAndOrders(t)
	from := parseFromClause(t, "SELECT * FROM users AS u, schema1.orders AS o")

	scope, err := a.BuildScopeFromFromClause(nil, from)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srcs := scope.Sources()
	if len(srcs) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(srcs))
	}
	if got, want := srcs[0].Alias.Name(), "U"; got != want {
		t.Fatalf("first Alias: got %q, want %q", got, want)
	}
	if got, want := srcs[1].Alias.Name(), "O"; got != want {
		t.Fatalf("second Alias: got %q, want %q", got, want)
	}
	// Column resolution still works across both.
	col, src, err := scope.ResolveColumn(NewUnquoted("order_id"))
	if err != nil {
		t.Fatalf("resolve order_id: %v", err)
	}
	if src.Alias.Name() != "O" {
		t.Fatalf("order_id should come from O, got %q", src.Alias.Name())
	}
	if col.Type != "INT" {
		t.Fatalf("Type: got %q, want INT", col.Type)
	}
}

func TestBuildScopeFromFromClause_MissingTable(t *testing.T) {
	t.Parallel()
	a := buildAnalyzerWithUsersAndOrders(t)
	from := parseFromClause(t, "SELECT * FROM no_such_table")

	_, err := a.BuildScopeFromFromClause(nil, from)
	if err == nil {
		t.Fatal("expected error")
	}
	var tnf *TableNotFoundError
	if !errors.As(err, &tnf) {
		t.Fatalf("expected TableNotFoundError, got %T", err)
	}
}

func TestBuildScopeFromFromClause_JoinUnsupported(t *testing.T) {
	t.Parallel()
	a := buildAnalyzerWithUsersAndOrders(t)
	from := parseFromClause(t, "SELECT * FROM users u INNER JOIN schema1.orders o ON u.id = o.user_id")

	_, err := a.BuildScopeFromFromClause(nil, from)
	if err == nil {
		t.Fatal("expected UnsupportedFromShapeError for JOIN")
	}
	var uf *UnsupportedFromShapeError
	if !errors.As(err, &uf) {
		t.Fatalf("expected UnsupportedFromShapeError, got %T", err)
	}
	if uf.Shape != "JOIN" {
		t.Fatalf("Shape: got %q, want JOIN", uf.Shape)
	}
}

func TestBuildScopeFromFromClause_NilInput(t *testing.T) {
	t.Parallel()
	a := buildAnalyzerWithUsersAndOrders(t)
	scope, err := a.BuildScopeFromFromClause(nil, nil)
	if err != nil {
		t.Fatalf("nil from: %v", err)
	}
	if scope == nil {
		t.Fatal("nil from should produce an empty scope, not nil")
	}
	if len(scope.Sources()) != 0 {
		t.Fatalf("empty scope should have 0 sources, got %d", len(scope.Sources()))
	}
}
