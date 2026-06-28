package sqldriver_test

// KNOWN GAP sentinel — DELETE/UPDATE ... RETURNING is silently ignored (TODO.md
// "DML RETURNING not implemented"). The shared grammar carries `(RETURNING
// selectElements)?` on deleteStatement/updateStatement, and JAVA SUPPORTS IT —
// QueryVisitor.visitDeleteStatement:848 / visitUpdateStatement:882 build a
// generateSelect from the RETURNING selectElements and return the affected rows as a
// result set. Go drops the clause: via Query you hit the generic DML-via-Query guard
// (0A000 "INSERT/UPDATE/DELETE return a row count, not rows"), and via Exec the
// DML executes correctly but the RETURNING values never surface (count only).
//
// This is NOT data loss (the DELETE/UPDATE is correct) — it's a Java-supported feature
// silently unimplemented. Fix = port Java's generateSelect-from-RETURNING and wire DML
// result sets through the driver Query path (a feature port, not a one-liner). This
// pins the current boundary; flip when implemented. (INSERT RETURNING is a 42601 — not
// in the INSERT grammar at all — so it is not covered here.)

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ReturningClauseProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ret")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ret")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ret CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ret/s WITH TEMPLATE ret")
	dsn := fmt.Sprintf("fdbsql:///testdb_ret?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	count := func() int {
		var n int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n)
		return n
	}

	t.Run("delete_returning_via_query_rejected_0A000", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20)")
		rows, err := db.QueryContext(ctx, "DELETE FROM t WHERE id = 1 RETURNING id")
		if err == nil {
			rows.Close()
			t.Fatalf("DELETE ... RETURNING via Query unexpectedly returned rows — " +
				"RETURNING may now be implemented; flip this sentinel + update TODO.md")
		}
		if !strings.Contains(err.Error(), "0A000") {
			t.Errorf("DELETE RETURNING via Query error = %v, want 0A000 (DML returns a count)", err)
		}
	})
	t.Run("delete_returning_via_exec_drops_clause_but_executes", func(t *testing.T) {
		// Exec runs the DELETE (correct) but the RETURNING is silently dropped.
		before := count()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE id = 2 RETURNING id")
		if err != nil {
			t.Fatalf("DELETE ... RETURNING via Exec: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("RowsAffected = %d, want 1 (DELETE still executes)", n)
		}
		if c := count(); c != before-1 {
			t.Errorf("count = %d, want %d (one row deleted)", c, before-1)
		}
	})
	t.Run("update_returning_via_query_rejected_0A000", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (3,30)")
		rows, err := db.QueryContext(ctx, "UPDATE t SET a = 99 WHERE id = 3 RETURNING a")
		if err == nil {
			rows.Close()
			t.Fatalf("UPDATE ... RETURNING via Query unexpectedly returned rows — " +
				"RETURNING may now be implemented; flip this sentinel + update TODO.md")
		}
		if !strings.Contains(err.Error(), "0A000") {
			t.Errorf("UPDATE RETURNING via Query error = %v, want 0A000", err)
		}
	})
}
