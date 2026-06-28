package sqldriver_test

// Probes two UNIQUE/INSERT subtleties: a UNIQUE index permits MULTIPLE NULLs
// (SQL-standard — NULLs are distinct for uniqueness), and a multi-row INSERT is
// ATOMIC (one duplicate fails the whole batch, nothing inserted).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_UniqueNullAndBatchAtomicity(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uniqnullb")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uniqnullb")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uniqnullb "+
			"CREATE TABLE t (id BIGINT NOT NULL, email STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email ON t (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uniqnullb/s WITH TEMPLATE uniqnullb")
	dsn := fmt.Sprintf("fdbsql:///testdb_uniqnullb?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	count := func() int64 {
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&c); err != nil {
			t.Fatalf("count: %v", err)
		}
		return c
	}
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, email) VALUES (1, 'a@x'), (2, 'b@x')")

	t.Run("unique_allows_multiple_nulls", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (3)") // NULL email
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (4)") // NULL email again
		if got := count(); got != 4 {
			t.Errorf("after 2 NULL-email inserts count = %d, want 4 (UNIQUE allows multiple NULLs)", got)
		}
	})

	t.Run("batch_insert_atomic_on_dup", func(t *testing.T) {
		before := count()
		_, err := db.ExecContext(ctx, "INSERT INTO t (id, email) VALUES (5, 'c@x'), (6, 'a@x')") // 'a@x' dup
		if err == nil {
			t.Fatal("batch with duplicate succeeded; want 23505")
		}
		if !strings.Contains(err.Error(), "23505") {
			t.Errorf("batch dup error = %v, want 23505", err)
		}
		if got := count(); got != before {
			t.Errorf("after failed batch count = %d, want %d (atomic — neither row inserted)", got, before)
		}
		// the non-duplicate row (id=5) must NOT be present.
		var c5 int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = 5").Scan(&c5); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c5 != 0 {
			t.Errorf("id=5 present after failed batch (count=%d); batch must be all-or-nothing", c5)
		}
	})
}
