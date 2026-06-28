package sqldriver_test

// Probes identifier case semantics. Unquoted identifiers are case-INSENSITIVE
// (folded to upper case): a column declared `MyCol` resolves as MyCol/mycol/MYCOL,
// and a table declared `MyTable` is referenceable case-insensitively. This is the
// common, correct path. (Quoted DDL identifiers like "KeepCase" are a known
// divergence — created + visible in SELECT * but unreferenceable by name; see
// TODO.md "quoted-identifier case handling" — not exercised here.)

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_IdentifierCaseProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_identcase")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_identcase")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE identcase CREATE TABLE MyTable (id BIGINT NOT NULL, MyCol BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_identcase/s WITH TEMPLATE identcase")
	dsn := fmt.Sprintf("fdbsql:///testdb_identcase?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO MyTable (id, MyCol) VALUES (1, 42)")

	val := func(colRef string) (int64, error) {
		var v int64
		err := db.QueryRowContext(ctx, "SELECT "+colRef+" FROM MyTable WHERE id = 1").Scan(&v)
		return v, err
	}
	for _, ref := range []string{"MyCol", "mycol", "MYCOL", "MyCOL"} {
		ref := ref
		t.Run("col_"+ref, func(t *testing.T) {
			v, err := val(ref)
			if err != nil {
				t.Fatalf("SELECT %s: %v (unquoted identifiers must be case-insensitive)", ref, err)
			}
			if v != 42 {
				t.Errorf("SELECT %s = %d, want 42", ref, v)
			}
		})
	}
	// table name is also case-insensitive when unquoted.
	t.Run("table_case_insensitive", func(t *testing.T) {
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mytable").Scan(&c); err != nil {
			t.Fatalf("FROM mytable: %v", err)
		}
		if c != 1 {
			t.Errorf("COUNT(*) FROM mytable = %d, want 1", c)
		}
	})
}
