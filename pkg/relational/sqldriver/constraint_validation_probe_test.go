package sqldriver_test

// Probes data-integrity constraint enforcement (wire-relevant — prevents writing
// invalid records): NOT NULL violations on INSERT/UPDATE → 23502, NULL primary
// key → 23502, INSERT column/value count mismatch → 42601, and a nullable column
// may be omitted.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ConstraintValidationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_constrp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_constrp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE constrp "+
			"CREATE TABLE t (id BIGINT NOT NULL, req BIGINT NOT NULL, opt BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_constrp/s WITH TEMPLATE constrp")
	dsn := fmt.Sprintf("fdbsql:///testdb_constrp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, req, opt) VALUES (1, 10, 100)")

	rejects := func(name, q, code string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, q)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", name)
			}
			if !strings.Contains(err.Error(), code) {
				t.Errorf("%s error = %v, want SQLSTATE %s", name, err, code)
			}
		})
	}

	rejects("insert_null_into_not_null", "INSERT INTO t (id, req, opt) VALUES (2, NULL, 200)", "23502")
	rejects("insert_omit_not_null", "INSERT INTO t (id, opt) VALUES (3, 300)", "23502")
	rejects("update_not_null_to_null", "UPDATE t SET req = NULL WHERE id = 1", "23502")
	rejects("insert_null_pk", "INSERT INTO t (id, req) VALUES (NULL, 80)", "23502")
	rejects("insert_value_count_mismatch", "INSERT INTO t (id, req) VALUES (7, 70, 700)", "42601")

	t.Run("omit_nullable_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id, req) VALUES (4, 40)"); err != nil {
			t.Errorf("omitting nullable opt failed: %v", err)
		}
		var opt sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT opt FROM t WHERE id = 4").Scan(&opt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if opt.Valid {
			t.Errorf("omitted nullable opt = %d, want NULL", opt.Int64)
		}
	})
	t.Run("req_unchanged_after_failed_update", func(t *testing.T) {
		// the failed UPDATE req=NULL must not have changed id=1's req.
		var req int64
		if err := db.QueryRowContext(ctx, "SELECT req FROM t WHERE id = 1").Scan(&req); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if req != 10 {
			t.Errorf("req after failed NOT NULL update = %d, want 10 (unchanged)", req)
		}
	})
}
