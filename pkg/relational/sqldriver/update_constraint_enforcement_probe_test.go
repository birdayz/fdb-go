package sqldriver_test

// Pins constraint enforcement on the UPDATE path (less-covered than INSERT): an UPDATE
// that would violate a UNIQUE index → 23505, REJECTED with the row left UNCHANGED (no
// partial mutation / integrity corruption). This is a data-integrity sentinel: a
// regression here would silently corrupt (duplicate unique keys). The former
// UPDATE-to-NULL 23502 arm is gone: scalar NOT NULL is unexpressible (Java parity:
// NOT NULL is only allowed for ARRAY column type, rejected at CREATE); the scalar
// 23502 surface is gone.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_UpdateConstraintEnforcementProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uce")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uce")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE uce "+
		"CREATE TABLE t (id BIGINT, email STRING, nn BIGINT, PRIMARY KEY (id)) "+
		"CREATE UNIQUE INDEX t_email ON t (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uce/s WITH TEMPLATE uce")
	dsn := fmt.Sprintf("fdbsql:///testdb_uce?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, email, nn) VALUES (1,'a@x',10),(2,'b@x',20)")

	t.Run("update_to_duplicate_unique_rejected_23505_no_mutation", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE t SET email = 'a@x' WHERE id = 2")
		if err == nil || !strings.Contains(err.Error(), "23505") {
			t.Fatalf("UPDATE to duplicate unique = %v, want 23505", err)
		}
		var email string
		if err := db.QueryRowContext(ctx, "SELECT email FROM t WHERE id = 2").Scan(&email); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if email != "b@x" {
			t.Errorf("row 2 email = %q after rejected UPDATE, want b@x (no mutation)", email)
		}
	})
	t.Run("valid_update_still_works", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE t SET email = 'c@x' WHERE id = 2"); err != nil {
			t.Fatalf("valid UPDATE: %v", err)
		}
		var email string
		_ = db.QueryRowContext(ctx, "SELECT email FROM t WHERE id = 2").Scan(&email)
		if email != "c@x" {
			t.Errorf("row 2 email = %q, want c@x", email)
		}
	})
}
