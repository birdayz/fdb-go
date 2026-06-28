package sqldriver_test

// Probes UNIQUE-index enforcement on the UPDATE path: updating a row so its
// indexed value collides with another row's must fail 23505 (and not corrupt the
// index); updating a row to its OWN current value is a no-op (no false collision);
// updating to a fresh unique value succeeds and maintains the index.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_UpdateUniqueViolationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_upduniq")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_upduniq")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE upduniq "+
			"CREATE TABLE t (id BIGINT NOT NULL, email STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX t_email ON t (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_upduniq/s WITH TEMPLATE upduniq")
	dsn := fmt.Sprintf("fdbsql:///testdb_upduniq?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, email) VALUES (1, 'a@x'), (2, 'b@x')")

	emailOf := func(id int) string {
		var s sql.NullString
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT email FROM t WHERE id = %d", id)).Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return s.String
	}

	t.Run("update_to_existing_value_23505", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE t SET email = 'a@x' WHERE id = 2")
		if err == nil || !strings.Contains(err.Error(), "23505") {
			t.Errorf("UPDATE id=2 email→'a@x' (dup) error = %v, want 23505", err)
		}
		// id=2 must be unchanged.
		if got := emailOf(2); got != "b@x" {
			t.Errorf("id=2 email after failed UPDATE = %q, want b@x (unchanged)", got)
		}
		// the unique index for 'a@x' must still resolve to id=1 only.
		var n int
		rows, _ := db.QueryContext(ctx, "SELECT id FROM t WHERE email = 'a@x'")
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 1 {
			t.Errorf("email='a@x' returns %d rows after failed UPDATE, want 1 (index not corrupted)", n)
		}
	})

	t.Run("update_to_same_value_noop", func(t *testing.T) {
		// updating id=1 to its OWN value must NOT be a false self-collision.
		if _, err := db.ExecContext(ctx, "UPDATE t SET email = 'a@x' WHERE id = 1"); err != nil {
			t.Errorf("UPDATE id=1 email→'a@x' (same value) failed: %v", err)
		}
		if got := emailOf(1); got != "a@x" {
			t.Errorf("id=1 email = %q, want a@x", got)
		}
	})

	t.Run("update_to_fresh_value_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE t SET email = 'c@x' WHERE id = 2"); err != nil {
			t.Fatalf("UPDATE to fresh value failed: %v", err)
		}
		// old 'b@x' gone, new 'c@x' present.
		if emailOf(2) != "c@x" {
			t.Errorf("id=2 email = %q, want c@x", emailOf(2))
		}
		var nb int
		rows, _ := db.QueryContext(ctx, "SELECT id FROM t WHERE email = 'b@x'")
		for rows.Next() {
			nb++
		}
		rows.Close()
		if nb != 0 {
			t.Errorf("email='b@x' returns %d rows, want 0 (stale unique entry gone)", nb)
		}
		// now 'b@x' is free — a new row can take it.
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id, email) VALUES (3, 'b@x')"); err != nil {
			t.Errorf("INSERT freed 'b@x' failed: %v", err)
		}
	})
}
