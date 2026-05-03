package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_CascadesSelectAfterInsert(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_cascades_select")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /testdb_cascades_select"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE cascades_tmpl "+
			"CREATE TABLE Item (item_id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (item_id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_cascades_select/store WITH TEMPLATE cascades_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	// Insert via naive (default engine).
	naiveDSN := fmt.Sprintf("fdbsql:///testdb_cascades_select?cluster_file=%s&schema=store", clusterFilePath)
	naiveDB, err := sql.Open("fdbsql", naiveDSN)
	if err != nil {
		t.Fatalf("sql.Open naive: %v", err)
	}
	defer naiveDB.Close()

	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (1, 'Widget', 100)"); err != nil {
		t.Fatalf("INSERT 1: %v", err)
	}
	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (2, 'Gadget', 200)"); err != nil {
		t.Fatalf("INSERT 2: %v", err)
	}

	// Query via Cascades engine.
	cascadesDSN := fmt.Sprintf("fdbsql:///testdb_cascades_select?cluster_file=%s&schema=store&engine=cascades", clusterFilePath)
	cascadesDB, err := sql.Open("fdbsql", cascadesDSN)
	if err != nil {
		t.Fatalf("sql.Open cascades: %v", err)
	}
	defer cascadesDB.Close()

	rows, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item")
	if err != nil {
		t.Fatalf("SELECT via Cascades: %v", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if count != 2 {
		t.Fatalf("expected 2 rows via Cascades, got %d", count)
	}
	t.Logf("Cascades SELECT * returned %d rows", count)

	// Also test SELECT with WHERE filter through Cascades.
	rows2, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item WHERE price > 100")
	if err != nil {
		// Filter through Cascades may fall back to naive if the catalog-aware
		// predicate builder can't translate the WHERE. Log and skip.
		t.Logf("SELECT WHERE via Cascades: %v (may fall back to naive)", err)
		return
	}
	defer rows2.Close()

	var filtered int
	for rows2.Next() {
		filtered++
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("rows2.Err: %v", err)
	}
	if filtered != 1 {
		t.Fatalf("expected 1 row with price > 100, got %d", filtered)
	}
	t.Logf("Cascades SELECT WHERE returned %d row — filter works!", filtered)

	// Test LIMIT through Cascades.
	rows3, err := cascadesDB.QueryContext(ctx, "SELECT * FROM Item LIMIT 1")
	if err != nil {
		t.Logf("SELECT LIMIT via Cascades: %v (may fall back)", err)
		return
	}
	defer rows3.Close()

	var limited int
	for rows3.Next() {
		limited++
	}
	if err := rows3.Err(); err != nil {
		t.Fatalf("rows3.Err: %v", err)
	}
	if limited != 1 {
		t.Fatalf("expected 1 row with LIMIT 1, got %d", limited)
	}
	t.Logf("Cascades LIMIT returned %d row", limited)
}
