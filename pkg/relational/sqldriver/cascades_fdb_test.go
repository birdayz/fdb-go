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
	t.Logf("Cascades SELECT returned %d rows — end-to-end works!", count)
}
