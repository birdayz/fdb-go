package embedded

import (
	"strings"
	"testing"
)

// A quoted primary-key column keeps its case in the descriptor, and the
// primary-scan candidate names it verbatim, as the index candidates do: an
// equality on it is a primary-key scan (the target plans
// SCAN([IS footab, EQUALS ...]), WS-F oracle w13_quoted_pk_eq_*).
func TestPlanHarness_QuotedPrimaryKeyColumnIsScannable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ schema, sql string }{
		{`CREATE TABLE "footab" ("id" BIGINT, v BIGINT, PRIMARY KEY ("id"))`, `SELECT * FROM "footab" WHERE "id" = 1`},
		{`CREATE TABLE "fooTab" ("Id" BIGINT, v BIGINT, PRIMARY KEY ("Id"))`, `SELECT * FROM "fooTab" WHERE "Id" = 1`},
		{`CREATE TABLE T1 (id BIGINT, v BIGINT, PRIMARY KEY (id))`, `SELECT * FROM T1 WHERE id = 1`},
	} {
		plan, err := PlanPhysicalForTest(c.sql, c.schema, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if got := plan.Explain(); !strings.Contains(got, ", [=])") || strings.Contains(got, "PredicatesFilter") {
			t.Errorf("%s: plan %s, want a primary-key equality scan", c.sql, got)
		}
	}
}

// The vector candidate names its columns verbatim too: a quoted lowercase
// vector or primary-key column still reaches the vector index.
func TestPlanHarness_QuotedVectorColumnsReachTheIndex(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ schema, sql string }{
		{
			`CREATE TABLE "vt" ("id" BIGINT, "emb" VECTOR(3, FLOAT), PRIMARY KEY ("id")) CREATE VECTOR INDEX vi USING HNSW ON "vt" ("emb")`,
			`SELECT "id" FROM "vt" QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance("emb", [1.0, 0.0, 0.0])) <= 2`,
		},
		{
			`CREATE TABLE VT (id BIGINT, emb VECTOR(3, FLOAT), PRIMARY KEY (id)) CREATE VECTOR INDEX vi USING HNSW ON VT (emb)`,
			`SELECT id FROM vt QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(emb, [1.0, 0.0, 0.0])) <= 2`,
		},
	} {
		plan, err := PlanPhysicalForTest(c.sql, c.schema, nil)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if got := plan.Explain(); !strings.Contains(got, "VectorIndexScan") {
			t.Errorf("%s: plan %s, want a vector index scan", c.sql, got)
		}
	}
}
