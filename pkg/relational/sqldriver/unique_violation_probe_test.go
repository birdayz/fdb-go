package sqldriver_test

// Probes the UNIQUE / primary-key violation → SQLSTATE 23505 mappings (flagged in
// CLAUDE.md as fragile — a deleted DML helper once silently dropped the
// secondary-UNIQUE→23505 mapping). Covers PK duplicate, secondary-UNIQUE
// duplicate on INSERT, and a UNIQUE violation introduced by UPDATE.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_UniqueViolationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uniqv")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uniqv")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uniqv "+
			"CREATE TABLE t (id BIGINT NOT NULL, email STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email ON t (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uniqv/s WITH TEMPLATE uniqv")
	dsn := fmt.Sprintf("fdbsql:///testdb_uniqv?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, email) VALUES (1, 'a@x'), (2, 'b@x')")

	exec := func(q string) error {
		_, err := db.ExecContext(ctx, q)
		return err
	}

	t.Run("pk_duplicate_23505", func(t *testing.T) {
		err := exec("INSERT INTO t (id, email) VALUES (1, 'z@x')")
		if err == nil {
			t.Fatal("duplicate PK insert succeeded; want 23505")
		}
		if !strings.Contains(err.Error(), "23505") {
			t.Errorf("PK duplicate error = %v, want SQLSTATE 23505", err)
		}
	})

	t.Run("secondary_unique_duplicate_23505", func(t *testing.T) {
		err := exec("INSERT INTO t (id, email) VALUES (3, 'a@x')") // email a@x taken by id=1
		if err == nil {
			t.Fatal("duplicate secondary-UNIQUE insert succeeded; want 23505")
		}
		if !strings.Contains(err.Error(), "23505") {
			t.Errorf("secondary-UNIQUE duplicate error = %v, want SQLSTATE 23505", err)
		}
	})

	t.Run("update_into_unique_violation_23505", func(t *testing.T) {
		// set id=1's email to b@x, which is taken by id=2 → UNIQUE violation.
		err := exec("UPDATE t SET email = 'b@x' WHERE id = 1")
		if err == nil {
			t.Fatal("UPDATE into duplicate email succeeded; want 23505")
		}
		if !strings.Contains(err.Error(), "23505") {
			t.Errorf("UPDATE UNIQUE violation error = %v, want SQLSTATE 23505", err)
		}
	})

	t.Run("distinct_email_insert_ok", func(t *testing.T) {
		// A genuinely-new row with a distinct email must still succeed (guard
		// against an over-broad UNIQUE check).
		if err := exec("INSERT INTO t (id, email) VALUES (4, 'd@x')"); err != nil {
			t.Errorf("distinct insert failed: %v", err)
		}
	})
}
