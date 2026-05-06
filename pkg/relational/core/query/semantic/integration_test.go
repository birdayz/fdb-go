package semantic

import "testing"

// End-to-end integration: parse a SELECT, build a scope from its
// FROM clause, resolve each SELECT-list column against the scope.
// Exercises Identifier + Analyzer + Scope + Catalog + parse-tree
// bridge as one pipeline.
func TestIntegration_ResolveSelectList(t *testing.T) {
	t.Parallel()
	// Catalog: USERS(id INT, name STRING, age INT NULLABLE).
	cat := NewInMemoryCatalog(&StaticTable{
		TableName: ParseQualifiedName("USERS", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "INT"},
			{Id: NewUnquoted("name"), Type: "STRING", Nullable: true},
			{Id: NewUnquoted("age"), Type: "INT", Nullable: true},
		},
	})
	a := NewAnalyzer(cat, false)

	// Parse `SELECT name, age FROM users` (case-insensitive
	// identifiers) and walk its tree via the shared helper.
	fullId := parseTableFullId(t, "SELECT name, age FROM users")
	tbl, err := a.ResolveTableRef(fullId)
	if err != nil {
		t.Fatalf("ResolveTableRef: %v", err)
	}

	// Build a scope with the resolved table.
	scope := NewScope(nil)
	if err := scope.AddSource(ScopeSource{
		Table:           tbl,
		Alias:           NewUnquoted("users"),
		CorrelationName: "users",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	// Resolve each SELECT-list column against the scope.
	for _, name := range []string{"name", "age"} {
		col, src, err := scope.ResolveColumn(NewUnquoted(name))
		if err != nil {
			t.Fatalf("resolve %q: %v", name, err)
		}
		if !src.Alias.EqualsIgnoreQuoting(NewUnquoted("users")) {
			t.Fatalf("resolved %q from unexpected source %q", name, src.Alias)
		}
		if col.Id.Name() != NewUnquoted(name).Name() {
			t.Fatalf("column Id: got %q, want %q", col.Id.Name(), NewUnquoted(name).Name())
		}
	}

	// SELECT * should expand to all 3 columns in declared order.
	expanded := a.ExpandScopeStar(scope)
	if len(expanded) != 3 {
		t.Fatalf("SELECT * should yield 3 columns, got %d", len(expanded))
	}
	want := []string{"ID", "NAME", "AGE"}
	for i, c := range expanded {
		if c.Column.Id.Name() != want[i] {
			t.Fatalf("expanded[%d]: got %q, want %q",
				i, c.Column.Id.Name(), want[i])
		}
	}

	// Missing column raises ColumnNotFoundError.
	if _, _, err := scope.ResolveColumn(NewUnquoted("nonexistent")); err == nil {
		t.Fatal("expected ColumnNotFoundError for nonexistent column")
	}

	// Missing table raises TableNotFoundError at resolve time.
	otherFullId := parseTableFullId(t, "SELECT * FROM orders")
	if _, err := a.ResolveTableRef(otherFullId); err == nil {
		t.Fatal("expected TableNotFoundError for missing table")
	}
}
