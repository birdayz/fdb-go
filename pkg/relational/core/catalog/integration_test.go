package catalog

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/parser"
)

// TestIntegration_ParseAndResolveTable wires three packages together:
// parser, metadata, catalog. Exercises the end-to-end flow that the
// future semantic analyzer will use — parse a SELECT, walk the parse
// tree for a table identifier, look it up in the catalog, confirm the
// schema describes the expected columns.
//
// Catches regressions in the boundary between Phase 1 (parser) and
// Phase 2 (catalog) — each package has its own tests, but this one
// fails if either end changes its contract unilaterally.
func TestIntegration_ParseAndResolveTable(t *testing.T) {
	t.Parallel()

	// Build an in-memory catalog seeded with the demo template.
	c, tx, tmpl := newSeededCatalog(t, "integration")
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "public"), true); err != nil {
		t.Fatalf("SaveSchema: %v", err)
	}

	// Parse a simple SELECT. Parse() returns a non-nil IRootContext
	// on success.
	root, err := parser.Parse("SELECT order_id, price FROM Order WHERE order_id = 42")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Sanity-check: rendered text contains the table reference.
	if !strings.Contains(root.GetText(), "Order") {
		t.Errorf("parse tree text lost Order identifier: %q", root.GetText())
	}

	// Resolve "Order" via the catalog's DatabaseMetaData.
	md := NewCatalogDatabaseMetaData(CatalogDatabaseMetaDataOptions{StoreCatalog: c})
	rs, err := md.Tables(context.Background(), "/db", "public", "Order", nil)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("Tables('Order') returned no rows")
	}
	name, _ := rs.String(3) // TABLE_NAME
	typ, _ := rs.String(4)  // TABLE_TYPE
	if name != "Order" || typ != "TABLE" {
		t.Errorf("Tables row = (%q, %q), want (Order, TABLE)", name, typ)
	}
	if rs.Next() {
		t.Error("Tables('Order') returned more than one row")
	}
}

// TestIntegration_ResolveColumnsForParsedQuery goes one level deeper:
// parse + resolve the columns a SELECT references, confirm the catalog
// knows their types. Simulates what the semantic analyzer will do
// during Phase 3.
func TestIntegration_ResolveColumnsForParsedQuery(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "integration-cols")
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "public"), true); err != nil {
		t.Fatal(err)
	}

	// Target the same Order table. The parsed SELECT references
	// order_id and price — both must resolve to known columns with
	// the right types in the catalog.
	if _, err := parser.Parse("SELECT order_id, price FROM Order"); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	md := NewCatalogDatabaseMetaData(CatalogDatabaseMetaDataOptions{StoreCatalog: c})
	resolve := func(colName string) (jdbcType int64, found bool) {
		rs, err := md.Columns(context.Background(), "/db", "public", "Order", colName)
		if err != nil {
			t.Fatalf("Columns(%s): %v", colName, err)
		}
		defer rs.Close()
		if !rs.Next() {
			return 0, false
		}
		dt, _ := rs.Long(5)
		return dt, true
	}

	if dt, ok := resolve("order_id"); !ok {
		t.Error("order_id column not found")
	} else if dt != int64(api.JDBCType(api.CodeLong)) {
		t.Errorf("order_id JDBC type = %d, want BIGINT=%d", dt, api.JDBCType(api.CodeLong))
	}
	if dt, ok := resolve("price"); !ok {
		t.Error("price column not found")
	} else if dt != int64(api.JDBCType(api.CodeInteger)) {
		t.Errorf("price JDBC type = %d, want INTEGER=%d", dt, api.JDBCType(api.CodeInteger))
	}

	// A column that doesn't exist in Order must fail to resolve.
	// This is what the semantic analyzer will turn into a SQLSTATE
	// UNDEFINED_COLUMN.
	if _, ok := resolve("nonexistent_column"); ok {
		t.Error("resolve(nonexistent_column) found a row; catalog is lying about what's there")
	}
}

// TestIntegration_UnknownTableNotInCatalog: a query that references
// a table the catalog doesn't know about. The parser still succeeds —
// it's a pure syntax check — but Tables() should return zero rows, and
// the future semantic analyzer would error with UNDEFINED_TABLE.
func TestIntegration_UnknownTableNotInCatalog(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "integration-missing")
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "public"), true); err != nil {
		t.Fatal(err)
	}

	if _, err := parser.Parse("SELECT * FROM NotATable"); err != nil {
		t.Fatalf("Parse should accept any syntactically valid table reference: %v", err)
	}

	md := NewCatalogDatabaseMetaData(CatalogDatabaseMetaDataOptions{StoreCatalog: c})
	rs, err := md.Tables(context.Background(), "/db", "public", "NotATable", nil)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	defer rs.Close()
	if rs.Next() {
		t.Error("Tables('NotATable') returned a row; catalog should report empty")
	}
}

// newIntegrationCatalogWithIndexes is a fixture helper that seeds an
// in-memory catalog with a template that has a unique index — so the
// integration test for index resolution has something real to find.
func newIntegrationCatalogWithIndexes(t *testing.T) (*InMemoryStoreCatalog, api.Transaction) {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	unique := recordlayer.NewIndex("order_id_unique", recordlayer.Field("order_id"))
	unique.Options = map[string]string{"unique": "true"}
	b.AddIndex("Order", unique)
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplate("indexed", md)
	if err != nil {
		t.Fatal(err)
	}
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	if err := c.SchemaTemplateCatalog().CreateTemplate(tx, tmpl); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "public"), true); err != nil {
		t.Fatal(err)
	}
	return c, tx
}

// TestIntegration_IndexedTableMetadata proves that a schema template
// with a real index round-trips correctly through bridge + catalog +
// DatabaseMetaData.
func TestIntegration_IndexedTableMetadata(t *testing.T) {
	t.Parallel()
	c, _ := newIntegrationCatalogWithIndexes(t)
	md := NewCatalogDatabaseMetaData(CatalogDatabaseMetaDataOptions{StoreCatalog: c})

	// unique=true should surface the unique index.
	rs, err := md.IndexInfo(context.Background(), "/db", "public", "Order", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("IndexInfo(unique=true) returned no rows")
	}
	name, _ := rs.String(6) // INDEX_NAME
	if name != "order_id_unique" {
		t.Errorf("INDEX_NAME = %q, want order_customer_unique", name)
	}
}
