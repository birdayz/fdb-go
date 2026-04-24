package semantic

import "testing"

func buildTestCatalog() *InMemoryCatalog {
	users := &StaticTable{
		TableName: ParseQualifiedName("USERS", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "INT", Nullable: false},
			{Id: NewUnquoted("name"), Type: "STRING", Nullable: true},
			{Id: NewUnquoted("age"), Type: "INT", Nullable: true},
		},
		TableIndexes: []string{"idx_users_name"},
	}
	orders := &StaticTable{
		TableName: ParseQualifiedName("schema1.Orders", false),
		TableColumns: []Column{
			{Id: NewUnquoted("order_id"), Type: "INT"},
			{Id: NewUnquoted("user_id"), Type: "INT"},
		},
	}
	return NewInMemoryCatalog(users, orders)
}

func TestInMemoryCatalog_LookupTable(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()

	users, ok := c.LookupTable(ParseQualifiedName("users", false))
	if !ok {
		t.Fatal("expected USERS to exist (case-folded lookup)")
	}
	if got, want := users.Name().Name(), "USERS"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}

	orders, ok := c.LookupTable(ParseQualifiedName("schema1.orders", false))
	if !ok {
		t.Fatal("expected SCHEMA1.ORDERS to exist")
	}
	if !orders.Name().IsQualified() {
		t.Fatal("orders table should be qualified")
	}
	if got, want := orders.Name().String(), "SCHEMA1.ORDERS"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
}

func TestInMemoryCatalog_TableExists(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()

	if !c.TableExists(ParseQualifiedName("users", false)) {
		t.Fatal("USERS should exist")
	}
	if c.TableExists(ParseQualifiedName("nonexistent", false)) {
		t.Fatal("nonexistent table should not exist")
	}
	// Schema-qualified lookup for an unqualified name should not match.
	if c.TableExists(ParseQualifiedName("orders", false)) {
		t.Fatal("bare 'orders' shouldn't match schema1.Orders — search-path resolution is caller's job")
	}
}

func TestStaticTable_LookupColumn(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("USERS", false))

	col, ok := users.LookupColumn(NewUnquoted("name"))
	if !ok {
		t.Fatal("name column should exist")
	}
	if got, want := col.Type, "STRING"; got != want {
		t.Fatalf("Type: got %q, want %q", got, want)
	}
	if !col.Nullable {
		t.Fatal("name should be nullable")
	}

	// Case-insensitive lookup.
	if _, ok := users.LookupColumn(NewUnquoted("NAME")); !ok {
		t.Fatal("NAME should match name")
	}

	if _, ok := users.LookupColumn(NewUnquoted("nonexistent")); ok {
		t.Fatal("nonexistent column shouldn't match")
	}
}

func TestStaticTable_ColumnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("USERS", false))

	cols := users.Columns()
	cols[0].Type = "HACKED"

	cols2 := users.Columns()
	if cols2[0].Type == "HACKED" {
		t.Fatal("Columns() returned reference to internal slice; mutation leaked")
	}
}

func TestInMemoryCatalog_AllTableNames(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	names := c.AllTableNames()
	if len(names) != 2 {
		t.Fatalf("expected 2 tables, got %d", len(names))
	}
	// Order is unspecified; check both are present.
	got := map[string]bool{}
	for _, n := range names {
		got[n.String()] = true
	}
	if !got["USERS"] {
		t.Fatal("USERS missing from AllTableNames")
	}
	if !got["SCHEMA1.ORDERS"] {
		t.Fatal("SCHEMA1.ORDERS missing from AllTableNames")
	}
}

func TestStaticTable_IndexesDefensiveCopy(t *testing.T) {
	t.Parallel()
	c := buildTestCatalog()
	users, _ := c.LookupTable(ParseQualifiedName("USERS", false))

	idx := users.Indexes()
	if len(idx) != 1 || idx[0] != "idx_users_name" {
		t.Fatalf("Indexes: got %v", idx)
	}
	idx[0] = "HACKED"
	if users.Indexes()[0] == "HACKED" {
		t.Fatal("Indexes() returned reference; mutation leaked")
	}
}
