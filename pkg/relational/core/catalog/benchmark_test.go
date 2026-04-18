package catalog

import (
	"strconv"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

func benchmarkCatalogWithSchemas(b *testing.B, numDatabases, schemasPerDB int) (*InMemoryStoreCatalog, api.Transaction, api.SchemaTemplate) {
	b.Helper()
	c, tx, tmpl := newSeededCatalog(b, "demo")
	for d := 0; d < numDatabases; d++ {
		db := "/db-" + strconv.Itoa(d)
		for s := 0; s < schemasPerDB; s++ {
			if err := c.SaveSchema(tx, tmpl.GenerateSchema(db, "s-"+strconv.Itoa(s)), true); err != nil {
				b.Fatalf("seed SaveSchema: %v", err)
			}
		}
	}
	return c, tx, tmpl
}

// BenchmarkLoadSchema measures the steady-state schema-lookup cost.
// Catalog is pre-populated so only the lookup path matters.
func BenchmarkLoadSchema(b *testing.B) {
	c, tx, _ := benchmarkCatalogWithSchemas(b, 10, 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.LoadSchema(tx, "/db-5", "s-5"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDoesSchemaExist mirrors LoadSchema without the Schema
// materialisation — a hot path for parse-time validation.
func BenchmarkDoesSchemaExist(b *testing.B) {
	c, tx, _ := benchmarkCatalogWithSchemas(b, 10, 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.DoesSchemaExist(tx, "/db-5", "s-5"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkListSchemasInDatabase walks a moderately-sized DB's
// schemas. Matches what a SHOW SCHEMAS command will do.
func BenchmarkListSchemasInDatabase(b *testing.B) {
	c, tx, _ := benchmarkCatalogWithSchemas(b, 1, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rs, err := c.ListSchemasInDatabase(tx, "/db-0", nil)
		if err != nil {
			b.Fatal(err)
		}
		for rs.Next() {
			_, _ = rs.String(2)
		}
		_ = rs.Close()
	}
}

// BenchmarkSaveSchemaUpsert captures the cost of replacing an existing
// schema in place — the repair / DDL-update hot path.
func BenchmarkSaveSchemaUpsert(b *testing.B) {
	c, tx, tmpl := benchmarkCatalogWithSchemas(b, 1, 1)
	existing := tmpl.GenerateSchema("/db-0", "s-0")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.SaveSchema(tx, existing, false); err != nil {
			b.Fatal(err)
		}
	}
}
