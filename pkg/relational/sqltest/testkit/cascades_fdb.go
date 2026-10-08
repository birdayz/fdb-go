package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func SetupCascadesTestDB(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/FRL/casc_%s", t.Name())
	setup := OpenDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("casc_tmpl_%s", t.Name())
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE Item (item_id BIGINT, name STRING, price BIGINT, PRIMARY KEY (item_id))", tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	naiveDSN := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=STORE", strings.ToUpper(dbPath), clusterFilePath)
	naiveDB, err := sql.Open("fdbsql", naiveDSN)
	if err != nil {
		t.Fatalf("sql.Open naive: %v", err)
	}
	t.Cleanup(func() { naiveDB.Close() })

	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (1, 'Widget', 100)"); err != nil {
		t.Fatalf("INSERT 1: %v", err)
	}
	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (2, 'Gadget', 200)"); err != nil {
		t.Fatalf("INSERT 2: %v", err)
	}
	if _, err := naiveDB.ExecContext(ctx, "INSERT INTO Item VALUES (3, 'Doohickey', 50)"); err != nil {
		t.Fatalf("INSERT 3: %v", err)
	}

	cascadesDSN := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=STORE", strings.ToUpper(dbPath), clusterFilePath)
	cascadesDB, err := sql.Open("fdbsql", cascadesDSN)
	if err != nil {
		t.Fatalf("sql.Open cascades: %v", err)
	}
	t.Cleanup(func() { cascadesDB.Close() })

	return naiveDB, cascadesDB
}

func CountRows(t *testing.T, rows *sql.Rows) int {
	t.Helper()
	var n int
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return n
}
