package semantic

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// parseTableFullId walks the parse tree for a SELECT and returns the
// IFullIdContext of the first FROM table. Shared with parse_bridge_test.
func parseTableFullId(t *testing.T, sql string) antlrgen.IFullIdContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	body := sel.Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	simple := body.QueryTerm().(*antlrgen.SimpleTableContext)
	srcBase := simple.FromClause().TableSources().AllTableSource()[0].(*antlrgen.TableSourceBaseContext)
	atom := srcBase.TableSourceItem().(*antlrgen.AtomTableItemContext)
	return atom.TableName().FullId()
}

func TestAnalyzer_ResolveTable(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)

	tbl, err := a.ResolveTable(ParseQualifiedName("users", false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := tbl.Name().String(), "USERS"; got != want {
		t.Fatalf("resolved table name: got %q, want %q", got, want)
	}
}

func TestAnalyzer_ResolveTable_NotFound(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)

	_, err := a.ResolveTable(ParseQualifiedName("no_such_table", false))
	if err == nil {
		t.Fatal("expected error for missing table")
	}
	var tnf *TableNotFoundError
	if !errors.As(err, &tnf) {
		t.Fatalf("expected TableNotFoundError, got %T", err)
	}
	if got, want := tnf.Name.Name(), "NO_SUCH_TABLE"; got != want {
		t.Fatalf("TableNotFoundError.Name: got %q, want %q", got, want)
	}
}

func TestAnalyzer_ResolveTable_EmptyName(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)

	_, err := a.ResolveTable(QualifiedName{})
	if err == nil {
		t.Fatal("expected error for zero-value name")
	}
	var tnf *TableNotFoundError
	if !errors.As(err, &tnf) {
		t.Fatalf("expected TableNotFoundError, got %T", err)
	}
}

func TestAnalyzer_ResolveColumn(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)

	tbl, err := a.ResolveTable(ParseQualifiedName("users", false))
	if err != nil {
		t.Fatalf("lookup users: %v", err)
	}
	col, err := a.ResolveColumn(tbl, NewUnquoted("name"))
	if err != nil {
		t.Fatalf("resolve name: %v", err)
	}
	if got, want := col.Type, "STRING"; got != want {
		t.Fatalf("Type: got %q, want %q", got, want)
	}
}

func TestAnalyzer_ResolveColumn_NotFound(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	tbl, _ := a.ResolveTable(ParseQualifiedName("users", false))

	_, err := a.ResolveColumn(tbl, NewUnquoted("no_such_col"))
	if err == nil {
		t.Fatal("expected error for missing column")
	}
	var cnf *ColumnNotFoundError
	if !errors.As(err, &cnf) {
		t.Fatalf("expected ColumnNotFoundError, got %T", err)
	}
	if cnf.TableName.IsZero() {
		t.Fatal("ColumnNotFoundError should carry TableName context")
	}
	if got, want := cnf.TableName.Name(), "USERS"; got != want {
		t.Fatalf("TableName: got %q, want %q", got, want)
	}
}

func TestAnalyzer_ResolveColumn_NilTable(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)

	_, err := a.ResolveColumn(nil, NewUnquoted("name"))
	if err == nil {
		t.Fatal("expected error for nil table")
	}
	var cnf *ColumnNotFoundError
	if !errors.As(err, &cnf) {
		t.Fatalf("expected ColumnNotFoundError, got %T", err)
	}
	if !cnf.TableName.IsZero() {
		t.Fatal("ColumnNotFoundError.TableName should be zero when table is nil")
	}
}

func TestAnalyzer_ResolveTableRef(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)

	ctx := parseTableFullId(t, "SELECT * FROM Users")
	tbl, err := a.ResolveTableRef(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := tbl.Name().String(), "USERS"; got != want {
		t.Fatalf("resolved name: got %q, want %q", got, want)
	}
}

func TestAnalyzer_ResolveTableRef_NotFound(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)

	ctx := parseTableFullId(t, "SELECT * FROM no_such_table")
	_, err := a.ResolveTableRef(ctx)
	if err == nil {
		t.Fatal("expected error for missing table")
	}
	var tnf *TableNotFoundError
	if !errors.As(err, &tnf) {
		t.Fatalf("expected TableNotFoundError, got %T", err)
	}
	if got, want := tnf.Name.Name(), "NO_SUCH_TABLE"; got != want {
		t.Fatalf("TableNotFoundError.Name: got %q, want %q", got, want)
	}
}

func TestAnalyzer_ExpandStar(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	tbl, _ := a.ResolveTable(ParseQualifiedName("users", false))

	cols := a.ExpandStar(tbl)
	if len(cols) != 3 {
		t.Fatalf("users has 3 columns; got %d", len(cols))
	}
	names := []string{cols[0].Id.Name(), cols[1].Id.Name(), cols[2].Id.Name()}
	wantNames := []string{"ID", "NAME", "AGE"}
	for i, want := range wantNames {
		if names[i] != want {
			t.Fatalf("column %d: got %q, want %q (order matters)", i, names[i], want)
		}
	}
}

func TestAnalyzer_ResolveColumnRef(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	scope, _, _ := buildScope(t)

	// Bare: qualifier zero.
	col, src, err := a.ResolveColumnRef(scope, Identifier{}, NewUnquoted("name"))
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	if src.Alias.Name() != "U" {
		t.Fatalf("bare: expected U, got %q", src.Alias.Name())
	}
	if col.Id.Name() != "NAME" {
		t.Fatalf("bare col: got %q", col.Id.Name())
	}

	// Qualified.
	col, src, err = a.ResolveColumnRef(scope, NewUnquoted("o"), NewUnquoted("order_id"))
	if err != nil {
		t.Fatalf("qualified: %v", err)
	}
	if src.Alias.Name() != "O" {
		t.Fatalf("qualified: expected O, got %q", src.Alias.Name())
	}
	if col.Id.Name() != "ORDER_ID" {
		t.Fatalf("qualified col: got %q", col.Id.Name())
	}
}

func TestAnalyzer_ResolveColumnRef_NilScope(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	_, _, err := a.ResolveColumnRef(nil, Identifier{}, NewUnquoted("x"))
	if err == nil {
		t.Fatal("expected error for nil scope")
	}
}

func TestAnalyzer_ExpandScopeStar(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	scope, _, _ := buildScope(t)

	expanded := a.ExpandScopeStar(scope)
	// users: id, name, age (3); orders: order_id, user_id (2). Total 5.
	if len(expanded) != 5 {
		t.Fatalf("expected 5 expanded columns, got %d", len(expanded))
	}
	// First three should come from users, last two from orders.
	for i := 0; i < 3; i++ {
		if expanded[i].Source.Alias.Name() != "U" {
			t.Fatalf("expanded[%d] should come from users alias U, got %q",
				i, expanded[i].Source.Alias.Name())
		}
	}
	for i := 3; i < 5; i++ {
		if expanded[i].Source.Alias.Name() != "O" {
			t.Fatalf("expanded[%d] should come from orders alias O, got %q",
				i, expanded[i].Source.Alias.Name())
		}
	}
}

func TestAnalyzer_ExpandScopeStar_NilScope(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	if got := a.ExpandScopeStar(nil); got != nil {
		t.Fatalf("nil scope should produce nil, got %v", got)
	}
}

func TestAnalyzer_ExpandQualifiedStar(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	scope, _, _ := buildScope(t)

	cols, err := a.ExpandQualifiedStar(scope, NewUnquoted("u"))
	if err != nil {
		t.Fatalf("u.*: %v", err)
	}
	if len(cols) != 3 {
		t.Fatalf("u.* should yield 3 columns, got %d", len(cols))
	}
	for _, c := range cols {
		if c.Source.Alias.Name() != "U" {
			t.Fatalf("all cols should have source U, got %q", c.Source.Alias.Name())
		}
	}
}

func TestAnalyzer_ExpandQualifiedStar_UnknownAlias(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	scope, _, _ := buildScope(t)

	_, err := a.ExpandQualifiedStar(scope, NewUnquoted("unknown"))
	if err == nil {
		t.Fatal("expected error")
	}
	var snf *SourceNotFoundError
	if !errors.As(err, &snf) {
		t.Fatalf("expected SourceNotFoundError, got %T", err)
	}
	// Error should list the scope's actual aliases so a human can
	// see what they could have typed.
	if len(snf.Available) != 2 {
		t.Fatalf("expected 2 available aliases, got %d", len(snf.Available))
	}
}

// Correlated-star reference: inner scope doesn't have the alias,
// but the parent scope does. ExpandQualifiedStar walks the chain.
func TestAnalyzer_ExpandQualifiedStar_Correlated(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	parent, _, _ := buildScope(t) // has aliases u, o
	child := NewScope(parent)     // empty

	cols, err := a.ExpandQualifiedStar(child, NewUnquoted("u"))
	if err != nil {
		t.Fatalf("correlated u.*: %v", err)
	}
	if len(cols) == 0 {
		t.Fatal("expected columns from parent scope")
	}
	// First column should come from the U source in the parent scope.
	if cols[0].Source.Alias.Name() != "U" {
		t.Fatalf("source alias: got %q, want U", cols[0].Source.Alias.Name())
	}
}

func TestAnalyzer_ExpandStar_NilTable(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer(buildTestCatalog(), false)
	if got := a.ExpandStar(nil); got != nil {
		t.Fatalf("nil table should produce nil, got %v", got)
	}
}

func TestAnalyzer_NilCatalogPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nil catalog")
		}
	}()
	_ = NewAnalyzer(nil, false)
}

func TestAnalyzer_AccessorsReturnInputs(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	a := NewAnalyzer(c, true)
	if a.Catalog() != c {
		t.Fatal("Catalog accessor should return the input catalog")
	}
	if !a.CaseSensitive() {
		t.Fatal("CaseSensitive accessor should return true")
	}
}
